interface Condition {
  field: string;
  operator: string;
  value: string;
  case_sensitive: boolean;
}

interface Folder {
  name: string;
  match: string;
  include_bad: boolean;
  conditions: Condition[];
}

interface FieldDef {
  value: string;
  label: string;
  help: string;
  operators: readonly (readonly [string, string])[];
  placeholder?: string;
}

interface Sample {
  key: string;
  label: string;
  description: string;
  condition: Condition;
}

export type { Condition, FieldDef, Folder, Sample };
