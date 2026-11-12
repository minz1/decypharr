import { numOrZero, optBool, strOrEmpty } from "#ui/lib/common/json";
import { dfsView, parseDFS, renderDFS } from "#ui/lib/stats/dfs";
import { emptyMessage } from "#ui/lib/stats/dom";
import type { MountElements } from "#ui/lib/stats/elements";
import { parseRclone, rcloneView, renderRclone } from "#ui/lib/stats/rclone";
import { optRecord, recordOrEmpty } from "#ui/lib/stats/read";

type MountKind = "None" | "Rclone" | "DFS" | "External";

interface MountStats {
  kind: MountKind;
  ready: boolean;
  // The state is rcd's state for rclone and the filesystem's for DFS; mountState is rclone's mount, "" before an attempt.
  state: string;
  error: string;
  mountState: string;
  restarts: number;
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
  note: string | undefined;
}

type Status = Pick<MountState, "statusText" | "statusClass" | "message">;

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
  const mounts = optRecord(detail, "mounts") ?? {};
  return {
    kind,
    ready,
    state: strOrEmpty(detail, "state"),
    error: kind === "DFS" ? strOrEmpty(detail, "error") : strOrEmpty(mounts, "error"),
    mountState: strOrEmpty(mounts, "state"),
    restarts: numOrZero(detail, "restarts"),
    dfs: ready && kind === "DFS" ? parseDFS(detail) : undefined,
    rclone: ready && (kind === "Rclone" || kind === "External") ? parseRclone(detail) : undefined,
  };
};

const status = (statusText: string, tone: string, message?: string): Status => ({
  statusText,
  statusClass: `badge badge-${tone}`,
  message,
});

const active = (): Status => status("Active", "success");

const withError = (text: string, error: string): string => (error === "" ? `${text}.` : `${text}: ${error}`);

const rcloneStatus = (mount: MountStats): Status => {
  if (!mount.ready) {
    const rcd = mount.state === "" ? "" : ` (${mount.state})`;
    return status("Not Ready", "warning", `Rclone server is not ready${rcd}.`);
  }
  switch (mount.mountState) {
    case "mounted": {
      return active();
    }
    case "degraded": {
      return status("Degraded", "warning", withError("The mount is degraded", mount.error));
    }
    case "failed": {
      return status("Failed", "error", withError("The mount failed", mount.error));
    }
    case "unmounting":
    case "unmounted": {
      return status("Stopped", "ghost", "The mount is stopped.");
    }
    default: {
      return status("Mounting", "warning", "The mount is starting.");
    }
  }
};

const dfsStatus = (mount: MountStats): Status => {
  const fallback = mount.ready ? "ready" : "new";
  switch (mount.state === "" ? fallback : mount.state) {
    case "ready": {
      return active();
    }
    case "starting": {
      return status("Starting", "warning", "DFS filesystem is starting.");
    }
    case "failed": {
      return status("Failed", "error", withError("DFS filesystem failed", mount.error));
    }
    case "stopping":
    case "stopped": {
      return status("Stopped", "ghost", "DFS filesystem is stopped.");
    }
    default: {
      return status("Not Ready", "warning", "DFS filesystem is not ready.");
    }
  }
};

const statusOf = (mount: MountStats): Status => {
  if (mount.kind === "None") {
    return status("Disabled", "error", "No mount system is enabled or configured.");
  }
  if (mount.kind === "Rclone") {
    return rcloneStatus(mount);
  }
  if (mount.kind === "DFS") {
    return dfsStatus(mount);
  }
  return mount.ready ? active() : status("Not Ready", "warning", "External server is not ready.");
};

const logsDisplayOf = (kind: MountKind): string | undefined => {
  if (kind === "None") {
    return undefined;
  }
  return kind === "DFS" ? "none" : "inline-flex";
};

const noteOf = (mount: MountStats): string | undefined =>
  mount.kind === "Rclone" && mount.restarts > 0 ? `rclone restarted ${String(mount.restarts)} times` : undefined;

const mountState = (mount: MountStats): MountState => ({
  typeText: mount.kind,
  typeClass: mount.kind === "None" ? "badge badge-ghost" : "badge badge-info",
  logsDisplay: logsDisplayOf(mount.kind),
  ...statusOf(mount),
  note: noteOf(mount),
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
  // A status message replaces the panel, so the restart count is its own line beside either.
  if (state.note !== undefined) {
    elements.content.append(emptyMessage(state.note));
  }
};

export { mountState, parseMount, renderMount };
