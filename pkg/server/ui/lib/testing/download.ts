import { expect, vi } from "vitest";

import { stubFormAPI } from "#ui/lib/testing/api";
import { element } from "#ui/lib/testing/dom";
import { loadFixture } from "#ui/lib/testing/fixture";
import { shownToasts } from "#ui/lib/testing/toast";

type Respond = () => Promise<Response>;

interface Boot {
  fixture?: string;
  respond?: Respond;
  url?: string;
  stored?: Readonly<Record<string, string>>;
  prepare?: () => void;
}

const prepareState = (boot: Boot): void => {
  localStorage.clear();
  for (const [key, stored] of Object.entries(boot.stored ?? {})) {
    localStorage.setItem(key, stored);
  }
  history.replaceState({}, "", boot.url ?? "/download");
  loadFixture(boot.fixture ?? "download");
  boot.prepare?.();
};

const bootDownload = async (boot: Boot = {}): Promise<ReturnType<typeof stubFormAPI>> => {
  vi.resetModules();
  prepareState(boot);
  const requests = stubFormAPI(async () => boot.respond?.() ?? Response.json([{ status: "queued" }]));
  await import("#ui/pages/download");
  return requests;
};

const textArea = (id: string): HTMLTextAreaElement => element(`#${id}`, HTMLTextAreaElement);

const input = (id: string): HTMLInputElement => element(`#${id}`, HTMLInputElement);

const select = (id: string): HTMLSelectElement => element(`#${id}`, HTMLSelectElement);

const names = (files: FileList | null): string[] => [...(files ?? [])].map((file) => file.name);

const fileList = (files: readonly File[]): FileList => {
  const transfer = new DataTransfer();
  for (const file of files) {
    transfer.items.add(file);
  }
  return transfer.files;
};

const dropEvent = (files: readonly File[]): Event => {
  const drop = new Event("drop", { bubbles: true, cancelable: true });
  Object.defineProperty(drop, "dataTransfer", { value: { files: fileList(files) } });
  return drop;
};

const giveFiles = (id: string, files: readonly File[]): HTMLInputElement => {
  const target = element(`#${id}`, HTMLInputElement);
  let current = fileList(files);
  Object.defineProperty(target, "files", {
    get: () => current,
    set: (next: FileList) => {
      current = next;
    },
  });
  return target;
};

const submitTwice = async (requests: ReturnType<typeof stubFormAPI>): Promise<void> => {
  element("#submitDownload", HTMLButtonElement).click();
  element("#submitDownload", HTMLButtonElement).click();
  await vi.waitFor(() => {
    expect(document.querySelector("#submitDownload:not([disabled])")).not.toBeNull();
  });
  expect(requests.length).toBeLessThanOrEqual(1);
};

const toasts = (): string[] => shownToasts().map((toast) => `alert-${toast.type}: ${toast.message}`);

const waitForToasts = async (count: number): Promise<string[]> => {
  await vi.waitFor(() => {
    expect(toasts().length).toBeGreaterThanOrEqual(count);
  });
  return toasts();
};

export {
  bootDownload,
  dropEvent,
  fileList,
  giveFiles,
  input,
  names,
  select,
  submitTwice,
  textArea,
  toasts,
  waitForToasts,
};
