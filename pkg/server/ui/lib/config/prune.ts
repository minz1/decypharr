import { asRecord, optBool, optNum, optStr } from "#ui/lib/common/json";
import {
  checkedOf,
  intOr,
  lines,
  numberText,
  orDefault,
  setChecked,
  setText,
  textList,
  withId,
} from "#ui/lib/config/form";

interface PruneSettings {
  enabled: boolean;
  mode: string;
  schedule: string;
  grace: string;
  max_per_run: number;
  roots: string[];
}

const baselines = new WeakMap<Document, PruneSettings>();

const field = (doc: Document, key: string): ReturnType<typeof withId> => withId(doc, `prune.${key}`);

const collectPrune = (doc: Document): PruneSettings => ({
  enabled: checkedOf(field(doc, "enabled")) ?? false,
  mode: orDefault(field(doc, "mode")?.value, "report"),
  schedule: field(doc, "schedule")?.value.trim() ?? "",
  grace: field(doc, "grace")?.value.trim() ?? "",
  max_per_run: intOr(field(doc, "max_per_run")?.value, 0),
  roots: lines(field(doc, "roots")?.value),
});

const populatePrune = (doc: Document, value: unknown): void => {
  const prune = value === undefined || value === null ? {} : asRecord(value, "prune");
  setChecked(field(doc, "enabled"), optBool(prune, "enabled") === true);
  setText(field(doc, "mode"), optStr(prune, "mode") === "delete" ? "delete" : "report");
  setText(field(doc, "schedule"), optStr(prune, "schedule") ?? "");
  setText(field(doc, "grace"), optStr(prune, "grace") ?? "");
  setText(field(doc, "max_per_run"), numberText(optNum(prune, "max_per_run")));
  setText(field(doc, "roots"), textList(prune["roots"], "prune.roots").join("\n"));
  baselines.set(doc, collectPrune(doc));
};

const prunePayload = (doc: Document): PruneSettings | undefined => {
  const baseline = baselines.get(doc);
  if (baseline === undefined) {
    return undefined;
  }
  const prune = collectPrune(doc);
  return JSON.stringify(prune) === JSON.stringify(baseline) ? undefined : prune;
};

const savedPrune = (doc: Document, prune: PruneSettings | undefined): void => {
  if (prune !== undefined) {
    baselines.set(doc, prune);
  }
};

const confirmPruneDelete = async (
  prune: PruneSettings | undefined,
  confirm: (message: string) => Promise<boolean>,
): Promise<boolean> => {
  if (prune === undefined || !prune.enabled || prune.mode !== "delete") {
    return true;
  }
  const limit = prune.max_per_run === 0 ? "the default number of" : String(prune.max_per_run);
  const grace = prune.grace === "" ? "the default grace period" : prune.grace;
  return confirm(
    `Save prune in delete mode?\n\nFrom the next scheduled run on, each run deletes up to ${limit} entries ` +
      `that no library symlink has pointed to for ${grace}.`,
  );
};

export type { PruneSettings };
export { collectPrune, confirmPruneDelete, populatePrune, prunePayload, savedPrune };
