import {
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
} from "#ui/lib/browse/actions";
import { createBrowseState, isActionable } from "#ui/lib/browse/model";
import type { BrowseRefs, Browser, Entry, SortKey } from "#ui/lib/browse/types";
import { bootPage, byId, onAction } from "#ui/lib/common/dom";
import { debounce } from "#ui/lib/common/format";

const SEARCH_DELAY_MS = 300;
const SORT_KEYS: readonly SortKey[] = ["name", "size", "mod_time", "active_debrid"];

const entryAt = (browser: Browser, target: EventTarget | null): Entry | undefined => {
  const row = target instanceof Element ? target.closest<HTMLElement>("tr[data-index]") : undefined;
  return row ? browser.state.entries[Number(row.dataset["index"])] : undefined;
};

const withEntry =
  (browser: Browser, act: (entry: Entry) => void) =>
  (target: HTMLElement): void => {
    const entry = entryAt(browser, target);
    if (entry !== undefined) {
      act(entry);
    }
  };

const bindRows = (browser: Browser): void => {
  const list = browser.refs.fileBrowserList;
  onAction(list, "click", {
    open: withEntry(browser, (entry) => {
      if (entry.isDir) {
        navigate(browser, entry.path);
      } else {
        downloadFile(browser, entry.path);
      }
    }),
    download: withEntry(browser, (entry) => {
      downloadFile(browser, entry.path);
    }),
    recheck: withEntry(browser, (entry) => {
      void recheckEntry(browser, entry.name);
    }),
    delete: withEntry(browser, (entry) => {
      void deleteEntry(browser, entry);
    }),
  });
  list.addEventListener("change", (event) => {
    const entry = entryAt(browser, event.target);
    if (event.target instanceof HTMLInputElement && entry !== undefined) {
      selectEntry(browser, entry, event.target.checked);
    }
  });
  list.addEventListener("contextmenu", (event) => {
    const entry = entryAt(browser, event.target);
    if (entry !== undefined) {
      event.preventDefault();
    }
    if (entry !== undefined && isActionable(entry)) {
      showContextMenu(browser, event, entry);
    }
  });
};

const bindContextMenu = (browser: Browser): void => {
  const { contextMenu, contextDownload, contextDelete } = browser.refs;
  document.addEventListener("click", (event) => {
    if (!(event.target instanceof Node) || !contextMenu.contains(event.target)) {
      hideContextMenu(browser);
    }
  });
  contextDownload.addEventListener("click", () => {
    downloadFile(browser, browser.menuEntry?.path ?? "");
  });
  contextDelete.addEventListener("click", () => {
    if (browser.menuEntry !== undefined) {
      void deleteEntry(browser, browser.menuEntry);
    }
  });
};

const bindNavigation = (browser: Browser): void => {
  const { refs } = browser;
  onAction(refs.breadcrumbNav, "click", {
    navigate: (target, event) => {
      event.preventDefault();
      navigate(browser, target.dataset["path"] ?? "/");
    },
  });
  onAction(refs.paginationControls, "click", {
    page: (target) => {
      goToPage(browser, Number(target.dataset["page"]));
    },
  });
  for (const button of document.querySelectorAll<HTMLElement>("[data-sort-key]")) {
    button.addEventListener("click", () => {
      sortBy(browser, button.dataset["sortKey"] ?? "");
    });
  }
  globalThis.addEventListener("popstate", () => {
    readLocation(browser);
    void loadEntries(browser);
  });
};

const bindToolbar = (browser: Browser): void => {
  const { refs } = browser;
  byId("refreshBtn", HTMLButtonElement).addEventListener("click", () => {
    void loadEntries(browser);
  });
  refs.searchInput.addEventListener(
    "input",
    debounce(() => {
      reloadAt(browser, { search: refs.searchInput.value, page: 1 });
    }, SEARCH_DELAY_MS),
  );
  refs.pageSizeSelect.addEventListener("change", () => {
    reloadAt(browser, { perPage: Number(refs.pageSizeSelect.value), page: 1 });
  });
  refs.selectAllCheckbox.addEventListener("change", () => {
    selectAll(browser, refs.selectAllCheckbox.checked);
  });
  byId("bulkDownloadBtn", HTMLButtonElement).addEventListener("click", () => {
    bulkDownload(browser);
  });
  byId("bulkDeleteBtn", HTMLButtonElement).addEventListener("click", () => {
    void bulkDelete(browser);
  });
  byId("clearSelectionBtn", HTMLButtonElement).addEventListener("click", () => {
    clearSelection(browser);
  });
};

const findRefs = (): BrowseRefs => ({
  breadcrumbNav: byId("breadcrumbNav", HTMLElement),
  searchInput: byId("searchInput", HTMLInputElement),
  pageSizeSelect: byId("pageSizeSelect", HTMLSelectElement),
  fileBrowserList: byId("fileBrowserList", HTMLElement),
  paginationInfo: byId("paginationInfo", HTMLElement),
  paginationControls: byId("paginationControls", HTMLElement),
  emptyState: byId("emptyState", HTMLElement),
  emptyStateMessage: byId("emptyStateMessage", HTMLElement),
  emptyStateVirtualFolderLink: byId("emptyStateVirtualFolderLink", HTMLElement),
  currentViewNotice: byId("currentViewNotice", HTMLElement),
  selectAllCheckbox: byId("selectAllCheckbox", HTMLInputElement),
  bulkActionsBar: byId("bulkActionsBar", HTMLElement),
  selectedCount: byId("selectedCount", HTMLElement),
  contextMenu: byId("contextMenu", HTMLElement),
  contextDownload: byId("contextDownload", HTMLElement),
  contextDelete: byId("contextDelete", HTMLElement),
  sortIndicators: new Map(SORT_KEYS.map((key) => [key, byId(`sortIndicator-${key}`, HTMLElement)])),
});

bootPage("browse", async () => {
  const browser: Browser = {
    refs: findRefs(),
    state: createBrowseState(),
    menuEntry: undefined,
    loads: { sequence: 0, controller: undefined },
  };
  bindToolbar(browser);
  bindNavigation(browser);
  bindRows(browser);
  bindContextMenu(browser);
  readLocation(browser);
  await loadEntries(browser);
});
