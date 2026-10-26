import { describe, expect, it } from "vitest";

import { click, element, mutations, settle, stubReload, toasts } from "#ui/lib/testing/api";
import { XSS, answerConfirm, bootRepair } from "#ui/lib/testing/repair";

const submitRecheck = async (mediaId: string): Promise<void> => {
  element("#recheckMediaId", HTMLInputElement).value = mediaId;
  element("#recheckArr", HTMLSelectElement).value = "sonarr";
  element("#recheckFix", HTMLInputElement).checked = true;
  element("#recheckMediaBtn", HTMLButtonElement).click();
  element("#recheckMediaBtn", HTMLButtonElement).click();
  await settle();
};

describe("the run history controls", () => {
  it("clears the history after asking", async () => {
    const requests = await bootRepair({ "DELETE /api/repair/runs": () => new Response("", { status: 200 }) });
    await click("#clearHistoryBtn");
    const asked = await answerConfirm("ok");
    expect({ asked, posted: mutations(requests), last: requests.at(-1)?.url }).toStrictEqual({
      asked: "Clear all run history?",
      posted: [{ url: "/api/repair/runs", method: "DELETE", body: "" }],
      last: "/api/repair/runs",
    });
  });

  it("keeps the history when declined", async () => {
    const requests = await bootRepair();
    await click("#clearHistoryBtn");
    await answerConfirm("cancel");
    await click("#refreshHistoryBtn");
    expect(mutations(requests)).toStrictEqual([]);
  });

  it("reports a failed clear by its status", async () => {
    await bootRepair({ "DELETE /api/repair/runs": () => new Response("disk full", { status: 500 }) });
    await click("#clearHistoryBtn");
    await answerConfirm("ok");
    expect(toasts("error")).toStrictEqual(["Clear failed: HTTP 500"]);
  });

  it("reloads the history", async () => {
    const requests = await bootRepair();
    await click("#refreshHistoryBtn");
    expect(requests.at(-1)?.url).toBe("/api/repair/runs");
  });
});

describe("rechecking a media id", () => {
  it("requires a media id", async () => {
    const requests = await bootRepair();
    await submitRecheck("   ");
    expect({ toasts: toasts("warning"), posted: mutations(requests) }).toStrictEqual({
      toasts: ["Media id is required", "Media id is required"],
      posted: [],
    });
  });

  it("posts the trimmed id once and reloads", async () => {
    const requests = await bootRepair({ "POST /api/repair/recheck/media": () => Response.json({ id: "r" }) });
    const reloads = stubReload();
    await submitRecheck(" 42 ");
    expect({ posted: mutations(requests), reloads: reloads.count, toasts: toasts("success") }).toStrictEqual({
      posted: [
        { url: "/api/repair/recheck/media", method: "POST", body: '{"arr":"sonarr","media_id":"42","fix":true}' },
      ],
      reloads: 1,
      toasts: ["Recheck started"],
    });
  });

  it("shows the failure as text below the form", async () => {
    await bootRepair({ "POST /api/repair/recheck/media": () => Response.json({ error: XSS }, { status: 404 }) });
    await submitRecheck("42");
    const result = element("#recheckMediaResult", HTMLElement);
    expect({
      text: result.textContent,
      hidden: result.classList.contains("hidden"),
      images: result.querySelectorAll("img").length,
      disabled: element("#recheckMediaBtn", HTMLButtonElement).disabled,
    }).toStrictEqual({ text: `Recheck failed: ${XSS}`, hidden: false, images: 0, disabled: false });
  });
});
