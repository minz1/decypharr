import { describe, expect, inject, it } from "vitest";

import { asArray, asRecord } from "#ui/lib/common/json";
import { bootSettings, fill, respond, savedBody, settingsLoaded, submitSettings } from "#ui/lib/testing/config-page";

const firstItem = (config: Record<string, unknown>, list: string): Record<string, unknown> =>
  asRecord(asArray(config[list], list)[0], `${list}[0]`);

const storedConfig = (): Record<string, unknown> => {
  const config = asRecord(structuredClone(inject("configResponses")["config"]), "config");
  Object.assign(firstItem(config, "debrids"), { download_api_keys: ["old-a", "old-b"] });
  Object.assign(firstItem(config, "arrs"), { download_uncached: true });
  return config;
};

const sent = (item: Record<string, unknown>, key: string): string =>
  JSON.stringify(Object.fromEntries(Object.entries(item).filter(([name]) => name === key)));

describe("clearing a stored value", () => {
  it("sends an arr's inherit choice and an emptied download key list, so the server drops the old values", async () => {
    const { requests } = await bootSettings({ routes: { "GET api/config": () => respond(200, storedConfig()) } });
    await settingsLoaded();
    fill('[name="arr[0].download_uncached"]', "");
    fill('[name="debrid[0].download_api_keys"]', "  \n ");
    submitSettings();
    const body = await savedBody(requests);
    expect([
      sent(firstItem(body, "arrs"), "download_uncached"),
      sent(firstItem(body, "debrids"), "download_api_keys"),
    ]).toStrictEqual(['{"download_uncached":null}', '{"download_api_keys":[]}']);
  });
});
