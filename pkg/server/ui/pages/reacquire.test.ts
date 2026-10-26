import { describe, expect, it, onTestFinished, vi } from "vitest";

import { element, stubAPI, toasts } from "#ui/lib/testing/api";
import { loadFixture } from "#ui/lib/testing/fixture";
import { XSS, bootReacquire, job } from "#ui/lib/testing/reacquire";

const text = (selector: string): string => element(selector, HTMLElement).textContent.trim();

const jobRows = (): string[][] =>
  [...document.querySelectorAll("#jobsTableBody tr")].map((row) =>
    [...row.querySelectorAll("td")].map((cell) => cell.textContent.replaceAll(/\s+/gu, " ").trim()),
  );

const useFakeTimers = (): void => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
  onTestFinished(() => {
    vi.useRealTimers();
  });
};

const JOBS = [
  job("a", "searching", { bindings: [{ entryFileName: XSS, entryName: XSS }], arrName: "Sonarr 4K", cause: XSS }),
  job("b", "ready"),
  job("c", "needs_attention"),
];

describe("loading the reacquire page", () => {
  it("reads the jobs and the index", async () => {
    const requests = await bootReacquire();
    expect(requests.map((request) => `${request.method} ${request.url}`).toSorted()).toStrictEqual([
      "GET /api/arr/index",
      "GET /api/arr/reacquire/jobs",
    ]);
  });

  it("reads under the URL base", async () => {
    const requests = await bootReacquire({}, "reacquire@base");
    expect(requests.map((request) => request.url).toSorted()).toStrictEqual([
      "/decypharr/api/arr/index",
      "/decypharr/api/arr/reacquire/jobs",
    ]);
  });

  it("says when there are no jobs", async () => {
    await bootReacquire();
    expect({ line: text("#reacquireStatusLine"), rows: jobRows(), info: text("#jobsPaginationInfo") }).toStrictEqual({
      line: "No reacquisitions yet.",
      rows: [["No jobs to show. A stream or repair failure queues one automatically."]],
      info: "No jobs",
    });
  });
});

describe("the jobs table", () => {
  it("renders each job as text", async () => {
    await bootReacquire({ "GET /api/arr/reacquire/jobs": () => Response.json(JOBS) });
    expect({
      rows: jobRows(),
      images: document.querySelectorAll("#jobsTableBody img").length,
      spinning: document.querySelectorAll("#jobsTableBody .animate-spin").length,
      disabled: [...document.querySelectorAll<HTMLInputElement>("#jobsTableBody .job-checkbox")].map(
        (box) => box.disabled,
      ),
      title: document.querySelector("#jobsTableBody .job-checkbox")?.getAttribute("title"),
      label: document.querySelector("#jobsTableBody .job-detail-btn")?.getAttribute("aria-label"),
    }).toStrictEqual({
      rows: [
        ["", "Searching", `${XSS} ${XSS}`, "Sonarr 4K sonarr", XSS, "-", ""],
        ["", "Ready", "b.mkv Show b", "sonarr", "stream", "-", ""],
        ["", "Needs attention", "c.mkv Show c", "sonarr", "stream", "-", ""],
      ],
      images: 0,
      spinning: 1,
      disabled: [true, false, true],
      title: "Only completed jobs can be deleted",
      label: `View details for ${XSS}`,
    });
  });

  it("counts the jobs by group", async () => {
    await bootReacquire({ "GET /api/arr/reacquire/jobs": () => Response.json(JOBS) });
    expect({
      line: text("#reacquireStatusLine"),
      count: text("#jobsCount"),
      tiles: [...document.querySelectorAll("#jobCountsGrid > div")].map((tile) =>
        tile.textContent.replaceAll(/\s+/gu, ""),
      ),
      info: text("#jobsPaginationInfo"),
    }).toStrictEqual({
      line: "1 job in progress.",
      count: "3",
      tiles: ["Working1", "Waiting0", "Ready1", "Needsattention1", "Failed0", "Cancelled0"],
      info: "Showing 1-3 of 3 jobs",
    });
  });
});

describe("a failing reacquire API", () => {
  it("says the service is unavailable", async () => {
    await bootReacquire({
      "GET /api/arr/reacquire/jobs": () =>
        Response.json({ error: "Arr reacquisition service is not available" }, { status: 503 }),
    });
    expect({ line: text("#reacquireStatusLine"), rows: jobRows() }).toStrictEqual({
      line: "Reacquisition service is unavailable.",
      rows: [["No jobs to show. A stream or repair failure queues one automatically."]],
    });
  });

  it("says the index is unavailable when it is malformed", async () => {
    await bootReacquire({ "GET /api/arr/index": () => new Response("<html>", { status: 200 }) });
    expect(text("#indexSummary")).toBe("Index is unavailable.");
  });

  it("reports a missing element by its id", async () => {
    vi.resetModules();
    loadFixture("reacquire");
    element("#jobsTableBody", HTMLElement).remove();
    stubAPI({});
    await import("#ui/pages/reacquire");
    await vi.waitFor(() => {
      expect(toasts("error")).toStrictEqual(["reacquire: missing #jobsTableBody"]);
    });
  });
});

describe("polling jobs", () => {
  it("reloads every 5 s while a job is working", async () => {
    useFakeTimers();
    const requests = await bootReacquire({ "GET /api/arr/reacquire/jobs": () => Response.json(JOBS) });
    await vi.advanceTimersByTimeAsync(5000);
    expect(requests.filter((request) => request.url === "/api/arr/reacquire/jobs")).toHaveLength(2);
  });

  it("stops once every job is finished or needs attention", async () => {
    useFakeTimers();
    const requests = await bootReacquire({
      "GET /api/arr/reacquire/jobs": () => Response.json([job("b", "ready"), job("c", "needs_attention")]),
    });
    await vi.advanceTimersByTimeAsync(20_000);
    expect(requests.filter((request) => request.url === "/api/arr/reacquire/jobs")).toHaveLength(1);
  });
});
