import type { Condition, FieldDef, Folder, Sample } from "#ui/lib/config/vf-types";

const TEXT_OPERATORS = [
  ["contains", "contains"],
  ["not_contains", "does not contain"],
  ["starts_with", "starts with"],
  ["not_starts_with", "does not start with"],
  ["ends_with", "ends with"],
  ["not_ends_with", "does not end with"],
  ["equals", "is exactly"],
  ["not_equals", "is not exactly"],
  ["matches_regex", "matches regular expression"],
  ["not_matches_regex", "does not match regular expression"],
] as const;

const IS_OPERATORS = [
  ["equals", "is"],
  ["not_equals", "is not"],
] as const;

const ENTRY_NAME: FieldDef = {
  value: "entry_name",
  label: "Item name",
  help: "The release or library folder name",
  operators: TEXT_OPERATORS,
  placeholder: "e.g., 2160p",
};

const FIELDS: readonly FieldDef[] = [
  ENTRY_NAME,
  {
    value: "file_name",
    label: "File name inside item",
    help: "Any file path contained in the item",
    operators: TEXT_OPERATORS,
    placeholder: "e.g., S01E",
  },
  {
    value: "size",
    label: "Total item size",
    help: "The combined size of the item",
    operators: [
      ["greater_than", "is larger than"],
      ["less_than", "is smaller than"],
    ],
    placeholder: "e.g., 20GB",
  },
  {
    value: "added",
    label: "Date added",
    help: "How recently the item was added",
    operators: [["within_last", "is within the last"]],
    placeholder: "e.g., 7d",
  },
  {
    value: "file_count",
    label: "Number of files",
    help: "How many files the item contains",
    operators: [
      ["greater_than", "is more than"],
      ["less_than", "is fewer than"],
    ],
    placeholder: "e.g., 5",
  },
  { value: "protocol", label: "Source type", help: "Torrent or Usenet/NZB", operators: IS_OPERATORS },
  {
    value: "provider",
    label: "Provider",
    help: "The provider currently serving the item",
    operators: IS_OPERATORS,
    placeholder: "Provider name",
  },
  {
    value: "category",
    label: "Category",
    help: "The Arr or download category assigned to the item",
    operators: TEXT_OPERATORS,
    placeholder: "e.g., radarr",
  },
];

const EPISODE_PATTERN = String.raw`(?:S\d{1,2}E\d{1,3}|\b\d{1,2}x\d{1,3}\b)`;

const condition = (field: string, operator: string, value: string): Condition => ({
  field,
  operator,
  value,
  case_sensitive: false,
});

const SAMPLES: readonly Sample[] = [
  {
    key: "episodes",
    label: "Episode files",
    description: "Has a file named like S01E02 or 1x02",
    condition: condition("file_name", "matches_regex", EPISODE_PATTERN),
  },
  {
    key: "movies",
    label: "Likely movie",
    description: "Has no file named like S01E02 or 1x02; this is a filename-based estimate",
    condition: condition("file_name", "not_matches_regex", EPISODE_PATTERN),
  },
  {
    key: "multiple-files",
    label: "More than one file",
    description: "Contains at least two files",
    condition: condition("file_count", "greater_than", "1"),
  },
  {
    key: "4k",
    label: "4K items",
    description: "Item name contains 2160p",
    condition: condition("entry_name", "contains", "2160p"),
  },
  {
    key: "recent",
    label: "Added this week",
    description: "Was added within the last seven days",
    condition: condition("added", "within_last", "7d"),
  },
  {
    key: "torrent",
    label: "Torrent only",
    description: "Uses a torrent source",
    condition: condition("protocol", "equals", "torrent"),
  },
];

const CASE_FIELDS = new Set(["entry_name", "file_name", "provider", "category"]);
const PROTOCOLS = ["torrent", "nzb"];
const RESERVED_FOLDER_NAMES = ["__all__", "__bad__", "torrents", "nzbs", "version.txt"];
const UNPORTABLE_NAME = /[<>:"/\\|?*]/u;
const FIRST_PRINTABLE = 32;
const DELETE = 127;
const RESERVED_NAME = /^(?:CON|PRN|AUX|NUL|COM[1-9]|LPT[1-9])(?:\..*)?$/iu;
const SIZE_VALUE = /^\d+(?:\.\d+)?\s*(?:B|KB|MB|GB|TB|PB)?$/iu;
const DURATION_VALUE = /^(?=.)(?:\d+w)?(?:\d+d)?(?:\d+(?:\.\d+)?(?:h|m|s))?$/u;
const WHOLE_NUMBER = /^\d+$/u;

const fieldDef = (field: string): FieldDef => FIELDS.find((option) => option.value === field) ?? ENTRY_NAME;

const supportsOperator = (def: FieldDef, operator: string): boolean =>
  def.operators.some(([value]) => value === operator);

const firstOperator = (def: FieldDef): string => def.operators[0]?.[0] ?? "";

const normalizeCondition = (given: Partial<Condition>): Condition => {
  const def = fieldDef(given.field === undefined || given.field === "" ? "entry_name" : given.field);
  return {
    field: def.value,
    operator:
      given.operator !== undefined && supportsOperator(def, given.operator) ? given.operator : firstOperator(def),
    value: given.value ?? "",
    case_sensitive: given.case_sensitive === true,
  };
};

const refreshedCondition = (current: Condition, previousField: string, providers: readonly string[]): Condition => {
  const def = fieldDef(current.field);
  const next = { ...current, value: previousField === current.field ? current.value : "" };
  if (!supportsOperator(def, next.operator)) {
    next.operator = firstOperator(def);
  }
  if (next.field === "protocol" && !PROTOCOLS.includes(next.value)) {
    next.value = "torrent";
  }
  if (next.field === "provider" && providers.length > 0 && !providers.includes(next.value)) {
    next.value = providers[0] ?? "";
  }
  return next;
};

const REGEX_OPERATORS = new Set(["matches_regex", "not_matches_regex"]);

const isValidRegex = (pattern: string): boolean => {
  try {
    return new RegExp(pattern, "u") instanceof RegExp;
  } catch {
    return false;
  }
};

const conditionErrors = (given: Condition, index: number): string[] => {
  const label = `Condition ${index + 1}`;
  const checks: [boolean, string][] = [
    [!supportsOperator(fieldDef(given.field), given.operator), `${label} has an unsupported rule.`],
    [given.value === "", `${label} needs a value.`],
    [
      given.field === "size" && given.value !== "" && !SIZE_VALUE.test(given.value),
      `${label} needs a size such as 700MB or 20GB.`,
    ],
    [
      given.field === "added" && given.value !== "" && !DURATION_VALUE.test(given.value),
      `${label} needs a duration such as 12h, 7d, or 2w.`,
    ],
    [
      given.field === "file_count" && given.value !== "" && !WHOLE_NUMBER.test(given.value),
      `${label} needs a whole number of files.`,
    ],
    [
      REGEX_OPERATORS.has(given.operator) && given.value !== "" && !isValidRegex(given.value),
      `${label} contains an invalid regular expression.`,
    ],
  ];
  return checks.filter(([failed]) => failed).map(([, message]) => message);
};

const hasControl = (name: string): boolean =>
  Array.from({ length: name.length }, (_unused, index) => name.codePointAt(index) ?? FIRST_PRINTABLE).some(
    (code) => code < FIRST_PRINTABLE || code === DELETE,
  );

const validateFolder = (folder: Folder): string[] => {
  const checks: [boolean, string][] = [
    [folder.name === "", "Folder name is required."],
    [
      folder.name === "." || folder.name === ".." || UNPORTABLE_NAME.test(folder.name) || hasControl(folder.name),
      "Folder name contains characters that are not portable across mounts and shares.",
    ],
    [folder.name.endsWith(".") || RESERVED_NAME.test(folder.name), "Folder name is reserved by common filesystems."],
  ];
  return [
    ...checks.filter(([failed]) => failed).map(([, message]) => message),
    ...folder.conditions.flatMap((given, index) => conditionErrors(given, index)),
  ];
};

export {
  CASE_FIELDS,
  FIELDS,
  PROTOCOLS,
  RESERVED_FOLDER_NAMES,
  SAMPLES,
  fieldDef,
  normalizeCondition,
  refreshedCondition,
  validateFolder,
};
