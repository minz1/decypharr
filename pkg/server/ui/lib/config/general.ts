import { asRecord } from "#ui/lib/common/json";
import { assignFields, checkedOf, intOr, named, orDefault, requireNamed, splitList, toText } from "#ui/lib/config/form";

const GENERAL_FIELDS = [
  "log_level",
  "url_base",
  "bind_address",
  "port",
  "min_file_size",
  "max_file_size",
  "folder_naming",
  "refresh_dirs",
  "disable_webdav",
  "enable_webdav_auth",
  "app_url",
  "retries",
  "tls_ca_file",
  "shared_dir_mode",
  "shared_file_mode",
] as const;

const DOWNLOAD_FIELDS = [
  "remove_stalled_after",
  "nzb_user_agent",
  "download_folder",
  "refresh_interval",
  "max_active_downloads",
  "skip_pre_cache",
  "always_rm_tracker_urls",
  "default_download_action",
  "allow_samples",
] as const;

const DEFAULT_MAX_ACTIVE_DOWNLOADS = 5;

const assignJoined = (doc: Document, name: string, value: unknown): void => {
  if (Array.isArray(value)) {
    requireNamed(doc, name).value = value.map((item) => toText(item)).join(", ");
  }
};

const populateGeneral = (doc: Document, value: unknown): void => {
  const config = asRecord(value, "config");
  assignFields(doc, config, GENERAL_FIELDS);
  assignJoined(doc, "allowed_file_types", config["allowed_file_types"]);
  assignFields(doc, config, DOWNLOAD_FIELDS);
  assignJoined(doc, "categories", config["categories"]);
};

const text = (doc: Document, name: string): string => requireNamed(doc, name).value;

const checked = (doc: Document, name: string): boolean | undefined => checkedOf(requireNamed(doc, name));

const commaList = (doc: Document, name: string): string[] => splitList(text(doc, name), ",");

const collectGeneral = (doc: Document): Record<string, unknown> => ({
  log_level: text(doc, "log_level"),
  url_base: text(doc, "url_base"),
  bind_address: text(doc, "bind_address"),
  app_url: text(doc, "app_url"),
  port: text(doc, "port"),
  allowed_file_types: commaList(doc, "allowed_file_types"),
  min_file_size: text(doc, "min_file_size"),
  max_file_size: text(doc, "max_file_size"),
  remove_stalled_after: orDefault(text(doc, "remove_stalled_after"), "10m"),
  nzb_user_agent: text(doc, "nzb_user_agent"),
  download_folder: text(doc, "download_folder"),
  refresh_interval: orDefault(text(doc, "refresh_interval"), "30s"),
  default_download_action: orDefault(named(doc, "default_download_action")?.value, "symlink"),
  max_active_downloads: intOr(text(doc, "max_active_downloads"), DEFAULT_MAX_ACTIVE_DOWNLOADS),
  skip_pre_cache: checked(doc, "skip_pre_cache"),
  always_rm_tracker_urls: checked(doc, "always_rm_tracker_urls"),
  folder_naming: orDefault(named(doc, "folder_naming")?.value, ""),
  disable_webdav: checked(doc, "disable_webdav"),
  enable_webdav_auth: checked(doc, "enable_webdav_auth"),
  refresh_dirs: orDefault(named(doc, "refresh_dirs")?.value, ""),
  retries: intOr(text(doc, "retries"), 0),
  tls_ca_file: text(doc, "tls_ca_file"),
  shared_dir_mode: text(doc, "shared_dir_mode"),
  shared_file_mode: text(doc, "shared_file_mode"),
  allow_samples: checked(doc, "allow_samples"),
  categories: commaList(doc, "categories"),
});

export { collectGeneral, populateGeneral };
