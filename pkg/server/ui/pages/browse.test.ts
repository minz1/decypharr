import { describe, expect, it, vi } from "vitest";

import {
  XSS,
  bootBrowse,
  browseCalls,
  element,
  library,
  listing,
  rowControl,
  rowText,
  rows,
  toastText,
  waitForName,
  waitForToast,
} from "#ui/lib/browse/testing";

const slowFolder =
  (first: Promise<Response>) =>
  async (url: string, method: string): Promise<Response> =>
    url.includes("__all__") ? first : library(url, method);

const emptyVirtual = (): Response => listing([], { current_kind: "virtual" });

describe("the browse root", () => {
  it("lists library and virtual folders without actions", async () => {
    await bootBrowse(library);
    await waitForName("Kids");
    expect({
      rows: [rowText(0), rowText(1)],
      disabled: rows().map((row) => row.querySelector<HTMLInputElement>(".entry-checkbox")?.disabled),
      selectAll: element("#selectAllCheckbox", HTMLInputElement).disabled,
      info: element("#paginationInfo", HTMLElement).textContent,
    }).toStrictEqual({
      rows: ["Select __all__ __all__ Library - - - — —", "Select Kids Kids Virtual view - - - — —"],
      disabled: [true, true],
      selectAll: true,
      info: "Showing 1-2 of 2 items",
    });
  });

  it("asks under the URL base", async () => {
    const requests = await bootBrowse(library, { fixture: "browse@base", location: "/decypharr/browse" });
    await waitForName("Kids");
    expect(browseCalls(requests)).toStrictEqual([
      "GET /decypharr/api/browse/?page=1&limit=20&sort_by=name&sort_order=asc",
    ]);
  });
});

describe("a library folder", () => {
  it("renders names as text and asks for each entry's health", async () => {
    const requests = await bootBrowse(library, { location: "/browse?path=%2F__all__" });
    await waitForName(XSS);
    await vi.waitFor(() => {
      expect(rowControl(0, '[data-field="health"] span', HTMLElement).textContent).toBe("healthy");
    });
    expect({
      img: document.querySelectorAll("#fileBrowserList img").length,
      row: rowText(0),
      label: rowControl(0, '[data-field="menu-button"]', HTMLElement).getAttribute("aria-label"),
      health: requests.filter((request) => request.url.includes("health")).map((request) => request.url),
    }).toStrictEqual({
      img: 0,
      row: `Select ${XSS} ${XSS} - 2024-01-01 10:00 realdebrid healthy Recheck health Delete original item`,
      label: `Actions for ${XSS}`,
      health: [`/api/repair/health/${encodeURIComponent(XSS)}`, "/api/repair/health/Show"],
    });
  });

  it("reads page, search, size and sort from the address", async () => {
    const requests = await bootBrowse(library, {
      location: "/browse?path=%2F__all__&page=2&search=x&limit=50&sort_by=size&sort_order=desc",
    });
    await waitForName("Show");
    expect({
      calls: browseCalls(requests),
      search: element("#searchInput", HTMLInputElement).value,
      size: element("#pageSizeSelect", HTMLSelectElement).value,
      indicator: element("#sortIndicator-size", HTMLElement).className,
    }).toStrictEqual({
      calls: ["GET /api/browse/__all__?page=2&limit=50&sort_by=size&sort_order=desc&search=x"],
      search: "x",
      size: "50",
      indicator: "bi bi-sort-down text-xs",
    });
  });
});

describe("loading a folder", () => {
  it("reports a failed load as a toast", async () => {
    await bootBrowse(() => new Response("broken", { status: 500 }));
    await waitForToast();
    expect(toastText()).toStrictEqual(["Failed to load directory"]);
  });

  it("reports a malformed response as a toast", async () => {
    await bootBrowse(() => new Response("{not json"));
    await waitForToast();
    expect(toastText()).toStrictEqual(["Failed to load directory"]);
  });

  it("names a missing element in a toast", async () => {
    await bootBrowse(library, {
      prepare: () => {
        document.querySelector("#fileBrowserList")?.remove();
      },
    });
    await waitForToast();
    expect(toastText()).toStrictEqual(["browse: missing #fileBrowserList"]);
  });

  it("explains an empty virtual folder", async () => {
    await bootBrowse(emptyVirtual);
    await vi.waitFor(() => {
      expect(element("#emptyState", HTMLElement).classList).not.toContain("hidden");
    });
    expect({
      notice: element("#currentViewNotice", HTMLElement).classList.contains("hidden"),
      message: element("#emptyStateMessage", HTMLElement).textContent,
      link: element("#emptyStateVirtualFolderLink", HTMLElement).classList.contains("hidden"),
      info: element("#paginationInfo", HTMLElement).textContent,
    }).toStrictEqual({
      notice: false,
      message: "No current items match this virtual folder’s conditions.",
      link: false,
      info: "No items",
    });
  });
});

describe("overlapping loads", () => {
  it("show only the latest folder", async () => {
    const first = Promise.withResolvers<Response>();
    await bootBrowse(slowFolder(first.promise), { location: "/browse?path=%2F__all__" });
    globalThis.history.replaceState({}, "", "/browse");
    globalThis.dispatchEvent(new PopStateEvent("popstate"));
    await waitForName("Kids");
    first.resolve(library("/api/browse/__all__", "GET"));
    await vi.advanceTimersByTimeAsync(1);
    expect(rowText(1)).toBe("Select Kids Kids Virtual view - - - — —");
  });
});
