import { describeError } from "#ui/lib/common/errors";
import { fetcher } from "#ui/lib/common/http";
import { readJSON } from "#ui/lib/common/json";
import { createToast } from "#ui/lib/common/toast";
import { askConfirm } from "#ui/lib/confirm";
import { deleteSelectedURL, deleteTorrentURL, parseTorrentPage, torrentsURL } from "#ui/lib/dashboard/model";
import type { Dashboard, DashboardState, MenuTorrent } from "#ui/lib/dashboard/types";
import { renderDashboard, renderSelection, renderTorrents } from "#ui/lib/dashboard/view";

const REFRESH_MS = 10_000;
const MENU_WIDTH_PX = 200;
const MENU_HEIGHT_PX = 150;

const fetchPage = async (state: DashboardState): Promise<void> => {
  const response = await fetcher(torrentsURL(state));
  if (!response.ok) {
    throw new Error("Failed to fetch items");
  }
  Object.assign(state, parseTorrentPage(await readJSON(response)));
};

const loadTorrents = async (dashboard: Dashboard): Promise<void> => {
  const { refs, state } = dashboard;
  try {
    refs.refreshBtn.disabled = true;
    refs.paginationInfo.textContent = "Loading torrents...";
    await fetchPage(state);
    renderDashboard(refs, state);
  } catch (error) {
    createToast(`Error loading items: ${describeError(error)}`, "error");
  } finally {
    refs.refreshBtn.disabled = false;
  }
};

const sendDelete = async (url: string): Promise<void> => {
  const response = await fetcher(url, { method: "DELETE" });
  if (!response.ok) {
    throw new Error(`DELETE ${url}: ${response.status}`);
  }
};

const deleteTorrent = async (
  dashboard: Dashboard,
  torrent: Pick<MenuTorrent, "hash" | "category">,
  removeFromDebrid: boolean,
): Promise<void> => {
  if (!(await askConfirm("Are you sure you want to delete this torrent?"))) {
    return;
  }
  try {
    await sendDelete(deleteTorrentURL(torrent.hash, torrent.category, removeFromDebrid));
    createToast("Item deleted successfully");
    dashboard.state.selected.delete(torrent.hash);
    void loadTorrents(dashboard);
  } catch {
    createToast("Failed to delete entry", "error");
  }
};

const deleteSelectedTorrents = async (dashboard: Dashboard, removeFromDebrid: boolean): Promise<void> => {
  const { selected } = dashboard.state;
  if (selected.size === 0 || !(await askConfirm(`Delete ${selected.size} selected items?`))) {
    return;
  }
  try {
    await sendDelete(deleteSelectedURL([...selected], removeFromDebrid));
    createToast(`Deleted ${selected.size} items successfully`);
    selected.clear();
    void loadTorrents(dashboard);
  } catch {
    createToast("Failed to delete items", "error");
  }
};

const goToPage = (dashboard: Dashboard, page: number): void => {
  if (page < 1 || page > dashboard.state.totalPages) {
    return;
  }
  dashboard.state.page = page;
  void loadTorrents(dashboard);
};

const filterBy = (dashboard: Dashboard, change: Partial<DashboardState>): void => {
  Object.assign(dashboard.state, change, { page: 1 });
  void loadTorrents(dashboard);
};

const toggleSelectAll = (dashboard: Dashboard, checked: boolean): void => {
  const { refs, state } = dashboard;
  if (checked) {
    for (const torrent of state.torrents) {
      state.selected.add(torrent.infoHash);
    }
  } else {
    state.selected.clear();
  }
  renderTorrents(refs, state);
  renderSelection(refs, state);
};

const toggleTorrentSelection = (dashboard: Dashboard, hash: string, checked: boolean): void => {
  if (checked) {
    dashboard.state.selected.add(hash);
  } else {
    dashboard.state.selected.delete(hash);
  }
  renderSelection(dashboard.refs, dashboard.state);
};

const showContextMenu = (dashboard: Dashboard, event: MouseEvent, row: HTMLElement): void => {
  const menu = dashboard.refs.contextMenu;
  const { hash = "", name = "", category = "" } = row.dataset;
  dashboard.menuTorrent = { hash, name, category };
  const title = menu.querySelector(".torrent-name");
  if (title !== null) {
    title.textContent = name;
  }
  const { clientWidth, clientHeight } = document.documentElement;
  menu.style.left = `${Math.min(event.pageX, clientWidth - MENU_WIDTH_PX)}px`;
  menu.style.top = `${Math.min(event.pageY, clientHeight - MENU_HEIGHT_PX)}px`;
  menu.classList.remove("hidden");
};

const hideContextMenu = (dashboard: Dashboard): void => {
  dashboard.refs.contextMenu.classList.add("hidden");
  dashboard.menuTorrent = undefined;
};

const copyText = async (text: string, success: string, failure: string): Promise<void> => {
  try {
    await navigator.clipboard.writeText(text);
    createToast(success);
  } catch {
    createToast(failure, "error");
  }
};

const runContextAction = async (dashboard: Dashboard, action: string): Promise<void> => {
  const torrent = dashboard.menuTorrent;
  if (torrent === undefined) {
    return;
  }
  const actions: Readonly<Record<string, () => Promise<void>>> = {
    "copy-magnet": async () =>
      copyText(`magnet:?xt=urn:btih:${torrent.hash}`, "Magnet link copied to clipboard", "Failed to copy magnet link"),
    "copy-name": async () => copyText(torrent.name, "Torrent name copied to clipboard", "Failed to copy torrent name"),
    delete: async () => deleteTorrent(dashboard, torrent, false),
    "delete-debrid": async () => deleteTorrent(dashboard, torrent, true),
  };
  await actions[action]?.();
};

const startAutoRefresh = (dashboard: Dashboard): void => {
  setInterval(() => {
    if (document.visibilityState !== "hidden") {
      void loadTorrents(dashboard);
    }
  }, REFRESH_MS);
};

export {
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
};
