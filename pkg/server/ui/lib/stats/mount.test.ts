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
      note: undefined,
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
      note: undefined,
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
    expect(
      state({ ready: true, enabled: true, type: "rclone", detail: { mounts: { state: "mounted" } } }),
    ).toMatchObject({
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

// The Go state-name tests hold the same lists (mount_state_internal_test.go, dfs state_internal_test.go).
const rcloneStates = ["waiting", "mounting", "mounted", "degraded", "failed", "unmounting", "unmounted"] as const;
const dfsStates = ["new", "starting", "ready", "failed", "stopping", "stopped"] as const;

const rcloneBadge: Record<(typeof rcloneStates)[number], [string, string]> = {
  waiting: ["Mounting", "badge badge-warning"],
  mounting: ["Mounting", "badge badge-warning"],
  mounted: ["Active", "badge badge-success"],
  degraded: ["Degraded", "badge badge-warning"],
  failed: ["Failed", "badge badge-error"],
  unmounting: ["Stopped", "badge badge-ghost"],
  unmounted: ["Stopped", "badge badge-ghost"],
};

const dfsBadge: Record<(typeof dfsStates)[number], [string, string]> = {
  new: ["Not Ready", "badge badge-warning"],
  starting: ["Starting", "badge badge-warning"],
  ready: ["Active", "badge badge-success"],
  failed: ["Failed", "badge badge-error"],
  stopping: ["Stopped", "badge badge-ghost"],
  stopped: ["Stopped", "badge badge-ghost"],
};

const rclone = (mountStateName?: string, extra: Record<string, unknown> = {}): Record<string, unknown> => ({
  ready: true,
  enabled: true,
  type: "rclone",
  detail: {
    state: "ready",
    restarts: 0,
    // The API sends null before the first attempt.
    mounts: mountStateName === undefined ? (JSON.parse("null") as unknown) : { state: mountStateName, error: "boom" },
    ...extra,
  },
});

const dfs = (name: string): Record<string, unknown> => ({
  ready: name === "ready",
  enabled: true,
  type: "dfs",
  detail: { state: name, error: "boom" },
});

describe("the mount's own state", () => {
  it("rcd ready + mount failed is not Active", () => {
    expect(state(rclone("failed"))).toMatchObject({ statusText: "Failed", statusClass: "badge badge-error" });
    expect(state(rclone("failed")).message).toContain("boom");
  });

  it.each(rcloneStates)("rclone %s has its badge", (name) => {
    const [text, cls] = rcloneBadge[name];
    expect(state(rclone(name))).toMatchObject({ statusText: text, statusClass: cls });
  });

  it.each(dfsStates)("dfs %s has its badge", (name) => {
    const [text, cls] = dfsBadge[name];
    expect(state(dfs(name))).toMatchObject({ statusText: text, statusClass: cls });
  });

  it("reads mounts: null as Mounting", () => {
    expect(state(rclone())).toMatchObject({ statusText: "Mounting" });
  });

  it("shows failure error for every non-up state", () => {
    for (const name of ["degraded", "failed"] as const) {
      expect(state(rclone(name)).message).toContain("boom");
    }
    expect(state(dfs("failed")).message).toContain("boom");
  });

  it("names the rcd state when rcd is not ready", () => {
    expect(state({ ready: false, enabled: true, type: "rclone", detail: { state: "starting" } })).toMatchObject({
      statusText: "Not Ready",
      message: "Rclone server is not ready (starting).",
    });
  });

  it("shows restarts on their own line and keeps the stats panel when Active", () => {
    const mounted = state(rclone("mounted", { restarts: 2 }));
    expect(mounted).toMatchObject({ statusText: "Active", message: undefined, note: "rclone restarted 2 times" });
    expect(parseMount(rclone("mounted")).rclone).toBeDefined();
    expect(state(rclone("mounted")).note).toBeUndefined();
  });
});
