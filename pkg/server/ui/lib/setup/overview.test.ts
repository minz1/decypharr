import { describe, expect, it } from "vitest";

import { setupOverview } from "#ui/lib/setup/overview";
import { emptySetupState } from "#ui/lib/setup/steps";

describe("the overview of a fresh wizard", () => {
  it("says nothing is configured", () => {
    expect(setupOverview(emptySetupState())).toStrictEqual({
      auth: "Not configured",
      debrid: "Not configured",
      usenet: "Not configured",
      download: "Not set",
      mount: "Not configured",
    });
  });
});

describe("the overview of skipped steps", () => {
  it("says which steps were skipped", () => {
    const state = emptySetupState();
    state.auth = { skip_auth: true };
    state.debrid = { skip_debrid: true };
    state.usenet = { skip_usenet: true };
    expect(setupOverview(state)).toMatchObject({
      auth: "Authentication disabled (skipped)",
      debrid: "Debrid disabled (skipped)",
      usenet: "Usenet disabled (skipped)",
    });
  });

  it("describes token-only authentication", () => {
    const state = emptySetupState();
    state.auth = { token_only: true };
    expect(setupOverview(state).auth).toBe("API token only (no username or password)");
  });
});

describe("the overview of configured steps", () => {
  it("lists each step's settings", () => {
    const state = emptySetupState();
    state.auth = { username: "admin", password: "secret", skip_auth: false };
    state.debrid = { provider: "torbox", api_key: "key" };
    state.usenet = {
      host: "news",
      port: 563,
      username: "user",
      password: "pass",
      max_connections: 30,
      reader_connections: 15,
      ssl: false,
      skip_usenet: false,
    };
    state.download = { download_folder: "/downloads" };
    state.mount = { mount_type: "external_rclone", mount_path: "/mnt", cache_dir: "" };
    expect(setupOverview(state)).toStrictEqual({
      auth: "Username: admin",
      debrid: [["Provider:", "torbox"]],
      usenet: [
        ["Server:", "news:563"],
        ["Username:", "user"],
        ["Max Connections:", "30"],
        ["Connections Per Stream:", "15"],
        ["Use SSL:", "No"],
      ],
      download: "/downloads",
      mount: [
        ["Type:", "External Rclone"],
        ["Mount Path:", "/mnt"],
        ["Cache Directory:", ""],
      ],
    });
  });

  it.each([
    ["dfs", "DFS (Decypharr File System)"],
    ["rclone", "Rclone"],
    ["none", "None"],
  ])("names the %s mount type %s", (mountType, name) => {
    const state = emptySetupState();
    state.mount = { mount_type: mountType, mount_path: "", cache_dir: "" };
    expect(setupOverview(state).mount).toContainEqual(["Type:", name]);
  });
});
