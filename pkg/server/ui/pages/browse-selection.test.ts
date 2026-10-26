import { describe, expect, it } from "vitest";

import {
  answerConfirm,
  bootBrowse,
  browseCalls,
  check,
  element,
  library,
  rowControl,
  stubOpen,
  toastText,
  waitForName,
  waitForToast,
} from "#ui/lib/browse/testing";

const bar = (): { hidden: boolean; count: string } => ({
  hidden: element("#bulkActionsBar", HTMLElement).classList.contains("hidden"),
  count: element("#selectedCount", HTMLElement).textContent,
});

const checkedBoxes = (): number => document.querySelectorAll<HTMLInputElement>(".entry-checkbox:checked").length;

const openMenu = (index: number): HTMLElement => {
  rowControl(index, "td", HTMLElement).dispatchEvent(
    new MouseEvent("contextmenu", { bubbles: true, cancelable: true }),
  );
  return element("#contextMenu", HTMLElement);
};

describe("selecting entries", () => {
  it("shows the bulk bar with the count", async () => {
    await bootBrowse(library, { location: "/browse?path=%2F__all__" });
    await waitForName("Show");
    check(rowControl(0, ".entry-checkbox", HTMLInputElement));
    expect({ bar: bar(), indeterminate: element("#selectAllCheckbox", HTMLInputElement).indeterminate }).toStrictEqual({
      bar: { hidden: false, count: "1" },
      indeterminate: true,
    });
  });

  it("selects all and clears", async () => {
    await bootBrowse(library, { location: "/browse?path=%2F__all__" });
    await waitForName("Show");
    check(element("#selectAllCheckbox", HTMLInputElement));
    const all = bar();
    element("#clearSelectionBtn", HTMLButtonElement).click();
    expect({ all, cleared: bar(), checked: checkedBoxes() }).toStrictEqual({
      all: { hidden: false, count: "2" },
      cleared: { hidden: true, count: "0" },
      checked: 0,
    });
  });
});

describe("bulk actions", () => {
  it("download every selected file", async () => {
    await bootBrowse(library, { location: "/browse?path=%2F__all__%2FShow" });
    await waitForName("a.mkv");
    const opened = stubOpen();
    check(element("#selectAllCheckbox", HTMLInputElement));
    element("#bulkDownloadBtn", HTMLButtonElement).click();
    expect({ opened, toasts: toastText() }).toStrictEqual({
      opened: ["_blank /api/browse/download/Show/a.mkv", "_blank /api/browse/download/Show/b.mkv"],
      toasts: ["Downloading 2 file(s)"],
    });
  });

  it("delete the selection once, however often it is clicked", async () => {
    const requests = await bootBrowse(library, { location: "/browse?path=%2F__all__" });
    await waitForName("Show");
    check(element("#selectAllCheckbox", HTMLInputElement));
    element("#bulkDeleteBtn", HTMLButtonElement).click();
    element("#bulkDeleteBtn", HTMLButtonElement).click();
    const asked = await answerConfirm("ok");
    await waitForToast();
    expect({
      asked,
      deletes: requests.filter((request) => request.method === "DELETE"),
      toasts: toastText(),
    }).toStrictEqual({
      asked:
        "Permanently delete 2 original item(s)?\n\n<img src=x onerror=alert(1)>\nShow\n\nThis removes them from every library and virtual folder, and removes their provider placements. This cannot be undone from Decypharr.",
      deletes: [{ url: "/api/browse/torrents/batch", method: "DELETE", body: '{"ids":["abc","def"]}' }],
      toasts: ["Deleted 2 item(s)"],
    });
  });

  it("warn when nothing selected can be downloaded", async () => {
    await bootBrowse(library, { location: "/browse?path=%2F__all__" });
    await waitForName("Show");
    check(element("#selectAllCheckbox", HTMLInputElement));
    element("#bulkDownloadBtn", HTMLButtonElement).click();
    expect(toastText()).toStrictEqual(["No files selected for download"]);
  });
});

describe("the context menu", () => {
  it("offers delete but not download for a folder entry", async () => {
    await bootBrowse(library, { location: "/browse?path=%2F__all__" });
    await waitForName("Show");
    const menu = openMenu(1);
    expect(
      ["#contextMenu", "#contextDownload", "#contextDelete"].map((id) =>
        element(id, HTMLElement).classList.contains("hidden"),
      ),
    ).toStrictEqual([false, true, false]);
    expect(menu.style.left).toBe("0px");
  });

  it("deletes the entry it was opened on", async () => {
    const requests = await bootBrowse(library, { location: "/browse?path=%2F__all__" });
    await waitForName("Show");
    openMenu(1);
    element("#contextDelete button", HTMLButtonElement).click();
    await answerConfirm("ok");
    await waitForToast();
    expect({
      calls: browseCalls(requests)[1],
      hidden: element("#contextMenu", HTMLElement).classList.contains("hidden"),
    }).toStrictEqual({
      calls: "DELETE /api/browse/torrents/def",
      hidden: true,
    });
  });

  it("downloads a file", async () => {
    await bootBrowse(library, { location: "/browse?path=%2F__all__%2FShow" });
    await waitForName("a.mkv");
    const opened = stubOpen();
    openMenu(0);
    const shown = !element("#contextDownload", HTMLElement).classList.contains("hidden");
    element("#contextDownload button", HTMLButtonElement).click();
    expect({ shown, opened }).toStrictEqual({ shown: true, opened: ["_blank /api/browse/download/Show/a.mkv"] });
  });
});
