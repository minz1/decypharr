type SortKey = "name" | "size" | "mod_time" | "active_debrid";

type SortOrder = "asc" | "desc";

interface Entry {
  name: string;
  path: string;
  size: number;
  modTime: string;
  isDir: boolean;
  infoHash: string;
  canDelete: boolean;
  activeDebrid: string;
  kind: string;
}

interface BrowsePage {
  entries: Entry[];
  total: number;
  totalPages: number;
  currentKind: string;
}

interface Health {
  status: string;
  lastCheckedAt: string;
}

interface BrowseLocation {
  path: string;
  page: number;
  search: string;
  perPage: number;
  sortBy: SortKey;
  sortOrder: SortOrder;
}

interface BrowseState extends BrowseLocation, BrowsePage {
  selected: Map<string, Entry>;
  health: Map<string, Health>;
}

interface HealthBadge {
  className: string;
  text: string;
  title: string | undefined;
}

interface Crumb {
  path: string;
  label: string;
}

interface BrowseRefs {
  breadcrumbNav: HTMLElement;
  searchInput: HTMLInputElement;
  pageSizeSelect: HTMLSelectElement;
  fileBrowserList: HTMLElement;
  paginationInfo: HTMLElement;
  paginationControls: HTMLElement;
  emptyState: HTMLElement;
  emptyStateMessage: HTMLElement;
  emptyStateVirtualFolderLink: HTMLElement;
  currentViewNotice: HTMLElement;
  selectAllCheckbox: HTMLInputElement;
  bulkActionsBar: HTMLElement;
  selectedCount: HTMLElement;
  contextMenu: HTMLElement;
  contextDownload: HTMLElement;
  contextDelete: HTMLElement;
  sortIndicators: ReadonlyMap<SortKey, HTMLElement>;
}

interface Browser {
  refs: BrowseRefs;
  state: BrowseState;
  menuEntry: Entry | undefined;
  loads: { sequence: number; controller: AbortController | undefined };
}

export type {
  BrowseLocation,
  BrowsePage,
  BrowseRefs,
  BrowseState,
  Browser,
  Crumb,
  Entry,
  Health,
  HealthBadge,
  SortKey,
  SortOrder,
};
