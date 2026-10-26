import { describe, expect, it } from "vitest";

import { BODY_ORDER, assembleSettings, sections, valueAt } from "#ui/lib/config/sections";

const collectedFor = (overrides: Readonly<Record<string, unknown>>): Map<string, unknown> =>
  new Map(BODY_ORDER.map((prefix) => [prefix, overrides[prefix] ?? prefix]));

describe("the settings sections", () => {
  it("the body order places every section exactly once", () => {
    expect([...BODY_ORDER].toSorted()).toStrictEqual(sections.map((section) => section.prefix).toSorted());
  });

  it("assembles nested sections in config.js's key order", () => {
    const body = assembleSettings(
      collectedFor({ "": { log_level: "info" }, mount: { type: "dfs", mount_path: "/mnt" } }),
    );
    expect({ keys: Object.keys(body), mount: body["mount"] }).toStrictEqual({
      keys: [
        "log_level",
        "virtual_folders",
        "debrids",
        "arrs",
        "queue_cleanup",
        "mount",
        "nfs",
        "smb",
        "share_cache",
        "usenet",
        "notifications",
        "repair",
        "prune",
        "strm",
        "hearsay",
      ],
      mount: {
        type: "dfs",
        mount_path: "/mnt",
        dfs: "mount.dfs",
        rclone: "mount.rclone",
        external_rclone: "mount.external_rclone",
      },
    });
  });

  it("reads a section's value by its prefix", () => {
    const config = { mount: { rclone: { port: "1" } } };
    expect([valueAt(config, ""), valueAt(config, "mount.rclone"), valueAt(config, "smb.port")]).toStrictEqual([
      config,
      { port: "1" },
      undefined,
    ]);
  });
});
