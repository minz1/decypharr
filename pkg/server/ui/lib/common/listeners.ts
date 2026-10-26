import { createToast } from "#ui/lib/common/toast";

const INVALID_MS = 3000;

const reasonOf = (reason: unknown): string =>
  reason instanceof Error && reason.message !== "" ? reason.message : "Unknown error";

const closeOpenMenus = (doc: Document): void => {
  for (const modal of doc.querySelectorAll(".modal[open]")) {
    if (modal instanceof HTMLDialogElement) {
      modal.close();
    }
  }
  for (const dropdown of doc.querySelectorAll(".dropdown-open")) {
    dropdown.classList.remove("dropdown-open");
  }
  for (const menu of doc.querySelectorAll(".context-menu:not(.hidden)")) {
    menu.classList.add("hidden");
  }
};

const scrollToAnchor = (doc: Document, event: Event): void => {
  if (!(event.target instanceof Element)) {
    return;
  }
  const href = event.target.closest('a[href^="#"]')?.getAttribute("href") ?? "#";
  if (href === "#") {
    return;
  }
  event.preventDefault();
  doc.querySelector(href)?.scrollIntoView({ behavior: "smooth", block: "start" });
};

const setupGlobalListeners = (doc: Document): void => {
  const view = doc.defaultView;
  view?.addEventListener("error", (event) => {
    createToast(`Unexpected error: ${reasonOf(event.error)}`, "error");
  });
  view?.addEventListener("unhandledrejection", (event) => {
    createToast(`Promise rejected: ${reasonOf(event.reason)}`, "error");
  });
  view?.addEventListener("online", () => {
    createToast("Connection restored", "success");
  });
  view?.addEventListener("offline", () => {
    createToast("Connection lost - working offline", "warning");
  });
  doc.addEventListener(
    "invalid",
    (event) => {
      const field = event.target;
      if (field instanceof Element) {
        field.classList.add("input-error");
        view?.setTimeout(() => {
          field.classList.remove("input-error");
        }, INVALID_MS);
      }
    },
    true,
  );
  doc.addEventListener("click", (event) => {
    scrollToAnchor(doc, event);
  });
  doc.addEventListener("keydown", (event) => {
    if (event.key === "Escape") {
      closeOpenMenus(doc);
    }
  });
};

export { setupGlobalListeners };
