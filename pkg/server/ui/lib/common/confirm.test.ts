import { describe, expect, it } from "vitest";

import { askConfirm } from "#ui/lib/common/confirm";
import { loadFixture } from "#ui/lib/testing/fixture";

const answer = (value: string): void => {
  document.querySelector<HTMLButtonElement>(`#confirmDialog [data-answer="${value}"]`)?.click();
};

const dialog = (): HTMLDialogElement | null => document.querySelector("#confirmDialog");

const CONFIRMING_PAGES = ["index", "browse", "repair", "reacquire"];

describe("the confirm dialog", () => {
  it.each(CONFIRMING_PAGES)(
    "is on %s, shows the message as text with its newlines and resolves true on OK",
    async (page) => {
      loadFixture(page);
      const answered = askConfirm("Delete <b>x</b>?\nSure?");
      const message = document.querySelector("#confirmDialogMessage");
      const shown = {
        open: dialog()?.open,
        text: message?.textContent,
        lines: message?.classList.contains("whitespace-pre-line"),
      };
      answer("ok");
      expect({ shown, answer: await answered, open: dialog()?.open }).toStrictEqual({
        shown: { open: true, text: "Delete <b>x</b>?\nSure?", lines: true },
        answer: true,
        open: false,
      });
    },
  );
});

describe("answering the confirm dialog", () => {
  it("starts with focus on Cancel", async () => {
    loadFixture("index");
    const answered = askConfirm("Delete?");
    const focused = document.activeElement;
    answer("cancel");
    await answered;
    expect(focused).toBe(document.querySelector('#confirmDialog [data-answer="cancel"]'));
  });

  it("resolves false on Cancel", async () => {
    loadFixture("index");
    const answered = askConfirm("Delete?");
    answer("cancel");
    expect({ answer: await answered }).toStrictEqual({ answer: false });
  });

  it("resolves false on the backdrop", async () => {
    loadFixture("index");
    const answered = askConfirm("Delete?");
    document.querySelector<HTMLButtonElement>("#confirmDialog .modal-backdrop button")?.click();
    expect({ answer: await answered, open: dialog()?.open }).toStrictEqual({ answer: false, open: false });
  });

  it("resolves false on Escape", async () => {
    loadFixture("index");
    const answered = askConfirm("Delete?");
    dialog()?.dispatchEvent(new Event("cancel", { cancelable: true }));
    dialog()?.close();
    expect({ answer: await answered }).toStrictEqual({ answer: false });
  });
});

describe("asking again", () => {
  it("answers false to a second question while one is open", async () => {
    loadFixture("index");
    const first = askConfirm("Delete?");
    const second = askConfirm("Delete again?");
    answer("ok");
    expect([await first, await second]).toStrictEqual([true, false]);
  });

  it("answers a later question afresh", async () => {
    loadFixture("index");
    const first = askConfirm("Delete?");
    answer("ok");
    await first;
    const second = askConfirm("Delete again?");
    answer("cancel");
    expect([await first, await second]).toStrictEqual([true, false]);
  });
});
