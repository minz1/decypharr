import { cloneTemplate } from "#ui/lib/common/dom";

const field = (root: ParentNode, name: string): HTMLElement => {
  const found = root.querySelector(`[data-field="${name}"]`);
  if (!(found instanceof HTMLElement)) {
    throw new TypeError(`missing [data-field="${name}"]`);
  }
  return found;
};

const fillFields = (root: ParentNode, texts: Readonly<Record<string, string>>): void => {
  for (const [name, text] of Object.entries(texts)) {
    field(root, name).textContent = text;
  }
};

const cloneRows = (id: string): DocumentFragment => {
  const rows = document.createDocumentFragment();
  rows.append(...cloneTemplate(id).querySelectorAll("tbody > tr"));
  return rows;
};

export { cloneRows, field, fillFields };
