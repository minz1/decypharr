import { byId } from "#ui/lib/common/dom";
import { cloneRows, field, fillFields } from "#ui/lib/repair/dom";
import { formatRunDuration, formatTimestamp, runBadgeClass } from "#ui/lib/repair/format";
import type { HistoryView, RepairRun } from "#ui/lib/repair/types";

const historyView = (doc: Document): HistoryView => ({
  body: byId("runsTableBody", HTMLElement, doc),
  empty: byId("noRunsMessage", HTMLElement, doc),
});

const fillCount = (cell: HTMLElement, value: number, highlight: string): void => {
  cell.textContent = String(value);
  if (value !== 0) {
    cell.classList.add(highlight, "font-medium");
  }
};

const historyRow = (run: RepairRun): DocumentFragment => {
  const row = cloneRows("repair-history-row");
  fillFields(row, {
    started: formatTimestamp(run.startedAt, "-"),
    trigger: run.trigger === "" ? "-" : run.trigger,
    status: run.status === "" ? "unknown" : run.status,
    probed: String(run.stats.probed),
    duration: formatRunDuration(run.startedAt, run.completedAt),
    error: run.error,
  });
  field(row, "status").classList.add(runBadgeClass(run.status));
  fillCount(field(row, "broken"), run.stats.broken, "text-error");
  fillCount(field(row, "repaired"), run.stats.repaired, "text-success");
  fillCount(field(row, "cleared"), run.stats.cleared, "text-warning");
  return row;
};

const renderHistory = (view: HistoryView, runs: readonly RepairRun[]): void => {
  view.body.replaceChildren(...runs.map((run) => historyRow(run)));
  view.empty.classList.toggle("hidden", runs.length > 0);
};

export { historyView, renderHistory };
