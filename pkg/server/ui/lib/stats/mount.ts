import { optBool, strOrEmpty } from "#ui/lib/common/json";
import { dfsView, parseDFS, renderDFS } from "#ui/lib/stats/dfs";
import { emptyMessage } from "#ui/lib/stats/dom";
import type { MountElements } from "#ui/lib/stats/elements";
import { parseRclone, rcloneView, renderRclone } from "#ui/lib/stats/rclone";
import { recordOrEmpty } from "#ui/lib/stats/read";

type MountKind = "None" | "Rclone" | "DFS" | "External";

interface MountStats {
  kind: MountKind;
  ready: boolean;
  dfs: ReturnType<typeof parseDFS> | undefined;
  rclone: ReturnType<typeof parseRclone> | undefined;
}

interface MountState {
  typeText: MountKind;
  typeClass: string;
  logsDisplay: string | undefined;
  statusText: string;
  statusClass: string;
  message: string | undefined;
}

const kindOf = (mount: Record<string, unknown>): MountKind => {
  const type = strOrEmpty(mount, "type");
  const enabled = optBool(mount, "enabled") ?? false;
  if (type === "rclone" && enabled) {
    return "Rclone";
  }
  if (type === "dfs" && enabled) {
    return "DFS";
  }
  return type === "external" ? "External" : "None";
};

const parseMount = (mount: Record<string, unknown> | undefined): MountStats => {
  const record = mount ?? {};
  const kind = kindOf(record);
  const ready = optBool(record, "ready") ?? false;
  const detail = recordOrEmpty(record, "detail");
  return {
    kind,
    ready,
    dfs: ready && kind === "DFS" ? parseDFS(detail) : undefined,
    rclone: ready && (kind === "Rclone" || kind === "External") ? parseRclone(detail) : undefined,
  };
};

const statusOf = (mount: MountStats): Pick<MountState, "statusText" | "statusClass" | "message"> => {
  if (mount.kind === "None") {
    return {
      statusText: "Disabled",
      statusClass: "badge badge-error",
      message: "No mount system is enabled or configured.",
    };
  }
  if (!mount.ready) {
    return {
      statusText: "Not Ready",
      statusClass: "badge badge-warning",
      message: mount.kind === "DFS" ? "DFS filesystem is not ready." : `${mount.kind} server is not ready.`,
    };
  }
  return { statusText: "Active", statusClass: "badge badge-success", message: undefined };
};

const logsDisplayOf = (kind: MountKind): string | undefined => {
  if (kind === "None") {
    return undefined;
  }
  return kind === "DFS" ? "none" : "inline-flex";
};

const mountState = (mount: MountStats): MountState => ({
  typeText: mount.kind,
  typeClass: mount.kind === "None" ? "badge badge-ghost" : "badge badge-info",
  logsDisplay: logsDisplayOf(mount.kind),
  ...statusOf(mount),
});

const renderBadges = (elements: MountElements, state: MountState): void => {
  if (state.logsDisplay !== undefined) {
    elements.logs.style.display = state.logsDisplay;
  }
  elements.type.textContent = state.typeText;
  elements.type.className = state.typeClass;
  elements.status.textContent = state.statusText;
  elements.status.className = state.statusClass;
};

const renderMount = (elements: MountElements, mount: MountStats): void => {
  const state = mountState(mount);
  renderBadges(elements, state);
  if (state.message !== undefined) {
    elements.content.replaceChildren(emptyMessage(state.message));
  } else if (mount.dfs) {
    renderDFS(elements.content, dfsView(mount.dfs));
  } else if (mount.rclone) {
    renderRclone(elements.content, rcloneView(mount.rclone));
  }
};

export { mountState, parseMount, renderMount };
