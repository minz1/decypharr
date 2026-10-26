import { byId, cloneTemplate, field } from "#ui/lib/common/dom";
import { arrKind, bindingMedia, formatTime } from "#ui/lib/reacquire/format";
import type { Binding, IndexSummary, ReacquireModalView } from "#ui/lib/reacquire/types";
import { cloneRows, fillFields } from "#ui/lib/repair/dom";

const reacquireModalView = (doc: Document): ReacquireModalView => ({
  modal: byId("reacquireModal", HTMLDialogElement, doc),
  arr: byId("reacquireArr", HTMLSelectElement, doc),
  query: byId("reacquireQuery", HTMLInputElement, doc),
  results: byId("bindingResults", HTMLElement, doc),
  strategy: byId("reacquireStrategy", HTMLSelectElement, doc),
  error: byId("reacquireError", HTMLElement, doc),
  submit: byId("reacquireSubmit", HTMLButtonElement, doc),
});

const message = (templateId: string, text: string): DocumentFragment => {
  const row = cloneRows(templateId);
  field(row, "message").textContent = text;
  return row;
};

const indexCard = (summary: IndexSummary, now: number): DocumentFragment => {
  const card = cloneTemplate("reacquire-index-card");
  fillFields(card, {
    name: summary.arrName,
    bindings: String(summary.bindings),
    actionable: `${summary.actionable} can be replaced · updated ${formatTime(summary.updatedAt, now)}`,
  });
  const type = field(card, "type");
  if (arrKind(summary.arrName, summary.arrType)) {
    type.textContent = summary.arrType;
  } else {
    type.remove();
  }
  return card;
};

const renderIndex = (container: HTMLElement, summaries: readonly IndexSummary[], now: number): void => {
  if (summaries.length === 0) {
    container.replaceChildren(cloneTemplate("reacquire-index-empty"));
    return;
  }
  container.replaceChildren(...summaries.map((summary) => indexCard(summary, now)));
};

const indexUnavailable = (container: HTMLElement): void => {
  const note = document.createElement("div");
  note.classList.add("text-sm", "opacity-60");
  note.textContent = "Index is unavailable.";
  container.replaceChildren(note);
};

const fillArrFilter = (select: HTMLSelectElement, summaries: readonly IndexSummary[]): void => {
  const current = select.value;
  select.replaceChildren(
    ...["", ...summaries.map((summary) => summary.arrName)].map((name) => {
      const option = document.createElement("option");
      option.value = name;
      option.textContent = name === "" ? "All" : name;
      return option;
    }),
  );
  select.value = current;
};

const bindingRow = (binding: Binding, index: number): DocumentFragment => {
  const row = cloneRows("reacquire-binding-row");
  field(row, "row").dataset["index"] = String(index);
  const radio = field(row, "radio");
  radio.id = `binding-${index}`;
  if (radio instanceof HTMLInputElement) {
    radio.disabled = binding.arrFileId === 0;
  }
  fillFields(row, {
    file: binding.entryFileName === "" ? binding.entryFileId : binding.entryFileName,
    entry: binding.entryName,
    arr: binding.arrName,
    media: bindingMedia(binding),
  });
  return row;
};

const renderBindings = (body: HTMLElement, bindings: readonly Binding[]): void => {
  if (bindings.length === 0) {
    body.replaceChildren(message("reacquire-binding-message", "No indexed file matches."));
    return;
  }
  body.replaceChildren(...bindings.map((binding, index) => bindingRow(binding, index)));
};

const bindingsFailed = (body: HTMLElement): void => {
  body.replaceChildren(message("reacquire-binding-message", "Search failed."));
};

const markBinding = (body: HTMLElement, index: number): void => {
  for (const [position, radio] of body.querySelectorAll<HTMLInputElement>('input[type="radio"]').entries()) {
    radio.checked = position === index;
  }
};

export {
  bindingsFailed,
  fillArrFilter,
  indexUnavailable,
  markBinding,
  reacquireModalView,
  renderBindings,
  renderIndex,
};
