import { byId } from "#ui/lib/common/dom";

const confirmAction = async (message: string): Promise<boolean> => {
  const dialog = byId("repairConfirmModal", HTMLDialogElement);
  byId("repairConfirmMessage", HTMLElement).textContent = message;
  dialog.returnValue = "";
  const { promise, resolve } = Promise.withResolvers<boolean>();
  dialog.addEventListener(
    "close",
    () => {
      resolve(dialog.returnValue === "ok");
    },
    { once: true },
  );
  dialog.showModal();
  return promise;
};

export { confirmAction };
