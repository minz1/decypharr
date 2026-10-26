import type { Binding, Job, JobsPage, PageControl, StatusMeta } from "#ui/lib/reacquire/types";

const ACTIVE = new Set(["queued", "resolving", "invalidating", "blocklisting", "searching"]);
const WAITING = new Set(["waiting_for_grab", "waiting_for_download", "waiting_for_import"]);
const TERMINAL = new Set(["ready", "failed", "cancelled"]);

const STATUS: Readonly<Record<string, StatusMeta>> = {
  queued: { label: "Queued", badge: "badge-ghost", icon: "bi-hourglass" },
  resolving: { label: "Resolving", badge: "badge-info", icon: "bi-search" },
  invalidating: { label: "Deleting", badge: "badge-info", icon: "bi-trash3" },
  blocklisting: { label: "Blocklisting", badge: "badge-info", icon: "bi-slash-circle" },
  searching: { label: "Searching", badge: "badge-info", icon: "bi-binoculars" },
  waiting_for_grab: { label: "Waiting for grab", badge: "badge-warning", icon: "bi-clock" },
  waiting_for_download: { label: "Downloading", badge: "badge-warning", icon: "bi-download" },
  waiting_for_import: { label: "Waiting for import", badge: "badge-warning", icon: "bi-box-arrow-in-down" },
  needs_attention: { label: "Needs attention", badge: "badge-error", icon: "bi-exclamation-triangle" },
  ready: { label: "Ready", badge: "badge-success", icon: "bi-check-circle" },
  failed: { label: "Failed", badge: "badge-error", icon: "bi-x-circle" },
  cancelled: { label: "Cancelled", badge: "badge-ghost", icon: "bi-dash-circle" },
};

const COUNT_TILES = [
  { key: "active", label: "Working", tone: "text-info", icon: "bi-arrow-repeat" },
  { key: "waiting", label: "Waiting", tone: "text-warning", icon: "bi-clock" },
  { key: "ready", label: "Ready", tone: "text-success", icon: "bi-check-circle" },
  { key: "needs_attention", label: "Needs attention", tone: "text-error", icon: "bi-exclamation-triangle" },
  { key: "failed", label: "Failed", tone: "text-error", icon: "bi-x-circle" },
  { key: "cancelled", label: "Cancelled", tone: "opacity-60", icon: "bi-dash-circle" },
] as const;

const MS_PER_SECOND = 1000;
const SECONDS_PER_MINUTE = 60;
const SECONDS_PER_HOUR = 3600;
const SECONDS_PER_DAY = 86_400;
const EARLIEST_YEAR = 2000;
const PAGE_WINDOW = 2;
const DEFAULT_PAGE_SIZE = 10;

const isTerminal = (status: string): boolean => TERMINAL.has(status);

const isActive = (status: string): boolean => ACTIVE.has(status);

const isWorking = (job: Job): boolean => !isTerminal(job.status) && job.status !== "needs_attention";

const statusGroup = (status: string): string => {
  if (ACTIVE.has(status)) {
    return "active";
  }
  return WAITING.has(status) ? "waiting" : status;
};

const statusMeta = (status: string): StatusMeta =>
  (Object.hasOwn(STATUS, status) ? STATUS[status] : undefined) ?? {
    label: status,
    badge: "badge-ghost",
    icon: "bi-question",
  };

const jobCounts = (jobs: readonly Job[]): Record<string, number> => {
  const counts: Record<string, number> = Object.fromEntries(COUNT_TILES.map((tile) => [tile.key, 0]));
  for (const job of jobs) {
    const group = statusGroup(job.status);
    if (Object.hasOwn(counts, group)) {
      counts[group] = (counts[group] ?? 0) + 1;
    }
  }
  return counts;
};

const statusLine = (jobs: readonly Job[]): string => {
  const counts = jobCounts(jobs);
  const working = (counts["active"] ?? 0) + (counts["waiting"] ?? 0);
  if (working > 0) {
    return `${working} job${working === 1 ? "" : "s"} in progress.`;
  }
  return jobs.length > 0 ? "No job is running." : "No reacquisitions yet.";
};

const arrKind = (name: string, type: string): boolean => type !== "" && name.toLowerCase() !== type.toLowerCase();

const validDate = (value: string): Date | undefined => {
  const date = new Date(value);
  return value === "" || Number.isNaN(date.getTime()) || date.getFullYear() < EARLIEST_YEAR ? undefined : date;
};

const relativeTime = (seconds: number): string | undefined => {
  if (seconds < SECONDS_PER_MINUTE) {
    return "just now";
  }
  if (seconds < SECONDS_PER_HOUR) {
    return `${Math.floor(seconds / SECONDS_PER_MINUTE)}m ago`;
  }
  return seconds < SECONDS_PER_DAY ? `${Math.floor(seconds / SECONDS_PER_HOUR)}h ago` : undefined;
};

const formatTime = (value: string, now: number): string => {
  const date = validDate(value);
  if (date === undefined) {
    return "-";
  }
  return relativeTime(Math.floor((now - date.getTime()) / MS_PER_SECOND)) ?? date.toLocaleString();
};

const filterJobs = (jobs: readonly Job[], filter: string): Job[] =>
  filter === "" ? [...jobs] : jobs.filter((job) => statusGroup(job.status) === filter);

const jobsPage = (filtered: readonly Job[], page: number, pageSize: number): JobsPage => {
  const totalPages = Math.max(1, Math.ceil(filtered.length / pageSize));
  const current = Math.min(Math.max(1, page), totalPages);
  const offset = (current - 1) * pageSize;
  return { jobs: filtered.slice(offset, offset + pageSize), total: filtered.length, totalPages, offset, page: current };
};

const pageNumbers = (page: number, totalPages: number): number[] => {
  const numbers = new Set([1, totalPages]);
  for (let number = page - PAGE_WINDOW; number <= page + PAGE_WINDOW; number += 1) {
    if (number > 0 && number <= totalPages) {
      numbers.add(number);
    }
  }
  return [...numbers].toSorted((left, right) => left - right);
};

const pageControls = (page: number, totalPages: number): PageControl[] => {
  const controls: PageControl[] = [{ kind: "previous", page: page - 1, current: false, disabled: page === 1 }];
  let previous = 0;
  for (const number of pageNumbers(page, totalPages)) {
    if (previous > 0 && number > previous + 1) {
      controls.push({ kind: "gap", page: 0, current: false, disabled: true });
    }
    controls.push({ kind: "page", page: number, current: number === page, disabled: false });
    previous = number;
  }
  controls.push({ kind: "next", page: page + 1, current: false, disabled: page === totalPages });
  return controls;
};

const bindingMedia = (binding: Binding): string =>
  binding.movieId === 0
    ? `series ${binding.seriesId === 0 ? "-" : binding.seriesId} · season ${binding.seasonNumber ?? "-"}`
    : `movie ${binding.movieId}`;

const jobFile = (job: Job): string => {
  const fileName = job.bindings[0]?.entryFileName ?? "";
  if (fileName !== "") {
    return fileName;
  }
  return job.fileId === "" ? "-" : job.fileId;
};

export {
  COUNT_TILES,
  DEFAULT_PAGE_SIZE,
  arrKind,
  bindingMedia,
  filterJobs,
  formatTime,
  isActive,
  isTerminal,
  isWorking,
  jobCounts,
  jobFile,
  jobsPage,
  pageControls,
  statusLine,
  statusMeta,
};
