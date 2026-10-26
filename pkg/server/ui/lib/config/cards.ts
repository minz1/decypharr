import { cloneTemplate } from "#ui/lib/common/dom";

interface CardNames {
  name: (field: string) => string;
  id: (field: string) => string;
}

interface CardSpec {
  index: number;
  template: string;
  title: string;
  names: CardNames;
}

const counters = new WeakMap<Element, number>();

const nextIndex = (container: Element): number => {
  const index = counters.get(container) ?? 0;
  counters.set(container, index + 1);
  return index;
};

const nameCard = (card: ParentNode, names: CardNames): void => {
  for (const control of card.querySelectorAll<HTMLElement>("[data-field]")) {
    const field = control.dataset["field"] ?? "";
    control.setAttribute("name", names.name(field));
    control.id = names.id(field);
  }
  for (const label of card.querySelectorAll<HTMLLabelElement>("label[data-label-for]")) {
    label.htmlFor = names.id(label.dataset["labelFor"] ?? "");
  }
  for (const icon of card.querySelectorAll<HTMLElement>("[data-icon-for]")) {
    icon.id = `${names.id(icon.dataset["iconFor"] ?? "")}_icon`;
  }
};

const appendCard = (doc: Document, container: Element, spec: CardSpec): HTMLElement => {
  const card = cloneTemplate(spec.template, doc).firstElementChild;
  if (!(card instanceof HTMLElement)) {
    throw new TypeError(`template #${spec.template} is empty`);
  }
  card.dataset["index"] = String(spec.index);
  nameCard(card, spec.names);
  const title = card.querySelector("[data-card-title]");
  if (title !== null) {
    title.textContent = spec.title;
  }
  container.append(card);
  return card;
};

const cardIndex = (card: Element): string => (card instanceof HTMLElement ? (card.dataset["index"] ?? "") : "");

export { appendCard, cardIndex, nextIndex };
