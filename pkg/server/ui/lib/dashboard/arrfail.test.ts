import { describe, expect, it } from "vitest";

import { arrFailText, parseTorrentPage } from "#ui/lib/dashboard/model";

const torrent = {
  info_hash: "abcdef0123456789",
  name: "Show.S01",
  size: 2048,
  progress: 0.5,
  phase: "failed",
  label: "Failed",
};

describe("the Arr fail intent", () => {
  const retry = "2026-11-20T10:00:00Z";

  it("reads the arr_fail object, and leaves the field off when the API omits it", () => {
    const [shown, bare] = parseTorrentPage({
      torrents: [
        {
          ...torrent,
          arr_fail: {
            state: "pending",
            arr: "sonarr",
            waiting: "other_job_active",
            last_error: "boom",
            next_retry: retry,
          },
        },
        torrent,
      ],
    }).torrents;
    expect(shown?.arrFail).toStrictEqual({
      state: "pending",
      arr: "sonarr",
      waiting: "other_job_active",
      lastError: "boom",
      nextRetry: retry,
    });
    expect(bare).not.toHaveProperty("arrFail");
  });
});

describe("the Arr fail text", () => {
  const retry = "2026-11-20T10:00:00Z";

  it("says what is happening to each intent", () => {
    expect([
      arrFailText({ state: "pending", arr: "sonarr" }),
      arrFailText({ state: "pending", arr: "sonarr", waiting: "arr_not_registered" }),
      arrFailText({ state: "pending", arr: "sonarr", waiting: "other_job_active" }),
      arrFailText({ state: "pending", arr: "sonarr", lastError: "boom", nextRetry: retry }),
      arrFailText({ state: "queued", arr: "sonarr" }),
      arrFailText({ state: "queued", arr: "sonarr", jobStatus: "blocklisting" }),
      arrFailText({ state: "skipped", arr: "sonarr" }),
    ]).toStrictEqual([
      "telling sonarr",
      "telling sonarr (waiting for sonarr to register)",
      "telling sonarr (waiting for another job)",
      `telling sonarr (boom; retry ${new Date(retry).toLocaleTimeString()})`,
      "fail job queued",
      "fail job queued (blocklisting)",
      "not told",
    ]);
  });

  it("shows nothing when there is no intent", () => {
    expect(arrFailText(parseTorrentPage({ torrents: [torrent] }).torrents[0]?.arrFail)).toBe("");
  });
});
