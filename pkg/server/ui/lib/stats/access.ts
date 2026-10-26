import { byId } from "#ui/lib/common/dom";
import { orElse } from "#ui/lib/common/format";
import { numOrZero, optBool, strOrEmpty } from "#ui/lib/common/json";
import { cloneElement, field } from "#ui/lib/stats/dom";
import type { PageLocation, Server, ServerCard } from "#ui/lib/stats/elements";
import { recordOrEmpty } from "#ui/lib/stats/read";

interface AccessStats {
  webdav: { enabled: boolean; path: string; port: string; authRequired: boolean };
  nfs: { enabled: boolean; port: number };
  smb: { enabled: boolean; port: number; shareName: string; username: string };
}

interface ConnectRow {
  label: string;
  value: string;
}

type Note =
  | { kind: "text"; text: string }
  | { kind: "off"; text: string }
  | { kind: "nfs"; port: string }
  | { kind: "smb"; port: string };

interface ServerView {
  status: { text: string; className: string };
  rows: ConnectRow[];
  note: Note;
}

const SERVERS: readonly Server[] = ["webdav", "nfs", "smb"];
const DEFAULT_NFS_PORT = 20_490;
const DEFAULT_SMB_PORT = 1445;
const COPIED_MS = 1200;
const ENABLED = { text: "Enabled", className: "badge badge-success" };
const DISABLED = { text: "Disabled", className: "badge badge-ghost" };

const enabled = (record: Record<string, unknown>): boolean => optBool(record, "enabled") ?? false;

const parseAccess = (record: Record<string, unknown>): AccessStats => {
  const webdav = recordOrEmpty(record, "webdav");
  const nfs = recordOrEmpty(record, "nfs");
  const smb = recordOrEmpty(record, "smb");
  return {
    webdav: {
      enabled: enabled(webdav),
      path: strOrEmpty(webdav, "path"),
      port: strOrEmpty(webdav, "port"),
      authRequired: optBool(webdav, "auth_required") ?? false,
    },
    nfs: { enabled: enabled(nfs), port: numOrZero(nfs, "port") },
    smb: {
      enabled: enabled(smb),
      port: numOrZero(smb, "port"),
      shareName: strOrEmpty(smb, "share_name"),
      username: strOrEmpty(smb, "username"),
    },
  };
};

const webdavView = (webdav: AccessStats["webdav"], host: string, protocol: string): ServerView => {
  if (!webdav.enabled) {
    return { status: DISABLED, rows: [], note: { kind: "off", text: "WebDAV is disabled in settings." } };
  }
  const port = webdav.port === "" ? "" : `:${webdav.port}`;
  const url = `${protocol}//${host}${port}${orElse(webdav.path, "/webdav")}/`;
  return {
    status: ENABLED,
    rows: [{ label: "URL (browser, rclone, Kodi, Infuse…)", value: url }],
    note: {
      kind: "text",
      text: webdav.authRequired ? "Requires your Decypharr username and password." : "No authentication required.",
    },
  };
};

const nfsView = (nfs: AccessStats["nfs"], host: string): ServerView => {
  if (!nfs.enabled) {
    return {
      status: DISABLED,
      rows: [],
      note: { kind: "off", text: "Enable the NFSv4 server in Settings → Mount Settings." },
    };
  }
  const port = nfs.port || DEFAULT_NFS_PORT;
  const opts = `vers=4.0,tcp,port=${port},ro`;
  return {
    status: ENABLED,
    rows: [
      { label: "Linux", value: `sudo mount -t nfs -o ${opts} ${host}:/ /mnt/decypharr` },
      { label: "macOS", value: `sudo mount -t nfs -o ${opts},resvport ${host}:/ /Volumes/decypharr` },
    ],
    note: { kind: "nfs", port: String(port) },
  };
};

const smbView = (smb: AccessStats["smb"], host: string): ServerView => {
  if (!smb.enabled) {
    return {
      status: DISABLED,
      rows: [],
      note: { kind: "off", text: "Enable the SMB server in Settings → Mount Settings." },
    };
  }
  const share = orElse(smb.shareName, "decypharr");
  const user = orElse(smb.username, "USERNAME");
  const port = smb.port || DEFAULT_SMB_PORT;
  return {
    status: { text: "Experimental", className: "badge badge-warning" },
    rows: [
      { label: "Windows (needs host port 445)", value: `net use Z: \\\\${host}\\${share} /user:${user}` },
      { label: "macOS", value: `open smb://${user}@${host}:${port}/${share}` },
      {
        label: "Linux",
        value: `sudo mount -t cifs -o vers=3.1.1,port=${port},user=${user},ro //${host}/${share} /mnt/decypharr`,
      },
    ],
    note: { kind: "smb", port: String(port) },
  };
};

const accessViews = (access: AccessStats, where: PageLocation): Record<Server, ServerView> => {
  const host = orElse(where.hostname, "SERVER_IP");
  return {
    webdav: webdavView(access.webdav, host, where.protocol),
    nfs: nfsView(access.nfs, host),
    smb: smbView(access.smb, host),
  };
};

const connectRow = (row: ConnectRow): HTMLElement => {
  const element = cloneElement("stats-connect-row");
  field(element, "label").textContent = row.label;
  field(element, "value").textContent = row.value;
  field(element, "copy").dataset["copy"] = row.value;
  return element;
};

const noteElement = (note: Note): HTMLElement => {
  if (note.kind === "nfs" || note.kind === "smb") {
    const element = cloneElement(`stats-access-${note.kind}-note`);
    field(element, "port").textContent = note.port;
    return element;
  }
  const element = cloneElement(note.kind === "off" ? "stats-access-off" : "stats-access-note");
  element.textContent = note.text;
  return element;
};

const findAccess = (doc: Document): Record<Server, ServerCard> => {
  const card = (server: Server): ServerCard => ({
    status: byId(`${server}-status`, HTMLElement, doc),
    details: byId(`${server}-details`, HTMLElement, doc),
  });
  return { webdav: card("webdav"), nfs: card("nfs"), smb: card("smb") };
};

const renderAccess = (cards: Record<Server, ServerCard>, views: Record<Server, ServerView>): void => {
  for (const server of SERVERS) {
    const view = views[server];
    const card = cards[server];
    card.status.textContent = view.status.text;
    card.status.className = view.status.className;
    card.details.replaceChildren(...view.rows.map((row) => connectRow(row)), noteElement(view.note));
  }
};

const copyConnectValue = async (button: HTMLElement): Promise<void> => {
  try {
    await navigator.clipboard.writeText(button.dataset["copy"] ?? "");
  } catch {
    return;
  }
  const original = [...button.childNodes];
  const check = document.createElement("i");
  check.classList.add("bi", "bi-check-lg");
  button.replaceChildren(check);
  setTimeout(() => {
    button.replaceChildren(...original);
  }, COPIED_MS);
};

export { accessViews, copyConnectValue, findAccess, parseAccess, renderAccess };
