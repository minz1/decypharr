import { describe, expect, it } from "vitest";

import { ShapeError } from "#ui/lib/common/json";
import { parseRclone, rcloneView } from "#ui/lib/stats/rclone";

const detail = {
  ready: true,
  enabled: true,
  type: "rclone",
  version: { arch: "amd64", version: "v1.71.0", os: "linux" },
  core: {
    bytes: 1_073_741_824,
    checks: 3,
    elapsedTime: 3661,
    errors: 1,
    eta: 0,
    speed: 2048,
    totalBytes: 0,
    totalChecks: 9,
    totalTransfers: 0,
    transferTime: 65,
    transfers: 4,
    transferring: [
      { bytes: 512, eta: 90, name: "Show.S01E01.mkv", speed: 1024, size: 2048, progress: 25 },
      { bytes: 0, eta: 0, name: "", speed: 0, size: 0, progress: 0 },
    ],
  },
  memory: { Sys: 52_428_800, TotalAlloc: 10_485_760 },
  bandwidth: { bytesPerSecond: 1_048_576, rate: "1048576" },
};

const view = (overrides: Record<string, unknown> = {}): ReturnType<typeof rcloneView> =>
  rcloneView(parseRclone({ ...detail, ...overrides }));

describe("the rclone stats", () => {
  it("shows the version, transfer counters and memory", () => {
    expect(view()).toMatchObject({
      version: { version: "v1.71.0", platform: "amd64 linux" },
      core: {
        transferred: "1 GB",
        speed: "Speed: 2 KB/s",
        transfers: "4",
        errors: "Errors: 1",
        checks: "3",
        "total-checks": "Total: 9",
        elapsed: "1h 1m",
        "transfer-time": "Transfer: 1m 5s",
      },
      memory: { sys: "50 MB", heap: "Heap: 10 MB" },
    });
  });

  it("shows Unknown for an rclone that reported no version", () => {
    expect(view({ version: { arch: "", version: "", os: "" } }).version).toStrictEqual({
      version: "Unknown",
      platform: " ",
    });
  });
});

describe("the rclone transfers", () => {
  it("lists active transfers with progress and ETA", () => {
    expect(view().transfers).toStrictEqual([
      {
        name: "Show.S01E01.mkv",
        speed: "1 KB/s",
        progress: 25,
        percent: "25.0%",
        sizes: "512 B / 2 KB",
        eta: "ETA: 1m 30s",
      },
      { name: "Unknown", speed: "0 B/s", progress: 0, percent: "0.0%", sizes: "0 B / 0 B", eta: "ETA: Unknown" },
    ]);
  });

  it("measures a transfer of unknown size against one byte", () => {
    const stats = parseRclone({ core: { transferring: [{ name: "a", bytes: 5, size: 0 }] } });
    expect(rcloneView(stats).transfers[0]).toMatchObject({ progress: 500, percent: "500.0%" });
  });
});

describe("the rclone bandwidth and missing sections", () => {
  it("shows bandwidth limits unless the rate is off", () => {
    expect(view().bandwidth).toStrictEqual({ bytesPerSecond: "1 MB", rate: "1 MB" });
    expect(view({ bandwidth: { bytesPerSecond: 0, rate: "off" } }).bandwidth).toBeUndefined();
    expect(view({ bandwidth: { bytesPerSecond: 0, rate: "" } }).bandwidth).toStrictEqual({
      bytesPerSecond: "0 B",
      rate: "0 B",
    });
  });

  it("leaves out the sections rclone did not report", () => {
    expect(rcloneView(parseRclone({}))).toStrictEqual({
      version: undefined,
      core: undefined,
      memory: undefined,
      transfers: [],
      bandwidth: undefined,
    });
  });

  it("rejects a malformed transfer", () => {
    expect(() => parseRclone({ core: { transferring: [{ name: 3 }] } })).toThrow(ShapeError);
  });
});
