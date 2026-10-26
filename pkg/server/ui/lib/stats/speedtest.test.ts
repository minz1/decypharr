import { describe, expect, it } from "vitest";

import { ShapeError } from "#ui/lib/common/json";
import { debridSpeedParts, parseSpeedTest, testedAtText } from "#ui/lib/stats/speedtest";

const result = {
  provider: "realdebrid",
  protocol: "debrid",
  speed_mbps: 12.5,
  latency_ms: 42,
  bytes_read: 1024,
  tested_at: "2026-10-26T07:00:00Z",
};

describe("a speed test result", () => {
  it("reads the server's response", () => {
    expect(parseSpeedTest(result)).toStrictEqual({
      error: "",
      latencyMs: 42,
      speedMbps: 12.5,
      testedAt: "2026-10-26T07:00:00Z",
    });
    expect(parseSpeedTest({ ...result, error: "dial tcp: timeout" }).error).toBe("dial tcp: timeout");
  });

  it("rejects a malformed response", () => {
    expect(() => parseSpeedTest("ok")).toThrow(ShapeError);
    expect(() => parseSpeedTest({ ...result, latency_ms: "42" })).toThrow(ShapeError);
  });

  it("shows the test time in the browser's locale, or nothing", () => {
    expect(testedAtText("2026-10-26T07:00:00Z")).toBe(new Date("2026-10-26T07:00:00Z").toLocaleTimeString());
    expect(testedAtText("")).toBe("");
  });
});

describe("a debrid speed result", () => {
  it("shows latency and the speed in megabits", () => {
    expect(debridSpeedParts(parseSpeedTest(result))).toStrictEqual({
      latency: "42ms",
      speed: "100.0 Mbps",
      tested: `Tested: ${new Date("2026-10-26T07:00:00Z").toLocaleTimeString()}`,
    });
  });

  it("leaves out a zero latency or speed", () => {
    expect(debridSpeedParts(parseSpeedTest({ speed_mbps: 0, latency_ms: 0, tested_at: "" }))).toStrictEqual({
      latency: "",
      speed: "",
      tested: "Tested: ",
    });
  });
});
