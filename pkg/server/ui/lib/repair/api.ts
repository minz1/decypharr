import { fetcher } from "#ui/lib/common/http";
import { asArray, asRecord, num, optBool, optNum, optStr, readJSON } from "#ui/lib/common/json";
import type {
  ActiveRun,
  BrokenFile,
  EntryHealth,
  RecheckMedia,
  RepairConfig,
  RepairRun,
  RepairStatus,
  RunOptions,
  RunStats,
} from "#ui/lib/repair/types";

type JSONRecord = Record<string, unknown>;

const listOrEmpty = (value: unknown, path: string): readonly unknown[] => (value === null ? [] : asArray(value, path));

const recordOrEmpty = (value: unknown, path: string): JSONRecord =>
  value === null || value === undefined ? {} : asRecord(value, path);

const text = (record: JSONRecord, key: string): string => optStr(record, key) ?? "";

const count = (record: JSONRecord, key: string): number => optNum(record, key) ?? 0;

const parseRepairConfig = (value: unknown): RepairConfig => {
  const config = recordOrEmpty(value, "repair config");
  return {
    autoRepair: optBool(config, "auto_repair") ?? false,
    verifyContent: optBool(config, "verify_content") ?? false,
  };
};

const parseStats = (value: unknown): RunStats => {
  const stats = recordOrEmpty(value, "stats");
  return {
    candidates: count(stats, "candidates"),
    skippedFresh: count(stats, "skipped_fresh"),
    probed: count(stats, "probed"),
    healthy: count(stats, "healthy"),
    broken: count(stats, "broken"),
    repaired: count(stats, "repaired"),
    cleared: count(stats, "cleared"),
    repairFailed: count(stats, "repair_failed"),
  };
};

const parseActiveRun = (value: unknown): ActiveRun | undefined => {
  if (value === null || value === undefined) {
    return undefined;
  }
  const run = asRecord(value, "active_run");
  return {
    id: text(run, "id"),
    stage: text(run, "stage"),
    startedAt: text(run, "started_at"),
    stats: parseStats(run["stats"]),
  };
};

const parseCounts = (value: unknown): Record<string, number> => {
  const counts = recordOrEmpty(value, "health_counts");
  return Object.fromEntries(Object.keys(counts).map((key) => [key, num(counts, key)]));
};

const parseRepairStatus = (value: unknown): RepairStatus => {
  const status = recordOrEmpty(value, "repair status");
  return {
    enabled: optBool(status, "enabled") ?? false,
    nextRunAt: optStr(status, "next_run_at"),
    activeRun: parseActiveRun(status["active_run"]),
    healthCounts: parseCounts(status["health_counts"]),
  };
};

const parseRun = (value: unknown): RepairRun => {
  const run = asRecord(value, "run");
  return {
    trigger: text(run, "trigger"),
    status: text(run, "status"),
    startedAt: text(run, "started_at"),
    completedAt: text(run, "completed_at"),
    stats: parseStats(run["stats"]),
    error: text(run, "error"),
  };
};

const parseRuns = (value: unknown): RepairRun[] => listOrEmpty(value, "runs").map((item) => parseRun(item));

const parseBrokenFile = (value: unknown): BrokenFile => {
  const file = asRecord(value, "broken file");
  return {
    fileName: text(file, "file_name"),
    reason: text(file, "reason"),
    size: count(file, "size"),
    arrName: text(file, "arr_name"),
    arrKind: text(file, "arr_kind"),
    mediaId: count(file, "media_id"),
    episodeId: count(file, "episode_id"),
    arrFileId: count(file, "arr_file_id"),
  };
};

const parseEntry = (value: unknown): EntryHealth => {
  const entry = asRecord(value, "entry");
  const brokenFiles = (
    entry["broken_files"] === undefined ? [] : listOrEmpty(entry["broken_files"], "broken_files")
  ).map((item) => parseBrokenFile(item));
  return {
    entryName: text(entry, "entry_name"),
    protocol: text(entry, "protocol"),
    fileCount: count(entry, "file_count"),
    brokenCount: optNum(entry, "broken_count") ?? brokenFiles.length,
    failureReason: text(entry, "failure_reason"),
    lastCheckedAt: text(entry, "last_checked_at"),
    lastFailedAt: text(entry, "last_failed_at"),
    lastRepairAt: text(entry, "last_repair_at"),
    brokenFiles,
  };
};

const parseEntries = (value: unknown): EntryHealth[] => listOrEmpty(value, "entries").map((item) => parseEntry(item));

const arrName = (value: unknown): string => {
  if (typeof value !== "object" || value === null) {
    return "";
  }
  return text(asRecord(value, "arr"), "name");
};

const parseArrNames = (value: unknown): string[] =>
  listOrEmpty(value, "arrs")
    .map((item) => arrName(item))
    .filter((name) => name !== "");

const postJSON = (body: unknown): RequestInit => ({
  method: "POST",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify(body),
});

const runRequest = (options: RunOptions): RequestInit =>
  postJSON({
    ignore_last_checked: options.ignoreLastChecked,
    auto_repair: options.autoRepair,
    unrestrict_link: true,
    verify_content: options.verifyContent,
    protocol: options.protocol,
  });

const fixAllRequest = (): RequestInit => postJSON({});

const fixOneRequest = (name: string): RequestInit => postJSON({ names: [name] });

const clearStateRequest = (statuses: readonly string[]): RequestInit => postJSON({ statuses });

const recheckMediaRequest = (recheck: RecheckMedia): RequestInit =>
  postJSON({ arr: recheck.arr, media_id: recheck.mediaId, fix: recheck.fix });

const getJSON = async (endpoint: string): Promise<unknown> => readJSON(await fetcher(endpoint));

const parseLoosely = (body: string): unknown => {
  try {
    return JSON.parse(body);
  } catch {
    return undefined;
  }
};

const looseField = (
  body: string,
  read: (record: JSONRecord) => string | number | undefined,
): string | number | undefined => {
  const data = parseLoosely(body);
  if (typeof data !== "object" || data === null || Array.isArray(data)) {
    return undefined;
  }
  try {
    return read(asRecord(data, "response"));
  } catch {
    return undefined;
  }
};

const plainFailure = (body: string, status: number): string => (body === "" ? `HTTP ${status}` : body);

const failureMessage = (body: string, status: number): string => {
  const reason = looseField(body, (record) => {
    const error = optStr(record, "error") ?? "";
    return error === "" ? optStr(record, "message") : error;
  });
  return typeof reason === "string" && reason !== "" ? reason : plainFailure(body, status);
};

const clearedMessage = (body: string): string => {
  const cleared = looseField(body, (record) => optNum(record, "cleared")) ?? 0;
  return `Cleared ${cleared} repair state entr${cleared === 1 ? "y" : "ies"}`;
};

export {
  clearStateRequest,
  clearedMessage,
  failureMessage,
  fixAllRequest,
  fixOneRequest,
  getJSON,
  parseArrNames,
  parseEntries,
  parseRepairConfig,
  parseRepairStatus,
  parseRuns,
  plainFailure,
  recheckMediaRequest,
  runRequest,
};
