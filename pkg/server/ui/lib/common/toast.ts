type ToastType = "success" | "error" | "warning" | "info";

const DURATIONS_MS: Readonly<Record<ToastType, number>> = { success: 5000, warning: 10_000, error: 15_000, info: 7000 };
const ALERT_CLASSES: Readonly<Record<ToastType, string>> = {
  success: "alert-success",
  warning: "alert-warning",
  error: "alert-error",
  info: "alert-info",
};
const CLOSE_ANIMATION_MS = 300;
const CONTAINER_CLASSES = [
  "toast-container",
  "fixed",
  "inset-x-3",
  "bottom-3",
  "z-50",
  "space-y-2",
  "sm:left-auto",
  "sm:right-4",
  "sm:w-auto",
];

let lastId = 0;

const toastContainer = (): Element => {
  const existing = document.querySelector(".toast-container");
  if (existing !== null) {
    return existing;
  }
  const created = document.createElement("div");
  created.classList.add(...CONTAINER_CLASSES);
  document.body.append(created);
  return created;
};

const writeLines = (target: Element, message: string): void => {
  const [first = "", ...rest] = message.split("\n");
  target.append(first);
  for (const line of rest) {
    target.append(document.createElement("br"), line);
  }
};

const closeToast = (id: string): void => {
  const toast = document.querySelector(`#${CSS.escape(id)}`);
  if (toast === null) {
    return;
  }
  toast.classList.add("toast-closing");
  setTimeout(() => {
    toast.remove();
  }, CLOSE_ANIMATION_MS);
};

const plainToast = (): Element => {
  const toast = document.createElement("div");
  toast.classList.add("alert", "mb-2", "max-w-full", "shadow-lg");
  return toast;
};

const templateToast = (): Element => {
  const template = document.querySelector("#toast-template");
  if (!(template instanceof HTMLTemplateElement)) {
    return plainToast();
  }
  return document.importNode(template.content, true).firstElementChild ?? plainToast();
};

const buildToast = (id: string, type: ToastType, message: string): Element => {
  const toast = templateToast();
  toast.id = id;
  toast.classList.add(ALERT_CLASSES[type]);
  for (const icon of toast.querySelectorAll(`[data-type]:not([data-type="${type}"])`)) {
    icon.remove();
  }
  writeLines(toast.querySelector("[data-toast-message]") ?? toast, message);
  toast.querySelector('[data-action="close-toast"]')?.addEventListener("click", () => {
    closeToast(id);
  });
  return toast;
};

const createToast = (message: string, type: ToastType = "success", durationMs = DURATIONS_MS[type]): string => {
  lastId += 1;
  const id = `toast-${Date.now()}-${lastId}`;
  toastContainer().append(buildToast(id, type, message));
  setTimeout(() => {
    closeToast(id);
  }, durationMs);
  return id;
};

const closeAllToasts = (): void => {
  for (const toast of document.querySelectorAll(".toast-container .alert")) {
    if (toast.id !== "") {
      closeToast(toast.id);
    }
  }
};

export type { ToastType };
export { closeAllToasts, closeToast, createToast };
