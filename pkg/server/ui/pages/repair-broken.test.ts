import { describe, expect, it, onTestFinished, vi } from "vitest";

import { click, mutations, stubReload } from "#ui/lib/testing/api";
import { answerConfirm } from "#ui/lib/testing/confirm";
import { element } from "#ui/lib/testing/dom";
import { bootRepair, brokenEntry } from "#ui/lib/testing/repair";
import { toasts } from "#ui/lib/testing/toast";
import { XSS } from "#ui/lib/testing/xss";

type Routes = Parameters<typeof bootRepair>[0];

const openBroken = async (entries: unknown[], routes: Routes = {}): Promise<Awaited<ReturnType<typeof bootRepair>>> => {
  const requests = await bootRepair({
    "GET /api/repair/health?status=broken": () => Response.json(entries),
    ...routes,
  });
  await click("#viewBrokenBtn");
  return requests;
};

const visibleNames = (): (string | undefined)[] =>
  [...document.querySelectorAll("#brokenTableBody > tr:not(.hidden)")].map((row) => row.children[1]?.textContent);

const pageLabels = (): string[] =>
  [...document.querySelectorAll("#brokenPaginationControls button")].map((button) => button.textContent);

const text = (selector: string): string => element(selector, HTMLElement).textContent;

describe("the broken entries modal", () => {
  it("loads the broken entries and renders names as text", async () => {
    const requests = await openBroken([brokenEntry(XSS, "2026-01-01T00:00:00Z"), brokenEntry("a.show")]);
    expect({
      url: requests.at(-1)?.url,
      open: element("#brokenModal", HTMLDialogElement).open,
      names: visibleNames(),
      labels: [...document.querySelectorAll("#brokenTableBody [data-action='recheck-entry']")].map((button) =>
        button.getAttribute("aria-label"),
      ),
      images: document.querySelectorAll("#brokenModal img").length,
      counts: [text("#brokenModalCount"), text("#brokenPaginationInfo")],
      pages: pageLabels(),
    }).toStrictEqual({
      url: "/api/repair/health?status=broken",
      open: true,
      names: [XSS, "a.show"],
      labels: [`Recheck ${XSS}`, "Recheck a.show"],
      images: 0,
      counts: ["2", "Showing 1-2 of 2"],
      pages: [],
    });
  });

  it("expands a row to its broken files", async () => {
    await openBroken([brokenEntry("a.show")]);
    await click("#brokenTableBody > tr td:nth-child(2)");
    const detail = element("#brokenTableBody > tr:nth-child(2)", HTMLElement);
    expect({
      hidden: detail.classList.contains("hidden"),
      cells: [...detail.querySelectorAll("tbody td")].map((cell) => cell.textContent.trim()),
      caret: element("#brokenTableBody i.bi-chevron-right", HTMLElement).style.transform,
    }).toStrictEqual({
      hidden: false,
      cells: ["a.show.mkv", "404", "1.5 KB", "sonarr", "media:4"],
      caret: "rotate(90deg)",
    });
  });

  it("shows the empty message", async () => {
    await openBroken([]);
    expect(
      [element("#noBrokenMessage", HTMLElement), element("#brokenPaginationBar", HTMLElement)].map((part) =>
        part.classList.contains("hidden"),
      ),
    ).toStrictEqual([false, true]);
  });
});

describe("paging broken entries", () => {
  it("pages through more than 25 entries", async () => {
    await openBroken(
      Array.from({ length: 30 }, (_unused, index) => brokenEntry(`entry-${String(index).padStart(2, "0")}`)),
    );
    const firstPage = pageLabels();
    await click('#brokenPaginationControls button[data-page="2"]');
    expect({
      firstPage,
      info: text("#brokenPaginationInfo"),
      first: visibleNames()[0],
      active: document.querySelector("#brokenPaginationControls .btn-active")?.textContent,
      next: document.querySelector("#brokenPaginationControls button:last-child")?.classList.contains("btn-disabled"),
    }).toStrictEqual({
      firstPage: ["«", "1", "2", "»"],
      info: "Showing 26-30 of 30",
      first: "entry-25",
      active: "2",
      next: true,
    });
  });
});

describe("acting on one broken entry", () => {
  it("rechecks it and refreshes shortly after", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    onTestFinished(() => {
      vi.useRealTimers();
    });
    const requests = await openBroken([brokenEntry("a/b c")], {
      "POST /api/repair/health/a%2Fb%20c/check": () => new Response("", { status: 202 }),
    });
    await click("#brokenTableBody [data-action='recheck-entry']");
    const before = requests.length;
    await vi.advanceTimersByTimeAsync(800);
    expect({
      posted: mutations(requests),
      toasts: toasts("success"),
      detailHidden: element("#brokenTableBody > tr:nth-child(2)", HTMLElement).classList.contains("hidden"),
      refreshed: requests
        .slice(before)
        .map((request) => request.url)
        .toSorted(),
    }).toStrictEqual({
      posted: [{ url: "/api/repair/health/a%2Fb%20c/check", method: "POST", body: "" }],
      toasts: ["Recheck started for a/b c"],
      detailHidden: true,
      refreshed: ["/api/repair/health?status=broken", "/api/repair/status"],
    });
  });

  it("fixes it after asking", async () => {
    const requests = await openBroken([brokenEntry(XSS)], { "POST /api/repair/fix": () => Response.json({}) });
    const reloads = stubReload();
    await click("#brokenTableBody [data-action='fix-entry']");
    const asked = await answerConfirm("ok");
    expect({ asked, posted: mutations(requests), reloads: reloads.count, toasts: toasts("success") }).toStrictEqual({
      asked: `Send delete + re-search for "${XSS}" to its Arr?`,
      posted: [{ url: "/api/repair/fix", method: "POST", body: JSON.stringify({ names: [XSS] }) }],
      reloads: 1,
      toasts: [`Fix started for ${XSS}`],
    });
  });
});
