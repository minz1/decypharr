import { describe, expect, it } from "vitest";

import { ShapeError } from "#ui/lib/common/json";
import { cleanupToast, parseCleanup, parsePurge, purgeSummary, purgeToast } from "#ui/lib/stats/cache";

describe("the cache cleanup toast", () => {
  it("reports the freed bytes and evicted files", () => {
    const result = parseCleanup({
      status: "success",
      cache: {
        cleanup_status: "ok",
        cleanup_freed_bytes: 1_572_864,
        cleanup_removed_items: 1234,
        cleanup_warning_count: 0,
      },
    });
    expect(cleanupToast(result)).toStrictEqual({
      message: `Cache cleanup completed. Freed 1.5 MB and evicted ${(1234).toLocaleString()} file(s).`,
      type: "success",
    });
  });

  it("warns about warnings", () => {
    expect(cleanupToast(parseCleanup({ cache: { cleanup_warning_count: 2, cleanup_freed_bytes: 5 } }))).toStrictEqual({
      message: "Cache cleanup completed with 2 warning(s).",
      type: "warning",
    });
  });

  it("reads a response without cache figures as zero", () => {
    expect(cleanupToast(parseCleanup({ status: "success" })).message).toBe(
      "Cache cleanup completed. Freed 0 B and evicted 0 file(s).",
    );
  });

  it("rejects a malformed response", () => {
    expect(() => parseCleanup([])).toThrow(ShapeError);
    expect(() => parseCleanup({ cache: { cleanup_warning_count: "2" } })).toThrow(ShapeError);
  });
});

describe("the cache purge toast", () => {
  it("reports the freed bytes and removed files", () => {
    const result = parsePurge({
      status: "success",
      cache: { purge_freed_bytes: 2048, purge_removed_items: 3, purge_warning_count: 0, purge_skipped_busy_items: 0 },
    });
    expect(purgeToast(result)).toStrictEqual({
      message: "Cache purge completed. Freed 2 KB and removed 3 file(s).",
      type: "success",
    });
  });

  it("adds the skipped active items", () => {
    expect(purgeToast(parsePurge({ cache: { purge_removed_items: 3, purge_skipped_busy_items: 2 } })).message).toBe(
      "Cache purge completed. Freed 0 B and removed 3 file(s). Skipped 2 active item(s).",
    );
  });

  it("warns about warnings and still adds the skipped items", () => {
    expect(purgeToast(parsePurge({ cache: { purge_warning_count: 1, purge_skipped_busy_items: 4 } }))).toStrictEqual({
      message: "Cache purge completed with 1 warning(s). Skipped 4 active item(s).",
      type: "warning",
    });
  });
});

describe("the purge summary", () => {
  it("shows the files and the disk used against the limit", () => {
    expect(purgeSummary({ items: 1200, size: 1_073_741_824, max: 2_147_483_648 })).toStrictEqual({
      files: (1200).toLocaleString(),
      diskUsed: "1 GB / 2 GB",
    });
  });

  it("calls a zero limit unlimited", () => {
    expect(purgeSummary({ items: 0, size: 0, max: 0 })).toStrictEqual({ files: "0", diskUsed: "0 B / unlimited" });
  });
});
