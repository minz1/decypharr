import { describe, expect, it } from "vitest";

import { click, mutations } from "#ui/lib/testing/api";
import { answerConfirm } from "#ui/lib/testing/confirm";
import { element, settle } from "#ui/lib/testing/dom";
import { bootReacquire, job } from "#ui/lib/testing/reacquire";
import { toasts } from "#ui/lib/testing/toast";

const MANY = Array.from({ length: 25 }, (_unused, index) => job(`j${String(index).padStart(2, "0")}`, "ready"));

const select = (selector: string, value: string): void => {
  const field = element(selector, HTMLSelectElement);
  field.value = value;
  field.dispatchEvent(new Event("change", { bubbles: true }));
};

const tick = async (selector: string): Promise<void> => {
  const box = element(selector, HTMLInputElement);
  box.checked = !box.checked;
  box.dispatchEvent(new Event("change", { bubbles: true }));
  await settle();
};

const firstFile = (): string | undefined =>
  document.querySelector("#jobsTableBody tr td:nth-child(3) div")?.textContent ?? undefined;

const pageLabels = (): (string | null)[] =>
  [...document.querySelectorAll("#jobsPaginationControls button")].map(
    (button) => button.getAttribute("aria-label") ?? button.textContent,
  );

const deleteBoth = async (answer: "cancel" | "ok"): Promise<string> => {
  await tick("#selectAllJobs");
  await click("#deleteSelectedJobsBtn");
  return answerConfirm(answer);
};

describe("paging jobs", () => {
  it("shows ten jobs a page and moves between pages", async () => {
    await bootReacquire({ "GET /api/arr/reacquire/jobs": () => Response.json(MANY) });
    const first = pageLabels();
    await click('#jobsPaginationControls button[data-page="3"]');
    expect({
      first,
      info: element("#jobsPaginationInfo", HTMLElement).textContent,
      file: firstFile(),
      current: document.querySelector('#jobsPaginationControls [aria-current="page"]')?.textContent,
      next: element('#jobsPaginationControls button[aria-label="Next jobs page"]', HTMLButtonElement).disabled,
    }).toStrictEqual({
      first: ["Previous jobs page", "Jobs page 1", "Jobs page 2", "Jobs page 3", "Next jobs page"],
      info: "Showing 21-25 of 25 jobs",
      file: "j20.mkv",
      current: "3",
      next: true,
    });
  });

  it("changes the page size and the filter", async () => {
    await bootReacquire({ "GET /api/arr/reacquire/jobs": () => Response.json([...MANY, job("w", "queued")]) });
    await click('#jobsPaginationControls button[data-page="2"]');
    select("#jobPageSize", "25");
    const sized = element("#jobsPaginationInfo", HTMLElement).textContent;
    select("#jobStatusFilter", "active");
    const active = element("#jobsPaginationInfo", HTMLElement).textContent;
    select("#jobStatusFilter", "failed");
    expect({ sized, active, empty: element("#jobsTableBody", HTMLElement).textContent.trim() }).toStrictEqual({
      sized: "Showing 1-25 of 26 jobs",
      active: "Showing 1-1 of 1 matching jobs",
      empty: "No jobs match this status.",
    });
  });
});

describe("filtering jobs", () => {
  it("returns to the first page", async () => {
    await bootReacquire({ "GET /api/arr/reacquire/jobs": () => Response.json(MANY) });
    await click('#jobsPaginationControls button[data-page="3"]');
    select("#jobStatusFilter", "ready");
    expect(element("#jobsPaginationInfo", HTMLElement).textContent).toBe("Showing 1-10 of 25 matching jobs");
  });
});

describe("selecting jobs", () => {
  it("selects completed jobs and clears the selection", async () => {
    await bootReacquire({
      "GET /api/arr/reacquire/jobs": () => Response.json([job("a", "ready"), job("b", "failed")]),
    });
    await tick("#jobsTableBody .job-checkbox");
    const one = {
      bar: element("#jobBulkActions", HTMLElement).classList.contains("hidden"),
      count: element("#selectedJobsCount", HTMLElement).textContent,
      indeterminate: element("#selectAllJobs", HTMLInputElement).indeterminate,
    };
    await tick("#selectAllJobs");
    const all = element("#selectedJobsCount", HTMLElement).textContent;
    await click("#clearJobSelectionBtn");
    expect({ one, all, after: element("#jobBulkActions", HTMLElement).classList.contains("hidden") }).toStrictEqual({
      one: { bar: false, count: "1", indeterminate: true },
      all: "2",
      after: true,
    });
  });
});

describe("deleting selected jobs", () => {
  it("asks first and does nothing when declined", async () => {
    const requests = await bootReacquire({
      "GET /api/arr/reacquire/jobs": () => Response.json([job("a", "ready"), job("b", "failed")]),
    });
    const asked = await deleteBoth("cancel");
    await click("#refreshJobsBtn");
    expect({ asked, posted: mutations(requests) }).toStrictEqual({
      asked: "Permanently delete 2 selected jobs?\n\nOnly the job history will be removed. This cannot be undone.",
      posted: [],
    });
  });

  it("deletes them and reloads the jobs", async () => {
    const requests = await bootReacquire({
      "GET /api/arr/reacquire/jobs": () => Response.json([job("a", "ready"), job("b", "failed")]),
      "DELETE /api/arr/reacquire/jobs": () => Response.json({ deleted: 2 }),
    });
    await deleteBoth("ok");
    expect({ posted: mutations(requests), toasts: toasts("success"), last: requests.at(-1)?.url }).toStrictEqual({
      posted: [{ url: "/api/arr/reacquire/jobs", method: "DELETE", body: '{"ids":["a","b"]}' }],
      toasts: ["Deleted 2 jobs"],
      last: "/api/arr/reacquire/jobs",
    });
  });

  it("reports a refused delete", async () => {
    await bootReacquire({
      "GET /api/arr/reacquire/jobs": () => Response.json([job("a", "ready"), job("b", "failed")]),
      "DELETE /api/arr/reacquire/jobs": () => Response.json({ error: "store is read-only" }, { status: 500 }),
    });
    await deleteBoth("ok");
    expect(toasts("error")).toStrictEqual(["Could not delete jobs: store is read-only"]);
  });
});
