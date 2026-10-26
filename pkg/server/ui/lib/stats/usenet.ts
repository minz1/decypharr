import { formatBytes } from "#ui/lib/common/format";
import { asRecord, optBool, str } from "#ui/lib/common/json";
import { cloneElement, emptyMessage, field, setFields } from "#ui/lib/stats/dom";
import { PERCENT, formatNumber, orElse } from "#ui/lib/stats/format";
import { count, listOrEmpty, optRecord, recordOrEmpty, text } from "#ui/lib/stats/read";
import { parseSpeedTest, withText } from "#ui/lib/stats/speedtest";

interface Provider {
  id: string;
  host: string;
  username: string;
  ssl: boolean;
  maxConnections: number;
  active: number;
  idle: number;
  speedTest: ReturnType<typeof parseSpeedTest> | undefined;
}

interface UsenetStats {
  maxConnections: number;
  active: number;
  idle: number;
  totalCreated: number;
  memoryAllocated: number;
  memoryInUse: number;
  cachedBodyBytes: number;
  providers: Provider[];
}

interface Bars {
  active: number;
  idle: number;
}

interface ProviderView {
  provider: string;
  texts: Record<string, string>;
  securityClass: string;
  bars: Bars;
  stored: Provider["speedTest"];
}

const parseProvider = (value: unknown): Provider => {
  const record = asRecord(value, "providers");
  const speedTest = optRecord(record, "speed_test");
  return {
    id: text(record, "id"),
    host: str(record, "host"),
    username: text(record, "username"),
    ssl: optBool(record, "ssl") ?? false,
    maxConnections: Math.max(count(record, "max_connections"), 0),
    active: Math.max(count(record, "active"), 0),
    idle: Math.max(count(record, "idle"), 0),
    speedTest: speedTest && parseSpeedTest(speedTest),
  };
};

const parseUsenet = (record?: Record<string, unknown>): UsenetStats | undefined => {
  if (!record) {
    return undefined;
  }
  const pool = recordOrEmpty(record, "pool");
  const buffers = recordOrEmpty(record, "buffers");
  return {
    maxConnections: count(pool, "max_connections"),
    active: count(pool, "active"),
    idle: count(pool, "idle"),
    totalCreated: count(pool, "total_created"),
    memoryAllocated: count(buffers, "memory_allocated"),
    memoryInUse: count(buffers, "memory_in_use"),
    cachedBodyBytes: count(recordOrEmpty(record, "analyzer"), "CachedBodyBytes"),
    providers: listOrEmpty(record, "providers").map((provider) => parseProvider(provider)),
  };
};

const share = (part: number, whole: number): number => (whole > 0 ? (part / whole) * PERCENT : 0);

const usenetView = (stats: UsenetStats): { texts: Record<string, string>; bars: Bars } => ({
  texts: {
    servers: `${stats.providers.length} server(s) configured`,
    connections: `${formatNumber(stats.totalCreated)} / ${formatNumber(stats.maxConnections)} connections`,
    buffers: `Stream buffer allocations: ${formatBytes(stats.memoryAllocated)} · In use: ${formatBytes(stats.memoryInUse)}`,
    "parser-cache": `Parser body cache: ${formatBytes(stats.cachedBodyBytes)}. This cache is separate from stream buffers.`,
    utilization: `${share(stats.totalCreated, stats.maxConnections).toFixed(1)}%`,
    active: `Active: ${stats.active}`,
    idle: `Idle: ${stats.idle}`,
  },
  bars: { active: share(stats.active, stats.maxConnections), idle: share(stats.idle, stats.maxConnections) },
});

const providerView = (provider: Provider): ProviderView => {
  const active = Math.min(share(provider.active, provider.maxConnections), PERCENT);
  const idle = Math.min(share(provider.idle, provider.maxConnections), PERCENT - active);
  const user = provider.username === "" ? "" : `${provider.username} · `;
  return {
    provider: orElse(provider.id, provider.host),
    texts: {
      host: provider.host,
      security: provider.ssl ? "SSL" : "Plain",
      connections: `${user}${provider.maxConnections} connections`,
      utilization: `${Math.min(share(provider.active + provider.idle, provider.maxConnections), PERCENT).toFixed(1)}%`,
      active: `Active: ${provider.active}`,
      idle: `Idle: ${provider.idle}`,
    },
    securityClass: provider.ssl ? "badge-success" : "badge-warning",
    bars: { active, idle },
    stored: provider.speedTest,
  };
};

const storedSpeed = (stored: Provider["speedTest"]): { ok: boolean; text: string } | undefined => {
  if (!stored || stored.testedAt === "") {
    return undefined;
  }
  if (stored.error !== "") {
    return { ok: false, text: "Error" };
  }
  return { ok: true, text: stored.speedMbps > 0 ? `${stored.speedMbps.toFixed(1)} MB/s` : `${stored.latencyMs}ms` };
};

const fillBars = (root: ParentNode, bars: Bars): void => {
  field(root, "active-bar").style.width = `${bars.active}%`;
  const idle = field(root, "idle-bar");
  idle.style.left = `${bars.active}%`;
  idle.style.width = `${bars.idle}%`;
};

const storedSpeedBadge = (stored: Provider["speedTest"]): HTMLElement[] => {
  const speed = storedSpeed(stored);
  if (!speed) {
    return [];
  }
  return [
    speed.ok ? withText(cloneElement("stats-nntp-stored-ok"), speed.text) : cloneElement("stats-nntp-stored-error"),
  ];
};

const providerCard = (view: ProviderView): HTMLElement => {
  const card = cloneElement("stats-nntp-provider");
  setFields(card, view.texts);
  field(card, "security").classList.add(view.securityClass);
  fillBars(card, view.bars);
  const test = field(card, "test");
  test.dataset["provider"] = view.provider;
  test.before(...storedSpeedBadge(view.stored));
  return card;
};

const renderUsenet = (content: HTMLElement, stats: UsenetStats | undefined): void => {
  if (!stats || stats.providers.length === 0) {
    content.replaceChildren(emptyMessage("No usenet providers configured."));
    return;
  }
  const view = usenetView(stats);
  const panel = cloneElement("stats-usenet");
  setFields(panel, view.texts);
  fillBars(panel, view.bars);
  field(panel, "providers").replaceChildren(...stats.providers.map((provider) => providerCard(providerView(provider))));
  content.replaceChildren(panel);
};

export { parseUsenet, providerView, renderUsenet, storedSpeed, usenetView };
