import { describe, expect, it } from "vitest";

import { mountState, parseMount } from "#ui/lib/stats/mount";

const state = (mount?: Record<string, unknown>): ReturnType<typeof mountState> => mountState(parseMount(mount));

describe("the mount badges", () => {
  it("shows a disabled mount when none is configured", () => {
    expect(state()).toStrictEqual({
      typeText: "None",
      typeClass: "badge badge-ghost",
      logsDisplay: undefined,
      statusText: "Disabled",
      statusClass: "badge badge-error",
      message: "No mount system is enabled or configured.",
    });
    expect(state({ ready: false, enabled: true })).toMatchObject({ typeText: "None", statusText: "Disabled" });
    expect(state({ ready: true, enabled: false, type: "dfs" })).toMatchObject({ typeText: "None" });
  });

  it("hides the logs button for DFS and says when it is not ready", () => {
    expect(state({ ready: false, enabled: true, type: "dfs" })).toStrictEqual({
      typeText: "DFS",
      typeClass: "badge badge-info",
      logsDisplay: "none",
      statusText: "Not Ready",
      statusClass: "badge badge-warning",
      message: "DFS filesystem is not ready.",
    });
  });

  it("shows the logs button for rclone and external mounts", () => {
    expect(state({ ready: false, enabled: true, type: "rclone" })).toMatchObject({
      typeText: "Rclone",
      logsDisplay: "inline-flex",
      message: "Rclone server is not ready.",
    });
    expect(state({ ready: false, enabled: false, type: "external" })).toMatchObject({
      typeText: "External",
      logsDisplay: "inline-flex",
      message: "External server is not ready.",
    });
  });

  it("shows an active mount without a message", () => {
    expect(state({ ready: true, enabled: true, type: "rclone", detail: {} })).toMatchObject({
      statusText: "Active",
      statusClass: "badge badge-success",
      message: undefined,
    });
  });
});

describe("the mount details", () => {
  it("parses DFS details only for a ready DFS mount", () => {
    expect(parseMount({ ready: true, enabled: true, type: "dfs", detail: { cache_item_count: 3 } }).dfs?.items).toBe(3);
    expect(parseMount({ ready: true, enabled: true, type: "dfs" }).dfs?.items).toBe(0);
    expect(parseMount({ ready: true, enabled: true, type: "rclone", detail: {} }).dfs).toBeUndefined();
    expect(parseMount({ ready: false, enabled: true, type: "dfs", detail: {} }).dfs).toBeUndefined();
  });

  it("parses rclone details for a ready rclone or external mount", () => {
    expect(
      parseMount({ ready: true, enabled: true, type: "external", detail: { memory: { Sys: 1 } } }).rclone?.memory,
    ).toStrictEqual({
      sys: 1,
      totalAlloc: 0,
    });
    expect(parseMount({ ready: false, enabled: true, type: "rclone" }).rclone).toBeUndefined();
  });
});
