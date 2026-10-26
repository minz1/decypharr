import { byId, cloneTemplate, field } from "#ui/lib/common/dom";
import { formatTimestamp, healthColor } from "#ui/lib/repair/format";
import type { RepairStatus, RunStats, StatusView } from "#ui/lib/repair/types";

const HEALTH_ORDER = ["healthy", "broken", "repairing", "stale", "unknown", "unsupported"] as const;

const RUN_STAT_FIELDS: readonly (readonly [keyof RunStats, string])[] = [
  ["candidates", "Candidates"],
  ["skippedFresh", "Skipped"],
  ["probed", "Probed"],
  ["healthy", "Healthy"],
  ["broken", "Broken"],
  ["repaired", "Repaired"],
  ["cleared", "Cleared"],
  ["repairFailed", "Repair fail"],
];

const statusView = (doc: Document): StatusView => ({
  line: byId("repairStatusLine", HTMLElement, doc),
  runNow: byId("runNowBtn", HTMLButtonElement, doc),
  stop: byId("stopRunBtn", HTMLButtonElement, doc),
  fixBroken: byId("fixBrokenBtn", HTMLButtonElement, doc),
  clearState: byId("clearStateBtn", HTMLButtonElement, doc),
  viewBroken: byId("viewBrokenBtn", HTMLButtonElement, doc),
  brokenBadge: byId("brokenCountBadge", HTMLElement, doc),
  brokenModalCount: byId("brokenModalCount", HTMLElement, doc),
  panel: byId("activeRunPanel", HTMLElement, doc),
  stage: byId("activeRunStage", HTMLElement, doc),
  runId: byId("activeRunIdText", HTMLElement, doc),
  started: byId("activeRunStarted", HTMLElement, doc),
  stats: byId("activeRunStats", HTMLElement, doc),
  grid: byId("healthCountsGrid", HTMLElement, doc),
  clearCounts: [...doc.querySelectorAll<HTMLElement>("[data-clear-state-count]")],
});

const countOf = (counts: Readonly<Record<string, number>>, key: string): number =>
  Object.hasOwn(counts, key) ? (counts[key] ?? 0) : 0;

const renderBrokenCount = (view: StatusView, broken: number): void => {
  view.brokenBadge.textContent = String(broken);
  view.brokenBadge.classList.toggle("hidden", broken === 0);
  view.brokenModalCount.textContent = String(broken);
};

const renderClearCounts = (view: StatusView, counts: Readonly<Record<string, number>>): void => {
  for (const badge of view.clearCounts) {
    badge.textContent = String(countOf(counts, badge.dataset["clearStateCount"] ?? ""));
  }
};

const labelled = (templateId: string, label: string, value: number): DocumentFragment => {
  const card = cloneTemplate(templateId);
  field(card, "label").textContent = label;
  field(card, "value").textContent = String(value);
  return card;
};

const renderRunStats = (container: HTMLElement, stats: RunStats): void => {
  container.replaceChildren(...RUN_STAT_FIELDS.map(([key, label]) => labelled("repair-run-stat", label, stats[key])));
};

const healthCard = (key: string, count: number): DocumentFragment => {
  const card = labelled("repair-health-card", key, count);
  const colour = healthColor(key);
  if (colour !== "") {
    field(card, "value").classList.add(colour);
  }
  return card;
};

const statusLine = (status: RepairStatus): string =>
  status.enabled
    ? `Repair enabled · next scheduled run: ${formatTimestamp(status.nextRunAt ?? "", "unknown")}`
    : 'Repair is disabled. Enable it in Settings → Repair, or click "Run now" for a one-off check.';

const renderRunHeader = (view: StatusView, [stage, id, started]: readonly [string, string, string]): void => {
  view.stage.textContent = stage;
  view.runId.textContent = id;
  view.started.textContent = started;
};

const renderActiveRun = (view: StatusView, status: RepairStatus): void => {
  const run = status.activeRun;
  view.stop.disabled = run === undefined;
  view.runNow.disabled = run !== undefined;
  view.panel.classList.toggle("hidden", run === undefined);
  if (run === undefined) {
    renderRunHeader(view, ["-", "-", "-"]);
    return;
  }
  renderRunHeader(view, [
    run.stage === "" ? "running" : run.stage,
    run.id === "" ? "-" : run.id,
    formatTimestamp(run.startedAt, "-"),
  ]);
  renderRunStats(view.stats, run.stats);
};

const renderBrokenButtons = (view: StatusView, status: RepairStatus): void => {
  const broken = countOf(status.healthCounts, "broken");
  const running = status.activeRun !== undefined;
  renderBrokenCount(view, broken);
  view.fixBroken.disabled = running || broken === 0;
  view.clearState.disabled = running;
  view.viewBroken.disabled = broken === 0;
};

const renderStatus = (view: StatusView, status: RepairStatus): void => {
  view.line.textContent = statusLine(status);
  renderBrokenButtons(view, status);
  renderClearCounts(view, status.healthCounts);
  renderActiveRun(view, status);
  view.grid.replaceChildren(...HEALTH_ORDER.map((key) => healthCard(key, countOf(status.healthCounts, key))));
};

export { renderBrokenCount, renderClearCounts, renderStatus, statusView };
