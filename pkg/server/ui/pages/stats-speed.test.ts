import { describe, expect, it, vi } from "vitest";

import {
  XSS,
  bootStats,
  debridEntry,
  element,
  json,
  loaded,
  pageHost,
  statsRoute,
  text,
  textOf,
} from "#ui/lib/stats/testing";

const nntpResult = { provider: "news:563/kim", protocol: "nntp", latency_ms: 42, tested_at: "2026-10-26T07:00:00Z" };
const debridResult = {
  provider: XSS,
  protocol: "debrid",
  speed_mbps: 12.5,
  latency_ms: 80,
  tested_at: "2026-10-26T07:00:00Z",
};

const tested = `Tested: ${new Date("2026-10-26T07:00:00Z").toLocaleTimeString()}`;

const button = (selector: string): HTMLButtonElement => {
  const found = element(selector);
  if (!(found instanceof HTMLButtonElement)) {
    throw new TypeError(`no button ${selector}`);
  }
  return found;
};

const NNTP = '#usenet-content [data-action="nntp-speed-test"]';
const DEBRID = '#debrid-content [data-action="debrid-speed-test"]';

const pending = (): { route: () => Promise<Response>; answer: (response: Response) => void } => {
  const deferred = Promise.withResolvers<Response>();
  return { route: async () => deferred.promise, answer: deferred.resolve };
};

describe("testing an NNTP server's speed", () => {
  it("posts once while the button is busy, then shows the latency", async () => {
    const reply = pending();
    const sent = await bootStats({ ...statsRoute(), "POST /debug/speedtest": reply.route });
    await loaded();
    button(NNTP).click();
    button(NNTP).click();
    element(`${NNTP} .loading`).click();
    expect({
      disabled: button(NNTP).disabled,
      label: textOf(NNTP),
      spinner: element(NNTP).querySelector(".loading-xs") !== null,
    }).toStrictEqual({
      disabled: true,
      label: "Testing...",
      spinner: true,
    });
    reply.answer(Response.json(nntpResult));
    await vi.waitFor(() => {
      expect(textOf("#usenet-content [data-field=speed-result]")).toBe(`42ms latency ${tested}`);
    });
    expect({ disabled: button(NNTP).disabled, label: textOf(NNTP), posts: sent.slice(1) }).toStrictEqual({
      disabled: false,
      label: "Test Speed",
      posts: [
        {
          url: "/debug/speedtest",
          method: "POST",
          body: '{"protocol":"nntp","provider":"news:563/kim"}',
          contentType: "application/json",
        },
      ],
    });
  });

  it.each([
    [json({ ...nntpResult, error: XSS }), XSS],
    [text("boom", 500), "HTTP 500"],
    [text("{", 200), "response is not JSON"],
  ])("shows a failure as text (%#)", async (route, message) => {
    await bootStats({ ...statsRoute(), "POST /debug/speedtest": route }, "stats@base");
    await loaded();
    button(NNTP).click();
    await vi.waitFor(() => {
      expect(textOf("#usenet-content [data-field=speed-result] .text-error")).toBe(message);
    });
    expect(document.querySelectorAll("#stats-content img")).toHaveLength(0);
  });
});

describe("testing a debrid provider's speed", () => {
  it("posts once while the button is busy, then shows latency and megabits", async () => {
    const reply = pending();
    const sent = await bootStats({ ...statsRoute(), "POST /debug/speedtest": reply.route }, "stats@base");
    await loaded();
    button(DEBRID).click();
    button(DEBRID).click();
    element(`${DEBRID} .loading`).click();
    expect({
      disabled: button(DEBRID).disabled,
      label: textOf(DEBRID),
      result: textOf("#debrid-content [data-field=speed-result]"),
    }).toStrictEqual({ disabled: true, label: "", result: "Testing..." });
    reply.answer(Response.json(debridResult));
    await vi.waitFor(() => {
      expect(textOf("#debrid-content [data-field=speed-result]")).toBe(`80ms latency 100.0 Mbps ${tested}`);
    });
    expect({ label: textOf(DEBRID), posts: sent.slice(1).map((request) => [request.url, request.body]) }).toStrictEqual(
      {
        label: "Test Speed",
        posts: [["/decypharr/debug/speedtest", JSON.stringify({ protocol: "debrid", provider: XSS })]],
      },
    );
  });

  it("shows the stored result from the stats", async () => {
    await bootStats(statsRoute({ debrids: [{ ...debridEntry, speed_test_result: debridResult }] }));
    await loaded();
    expect(textOf("#debrid-content [data-field=speed-result]")).toBe(`80ms latency 100.0 Mbps ${tested}`);
  });
});

describe("copying a connection command", () => {
  it("copies the exact command and flashes a check mark", async () => {
    const copied: string[] = [];
    vi.spyOn(navigator.clipboard, "writeText").mockImplementation(async (value) => {
      copied.push(value);
      return Promise.resolve();
    });
    await bootStats(statsRoute());
    await loaded();
    const copy = element('#smb-details [data-action="copy"]');
    copy.click();
    await vi.waitFor(() => {
      expect(copy.querySelector(".bi-check-lg")).not.toBeNull();
    });
    expect(copied).toStrictEqual([String.raw`net use Z: \\${pageHost()}\media /user:kim`]);
    await vi.waitFor(
      () => {
        expect(copy.querySelector(".bi-clipboard")).not.toBeNull();
      },
      { timeout: 2000 },
    );
  });
});
