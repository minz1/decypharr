import { describe, expect, it } from "vitest";

import { setupPasswordToggles } from "#ui/lib/common/password";

const listening = new Set<Document>();

const page = (field: string): { button: HTMLElement; icon: Element } => {
  if (!listening.has(document)) {
    setupPasswordToggles(document);
    listening.add(document);
  }
  document.body.innerHTML = `<div class="password-toggle-container">${field}
    <button type="button" class="password-toggle-btn"><i class="bi bi-eye"></i></button></div>`;
  const button = document.querySelector<HTMLElement>(".password-toggle-btn");
  const icon = button?.querySelector("i");
  if (!button || !icon) {
    throw new Error("no toggle");
  }
  return { button, icon };
};

describe("a password input's toggle", () => {
  it("shows the password, then hides it again", () => {
    const { button, icon } = page('<input id="secret" type="password">');
    const input = document.querySelector<HTMLInputElement>("#secret");
    button.click();
    const shown = { type: input?.type, icon: icon.className };
    button.click();
    expect([shown, { type: input?.type, icon: icon.className }]).toStrictEqual([
      { type: "text", icon: "bi bi-eye-slash" },
      { type: "password", icon: "bi bi-eye" },
    ]);
  });

  it("works from a click on its icon", () => {
    const { icon } = page('<input id="secret" type="password">');
    icon.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    expect(document.querySelector<HTMLInputElement>("#secret")?.type).toBe("text");
  });
});

describe("a password textarea's toggle", () => {
  it("marks the text visible, then hidden again", () => {
    const { button, icon } = page('<textarea id="secret" style="-webkit-text-security: disc"></textarea>');
    const area = document.querySelector<HTMLTextAreaElement>("#secret");
    const read = (): { visible: string | undefined; icon: string } => ({
      visible: area?.dataset["passwordVisible"],
      icon: icon.className,
    });
    button.click();
    const shown = read();
    button.click();
    expect([shown, read()]).toStrictEqual([
      { visible: "true", icon: "bi bi-eye-slash" },
      { visible: "false", icon: "bi bi-eye" },
    ]);
  });
});
