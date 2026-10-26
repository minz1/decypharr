import { bootPage, byId, onAction } from "#ui/lib/common/dom";
import {
  clearHistory,
  clearState,
  fixBroken,
  fixEntry,
  openBrokenModal,
  openClearStateModal,
  openRunModal,
  recheckEntry,
  recheckMedia,
  runNow,
  stopRun,
} from "#ui/lib/repair/actions";
import { toggleBrokenRow } from "#ui/lib/repair/broken";
import { goToBrokenPage, loadAll, loadHistory, repairPage } from "#ui/lib/repair/loads";
import type { RepairPage } from "#ui/lib/repair/types";

const onClick = (button: HTMLButtonElement, action: () => unknown): void => {
  button.addEventListener("click", () => {
    void action();
  });
};

const onSubmit = (form: HTMLFormElement, submit: () => Promise<void>): void => {
  form.addEventListener("submit", (event) => {
    event.preventDefault();
    void submit();
  });
};

const bindStatus = (page: RepairPage): void => {
  onClick(page.status.runNow, () => {
    openRunModal(page);
  });
  onClick(page.status.stop, async () => stopRun(page));
  onClick(page.status.fixBroken, async () => fixBroken(page));
  onClick(page.status.clearState, () => {
    openClearStateModal(page);
  });
  onClick(page.status.viewBroken, () => {
    openBrokenModal(page);
  });
  onClick(byId("refreshHistoryBtn", HTMLButtonElement), async () => loadHistory(page));
  onClick(byId("clearHistoryBtn", HTMLButtonElement), async () => clearHistory(page));
};

const bindForms = (page: RepairPage): void => {
  onClick(byId("runRepairCancelBtn", HTMLButtonElement), () => {
    page.run.modal.close();
  });
  onClick(byId("clearStateCancelBtn", HTMLButtonElement), () => {
    page.clear.modal.close();
  });
  onSubmit(byId("runRepairForm", HTMLFormElement), async () => runNow(page));
  onSubmit(byId("clearStateForm", HTMLFormElement), async () => clearState(page));
  onSubmit(byId("recheckMediaForm", HTMLFormElement), async () => recheckMedia(page));
};

const bindBroken = (page: RepairPage): void => {
  onAction(page.broken.body, "click", {
    "toggle-broken": (row) => {
      toggleBrokenRow(row);
    },
    "recheck-entry": (button) => {
      void recheckEntry(page, button.dataset["name"] ?? "");
    },
    "fix-entry": (button) => {
      void fixEntry(button.dataset["name"] ?? "");
    },
  });
  onAction(page.broken.controls, "click", {
    "broken-page": (button) => {
      goToBrokenPage(page, Number(button.dataset["page"] ?? "0"));
    },
  });
};

bootPage("repair", async () => {
  const page = repairPage(document);
  byId("repairConfirmModal", HTMLDialogElement);
  bindStatus(page);
  bindForms(page);
  bindBroken(page);
  await loadAll(page);
});
