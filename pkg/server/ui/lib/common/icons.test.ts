import { describe, expect, inject, it } from "vitest";

describe("bootstrap icons", () => {
  it("every bi- class used exists in bootstrap-icons.css", () => {
    const css = inject("iconCSS");
    const used = inject("iconClasses");
    expect(used.length).toBeGreaterThan(100);
    expect(used.filter((iconClass) => !css.includes(`.${iconClass}::before`))).toStrictEqual([]);
  });
});
