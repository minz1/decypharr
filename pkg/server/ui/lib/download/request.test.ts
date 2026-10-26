import { describe, expect, it } from "vitest";

import { ShapeError } from "#ui/lib/common/json";
import {
  buildAddRequest,
  parseAddResults,
  partitionDrop,
  selectionMessage,
  summarizeResults,
} from "#ui/lib/download/request";

const torrent = new File(["t"], "a.torrent");
const nzb = new File(["n"], "b.nzb");

const request = {
  magnets: " magnet:?xt=1 \n\n  https://x/t.torrent \n",
  torrentFiles: [torrent],
  nzbURLs: "",
  nzbFiles: [nzb],
  arr: "sonarr",
  downloadFolder: "/downloads",
  action: "symlink",
  downloadUncached: false,
  rmTrackerUrls: true,
  debrid: undefined,
};

const entries = (outcome: ReturnType<typeof buildAddRequest>): [string, FormDataEntryValue][] =>
  "form" in outcome ? [...outcome.form.entries()] : [];

const lines = (count: number): string => Array.from({ length: count }, (_line, index) => `m${index}`).join("\n");

describe("building the add request", () => {
  it("sends trimmed lines, files and options in download.js's order", () => {
    expect(entries(buildAddRequest(request))).toStrictEqual([
      ["urls", "magnet:?xt=1\nhttps://x/t.torrent"],
      ["files", torrent],
      ["nzbFiles", nzb],
      ["arr", "sonarr"],
      ["downloadFolder", "/downloads"],
      ["action", "symlink"],
      ["downloadUncached", "false"],
      ["rmTrackerUrls", "true"],
    ]);
  });

  it("sends NZB URLs and the chosen debrid", () => {
    const outcome = buildAddRequest({
      ...request,
      magnets: "",
      torrentFiles: [],
      nzbFiles: [],
      nzbURLs: "https://x/1.nzb\n https://x/2.nzb",
      debrid: "torbox",
    });
    expect(entries(outcome)).toStrictEqual([
      ["nzbURLs", "https://x/1.nzb\nhttps://x/2.nzb"],
      ["arr", "sonarr"],
      ["downloadFolder", "/downloads"],
      ["action", "symlink"],
      ["downloadUncached", "false"],
      ["rmTrackerUrls", "true"],
      ["debrid", "torbox"],
    ]);
  });

  it("asks for at least one item", () => {
    expect(buildAddRequest({ ...request, magnets: " \n ", torrentFiles: [], nzbFiles: [] })).toStrictEqual({
      warning: "Please provide at least one torrent or NZB",
    });
  });

  it("caps a batch at 100 items", () => {
    expect([
      "form" in buildAddRequest({ ...request, magnets: lines(98) }),
      buildAddRequest({ ...request, magnets: lines(99) }),
    ]).toStrictEqual([true, { warning: "Please submit up to 100 items at a time" }]);
  });
});

describe("reading the add results", () => {
  it("reads each result's status and error", () => {
    expect(parseAddResults([{ status: "queued", name: "x" }, { status: "error", error: "bad" }, {}])).toStrictEqual([
      { status: "queued", error: undefined },
      { status: "error", error: "bad" },
      { status: undefined, error: undefined },
    ]);
  });

  it.each([{ error: "x" }, [1], [{ status: 1 }]])("rejects %o", (value) => {
    expect(() => parseAddResults(value)).toThrow(ShapeError);
  });
});

const ok = { status: "queued", error: undefined };

const failed = (error?: string): { status: string; error: string | undefined } => ({ status: "error", error });

describe("summarizing the add results", () => {
  it("celebrates and clears the form when everything was added", () => {
    expect([summarizeResults([ok]), summarizeResults([ok, ok])]).toStrictEqual([
      { toasts: [{ message: "Successfully added 1 item!", type: "success" }], clear: true },
      { toasts: [{ message: "Successfully added 2 items!", type: "success" }], clear: true },
    ]);
  });

  it("warns about a partial success and lists the errors", () => {
    expect(summarizeResults([ok, failed("bad"), failed()])).toStrictEqual({
      toasts: [
        { message: "Added 1 item(s) with 2 error(s)", type: "warning" },
        { message: "Errors occurred while adding items:\n• bad\n• Unknown error", type: "error" },
      ],
      clear: false,
    });
  });

  it("lists the errors when nothing was added", () => {
    expect(summarizeResults([failed("bad")])).toStrictEqual({
      toasts: [{ message: "Errors occurred while adding items:\n• bad", type: "error" }],
      clear: false,
    });
  });

  it("warns when the server added nothing", () => {
    expect(summarizeResults([])).toStrictEqual({
      toasts: [{ message: "No items were added", type: "warning" }],
      clear: false,
    });
  });
});

describe("picking files", () => {
  it("describes a selection", () => {
    expect([selectionMessage("torrent", [torrent]), selectionMessage("nzb", [nzb, nzb])]).toStrictEqual([
      "Selected 1 torrent file: a.torrent",
      "Selected 2 NZB files: b.nzb, b.nzb",
    ]);
  });

  it("keeps dropped torrents and the first dropped NZB, by extension in any case", () => {
    const upper = new File(["t"], "C.TORRENT");
    const second = new File(["n"], "d.NZB");
    const other = new File(["o"], "e.txt");
    expect(partitionDrop([other, torrent, second, upper, nzb])).toStrictEqual({
      torrents: [torrent, upper],
      nzbs: [second],
    });
  });
});
