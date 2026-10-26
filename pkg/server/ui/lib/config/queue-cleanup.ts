import { cloneTemplate } from "#ui/lib/common/dom";
import { asArray, isRecord, optStr } from "#ui/lib/common/json";

const CATALOG = [
  { id: "failed_download", label: "Failed download" },
  { id: "title_mismatch", label: "Title mismatch (automatic import not possible)" },
  { id: "matched_by_id", label: "Release matched to series/movie by ID" },
  { id: "unable_to_parse", label: "Unable to parse download" },
  { id: "no_eligible_files", label: "No files eligible for import" },
  { id: "episodes_missing", label: "Episodes missing / not imported from release" },
  { id: "file_empty", label: "Downloaded file is empty" },
  { id: "invalid_local_path", label: "Invalid local path (remote path mapping)" },
  { id: "not_grabbed", label: "Not grabbed by the arr / no category" },
] as const;

interface CustomRule {
  match: string;
  action: string;
}

const selectAction = (row: ParentNode, action: string): void => {
  const select = row.querySelector<HTMLSelectElement>(".queue-cleanup-action");
  if (select !== null) {
    const known = [...select.options].some((option) => option.value === action);
    select.value = known ? action : (select.options[0]?.value ?? "");
  }
};

const addQueueCleanupRow = (doc: Document, rule?: CustomRule): void => {
  const custom = doc.querySelector("#queueCleanupCustom");
  if (custom === null) {
    return;
  }
  const row = cloneTemplate("config-queue-custom-row", doc);
  const match = row.querySelector<HTMLInputElement>(".queue-cleanup-match");
  if (match !== null) {
    match.value = rule?.match ?? "";
  }
  selectAction(row, rule?.action ?? "");
  custom.append(row);
};

const catalogRow = (doc: Document, entry: (typeof CATALOG)[number], action: string): DocumentFragment => {
  const row = cloneTemplate("config-queue-catalog-row", doc);
  const root = row.firstElementChild;
  if (root instanceof HTMLElement) {
    root.dataset["ruleId"] = entry.id;
  }
  const label = row.querySelector("[data-rule-label]");
  if (label !== null) {
    label.textContent = entry.label;
  }
  selectAction(row, action);
  return row;
};

const readRules = (value: unknown): { actions: Map<string, string>; custom: CustomRule[] } => {
  const rules = isRecord(value) && Array.isArray(value["rules"]) ? asArray(value["rules"], "queue_cleanup.rules") : [];
  const actions = new Map<string, string>();
  const custom: CustomRule[] = [];
  for (const rule of rules.filter((item) => isRecord(item))) {
    const id = optStr(rule, "id") ?? "";
    const match = optStr(rule, "match") ?? "";
    if (id !== "") {
      actions.set(id, optStr(rule, "action") ?? "");
    } else if (match.trim() !== "") {
      custom.push({ match, action: optStr(rule, "action") ?? "" });
    }
  }
  return { actions, custom };
};

const populateQueueCleanup = (doc: Document, value: unknown): void => {
  const catalog = doc.querySelector("#queueCleanupCatalog");
  const customRows = doc.querySelector("#queueCleanupCustom");
  if (catalog === null || customRows === null) {
    return;
  }
  const { actions, custom } = readRules(value);
  catalog.replaceChildren(...CATALOG.map((entry) => catalogRow(doc, entry, actions.get(entry.id) ?? "")));
  customRows.replaceChildren();
  for (const rule of custom) {
    addQueueCleanupRow(doc, rule);
  }
};

const actionOf = (row: Element): string => row.querySelector<HTMLSelectElement>(".queue-cleanup-action")?.value ?? "";

const collectQueueCleanup = (doc: Document): Record<string, unknown> => {
  const catalog = [...doc.querySelectorAll<HTMLElement>("#queueCleanupCatalog [data-rule-id]")].map((row) => ({
    id: row.dataset["ruleId"],
    action: actionOf(row),
  }));
  const custom = [...doc.querySelectorAll("#queueCleanupCustom .queue-cleanup-custom-row")]
    .map((row) => ({
      match: row.querySelector<HTMLInputElement>(".queue-cleanup-match")?.value.trim() ?? "",
      action: actionOf(row),
    }))
    .filter((rule) => rule.match !== "");
  return { rules: [...catalog, ...custom] };
};

export { addQueueCleanupRow, collectQueueCleanup, populateQueueCleanup };
