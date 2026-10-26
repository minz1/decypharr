import { describe, expect, it } from "vitest";

import { floatOr, intOr, lines, orDefault, splitList, toText } from "#ui/lib/config/form";

describe("reading numbers as config.js did with parseInt and parseFloat", () => {
  it.each([
    ["12", 7, 12],
    ["-3", 7, -3],
    ["  42", 7, 42],
    ["1.9", 7, 1],
    ["1e3", 7, 1],
    ["12abc", 7, 12],
    ["0", 7, 7],
    ["", 7, 7],
    ["abc", 7, 7],
    [undefined, 7, 7],
  ])("intOr(%j, %d) is %d", (text, fallback, want) => {
    expect(intOr(text, fallback)).toBe(want);
  });

  it.each([
    ["0.25", 0, 0.25],
    [".5", 0, 0.5],
    ["1e3", 0, 1000],
    ["2.5E-1", 0, 0.25],
    ["-Infinity", 0, Number.NEGATIVE_INFINITY],
    ["infinity", 9, 9],
    ["", 9, 9],
    ["0", 9, 9],
  ])("floatOr(%j, %d) is %d", (text, fallback, want) => {
    expect(floatOr(text, fallback)).toBe(want);
  });
});

describe("reading text as config.js did", () => {
  it("orDefault replaces only the empty string and a missing control", () => {
    expect([orDefault("", "x"), orDefault(undefined, "x"), orDefault(" ", "x"), orDefault("a", "x")]).toStrictEqual([
      "x",
      "x",
      " ",
      "a",
    ]);
  });

  it("lines and lists drop blanks and trim", () => {
    expect({
      lines: lines(" a \n\n b\n"),
      missing: lines(),
      networks: splitList("10.0.0.0/8, 192.168.0.0/16;\n::1/128", /[\s,;]+/u),
    }).toStrictEqual({ lines: ["a", "b"], missing: [], networks: ["10.0.0.0/8", "192.168.0.0/16", "::1/128"] });
  });

  it("toText writes values the way a control coerces them", () => {
    expect([
      toText("a"),
      toText(5),
      toText(false),
      toText(["a", 1]),
      toText(JSON.parse("null")),
      toText({}),
    ]).toStrictEqual(["a", "5", "false", "a,1", "", ""]);
  });
});
