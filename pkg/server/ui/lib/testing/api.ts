import { vi } from "vitest";

import { element, settle } from "#ui/lib/testing/dom";

interface RecordedRequest {
  url: string;
  method: string;
  body: string;
}

interface FormRequest {
  url: string;
  method: string;
  fields: [string, FormDataEntryValue][];
}

type Answer = Error | (() => Response | Promise<Response>);

type Routes = Readonly<Record<string, Answer>>;

type Fallback = (url: string, method: string) => Response | Promise<Response>;

const BASE_PREFIX = "/decypharr";

const routeKey = (method: string, url: string): string =>
  `${method} ${url.startsWith(`${BASE_PREFIX}/`) ? url.slice(BASE_PREFIX.length) : url}`;

const noRoute: Fallback = () => new Response("no route", { status: 404 });

const stubAPI = (routes: Routes, fallback: Fallback = noRoute): RecordedRequest[] => {
  const requests: RecordedRequest[] = [];
  vi.stubGlobal("fetch", async (url: string, init: RequestInit = {}): Promise<Response> => {
    const method = init.method ?? "GET";
    requests.push({ url, method, body: typeof init.body === "string" ? init.body : "" });
    const route = routes[routeKey(method, url)];
    if (route instanceof Error) {
      throw route;
    }
    return route === undefined ? fallback(url, method) : route();
  });
  return requests;
};

const stubFormAPI = (respond: () => Response | Promise<Response>): FormRequest[] => {
  const requests: FormRequest[] = [];
  vi.stubGlobal("fetch", async (url: string, init: RequestInit = {}): Promise<Response> => {
    const fields = init.body instanceof FormData ? [...init.body.entries()] : [];
    requests.push({ url, method: init.method ?? "GET", fields });
    return respond();
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

const click = async (selector: string): Promise<void> => {
  element(selector, HTMLElement).click();
  await settle();
};

const mutations = (requests: readonly RecordedRequest[]): RecordedRequest[] =>
  requests.filter((request) => request.method !== "GET");

const stubReload = (): { count: number } => {
  const reloads = { count: 0 };
  vi.spyOn(globalThis.location, "reload").mockImplementation(() => {
    reloads.count += 1;
  });
  return reloads;
};

export type { Answer, FormRequest, RecordedRequest, Routes };
export { click, mutations, sequence, stubAPI, stubFormAPI, stubReload };
