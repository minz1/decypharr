import { describe, expect, it } from "vitest";

import {
  brokenFileIds,
  clampPage,
  formatFileSize,
  formatRunDuration,
  formatTimestamp,
  healthColor,
  pageButtons,
  runBadgeClass,
  sortBroken,
} from "#ui/lib/repair/format";
import type { EntryHealth } from "#ui/lib/repair/types";

const entry = (entryName: string, lastFailedAt: string): EntryHealth => ({
  entryName,
  protocol: "",
  fileCount: 0,
  brokenCount: 0,
  failureReason: "",
  lastCheckedAt: "",
  lastFailedAt,
  lastRepairAt: "",
  brokenFiles: [],
});

describe("the health colour", () => {
  it.each([
    ["healthy", "text-success"],
    ["broken", "text-error"],
    ["repairing", "text-info"],
    ["stale", "text-warning"],
    ["unsupported", "text-base-content/60"],
    ["unknown", ""],
  ])("of %s is %j", (status, colour) => {
    expect(healthColor(status)).toBe(colour);
  });
});

describe("the run badge", () => {
  it.each([
    ["running", "badge-info"],
    ["completed", "badge-success"],
    ["failed", "badge-error"],
    ["cancelled", "badge-warning"],
    ["", "badge-ghost"],
    ["toString", "badge-ghost"],
  ])("of %j is %s", (status, badge) => {
    expect(runBadgeClass(status)).toBe(badge);
  });
});

describe("the run duration", () => {
  it.each([
    ["2026-01-01T00:00:00Z", "2026-01-01T00:00:42Z", "42s"],
    ["2026-01-01T00:00:00Z", "2026-01-01T00:02:05.600Z", "2m 6s"],
    ["2026-01-01T00:00:00Z", "0001-01-01T00:00:00Z", "-"],
    ["2026-01-01T00:00:00Z", "", "running"],
    ["", "", "-"],
    ["2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z", "-"],
  ])("from %j to %j is %j", (startedAt, completedAt, duration) => {
    expect(formatRunDuration(startedAt, completedAt)).toBe(duration);
  });
});

describe("the file size", () => {
  it.each([
    [0, "-"],
    [-5, "-"],
    [512, "512 B"],
    [1536, "1.5 KB"],
    [10_240, "10 KB"],
    [5 * 1024 ** 4, "5.0 TB"],
    [2048 * 1024 ** 4, "2048 TB"],
  ])("of %d is %j", (size, text) => {
    expect(formatFileSize(size)).toBe(text);
  });
});

describe("a timestamp", () => {
  it("is the local time", () => {
    expect(formatTimestamp("2026-01-02T03:04:05Z", "-")).toBe(new Date("2026-01-02T03:04:05Z").toLocaleString());
  });

  it("falls back when empty", () => {
    expect(formatTimestamp("", "unknown")).toBe("unknown");
  });
});

describe("the broken file ids", () => {
  it("lists the ids that are set", () => {
    expect([
      brokenFileIds({ mediaId: 1, episodeId: 2, arrFileId: 3 }),
      brokenFileIds({ mediaId: 1, episodeId: 0, arrFileId: 3 }),
      brokenFileIds({ mediaId: 0, episodeId: 0, arrFileId: 0 }),
    ]).toStrictEqual(["media:1 · ep:2 · file:3", "media:1 · file:3", ""]);
  });
});

describe("sorting broken entries", () => {
  it("puts the latest failure first, then orders by name", () => {
    const sorted = sortBroken([
      entry("b", ""),
      entry("old", "2026-01-01T00:00:00Z"),
      entry("a", ""),
      entry("new", "2026-02-01T00:00:00Z"),
      entry("zero", "0001-01-01T00:00:00Z"),
    ]);
    expect(sorted.map((item) => item.entryName)).toStrictEqual(["new", "old", "a", "b", "zero"]);
  });
});

describe("clamping a page", () => {
  it.each([
    [1, 0, 1],
    [3, 30, 2],
    [0, 30, 1],
    [2, 51, 2],
  ])("page %d of %d items is %d", (page, items, clamped) => {
    expect(clampPage(page, items, 25)).toBe(clamped);
  });
});

describe("the page buttons", () => {
  it("disables the arrows at the edges", () => {
    expect(pageButtons(1, 2)).toStrictEqual([
      { label: "«", target: 0, active: false, disabled: true },
      { label: "1", target: 1, active: true, disabled: false },
      { label: "2", target: 2, active: false, disabled: false },
      { label: "»", target: 2, active: false, disabled: false },
    ]);
  });

  it("shows two pages either side, the ends, and a gap marker", () => {
    expect(pageButtons(6, 10).map((button) => button.label)).toStrictEqual([
      "«",
      "1",
      "…",
      "4",
      "5",
      "6",
      "7",
      "8",
      "…",
      "10",
      "»",
    ]);
  });

  it("marks only one gap when the window touches an end", () => {
    const buttons = pageButtons(10, 10);
    expect(buttons.map((button) => button.label)).toStrictEqual(["«", "1", "…", "8", "9", "10", "»"]);
    expect(buttons.at(-1)).toStrictEqual({ label: "»", target: 11, active: false, disabled: true });
    expect(buttons[2]).toStrictEqual({ label: "…", target: undefined, active: false, disabled: true });
  });
});
