const BYTE_UNITS = ["B", "KB", "MB", "GB", "TB", "PB"] as const;
const KIBI = 1024;
const BYTE_DECIMALS = 2;
const DURATION_UNITS = [
  { label: "d", seconds: 86_400 },
  { label: "h", seconds: 3600 },
  { label: "m", seconds: 60 },
  { label: "s", seconds: 1 },
] as const;
const DURATION_PARTS = 2;

const formatBytes = (bytes: number): string => {
  if (!bytes) {
    return "0 B";
  }
  const exponent = Math.min(Math.floor(Math.log(bytes) / Math.log(KIBI)), BYTE_UNITS.length - 1);
  return `${Number((bytes / KIBI ** exponent).toFixed(BYTE_DECIMALS))} ${BYTE_UNITS[exponent] ?? "B"}`;
};

const formatSpeed = (bytesPerSecond: number): string => `${formatBytes(bytesPerSecond)}/s`;

const formatDuration = (seconds: number): string => {
  const parts: string[] = [];
  let remaining = seconds;
  for (const unit of DURATION_UNITS) {
    const count = Math.floor(remaining / unit.seconds);
    if (count > 0) {
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
  let timer: ReturnType<typeof setTimeout> | undefined = undefined;
  return (...args: Args): void => {
    clearTimeout(timer);
    timer = setTimeout(() => {
      action(...args);
    }, waitMs);
  };
};

const orElse = (value: string, fallback: string): string => (value === "" ? fallback : value);

export { debounce, formatBytes, formatDuration, formatSpeed, orElse };
