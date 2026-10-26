import { byId } from "#ui/lib/common/dom";

const listenForAnswer = (dialog: HTMLDialogElement, resolve: (answer: boolean) => void): void => {
  const listening = new AbortController();
  dialog.addEventListener(
    "click",
    (event) => {
      const button = event.target instanceof Element ? event.target.closest<HTMLElement>("[data-answer]") : undefined;
      if (button) {
        dialog.close(button.dataset["answer"]);
      }
    },
    { signal: listening.signal },
  );
  dialog.addEventListener(
    "close",
    () => {
      listening.abort();
      resolve(dialog.returnValue === "ok");
    },
    { signal: listening.signal },
  );
};

const askConfirm = async (message: string): Promise<boolean> => {
  const dialog = byId("confirmDialog", HTMLDialogElement);
  if (dialog.open) {
    return false;
  }
  byId("confirmDialogMessage", HTMLElement).textContent = message;
  const { promise, resolve } = Promise.withResolvers<boolean>();
  listenForAnswer(dialog, resolve);
  dialog.returnValue = "";
  dialog.showModal();
  dialog.querySelector<HTMLElement>('[data-answer="cancel"]')?.focus();
  return promise;
};

export { askConfirm };
