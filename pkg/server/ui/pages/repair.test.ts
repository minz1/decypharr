import { describe, expect, it, onTestFinished, vi } from "vitest";

import { sequence, stubAPI } from "#ui/lib/testing/api";
import { element } from "#ui/lib/testing/dom";
import { loadFixture } from "#ui/lib/testing/fixture";
import { IDLE_STATUS, bootRepair } from "#ui/lib/testing/repair";
import { toasts } from "#ui/lib/testing/toast";
import { XSS } from "#ui/lib/testing/xss";

const LOADS = ["GET /api/arrs", "GET /api/repair/config", "GET /api/repair/runs", "GET /api/repair/status"];

const ACTIVE_STATUS = {
  enabled: false,
  health_counts: {},
  active_run: { id: "run-1", stage: "", started_at: "2026-01-02T03:00:00Z", stats: { probed: 7 } },
};

const FAILED_RUN = {
  trigger: XSS,
  status: "failed",
  started_at: "2026-01-01T00:00:00Z",
  completed_at: "2026-01-01T00:01:05Z",
  stats: { probed: 3, broken: 2 },
  error: XSS,
};

const running = (): Response => Response.json({ enabled: true, active_run: { id: "r", stats: {} }, health_counts: {} });

const text = (selector: string): string => element(selector, HTMLElement).textContent;

const hidden = (selector: string): boolean => element(selector, HTMLElement).classList.contains("hidden");

const disabled = (selector: string): boolean => element(selector, HTMLButtonElement).disabled;

const useFakeTimers = (): void => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
  onTestFinished(() => {
    vi.useRealTimers();
  });
};

describe("loading the repair page", () => {
  it("reads config, status, history and Arrs from the API", async () => {
    const requests = await bootRepair();
    expect(requests.map((request) => `${request.method} ${request.url}`).toSorted()).toStrictEqual(LOADS);
  });

  it("reads under the URL base", async () => {
    const requests = await bootRepair({}, "repair@base");
    expect(requests.map((request) => request.url).toSorted()).toStrictEqual(
      LOADS.map((load) => load.replace("GET /", "/decypharr/")),
    );
  });

  it("lists the Arrs as text", async () => {
    await bootRepair();
    expect({
      arrs: [...element("#recheckArr", HTMLSelectElement).options].map((option) => option.textContent),
      images: document.querySelectorAll("#recheckArr img").length,
    }).toStrictEqual({ arrs: ["Auto-detect", "sonarr", XSS], images: 0 });
  });
});

describe("the repair status", () => {
  it("shows an idle status with its counts", async () => {
    await bootRepair();
    expect({
      line: text("#repairStatusLine"),
      badge: text("#brokenCountBadge"),
      badgeHidden: hidden("#brokenCountBadge"),
      cards: [...document.querySelectorAll("#healthCountsGrid .stat")].map((card) =>
        card.textContent.replaceAll(/\s+/gu, ""),
      ),
      brokenColour: document.querySelectorAll("#healthCountsGrid .stat-value")[1]?.className,
      buttons: ["#fixBrokenBtn", "#stopRunBtn", "#runNowBtn"].map((selector) => disabled(selector)),
      panelHidden: hidden("#activeRunPanel"),
    }).toStrictEqual({
      line: `Repair enabled · next scheduled run: ${new Date("2026-01-02T03:04:05Z").toLocaleString()}`,
      badge: "1",
      badgeHidden: false,
      cards: ["healthy4", "broken1", "repairing0", "stale0", "unknown0", "unsupported0"],
      brokenColour: "stat-value text-lg text-error",
      buttons: [false, true, false],
      panelHidden: true,
    });
  });

  it("shows an active run with its stats", async () => {
    await bootRepair({ "GET /api/repair/status": () => Response.json(ACTIVE_STATUS) });
    expect({
      line: text("#repairStatusLine"),
      header: ["#activeRunStage", "#activeRunIdText"].map((selector) => text(selector)),
      stats: text("#activeRunStats").replaceAll(/\s+/gu, ""),
      panelHidden: hidden("#activeRunPanel"),
      buttons: ["#stopRunBtn", "#runNowBtn", "#clearStateBtn", "#viewBrokenBtn"].map((selector) => disabled(selector)),
    }).toStrictEqual({
      line: 'Repair is disabled. Enable it in Settings → Repair, or click "Run now" for a one-off check.',
      header: ["running", "run-1"],
      stats: "Candidates0Skipped0Probed7Healthy0Broken0Repaired0Cleared0Repairfail0",
      panelHidden: false,
      buttons: [false, true, true, true],
    });
  });
});

describe("the run history", () => {
  it("renders each run as text", async () => {
    await bootRepair({ "GET /api/repair/runs": () => Response.json([FAILED_RUN]) });
    const cells = [...document.querySelectorAll("#runsTableBody td")];
    expect({
      cells: cells.map((cell) => cell.textContent.trim()),
      classes: [...document.querySelectorAll("#runsTableBody .badge"), cells[4], cells[5]].map(
        (cell) => cell?.className,
      ),
      images: document.querySelectorAll("#runsTableBody img").length,
      emptyHidden: hidden("#noRunsMessage"),
    }).toStrictEqual({
      cells: [new Date("2026-01-01T00:00:00Z").toLocaleString(), XSS, "failed", "3", "2", "0", "0", "1m 5s", XSS],
      classes: ["badge badge-error", "text-error font-medium", ""],
      images: 0,
      emptyHidden: true,
    });
  });

  it("says when there are no runs", async () => {
    await bootRepair();
    expect({ hidden: hidden("#noRunsMessage") }).toStrictEqual({ hidden: false });
  });
});

describe("a failing repair API", () => {
  it("reports a failing status request as an error toast", async () => {
    await bootRepair({ "GET /api/repair/status": () => new Response("repair offline", { status: 500 }) });
    expect(toasts("error")).toStrictEqual(["Failed to load status: repair offline"]);
  });

  it("reports malformed history as an error toast", async () => {
    await bootRepair({ "GET /api/repair/runs": () => new Response("<html>", { status: 200 }) });
    expect(toasts("error")).toStrictEqual(["Failed to load history: response is not JSON"]);
  });

  it("reports a missing element by its id", async () => {
    vi.resetModules();
    loadFixture("repair");
    element("#runNowBtn", HTMLElement).remove();
    stubAPI({});
    await import("#ui/pages/repair");
    await vi.waitFor(() => {
      expect(toasts("error")).toStrictEqual(["repair: missing #runNowBtn"]);
    });
  });
});

describe("status polling", () => {
  it("polls every 2 s during a run and reloads the history when it ends", async () => {
    useFakeTimers();
    const requests = await bootRepair({
      "GET /api/repair/status": sequence(running, running, () => Response.json(IDLE_STATUS)),
    });
    const statusCount = (): number => requests.filter((request) => request.url === "/api/repair/status").length;
    await vi.advanceTimersByTimeAsync(2000);
    const afterOnePoll = statusCount();
    await vi.advanceTimersByTimeAsync(2000);
    expect({ afterOnePoll, last: requests.slice(-2).map((request) => request.url) }).toStrictEqual({
      afterOnePoll: 2,
      last: ["/api/repair/status", "/api/repair/runs"],
    });
  });

  it("polls every 15 s while idle", async () => {
    useFakeTimers();
    const requests = await bootRepair();
    const statusCount = (): number => requests.filter((request) => request.url === "/api/repair/status").length;
    await vi.advanceTimersByTimeAsync(14_000);
    const early = statusCount();
    await vi.advanceTimersByTimeAsync(1000);
    expect([early, statusCount()]).toStrictEqual([1, 2]);
  });
});
