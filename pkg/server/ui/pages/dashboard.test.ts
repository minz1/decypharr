import { describe, expect, it, vi } from "vitest";

import {
  XSS,
  bootDashboard,
  cellText,
  element,
  page,
  toastText,
  twoTorrents,
  waitForRows,
  waitForToast,
} from "#ui/lib/dashboard/testing";

const hangs = async (): Promise<Response> => Promise.withResolvers<Response>().promise;

describe("the dashboard's rows", () => {
  it("render the queue, with names as text", async () => {
    await bootDashboard(twoTorrents);
    await waitForRows(2);
    expect({
      img: document.querySelectorAll("#torrentsList img").length,
      cells: ["name", "hash", "size", "percent", "speed", "state", "protocol", "seeds"].map((name) =>
        cellText(0, name),
      ),
      category: cellText(1, "category"),
    }).toStrictEqual({
      img: 0,
      cells: [XSS, "aaaaaaaa...", "1 GB", "50%", "2 KB/s", "Downloading", "Torrent", "0"],
      category: "radarr",
    });
  });

  it("fill the category filter and the page summary", async () => {
    await bootDashboard(twoTorrents);
    await waitForRows(2);
    expect({
      info: element("#paginationInfo", HTMLElement).textContent,
      categories: [...element("#categoryFilter", HTMLSelectElement).options].map((option) => option.value),
    }).toStrictEqual({ info: "Showing 1-2 of 2 items", categories: ["", "sonarr", "radarr"] });
  });

  it("give way to the empty state when the queue is empty", async () => {
    await bootDashboard(() => page([]));
    await vi.waitFor(() => {
      expect(element("#paginationInfo", HTMLElement).textContent).toBe("No items found");
    });
    expect({
      empty: element("#emptyState", HTMLElement).classList.contains("hidden"),
      table: element("#torrentsList", HTMLElement).closest(".card")?.classList.contains("hidden"),
    }).toStrictEqual({ empty: false, table: true });
  });
});

describe("loading the dashboard", () => {
  it("asks under the URL base", async () => {
    const requests = await bootDashboard(twoTorrents, { fixture: "index@base" });
    await waitForRows(2);
    expect(requests.map((request) => request.url)).toStrictEqual([
      "/decypharr/api/torrents?page=1&limit=20&sort_by=added_on&sort_order=desc",
    ]);
  });

  it("reports a failed load as a toast and re-enables refresh", async () => {
    await bootDashboard(() => new Response("queue unavailable", { status: 500 }));
    await waitForToast();
    expect({ toasts: toastText(), refresh: element("#refreshBtn", HTMLButtonElement).disabled }).toStrictEqual({
      toasts: ["Error loading items: Failed to fetch items"],
      refresh: false,
    });
  });

  it("reports a malformed response as a toast", async () => {
    await bootDashboard(() => new Response("{not json", { status: 200 }));
    await waitForToast();
    expect(toastText()).toStrictEqual(["Error loading items: response is not JSON"]);
  });

  it("names a missing element in a toast", async () => {
    await bootDashboard(twoTorrents, {
      prepare: () => {
        document.querySelector("#refreshBtn")?.remove();
      },
    });
    await waitForToast();
    expect(toastText()).toStrictEqual(["dashboard: missing #refreshBtn"]);
  });
});

describe("refreshing the dashboard", () => {
  it("keeps refresh disabled while a load is running", async () => {
    const requests = await bootDashboard(hangs);
    const refresh = element("#refreshBtn", HTMLButtonElement);
    refresh.click();
    refresh.click();
    expect({ disabled: refresh.disabled, requests: requests.length }).toStrictEqual({ disabled: true, requests: 1 });
  });

  it("reloads when refresh is clicked", async () => {
    const requests = await bootDashboard(twoTorrents);
    await waitForRows(2);
    element("#refreshBtn", HTMLButtonElement).click();
    expect(requests).toHaveLength(2);
  });

  it("polls every ten seconds", async () => {
    const requests = await bootDashboard(twoTorrents);
    await waitForRows(2);
    const loaded = requests.length;
    await vi.advanceTimersByTimeAsync(10_000);
    expect([loaded, requests.length]).toStrictEqual([1, 2]);
  });
});
