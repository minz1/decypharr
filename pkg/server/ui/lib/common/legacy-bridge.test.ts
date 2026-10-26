import { describe, expect, it } from "vitest";

import { escapeHtml, legacyToast } from "#ui/lib/common/legacy-bridge";

describe("escaping HTML for legacy pages", () => {
  it("escapes the five special characters", () => {
    expect(escapeHtml(`<img src=x onerror="a('b')">&`)).toBe(
      "&lt;img src=x onerror=&quot;a(&#039;b&#039;)&quot;&gt;&amp;",
    );
  });

  it.each(["null", '""', "0"])("turns %s into an empty string, as common.js did", (json) => {
    expect(escapeHtml(JSON.parse(json))).toBe("");
  });
});

const shown = (): string[] =>
  [...document.querySelectorAll(".toast-container .alert")].map((toast) => `${toast.className} ${toast.textContent}`);

describe("toasts from legacy pages", () => {
  it("coerce what untyped callers pass, as common.js did", () => {
    document.body.replaceChildren();
    legacyToast(new Error("boom"), "nope", 0);
    legacyToast(JSON.parse("null"));
    expect(shown()).toStrictEqual([
      "alert mb-2 max-w-full shadow-lg alert-success Error: boom",
      "alert mb-2 max-w-full shadow-lg alert-success ",
    ]);
  });
});
