import { describe, expect, it } from "vitest";

import { ShapeError } from "#ui/lib/common/json";
import { debridView, parseDebrids } from "#ui/lib/stats/debrid";

const debrid = {
  profile: {
    name: "realdebrid",
    id: 1,
    username: "kim",
    email: "kim@example.com",
    points: 1500,
    type: "premium",
    premium: 1,
    expiration: "2027-01-31T00:00:00Z",
  },
  library: { total: 2500, bad: 3, active_links: 42 },
  accounts: [
    {
      in_use: true,
      order: 0,
      disabled: false,
      token_masked: "abc***",
      username: "kim",
      traffic_used: 1_073_741_824,
      expiration: "2027-01-31T00:00:00Z",
      links_count: 4,
      debrid: "realdebrid",
    },
    { in_use: false, order: 1, disabled: true, username: "", traffic_used: 0, expiration: "" },
  ],
  speed_test_result: {
    provider: "realdebrid",
    speed_mbps: 2.5,
    latency_ms: 80,
    bytes_read: 1,
    tested_at: "2026-10-26T07:00:00Z",
  },
};

const view = (overrides: Record<string, unknown> = {}): ReturnType<typeof debridView> => {
  const [parsed] = parseDebrids([{ ...debrid, ...overrides }]);
  if (!parsed) {
    throw new Error("no debrid parsed");
  }
  return debridView(parsed);
};

const date = (iso: string): string => new Date(iso).toLocaleDateString();

describe("a debrid provider card", () => {
  it("shows the profile and library figures", () => {
    expect(view()).toMatchObject({
      provider: "realdebrid",
      texts: {
        name: "realdebrid",
        username: "kim",
        points: `${(1500).toLocaleString()} points`,
        type: "Type: premium",
        expires: `Expires: ${date("2027-01-31T00:00:00Z")}`,
        "library-total": (2500).toLocaleString(),
        "library-bad": "3",
        "library-active": "42",
        "accounts-total": "2",
      },
    });
  });

  it("falls back for missing profile fields", () => {
    expect(view({ profile: { name: "torbox" }, library: {} }).texts).toMatchObject({
      name: "torbox",
      username: "No username",
      points: "0 points",
      type: "Type: Unknown",
      expires: "Expires: Unknown",
      "library-total": "0",
    });
  });
});

describe("a debrid provider's accounts and speed test", () => {
  it("lists each account with its state", () => {
    expect(view().accounts).toStrictEqual([
      {
        title: "Account #1",
        disabled: false,
        inUse: true,
        username: "kim",
        usage: "Daily Usage: 1 GB",
        expires: `Expires: ${date("2027-01-31T00:00:00Z")}`,
      },
      {
        title: "Account #2",
        disabled: true,
        inUse: false,
        username: "-",
        usage: "Daily Usage: 0 B",
        expires: "Expires: Unknown",
      },
    ]);
  });

  it("keeps the stored speed test unless it failed", () => {
    expect(view().speedTest?.latencyMs).toBe(80);
    expect(view({ speed_test_result: { ...debrid.speed_test_result, error: "timeout" } }).speedTest).toBeUndefined();
    expect(view({ speed_test_result: undefined }).speedTest).toBeUndefined();
  });
});

describe("the debrid section", () => {
  it("reads no providers as an empty list", () => {
    expect(parseDebrids(JSON.parse("null"))).toStrictEqual([]);
    expect(parseDebrids()).toStrictEqual([]);
  });

  it("rejects a provider without a name", () => {
    expect(() => parseDebrids([{ ...debrid, profile: {} }])).toThrow(ShapeError);
    expect(() => parseDebrids({})).toThrow(ShapeError);
  });
});
