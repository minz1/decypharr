import { describe, expect, it } from "vitest";

import { normalizeCondition, refreshedCondition, validateFolder } from "#ui/lib/config/vf-model";

const folder = (
  name: string,
  conditions: Parameters<typeof validateFolder>[0]["conditions"] = [],
): Parameters<typeof validateFolder>[0] => ({
  name,
  match: "all",
  include_bad: false,
  conditions,
});

const condition = (
  field: string,
  operator: string,
  value: string,
): Parameters<typeof validateFolder>[0]["conditions"][number] => ({
  field,
  operator,
  value,
  case_sensitive: false,
});

describe("virtual folder conditions", () => {
  it("normalize an unknown field and operator to the first choice", () => {
    expect([
      normalizeCondition({}),
      normalizeCondition({ field: "nope", operator: "contains", value: "x", case_sensitive: true }),
      normalizeCondition({ field: "size", operator: "contains" }),
    ]).toStrictEqual([
      condition("entry_name", "contains", ""),
      { ...condition("entry_name", "contains", "x"), case_sensitive: true },
      condition("size", "greater_than", ""),
    ]);
  });

  it("refresh clears the value of a changed field and picks valid choices", () => {
    expect([
      refreshedCondition(condition("size", "contains", "2160p"), "entry_name", []),
      refreshedCondition(condition("protocol", "contains", ""), "size", []),
      refreshedCondition(condition("provider", "equals", ""), "protocol", ["rd", "usenet"]),
      refreshedCondition(condition("entry_name", "contains", "keep"), "entry_name", []),
    ]).toStrictEqual([
      condition("size", "greater_than", ""),
      condition("protocol", "equals", "torrent"),
      condition("provider", "equals", "rd"),
      condition("entry_name", "contains", "keep"),
    ]);
  });
});

const troubled = folder("x", [
  condition("size", "contains", "big"),
  condition("entry_name", "contains", ""),
  condition("size", "greater_than", "big"),
  condition("added", "within_last", "7days"),
  condition("file_count", "greater_than", "1.5"),
  condition("file_name", "matches_regex", "(unclosed"),
  condition("added", "within_last", "2w3d1.5h"),
]);
const episodes = folder("x", [condition("file_name", "matches_regex", String.raw`S\d+E\d+`)]);
const escapedHyphen = folder("x", [condition("file_name", "not_matches_regex", String.raw`a\-b`)]);

describe("validating a virtual folder", () => {
  it.each([
    ["", "Folder name is required."],
    ["..", "Folder name contains characters that are not portable across mounts and shares."],
    ["a:b", "Folder name contains characters that are not portable across mounts and shares."],
    ["tab\there", "Folder name contains characters that are not portable across mounts and shares."],
    ["COM1.txt", "Folder name is reserved by common filesystems."],
    ["trailing.", "Folder name is reserved by common filesystems."],
  ])("rejects the name %j", (name, error) => {
    expect(validateFolder(folder(name))).toContain(error);
  });

  it("accepts a plain name", () => {
    expect(validateFolder(folder("4K Movies"))).toStrictEqual([]);
  });

  it("names each condition's problem", () => {
    expect(validateFolder(troubled)).toStrictEqual([
      "Condition 1 has an unsupported rule.",
      "Condition 1 needs a size such as 700MB or 20GB.",
      "Condition 2 needs a value.",
      "Condition 3 needs a size such as 700MB or 20GB.",
      "Condition 4 needs a duration such as 12h, 7d, or 2w.",
      "Condition 5 needs a whole number of files.",
    ]);
  });

  it("leaves regex syntax to the server, which compiles patterns with Go's engine", () => {
    expect([validateFolder(episodes), validateFolder(escapedHyphen)]).toStrictEqual([[], []]);
  });
});
