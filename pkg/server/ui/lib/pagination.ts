import { cloneTemplate } from "#ui/lib/common/dom";

interface PageItem {
  kind: "previous" | "next" | "page" | "gap";
  page: number;
  active: boolean;
  disabled: boolean;
}

const NEARBY_PAGES = 2;
const GAP_DISTANCE = 3;

const numberItems = (current: number, totalPages: number): PageItem[] => {
  const items: PageItem[] = [];
  for (let page = 1; page <= totalPages; page += 1) {
    if (page === 1 || page === totalPages || Math.abs(page - current) <= NEARBY_PAGES) {
      items.push({ kind: "page", page, active: page === current, disabled: false });
    } else if (Math.abs(page - current) === GAP_DISTANCE) {
      items.push({ kind: "gap", page, active: false, disabled: true });
    }
  }
  return items;
};

const pageItems = (current: number, totalPages: number): PageItem[] => {
  if (totalPages <= 1) {
    return [];
  }
  return [
    { kind: "previous", page: current - 1, active: false, disabled: current === 1 },
    ...numberItems(current, totalPages),
    { kind: "next", page: current + 1, active: false, disabled: current === totalPages },
  ];
};

interface PageWindow {
  page: number;
  perPage: number;
  total: number;
}

interface PageButtons {
  prefix: string;
  setDisabled: boolean;
}

const showingText = ({ page, perPage, total }: PageWindow, emptyText: string): string => {
  if (total <= 0) {
    return emptyText;
  }
  const start = (page - 1) * perPage + 1;
  const end = Math.min(start + perPage - 1, total);
  return `Showing ${start}-${end} of ${total} items`;
};

const pageButton = (item: PageItem, { prefix, setDisabled }: PageButtons): Element | null => {
  const button = cloneTemplate(`${prefix}-page-${item.kind === "page" ? "number" : item.kind}`).firstElementChild;
  if (button instanceof HTMLButtonElement) {
    button.dataset["page"] = String(item.page);
    button.classList.toggle("btn-disabled", item.disabled);
    button.classList.toggle("btn-active", item.active);
    button.disabled ||= setDisabled && item.disabled;
    if (item.kind === "page") {
      button.textContent = String(item.page);
    }
  }
  return button;
};

const renderPageItems = (container: HTMLElement, items: readonly PageItem[], buttons: PageButtons): void => {
  container.replaceChildren(...items.map((item) => pageButton(item, buttons)).filter((button) => button !== null));
};

export type { PageButtons, PageItem, PageWindow };
export { pageItems, renderPageItems, showingText };
