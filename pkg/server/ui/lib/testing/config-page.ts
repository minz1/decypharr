import { expect, inject, vi } from "vitest";

import { loadFixture } from "#ui/lib/testing/fixture";

interface Request {
  url: string;
  method: string;
  body: string;
}

type Answer = Error | (() => Promise<Response> | Response);

interface BootOptions {
  fixture?: string;
  routes?: Readonly<Record<string, Answer>>;
  answers?: readonly boolean[];
  before?: () => void;
}

interface Page {
  requests: Request[];
  confirms: string[];
}

const HTTP_OK = 200;
const HTTP_NOT_FOUND = 404;

const respond = (status: number, body: unknown): Response =>
  typeof body === "string" ? new Response(body, { status }) : Response.json(body, { status });

const routeKey = (method: string, url: string): string => `${method} ${url.replace(/^\/(?:decypharr\/)?/u, "")}`;

const defaultRoutes = (fixture: string): Record<string, Answer> => ({
  "GET api/config": () => respond(HTTP_OK, inject("configResponses")[fixture]),
  "GET api/prune/status": () => respond(HTTP_OK, '{"mode":"report","candidates":null}'),
  "POST api/config": () => respond(HTTP_OK, { status: "success", restarted: false }),
});

const stubFetch = (routes: Readonly<Record<string, Answer>>): Request[] => {
  const requests: Request[] = [];
  vi.stubGlobal("fetch", async (url: string, init: RequestInit = {}): Promise<Response> => {
    const method = init.method ?? "GET";
    requests.push({ url, method, body: typeof init.body === "string" ? init.body : "" });
    const route = routes[routeKey(method, url)];
    if (route instanceof Error) {
      throw route;
    }
    return route === undefined ? respond(HTTP_NOT_FOUND, "not found") : route();
  });
  return requests;
};

const stubConfirm = (answers: readonly boolean[]): string[] => {
  const confirms: string[] = [];
  vi.stubGlobal("confirm", (message: string): boolean => {
    confirms.push(message);
    return answers[confirms.length - 1] ?? false;
  });
  return confirms;
};

const bootSettings = async (options: BootOptions = {}): Promise<Page> => {
  const fixture = options.fixture ?? "config";
  vi.resetModules();
  loadFixture(fixture);
  options.before?.();
  const requests = stubFetch({ ...defaultRoutes(fixture), ...options.routes });
  const confirms = stubConfirm(options.answers ?? []);
  await import("#ui/pages/config");
  return { requests, confirms };
};

const settingsLoaded = async (): Promise<void> => {
  await vi.waitFor(() => {
    expect(document.querySelector(".debrid-config")).not.toBeNull();
  });
};

const toastTexts = (): string[] =>
  [...document.querySelectorAll(".toast-container .alert")].map(
    (toast) => `${[...toast.classList].find((name) => name.startsWith("alert-")) ?? ""} ${toast.textContent.trim()}`,
  );

const waitForToast = async (text: string): Promise<void> => {
  await vi.waitFor(() => {
    expect(toastTexts()).toContain(text);
  });
};

const shownToasts = async (text: string): Promise<string[]> => {
  await waitForToast(text);
  return toastTexts();
};

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const member = (value: unknown, key: string): unknown => (isRecord(value) ? value[key] : undefined);

const lastItem = (value: unknown): unknown => (Array.isArray(value) ? value.at(-1) : undefined);

const hasKey = (value: unknown, key: string): boolean => isRecord(value) && Object.hasOwn(value, key);

const find = <Wanted extends Element>(selector: string, type: abstract new () => Wanted): Wanted => {
  const found = document.querySelector(selector);
  if (!(found instanceof type)) {
    throw new TypeError(`the settings page has no ${selector}`);
  }
  return found;
};

const isHidden = (selector: string): boolean => find(selector, HTMLElement).classList.contains("hidden");

const click = (selector: string): void => {
  find(selector, HTMLElement).click();
};

const fill = (selector: string, value: string): void => {
  const field = find(selector, HTMLElement);
  if (field instanceof HTMLInputElement || field instanceof HTMLSelectElement || field instanceof HTMLTextAreaElement) {
    field.value = value;
    field.dispatchEvent(new Event("input", { bubbles: true }));
    field.dispatchEvent(new Event("change", { bubbles: true }));
  }
};

const submitSettings = (): void => {
  find("#configForm", HTMLFormElement).dispatchEvent(new Event("submit", { cancelable: true }));
};

const posts = (requests: readonly Request[], path: string): Request[] =>
  requests.filter((request) => request.method === "POST" && request.url.endsWith(path));

const savedBody = async (requests: readonly Request[]): Promise<Record<string, unknown>> => {
  await vi.waitFor(() => {
    expect(posts(requests, "api/config")).toHaveLength(1);
  });
  const parsed: unknown = JSON.parse(posts(requests, "api/config")[0]?.body ?? "{}");
  if (!isRecord(parsed)) {
    throw new TypeError("the save body is not an object");
  }
  return parsed;
};

export type { Request };
export {
  bootSettings,
  click,
  fill,
  find,
  hasKey,
  isHidden,
  lastItem,
  member,
  shownToasts,
  posts,
  respond,
  savedBody,
  settingsLoaded,
  submitSettings,
  toastTexts,
  waitForToast,
};
