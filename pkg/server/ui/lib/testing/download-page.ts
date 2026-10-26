import { expect, vi } from "vitest";

import { loadFixture } from "#ui/lib/testing/fixture";

type Respond = () => Promise<Response>;

interface Request {
  url: string;
  method: string;
  fields: [string, FormDataEntryValue][];
}

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

const bootDownload = async (boot: Boot = {}): Promise<Request[]> => {
  vi.resetModules();
  prepareState(boot);
  const requests: Request[] = [];
  vi.stubGlobal("fetch", async (url: string, init: RequestInit): Promise<Response> => {
    const fields = init.body instanceof FormData ? [...init.body.entries()] : [];
    requests.push({ url, method: init.method ?? "GET", fields });
    return boot.respond?.() ?? Response.json([{ status: "queued" }]);
  });
  await import("#ui/pages/download");
  return requests;
};

const element = <Wanted extends HTMLElement>(id: string, type: abstract new () => Wanted): Wanted => {
  const found = document.querySelector(`#${id}`);
  if (!(found instanceof type)) {
    throw new TypeError(`the download fixture has no #${id}`);
  }
  return found;
};

const textArea = (id: string): HTMLTextAreaElement => element(id, HTMLTextAreaElement);

const input = (id: string): HTMLInputElement => element(id, HTMLInputElement);

const select = (id: string): HTMLSelectElement => element(id, HTMLSelectElement);

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
  const target = element(id, HTMLInputElement);
  let current = fileList(files);
  Object.defineProperty(target, "files", {
    get: () => current,
    set: (next: FileList) => {
      current = next;
    },
  });
  return target;
};

const submitTwice = async (requests: Request[]): Promise<void> => {
  element("submitDownload", HTMLButtonElement).click();
  element("submitDownload", HTMLButtonElement).click();
  await vi.waitFor(() => {
    expect(document.querySelector("#submitDownload:not([disabled])")).not.toBeNull();
  });
  expect(requests.length).toBeLessThanOrEqual(1);
};

const toasts = (): string[] =>
  [...document.querySelectorAll(".toast-container .alert")].map((toast) => {
    const kind = [...toast.classList].find((name) => name.startsWith("alert-")) ?? "";
    return `${kind}: ${toast.querySelector("[data-toast-message]")?.textContent ?? ""}`;
  });

const waitForToasts = async (count: number): Promise<string[]> => {
  await vi.waitFor(() => {
    expect(toasts().length).toBeGreaterThanOrEqual(count);
  });
  return toasts();
};

export {
  bootDownload,
  dropEvent,
  element,
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
