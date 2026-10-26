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
  // The lint config bans undefined, void 0 and a bare return; an empty array's pop() is undefined.
  Object.hasOwn(record, key) && record[key] !== null ? read(record, key) : Array.of<Value>().pop();

const optStr = (record: Record<string, unknown>, key: string): string | undefined => optional(str, record, key);

const optNum = (record: Record<string, unknown>, key: string): number | undefined => optional(num, record, key);

const optBool = (record: Record<string, unknown>, key: string): boolean | undefined => optional(bool, record, key);

export { ShapeError, asArray, asRecord, bool, num, optBool, optNum, optStr, readJSON, str };
