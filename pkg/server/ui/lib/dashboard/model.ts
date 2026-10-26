import { formatSpeed } from "#ui/lib/common/format";
import { asRecord, num, optList, optNum, optStr, str } from "#ui/lib/common/json";
import type { Badge, DashboardState, ProtocolBadge, Torrent, TorrentPage } from "#ui/lib/dashboard/types";

const PER_PAGE = 20;
const PERCENT = 100;
const LOW_PERCENT = 25;
const MID_PERCENT = 75;

const STATE_BADGES: Readonly<Record<string, Badge>> = {
  pausedUP: { className: "badge-success", text: "Completed" },
  downloading: { className: "badge-info", text: "Downloading" },
  error: { className: "badge-error", text: "Error" },
  queued: { className: "badge-ghost", text: "Queued" },
  paused: { className: "badge-warning", text: "Paused" },
};

const PROTOCOL_BADGES: Readonly<Record<string, ProtocolBadge>> = {
  torrent: { className: "badge-accent", icon: "bi-magnet", text: "Torrent" },
  nzb: { className: "badge-secondary", icon: "bi-newspaper", text: "Usenet" },
};

const parseTorrent = (value: unknown): Torrent => {
  const record = asRecord(value, "torrent");
  return {
    infoHash: str(record, "info_hash"),
    name: str(record, "name"),
    size: optNum(record, "size") ?? 0,
    progress: num(record, "progress"),
    speed: optNum(record, "speed") ?? optNum(record, "dlspeed"),
    category: optStr(record, "category") ?? "",
    protocol: optStr(record, "protocol") ?? "",
    activeProvider: optStr(record, "active_provider") ?? "",
    numSeeds: optNum(record, "num_seeds") ?? optNum(record, "seeders") ?? 0,
    state: optStr(record, "state") ?? "",
  };
};

const parseCategory = (value: unknown): string => {
  if (typeof value !== "string") {
    throw new TypeError("categories: expected strings");
  }
  return value;
};

const parseTorrentPage = (value: unknown): TorrentPage => {
  const record = asRecord(value, "torrents response");
  return {
    torrents: optList(record, "torrents").map((item) => parseTorrent(item)),
    total: optNum(record, "total") ?? 0,
    totalPages: optNum(record, "total_pages") ?? 0,
    categories: optList(record, "categories").map((item) => parseCategory(item)),
  };
};

const createDashboardState = (): DashboardState => ({
  torrents: [],
  total: 0,
  totalPages: 0,
  categories: [],
  page: 1,
  perPage: PER_PAGE,
  search: "",
  category: "",
  stateFilter: "",
  sortBy: "added_on",
  sortOrder: "desc",
  selected: new Set(),
});

const parseSort = (value: string): Pick<DashboardState, "sortBy" | "sortOrder"> => {
  if (value.endsWith("_asc")) {
    return { sortBy: value.replace("_asc", ""), sortOrder: "asc" };
  }
  return { sortBy: value.replace(/_desc$/u, ""), sortOrder: "desc" };
};

const torrentsURL = (state: DashboardState): string => {
  const params = new URLSearchParams({
    page: String(state.page),
    limit: String(state.perPage),
    sort_by: state.sortBy,
    sort_order: state.sortOrder,
  });
  for (const [key, value] of [
    ["search", state.search],
    ["category", state.category],
    ["state", state.stateFilter],
  ] as const) {
    if (value !== "") {
      params.set(key, value);
    }
  }
  return `/api/torrents?${params.toString()}`;
};

const deleteTorrentURL = (hash: string, category: string, removeFromDebrid: boolean): string =>
  `/api/torrents/${category}/${hash}?removeFromDebrid=${String(removeFromDebrid)}`;

const deleteSelectedURL = (hashes: readonly string[], removeFromDebrid: boolean): string =>
  `/api/torrents?hashes=${hashes.join(",")}&removeFromDebrid=${String(removeFromDebrid)}`;

const progressPercent = (progress: number): number => Math.round(progress * PERCENT);

const progressColor = (percent: number): string => {
  if (percent === PERCENT) {
    return "progress-success";
  }
  if (percent < LOW_PERCENT) {
    return "progress-error";
  }
  return percent < MID_PERCENT ? "progress-warning" : "progress-info";
};

const stateBadge = (state: string): Badge => STATE_BADGES[state] ?? { className: "badge-ghost", text: state };

const protocolBadge = (protocol: string): ProtocolBadge =>
  PROTOCOL_BADGES[protocol] ?? {
    className: "badge-ghost",
    icon: "bi-question-circle",
    text: protocol === "" ? "Unknown" : protocol,
  };

const torrentSpeed = (bytesPerSecond: number | undefined): string =>
  bytesPerSecond === undefined || bytesPerSecond === 0 ? "-" : formatSpeed(bytesPerSecond);

export {
  createDashboardState,
  deleteSelectedURL,
  deleteTorrentURL,
  parseSort,
  parseTorrentPage,
  progressColor,
  progressPercent,
  protocolBadge,
  stateBadge,
  torrentSpeed,
  torrentsURL,
};
