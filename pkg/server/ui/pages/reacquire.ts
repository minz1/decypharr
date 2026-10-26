import { bootPage, byId, onAction } from "#ui/lib/common/dom";
import { debounce } from "#ui/lib/common/format";
import {
  changeFilter,
  changePageSize,
  clearJobSelection,
  deleteSelectedJobs,
  goToJobsPage,
  refreshIndex,
  selectJobsOnPage,
  showJob,
  toggleJob,
} from "#ui/lib/reacquire/actions";
import { loadAll, loadJobs, reacquirePage, searchBindings } from "#ui/lib/reacquire/loads";
import { openReacquireModal, selectBinding, submitReacquire } from "#ui/lib/reacquire/modal";
import type { ReacquirePage } from "#ui/lib/reacquire/types";

const SEARCH_DEBOUNCE_MS = 250;

const onClick = (button: HTMLButtonElement, action: () => unknown): void => {
  button.addEventListener("click", () => {
    void action();
  });
};

const bindJobs = (page: ReacquirePage): void => {
  const { jobs } = page;
  onClick(byId("refreshJobsBtn", HTMLButtonElement), async () => loadJobs(page));
  onClick(page.refreshIndex, async () => refreshIndex(page));
  onClick(jobs.deleteSelected, async () => deleteSelectedJobs(page));
  onClick(byId("clearJobSelectionBtn", HTMLButtonElement), () => {
    clearJobSelection(page);
  });
  jobs.filter.addEventListener("change", () => {
    changeFilter(page);
  });
  jobs.pageSize.addEventListener("change", () => {
    changePageSize(page);
  });
  jobs.selectAll.addEventListener("change", () => {
    selectJobsOnPage(page, jobs.selectAll.checked);
  });
};

const bindJobRows = (page: ReacquirePage): void => {
  onAction(page.jobs.body, "change", {
    "select-job": (box) => {
      toggleJob(page, box.dataset["jobId"] ?? "", box instanceof HTMLInputElement && box.checked);
    },
  });
  onAction(page.jobs.body, "click", {
    "show-job": (button) => {
      showJob(page, button.dataset["jobId"] ?? "");
    },
  });
  onAction(page.jobs.controls, "click", {
    "jobs-page": (button) => {
      goToJobsPage(page, Number(button.dataset["page"] ?? "0"));
    },
  });
};

const bindModal = (page: ReacquirePage): void => {
  const { modal } = page;
  const search = debounce(() => {
    void searchBindings(page);
  }, SEARCH_DEBOUNCE_MS);
  onClick(byId("reacquireBtn", HTMLButtonElement), () => {
    openReacquireModal(page);
  });
  onClick(byId("reacquireCancelBtn", HTMLButtonElement), () => {
    modal.modal.close();
  });
  modal.arr.addEventListener("change", () => {
    void searchBindings(page);
  });
  modal.query.addEventListener("input", search);
  onAction(modal.results, "click", {
    "select-binding": (row) => {
      selectBinding(page, Number(row.dataset["index"] ?? "-1"));
    },
  });
  byId("reacquireForm", HTMLFormElement).addEventListener("submit", (event) => {
    event.preventDefault();
    void submitReacquire(page);
  });
};

bootPage("reacquire", async () => {
  const page = reacquirePage(document);
  byId("confirmDialog", HTMLDialogElement);
  bindJobs(page);
  bindJobRows(page);
  bindModal(page);
  await loadAll(page);
});
