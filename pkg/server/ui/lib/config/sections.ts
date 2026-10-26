import { collectGeneral, populateGeneral } from "#ui/lib/config/general";
import { collectHearsay, populateHearsay } from "#ui/lib/config/hearsay";
import { mountSections } from "#ui/lib/config/mount";
import { collectNotifications, populateNotifications } from "#ui/lib/config/notifications";
import { collectPrune, populatePrune, prunePayload } from "#ui/lib/config/prune";
import { collectRepair, populateRepair } from "#ui/lib/config/repair";
import type { Section } from "#ui/lib/config/section";
import { serviceSections } from "#ui/lib/config/services";
import { shareSections } from "#ui/lib/config/shares";
import { collectStrm, populateStrm } from "#ui/lib/config/strm";

const sections: readonly Section[] = [
  { prefix: "", populate: populateGeneral, collect: collectGeneral },
  { prefix: "strm", populate: populateStrm, collect: collectStrm },
  ...serviceSections,
  ...mountSections,
  ...shareSections,
  { prefix: "notifications", populate: populateNotifications, collect: collectNotifications },
  { prefix: "repair", populate: populateRepair, collect: collectRepair },
  { prefix: "prune", populate: populatePrune, collect: collectPrune },
  { prefix: "hearsay", populate: populateHearsay, collect: collectHearsay },
];

const BODY_ORDER = [
  "",
  "virtual_folders",
  "debrids",
  "arrs",
  "queue_cleanup",
  "mount",
  "mount.dfs",
  "mount.rclone",
  "mount.external_rclone",
  "nfs",
  "smb",
  "share_cache",
  "usenet",
  "notifications",
  "repair",
  "prune",
  "strm",
  "hearsay",
] as const;

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const valueAt = (config: unknown, prefix: string): unknown => {
  let value = config;
  for (const key of prefix === "" ? [] : prefix.split(".")) {
    value = isRecord(value) ? value[key] : undefined;
  }
  return value;
};

const setAt = (body: Record<string, unknown>, prefix: string, value: unknown): void => {
  if (prefix === "") {
    Object.assign(body, value);
    return;
  }
  const keys = prefix.split(".");
  const last = keys.pop() ?? prefix;
  let parent = body;
  for (const key of keys) {
    const child = parent[key];
    parent = isRecord(child) ? child : {};
  }
  parent[last] = value;
};

const assembleSettings = (collected: ReadonlyMap<string, unknown>): Record<string, unknown> => {
  const body: Record<string, unknown> = {};
  for (const prefix of BODY_ORDER) {
    setAt(body, prefix, collected.get(prefix));
  }
  return body;
};

const populateSections = (doc: Document, config: unknown): void => {
  for (const section of sections) {
    section.populate(doc, valueAt(config, section.prefix));
  }
};

const collectSections = (doc: Document): Map<string, unknown> =>
  new Map(sections.map((section) => [section.prefix, section.collect(doc)]));

const collectSettings = (doc: Document): Record<string, unknown> =>
  assembleSettings(collectSections(doc).set("prune", prunePayload(doc)));

export { BODY_ORDER, assembleSettings, collectSections, collectSettings, populateSections, sections, valueAt };
