import { cloneTemplate } from "#ui/lib/common/dom";
import { formatBytes, formatSpeed } from "#ui/lib/common/format";
import { optNum } from "#ui/lib/common/json";
import { field, setFields } from "#ui/lib/stats/dom";
import { PERCENT, formatNumber, orElse } from "#ui/lib/stats/format";
import { count, text } from "#ui/lib/stats/read";

interface DFSStats {
  activeFiles: number;
  totalFiles: number;
  cacheType: string;
  items: number;
  activeItems: number;
  totalSize: number;
  maxSize: number;
  utilization: number | undefined;
  activeDownloads: number;
  downloadSpeed: number;
  totalDownloaded: number;
  hits: number;
  misses: number;
  hitRate: number;
  circuitBreakers: number;
  bufferAllocated: number;
  bufferInUse: number;
}

interface Health {
  className: string;
  icon: string;
  label: string;
  desc: string;
  title: string;
}

interface PurgeTarget {
  items: number;
  size: number;
  max: number;
}

interface DFSView {
  texts: Record<string, string>;
  downloading: boolean;
  hitRateClass: string;
  usageClass: string;
  health: Health;
  purge: PurgeTarget;
}

const HIGH_USAGE = 90;
const WARN_USAGE = 75;
const GOOD_HIT_RATE = 0.8;
const FAIR_HIT_RATE = 0.5;

const parseDFS = (detail: Record<string, unknown>): DFSStats => ({
  activeFiles: count(detail, "active_files"),
  totalFiles: count(detail, "total_files"),
  cacheType: text(detail, "cache_type"),
  items: count(detail, "cache_item_count"),
  activeItems: count(detail, "cache_active_item_count"),
  totalSize: count(detail, "cache_total_size"),
  maxSize: count(detail, "cache_max_size"),
  utilization: optNum(detail, "cache_utilization"),
  activeDownloads: count(detail, "cache_active_downloads"),
  downloadSpeed: count(detail, "cache_download_speed"),
  totalDownloaded: count(detail, "cache_total_downloaded"),
  hits: count(detail, "cache_cache_hits"),
  misses: count(detail, "cache_cache_misses"),
  hitRate: count(detail, "cache_cache_hit_rate"),
  circuitBreakers: count(detail, "cache_circuit_breakers"),
  bufferAllocated: count(detail, "cache_buffer_memory_allocated"),
  bufferInUse: count(detail, "cache_buffer_memory_in_use"),
});

const usageClass = (percent: number): string => {
  if (percent > HIGH_USAGE) {
    return "text-error";
  }
  return percent > WARN_USAGE ? "text-warning" : "text-success";
};

const hitRateClass = (stats: DFSStats): string => {
  if (stats.hitRate > GOOD_HIT_RATE) {
    return "text-success";
  }
  if (stats.hitRate > FAIR_HIT_RATE) {
    return "text-warning";
  }
  return stats.hits + stats.misses === 0 ? "text-base-content/50" : "text-error";
};

const health = (breakers: number): Health =>
  breakers > 0
    ? {
        className: "text-warning",
        icon: "bi-shield-exclamation",
        label: "Attention",
        desc: `${formatNumber(breakers)} ${breakers === 1 ? "file" : "files"} temporarily paused`,
        title:
          "Decypharr paused cache reads for these files after repeated provider errors. It will retry automatically.",
      }
    : {
        className: "text-success",
        icon: "bi-shield-check",
        label: "OK",
        desc: "Cache reads normal",
        title: "Cache reads are operating normally.",
      };

const utilizationOf = (stats: DFSStats): number =>
  stats.utilization ?? (stats.maxSize > 0 ? stats.totalSize / stats.maxSize : 0);

const dfsTexts = (stats: DFSStats, usagePercent: string): Record<string, string> => ({
  "active-downloads": formatNumber(stats.activeDownloads),
  "download-speed": formatSpeed(stats.downloadSpeed),
  "download-total": `Total: ${formatBytes(stats.totalDownloaded)}`,
  "hit-rate": `${(stats.hitRate * PERCENT).toFixed(1)}%`,
  "hit-counts": `${formatNumber(stats.hits)} hits / ${formatNumber(stats.misses)} misses`,
  "open-files": formatNumber(stats.activeFiles),
  "opened-total": `Opened since startup: ${formatNumber(stats.totalFiles)}`,
  buffers: `Buffer allocations: ${formatBytes(stats.bufferAllocated)} · In use: ${formatBytes(stats.bufferInUse)}. Allocations include blocks kept for reuse or waiting for release.`,
  "cached-files": formatNumber(stats.items),
  "disk-used": formatBytes(stats.totalSize),
  "disk-share": `${usagePercent}% of ${formatBytes(stats.maxSize)}`,
  "loaded-items": formatNumber(stats.activeItems),
  "loaded-desc": `Resident in ${orElse(stats.cacheType, "vfs").toUpperCase()} memory`,
});

const dfsView = (stats: DFSStats): DFSView => {
  const usagePercent = (utilizationOf(stats) * PERCENT).toFixed(1);
  return {
    texts: dfsTexts(stats, usagePercent),
    downloading: stats.activeDownloads > 0,
    hitRateClass: hitRateClass(stats),
    usageClass: usageClass(Number(usagePercent)),
    health: health(stats.circuitBreakers),
    purge: { items: stats.items, size: stats.totalSize, max: stats.maxSize },
  };
};

const addClass = (root: ParentNode, name: string, className: string): void => {
  for (const element of root.querySelectorAll(`[data-class="${name}"]`)) {
    element.classList.add(className);
  }
};

const downloadingBadge = (): HTMLSpanElement => {
  const span = document.createElement("span");
  span.classList.add("text-success");
  span.textContent = "● Downloading";
  return span;
};

const renderHealth = (panel: ParentNode, view: Health): void => {
  addClass(panel, "health", view.className);
  field(panel, "health-icon").classList.add(view.icon);
  field(panel, "health-label").textContent = view.label;
  const desc = field(panel, "health-desc");
  desc.textContent = view.desc;
  desc.title = view.title;
};

const setPurgeTarget = (button: HTMLElement, target: PurgeTarget): void => {
  button.dataset["cacheItems"] = String(target.items);
  button.dataset["cacheSize"] = String(target.size);
  button.dataset["cacheMax"] = String(target.max);
};

const renderDFS = (content: HTMLElement, view: DFSView): void => {
  const panel = cloneTemplate("stats-dfs");
  setFields(panel, view.texts);
  field(panel, "downloads-state").replaceChildren(view.downloading ? downloadingBadge() : "Idle");
  addClass(panel, "hit-rate", view.hitRateClass);
  addClass(panel, "usage", view.usageClass);
  renderHealth(panel, view.health);
  setPurgeTarget(field(panel, "purge"), view.purge);
  content.replaceChildren(panel);
};

export { dfsView, parseDFS, renderDFS };
