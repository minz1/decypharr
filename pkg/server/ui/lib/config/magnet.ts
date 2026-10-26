import { byId } from "#ui/lib/common/dom";
import { createToast } from "#ui/lib/common/toast";
import { readURLBase } from "#ui/lib/common/url";

const REGISTERED = "magnetHandler";

const markRegistered = (button: HTMLButtonElement): void => {
  const icon = button.ownerDocument.createElement("i");
  icon.className = "bi bi-check-circle mr-2";
  button.replaceChildren(icon, "Magnet Handler Registered");
  button.classList.remove("btn-primary");
  button.classList.add("btn-success");
  button.disabled = true;
};

const registerMagnetHandler = (doc: Document): void => {
  if (!("registerProtocolHandler" in navigator)) {
    createToast("Magnet link registration not supported in this browser", "warning");
    return;
  }
  try {
    navigator.registerProtocolHandler("magnet", `${globalThis.location.origin}${readURLBase(doc)}download?magnet=%s`);
    localStorage.setItem(REGISTERED, "true");
    markRegistered(byId("registerMagnetLink", HTMLButtonElement, doc));
    createToast("Magnet link handler registered successfully");
  } catch {
    createToast("Failed to register magnet link handler", "error");
  }
};

const showMagnetHandler = (doc: Document): void => {
  const button = doc.querySelector("#registerMagnetLink");
  if (localStorage.getItem(REGISTERED) === "true" && button instanceof HTMLButtonElement) {
    markRegistered(button);
  }
};

export { registerMagnetHandler, showMagnetHandler };
