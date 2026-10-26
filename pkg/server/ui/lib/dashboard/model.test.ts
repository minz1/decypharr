import { describe, expect, it } from "vitest";

import {
  createDashboardState,
  deleteSelectedURL,
  deleteTorrentURL,
  parseSort,
  parseTorrentPage,
  progressColor,
  protocolBadge,
  stateBadge,
  torrentSpeed,
  torrentsURL,
} from "#ui/lib/dashboard/model";

const torrent = {
  info_hash: "abcdef0123456789",
  name: "Show.S01",
  size: 2048,
  progress: 0.5,
  speed: 1024,
  category: "sonarr",
  protocol: "torrent",
  active_provider: "realdebrid",
  state: "downloading",
};

describe("the torrent page parser", () => {
  it("reads the page the queue API returns", () => {
    expect(
      parseTorrentPage({ torrents: [torrent], total: 41, total_pages: 3, categories: ["sonarr", "radarr"] }),
    ).toStrictEqual({
      torrents: [
        {
          infoHash: "abcdef0123456789",
          name: "Show.S01",
          size: 2048,
          progress: 0.5,
          speed: 1024,
          category: "sonarr",
          protocol: "torrent",
          activeProvider: "realdebrid",
          numSeeds: 0,
          state: "downloading",
        },
      ],
      total: 41,
      totalPages: 3,
      categories: ["sonarr", "radarr"],
    });
  });

  it("treats null lists and absent counts as empty", () => {
    expect(parseTorrentPage(JSON.parse('{"torrents":null,"categories":null}'))).toStrictEqual({
      torrents: [],
      total: 0,
      totalPages: 0,
      categories: [],
    });
  });

  it("falls back to dlspeed, and reads num_seeds when present", () => {
    const { speed: _speed, ...withoutSpeed } = torrent;
    const [parsed] = parseTorrentPage({ torrents: [{ ...withoutSpeed, dlspeed: 7, num_seeds: 4 }] }).torrents;
    expect({ speed: parsed?.speed, seeds: parsed?.numSeeds }).toStrictEqual({ speed: 7, seeds: 4 });
  });

  it("rejects a torrent without a hash", () => {
    expect(() => parseTorrentPage({ torrents: [{ name: "x" }] })).toThrow("info_hash: expected a string");
  });

  it("rejects a body that is not an object", () => {
    expect(() => parseTorrentPage([])).toThrow("torrents response: expected an object");
  });
});

describe("the sort selector parser", () => {
  it("reads the suffix as the order", () => {
    expect([parseSort("name_asc"), parseSort("size_desc"), parseSort("added_on")]).toStrictEqual([
      { sortBy: "name", sortOrder: "asc" },
      { sortBy: "size", sortOrder: "desc" },
      { sortBy: "added_on", sortOrder: "desc" },
    ]);
  });
});

describe("the torrents URL", () => {
  it("asks for the first page by date added, newest first", () => {
    expect(torrentsURL(createDashboardState())).toBe("/api/torrents?page=1&limit=20&sort_by=added_on&sort_order=desc");
  });

  it("adds search, category and state only when set", () => {
    const state = { ...createDashboardState(), page: 2, search: "a b&c", category: "sonarr", stateFilter: "error" };
    expect(torrentsURL(state)).toBe(
      "/api/torrents?page=2&limit=20&sort_by=added_on&sort_order=desc&search=a+b%26c&category=sonarr&state=error",
    );
  });
});

describe("delete URLs", () => {
  it("deletes one torrent under its category", () => {
    expect([deleteTorrentURL("abc", "sonarr", false), deleteTorrentURL("abc", "", true)]).toStrictEqual([
      "/api/torrents/sonarr/abc?removeFromDebrid=false",
      "/api/torrents//abc?removeFromDebrid=true",
    ]);
  });

  it("deletes the selection by comma-joined hashes", () => {
    expect(deleteSelectedURL(["a", "b"], true)).toBe("/api/torrents?hashes=a,b&removeFromDebrid=true");
  });
});

describe("badges", () => {
  it("colours progress by how far it got", () => {
    expect([progressColor(100), progressColor(10), progressColor(50), progressColor(80)]).toStrictEqual([
      "progress-success",
      "progress-error",
      "progress-warning",
      "progress-info",
    ]);
  });

  it("names known states and passes unknown ones through", () => {
    expect([stateBadge("pausedUP"), stateBadge("stalledDL")]).toStrictEqual([
      { className: "badge-success", text: "Completed" },
      { className: "badge-ghost", text: "stalledDL" },
    ]);
  });

  it("names the protocol, with Unknown for none", () => {
    expect([protocolBadge("nzb"), protocolBadge("")]).toStrictEqual([
      { className: "badge-secondary", icon: "bi-newspaper", text: "Usenet" },
      { className: "badge-ghost", icon: "bi-question-circle", text: "Unknown" },
    ]);
  });

  it("shows a dash for no speed", () => {
    expect([torrentSpeed(0), torrentSpeed(2048)]).toStrictEqual(["-", "2 KB/s"]);
  });
});
