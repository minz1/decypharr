import { describe, expect, it } from "vitest";

import { click, mutations } from "#ui/lib/testing/api";
import { answerConfirm } from "#ui/lib/testing/confirm";
import { element } from "#ui/lib/testing/dom";
import { bootReacquire, job } from "#ui/lib/testing/reacquire";
import { toasts } from "#ui/lib/testing/toast";
import { XSS } from "#ui/lib/testing/xss";

const DETAILED = job("a/1", "needs_attention", {
  arrName: "Sonarr 4K",
  downloadId: XSS,
  lastError: XSS,
  bindings: [{ entryFileName: "e.mkv", arrFileId: 4, confidence: "exact_path" }],
  mutations: [
    { kind: "delete_file", state: "confirmed", attempts: 1 },
    { kind: "blocklist", state: "intent" },
  ],
});

const openDetail = async (routes: Parameters<typeof bootReacquire>[0] = {}): Promise<ReturnType<typeof mutations>> => {
  const requests = await bootReacquire({ "GET /api/arr/reacquire/jobs": () => Response.json([DETAILED]), ...routes });
  await click("#jobsTableBody .job-detail-btn");
  return requests;
};

const detailText = (selector: string): string[] =>
  [...document.querySelectorAll(`#jobDetailBody ${selector}`)].map((node) =>
    node.textContent.replaceAll(/\s+/gu, " ").trim(),
  );

describe("the job detail", () => {
  it("shows the job as text", async () => {
    await openDetail();
    expect({
      open: element("#jobDetailModal", HTMLDialogElement).open,
      rows: detailText(".grid > div"),
      error: detailText(".alert-error"),
      tables: detailText("tbody tr"),
      images: document.querySelectorAll("#jobDetailBody img").length,
    }).toStrictEqual({
      open: true,
      rows: [
        "Status Needs attention",
        "Arr Sonarr 4K (sonarr)",
        "Source stream",
        "Strategy history_failed",
        `Broken download ${XSS}`,
        "Replacement -",
        "Created -",
        "Updated -",
        "Completed -",
      ],
      error: [XSS],
      tables: ["e.mkv 4 exact_path", "delete_file confirmed 1 -", "blocklist intent 0 -"],
      images: 0,
    });
  });
});

describe("acknowledging a job", () => {
  it("acknowledges a job that needs attention", async () => {
    const requests = await openDetail({ "POST /api/arr/reacquire/jobs/a%2F1/acknowledge": () => Response.json({}) });
    await click("#jobDetailBody > button");
    const asked = await answerConfirm("ok");
    expect({
      asked,
      posted: mutations(requests),
      open: element("#jobDetailModal", HTMLDialogElement).open,
      last: requests.at(-1)?.url,
    }).toStrictEqual({
      asked:
        "Check this job’s actions in Arr first. Acknowledgement releases duplicate protection and allows a new reacquisition. Have you checked the remote actions?",
      posted: [{ url: "/api/arr/reacquire/jobs/a%2F1/acknowledge", method: "POST", body: "" }],
      open: false,
      last: "/api/arr/reacquire/jobs",
    });
  });

  it("reports a failed acknowledgement", async () => {
    await openDetail({
      "POST /api/arr/reacquire/jobs/a%2F1/acknowledge": () => new Response("conflict", { status: 409 }),
    });
    await click("#jobDetailBody > button");
    await answerConfirm("ok");
    expect({
      toasts: toasts("error"),
      disabled: element("#jobDetailBody > button", HTMLButtonElement).disabled,
    }).toStrictEqual({ toasts: ["Could not acknowledge job: HTTP 409"], disabled: false });
  });

  it("does not acknowledge when declined", async () => {
    const requests = await openDetail();
    await click("#jobDetailBody > button");
    await answerConfirm("cancel");
    await click("#refreshJobsBtn");
    expect(mutations(requests)).toStrictEqual([]);
  });
});
