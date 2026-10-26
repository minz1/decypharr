import { byId, cloneTemplate, field } from "#ui/lib/common/dom";
import { statusBadge } from "#ui/lib/reacquire/badge";
import {
  COUNT_TILES,
  arrKind,
  filterJobs,
  formatTime,
  isTerminal,
  jobCounts,
  jobFile,
  jobsPage,
  pageControls,
  statusLine,
} from "#ui/lib/reacquire/format";
import type { Job, JobsPage, JobsView, PageControl, ReacquireState } from "#ui/lib/reacquire/types";
import { cloneRows, fillFields } from "#ui/lib/repair/dom";

const jobsView = (doc: Document): JobsView => ({
  statusLine: byId("reacquireStatusLine", HTMLElement, doc),
  countsGrid: byId("jobCountsGrid", HTMLElement, doc),
  count: byId("jobsCount", HTMLElement, doc),
  filter: byId("jobStatusFilter", HTMLSelectElement, doc),
  pageSize: byId("jobPageSize", HTMLSelectElement, doc),
  body: byId("jobsTableBody", HTMLElement, doc),
  info: byId("jobsPaginationInfo", HTMLElement, doc),
  controls: byId("jobsPaginationControls", HTMLElement, doc),
  bulkBar: byId("jobBulkActions", HTMLElement, doc),
  selectedCount: byId("selectedJobsCount", HTMLElement, doc),
  deleteSelected: byId("deleteSelectedJobsBtn", HTMLButtonElement, doc),
  selectAll: byId("selectAllJobs", HTMLInputElement, doc),
  detailModal: byId("jobDetailModal", HTMLDialogElement, doc),
  detailBody: byId("jobDetailBody", HTMLElement, doc),
});

const countTile = (tile: (typeof COUNT_TILES)[number], value: number): DocumentFragment => {
  const card = cloneTemplate("reacquire-count-tile");
  field(card, "icon").classList.add(tile.icon);
  field(card, "label").textContent = tile.label;
  const count = field(card, "value");
  count.classList.add(tile.tone);
  count.textContent = String(value);
  return card;
};

const renderCounts = (view: JobsView, jobs: readonly Job[]): void => {
  const counts = jobCounts(jobs);
  view.countsGrid.replaceChildren(...COUNT_TILES.map((tile) => countTile(tile, counts[tile.key] ?? 0)));
  view.statusLine.textContent = statusLine(jobs);
};

const currentPage = (view: JobsView, state: ReacquireState): JobsPage => {
  const page = jobsPage(filterJobs(state.jobs, view.filter.value), state.page, state.pageSize);
  state.page = page.page;
  return page;
};

const textOrRemove = (target: HTMLElement, text: string, title: boolean): void => {
  if (text === "") {
    target.remove();
    return;
  }
  target.textContent = text;
  if (title) {
    target.title = text;
  }
};

const fillCheckbox = (row: ParentNode, job: Job, state: ReacquireState): void => {
  const checkbox = field(row, "select");
  if (!(checkbox instanceof HTMLInputElement)) {
    throw new TypeError('[data-field="select"] is not an input');
  }
  checkbox.dataset["jobId"] = job.id;
  checkbox.setAttribute("aria-label", `Select job for ${jobFile(job)}`);
  checkbox.checked = state.selected.has(job.id);
  if (!isTerminal(job.status)) {
    checkbox.disabled = true;
    checkbox.title = "Only completed jobs can be deleted";
  }
};

const fillDetailButton = (button: HTMLElement, job: Job): void => {
  button.dataset["jobId"] = job.id;
  button.setAttribute("aria-label", `View details for ${jobFile(job)}`);
};

const jobRow = (job: Job, state: ReacquireState, now: number): DocumentFragment => {
  const row = cloneRows("reacquire-job-row");
  fillCheckbox(row, job, state);
  field(row, "status").replaceChildren(statusBadge(job.status));
  textOrRemove(field(row, "file"), jobFile(job), true);
  textOrRemove(field(row, "entry"), job.bindings[0]?.entryName ?? "", true);
  textOrRemove(field(row, "arr-type"), arrKind(job.arrName, job.arrType) ? job.arrType : "", false);
  fillFields(row, {
    arr: job.arrName === "" ? "-" : job.arrName,
    cause: job.cause === "" ? "-" : job.cause,
    updated: formatTime(job.updatedAt, now),
  });
  fillDetailButton(field(row, "detail"), job);
  return row;
};

const gapButton = (): HTMLButtonElement => {
  const button = document.createElement("button");
  button.classList.add("join-item", "btn", "btn-sm");
  button.disabled = true;
  button.setAttribute("aria-hidden", "true");
  button.textContent = "…";
  return button;
};

const templateButton = (control: PageControl): HTMLButtonElement => {
  const button = cloneTemplate("reacquire-page-button").querySelector("button");
  if (button === null) {
    throw new TypeError("missing template #reacquire-page-button button");
  }
  button.disabled = control.disabled;
  button.dataset["page"] = String(control.page);
  return button;
};

const numberButton = (control: PageControl): HTMLButtonElement => {
  const button = templateButton(control);
  button.textContent = String(control.page);
  button.setAttribute("aria-label", `Jobs page ${control.page}`);
  if (control.current) {
    button.classList.add("btn-active");
    button.setAttribute("aria-current", "page");
  }
  return button;
};

const arrowButton = (control: PageControl): HTMLButtonElement => {
  const button = templateButton(control);
  const previous = control.kind === "previous";
  const icon = document.createElement("i");
  icon.classList.add("bi", previous ? "bi-chevron-left" : "bi-chevron-right");
  button.replaceChildren(icon);
  button.setAttribute("aria-label", previous ? "Previous jobs page" : "Next jobs page");
  return button;
};

const pageButton = (control: PageControl): HTMLButtonElement => {
  if (control.kind === "gap") {
    return gapButton();
  }
  return control.kind === "page" ? numberButton(control) : arrowButton(control);
};

const renderPagination = (view: JobsView, page: JobsPage): void => {
  if (page.total === 0) {
    view.info.textContent = "No jobs";
    view.controls.replaceChildren();
    return;
  }
  const end = Math.min(page.offset + page.jobs.length, page.total);
  const matching = view.filter.value === "" ? "" : " matching";
  view.info.textContent = `Showing ${page.offset + 1}-${end} of ${page.total}${matching} jobs`;
  view.controls.replaceChildren(
    ...(page.totalPages <= 1 ? [] : pageControls(page.page, page.totalPages)).map((control) => pageButton(control)),
  );
};

const renderSelection = (view: JobsView, state: ReacquireState, pageJobs: readonly Job[]): void => {
  const selected = state.selected.size;
  view.bulkBar.classList.toggle("hidden", selected === 0);
  view.selectedCount.textContent = String(selected);
  view.deleteSelected.disabled = selected === 0 || state.deleting;
  const deletable = pageJobs.filter((job) => isTerminal(job.status));
  const selectedOnPage = deletable.filter((job) => state.selected.has(job.id)).length;
  view.selectAll.checked = deletable.length > 0 && selectedOnPage === deletable.length;
  view.selectAll.indeterminate = selectedOnPage > 0 && selectedOnPage < deletable.length;
  view.selectAll.disabled = deletable.length === 0;
};

const emptyRow = (filtered: boolean): DocumentFragment => {
  const row = cloneRows("reacquire-empty-jobs");
  field(row, "message").textContent = filtered
    ? "No jobs match this status."
    : "No jobs to show. A stream or repair failure queues one automatically.";
  return row;
};

const renderJobs = (view: JobsView, state: ReacquireState, now: number): void => {
  const page = currentPage(view, state);
  view.count.textContent = String(state.jobs.length);
  view.body.replaceChildren(
    ...(page.jobs.length === 0
      ? [emptyRow(view.filter.value !== "")]
      : page.jobs.map((job) => jobRow(job, state, now))),
  );
  renderPagination(view, page);
  renderSelection(view, state, page.jobs);
};

export { currentPage, jobsView, renderCounts, renderJobs, renderSelection };
