import { byId, cloneTemplate } from "#ui/lib/common/dom";
import type { SetupConfig } from "#ui/lib/setup/config";
import type { OverviewSection, SetupOverview } from "#ui/lib/setup/overview";
import { STEP_ORDER } from "#ui/lib/setup/steps";
import type { StepName } from "#ui/lib/setup/types";

const STEP_NAMES: Readonly<Record<StepName, string>> = {
  auth: "Authentication",
  debrid: "Debrid Account",
  usenet: "Usenet Provider",
  download: "Download Folder",
  mount: "Mount System",
  overview: "Overview",
};

const INPUTS = [
  "auth-username",
  "auth-password",
  "auth-confirm-password",
  "debrid-api-key",
  "usenet-host",
  "usenet-port",
  "usenet-username",
  "usenet-password",
  "usenet-max-connections",
  "usenet-max-connections-per-reader",
  "usenet-ssl",
  "download-folder",
  "mount-type-dfs",
  "mount-type-rclone",
  "mount-type-external",
  "mount-type-none",
  "mount-path",
  "cache-dir",
  "rclone-buffer-size",
  "setup-token-value",
] as const;

const PANELS = [
  "loading-state",
  "error-state",
  "error-message",
  "setup-progress-name",
  "setup-progress-label",
  "rclone-options",
  "overview-auth",
  "overview-debrid",
  "overview-usenet",
  "overview-download",
  "overview-mount",
  "finish-btn-text",
  "finish-btn-loading",
] as const;

type InputId = (typeof INPUTS)[number];
type PanelId = (typeof PANELS)[number];

interface SetupView {
  root: HTMLElement;
  finish: HTMLButtonElement;
  input: (id: InputId) => HTMLInputElement;
  panel: (id: PanelId) => HTMLElement;
  provider: HTMLSelectElement;
  progress: HTMLProgressElement;
  tokenDialog: HTMLDialogElement;
  steps: readonly HTMLElement[];
  indicators: readonly HTMLElement[];
  mountTypes: readonly HTMLInputElement[];
}

const lookup = <Id extends string, Wanted extends HTMLElement>(
  ids: readonly Id[],
  type: abstract new () => Wanted,
): ((id: Id) => Wanted) => {
  const found = new Map(ids.map((id) => [id, byId(id, type)]));
  return (id) => found.get(id) ?? byId(id, type);
};

const numbered = (prefix: string): HTMLElement[] =>
  STEP_ORDER.map((_name, index) => byId(`${prefix}${index + 1}`, HTMLElement));

const setupView = (): SetupView => {
  const input = lookup(INPUTS, HTMLInputElement);
  return {
    root: byId("setup-wizard", HTMLElement),
    finish: byId("finish-btn", HTMLButtonElement),
    input,
    panel: lookup(PANELS, HTMLElement),
    provider: byId("debrid-provider", HTMLSelectElement),
    progress: byId("setup-progress", HTMLProgressElement),
    tokenDialog: byId("setup-token-dialog", HTMLDialogElement),
    steps: numbered("step-"),
    indicators: numbered("step-indicator-"),
    mountTypes: [
      input("mount-type-dfs"),
      input("mount-type-rclone"),
      input("mount-type-external"),
      input("mount-type-none"),
    ],
  };
};

const showError = (view: SetupView, message: string): void => {
  view.panel("error-message").textContent = message;
  view.panel("error-state").classList.remove("hidden");
};

const showStep = (view: SetupView, step: StepName): void => {
  const current = STEP_ORDER.indexOf(step);
  view.panel("error-state").classList.add("hidden");
  for (const [index, panel] of view.steps.entries()) {
    panel.classList.toggle("hidden", index !== current);
  }
  view.progress.value = current + 1;
  view.panel("setup-progress-name").textContent = STEP_NAMES[step];
  view.panel("setup-progress-label").textContent = `Step ${current + 1} of ${STEP_ORDER.length}`;
  for (const [index, indicator] of view.indicators.entries()) {
    indicator.classList.toggle("step-primary", index <= current);
  }
};

const overviewLine = (label: string, value: string): Node => {
  const line = cloneTemplate("setup-overview-line");
  line.querySelector("[data-field='label']")?.append(label);
  line.querySelector("p")?.append(` ${value}`);
  return line;
};

const fillSection = (panel: HTMLElement, section: OverviewSection): void => {
  if (typeof section === "string") {
    panel.textContent = section;
    return;
  }
  panel.replaceChildren(...section.map(([label, value]) => overviewLine(label, value)));
};

const showOverview = (view: SetupView, overview: SetupOverview): void => {
  view.panel("overview-auth").textContent = overview.auth;
  fillSection(view.panel("overview-debrid"), overview.debrid);
  fillSection(view.panel("overview-usenet"), overview.usenet);
  view.panel("overview-download").textContent = overview.download;
  fillSection(view.panel("overview-mount"), overview.mount);
};

const fillUsenet = (view: SetupView, usenet: NonNullable<SetupConfig["usenet"]>): void => {
  view.input("usenet-host").value = usenet.host;
  view.input("usenet-port").value = usenet.port;
  view.input("usenet-username").value = usenet.username;
  view.input("usenet-password").value = usenet.password;
  view.input("usenet-max-connections").value = usenet.maxConnections;
  view.input("usenet-max-connections-per-reader").value = usenet.readerConnections;
  view.input("usenet-ssl").checked = usenet.ssl;
};

const fillMount = (view: SetupView, mount: NonNullable<SetupConfig["mount"]>): void => {
  view.input(mount.type === "dfs" ? "mount-type-dfs" : "mount-type-rclone").checked = true;
  view.input("mount-path").value = mount.mountPath;
  view.input("cache-dir").value = mount.cacheDir;
  if (mount.bufferSize !== undefined) {
    view.input("rclone-buffer-size").value = mount.bufferSize;
  }
};

const fillAccounts = (view: SetupView, config: SetupConfig): void => {
  if (config.authUsername !== undefined) {
    view.input("auth-username").value = config.authUsername;
  }
  if (config.debrid !== undefined) {
    view.provider.value = config.debrid.provider;
    view.input("debrid-api-key").value = config.debrid.apiKey;
  }
  if (config.usenet !== undefined) {
    fillUsenet(view, config.usenet);
  }
};

const fillFields = (view: SetupView, config: SetupConfig): void => {
  fillAccounts(view, config);
  if (config.downloadFolder !== undefined) {
    view.input("download-folder").value = config.downloadFolder;
  }
  if (config.mount !== undefined) {
    fillMount(view, config.mount);
  }
};

const setFinishBusy = (view: SetupView, busy: boolean): void => {
  view.finish.disabled = busy;
  view.panel("finish-btn-text").classList.toggle("hidden", busy);
  view.panel("finish-btn-loading").classList.toggle("hidden", !busy);
};

export type { SetupView };
export { fillFields, setFinishBusy, setupView, showError, showOverview, showStep };
