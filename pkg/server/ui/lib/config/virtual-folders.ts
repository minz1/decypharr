import { byId, cloneTemplate } from "#ui/lib/common/dom";
import { asArray, asRecord, optStr } from "#ui/lib/common/json";
import { orDefault } from "#ui/lib/config/form";
import { SAMPLES, refreshedCondition } from "#ui/lib/config/vf-model";
import { conditionRow, element, folderCard, titleCard } from "#ui/lib/config/vf-render";
import type { Condition, Folder } from "#ui/lib/config/vf-types";

interface FolderState {
  next: number;
  conditions: Map<number, number>;
  providers: string[];
}

const states = new WeakMap<Document, FolderState>();

const stateOf = (doc: Document): FolderState => {
  const known = states.get(doc);
  if (known !== undefined) {
    return known;
  }
  const created: FolderState = { next: 0, conditions: new Map(), providers: [] };
  states.set(doc, created);
  return created;
};

const container = (doc: Document): HTMLElement => byId("virtualFoldersContainer", HTMLElement, doc);

const announce = (doc: Document, message: string): void => {
  const status = doc.querySelector("#virtualFolderEditorStatus");
  if (status !== null) {
    status.textContent = message;
  }
};

const noConditions = (doc: Document): DocumentFragment => cloneTemplate("config-vf-no-conditions", doc);

const readCondition = (value: unknown): Partial<Condition> => {
  const record = asRecord(value, "virtual folder condition");
  const field = optStr(record, "field");
  const operator = optStr(record, "operator");
  const text = optStr(record, "value");
  return {
    case_sensitive: Boolean(record["case_sensitive"]),
    ...(field === undefined ? {} : { field }),
    ...(operator === undefined ? {} : { operator }),
    ...(text === undefined ? {} : { value: text }),
  };
};

const startingConditions = (definition: Record<string, unknown> | undefined): Partial<Condition>[] => {
  if (definition === undefined) {
    return [{ field: "entry_name", operator: "contains", value: "" }];
  }
  const given = definition["conditions"];
  return Array.isArray(given) ? asArray(given, "conditions").map((item) => readCondition(item)) : [];
};

const fillConditions = (doc: Document, card: HTMLElement, conditions: readonly Partial<Condition>[]): void => {
  const list = element(card, "[data-vf-conditions]", HTMLElement);
  const folder = Number(card.dataset["virtualFolder"]);
  const { providers } = stateOf(doc);
  list.replaceChildren(
    ...(conditions.length === 0
      ? [noConditions(doc)]
      : conditions.map((given, index) => conditionRow(doc, { ids: { folder, condition: index }, given, providers }))),
  );
};

const newFolderCard = (doc: Document, id: number, definition: Record<string, unknown>): HTMLElement => {
  const name = optStr(definition, "name") ?? "";
  const card = folderCard(doc, id, {
    name,
    match: optStr(definition, "match") ?? "",
    include_bad: Boolean(definition["include_bad"]),
  });
  titleCard(card, orDefault(name, `New virtual folder ${id + 1}`));
  return card;
};

const takeFolderId = (doc: Document, conditionCount: number): number => {
  const state = stateOf(doc);
  const id = state.next;
  state.next += 1;
  state.conditions.set(id, conditionCount);
  return id;
};

const addVirtualFolder = (doc: Document, definition?: Record<string, unknown>, focus = true): void => {
  container(doc).querySelector("[data-vf-empty-state]")?.remove();
  const conditions = startingConditions(definition);
  const id = takeFolderId(doc, conditions.length);
  const card = newFolderCard(doc, id, definition ?? {});
  fillConditions(doc, card, conditions);
  container(doc).append(card);
  if (focus) {
    card.querySelector<HTMLInputElement>("[data-vf-name]")?.focus();
    announce(doc, `Added ${element(card, "[data-vf-title]", HTMLElement).textContent}.`);
  }
};

const renderEmptyState = (doc: Document): void => {
  container(doc).replaceChildren(cloneTemplate("config-vf-empty", doc));
};

const providerNames = (doc: Document): string[] => {
  const debrids = [...doc.querySelectorAll<HTMLInputElement>('.debrid-config input[name$="].name"]')]
    .map((input) => input.value)
    .filter((name) => name !== "");
  return doc.querySelector(".usenet-provider") === null ? debrids : [...debrids, "usenet"];
};

const populateVirtualFolders = (doc: Document, value: unknown): void => {
  stateOf(doc).providers = providerNames(doc);
  const folders = value ?? [];
  if (!Array.isArray(folders)) {
    return;
  }
  if (folders.length === 0) {
    renderEmptyState(doc);
    return;
  }
  for (const folder of asArray(folders, "virtual_folders")) {
    addVirtualFolder(doc, asRecord(folder, "virtual folder"), false);
  }
};

const controlValue = (root: Element, selector: string): string | undefined =>
  root.querySelector<HTMLInputElement | HTMLSelectElement>(selector)?.value;

const conditionFromRow = (row: Element): Condition => ({
  field: orDefault(controlValue(row, "[data-vf-field]"), "entry_name"),
  operator: orDefault(controlValue(row, "[data-vf-operator]"), "contains"),
  value: (controlValue(row, "[data-vf-value]") ?? "").trim(),
  case_sensitive: row.querySelector<HTMLInputElement>("[data-vf-case]")?.checked === true,
});

const collectFolder = (card: Element): Folder => ({
  name: (controlValue(card, "[data-vf-name]") ?? "").trim(),
  match: orDefault(controlValue(card, "[data-vf-match]"), "all"),
  include_bad: card.querySelector<HTMLInputElement>("[data-vf-include-bad]")?.checked === true,
  conditions: [...card.querySelectorAll("[data-vf-condition]")].map((row) => conditionFromRow(row)),
});

const collectVirtualFolders = (doc: Document): Folder[] =>
  [...container(doc).querySelectorAll("[data-virtual-folder]")].map((card) => collectFolder(card));

const invalidatePreview = (card: Element | null): void => {
  const result = card?.querySelector("[data-vf-preview-result]");
  result?.classList.add("hidden");
  result?.replaceChildren();
};

const conditionList = (doc: Document, folder: number): HTMLElement | null =>
  doc.querySelector(`[id="virtual_folder_${folder}_conditions"]`);

const newRow = (doc: Document, folder: number, given: Partial<Condition> = {}): HTMLElement => {
  const { conditions, providers } = stateOf(doc);
  const condition = conditions.get(folder) ?? 0;
  conditions.set(folder, condition + 1);
  return conditionRow(doc, { ids: { folder, condition }, given, providers });
};

const addCondition = (doc: Document, folder: number): void => {
  const list = conditionList(doc, folder);
  if (list === null) {
    return;
  }
  list.querySelector("[data-vf-no-conditions]")?.remove();
  const row = newRow(doc, folder);
  list.append(row);
  row.querySelector<HTMLSelectElement>("[data-vf-field]")?.focus();
  invalidatePreview(list.closest("[data-virtual-folder]"));
  announce(doc, "Condition added.");
};

const placeSample = (doc: Document, list: HTMLElement, given: { folder: number; sample: Condition }): HTMLElement => {
  const blank = [...list.querySelectorAll<HTMLElement>("[data-vf-condition]")].find(
    (row) => conditionFromRow(row).value === "",
  );
  if (blank === undefined) {
    list.querySelector("[data-vf-no-conditions]")?.remove();
    const row = newRow(doc, given.folder, given.sample);
    list.append(row);
    return row;
  }
  const ids = { folder: given.folder, condition: Number(blank.dataset["vfCondition"]) };
  const row = conditionRow(doc, { ids, given: given.sample, providers: stateOf(doc).providers });
  blank.replaceWith(row);
  return row;
};

const applySample = (doc: Document, folder: number, key: string): void => {
  const sample = SAMPLES.find((candidate) => candidate.key === key);
  const list = conditionList(doc, folder);
  if (sample === undefined || list === null) {
    return;
  }
  const row = placeSample(doc, list, { folder, sample: sample.condition });
  row.querySelector<HTMLElement>("[data-vf-value]")?.focus();
  invalidatePreview(list.closest("[data-virtual-folder]"));
  announce(doc, `${sample.label} example applied. You can edit it before saving.`);
};

const removeCondition = (doc: Document, folder: number, condition: string): void => {
  const list = conditionList(doc, folder);
  if (list === null) {
    return;
  }
  list.querySelector(`[data-vf-condition="${CSS.escape(condition)}"]`)?.remove();
  if (list.querySelector("[data-vf-condition]") === null) {
    list.replaceChildren(noConditions(doc));
  }
  invalidatePreview(list.closest("[data-virtual-folder]"));
  announce(doc, "Condition removed.");
};

const refreshCondition = (doc: Document, card: HTMLElement, row: HTMLElement): void => {
  const { providers } = stateOf(doc);
  const ids = { folder: Number(card.dataset["virtualFolder"]), condition: Number(row.dataset["vfCondition"]) };
  const given = refreshedCondition(conditionFromRow(row), row.dataset["vfCurrentField"] ?? "", providers);
  const fresh = conditionRow(doc, { ids, given, providers });
  row.replaceWith(fresh);
  fresh.querySelector<HTMLSelectElement>("[data-vf-field]")?.focus();
};

const retitle = (card: HTMLElement, name: HTMLInputElement): void => {
  const title = orDefault(name.value.trim(), `New virtual folder ${Number(card.dataset["virtualFolder"]) + 1}`);
  element(card, "[data-vf-title]", HTMLElement).textContent = title;
};

const handleFolderInput = (doc: Document, event: Event): void => {
  const { target } = event;
  const card = target instanceof HTMLElement ? target.closest<HTMLElement>("[data-virtual-folder]") : undefined;
  if (!(target instanceof HTMLElement) || card === null || card === undefined) {
    return;
  }
  const row = target.closest<HTMLElement>("[data-vf-condition]");
  if (event.type === "change" && target.matches("[data-vf-field]") && row !== null) {
    refreshCondition(doc, card, row);
  }
  if (target instanceof HTMLInputElement && target.matches("[data-vf-name]")) {
    retitle(card, target);
  }
  invalidatePreview(card);
};

const removeFolder = async (
  doc: Document,
  card: HTMLElement,
  confirm: (message: string) => Promise<boolean>,
): Promise<void> => {
  const name = orDefault(controlValue(card, "[data-vf-name]")?.trim(), "this virtual folder");
  if (!(await confirm(`Remove "${name}"?\n\nThis removes only the filtered view. Your media will not be deleted.`))) {
    return;
  }
  card.remove();
  stateOf(doc).conditions.delete(Number(card.dataset["virtualFolder"]));
  if (container(doc).querySelector("[data-virtual-folder]") === null) {
    renderEmptyState(doc);
  }
  announce(doc, `${name} removed. Save settings to apply this change.`);
};

export {
  addCondition,
  addVirtualFolder,
  applySample,
  collectFolder,
  collectVirtualFolders,
  handleFolderInput,
  populateVirtualFolders,
  removeCondition,
  removeFolder,
};
