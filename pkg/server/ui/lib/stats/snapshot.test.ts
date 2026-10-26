import { describe, expect, it } from "vitest";

import { ShapeError } from "#ui/lib/common/json";
import { parseSnapshot } from "#ui/lib/stats/snapshot";

describe("the stats snapshot", () => {
  it("reads every panel from one response", () => {
    const snapshot = parseSnapshot({
      system: { goroutines: 3 },
      debrids: [{ profile: { name: "realdebrid" }, library: {}, accounts: [] }],
      mount: { ready: false, enabled: false },
      access: { webdav: { enabled: true } },
      active_streams: { count: 0, streams: [] },
      storage: {},
      queue: {},
      arrs: { count: 0, names: [] },
      repair: {},
      hearsay: { enabled: false },
    });
    expect({
      goroutines: snapshot.overview.system.goroutines,
      debrids: snapshot.debrids.map((debrid) => debrid.name),
      mount: snapshot.mount.kind,
      usenet: snapshot.usenet,
      streams: snapshot.streams.count,
      webdav: snapshot.access.webdav.enabled,
    }).toStrictEqual({
      goroutines: 3,
      debrids: ["realdebrid"],
      mount: "None",
      usenet: undefined,
      streams: 0,
      webdav: true,
    });
  });

  it("rejects a response that is not an object", () => {
    expect(() => parseSnapshot([])).toThrow(new ShapeError("stats: expected an object"));
  });
});
