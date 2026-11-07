import { cloneTemplate } from "#ui/lib/common/dom";
import { asRecord, optBool, optNum, optStr } from "#ui/lib/common/json";
import { checkedOf, intOr, numberText, orDefault, setChecked, setText, textList, withId } from "#ui/lib/config/form";

const DEFAULT_WORKERS = 5;
const DEFAULT_NNTP_PERCENT = 20;

const arrNames = (doc: Document): string[] =>
  [...doc.querySelectorAll<HTMLInputElement>('.arr-config input[name$="].name"]')]
    .map((input) => input.value)
    .filter((name) => name !== "");

const arrChoice = (doc: Document, name: string, wanted: ReadonlySet<string>): DocumentFragment => {
  const choice = cloneTemplate("config-repair-arr", doc);
  const box = choice.querySelector("input");
  if (box !== null) {
    box.value = name;
    box.checked = wanted.has(name);
  }
  const label = choice.querySelector("[data-arr-name]");
  if (label !== null) {
    label.textContent = name;
  }
  return choice;
};

const renderArrChoices = (doc: Document, names: readonly string[], wanted: ReadonlySet<string>): void => {
  const group = doc.querySelector('[id="repair.arrs"]');
  if (group === null) {
    return;
  }
  group.replaceChildren(...names.map((name) => arrChoice(doc, name, wanted)));
  if (names.length === 0) {
    group.append(cloneTemplate("config-repair-arrs-empty", doc));
  }
};

const field = (doc: Document, key: string): ReturnType<typeof withId> => withId(doc, `repair.${key}`);

const numberOr = (repair: Record<string, unknown>, key: string, fallback: number): string =>
  orDefault(numberText(optNum(repair, key)), String(fallback));

// The breaker keys are empty when unset: the server reads empty or 0 as the default, and the form shows it as a placeholder.
const BREAKER_DURATIONS = [
  "confirm_gap",
  "breaker_release_after",
  "canary_recent",
  "canary_fallback",
  "canary_cache",
  "breaker_window",
] as const;
const BREAKER_COUNTS = [
  "breaker_min_confirmed",
  "breaker_percent",
  "breaker_confirmed",
  "heal_cap_per_bucket",
  "heal_cap_per_run",
  "canary_size",
] as const;

const populateBreakerFields = (doc: Document, repair: Record<string, unknown>): void => {
  for (const key of BREAKER_DURATIONS) {
    setText(field(doc, key), optStr(repair, key) ?? "");
  }
  for (const key of BREAKER_COUNTS) {
    setText(field(doc, key), numberText(optNum(repair, key)));
  }
};

// Only the keys that are set are sent, as the server stores them: a save of a config that never set one is unchanged.
const collectBreakerFields = (doc: Document): Record<string, unknown> => {
  const entries: [string, string | number][] = [
    ...BREAKER_DURATIONS.map((key): [string, string] => [key, field(doc, key)?.value.trim() ?? ""]),
    ...BREAKER_COUNTS.map((key): [string, number] => [key, intOr(field(doc, key)?.value, 0)]),
  ];
  return Object.fromEntries(entries.filter(([, value]) => value !== "" && value !== 0));
};

const populateRepairFields = (doc: Document, repair: Record<string, unknown>): void => {
  setChecked(field(doc, "enabled"), optBool(repair, "enabled") === true);
  setText(field(doc, "source"), orDefault(optStr(repair, "source"), "arr"));
  setText(field(doc, "schedule"), optStr(repair, "schedule") ?? "");
  setText(field(doc, "recheck_interval"), optStr(repair, "recheck_interval") ?? "");
  setText(field(doc, "workers"), numberOr(repair, "workers", DEFAULT_WORKERS));
  setText(field(doc, "nntp_connection_percent"), numberOr(repair, "nntp_connection_percent", DEFAULT_NNTP_PERCENT));
  setText(field(doc, "strategy"), orDefault(optStr(repair, "strategy"), "per_entry"));
  setText(field(doc, "stop_schedule"), optStr(repair, "stop_schedule") ?? "");
  setChecked(field(doc, "auto_repair"), optBool(repair, "auto_repair") === true);
  setChecked(field(doc, "verify_content"), optBool(repair, "verify_content") === true);
};

const populateRepair = (doc: Document, value: unknown): void => {
  const repair = value === undefined || value === null ? undefined : asRecord(value, "repair");
  renderArrChoices(doc, arrNames(doc), new Set(textList(repair?.["arrs"], "repair.arrs")));
  if (repair !== undefined) {
    populateRepairFields(doc, repair);
    populateBreakerFields(doc, repair);
  }
};

const chosenArrs = (doc: Document): string[] => {
  const group = doc.querySelector('[id="repair.arrs"]');
  return group === null
    ? []
    : [...group.querySelectorAll<HTMLInputElement>('input[type="checkbox"]:checked')]
        .map((choice) => choice.value)
        .filter((name) => name !== "");
};

const collectRepair = (doc: Document): Record<string, unknown> => ({
  enabled: checkedOf(field(doc, "enabled")) ?? false,
  source: orDefault(field(doc, "source")?.value, "arr"),
  schedule: field(doc, "schedule")?.value.trim() ?? "",
  recheck_interval: field(doc, "recheck_interval")?.value.trim() ?? "",
  workers: intOr(field(doc, "workers")?.value, 0),
  nntp_connection_percent: intOr(field(doc, "nntp_connection_percent")?.value, 0),
  strategy: orDefault(field(doc, "strategy")?.value, "per_entry"),
  stop_schedule: field(doc, "stop_schedule")?.value.trim() ?? "",
  auto_repair: checkedOf(field(doc, "auto_repair")) ?? false,
  verify_content: checkedOf(field(doc, "verify_content")) ?? false,
  ...collectBreakerFields(doc),
  arrs: chosenArrs(doc),
});

export { collectRepair, populateRepair };
