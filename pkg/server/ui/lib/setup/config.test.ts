import { describe, expect, it } from "vitest";

import { ShapeError } from "#ui/lib/common/json";
import { parseSetupConfig } from "#ui/lib/setup/config";

describe("reading the saved config", () => {
  it("finds nothing to fill in an empty config", () => {
    expect(parseSetupConfig({})).toStrictEqual({
      authUsername: undefined,
      debrid: undefined,
      usenet: undefined,
      downloadFolder: undefined,
      mount: undefined,
    });
  });

  it("reads the username, the first debrid account and the download folder", () => {
    expect(
      parseSetupConfig({
        auth_username: "admin",
        debrids: [{ provider: "torbox", api_key: "key", name: "torbox" }, { provider: "realdebrid" }],
        download_folder: "/downloads",
      }),
    ).toMatchObject({
      authUsername: "admin",
      debrid: { provider: "torbox", apiKey: "key" },
      downloadFolder: "/downloads",
    });
  });

  it("treats empty values as absent and missing debrid fields as empty", () => {
    expect(parseSetupConfig({ auth_username: "", debrids: [{}], download_folder: "" })).toMatchObject({
      authUsername: undefined,
      debrid: { provider: "", apiKey: "" },
      downloadFolder: undefined,
    });
  });
});

describe("reading saved servers and mounts", () => {
  it("reads the first usenet server only from a list", () => {
    const server = { host: "h", port: 563, username: "u", password: "p", max_connections: 0, ssl: true };
    expect([
      parseSetupConfig({ usenet: [server] }).usenet,
      parseSetupConfig({ usenet: { providers: [server] } }).usenet,
    ]).toStrictEqual([
      {
        host: "h",
        port: "563",
        username: "u",
        password: "p",
        maxConnections: "",
        readerConnections: "",
        ssl: true,
      },
      undefined,
    ]);
  });

  it("reads an enabled DFS mount before an enabled rclone mount", () => {
    expect(
      parseSetupConfig({
        dfs: { enabled: true, mount_path: "/mnt/dfs", cache_dir: "/cache" },
        rclone: { enabled: true, mount_path: "/mnt/rclone" },
      }).mount,
    ).toStrictEqual({ type: "dfs", mountPath: "/mnt/dfs", cacheDir: "/cache", bufferSize: undefined });
  });

  it("reads an enabled rclone mount with its VFS cache dir and buffer size", () => {
    expect(
      parseSetupConfig({
        dfs: { enabled: false },
        rclone: { enabled: true, mount_path: "/mnt/rclone", cache_dir: "/ignored", buffer_size: "64M" },
      }).mount,
    ).toStrictEqual({ type: "rclone", mountPath: "/mnt/rclone", cacheDir: "", bufferSize: "64M" });
  });
});

describe("reading a malformed config", () => {
  it.each([
    ["a list", []],
    ["debrids that are not a list", { debrids: "x" }],
    ["a username that is not a string", { auth_username: 1 }],
    ["a debrid account that is not an object", { debrids: ["x"] }],
  ])("rejects %s", (_name, config) => {
    expect(() => parseSetupConfig(config)).toThrow(ShapeError);
  });
});
