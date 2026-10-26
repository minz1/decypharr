import { describeError } from "#ui/lib/common/errors";
import { createToast } from "#ui/lib/common/toast";
import {
  getJSON,
  parseArrNames,
  parseEntries,
  parseRepairConfig,
  parseRepairStatus,
  parseRuns,
} from "#ui/lib/repair/api";
import { BROKEN_PAGE_SIZE, brokenView, renderBrokenPage } from "#ui/lib/repair/broken";
import { clampPage, sortBroken } from "#ui/lib/repair/format";
import { historyView, renderHistory } from "#ui/lib/repair/history";
import { clearModalView, fillArrOptions, recheckView, runModalView } from "#ui/lib/repair/modals";
import { renderBrokenCount, renderStatus, statusView } from "#ui/lib/repair/status";
import type { RepairPage, RepairStatus } from "#ui/lib/repair/types";

const RUNNING_POLL_MS = 2000;
const IDLE_POLL_MS = 15_000;

const repairPage = (doc: Document): RepairPage => ({
  status: statusView(doc),
  history: historyView(doc),
  broken: brokenView(doc),
  run: runModalView(doc),
  clear: clearModalView(doc),
  recheck: recheckView(doc),
  state: {
    config: { autoRepair: false, verifyContent: false },
    latest: undefined,
    wasRunning: false,
    timer: undefined,
    broken: [],
    brokenPage: 1,
  },
});

const reportLoadFailure = (what: string, error: unknown): void => {
  createToast(`Failed to load ${what}: ${describeError(error)}`, "error");
};

const loadRepairConfig = async (page: RepairPage): Promise<void> => {
  try {
    page.state.config = parseRepairConfig(await getJSON("/api/repair/config"));
  } catch (error) {
    reportLoadFailure("repair config", error);
  }
};

const loadArrs = async (page: RepairPage): Promise<void> => {
  try {
    fillArrOptions(page.recheck.arr, parseArrNames(await getJSON("/api/arrs")));
  } catch (error) {
    reportLoadFailure("arrs", error);
  }
};

const loadHistory = async (page: RepairPage): Promise<void> => {
  try {
    renderHistory(page.history, parseRuns(await getJSON("/api/repair/runs")));
  } catch (error) {
    reportLoadFailure("history", error);
  }
};

const showBrokenPage = (page: RepairPage): void => {
  renderBrokenPage(page.broken, page.state.broken, page.state.brokenPage);
};

const loadBroken = async (page: RepairPage): Promise<void> => {
  try {
    const entries = sortBroken(parseEntries(await getJSON("/api/repair/health?status=broken")));
    renderBrokenCount(page.status, entries.length);
    page.state.broken = entries;
    page.state.brokenPage = clampPage(page.state.brokenPage, entries.length, BROKEN_PAGE_SIZE);
    showBrokenPage(page);
  } catch (error) {
    reportLoadFailure("broken entries", error);
  }
};

const goToBrokenPage = (page: RepairPage, target: number): void => {
  const { broken, brokenPage } = page.state;
  if (target === brokenPage || clampPage(target, broken.length, BROKEN_PAGE_SIZE) !== target) {
    return;
  }
  page.state.brokenPage = target;
  showBrokenPage(page);
};

const scheduleStatusPoll = (page: RepairPage, status: RepairStatus, poll: () => void): void => {
  const running = status.activeRun !== undefined;
  const { wasRunning } = page.state;
  page.state.wasRunning = running;
  if (wasRunning && !running) {
    void loadHistory(page);
    if (page.broken.modal.open) {
      void loadBroken(page);
    }
  }
  clearTimeout(page.state.timer);
  page.state.timer = setTimeout(poll, running ? RUNNING_POLL_MS : IDLE_POLL_MS);
};

const loadStatus = async (page: RepairPage): Promise<void> => {
  try {
    const status = parseRepairStatus(await getJSON("/api/repair/status"));
    page.state.latest = status;
    renderStatus(page.status, status);
    scheduleStatusPoll(page, status, () => {
      void loadStatus(page);
    });
  } catch (error) {
    reportLoadFailure("status", error);
  }
};

const loadAll = async (page: RepairPage): Promise<void> => {
  await Promise.all([loadRepairConfig(page), loadStatus(page), loadHistory(page), loadArrs(page)]);
};

export { goToBrokenPage, loadAll, loadBroken, loadHistory, loadStatus, repairPage };
