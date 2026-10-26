import { asRecord, optBool, optNum, optStr } from "#ui/lib/common/json";
import {
  checkedOf,
  floatOr,
  intOr,
  lines,
  numberText,
  orDefault,
  setChecked,
  setText,
  textList,
  withId,
} from "#ui/lib/config/form";

const NUMBER_FIELDS = [
  "min_support",
  "min_evidence",
  "min_sources",
  "port",
  "gossip_port",
  "max_storage_bytes",
  "max_feeds_per_namespace",
  "max_seeded_torrents",
] as const;

const field = (doc: Document, key: string): ReturnType<typeof withId> => withId(doc, `hearsay.${key}`);

const populateNumbers = (doc: Document, hearsay: Record<string, unknown>): void => {
  for (const key of NUMBER_FIELDS) {
    setText(field(doc, key), numberText(optNum(hearsay, key)));
  }
};

const populateHearsay = (doc: Document, value: unknown): void => {
  const hearsay = value === undefined || value === null ? {} : asRecord(value, "hearsay");
  setChecked(field(doc, "enabled"), optBool(hearsay, "disabled") !== true);
  setChecked(field(doc, "participate"), optBool(hearsay, "participate") ?? true);
  setChecked(field(doc, "publish"), optBool(hearsay, "publish") ?? true);
  setText(field(doc, "advice_mode"), orDefault(optStr(hearsay, "advice_mode"), "shadow"));
  populateNumbers(doc, hearsay);
  setText(field(doc, "interval"), optStr(hearsay, "interval") ?? "");
  setText(field(doc, "follow"), textList(hearsay["follow"], "hearsay.follow").join("\n"));
  setChecked(field(doc, "block_private_peers"), optBool(hearsay, "block_private_peers") === true);
  setText(field(doc, "blocklist"), textList(hearsay["blocklist"], "hearsay.blocklist").join("\n"));
};

const int = (doc: Document, key: string): number => intOr(field(doc, key)?.value, 0);

const collectHearsay = (doc: Document): Record<string, unknown> => ({
  disabled: !(checkedOf(field(doc, "enabled")) ?? true),
  participate: checkedOf(field(doc, "participate")) ?? true,
  publish: checkedOf(field(doc, "publish")) ?? true,
  advice_mode: orDefault(field(doc, "advice_mode")?.value, "shadow"),
  min_support: floatOr(field(doc, "min_support")?.value, 0),
  min_evidence: floatOr(field(doc, "min_evidence")?.value, 0),
  min_sources: int(doc, "min_sources"),
  port: int(doc, "port"),
  gossip_port: int(doc, "gossip_port"),
  interval: field(doc, "interval")?.value.trim() ?? "",
  max_storage_bytes: int(doc, "max_storage_bytes"),
  max_feeds_per_namespace: int(doc, "max_feeds_per_namespace"),
  max_seeded_torrents: int(doc, "max_seeded_torrents"),
  follow: lines(field(doc, "follow")?.value),
  block_private_peers: checkedOf(field(doc, "block_private_peers")) ?? false,
  blocklist: lines(field(doc, "blocklist")?.value),
});

export { collectHearsay, populateHearsay };
