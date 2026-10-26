type Server = "webdav" | "nfs" | "smb";

interface ServerCard {
  status: HTMLElement;
  details: HTMLElement;
}

interface PageLocation {
  hostname: string;
  protocol: string;
}

interface MountElements {
  type: HTMLElement;
  status: HTMLElement;
  content: HTMLElement;
  logs: HTMLElement;
}

interface StreamsElements {
  count: HTMLElement;
  tabCount: HTMLElement;
  content: HTMLElement;
}

interface PurgeDialog {
  modal: HTMLDialogElement;
  confirm: HTMLButtonElement;
  cancel: HTMLButtonElement;
  files: HTMLElement;
  diskUsed: HTMLElement;
}

interface StatsElements {
  loading: HTMLElement;
  content: HTMLElement;
  error: HTMLElement;
  errorMessage: HTMLElement;
  overview: ReadonlyMap<string, HTMLElement>;
  access: Record<Server, ServerCard>;
  mount: MountElements;
  streams: StreamsElements;
  debrids: HTMLElement;
  usenet: HTMLElement;
}

export type { MountElements, PageLocation, PurgeDialog, Server, ServerCard, StatsElements, StreamsElements };
