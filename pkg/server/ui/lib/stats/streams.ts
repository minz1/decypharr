import { formatBytes } from "#ui/lib/common/format";
import { asRecord, num, numOrZero, str, strOrEmpty } from "#ui/lib/common/json";
import { cloneElement, emptyMessage, field, setFields } from "#ui/lib/stats/dom";
import type { StreamsElements } from "#ui/lib/stats/elements";
import { formatUptime } from "#ui/lib/stats/format";
import { listOrEmpty } from "#ui/lib/stats/read";

interface Stream {
  entryName: string;
  fileName: string;
  fileSize: number;
  source: string;
  startedAt: number;
  debrid: string;
  client: string;
}

interface StreamStats {
  count: number;
  streams: Stream[];
}

interface StreamView {
  sourceText: string;
  sourceClass: string;
  debrid: string;
  client: { text: string; title: string } | undefined;
  entryName: string;
  fileName: string;
  size: string;
  duration: string;
}

const MS_PER_SECOND = 1000;
const CLIENT_MAX_LENGTH = 30;
const APP_NAME = /^(?<name>[^\s/]+(?:\/[\d.]+)?)/u;

const parseStream = (value: unknown): Stream => {
  const record = asRecord(value, "streams");
  return {
    entryName: str(record, "entry_name"),
    fileName: str(record, "file_name"),
    fileSize: num(record, "file_size"),
    source: strOrEmpty(record, "source"),
    startedAt: num(record, "started_at"),
    debrid: strOrEmpty(record, "debrid"),
    client: strOrEmpty(record, "client"),
  };
};

const parseStreams = (record?: Record<string, unknown>): StreamStats => {
  if (!record) {
    return { count: 0, streams: [] };
  }
  return {
    count: numOrZero(record, "count"),
    streams: listOrEmpty(record, "streams").map((item) => parseStream(item)),
  };
};

const streamBadges = (total: number): { count: string; cardClass: string; tabClass: string } => {
  const tone = total > 0 ? "badge-success" : "badge-ghost";
  return { count: String(total), cardClass: `badge ${tone}`, tabClass: `badge badge-sm ${tone} ml-2` };
};

const clientLabel = (client: string): string => {
  if (client.length <= CLIENT_MAX_LENGTH) {
    return client;
  }
  return APP_NAME.exec(client)?.groups?.["name"] ?? `${client.slice(0, CLIENT_MAX_LENGTH)}...`;
};

const streamView = (stream: Stream, nowMs: number): StreamView => {
  const nzb = stream.source === "nzb";
  return {
    sourceText: nzb ? "NZB" : "Torrent",
    sourceClass: nzb ? "badge-accent" : "badge-secondary",
    debrid: stream.debrid,
    client: stream.client === "" ? undefined : { text: clientLabel(stream.client), title: stream.client },
    entryName: stream.entryName,
    fileName: stream.fileName,
    size: formatBytes(stream.fileSize),
    duration: formatUptime(Math.floor((nowMs - stream.startedAt * MS_PER_SECOND) / MS_PER_SECOND)),
  };
};

const optionalText = (element: HTMLElement, value: string | undefined): void => {
  if (value === undefined || value === "") {
    element.remove();
  } else {
    element.textContent = value;
  }
};

const streamCard = (view: StreamView): HTMLElement => {
  const card = cloneElement("stats-stream");
  setFields(card, {
    source: view.sourceText,
    entry: view.entryName,
    file: view.fileName,
    size: view.size,
    duration: view.duration,
  });
  field(card, "source").classList.add(view.sourceClass);
  field(card, "entry").title = view.entryName;
  field(card, "file").title = view.fileName;
  optionalText(field(card, "debrid"), view.debrid);
  const client = field(card, "client");
  optionalText(client, view.client?.text);
  client.title = view.client?.title ?? "";
  return card;
};

const renderBadges = (elements: StreamsElements, total: number): void => {
  const badges = streamBadges(total);
  elements.count.textContent = badges.count;
  elements.tabCount.textContent = badges.count;
  elements.count.className = badges.cardClass;
  elements.tabCount.className = badges.tabClass;
};

const renderStreams = (elements: StreamsElements, stats: StreamStats, nowMs: number): void => {
  renderBadges(elements, stats.count);
  if (stats.count === 0 || stats.streams.length === 0) {
    elements.content.replaceChildren(emptyMessage("No active streams"));
    return;
  }
  const list = cloneElement("stats-streams-list");
  list.replaceChildren(...stats.streams.map((stream) => streamCard(streamView(stream, nowMs))));
  elements.content.replaceChildren(list);
};

export { clientLabel, parseStreams, renderStreams, streamBadges, streamView };
