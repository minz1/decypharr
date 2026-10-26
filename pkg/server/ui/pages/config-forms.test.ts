import { describe, expect, it, vi } from "vitest";

import { JSON_NULL } from "#ui/lib/common/json";
import {
  bootSettings,
  click,
  fill,
  find,
  isHidden,
  lastItem,
  member,
  posts,
  respond,
  savedBody,
  settingsLoaded,
  submitSettings,
  waitForToast,
} from "#ui/lib/testing/config";

type Page = Awaited<ReturnType<typeof bootSettings>>;

const texts = (selector: string): string[] =>
  [...document.querySelectorAll(selector)].map((node) => node.textContent.replaceAll(/\s+/gu, " ").trim());

const values = (selector: string): string[] =>
  [...document.querySelectorAll<HTMLInputElement | HTMLSelectElement>(selector)].map((control) => control.value);

const conditionRows = (): string[][] =>
  [...document.querySelectorAll<HTMLElement>("[data-vf-condition]")].map((row) => [
    row.dataset["vfCondition"] ?? "",
    row.dataset["vfCurrentField"] ?? "",
  ]);

const exists = (selector: string): boolean => document.querySelector(selector) !== null;

const status = (): string => find("#virtualFolderEditorStatus", HTMLElement).textContent;

const loaded = async (options?: Parameters<typeof bootSettings>[0]): Promise<Page> => {
  const page = await bootSettings(options);
  await settingsLoaded();
  return page;
};

const addArrAndProvider = (): void => {
  click("#addArrBtn");
  click('.arr-config[data-index="0"] [data-action="remove-arr"]');
  click("#addArrBtn");
  fill('[name="arr[2].name"]', "radarr");
  fill('[name="arr[2].host"]', "http://radarr:7878");
  click("#addUsenetProviderBtn");
  fill('[name="usenet.providers[0].host"]', "news.example");
  fill('[name="usenet.providers[0].username"]', "u");
  fill('[name="usenet.providers[0].password"]', "p");
};

const sentProvider = {
  host: "news.example",
  port: 119,
  username: "u",
  password: "p",
  backbone: "",
  ssl: false,
  max_connections: 100,
  priority: 0,
  backup: false,
  tls_server_name: "",
};

const sentArr = {
  name: "radarr",
  host: "http://radarr:7878",
  token: "",
  skip_repair: false,
  selected_debrid: "",
  source: "",
  import_webhook: false,
  download_uncached: JSON_NULL,
};

describe("debrid cards", () => {
  it("adds a debrid that the arr picker offers once its name is set", async () => {
    await loaded();
    click("#addDebridBtn");
    fill('[name="debrid[1].name"]', "torbox-main");
    find('[name="debrid[1].name"]', HTMLInputElement).dispatchEvent(new FocusEvent("focusout", { bubbles: true }));
    expect({
      titles: texts(".debrid-config [data-card-title]"),
      labelFor: find('.debrid-config[data-index="1"] label', HTMLLabelElement).htmlFor,
      options: values('[name="arr[0].selected_debrid"] option'),
    }).toStrictEqual({
      titles: ["Debrid #1", "Debrid #2"],
      labelFor: "debrid[1].name",
      options: ["", "realdebrid", "torbox-main"],
    });
  });

  it("removes a debrid and drops it from the arr picker", async () => {
    await loaded();
    click('.debrid-config[data-index="0"] [data-action="remove-debrid"]');
    expect({
      cards: exists(".debrid-config"),
      options: values('[name="arr[0].selected_debrid"] option'),
    }).toStrictEqual({
      cards: false,
      options: [""],
    });
  });
});

describe("arr and usenet cards", () => {
  it("adds and removes arrs and usenet providers, numbering past removed cards", async () => {
    const { requests } = await loaded();
    addArrAndProvider();
    submitSettings();
    const body = await savedBody(requests);
    expect({
      arrs: texts(".arr-config [data-card-title]"),
      provider: texts('label[for="usenet_provider_0_host"]'),
      sentArrs: body["arrs"],
      sentProviders: member(body["usenet"], "providers"),
    }).toStrictEqual({
      arrs: ["Arr Service #2", "Arr Service #3"],
      provider: ["Server Host"],
      sentArrs: [sentArr],
      sentProviders: [sentProvider],
    });
  });

  it("locks an auto-detected arr", async () => {
    const config = { arrs: [{ name: "auto", host: "http://s", token: "t", source: "auto", download_uncached: false }] };
    await bootSettings({ routes: { "GET api/config": () => respond(200, config) } });
    await vi.waitFor(() => {
      expect({ card: exists(".arr-config") }).toStrictEqual({ card: true });
    });
    expect({
      border: find(".arr-config", HTMLElement).classList.contains("border-info"),
      badge: exists(".arr-config [data-auto-badge]"),
      remove: exists('.arr-config [data-action="remove-arr"]'),
      readonly: find('[name="arr[0].name"]', HTMLInputElement).readOnly,
      toggle: find(".arr-config .password-toggle-btn", HTMLButtonElement).disabled,
      uncached: find('[name="arr[0].download_uncached"]', HTMLSelectElement).value,
    }).toStrictEqual({ border: true, badge: true, remove: false, readonly: true, toggle: true, uncached: "false" });
  });
});

describe("adding virtual folders", () => {
  it("adds a folder with one blank condition and names it as it is typed", async () => {
    await loaded();
    click("#addVirtualFolderBtn");
    const before = [find("[data-vf-title]", HTMLElement).textContent, status(), exists("[data-vf-empty-state]")];
    fill("[data-vf-name]", "4K Movies");
    expect({ before, after: find("[data-vf-title]", HTMLElement).textContent, rows: conditionRows() }).toStrictEqual({
      before: ["New virtual folder 1", "Added New virtual folder 1.", false],
      after: "4K Movies",
      rows: [["0", "entry_name"]],
    });
  });

  it("fills the blank condition with a sample, then adds one after it", async () => {
    await loaded();
    click("#addVirtualFolderBtn");
    click('[data-action="apply-sample"][data-sample="4k"]');
    click('[data-action="apply-sample"][data-sample="torrent"]');
    expect({ rows: conditionRows(), values: values("[data-vf-value]"), status: status() }).toStrictEqual({
      rows: [
        ["0", "entry_name"],
        ["1", "protocol"],
      ],
      values: ["2160p", "torrent"],
      status: "Torrent only example applied. You can edit it before saving.",
    });
  });
});

describe("editing virtual folder conditions", () => {
  it("rebuilds a condition when its field changes and removes it", async () => {
    await loaded();
    click("#addVirtualFolderBtn");
    fill("[data-vf-value]", "keep me");
    fill("[data-vf-field]", "file_count");
    const changed = [
      find("[data-vf-value]", HTMLInputElement).type,
      values("[data-vf-value]"),
      values("[data-vf-operator]"),
    ];
    click('[data-action="remove-condition"]');
    expect({ changed, rows: conditionRows(), none: exists("[data-vf-no-conditions]") }).toStrictEqual({
      changed: ["number", [""], ["greater_than"]],
      rows: [],
      none: true,
    });
  });

  it("removes a folder only after the confirmation", async () => {
    const { confirms } = await loaded({ answers: [false, true] });
    click("#addVirtualFolderBtn");
    click('[data-action="remove-folder"]');
    await vi.waitFor(() => {
      expect(confirms).toHaveLength(1);
    });
    const kept = exists("[data-virtual-folder]");
    click('[data-action="remove-folder"]');
    await vi.waitFor(() => {
      expect(document.querySelector("[data-vf-empty-state]")).not.toBeNull();
    });
    expect({ kept, confirms: confirms.length, empty: exists("[data-vf-empty-state]"), status: status() }).toStrictEqual(
      {
        kept: true,
        confirms: 2,
        empty: true,
        status: "this virtual folder removed. Save settings to apply this change.",
      },
    );
  });
});

describe("previewing a virtual folder", () => {
  const preview = { total: 1, samples: [{ name: "<b>x</b>", provider: "rd", size: 1024 }] };

  it("shows a folder's errors before asking the server", async () => {
    const { requests } = await loaded();
    click("#addVirtualFolderBtn");
    click('[data-action="preview"]');
    expect({ errors: texts("[data-vf-preview-result] li"), sent: posts(requests, "preview") }).toStrictEqual({
      errors: ["Folder name is required.", "Condition 1 needs a value."],
      sent: [],
    });
  });

  it("previews matches as text", async () => {
    const { requests } = await loaded({ routes: { "POST api/virtual-folders/preview": () => respond(200, preview) } });
    click("#addVirtualFolderBtn");
    fill("[data-vf-name]", "Movies 4K");
    fill("[data-vf-value]", "2160p");
    click('[data-action="preview"]');
    await vi.waitFor(() => {
      expect(texts("[data-vf-preview-result] li")).toStrictEqual(["<b>x</b>rd · 1 KB"]);
    });
    expect(posts(requests, "preview").map((request) => request.body)).toStrictEqual([
      '{"folder":{"name":"Movies 4K","match":"all","include_bad":false,"conditions":[{"field":"entry_name","operator":"contains","value":"2160p","case_sensitive":false}]},"limit":5}',
    ]);
  });
});

describe("saving virtual folders", () => {
  it("blocks a save on an invalid folder and lists why", async () => {
    const { requests } = await loaded();
    click("#addVirtualFolderBtn");
    click('[data-tab="arrs"]');
    submitSettings();
    await waitForToast(
      "alert-error Error saving configuration: Virtual folder #1: Folder name is required.Virtual folder #1: Condition 1 needs a value.",
    );
    expect({
      posts: posts(requests, "api/config"),
      listed: texts("[data-vf-validation-list] li"),
      shown: isHidden('[data-subtab-panel="virtual-folders"]'),
    }).toStrictEqual({
      posts: [],
      listed: ["Virtual folder #1: Folder name is required.", "Virtual folder #1: Condition 1 needs a value."],
      shown: false,
    });
  });
});

describe("queue cleanup rules", () => {
  it("lists the catalog and sends custom rules that have text", async () => {
    const { requests } = await loaded();
    click("#addQueueCleanupRuleBtn");
    click("#addQueueCleanupRuleBtn");
    fill("#queueCleanupCustom .queue-cleanup-custom-row:first-child .queue-cleanup-match", " stalled ");
    fill("#queueCleanupCustom .queue-cleanup-custom-row:first-child .queue-cleanup-action", "blacklist");
    click('#queueCleanupCustom .queue-cleanup-custom-row:last-child [data-action="remove-rule"]');
    submitSettings();
    const body = await savedBody(requests);
    expect({
      catalog: texts("#queueCleanupCatalog [data-rule-label]").length,
      custom: lastItem(member(body["queue_cleanup"], "rules")),
    }).toStrictEqual({ catalog: 9, custom: { match: "stalled", action: "blacklist" } });
  });
});
