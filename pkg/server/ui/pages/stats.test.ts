import { describe, expect, it, vi } from "vitest";

import {
  bootStats,
  click,
  element,
  loaded,
  pageHost,
  statsRoute,
  text,
  textOf,
  toasts,
  XSS,
} from "#ui/lib/testing/stats";

describe("loading the stats page", () => {
  it("fetches the stats once and shows them", async () => {
    const sent = await bootStats(statsRoute());
    await loaded();
    expect(sent.map((request) => `${request.method} ${request.url}`)).toStrictEqual(["GET /debug/stats"]);
    expect({
      loading: element("#loading-stats").style.display,
      error: element("#error-stats").style.display,
      uptime: textOf("#uptime"),
      memory: textOf("#memory-used"),
      queue: textOf("#queue-pending"),
      mount: [textOf("#mount-type"), textOf("#mount-status")],
      streams: [textOf("#streams-count"), textOf("#streams-tab-count")],
      webdav: textOf("#webdav-details code"),
    }).toStrictEqual({
      loading: "none",
      error: "none",
      uptime: "1h 2m 5s",
      memory: "40.00MB",
      queue: "2",
      mount: ["DFS", "Active"],
      streams: ["1", "1"],
      webdav: `${globalThis.location.protocol}//${pageHost()}:8282/webdav/`,
    });
  });

  it("fetches under the URL base", async () => {
    const sent = await bootStats(statsRoute(), "stats@base");
    await loaded();
    expect(sent.map((request) => request.url)).toStrictEqual(["/decypharr/debug/stats"]);
  });

  it("renders user-controlled names as text", async () => {
    await bootStats(statsRoute());
    await loaded();
    expect(document.querySelectorAll("#stats-content img")).toHaveLength(0);
    expect([
      textOf("#streams-content h4"),
      textOf("#debrid-content h3"),
      textOf("#usenet-content [data-field=host]"),
    ]).toStrictEqual([XSS, XSS, XSS]);
  });
});

describe("a failed stats load", () => {
  it("shows the status in the error panel", async () => {
    await bootStats({ "GET /debug/stats": text("boom", 500, "Internal Server Error") });
    await vi.waitFor(() => {
      expect(element("#error-stats").style.display).toBe("flex");
    });
    expect([textOf("#error-message"), element("#stats-content").style.display]).toStrictEqual([
      "HTTP 500: Internal Server Error",
      "none",
    ]);
  });

  it("shows a malformed response in the error panel", async () => {
    await bootStats({ "GET /debug/stats": text("<html>", 200) });
    await vi.waitFor(() => {
      expect(textOf("#error-message")).toBe("response is not JSON");
    });
  });

  it("retries from the error panel", async () => {
    const sent = await bootStats({ "GET /debug/stats": text("", 502, "Bad Gateway") });
    await vi.waitFor(() => {
      expect(textOf("#error-message")).toBe("HTTP 502: Bad Gateway");
    });
    click("#retry-stats");
    await vi.waitFor(() => {
      expect(sent).toHaveLength(2);
    });
  });

  it("reports a missing element as a toast naming it", async () => {
    await bootStats(statsRoute(), "stats", () => {
      element("#refresh-stats").remove();
    });
    await vi.waitFor(() => {
      expect(toasts()).toStrictEqual(["stats: missing #refresh-stats"]);
    });
  });
});

describe("refreshing the stats", () => {
  it("fetches again on refresh", async () => {
    const sent = await bootStats(statsRoute());
    await loaded();
    click("#refresh-stats");
    await vi.waitFor(() => {
      expect(sent).toHaveLength(2);
    });
  });

  it("fetches again every 30 seconds", async () => {
    const sent = await bootStats(statsRoute());
    await loaded();
    vi.advanceTimersByTime(29_000);
    expect(sent).toHaveLength(1);
    vi.advanceTimersByTime(1000);
    await vi.waitFor(() => {
      expect(sent).toHaveLength(2);
    });
  });
});

describe("switching tabs", () => {
  it.each(["streams", "debrid", "usenet", "mount", "overview"])("shows only the %s tab", async (tab) => {
    await bootStats(statsRoute());
    await loaded();
    click(`#stats-tabs [data-tab="${tab}"]`);
    const visible = [...document.querySelectorAll(".tab-content")].filter(
      (panel) => !panel.classList.contains("hidden"),
    );
    const active = [...document.querySelectorAll<HTMLElement>("#stats-tabs .tab-active")].map(
      (link) => link.dataset["tab"],
    );
    expect({ visible: visible.map((panel) => panel.id), active }).toStrictEqual({
      visible: [`tab-${tab}`],
      active: [tab],
    });
  });
});
