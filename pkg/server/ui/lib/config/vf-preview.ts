import { cloneTemplate } from "#ui/lib/common/dom";
import { describeError } from "#ui/lib/common/errors";
import { formatBytes } from "#ui/lib/common/format";
import { fetcher } from "#ui/lib/common/http";
import { asArray, asRecord, optNum, optStr } from "#ui/lib/common/json";
import { orDefault } from "#ui/lib/config/form";
import { validateFolder } from "#ui/lib/config/vf-model";
import { collectFolder } from "#ui/lib/config/virtual-folders";

interface PreviewItem {
  name: string;
  source: string;
  size: number;
}

const PREVIEW_LIMIT = 5;

const setText = (node: Element | null, text: string): void => {
  if (node !== null) {
    node.textContent = text;
  }
};

const textItem = (doc: Document, text: string): DocumentFragment => {
  const item = cloneTemplate("config-vf-error", doc);
  setText(item.querySelector("li"), text);
  return item;
};

const showErrors = (doc: Document, result: Element, errors: readonly string[]): void => {
  const view = cloneTemplate("config-vf-preview-errors", doc);
  view.querySelector("ul")?.append(...errors.map((error) => textItem(doc, error)));
  result.replaceChildren(view);
};

const readItem = (value: unknown): PreviewItem => {
  const record = asRecord(value, "preview sample");
  return {
    name: optStr(record, "name") ?? "",
    source: orDefault(optStr(record, "provider"), optStr(record, "protocol") ?? ""),
    size: optNum(record, "size") ?? 0,
  };
};

const sampleItem = (doc: Document, item: PreviewItem): DocumentFragment => {
  const node = cloneTemplate("config-vf-preview-item", doc);
  setText(node.querySelector("[data-name]"), item.name);
  setText(node.querySelector("[data-meta]"), `${item.source}${item.size === 0 ? "" : ` · ${formatBytes(item.size)}`}`);
  return node;
};

const fillSamples = (doc: Document, view: DocumentFragment, samples: readonly PreviewItem[]): void => {
  if (samples.length === 0) {
    view.querySelector("[data-examples]")?.remove();
    view.querySelector("[data-samples]")?.remove();
  } else {
    view.querySelector("[data-none]")?.remove();
    view.querySelector("[data-samples]")?.append(...samples.map((item) => sampleItem(doc, item)));
  }
};

const showMatches = (doc: Document, result: Element, value: unknown): void => {
  const preview = asRecord(value, "preview");
  const total = optNum(preview, "total");
  const samples = Array.isArray(preview["samples"])
    ? asArray(preview["samples"], "samples").map((item) => readItem(item))
    : [];
  const view = cloneTemplate("config-vf-preview-matches", doc);
  setText(view.querySelector("[data-total]"), `${String(total)} matching item${total === 1 ? "" : "s"}`);
  fillSamples(doc, view, samples);
  result.replaceChildren(view);
};

const showFailure = (doc: Document, result: Element, error: unknown): void => {
  const view = cloneTemplate("config-vf-preview-failed", doc);
  setText(view.querySelector("p"), `Could not preview: ${describeError(error)}`);
  result.replaceChildren(view);
};

const fetchPreview = async (folder: unknown): Promise<unknown> => {
  const response = await fetcher("/api/virtual-folders/preview", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ folder, limit: PREVIEW_LIMIT }),
  });
  if (!response.ok) {
    const reason = await response.text();
    throw new Error(orDefault(reason.trim(), "Preview failed"));
  }
  const preview: unknown = await response.json();
  return preview;
};

const setBusy = (button: HTMLButtonElement | null, busy: boolean): void => {
  if (button === null) {
    return;
  }
  button.disabled = busy;
  if (busy) {
    button.setAttribute("aria-busy", "true");
  } else {
    button.removeAttribute("aria-busy");
  }
};

const runPreview = async (doc: Document, card: Element, result: Element): Promise<void> => {
  const button = card.querySelector<HTMLButtonElement>("[data-vf-preview-button]");
  setBusy(button, true);
  result.textContent = "Checking your library…";
  try {
    showMatches(doc, result, await fetchPreview(collectFolder(card)));
  } catch (error) {
    showFailure(doc, result, error);
  } finally {
    setBusy(button, false);
  }
};

const previewFolder = async (doc: Document, card: Element): Promise<void> => {
  const result = card.querySelector("[data-vf-preview-result]");
  if (result === null) {
    return;
  }
  const errors = validateFolder(collectFolder(card));
  result.classList.remove("hidden");
  if (errors.length > 0) {
    showErrors(doc, result, errors);
    return;
  }
  await runPreview(doc, card, result);
};

export { previewFolder, textItem };
