import { describe, expect, it } from "vitest";

import { click, element, mutations, stubReload, toasts } from "#ui/lib/testing/api";
import { answerConfirm, bootRepair } from "#ui/lib/testing/repair";

const RUN_BODY =
  '{"ignore_last_checked":true,"auto_repair":true,"unrestrict_link":true,"verify_content":false,"protocol":"nzb"}';

const dialogOpen = (selector: string): boolean => element(selector, HTMLDialogElement).open;

const checked = (selector: string): boolean => element(selector, HTMLInputElement).checked;

const submitRun = async (): Promise<void> => {
  await click("#runNowBtn");
  element("#runIgnoreLastChecked", HTMLInputElement).checked = true;
  element("#runProtocolNZB", HTMLInputElement).checked = true;
  element("#runRepairSubmitBtn", HTMLButtonElement).click();
  await click("#runRepairSubmitBtn");
};

const tick = (value: string): void => {
  element(`input[name="repair_state"][value="${value}"]`, HTMLInputElement).checked = true;
};

const submitClear = async (...values: readonly string[]): Promise<void> => {
  await click("#clearStateBtn");
  for (const value of values) {
    tick(value);
  }
  await click("#clearStateSubmitBtn");
};

describe("the run modal", () => {
  it("opens with the configured defaults", async () => {
    await bootRepair();
    element("#runIgnoreLastChecked", HTMLInputElement).checked = true;
    await click("#runNowBtn");
    expect({
      open: dialogOpen("#runRepairModal"),
      toggles: ["#runIgnoreLastChecked", "#runAutoRepair", "#runVerifyContent", "#runProtocolAll"].map((selector) =>
        checked(selector),
      ),
    }).toStrictEqual({ open: true, toggles: [false, true, false, true] });
  });

  it("closes with Cancel", async () => {
    await bootRepair();
    await click("#runNowBtn");
    await click("#runRepairCancelBtn");
    expect({ open: dialogOpen("#runRepairModal") }).toStrictEqual({ open: false });
  });
});

describe("running a sweep", () => {
  it("posts the options once and reports the start", async () => {
    const requests = await bootRepair({ "POST /api/repair/run": () => Response.json({ id: "r" }) });
    await submitRun();
    expect({
      posted: mutations(requests),
      open: dialogOpen("#runRepairModal"),
      toasts: toasts("success"),
      statusLoads: requests.filter((request) => request.url === "/api/repair/status").length,
      submitDisabled: element("#runRepairSubmitBtn", HTMLButtonElement).disabled,
    }).toStrictEqual({
      posted: [{ url: "/api/repair/run", method: "POST", body: RUN_BODY }],
      open: false,
      toasts: ["Sweep started, including freshly checked entries"],
      statusLoads: 2,
      submitDisabled: false,
    });
  });

  it("reports a refused run and keeps the modal open", async () => {
    await bootRepair({ "POST /api/repair/run": () => new Response("already running", { status: 409 }) });
    await submitRun();
    expect({ toasts: toasts("error"), open: dialogOpen("#runRepairModal") }).toStrictEqual({
      toasts: ["Run failed: already running"],
      open: true,
    });
  });

  it("asks the server to stop the run", async () => {
    const requests = await bootRepair({ "POST /api/repair/stop": () => new Response("", { status: 200 }) });
    element("#stopRunBtn", HTMLButtonElement).disabled = false;
    await click("#stopRunBtn");
    expect({ posted: mutations(requests), toasts: toasts("info") }).toStrictEqual({
      posted: [{ url: "/api/repair/stop", method: "POST", body: "" }],
      toasts: ["Stop requested"],
    });
  });
});

describe("fixing every broken entry", () => {
  it("asks first and does nothing when declined", async () => {
    const requests = await bootRepair();
    await click("#fixBrokenBtn");
    const asked = await answerConfirm("cancel");
    await click("#refreshHistoryBtn");
    expect({ asked, posted: mutations(requests) }).toStrictEqual({
      asked: "Send delete + re-search for every currently broken entry to its Arr?",
      posted: [],
    });
  });

  it("posts an empty object and reloads", async () => {
    const requests = await bootRepair({ "POST /api/repair/fix": () => Response.json({}) });
    const reloads = stubReload();
    await click("#fixBrokenBtn");
    await answerConfirm("ok");
    expect({ posted: mutations(requests), reloads: reloads.count, toasts: toasts("success") }).toStrictEqual({
      posted: [{ url: "/api/repair/fix", method: "POST", body: "{}" }],
      reloads: 1,
      toasts: ["Fix-broken started"],
    });
  });

  it("reports a failure and re-enables the button", async () => {
    await bootRepair({ "POST /api/repair/fix": () => new Response("", { status: 502 }) });
    await click("#fixBrokenBtn");
    await answerConfirm("ok");
    expect({ toasts: toasts("error"), disabled: element("#fixBrokenBtn", HTMLButtonElement).disabled }).toStrictEqual({
      toasts: ["Fix failed: HTTP 502"],
      disabled: false,
    });
  });
});

describe("the clear state modal", () => {
  it("opens with nothing ticked and the latest counts", async () => {
    await bootRepair();
    tick("broken");
    await click("#clearStateBtn");
    expect({
      open: dialogOpen("#clearStateModal"),
      ticked: document.querySelectorAll('input[name="repair_state"]:checked').length,
      counts: [...document.querySelectorAll("[data-clear-state-count]")].map((badge) => badge.textContent),
    }).toStrictEqual({ open: true, ticked: 0, counts: ["0", "1", "4", "0", "0", "0"] });
  });

  it("requires a state", async () => {
    const requests = await bootRepair();
    await submitClear();
    const error = element("#clearStateError", HTMLElement);
    expect({
      error: error.textContent,
      hidden: error.classList.contains("hidden"),
      posted: mutations(requests),
    }).toStrictEqual({ error: "Select at least one state.", hidden: false, posted: [] });
  });
});

describe("clearing repair state", () => {
  it("posts the ticked states and reports the count", async () => {
    const requests = await bootRepair({ "POST /api/repair/clear-state": () => Response.json({ cleared: 1 }) });
    await submitClear("broken", "stale");
    expect({
      posted: mutations(requests),
      toasts: toasts("success"),
      open: dialogOpen("#clearStateModal"),
      reloaded: requests
        .slice(-2)
        .map((request) => request.url)
        .toSorted(),
    }).toStrictEqual({
      posted: [{ url: "/api/repair/clear-state", method: "POST", body: '{"statuses":["broken","stale"]}' }],
      toasts: ["Cleared 1 repair state entry"],
      open: false,
      reloaded: ["/api/repair/health?status=broken", "/api/repair/status"],
    });
  });

  it("shows the server's error in the modal and as a toast", async () => {
    await bootRepair({
      "POST /api/repair/clear-state": () => Response.json({ error: "Repair service not available" }, { status: 503 }),
    });
    await submitClear("unknown");
    expect({ error: element("#clearStateError", HTMLElement).textContent, toasts: toasts("error") }).toStrictEqual({
      error: "Repair service not available",
      toasts: ["Clear failed: Repair service not available"],
    });
  });
});
