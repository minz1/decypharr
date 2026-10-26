import { expect, onTestFinished, vi } from "vitest";

import { loadFixture } from "#ui/lib/testing/fixture";

interface Request {
  url: string;
  method: string;
}

type Responder = (url: string) => Response | Promise<Response>;

interface BootOptions {
  fixture?: string;
  prepare?: () => void;
}

const XSS = "<img src=x onerror=alert(1)>";
const GIB = 1_073_741_824;
const HALF = 0.5;
const SPEED = 2048;

const torrent = (hash: string, name: string, category = "sonarr"): Record<string, unknown> => ({
  info_hash: hash,
  name,
  size: GIB,
  progress: HALF,
  speed: SPEED,
  category,
  protocol: "torrent",
  active_provider: "realdebrid",
  state: "downloading",
});

const page = (torrents: Record<string, unknown>[], totalPages = 1): Response =>
  Response.json({ torrents, total: torrents.length, total_pages: totalPages, categories: ["sonarr", "radarr"] });

const twoTorrents = (): Response =>
  page([torrent("aaaaaaaaaaaa", XSS), torrent("bbbbbbbbbbbb", "Movie.2024", "radarr")]);

const bootDashboard = async (respond: Responder, options: BootOptions = {}): Promise<Request[]> => {
  vi.resetModules();
  vi.useFakeTimers();
  onTestFinished(() => {
    vi.useRealTimers();
  });
  loadFixture(options.fixture ?? "index");
  options.prepare?.();
  const requests: Request[] = [];
  vi.stubGlobal("fetch", async (url: string, init: RequestInit): Promise<Response> => {
    requests.push({ url, method: init.method ?? "GET" });
    return respond(url);
  });
  await import("#ui/pages/dashboard");
  return requests;
};

const toastText = (): string[] =>
  [...document.querySelectorAll(".toast-container .alert")].map((toast) => toast.textContent.trim());

const waitForToast = async (): Promise<void> => {
  await vi.waitFor(() => {
    expect(toastText().length).toBeGreaterThan(0);
  });
};

const element = <Wanted extends Element>(selector: string, type: abstract new () => Wanted): Wanted => {
  const found = document.querySelector(selector);
  if (!(found instanceof type)) {
    throw new TypeError(`the fixture has no ${selector}`);
  }
  return found;
};

const rows = (): HTMLTableRowElement[] => [...document.querySelectorAll<HTMLTableRowElement>("#torrentsList tr")];

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

const cellText = (index: number, name: string): string =>
  rowControl(index, `[data-field="${name}"]`, HTMLElement).textContent.trim();

const waitForRows = async (count: number): Promise<void> => {
  await vi.waitFor(() => {
    expect(rows()).toHaveLength(count);
  });
};

const change = (selector: string, value: string): void => {
  const field = element(selector, HTMLElement);
  if (field instanceof HTMLInputElement || field instanceof HTMLSelectElement) {
    field.value = value;
  }
  field.dispatchEvent(new Event(field instanceof HTMLInputElement ? "input" : "change", { bubbles: true }));
};

const check = (box: HTMLInputElement): void => {
  box.checked = true;
  box.dispatchEvent(new Event("change", { bubbles: true }));
};

const confirmText = async (): Promise<string> => {
  const dialog = element("#confirmDialog", HTMLDialogElement);
  await vi.waitFor(() => {
    expect(dialog.open).toBe(true);
  });
  return element("#confirmDialogMessage", HTMLElement).textContent;
};

const answerConfirm = async (answer: "ok" | "cancel"): Promise<string> => {
  const text = await confirmText();
  element(`#confirmDialog [data-answer="${answer}"]`, HTMLButtonElement).click();
  return text;
};

const lastURL = (requests: readonly Request[]): string => requests.at(-1)?.url ?? "";

export type { Request };
export {
  XSS,
  answerConfirm,
  bootDashboard,
  cellText,
  change,
  check,
  element,
  lastURL,
  page,
  rowControl,
  rows,
  toastText,
  torrent,
  twoTorrents,
  waitForRows,
  waitForToast,
};
