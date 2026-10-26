import type {
  BrowseLocation,
  BrowsePage,
  BrowseState,
  Crumb,
  Entry,
  Health,
  HealthBadge,
  SortKey,
  SortOrder,
} from "#ui/lib/browse/types";
import { asRecord, optBool, optList, optNum, optStr, str } from "#ui/lib/common/json";

const PER_PAGE = 20;
const GO_ZERO_TIME = "0001-01-01T00:00:00Z";
const MAX_API_LEVELS = 3;
const DOWNLOAD_LEVELS = 3;
const SORT_KEYS: readonly SortKey[] = ["name", "size", "mod_time", "active_debrid"];
const DESCENDING_FIRST: ReadonlySet<string> = new Set(["size", "mod_time"]);
const FOLDER_KINDS: ReadonlySet<string> = new Set(["system", "provider", "virtual"]);
const FOLDER_AND_FILE = 2;

const HEALTH_CLASSES: Readonly<Record<string, string>> = {
  healthy: "badge-success",
  broken: "badge-error",
  repairing: "badge-info",
  stale: "badge-warning",
  unsupported: "badge-ghost",
  unknown: "badge-ghost",
};

const ENTRY_ICONS: Readonly<Record<string, readonly string[]>> = {
  virtual: ["bi-folder-symlink", "text-info", "text-lg"],
  provider: ["bi-cloud", "text-secondary", "text-lg"],
  system: ["bi-folder-fill", "text-warning", "text-lg"],
  entry: ["bi-folder-fill", "text-warning", "text-lg"],
};
const FILE_ICON: readonly string[] = ["bi-file-earmark", "text-info"];

const KIND_BADGES: Readonly<Record<string, { classes: readonly string[]; text: string }>> = {
  virtual: { classes: ["badge-info", "badge-outline"], text: "Virtual view" },
  provider: { classes: ["badge-secondary", "badge-outline"], text: "Provider" },
  system: { classes: ["badge-ghost"], text: "Library" },
};

const parseEntry = (value: unknown): Entry => {
  const record = asRecord(value, "entry");
  return {
    name: str(record, "name"),
    path: str(record, "path"),
    size: optNum(record, "size") ?? 0,
    modTime: optStr(record, "mod_time") ?? "",
    isDir: optBool(record, "is_dir") ?? false,
    infoHash: optStr(record, "info_hash") ?? "",
    canDelete: optBool(record, "can_delete") ?? false,
    activeDebrid: optStr(record, "active_debrid") ?? "",
    kind: optStr(record, "kind") ?? "",
  };
};

const parseBrowsePage = (value: unknown): BrowsePage => {
  const record = asRecord(value, "browse response");
  return {
    entries: optList(record, "entries").map((item) => parseEntry(item)),
    total: optNum(record, "total") ?? 0,
    totalPages: optNum(record, "total_pages") ?? 0,
    currentKind: optStr(record, "current_kind") ?? "",
  };
};

const parseHealth = (value: unknown): Health => {
  const record = asRecord(value, "health response");
  return { status: optStr(record, "status") ?? "", lastCheckedAt: optStr(record, "last_checked_at") ?? "" };
};

const isSortKey = (value: string | null): value is SortKey => SORT_KEYS.some((key) => key === value);

const positive = (value: string | null, fallback: number): number => {
  const parsed = Math.trunc(Number(value ?? ""));
  return parsed > 0 ? parsed : fallback;
};

const nonEmpty = (value: string | null, fallback: string): string =>
  value === null || value === "" ? fallback : value;

const readBrowseLocation = (search: string): BrowseLocation => {
  const params = new URLSearchParams(search);
  const sortBy = params.get("sort_by");
  return {
    path: nonEmpty(params.get("path"), "/"),
    page: positive(params.get("page"), 1),
    search: params.get("search") ?? "",
    perPage: positive(params.get("limit"), PER_PAGE),
    sortBy: isSortKey(sortBy) ? sortBy : "name",
    sortOrder: params.get("sort_order") === "desc" ? "desc" : "asc",
  };
};

const createBrowseState = (): BrowseState => ({
  ...readBrowseLocation(""),
  entries: [],
  total: 0,
  totalPages: 0,
  currentKind: "",
  selected: new Map(),
  health: new Map(),
});

const browseQuery = (state: BrowseLocation): string => {
  const params = new URLSearchParams();
  for (const [key, value, unset] of [
    ["path", state.path, "/"],
    ["page", String(state.page), "1"],
    ["search", state.search, ""],
    ["limit", String(state.perPage), String(PER_PAGE)],
    ["sort_by", state.sortBy, "name"],
    ["sort_order", state.sortOrder, "asc"],
  ] as const) {
    if (value !== unset) {
      params.set(key, value);
    }
  }
  const query = params.toString();
  return query === "" ? "" : `?${query}`;
};

const pathParts = (path: string): string[] => path.split("/").filter((part) => part !== "");

const browseAPIURL = (state: BrowseLocation): string => {
  const parts = pathParts(state.path);
  const levels = parts.length > MAX_API_LEVELS ? "" : `/${parts.map((part) => encodeURIComponent(part)).join("/")}`;
  const params = new URLSearchParams({
    page: String(state.page),
    limit: String(state.perPage),
    sort_by: state.sortBy,
    sort_order: state.sortOrder,
  });
  if (state.search !== "") {
    params.set("search", state.search);
  }
  return `/api/browse${levels}?${params.toString()}`;
};

const nextSort = (state: BrowseLocation, key: string): Pick<BrowseLocation, "sortBy" | "sortOrder"> | undefined => {
  if (!isSortKey(key)) {
    return undefined;
  }
  if (state.sortBy === key) {
    return { sortBy: key, sortOrder: state.sortOrder === "asc" ? "desc" : "asc" };
  }
  const sortOrder: SortOrder = DESCENDING_FIRST.has(key) ? "desc" : "asc";
  return { sortBy: key, sortOrder };
};

const sortIndicator = (state: BrowseLocation, key: SortKey): string => {
  if (state.sortBy !== key) {
    return "bi bi-arrow-down-up text-xs";
  }
  return state.sortOrder === "asc" ? "bi bi-sort-up text-xs" : "bi bi-sort-down text-xs";
};

const isActionable = (entry: Entry): boolean =>
  !FOLDER_KINDS.has(entry.kind) && (entry.kind === "entry" || entry.kind === "file" || entry.canDelete || !entry.isDir);

const entryId = (entry: Entry): string => (entry.infoHash === "" ? entry.path : entry.infoHash);

const downloadURL = (path: string): string | undefined => {
  const parts = pathParts(path);
  if (parts.length < DOWNLOAD_LEVELS) {
    return undefined;
  }
  const [folder = "", file = ""] = parts.slice(parts.length - FOLDER_AND_FILE);
  return `/api/browse/download/${encodeURIComponent(folder)}/${encodeURIComponent(file)}`;
};

const healthBadge = (health?: Health): HealthBadge => {
  if (health === undefined) {
    return { className: "badge-ghost", text: "unknown", title: undefined };
  }
  return {
    className: HEALTH_CLASSES[health.status] ?? "badge-ghost",
    text: health.status === "" ? "unknown" : health.status,
    title:
      health.lastCheckedAt === "" || health.lastCheckedAt === GO_ZERO_TIME
        ? "never checked"
        : `last checked ${new Date(health.lastCheckedAt).toLocaleString()}`,
  };
};

const entryIcon = (entry: Entry): readonly string[] =>
  entry.isDir ? (ENTRY_ICONS[entry.kind] ?? ENTRY_ICONS["entry"] ?? []) : FILE_ICON;

const kindBadge = (kind: string): { classes: readonly string[]; text: string } | undefined => KIND_BADGES[kind];

const breadcrumbs = (path: string): Crumb[] => {
  let current = "";
  return pathParts(path).map((part) => {
    current += `/${part}`;
    return { path: current, label: decodeURIComponent(part) };
  });
};

export {
  breadcrumbs,
  browseAPIURL,
  browseQuery,
  createBrowseState,
  downloadURL,
  entryIcon,
  entryId,
  healthBadge,
  isActionable,
  kindBadge,
  nextSort,
  parseBrowsePage,
  parseHealth,
  readBrowseLocation,
  sortIndicator,
};
