import { askConfirm } from "#ui/lib/common/confirm";
import { bootPage, byId } from "#ui/lib/common/dom";
import { bindAddButtons, bindAuth, bindCards, bindFolders, onClick } from "#ui/lib/config/bindings";
import { registerMagnetHandler, showMagnetHandler } from "#ui/lib/config/magnet";
import { loadPruneStatus, runPruneNow } from "#ui/lib/config/prune-status";
import { loadConfiguration, saveConfiguration, warnIncomplete } from "#ui/lib/config/settings";
import { regenerateStrm } from "#ui/lib/config/strm";
import { bindTabs } from "#ui/lib/config/tabs";

const state = { loaded: false, saving: false };

const bindSave = (): void => {
  const overlay = byId("loadingOverlay", HTMLElement);
  const form = byId("configForm", HTMLFormElement);
  const button = form.querySelector('button[type="submit"]');
  if (!(button instanceof HTMLButtonElement)) {
    throw new TypeError('missing #configForm button[type="submit"]');
  }
  form.addEventListener("submit", (event) => {
    event.preventDefault();
    void saveConfiguration(document, {
      state,
      overlay,
      button,
      confirm: askConfirm,
      reload: () => {
        globalThis.location.reload();
      },
    });
  });
};

const bindActions = (): void => {
  onClick(document, "strmRegenerateBtn", async () => regenerateStrm(byId("strmRegenerateBtn", HTMLButtonElement)));
  onClick(document, "pruneRunBtn", async () =>
    runPruneNow(document, byId("pruneRunBtn", HTMLButtonElement), askConfirm),
  );
  onClick(document, "registerMagnetLink", () => {
    registerMagnetHandler(document);
  });
};

const bindPage = (): void => {
  bindSave();
  bindCards(document);
  bindFolders(document, askConfirm);
  bindAddButtons(document);
  bindActions();
  bindAuth(document);
};

bootPage("config", () => {
  bindPage();
  void loadConfiguration(document, state);
  void loadPruneStatus(document);
  showMagnetHandler(document);
  warnIncomplete(globalThis.location.search);
  bindTabs(document);
});
