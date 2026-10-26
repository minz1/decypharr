const showShortcuts = (doc: Document): void => {
  const template = doc.querySelector("#keyboard-shortcuts-template");
  if (!(template instanceof HTMLTemplateElement)) {
    return;
  }
  const dialog = doc.importNode(template.content, true).firstElementChild;
  if (dialog instanceof HTMLDialogElement) {
    doc.body.append(dialog);
    dialog.addEventListener("close", () => {
      dialog.remove();
    });
    dialog.showModal();
  }
};

const setupKeyboardShortcuts = (doc: Document): void => {
  doc.addEventListener("keydown", (event) => {
    if ((event.ctrlKey || event.metaKey) && event.key === "/") {
      event.preventDefault();
      showShortcuts(doc);
    }
  });
};

export { setupKeyboardShortcuts };
