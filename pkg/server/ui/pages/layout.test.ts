import { describe, expect, it, vi } from "vitest";

const LAYOUT = `<div class="toast-container"></div>
  <template id="toast-template"><div class="alert"><span data-toast-message></span></div></template>
  <header class="navbar">
    <div class="navbar-start"><div class="dropdown">
      <div role="button" tabindex="0">menu</div><ul class="dropdown-content"><li><a href="#x">x</a></li></ul>
    </div></div>
    <input type="checkbox" id="themeToggle">
    <div id="version-badge">Loading...</div>
  </header>
  <dialog id="open-modal" class="modal" open><p>modal</p></dialog>`;

const BRIDGE_MEMBERS = [
  "closeToast",
  "copyToClipboard",
  "createToast",
  "escapeHtml",
  "fetcher",
  "formatBytes",
  "formatDuration",
  "joinURL",
  "setButtonLoading",
];

const loadLayout = async (urlBase: string, theme = ""): Promise<void> => {
  vi.resetModules();
  localStorage.clear();
  if (theme !== "") {
    localStorage.setItem("theme", theme);
  }
  document.head.innerHTML = `<meta name="decypharr-url-base" content="${urlBase}">`;
  document.body.innerHTML = LAYOUT;
  vi.stubGlobal("fetch", async (): Promise<Response> =>
    Promise.resolve(Response.json({ version: "1.2.3", channel: "beta" })),
  );
  await import("#ui/pages/layout");
};

const toastTexts = (): (string | null)[] =>
  [...document.querySelectorAll(".toast-container .alert-error")].map((toast) => toast.textContent);

describe("the legacy bridge", () => {
  it("is in place as soon as the layout module has run", async () => {
    await loadLayout("/decypharr/");
    expect(globalThis.urlBase).toBe("/decypharr/");
    expect(Object.keys(globalThis.decypharrUtils).toSorted()).toStrictEqual(BRIDGE_MEMBERS);
    expect(Object.values(globalThis.decypharrUtils).map((member) => typeof member)).toStrictEqual(
      BRIDGE_MEMBERS.map(() => "function"),
    );
  });
});

describe("the theme toggle", () => {
  it("flips data-theme and stores it", async () => {
    await loadLayout("/");
    const toggle = document.querySelector<HTMLInputElement>("#themeToggle");
    const before = document.documentElement.dataset["theme"];
    toggle?.click();
    const after = document.documentElement.dataset["theme"];
    expect(new Set([before, after])).toStrictEqual(new Set(["dark", "light"]));
    expect(localStorage.getItem("theme")).toBe(after);
  });

  it.each(["light", "dark"])("starts from the stored %s theme", async (theme) => {
    await loadLayout("/", theme);
    expect({
      theme: document.documentElement.dataset["theme"],
      checked: document.querySelector<HTMLInputElement>("#themeToggle")?.checked,
    }).toStrictEqual({ theme, checked: theme === "dark" });
  });
});

describe("the global listeners", () => {
  it("close an open dialog on Escape", async () => {
    await loadLayout("/");
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    expect(document.querySelectorAll("#open-modal[open]")).toHaveLength(0);
  });

  it("toast a global error", async () => {
    await loadLayout("/");
    globalThis.dispatchEvent(new ErrorEvent("error", { error: new Error("boom") }));
    expect(new Set(toastTexts())).toStrictEqual(new Set(["Unexpected error: boom"]));
  });
});

describe("the version badge", () => {
  it("links the release and marks a beta", async () => {
    await loadLayout("/");
    await vi.waitFor(() => {
      expect(document.querySelector("#version-badge a")?.textContent).toBe("beta-1.2.3");
    });
    const badge = document.querySelector("#version-badge");
    expect(badge?.querySelector("a")?.href).toBe("https://github.com/sirrobot01/decypharr/releases/tag/v1.2.3");
    expect(badge?.className).toContain("badge-warning");
  });
});
