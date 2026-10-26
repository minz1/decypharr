import { describe, expect, it } from "vitest";

import { ShapeError } from "#ui/lib/common/json";
import { clientLabel, parseStreams, streamBadges, streamView } from "#ui/lib/stats/streams";

const NOW_MS = 1_800_000_000_000;

const parsedValue = (json: string): unknown => JSON.parse(json);

const stream = {
  id: "s1",
  entry_id: "e1",
  file_id: "f1",
  entry_name: "Show S01",
  file_name: "Show.S01E01.mkv",
  file_size: 1_610_612_736,
  source: "torrent",
  started_at: NOW_MS / 1000 - 125,
  last_active: NOW_MS / 1000,
  resumes: 0,
  debrid: "realdebrid",
  client: "VLC/3.0.18 LibVLC/3.0.18",
};

const firstView = (overrides: Record<string, unknown> = {}): ReturnType<typeof streamView> => {
  const [parsed] = parseStreams({ count: 1, streams: [{ ...stream, ...overrides }] }).streams;
  if (!parsed) {
    throw new Error("no stream parsed");
  }
  return streamView(parsed, NOW_MS);
};

describe("the stream count badges", () => {
  it("are green with streams and ghosts without", () => {
    expect(streamBadges(2)).toStrictEqual({
      count: "2",
      cardClass: "badge badge-success",
      tabClass: "badge badge-sm badge-success ml-2",
    });
    expect(streamBadges(0)).toStrictEqual({
      count: "0",
      cardClass: "badge badge-ghost",
      tabClass: "badge badge-sm badge-ghost ml-2",
    });
  });
});

describe("a stream row", () => {
  it("shows the source, debrid, client, names, size and duration", () => {
    expect(firstView()).toStrictEqual({
      sourceText: "Torrent",
      sourceClass: "badge-secondary",
      debrid: "realdebrid",
      client: { text: "VLC/3.0.18 LibVLC/3.0.18", title: "VLC/3.0.18 LibVLC/3.0.18" },
      entryName: "Show S01",
      fileName: "Show.S01E01.mkv",
      size: "1.5 GB",
      duration: "2m 5s",
    });
  });

  it("marks NZB streams and leaves out a missing debrid and client", () => {
    expect(firstView({ source: "nzb", debrid: "", client: "" })).toMatchObject({
      sourceText: "NZB",
      sourceClass: "badge-accent",
      debrid: "",
      client: undefined,
    });
  });
});

describe("a stream's client label", () => {
  it.each([
    ["DFS", "DFS"],
    ["VLC/3.0.18 LibVLC/3.0.18 (x64)", "VLC/3.0.18 LibVLC/3.0.18 (x64)"],
    ["Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36", "Mozilla/5.0"],
    ["Infuse-Direct-Playback-For-Apple-TV-Version-8", "Infuse-Direct-Playback-For-Apple-TV-Version-8"],
    [" leading space makes the app name pattern miss it", " leading space makes the app n..."],
  ])("labels %j as %j", (client, want) => {
    expect(clientLabel(client)).toBe(want);
  });
});

describe("the streams section", () => {
  it("reads a missing section or stream list as no streams", () => {
    expect(parseStreams()).toStrictEqual({ count: 0, streams: [] });
    expect(parseStreams({ count: 0, streams: parsedValue("null") })).toStrictEqual({ count: 0, streams: [] });
  });

  it("rejects a malformed stream", () => {
    expect(() => parseStreams({ count: 1, streams: [{ ...stream, file_size: "big" }] })).toThrow(ShapeError);
  });
});
