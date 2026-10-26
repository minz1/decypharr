const BYTE_UNITS = ["B", "KB", "MB", "GB", "TB", "PB"] as const;
const LAST_BYTE_UNIT = BYTE_UNITS.indexOf("PB");
const KIBI = 1024;
const BYTE_DECIMALS = 2;
const DURATION_UNITS = [
  { label: "d", seconds: 86_400 },
  { label: "h", seconds: 3600 },
  { label: "m", seconds: 60 },
  { label: "s", seconds: 1 },
] as const;
const DURATION_PARTS = 2;
const MIN_COUNT = 1;

const formatBytes = (bytes: number): string => {
  if (!bytes) {
    return "0 B";
  }
  const exponent = Math.min(Math.floor(Math.log(bytes) / Math.log(KIBI)), LAST_BYTE_UNIT);
  return `${Number((bytes / KIBI ** exponent).toFixed(BYTE_DECIMALS))} ${BYTE_UNITS[exponent] ?? "B"}`;
};

const formatSpeed = (bytesPerSecond: number): string => `${formatBytes(bytesPerSecond)}/s`;

const formatDuration = (seconds: number): string => {
  const parts: string[] = [];
  let remaining = seconds;
  for (const unit of DURATION_UNITS) {
    const count = Math.floor(remaining / unit.seconds);
    if (count >= MIN_COUNT) {
      parts.push(`${count}${unit.label}`);
      remaining %= unit.seconds;
    }
  }
  const text = parts.toSpliced(DURATION_PARTS).join(" ");
  return text === "" ? "0s" : text;
};

const debounce = <Args extends unknown[]>(
  action: (...args: Args) => void,
  waitMs: number,
): ((...args: Args) => void) => {
  const pending: { timer?: ReturnType<typeof setTimeout> } = {};
  return (...args: Args): void => {
    clearTimeout(pending.timer);
    pending.timer = setTimeout(() => {
      action(...args);
    }, waitMs);
  };
};

export { debounce, formatBytes, formatDuration, formatSpeed };
