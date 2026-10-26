import { describe, expect, it } from "vitest";

import { setButtonLoading } from "#ui/lib/common/button";

const button = (): HTMLButtonElement => {
  document.body.innerHTML = '<button><i class="bi bi-save"></i> Save</button>';
  const found = document.querySelector("button");
  if (found === null) {
    throw new Error("no button");
  }
  return found;
};

describe("a loading button", () => {
  it("disables the button and shows a spinner while loading", () => {
    const save = button();
    setButtonLoading(save, true);
    expect({ disabled: save.disabled, text: save.textContent }).toStrictEqual({
      disabled: true,
      text: "Processing...",
    });
    expect(save.querySelector("span.loading.loading-spinner.loading-sm")).not.toBeNull();
    expect(save.querySelector("i")).toBeNull();
  });

  it("puts the original content back", () => {
    const save = button();
    const icon = save.querySelector("i");
    setButtonLoading(save, true);
    setButtonLoading(save, false);
    expect({ disabled: save.disabled, text: save.textContent }).toStrictEqual({ disabled: false, text: " Save" });
    expect(save.querySelector("i")).toBe(icon);
  });

  it("keeps the first content when set loading twice", () => {
    const save = button();
    setButtonLoading(save, true);
    setButtonLoading(save, true);
    setButtonLoading(save, false);
    expect(save.textContent).toBe(" Save");
  });

  it("shows Submit when restoring a button it never set loading, as common.js did", () => {
    const save = button();
    setButtonLoading(save, false);
    expect({ disabled: save.disabled, text: save.textContent }).toStrictEqual({ disabled: false, text: "Submit" });
  });

  it("does nothing without a button", () => {
    expect(() => {
      setButtonLoading(document.querySelector<HTMLButtonElement>("#absent"), true);
    }).not.toThrow();
  });
});
