import { expect, vi } from "vitest";

interface RecordedRequest {
  url: string;
  method: string;
  body: string;
}

type Routes = Readonly<Record<string, () => Response>>;

const BASE_PREFIX = "/decypharr";

const realSetTimeout = globalThis.setTimeout;

const routeKey = (method: string, url: string): string =>
  `${method} ${url.startsWith(`${BASE_PREFIX}/`) ? url.slice(BASE_PREFIX.length) : url}`;

const stubAPI = (routes: Routes): RecordedRequest[] => {
  const requests: RecordedRequest[] = [];
  vi.stubGlobal("fetch", (url: string, init: RequestInit = {}): Response => {
    const method = init.method ?? "GET";
    requests.push({ url, method, body: typeof init.body === "string" ? init.body : "" });
    const route = routes[routeKey(method, url)];
    return route === undefined ? new Response("no route", { status: 404 }) : route();
  });
  return requests;
};

const sequence = (...responses: readonly (() => Response)[]): (() => Response) => {
  let index = 0;
  return () => {
    const next = responses[Math.min(index, responses.length - 1)];
    index += 1;
    if (next === undefined) {
      throw new Error("an empty response sequence");
    }
    return next();
  };
};

const settle = async (): Promise<void> => {
  const tick = { done: false };
  realSetTimeout(() => {
    tick.done = true;
  }, 0);
  await vi.waitFor(() => {
    expect(tick.done).toBe(true);
  });
};

const element = <Wanted extends HTMLElement>(selector: string, type: abstract new () => Wanted): Wanted => {
  const found = document.querySelector(selector);
  if (!(found instanceof type)) {
    throw new TypeError(`no ${selector}`);
  }
  return found;
};

const click = async (selector: string): Promise<void> => {
  element(selector, HTMLElement).click();
  await settle();
};

const toasts = (type: "error" | "info" | "success" | "warning"): string[] =>
  [...document.querySelectorAll(`.toast-container .alert-${type}`)].map((toast) => toast.textContent.trim());

const mutations = (requests: readonly RecordedRequest[]): RecordedRequest[] =>
  requests.filter((request) => request.method !== "GET");

const stubReload = (): { count: number } => {
  const reloads = { count: 0 };
  vi.spyOn(globalThis.location, "reload").mockImplementation(() => {
    reloads.count += 1;
  });
  return reloads;
};

export type { RecordedRequest, Routes };
export { click, element, mutations, sequence, settle, stubAPI, stubReload, toasts };
