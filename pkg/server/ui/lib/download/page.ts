import { setButtonLoading } from "#ui/lib/common/button";
import { fetcher } from "#ui/lib/common/http";
import { asRecord, optStr, readJSON } from "#ui/lib/common/json";
import { createToast } from "#ui/lib/common/toast";
import { loadDownloadOptions, saveDownloadOptions } from "#ui/lib/download/options";
import {
  buildAddRequest,
  parseAddResults,
  partitionDrop,
  selectionMessage,
  summarizeResults,
} from "#ui/lib/download/request";

interface DownloadElements {
  form: HTMLFormElement;
  magnetURI: HTMLTextAreaElement;
  torrentFiles: HTMLInputElement;
  nzbURL: HTMLTextAreaElement;
  nzbFile: HTMLInputElement;
  arr: HTMLInputElement;
  downloadAction: HTMLSelectElement;
  downloadUncached: HTMLInputElement;
  rmTrackerUrls: HTMLInputElement;
  downloadFolder: HTMLInputElement;
  debrid: HTMLSelectElement | undefined;
  submit: HTMLButtonElement;
}

const UNKNOWN_ERROR = "Unknown error";
const HIGHLIGHT = ["border-primary", "border-2", "border-dashed", "bg-primary/5"];

const describeError = (error: unknown): string => (error instanceof Error ? error.message : String(error));

const filesOf = (input: HTMLInputElement): File[] => [...(input.files ?? [])];

const fileList = (files: readonly File[]): FileList => {
  const transfer = new DataTransfer();
  for (const file of files) {
    transfer.items.add(file);
  }
  return transfer.files;
};

const applyOptions = (elements: DownloadElements, defaultFolder: string): void => {
  const options = loadDownloadOptions(localStorage, defaultFolder);
  elements.arr.value = options.category;
  elements.downloadAction.value = options.action;
  elements.downloadUncached.checked = options.uncached;
  if (!elements.rmTrackerUrls.disabled) {
    elements.rmTrackerUrls.checked = options.rmTrackerUrls;
  }
  elements.downloadFolder.value = options.folder;
};

const storeOptions = (elements: DownloadElements): void => {
  const options = {
    category: elements.arr.value,
    action: elements.downloadAction.value,
    uncached: elements.downloadUncached.checked,
    rmTrackerUrls: elements.rmTrackerUrls.checked,
    folder: elements.downloadFolder.value,
  };
  saveDownloadOptions(localStorage, options, !elements.rmTrackerUrls.disabled);
};

const refusal = (body: string): string => {
  try {
    return optStr(asRecord(JSON.parse(body), "response"), "error") ?? UNKNOWN_ERROR;
  } catch {
    return body.trim() === "" ? UNKNOWN_ERROR : body.trim();
  }
};

const postDownloads = async (form: FormData): Promise<unknown> => {
  const response = await fetcher("/api/add", { method: "POST", body: form, headers: {} });
  if (!response.ok) {
    throw new Error(refusal(await response.text()));
  }
  return readJSON(response);
};

const clearInputs = (elements: DownloadElements): void => {
  elements.magnetURI.value = "";
  elements.torrentFiles.value = "";
  elements.nzbURL.value = "";
  elements.nzbFile.value = "";
};

const report = (elements: DownloadElements, answer: unknown): void => {
  const summary = summarizeResults(parseAddResults(answer));
  for (const toast of summary.toasts) {
    createToast(toast.message, toast.type);
  }
  if (summary.clear) {
    clearInputs(elements);
  }
};

const submitDownloads = async (elements: DownloadElements): Promise<void> => {
  const outcome = buildAddRequest({
    magnets: elements.magnetURI.value,
    torrentFiles: filesOf(elements.torrentFiles),
    nzbURLs: elements.nzbURL.value,
    nzbFiles: filesOf(elements.nzbFile),
    arr: elements.arr.value,
    downloadFolder: elements.downloadFolder.value,
    action: elements.downloadAction.value,
    downloadUncached: elements.downloadUncached.checked,
    rmTrackerUrls: elements.rmTrackerUrls.checked,
    debrid: elements.debrid?.value,
  });
  if ("warning" in outcome) {
    createToast(outcome.warning, "warning");
    return;
  }
  setButtonLoading(elements.submit, true);
  try {
    report(elements, await postDownloads(outcome.form));
  } catch (error) {
    createToast(`Error adding downloads: ${describeError(error)}`, "error");
  } finally {
    setButtonLoading(elements.submit, false);
  }
};

const announceSelection = (kind: "torrent" | "nzb", files: readonly File[]): void => {
  if (files.length > 0) {
    createToast(selectionMessage(kind, files), "info");
  }
};

const takeDrop = (elements: DownloadElements, event: DragEvent): void => {
  const { torrents, nzbs } = partitionDrop([...(event.dataTransfer?.files ?? [])]);
  if (torrents.length > 0) {
    elements.torrentFiles.files = fileList(torrents);
    announceSelection("torrent", torrents);
  }
  if (nzbs.length > 0) {
    elements.nzbFile.files = fileList(nzbs);
    announceSelection("nzb", nzbs);
  }
  if (torrents.length === 0 && nzbs.length === 0) {
    createToast("Please drop .torrent or .nzb files only", "warning");
  }
};

const bindDragAndDrop = (elements: DownloadElements): void => {
  const { form } = elements;
  for (const name of ["dragenter", "dragover", "dragleave", "drop"]) {
    form.addEventListener(name, (event) => {
      event.preventDefault();
      event.stopPropagation();
      const highlighted = name === "dragenter" || name === "dragover";
      for (const highlight of HIGHLIGHT) {
        form.classList.toggle(highlight, highlighted);
      }
    });
  }
  form.addEventListener("drop", (event) => {
    takeDrop(elements, event);
  });
};

const loadMagnetFromURL = (elements: DownloadElements): void => {
  const magnet = new URLSearchParams(globalThis.location.search).get("magnet");
  if (magnet === null || magnet === "") {
    return;
  }
  elements.magnetURI.value = magnet;
  history.replaceState({}, document.title, globalThis.location.pathname);
  createToast("Magnet link loaded from URL", "info");
};

const bindDownloadPage = (elements: DownloadElements, defaultFolder: string): void => {
  applyOptions(elements, defaultFolder);
  elements.form.addEventListener("submit", (event) => {
    event.preventDefault();
    void submitDownloads(elements);
  });
  for (const control of [elements.arr, elements.downloadAction, elements.downloadUncached, elements.rmTrackerUrls]) {
    control.addEventListener("change", () => {
      storeOptions(elements);
    });
  }
  elements.downloadFolder.addEventListener("change", () => {
    storeOptions(elements);
  });
  elements.torrentFiles.addEventListener("change", () => {
    announceSelection("torrent", filesOf(elements.torrentFiles));
  });
  elements.nzbFile.addEventListener("change", () => {
    announceSelection("nzb", filesOf(elements.nzbFile));
  });
  bindDragAndDrop(elements);
  loadMagnetFromURL(elements);
};

export type { DownloadElements };
export { bindDownloadPage };
