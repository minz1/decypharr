import { describe, expect, it } from "vitest";

import { describeError } from "#ui/lib/common/errors";

describe("describing an error", () => {
  it("gives an Error's message without its name", () => {
    expect(describeError(new TypeError("bad input"))).toBe("bad input");
  });

  it.each([
    ["text", "text"],
    [42, "42"],
    [undefined, "undefined"],
  ])("stringifies the thrown value %j", (thrown, want) => {
    expect(describeError(thrown)).toBe(want);
  });
});
