import { describe, expect, it } from "vitest";

import { setupKeyboardShortcuts } from "#ui/lib/common/shortcuts";

const listening = new Set<Document>();

const HELP = `<template id="keyboard-shortcuts-template">
  <dialog class="modal"><div class="modal-box"><span class="kbd">Esc</span></div></dialog>
</template>`;

const page = (body: string): void => {
  if (!listening.has(document)) {
    setupKeyboardShortcuts(document);
    listening.add(document);
  }
  document.body.innerHTML = body;
};

const press = (modifier: "ctrlKey" | "metaKey" | "none"): KeyboardEvent => {
  const event = new KeyboardEvent("keydown", {
    key: "/",
    ctrlKey: modifier === "ctrlKey",
    metaKey: modifier === "metaKey",
    bubbles: true,
    cancelable: true,
  });
  document.dispatchEvent(event);
  return event;
};

describe("the ctrl + / shortcut", () => {
  it("opens the shortcut help", () => {
    page(HELP);
    const event = press("ctrlKey");
    const dialog = document.querySelector("body > dialog.modal");
    expect({
      prevented: event.defaultPrevented,
      open: dialog?.hasAttribute("open"),
      keys: dialog?.textContent,
    }).toStrictEqual({ prevented: true, open: true, keys: "Esc" });
  });

  it("works with Cmd too, and the help goes away when closed", () => {
    page(HELP);
    press("metaKey");
    const opened = document.querySelectorAll("body > dialog[open]").length;
    document.querySelector<HTMLDialogElement>("body > dialog")?.close();
    expect({ opened, left: document.querySelectorAll("body > dialog").length }).toStrictEqual({ opened: 1, left: 0 });
  });

  it("does nothing without the help template", () => {
    page("");
    press("ctrlKey");
    expect(document.querySelectorAll("dialog")).toHaveLength(0);
  });
});

describe("a plain /", () => {
  it("is left alone", () => {
    page(HELP);
    const event = press("none");
    expect({
      prevented: event.defaultPrevented,
      dialogs: document.querySelectorAll("body > dialog").length,
    }).toStrictEqual({ prevented: false, dialogs: 0 });
  });
});
