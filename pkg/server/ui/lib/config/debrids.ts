import { byId } from "#ui/lib/common/dom";
import { asArray, asRecord } from "#ui/lib/common/json";
import { appendCard, cardIndex, nextIndex } from "#ui/lib/config/cards";
import { assign, checkedOf, intOr, isCheckbox, named, toText } from "#ui/lib/config/form";

const REQUIRED_FIELDS = [
  "name",
  "provider",
  "api_key",
  "rate_limit",
  "repair_rate_limit",
  "download_rate_limit",
  "minimum_free_slot",
  "priority",
  "proxy",
  "download_uncached",
  "unpack_rar",
  "add_samples",
  "user_agent",
  "torrents_refresh_interval",
  "download_links_refresh_interval",
  "auto_expire_links_after",
] as const;

const debridContainer = (doc: Document): HTMLElement => byId("debridConfigs", HTMLElement, doc);

const maskKeys = (control: HTMLTextAreaElement): void => {
  control.style.setProperty("-webkit-text-security", "disc");
  control.style.setProperty("text-security", "disc");
  control.dataset["passwordVisible"] = "false";
};

const fillDebrid = (card: Element, index: number, data: Record<string, unknown>): void => {
  for (const [key, value] of Object.entries(data)) {
    const control = named(card, `debrid[${index}].${key}`);
    if (control === undefined || isCheckbox(control) || key !== "download_api_keys" || !Array.isArray(value)) {
      assign(control, value);
    } else {
      control.value = value.map((item) => toText(item)).join("\n");
      if (control instanceof HTMLTextAreaElement) {
        maskKeys(control);
      }
    }
  }
};

const debridNames = (doc: Document): string[] =>
  [...doc.querySelectorAll(".debrid-config")]
    .map((card) => named(doc, `debrid[${cardIndex(card)}].name`)?.value.trim() ?? "")
    .filter((name) => name !== "");

const debridOption = (doc: Document, name: string): HTMLOptionElement => {
  const option = doc.createElement("option");
  option.value = name;
  option.textContent = name;
  return option;
};

const fillDebridOptions = (doc: Document, select: HTMLSelectElement): void => {
  const auto = debridOption(doc, "");
  auto.textContent = "Auto Select";
  select.replaceChildren(auto, ...debridNames(doc).map((name) => debridOption(doc, name)));
};

const updateArrDebridDropdowns = (doc: Document): void => {
  for (const card of doc.querySelectorAll(".arr-config")) {
    const select = named(doc, `arr[${cardIndex(card)}].selected_debrid`);
    if (select instanceof HTMLSelectElement) {
      const current = select.value;
      fillDebridOptions(doc, select);
      if (current !== "") {
        select.value = current;
      }
    }
  }
};

const addDebrid = (doc: Document, data: Record<string, unknown> = {}): void => {
  const container = debridContainer(doc);
  const index = nextIndex(container);
  const card = appendCard(doc, container, {
    index,
    template: "config-debrid",
    title: `Debrid #${index + 1}`,
    names: { name: (field) => `debrid[${index}].${field}`, id: (field) => `debrid[${index}].${field}` },
  });
  fillDebrid(card, index, data);
  updateArrDebridDropdowns(doc);
};

const populateDebrids = (doc: Document, value: unknown): void => {
  if (Array.isArray(value)) {
    for (const debrid of asArray(value, "debrids")) {
      addDebrid(doc, asRecord(debrid, "debrid"));
    }
  }
};

const debridFrom = (card: Element): Record<string, unknown> | undefined => {
  const index = cardIndex(card);
  const field = (key: string): ReturnType<typeof named> => named(card, `debrid[${index}].${key}`);
  if (REQUIRED_FIELDS.some((key) => field(key) === undefined)) {
    return undefined;
  }
  const text = (key: string): string => field(key)?.value ?? "";
  const keys = field("download_api_keys")?.value ?? "";
  return {
    name: text("name"),
    provider: text("provider"),
    api_key: text("api_key"),
    rate_limit: text("rate_limit"),
    repair_rate_limit: text("repair_rate_limit"),
    download_rate_limit: text("download_rate_limit"),
    minimum_free_slot: intOr(text("minimum_free_slot"), 0),
    priority: intOr(text("priority"), 0),
    proxy: text("proxy"),
    download_uncached: checkedOf(field("download_uncached")),
    unpack_rar: checkedOf(field("unpack_rar")),
    add_samples: checkedOf(field("add_samples")),
    user_agent: text("user_agent"),
    limit: intOr(field("limit")?.value, 0),
    download_api_keys:
      keys.trim() === ""
        ? undefined
        : keys
            .split("\n")
            .map((key) => key.trim())
            .filter((key) => key !== ""),
    torrents_refresh_interval: text("torrents_refresh_interval"),
    download_links_refresh_interval: text("download_links_refresh_interval"),
    auto_expire_links_after: text("auto_expire_links_after"),
  };
};

const collectDebrids = (doc: Document): Record<string, unknown>[] =>
  [...debridContainer(doc).querySelectorAll(".debrid-config")]
    .map((card) => debridFrom(card))
    .filter(
      (debrid): debrid is Record<string, unknown> =>
        debrid !== undefined && debrid["name"] !== "" && debrid["api_key"] !== "" && debrid["provider"] !== "",
    );

export { addDebrid, collectDebrids, fillDebridOptions, populateDebrids, updateArrDebridDropdowns };
