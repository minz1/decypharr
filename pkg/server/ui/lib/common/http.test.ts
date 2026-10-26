import { describe, expect, it, vi } from "vitest";

import { fetcher } from "#ui/lib/common/http";

interface Call {
  url: string;
  init: RequestInit | undefined;
}

const stubFetch = (urlBase: string): Call[] => {
  const calls: Call[] = [];
  document.head.innerHTML = urlBase === "" ? "" : `<meta name="decypharr-url-base" content="${urlBase}">`;
  vi.stubGlobal("fetch", async (input: string, init?: RequestInit): Promise<Response> => {
    calls.push({ url: input, init });
    return Promise.resolve(new Response("ok"));
  });
  return calls;
};

const sentHeaders = (calls: Call[]): Record<string, string> =>
  Object.fromEntries([...new Headers(calls[0]?.init?.headers)].map(([name, value]) => [name.toLowerCase(), value]));

describe("fetching through the URL base", () => {
  it("prefixes the URL base", async () => {
    const calls = stubFetch("/decypharr/");
    await fetcher("/api/torrents");
    expect(calls.map((call) => call.url)).toStrictEqual(["/decypharr/api/torrents"]);
  });

  it("uses / when the page has no URL base", async () => {
    const calls = stubFetch("");
    await fetcher("api/torrents");
    expect(calls.map((call) => call.url)).toStrictEqual(["/api/torrents"]);
  });

  it("sends JSON by default and keeps the caller's options", async () => {
    const calls = stubFetch("/");
    const init: RequestInit = { method: "POST", body: "{}" };
    await fetcher("/api/x", init);
    expect(sentHeaders(calls)).toStrictEqual({ "content-type": "application/json" });
    expect(calls[0]?.init?.method).toBe("POST");
    expect(calls[0]?.init?.body).toBe("{}");
    expect(init).toStrictEqual({ method: "POST", body: "{}" });
  });

  it("lets the caller's headers win", async () => {
    const calls = stubFetch("/");
    await fetcher("/api/x", { headers: { "Content-Type": "text/plain", "X-Extra": "1" } });
    expect(sentHeaders(calls)).toStrictEqual({ "content-type": "text/plain", "x-extra": "1" });
  });

  it("leaves Content-Type to the browser for FormData", async () => {
    const calls = stubFetch("/");
    await fetcher("/api/x", { method: "POST", body: new FormData() });
    expect(sentHeaders(calls)).toStrictEqual({});
  });

  it("returns the response", async () => {
    stubFetch("/");
    const response = await fetcher("/api/x");
    await expect(response.text()).resolves.toBe("ok");
  });
});
