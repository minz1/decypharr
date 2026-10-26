import type { setButtonLoading } from "#ui/lib/common/button";
import type { copyToClipboard } from "#ui/lib/common/clipboard";
import type { formatBytes, formatDuration } from "#ui/lib/common/format";
import type { fetcher } from "#ui/lib/common/http";
import type { closeToast, createToast } from "#ui/lib/common/toast";
import type { joinURL } from "#ui/lib/common/url";

declare global {
  var urlBase: string;
  var decypharrUtils: DecypharrUtils;
}

export interface DecypharrUtils {
  fetcher: typeof fetcher;
  createToast: typeof createToast;
  closeToast: typeof closeToast;
  setButtonLoading: typeof setButtonLoading;
  escapeHtml: (text: string) => string;
  formatBytes: typeof formatBytes;
  formatDuration: typeof formatDuration;
  joinURL: typeof joinURL;
  copyToClipboard: typeof copyToClipboard;
}
