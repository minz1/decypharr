import { describe, expect, it } from "vitest";

import { escapeHtml } from "#ui/lib/common/legacy-bridge";

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
