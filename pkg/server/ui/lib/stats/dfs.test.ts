import { describe, expect, it } from "vitest";

import { ShapeError } from "#ui/lib/common/json";
import { dfsView, parseDFS } from "#ui/lib/stats/dfs";

const detail = {
  enabled: true,
  ready: true,
  type: "dfs",
  backend: "vfs",
  total_files: 3456,
  active_files: 4,
  cache_type: "vfs",
  cache_total_size: 1_610_612_736,
  cache_max_size: 2_147_483_648,
  cache_item_count: 120,
  cache_active_item_count: 9,
  cache_utilization: 0.75,
  cache_cache_hits: 900,
  cache_cache_misses: 100,
  cache_cache_hit_rate: 0.9,
  cache_active_downloads: 2,
  cache_total_downloaded: 5_368_709_120,
  cache_download_speed: 1_048_576,
  cache_circuit_breakers: 0,
  cache_buffer_memory_in_use: 1024,
  cache_buffer_memory_allocated: 4096,
};

const view = (overrides: Record<string, unknown> = {}, omit: readonly string[] = []): ReturnType<typeof dfsView> => {
  const fields = Object.entries({ ...detail, ...overrides }).filter(([key]) => !omit.includes(key));
  return dfsView(parseDFS(Object.fromEntries(fields)));
};

describe("the DFS current activity", () => {
  it("shows downloads, speed, hit rate and open files", () => {
    expect(view().texts).toMatchObject({
      "active-downloads": "2",
      "download-speed": "1 MB/s",
      "download-total": "Total: 5 GB",
      "hit-rate": "90.0%",
      "hit-counts": "900 hits / 100 misses",
      "open-files": "4",
      "opened-total": `Opened since startup: ${(3456).toLocaleString()}`,
      buffers:
        "Buffer allocations: 4 KB · In use: 1 KB. Allocations include blocks kept for reuse or waiting for release.",
    });
    expect([view().downloading, view({ cache_active_downloads: 0 }).downloading]).toStrictEqual([true, false]);
  });

  it.each([
    [{ cache_cache_hit_rate: 0.81 }, "text-success"],
    [{ cache_cache_hit_rate: 0.8 }, "text-warning"],
    [{ cache_cache_hit_rate: 0.5 }, "text-error"],
    [{ cache_cache_hit_rate: 0, cache_cache_hits: 0, cache_cache_misses: 0 }, "text-base-content/50"],
  ])("colours a hit rate of %j with %s", (overrides, want) => {
    expect(view(overrides).hitRateClass).toBe(want);
  });

  it("reads a missing hit rate as zero", () => {
    expect(view({ cache_cache_hits: 0, cache_cache_misses: 3 }, ["cache_cache_hit_rate"]).hitRateClass).toBe(
      "text-error",
    );
  });
});

describe("the DFS cache storage", () => {
  it("shows the cached files, disk use and loaded items", () => {
    expect(view().texts).toMatchObject({
      "cached-files": "120",
      "disk-used": "1.5 GB",
      "disk-share": "75.0% of 2 GB",
      "loaded-items": "9",
      "loaded-desc": "Resident in VFS memory",
    });
    expect(view().purge).toStrictEqual({ items: 120, size: 1_610_612_736, max: 2_147_483_648 });
  });

  it("derives the utilization from the sizes when the server leaves it out", () => {
    expect(view({}, ["cache_utilization"]).texts["disk-share"]).toBe("75.0% of 2 GB");
    expect(view({ cache_max_size: 0 }, ["cache_utilization"]).texts["disk-share"]).toBe("0.0% of 0 B");
  });

  it.each([
    [0.9, "text-warning"],
    [0.9004, "text-warning"],
    [0.9006, "text-error"],
    [0.75, "text-success"],
    [0.7504, "text-success"],
    [0.7506, "text-warning"],
  ])("colours a utilization of %d with %s", (utilization, want) => {
    expect(view({ cache_utilization: utilization }).usageClass).toBe(want);
  });

  it("defaults the cache type to VFS", () => {
    expect(view({}, ["cache_type"]).texts["loaded-desc"]).toBe("Resident in VFS memory");
  });
});

describe("the DFS cache health", () => {
  it("is OK without circuit breakers", () => {
    expect(view().health).toStrictEqual({
      className: "text-success",
      icon: "bi-shield-check",
      label: "OK",
      desc: "Cache reads normal",
      title: "Cache reads are operating normally.",
    });
  });

  it("asks for attention with paused files", () => {
    expect(view({ cache_circuit_breakers: 1 }).health).toStrictEqual({
      className: "text-warning",
      icon: "bi-shield-exclamation",
      label: "Attention",
      desc: "1 file temporarily paused",
      title:
        "Decypharr paused cache reads for these files after repeated provider errors. It will retry automatically.",
    });
    expect(view({ cache_circuit_breakers: 2 }).health.desc).toBe("2 files temporarily paused");
  });
});

describe("a DFS mount without details", () => {
  it("shows zeros", () => {
    expect(dfsView(parseDFS({})).texts).toMatchObject({ "active-downloads": "0", "hit-rate": "0.0%" });
  });

  it("rejects a malformed counter", () => {
    expect(() => parseDFS({ cache_item_count: "many" })).toThrow(ShapeError);
  });
});
