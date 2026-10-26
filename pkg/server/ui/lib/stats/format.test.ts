import { describe, expect, it } from "vitest";

import { formatNumber, formatPercent, formatUptime } from "#ui/lib/stats/format";

describe("formatting an uptime", () => {
  it.each([
    [0, "-"],
    [Number.NaN, "-"],
    [42, "42s"],
    [61, "1m 1s"],
    [3725, "1h 2m 5s"],
    [90_061, "1d 1h 1m"],
    [12.9, "12s"],
  ])("formats %d s as %j", (seconds, want) => {
    expect(formatUptime(seconds)).toBe(want);
  });
});

describe("formatting a count", () => {
  it("groups thousands in the browser's locale", () => {
    expect(formatNumber(1_234_567)).toBe((1_234_567).toLocaleString());
    expect(formatNumber(0)).toBe("0");
  });
});

describe("formatting a ratio as a percentage", () => {
  it.each([
    [0, "0.0%"],
    [0.5, "50.0%"],
    [0.12345, "12.3%"],
    [1, "100.0%"],
  ])("formats %d as %j", (ratio, want) => {
    expect(formatPercent(ratio)).toBe(want);
  });
});
