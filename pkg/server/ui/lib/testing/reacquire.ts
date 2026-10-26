import { expect, onTestFinished, vi } from "vitest";

import { stubAPI } from "#ui/lib/testing/api";
import { settle } from "#ui/lib/testing/dom";
import { loadFixture } from "#ui/lib/testing/fixture";

type Routes = Parameters<typeof stubAPI>[0];

const job = (id: string, status: string, fields: Record<string, unknown> = {}): Record<string, unknown> => ({
  id,
  status,
  cause: "stream",
  strategy: "history_failed",
  arrName: "sonarr",
  arrType: "sonarr",
  entryId: `entry-${id}`,
  fileId: `file-${id}`,
  bindings: [
    { entryFileName: `${id}.mkv`, entryName: `Show ${id}`, entryId: `entry-${id}`, entryFileId: `file-${id}` },
  ],
  ...fields,
});

const binding = (name: string, fields: Record<string, unknown> = {}): Record<string, unknown> => ({
  arrName: "sonarr",
  arrType: "sonarr",
  entryId: `entry-${name}`,
  entryName: `Show ${name}`,
  entryFileId: `file-${name}`,
  entryFileName: `${name}.mkv`,
  arrFileId: 7,
  seriesId: 3,
  seasonNumber: 1,
  ...fields,
});

const SUMMARY = { arrName: "Sonarr 4K", arrType: "sonarr", bindings: 12, actionable: 9, updatedAt: "" };

const REACQUIRE_ROUTES: Routes = {
  "GET /api/arr/reacquire/jobs": () => Response.json([]),
  "GET /api/arr/index": () => Response.json([SUMMARY]),
};

const LOAD_REQUESTS = Object.keys(REACQUIRE_ROUTES).length;

const bootReacquire = async (routes: Routes = {}, fixture = "reacquire"): Promise<ReturnType<typeof stubAPI>> => {
  vi.resetModules();
  if (!vi.isFakeTimers()) {
    // Real poll timers would outlive the test and call the next test's fetch stub.
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    onTestFinished(() => {
      vi.useRealTimers();
    });
  }
  loadFixture(fixture);
  const requests = stubAPI({ ...REACQUIRE_ROUTES, ...routes });
  await import("#ui/pages/reacquire");
  await vi.waitFor(() => {
    expect(requests.length).toBeGreaterThanOrEqual(LOAD_REQUESTS);
  });
  await settle();
  return requests;
};

export { SUMMARY, binding, bootReacquire, job };
