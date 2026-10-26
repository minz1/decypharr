import { cloneTemplate, field } from "#ui/lib/common/dom";

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

export { cloneRows, fillFields };
