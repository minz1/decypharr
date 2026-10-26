import { describe, expect, it, vi } from "vitest";

import { cloneRow, field, onClick } from "#ui/lib/common/dom";
import { getJSON } from "#ui/lib/common/http";
import { isRecord, numOrZero, optList, strOrEmpty } from "#ui/lib/common/json";
import { element } from "#ui/lib/testing/dom";

const page = (): void => {
  document.body.innerHTML = `<div data-field="name">x</div><button id="go" type="button">Go</button>
    <template id="row"><table><tbody><tr><td>one</td></tr></tbody></table></template>
    <template id="no-row"><p>none</p></template>`;
};

describe("reading loose JSON fields", () => {
  it("tells a plain object from null and arrays", () => {
    expect([isRecord({}), isRecord(JSON.parse("null")), isRecord([]), isRecord("x")]).toStrictEqual([
      true,
      false,
      false,
      false,
    ]);
  });

  it("reads an absent or null list as empty and rejects a non-list", () => {
    const record: Record<string, unknown> = { none: JSON.parse("null"), list: [1, 2], bad: "x" };
    expect([optList(record, "missing"), optList(record, "none"), optList(record, "list")]).toStrictEqual([
      [],
      [],
      [1, 2],
    ]);
    expect(() => optList(record, "bad")).toThrow("bad");
  });

  it("falls back to an empty string and zero", () => {
    const record: Record<string, unknown> = { name: "a", size: 3, none: JSON.parse("null") };
    expect([
      strOrEmpty(record, "name"),
      strOrEmpty(record, "none"),
      numOrZero(record, "size"),
      numOrZero(record, "x"),
    ]).toStrictEqual(["a", "", 3, 0]);
  });
});

describe("shared DOM lookups", () => {
  it("finds a data-field and names a missing one", () => {
    page();
    expect(field(document, "name").textContent).toBe("x");
    expect(() => field(document, "size")).toThrow('missing [data-field="size"]');
  });

  it("clones a template's row and names a template without one", () => {
    page();
    expect(cloneRow("row").textContent).toBe("one");
    expect(() => cloneRow("no-row")).toThrow("template #no-row has no row");
  });

  it("runs a click action, sync or async", async () => {
    page();
    const ran: string[] = [];
    const button = element("#go", HTMLButtonElement);
    onClick(button, () => ran.push("sync"));
    onClick(button, async () => {
      ran.push("async");
      return Promise.resolve();
    });
    button.click();
    await Promise.resolve();
    expect(ran).toStrictEqual(["sync", "async"]);
  });
});

describe("getting JSON", () => {
  it("reads the JSON body through the URL base", async () => {
    document.head.innerHTML = '<meta name="decypharr-url-base" content="/base/">';
    const urls: string[] = [];
    vi.stubGlobal("fetch", async (url: string): Promise<Response> => {
      urls.push(url);
      return Promise.resolve(Response.json({ ok: true }));
    });
    expect({ body: await getJSON("/api/x"), urls }).toStrictEqual({ body: { ok: true }, urls: ["/base/api/x"] });
  });
});
