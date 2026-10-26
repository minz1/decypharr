import type { BrokenFile, EntryHealth, PageButton } from "#ui/lib/repair/types";

const HEALTH_COLORS: Readonly<Record<string, string>> = {
  healthy: "text-success",
  broken: "text-error",
  repairing: "text-info",
  stale: "text-warning",
  unsupported: "text-base-content/60",
};

const RUN_BADGES: Readonly<Record<string, string>> = {
  running: "badge-info",
  completed: "badge-success",
  failed: "badge-error",
  cancelled: "badge-warning",
};

const SIZE_UNITS = ["B", "KB", "MB", "GB", "TB"] as const;
const KIBI = 1024;
const WHOLE_SIZE_FROM = 10;
const MS_PER_SECOND = 1000;
const SECONDS_PER_MINUTE = 60;
const PAGE_WINDOW = 2;
const GAP_DISTANCE = 3;

const lookup = (table: Readonly<Record<string, string>>, key: string, fallback: string): string =>
  Object.hasOwn(table, key) ? (table[key] ?? fallback) : fallback;

const healthColor = (status: string): string => lookup(HEALTH_COLORS, status, "");

const runBadgeClass = (status: string): string => lookup(RUN_BADGES, status, "badge-ghost");

const formatSeconds = (ms: number): string => {
  if (Number.isNaN(ms) || ms <= 0) {
    return "-";
  }
  const seconds = Math.round(ms / MS_PER_SECOND);
  if (seconds < SECONDS_PER_MINUTE) {
    return `${seconds}s`;
  }
  return `${Math.floor(seconds / SECONDS_PER_MINUTE)}m ${seconds % SECONDS_PER_MINUTE}s`;
};

const formatRunDuration = (startedAt: string, completedAt: string): string => {
  if (startedAt === "") {
    return "-";
  }
  if (completedAt === "") {
    return "running";
  }
  return formatSeconds(new Date(completedAt).getTime() - new Date(startedAt).getTime());
};

const formatFileSize = (size: number): string => {
  if (size <= 0) {
    return "-";
  }
  let unit = 0;
  let value = size;
  while (value >= KIBI && unit < SIZE_UNITS.length - 1) {
    value /= KIBI;
    unit += 1;
  }
  return `${value.toFixed(value >= WHOLE_SIZE_FROM || unit === 0 ? 0 : 1)} ${SIZE_UNITS[unit] ?? "B"}`;
};

const formatTimestamp = (value: string, fallback: string): string =>
  value === "" ? fallback : new Date(value).toLocaleString();

const brokenFileIds = (file: Pick<BrokenFile, "arrFileId" | "episodeId" | "mediaId">): string =>
  [
    file.mediaId === 0 ? "" : `media:${file.mediaId}`,
    file.episodeId === 0 ? "" : `ep:${file.episodeId}`,
    file.arrFileId === 0 ? "" : `file:${file.arrFileId}`,
  ]
    .filter((part) => part !== "")
    .join(" · ");

const failedTime = (entry: EntryHealth): number =>
  entry.lastFailedAt === "" ? 0 : new Date(entry.lastFailedAt).getTime();

const sortBroken = (entries: readonly EntryHealth[]): EntryHealth[] =>
  entries.toSorted((left, right) => {
    const difference = failedTime(right) - failedTime(left);
    return difference === 0 ? left.entryName.localeCompare(right.entryName) : difference;
  });

const pageCount = (items: number, pageSize: number): number => Math.max(1, Math.ceil(items / pageSize));

const clampPage = (page: number, items: number, pageSize: number): number =>
  Math.max(1, Math.min(page, pageCount(items, pageSize)));

const numberButton = (number: number, page: number, totalPages: number): PageButton | undefined => {
  if (number === 1 || number === totalPages || Math.abs(number - page) <= PAGE_WINDOW) {
    return { label: String(number), target: number, active: number === page, disabled: false };
  }
  if (Math.abs(number - page) === GAP_DISTANCE) {
    return { label: "…", target: undefined, active: false, disabled: true };
  }
  return undefined;
};

const pageButtons = (page: number, totalPages: number): PageButton[] => [
  { label: "«", target: page - 1, active: false, disabled: page === 1 },
  ...Array.from({ length: totalPages }, (_unused, index) => numberButton(index + 1, page, totalPages)).filter(
    (button) => button !== undefined,
  ),
  { label: "»", target: page + 1, active: false, disabled: page === totalPages },
];

export {
  brokenFileIds,
  clampPage,
  formatFileSize,
  formatRunDuration,
  formatTimestamp,
  healthColor,
  pageButtons,
  pageCount,
  runBadgeClass,
  sortBroken,
};
