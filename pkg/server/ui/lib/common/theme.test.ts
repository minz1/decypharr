import { describe, expect, it, vi } from "vitest";

import { setupThemeToggle } from "#ui/lib/common/theme";

const systemQuery = (dark: boolean): MediaQueryList => {
  const list = globalThis.matchMedia(dark ? "(min-width: 0px)" : "(max-width: 0px)");
  vi.spyOn(globalThis, "matchMedia").mockReturnValue(list);
  return list;
};

const page = (stored: string, systemDark: boolean): MediaQueryList => {
  vi.restoreAllMocks();
  localStorage.clear();
  if (stored !== "") {
    localStorage.setItem("theme", stored);
  }
  document.body.innerHTML = '<input type="checkbox" id="themeToggle">';
  const list = systemQuery(systemDark);
  setupThemeToggle(document);
  return list;
};

const systemChange = (list: MediaQueryList, dark: boolean): void => {
  list.dispatchEvent(Object.assign(new Event("change"), { matches: dark }));
};

const state = (): { theme: string; stored: string; checked: boolean } => ({
  theme: document.documentElement.dataset["theme"] ?? "",
  stored: localStorage.getItem("theme") ?? "",
  checked: document.querySelector<HTMLInputElement>("#themeToggle")?.checked ?? false,
});

describe("the theme on load", () => {
  it("is the stored theme", () => {
    page("light", true);
    expect(state()).toStrictEqual({ theme: "light", stored: "light", checked: false });
  });

  it("follows the system when nothing is stored, and stores it", () => {
    page("", true);
    expect(state()).toStrictEqual({ theme: "dark", stored: "dark", checked: true });
  });

  it("is light when the system is not dark", () => {
    page("", false);
    expect(state()).toStrictEqual({ theme: "light", stored: "light", checked: false });
  });
});

describe("the theme toggle", () => {
  it("flips the theme and stores it", () => {
    page("dark", true);
    document.querySelector<HTMLInputElement>("#themeToggle")?.click();
    expect(state()).toStrictEqual({ theme: "light", stored: "light", checked: false });
  });

  it("is optional", () => {
    document.documentElement.dataset["theme"] = "dark";
    document.body.innerHTML = "";
    localStorage.clear();
    setupThemeToggle(document);
    expect(state()).toStrictEqual({ theme: "dark", stored: "", checked: false });
  });
});

describe("a system theme change", () => {
  it("is followed only while nothing is stored", () => {
    const list = page("dark", true);
    systemChange(list, false);
    const kept = state().theme;
    localStorage.clear();
    systemChange(list, false);
    expect({ kept, followed: state().theme }).toStrictEqual({ kept: "dark", followed: "light" });
  });
});
