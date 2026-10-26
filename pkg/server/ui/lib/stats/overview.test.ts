import { describe, expect, it } from "vitest";

import { ShapeError } from "#ui/lib/common/json";
import { hearsayBadge, overviewTexts, parseOverview } from "#ui/lib/stats/overview";

const system = {
  memory_used: "40.00MB",
  process_rss_mb: "120.50MB",
  heap_alloc_mb: "12.00MB",
  heap_inuse_mb: "14.00MB",
  heap_released_mb: "3.00MB",
  sys_mb: "43.00MB",
  gc_cycles: 1234,
  goroutines: 56,
  num_cpu: 8,
  os: "linux",
  arch: "amd64",
  go_version: "go1.27.3",
  uptime_seconds: 3725,
  uptime: "1h2m5s",
  start_time: "2026-10-26 06:00:00",
};

const hearsayOn = {
  enabled: true,
  protocol: "1",
  participate: true,
  publish: true,
  advice_mode: "active",
  identity: "ed25519:abcd",
  feeds: { movies: 3, tv: 2 },
  metrics: {},
  advice: {
    realdebrid: { evaluations: 10, positive: 3, negative: 1, verified: 4, correct: 3 },
    torbox: { evaluations: 10, positive: 2, negative: 0, verified: 0, correct: 0 },
  },
  transport: {
    torrents: [
      { infohash: "a", active_peers: 2 },
      { infohash: "b", active_peers: 5 },
    ],
  },
};

const snapshot = (overrides: Record<string, unknown> = {}): Record<string, unknown> => ({
  system,
  storage: { db_size: 1536, total_entries: 12_345 },
  queue: { pending: 7, active: 1 },
  repair: { enabled: true, active: false },
  hearsay: { enabled: false, participate: false, publish: false, metrics: {} },
  ...overrides,
});

const texts = (overrides: Record<string, unknown> = {}): Record<string, string> => {
  const overview = parseOverview(snapshot(overrides));
  return Object.fromEntries(overviewTexts(overview));
};

const parsedValue = (json: string): unknown => JSON.parse(json);

describe("the system overview", () => {
  it("shows the process memory and the runtime figures", () => {
    expect(texts()).toMatchObject({
      uptime: "1h 2m 5s",
      "start-time": "Started: 2026-10-26 06:00:00",
      "memory-label": "Process Memory (RSS)",
      "memory-used": "120.50MB",
      "heap-alloc": "Heap: 12.00MB · Released: 3.00MB",
      "runtime-memory": "Go runtime: 40.00MB",
      goroutines: "56",
      "gc-cycles": `GC: ${(1234).toLocaleString()} cycles`,
      "num-cpu": "8",
      arch: "amd64",
      os: "linux",
      "go-version": "go1.27.3",
    });
  });

  it("falls back to the Go runtime memory and dashes when fields are missing", () => {
    expect(texts({ system: { memory_used: "40.00MB" } })).toMatchObject({
      uptime: "-",
      "start-time": "Started: -",
      "memory-label": "Go Runtime Memory",
      "memory-used": "40.00MB",
      "heap-alloc": "Heap: - · Released: -",
      goroutines: "0",
      "gc-cycles": "GC: 0 cycles",
      "num-cpu": "-",
      arch: "-",
      os: "-",
      "go-version": "-",
    });
  });

  it("shows a dash for memory when the system section is missing", () => {
    expect(texts({ system: undefined })).toMatchObject({ "memory-used": "-", "runtime-memory": "Go runtime: -" });
  });
});

describe("the library cards", () => {
  it("shows storage, queue and repair counts", () => {
    expect(texts()).toMatchObject({
      "db-size": "1.5 KB",
      "total-entries": (12_345).toLocaleString(),
      "queue-pending": "7",
      "repair-active": "0",
      "repair-pending": "0",
      "repair-completed": "0",
      "repair-failed": "0",
    });
  });

  it("reads repair job counts when the server sends them", () => {
    expect(texts({ repair: { active_jobs: 1, pending_jobs: 2, completed_jobs: 3, failed_jobs: 4 } })).toMatchObject({
      "repair-active": "1",
      "repair-pending": "2",
      "repair-completed": "3",
      "repair-failed": "4",
    });
  });

  it("leaves a card alone when its section is missing", () => {
    const shown = texts({ storage: parsedValue("null"), queue: undefined, repair: undefined });
    expect(["db-size", "total-entries", "queue-pending", "repair-active"].filter((id) => id in shown)).toStrictEqual(
      [],
    );
  });
});

describe("the hearsay card", () => {
  it("shows dashes when hearsay is off", () => {
    expect(texts()).toMatchObject({
      "hearsay-feeds": "-",
      "hearsay-peers": "-",
      "hearsay-coverage": "-",
      "hearsay-accuracy": "-",
      "hearsay-identity": "",
    });
  });

  it("sums feeds and peers and derives coverage and accuracy", () => {
    expect(texts({ hearsay: hearsayOn })).toMatchObject({
      "hearsay-feeds": "5",
      "hearsay-peers": "7",
      "hearsay-coverage": "30.0%",
      "hearsay-accuracy": "75.0%",
      "hearsay-identity": "ed25519:abcd",
    });
  });

  it.each([
    [{ enabled: false }, "Off", "badge badge-ghost badge-sm ml-auto"],
    [{ enabled: true, participate: false }, "Local · shadow", "badge badge-ghost badge-sm ml-auto"],
    [
      { enabled: true, participate: true, publish: true, advice_mode: "active" },
      "Sharing · active",
      "badge badge-success badge-sm ml-auto",
    ],
    [{ enabled: true, participate: true, publish: false }, "Receive · shadow", "badge badge-info badge-sm ml-auto"],
  ])("labels %j as %j", (hearsay, text, className) => {
    const overview = parseOverview(snapshot({ hearsay }));
    expect(hearsayBadge(overview.hearsay)).toStrictEqual({ text, className });
  });
});

describe("a malformed overview", () => {
  it("throws a shape error", () => {
    expect(() => parseOverview(snapshot({ system: { goroutines: "many" } }))).toThrow(ShapeError);
    expect(() => parseOverview(snapshot({ hearsay: { enabled: true, feeds: { tv: "x" } } }))).toThrow(ShapeError);
  });
});
