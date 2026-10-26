import { describe, expect, it } from "vitest";

import { setupMobileNavigation } from "#ui/lib/common/nav";

const page = (): { button: HTMLElement; dropdown: HTMLElement } => {
  document.body.innerHTML = `<div class="navbar-start"><div class="dropdown">
      <div role="button" tabindex="0">menu</div>
      <ul class="dropdown-content"><li><a id="item" href="/download">Download</a></li></ul>
    </div></div><p id="outside">elsewhere</p>`;
  setupMobileNavigation(document);
  const button = document.querySelector<HTMLElement>('[role="button"]');
  const dropdown = document.querySelector<HTMLElement>(".dropdown");
  if (button === null || dropdown === null) {
    throw new Error("no menu");
  }
  return { button, dropdown };
};

const shown = (dropdown: HTMLElement, button: HTMLElement): { open: boolean; expanded: string | null } => ({
  open: dropdown.classList.contains("dropdown-open"),
  expanded: button.getAttribute("aria-expanded"),
});

const key = (target: HTMLElement, name: string): void => {
  target.dispatchEvent(new KeyboardEvent("keydown", { key: name, bubbles: true }));
};

describe("the mobile menu button", () => {
  it("starts closed and announces a popup", () => {
    const { button, dropdown } = page();
    expect([shown(dropdown, button), button.getAttribute("aria-haspopup")]).toStrictEqual([
      { open: false, expanded: "false" },
      "true",
    ]);
  });

  it("opens and closes on click", () => {
    const { button, dropdown } = page();
    button.click();
    const opened = shown(dropdown, button);
    button.click();
    expect([opened, shown(dropdown, button)]).toStrictEqual([
      { open: true, expanded: "true" },
      { open: false, expanded: "false" },
    ]);
  });

  it("opens on Enter and Space, and closes on Escape", () => {
    const { button, dropdown } = page();
    key(button, "Enter");
    const entered = shown(dropdown, button).open;
    key(button, "Escape");
    const escaped = shown(dropdown, button).open;
    key(button, " ");
    expect([entered, escaped, shown(dropdown, button).open]).toStrictEqual([true, false, true]);
  });

  it("opens on touch", () => {
    const { button, dropdown } = page();
    button.dispatchEvent(new Event("touchend", { bubbles: true, cancelable: true }));
    expect(shown(dropdown, button)).toStrictEqual({ open: true, expanded: "true" });
  });
});

describe("an open mobile menu", () => {
  it("closes on a click outside it", () => {
    const { button, dropdown } = page();
    button.click();
    document.querySelector<HTMLElement>("#outside")?.click();
    expect(shown(dropdown, button)).toStrictEqual({ open: false, expanded: "false" });
  });

  it("closes when an item is chosen", () => {
    const { button, dropdown } = page();
    button.click();
    document.querySelector<HTMLAnchorElement>("#item")?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    expect(shown(dropdown, button)).toStrictEqual({ open: false, expanded: "false" });
  });
});
