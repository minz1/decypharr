import { asArray, asRecord, optStr } from "#ui/lib/common/json";
import type { ToastType } from "#ui/lib/common/toast";

interface AddRequest {
  magnets: string;
  torrentFiles: readonly File[];
  nzbURLs: string;
  nzbFiles: readonly File[];
  arr: string;
  downloadFolder: string;
  action: string;
  downloadUncached: boolean;
  rmTrackerUrls: boolean;
  debrid: string | undefined;
}

interface AddResult {
  status: string | undefined;
  error: string | undefined;
}

interface Summary {
  toasts: { message: string; type: ToastType }[];
  clear: boolean;
}

const MAX_ITEMS = 100;

const lines = (text: string): string[] =>
  text
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "");

const appendFiles = (form: FormData, name: string, files: readonly File[]): void => {
  for (const file of files) {
    form.append(name, file);
  }
};

const appendOptions = (form: FormData, request: AddRequest): void => {
  form.append("arr", request.arr);
  form.append("downloadFolder", request.downloadFolder);
  form.append("action", request.action);
  form.append("downloadUncached", String(request.downloadUncached));
  form.append("rmTrackerUrls", String(request.rmTrackerUrls));
  if (request.debrid !== undefined) {
    form.append("debrid", request.debrid);
  }
};

const itemsForm = (request: AddRequest, urls: readonly string[], nzbURLs: readonly string[]): FormData => {
  const form = new FormData();
  if (urls.length > 0) {
    form.append("urls", urls.join("\n"));
  }
  appendFiles(form, "files", request.torrentFiles);
  if (nzbURLs.length > 0) {
    form.append("nzbURLs", nzbURLs.join("\n"));
  }
  appendFiles(form, "nzbFiles", request.nzbFiles);
  return form;
};

const buildAddRequest = (request: AddRequest): { form: FormData } | { warning: string } => {
  const urls = lines(request.magnets);
  const nzbURLs = lines(request.nzbURLs);
  const total = urls.length + request.torrentFiles.length + nzbURLs.length + request.nzbFiles.length;
  if (total === 0) {
    return { warning: "Please provide at least one torrent or NZB" };
  }
  if (total > MAX_ITEMS) {
    return { warning: "Please submit up to 100 items at a time" };
  }
  const form = itemsForm(request, urls, nzbURLs);
  appendOptions(form, request);
  return { form };
};

const parseAddResults = (value: unknown): AddResult[] =>
  asArray(value, "response").map((item, index) => {
    const result = asRecord(item, `response[${index}]`);
    return { status: optStr(result, "status"), error: optStr(result, "error") };
  });

const errorList = (failures: readonly AddResult[]): string =>
  `Errors occurred while adding items:\n${failures.map((failure) => `• ${failure.error ?? "Unknown error"}`).join("\n")}`;

const summarizeResults = (results: readonly AddResult[]): Summary => {
  const failures = results.filter((result) => result.status === "error");
  const successes = results.length - failures.length;
  if (failures.length > 0) {
    const details = { message: errorList(failures), type: "error" } as const;
    const partial = {
      message: `Added ${successes} item(s) with ${failures.length} error(s)`,
      type: "warning",
    } as const;
    return { toasts: successes > 0 ? [partial, details] : [details], clear: false };
  }
  if (successes > 0) {
    const plural = successes > 1 ? "s" : "";
    return { toasts: [{ message: `Successfully added ${successes} item${plural}!`, type: "success" }], clear: true };
  }
  return { toasts: [{ message: "No items were added", type: "warning" }], clear: false };
};

const selectionMessage = (kind: "torrent" | "nzb", files: readonly File[]): string => {
  const names = files.map((file) => file.name).join(", ");
  const plural = files.length > 1 ? "s" : "";
  return `Selected ${files.length} ${kind === "nzb" ? "NZB" : "torrent"} file${plural}: ${names}`;
};

const partitionDrop = (files: readonly File[]): { torrents: File[]; nzbs: File[] } => {
  const named = (extension: string): File[] => files.filter((file) => file.name.toLowerCase().endsWith(extension));
  return { torrents: named(".torrent"), nzbs: named(".nzb").slice(0, 1) };
};

export type { AddRequest };
export { buildAddRequest, parseAddResults, partitionDrop, selectionMessage, summarizeResults };
