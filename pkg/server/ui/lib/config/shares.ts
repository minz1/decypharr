import { asRecord } from "#ui/lib/common/json";
import {
  assignFields,
  checkedOf,
  leadingInt,
  named,
  prefixed,
  setChecked,
  splitList,
  textList,
} from "#ui/lib/config/form";
import type { Section } from "#ui/lib/config/section";

const NETWORK_SEPARATOR = /[\s,;]+/u;
const DEFAULT_NFS_PORT = 20_490;
const DEFAULT_SMB_PORT = 1445;

const recordOrNothing = (value: unknown, path: string): Record<string, unknown> | undefined =>
  value === undefined || value === null ? undefined : asRecord(value, path);

const assignNetworks = (doc: Document, prefix: string, value: unknown): void => {
  const control = named(doc, `${prefix}allowed_networks`);
  if (control !== undefined && Array.isArray(value)) {
    control.value = textList(value, `${prefix}allowed_networks`).join("\n");
  }
};

const populateNFS = (doc: Document, value: unknown): void => {
  const nfs = recordOrNothing(value, "nfs");
  if (nfs === undefined) {
    return;
  }
  setChecked(named(doc, "nfs.enabled"), Boolean(nfs["enabled"]));
  assignFields(doc, nfs, ["nfs.bind_address", "nfs.port"]);
  assignNetworks(doc, "nfs.", nfs["allowed_networks"]);
};

const populateSMB = (doc: Document, value: unknown): void => {
  const smb = recordOrNothing(value, "smb");
  if (smb === undefined) {
    return;
  }
  setChecked(named(doc, "smb.enabled"), Boolean(smb["enabled"]));
  setChecked(named(doc, "smb.require_signing"), Boolean(smb["require_signing"]));
  assignFields(doc, smb, prefixed("smb.", ["bind_address", "port", "share_name", "username", "password"]));
  assignNetworks(doc, "smb.", smb["allowed_networks"]);
};

const populateShareCache = (doc: Document, value: unknown): void => {
  const cache = recordOrNothing(value, "share_cache");
  if (cache === undefined) {
    return;
  }
  setChecked(named(doc, "share_cache.enabled"), cache["enabled"] === true);
  assignFields(doc, cache, prefixed("share_cache.", ["dir", "max_size", "max_age", "chunk_size", "read_ahead"]));
};

type Values = (name: string, fallback?: string) => string;

const valuesOf =
  (doc: Document, prefix: string): Values =>
  (name, fallback = "") => {
    const value = named(doc, `${prefix}${name}`)?.value;
    return value === undefined || value === "" ? fallback : value;
  };

const portOr = (text: string, fallback: number): number => {
  const port = leadingInt(text);
  return Number.isNaN(port) ? fallback : port;
};

const collectNFS = (doc: Document): Record<string, unknown> => {
  const value = valuesOf(doc, "nfs.");
  return {
    enabled: checkedOf(named(doc, "nfs.enabled")) ?? false,
    bind_address: value("bind_address", "0.0.0.0"),
    port: portOr(value("port"), DEFAULT_NFS_PORT),
    allowed_networks: splitList(value("allowed_networks"), NETWORK_SEPARATOR),
  };
};

const collectSMB = (doc: Document): Record<string, unknown> => {
  const value = valuesOf(doc, "smb.");
  return {
    enabled: checkedOf(named(doc, "smb.enabled")) ?? false,
    bind_address: value("bind_address", "0.0.0.0"),
    port: portOr(value("port"), DEFAULT_SMB_PORT),
    share_name: value("share_name", "decypharr"),
    username: value("username"),
    password: value("password"),
    require_signing: checkedOf(named(doc, "smb.require_signing")) ?? false,
    allowed_networks: splitList(value("allowed_networks"), NETWORK_SEPARATOR),
  };
};

const collectShareCache = (doc: Document): Record<string, unknown> => {
  const value = (name: string): string => named(doc, `share_cache.${name}`)?.value.trim() ?? "";
  return {
    enabled: checkedOf(named(doc, "share_cache.enabled")) ?? false,
    dir: value("dir"),
    max_size: value("max_size"),
    max_age: value("max_age"),
    chunk_size: value("chunk_size"),
    read_ahead: value("read_ahead"),
  };
};

const shareSections: readonly Section[] = [
  { prefix: "nfs", populate: populateNFS, collect: collectNFS },
  { prefix: "smb", populate: populateSMB, collect: collectSMB },
  { prefix: "share_cache", populate: populateShareCache, collect: collectShareCache },
];

export { shareSections, collectNFS, collectSMB, collectShareCache, populateNFS, populateSMB, populateShareCache };
