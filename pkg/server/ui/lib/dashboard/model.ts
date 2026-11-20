import { formatSpeed } from "#ui/lib/common/format";
import { asRecord, num, optList, optNum, optStr, str } from "#ui/lib/common/json";
import type {
  ArrFail,
  Badge,
  Deadline,
  DashboardState,
  ProtocolBadge,
  Torrent,
  TorrentPage,
} from "#ui/lib/dashboard/types";

const PER_PAGE = 20;
const PERCENT = 100;
const LOW_PERCENT = 25;
const MID_PERCENT = 75;

const STATE_BADGES: Readonly<Record<string, string>> = {
  queued: "badge-neutral",
  throttled: "badge-neutral",
  submitting: "badge-neutral",
  provider: "badge-info",
  awaiting_links: "badge-info",
  acting: "badge-info",
  complete: "badge-success",
  failed: "badge-error",
  deleting: "badge-warning",
};

const PROTOCOL_BADGES: Readonly<Record<string, ProtocolBadge>> = {
  torrent: { className: "badge-accent", icon: "bi-magnet", text: "Torrent" },
  nzb: { className: "badge-secondary", icon: "bi-newspaper", text: "Usenet" },
};

const parseArrFail = (value: unknown): ArrFail => {
  const record = asRecord(value, "arr_fail");
  const waiting = optStr(record, "waiting");
  const lastError = optStr(record, "last_error");
  const nextRetry = optStr(record, "next_retry");
  const jobStatus = optStr(record, "job_status");
  return {
    state: str(record, "state"),
    arr: str(record, "arr"),
    ...(waiting === undefined ? {} : { waiting }),
    ...(lastError === undefined ? {} : { lastError }),
    ...(nextRetry === undefined ? {} : { nextRetry }),
    ...(jobStatus === undefined ? {} : { jobStatus }),
  };
};

const parseDeadline = (value: unknown): Deadline => {
  const record = asRecord(value, "deadline");
  const waitingFor = optStr(record, "waiting_for");
  return {
    event: str(record, "event"),
    due: str(record, "due"),
    ...(waitingFor === undefined ? {} : { waitingFor }),
  };
};

const parseTorrent = (value: unknown): Torrent => {
  const record = asRecord(value, "torrent");
  const arrFail =
    record["arr_fail"] === undefined || record["arr_fail"] === null ? undefined : parseArrFail(record["arr_fail"]);
  const deadline =
    record["deadline"] === undefined || record["deadline"] === null ? undefined : parseDeadline(record["deadline"]);
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
    phase: optStr(record, "phase") ?? "",
    label: optStr(record, "label") ?? "",
    ...(arrFail === undefined ? {} : { arrFail }),
    ...(deadline === undefined ? {} : { deadline }),
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

const WAITING_TEXT: Readonly<Record<string, string>> = {
  arr_not_registered: "waiting for {arr} to register",
  other_job_active: "waiting for another job",
};

const pendingNote = (arrFail: ArrFail): string => {
  if (arrFail.waiting !== undefined) {
    return (WAITING_TEXT[arrFail.waiting] ?? arrFail.waiting).replace("{arr}", arrFail.arr);
  }
  const retry = arrFail.nextRetry === undefined ? [] : [`retry ${new Date(arrFail.nextRetry).toLocaleTimeString()}`];
  return [...(arrFail.lastError === undefined ? [] : [arrFail.lastError]), ...retry].join("; ");
};

const arrFailText = (arrFail: ArrFail | undefined): string => {
  if (arrFail === undefined) {
    return "";
  }
  switch (arrFail.state) {
    case "pending": {
      const note = pendingNote(arrFail);
      return `telling ${arrFail.arr}${note === "" ? "" : ` (${note})`}`;
    }
    case "queued": {
      return `fail job queued${arrFail.jobStatus === undefined ? "" : ` (${arrFail.jobStatus})`}`;
    }
    default: {
      return "not told";
    }
  }
};

const deadlineText = (deadline: Deadline | undefined, provider: string): string => {
  if (deadline === undefined) {
    return "";
  }
  const due = new Date(deadline.due).toLocaleString();
  if (deadline.waitingFor === "links") {
    return `waiting for a fresh answer from ${provider === "" ? "the provider" : provider} (gives up ${due})`;
  }
  return `${deadline.event} at ${due}`;
};

const stateBadge = (phase: string, label: string): Badge => ({
  className: STATE_BADGES[phase] ?? "badge-ghost",
  text: label === "" ? phase : label,
});

const protocolBadge = (protocol: string): ProtocolBadge =>
  PROTOCOL_BADGES[protocol] ?? {
    className: "badge-ghost",
    icon: "bi-question-circle",
    text: protocol === "" ? "Unknown" : protocol,
  };

const torrentSpeed = (bytesPerSecond: number | undefined): string =>
  bytesPerSecond === undefined || bytesPerSecond === 0 ? "-" : formatSpeed(bytesPerSecond);

export {
  arrFailText,
  createDashboardState,
  deadlineText,
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
