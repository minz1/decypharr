interface Binding {
  arrName: string;
  arrType: string;
  entryId: string;
  entryName: string;
  entryFileId: string;
  entryFileName: string;
  arrFileId: number;
  seriesId: number;
  seasonNumber: number | undefined;
  movieId: number;
  confidence: string;
}

interface Mutation {
  kind: string;
  state: string;
  attempts: number;
  confirmedAt: string;
}

interface Job {
  id: string;
  status: string;
  cause: string;
  strategy: string;
  arrName: string;
  arrType: string;
  fileId: string;
  downloadId: string;
  replacementDownloadId: string;
  lastError: string;
  createdAt: string;
  updatedAt: string;
  completedAt: string;
  bindings: Binding[];
  mutations: Mutation[];
}

interface IndexSummary {
  arrName: string;
  arrType: string;
  bindings: number;
  actionable: number;
  updatedAt: string;
}

interface StatusMeta {
  label: string;
  badge: string;
  icon: string;
}

interface JobsPage {
  jobs: Job[];
  total: number;
  totalPages: number;
  offset: number;
  page: number;
}

interface PageControl {
  kind: "gap" | "next" | "page" | "previous";
  page: number;
  current: boolean;
  disabled: boolean;
}

interface JobsView {
  statusLine: HTMLElement;
  countsGrid: HTMLElement;
  count: HTMLElement;
  filter: HTMLSelectElement;
  pageSize: HTMLSelectElement;
  body: HTMLElement;
  info: HTMLElement;
  controls: HTMLElement;
  bulkBar: HTMLElement;
  selectedCount: HTMLElement;
  deleteSelected: HTMLButtonElement;
  selectAll: HTMLInputElement;
  detailModal: HTMLDialogElement;
  detailBody: HTMLElement;
}

interface ReacquireModalView {
  modal: HTMLDialogElement;
  arr: HTMLSelectElement;
  query: HTMLInputElement;
  results: HTMLElement;
  strategy: HTMLSelectElement;
  error: HTMLElement;
  submit: HTMLButtonElement;
}

interface ReacquireState {
  jobs: Job[];
  page: number;
  pageSize: number;
  selected: Set<string>;
  deleting: boolean;
  timer: ReturnType<typeof setTimeout> | undefined;
  results: Binding[];
  chosen: Binding | undefined;
}

interface ReacquirePage {
  jobs: JobsView;
  modal: ReacquireModalView;
  index: HTMLElement;
  refreshIndex: HTMLButtonElement;
  state: ReacquireState;
}

export type {
  Binding,
  IndexSummary,
  Job,
  JobsPage,
  JobsView,
  Mutation,
  PageControl,
  ReacquireModalView,
  ReacquirePage,
  ReacquireState,
  StatusMeta,
};
