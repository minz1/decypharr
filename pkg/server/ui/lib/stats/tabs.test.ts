import { describe, expect, it } from "vitest";

import { scrollTarget } from "#ui/lib/stats/tabs";

describe("centring a tab in its scroller", () => {
  it("scrolls so the tab sits in the middle", () => {
    expect(scrollTarget({ left: 300, width: 100 }, 200)).toBe(250);
  });

  it("never scrolls before the start", () => {
    expect(scrollTarget({ left: 20, width: 100 }, 400)).toBe(0);
  });
});
