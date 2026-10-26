import { describe, expect, it } from "vitest";

import {
  bootDownload,
  dropEvent,
  element,
  giveFiles,
  input,
  names,
  select,
  submitTwice,
  textArea,
  waitForToasts,
} from "#ui/lib/testing/download-page";

const HIGHLIGHT = ["border-primary", "border-2", "border-dashed", "bg-primary/5"];

type Respond = () => Promise<Response>;

const reply =
  (body: string, status = 200): Respond =>
  async () =>
    Promise.resolve(new Response(body, { status }));

const added = (statuses: readonly string[]): Respond =>
  reply(JSON.stringify(statuses.map((status) => ({ status, error: status === "error" ? "<b>bad</b>" : undefined }))));

const submitMagnet = async (respond: Respond): Promise<string[]> => {
  await bootDownload({ respond });
  textArea("magnetURI").value = "magnet:?xt=1";
  element("submitDownload", HTMLButtonElement).click();
  const shown = await waitForToasts(1);
  expect(document.querySelector("#submitDownload:not([disabled])")).not.toBeNull();
  return shown;
};

describe("adding torrents", () => {
  it("posts the magnets, files and options once, then clears the inputs", async () => {
    const torrent = new File(["t"], "a.torrent");
    const requests = await bootDownload({ respond: added(["queued", "queued"]) });
    textArea("magnetURI").value = " magnet:?xt=1 \n\n";
    giveFiles("torrentFiles", [torrent]);
    input("arr").value = "sonarr";
    await submitTwice(requests);
    expect(requests).toStrictEqual([
      {
        url: "/api/add",
        method: "POST",
        fields: [
          ["urls", "magnet:?xt=1"],
          ["files", torrent],
          ["arr", "sonarr"],
          ["downloadFolder", "/downloads"],
          ["action", "symlink"],
          ["downloadUncached", "false"],
          ["rmTrackerUrls", "false"],
        ],
      },
    ]);
    expect([await waitForToasts(1), textArea("magnetURI").value]).toStrictEqual([
      ["alert-success: Successfully added 2 items!"],
      "",
    ]);
  });

  it("asks for something to add without posting", async () => {
    const requests = await bootDownload();
    element("submitDownload", HTMLButtonElement).click();
    expect([await waitForToasts(1), requests]).toStrictEqual([
      ["alert-warning: Please provide at least one torrent or NZB"],
      [],
    ]);
  });
});

describe("adding NZBs", () => {
  it("posts NZB URLs, NZB files and the chosen debrid under the URL base", async () => {
    const nzb = new File(["n"], "b.nzb");
    const requests = await bootDownload({ fixture: "download-options@base" });
    textArea("nzbURL").value = "https://x/1.nzb\n https://x/2.nzb ";
    giveFiles("nzbFile", [nzb]);
    select("debrid").value = "torbox";
    element("submitDownload", HTMLButtonElement).click();
    await waitForToasts(1);
    expect(
      requests.map((request) => [request.url, request.fields.filter(([name]) => name !== "rmTrackerUrls")]),
    ).toStrictEqual([
      [
        "/decypharr/api/add",
        [
          ["nzbURLs", "https://x/1.nzb\nhttps://x/2.nzb"],
          ["nzbFiles", nzb],
          ["arr", ""],
          ["downloadFolder", "/downloads"],
          ["action", "symlink"],
          ["downloadUncached", "false"],
          ["debrid", "torbox"],
        ],
      ],
    ]);
  });
});

describe("the server's answer", () => {
  it("warns about a partial success and lists the errors as text", async () => {
    const shown = await submitMagnet(added(["queued", "error"]));
    expect({ shown, bold: document.querySelectorAll(".toast-container b").length }).toStrictEqual({
      shown: [
        "alert-warning: Added 1 item(s) with 1 error(s)",
        "alert-error: Errors occurred while adding items:• <b>bad</b>",
      ],
      bold: 0,
    });
  });

  it.each([
    ["a text refusal", reply("upload too large", 400), "upload too large"],
    ["a JSON refusal", reply('{"error":"no arr"}', 400), "no arr"],
    ["a malformed answer", reply("<html>"), "response is not JSON"],
    ["an answer of the wrong shape", reply('{"ok":true}'), "response: expected an array"],
  ])("reports %s", async (_name, respond, reason) => {
    await expect(submitMagnet(respond)).resolves.toStrictEqual([`alert-error: Error adding downloads: ${reason}`]);
  });
});

describe("opening the page", () => {
  it("keeps tracker removal checked and locked when the config forces it", async () => {
    await bootDownload({ fixture: "download-options", stored: { rmTrackerUrls: "false" } });
    const box = input("rmTrackerUrls");
    expect([box.checked, box.disabled]).toStrictEqual([true, true]);
  });

  it("shows the configured download folder when none is saved", async () => {
    await bootDownload({ fixture: "download@base" });
    expect(input("downloadFolder").value).toBe("/downloads");
  });

  it("fills the form from the saved options and saves them again on change", async () => {
    await bootDownload({
      stored: { downloadCategory: "radarr", downloadAction: "none", downloadUncached: "true", downloadFolder: "/x" },
    });
    const loaded = [input("arr").value, select("downloadAction").value, input("downloadFolder").value];
    const uncached = element("downloadUncached", HTMLInputElement).checked;
    input("downloadFolder").value = "/y";
    input("downloadFolder").dispatchEvent(new Event("change", { bubbles: true }));
    expect([
      loaded,
      uncached,
      localStorage.getItem("downloadFolder"),
      localStorage.getItem("rmTrackerUrls"),
    ]).toStrictEqual([["radarr", "none", "/x"], true, "/y", "false"]);
  });

  it("fills the magnet field, says so and leaves the address clean", async () => {
    await bootDownload({ url: "/download?magnet=magnet%3A%3Fxt%3D1" });
    expect([textArea("magnetURI").value, await waitForToasts(1), location.search]).toStrictEqual([
      "magnet:?xt=1",
      ["alert-info: Magnet link loaded from URL"],
      "",
    ]);
  });
});

describe("choosing files", () => {
  it("lists the chosen file names as text", async () => {
    await bootDownload();
    giveFiles("torrentFiles", [new File(["t"], "<img src=x onerror=alert(1)>.torrent")]).dispatchEvent(
      new Event("change", { bubbles: true }),
    );
    expect([await waitForToasts(1), document.querySelectorAll(".toast-container img").length]).toStrictEqual([
      ["alert-info: Selected 1 torrent file: <img src=x onerror=alert(1)>.torrent"],
      0,
    ]);
  });

  it("takes dropped torrents and the first dropped NZB", async () => {
    await bootDownload();
    const torrents = giveFiles("torrentFiles", []);
    const nzbs = giveFiles("nzbFile", []);
    const drop = dropEvent([new File(["t"], "a.torrent"), new File(["n"], "b.nzb"), new File(["n"], "c.nzb")]);
    element("downloadForm", HTMLFormElement).dispatchEvent(drop);
    expect([names(torrents.files), names(nzbs.files), drop.defaultPrevented]).toStrictEqual([
      ["a.torrent"],
      ["b.nzb"],
      true,
    ]);
  });
});

describe("dragging over the form", () => {
  it("highlights the form while something is dragged over it", async () => {
    await bootDownload();
    const form = element("downloadForm", HTMLFormElement);
    const after = (name: string): string => {
      form.dispatchEvent(new Event(name, { bubbles: true, cancelable: true }));
      return `${name}:${HIGHLIGHT.filter((highlight) => form.classList.contains(highlight)).length}`;
    };
    expect(["dragenter", "dragleave", "dragover"].map((name) => after(name))).toStrictEqual([
      "dragenter:4",
      "dragleave:0",
      "dragover:4",
    ]);
  });

  it("drops the highlight and refuses other files", async () => {
    await bootDownload();
    const form = element("downloadForm", HTMLFormElement);
    form.dispatchEvent(new Event("dragover", { bubbles: true, cancelable: true }));
    form.dispatchEvent(dropEvent([new File(["o"], "e.txt")]));
    expect([HIGHLIGHT.filter((name) => form.classList.contains(name)), await waitForToasts(1)]).toStrictEqual([
      [],
      ["alert-warning: Please drop .torrent or .nzb files only"],
    ]);
  });
});

describe("a template out of step with the page", () => {
  it("names the missing element in an error toast", async () => {
    await bootDownload({
      prepare: () => {
        document.querySelector("#submitDownload")?.remove();
      },
    });
    await expect(waitForToasts(1)).resolves.toStrictEqual(["alert-error: download: missing #submitDownload"]);
  });
});
