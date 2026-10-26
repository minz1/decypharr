import { bootPage, byId, onAction } from "#ui/lib/common/dom";
import { copyConnectValue, findAccess } from "#ui/lib/stats/access";
import { runCacheCleanup, runCachePurge } from "#ui/lib/stats/cache";
import type { PurgeDialog, StatsElements } from "#ui/lib/stats/elements";
import { loadStats } from "#ui/lib/stats/load";
import { findOverview } from "#ui/lib/stats/overview";
import { runDebridSpeedTest, runNNTPSpeedTest } from "#ui/lib/stats/speedtest";
import { bindTabs } from "#ui/lib/stats/tabs";

const REFRESH_MS = 30_000;

const findElements = (): StatsElements => ({
  loading: byId("loading-stats", HTMLElement),
  content: byId("stats-content", HTMLElement),
  error: byId("error-stats", HTMLElement),
  errorMessage: byId("error-message", HTMLElement),
  overview: findOverview(document),
  access: findAccess(document),
  mount: {
    type: byId("mount-type", HTMLElement),
    status: byId("mount-status", HTMLElement),
    content: byId("mount-content", HTMLElement),
    logs: byId("rclone-logs-btn", HTMLElement),
  },
  streams: {
    count: byId("streams-count", HTMLElement),
    tabCount: byId("streams-tab-count", HTMLElement),
    content: byId("streams-content", HTMLElement),
  },
  debrids: byId("debrid-content", HTMLElement),
  usenet: byId("usenet-content", HTMLElement),
});

const findPurgeDialog = (): PurgeDialog => ({
  modal: byId("purge-cache-modal", HTMLDialogElement),
  confirm: byId("confirm-purge-cache", HTMLButtonElement),
  cancel: byId("cancel-purge-cache", HTMLButtonElement),
  files: byId("purge-cache-files", HTMLElement),
  diskUsed: byId("purge-cache-disk-used", HTMLElement),
});

const bindPanelActions = (elements: StatsElements, purgeDialog: PurgeDialog, reload: () => void): void => {
  onAction(elements.mount.content, "click", {
    "cache-cleanup": () => {
      void runCacheCleanup(reload);
    },
    "cache-purge": (button) => {
      void runCachePurge(button, purgeDialog, reload);
    },
  });
  onAction(elements.debrids, "click", {
    "debrid-speed-test": (button) => {
      if (button instanceof HTMLButtonElement) {
        void runDebridSpeedTest(button);
      }
    },
  });
  onAction(elements.usenet, "click", {
    "nntp-speed-test": (button) => {
      if (button instanceof HTMLButtonElement) {
        void runNNTPSpeedTest(button);
      }
    },
  });
  onAction(byId("access-card", HTMLElement), "click", {
    copy: (button) => {
      void copyConnectValue(button);
    },
  });
};

bootPage("stats", () => {
  const elements = findElements();
  const purgeDialog = findPurgeDialog();
  const reload = (): void => {
    void loadStats(elements, globalThis.location);
  };
  bindTabs(byId("stats-tabs", HTMLElement), elements.content);
  byId("refresh-stats", HTMLButtonElement).addEventListener("click", reload);
  byId("retry-stats", HTMLButtonElement).addEventListener("click", reload);
  bindPanelActions(elements, purgeDialog, reload);
  setInterval(reload, REFRESH_MS);
  reload();
});
