import { describeError } from "#ui/lib/common/errors";
import { fetcher } from "#ui/lib/common/http";
import { createToast } from "#ui/lib/common/toast";
import { deleteJobsRequest, deletedCount, errorDetail } from "#ui/lib/reacquire/api";
import { acknowledgeButton, jobDetail } from "#ui/lib/reacquire/detail";
import { DEFAULT_PAGE_SIZE, filterJobs, isTerminal, jobsPage } from "#ui/lib/reacquire/format";
import { currentPage, renderSelection } from "#ui/lib/reacquire/jobs";
import { loadIndex, loadJobs, showJobs } from "#ui/lib/reacquire/loads";
import type { Job, ReacquirePage } from "#ui/lib/reacquire/types";
import { confirmAction } from "#ui/lib/repair/confirm";

const INDEX_REREAD_MS = 3000;
const ACKNOWLEDGE_QUESTION =
  "Check this job’s actions in Arr first. Acknowledgement releases duplicate protection and allows a new reacquisition. Have you checked the remote actions?";

const updateSelection = (page: ReacquirePage): void => {
  renderSelection(page.jobs, page.state, currentPage(page.jobs, page.state).jobs);
};

const goToJobsPage = (page: ReacquirePage, target: number): void => {
  const { totalPages } = jobsPage(filterJobs(page.state.jobs, page.jobs.filter.value), 1, page.state.pageSize);
  if (target < 1 || target > totalPages || target === page.state.page) {
    return;
  }
  page.state.page = target;
  showJobs(page);
};

const changeFilter = (page: ReacquirePage): void => {
  page.state.page = 1;
  showJobs(page);
};

const changePageSize = (page: ReacquirePage): void => {
  const size = Math.trunc(Number(page.jobs.pageSize.value));
  page.state.pageSize = size > 0 ? size : DEFAULT_PAGE_SIZE;
  page.state.page = 1;
  showJobs(page);
};

const setSelected = (page: ReacquirePage, id: string, checked: boolean): void => {
  if (checked) {
    page.state.selected.add(id);
  } else {
    page.state.selected.delete(id);
  }
};

const toggleJob = (page: ReacquirePage, id: string, checked: boolean): void => {
  const job = page.state.jobs.find((candidate) => candidate.id === id);
  if (job === undefined || !isTerminal(job.status)) {
    return;
  }
  setSelected(page, id, checked);
  updateSelection(page);
};

const selectJobsOnPage = (page: ReacquirePage, checked: boolean): void => {
  for (const job of currentPage(page.jobs, page.state).jobs.filter((candidate) => isTerminal(candidate.status))) {
    setSelected(page, job.id, checked);
  }
  showJobs(page);
};

const clearJobSelection = (page: ReacquirePage): void => {
  page.state.selected.clear();
  showJobs(page);
};

const deletionQuestion = (count: number): string =>
  `Permanently delete ${count} selected job${count === 1 ? "" : "s"}?\n\nOnly the job history will be removed. This cannot be undone.`;

const reportDeleted = (page: ReacquirePage, ids: readonly string[], deleted: number): void => {
  for (const id of ids) {
    page.state.selected.delete(id);
  }
  createToast(`Deleted ${deleted} job${deleted === 1 ? "" : "s"}`, "success");
};

const postDelete = async (page: ReacquirePage, ids: readonly string[]): Promise<void> => {
  try {
    const response = await fetcher("/api/arr/reacquire/jobs", deleteJobsRequest(ids));
    const body = await response.text();
    if (!response.ok) {
      throw new Error(errorDetail(body, response.status));
    }
    reportDeleted(page, ids, deletedCount(body, ids.length));
  } catch (error) {
    createToast(`Could not delete jobs: ${describeError(error)}`, "error");
  }
  await loadJobs(page);
};

const deleteJobs = async (page: ReacquirePage, ids: readonly string[]): Promise<void> => {
  page.state.deleting = true;
  updateSelection(page);
  await postDelete(page, ids);
  page.state.deleting = false;
  updateSelection(page);
};

const deletableSelection = (page: ReacquirePage): string[] => {
  const jobs = new Map(page.state.jobs.map((job) => [job.id, job]));
  return [...page.state.selected].filter((id) => isTerminal(jobs.get(id)?.status ?? ""));
};

const deleteSelectedJobs = async (page: ReacquirePage): Promise<void> => {
  const ids = deletableSelection(page);
  if (ids.length === 0) {
    clearJobSelection(page);
    createToast("No completed jobs are selected", "warning");
    return;
  }
  if (!(await confirmAction(deletionQuestion(ids.length)))) {
    return;
  }
  await deleteJobs(page, ids);
};

const postAcknowledge = async (page: ReacquirePage, job: Job, button: HTMLButtonElement): Promise<void> => {
  try {
    const endpoint = `/api/arr/reacquire/jobs/${encodeURIComponent(job.id)}/acknowledge`;
    const response = await fetcher(endpoint, { method: "POST" });
    if (!response.ok) {
      throw new Error(`HTTP ${response.status}`);
    }
    page.jobs.detailModal.close();
    await loadJobs(page);
  } catch (error) {
    createToast(`Could not acknowledge job: ${describeError(error)}`, "error");
    button.disabled = false;
  }
};

const acknowledge = async (page: ReacquirePage, job: Job, button: HTMLButtonElement): Promise<void> => {
  if (!(await confirmAction(ACKNOWLEDGE_QUESTION))) {
    return;
  }
  button.disabled = true;
  await postAcknowledge(page, job, button);
};

const showJob = (page: ReacquirePage, id: string): void => {
  const job = page.state.jobs.find((candidate) => candidate.id === id);
  if (job === undefined) {
    return;
  }
  page.jobs.detailBody.replaceChildren(jobDetail(job, Date.now()));
  if (job.status === "needs_attention") {
    const button = acknowledgeButton();
    button.addEventListener("click", () => {
      void acknowledge(page, job, button);
    });
    page.jobs.detailBody.append(button);
  }
  page.jobs.detailModal.showModal();
};

const refreshIndex = async (page: ReacquirePage): Promise<void> => {
  page.refreshIndex.disabled = true;
  try {
    const response = await fetcher("/api/arr/index/refresh", { method: "POST" });
    if (!response.ok) {
      throw new Error(`HTTP ${response.status}`);
    }
    createToast("Index rebuild queued", "success");
    setTimeout(() => {
      void loadIndex(page);
    }, INDEX_REREAD_MS);
  } catch {
    createToast("Could not queue the index rebuild", "error");
  } finally {
    page.refreshIndex.disabled = false;
  }
};

export {
  changeFilter,
  changePageSize,
  clearJobSelection,
  deleteSelectedJobs,
  goToJobsPage,
  refreshIndex,
  selectJobsOnPage,
  showJob,
  toggleJob,
};
