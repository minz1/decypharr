import { describe, expect, it, vi } from "vitest";

import { loadPruneStatus, renderPruneStatus, runPruneNow } from "#ui/lib/config/prune-status";
import { loadFixture } from "#ui/lib/testing/fixture";
import { labelledToasts } from "#ui/lib/testing/toast";

type Route = Response | Error | (() => Response);

interface Run {
  requests: string[];
  confirms: string[];
  button: HTMLButtonElement;
}

const deletePrune = { enabled: true, mode: "delete", schedule: "1h", grace: "72h", max_per_run: 5, roots: ["/data/a"] };
const report = { started_at: "2026-10-24T12:00:00Z", mode: "report", aborted: "", candidates: [] };
const noCandidates: unknown = JSON.parse("null");
const xss = "<img src=x onerror=1>";

const json = (status: number, body: unknown): Response => Response.json(body, { status });

const reportConfig = { "GET /api/config": (): Response => json(200, { prune: { enabled: true, mode: "report" } }) };

const serve = (routes: Readonly<Record<string, Route>>): string[] => {
  const requests: string[] = [];
  vi.stubGlobal("fetch", async (url: string, init: RequestInit = {}): Promise<Response> => {
    const key = `${init.method ?? "GET"} ${url}`;
    requests.push(key);
    const route = routes[key];
    if (route instanceof Error) {
      return Promise.reject(route);
    }
    return Promise.resolve(typeof route === "function" ? route() : (route ?? json(404, "not found")));
  });
  return requests;
};

const pruneButton = (): HTMLButtonElement => {
  const button = document.querySelector("#pruneRunBtn");
  if (!(button instanceof HTMLButtonElement)) {
    throw new TypeError("the config fixture has no #pruneRunBtn");
  }
  return button;
};

const run = async (routes: Readonly<Record<string, Route>>, answers: readonly boolean[] = []): Promise<Run> => {
  loadFixture("config");
  const requests = serve(routes);
  const confirms: string[] = [];
  const button = pruneButton();
  await runPruneNow(document, button, async (message) => {
    confirms.push(message);
    return Promise.resolve(answers[confirms.length - 1] ?? false);
  });
  return { requests, confirms, button };
};

const render = (value: unknown): Element => {
  loadFixture("config");
  renderPruneStatus(document, value);
  const container = document.querySelector("#pruneStatus");
  if (container === null) {
    throw new Error("the config fixture has no #pruneStatus");
  }
  return container;
};

const cellTexts = (container: Element, selector: string): string[] =>
  [...container.querySelectorAll(selector)].map((cell) => cell.textContent);

const counts = (container: Element): [string, string][] =>
  [...container.querySelectorAll("dl div")].map((count) => [
    count.querySelector("dt")?.textContent ?? "",
    count.querySelector("dd")?.textContent ?? "",
  ]);

const waiting = (index: number): Record<string, unknown> => ({
  name: `waiting-${index}`,
  provider: "rd",
  excluded: "",
  size: 1,
  eligible_at: `2026-10-25T00:00:${String(index).padStart(2, "0")}Z`,
});

const queued = Array.from({ length: 100 }, (_unused, index) => ({
  name: `excluded-x${index}`,
  provider: "rd",
  excluded: "queued",
  size: 1,
}));

const crowded = {
  ...report,
  candidates: [
    { name: "excluded-1", provider: "rd", excluded: "manual", size: 1 },
    waiting(9),
    waiting(1),
    { name: "eligible-1", provider: "rd", excluded: "", size: 1, eligible_at: "2026-10-20T00:00:00Z" },
    ...queued,
  ],
};

describe("running prune now", () => {
  it("run now confirms when the saved config is delete", async () => {
    const { requests, confirms } = await run({ "GET /api/config": json(200, { prune: deletePrune }) }, [false]);
    expect({ requests, confirms }).toStrictEqual({
      requests: ["GET /api/config"],
      confirms: ["Run prune now in delete mode? It deletes up to 5 eligible entries."],
    });
  });

  it("run now posts after an accepted delete confirm", async () => {
    const routes = { "GET /api/config": json(200, { prune: deletePrune }), "POST /api/prune/run": json(200, report) };
    const { requests } = await run(routes, [true]);
    expect(requests).toStrictEqual(["GET /api/config", "POST /api/prune/run"]);
  });

  it("run now in report mode posts without a confirm", async () => {
    const during: boolean[] = [];
    const routes = {
      ...reportConfig,
      "POST /api/prune/run": (): Response => {
        during.push(pruneButton().disabled);
        return json(200, report);
      },
    };
    const { confirms, button } = await run(routes);
    expect({
      confirms,
      during,
      after: button.disabled,
      shown: cellTexts(document.body, "[data-prune-started]"),
    }).toStrictEqual({
      confirms: [],
      during: [true],
      after: false,
      shown: [`Started ${new Date(report.started_at).toLocaleString()}, mode report`],
    });
  });
});

describe("a failed prune run", () => {
  it.each([
    ["offline", new Error("offline"), "alert-error Could not read the saved prune settings: offline"],
    [
      "unauthorized",
      json(401, { error: "unauthorized" }),
      "alert-error Could not read the saved prune settings: unauthorized",
    ],
    ["malformed", new Response("{"), "alert-error Could not read the saved prune settings: response is not JSON"],
  ])("run now aborts when the config read fails: %s", async (_name, failure, toast) => {
    const { requests, button } = await run({ "GET /api/config": failure });
    expect({ requests, toasts: labelledToasts(), disabled: button.disabled }).toStrictEqual({
      requests: ["GET /api/config"],
      toasts: [toast],
      disabled: false,
    });
  });

  it.each([
    [
      "disabled",
      json(409, { error: "prune is disabled" }),
      "alert-warning Prune is disabled: enable it and save first",
    ],
    ["failed", json(500, { error: "scan failed: boom" }), "alert-error Prune run failed: scan failed: boom"],
    [
      "aborted",
      json(200, { ...report, aborted: "baseline dropped" }),
      "alert-warning Prune run aborted: baseline dropped",
    ],
    ["finished", json(200, report), "alert-success Prune run finished"],
    [
      "network",
      new Error("network"),
      "alert-warning Prune run: network. The run may still be in progress; reload the status.",
    ],
  ])("run now reports the server errors: %s", async (_name, answer, toast) => {
    const { button } = await run({ ...reportConfig, "POST /api/prune/run": answer });
    expect({ toasts: labelledToasts(), disabled: button.disabled }).toStrictEqual({ toasts: [toast], disabled: false });
  });
});

describe("the prune status text", () => {
  it("status renders server strings as text", () => {
    const container = render({
      ...report,
      aborted: xss,
      mode: xss,
      candidates: [
        { name: xss, provider: xss, excluded: "", size: 10, eligible_at: "2026-10-20T00:00:00Z" },
        { name: "b", provider: "rd", excluded: xss, size: 0 },
      ],
    });
    expect({
      images: container.querySelectorAll("img").length,
      aborted: cellTexts(container, ".alert-warning"),
      cells: cellTexts(container, "tbody td:first-child"),
    }).toStrictEqual({ images: 0, aborted: [`Aborted: ${xss}`], cells: [xss, "b"] });
  });

  it("an aborted run with null candidates shows the reason", () => {
    const container = render({ ...report, aborted: "referenced fell below 90%", candidates: noCandidates });
    expect(cellTexts(container, ".alert-warning")).toStrictEqual(["Aborted: referenced fell below 90%"]);
  });

  it("no run yet", () => {
    expect(render({ mode: "report", candidates: noCandidates }).textContent.trim()).toBe(
      "No run since decypharr started.",
    );
  });

  it("loads the status and writes a failure into the panel, not a toast", async () => {
    loadFixture("config");
    serve({ "GET /api/prune/status": json(500, { error: "status broke" }) });
    await loadPruneStatus(document);
    expect(document.querySelector("#pruneStatus")?.textContent.trim()).toBe(
      "Could not load the prune status: status broke",
    );
    expect(labelledToasts()).toStrictEqual([]);
  });
});

describe("the prune candidates", () => {
  it("eligibility compares instants, not strings", () => {
    const container = render({
      ...report,
      started_at: "2026-10-24T12:00:00+02:00",
      candidates: [{ name: "late", provider: "rd", excluded: "", size: 1, eligible_at: "2026-10-24T11:00:00Z" }],
    });
    expect(counts(container)).toContainEqual(["Eligible", "0"]);
  });

  it("candidates sort eligible, waiting, excluded and cap at 100", () => {
    const container = render(crowded);
    const names = cellTexts(container, "tbody td:first-child");
    expect({
      first: names.slice(0, 4),
      rows: names.length,
      last: names.at(-1),
      more: cellTexts(container, "[data-prune-more]"),
    }).toStrictEqual({
      first: ["eligible-1", "waiting-1", "waiting-9", "excluded-1"],
      rows: 100,
      last: "excluded-x95",
      more: ["and 4 more"],
    });
  });

  it("shows the counts and a candidate's cells as the legacy page did", () => {
    const container = render({
      ...report,
      accepted_baseline: true,
      links: 7,
      bytes_freed: 2048,
      candidates: [{ name: "a", provider: "rd", excluded: "manual", size: 1024 }],
    });
    expect({
      started: cellTexts(container, "[data-prune-started]"),
      counts: cellTexts(container, "dd"),
      cells: cellTexts(container, "tbody td"),
      aborted: cellTexts(container, ".alert-warning"),
    }).toStrictEqual({
      started: [`Started ${new Date(report.started_at).toLocaleString()}, mode report, baseline accepted`],
      counts: ["7", "", "", "", "", "1", "0", "", "", "2 KB"],
      cells: ["a", "rd", "1 KB", "", "Excluded: manual"],
      aborted: [],
    });
  });
});
