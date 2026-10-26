import { describe, expect, it } from "vitest";

import { ShapeError, asArray, asRecord, bool, num, optBool, optNum, optStr, readJSON, str } from "#ui/lib/common/json";

const parsed = (text: string): unknown => JSON.parse(text);

const record = asRecord(parsed('{"name":"a","size":3,"done":true,"gone":null}'), "record");

describe("reading a JSON response", () => {
  it("parses a JSON body", async () => {
    await expect(readJSON(new Response('{"list":[1]}'))).resolves.toStrictEqual({ list: [1] });
  });

  it("rejects a malformed body with a ShapeError", async () => {
    await expect(readJSON(new Response("<html>"))).rejects.toThrow(new ShapeError("response is not JSON"));
  });

  it("rejects a non-OK response with its text", async () => {
    await expect(readJSON(new Response("torrent not found\n", { status: 404 }))).rejects.toThrow(
      new Error("torrent not found"),
    );
  });

  it("rejects a non-OK response without text with its status", async () => {
    await expect(readJSON(new Response("", { status: 502, statusText: "Bad Gateway" }))).rejects.toThrow(
      new Error("502 Bad Gateway"),
    );
  });
});

describe("reading an object", () => {
  it("accepts an object", () => {
    expect(asRecord(record, "torrent")).toBe(record);
  });

  it.each(["null", "[]", '"x"', "1"])("rejects %s, naming the path", (text) => {
    expect(() => asRecord(parsed(text), "torrent")).toThrow(new ShapeError("torrent: expected an object"));
  });
});

describe("reading an array", () => {
  it("accepts an array", () => {
    expect(asArray(parsed('[1,"a"]'), "list")).toStrictEqual([1, "a"]);
  });

  it.each(["null", "{}", '"x"'])("rejects %s, naming the path", (text) => {
    expect(() => asArray(parsed(text), "list")).toThrow(new ShapeError("list: expected an array"));
  });
});

describe("required fields", () => {
  it("read values of the right type", () => {
    expect(str(record, "name")).toBe("a");
    expect(num(record, "size")).toBe(3);
    expect([bool(record, "done")]).toStrictEqual([true]);
  });

  it.each([
    ["str", (): unknown => str(record, "size"), "size: expected a string"],
    ["num", (): unknown => num(record, "name"), "name: expected a number"],
    ["bool", (): unknown => bool(record, "name"), "name: expected a boolean"],
    ["str", (): unknown => str(record, "missing"), "missing: expected a string"],
    ["num", (): unknown => num(record, "gone"), "gone: expected a number"],
    ["str", (): unknown => str(record, "toString"), "toString: expected a string"],
  ])("%s throws a ShapeError naming the key", (_reader, read, message) => {
    expect(read).toThrow(new ShapeError(message));
  });
});

describe("optional fields", () => {
  it("read values of the right type", () => {
    expect(optStr(record, "name")).toBe("a");
    expect(optNum(record, "size")).toBe(3);
    expect([optBool(record, "done")]).toStrictEqual([true]);
  });

  it("give no value when the key is absent or null", () => {
    expect(optStr(record, "missing")).toBeUndefined();
    expect(optNum(record, "gone")).toBeUndefined();
    expect(optBool(record, "missing")).toBeUndefined();
    expect(optStr(record, "toString")).toBeUndefined();
  });

  it("throw a ShapeError on the wrong type", () => {
    expect(() => optStr(record, "size")).toThrow(new ShapeError("size: expected a string"));
    expect(() => optNum(record, "name")).toThrow(new ShapeError("name: expected a number"));
    expect(() => optBool(record, "size")).toThrow(new ShapeError("size: expected a boolean"));
  });
});

describe("a shape error", () => {
  it("is named ShapeError", () => {
    expect(new ShapeError("x").name).toBe("ShapeError");
    expect(new ShapeError("x")).toBeInstanceOf(Error);
  });
});
