import { RESERVED_FOLDER_NAMES, validateFolder } from "#ui/lib/config/vf-model";
import type { Folder } from "#ui/lib/config/vf-types";

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const records = (value: unknown): Record<string, unknown>[] =>
  Array.isArray(value) ? value.filter((item) => isRecord(item)) : [];

const record = (value: unknown): Record<string, unknown> => (isRecord(value) ? value : {});

const text = (value: unknown): string => (typeof value === "string" ? value : "");

const isValidUrl = (value: string): boolean => URL.canParse(value);

const asFolder = (value: Record<string, unknown>): Folder => ({
  name: text(value["name"]),
  match: text(value["match"]),
  include_bad: value["include_bad"] === true,
  conditions: records(value["conditions"]).map((condition) => ({
    field: text(condition["field"]),
    operator: text(condition["operator"]),
    value: text(condition["value"]),
    case_sensitive: condition["case_sensitive"] === true,
  })),
});

const debridErrors = (settings: Record<string, unknown>): string[] =>
  records(settings["debrids"]).flatMap((debrid, index) =>
    text(debrid["name"]) === "" || text(debrid["api_key"]) === "" || text(debrid["provider"]) === ""
      ? [`Debrid service #${index + 1}: Name, API key are required`]
      : [],
  );

const arrErrors = (settings: Record<string, unknown>): string[] =>
  records(settings["arrs"]).flatMap((arr, index) => {
    const host = text(arr["host"]);
    const checks: [boolean, string][] = [
      [text(arr["name"]) === "" || host === "", `Arr service #${index + 1}: Name and host are required`],
      [host !== "" && !isValidUrl(host), `Arr service #${index + 1}: Invalid host URL format`],
    ];
    return checks.filter(([failed]) => failed).map(([, message]) => message);
  });

const nameErrors = (name: string, known: { reserved: ReadonlySet<string>; seen: Set<string> }): string[] => {
  const normalized = name.trim().toLowerCase();
  if (normalized === "") {
    return [];
  }
  const checks: [boolean, string][] = [
    [
      known.reserved.has(normalized),
      `Virtual folder "${name}": the name conflicts with a built-in or provider folder.`,
    ],
    [known.seen.has(normalized), `Virtual folder "${name}" is defined more than once.`],
  ];
  known.seen.add(normalized);
  return checks.filter(([failed]) => failed).map(([, message]) => message);
};

const folderErrors = (settings: Record<string, unknown>): string[] => {
  const reserved = new Set([
    ...RESERVED_FOLDER_NAMES,
    ...records(settings["debrids"]).map((debrid) => text(debrid["name"]).trim().toLowerCase()),
  ]);
  const seen = new Set<string>();
  const errors: string[] = [];
  for (const [index, value] of records(settings["virtual_folders"]).entries()) {
    const folder = asFolder(value);
    errors.push(
      ...validateFolder(folder).map((error) => `Virtual folder #${index + 1}: ${error}`),
      ...nameErrors(folder.name, { reserved, seen }),
    );
  }
  return errors;
};

const mountErrors = (settings: Record<string, unknown>): string[] => {
  const mount = record(settings["mount"]);
  const type = text(mount["type"]);
  if (type === "") {
    return ["Mounts: a mount type must be selected"];
  }
  return type !== "none" && text(mount["mount_path"]) === ""
    ? ['Mounts: mount path is required unless the mount type is "No Mount"']
    : [];
};

const repairErrors = (settings: Record<string, unknown>): string[] => {
  const repair = record(settings["repair"]);
  return repair["enabled"] === true && text(repair["schedule"]) === ""
    ? ["Repair: schedule is required when Repair is enabled"]
    : [];
};

const smbErrors = (settings: Record<string, unknown>): string[] => {
  const smb = record(settings["smb"]);
  if (smb["enabled"] !== true) {
    return [];
  }
  const missing = [
    ...(text(smb["username"]) === "" ? ["username"] : []),
    ...(text(smb["password"]) === "" ? ["password"] : []),
  ];
  return missing.length === 0
    ? []
    : [`Shares > SMB: ${missing.join(" and ")} ${missing.length > 1 ? "are" : "is"} required when SMB is enabled`];
};

const validateSettings = (settings: Record<string, unknown>): string[] => [
  ...debridErrors(settings),
  ...arrErrors(settings),
  ...folderErrors(settings),
  ...mountErrors(settings),
  ...repairErrors(settings),
  ...smbErrors(settings),
];

export { validateSettings };
