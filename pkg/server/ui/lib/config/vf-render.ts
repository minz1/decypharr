import { cloneTemplate } from "#ui/lib/common/dom";
import { CASE_FIELDS, FIELDS, SAMPLES, fieldDef, normalizeCondition } from "#ui/lib/config/vf-model";
import type { Condition, Sample } from "#ui/lib/config/vf-types";

interface RowIds {
  folder: number;
  condition: number;
}

interface RowSpec {
  ids: RowIds;
  given: Partial<Condition>;
  providers: readonly string[];
}

const option = (doc: Document, value: string, label: string): HTMLOptionElement => {
  const node = doc.createElement("option");
  node.value = value;
  node.textContent = label;
  return node;
};

const fillSelect = (select: HTMLSelectElement | null, options: readonly HTMLOptionElement[], value: string): void => {
  if (select === null) {
    return;
  }
  select.replaceChildren(...options);
  const known = options.some((candidate) => candidate.value === value);
  select.value = known ? value : (options[0]?.value ?? "");
};

const element = <Wanted extends Element>(
  root: ParentNode,
  selector: string,
  type: abstract new () => Wanted,
): Wanted => {
  const found = root.querySelector(selector);
  if (!(found instanceof type)) {
    throw new TypeError(`virtual folder template has no ${selector}`);
  }
  return found;
};

const rowId = (ids: RowIds, part: string): string => `virtual_folder_${ids.folder}_condition_${ids.condition}_${part}`;

const valueSelect = (doc: Document, options: readonly HTMLOptionElement[], value: string): HTMLSelectElement => {
  const select = element(cloneTemplate("config-vf-value-select", doc), "select", HTMLSelectElement);
  fillSelect(select, options, value);
  return select;
};

const providerSelect = (doc: Document, condition: Condition, providers: readonly string[]): HTMLSelectElement => {
  const extra = condition.value !== "" && !providers.includes(condition.value);
  const values = extra ? [...providers, condition.value] : providers;
  return valueSelect(
    doc,
    values.map((value) => option(doc, value, value)),
    condition.value,
  );
};

const valueInput = (doc: Document, condition: Condition): HTMLInputElement => {
  const input = element(cloneTemplate("config-vf-value-input", doc), "input", HTMLInputElement);
  input.type = condition.field === "file_count" ? "number" : "text";
  if (condition.field === "file_count") {
    input.min = "0";
    input.step = "1";
    input.inputMode = "numeric";
  }
  input.value = condition.value;
  input.placeholder = fieldDef(condition.field).placeholder ?? "";
  return input;
};

const valueControl = (doc: Document, condition: Condition, providers: readonly string[]): HTMLElement => {
  if (condition.field === "protocol") {
    return valueSelect(doc, [option(doc, "torrent", "Torrent"), option(doc, "nzb", "Usenet / NZB")], condition.value);
  }
  if (condition.field === "provider" && providers.length > 0) {
    return providerSelect(doc, condition, providers);
  }
  return valueInput(doc, condition);
};

const labelRow = (row: HTMLElement, ids: RowIds): void => {
  for (const label of row.querySelectorAll<HTMLLabelElement>("label[data-vf-for]")) {
    label.htmlFor = rowId(ids, label.dataset["vfFor"] ?? "");
  }
  const help = element(row, "[data-vf-field-help]", HTMLElement);
  help.id = rowId(ids, "help");
  const field = element(row, "[data-vf-field]", HTMLSelectElement);
  field.id = rowId(ids, "field");
  field.setAttribute("aria-describedby", help.id);
  element(row, "[data-vf-operator]", HTMLSelectElement).id = rowId(ids, "operator");
};

const fillRowControls = (doc: Document, row: HTMLElement, condition: Condition): void => {
  const def = fieldDef(condition.field);
  const fields = FIELDS.map((field) => option(doc, field.value, field.label));
  fillSelect(row.querySelector<HTMLSelectElement>("[data-vf-field]"), fields, condition.field);
  const operators = def.operators.map(([value, label]) => option(doc, value, label));
  fillSelect(row.querySelector<HTMLSelectElement>("[data-vf-operator]"), operators, condition.operator);
  element(row, "[data-vf-field-help]", HTMLElement).textContent = def.help;
  row.querySelector("[data-vf-case-wrap]")?.classList.toggle("hidden", !CASE_FIELDS.has(condition.field));
  element(row, "[data-vf-case]", HTMLInputElement).checked = condition.case_sensitive;
};

const conditionRow = (doc: Document, spec: RowSpec): HTMLElement => {
  const condition = normalizeCondition(spec.given);
  const row = element(cloneTemplate("config-vf-condition", doc), "[data-vf-condition]", HTMLElement);
  row.dataset["vfCondition"] = String(spec.ids.condition);
  row.dataset["vfCurrentField"] = condition.field;
  labelRow(row, spec.ids);
  fillRowControls(doc, row, condition);
  const control = valueControl(doc, condition, spec.providers);
  control.id = rowId(spec.ids, "value");
  row.querySelector("[data-vf-value-slot]")?.append(control);
  return row;
};

const sampleButton = (doc: Document, sample: Sample): HTMLElement => {
  const button = element(cloneTemplate("config-vf-sample", doc), "button", HTMLButtonElement);
  button.title = sample.description;
  button.setAttribute("aria-label", `${sample.label}: ${sample.description}`);
  button.dataset["sample"] = sample.key;
  button.textContent = sample.label;
  return button;
};

const prefixIds = (card: HTMLElement, id: number): void => {
  const prefix = `virtual_folder_${id}_`;
  for (const node of card.querySelectorAll<HTMLElement>("[data-vf-id]")) {
    node.id = `${prefix}${node.dataset["vfId"] ?? ""}`;
  }
  for (const label of card.querySelectorAll<HTMLLabelElement>("label[data-vf-for]")) {
    label.htmlFor = `${prefix}${label.dataset["vfFor"] ?? ""}`;
  }
  for (const node of card.querySelectorAll<HTMLElement>("[data-vf-describedby]")) {
    node.setAttribute("aria-describedby", `${prefix}${node.dataset["vfDescribedby"] ?? ""}`);
  }
  for (const node of [card, ...card.querySelectorAll<HTMLElement>("[data-vf-labelledby]")].filter(
    (candidate) => candidate.dataset["vfLabelledby"] !== undefined,
  )) {
    node.setAttribute("aria-labelledby", `${prefix}${node.dataset["vfLabelledby"] ?? ""}`);
  }
};

const titleCard = (card: HTMLElement, title: string): void => {
  element(card, "[data-vf-title]", HTMLElement).textContent = title;
  element(card, "[data-action='remove-folder']", HTMLElement).setAttribute(
    "aria-label",
    `Remove virtual folder ${title}`,
  );
  element(card, "legend", HTMLElement).textContent = `Conditions for ${title}`;
};

const folderCard = (
  doc: Document,
  id: number,
  definition: { name: string; match: string; include_bad: boolean },
): HTMLElement => {
  const card = element(cloneTemplate("config-vf-folder", doc), "[data-virtual-folder]", HTMLElement);
  card.dataset["virtualFolder"] = String(id);
  prefixIds(card, id);
  card.querySelector("[data-vf-samples]")?.append(...SAMPLES.map((sample) => sampleButton(doc, sample)));
  element(card, "[data-vf-name]", HTMLInputElement).value = definition.name;
  element(card, "[data-vf-match]", HTMLSelectElement).value = definition.match === "any" ? "any" : "all";
  element(card, "[data-vf-include-bad]", HTMLInputElement).checked = definition.include_bad;
  return card;
};

export type { RowIds };
export { conditionRow, element, folderCard, titleCard };
