import { describe, expect, it } from "vitest";

import { cloneElement, field, setFields } from "#ui/lib/stats/dom";

const page = (): void => {
  document.body.innerHTML = `<div id="root"><b data-field="name"></b><i data-field="size"></i></div>
    <template id="row"><li class="row"><span data-field="name"></span></li></template>
    <template id="blank"> </template>`;
};

describe("filling fields", () => {
  it("sets the text of each named field", () => {
    page();
    const root = document.body;
    setFields(root, { name: "<img src=x onerror=alert(1)>", size: "1 KB" });
    expect([field(root, "name").textContent, field(root, "size").textContent]).toStrictEqual([
      "<img src=x onerror=alert(1)>",
      "1 KB",
    ]);
    expect(root.querySelector("img")).toBeNull();
  });

  it("throws naming a missing field", () => {
    page();
    expect(() => field(document.body, "absent")).toThrow(new TypeError('missing [data-field="absent"]'));
  });
});

describe("cloning a template's element", () => {
  it("returns a detached copy of the first element", () => {
    page();
    const row = cloneElement("row");
    expect([row.tagName, row.isConnected]).toStrictEqual(["LI", false]);
  });

  it("throws when the template holds no element", () => {
    page();
    expect(() => cloneElement("blank")).toThrow(new TypeError("empty template #blank"));
  });
});
