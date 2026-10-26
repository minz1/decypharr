import { cloneTemplate } from "#ui/lib/common/dom";
import { describeError } from "#ui/lib/common/errors";
import { formatBytes } from "#ui/lib/common/format";
import { fetcher } from "#ui/lib/common/http";
import { asArray, asRecord, optBool, optNum, optStr, readJSON } from "#ui/lib/common/json";
import { createToast } from "#ui/lib/common/toast";

interface PruneCandidate {
  name: string;
  provider: string;
  excluded: string;
  size: number;
  unreferencedSince: string;
  eligibleAt: string;
}

interface PruneReport {
  record: Record<string, unknown>;
  startedAt: string;
  candidates: PruneCandidate[];
}

const SHOWN_CANDIDATES = 100;
const HTTP_CONFLICT = 409;
const TIERS = { eligible: 0, waiting: 1, excluded: 2 } as const;

const parseCandidate = (value: unknown): PruneCandidate => {
  const record = asRecord(value, "candidate");
  return {
    name: optStr(record, "name") ?? "",
    provider: optStr(record, "provider") ?? "",
    excluded: optStr(record, "excluded") ?? "",
    size: optNum(record, "size") ?? 0,
    unreferencedSince: optStr(record, "unreferenced_since") ?? "",
    eligibleAt: optStr(record, "eligible_at") ?? "",
  };
};

const parsePruneReport = (value: unknown): PruneReport | undefined => {
  if (value === null) {
    return undefined;
  }
  const record = asRecord(value, "prune report");
  const startedAt = optStr(record, "started_at") ?? "";
  const candidates = record["candidates"] ?? [];
  return startedAt === ""
    ? undefined
    : { record, startedAt, candidates: asArray(candidates, "candidates").map((item) => parseCandidate(item)) };
};

const when = (stamp: string): string => (stamp === "" ? "" : new Date(stamp).toLocaleString());

const isEligible = (candidate: PruneCandidate, started: number): boolean =>
  candidate.excluded === "" && Date.parse(candidate.eligibleAt) <= started;

const tier = (candidate: PruneCandidate, started: number): number => {
  if (isEligible(candidate, started)) {
    return TIERS.eligible;
  }
  return candidate.excluded === "" ? TIERS.waiting : TIERS.excluded;
};

const eligibleTime = (candidate: PruneCandidate): number => {
  const parsed = Date.parse(candidate.eligibleAt);
  return Number.isNaN(parsed) ? 0 : parsed;
};

const sortCandidates = (candidates: readonly PruneCandidate[], started: number): PruneCandidate[] =>
  candidates.toSorted(
    (left, right) => tier(left, started) - tier(right, started) || eligibleTime(left) - eligibleTime(right),
  );

const countText = (record: Record<string, unknown>, key: string): string => {
  const value = optNum(record, key);
  return value === undefined ? "" : String(value);
};

const counts = (report: PruneReport, sorted: readonly PruneCandidate[]): (readonly [string, string])[] => {
  const started = Date.parse(report.startedAt);
  const { record } = report;
  return [
    ["Links", countText(record, "links")],
    ["Referenced", countText(record, "referenced")],
    ["Dangling", countText(record, "dangling")],
    ["Newly unreferenced", countText(record, "newly_unreferenced")],
    ["Missing referenced", countText(record, "missing_referenced")],
    ["Candidates", String(sorted.length)],
    ["Eligible", String(sorted.filter((candidate) => isEligible(candidate, started)).length)],
    ["Deleted", countText(record, "deleted")],
    ["Failed", countText(record, "failed")],
    ["Freed", formatBytes(optNum(record, "bytes_freed") ?? 0)],
  ];
};

const setText = (node: Element | null, text: string): void => {
  if (node !== null) {
    node.textContent = text;
  }
};

const countNode = (doc: Document, [label, value]: readonly [string, string]): DocumentFragment => {
  const node = cloneTemplate("config-prune-count", doc);
  setText(node.querySelector("dt"), label);
  setText(node.querySelector("dd"), value);
  return node;
};

const candidateRow = (doc: Document, candidate: PruneCandidate): HTMLTableRowElement => {
  const row = cloneTemplate("config-prune-row", doc).querySelector("tr");
  if (row === null) {
    throw new Error("missing template row #config-prune-row");
  }
  const cells = [
    candidate.name,
    candidate.provider,
    formatBytes(candidate.size),
    when(candidate.unreferencedSince),
    candidate.excluded === "" ? when(candidate.eligibleAt) : `Excluded: ${candidate.excluded}`,
  ];
  for (const [index, cell] of [...row.cells].entries()) {
    cell.textContent = cells[index] ?? "";
  }
  return row;
};

const fillOrRemove = (node: Element | null, text: string): void => {
  if (text === "") {
    node?.remove();
  } else if (node !== null) {
    node.textContent = text;
  }
};

const fillSummary = (node: DocumentFragment, report: PruneReport): void => {
  const baseline = optBool(report.record, "accepted_baseline") === true ? ", baseline accepted" : "";
  fillOrRemove(
    node.querySelector("[data-prune-started]"),
    `Started ${when(report.startedAt)}, mode ${optStr(report.record, "mode") ?? ""}${baseline}`,
  );
  const aborted = optStr(report.record, "aborted") ?? "";
  fillOrRemove(node.querySelector("[data-prune-aborted]"), aborted === "" ? "" : `Aborted: ${aborted}`);
};

const fillCandidates = (doc: Document, node: DocumentFragment, sorted: readonly PruneCandidate[]): void => {
  const rows = sorted.slice(0, SHOWN_CANDIDATES).map((candidate) => candidateRow(doc, candidate));
  node.querySelector("[data-prune-rows]")?.append(...rows);
  if (rows.length === 0) {
    node.querySelector("[data-prune-table]")?.remove();
  }
  const more = sorted.length - SHOWN_CANDIDATES;
  fillOrRemove(node.querySelector("[data-prune-more]"), more > 0 ? `and ${more} more` : "");
};

const reportNode = (doc: Document, report: PruneReport): DocumentFragment => {
  const node = cloneTemplate("config-prune-report", doc);
  const sorted = sortCandidates(report.candidates, Date.parse(report.startedAt));
  fillSummary(node, report);
  node.querySelector("[data-prune-counts]")?.append(...counts(report, sorted).map((count) => countNode(doc, count)));
  fillCandidates(doc, node, sorted);
  return node;
};

const renderPruneStatus = (doc: Document, value: unknown): void => {
  const container = doc.querySelector("#pruneStatus");
  if (container === null) {
    return;
  }
  const report = parsePruneReport(value);
  container.replaceChildren(report === undefined ? cloneTemplate("config-prune-empty", doc) : reportNode(doc, report));
};

const errorText = (text: string): string => {
  try {
    const parsed: unknown = JSON.parse(text);
    const message: unknown = typeof parsed === "object" && parsed !== null ? Reflect.get(parsed, "error") : undefined;
    return typeof message === "string" && message !== "" ? message : text;
  } catch {
    return text;
  }
};

const readPruneJSON = async (response: Response): Promise<unknown> => {
  if (!response.ok) {
    throw new Error(errorText(await response.text()));
  }
  return readJSON(response);
};

const loadPruneStatus = async (doc: Document): Promise<void> => {
  try {
    renderPruneStatus(doc, await readPruneJSON(await fetcher("/api/prune/status")));
  } catch (error) {
    createToast(`Could not load the prune status: ${describeError(error)}`, "error");
  }
};

const savedPruneSettings = async (): Promise<Record<string, unknown> | undefined> => {
  try {
    const config = asRecord(await readPruneJSON(await fetcher("/api/config")), "config");
    const { prune } = config;
    return prune === undefined || prune === null ? {} : asRecord(prune, "prune");
  } catch (error) {
    createToast(`Could not read the saved prune settings: ${describeError(error)}`, "error");
    return undefined;
  }
};

const confirmsRun = async (
  saved: Record<string, unknown>,
  confirm: (message: string) => Promise<boolean>,
): Promise<boolean> =>
  optBool(saved, "enabled") !== true ||
  optStr(saved, "mode") !== "delete" ||
  confirm(`Run prune now in delete mode? It deletes up to ${String(optNum(saved, "max_per_run"))} eligible entries.`);

const reportRefusal = async (response: Response): Promise<void> => {
  if (response.status === HTTP_CONFLICT) {
    createToast("Prune is disabled: enable it and save first", "warning");
  } else {
    createToast(`Prune run failed: ${errorText(await response.text())}`, "error");
  }
};

const reportRun = async (doc: Document, response: Response): Promise<void> => {
  if (response.status === HTTP_CONFLICT || !response.ok) {
    await reportRefusal(response);
    return;
  }
  const report: unknown = await readJSON(response);
  renderPruneStatus(doc, report);
  const aborted = optStr(asRecord(report, "prune report"), "aborted") ?? "";
  if (aborted === "") {
    createToast("Prune run finished", "success");
  } else {
    createToast(`Prune run aborted: ${aborted}`, "warning");
  }
};

const postRun = async (doc: Document): Promise<void> => {
  try {
    await reportRun(doc, await fetcher("/api/prune/run", { method: "POST" }));
  } catch (error) {
    createToast(`Prune run: ${describeError(error)}. The run may still be in progress; reload the status.`, "warning");
  }
};

const runPruneNow = async (
  doc: Document,
  button: HTMLButtonElement,
  confirm: (message: string) => Promise<boolean>,
): Promise<void> => {
  button.disabled = true;
  try {
    const saved = await savedPruneSettings();
    if (saved !== undefined && (await confirmsRun(saved, confirm))) {
      await postRun(doc);
    }
  } finally {
    button.disabled = false;
  }
};

export { loadPruneStatus, renderPruneStatus, runPruneNow };
