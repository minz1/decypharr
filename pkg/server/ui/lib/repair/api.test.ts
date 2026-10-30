import { describe, expect, it } from "vitest";

import {
  clearStateRequest,
  clearedMessage,
  failureMessage,
  fixAllRequest,
  fixOneRequest,
  parseArrNames,
  parseEntries,
  parseRepairConfig,
  parseRepairStatus,
  parseRuns,
  recheckMediaRequest,
  runRequest,
} from "#ui/lib/repair/api";

const parsed = (text: string): unknown => JSON.parse(text);

const ZERO_STATS = {
  candidates: 0,
  skippedFresh: 0,
  probed: 0,
  healthy: 0,
  broken: 0,
  repaired: 0,
  cleared: 0,
  repairFailed: 0,
};

describe("the repair config", () => {
  it("reads the run modal defaults", () => {
    expect(parseRepairConfig(parsed('{"auto_repair":true,"verify_content":true,"interval":"1h"}'))).toStrictEqual({
      autoRepair: true,
      verifyContent: true,
    });
  });

  it("treats omitted flags and a null body as off", () => {
    expect([parseRepairConfig(parsed("{}")), parseRepairConfig(parsed("null"))]).toStrictEqual([
      { autoRepair: false, verifyContent: false },
      { autoRepair: false, verifyContent: false },
    ]);
  });

  it("rejects a flag of the wrong type", () => {
    expect(() => parseRepairConfig(parsed('{"auto_repair":"yes"}'))).toThrow("auto_repair: expected a boolean");
  });
});

describe("the repair status", () => {
  it("reads an idle status", () => {
    expect(parseRepairStatus(parsed('{"enabled":false,"health_counts":{"broken":2,"healthy":5}}'))).toStrictEqual({
      enabled: false,
      nextRunAt: undefined,
      activeRun: undefined,
      healthCounts: { broken: 2, healthy: 5 },
    });
  });

  it("reads an active run with its stats", () => {
    const status = parseRepairStatus(
      parsed(
        '{"enabled":true,"next_run_at":"2026-01-02T03:04:05Z","health_counts":null,' +
          '"active_run":{"id":"r1","stage":"probing","started_at":"2026-01-02T03:00:00Z",' +
          '"stats":{"candidates":9,"skipped_fresh":1,"probed":4,"healthy":3,"broken":1,"repair_failed":2}}}',
      ),
    );
    expect(status).toStrictEqual({
      enabled: true,
      nextRunAt: "2026-01-02T03:04:05Z",
      healthCounts: {},
      activeRun: {
        id: "r1",
        stage: "probing",
        startedAt: "2026-01-02T03:00:00Z",
        stats: { ...ZERO_STATS, candidates: 9, skippedFresh: 1, probed: 4, healthy: 3, broken: 1, repairFailed: 2 },
      },
    });
  });

  it("rejects a count that is not a number", () => {
    expect(() => parseRepairStatus(parsed('{"health_counts":{"broken":"2"}}'))).toThrow("broken: expected a number");
  });
});

describe("the run history", () => {
  it("reads each run, defaulting what is missing", () => {
    expect(
      parseRuns(
        parsed(
          '[{"id":"a","trigger":"manual","status":"completed","started_at":"s","completed_at":"c",' +
            '"stats":{"probed":3},"error":"boom"},{}]',
        ),
      ),
    ).toStrictEqual([
      {
        trigger: "manual",
        status: "completed",
        startedAt: "s",
        completedAt: "c",
        stats: { ...ZERO_STATS, probed: 3 },
        error: "boom",
      },
      { trigger: "", status: "", startedAt: "", completedAt: "", stats: ZERO_STATS, error: "" },
    ]);
  });

  it("reads null as no runs", () => {
    expect(parseRuns(parsed("null"))).toStrictEqual([]);
  });

  it("rejects an object", () => {
    expect(() => parseRuns(parsed("{}"))).toThrow("runs: expected an array");
  });
});

describe("a broken entry", () => {
  it("reads an entry and its files", () => {
    expect(
      parseEntries(
        parsed(
          '[{"entry_name":"Show.S01","protocol":"torrent","status":"broken","file_count":4,"broken_count":2,' +
            '"failure_reason":"missing","last_checked_at":"lc","last_failed_at":"lf","last_repair_at":"lr",' +
            '"broken_files":[{"file_name":"e1.mkv","reason":"404","size":2048,"arr_name":"sonarr",' +
            '"arr_kind":"sonarr","media_id":7,"episode_id":8,"arr_file_id":9}]}]',
        ),
      ),
    ).toStrictEqual([
      {
        entryName: "Show.S01",
        protocol: "torrent",
        fileCount: 4,
        brokenCount: 2,
        failureReason: "missing",
        lastCheckedAt: "lc",
        lastFailedAt: "lf",
        lastRepairAt: "lr",
        brokenFiles: [
          {
            fileName: "e1.mkv",
            reason: "404",
            size: 2048,
            arrName: "sonarr",
            arrKind: "sonarr",
            mediaId: 7,
            episodeId: 8,
            arrFileId: 9,
          },
        ],
      },
    ]);
  });
});

describe("an entry without details", () => {
  it("counts the broken files when broken_count is missing", () => {
    const [entry] = parseEntries(parsed('[{"entry_name":"a","broken_files":[{},{}]}]'));
    expect(entry).toStrictEqual({
      entryName: "a",
      protocol: "",
      fileCount: 0,
      brokenCount: 2,
      failureReason: "",
      lastCheckedAt: "",
      lastFailedAt: "",
      lastRepairAt: "",
      brokenFiles: [
        { fileName: "", reason: "", size: 0, arrName: "", arrKind: "", mediaId: 0, episodeId: 0, arrFileId: 0 },
        { fileName: "", reason: "", size: 0, arrName: "", arrKind: "", mediaId: 0, episodeId: 0, arrFileId: 0 },
      ],
    });
  });

  it("reads null as no entries", () => {
    expect(parseEntries(parsed("null"))).toStrictEqual([]);
  });
});

describe("the Arr names", () => {
  it("keeps every Arr that has a name", () => {
    expect(parseArrNames(parsed('[{"name":"sonarr"},null,{"name":""},{"host":"x"},{"name":"radarr"}]'))).toStrictEqual([
      "sonarr",
      "radarr",
    ]);
  });

  it("reads null as no Arrs", () => {
    expect(parseArrNames(parsed("null"))).toStrictEqual([]);
  });
});

describe("the requests", () => {
  it("runs a sweep with the modal's options and link generation on", () => {
    expect(
      runRequest({ ignoreLastChecked: true, autoRepair: false, verifyContent: true, protocol: "nzb" }),
    ).toStrictEqual({
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: '{"ignore_last_checked":true,"auto_repair":false,"unrestrict_link":true,"verify_content":true,"protocol":"nzb"}',
    });
  });

  it("fixes every broken entry with an empty object", () => {
    expect(fixAllRequest()).toStrictEqual({
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
  });

  it("fixes one entry by name", () => {
    expect(fixOneRequest('Show "1"')).toStrictEqual({
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: String.raw`{"names":["Show \"1\""]}`,
    });
  });

  it("clears the chosen states", () => {
    expect(clearStateRequest(["broken", "stale"])).toStrictEqual({
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: '{"statuses":["broken","stale"]}',
    });
  });

  it("rechecks one media id", () => {
    expect(recheckMediaRequest({ arr: "", mediaId: "12", fix: true })).toStrictEqual({
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: '{"arr":"","media_id":"12","fix":true}',
    });
  });
});

describe("the failure message", () => {
  it.each([
    ['{"error":"no arr"}', "no arr"],
    ['{"message":"bad id"}', "bad id"],
    ['{"error":"","message":"bad id"}', "bad id"],
    ["plain text", "plain text"],
    ['{"other":1}', '{"other":1}'],
    ["", "HTTP 503"],
  ])("of %j is %j", (text, message) => {
    expect(failureMessage(text, 503)).toBe(message);
  });
});

describe("the cleared message", () => {
  it.each([
    ['{"cleared":1}', "Cleared 1 repair state entry"],
    ['{"cleared":3}', "Cleared 3 repair state entries"],
    ['{"cleared":0}', "Cleared 0 repair state entries"],
    ["", "Cleared 0 repair state entries"],
    ["not json", "Cleared 0 repair state entries"],
  ])("of %j is %j", (text, message) => {
    expect(clearedMessage(text)).toBe(message);
  });
});
