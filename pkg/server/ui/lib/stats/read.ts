import { asArray, asRecord } from "#ui/lib/common/json";

const optRecord = (record: Record<string, unknown>, key: string): Record<string, unknown> | undefined => {
  const value = record[key];
  return value === undefined || value === null ? undefined : asRecord(value, key);
};

const recordOrEmpty = (record: Record<string, unknown>, key: string): Record<string, unknown> =>
  optRecord(record, key) ?? {};

const listOrEmpty = (record: Record<string, unknown>, key: string): readonly unknown[] => {
  const value = record[key];
  return value === undefined || value === null ? [] : asArray(value, key);
};

export { listOrEmpty, optRecord, recordOrEmpty };
