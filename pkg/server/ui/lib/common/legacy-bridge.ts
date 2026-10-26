import { setButtonLoading } from "#ui/lib/common/button";
import { copyToClipboard } from "#ui/lib/common/clipboard";
import { formatBytes, formatDuration } from "#ui/lib/common/format";
import { fetcher } from "#ui/lib/common/http";
import { closeToast, createToast } from "#ui/lib/common/toast";
import { joinURL, readURLBase } from "#ui/lib/common/url";

const ENTITIES: Readonly<Record<string, string>> = {
  "&": "&amp;",
  "<": "&lt;",
  ">": "&gt;",
  '"': "&quot;",
  "'": "&#039;",
};

const escapeHtml = (text: unknown): string =>
  typeof text === "string" ? text.replaceAll(/[&<>"']/gu, (char) => ENTITIES[char] ?? char) : "";

const installBridge = (): void => {
  globalThis.urlBase = readURLBase(document);
  globalThis.decypharrUtils = {
    fetcher,
    createToast,
    closeToast,
    setButtonLoading,
    escapeHtml,
    formatBytes,
    formatDuration,
    joinURL,
    copyToClipboard,
  };
};

export { escapeHtml, installBridge };
