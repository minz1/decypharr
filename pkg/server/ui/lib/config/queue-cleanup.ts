import { cloneTemplate } from "#ui/lib/common/dom";
import { asArray, isRecord, optNum, optStr } from "#ui/lib/common/json";
import { intOr, numberText, setText, withId } from "#ui/lib/config/form";

const LIMIT_COUNTS = ["blocklist_per_bucket", "blocklist_per_arr"] as const;
const LIMIT_WINDOW = "blocklist_window";

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

// The limits are empty when unset: the server reads empty or 0 as the default, and the form shows it as a placeholder.
const field = (doc: Document, key: string): ReturnType<typeof withId> => withId(doc, `queue_cleanup.${key}`);

const populateLimits = (doc: Document, value: unknown): void => {
  const queue = isRecord(value) ? value : {};
  for (const key of LIMIT_COUNTS) {
    setText(field(doc, key), numberText(optNum(queue, key)));
  }
  setText(field(doc, LIMIT_WINDOW), optStr(queue, LIMIT_WINDOW) ?? "");
};

// Only the limits that are set are sent, as the server stores them: a save of a config that never set one is unchanged.
const collectLimits = (doc: Document): Record<string, unknown> => {
  const entries: [string, string | number][] = [
    ...LIMIT_COUNTS.map((key): [string, number] => [key, intOr(field(doc, key)?.value, 0)]),
    [LIMIT_WINDOW, field(doc, LIMIT_WINDOW)?.value.trim() ?? ""],
  ];
  return Object.fromEntries(entries.filter(([, limit]) => limit !== "" && limit !== 0));
};

const populateQueueCleanup = (doc: Document, value: unknown): void => {
  populateLimits(doc, value);
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
  return { rules: [...catalog, ...custom], ...collectLimits(doc) };
};

export { addQueueCleanupRow, collectQueueCleanup, populateQueueCleanup };
