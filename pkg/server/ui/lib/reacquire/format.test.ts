import { describe, expect, it } from "vitest";

import {
  arrKind,
  bindingMedia,
  filterJobs,
  formatTime,
  isWorking,
  jobCounts,
  jobFile,
  jobsPage,
  pageControls,
  statusLine,
  statusMeta,
} from "#ui/lib/reacquire/format";
import type { Binding, Job, PageControl } from "#ui/lib/reacquire/types";

const job = (id: string, status: string, bindings: Binding[] = []): Job => ({
  id,
  status,
  cause: "",
  strategy: "",
  arrName: "",
  arrType: "",
  fileId: "",
  downloadId: "",
  replacementDownloadId: "",
  lastError: "",
  createdAt: "",
  updatedAt: "",
  completedAt: "",
  bindings,
  mutations: [],
});

const binding = (fields: Partial<Binding>): Binding => ({
  arrName: "",
  arrType: "",
  entryId: "",
  entryName: "",
  entryFileId: "",
  entryFileName: "",
  arrFileId: 0,
  seriesId: 0,
  seasonNumber: undefined,
  movieId: 0,
  confidence: "",
  ...fields,
});

const controlLabel = (control: PageControl): string => (control.kind === "page" ? String(control.page) : control.kind);

const NOW = Date.parse("2026-06-01T12:00:00Z");

describe("a job status", () => {
  it.each([
    ["searching", { label: "Searching", badge: "badge-info", icon: "bi-binoculars" }],
    ["waiting_for_download", { label: "Downloading", badge: "badge-warning", icon: "bi-download" }],
    ["mystery", { label: "mystery", badge: "badge-ghost", icon: "bi-question" }],
    ["toString", { label: "toString", badge: "badge-ghost", icon: "bi-question" }],
  ])("%s is shown as %j", (status, meta) => {
    expect(statusMeta(status)).toStrictEqual(meta);
  });

  it.each([
    ["queued", true],
    ["waiting_for_import", true],
    ["needs_attention", false],
    ["ready", false],
    ["failed", false],
    ["cancelled", false],
  ])("%s keeps polling: %s", (status, working) => {
    expect(isWorking(job("j", status))).toBe(working);
  });
});

describe("the job counts", () => {
  const jobs = [
    job("a", "queued"),
    job("b", "searching"),
    job("c", "waiting_for_grab"),
    job("d", "ready"),
    job("e", "odd"),
  ];

  it("groups active and waiting statuses", () => {
    expect(jobCounts(jobs)).toStrictEqual({
      active: 2,
      waiting: 1,
      ready: 1,
      needs_attention: 0,
      failed: 0,
      cancelled: 0,
    });
  });

  it.each([
    [jobs, "3 jobs in progress."],
    [[job("a", "queued")], "1 job in progress."],
    [[job("a", "ready")], "No job is running."],
    [[], "No reacquisitions yet."],
  ])("says %#", (list, line) => {
    expect(statusLine(list)).toBe(line);
  });

  it("filters by group", () => {
    expect(filterJobs(jobs, "active").map((item) => item.id)).toStrictEqual(["a", "b"]);
  });
});

describe("paging jobs", () => {
  const jobs = Array.from({ length: 23 }, (_unused, index) => job(String(index), "ready"));

  it("clamps the page and slices it", () => {
    const page = jobsPage(jobs, 9, 10);
    expect({ ...page, jobs: page.jobs.map((item) => item.id) }).toStrictEqual({
      jobs: ["20", "21", "22"],
      total: 23,
      totalPages: 3,
      offset: 20,
      page: 3,
    });
  });

  it("stays on page 1 without jobs", () => {
    expect(jobsPage([], 0, 10)).toStrictEqual({ jobs: [], total: 0, totalPages: 1, offset: 0, page: 1 });
  });

  it("shows the ends, two pages either side and gaps", () => {
    expect(pageControls(6, 12).map((control) => controlLabel(control))).toStrictEqual([
      "previous",
      "1",
      "gap",
      "4",
      "5",
      "6",
      "7",
      "8",
      "gap",
      "12",
      "next",
    ]);
  });

  it("disables the arrows at the edges and marks the current page", () => {
    expect(pageControls(1, 2)).toStrictEqual([
      { kind: "previous", page: 0, current: false, disabled: true },
      { kind: "page", page: 1, current: true, disabled: false },
      { kind: "page", page: 2, current: false, disabled: false },
      { kind: "next", page: 2, current: false, disabled: false },
    ]);
  });
});

describe("the relative time", () => {
  it.each([
    ["", "-"],
    ["nonsense", "-"],
    ["0001-01-01T00:00:00Z", "-"],
    ["2026-06-01T11:59:30Z", "just now"],
    ["2026-06-01T11:15:00Z", "45m ago"],
    ["2026-06-01T07:00:00Z", "5h ago"],
  ])("of %j is %j", (value, text) => {
    expect(formatTime(value, NOW)).toBe(text);
  });

  it("is the local time after a day", () => {
    expect(formatTime("2026-05-01T00:00:00Z", NOW)).toBe(new Date("2026-05-01T00:00:00Z").toLocaleString());
  });
});

describe("the labels", () => {
  it.each([
    ["sonarr", "sonarr", false],
    ["Sonarr 4K", "sonarr", true],
    ["SONARR", "sonarr", false],
    ["x", "", false],
  ])("the arr %j of type %j adds its kind: %s", (name, type, shown) => {
    expect(arrKind(name, type)).toBe(shown);
  });

  it.each([
    [binding({ movieId: 5 }), "movie 5"],
    [binding({ seriesId: 3, seasonNumber: 2 }), "series 3 · season 2"],
    [binding({}), "series - · season -"],
  ])("media of %# is %j", (value, media) => {
    expect(bindingMedia(value)).toBe(media);
  });

  it.each([
    [job("a", "ready", [binding({ entryFileName: "e.mkv" })]), "e.mkv"],
    [{ ...job("a", "ready"), fileId: "f9" }, "f9"],
    [job("a", "ready"), "-"],
  ])("file of %# is %j", (value, file) => {
    expect(jobFile(value)).toBe(file);
  });
});
