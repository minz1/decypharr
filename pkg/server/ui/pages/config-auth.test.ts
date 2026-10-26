import { describe, expect, it, onTestFinished, vi } from "vitest";

import {
  bootSettings,
  click,
  fill,
  isHidden,
  posts,
  respond,
  settingsLoaded,
  shownToasts,
  waitForToast,
} from "#ui/lib/testing/config";
import { element } from "#ui/lib/testing/dom";

const input = (id: string): HTMLInputElement => element(`#${id}`, HTMLInputElement);

const loaded = async (
  options?: Parameters<typeof bootSettings>[0],
): Promise<Awaited<ReturnType<typeof bootSettings>>> => {
  const page = await bootSettings(options);
  await settingsLoaded();
  return page;
};

const withConfig = (config: Record<string, unknown>): Parameters<typeof bootSettings>[0] => {
  const merged = { debrids: [{ provider: "realdebrid", name: "rd", api_key: "k" }], ...config };
  return { routes: { "GET api/config": () => respond(200, merged) } };
};

const fakeClipboard = (): string[] => {
  const writes: string[] = [];
  vi.stubGlobal("navigator", {
    clipboard: {
      writeText: async (text: string): Promise<void> => {
        writes.push(text);
        return Promise.resolve();
      },
    },
  });
  return writes;
};

const nextAnswer = (answers: Response[]): Response => answers.shift() ?? respond(500, "no more answers");

const copyToken = async (token: string, toast: string): Promise<void> => {
  input("api-token-display").value = token;
  click("#copy-token-btn");
  await waitForToast(toast);
};

describe("the API token", () => {
  it("shows the token and disables the credentials in token-only mode", async () => {
    await loaded(withConfig({ api_token: "abc", auth_username: "admin", auth_token_only: true }));
    const tokenOnly = [input("api-token-display").value, input("auth-username").disabled, input("auth-username").value];
    input("auth-token-only").checked = false;
    input("auth-token-only").dispatchEvent(new Event("change"));
    expect({ tokenOnly, disabled: input("auth-username").disabled }).toStrictEqual({
      tokenOnly: ["abc", true, ""],
      disabled: false,
    });
  });

  it("refreshes the token", async () => {
    const reply = { token: "fresh", message: "Token refreshed" };
    const { requests } = await loaded({ routes: { "POST api/refresh-token": () => respond(200, reply) } });
    click("#refresh-token-btn");
    await waitForToast("alert-success Token refreshed");
    expect({
      token: input("api-token-display").value,
      posts: posts(requests, "api/refresh-token").length,
    }).toStrictEqual({
      token: "fresh",
      posts: 1,
    });
  });

  it("reports a failed token refresh and restores the button", async () => {
    await loaded({ routes: { "POST api/refresh-token": () => respond(500, "nope") } });
    click("#refresh-token-btn");
    await waitForToast("alert-error Failed to refresh token: Failed to refresh token");
    expect({ disabled: element("#refresh-token-btn", HTMLButtonElement).disabled }).toStrictEqual({ disabled: false });
  });

  it("refuses to copy a missing token and copies a present one", async () => {
    const writes = fakeClipboard();
    await loaded(withConfig({ api_token: "" }));
    await copyToken("", "alert-warning No token to copy. Please refresh the token first.");
    await copyToken("secret", "alert-success Copied to clipboard");
    expect(writes).toStrictEqual(["secret"]);
  });
});

describe("the credentials", () => {
  it("shows whether the passwords match", async () => {
    await loaded();
    fill("#auth-password", "a");
    fill("#auth-password-confirm", "b");
    const mismatch = element("#password-match-indicator", HTMLElement).className;
    fill("#auth-password-confirm", "a");
    expect([mismatch, element("#password-match-indicator", HTMLElement).textContent]).toStrictEqual([
      "label-text-alt text-error",
      "✓ Passwords match",
    ]);
  });

  it("refuses mismatched passwords without a request", async () => {
    const { requests } = await loaded();
    fill("#auth-password", "a");
    fill("#auth-password-confirm", "b");
    click("#update-auth-btn");
    await waitForToast("alert-error Passwords do not match");
    expect(posts(requests, "api/update-auth")).toStrictEqual([]);
  });

  it("updates the credentials and clears the passwords", async () => {
    const reply = { message: "Updated", warnings: ["careful"], token: "t2" };
    const { requests } = await loaded({ routes: { "POST api/update-auth": () => respond(200, reply) } });
    fill("#auth-username", "admin");
    fill("#auth-password", "pw");
    fill("#auth-password-confirm", "pw");
    click("#update-auth-btn");
    await waitForToast("alert-warning Updated");
    expect({
      body: posts(requests, "api/update-auth").map((request) => request.body),
      fields: [input("api-token-display").value, input("auth-password").value, input("auth-password-confirm").value],
    }).toStrictEqual({
      body: ['{"username":"admin","password":"pw","confirm_password":"pw","token_only":false}'],
      fields: ["t2", "", ""],
    });
  });
});

describe("the magnet handler", () => {
  it("registers the magnet handler and remembers it", async () => {
    const handlers: string[] = [];
    vi.stubGlobal("navigator", {
      registerProtocolHandler: (scheme: string, url: string) => handlers.push(`${scheme} ${url}`),
    });
    onTestFinished(() => {
      localStorage.clear();
    });
    await bootSettings({ fixture: "config@base" });
    click("#registerMagnetLink");
    await waitForToast("alert-success Magnet link handler registered successfully");
    const button = element("#registerMagnetLink", HTMLButtonElement);
    expect([handlers, localStorage.getItem("magnetHandler"), button.textContent, button.disabled]).toStrictEqual([
      [`magnet ${globalThis.location.origin}/decypharr/download?magnet=%s`],
      "true",
      "Magnet Handler Registered",
      true,
    ]);
  });

  it("warns when the browser cannot register a magnet handler", async () => {
    vi.stubGlobal("navigator", {});
    await bootSettings();
    click("#registerMagnetLink");
    await expect(
      shownToasts("alert-warning Magnet link registration not supported in this browser"),
    ).resolves.toHaveLength(1);
  });
});

describe("regenerating STRM files", () => {
  it("starts an STRM regeneration and reports a failure", async () => {
    const answers = [respond(200, ""), respond(400, "STRM is disabled")];
    await bootSettings({ routes: { "POST api/strm/regenerate": () => nextAnswer(answers) } });
    click("#strmRegenerateBtn");
    await waitForToast("alert-success STRM regeneration started in the background");
    click("#strmRegenerateBtn");
    await expect(shownToasts("alert-error STRM regenerate failed: STRM is disabled")).resolves.toHaveLength(2);
  });
});

describe("tabs", () => {
  it("opens a moved tab's parent and sub-tab from the hash", async () => {
    globalThis.location.hash = "tab-auth";
    await bootSettings();
    expect([
      isHidden('[data-tab-content="general"]'),
      isHidden('[data-subtab-panel="auth"]'),
      isHidden('[data-subtab-panel="settings"]'),
    ]).toStrictEqual([false, false, true]);
  });

  it("switches tabs on click and follows the loaded mount type", async () => {
    await loaded(withConfig({ mount: { type: "rclone", mount_path: "/mnt" } }));
    click('[data-tab="mounts"]');
    expect({
      shown: [isHidden('[data-tab-content="mounts"]'), isHidden('[data-mount-tab-content="rclone"]')],
      hidden: [isHidden('[data-tab-content="general"]'), isHidden('[data-mount-tab-content="dfs"]')],
      hash: globalThis.location.hash,
    }).toStrictEqual({ shown: [false, false], hidden: [true, true], hash: "#tab-mounts" });
  });
});
