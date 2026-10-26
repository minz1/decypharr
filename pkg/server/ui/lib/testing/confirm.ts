import { expect, vi } from "vitest";

import { element, settle } from "#ui/lib/testing/dom";

const confirmMessage = async (): Promise<string> => {
  const dialog = element("#confirmDialog", HTMLDialogElement);
  await vi.waitFor(() => {
    expect(dialog.open).toBe(true);
  });
  return element("#confirmDialogMessage", HTMLElement).textContent;
};

const answerConfirm = async (answer: "cancel" | "ok"): Promise<string> => {
  const message = await confirmMessage();
  element(`#confirmDialog [data-answer="${answer}"]`, HTMLButtonElement).click();
  await settle();
  return message;
};

export { answerConfirm, confirmMessage };
