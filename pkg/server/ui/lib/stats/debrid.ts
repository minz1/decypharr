import { formatBytes } from "#ui/lib/common/format";
import { asArray, asRecord, num, optBool, str } from "#ui/lib/common/json";
import { cloneElement, emptyMessage, field, setFields } from "#ui/lib/stats/dom";
import { formatNumber, orElse } from "#ui/lib/stats/format";
import { count, listOrEmpty, optRecord, recordOrEmpty, text } from "#ui/lib/stats/read";
import { debridSpeed, parseSpeedTest } from "#ui/lib/stats/speedtest";

interface Account {
  order: number;
  disabled: boolean;
  inUse: boolean;
  username: string;
  trafficUsed: number;
  expiration: string;
}

interface DebridStats {
  name: string;
  username: string;
  points: number;
  type: string;
  expiration: string;
  total: number;
  bad: number;
  activeLinks: number;
  accounts: Account[];
  speedTest: ReturnType<typeof parseSpeedTest> | undefined;
}

interface AccountView {
  title: string;
  disabled: boolean;
  inUse: boolean;
  username: string;
  usage: string;
  expires: string;
}

interface DebridView {
  provider: string;
  texts: Record<string, string>;
  accounts: AccountView[];
  speedTest: DebridStats["speedTest"];
}

const parseAccount = (value: unknown): Account => {
  const record = asRecord(value, "accounts");
  return {
    order: num(record, "order"),
    disabled: optBool(record, "disabled") ?? false,
    inUse: optBool(record, "in_use") ?? false,
    username: text(record, "username"),
    trafficUsed: count(record, "traffic_used"),
    expiration: text(record, "expiration"),
  };
};

const storedSpeedTest = (record: Record<string, unknown> | undefined): DebridStats["speedTest"] => {
  const result = record && parseSpeedTest(record);
  return result?.error === "" ? result : undefined;
};

const parseDebrid = (value: unknown): DebridStats => {
  const record = asRecord(value, "debrids");
  const profile = recordOrEmpty(record, "profile");
  const library = recordOrEmpty(record, "library");
  return {
    name: str(profile, "name"),
    username: text(profile, "username"),
    points: count(profile, "points"),
    type: text(profile, "type"),
    expiration: text(profile, "expiration"),
    total: count(library, "total"),
    bad: count(library, "bad"),
    activeLinks: count(library, "active_links"),
    accounts: listOrEmpty(record, "accounts").map((account) => parseAccount(account)),
    speedTest: storedSpeedTest(optRecord(record, "speed_test_result")),
  };
};

const parseDebrids = (value?: unknown): DebridStats[] =>
  value === undefined || value === null ? [] : asArray(value, "debrids").map((debrid) => parseDebrid(debrid));

const expiresText = (expiration: string): string =>
  `Expires: ${expiration === "" ? "Unknown" : new Date(expiration).toLocaleDateString()}`;

const accountView = (account: Account): AccountView => ({
  title: `Account #${account.order + 1}`,
  disabled: account.disabled,
  inUse: account.inUse,
  username: orElse(account.username, "-"),
  usage: `Daily Usage: ${formatBytes(account.trafficUsed)}`,
  expires: expiresText(account.expiration),
});

const debridView = (debrid: DebridStats): DebridView => ({
  provider: debrid.name,
  texts: {
    name: orElse(debrid.name, "Unknown Service"),
    username: orElse(debrid.username, "No username"),
    points: `${formatNumber(debrid.points)} points`,
    type: `Type: ${orElse(debrid.type, "Unknown")}`,
    expires: expiresText(debrid.expiration),
    "library-total": formatNumber(debrid.total),
    "library-bad": formatNumber(debrid.bad),
    "library-active": formatNumber(debrid.activeLinks),
    "accounts-total": String(debrid.accounts.length),
  },
  accounts: debrid.accounts.map((account) => accountView(account)),
  speedTest: debrid.speedTest,
});

const accountCard = (account: AccountView): HTMLElement => {
  const card = cloneElement("stats-debrid-account");
  setFields(card, { title: account.title, username: account.username, usage: account.usage, expires: account.expires });
  const status = field(card, "status");
  status.textContent = account.disabled ? "Disabled" : "Active";
  status.classList.add(account.disabled ? "badge-error" : "badge-success");
  if (!account.inUse) {
    field(card, "in-use").remove();
  }
  return card;
};

const debridCard = (view: DebridView): HTMLElement => {
  const card = cloneElement("stats-debrid");
  setFields(card, view.texts);
  field(card, "test").dataset["provider"] = view.provider;
  if (view.speedTest) {
    field(card, "speed-result").replaceChildren(debridSpeed(view.speedTest));
  }
  if (view.accounts.length === 0) {
    field(card, "accounts").remove();
  } else {
    field(card, "account-list").replaceChildren(...view.accounts.map((account) => accountCard(account)));
  }
  return card;
};

const renderDebrids = (content: HTMLElement, debrids: readonly DebridStats[]): void => {
  if (debrids.length === 0) {
    content.replaceChildren(emptyMessage("No debrid providers configured."));
    return;
  }
  const list = cloneElement("stats-provider-list");
  list.replaceChildren(...debrids.map((debrid) => debridCard(debridView(debrid))));
  content.replaceChildren(list);
};

export { debridView, parseDebrids, renderDebrids };
