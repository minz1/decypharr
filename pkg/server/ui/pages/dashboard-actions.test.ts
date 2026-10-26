import { describe, expect, it, vi } from "vitest";

import {
  XSS,
  answerConfirm,
  bootDashboard,
  change,
  check,
  element,
  lastURL,
  page,
  rowControl,
  toastText,
  torrent,
  twoTorrents,
  waitForRows,
  waitForToast,
} from "#ui/lib/testing/dashboard";

const threePages = (): Response => page([torrent("aaaaaaaaaaaa", "One"), torrent("bbbbbbbbbbbb", "Two")], 3);

const deletesOK = (url: string): Response => (url.includes("removeFromDebrid") ? new Response("") : twoTorrents());

const batchDeletesOK = (url: string): Response => (url.includes("hashes") ? new Response("") : twoTorrents());

const deletesFail = (url: string): Response =>
  url.includes("removeFromDebrid") ? new Response("no", { status: 500 }) : twoTorrents();

const openMenu = (): HTMLElement => {
  rowControl(0, "td", HTMLElement).dispatchEvent(new MouseEvent("contextmenu", { bubbles: true, cancelable: true }));
  return element("#torrentContextMenu", HTMLElement);
};

const stubClipboard = (): string[] => {
  const copied: string[] = [];
  const writeText = async (text: string): Promise<void> => {
    copied.push(text);
    return Promise.resolve();
  };
  vi.stubGlobal("navigator", { clipboard: { writeText } });
  return copied;
};

describe("the dashboard's pagination", () => {
  it("goes to the page clicked", async () => {
    const requests = await bootDashboard(threePages);
    await waitForRows(2);
    const buttons = [...element("#paginationControls", HTMLElement).querySelectorAll("button")];
    buttons[2]?.click();
    expect({ labels: buttons.map((button) => button.textContent.trim()), url: lastURL(requests) }).toStrictEqual({
      labels: ["«", "1", "2", "3", "»"],
      url: "/api/torrents?page=2&limit=20&sort_by=added_on&sort_order=desc",
    });
  });

  it("ignores previous on the first page", async () => {
    const requests = await bootDashboard(threePages);
    await waitForRows(2);
    element("#paginationControls button", HTMLButtonElement).click();
    expect(requests).toHaveLength(1);
  });
});

describe("the dashboard's filters", () => {
  it("filter by state, category and sort from the first page", async () => {
    const requests = await bootDashboard(twoTorrents);
    await waitForRows(2);
    change("#stateFilter", "error");
    change("#categoryFilter", "radarr");
    change("#sortSelector", "name_asc");
    expect(lastURL(requests)).toBe(
      "/api/torrents?page=1&limit=20&sort_by=name&sort_order=asc&category=radarr&state=error",
    );
  });

  it("search after typing stops", async () => {
    const requests = await bootDashboard(twoTorrents);
    await waitForRows(2);
    change("#searchInput", "mov");
    await vi.advanceTimersByTimeAsync(299);
    const early = requests.length;
    await vi.advanceTimersByTimeAsync(1);
    expect({ early, late: requests.length, url: lastURL(requests) }).toStrictEqual({
      early: 1,
      late: 2,
      url: "/api/torrents?page=1&limit=20&sort_by=added_on&sort_order=desc&search=mov",
    });
  });
});

describe("deleting one torrent", () => {
  it("asks, then deletes it under its category", async () => {
    const requests = await bootDashboard(deletesOK);
    await waitForRows(2);
    rowControl(1, '[data-action="delete-debrid"]', HTMLButtonElement).click();
    const asked = await answerConfirm("ok");
    await waitForToast();
    expect({ asked, request: requests[1], toasts: toastText() }).toStrictEqual({
      asked: "Are you sure you want to delete this torrent?",
      request: { url: "/api/torrents/radarr/bbbbbbbbbbbb?removeFromDebrid=true", method: "DELETE" },
      toasts: ["Item deleted successfully"],
    });
  });

  it("deletes nothing when the question is declined", async () => {
    const requests = await bootDashboard(twoTorrents);
    await waitForRows(2);
    rowControl(0, '[data-action="delete"]', HTMLButtonElement).click();
    await answerConfirm("cancel");
    await vi.advanceTimersByTimeAsync(1);
    expect(requests).toHaveLength(1);
  });

  it("reports a failed delete as a toast", async () => {
    await bootDashboard(deletesFail);
    await waitForRows(2);
    rowControl(0, '[data-action="delete"]', HTMLButtonElement).click();
    await answerConfirm("ok");
    await waitForToast();
    expect(toastText()).toStrictEqual(["Failed to delete entry"]);
  });
});

describe("the dashboard's selection", () => {
  it("shows the batch buttons for one selected row", async () => {
    await bootDashboard(twoTorrents);
    await waitForRows(2);
    check(rowControl(0, ".torrent-select", HTMLInputElement));
    expect({
      batch: ["#batchDeleteBtn", "#batchDeleteDebridBtn"].map((id) =>
        element(id, HTMLButtonElement).classList.contains("hidden"),
      ),
      all: element("#selectAll", HTMLInputElement).checked,
    }).toStrictEqual({ batch: [false, false], all: false });
  });

  it("deletes everything selected", async () => {
    const requests = await bootDashboard(batchDeletesOK);
    await waitForRows(2);
    check(element("#selectAll", HTMLInputElement));
    element("#batchDeleteBtn", HTMLButtonElement).click();
    const asked = await answerConfirm("ok");
    await waitForToast();
    expect({ asked, request: requests[1], toasts: toastText() }).toStrictEqual({
      asked: "Delete 2 selected items?",
      request: { url: "/api/torrents?hashes=aaaaaaaaaaaa,bbbbbbbbbbbb&removeFromDebrid=false", method: "DELETE" },
      toasts: ["Deleted 2 items successfully"],
    });
  });
});

describe("the dashboard's context menu", () => {
  it("opens on a row with its name as text", async () => {
    await bootDashboard(twoTorrents);
    await waitForRows(2);
    const menu = openMenu();
    expect({
      shown: !menu.classList.contains("hidden"),
      title: menu.querySelector(".torrent-name")?.textContent,
    }).toStrictEqual({ shown: true, title: XSS });
  });

  it("copies the magnet link and closes", async () => {
    await bootDashboard(twoTorrents);
    await waitForRows(2);
    const copied = stubClipboard();
    const menu = openMenu();
    element('#torrentContextMenu [data-action="copy-magnet"]', HTMLElement).click();
    await waitForToast();
    expect({ copied, toasts: toastText(), hidden: menu.classList.contains("hidden") }).toStrictEqual({
      copied: ["magnet:?xt=urn:btih:aaaaaaaaaaaa"],
      toasts: ["Magnet link copied to clipboard"],
      hidden: true,
    });
  });
});

describe("deleting from the dashboard's context menu", () => {
  it("deletes the row's torrent", async () => {
    const requests = await bootDashboard(deletesOK);
    await waitForRows(2);
    openMenu();
    element('#torrentContextMenu [data-action="delete"]', HTMLElement).click();
    await answerConfirm("ok");
    await waitForToast();
    expect(requests[1]).toStrictEqual({
      url: "/api/torrents/sonarr/aaaaaaaaaaaa?removeFromDebrid=false",
      method: "DELETE",
    });
  });

  it("removes the row's torrent from the provider", async () => {
    const requests = await bootDashboard(deletesOK);
    await waitForRows(2);
    openMenu();
    element('#torrentContextMenu [data-action="delete-debrid"]', HTMLElement).click();
    await answerConfirm("ok");
    await waitForToast();
    expect(requests[1]).toStrictEqual({
      url: "/api/torrents/sonarr/aaaaaaaaaaaa?removeFromDebrid=true",
      method: "DELETE",
    });
  });
});
