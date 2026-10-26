import {
  breadcrumbs,
  entryIcon,
  entryId,
  healthBadge,
  isActionable,
  kindBadge,
  sortIndicator,
} from "#ui/lib/browse/model";
import type { BrowseRefs, BrowseState, Entry, Health } from "#ui/lib/browse/types";
import { cloneRow, cloneTemplate, field } from "#ui/lib/common/dom";
import { formatBytes } from "#ui/lib/common/format";
import { pageItems, renderPageItems, showingText } from "#ui/lib/common/pagination";
import { joinURL, readURLBase } from "#ui/lib/common/url";

const fillHealthBadge = (badge: Element, health: Health | undefined): void => {
  const { className, text, title } = healthBadge(health);
  badge.className = `badge ${className} badge-sm`;
  badge.textContent = text;
  if (title === undefined) {
    badge.removeAttribute("title");
  } else {
    badge.setAttribute("title", title);
  }
};

const fillCheckbox = (box: HTMLElement, entry: Entry, selected: boolean): void => {
  if (box instanceof HTMLInputElement) {
    box.setAttribute("aria-label", `Select ${entry.name}`);
    box.dataset["entryId"] = entryId(entry);
    box.checked = selected;
    box.disabled = !isActionable(entry);
  }
};

const fillSelect = (row: ParentNode, entry: Entry, selected: boolean): void => {
  field(row, "select-label").classList.toggle("cursor-pointer", isActionable(entry));
  field(row, "select-text").textContent = `Select ${entry.name}`;
  fillCheckbox(field(row, "select"), entry, selected);
};

const fillKind = (kind: HTMLElement, entry: Entry): void => {
  const badge = kindBadge(entry.kind);
  if (badge === undefined) {
    kind.remove();
    return;
  }
  kind.firstElementChild?.classList.add(...badge.classes);
  kind.firstElementChild?.append(badge.text);
};

const fillName = (row: ParentNode, entry: Entry): void => {
  const name = field(row, "name");
  name.textContent = entry.name;
  if (!entry.isDir && !isActionable(entry)) {
    field(row, "open").replaceWith(name);
  }
  fillKind(field(row, "kind"), entry);
};

const fillProvider = (cell: HTMLElement, provider: string): void => {
  if (provider === "") {
    cell.replaceChildren("-");
  } else {
    field(cell, "provider-name").textContent = provider;
  }
};

const fillHealthCell = (cell: HTMLElement, entry: Entry, health: Health | undefined): void => {
  if (entry.kind === "entry") {
    cell.dataset["healthCell"] = entry.name;
    fillHealthBadge(cell.firstElementChild ?? cell, health);
  } else {
    cell.replaceChildren("—");
  }
};

const fillCells = (row: ParentNode, entry: Entry, health: Health | undefined): void => {
  field(row, "icon").classList.add(...entryIcon(entry));
  field(row, "size").textContent = entry.size <= 0 ? "-" : formatBytes(entry.size);
  field(row, "modified").textContent = entry.modTime === "" ? "-" : entry.modTime;
  fillProvider(field(row, "provider"), entry.activeDebrid);
  fillHealthCell(field(row, "health"), entry, health);
};

const hiddenDash = (): HTMLSpanElement => {
  const dash = document.createElement("span");
  dash.setAttribute("aria-hidden", "true");
  dash.textContent = "—";
  return dash;
};

const fillActions = (row: ParentNode, entry: Entry): void => {
  if (!isActionable(entry)) {
    field(row, "actions").replaceChildren(hiddenDash());
    return;
  }
  field(row, "menu-button").setAttribute("aria-label", `Actions for ${entry.name}`);
  for (const [name, shown] of [
    ["download", !entry.isDir],
    ["recheck", entry.kind === "entry"],
    ["delete", entry.canDelete],
  ] as const) {
    if (!shown) {
      field(row, name).remove();
    }
  }
};

const entryRow = (entry: Entry, index: number, state: BrowseState): HTMLTableRowElement => {
  const row = cloneRow("browse-row");
  row.dataset["index"] = String(index);
  row.dataset["entryId"] = entryId(entry);
  fillSelect(row, entry, state.selected.has(entryId(entry)));
  fillName(row, entry);
  fillCells(row, entry, state.health.get(entry.name));
  fillActions(row, entry);
  return row;
};

const renderSelection = (refs: BrowseRefs, state: BrowseState): void => {
  const count = state.selected.size;
  refs.selectedCount.textContent = String(count);
  refs.bulkActionsBar.classList.toggle("hidden", count === 0);
  const actionable = state.entries.filter((entry) => isActionable(entry));
  const allSelected = actionable.length > 0 && actionable.every((entry) => state.selected.has(entryId(entry)));
  refs.selectAllCheckbox.checked = allSelected;
  refs.selectAllCheckbox.indeterminate = count > 0 && !allSelected;
  refs.selectAllCheckbox.disabled = actionable.length === 0;
};

const renderEntries = (refs: BrowseRefs, state: BrowseState): void => {
  const empty = state.entries.length === 0;
  refs.emptyState.classList.toggle("hidden", !empty);
  refs.fileBrowserList.replaceChildren(...state.entries.map((entry, index) => entryRow(entry, index, state)));
  if (!empty) {
    renderSelection(refs, state);
  }
};

const renderBreadcrumbs = (refs: BrowseRefs, path: string): void => {
  const base = readURLBase(document);
  const crumbs = breadcrumbs(path).map((crumb) => {
    const item = cloneTemplate("browse-crumb");
    const link = item.querySelector("a");
    if (link !== null) {
      link.href = joinURL(base, `browse?path=${encodeURIComponent(crumb.path)}`);
      link.dataset["path"] = crumb.path;
      link.textContent = crumb.label;
    }
    return item;
  });
  refs.breadcrumbNav.replaceChildren(refs.breadcrumbNav.firstElementChild ?? "", ...crumbs);
};

const renderViewNotice = (refs: BrowseRefs, currentKind: string): void => {
  const virtual = currentKind === "virtual";
  refs.currentViewNotice.classList.toggle("hidden", !virtual);
  refs.emptyStateMessage.textContent = virtual
    ? "No current items match this virtual folder’s conditions."
    : "No files or folders found here.";
  refs.emptyStateVirtualFolderLink.classList.toggle("hidden", !virtual);
};

const renderPagination = (refs: BrowseRefs, state: BrowseState): void => {
  refs.paginationInfo.textContent = showingText(state, "No items");
  renderPageItems(refs.paginationControls, pageItems(state.page, state.totalPages), {
    prefix: "browse",
    setDisabled: true,
  });
};

const renderSortIndicators = (refs: BrowseRefs, state: BrowseState): void => {
  for (const [key, indicator] of refs.sortIndicators) {
    indicator.className = sortIndicator(state, key);
  }
};

const renderHealthBadges = (refs: BrowseRefs, state: BrowseState): void => {
  for (const cell of refs.fileBrowserList.querySelectorAll<HTMLElement>("[data-health-cell]")) {
    const badge = cell.firstElementChild;
    if (badge !== null) {
      fillHealthBadge(badge, state.health.get(cell.dataset["healthCell"] ?? ""));
    }
  }
};

const renderBrowser = (refs: BrowseRefs, state: BrowseState): void => {
  renderBreadcrumbs(refs, state.path);
  renderViewNotice(refs, state.currentKind);
  renderEntries(refs, state);
  renderPagination(refs, state);
};

export { renderBrowser, renderHealthBadges, renderSelection, renderSortIndicators };
