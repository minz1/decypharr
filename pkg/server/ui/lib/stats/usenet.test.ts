import { describe, expect, it } from "vitest";

import { ShapeError } from "#ui/lib/common/json";
import { parseUsenet, providerView, storedSpeed, usenetView } from "#ui/lib/stats/usenet";

const provider = {
  id: "news.example.com:563/kim",
  host: "news.example.com",
  username: "kim",
  port: 563,
  max_connections: 20,
  active: 5,
  idle: 3,
  ssl: true,
  waiting: 0,
};

const usenet = {
  providers: [
    provider,
    { ...provider, id: "", host: "backup.example.com", username: "", ssl: false, max_connections: 0 },
  ],
  pool: { max_connections: 40, total_created: 10, active: 6, idle: 4 },
  buffers: { memory_in_use: 1024, memory_allocated: 2048 },
  analyzer: { CachedBodyBytes: 1_048_576 },
  readers: 2,
};

const parsed = (overrides: Record<string, unknown> = {}): NonNullable<ReturnType<typeof parseUsenet>> => {
  const stats = parseUsenet({ ...usenet, ...overrides });
  if (!stats) {
    throw new Error("no usenet parsed");
  }
  return stats;
};

const firstProvider = (overrides: Record<string, unknown>): ReturnType<typeof providerView> => {
  const [first] = parsed({ providers: [{ ...provider, ...overrides }] }).providers;
  if (!first) {
    throw new Error("no provider parsed");
  }
  return providerView(first);
};

describe("the usenet summary", () => {
  it("shows servers, connections, memory and pool use", () => {
    expect(usenetView(parsed())).toStrictEqual({
      texts: {
        servers: "2 server(s) configured",
        connections: "10 / 40 connections",
        buffers: "Stream buffer allocations: 2 KB · In use: 1 KB",
        "parser-cache": "Parser body cache: 1 MB. This cache is separate from stream buffers.",
        utilization: "25.0%",
        active: "Active: 6",
        idle: "Idle: 4",
      },
      bars: { active: 15, idle: 10 },
    });
  });

  it("shows empty bars without a connection limit", () => {
    expect(usenetView(parsed({ pool: {} })).bars).toStrictEqual({ active: 0, idle: 0 });
    expect(usenetView(parsed({ pool: {} })).texts["utilization"]).toBe("0.0%");
  });
});

describe("an NNTP server", () => {
  it("shows its host, security, user, connections and pool use", () => {
    expect(firstProvider({})).toStrictEqual({
      provider: "news.example.com:563/kim",
      texts: {
        host: "news.example.com",
        security: "SSL",
        connections: "kim · 20 connections",
        utilization: "40.0%",
        active: "Active: 5",
        idle: "Idle: 3",
      },
      securityClass: "badge-success",
      bars: { active: 25, idle: 15 },
      stored: undefined,
    });
  });

  it("falls back to the host as its id and shows a plain connection", () => {
    expect(firstProvider({ id: "", username: "", ssl: false })).toMatchObject({
      provider: "news.example.com",
      texts: { security: "Plain", connections: "20 connections" },
      securityClass: "badge-warning",
    });
  });

  it("clamps the bars to the connection limit", () => {
    expect(firstProvider({ max_connections: 4, active: 3, idle: 3 })).toMatchObject({
      texts: { utilization: "100.0%" },
      bars: { active: 75, idle: 25 },
    });
    expect(firstProvider({ max_connections: -2, active: -1 })).toMatchObject({
      texts: { utilization: "0.0%", connections: "kim · 0 connections", active: "Active: 0" },
      bars: { active: 0, idle: 0 },
    });
  });
});

describe("an NNTP server's last speed test", () => {
  it.each([
    [
      { tested_at: "2026-10-26T07:00:00Z", speed_mbps: 12.345, latency_ms: 40, error: "" },
      { ok: true, text: "12.3 MB/s" },
    ],
    [
      { tested_at: "2026-10-26T07:00:00Z", speed_mbps: 0, latency_ms: 40, error: "" },
      { ok: true, text: "40ms" },
    ],
    [
      { tested_at: "2026-10-26T07:00:00Z", speed_mbps: 0, latency_ms: 0, error: "timeout" },
      { ok: false, text: "Error" },
    ],
    [{ tested_at: "", speed_mbps: 5, latency_ms: 40, error: "" }, undefined],
  ])("shows %j as %j", (speedTest, want) => {
    expect(storedSpeed(firstProvider({ speed_test: speedTest }).stored)).toStrictEqual(want);
  });
});

describe("the usenet section", () => {
  it("is absent without usenet or providers", () => {
    expect(parseUsenet()).toBeUndefined();
    expect(parsed({ providers: [] }).providers).toStrictEqual([]);
  });

  it("rejects a malformed provider", () => {
    expect(() => parsed({ providers: [{ ...provider, host: 7 }] })).toThrow(ShapeError);
  });
});
