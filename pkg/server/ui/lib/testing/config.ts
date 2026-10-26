import { expect, inject, onTestFinished, vi } from "vitest";

import { isRecord } from "#ui/lib/common/json";
import { stubAPI } from "#ui/lib/testing/api";
import { element } from "#ui/lib/testing/dom";
import { loadFixture } from "#ui/lib/testing/fixture";
import { labelledToasts } from "#ui/lib/testing/toast";

type Request = ReturnType<typeof stubAPI>[number];

interface BootOptions {
  fixture?: string;
  routes?: Parameters<typeof stubAPI>[0];
  answers?: readonly boolean[];
  before?: () => void;
}

interface Page {
  requests: ReturnType<typeof stubAPI>;
  confirms: string[];
}

const HTTP_OK = 200;
const HTTP_NOT_FOUND = 404;

const respond = (status: number, body: unknown): Response =>
  typeof body === "string" ? new Response(body, { status }) : Response.json(body, { status });

const defaultRoutes = (fixture: string): Parameters<typeof stubAPI>[0] => ({
  "GET api/config": () => respond(HTTP_OK, inject("configResponses")[fixture]),
  "GET api/prune/status": () => respond(HTTP_OK, '{"mode":"report","candidates":null}'),
  "POST api/config": () => respond(HTTP_OK, { status: "success", restarted: false }),
});

const stubFetch = (routes: Parameters<typeof stubAPI>[0]): Request[] =>
  stubAPI(Object.fromEntries(Object.entries(routes).map(([key, answer]) => [key.replace(" ", " /"), answer])), () =>
    respond(HTTP_NOT_FOUND, "not found"),
  );

const answerConfirms = (answers: readonly boolean[]): string[] => {
  const confirms: string[] = [];
  const dialog = element("#confirmDialog", HTMLDialogElement);
  const observer = new MutationObserver(() => {
    if (!dialog.open) {
      return;
    }
    confirms.push(element("#confirmDialogMessage", HTMLElement).textContent);
    const answer = (answers[confirms.length - 1] ?? false) ? "ok" : "cancel";
    element(`#confirmDialog [data-answer="${answer}"]`, HTMLButtonElement).click();
  });
  observer.observe(dialog, { attributeFilter: ["open"] });
  onTestFinished(() => {
    observer.disconnect();
  });
  return confirms;
};

const bootSettings = async (options: BootOptions = {}): Promise<Page> => {
  const fixture = options.fixture ?? "config";
  vi.resetModules();
  loadFixture(fixture);
  options.before?.();
  const requests = stubFetch({ ...defaultRoutes(fixture), ...options.routes });
  const confirms = answerConfirms(options.answers ?? []);
  await import("#ui/pages/config");
  return { requests, confirms };
};

const settingsLoaded = async (): Promise<void> => {
  await vi.waitFor(() => {
    expect(document.querySelector(".debrid-config")).not.toBeNull();
  });
};

const waitForToast = async (text: string): Promise<void> => {
  await vi.waitFor(() => {
    expect(labelledToasts()).toContain(text);
  });
};

const shownToasts = async (text: string): Promise<string[]> => {
  await waitForToast(text);
  return labelledToasts();
};

const member = (value: unknown, key: string): unknown => (isRecord(value) ? value[key] : undefined);

const lastItem = (value: unknown): unknown => (Array.isArray(value) ? value.at(-1) : undefined);

const hasKey = (value: unknown, key: string): boolean => isRecord(value) && Object.hasOwn(value, key);

const isHidden = (selector: string): boolean => element(selector, HTMLElement).classList.contains("hidden");

const click = (selector: string): void => {
  element(selector, HTMLElement).click();
};

const fill = (selector: string, value: string): void => {
  const field = element(selector, HTMLElement);
  if (field instanceof HTMLInputElement || field instanceof HTMLSelectElement || field instanceof HTMLTextAreaElement) {
    field.value = value;
    field.dispatchEvent(new Event("input", { bubbles: true }));
    field.dispatchEvent(new Event("change", { bubbles: true }));
  }
};

const submitSettings = (): void => {
  element("#configForm", HTMLFormElement).dispatchEvent(new Event("submit", { cancelable: true }));
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
  waitForToast,
};
