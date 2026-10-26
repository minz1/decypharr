import { describe, expect, it } from "vitest";

import { joinURL, readURLBase } from "#ui/lib/common/url";

const withHead = (head: string): Document => {
  const doc = document.implementation.createHTMLDocument();
  doc.head.innerHTML = head;
  return doc;
};

describe("reading the URL base", () => {
  it("reads the decypharr-url-base meta", () => {
    expect(readURLBase(withHead('<meta name="decypharr-url-base" content="/decypharr/">'))).toBe("/decypharr/");
  });

  it("is empty without the meta", () => {
    expect(readURLBase(withHead(""))).toBe("");
  });
});

describe("joining URLs", () => {
  it.each([
    ["", "/version", "/version"],
    ["/", "version", "/version"],
    ["/", "/api/x", "/api/x"],
    ["/decypharr/", "/api/x", "/decypharr/api/x"],
    ["/decypharr", "api/x", "/decypharr/api/x"],
    ["/decypharr/", "", "/decypharr/"],
  ])("joins %j and %j as %j", (base, path, want) => {
    expect(joinURL(base, path)).toBe(want);
  });
});
