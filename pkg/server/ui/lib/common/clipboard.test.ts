import { describe, expect, it, vi } from "vitest";

import { copyToClipboard } from "#ui/lib/common/clipboard";

const messages = (): (string | null)[] =>
  [...document.querySelectorAll(".toast-container .alert")].map((toast) => toast.textContent);

const stubClipboard = (writeText: (text: string) => Promise<void>): void => {
  document.body.innerHTML = '<div class="toast-container"></div>';
  vi.spyOn(navigator.clipboard, "writeText").mockImplementation(writeText);
};

describe("copying to the clipboard", () => {
  it("writes the text and toasts success", async () => {
    const written: string[] = [];
    stubClipboard(async (text) => {
      written.push(text);
      return Promise.resolve();
    });
    expect({ copied: await copyToClipboard("tok") }).toStrictEqual({ copied: true });
    expect(written).toStrictEqual(["tok"]);
    expect(messages()).toStrictEqual(["Copied to clipboard"]);
  });

  it("toasts an error and resolves false when the write fails", async () => {
    stubClipboard(async () => Promise.reject(new Error("denied")));
    expect({ copied: await copyToClipboard("tok") }).toStrictEqual({ copied: false });
    expect(messages()).toStrictEqual(["Failed to copy to clipboard"]);
  });

  it("toasts an error and resolves false without the Clipboard API", async () => {
    document.body.innerHTML = '<div class="toast-container"></div>';
    vi.spyOn(navigator, "clipboard", "get").mockImplementation(() => {
      throw new TypeError("navigator.clipboard is undefined");
    });
    expect({ copied: await copyToClipboard("tok") }).toStrictEqual({ copied: false });
    expect(messages()).toStrictEqual(["Failed to copy to clipboard"]);
  });
});
