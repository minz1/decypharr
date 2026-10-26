// JSON's null, for a request field that must clear a stored value; unicorn/no-null bans the literal.
const JSON_NULL: unknown = JSON.parse("null");

class ShapeError extends Error {
  public override readonly name = "ShapeError";
}

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const readJSON = async (response: Response): Promise<unknown> => {
  const text = await response.text();
  if (!response.ok) {
    const reason = text.trim();
    throw new Error(reason === "" ? `${response.status} ${response.statusText}`.trim() : reason);
  }
  try {
    return JSON.parse(text);
  } catch {
    throw new ShapeError("response is not JSON");
  }
};

const asRecord = (value: unknown, path: string): Record<string, unknown> => {
  if (!isRecord(value)) {
    throw new ShapeError(`${path}: expected an object`);
  }
  return value;
};

const asArray = (value: unknown, path: string): readonly unknown[] => {
  if (!Array.isArray(value)) {
    throw new ShapeError(`${path}: expected an array`);
  }
  return value;
};

const str = (record: Record<string, unknown>, key: string): string => {
  const value = record[key];
  if (typeof value !== "string") {
    throw new ShapeError(`${key}: expected a string`);
  }
  return value;
};

const num = (record: Record<string, unknown>, key: string): number => {
  const value = record[key];
  if (typeof value !== "number") {
    throw new ShapeError(`${key}: expected a number`);
  }
  return value;
};

const bool = (record: Record<string, unknown>, key: string): boolean => {
  const value = record[key];
  if (typeof value !== "boolean") {
    throw new ShapeError(`${key}: expected a boolean`);
  }
  return value;
};

type Reader<Value> = (record: Record<string, unknown>, key: string) => Value;

const optional = <Value>(read: Reader<Value>, record: Record<string, unknown>, key: string): Value | undefined =>
  Object.hasOwn(record, key) && record[key] !== null ? read(record, key) : undefined;

const optStr = (record: Record<string, unknown>, key: string): string | undefined => optional(str, record, key);

const optNum = (record: Record<string, unknown>, key: string): number | undefined => optional(num, record, key);

const optBool = (record: Record<string, unknown>, key: string): boolean | undefined => optional(bool, record, key);

const strOrEmpty = (record: Record<string, unknown>, key: string): string => optStr(record, key) ?? "";

const numOrZero = (record: Record<string, unknown>, key: string): number => optNum(record, key) ?? 0;

const optList = (record: Record<string, unknown>, key: string): readonly unknown[] =>
  record[key] === undefined || record[key] === null ? [] : asArray(record[key], key);

export {
  JSON_NULL,
  ShapeError,
  asArray,
  asRecord,
  bool,
  isRecord,
  num,
  numOrZero,
  optBool,
  optList,
  optNum,
  optStr,
  readJSON,
  str,
  strOrEmpty,
};
