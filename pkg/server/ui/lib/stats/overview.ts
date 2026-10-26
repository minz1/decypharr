import { byId } from "#ui/lib/common/dom";
import { formatBytes, orElse } from "#ui/lib/common/format";
import { asRecord, num, numOrZero, optBool, strOrEmpty } from "#ui/lib/common/json";
import { formatNumber, formatPercent, formatUptime } from "#ui/lib/stats/format";
import { listOrEmpty, optRecord, recordOrEmpty } from "#ui/lib/stats/read";

interface SystemStats {
  uptimeSeconds: number;
  startTime: string;
  processRSS: string;
  memoryUsed: string;
  heapAlloc: string;
  heapReleased: string;
  goroutines: number;
  gcCycles: number;
  numCPU: number;
  arch: string;
  os: string;
  goVersion: string;
}

interface StorageStats {
  dbSize: number;
  totalEntries: number;
}

interface RepairStats {
  active: number;
  pending: number;
  completed: number;
  failed: number;
}

interface HearsayStats {
  enabled: boolean;
  participate: boolean;
  publish: boolean;
  adviceMode: string;
  identity: string;
  feeds: number;
  peers: number;
  evaluations: number;
  known: number;
  verified: number;
  correct: number;
}

interface Overview {
  system: SystemStats;
  storage: StorageStats | undefined;
  queuePending: number | undefined;
  repair: RepairStats | undefined;
  hearsay: HearsayStats;
}

interface Badge {
  text: string;
  className: string;
}

const OVERVIEW_IDS = [
  "uptime",
  "start-time",
  "memory-label",
  "memory-used",
  "heap-alloc",
  "runtime-memory",
  "goroutines",
  "gc-cycles",
  "num-cpu",
  "arch",
  "os",
  "go-version",
  "db-size",
  "total-entries",
  "queue-pending",
  "repair-active",
  "repair-pending",
  "repair-completed",
  "repair-failed",
  "hearsay-feeds",
  "hearsay-peers",
  "hearsay-coverage",
  "hearsay-accuracy",
  "hearsay-identity",
  "hearsay-state",
] as const;

type OverviewId = (typeof OVERVIEW_IDS)[number];

type Entry = readonly [OverviewId, string];

const parseSystem = (record: Record<string, unknown>): SystemStats => ({
  uptimeSeconds: numOrZero(record, "uptime_seconds"),
  startTime: strOrEmpty(record, "start_time"),
  processRSS: strOrEmpty(record, "process_rss_mb"),
  memoryUsed: strOrEmpty(record, "memory_used"),
  heapAlloc: strOrEmpty(record, "heap_alloc_mb"),
  heapReleased: strOrEmpty(record, "heap_released_mb"),
  goroutines: numOrZero(record, "goroutines"),
  gcCycles: numOrZero(record, "gc_cycles"),
  numCPU: numOrZero(record, "num_cpu"),
  arch: strOrEmpty(record, "arch"),
  os: strOrEmpty(record, "os"),
  goVersion: strOrEmpty(record, "go_version"),
});

const parseStorage = (record: Record<string, unknown> | undefined): StorageStats | undefined =>
  record && { dbSize: numOrZero(record, "db_size"), totalEntries: numOrZero(record, "total_entries") };

const parseRepair = (record: Record<string, unknown> | undefined): RepairStats | undefined =>
  record && {
    active: numOrZero(record, "active_jobs"),
    pending: numOrZero(record, "pending_jobs"),
    completed: numOrZero(record, "completed_jobs"),
    failed: numOrZero(record, "failed_jobs"),
  };

const sumValues = (record: Record<string, unknown>): number => {
  let total = 0;
  for (const key of Object.keys(record)) {
    total += num(record, key);
  }
  return total;
};

const sumField = (items: readonly unknown[], path: string, keys: readonly string[]): number => {
  let total = 0;
  for (const item of items) {
    const record = asRecord(item, path);
    for (const key of keys) {
      total += numOrZero(record, key);
    }
  }
  return total;
};

const parseHearsay = (record: Record<string, unknown>): HearsayStats => {
  const advice = Object.values(recordOrEmpty(record, "advice"));
  const torrents = listOrEmpty(recordOrEmpty(record, "transport"), "torrents");
  return {
    enabled: optBool(record, "enabled") ?? false,
    participate: optBool(record, "participate") ?? false,
    publish: optBool(record, "publish") ?? false,
    adviceMode: strOrEmpty(record, "advice_mode"),
    identity: strOrEmpty(record, "identity"),
    feeds: sumValues(recordOrEmpty(record, "feeds")),
    peers: sumField(torrents, "torrents", ["active_peers"]),
    evaluations: sumField(advice, "advice", ["evaluations"]),
    known: sumField(advice, "advice", ["positive", "negative"]),
    verified: sumField(advice, "advice", ["verified"]),
    correct: sumField(advice, "advice", ["correct"]),
  };
};

const parseOverview = (stats: Record<string, unknown>): Overview => {
  const queue = optRecord(stats, "queue");
  return {
    system: parseSystem(recordOrEmpty(stats, "system")),
    storage: parseStorage(optRecord(stats, "storage")),
    queuePending: queue && numOrZero(queue, "pending"),
    repair: parseRepair(optRecord(stats, "repair")),
    hearsay: parseHearsay(recordOrEmpty(stats, "hearsay")),
  };
};

const systemTexts = (sys: SystemStats): Entry[] => [
  ["uptime", formatUptime(sys.uptimeSeconds)],
  ["start-time", `Started: ${orElse(sys.startTime, "-")}`],
  ["memory-label", sys.processRSS === "" ? "Go Runtime Memory" : "Process Memory (RSS)"],
  ["memory-used", orElse(orElse(sys.processRSS, sys.memoryUsed), "-")],
  ["heap-alloc", `Heap: ${orElse(sys.heapAlloc, "-")} · Released: ${orElse(sys.heapReleased, "-")}`],
  ["runtime-memory", `Go runtime: ${orElse(sys.memoryUsed, "-")}`],
  ["goroutines", formatNumber(sys.goroutines)],
  ["gc-cycles", `GC: ${formatNumber(sys.gcCycles)} cycles`],
  ["num-cpu", sys.numCPU ? String(sys.numCPU) : "-"],
  ["arch", orElse(sys.arch, "-")],
  ["os", orElse(sys.os, "-")],
  ["go-version", orElse(sys.goVersion, "-")],
];

const libraryTexts = (overview: Overview): Entry[] => [
  ...(overview.storage
    ? ([
        ["db-size", formatBytes(overview.storage.dbSize)],
        ["total-entries", formatNumber(overview.storage.totalEntries)],
      ] satisfies Entry[])
    : []),
  ...(overview.queuePending === undefined
    ? []
    : ([["queue-pending", formatNumber(overview.queuePending)]] satisfies Entry[])),
  ...(overview.repair
    ? ([
        ["repair-active", formatNumber(overview.repair.active)],
        ["repair-pending", formatNumber(overview.repair.pending)],
        ["repair-completed", formatNumber(overview.repair.completed)],
        ["repair-failed", formatNumber(overview.repair.failed)],
      ] satisfies Entry[])
    : []),
];

const ratio = (part: number, whole: number): string => (whole ? formatPercent(part / whole) : "-");

const hearsayTexts = (hearsay: HearsayStats): Entry[] => [
  ["hearsay-feeds", hearsay.enabled ? formatNumber(hearsay.feeds) : "-"],
  ["hearsay-peers", hearsay.enabled ? formatNumber(hearsay.peers) : "-"],
  ["hearsay-coverage", ratio(hearsay.known, hearsay.evaluations)],
  ["hearsay-accuracy", ratio(hearsay.correct, hearsay.verified)],
  ["hearsay-identity", hearsay.identity],
];

const overviewTexts = (overview: Overview): Map<OverviewId, string> =>
  new Map([...systemTexts(overview.system), ...libraryTexts(overview), ...hearsayTexts(overview.hearsay)]);

const hearsayBadge = (hearsay: HearsayStats): Badge => {
  const mode = orElse(hearsay.adviceMode, "shadow");
  if (!hearsay.enabled) {
    return { text: "Off", className: "badge badge-ghost badge-sm ml-auto" };
  }
  if (!hearsay.participate) {
    return { text: `Local · ${mode}`, className: "badge badge-ghost badge-sm ml-auto" };
  }
  return hearsay.publish
    ? { text: `Sharing · ${mode}`, className: "badge badge-success badge-sm ml-auto" }
    : { text: `Receive · ${mode}`, className: "badge badge-info badge-sm ml-auto" };
};

const findOverview = (doc: Document): ReadonlyMap<string, HTMLElement> =>
  new Map(OVERVIEW_IDS.map((id) => [id, byId(id, HTMLElement, doc)]));

const renderOverview = (fields: ReadonlyMap<string, HTMLElement>, overview: Overview): void => {
  for (const [id, value] of overviewTexts(overview)) {
    const element = fields.get(id);
    if (element) {
      element.textContent = value;
    }
  }
  const state = fields.get("hearsay-state");
  if (state) {
    const badge = hearsayBadge(overview.hearsay);
    state.textContent = badge.text;
    state.className = badge.className;
  }
};

export { findOverview, hearsayBadge, overviewTexts, parseOverview, renderOverview };
