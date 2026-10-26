import { describe, expect, it, vi } from "vitest";

import { debounce, formatBytes, formatDuration, formatSpeed, orElse } from "#ui/lib/common/format";

describe("formatting bytes", () => {
  it.each([
    [0, "0 B"],
    [1023, "1023 B"],
    [1024, "1 KB"],
    [1536, "1.5 KB"],
    [1.5 * 1024 ** 3, "1.5 GB"],
    [1_234_567, "1.18 MB"],
    [Number.NaN, "0 B"],
  ])("formats %d as %j", (bytes, want) => {
    expect(formatBytes(bytes)).toBe(want);
  });
});

describe("formatting a speed", () => {
  it("adds /s", () => {
    expect(formatSpeed(1024)).toBe("1 KB/s");
    expect(formatSpeed(0)).toBe("0 B/s");
  });
});

describe("formatting a duration", () => {
  it.each([
    [0, "0s"],
    [59, "59s"],
    [60, "1m"],
    [3661, "1h 1m"],
    [90_061, "1d 1h"],
    [86_400, "1d"],
    [-5, "0s"],
    [Number.NaN, "0s"],
  ])("formats %d s as %j", (seconds, want) => {
    expect(formatDuration(seconds)).toBe(want);
  });
});

const debounced = (): { seen: string[]; call: (text: string) => void } => {
  vi.useFakeTimers();
  const seen: string[] = [];
  const call = debounce((text: string) => {
    seen.push(text);
  }, 100);
  return { seen, call };
};

describe("debouncing", () => {
  it("waits until the calls stop", () => {
    const { seen, call } = debounced();
    call("a");
    vi.advanceTimersByTime(99);
    call("b");
    vi.advanceTimersByTime(99);
    vi.useRealTimers();
    expect(seen).toStrictEqual([]);
  });

  it("then calls once with the last arguments", () => {
    const { seen, call } = debounced();
    call("a");
    vi.advanceTimersByTime(99);
    call("b");
    vi.advanceTimersByTime(100);
    vi.useRealTimers();
    expect(seen).toStrictEqual(["b"]);
  });
});

describe("falling back from an empty string", () => {
  it("keeps a non-empty value and replaces an empty one", () => {
    expect([orElse("x", "-"), orElse("", "-")]).toStrictEqual(["x", "-"]);
  });

  it("keeps a value that is only whitespace", () => {
    expect(orElse(" ", "-")).toBe(" ");
  });
});
