import { cloneRow, field } from "#ui/lib/common/dom";
import { formatBytes } from "#ui/lib/common/format";
import { pageItems, renderPageItems, showingText } from "#ui/lib/common/pagination";
import { progressColor, progressPercent, protocolBadge, stateBadge, torrentSpeed } from "#ui/lib/dashboard/model";
import type { DashboardRefs, DashboardState, Torrent } from "#ui/lib/dashboard/types";

const HASH_PREFIX = 8;

const fillBadgeCell = (cell: HTMLElement, text: string): void => {
  if (text === "") {
    cell.replaceChildren("-");
    return;
  }
  const badge = cell.querySelector("span");
  if (badge !== null) {
    badge.textContent = text;
  }
};

const fillProgress = (row: ParentNode, progress: number): void => {
  const percent = progressPercent(progress);
  const bar = field(row, "progress");
  bar.classList.add(progressColor(percent));
  bar.setAttribute("value", String(percent));
  field(row, "percent").textContent = `${percent}%`;
};

const fillProtocol = (row: ParentNode, protocol: string): void => {
  const badge = protocolBadge(protocol);
  const target = field(row, "protocol");
  target.classList.add(badge.className);
  target.querySelector("i")?.classList.add(badge.icon);
  target.append(badge.text);
};

const fillState = (row: ParentNode, state: string): void => {
  const badge = stateBadge(state);
  const target = field(row, "state");
  target.classList.add(badge.className);
  target.textContent = badge.text;
};

const fillSelection = (row: HTMLTableRowElement, torrent: Torrent, selected: boolean): void => {
  Object.assign(row.dataset, { hash: torrent.infoHash, name: torrent.name, category: torrent.category });
  const checkbox = row.querySelector<HTMLInputElement>(".torrent-select");
  if (checkbox !== null) {
    checkbox.dataset["hash"] = torrent.infoHash;
    checkbox.checked = selected;
  }
};

const fillText = (row: ParentNode, torrent: Torrent): void => {
  field(row, "name").textContent = torrent.name;
  field(row, "hash").textContent = `${torrent.infoHash.slice(0, HASH_PREFIX)}...`;
  field(row, "size").textContent = formatBytes(torrent.size);
  field(row, "speed").textContent = torrentSpeed(torrent.speed);
  field(row, "seeds").textContent = String(torrent.numSeeds);
};

const torrentRow = (torrent: Torrent, selected: boolean): HTMLTableRowElement => {
  const row = cloneRow("dashboard-row");
  fillSelection(row, torrent, selected);
  fillText(row, torrent);
  fillProgress(row, torrent.progress);
  fillBadgeCell(field(row, "category"), torrent.category);
  fillProtocol(row, torrent.protocol);
  fillBadgeCell(field(row, "provider"), torrent.activeProvider);
  fillState(row, torrent.state);
  return row;
};

const renderTorrents = (refs: DashboardRefs, state: DashboardState): void => {
  refs.torrentsList.replaceChildren(
    ...state.torrents.map((torrent) => torrentRow(torrent, state.selected.has(torrent.infoHash))),
  );
};

const categoryOption = (value: string, label: string, selected: boolean): HTMLOptionElement => {
  const option = document.createElement("option");
  option.value = value;
  option.textContent = label;
  option.selected = selected;
  return option;
};

const renderCategories = (refs: DashboardRefs, state: DashboardState): void => {
  const current = refs.categoryFilter.value;
  refs.categoryFilter.replaceChildren(
    categoryOption("", "All Categories", false),
    ...state.categories.map((category) => categoryOption(category, category, category === current)),
  );
};

const renderPagination = (refs: DashboardRefs, state: DashboardState): void => {
  refs.paginationInfo.textContent = showingText(state, "No items found");
  renderPageItems(refs.paginationControls, pageItems(state.page, state.totalPages), {
    prefix: "dashboard",
    setDisabled: false,
  });
};

const renderSelection = (refs: DashboardRefs, state: DashboardState): void => {
  const hasSelection = state.selected.size > 0;
  refs.batchDeleteBtn.classList.toggle("hidden", !hasSelection);
  refs.batchDeleteDebridBtn.classList.toggle("hidden", !hasSelection);
  refs.selectAll.checked =
    state.torrents.length > 0 && state.torrents.every((torrent) => state.selected.has(torrent.infoHash));
};

const renderEmptyState = (refs: DashboardRefs, state: DashboardState): void => {
  const hasResults = state.total > 0;
  refs.emptyState.classList.toggle("hidden", hasResults);
  refs.torrentsList.closest(".card")?.classList.toggle("hidden", !hasResults);
};

const renderDashboard = (refs: DashboardRefs, state: DashboardState): void => {
  renderCategories(refs, state);
  renderTorrents(refs, state);
  renderPagination(refs, state);
  renderSelection(refs, state);
  renderEmptyState(refs, state);
};

export { renderDashboard, renderSelection, renderTorrents };
