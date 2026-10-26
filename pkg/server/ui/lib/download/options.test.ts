import { describe, expect, it } from "vitest";

import { loadDownloadOptions, saveDownloadOptions } from "#ui/lib/download/options";

const stored = (entries: Readonly<Record<string, string>>): Storage => {
  localStorage.clear();
  for (const [key, value] of Object.entries(entries)) {
    localStorage.setItem(key, value);
  }
  return localStorage;
};

const options = { category: "sonarr", action: "download", uncached: true, rmTrackerUrls: true, folder: "/data" };

describe("loading the saved download options", () => {
  it("falls back to symlink, unchecked boxes and the configured folder", () => {
    expect(loadDownloadOptions(stored({}), "/downloads")).toStrictEqual({
      category: "",
      action: "symlink",
      uncached: false,
      rmTrackerUrls: false,
      folder: "/downloads",
    });
  });

  it("reads each option from its legacy key", () => {
    const storage = stored({
      downloadCategory: "sonarr",
      downloadAction: "download",
      downloadUncached: "true",
      rmTrackerUrls: "true",
      downloadFolder: "/data",
    });
    expect(loadDownloadOptions(storage, "/downloads")).toStrictEqual(options);
  });

  it("treats empty values as unset and only true as checked", () => {
    const storage = stored({ downloadAction: "", downloadFolder: "", downloadUncached: "yes" });
    expect(loadDownloadOptions(storage, "/downloads")).toMatchObject({
      action: "symlink",
      uncached: false,
      folder: "/downloads",
    });
  });
});

describe("saving the download options", () => {
  it("writes every option under its legacy key", () => {
    const storage = stored({});
    saveDownloadOptions(storage, options, true);
    const keys = ["downloadCategory", "downloadAction", "downloadUncached", "rmTrackerUrls", "downloadFolder"];
    expect(keys.map((key) => storage.getItem(key))).toStrictEqual(["sonarr", "download", "true", "true", "/data"]);
  });

  it("leaves the tracker choice alone when the config forces it", () => {
    const storage = stored({ rmTrackerUrls: "false" });
    saveDownloadOptions(storage, options, false);
    expect(storage.getItem("rmTrackerUrls")).toBe("false");
  });
});
