import { describe, expect, inject, it, vi } from "vitest";

import { installBridge } from "#ui/lib/common/legacy-bridge";
import { loadFixture } from "#ui/lib/testing/fixture";
import { roundTripMismatches } from "#ui/lib/testing/round-trip";

interface Request {
  url: string;
  method: string;
  body: string;
}

const json = (body: unknown): Response => Response.json(body);

const fakeServer = (config: unknown): Request[] => {
  const requests: Request[] = [];
  vi.stubGlobal("fetch", async (url: string, init: RequestInit = {}): Promise<Response> => {
    const method = init.method ?? "GET";
    requests.push({ url, method, body: typeof init.body === "string" ? init.body : "" });
    if (url.endsWith("api/config") && method === "GET") {
      return Promise.resolve(json(config));
    }
    if (url.endsWith("api/config")) {
      return Promise.resolve(json({ status: "success", restarted: false }));
    }
    return Promise.resolve(json({}));
  });
  return requests;
};

const call = (target: object, name: string): unknown => {
  const method: unknown = Reflect.get(target, name);
  if (typeof method !== "function") {
    throw new TypeError(`legacy ConfigManager has no ${name}`);
  }
  const result: unknown = Reflect.apply(method, target, []);
  return result;
};

const asObject = (value: unknown, what: string): object => {
  if (typeof value !== "object" || value === null) {
    throw new TypeError(`${what} is not an object`);
  }
  return value;
};

const bootLegacy = async (fixture: string, config: unknown): Promise<{ manager: object; requests: Request[] }> => {
  loadFixture(fixture);
  installBridge();
  const requests = fakeServer(config);
  const legacy: unknown = await import(inject("legacyConfigModule"));
  const managerClass: unknown = Reflect.get(asObject(legacy, "config.js"), "ConfigManager");
  if (typeof managerClass !== "function") {
    throw new TypeError("config.js has no ConfigManager");
  }
  const manager = asObject(Reflect.construct(managerClass, []), "ConfigManager");
  await vi.waitFor(() => {
    const loaded: unknown = Reflect.get(manager, "loaded");
    expect({ loaded }).toStrictEqual({ loaded: true });
  });
  return { manager, requests };
};

const collectLegacy = (manager: object): unknown => {
  const collected = call(manager, "collectFormData");
  return { ...asObject(collected, "collectFormData"), prune: call(manager, "collectPruneConfig") };
};

const repeat = (value: string, times: number): string[] => Array.from({ length: times }, () => value);

const happyDOMQuirks = [
  "want usenet.body_pipeline_depth: [3], got [2]",
  `want queue_cleanup.rules[].action: ${JSON.stringify([...repeat("blacklist", 9), "import"])}, got ${JSON.stringify(repeat("import", 10))}`,
];

describe("the settings page", () => {
  it("round-trips every config field", async () => {
    const { manager } = await bootLegacy("config", inject("configSample"));
    // Happy-dom 21 drops a number assigned to select.value and misreads a parsed selected option; browsers do neither.
    expect(
      roundTripMismatches(inject("configSample"), collectLegacy(manager), inject("configExclusions")),
    ).toStrictEqual(happyDOMQuirks);
  });
});
