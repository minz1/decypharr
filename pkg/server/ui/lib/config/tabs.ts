const MOVED_TABS: Readonly<Record<string, readonly [string, string]>> = {
  downloads: ["general", "downloads"],
  "virtual-folders": ["general", "virtual-folders"],
  strm: ["general", "strm"],
  auth: ["general", "auth"],
  notifications: ["general", "notifications"],
  nfs: ["shares", "nfs"],
  smb: ["shares", "smb"],
  webdav: ["shares", "webdav"],
};

const TAB_ALIASES: Readonly<Record<string, string>> = { providers: "debrid" };

const HASH_PREFIX = "#tab-";
const HALF = 2;

const revealSettingsTab = (button: HTMLElement | undefined): void => {
  const scroller = button?.closest("nav")?.parentElement;
  if (
    button === undefined ||
    scroller === null ||
    scroller === undefined ||
    scroller.scrollWidth <= scroller.clientWidth
  ) {
    return;
  }
  const targetLeft = button.offsetLeft - (scroller.clientWidth - button.offsetWidth) / HALF;
  scroller.scrollTo({ left: Math.max(0, targetLeft), behavior: "auto" });
};

const all = (root: ParentNode, selector: string): HTMLElement[] => [...root.querySelectorAll<HTMLElement>(selector)];

const markSubTabs = (buttons: readonly HTMLElement[], accent: string, key: string): void => {
  for (const button of buttons) {
    const on = button.dataset["subtab"] === key;
    button.classList.toggle("active", on);
    button.classList.toggle(`border-${accent}`, on);
    button.classList.toggle(`text-${accent}`, on);
    button.classList.toggle("border-transparent", !on);
    button.classList.toggle("text-base-content/70", !on);
  }
};

const switchSubTab = (doc: Document, group: string, key: string): void => {
  const nav = all(doc, ".subtab-nav").find((candidate) => candidate.dataset["subtabGroup"] === group);
  if (nav === undefined) {
    return;
  }
  const buttons = all(nav, ".subtab-button");
  markSubTabs(buttons, nav.dataset["subtabAccent"] ?? "primary", key);
  revealSettingsTab(buttons.find((button) => button.dataset["subtab"] === key));
  for (const panel of all(doc, ".subtab-panel").filter((candidate) => candidate.dataset["subtabGroup"] === group)) {
    panel.classList.toggle("hidden", panel.dataset["subtabPanel"] !== key);
  }
};

const showTab = (doc: Document, tab: string): void => {
  for (const button of all(doc, ".tab-button")) {
    button.classList.remove("active");
  }
  const active = all(doc, "[data-tab]").find((button) => button.dataset["tab"] === tab);
  active?.classList.add("active");
  revealSettingsTab(active);
  for (const content of all(doc, ".tab-content")) {
    content.classList.add("hidden");
  }
  all(doc, "[data-tab-content]")
    .find((content) => content.dataset["tabContent"] === tab)
    ?.classList.remove("hidden");
  globalThis.location.hash = `tab-${tab}`;
};

const switchTab = (doc: Document, requested: string): void => {
  const tab = TAB_ALIASES[requested] ?? requested;
  const moved = MOVED_TABS[tab];
  if (moved === undefined) {
    showTab(doc, tab);
    return;
  }
  const [parent, subTab] = moved;
  switchTab(doc, parent);
  switchSubTab(doc, parent, subTab);
};

const switchToHash = (doc: Document, fallback?: string): void => {
  const { hash } = globalThis.location;
  if (hash.startsWith(HASH_PREFIX)) {
    switchTab(doc, hash.replace(HASH_PREFIX, ""));
  } else if (fallback !== undefined) {
    switchTab(doc, fallback);
  }
};

const switchMountSubTab = (doc: Document, tab: string): void => {
  const buttons = all(doc, ".mount-subtab-button");
  for (const button of buttons) {
    button.classList.remove("active", "border-primary", "text-primary");
    button.classList.add("border-transparent", "text-base-content/70");
  }
  const active = buttons.find((button) => button.dataset["mountTab"] === tab);
  active?.classList.add("active", "border-primary", "text-primary");
  active?.classList.remove("border-transparent", "text-base-content/70");
  revealSettingsTab(active);
  for (const content of all(doc, ".mount-subtab-content")) {
    content.classList.toggle("hidden", content.dataset["mountTabContent"] !== tab);
  }
};

const bindMountTabs = (doc: Document): void => {
  const radios = [...doc.querySelectorAll<HTMLInputElement>('input[name="mount.type"]')];
  for (const button of all(doc, ".mount-subtab-button")) {
    button.addEventListener("click", (event) => {
      event.preventDefault();
      const tab = button.dataset["mountTab"] ?? "";
      switchMountSubTab(doc, tab);
      const radio = radios.find((candidate) => candidate.value === tab);
      if (radio !== undefined) {
        radio.checked = true;
      }
    });
  }
  for (const radio of radios) {
    radio.addEventListener("change", () => {
      if (radio.checked) {
        switchMountSubTab(doc, radio.value);
      }
    });
  }
  switchMountSubTab(doc, "dfs");
};

const bindSubTabs = (doc: Document): void => {
  for (const nav of all(doc, ".subtab-nav")) {
    const group = nav.dataset["subtabGroup"] ?? "";
    for (const button of all(nav, ".subtab-button")) {
      button.addEventListener("click", (event) => {
        event.preventDefault();
        switchSubTab(doc, group, button.dataset["subtab"] ?? "");
      });
    }
  }
};

const bindTabs = (doc: Document): void => {
  for (const button of all(doc, ".tab-button")) {
    button.addEventListener("click", (event) => {
      event.preventDefault();
      switchTab(doc, button.dataset["tab"] ?? "");
    });
  }
  switchToHash(doc, "general");
  globalThis.addEventListener("hashchange", () => {
    switchToHash(doc);
  });
  bindMountTabs(doc);
  bindSubTabs(doc);
};

export { bindTabs, switchTab };
