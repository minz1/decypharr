import { describe, expect, inject, it, onTestFinished, vi } from "vitest";

import { JSON_NULL } from "#ui/lib/common/json";
import { assembleSettings, collectSections, populateSections } from "#ui/lib/config/sections";
import {
  bootSettings,
  find,
  hasKey,
  isHidden,
  posts,
  respond,
  savedBody,
  settingsLoaded,
  shownToasts,
  submitSettings,
  toastTexts,
  waitForToast,
} from "#ui/lib/testing/config-page";
import { loadFixture } from "#ui/lib/testing/fixture";
import { legacyNoEditSave } from "#ui/lib/testing/legacy-save";
import { roundTripMismatches } from "#ui/lib/testing/round-trip";

const xss = "<img src=x onerror=alert(1)>";
const reportPrune = { enabled: true, mode: "report", schedule: "1h", grace: "72h", max_per_run: 5, roots: ["/data"] };

const fixtureConfig = (): Record<string, unknown> => {
  const { config } = inject("configResponses");
  return typeof config === "object" && config !== null ? { ...config } : {};
};

const serving = (config: Record<string, unknown>): (() => Response) => {
  const merged = { ...fixtureConfig(), ...config };
  return () => respond(200, merged);
};

const hostileConfig = {
  debrids: [{ provider: "realdebrid", name: xss, api_key: "k" }],
  arrs: [{ name: xss, host: "http://a", token: "t", selected_debrid: xss }],
  virtual_folders: [
    { name: xss, match: "all", conditions: [{ field: "entry_name", operator: "contains", value: xss }] },
  ],
};

const switchPruneToDelete = async (answers: readonly boolean[]): Promise<Awaited<ReturnType<typeof bootSettings>>> => {
  const page = await bootSettings({ routes: { "GET api/config": serving({ prune: reportPrune }) }, answers });
  await settingsLoaded();
  find('[id="prune.mode"]', HTMLSelectElement).value = "delete";
  submitSettings();
  return page;
};

const secondSave = async (requests: Awaited<ReturnType<typeof bootSettings>>["requests"]): Promise<unknown> => {
  await waitForToast("alert-success Configuration saved and applied.");
  submitSettings();
  await vi.waitFor(() => {
    expect(posts(requests, "api/config")).toHaveLength(2);
  });
  const second: unknown = JSON.parse(posts(requests, "api/config")[1]?.body ?? "{}");
  return second;
};

describe("the settings sections", () => {
  it("round-trips every config field", () => {
    loadFixture("config");
    populateSections(document, inject("configSample"));
    const collected = assembleSettings(collectSections(document));
    expect(roundTripMismatches(inject("configSample"), collected, inject("configExclusions"))).toStrictEqual([]);
  });
});

describe("saving loaded settings", () => {
  it("a save after a successful config load posts what config.js posted, plus each arr's inherit choice", async () => {
    const { requests } = await bootSettings();
    await settingsLoaded();
    submitSettings();
    await savedBody(requests);
    const expected = {
      ...legacyNoEditSave,
      arrs: legacyNoEditSave.arrs.map((arr) => Object.assign(structuredClone(arr), { download_uncached: JSON_NULL })),
    };
    expect(posts(requests, "api/config").map((request) => request.body)).toStrictEqual([JSON.stringify(expected)]);
  });

  it("sends every request under the URL base", async () => {
    const { requests } = await bootSettings({ fixture: "config@base" });
    await settingsLoaded();
    submitSettings();
    await savedBody(requests);
    expect(requests.map((request) => `${request.method} ${request.url}`).toSorted()).toStrictEqual([
      "GET /decypharr/api/config",
      "GET /decypharr/api/prune/status",
      "POST /decypharr/api/config",
    ]);
  });
});

describe("a failed config load", () => {
  it.each([
    ["a server error", (): Response => respond(500, "down")],
    ["a network error", new Error("offline")],
    ["malformed JSON", (): Response => respond(200, "{")],
    ["a malformed field", (): Response => respond(200, { hearsay: { follow: "not a list" } })],
  ])("a save after a failed config load posts nothing: %s", async (_name, answer) => {
    const { requests } = await bootSettings({ routes: { "GET api/config": answer } });
    await waitForToast("alert-error Error loading configuration");
    submitSettings();
    await waitForToast("alert-error Settings didn't load. Reload the page before saving.");
    expect({ posts: posts(requests, "api/config"), overlay: isHidden("#loadingOverlay") }).toStrictEqual({
      posts: [],
      overlay: true,
    });
  });
});

describe("the save reply", () => {
  it("shows the server's refusal and hides the overlay", async () => {
    await bootSettings({ routes: { "POST api/config": () => respond(400, "invalid virtual folders: bad") } });
    await settingsLoaded();
    submitSettings();
    const toasts = await shownToasts("alert-error Error saving configuration: invalid virtual folders: bad");
    expect({ toasts: toasts.length, overlay: isHidden("#loadingOverlay") }).toStrictEqual({ toasts: 1, overlay: true });
  });

  it("treats a save reply that is not JSON as a restart and reloads", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout"] });
    onTestFinished(() => {
      vi.useRealTimers();
    });
    const reloads: string[] = [];
    vi.spyOn(globalThis.location, "reload").mockImplementation(() => {
      reloads.push("reload");
    });
    await bootSettings({ routes: { "POST api/config": () => respond(200, "") } });
    await settingsLoaded();
    submitSettings();
    await waitForToast("alert-success Configuration saved successfully! Services are restarting...");
    await vi.advanceTimersByTimeAsync(2000);
    expect({ reloads, overlay: isHidden("#loadingOverlay") }).toStrictEqual({ reloads: ["reload"], overlay: false });
  });

  it("shows warnings from the save and applies without a reload", async () => {
    const reply = { restarted: false, warnings: ["WebDAV auth locks out"] };
    await bootSettings({ routes: { "POST api/config": () => respond(200, reply) } });
    await settingsLoaded();
    submitSettings();
    await waitForToast("alert-success Configuration saved and applied.");
    expect({ toasts: toastTexts(), overlay: isHidden("#loadingOverlay") }).toStrictEqual({
      toasts: ["alert-warning WebDAV auth locks out", "alert-success Configuration saved and applied."],
      overlay: true,
    });
  });
});

describe("saving prune in delete mode", () => {
  it("a cancelled delete confirm posts nothing", async () => {
    const { requests, confirms } = await switchPruneToDelete([false]);
    await vi.waitFor(() => {
      expect(confirms).toHaveLength(1);
    });
    expect({ posts: posts(requests, "api/config"), overlay: isHidden("#loadingOverlay") }).toStrictEqual({
      posts: [],
      overlay: true,
    });
  });

  it("an accepted delete confirm posts once and moves the baseline", async () => {
    const { requests, confirms } = await switchPruneToDelete([true]);
    const first = await savedBody(requests);
    const second = await secondSave(requests);
    expect({ prune: first["prune"], confirms: confirms.length, second: hasKey(second, "prune") }).toStrictEqual({
      prune: { ...reportPrune, mode: "delete" },
      confirms: 1,
      second: false,
    });
  });
});

describe("the review focus cases", () => {
  it("posts once when save is submitted twice", async () => {
    const { requests } = await bootSettings();
    await settingsLoaded();
    submitSettings();
    submitSettings();
    await waitForToast("alert-success Configuration saved and applied.");
    expect({
      posts: posts(requests, "api/config").length,
      enabled: !find('#configForm button[type="submit"]', HTMLButtonElement).disabled,
    }).toStrictEqual({ posts: 1, enabled: true });
  });

  it("renders user-controlled names as text", async () => {
    await bootSettings({ routes: { "GET api/config": serving(hostileConfig) } });
    await settingsLoaded();
    expect({
      images: find("#configForm", HTMLFormElement).querySelectorAll("img").length,
      title: find("[data-vf-title]", HTMLElement).textContent,
      option: find('[name="arr[0].selected_debrid"]', HTMLSelectElement).value,
      repairArr: find('[id="repair.arrs"] [data-arr-name]', HTMLElement).textContent,
    }).toStrictEqual({ images: 0, title: xss, option: xss, repairArr: xss });
  });

  it("reports a missing element by its selector", async () => {
    await bootSettings({ before: () => document.querySelector("#addDebridBtn")?.remove() });
    await expect(shownToasts("alert-error config: missing #addDebridBtn")).resolves.toHaveLength(1);
  });

  it("names an incomplete configuration from the URL", async () => {
    globalThis.history.replaceState({}, "", "/settings?inco=no%20debrid");
    onTestFinished(() => {
      globalThis.history.replaceState({}, "", "/");
    });
    await bootSettings();
    await expect(shownToasts("alert-warning Incomplete configuration: no debrid")).resolves.toHaveLength(1);
  });
});
