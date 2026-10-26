import { describe, expect, it, vi } from "vitest";

import { loadFixture } from "#ui/lib/testing/fixture";

const loadLayout = async (fixture: string, theme = ""): Promise<string[]> => {
  vi.resetModules();
  localStorage.clear();
  if (theme !== "") {
    localStorage.setItem("theme", theme);
  }
  loadFixture(fixture);
  const urls: string[] = [];
  vi.stubGlobal("fetch", async (url: string): Promise<Response> => {
    urls.push(url);
    return Promise.resolve(Response.json({ version: "1.2.3", channel: "beta" }));
  });
  await import("#ui/pages/layout");
  return urls;
};

const toastTexts = (): string[] =>
  [...document.querySelectorAll(".toast-container .alert-error [data-toast-message]")].map(
    (toast) => toast.textContent,
  );

const press = (key: string, ctrlKey: boolean): void => {
  document.dispatchEvent(new KeyboardEvent("keydown", { key, ctrlKey, bubbles: true }));
};

describe("the theme toggle", () => {
  it("flips data-theme and stores it", async () => {
    await loadLayout("index");
    const before = document.documentElement.dataset["theme"];
    document.querySelector<HTMLInputElement>("#themeToggle")?.click();
    const after = document.documentElement.dataset["theme"];
    expect(new Set([before, after])).toStrictEqual(new Set(["dark", "light"]));
    expect(localStorage.getItem("theme")).toBe(after);
  });

  it.each(["light", "dark"])("starts from the stored %s theme", async (theme) => {
    await loadLayout("index", theme);
    expect({
      theme: document.documentElement.dataset["theme"],
      checked: document.querySelector<HTMLInputElement>("#themeToggle")?.checked,
    }).toStrictEqual({ theme, checked: theme === "dark" });
  });
});

describe("the keyboard", () => {
  it("opens the shortcut help on ctrl + /, and Escape closes it", async () => {
    await loadLayout("index");
    press("/", true);
    const opened = document.querySelectorAll("body > dialog.modal[open]").length;
    press("Escape", false);
    expect(opened).toBeGreaterThan(0);
    expect(document.querySelectorAll("body > dialog.modal[open]")).toHaveLength(0);
  });
});

describe("the global listeners", () => {
  it("toast a global error with the toast template", async () => {
    await loadLayout("index");
    globalThis.dispatchEvent(new ErrorEvent("error", { error: new Error("<img src=x onerror=alert(1)>") }));
    expect(new Set(toastTexts())).toStrictEqual(new Set(["Unexpected error: <img src=x onerror=alert(1)>"]));
    expect(document.querySelectorAll(".toast-container img")).toHaveLength(0);
  });
});

describe("the version badge", () => {
  it.each([
    ["index", "/version"],
    ["index@base", "/decypharr/version"],
  ])("on %s links the release from %s and marks a beta", async (fixture, url) => {
    const urls = await loadLayout(fixture);
    await vi.waitFor(() => {
      expect(document.querySelector("#version-badge a")?.textContent).toBe("beta-1.2.3");
    });
    const badge = document.querySelector("#version-badge");
    expect({
      urls,
      href: badge?.querySelector("a")?.href,
      beta: badge?.classList.contains("badge-warning"),
    }).toStrictEqual({
      urls: [url],
      href: "https://github.com/sirrobot01/decypharr/releases/tag/v1.2.3",
      beta: true,
    });
  });
});
