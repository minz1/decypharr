import { describeError } from "#ui/lib/common/errors";
import { fetcher } from "#ui/lib/common/http";
import { createToast } from "#ui/lib/common/toast";
import {
  clearStateRequest,
  clearedMessage,
  failureMessage,
  fixAllRequest,
  fixOneRequest,
  plainFailure,
  recheckMediaRequest,
  runRequest,
} from "#ui/lib/repair/api";
import { confirmAction } from "#ui/lib/repair/confirm";
import { loadBroken, loadHistory, loadStatus } from "#ui/lib/repair/loads";
import { readRunOptions, resetClearModal, resetRunModal, showError, showRecheckFailure } from "#ui/lib/repair/modals";
import { renderClearCounts } from "#ui/lib/repair/status";
import type { RepairPage } from "#ui/lib/repair/types";

const RECHECK_REFRESH_MS = 800;

const ensureOK = async (response: Response, describe: (body: string, status: number) => string): Promise<string> => {
  const body = await response.text();
  if (!response.ok) {
    throw new Error(describe(body, response.status));
  }
  return body;
};

const statusOnly = (_body: string, status: number): string => `HTTP ${status}`;

const reload = (): void => {
  globalThis.location.reload();
};

const openRunModal = (page: RepairPage): void => {
  resetRunModal(page.run, page.state.config);
  page.run.modal.showModal();
};

const openClearStateModal = (page: RepairPage): void => {
  resetClearModal(page.clear);
  renderClearCounts(page.status, page.state.latest?.healthCounts ?? {});
  page.clear.modal.showModal();
};

const openBrokenModal = (page: RepairPage): void => {
  void loadBroken(page);
  page.broken.modal.showModal();
};

const sweepStarted = (ignoreLastChecked: boolean): string =>
  ignoreLastChecked ? "Sweep started, including freshly checked entries" : "Sweep started";

const runNow = async (page: RepairPage): Promise<void> => {
  const options = readRunOptions(page.run);
  page.run.submit.disabled = true;
  try {
    await ensureOK(await fetcher("/api/repair/run", runRequest(options)), plainFailure);
    page.run.modal.close();
    createToast(sweepStarted(options.ignoreLastChecked), "success");
    await loadStatus(page);
  } catch (error) {
    createToast(`Run failed: ${describeError(error)}`, "error");
  } finally {
    page.run.submit.disabled = false;
  }
};

const fixBroken = async (page: RepairPage): Promise<void> => {
  if (!(await confirmAction("Send delete + re-search for every currently broken entry to its Arr?"))) {
    return;
  }
  page.status.fixBroken.disabled = true;
  try {
    await ensureOK(await fetcher("/api/repair/fix", fixAllRequest()), plainFailure);
    createToast("Fix-broken started", "success");
    reload();
  } catch (error) {
    createToast(`Fix failed: ${describeError(error)}`, "error");
    page.status.fixBroken.disabled = false;
  }
};

const postClearState = async (page: RepairPage, statuses: readonly string[]): Promise<void> => {
  try {
    const body = await ensureOK(await fetcher("/api/repair/clear-state", clearStateRequest(statuses)), failureMessage);
    page.clear.modal.close();
    createToast(clearedMessage(body), "success");
    await Promise.all([loadStatus(page), loadBroken(page)]);
  } catch (error) {
    createToast(`Clear failed: ${describeError(error)}`, "error");
    showError(page.clear.error, describeError(error));
  }
};

const clearState = async (page: RepairPage): Promise<void> => {
  const selected = page.clear.states.filter((state) => state.checked).map((state) => state.value);
  if (selected.length === 0) {
    showError(page.clear.error, "Select at least one state.");
    return;
  }
  page.clear.submit.disabled = true;
  await postClearState(page, selected);
  page.clear.submit.disabled = false;
};

const stopRun = async (page: RepairPage): Promise<void> => {
  try {
    await ensureOK(await fetcher("/api/repair/stop", { method: "POST" }), plainFailure);
    createToast("Stop requested", "info");
    await loadStatus(page);
  } catch (error) {
    createToast(`Stop failed: ${describeError(error)}`, "error");
  }
};

const clearHistory = async (page: RepairPage): Promise<void> => {
  if (!(await confirmAction("Clear all run history?"))) {
    return;
  }
  try {
    await ensureOK(await fetcher("/api/repair/runs", { method: "DELETE" }), statusOnly);
    await loadHistory(page);
  } catch (error) {
    createToast(`Clear failed: ${describeError(error)}`, "error");
  }
};

const postRecheckMedia = async (page: RepairPage, request: RequestInit): Promise<void> => {
  try {
    await ensureOK(await fetcher("/api/repair/recheck/media", request), failureMessage);
    createToast("Recheck started", "success");
    reload();
  } catch (error) {
    showRecheckFailure(page.recheck.result, describeError(error));
    page.recheck.submit.disabled = false;
  }
};

const recheckMedia = async (page: RepairPage): Promise<void> => {
  const view = page.recheck;
  const mediaId = view.mediaId.value.trim();
  if (mediaId === "") {
    createToast("Media id is required", "warning");
    return;
  }
  const request = recheckMediaRequest({ arr: view.arr.value, mediaId, fix: view.fix.checked });
  view.submit.disabled = true;
  view.result.classList.add("hidden");
  view.result.textContent = "";
  await postRecheckMedia(page, request);
};

const recheckEntry = async (page: RepairPage, name: string): Promise<void> => {
  try {
    const endpoint = `/api/repair/health/${encodeURIComponent(name)}/check`;
    await ensureOK(await fetcher(endpoint, { method: "POST" }), plainFailure);
    createToast(`Recheck started for ${name}`, "success");
    setTimeout(() => {
      void loadBroken(page);
      void loadStatus(page);
    }, RECHECK_REFRESH_MS);
  } catch (error) {
    createToast(`Recheck failed: ${describeError(error)}`, "error");
  }
};

const fixEntry = async (name: string): Promise<void> => {
  if (!(await confirmAction(`Send delete + re-search for "${name}" to its Arr?`))) {
    return;
  }
  try {
    await ensureOK(await fetcher("/api/repair/fix", fixOneRequest(name)), plainFailure);
    createToast(`Fix started for ${name}`, "success");
    reload();
  } catch (error) {
    createToast(`Fix failed: ${describeError(error)}`, "error");
  }
};

export {
  clearHistory,
  clearState,
  fixBroken,
  fixEntry,
  openBrokenModal,
  openClearStateModal,
  openRunModal,
  recheckEntry,
  recheckMedia,
  runNow,
  stopRun,
};
