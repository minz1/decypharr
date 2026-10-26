import {
  browseAPIURL,
  browseQuery,
  downloadURL,
  entryId,
  isActionable,
  nextSort,
  parseBrowsePage,
  parseHealth,
  readBrowseLocation,
} from "#ui/lib/browse/model";
import type { BrowseLocation, Browser, Entry, Health } from "#ui/lib/browse/types";
import { renderBrowser, renderHealthBadges, renderSelection, renderSortIndicators } from "#ui/lib/browse/view";
import { describeError } from "#ui/lib/common/errors";
import { fetcher } from "#ui/lib/common/http";
import { readJSON } from "#ui/lib/common/json";
import { createToast } from "#ui/lib/common/toast";
import { joinURL, readURLBase } from "#ui/lib/common/url";
import { askConfirm } from "#ui/lib/confirm";

const isAbort = (error: unknown): boolean =>
  (error instanceof DOMException || error instanceof Error) && error.name === "AbortError";

const fetchHealth = async (name: string): Promise<[string, Health | undefined]> => {
  try {
    const response = await fetcher(`/api/repair/health/${encodeURIComponent(name)}`);
    return [name, response.ok ? parseHealth(await readJSON(response)) : undefined];
  } catch {
    return [name, undefined];
  }
};

const loadHealth = async (browser: Browser): Promise<void> => {
  const names = [
    ...new Set(browser.state.entries.filter((entry) => entry.kind === "entry").map((entry) => entry.name)),
  ];
  if (names.length === 0) {
    return;
  }
  for (const [name, health] of await Promise.all(names.map(async (each) => fetchHealth(each)))) {
    if (health !== undefined) {
      browser.state.health.set(name, health);
    }
  }
  renderHealthBadges(browser.refs, browser.state);
};

const fetchPage = async (browser: Browser, signal: AbortSignal): Promise<unknown> => {
  const response = await fetcher(browseAPIURL(browser.state), { signal });
  if (!response.ok) {
    throw new Error("Failed to load directory");
  }
  return readJSON(response);
};

const startLoad = (loads: Browser["loads"]): { sequence: number; signal: AbortSignal } => {
  loads.sequence += 1;
  loads.controller?.abort();
  const controller = new AbortController();
  loads.controller = controller;
  return { sequence: loads.sequence, signal: controller.signal };
};

const loadEntries = async (browser: Browser): Promise<void> => {
  const { sequence, signal } = startLoad(browser.loads);
  try {
    const page = parseBrowsePage(await fetchPage(browser, signal));
    if (sequence !== browser.loads.sequence) {
      return;
    }
    Object.assign(browser.state, page);
    renderBrowser(browser.refs, browser.state);
    void loadHealth(browser);
  } catch (error) {
    if (!isAbort(error)) {
      createToast("Failed to load directory", "error");
    }
  }
};

const pushLocation = (browser: Browser): void => {
  globalThis.history.pushState({}, "", `${globalThis.location.pathname}${browseQuery(browser.state)}`);
};

const readLocation = (browser: Browser): void => {
  const location: BrowseLocation = readBrowseLocation(globalThis.location.search);
  Object.assign(browser.state, location);
  browser.refs.searchInput.value = location.search;
  browser.refs.pageSizeSelect.value = String(location.perPage);
  renderSortIndicators(browser.refs, browser.state);
};

const reloadAt = (browser: Browser, change: Partial<BrowseLocation>): void => {
  Object.assign(browser.state, change);
  pushLocation(browser);
  void loadEntries(browser);
};

const entryCheckboxes = (browser: Browser): HTMLInputElement[] => [
  ...browser.refs.fileBrowserList.querySelectorAll<HTMLInputElement>(".entry-checkbox"),
];

const clearSelection = (browser: Browser): void => {
  browser.state.selected.clear();
  for (const box of entryCheckboxes(browser)) {
    box.checked = false;
  }
  renderSelection(browser.refs, browser.state);
};

const navigate = (browser: Browser, path: string): void => {
  clearSelection(browser);
  reloadAt(browser, { path, page: 1 });
};

const goToPage = (browser: Browser, page: number): void => {
  if (page >= 1 && page <= browser.state.totalPages) {
    reloadAt(browser, { page });
  }
};

const sortBy = (browser: Browser, key: string): void => {
  const sort = nextSort(browser.state, key);
  if (sort !== undefined) {
    Object.assign(browser.state, sort, { page: 1 });
    renderSortIndicators(browser.refs, browser.state);
    reloadAt(browser, {});
  }
};

const selectEntry = (browser: Browser, entry: Entry, checked: boolean): void => {
  if (checked) {
    browser.state.selected.set(entryId(entry), entry);
  } else {
    browser.state.selected.delete(entryId(entry));
  }
  renderSelection(browser.refs, browser.state);
};

const selectAll = (browser: Browser, checked: boolean): void => {
  const { entries, selected } = browser.state;
  for (const entry of entries) {
    if (!checked) {
      selected.delete(entryId(entry));
    } else if (isActionable(entry)) {
      selected.set(entryId(entry), entry);
    }
  }
  for (const box of entryCheckboxes(browser)) {
    box.checked = checked && !box.disabled;
  }
  renderSelection(browser.refs, browser.state);
};

const hideContextMenu = (browser: Browser): void => {
  browser.refs.contextMenu.classList.add("hidden");
};

const showContextMenu = (browser: Browser, event: MouseEvent, entry: Entry): void => {
  const { contextMenu, contextDownload, contextDelete } = browser.refs;
  event.preventDefault();
  event.stopPropagation();
  contextMenu.style.left = `${event.pageX}px`;
  contextMenu.style.top = `${event.pageY}px`;
  contextMenu.classList.remove("hidden");
  contextDownload.classList.toggle("hidden", entry.isDir);
  contextDelete.classList.toggle("hidden", !entry.canDelete);
  browser.menuEntry = entry;
};

const downloadFile = (browser: Browser, path: string): void => {
  hideContextMenu(browser);
  const url = downloadURL(path);
  if (url !== undefined) {
    globalThis.open(joinURL(readURLBase(document), url), "_blank");
  }
};

const deleteQuestion = (name: string): string =>
  `Permanently delete the original item "${name}"?\n\nThis removes it from every library and virtual folder, and removes its placement from the provider. This cannot be undone from Decypharr.`;

const sendDelete = async (infoHash: string): Promise<void> => {
  const response = await fetcher(`/api/browse/torrents/${infoHash}`, { method: "DELETE" });
  if (!response.ok) {
    throw new Error("Failed to delete entry");
  }
};

const deleteEntry = async (browser: Browser, entry: Entry): Promise<void> => {
  hideContextMenu(browser);
  if (!(await askConfirm(deleteQuestion(entry.name)))) {
    return;
  }
  try {
    await sendDelete(entry.infoHash);
    createToast("Item deleted successfully", "success");
    void loadEntries(browser);
  } catch {
    createToast("Failed to delete item", "error");
  }
};

const postRecheck = async (name: string): Promise<Health> => {
  const response = await fetcher(`/api/repair/health/${encodeURIComponent(name)}/check`, { method: "POST" });
  if (!response.ok) {
    const text = await response.text();
    throw new Error(text === "" ? `HTTP ${response.status}` : text);
  }
  return parseHealth(await readJSON(response));
};

const recheckEntry = async (browser: Browser, name: string): Promise<void> => {
  try {
    createToast(`Rechecking ${name}…`, "info");
    const health = await postRecheck(name);
    browser.state.health.set(name, health);
    renderHealthBadges(browser.refs, browser.state);
    createToast(`Health: ${health.status}`, health.status === "broken" ? "warning" : "success");
  } catch (error) {
    createToast(`Recheck failed: ${describeError(error)}`, "error");
  }
};

const bulkDownload = (browser: Browser): void => {
  const files = [...browser.state.selected.values()].filter((entry) => !entry.isDir);
  if (files.length === 0) {
    createToast("No files selected for download", "warning");
    return;
  }
  for (const file of files) {
    downloadFile(browser, file.path);
  }
  createToast(`Downloading ${files.length} file(s)`, "success");
};

const sendBulkDelete = async (ids: readonly string[]): Promise<void> => {
  const response = await fetcher("/api/browse/torrents/batch", {
    method: "DELETE",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ ids }),
  });
  if (!response.ok) {
    throw new Error("Failed to delete selected items");
  }
};

const bulkDeleteQuestion = (targets: readonly Entry[]): string =>
  `Permanently delete ${targets.length} original item(s)?\n\n${targets.map((entry) => entry.name).join("\n")}\n\nThis removes them from every library and virtual folder, and removes their provider placements. This cannot be undone from Decypharr.`;

const confirmBulkDelete = async (targets: readonly Entry[]): Promise<boolean> => {
  if (targets.length === 0) {
    createToast("No items selected for deletion", "warning");
    return false;
  }
  return askConfirm(bulkDeleteQuestion(targets));
};

const bulkDelete = async (browser: Browser): Promise<void> => {
  const targets = [...browser.state.selected.values()].filter((entry) => entry.canDelete && entry.infoHash !== "");
  if (!(await confirmBulkDelete(targets))) {
    return;
  }
  const ids = [...new Set(targets.map((entry) => entry.infoHash))];
  try {
    await sendBulkDelete(ids);
    createToast(`Deleted ${ids.length} item(s)`, "success");
    clearSelection(browser);
    void loadEntries(browser);
  } catch {
    createToast("Failed to delete selected items", "error");
  }
};

export {
  bulkDelete,
  bulkDownload,
  clearSelection,
  deleteEntry,
  downloadFile,
  goToPage,
  hideContextMenu,
  loadEntries,
  navigate,
  readLocation,
  recheckEntry,
  reloadAt,
  selectAll,
  selectEntry,
  showContextMenu,
  sortBy,
};
