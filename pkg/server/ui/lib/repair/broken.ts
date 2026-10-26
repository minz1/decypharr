import { byId, cloneTemplate } from "#ui/lib/common/dom";
import { cloneRows, field, fillFields } from "#ui/lib/repair/dom";
import { brokenFileIds, formatFileSize, formatTimestamp, pageButtons, pageCount } from "#ui/lib/repair/format";
import type { BrokenFile, BrokenView, EntryHealth } from "#ui/lib/repair/types";

const BROKEN_PAGE_SIZE = 25;

const brokenView = (doc: Document): BrokenView => ({
  modal: byId("brokenModal", HTMLDialogElement, doc),
  body: byId("brokenTableBody", HTMLElement, doc),
  empty: byId("noBrokenMessage", HTMLElement, doc),
  bar: byId("brokenPaginationBar", HTMLElement, doc),
  info: byId("brokenPaginationInfo", HTMLElement, doc),
  controls: byId("brokenPaginationControls", HTMLElement, doc),
});

const arrCell = (cell: HTMLElement, file: BrokenFile): void => {
  if (file.arrName === "") {
    const dash = document.createElement("span");
    dash.classList.add("opacity-50");
    dash.textContent = "—";
    cell.replaceChildren(dash);
    return;
  }
  cell.textContent = file.arrKind === "" ? file.arrName : `${file.arrName} (${file.arrKind})`;
};

const idsCell = (cell: HTMLElement, file: BrokenFile): void => {
  const ids = brokenFileIds(file);
  if (ids === "") {
    return;
  }
  const span = document.createElement("span");
  span.classList.add("font-mono", "text-[10px]", "opacity-70");
  span.textContent = ids;
  cell.replaceChildren(span);
};

const fileRow = (file: BrokenFile): DocumentFragment => {
  const row = cloneRows("repair-broken-file-row");
  field(row, "name").textContent = file.fileName;
  field(row, "reason").textContent = file.reason === "" ? "-" : file.reason;
  field(row, "size").textContent = formatFileSize(file.size);
  arrCell(field(row, "arr"), file);
  idsCell(field(row, "ids"), file);
  return row;
};

const brokenFiles = (files: readonly BrokenFile[]): Node => {
  if (files.length === 0) {
    const none = document.createElement("div");
    none.classList.add("text-sm", "opacity-60");
    none.textContent = "No broken file details.";
    return none;
  }
  const table = cloneTemplate("repair-broken-files");
  field(table, "rows").replaceChildren(...files.map((file) => fileRow(file)));
  return table;
};

const nameButton = (button: HTMLElement, label: string, name: string): void => {
  button.dataset["name"] = name;
  button.setAttribute("aria-label", `${label} ${name}`);
};

const brokenRow = (entry: EntryHealth): DocumentFragment => {
  const row = cloneRows("repair-broken-row");
  fillFields(row, {
    name: entry.entryName,
    protocol: entry.protocol === "" ? "unknown" : entry.protocol,
    files: String(entry.fileCount),
    broken: String(entry.brokenCount),
    reason: entry.failureReason === "" ? "-" : entry.failureReason,
    checked: formatTimestamp(entry.lastCheckedAt, "-"),
    repaired: formatTimestamp(entry.lastRepairAt, "-"),
  });
  nameButton(field(row, "recheck"), "Recheck", entry.entryName);
  nameButton(field(row, "fix"), "Fix", entry.entryName);
  field(row, "detail-body").replaceChildren(brokenFiles(entry.brokenFiles));
  return row;
};

const pageButton = (label: string, target: number | undefined, classes: readonly string[]): DocumentFragment => {
  const fragment = cloneTemplate("repair-page-button");
  const button = fragment.querySelector<HTMLElement>("button");
  if (button !== null) {
    button.textContent = label;
    button.classList.add(...classes);
    if (target !== undefined) {
      button.dataset["page"] = String(target);
    }
  }
  return fragment;
};

const renderPagination = (view: BrokenView, total: number, page: number): void => {
  view.bar.classList.toggle("hidden", total === 0);
  if (total === 0) {
    return;
  }
  const start = (page - 1) * BROKEN_PAGE_SIZE + 1;
  view.info.textContent = `Showing ${start}-${Math.min(start + BROKEN_PAGE_SIZE - 1, total)} of ${total}`;
  const totalPages = pageCount(total, BROKEN_PAGE_SIZE);
  view.controls.replaceChildren(
    ...(totalPages <= 1 ? [] : pageButtons(page, totalPages)).map((button) =>
      pageButton(button.label, button.target, [
        ...(button.active ? ["btn-active"] : []),
        ...(button.disabled ? ["btn-disabled"] : []),
      ]),
    ),
  );
};

const renderBrokenPage = (view: BrokenView, entries: readonly EntryHealth[], page: number): void => {
  view.empty.classList.toggle("hidden", entries.length > 0);
  const start = (page - 1) * BROKEN_PAGE_SIZE;
  view.body.replaceChildren(...entries.slice(start, start + BROKEN_PAGE_SIZE).map((entry) => brokenRow(entry)));
  renderPagination(view, entries.length, page);
};

const toggleBrokenRow = (row: HTMLElement): void => {
  const detail = row.nextElementSibling;
  if (!(detail instanceof HTMLElement)) {
    return;
  }
  const hidden = detail.classList.toggle("hidden");
  const caret = row.querySelector<HTMLElement>('[data-field="caret"]');
  if (caret !== null) {
    caret.style.transform = hidden ? "" : "rotate(90deg)";
  }
};

export { BROKEN_PAGE_SIZE, brokenView, renderBrokenPage, toggleBrokenRow };
