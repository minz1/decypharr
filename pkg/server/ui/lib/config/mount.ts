import { asRecord, optStr } from "#ui/lib/common/json";
import { assign, assignFields, leadingInt, named, prefixed, requireNamed } from "#ui/lib/config/form";
import type { Section } from "#ui/lib/config/section";

const RCLONE_FIELDS = [
  "port",
  "cache_dir",
  "transfers",
  "vfs_cache_mode",
  "vfs_cache_max_size",
  "vfs_cache_max_age",
  "vfs_cache_poll_interval",
  "vfs_read_chunk_size",
  "vfs_read_chunk_size_limit",
  "buffer_size",
  "bw_limit",
  "uid",
  "gid",
  "vfs_read_ahead",
  "attr_timeout",
  "dir_cache_time",
  "poll_interval",
  "umask",
  "no_modtime",
  "no_checksum",
  "log_level",
  "vfs_cache_min_free_space",
  "vfs_fast_fingerprint",
  "vfs_read_chunk_streams",
  "async_read",
  "use_mmap",
  "vfs_disk_space_total",
] as const;

const DFS_FIELDS = [
  "cache_dir",
  "disk_cache_size",
  "buffer_memory",
  "cache_expiry",
  "cache_cleanup_interval",
  "chunk_size",
  "read_ahead_size",
  "daemon_timeout",
  "uid",
  "gid",
  "umask",
  "disable_cache",
  "drop_behind_margin",
  "fuse_max_background",
  "fuse_max_read_ahead",
] as const;

const EXTERNAL_FIELDS = ["rc_url", "rc_username", "rc_password"] as const;

const TYPE_RADIOS = 'input[name="mount.type"]';

const typeRadios = (doc: Document): HTMLInputElement[] => [...doc.querySelectorAll<HTMLInputElement>(TYPE_RADIOS)];

const populateMount = (doc: Document, value: unknown): void => {
  if (value === undefined || value === null) {
    return;
  }
  const mount = asRecord(value, "mount");
  const type = optStr(mount, "type");
  const radio = typeRadios(doc).find((candidate) => type !== undefined && type !== "" && candidate.value === type);
  if (radio !== undefined) {
    radio.checked = true;
    radio.dispatchEvent(new Event("change"));
  }
  assign(named(doc, "mount.mount_path"), mount["mount_path"]);
};

const populateFields =
  (prefix: string, fields: readonly string[]) =>
  (doc: Document, value: unknown): void => {
    if (value !== undefined && value !== null) {
      assignFields(doc, asRecord(value, prefix), prefixed(prefix, fields));
    }
  };

const populateRclone = populateFields("mount.rclone.", RCLONE_FIELDS);
const populateDFS = populateFields("mount.dfs.", DFS_FIELDS);
const populateExternalRclone = populateFields("mount.external_rclone.", EXTERNAL_FIELDS);

const collectMount = (doc: Document): Record<string, unknown> => ({
  type: typeRadios(doc).find((radio) => radio.checked)?.value ?? "none",
  mount_path: requireNamed(doc, "mount.mount_path").value,
});

type Reader = <Fallback>(name: string, fallback: Fallback) => Fallback | boolean | number | string;

const fieldReader =
  (doc: Document, prefix: string): Reader =>
  (name, fallback) => {
    const control = named(doc, `${prefix}${name}`);
    if (!(control instanceof HTMLInputElement) || (control.type !== "checkbox" && control.type !== "number")) {
      return control === undefined || control.value === "" ? fallback : control.value;
    }
    if (control.type === "checkbox") {
      return control.checked;
    }
    const parsed = leadingInt(control.value);
    return Number.isNaN(parsed) ? 0 : parsed;
  };

const RCLONE_TRANSFERS = 8;

const collectRclone = (doc: Document): Record<string, unknown> => {
  const read = fieldReader(doc, "mount.rclone.");
  return {
    port: read("port", "5572"),
    buffer_size: read("buffer_size", ""),
    bw_limit: read("bw_limit", ""),
    cache_dir: read("cache_dir", ""),
    transfers: read("transfers", RCLONE_TRANSFERS),
    vfs_cache_mode: read("vfs_cache_mode", "off"),
    vfs_cache_max_age: read("vfs_cache_max_age", "1h"),
    vfs_cache_max_size: read("vfs_cache_max_size", ""),
    vfs_cache_poll_interval: read("vfs_cache_poll_interval", "1m"),
    vfs_read_chunk_size: read("vfs_read_chunk_size", ""),
    vfs_read_chunk_size_limit: read("vfs_read_chunk_size_limit", "off"),
    vfs_cache_min_free_space: read("vfs_cache_min_free_space", ""),
    vfs_disk_space_total: read("vfs_disk_space_total", ""),
    vfs_fast_fingerprint: read("vfs_fast_fingerprint", false),
    vfs_read_chunk_streams: read("vfs_read_chunk_streams", 0),
    use_mmap: read("use_mmap", false),
    async_read: read("async_read", true),
    uid: read("uid", 0),
    gid: read("gid", 0),
    umask: read("umask", ""),
    vfs_read_ahead: read("vfs_read_ahead", ""),
    attr_timeout: read("attr_timeout", "1s"),
    dir_cache_time: read("dir_cache_time", "5m"),
    no_modtime: read("no_modtime", false),
    no_checksum: read("no_checksum", false),
    log_level: read("log_level", "INFO"),
  };
};

const collectDFS = (doc: Document): Record<string, unknown> => {
  const read = fieldReader(doc, "mount.dfs.");
  return {
    cache_dir: read("cache_dir", ""),
    disk_cache_size: read("disk_cache_size", ""),
    buffer_memory: read("buffer_memory", ""),
    cache_expiry: read("cache_expiry", ""),
    cache_cleanup_interval: read("cache_cleanup_interval", ""),
    chunk_size: read("chunk_size", ""),
    read_ahead_size: read("read_ahead_size", ""),
    daemon_timeout: read("daemon_timeout", ""),
    uid: read("uid", 0),
    gid: read("gid", 0),
    umask: read("umask", ""),
    disable_cache: read("disable_cache", false),
    drop_behind_margin: read("drop_behind_margin", ""),
    fuse_max_background: read("fuse_max_background", 0),
    fuse_max_read_ahead: read("fuse_max_read_ahead", ""),
  };
};

const collectExternalRclone = (doc: Document): Record<string, unknown> => ({
  rc_url: named(doc, "mount.external_rclone.rc_url")?.value ?? "",
  rc_username: named(doc, "mount.external_rclone.rc_username")?.value ?? "",
  rc_password: named(doc, "mount.external_rclone.rc_password")?.value ?? "",
});

const mountSections: readonly Section[] = [
  { prefix: "mount", populate: populateMount, collect: collectMount },
  { prefix: "mount.dfs", populate: populateDFS, collect: collectDFS },
  { prefix: "mount.rclone", populate: populateRclone, collect: collectRclone },
  { prefix: "mount.external_rclone", populate: populateExternalRclone, collect: collectExternalRclone },
];

export {
  mountSections,
  collectDFS,
  collectExternalRclone,
  collectMount,
  collectRclone,
  populateDFS,
  populateExternalRclone,
  populateMount,
  populateRclone,
};
