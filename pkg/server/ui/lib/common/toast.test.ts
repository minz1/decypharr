import { describe, expect, it, vi } from "vitest";

import { closeAllToasts, closeToast, createToast } from "#ui/lib/common/toast";

const TOAST_TEMPLATE = `<template id="toast-template">
  <div class="alert mb-2 max-w-full shadow-lg">
    <div class="flex min-w-0 items-start gap-3">
      <svg data-type="success"></svg><svg data-type="error"></svg>
      <svg data-type="warning"></svg><svg data-type="info"></svg>
      <div class="min-w-0 flex-1 break-words"><span class="text-sm" data-toast-message></span></div>
      <button class="btn" data-action="close-toast"></button>
    </div>
  </div>
</template>`;

const page = (body = `${TOAST_TEMPLATE}<div class="toast-container"></div>`): void => {
  vi.useFakeTimers();
  document.body.innerHTML = body;
};

const toasts = (): Element[] => [...document.querySelectorAll(".toast-container .alert")];

const only = (): Element => {
  const [toast, ...rest] = toasts();
  if (!toast || rest.length > 0) {
    throw new Error(`want one toast, have ${toasts().length}`);
  }
  return toast;
};

const icons = (toast: Element): string[] => [...toast.querySelectorAll("svg")].map((svg) => svg.dataset["type"] ?? "");

const messageOf = (toast: Element): Element => {
  const message = toast.querySelector("[data-toast-message]");
  if (message === null) {
    throw new Error("toast has no message");
  }
  return message;
};

describe("creating a toast", () => {
  it("clones the template with the type's class and icon", () => {
    page();
    const id = createToast("Saved", "warning");
    expect(only().id).toBe(id);
    expect([...only().classList]).toContain("alert-warning");
    expect(icons(only())).toStrictEqual(["warning"]);
    expect(messageOf(only()).textContent).toBe("Saved");
  });

  it("defaults to success", () => {
    page();
    createToast("Done");
    expect([...only().classList]).toContain("alert-success");
    expect(icons(only())).toStrictEqual(["success"]);
  });

  it("renders markup in the message as text", () => {
    page();
    createToast('<img src=x onerror="alert(1)">', "error");
    expect(messageOf(only()).textContent).toBe('<img src=x onerror="alert(1)">');
    expect(messageOf(only()).querySelector("img")).toBeNull();
  });

  it("turns newlines into line breaks", () => {
    page();
    createToast("one\ntwo\nthree", "info");
    expect(messageOf(only()).querySelectorAll("br")).toHaveLength(2);
    expect(messageOf(only()).textContent).toBe("onetwothree");
  });

  it("gives each toast its own id", () => {
    page();
    expect(createToast("a")).not.toBe(createToast("b"));
  });
});

describe("a toast's lifetime", () => {
  it.each([
    ["success", 5000],
    ["info", 7000],
    ["warning", 10_000],
    ["error", 15_000],
  ] as const)("closes a %s toast after %d ms", (type, duration) => {
    page();
    createToast("x", type);
    vi.advanceTimersByTime(duration - 1);
    expect([...only().classList]).not.toContain("toast-closing");
    vi.advanceTimersByTime(1);
    expect([...only().classList]).toContain("toast-closing");
    vi.advanceTimersByTime(300);
    expect(toasts()).toHaveLength(0);
  });

  it("honours a custom duration", () => {
    page();
    createToast("x", "success", 50);
    vi.advanceTimersByTime(350);
    expect(toasts()).toHaveLength(0);
  });

  it("ends when its close button is clicked", () => {
    page();
    createToast("x");
    only().querySelector<HTMLButtonElement>('[data-action="close-toast"]')?.click();
    vi.advanceTimersByTime(300);
    expect(toasts()).toHaveLength(0);
  });
});

describe("a page without the toast markup", () => {
  it("gets a container when it has none", () => {
    page(TOAST_TEMPLATE);
    createToast("x");
    expect(toasts()).toHaveLength(1);
  });

  it("gets a plain alert when the template is missing", () => {
    page('<div class="toast-container"></div>');
    const id = createToast("<b>a</b>\nb", "error");
    expect(only().id).toBe(id);
    expect([...only().classList]).toContain("alert-error");
    expect(only().textContent).toBe("<b>a</b>b");
    expect(only().querySelectorAll("br")).toHaveLength(1);
  });
});

describe("closing a toast", () => {
  it("animates out, then removes the toast", () => {
    page();
    closeToast(createToast("x"));
    expect([...only().classList]).toContain("toast-closing");
    vi.advanceTimersByTime(299);
    expect(toasts()).toHaveLength(1);
    vi.advanceTimersByTime(1);
    expect(toasts()).toHaveLength(0);
  });

  it("ignores an unknown id", () => {
    page();
    createToast("x");
    closeToast("nope");
    vi.advanceTimersByTime(300);
    expect(toasts()).toHaveLength(1);
  });
});

describe("closing every toast", () => {
  it("closes every toast", () => {
    page();
    createToast("a");
    createToast("b", "error");
    closeAllToasts();
    vi.advanceTimersByTime(300);
    expect(toasts()).toHaveLength(0);
  });
});
