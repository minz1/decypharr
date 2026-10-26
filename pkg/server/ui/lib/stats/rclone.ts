import { cloneTemplate } from "#ui/lib/common/dom";
import { formatBytes, formatDuration, formatSpeed } from "#ui/lib/common/format";
import { asRecord } from "#ui/lib/common/json";
import { cloneElement, field, setFields } from "#ui/lib/stats/dom";
import { PERCENT, orElse } from "#ui/lib/stats/format";
import { count, listOrEmpty, optRecord, text } from "#ui/lib/stats/read";

interface Transfer {
  name: string;
  bytes: number;
  size: number;
  speed: number;
  eta: number;
}

interface RcloneStats {
  version: { version: string; arch: string; os: string } | undefined;
  core:
    | {
        bytes: number;
        speed: number;
        transfers: number;
        errors: number;
        checks: number;
        totalChecks: number;
        elapsedTime: number;
        transferTime: number;
        transferring: Transfer[];
      }
    | undefined;
  memory: { sys: number; totalAlloc: number } | undefined;
  bandwidth: { bytesPerSecond: number; rate: string } | undefined;
}

interface TransferView {
  name: string;
  speed: string;
  progress: number;
  percent: string;
  sizes: string;
  eta: string;
}

interface RcloneView {
  version: { version: string; platform: string } | undefined;
  core: Record<string, string> | undefined;
  memory: { sys: string; heap: string } | undefined;
  transfers: TransferView[];
  bandwidth: { bytesPerSecond: string; rate: string } | undefined;
}

const parseTransfer = (value: unknown): Transfer => {
  const record = asRecord(value, "transferring");
  return {
    name: text(record, "name"),
    bytes: count(record, "bytes"),
    size: count(record, "size"),
    speed: count(record, "speed"),
    eta: count(record, "eta"),
  };
};

const parseCore = (core: Record<string, unknown> | undefined): RcloneStats["core"] =>
  core && {
    bytes: count(core, "bytes"),
    speed: count(core, "speed"),
    transfers: count(core, "transfers"),
    errors: count(core, "errors"),
    checks: count(core, "checks"),
    totalChecks: count(core, "totalChecks"),
    elapsedTime: count(core, "elapsedTime"),
    transferTime: count(core, "transferTime"),
    transferring: listOrEmpty(core, "transferring").map((transfer) => parseTransfer(transfer)),
  };

const parseRclone = (detail: Record<string, unknown>): RcloneStats => {
  const version = optRecord(detail, "version");
  const memory = optRecord(detail, "memory");
  const bandwidth = optRecord(detail, "bandwidth");
  return {
    version: version && { version: text(version, "version"), arch: text(version, "arch"), os: text(version, "os") },
    core: parseCore(optRecord(detail, "core")),
    memory: memory && { sys: count(memory, "Sys"), totalAlloc: count(memory, "TotalAlloc") },
    bandwidth: bandwidth && { bytesPerSecond: count(bandwidth, "bytesPerSecond"), rate: text(bandwidth, "rate") },
  };
};

const transferView = (transfer: Transfer): TransferView => {
  const progress = (transfer.bytes / (transfer.size || 1)) * PERCENT;
  return {
    name: orElse(transfer.name, "Unknown"),
    speed: formatSpeed(transfer.speed),
    progress,
    percent: `${progress.toFixed(1)}%`,
    sizes: `${formatBytes(transfer.bytes)} / ${formatBytes(transfer.size)}`,
    eta: `ETA: ${transfer.eta ? formatDuration(transfer.eta) : "Unknown"}`,
  };
};

const coreTexts = (core: NonNullable<RcloneStats["core"]>): Record<string, string> => ({
  transferred: formatBytes(core.bytes),
  speed: `Speed: ${formatSpeed(core.speed)}`,
  transfers: String(core.transfers),
  errors: `Errors: ${core.errors}`,
  checks: String(core.checks),
  "total-checks": `Total: ${core.totalChecks}`,
  elapsed: formatDuration(core.elapsedTime),
  "transfer-time": `Transfer: ${formatDuration(core.transferTime)}`,
});

const rcloneView = (stats: RcloneStats): RcloneView => ({
  version: stats.version && {
    version: orElse(stats.version.version, "Unknown"),
    platform: `${stats.version.arch} ${stats.version.os}`,
  },
  core: stats.core && coreTexts(stats.core),
  memory: stats.memory && { sys: formatBytes(stats.memory.sys), heap: `Heap: ${formatBytes(stats.memory.totalAlloc)}` },
  transfers: stats.core?.transferring.map((transfer) => transferView(transfer)) ?? [],
  bandwidth:
    stats.bandwidth && stats.bandwidth.rate !== "off"
      ? { bytesPerSecond: formatBytes(stats.bandwidth.bytesPerSecond), rate: formatBytes(Number(stats.bandwidth.rate)) }
      : undefined,
});

const filled = (id: string, texts: Readonly<Record<string, string>>): DocumentFragment => {
  const fragment = cloneTemplate(id);
  setFields(fragment, texts);
  return fragment;
};

const transferItem = (transfer: TransferView): HTMLElement => {
  const item = cloneElement("stats-rclone-transfer");
  setFields(item, {
    name: transfer.name,
    speed: transfer.speed,
    percent: transfer.percent,
    sizes: transfer.sizes,
    eta: transfer.eta,
  });
  const bar = field(item, "progress");
  if (bar instanceof HTMLProgressElement) {
    bar.value = transfer.progress;
  }
  return item;
};

const transfersSection = (transfers: readonly TransferView[]): HTMLElement => {
  const section = cloneElement("stats-rclone-transfers");
  field(section, "count").textContent = `Active Transfers (${transfers.length})`;
  field(section, "list").replaceChildren(...transfers.map((transfer) => transferItem(transfer)));
  return section;
};

const statsGrid = (view: RcloneView): HTMLElement => {
  const grid = cloneElement("stats-rclone-grid");
  if (view.version) {
    grid.append(filled("stats-rclone-version", view.version));
  }
  if (view.core) {
    grid.append(filled("stats-rclone-core", view.core));
  }
  if (view.memory) {
    grid.append(filled("stats-rclone-memory", view.memory));
  }
  return grid;
};

const renderRclone = (content: HTMLElement, view: RcloneView): void => {
  content.replaceChildren(statsGrid(view));
  if (view.transfers.length > 0) {
    content.append(transfersSection(view.transfers));
  }
  if (view.bandwidth) {
    content.append(filled("stats-rclone-bandwidth", view.bandwidth));
  }
};

export { parseRclone, rcloneView, renderRclone };
