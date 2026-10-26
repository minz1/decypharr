import { describe, expect, it, vi } from "vitest";

import { parseVersion, setupVersionInfo } from "#ui/lib/common/version";

const badgeAfter = async (response: Response): Promise<{ text: string | null; classes: string[]; href: string }> => {
  document.head.innerHTML = '<meta name="decypharr-url-base" content="/decypharr/">';
  document.body.innerHTML = '<div id="version-badge" class="badge badge-ghost">Loading...</div>';
  const urls: string[] = [];
  vi.stubGlobal("fetch", async (url: string): Promise<Response> => {
    urls.push(url);
    return Promise.resolve(response);
  });
  setupVersionInfo(document);
  const badge = document.querySelector("#version-badge");
  await vi.waitFor(() => {
    expect(badge?.textContent).not.toBe("Loading...");
  });
  expect(urls).toStrictEqual(["/decypharr/version"]);
  return {
    text: badge?.textContent ?? "",
    classes: [...(badge?.classList ?? [])],
    href: badge?.querySelector("a")?.getAttribute("href") ?? "",
  };
};

describe("the version badge", () => {
  it.each([
    ["stable", ["badge"]],
    ["beta", ["badge", "badge-warning"]],
    ["nightly", ["badge", "badge-error"]],
  ])("links the %s release", async (channel, classes) => {
    await expect(badgeAfter(Response.json({ version: "1.2.3", channel }))).resolves.toStrictEqual({
      text: `${channel}-1.2.3`,
      classes,
      href: "https://github.com/sirrobot01/decypharr/releases/tag/v1.2.3",
    });
  });

  it("renders the version as text", async () => {
    const shown = await badgeAfter(Response.json({ version: "<b>1</b>", channel: "stable" }));
    expect({ text: shown.text, bold: document.querySelector("#version-badge b") }).toStrictEqual({
      text: "stable-<b>1</b>",
      bold: document.querySelector("#absent"),
    });
  });

  it.each([
    ["a failed request", new Response("down", { status: 503 })],
    ["malformed JSON", new Response("<html>")],
    ["a wrong shape", Response.json({ version: 1 })],
  ])("says Unknown after %s", async (_case, response) => {
    await expect(badgeAfter(response)).resolves.toStrictEqual({
      text: "Unknown",
      classes: ["badge", "badge-ghost"],
      href: "",
    });
  });
});

describe("parsing /version", () => {
  it("reads the version and channel", () => {
    expect(parseVersion(JSON.parse('{"version":"1","channel":"beta","extra":true}'))).toStrictEqual({
      version: "1",
      channel: "beta",
    });
  });
});
