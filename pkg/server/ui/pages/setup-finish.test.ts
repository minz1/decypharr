import { describe, expect, it, vi } from "vitest";

import { element } from "#ui/lib/testing/dom";
import {
  REDIRECT_WAIT,
  bootSetup,
  click,
  hidden,
  posted,
  postedBodies,
  shownError,
  value,
  walkToOverview,
  walkTokenOnly,
} from "#ui/lib/testing/setup";

type Respond = () => Promise<Response>;

const json =
  (body: unknown, status = 200): Respond =>
  async () =>
    Promise.resolve(Response.json(body, { status }));

const text =
  (body: string, status: number): Respond =>
  async () =>
    Promise.resolve(new Response(body, { status }));

describe("finishing the setup", () => {
  it("posts what setup.js posted, once, and goes home under the URL base", async () => {
    const pending = Promise.withResolvers<Response>();
    const booted = await bootSetup({ fixture: "setup@base", complete: async () => pending.promise });
    walkToOverview();
    click("finish-btn");
    click("finish-btn");
    pending.resolve(Response.json({ success: true }));
    await vi.waitFor(() => {
      expect(booted.navigations).toStrictEqual(["/decypharr/"]);
    }, REDIRECT_WAIT);
    expect(posted(booted)).toStrictEqual([
      {
        url: "/decypharr/api/setup/complete",
        method: "POST",
        body:
          '{"auth":{"username":"admin","password":"secret","skip_auth":false},' +
          '"debrid":{"provider":"torbox","api_key":"key"},' +
          '"usenet":{"host":"news.example.com","port":563,"username":"user","password":"pass",' +
          '"max_connections":30,"reader_connections":15,"ssl":true,"skip_usenet":false},' +
          '"download":{"download_folder":"/downloads"},' +
          '"mount":{"mount_type":"dfs","mount_path":"/mnt/decypharr","cache_dir":"/cache"}}',
      },
    ]);
  });

  it.each([
    ["the server's error", json({ success: false, error: "Invalid request format" }, 400), "Invalid request format"],
    ["a fallback for a text refusal", text("forbidden", 403), "Failed to complete setup"],
    ["a malformed success", text("<html>", 200), "response is not JSON"],
  ])("shows %s and lets the user retry", async (_name, complete, error) => {
    const booted = await bootSetup({ complete });
    walkToOverview();
    click("finish-btn");
    await vi.waitFor(() => {
      expect(shownError()).toBe(error);
    });
    expect([
      element("#finish-btn", HTMLButtonElement).disabled,
      hidden("finish-btn-text"),
      hidden("finish-btn-loading"),
    ]).toStrictEqual([false, false, true]);
    expect(booted.navigations).toStrictEqual([]);
  });
});

describe("finishing a token-only setup", () => {
  it("shows the minted API token, then goes home once it is dismissed", async () => {
    const booted = await bootSetup({ complete: json({ success: true, api_token: "tok-<b>1</b>" }) });
    walkTokenOnly();
    click("finish-btn");
    await vi.waitFor(() => {
      expect(document.querySelector("#setup-token-dialog[open]")).not.toBeNull();
    });
    const shown = { token: value("setup-token-value"), navigations: [...booted.navigations] };
    click("setup-token-done");
    await vi.waitFor(() => {
      expect(booted.navigations).toStrictEqual(["/"]);
    }, REDIRECT_WAIT);
    expect(shown).toStrictEqual({ token: "tok-<b>1</b>", navigations: [] });
    expect(postedBodies(booted)).toMatchObject([{ auth: { token_only: true } }]);
  });
});
