import { describe, expect, it } from "vitest";

import { validateSettings } from "#ui/lib/config/validate";

const valid = {
  debrids: [{ name: "rd", api_key: "k", provider: "realdebrid" }],
  arrs: [{ name: "sonarr", host: "http://sonarr:8989" }],
  virtual_folders: [],
  mount: { type: "dfs", mount_path: "/mnt" },
  repair: { enabled: false, schedule: "" },
  smb: { enabled: false },
};

describe("validating settings before a save", () => {
  it("passes a valid form", () => {
    expect(validateSettings(valid)).toStrictEqual([]);
  });

  it("reports each problem with config.js's message", () => {
    expect(
      validateSettings({
        debrids: [
          { name: "", api_key: "k", provider: "realdebrid" },
          { name: "Movies", api_key: "k", provider: "x" },
        ],
        arrs: [{ name: "", host: "not a url" }],
        virtual_folders: [
          { name: "torrents", match: "all", conditions: [] },
          { name: "movies", match: "all", conditions: [] },
          { name: "Dup", match: "all", conditions: [] },
          { name: "dup", match: "all", conditions: [] },
        ],
        mount: { type: "dfs", mount_path: "" },
        repair: { enabled: true, schedule: "" },
        smb: { enabled: true, username: "", password: "" },
      }),
    ).toStrictEqual([
      "Debrid service #1: Name, API key are required",
      "Arr service #1: Name and host are required",
      "Arr service #1: Invalid host URL format",
      'Virtual folder "torrents": the name conflicts with a built-in or provider folder.',
      'Virtual folder "movies": the name conflicts with a built-in or provider folder.',
      'Virtual folder "dup" is defined more than once.',
      'Mounts: mount path is required unless the mount type is "No Mount"',
      "Repair: schedule is required when Repair is enabled",
      "Shares > SMB: username and password are required when SMB is enabled",
    ]);
  });

  it("names a single missing SMB credential and allows no mount path for no mount", () => {
    expect(
      validateSettings({
        ...valid,
        mount: { type: "none", mount_path: "" },
        smb: { enabled: true, username: "u", password: "" },
      }),
    ).toStrictEqual(["Shares > SMB: password is required when SMB is enabled"]);
  });
});
