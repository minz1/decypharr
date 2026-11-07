import { describe, expect, it, onTestFinished, vi } from "vitest";

import { bootSettings, isHidden, respond, settingsLoaded, submitSettings, waitForToast } from "#ui/lib/testing/config";
import { labelledToasts } from "#ui/lib/testing/toast";

/** Fake timers and a recorded page reload; both are undone when the test finishes. */
const fakeReloads = (): string[] => {
  vi.useFakeTimers({ toFake: ["setTimeout"] });
  onTestFinished(() => {
    vi.useRealTimers();
  });
  const reloads: string[] = [];
  vi.spyOn(globalThis.location, "reload").mockImplementation(() => {
    reloads.push("reload");
  });
  return reloads;
};

describe("a save whose restart could not be requested", () => {
  it("says so instead of claiming the settings were applied, and does not reload", async () => {
    const reloads = fakeReloads();
    const reply = { restarted: false, restart: "failed", restart_error: "no generation to restart" };
    await bootSettings({ routes: { "POST api/config": () => respond(200, reply) } });
    await settingsLoaded();
    submitSettings();
    await waitForToast("alert-warning Saved; restart failed: no generation to restart");
    await vi.advanceTimersByTimeAsync(10_000);
    expect({ toasts: labelledToasts(), reloads, overlay: isHidden("#loadingOverlay") }).toStrictEqual({
      toasts: ["alert-warning Saved; restart failed: no generation to restart"],
      reloads: [],
      overlay: true,
    });
  });

  it("still reloads when the restart is pending", async () => {
    const reloads = fakeReloads();
    const reply = { restarted: true, restart: "pending" };
    await bootSettings({ routes: { "POST api/config": () => respond(202, reply) } });
    await settingsLoaded();
    submitSettings();
    await waitForToast("alert-success Configuration saved successfully! Services are restarting...");
    await vi.advanceTimersByTimeAsync(2000);
    expect(reloads).toStrictEqual(["reload"]);
  });
});
