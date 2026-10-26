import { bootPage, byId, onAction } from "#ui/lib/common/dom";
import { debounce } from "#ui/lib/common/format";
import {
  deleteSelectedTorrents,
  deleteTorrent,
  filterBy,
  goToPage,
  hideContextMenu,
  loadTorrents,
  runContextAction,
  showContextMenu,
  startAutoRefresh,
  toggleSelectAll,
  toggleTorrentSelection,
} from "#ui/lib/dashboard/actions";
import { createDashboardState, parseSort } from "#ui/lib/dashboard/model";
import type { Dashboard } from "#ui/lib/dashboard/types";

const SEARCH_DELAY_MS = 300;

const rowOf = (event: Event): HTMLElement | undefined =>
  event.target instanceof Element ? (event.target.closest<HTMLElement>("tr[data-hash]") ?? undefined) : undefined;

const bindRows = (dashboard: Dashboard): void => {
  const { torrentsList } = dashboard.refs;
  const deleteRow = (event: Event, removeFromDebrid: boolean): void => {
    const { hash = "", category = "" } = rowOf(event)?.dataset ?? {};
    void deleteTorrent(dashboard, { hash, category }, removeFromDebrid);
  };
  onAction(torrentsList, "click", {
    delete: (_target, event) => {
      deleteRow(event, false);
    },
    "delete-debrid": (_target, event) => {
      deleteRow(event, true);
    },
  });
  torrentsList.addEventListener("change", (event) => {
    const box = event.target;
    if (box instanceof HTMLInputElement && box.classList.contains("torrent-select")) {
      toggleTorrentSelection(dashboard, box.dataset["hash"] ?? "", box.checked);
    }
  });
  torrentsList.addEventListener("contextmenu", (event) => {
    const row = rowOf(event);
    if (row) {
      event.preventDefault();
      showContextMenu(dashboard, event, row);
    }
  });
};

const bindContextMenu = (dashboard: Dashboard): void => {
  const menu = dashboard.refs.contextMenu;
  document.addEventListener("click", (event) => {
    if (!(event.target instanceof Node) || !menu.contains(event.target)) {
      hideContextMenu(dashboard);
    }
  });
  menu.addEventListener("click", (event) => {
    const action = event.target instanceof Element ? event.target.closest<HTMLElement>("[data-action]") : undefined;
    const name = action?.dataset["action"] ?? "";
    if (name !== "") {
      void runContextAction(dashboard, name);
      hideContextMenu(dashboard);
    }
  });
};

const bindFilters = (dashboard: Dashboard): void => {
  const search = byId("searchInput", HTMLInputElement);
  const stateFilter = byId("stateFilter", HTMLSelectElement);
  const sortSelector = byId("sortSelector", HTMLSelectElement);
  const { categoryFilter } = dashboard.refs;
  search.addEventListener(
    "input",
    debounce(() => {
      filterBy(dashboard, { search: search.value });
    }, SEARCH_DELAY_MS),
  );
  categoryFilter.addEventListener("change", () => {
    filterBy(dashboard, { category: categoryFilter.value });
  });
  stateFilter.addEventListener("change", () => {
    filterBy(dashboard, { stateFilter: stateFilter.value });
  });
  sortSelector.addEventListener("change", () => {
    filterBy(dashboard, parseSort(sortSelector.value));
  });
};

const bindToolbar = (dashboard: Dashboard): void => {
  const { refs } = dashboard;
  refs.refreshBtn.addEventListener("click", () => {
    void loadTorrents(dashboard);
  });
  refs.batchDeleteBtn.addEventListener("click", () => {
    void deleteSelectedTorrents(dashboard, false);
  });
  refs.batchDeleteDebridBtn.addEventListener("click", () => {
    void deleteSelectedTorrents(dashboard, true);
  });
  refs.selectAll.addEventListener("change", () => {
    toggleSelectAll(dashboard, refs.selectAll.checked);
  });
  onAction(refs.paginationControls, "click", {
    page: (target) => {
      goToPage(dashboard, Number(target.dataset["page"]));
    },
  });
};

bootPage("dashboard", async () => {
  const dashboard: Dashboard = {
    refs: {
      torrentsList: byId("torrentsList", HTMLElement),
      categoryFilter: byId("categoryFilter", HTMLSelectElement),
      selectAll: byId("selectAll", HTMLInputElement),
      batchDeleteBtn: byId("batchDeleteBtn", HTMLButtonElement),
      batchDeleteDebridBtn: byId("batchDeleteDebridBtn", HTMLButtonElement),
      refreshBtn: byId("refreshBtn", HTMLButtonElement),
      contextMenu: byId("torrentContextMenu", HTMLElement),
      paginationControls: byId("paginationControls", HTMLElement),
      paginationInfo: byId("paginationInfo", HTMLElement),
      emptyState: byId("emptyState", HTMLElement),
    },
    state: createDashboardState(),
    menuTorrent: undefined,
  };
  bindToolbar(dashboard);
  bindFilters(dashboard);
  bindRows(dashboard);
  bindContextMenu(dashboard);
  startAutoRefresh(dashboard);
  await loadTorrents(dashboard);
});
