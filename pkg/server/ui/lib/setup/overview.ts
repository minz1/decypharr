import type { MountStep, SetupState, UsenetStep } from "#ui/lib/setup/types";

type OverviewLine = readonly [label: string, value: string];

type OverviewSection = string | readonly OverviewLine[];

interface SetupOverview {
  auth: string;
  debrid: OverviewSection;
  usenet: OverviewSection;
  download: string;
  mount: OverviewSection;
}

const MOUNT_NAMES: Readonly<Record<string, string>> = {
  rclone: "Rclone",
  external_rclone: "External Rclone",
  dfs: "DFS (Decypharr File System)",
};

const shown = (value: string | number | undefined): string => (value === undefined ? "" : String(value));

const authOverview = (state: SetupState): string => {
  if (state.auth.skip_auth === true) {
    return "Authentication disabled (skipped)";
  }
  if (state.auth.token_only === true) {
    return "API token only (no username or password)";
  }
  const username = state.auth.username ?? "";
  return username === "" ? "Not configured" : `Username: ${username}`;
};

const debridOverview = (state: SetupState): OverviewSection => {
  if (state.debrid.skip_debrid === true) {
    return "Debrid disabled (skipped)";
  }
  const provider = state.debrid.provider ?? "";
  return provider === "" ? "Not configured" : [["Provider:", provider]];
};

const usenetOverview = (usenet: UsenetStep): OverviewSection => {
  if (usenet.skip_usenet === true) {
    return "Usenet disabled (skipped)";
  }
  if ((usenet.host ?? "") === "") {
    return "Not configured";
  }
  return [
    ["Server:", `${shown(usenet.host)}:${shown(usenet.port)}`],
    ["Username:", shown(usenet.username)],
    ["Max Connections:", shown(usenet.max_connections)],
    ["Connections Per Stream:", shown(usenet.reader_connections)],
    ["Use SSL:", usenet.ssl === true ? "Yes" : "No"],
  ];
};

const mountOverview = (mount: MountStep): OverviewSection => {
  const mountType = mount.mount_type ?? "";
  if (mountType === "") {
    return "Not configured";
  }
  return [
    ["Type:", MOUNT_NAMES[mountType] ?? "None"],
    ["Mount Path:", shown(mount.mount_path)],
    ["Cache Directory:", shown(mount.cache_dir)],
  ];
};

const setupOverview = (state: SetupState): SetupOverview => {
  const folder = state.download.download_folder ?? "";
  return {
    auth: authOverview(state),
    debrid: debridOverview(state),
    usenet: usenetOverview(state.usenet),
    download: folder === "" ? "Not set" : folder,
    mount: mountOverview(state.mount),
  };
};

export type { OverviewSection, SetupOverview };
export { setupOverview };
