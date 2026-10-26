import { byId, onAction } from "#ui/lib/common/dom";
import { addArr } from "#ui/lib/config/arrs";
import {
  copyAPIToken,
  refreshAPIToken,
  showPasswordMatch,
  toggleTokenOnly,
  updateAuthSettings,
} from "#ui/lib/config/auth";
import { addDebrid, updateArrDebridDropdowns } from "#ui/lib/config/debrids";
import { addQueueCleanupRow } from "#ui/lib/config/queue-cleanup";
import { addUsenetProvider } from "#ui/lib/config/usenet";
import { previewFolder } from "#ui/lib/config/vf-preview";
import {
  addCondition,
  addVirtualFolder,
  applySample,
  handleFolderInput,
  removeCondition,
  removeFolder,
} from "#ui/lib/config/virtual-folders";

type Confirm = (message: string) => Promise<boolean>;

const onClick = (doc: Document, id: string, action: () => unknown): void => {
  byId(id, HTMLButtonElement, doc).addEventListener("click", () => {
    void action();
  });
};

const bindCards = (doc: Document): void => {
  const debrids = byId("debridConfigs", HTMLElement, doc);
  onAction(debrids, "click", {
    "remove-debrid": (target) => {
      target.closest(".debrid-config")?.remove();
      updateArrDebridDropdowns(doc);
    },
  });
  onAction(debrids, "focusout", {
    "debrid-name": () => {
      updateArrDebridDropdowns(doc);
    },
  });
  onAction(byId("arrConfigs", HTMLElement, doc), "click", {
    "remove-arr": (target) => target.closest(".arr-config")?.remove(),
  });
  onAction(byId("usenetProviders", HTMLElement, doc), "click", {
    "remove-provider": (target) => target.closest(".usenet-provider")?.remove(),
  });
  onAction(byId("queueCleanupCustom", HTMLElement, doc), "click", {
    "remove-rule": (target) => target.closest(".queue-cleanup-custom-row")?.remove(),
  });
};

const folderOf = (target: HTMLElement): HTMLElement | null => target.closest<HTMLElement>("[data-virtual-folder]");

const folderId = (target: HTMLElement): number => Number(folderOf(target)?.dataset["virtualFolder"]);

const withFolder = (target: HTMLElement, action: (card: HTMLElement) => unknown): void => {
  const card = folderOf(target);
  if (card !== null) {
    void action(card);
  }
};

const bindFolders = (doc: Document, confirm: Confirm): void => {
  const folders = byId("virtualFoldersContainer", HTMLElement, doc);
  for (const type of ["input", "change"]) {
    folders.addEventListener(type, (event) => {
      handleFolderInput(doc, event);
    });
  }
  onAction(folders, "click", {
    "remove-folder": (target) => {
      withFolder(target, async (card) => removeFolder(doc, card, confirm));
    },
    "add-condition": (target) => {
      addCondition(doc, folderId(target));
    },
    "apply-sample": (target) => {
      applySample(doc, folderId(target), target.dataset["sample"] ?? "");
    },
    "remove-condition": (target) => {
      const row = target.closest<HTMLElement>("[data-vf-condition]");
      removeCondition(doc, folderId(target), row?.dataset["vfCondition"] ?? "");
    },
    preview: (target) => {
      withFolder(target, async (card) => previewFolder(doc, card));
    },
  });
};

const bindAddButtons = (doc: Document): void => {
  onClick(doc, "addDebridBtn", () => {
    addDebrid(doc);
  });
  onClick(doc, "addArrBtn", () => {
    addArr(doc);
  });
  onClick(doc, "addVirtualFolderBtn", () => {
    addVirtualFolder(doc);
  });
  onClick(doc, "addUsenetProviderBtn", () => {
    addUsenetProvider(doc);
  });
  onClick(doc, "addQueueCleanupRuleBtn", () => {
    addQueueCleanupRow(doc);
  });
};

const bindAuth = (doc: Document): void => {
  onClick(doc, "copy-token-btn", async () => copyAPIToken(doc));
  onClick(doc, "refresh-token-btn", async () => refreshAPIToken(doc));
  onClick(doc, "update-auth-btn", async () => updateAuthSettings(doc));
  byId("auth-token-only", HTMLInputElement, doc).addEventListener("change", () => {
    toggleTokenOnly(doc);
  });
  for (const id of ["auth-password", "auth-password-confirm"]) {
    byId(id, HTMLInputElement, doc).addEventListener("input", () => {
      showPasswordMatch(doc);
    });
  }
};

export { bindAddButtons, bindAuth, bindCards, bindFolders, onClick };
