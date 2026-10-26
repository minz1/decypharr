import { describe, expect, it, vi } from "vitest";

import {
  XSS,
  bootStats,
  click,
  dfsMount,
  element,
  json,
  loaded,
  settle,
  statsRoute,
  text,
  textOf,
  toasts,
} from "#ui/lib/testing/stats";

const cleanupResult = { status: "success", cache: { cleanup_freed_bytes: 2048, cleanup_removed_items: 3 } };
const purgeResult = { status: "success", cache: { purge_freed_bytes: 1_048_576, purge_removed_items: 7 } };

const requests = (sent: { method: string; url: string }[]): string[] =>
  sent.map((request) => `${request.method} ${request.url}`);

const dialog = (): HTMLDialogElement => {
  const found = element("#purge-cache-modal");
  if (!(found instanceof HTMLDialogElement)) {
    throw new TypeError("no purge dialog");
  }
  return found;
};

const openPurge = async (routes: Parameters<typeof bootStats>[0], fixture = "stats"): ReturnType<typeof bootStats> => {
  const sent = await bootStats({ ...statsRoute(), ...routes }, fixture);
  await loaded();
  click('#mount-content [data-action="cache-purge"]');
  return sent;
};

describe("running a cache cleanup", () => {
  it("posts once, toasts the result and reloads the stats", async () => {
    const sent = await bootStats({ ...statsRoute(), "POST /api/mount/cache/cleanup": json(cleanupResult) });
    await loaded();
    click('#mount-content [data-action="cache-cleanup"]');
    await vi.waitFor(() => {
      expect(toasts()).toStrictEqual(["Cache cleanup completed. Freed 2 KB and evicted 3 file(s)."]);
    });
    await vi.waitFor(() => {
      expect(requests(sent)).toStrictEqual(["GET /debug/stats", "POST /api/mount/cache/cleanup", "GET /debug/stats"]);
    });
    expect(sent[1]).toMatchObject({ body: "", contentType: "application/json" });
  });

  it("posts under the URL base", async () => {
    const sent = await bootStats(
      { ...statsRoute(), "POST /api/mount/cache/cleanup": json(cleanupResult) },
      "stats@base",
    );
    await loaded();
    click('#mount-content [data-action="cache-cleanup"]');
    await vi.waitFor(() => {
      expect(requests(sent)).toContain("POST /decypharr/api/mount/cache/cleanup");
    });
  });

  it.each([
    [text("Mount is not ready", 503), "Cache cleanup failed: Mount is not ready"],
    [text("", 500), "Cache cleanup failed: HTTP 500"],
    [text("{", 200), "Cache cleanup failed: response is not JSON"],
  ])("toasts a failure without reloading (%#)", async (route, message) => {
    const sent = await bootStats({ ...statsRoute(), "POST /api/mount/cache/cleanup": route });
    await loaded();
    click('#mount-content [data-action="cache-cleanup"]');
    await vi.waitFor(() => {
      expect(toasts()).toStrictEqual([message]);
    });
    expect(requests(sent)).toStrictEqual(["GET /debug/stats", "POST /api/mount/cache/cleanup"]);
  });
});

const CLEANUP = '#mount-content [data-action="cache-cleanup"]';
const PURGE = '#mount-content [data-action="cache-purge"]';

const buttonState = (selector: string): [boolean, string] => [element(selector).matches(":disabled"), textOf(selector)];

describe("a cache action in flight", () => {
  it("disables the cleanup button so a second click, even on its spinner, posts nothing", async () => {
    const reply = Promise.withResolvers<Response>();
    const sent = await bootStats({ ...statsRoute(), "POST /api/mount/cache/cleanup": async () => reply.promise });
    await loaded();
    for (const target of [CLEANUP, CLEANUP, `${CLEANUP} .loading`]) {
      click(target);
    }
    await settle();
    expect([buttonState(CLEANUP), requests(sent)]).toStrictEqual([
      [true, "Processing..."],
      ["GET /debug/stats", "POST /api/mount/cache/cleanup"],
    ]);
    reply.resolve(new Response("Mount is not ready", { status: 503 }));
    await vi.waitFor(() => {
      expect(toasts()).toStrictEqual(["Cache cleanup failed: Mount is not ready"]);
    });
    expect(buttonState(CLEANUP)).toStrictEqual([false, "Run Cleanup"]);
  });

  it("disables the purge button while the purge runs", async () => {
    const reply = Promise.withResolvers<Response>();
    await openPurge({ "POST /api/mount/cache/purge": async () => reply.promise });
    click("#confirm-purge-cache");
    await settle();
    expect(buttonState(PURGE)).toStrictEqual([true, "Processing..."]);
    reply.resolve(new Response("", { status: 500 }));
    await vi.waitFor(() => {
      expect(toasts()).toStrictEqual(["Cache purge failed: HTTP 500"]);
    });
    expect(buttonState(PURGE)).toStrictEqual([false, "Purge Cache"]);
  });
});

describe("purging the cache", () => {
  it("asks first, showing what will be purged", async () => {
    const sent = await openPurge({});
    expect({
      open: dialog().open,
      title: textOf("#purge-cache-modal h3"),
      summary: [textOf("#purge-cache-files"), textOf("#purge-cache-disk-used")],
      body: textOf("#purge-cache-modal p"),
      warning: textOf("#purge-cache-modal .alert-warning"),
      requests: requests(sent),
    }).toStrictEqual({
      open: true,
      title: "Purge DFS Cache",
      summary: ["120", "1.5 GB / 2 GB"],
      body: "This removes cached disk files that are not currently open or streaming. Active files will be skipped.",
      warning: "Cached files may need to be downloaded again after purging.",
      requests: ["GET /debug/stats"],
    });
  });

  it("does nothing when cancelled", async () => {
    const sent = await openPurge({ "POST /api/mount/cache/purge": json(purgeResult) });
    click("#cancel-purge-cache");
    click("#confirm-purge-cache");
    await settle();
    expect({ open: dialog().open, requests: requests(sent) }).toStrictEqual({
      open: false,
      requests: ["GET /debug/stats"],
    });
  });

  it("does nothing when the dialog is closed", async () => {
    const sent = await openPurge({ "POST /api/mount/cache/purge": json(purgeResult) });
    dialog().close();
    click("#confirm-purge-cache");
    await settle();
    expect(requests(sent)).toStrictEqual(["GET /debug/stats"]);
  });
});

describe("confirming a cache purge", () => {
  it("posts once when confirmed, toasts the result and reloads the stats", async () => {
    const sent = await openPurge({ "POST /api/mount/cache/purge": json(purgeResult) }, "stats@base");
    click("#confirm-purge-cache");
    click("#confirm-purge-cache");
    await vi.waitFor(() => {
      expect(toasts()).toStrictEqual(["Cache purge completed. Freed 1 MB and removed 7 file(s)."]);
    });
    await vi.waitFor(() => {
      expect(requests(sent)).toStrictEqual([
        "GET /decypharr/debug/stats",
        "POST /decypharr/api/mount/cache/purge",
        "GET /decypharr/debug/stats",
      ]);
    });
    expect([dialog().open, sent[1]?.contentType]).toStrictEqual([false, "application/json"]);
  });

  it("toasts a failure", async () => {
    await openPurge({ "POST /api/mount/cache/purge": text("Cache purge is only available for DFS mounts", 400) });
    click("#confirm-purge-cache");
    await vi.waitFor(() => {
      expect(toasts()).toStrictEqual(["Cache purge failed: Cache purge is only available for DFS mounts"]);
    });
  });

  it("calls an unlimited cache unlimited", async () => {
    const sent = await bootStats(
      statsRoute({ mount: { ...dfsMount, detail: { cache_total_size: 0, cache_max_size: 0 } } }),
    );
    await loaded();
    click('#mount-content [data-action="cache-purge"]');
    expect([textOf("#purge-cache-files"), textOf("#purge-cache-disk-used"), sent.length]).toStrictEqual([
      "0",
      "0 B / unlimited",
      1,
    ]);
  });
});

describe("the rclone mount", () => {
  it("shows counters, transfers and bandwidth with the logs button", async () => {
    const rclone = {
      ready: true,
      enabled: true,
      type: "rclone",
      detail: {
        version: { version: "v1.71.0", arch: "amd64", os: "linux" },
        core: { bytes: 1024, transferring: [{ name: XSS, bytes: 1, size: 4, speed: 1, eta: 0 }] },
        memory: { Sys: 1024, TotalAlloc: 0 },
        bandwidth: { bytesPerSecond: 1024, rate: "1024" },
      },
    };
    await bootStats(statsRoute({ mount: rclone }));
    await loaded();
    expect({
      badges: [textOf("#mount-type"), textOf("#mount-status")],
      logs: element("#rclone-logs-btn").style.display,
      version: textOf("#mount-content [data-field=version]"),
      transfers: textOf("#mount-content h3"),
      transfer: [textOf("#mount-content h4"), textOf("#mount-content [data-field=percent]")],
      rate: textOf("#mount-content [data-field=rate]"),
      images: document.querySelectorAll("#mount-content img").length,
    }).toStrictEqual({
      badges: ["Rclone", "Active"],
      logs: "inline-flex",
      version: "v1.71.0",
      transfers: "Active Transfers (1)",
      transfer: [XSS, "25.0%"],
      rate: "1 KB",
      images: 0,
    });
  });

  it("says when no mount is configured", async () => {
    await bootStats(statsRoute({ mount: { ready: false, enabled: false } }));
    await loaded();
    expect([textOf("#mount-type"), textOf("#mount-status"), textOf("#mount-content")]).toStrictEqual([
      "None",
      "Disabled",
      "No mount system is enabled or configured.",
    ]);
  });
});
