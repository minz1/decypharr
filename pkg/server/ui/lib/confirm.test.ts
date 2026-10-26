import { describe, expect, it } from "vitest";

import { askConfirm } from "#ui/lib/confirm";
import { loadFixture } from "#ui/lib/testing/fixture";

const answer = (value: string): void => {
  document.querySelector<HTMLButtonElement>(`#confirmDialog [data-answer="${value}"]`)?.click();
};

const dialog = (): HTMLDialogElement | null => document.querySelector("#confirmDialog");

describe("the confirm dialog", () => {
  it("shows the message as text and resolves true on OK", async () => {
    loadFixture("index");
    const answered = askConfirm("Delete <b>x</b>?\nSure?");
    const shown = { open: dialog()?.open, text: document.querySelector("#confirmDialogMessage")?.textContent };
    answer("ok");
    expect({ shown, answer: await answered, open: dialog()?.open }).toStrictEqual({
      shown: { open: true, text: "Delete <b>x</b>?\nSure?" },
      answer: true,
      open: false,
    });
  });

  it("resolves false on Cancel", async () => {
    loadFixture("index");
    const answered = askConfirm("Delete?");
    answer("cancel");
    expect({ answer: await answered }).toStrictEqual({ answer: false });
  });

  it("resolves false when the dialog closes another way", async () => {
    loadFixture("index");
    const answered = askConfirm("Delete?");
    dialog()?.close();
    expect({ answer: await answered }).toStrictEqual({ answer: false });
  });

  it("answers false to a second question while one is open", async () => {
    loadFixture("index");
    const first = askConfirm("Delete?");
    const second = askConfirm("Delete again?");
    answer("ok");
    expect([await first, await second]).toStrictEqual([true, false]);
  });
});
