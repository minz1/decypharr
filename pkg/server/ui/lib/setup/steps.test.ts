import { describe, expect, it } from "vitest";

import {
  emptySetupState,
  providerConfigured,
  readAuthStep,
  readDebridStep,
  readDownloadStep,
  readMountStep,
  readUsenetStep,
  setupPayload,
} from "#ui/lib/setup/steps";

const usenet = {
  host: " news.example.com ",
  port: "563",
  username: " user ",
  password: " pass ",
  maxConnections: "",
  readerConnections: "",
  ssl: true,
};

describe("the authentication step", () => {
  it("keeps a trimmed username and the password as typed", () => {
    expect(readAuthStep({ username: " admin ", password: " secret", confirmPassword: " secret" })).toStrictEqual({
      step: { username: "admin", password: " secret", skip_auth: false },
    });
  });

  it("skips authentication when every field is empty", () => {
    expect(readAuthStep({ username: "  ", password: "", confirmPassword: "" })).toStrictEqual({
      step: { skip_auth: true },
    });
  });

  it.each([
    [{ username: "", password: "secret", confirmPassword: "secret" }, "Username is required"],
    [{ username: "admin", password: "", confirmPassword: "x" }, "Password is required"],
    [{ username: "admin", password: "secret", confirmPassword: "secreT" }, "Passwords do not match"],
    [{ username: "admin", password: "short", confirmPassword: "short" }, "Password must be at least 6 characters"],
  ])("rejects %o with %s", (input, error) => {
    expect(readAuthStep(input)).toStrictEqual({ error });
  });
});

describe("the debrid step", () => {
  it("keeps the provider and the trimmed API key", () => {
    expect(readDebridStep({ provider: "torbox", apiKey: " key " })).toStrictEqual({
      step: { provider: "torbox", api_key: "key" },
    });
  });

  it.each([
    [{ provider: "", apiKey: "key" }, "Please select a debrid provider"],
    [{ provider: "torbox", apiKey: "  " }, "API key is required"],
  ])("rejects %o with %s", (input, error) => {
    expect(readDebridStep(input)).toStrictEqual({ error });
  });
});

describe("the usenet step", () => {
  it("trims every field and defaults the connection counts", () => {
    expect(readUsenetStep(usenet)).toStrictEqual({
      step: {
        host: "news.example.com",
        port: 563,
        username: "user",
        password: "pass",
        max_connections: 30,
        reader_connections: 15,
        ssl: true,
        skip_usenet: false,
      },
    });
  });

  it("keeps given connection counts", () => {
    expect(readUsenetStep({ ...usenet, maxConnections: "100", readerConnections: "50" })).toMatchObject({
      step: { max_connections: 100, reader_connections: 50 },
    });
  });

  it("skips usenet when every field is empty and SSL is off", () => {
    expect(
      readUsenetStep({ ...usenet, host: "", port: " ", username: "", password: "", maxConnections: "9", ssl: false }),
    ).toStrictEqual({ step: { skip_usenet: true } });
  });
});

describe("a rejected usenet step", () => {
  it.each([
    [{ host: "" }, "Usenet server host is required"],
    [{ port: "" }, "Usenet server port is required"],
    [{ username: "" }, "Usenet username is required"],
    [{ password: " " }, "Usenet password is required"],
    [{ port: "0" }, "Please enter a valid port number (1-65535)"],
    [{ port: "65536" }, "Please enter a valid port number (1-65535)"],
    [{ port: "abc" }, "Please enter a valid port number (1-65535)"],
    [{ maxConnections: "0" }, "Max connections must be between 1 and 100"],
    [{ maxConnections: "101" }, "Max connections must be between 1 and 100"],
    [{ readerConnections: "0" }, "Connections per stream must be between 1 and 50"],
    [{ readerConnections: "51" }, "Connections per stream must be between 1 and 50"],
  ])("rejects %o with %s", (change, error) => {
    expect(readUsenetStep({ ...usenet, ...change })).toStrictEqual({ error });
  });

  it("validates the other fields when only SSL is set", () => {
    expect(
      readUsenetStep({ ...usenet, host: "", port: "", username: "", password: "", maxConnections: "", ssl: true }),
    ).toStrictEqual({ error: "Usenet server host is required" });
  });

  it("reads numbers the way parseInt does", () => {
    expect(readUsenetStep({ ...usenet, port: "119.5", maxConnections: "8x" })).toMatchObject({
      step: { port: 119, max_connections: 8 },
    });
  });
});

describe("the download step", () => {
  it("keeps the trimmed folder", () => {
    expect(readDownloadStep(" /downloads ")).toStrictEqual({ step: { download_folder: "/downloads" } });
  });

  it("requires a folder", () => {
    expect(readDownloadStep("  ")).toStrictEqual({ error: "Download folder is required" });
  });
});

describe("the mount step", () => {
  const mount = { mountType: "dfs", mountPath: " /mnt ", cacheDir: " /cache ", rcloneBufferSize: " 128M " };

  it("keeps the trimmed paths and drops the buffer size unless the type is rclone", () => {
    expect(readMountStep(mount)).toStrictEqual({
      step: { mount_type: "dfs", mount_path: "/mnt", cache_dir: "/cache" },
    });
  });

  it("keeps the buffer size for rclone", () => {
    expect(readMountStep({ ...mount, mountType: "rclone" })).toStrictEqual({
      step: { mount_type: "rclone", mount_path: "/mnt", cache_dir: "/cache", rclone_buffer_size: "128M" },
    });
  });

  it("drops an empty rclone buffer size", () => {
    expect(readMountStep({ ...mount, mountType: "rclone", rcloneBufferSize: " " })).toStrictEqual({
      step: { mount_type: "rclone", mount_path: "/mnt", cache_dir: "/cache" },
    });
  });

  it("requires a mount path unless the type is none", () => {
    expect([
      readMountStep({ ...mount, mountPath: " " }),
      readMountStep({ ...mount, mountType: "none", mountPath: "" }),
    ]).toStrictEqual([
      { error: "Mount path is required" },
      { step: { mount_type: "none", mount_path: "", cache_dir: "/cache" } },
    ]);
  });
});

describe("the provider requirement", () => {
  it("holds with a debrid account or a whole usenet server, and not when skipped", () => {
    const state = emptySetupState();
    const unmet = providerConfigured(state);
    state.debrid = { provider: "torbox", api_key: "key" };
    const debrid = providerConfigured(state);
    state.debrid = { skip_debrid: true };
    const skipped = providerConfigured(state);
    state.usenet = { host: "h", port: 1, username: "u", password: "p", ssl: false, skip_usenet: false };
    const server = providerConfigured(state);
    state.usenet = { skip_usenet: true };
    expect([unmet, debrid, skipped, server, providerConfigured(state)]).toStrictEqual([
      false,
      true,
      false,
      true,
      false,
    ]);
  });
});

describe("the setup payload", () => {
  it("starts with every step empty", () => {
    const payload = setupPayload(emptySetupState());
    expect(JSON.stringify(payload)).toBe('{"auth":{},"debrid":{},"usenet":{},"download":{},"mount":{}}');
  });
});
