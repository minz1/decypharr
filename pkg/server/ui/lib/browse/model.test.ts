import { describe, expect, it } from "vitest";

import {
  browseAPIURL,
  browseQuery,
  breadcrumbs,
  createBrowseState,
  downloadURL,
  entryId,
  healthBadge,
  isActionable,
  nextSort,
  parseBrowsePage,
  parseHealth,
  readBrowseLocation,
  sortIndicator,
} from "#ui/lib/browse/model";
import type { Entry } from "#ui/lib/browse/types";

const entry = (fields: Partial<Entry>): Entry => ({
  name: "Show",
  path: "/__all__/Show",
  size: 0,
  modTime: "",
  isDir: true,
  infoHash: "",
  canDelete: false,
  activeDebrid: "",
  kind: "",
  ...fields,
});

describe("the browse page parser", () => {
  it("reads entries and counts", () => {
    expect(
      parseBrowsePage({
        entries: [
          {
            name: "Show",
            path: "/__all__/Show",
            size: 10,
            mod_time: "2024-01-01",
            is_dir: true,
            info_hash: "abc",
            can_delete: true,
            active_debrid: "realdebrid",
            kind: "entry",
          },
        ],
        total: 1,
        total_pages: 1,
        current_kind: "system",
      }),
    ).toStrictEqual({
      entries: [
        entry({
          size: 10,
          modTime: "2024-01-01",
          infoHash: "abc",
          canDelete: true,
          activeDebrid: "realdebrid",
          kind: "entry",
        }),
      ],
      total: 1,
      totalPages: 1,
      currentKind: "system",
    });
  });
});

describe("the browse page parser's defaults", () => {
  it("fills the fields Go omits when empty", () => {
    expect(parseBrowsePage({ entries: [{ name: "f", path: "/a/b/f", size: 1, is_dir: false }] })).toStrictEqual({
      entries: [entry({ name: "f", path: "/a/b/f", size: 1, isDir: false })],
      total: 0,
      totalPages: 0,
      currentKind: "",
    });
  });

  it("rejects an entry without a path", () => {
    expect(() => parseBrowsePage({ entries: [{ name: "f" }] })).toThrow("path: expected a string");
  });
});

describe("the health parser", () => {
  it("reads the status and the last check", () => {
    expect(parseHealth({ status: "healthy", last_checked_at: "2024-01-01T00:00:00Z", file_count: 3 })).toStrictEqual({
      status: "healthy",
      lastCheckedAt: "2024-01-01T00:00:00Z",
    });
  });
});

describe("the browse location", () => {
  it("reads path, page, search, limit and sort from the query", () => {
    expect(
      readBrowseLocation("?path=%2F__all__%2FShow&page=3&search=x&limit=50&sort_by=size&sort_order=desc"),
    ).toStrictEqual({ path: "/__all__/Show", page: 3, search: "x", perPage: 50, sortBy: "size", sortOrder: "desc" });
  });

  it("falls back to the defaults for missing or bad values", () => {
    expect(readBrowseLocation("?path=&page=0&limit=abc&sort_by=evil&sort_order=up")).toStrictEqual({
      path: "/",
      page: 1,
      search: "",
      perPage: 20,
      sortBy: "name",
      sortOrder: "asc",
    });
  });

  it("writes only what differs from the defaults", () => {
    const state = { ...createBrowseState(), path: "/a b", page: 2, search: "q", perPage: 50, sortBy: "size" as const };
    expect([browseQuery(createBrowseState()), browseQuery({ ...state, sortOrder: "desc" })]).toStrictEqual([
      "",
      "?path=%2Fa+b&page=2&search=q&limit=50&sort_by=size&sort_order=desc",
    ]);
  });
});

const at = (path: string, search = ""): string => browseAPIURL({ ...createBrowseState(), path, search });

describe("the browse API URL", () => {
  it("names up to three path levels", () => {
    expect([at("/"), at("/__all__"), at("/__all__/A B/c#d", "x"), at("/a/b/c/d")]).toStrictEqual([
      "/api/browse/?page=1&limit=20&sort_by=name&sort_order=asc",
      "/api/browse/__all__?page=1&limit=20&sort_by=name&sort_order=asc",
      "/api/browse/__all__/A%20B/c%23d?page=1&limit=20&sort_by=name&sort_order=asc&search=x",
      "/api/browse?page=1&limit=20&sort_by=name&sort_order=asc",
    ]);
  });
});

describe("sorting by a header", () => {
  it("flips the order of the current column", () => {
    expect(nextSort(createBrowseState(), "name")).toStrictEqual({ sortBy: "name", sortOrder: "desc" });
  });

  it("starts size and date descending, text ascending", () => {
    const state = createBrowseState();
    expect([nextSort(state, "size"), nextSort(state, "mod_time"), nextSort(state, "active_debrid")]).toStrictEqual([
      { sortBy: "size", sortOrder: "desc" },
      { sortBy: "mod_time", sortOrder: "desc" },
      { sortBy: "active_debrid", sortOrder: "asc" },
    ]);
  });

  it("ignores an unknown column", () => {
    expect(nextSort(createBrowseState(), "evil")).toBeUndefined();
  });

  it("marks the sorted column's direction", () => {
    const state = { ...createBrowseState(), sortOrder: "desc" as const };
    expect([sortIndicator(state, "name"), sortIndicator(state, "size")]).toStrictEqual([
      "bi bi-sort-down text-xs",
      "bi bi-arrow-down-up text-xs",
    ]);
  });
});

describe("entries", () => {
  it("are actionable unless they are library, provider or virtual folders", () => {
    expect(
      [
        entry({ kind: "system" }),
        entry({ kind: "provider" }),
        entry({ kind: "virtual" }),
        entry({ kind: "provider", canDelete: true, isDir: false }),
        entry({ kind: "entry" }),
        entry({ kind: "file", isDir: false }),
        entry({ canDelete: true }),
        entry({}),
      ].map((each) => isActionable(each)),
    ).toStrictEqual([false, false, false, false, true, true, true, false]);
  });

  it("are identified by hash, else by path", () => {
    expect([entryId(entry({ infoHash: "abc" })), entryId(entry({}))]).toStrictEqual(["abc", "/__all__/Show"]);
  });

  it("download through their folder and file name", () => {
    expect([downloadURL("/__all__/A B/f#1.mkv"), downloadURL("/__all__/f.mkv")]).toStrictEqual([
      "/api/browse/download/A%20B/f%231.mkv",
      undefined,
    ]);
  });
});

describe("the health badge", () => {
  it("is unknown without a state", () => {
    expect(healthBadge()).toStrictEqual({ className: "badge-ghost", text: "unknown", title: undefined });
  });

  it("colours the status and dates the last check", () => {
    expect([
      healthBadge({ status: "broken", lastCheckedAt: "" }),
      healthBadge({ status: "", lastCheckedAt: "2024-01-01T00:00:00Z" }).text,
    ]).toStrictEqual([{ className: "badge-error", text: "broken", title: "never checked" }, "unknown"]);
  });
});

describe("the breadcrumb trail", () => {
  it("lists each level with its decoded name", () => {
    expect(breadcrumbs("/__all__/A%20B")).toStrictEqual([
      { path: "/__all__", label: "__all__" },
      { path: "/__all__/A%20B", label: "A B" },
    ]);
  });
});
