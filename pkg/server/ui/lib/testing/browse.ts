import { expect, onTestFinished, vi } from "vitest";

import { stubAPI } from "#ui/lib/testing/api";
import { loadFixture } from "#ui/lib/testing/fixture";
import { XSS } from "#ui/lib/testing/xss";

type Responder = (url: string, method: string) => Response | Promise<Response>;

interface BootOptions {
  fixture?: string;
  location?: string;
  prepare?: () => void;
}

const FILE_SIZE = 2048;

const folder = (name: string, kind: string): Record<string, unknown> => ({
  name,
  path: `/${name}`,
  size: 0,
  is_dir: true,
  kind,
  active_debrid: "",
});

const item = (name: string, hash: string): Record<string, unknown> => ({
  name,
  path: `/__all__/${name}`,
  size: 0,
  mod_time: "2024-01-01 10:00",
  is_dir: true,
  info_hash: hash,
  can_delete: true,
  active_debrid: "realdebrid",
  kind: "entry",
});

const file = (name: string): Record<string, unknown> => ({
  name,
  path: `/__all__/Show/${name}`,
  size: FILE_SIZE,
  mod_time: "2024-01-02 10:00",
  is_dir: false,
  kind: "file",
  active_debrid: "",
});

const listing = (entries: Record<string, unknown>[], extra: Record<string, unknown> = {}): Response =>
  Response.json({ entries, total: entries.length, total_pages: 1, ...extra });

const healthy = (): Response => Response.json({ status: "healthy", last_checked_at: "" });

const library = (url: string, method: string): Response => {
  if (url.includes("/api/repair/health/")) {
    return url.endsWith("/check") ? Response.json({ status: "broken", last_checked_at: "" }) : healthy();
  }
  if (method === "DELETE") {
    return new Response("");
  }
  if (url.includes("/api/browse/__all__/Show")) {
    return listing([file("a.mkv"), file("b.mkv")]);
  }
  if (url.includes("/api/browse/__all__")) {
    return listing([item(XSS, "abc"), item("Show", "def")]);
  }
  return listing([folder("__all__", "system"), folder("Kids", "virtual")]);
};

const bootBrowse = async (respond: Responder, options: BootOptions = {}): Promise<ReturnType<typeof stubAPI>> => {
  vi.resetModules();
  vi.useFakeTimers();
  onTestFinished(() => {
    vi.useRealTimers();
  });
  loadFixture(options.fixture ?? "browse");
  globalThis.history.replaceState({}, "", options.location ?? "/browse");
  options.prepare?.();
  const requests = stubAPI({}, respond);
  await import("#ui/pages/browse");
  return requests;
};

const rows = (): HTMLTableRowElement[] => [...document.querySelectorAll<HTMLTableRowElement>("#fileBrowserList tr")];

const rowControl = <Wanted extends Element>(
  index: number,
  selector: string,
  type: abstract new () => Wanted,
): Wanted => {
  const found = rows()[index]?.querySelector(selector);
  if (!(found instanceof type)) {
    throw new TypeError(`row ${index} has no ${selector}`);
  }
  return found;
};

const rowText = (index: number): string => (rows()[index]?.textContent ?? "").replaceAll(/\s+/gu, " ").trim();

const waitForName = async (name: string): Promise<void> => {
  await vi.waitFor(() => {
    expect(rows().map((row) => row.querySelector('[data-field="name"]')?.textContent)).toContain(name);
  });
};

const openFolder = async (index: number, expected: string): Promise<void> => {
  rowControl(index, '[data-action="open"]', HTMLButtonElement).click();
  await waitForName(expected);
};

const stubOpen = (): string[] => {
  const opened: string[] = [];
  vi.stubGlobal("open", (url: string, target: string) => {
    opened.push(`${target} ${url}`);
  });
  return opened;
};

const browseCalls = (requests: ReturnType<typeof stubAPI>): string[] =>
  requests
    .filter((request) => request.url.includes("/api/browse"))
    .map((request) => `${request.method} ${request.url}`);

export {
  bootBrowse,
  browseCalls,
  file,
  folder,
  item,
  library,
  listing,
  openFolder,
  rowControl,
  rowText,
  rows,
  stubOpen,
  waitForName,
};
