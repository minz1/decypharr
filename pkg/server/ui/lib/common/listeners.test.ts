import { describe, expect, it, vi } from "vitest";

import { setupGlobalListeners } from "#ui/lib/common/listeners";

const listening = new Set<Document>();

const page = (body: string): void => {
  if (!listening.has(document)) {
    setupGlobalListeners(document);
    listening.add(document);
  }
  document.body.innerHTML = `<div class="toast-container"></div>${body}`;
};

const toasts = (): string[] =>
  [...document.querySelectorAll(".toast-container .alert")].map(
    (toast) => `${[...toast.classList].find((name) => name.startsWith("alert-")) ?? ""} ${toast.textContent ?? ""}`,
  );

const classesOf = (selector: string): string[] => [...(document.querySelector(selector)?.classList ?? [])];

const scrollOptions = (options: boolean | ScrollIntoViewOptions | undefined): ScrollIntoViewOptions =>
  typeof options === "object" ? options : {};

const keydown = (key: string): void => {
  document.dispatchEvent(new KeyboardEvent("keydown", { key, bubbles: true }));
};

describe("errors nobody caught", () => {
  it("toast an error event's message", () => {
    page("");
    globalThis.dispatchEvent(new ErrorEvent("error", { error: new Error("boom") }));
    globalThis.dispatchEvent(new ErrorEvent("error", { error: "text" }));
    const blank = Object.assign(new Error("blank"), { message: "" });
    globalThis.dispatchEvent(new ErrorEvent("error", { error: blank }));
    expect(toasts()).toStrictEqual([
      "alert-error Unexpected error: boom",
      "alert-error Unexpected error: Unknown error",
      "alert-error Unexpected error: Unknown error",
    ]);
  });

  it("toast an unhandled rejection's reason", () => {
    page("");
    globalThis.dispatchEvent(Object.assign(new Event("unhandledrejection"), { reason: new Error("no") }));
    globalThis.dispatchEvent(Object.assign(new Event("unhandledrejection"), { reason: "text" }));
    globalThis.dispatchEvent(Object.assign(new Event("unhandledrejection"), { reason: { message: "plain" } }));
    expect(toasts()).toStrictEqual([
      "alert-error Promise rejected: no",
      "alert-error Promise rejected: Unknown error",
      "alert-error Promise rejected: plain",
    ]);
  });
});

describe("the connection", () => {
  it("toasts going offline and coming back", () => {
    page("");
    globalThis.dispatchEvent(new Event("offline"));
    globalThis.dispatchEvent(new Event("online"));
    expect(toasts()).toStrictEqual([
      "alert-warning Connection lost - working offline",
      "alert-success Connection restored",
    ]);
  });
});

describe("an invalid field", () => {
  it("is highlighted for three seconds", () => {
    vi.useFakeTimers();
    page('<form><input id="field" required></form>');
    document.querySelector("#field")?.dispatchEvent(new Event("invalid"));
    const during = classesOf("#field");
    vi.advanceTimersByTime(3000);
    vi.useRealTimers();
    expect({ during, after: classesOf("#field") }).toStrictEqual({ during: ["input-error"], after: [] });
  });
});

describe("the Escape key", () => {
  it("closes open modals, dropdowns and context menus", () => {
    page(`<dialog id="modal" class="modal" open></dialog>
      <div id="dropdown" class="dropdown dropdown-open"></div>
      <div id="menu" class="context-menu"></div>`);
    keydown("Escape");
    expect({
      modal: document.querySelector("#modal")?.hasAttribute("open"),
      dropdown: document.querySelector("#dropdown")?.className,
      menu: document.querySelector("#menu")?.className,
    }).toStrictEqual({ modal: false, dropdown: "dropdown", menu: "context-menu hidden" });
  });

  it("leaves things open on other keys", () => {
    page('<div id="dropdown" class="dropdown dropdown-open"></div>');
    keydown("Enter");
    expect(document.querySelector("#dropdown")?.className).toBe("dropdown dropdown-open");
  });
});

describe("an in-page link", () => {
  it("scrolls smoothly to its target", () => {
    page('<a id="link" href="#target">go</a><section id="target"></section>');
    const scrolled: ScrollIntoViewOptions[] = [];
    vi.spyOn(Element.prototype, "scrollIntoView").mockImplementation((options) => {
      scrolled.push(scrollOptions(options));
    });
    const click = new MouseEvent("click", { bubbles: true, cancelable: true });
    document.querySelector("#link")?.dispatchEvent(click);
    vi.restoreAllMocks();
    expect({ prevented: click.defaultPrevented, scrolled }).toStrictEqual({
      prevented: true,
      scrolled: [{ behavior: "smooth", block: "start" }],
    });
  });

  it("is left alone when it is a bare #", () => {
    page('<a id="link" href="#">top</a>');
    const click = new MouseEvent("click", { bubbles: true, cancelable: true });
    document.querySelector("#link")?.dispatchEvent(click);
    expect({ prevented: click.defaultPrevented }).toStrictEqual({ prevented: false });
  });
});
