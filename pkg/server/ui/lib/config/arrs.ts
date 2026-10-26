import { byId } from "#ui/lib/common/dom";
import { JSON_NULL, asArray, asRecord, optStr } from "#ui/lib/common/json";
import { appendCard, cardIndex, nextIndex } from "#ui/lib/config/cards";
import { fillDebridOptions } from "#ui/lib/config/debrids";
import { assign, checkedOf, named } from "#ui/lib/config/form";

const REQUIRED_FIELDS = [
  "name",
  "host",
  "token",
  "skip_repair",
  "download_uncached",
  "selected_debrid",
  "source",
] as const;
const LOCKED_FIELDS = ["name", "host", "token"] as const;

const arrContainer = (doc: Document): HTMLElement => byId("arrConfigs", HTMLElement, doc);

const lockField = (input: Element | null): void => {
  input?.classList.add("input-disabled");
  input?.removeAttribute("required");
  input?.setAttribute("readonly", "");
};

const markAutoDetected = (card: HTMLElement): void => {
  card.classList.add("border-info");
  card.querySelector("[data-action='remove-arr']")?.remove();
  for (const key of LOCKED_FIELDS) {
    lockField(card.querySelector(`[data-field="${key}"]`));
  }
  const toggle = card.querySelector<HTMLButtonElement>(".password-toggle-btn");
  if (toggle !== null) {
    toggle.classList.add("opacity-50", "cursor-not-allowed");
    toggle.disabled = true;
  }
};

const fillArr = (card: Element, index: number, data: Record<string, unknown>): void => {
  for (const [key, value] of Object.entries(data)) {
    const control = named(card, `arr[${index}].${key}`);
    if (key === "download_uncached" && control instanceof HTMLSelectElement) {
      control.value = typeof value === "boolean" ? String(value) : "";
    } else {
      assign(control, value);
    }
  }
};

const prepareArr = (doc: Document, card: HTMLElement, source: string): void => {
  const debrids = card.querySelector("select[data-field='selected_debrid']");
  if (debrids instanceof HTMLSelectElement) {
    fillDebridOptions(doc, debrids);
  }
  if (source === "auto") {
    markAutoDetected(card);
  } else {
    card.querySelector("[data-auto-badge]")?.remove();
  }
  assign(card.querySelector<HTMLInputElement>("[data-field='source']") ?? undefined, source);
};

const addArr = (doc: Document, data: Record<string, unknown> = {}): void => {
  const container = arrContainer(doc);
  const index = nextIndex(container);
  const card = appendCard(doc, container, {
    index,
    template: "config-arr",
    title: `Arr Service #${index + 1}`,
    names: { name: (field) => `arr[${index}].${field}`, id: (field) => `arr[${index}].${field}` },
  });
  prepareArr(doc, card, optStr(data, "source") ?? "");
  fillArr(card, index, data);
};

const populateArrs = (doc: Document, value: unknown): void => {
  if (Array.isArray(value)) {
    for (const arr of asArray(value, "arrs")) {
      addArr(doc, asRecord(arr, "arr"));
    }
  }
};

const arrFrom = (card: Element): Record<string, unknown> | undefined => {
  const index = cardIndex(card);
  const field = (key: string): ReturnType<typeof named> => named(card, `arr[${index}].${key}`);
  if (REQUIRED_FIELDS.some((key) => field(key) === undefined)) {
    return undefined;
  }
  const uncached = field("download_uncached")?.value ?? "";
  return {
    name: field("name")?.value,
    host: field("host")?.value,
    token: field("token")?.value,
    skip_repair: checkedOf(field("skip_repair")),
    selected_debrid: field("selected_debrid")?.value,
    source: field("source")?.value,
    import_webhook: checkedOf(field("import_webhook")) ?? false,
    download_uncached: uncached === "" ? JSON_NULL : uncached === "true",
  };
};

const collectArrs = (doc: Document): Record<string, unknown>[] =>
  [...arrContainer(doc).querySelectorAll(".arr-config")]
    .map((card) => arrFrom(card))
    .filter((arr): arr is Record<string, unknown> => arr !== undefined && arr["name"] !== "" && arr["host"] !== "");

export { addArr, collectArrs, populateArrs };
