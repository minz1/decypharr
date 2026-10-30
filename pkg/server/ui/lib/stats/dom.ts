import { cloneTemplate } from "#ui/lib/common/dom";

const field = (root: ParentNode, name: string): HTMLElement => {
  const found = root.querySelector(`[data-field="${name}"]`);
  if (!(found instanceof HTMLElement)) {
    throw new TypeError(`missing [data-field="${name}"]`);
  }
  return found;
};

const setFields = (root: ParentNode, texts: Readonly<Record<string, string>>): void => {
  for (const [name, value] of Object.entries(texts)) {
    field(root, name).textContent = value;
  }
};

const cloneElement = (id: string): HTMLElement => {
  const element = cloneTemplate(id).firstElementChild;
  if (!(element instanceof HTMLElement)) {
    throw new TypeError(`empty template #${id}`);
  }
  return element;
};

const emptyMessage = (message: string): HTMLElement => {
  const element = cloneElement("stats-empty");
  element.textContent = message;
  return element;
};

export { cloneElement, emptyMessage, field, setFields };
