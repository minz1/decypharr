import { expect, onTestFinished, vi } from "vitest";

import { loadFixture } from "#ui/lib/testing/fixture";

interface Sent {
  url: string;
  method: string;
  body: string;
  contentType: string | null;
}

type Routes = Record<string, () => Response | Promise<Response>>;

const XSS = "<img src=x onerror=alert(1)>";
const MS_PER_SECOND = 1000;
const STREAM_AGE_SECONDS = 5;
const OK = 200;
const NOT_FOUND = 404;
const SETTLE_MS = 50;

const debridEntry = {
  profile: { name: XSS, username: XSS, points: 10, type: "premium", expiration: "2027-01-31T00:00:00Z" },
  library: { total: 1, bad: 0, active_links: 2 },
  accounts: [{ in_use: true, order: 0, disabled: false, username: XSS, traffic_used: 0, expiration: "" }],
};

const dfsMount = {
  ready: true,
  enabled: true,
  type: "dfs",
  detail: {
    total_files: 10,
    active_files: 1,
    cache_type: "vfs",
    cache_total_size: 1_610_612_736,
    cache_max_size: 2_147_483_648,
    cache_item_count: 120,
    cache_active_item_count: 9,
    cache_utilization: 0.75,
    cache_cache_hits: 9,
    cache_cache_misses: 1,
    cache_cache_hit_rate: 0.9,
    cache_active_downloads: 0,
    cache_circuit_breakers: 0,
  },
};

const stats = (overrides: Record<string, unknown> = {}): Record<string, unknown> => ({
  system: {
    memory_used: "40.00MB",
    goroutines: 56,
    num_cpu: 8,
    uptime_seconds: 3725,
    start_time: "2026-10-26 06:00:00",
  },
  debrids: [debridEntry],
  mount: dfsMount,
  access: {
    webdav: { enabled: true, path: "/webdav", port: "8282", auth_required: true },
    nfs: { enabled: false, port: 2049 },
    smb: { enabled: true, port: 1445, share_name: "media", username: "kim" },
  },
  usenet: {
    providers: [{ id: "news:563/kim", host: XSS, username: "kim", max_connections: 10, active: 1, idle: 1, ssl: true }],
    pool: { max_connections: 10, total_created: 2, active: 1, idle: 1 },
    buffers: {},
    analyzer: {},
  },
  active_streams: {
    count: 1,
    streams: [
      {
        entry_name: XSS,
        file_name: XSS,
        file_size: 1024,
        source: "nzb",
        started_at: Math.floor(Date.now() / MS_PER_SECOND) - STREAM_AGE_SECONDS,
        debrid: XSS,
        client: XSS,
      },
    ],
  },
  storage: { db_size: 1536, total_entries: 3 },
  queue: { pending: 2, active: 0 },
  repair: { enabled: true, active: false },
  hearsay: { enabled: false, participate: false, publish: false },
  ...overrides,
});

const json = (value: unknown, status = OK): (() => Response) => {
  const body = JSON.stringify(value);
  return () => new Response(body, { status, headers: { "Content-Type": "application/json" } });
};

const text =
  (body: string, status: number, statusText = ""): (() => Response) =>
  () =>
    new Response(body, { status, statusText });

const statsRoute = (overrides: Record<string, unknown> = {}): Routes => ({
  "GET /debug/stats": json(stats(overrides)),
});

const bootStats = async (routes: Routes, fixture = "stats", prepare?: () => void): Promise<Sent[]> => {
  vi.resetModules();
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  onTestFinished(() => {
    vi.useRealTimers();
  });
  loadFixture(fixture);
  prepare?.();
  const sent: Sent[] = [];
  vi.stubGlobal("fetch", async (url: string, init: RequestInit = {}): Promise<Response> => {
    const request = new Request(new URL(url, "http://stats.test"), init);
    const entry: Sent = { url, method: request.method, body: "", contentType: request.headers.get("Content-Type") };
    sent.push(entry);
    entry.body = await request.text();
    const route = routes[`${request.method} ${url.replace("/decypharr/", "/")}`];
    return route ? route() : new Response("not found", { status: NOT_FOUND });
  });
  await import("#ui/pages/stats");
  return sent;
};

const element = (selector: string): HTMLElement => {
  const found = document.querySelector(selector);
  if (!(found instanceof HTMLElement)) {
    throw new TypeError(`no ${selector}`);
  }
  return found;
};

const textOf = (selector: string): string => element(selector).textContent.replaceAll(/\s+/gu, " ").trim();

const toasts = (): string[] =>
  [...document.querySelectorAll(".toast-container .alert")].map((toast) =>
    toast.textContent.replaceAll(/\s+/gu, " ").trim(),
  );

const loaded = async (): Promise<void> => {
  await vi.waitFor(
    () => {
      expect(element("#stats-content").style.display).toBe("block");
    },
    { interval: 1 },
  );
};

const click = (selector: string): void => {
  element(selector).click();
};

const settle = async (): Promise<void> => {
  const { promise, resolve } = Promise.withResolvers<undefined>();
  setTimeout(resolve, SETTLE_MS);
  await promise;
};

const pageHost = (): string => (globalThis.location.hostname === "" ? "SERVER_IP" : globalThis.location.hostname);

export type { Routes, Sent };
export {
  XSS,
  bootStats,
  click,
  debridEntry,
  dfsMount,
  element,
  json,
  loaded,
  pageHost,
  settle,
  stats,
  statsRoute,
  text,
  textOf,
  toasts,
};
