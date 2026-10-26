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

const TOAST_TYPES = ["success", "error", "warning", "info"] as const;

const isToastType = (type: unknown): type is (typeof TOAST_TYPES)[number] =>
  TOAST_TYPES.some((known) => known === type);

const textOf = (message: unknown): string => {
  if (typeof message === "string") {
    return message;
  }
  if (message instanceof Error) {
    return message.toString();
  }
  return "";
};

const legacyToast = (message: unknown, type?: unknown, durationMs?: unknown): string => {
  const text = textOf(message);
  const kind = isToastType(type) ? type : "success";
  return typeof durationMs === "number" && durationMs > 0
    ? createToast(text, kind, durationMs)
    : createToast(text, kind);
};

const installBridge = (): void => {
  globalThis.urlBase = readURLBase(document);
  globalThis.decypharrUtils = {
    fetcher,
    createToast: legacyToast,
    closeToast,
    setButtonLoading,
    escapeHtml,
    formatBytes,
    formatDuration,
    joinURL,
    copyToClipboard,
  };
};

export { escapeHtml, installBridge, legacyToast };
