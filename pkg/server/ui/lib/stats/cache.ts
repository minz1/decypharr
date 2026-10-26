import { setButtonLoading } from "#ui/lib/common/button";
import { formatBytes } from "#ui/lib/common/format";
import { fetcher } from "#ui/lib/common/http";
import { asRecord, readJSON } from "#ui/lib/common/json";
import { createToast } from "#ui/lib/common/toast";
import type { PurgeDialog } from "#ui/lib/stats/elements";
import { errorMessage, formatNumber } from "#ui/lib/stats/format";
import { count, recordOrEmpty } from "#ui/lib/stats/read";

interface CleanupResult {
  warnings: number;
  freedBytes: number;
  removedItems: number;
}

interface PurgeResult extends CleanupResult {
  skippedBusyItems: number;
}

interface Toast {
  message: string;
  type: "success" | "warning";
}

interface PurgeTarget {
  items: number;
  size: number;
  max: number;
}

const cacheOf = (value: unknown, path: string): Record<string, unknown> =>
  recordOrEmpty(asRecord(value, path), "cache");

const parseCleanup = (value: unknown): CleanupResult => {
  const cache = cacheOf(value, "cleanup");
  return {
    warnings: count(cache, "cleanup_warning_count"),
    freedBytes: count(cache, "cleanup_freed_bytes"),
    removedItems: count(cache, "cleanup_removed_items"),
  };
};

const parsePurge = (value: unknown): PurgeResult => {
  const cache = cacheOf(value, "purge");
  return {
    warnings: count(cache, "purge_warning_count"),
    freedBytes: count(cache, "purge_freed_bytes"),
    removedItems: count(cache, "purge_removed_items"),
    skippedBusyItems: count(cache, "purge_skipped_busy_items"),
  };
};

const cleanupToast = (result: CleanupResult): Toast =>
  result.warnings > 0
    ? { message: `Cache cleanup completed with ${result.warnings} warning(s).`, type: "warning" }
    : {
        message: `Cache cleanup completed. Freed ${formatBytes(result.freedBytes)} and evicted ${formatNumber(result.removedItems)} file(s).`,
        type: "success",
      };

const purgeToast = (result: PurgeResult): Toast => {
  const skipped =
    result.skippedBusyItems > 0 ? ` Skipped ${formatNumber(result.skippedBusyItems)} active item(s).` : "";
  return result.warnings > 0
    ? { message: `Cache purge completed with ${result.warnings} warning(s).${skipped}`, type: "warning" }
    : {
        message: `Cache purge completed. Freed ${formatBytes(result.freedBytes)} and removed ${formatNumber(result.removedItems)} file(s).${skipped}`,
        type: "success",
      };
};

const purgeSummary = (target: PurgeTarget): { files: string; diskUsed: string } => {
  const size = formatBytes(target.size);
  return {
    files: formatNumber(target.items),
    diskUsed: target.max > 0 ? `${size} / ${formatBytes(target.max)}` : `${size} / unlimited`,
  };
};

const purgeTarget = (button: HTMLElement): PurgeTarget => ({
  items: Number(button.dataset["cacheItems"] ?? 0),
  size: Number(button.dataset["cacheSize"] ?? 0),
  max: Number(button.dataset["cacheMax"] ?? 0),
});

const postCacheAction = async (endpoint: string): Promise<unknown> => {
  const response = await fetcher(endpoint, { method: "POST" });
  if (!response.ok) {
    const message = await response.text();
    throw new Error(message === "" ? `HTTP ${response.status}` : message);
  }
  return readJSON(response);
};

const postWhileBusy = async (button: HTMLButtonElement, endpoint: string): Promise<unknown> => {
  setButtonLoading(button, true);
  try {
    return await postCacheAction(endpoint);
  } finally {
    setButtonLoading(button, false);
  }
};

const runCacheCleanup = async (button: HTMLButtonElement, reload: () => void): Promise<void> => {
  if (button.disabled) {
    return;
  }
  try {
    const toast = cleanupToast(parseCleanup(await postWhileBusy(button, "/api/mount/cache/cleanup")));
    createToast(toast.message, toast.type);
    reload();
  } catch (error) {
    createToast(`Cache cleanup failed: ${errorMessage(error)}`, "error");
  }
};

const answerOf = async (dialog: PurgeDialog): Promise<boolean> => {
  const controller = new AbortController();
  const answer = Promise.withResolvers<boolean>();
  const finish = (confirmed: boolean): void => {
    controller.abort();
    answer.resolve(confirmed);
    if (dialog.modal.open) {
      dialog.modal.close();
    }
  };
  const { signal } = controller;
  dialog.confirm.addEventListener(
    "click",
    () => {
      finish(true);
    },
    { signal },
  );
  dialog.cancel.addEventListener(
    "click",
    () => {
      finish(false);
    },
    { signal },
  );
  dialog.modal.addEventListener(
    "close",
    () => {
      finish(false);
    },
    { signal },
  );
  return answer.promise;
};

const confirmCachePurge = async (dialog: PurgeDialog, target: PurgeTarget): Promise<boolean> => {
  const summary = purgeSummary(target);
  dialog.files.textContent = summary.files;
  dialog.diskUsed.textContent = summary.diskUsed;
  const answer = answerOf(dialog);
  dialog.modal.showModal();
  return answer;
};

const runCachePurge = async (button: HTMLButtonElement, dialog: PurgeDialog, reload: () => void): Promise<void> => {
  if (button.disabled || !(await confirmCachePurge(dialog, purgeTarget(button)))) {
    return;
  }
  try {
    const toast = purgeToast(parsePurge(await postWhileBusy(button, "/api/mount/cache/purge")));
    createToast(toast.message, toast.type);
    reload();
  } catch (error) {
    createToast(`Cache purge failed: ${errorMessage(error)}`, "error");
  }
};

export { cleanupToast, parseCleanup, parsePurge, purgeSummary, purgeToast, runCacheCleanup, runCachePurge };
