interface RepairConfig {
  autoRepair: boolean;
  verifyContent: boolean;
}

interface RunStats {
  candidates: number;
  skippedFresh: number;
  probed: number;
  healthy: number;
  broken: number;
  repaired: number;
  cleared: number;
  repairFailed: number;
}

interface ActiveRun {
  id: string;
  stage: string;
  startedAt: string;
  stats: RunStats;
}

interface RepairStatus {
  enabled: boolean;
  nextRunAt: string | undefined;
  activeRun: ActiveRun | undefined;
  healthCounts: Readonly<Record<string, number>>;
}

interface RepairRun {
  trigger: string;
  status: string;
  startedAt: string;
  completedAt: string;
  stats: RunStats;
  error: string;
}

interface BrokenFile {
  fileName: string;
  reason: string;
  size: number;
  arrName: string;
  arrKind: string;
  mediaId: number;
  episodeId: number;
  arrFileId: number;
}

interface EntryHealth {
  entryName: string;
  protocol: string;
  fileCount: number;
  brokenCount: number;
  failureReason: string;
  lastCheckedAt: string;
  lastFailedAt: string;
  lastRepairAt: string;
  brokenFiles: BrokenFile[];
}

interface RunOptions {
  ignoreLastChecked: boolean;
  autoRepair: boolean;
  verifyContent: boolean;
  protocol: string;
}

interface RecheckMedia {
  arr: string;
  mediaId: string;
  fix: boolean;
}

interface PageButton {
  label: string;
  target: number | undefined;
  active: boolean;
  disabled: boolean;
}

interface StatusView {
  line: HTMLElement;
  runNow: HTMLButtonElement;
  stop: HTMLButtonElement;
  fixBroken: HTMLButtonElement;
  clearState: HTMLButtonElement;
  viewBroken: HTMLButtonElement;
  brokenBadge: HTMLElement;
  brokenModalCount: HTMLElement;
  panel: HTMLElement;
  stage: HTMLElement;
  runId: HTMLElement;
  started: HTMLElement;
  stats: HTMLElement;
  grid: HTMLElement;
  clearCounts: HTMLElement[];
}

interface HistoryView {
  body: HTMLElement;
  empty: HTMLElement;
}

interface BrokenView {
  modal: HTMLDialogElement;
  body: HTMLElement;
  empty: HTMLElement;
  bar: HTMLElement;
  info: HTMLElement;
  controls: HTMLElement;
}

interface RunModalView {
  modal: HTMLDialogElement;
  ignoreLastChecked: HTMLInputElement;
  autoRepair: HTMLInputElement;
  verifyContent: HTMLInputElement;
  protocolAll: HTMLInputElement;
  protocols: HTMLInputElement[];
  submit: HTMLButtonElement;
}

interface ClearModalView {
  modal: HTMLDialogElement;
  states: HTMLInputElement[];
  error: HTMLElement;
  submit: HTMLButtonElement;
}

interface RecheckView {
  arr: HTMLSelectElement;
  mediaId: HTMLInputElement;
  fix: HTMLInputElement;
  submit: HTMLButtonElement;
  result: HTMLElement;
}

interface RepairState {
  config: RepairConfig;
  latest: RepairStatus | undefined;
  wasRunning: boolean;
  timer: ReturnType<typeof setTimeout> | undefined;
  broken: EntryHealth[];
  brokenPage: number;
}

interface RepairPage {
  status: StatusView;
  history: HistoryView;
  broken: BrokenView;
  run: RunModalView;
  clear: ClearModalView;
  recheck: RecheckView;
  state: RepairState;
}

export type {
  ActiveRun,
  BrokenFile,
  BrokenView,
  ClearModalView,
  EntryHealth,
  HistoryView,
  PageButton,
  RecheckMedia,
  RecheckView,
  RepairConfig,
  RepairPage,
  RepairRun,
  RepairState,
  RepairStatus,
  RunModalView,
  RunOptions,
  RunStats,
  StatusView,
};
