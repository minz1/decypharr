import { byId } from "#ui/lib/common/dom";
import { bindingsEndpoint, getJSON, parseBindings, parseIndex, parseJobs } from "#ui/lib/reacquire/api";
import { DEFAULT_PAGE_SIZE, isTerminal, isWorking } from "#ui/lib/reacquire/format";
import { jobsView, renderCounts, renderJobs } from "#ui/lib/reacquire/jobs";
import {
  bindingsFailed,
  fillArrFilter,
  indexUnavailable,
  reacquireModalView,
  renderBindings,
  renderIndex,
} from "#ui/lib/reacquire/search";
import type { Job, ReacquirePage } from "#ui/lib/reacquire/types";

const POLL_MS = 5000;

const reacquirePage = (doc: Document): ReacquirePage => ({
  jobs: jobsView(doc),
  modal: reacquireModalView(doc),
  index: byId("indexSummary", HTMLElement, doc),
  refreshIndex: byId("refreshIndexBtn", HTMLButtonElement, doc),
  state: {
    jobs: [],
    page: 1,
    pageSize: DEFAULT_PAGE_SIZE,
    selected: new Set(),
    deleting: false,
    timer: undefined,
    results: [],
    chosen: undefined,
  },
});

const showJobs = (page: ReacquirePage): void => {
  renderJobs(page.jobs, page.state, Date.now());
};

const fetchJobs = async (): Promise<Job[] | undefined> => {
  try {
    return parseJobs(await getJSON("/api/arr/reacquire/jobs"));
  } catch {
    return undefined;
  }
};

const pruneSelection = (page: ReacquirePage): void => {
  const deletable = new Set(page.state.jobs.filter((job) => isTerminal(job.status)).map((job) => job.id));
  for (const id of page.state.selected) {
    if (!deletable.has(id)) {
      page.state.selected.delete(id);
    }
  }
};

const showUnavailable = (page: ReacquirePage): void => {
  page.state.selected.clear();
  renderCounts(page.jobs, []);
  page.jobs.statusLine.textContent = "Reacquisition service is unavailable.";
  showJobs(page);
};

const schedulePoll = (page: ReacquirePage, jobs: readonly Job[], poll: () => void): void => {
  clearTimeout(page.state.timer);
  if (jobs.some((job) => isWorking(job))) {
    page.state.timer = setTimeout(poll, POLL_MS);
  }
};

const loadJobs = async (page: ReacquirePage): Promise<void> => {
  const jobs = await fetchJobs();
  page.state.jobs = jobs ?? [];
  if (jobs === undefined) {
    showUnavailable(page);
    return;
  }
  pruneSelection(page);
  renderCounts(page.jobs, jobs);
  showJobs(page);
  schedulePoll(page, jobs, () => {
    void loadJobs(page);
  });
};

const loadIndex = async (page: ReacquirePage): Promise<void> => {
  try {
    const summaries = parseIndex(await getJSON("/api/arr/index"));
    fillArrFilter(page.modal.arr, summaries);
    renderIndex(page.index, summaries, Date.now());
  } catch {
    indexUnavailable(page.index);
  }
};

const searchBindings = async (page: ReacquirePage): Promise<void> => {
  const endpoint = bindingsEndpoint(page.modal.query.value, page.modal.arr.value);
  try {
    const bindings = parseBindings(await getJSON(endpoint));
    renderBindings(page.modal.results, bindings);
    if (bindings.length > 0) {
      page.state.results = bindings;
    }
  } catch {
    bindingsFailed(page.modal.results);
  }
};

const loadAll = async (page: ReacquirePage): Promise<void> => {
  await Promise.all([loadJobs(page), loadIndex(page)]);
};

export { loadAll, loadIndex, loadJobs, reacquirePage, searchBindings, showJobs };
