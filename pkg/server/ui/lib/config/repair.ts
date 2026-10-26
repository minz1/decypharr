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
  arrs: chosenArrs(doc),
});

export { collectRepair, populateRepair };
