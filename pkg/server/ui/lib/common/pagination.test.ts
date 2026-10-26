import { describe, expect, it } from "vitest";

import { pageItems, showingText } from "#ui/lib/common/pagination";

const summary = (current: number, totalPages: number): string[] =>
  pageItems(current, totalPages).map((item) => {
    if (item.kind === "page") {
      return item.active ? `[${item.page}]` : String(item.page);
    }
    if (item.kind === "gap") {
      return "...";
    }
    return `${item.kind}:${item.page}${item.disabled ? "x" : ""}`;
  });

describe("page items", () => {
  it("has no controls for one page or none", () => {
    expect([pageItems(1, 1), pageItems(1, 0)]).toStrictEqual([[], []]);
  });

  it("shows every page when there are few", () => {
    expect(summary(1, 3)).toStrictEqual(["previous:0x", "[1]", "2", "3", "next:2"]);
  });

  it("keeps the first, the last and two either side of the current page, with gaps three away", () => {
    expect(summary(6, 12)).toStrictEqual(["previous:5", "1", "...", "4", "5", "[6]", "7", "8", "...", "12", "next:7"]);
  });

  it("leaves a page two away from the first out of the gap", () => {
    expect(summary(5, 12)).toStrictEqual(["previous:4", "1", "...", "3", "4", "[5]", "6", "7", "...", "12", "next:6"]);
  });

  it("disables next on the last page", () => {
    expect(summary(10, 10)).toStrictEqual(["previous:9", "1", "...", "8", "9", "[10]", "next:11x"]);
  });
});

describe("the showing text", () => {
  it("names the range shown", () => {
    expect(showingText({ page: 2, perPage: 20, total: 45 }, "No items")).toBe("Showing 21-40 of 45 items");
  });

  it("clips the range to the total", () => {
    expect(showingText({ page: 3, perPage: 20, total: 45 }, "No items")).toBe("Showing 41-45 of 45 items");
  });

  it("uses the empty text when there is nothing", () => {
    expect(showingText({ page: 1, perPage: 20, total: 0 }, "No items found")).toBe("No items found");
  });
});
