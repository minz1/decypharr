interface Menu {
  dropdown: HTMLElement;
  button: HTMLElement;
  menu: HTMLElement;
  open: boolean;
}

const setOpen = (nav: Menu, open: boolean): void => {
  if (nav.open !== open) {
    nav.dropdown.classList.toggle("dropdown-open", open);
    nav.button.setAttribute("aria-expanded", String(open));
    nav.open = open;
  }
};

const bindButton = (nav: Menu): void => {
  const toggle = (event: Event): void => {
    event.preventDefault();
    event.stopPropagation();
    setOpen(nav, !nav.open);
  };
  nav.button.addEventListener("click", toggle);
  nav.button.addEventListener("touchend", toggle);
  nav.button.addEventListener("keydown", (event) => {
    if (event.key === "Enter" || event.key === " ") {
      toggle(event);
    } else if (event.key === "Escape") {
      setOpen(nav, false);
    }
  });
  nav.button.setAttribute("aria-expanded", "false");
  nav.button.setAttribute("aria-haspopup", "true");
};

const bindClosing = (doc: Document, nav: Menu): void => {
  const closeOutside = (event: Event): void => {
    if (!(event.target instanceof Node && nav.dropdown.contains(event.target))) {
      setOpen(nav, false);
    }
  };
  doc.addEventListener("click", closeOutside);
  doc.addEventListener("touchend", closeOutside);
  nav.menu.addEventListener("click", (event) => {
    if (event.target instanceof HTMLAnchorElement) {
      setOpen(nav, false);
    }
  });
};

const setupMobileNavigation = (doc: Document): void => {
  const dropdown = doc.querySelector(".navbar-start .dropdown");
  const button = dropdown?.querySelector('[role="button"]');
  const menu = dropdown?.querySelector(".dropdown-content");
  if (!(dropdown instanceof HTMLElement && button instanceof HTMLElement && menu instanceof HTMLElement)) {
    return;
  }
  const nav: Menu = { dropdown, button, menu, open: false };
  bindButton(nav);
  bindClosing(doc, nav);
};

export { setupMobileNavigation };
