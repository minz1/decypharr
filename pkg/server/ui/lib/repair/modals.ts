import { byId } from "#ui/lib/common/dom";
import type { ClearModalView, RecheckView, RepairConfig, RunModalView, RunOptions } from "#ui/lib/repair/types";

const runModalView = (doc: Document): RunModalView => ({
  modal: byId("runRepairModal", HTMLDialogElement, doc),
  ignoreLastChecked: byId("runIgnoreLastChecked", HTMLInputElement, doc),
  autoRepair: byId("runAutoRepair", HTMLInputElement, doc),
  verifyContent: byId("runVerifyContent", HTMLInputElement, doc),
  protocolAll: byId("runProtocolAll", HTMLInputElement, doc),
  protocols: [...doc.querySelectorAll<HTMLInputElement>('input[name="runProtocol"]')],
  submit: byId("runRepairSubmitBtn", HTMLButtonElement, doc),
});

const clearModalView = (doc: Document): ClearModalView => ({
  modal: byId("clearStateModal", HTMLDialogElement, doc),
  states: [...doc.querySelectorAll<HTMLInputElement>('input[name="repair_state"]')],
  error: byId("clearStateError", HTMLElement, doc),
  submit: byId("clearStateSubmitBtn", HTMLButtonElement, doc),
});

const recheckView = (doc: Document): RecheckView => ({
  arr: byId("recheckArr", HTMLSelectElement, doc),
  mediaId: byId("recheckMediaId", HTMLInputElement, doc),
  fix: byId("recheckFix", HTMLInputElement, doc),
  submit: byId("recheckMediaBtn", HTMLButtonElement, doc),
  result: byId("recheckMediaResult", HTMLElement, doc),
});

const resetRunModal = (view: RunModalView, config: RepairConfig): void => {
  view.ignoreLastChecked.checked = false;
  view.autoRepair.checked = config.autoRepair;
  view.verifyContent.checked = config.verifyContent;
  view.protocolAll.checked = true;
};

const readRunOptions = (view: RunModalView): RunOptions => ({
  ignoreLastChecked: view.ignoreLastChecked.checked,
  autoRepair: view.autoRepair.checked,
  verifyContent: view.verifyContent.checked,
  protocol: view.protocols.find((input) => input.checked)?.value ?? "all",
});

const showError = (target: HTMLElement, message: string): void => {
  target.textContent = message;
  target.classList.remove("hidden");
};

const resetClearModal = (view: ClearModalView): void => {
  view.error.classList.add("hidden");
  for (const state of view.states) {
    state.checked = false;
  }
};

const fillArrOptions = (select: HTMLSelectElement, names: readonly string[]): void => {
  const placeholder = select.querySelector('option[value=""]');
  const options = names.map((name) => {
    const option = document.createElement("option");
    option.value = name;
    option.textContent = name;
    return option;
  });
  select.replaceChildren(...(placeholder === null ? [] : [placeholder]), ...options);
};

const showRecheckFailure = (result: HTMLElement, message: string): void => {
  const span = document.createElement("span");
  span.classList.add("text-error");
  span.textContent = `Recheck failed: ${message}`;
  result.replaceChildren(span);
  result.classList.remove("hidden");
};

export {
  clearModalView,
  fillArrOptions,
  readRunOptions,
  recheckView,
  resetClearModal,
  resetRunModal,
  runModalView,
  showError,
  showRecheckFailure,
};
