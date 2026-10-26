import { describe, expect, it, vi } from "vitest";

import {
  XSS,
  answerConfirm,
  bootBrowse,
  browseCalls,
  element,
  library,
  listing,
  item,
  openFolder,
  rowControl,
  stubOpen,
  toastText,
  waitForName,
  waitForToast,
} from "#ui/lib/browse/testing";

const twoPages = (): Response => listing([item("One", "a")], { total: 25, total_pages: 2 });

const recheckFails = (url: string, method: string): Response =>
  url.endsWith("/check") ? new Response("checker offline", { status: 503 }) : library(url, method);

const setValue = (selector: string, value: string, eventType: string): void => {
  const field = element(selector, HTMLElement);
  if (field instanceof HTMLInputElement || field instanceof HTMLSelectElement) {
    field.value = value;
  }
  field.dispatchEvent(new Event(eventType));
};

const crumbs = (): string[] =>
  [...document.querySelectorAll("#breadcrumbNav a")].map(
    (link) => `${link.textContent.trim()} ${link.getAttribute("href") ?? ""}`,
  );

describe("folder navigation", () => {
  it("opens a folder, records it in the address and the breadcrumbs", async () => {
    const requests = await bootBrowse(library);
    await waitForName("Kids");
    await openFolder(0, "Show");
    expect({ calls: browseCalls(requests), address: globalThis.location.search, crumbs: crumbs() }).toStrictEqual({
      calls: [
        "GET /api/browse/?page=1&limit=20&sort_by=name&sort_order=asc",
        "GET /api/browse/__all__?page=1&limit=20&sort_by=name&sort_order=asc",
      ],
      address: "?path=%2F__all__",
      crumbs: ["Home /browse", "__all__ /browse?path=%2F__all__"],
    });
  });

  it("goes back up through a breadcrumb", async () => {
    const requests = await bootBrowse(library, { location: "/browse?path=%2F__all__%2FShow" });
    await waitForName("a.mkv");
    const followed = element('#breadcrumbNav a[data-path="/__all__"]', HTMLAnchorElement).dispatchEvent(
      new MouseEvent("click", { bubbles: true, cancelable: true }),
    );
    await waitForName("Show");
    expect({ followed, call: browseCalls(requests).at(-1) }).toStrictEqual({
      followed: false,
      call: "GET /api/browse/__all__?page=1&limit=20&sort_by=name&sort_order=asc",
    });
  });

  it("follows the browser's back button", async () => {
    const requests = await bootBrowse(library, { location: "/browse?path=%2F__all__" });
    await waitForName("Show");
    globalThis.history.replaceState({}, "", "/browse");
    globalThis.dispatchEvent(new PopStateEvent("popstate"));
    await waitForName("Kids");
    expect(browseCalls(requests).at(-1)).toBe("GET /api/browse/?page=1&limit=20&sort_by=name&sort_order=asc");
  });
});

describe("files", () => {
  it("download through the URL base when opened", async () => {
    await bootBrowse(library, { fixture: "browse@base", location: "/decypharr/browse?path=%2F__all__%2FShow" });
    await waitForName("a.mkv");
    const opened = stubOpen();
    rowControl(0, '[data-action="open"]', HTMLButtonElement).click();
    expect(opened).toStrictEqual(["_blank /decypharr/api/browse/download/Show/a.mkv"]);
  });
});

describe("sorting and paging", () => {
  it("sorts by a header, size starting with the largest", async () => {
    const requests = await bootBrowse(library);
    await waitForName("Kids");
    element('[data-sort-key="size"]', HTMLButtonElement).click();
    expect({
      call: browseCalls(requests).at(-1),
      address: globalThis.location.search,
      indicator: element("#sortIndicator-size", HTMLElement).className,
    }).toStrictEqual({
      call: "GET /api/browse/?page=1&limit=20&sort_by=size&sort_order=desc",
      address: "?sort_by=size&sort_order=desc",
      indicator: "bi bi-sort-down text-xs",
    });
  });

  it("goes to the page clicked", async () => {
    const requests = await bootBrowse(twoPages);
    await waitForName("One");
    const next = [...element("#paginationControls", HTMLElement).querySelectorAll("button")].at(-1);
    next?.click();
    expect(browseCalls(requests).at(-1)).toBe("GET /api/browse/?page=2&limit=20&sort_by=name&sort_order=asc");
  });

  it("searches after typing stops and changes the page size", async () => {
    const requests = await bootBrowse(library);
    await waitForName("Kids");
    setValue("#searchInput", "kid", "input");
    await vi.advanceTimersByTimeAsync(299);
    const early = browseCalls(requests).length;
    await vi.advanceTimersByTimeAsync(1);
    setValue("#pageSizeSelect", "50", "change");
    expect({ early, calls: browseCalls(requests).slice(1) }).toStrictEqual({
      early: 1,
      calls: [
        "GET /api/browse/?page=1&limit=20&sort_by=name&sort_order=asc&search=kid",
        "GET /api/browse/?page=1&limit=50&sort_by=name&sort_order=asc&search=kid",
      ],
    });
  });
});

describe("an entry's actions", () => {
  it("delete it after asking", async () => {
    const requests = await bootBrowse(library, { location: "/browse?path=%2F__all__" });
    await waitForName(XSS);
    rowControl(0, '[data-action="delete"]', HTMLButtonElement).click();
    const asked = await answerConfirm("ok");
    await waitForToast();
    expect({ asked, call: browseCalls(requests)[1], toasts: toastText() }).toStrictEqual({
      asked: `Permanently delete the original item "${XSS}"?\n\nThis removes it from every library and virtual folder, and removes its placement from the provider. This cannot be undone from Decypharr.`,
      call: "DELETE /api/browse/torrents/abc",
      toasts: ["Item deleted successfully"],
    });
  });

  it("recheck its health", async () => {
    const requests = await bootBrowse(library, { location: "/browse?path=%2F__all__" });
    await waitForName("Show");
    rowControl(1, '[data-action="recheck"]', HTMLButtonElement).click();
    await vi.waitFor(() => {
      expect(toastText()).toHaveLength(2);
    });
    expect({
      call: requests.filter((request) => request.method === "POST").map((request) => request.url),
      toasts: toastText(),
      warning: document.querySelectorAll(".toast-container .alert-warning").length,
      badge: rowControl(1, '[data-field="health"] span', HTMLElement).textContent,
    }).toStrictEqual({
      call: ["/api/repair/health/Show/check"],
      toasts: ["Rechecking Show…", "Health: broken"],
      warning: 1,
      badge: "broken",
    });
  });

  it("report a failed recheck", async () => {
    await bootBrowse(recheckFails, { location: "/browse?path=%2F__all__" });
    await waitForName("Show");
    rowControl(1, '[data-action="recheck"]', HTMLButtonElement).click();
    await vi.waitFor(() => {
      expect(toastText()).toHaveLength(2);
    });
    expect(toastText()[1]).toBe("Recheck failed: checker offline");
  });
});
