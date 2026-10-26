interface Torrent {
  infoHash: string;
  name: string;
  size: number;
  progress: number;
  speed: number | undefined;
  category: string;
  protocol: string;
  activeProvider: string;
  numSeeds: number;
  state: string;
}

interface TorrentPage {
  torrents: Torrent[];
  total: number;
  totalPages: number;
  categories: string[];
}

interface DashboardState extends TorrentPage {
  page: number;
  perPage: number;
  search: string;
  category: string;
  stateFilter: string;
  sortBy: string;
  sortOrder: "asc" | "desc";
  selected: Set<string>;
}

interface Badge {
  className: string;
  text: string;
}

interface ProtocolBadge extends Badge {
  icon: string;
}

interface DashboardRefs {
  torrentsList: HTMLElement;
  categoryFilter: HTMLSelectElement;
  selectAll: HTMLInputElement;
  batchDeleteBtn: HTMLButtonElement;
  batchDeleteDebridBtn: HTMLButtonElement;
  refreshBtn: HTMLButtonElement;
  contextMenu: HTMLElement;
  paginationControls: HTMLElement;
  paginationInfo: HTMLElement;
  emptyState: HTMLElement;
}

interface MenuTorrent {
  hash: string;
  name: string;
  category: string;
}

interface Dashboard {
  refs: DashboardRefs;
  state: DashboardState;
  menuTorrent: MenuTorrent | undefined;
}

export type { Badge, Dashboard, DashboardRefs, DashboardState, MenuTorrent, ProtocolBadge, Torrent, TorrentPage };
