interface DownloadOptions {
  category: string;
  action: string;
  uncached: boolean;
  rmTrackerUrls: boolean;
  folder: string;
}

const KEYS = {
  category: "downloadCategory",
  action: "downloadAction",
  uncached: "downloadUncached",
  rmTrackerUrls: "rmTrackerUrls",
  folder: "downloadFolder",
} as const;

const DEFAULT_ACTION = "symlink";

const storedText = (storage: Storage, key: string): string => storage.getItem(key) ?? "";

const orDefault = (value: string, fallback: string): string => (value === "" ? fallback : value);

const loadDownloadOptions = (storage: Storage, defaultFolder: string): DownloadOptions => ({
  category: storedText(storage, KEYS.category),
  action: orDefault(storedText(storage, KEYS.action), DEFAULT_ACTION),
  uncached: storage.getItem(KEYS.uncached) === "true",
  rmTrackerUrls: storage.getItem(KEYS.rmTrackerUrls) === "true",
  folder: orDefault(storedText(storage, KEYS.folder), defaultFolder),
});

const saveDownloadOptions = (storage: Storage, options: DownloadOptions, saveRmTracker: boolean): void => {
  storage.setItem(KEYS.category, options.category);
  storage.setItem(KEYS.action, options.action);
  storage.setItem(KEYS.uncached, String(options.uncached));
  if (saveRmTracker) {
    storage.setItem(KEYS.rmTrackerUrls, String(options.rmTrackerUrls));
  }
  storage.setItem(KEYS.folder, options.folder);
};

export type { DownloadOptions };
export { loadDownloadOptions, saveDownloadOptions };
