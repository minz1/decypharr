import { fetcher } from "#ui/lib/common/http";
import { readJSON } from "#ui/lib/common/json";
import { createToast } from "#ui/lib/common/toast";
import { populateAuth } from "#ui/lib/config/auth";
import { orDefault, toText } from "#ui/lib/config/form";
import { confirmPruneDelete, prunePayload, savedPrune } from "#ui/lib/config/prune";
import { assembleSettings, collectSections, populateSections } from "#ui/lib/config/sections";
import { validateSettings } from "#ui/lib/config/validate";
import { textItem } from "#ui/lib/config/vf-preview";

interface PageState {
  loaded: boolean;
}

interface SaveDeps {
  state: PageState;
  overlay: HTMLElement;
  confirm: (message: string) => boolean;
  reload: () => void;
}

interface SaveResult {
  restarted: boolean;
  warnings: string[];
}

const RELOAD_DELAY_MS = 2000;
const WARNING_RELOAD_DELAY_MS = 10_000;

const describe = (error: unknown): string => (error instanceof Error ? error.message : String(error));

const loadConfiguration = async (doc: Document, state: PageState): Promise<void> => {
  try {
    const config = await readJSON(await fetcher("/api/config"));
    populateSections(doc, config);
    populateAuth(doc, config);
    state.loaded = true;
  } catch {
    createToast("Error loading configuration", "error");
  }
};

const listFolderErrors = (doc: Document, summary: HTMLElement, errors: readonly string[]): void => {
  const list = summary.querySelector("[data-vf-validation-list]");
  if (list === null) {
    throw new Error("missing [data-vf-validation-list]");
  }
  list.replaceChildren(...errors.map((error) => textItem(doc, error)));
};

const showFolderErrors = (doc: Document, errors: readonly string[]): void => {
  const summary = doc.querySelector<HTMLElement>("#virtualFolderValidationSummary");
  const folderErrors = errors.filter((error) => error.startsWith("Virtual folder"));
  if (summary === null || folderErrors.length === 0) {
    return;
  }
  listFolderErrors(doc, summary, folderErrors);
  summary.classList.remove("hidden");
  globalThis.location.hash = "tab-virtual-folders";
  setTimeout(() => {
    summary.focus();
  }, 0);
};

const readSaveResult = async (response: Response): Promise<SaveResult> => {
  try {
    const result: unknown = JSON.parse(await response.text());
    const record = typeof result === "object" && result !== null ? result : {};
    const warnings: unknown = Reflect.get(record, "warnings");
    return {
      restarted: Reflect.get(record, "restarted") !== false,
      warnings: Array.isArray(warnings) ? warnings.map((warning) => toText(warning)) : [],
    };
  } catch {
    return { restarted: true, warnings: [] };
  }
};

const postSettings = async (body: Record<string, unknown>): Promise<Response> => {
  const response = await fetcher("/api/config", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!response.ok) {
    const reason = await response.text();
    throw new Error(orDefault(reason, "Failed to save configuration"));
  }
  return response;
};

const reportSaved = (result: SaveResult, deps: SaveDeps): void => {
  for (const warning of result.warnings) {
    createToast(warning, "warning");
  }
  if (result.restarted) {
    createToast("Configuration saved successfully! Services are restarting...", "success");
    setTimeout(deps.reload, result.warnings.length > 0 ? WARNING_RELOAD_DELAY_MS : RELOAD_DELAY_MS);
  } else {
    createToast("Configuration saved and applied.", "success");
    deps.overlay.classList.add("hidden");
  }
};

const checkSettings = (doc: Document, body: Record<string, unknown>): void => {
  const errors = validateSettings(body);
  if (errors.length > 0) {
    showFolderErrors(doc, errors);
    throw new Error(errors.join("\n"));
  }
  doc.querySelector("#virtualFolderValidationSummary")?.classList.add("hidden");
};

const submitSettings = async (doc: Document, deps: SaveDeps): Promise<void> => {
  const prune = prunePayload(doc);
  const body = assembleSettings(collectSections(doc).set("prune", prune));
  checkSettings(doc, body);
  if (!confirmPruneDelete(prune, deps.confirm)) {
    deps.overlay.classList.add("hidden");
    return;
  }
  const response = await postSettings(body);
  savedPrune(doc, prune);
  reportSaved(await readSaveResult(response), deps);
};

const saveConfiguration = async (doc: Document, deps: SaveDeps): Promise<void> => {
  if (!deps.state.loaded) {
    createToast("Settings didn't load. Reload the page before saving.", "error");
    return;
  }
  deps.overlay.classList.remove("hidden");
  try {
    await submitSettings(doc, deps);
  } catch (error) {
    createToast(`Error saving configuration: ${describe(error)}`, "error");
    deps.overlay.classList.add("hidden");
  }
};

const warnIncomplete = (search: string): void => {
  const params = new URLSearchParams(search);
  if (params.has("inco")) {
    createToast(`Incomplete configuration: ${params.get("inco") ?? ""}`, "warning");
  }
};

export type { PageState };
export { loadConfiguration, saveConfiguration, warnIncomplete };
