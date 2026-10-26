import { describe, expect, it } from "vitest";

import { collectPrune, confirmPruneDelete, populatePrune, prunePayload, savedPrune } from "#ui/lib/config/prune";
import { loadFixture } from "#ui/lib/testing/fixture";

type PruneSettings = ReturnType<typeof collectPrune>;

const absent: unknown = new Map<string, unknown>().get("prune");

const deletePrune: PruneSettings = {
  enabled: true,
  mode: "delete",
  schedule: "1h",
  grace: "72h",
  max_per_run: 5,
  roots: ["/data/a", "/data/b"],
};

const freshPage = (): Document => {
  loadFixture("config");
  const doc = document.implementation.createHTMLDocument("settings");
  doc.documentElement.remove();
  doc.append(doc.importNode(document.documentElement, true));
  return doc;
};

const setValue = (doc: Document, id: string, value: string): void => {
  const field = doc.querySelector<HTMLInputElement>(`[id="${id}"]`);
  if (field === null) {
    throw new Error(`missing #${id}`);
  }
  field.value = value;
};

const asks = (baseline: PruneSettings, sent?: PruneSettings): boolean => {
  const doc = freshPage();
  populatePrune(doc, baseline);
  const confirms: string[] = [];
  confirmPruneDelete(sent, (message) => {
    confirms.push(message);
    return true;
  });
  return confirms.length === 1;
};

describe("prune settings", () => {
  it("populate then collect round-trips", () => {
    const doc = freshPage();
    populatePrune(doc, deletePrune);
    expect({ collected: collectPrune(doc), payload: prunePayload(doc) }).toStrictEqual({
      collected: deletePrune,
      payload: undefined,
    });
  });

  it.each([
    ["absent", absent],
    ["report with a limit", { mode: "report", max_per_run: 100 }],
    ["an unknown mode", { mode: "nuke" }],
  ])("defaults and unknown modes collect as report: %s", (_name, prune) => {
    const doc = freshPage();
    populatePrune(doc, prune);
    expect(collectPrune(doc).mode).toBe("report");
  });

  it("collectFormData sends prune only when changed", () => {
    const doc = freshPage();
    populatePrune(doc, deletePrune);
    const unchanged = prunePayload(doc);
    setValue(doc, "prune.grace", "24h");
    expect({ unchanged, changed: prunePayload(doc), unloaded: prunePayload(freshPage()) }).toStrictEqual({
      unchanged: undefined,
      changed: { ...deletePrune, grace: "24h" },
      unloaded: undefined,
    });
  });

  it("a saved prune becomes the baseline", () => {
    const doc = freshPage();
    populatePrune(doc, { ...deletePrune, mode: "report" });
    setValue(doc, "prune.mode", "delete");
    savedPrune(doc, prunePayload(doc));
    expect(prunePayload(doc)).toBeUndefined();
  });
});

describe("confirming prune in delete mode", () => {
  it("confirmPruneDelete asks for every sent enabled delete prune", () => {
    const reportPrune = { ...deletePrune, mode: "report" };
    expect({
      notSent: asks(deletePrune),
      switchToDelete: asks(reportPrune, deletePrune),
      enableWithDeleteStored: asks({ ...deletePrune, enabled: false }, deletePrune),
      shorterGrace: asks(deletePrune, { ...deletePrune, grace: "24h" }),
      higherMaxPerRun: asks(deletePrune, { ...deletePrune, max_per_run: 50 }),
      rootRemoved: asks(deletePrune, { ...deletePrune, roots: ["/data/a"] }),
      disabledDelete: asks(deletePrune, { ...deletePrune, enabled: false }),
      enabledReport: asks(deletePrune, reportPrune),
    }).toStrictEqual({
      notSent: false,
      switchToDelete: true,
      enableWithDeleteStored: true,
      shorterGrace: true,
      higherMaxPerRun: true,
      rootRemoved: true,
      disabledDelete: false,
      enabledReport: false,
    });
  });

  it("the delete confirm names the mode, max_per_run and grace", () => {
    const messages: string[] = [];
    const answer = confirmPruneDelete(deletePrune, (message) => {
      messages.push(message);
      return false;
    });
    expect({ answer, messages }).toStrictEqual({
      answer: false,
      messages: [
        "Save prune in delete mode?\n\nFrom the next scheduled run on, each run deletes up to 5 entries that no " +
          "library symlink has pointed to for 72h.",
      ],
    });
  });

  it("the delete confirm falls back to the defaults it names", () => {
    const messages: string[] = [];
    confirmPruneDelete({ ...deletePrune, max_per_run: 0, grace: "" }, (message) => messages.push(message) > 0);
    expect(messages).toStrictEqual([
      "Save prune in delete mode?\n\nFrom the next scheduled run on, each run deletes up to the default number of " +
        "entries that no library symlink has pointed to for the default grace period.",
    ]);
  });
});
