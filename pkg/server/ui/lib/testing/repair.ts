import { expect, onTestFinished, vi } from "vitest";

import { element, settle, stubAPI } from "#ui/lib/testing/api";
import { loadFixture } from "#ui/lib/testing/fixture";

const XSS = "<img src=x onerror=alert(1)>";

const IDLE_STATUS = { enabled: true, next_run_at: "2026-01-02T03:04:05Z", health_counts: { broken: 1, healthy: 4 } };

type Routes = Parameters<typeof stubAPI>[0];

const REPAIR_ROUTES: Routes = {
  "GET /api/repair/config": () => Response.json({ auto_repair: true, verify_content: false }),
  "GET /api/repair/status": () => Response.json(IDLE_STATUS),
  "GET /api/repair/runs": () => Response.json([]),
  "GET /api/arrs": () => Response.json([{ name: "sonarr" }, { name: XSS }]),
};

const LOAD_REQUESTS = Object.keys(REPAIR_ROUTES).length;

const bootRepair = async (routes: Routes = {}, fixture = "repair"): Promise<ReturnType<typeof stubAPI>> => {
  vi.resetModules();
  if (!vi.isFakeTimers()) {
    // Real poll timers would outlive the test and call the next test's fetch stub.
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    onTestFinished(() => {
      vi.useRealTimers();
    });
  }
  loadFixture(fixture);
  const requests = stubAPI({ ...REPAIR_ROUTES, ...routes });
  await import("#ui/pages/repair");
  await vi.waitFor(() => {
    expect(requests.length).toBeGreaterThanOrEqual(LOAD_REQUESTS);
  });
  await settle();
  return requests;
};

const brokenEntry = (name: string, failedAt = ""): Record<string, unknown> => ({
  entry_name: name,
  protocol: "torrent",
  file_count: 3,
  broken_count: 1,
  failure_reason: "missing",
  last_failed_at: failedAt,
  broken_files: [{ file_name: `${name}.mkv`, reason: "404", size: 1536, arr_name: "sonarr", media_id: 4 }],
});

const answerConfirm = async (answer: "cancel" | "ok"): Promise<string> => {
  const dialog = element("#confirmDialog", HTMLDialogElement);
  await vi.waitFor(() => {
    expect(dialog.open).toBe(true);
  });
  const message = element("#confirmDialogMessage", HTMLElement).textContent;
  element(`#confirmDialog [data-answer="${answer}"]`, HTMLButtonElement).click();
  await settle();
  return message;
};

export { IDLE_STATUS, XSS, answerConfirm, bootRepair, brokenEntry };
