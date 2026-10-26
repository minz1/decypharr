import { expect, vi } from "vitest";

import { stubAPI } from "#ui/lib/testing/api";
import { element } from "#ui/lib/testing/dom";
import { loadFixture } from "#ui/lib/testing/fixture";
import { toasts } from "#ui/lib/testing/toast";

type Respond = () => Promise<Response>;

interface Booted {
  requests: ReturnType<typeof stubAPI>;
  navigations: string[];
}

interface Boot {
  fixture?: string;
  config?: Respond;
  complete?: Respond;
}

const REDIRECT_WAIT = { timeout: 3000 };

const stubFetch = (boot: Boot): ReturnType<typeof stubAPI> =>
  stubAPI(
    { "GET /api/config": async () => boot.config?.() ?? Response.json({}) },
    async () => boot.complete?.() ?? Response.json({ success: true }),
  );

const bootSetup = async (boot: Boot = {}): Promise<Booted> => {
  vi.resetModules();
  loadFixture(boot.fixture ?? "setup");
  const booted: Booted = { requests: stubFetch(boot), navigations: [] };
  vi.spyOn(globalThis.location, "assign").mockImplementation((url: string | URL) => {
    booted.navigations.push(String(url));
  });
  await import("#ui/pages/setup");
  await vi.waitFor(() => {
    expect(document.querySelector("#loading-state.hidden")).not.toBeNull();
  });
  return booted;
};

const value = (id: string): string => element(`#${id}`, HTMLInputElement).value;

const fill = (values: Readonly<Record<string, string>>): void => {
  for (const [id, typed] of Object.entries(values)) {
    element(`#${id}`, HTMLInputElement).value = typed;
  }
};

const click = (id: string): void => {
  element(`#${id}`, HTMLButtonElement).click();
};

const hidden = (id: string): boolean => element(`#${id}`, HTMLElement).classList.contains("hidden");

const visibleSteps = (): string[] => [...document.querySelectorAll(".setup-step:not(.hidden)")].map((step) => step.id);

const shownError = (): string => (hidden("error-state") ? "" : element("#error-message", HTMLElement).textContent);

const marked = (): string[] => [...document.querySelectorAll(".steps .step-primary")].map((step) => step.id);

const errorToasts = (): string[] => toasts("error");

const choose = (id: string): boolean => {
  const radio = element(`#${id}`, HTMLInputElement);
  radio.checked = true;
  radio.dispatchEvent(new Event("change", { bubbles: true }));
  return !hidden("rclone-options");
};

const passDebrid = (): void => {
  element("#debrid-provider", HTMLSelectElement).value = "torbox";
  fill({ "debrid-api-key": "key" });
  click("debrid-next-btn");
};

const fillUsenet = (host: string): void => {
  fill({ "usenet-host": host, "usenet-port": "563", "usenet-username": "user", "usenet-password": "pass" });
};

const passDownloadAndMount = (): void => {
  fill({ "download-folder": "/downloads" });
  click("download-next-btn");
  fill({ "mount-path": "/mnt/decypharr", "cache-dir": "/cache" });
  click("mount-next-btn");
};

const walkToOverview = (): void => {
  fill({ "auth-username": "admin", "auth-password": "secret", "auth-confirm-password": "secret" });
  click("auth-next-btn");
  passDebrid();
  fillUsenet("news.example.com");
  element("#usenet-ssl", HTMLInputElement).checked = true;
  click("usenet-next-btn");
  passDownloadAndMount();
};

const walkTokenOnly = (): void => {
  click("token-only-btn");
  passDebrid();
  click("skip-usenet-btn");
  passDownloadAndMount();
};

const posted = (booted: Booted): Booted["requests"] => booted.requests.filter((request) => request.method === "POST");

const postedBodies = (booted: Booted): unknown[] => posted(booted).map((request): unknown => JSON.parse(request.body));

export type { Booted, Respond };
export {
  REDIRECT_WAIT,
  bootSetup,
  choose,
  click,
  errorToasts,
  fill,
  fillUsenet,
  hidden,
  marked,
  passDebrid,
  passDownloadAndMount,
  posted,
  postedBodies,
  shownError,
  value,
  visibleSteps,
  walkTokenOnly,
  walkToOverview,
};
