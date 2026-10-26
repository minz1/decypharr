import { asRecord, isRecord, str } from "#ui/lib/common/json";

type Leaves = Map<string, unknown[]>;

const addLeaf = (leaves: Leaves, path: string, value: unknown): void => {
  const values = leaves.get(path) ?? [];
  values.push(value);
  leaves.set(path, values);
};

const collectLeaves = (value: unknown, path: string, leaves: Leaves): void => {
  if (isRecord(value)) {
    for (const [key, item] of Object.entries(value)) {
      collectLeaves(item, path === "" ? key : `${path}.${key}`, leaves);
    }
    return;
  }
  if (Array.isArray(value) && value.length > 0 && value.every((item) => isRecord(item))) {
    for (const item of value) {
      collectLeaves(item, `${path}[]`, leaves);
    }
    return;
  }
  if (value !== undefined) {
    addLeaf(leaves, path, value);
  }
};

const leavesOf = (value: unknown): Leaves => {
  const leaves: Leaves = new Map();
  collectLeaves(value, "", leaves);
  return leaves;
};

const readExclusions = (value: unknown): string[] => {
  const record = asRecord(value, "exclusions");
  return Object.keys(record).filter((key) => str(record, key) !== "");
};

const isExcluded = (exclusions: readonly string[], path: string): boolean =>
  exclusions.some((key) => key === path || (key.endsWith(".") && path.startsWith(key)));

const describeLeaf = (path: string, values: unknown): string => `${path}: ${JSON.stringify(values)}`;

interface Leaf {
  path: string;
  want: unknown[];
  sent: unknown[] | undefined;
}

const leafMismatch = (exclusions: readonly string[], { path, want, sent }: Leaf): string[] => {
  if (isExcluded(exclusions, path)) {
    return sent === undefined ? [] : [`excluded ${describeLeaf(path, sent)}`];
  }
  return JSON.stringify(sent) === JSON.stringify(want)
    ? []
    : [`want ${describeLeaf(path, want)}, got ${JSON.stringify(sent)}`];
};

const roundTripMismatches = (sample: unknown, collected: unknown, exclusionMap: unknown): string[] => {
  const exclusions = readExclusions(exclusionMap);
  const want = leavesOf(sample);
  const got = leavesOf(collected);
  const missed = [...want].flatMap(([path, values]) =>
    leafMismatch(exclusions, { path, want: values, sent: got.get(path) }),
  );
  const echoed = [...got]
    .filter(([path]) => !want.has(path) && isExcluded(exclusions, path))
    .map(([path, values]) => `excluded ${describeLeaf(path, values)}`);
  return [...missed, ...echoed];
};

export { roundTripMismatches };
