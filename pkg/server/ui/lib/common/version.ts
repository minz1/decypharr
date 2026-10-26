import { fetcher } from "#ui/lib/common/http";
import { asRecord, readJSON, str } from "#ui/lib/common/json";

interface VersionInfo {
  version: string;
  channel: string;
}

const CHANNEL_BADGES: ReadonlyMap<string, string> = new Map([
  ["beta", "badge-warning"],
  ["nightly", "badge-error"],
]);

const parseVersion = (value: unknown): VersionInfo => {
  const record = asRecord(value, "version");
  return { version: str(record, "version"), channel: str(record, "channel") };
};

const releaseLink = (doc: Document, info: VersionInfo): HTMLAnchorElement => {
  const link = doc.createElement("a");
  link.href = `https://github.com/sirrobot01/decypharr/releases/tag/v${info.version}`;
  link.target = "_blank";
  link.classList.add("text-current", "hover:text-primary", "transition-colors");
  link.textContent = `${info.channel}-${info.version}`;
  return link;
};

const showVersion = async (doc: Document, badge: Element): Promise<void> => {
  try {
    const info = parseVersion(await readJSON(await fetcher("/version")));
    badge.replaceChildren(releaseLink(doc, info));
    badge.classList.remove("badge-warning", "badge-error", "badge-ghost");
    const channelBadge = CHANNEL_BADGES.get(info.channel);
    if (typeof channelBadge === "string") {
      badge.classList.add(channelBadge);
    }
  } catch {
    badge.textContent = "Unknown";
    badge.classList.add("badge-ghost");
  }
};

const setupVersionInfo = (doc: Document): void => {
  const badge = doc.querySelector("#version-badge");
  if (badge !== null) {
    void showVersion(doc, badge);
  }
};

export { parseVersion, setupVersionInfo };
