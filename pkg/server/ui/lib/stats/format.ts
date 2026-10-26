const SECONDS_PER_MINUTE = 60;
const SECONDS_PER_HOUR = 3600;
const SECONDS_PER_DAY = 86_400;
const PERCENT = 100;

const formatNumber = (value: number): string => value.toLocaleString();

const uptimeParts = (seconds: number): [number, number, number, number] => [
  Math.floor(seconds / SECONDS_PER_DAY),
  Math.floor((seconds % SECONDS_PER_DAY) / SECONDS_PER_HOUR),
  Math.floor((seconds % SECONDS_PER_HOUR) / SECONDS_PER_MINUTE),
  Math.floor(seconds % SECONDS_PER_MINUTE),
];

const formatUptime = (seconds: number): string => {
  if (!seconds) {
    return "-";
  }
  const [days, hours, minutes, secs] = uptimeParts(seconds);
  if (days > 0) {
    return `${days}d ${hours}h ${minutes}m`;
  }
  if (hours > 0) {
    return `${hours}h ${minutes}m ${secs}s`;
  }
  return minutes > 0 ? `${minutes}m ${secs}s` : `${secs}s`;
};

const formatPercent = (ratio: number): string => `${(ratio * PERCENT).toFixed(1)}%`;

const orElse = (value: string, fallback: string): string => (value === "" ? fallback : value);

export { PERCENT, formatNumber, formatPercent, formatUptime, orElse };
