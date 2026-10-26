import { asArray, asRecord, optBool, optNum, optStr } from "#ui/lib/common/json";

interface SavedUsenet {
  host: string;
  port: string;
  username: string;
  password: string;
  maxConnections: string;
  readerConnections: string;
  ssl: boolean;
}

interface SavedMount {
  type: "dfs" | "rclone";
  mountPath: string;
  cacheDir: string;
  bufferSize: string | undefined;
}

interface SetupConfig {
  authUsername: string | undefined;
  debrid: { provider: string; apiKey: string } | undefined;
  usenet: SavedUsenet | undefined;
  downloadFolder: string | undefined;
  mount: SavedMount | undefined;
}

type Fields = Record<string, unknown>;

const nonEmpty = (value: string | undefined): string | undefined => (value === "" ? undefined : value);

const text = (record: Fields, key: string): string => optStr(record, key) ?? "";

const count = (record: Fields, key: string): string => {
  const value = optNum(record, key);
  return value === undefined || value === 0 ? "" : String(value);
};

const first = (config: Fields, key: string, path: string): Fields | undefined => {
  const value = config[key];
  if (value === undefined || value === null) {
    return undefined;
  }
  const [head] = asArray(value, path);
  return head === undefined ? undefined : asRecord(head, `${path}[0]`);
};

const readUsenet = (config: Fields): SavedUsenet | undefined => {
  const server = Array.isArray(config["usenet"]) ? first(config, "usenet", "usenet") : undefined;
  if (server === undefined) {
    return undefined;
  }
  return {
    host: text(server, "host"),
    port: count(server, "port"),
    username: text(server, "username"),
    password: text(server, "password"),
    maxConnections: count(server, "max_connections"),
    readerConnections: count(server, "reader_connections"),
    ssl: optBool(server, "ssl") ?? false,
  };
};

const enabled = (config: Fields, key: string): Fields | undefined => {
  const value = config[key];
  if (value === undefined || value === null) {
    return undefined;
  }
  const mount = asRecord(value, key);
  return optBool(mount, "enabled") === true ? mount : undefined;
};

const readMount = (config: Fields): SavedMount | undefined => {
  const dfs = enabled(config, "dfs");
  if (dfs !== undefined) {
    return { type: "dfs", mountPath: text(dfs, "mount_path"), cacheDir: text(dfs, "cache_dir"), bufferSize: undefined };
  }
  const rclone = enabled(config, "rclone");
  if (rclone === undefined) {
    return undefined;
  }
  return {
    type: "rclone",
    mountPath: text(rclone, "mount_path"),
    cacheDir: text(rclone, "vfs_cache_dir"),
    bufferSize: nonEmpty(optStr(rclone, "buffer_size")),
  };
};

const parseSetupConfig = (value: unknown): SetupConfig => {
  const config = asRecord(value, "config");
  const debrid = first(config, "debrids", "debrids");
  return {
    authUsername: nonEmpty(optStr(config, "auth_username")),
    debrid: debrid === undefined ? undefined : { provider: text(debrid, "provider"), apiKey: text(debrid, "api_key") },
    usenet: readUsenet(config),
    downloadFolder: nonEmpty(optStr(config, "download_folder")),
    mount: readMount(config),
  };
};

export type { SetupConfig };
export { parseSetupConfig };
