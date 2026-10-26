import { describe, expect, it } from "vitest";

import {
  bindingsEndpoint,
  deleteJobsRequest,
  deletedCount,
  errorDetail,
  parseBindings,
  parseIndex,
  parseJobs,
  reacquireRequest,
} from "#ui/lib/reacquire/api";

const parsed = (text: string): unknown => JSON.parse(text);

const BINDING = {
  arrName: "sonarr",
  arrType: "sonarr",
  entryId: "e1",
  entryName: "Show.S01",
  entryFileId: "f1",
  entryFileName: "e1.mkv",
  arrFileId: 9,
  seriesId: 3,
  seasonNumber: 1,
  movieId: 0,
  confidence: "exact_path",
};

const RAW_JOB = {
  id: "j1",
  status: "ready",
  cause: "manual",
  strategy: "history_failed",
  arrName: "sonarr",
  arrType: "sonarr",
  entryId: "e1",
  fileId: "f1",
  downloadId: "d1",
  bindings: [BINDING],
  mutations: [{ key: "k", kind: "delete_file", state: "confirmed", attempts: 2, confirmedAt: "c" }],
  createdAt: "a",
  updatedAt: "b",
};

describe("the jobs", () => {
  it("reads a job with its bindings and mutations", () => {
    const [job] = parseJobs(parsed(JSON.stringify([RAW_JOB])));
    expect(job).toStrictEqual({
      id: "j1",
      status: "ready",
      cause: "manual",
      strategy: "history_failed",
      arrName: "sonarr",
      arrType: "sonarr",
      fileId: "f1",
      downloadId: "d1",
      replacementDownloadId: "",
      lastError: "",
      createdAt: "a",
      updatedAt: "b",
      completedAt: "",
      bindings: [BINDING],
      mutations: [{ kind: "delete_file", state: "confirmed", attempts: 2, confirmedAt: "c" }],
    });
  });

  it("reads null bindings, null jobs and an omitted season", () => {
    expect([
      parseJobs(parsed('[{"id":"j","bindings":null}]'))[0]?.bindings,
      parseJobs(parsed("null")),
      parseBindings(parsed('[{"entryId":"e"}]'))[0]?.seasonNumber,
    ]).toStrictEqual([[], [], undefined]);
  });

  it("rejects a job list that is not an array", () => {
    expect(() => parseJobs(parsed('{"error":"down"}'))).toThrow("jobs: expected an array");
  });
});

describe("the index", () => {
  it("reads each Arr's summary", () => {
    expect(
      parseIndex(
        parsed('[{"arrName":"radarr","arrType":"radarr","bindings":4,"actionable":3,"generation":2,"updatedAt":"u"}]'),
      ),
    ).toStrictEqual([{ arrName: "radarr", arrType: "radarr", bindings: 4, actionable: 3, updatedAt: "u" }]);
  });

  it("rejects a count of the wrong type", () => {
    expect(() => parseIndex(parsed('[{"bindings":"4"}]'))).toThrow("bindings: expected a number");
  });
});

describe("the requests", () => {
  it.each([
    ["", "", "/api/arr/bindings?"],
    ["The Show & co", "", "/api/arr/bindings?q=The+Show+%26+co"],
    ["", "sonarr", "/api/arr/bindings?arr=sonarr"],
    ["x", "radarr", "/api/arr/bindings?q=x&arr=radarr"],
  ])("searches %j in %j at %s", (query, arrName, endpoint) => {
    expect(bindingsEndpoint(query, arrName)).toBe(endpoint);
  });

  it("deletes jobs by id", () => {
    expect(deleteJobsRequest(["a", "b"])).toStrictEqual({
      method: "DELETE",
      headers: { "Content-Type": "application/json" },
      body: '{"ids":["a","b"]}',
    });
  });

  it.each([
    ["command_search", '{"entryId":"e1","fileId":"f1","cause":"manual","strategy":"command_search"}'],
    ["", '{"entryId":"e1","fileId":"f1","cause":"manual","strategy":"history_failed"}'],
  ])("reacquires the chosen file with strategy %j", (strategy, body) => {
    expect(reacquireRequest(BINDING, strategy)).toStrictEqual({
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body,
    });
  });
});

describe("reading a response loosely", () => {
  it.each([
    ['{"error":"job not found"}', "job not found"],
    ['{"message":"x"}', "HTTP 409"],
    ["not json", "HTTP 409"],
    ["[1]", "HTTP 409"],
  ])("reads the error of %j as %j", (body, message) => {
    expect(errorDetail(body, 409)).toBe(message);
  });

  it.each([
    ['{"deleted":1}', 1],
    ["{}", 3],
    ["", 3],
  ])("reads the deleted count of %j as %d", (body, deleted) => {
    expect(deletedCount(body, 3)).toBe(deleted);
  });
});
