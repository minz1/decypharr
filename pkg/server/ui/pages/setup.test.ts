import { describe, expect, it, vi } from "vitest";

import { loadFixture } from "#ui/lib/testing/fixture";
import {
  bootSetup,
  choose,
  click,
  element,
  errorToasts,
  fill,
  fillUsenet,
  marked,
  passDownloadAndMount,
  shownError,
  value,
  visibleSteps,
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

describe("opening the wizard", () => {
  it("loads the config under the URL base and shows step 1", async () => {
    const booted = await bootSetup({ fixture: "setup@base" });
    expect({
      requests: booted.requests.map((request) => request.url),
      steps: visibleSteps(),
      label: element("setup-progress-label", HTMLElement).textContent,
      name: element("setup-progress-name", HTMLElement).textContent,
      progress: element("setup-progress", HTMLProgressElement).value,
      marked: marked(),
    }).toStrictEqual({
      requests: ["/decypharr/api/config"],
      steps: ["step-1"],
      label: "Step 1 of 6",
      name: "Authentication",
      progress: 1,
      marked: ["step-indicator-1"],
    });
  });

  it("fills the fields from the saved config", async () => {
    await bootSetup({
      config: json({
        auth_username: "admin",
        debrids: [{ provider: "torbox", api_key: "key" }],
        download_folder: "/downloads",
        rclone: { enabled: true, mount_path: "/mnt/rclone", buffer_size: "64M" },
      }),
    });
    expect([
      value("auth-username"),
      element("debrid-provider", HTMLSelectElement).value,
      value("debrid-api-key"),
      value("download-folder"),
      element("mount-type-rclone", HTMLInputElement).checked,
      value("mount-path"),
      value("rclone-buffer-size"),
    ]).toStrictEqual(["admin", "torbox", "key", "/downloads", true, "/mnt/rclone", "64M"]);
  });
});

describe("opening the wizard when something is wrong", () => {
  it("starts empty when the config cannot be read", async () => {
    await bootSetup({ config: text("denied", 401) });
    expect({ steps: visibleSteps(), toasts: errorToasts() }).toStrictEqual({ steps: ["step-1"], toasts: [] });
  });

  it("reports a malformed config in a toast and still starts", async () => {
    await bootSetup({ config: text("{", 200) });
    expect({ steps: visibleSteps(), toasts: errorToasts() }).toStrictEqual({
      steps: ["step-1"],
      toasts: ["Failed to load config: response is not JSON"],
    });
  });

  it("names a missing element in an error toast", async () => {
    vi.resetModules();
    loadFixture("setup");
    document.querySelector("#finish-btn")?.remove();
    vi.stubGlobal("fetch", json({}));
    await import("#ui/pages/setup");
    await vi.waitFor(() => {
      expect(errorToasts()).toStrictEqual(["setup: missing #finish-btn"]);
    });
  });
});

describe("moving through the steps", () => {
  it("validates a step before moving on and clears the message on the next step", async () => {
    await bootSetup();
    fill({ "auth-username": "admin" });
    click("auth-next-btn");
    const blocked = { steps: visibleSteps(), error: shownError() };
    fill({ "auth-password": "secret", "auth-confirm-password": "secret" });
    click("auth-next-btn");
    expect([blocked, { steps: visibleSteps(), error: shownError() }]).toStrictEqual([
      { steps: ["step-1"], error: "Password is required" },
      { steps: ["step-2"], error: "" },
    ]);
  });

  it("goes back a step and marks the progress", async () => {
    await bootSetup();
    click("skip-auth-btn");
    click("skip-debrid-btn");
    const third = { steps: visibleSteps(), label: element("setup-progress-label", HTMLElement).textContent };
    const thirdMarked = marked().length;
    click("usenet-back-btn");
    click("debrid-back-btn");
    expect([third, thirdMarked, visibleSteps(), marked()]).toStrictEqual([
      { steps: ["step-3"], label: "Step 3 of 6" },
      3,
      ["step-1"],
      ["step-indicator-1"],
    ]);
  });
});

describe("the wizard's guards", () => {
  it("will not pass the usenet step without a provider", async () => {
    await bootSetup();
    click("token-only-btn");
    click("skip-debrid-btn");
    click("skip-usenet-btn");
    expect({ steps: visibleSteps(), error: shownError() }).toStrictEqual({
      steps: ["step-3"],
      error: "Please configure at least one Debrid or Usenet provider before continuing.",
    });
  });

  it("shows the rclone options only for rclone and external mounts", async () => {
    await bootSetup();
    const radios = ["mount-type-rclone", "mount-type-dfs", "mount-type-external", "mount-type-none"];
    expect(radios.map((id) => choose(id))).toStrictEqual([true, false, true, false]);
  });

  it("renders user values in the overview as text", async () => {
    await bootSetup();
    click("skip-auth-btn");
    click("skip-debrid-btn");
    fillUsenet("<img src=x onerror=alert(1)>");
    click("usenet-next-btn");
    passDownloadAndMount();
    expect({
      steps: visibleSteps(),
      usenet: element("overview-usenet", HTMLElement).textContent.replaceAll(/\s+/gu, " ").trim(),
      images: document.querySelectorAll("#step-6 img").length,
    }).toStrictEqual({
      steps: ["step-6"],
      usenet:
        "Server: <img src=x onerror=alert(1)>:563 Username: user Max Connections: 30 Connections Per Stream: 15 Use SSL: No",
      images: 0,
    });
  });
});
