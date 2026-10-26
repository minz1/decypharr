import { asArray } from "#ui/lib/common/json";

type Control = HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement;

const isControl = (element: Element | null): element is Control =>
  element instanceof HTMLInputElement || element instanceof HTMLSelectElement || element instanceof HTMLTextAreaElement;

const named = (root: ParentNode, name: string): Control | undefined => {
  const found = root.querySelector(`[name="${name}"]`);
  return isControl(found) ? found : undefined;
};

const requireNamed = (root: ParentNode, name: string): Control => {
  const found = named(root, name);
  if (found === undefined) {
    throw new Error(`missing [name="${name}"]`);
  }
  return found;
};

const withId = (doc: Document, id: string): Control | undefined => {
  const found = doc.querySelector(`#${CSS.escape(id)}`);
  return isControl(found) ? found : undefined;
};

const isCheckbox = (control: Control): control is HTMLInputElement =>
  control instanceof HTMLInputElement && control.type === "checkbox";

const toText = (value: unknown): string => {
  if (typeof value === "string") {
    return value;
  }
  if (typeof value === "number" || typeof value === "boolean") {
    return String(value);
  }
  return Array.isArray(value) ? value.map((item) => toText(item)).join(",") : "";
};

const assign = (control: Control | undefined, value: unknown): void => {
  if (control === undefined || value === undefined) {
    return;
  }
  if (isCheckbox(control)) {
    control.checked = Boolean(value);
    return;
  }
  control.value = toText(value);
};

const assignFields = (root: ParentNode, record: Record<string, unknown>, fields: readonly string[]): void => {
  for (const field of fields) {
    assign(named(root, field), record[field.slice(field.lastIndexOf(".") + 1)]);
  }
};

const setChecked = (control: Control | undefined, value: boolean): void => {
  if (control instanceof HTMLInputElement) {
    control.checked = value;
  }
};

const setText = (control: Control | undefined, value: string): void => {
  if (control !== undefined) {
    control.value = value;
  }
};

const numberText = (value: number | undefined): string => (value === undefined || value === 0 ? "" : String(value));

const textList = (value: unknown, path: string): string[] =>
  value === undefined || value === null ? [] : asArray(value, path).map((item) => toText(item));

const checkedOf = (control: Control | undefined): boolean | undefined =>
  control instanceof HTMLInputElement ? control.checked : undefined;

const truthyNumber = (parsed: number, fallback: number): number =>
  Number.isNaN(parsed) || parsed === 0 ? fallback : parsed;

const LEADING_INT = /^\s*[+-]?\d+/u;
const LEADING_FLOAT = /^\s*[+-]?(?:Infinity|(?:\d+\.?\d*|\.\d+)(?:[Ee][+-]?\d+)?)/u;

// Number.parseInt and parseFloat semantics for decimal text, without the radix and prefix cases.
const leadingNumber = (text: string, pattern: RegExp): number => {
  const [found] = pattern.exec(text) ?? [];
  return found === undefined ? Number.NaN : Number(found);
};

const leadingInt = (text: string): number => leadingNumber(text, LEADING_INT);

const intOr = (text: string | undefined, fallback: number): number => truthyNumber(leadingInt(text ?? ""), fallback);

const floatOr = (text: string | undefined, fallback: number): number =>
  truthyNumber(leadingNumber(text ?? "", LEADING_FLOAT), fallback);

const splitList = (text: string, separator: RegExp | string): string[] =>
  text
    .split(separator)
    .map((item) => item.trim())
    .filter((item) => item !== "");

const lines = (text?: string): string[] => splitList(text ?? "", "\n");

const orDefault = (text: string | undefined, fallback: string): string =>
  text === undefined || text === "" ? fallback : text;

const prefixed = (prefix: string, fields: readonly string[]): string[] => fields.map((field) => `${prefix}${field}`);

export type { Control };
export {
  assign,
  assignFields,
  checkedOf,
  floatOr,
  intOr,
  isCheckbox,
  leadingInt,
  lines,
  named,
  numberText,
  orDefault,
  prefixed,
  requireNamed,
  setChecked,
  setText,
  splitList,
  textList,
  toText,
  withId,
};
