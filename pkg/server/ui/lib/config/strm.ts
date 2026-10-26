import { describeError } from "#ui/lib/common/errors";
import { fetcher } from "#ui/lib/common/http";
import { asRecord, optBool, optStr } from "#ui/lib/common/json";
import { createToast } from "#ui/lib/common/toast";
import { checkedOf, named, orDefault, requireNamed } from "#ui/lib/config/form";

const input = (doc: Document, key: string): ReturnType<typeof requireNamed> => requireNamed(doc, `strm_${key}`);

const setChecked = (doc: Document, key: string, value: boolean): void => {
  const control = input(doc, key);
  if (control instanceof HTMLInputElement) {
    control.checked = value;
  }
};

const populateStrm = (doc: Document, value: unknown): void => {
  if (value === undefined || value === null) {
    return;
  }
  const strm = asRecord(value, "strm");
  setChecked(doc, "enabled", optBool(strm, "enabled") ?? false);
  input(doc, "path").value = optStr(strm, "path") ?? "";
  input(doc, "delivery_mode").value = orDefault(optStr(strm, "delivery_mode"), "proxy");
  input(doc, "sidecar_max_size").value = optStr(strm, "sidecar_max_size") ?? "";
  setChecked(doc, "keep_media_extension", optBool(strm, "keep_media_extension") ?? false);
  setChecked(doc, "download_sidecars", optBool(strm, "download_sidecars") !== false);
};

const optional = (doc: Document, key: string): ReturnType<typeof named> => named(doc, `strm_${key}`);

const collectStrm = (doc: Document): Record<string, unknown> => ({
  enabled: checkedOf(optional(doc, "enabled")) ?? false,
  path: optional(doc, "path")?.value.trim() ?? "",
  delivery_mode: orDefault(optional(doc, "delivery_mode")?.value, "proxy"),
  sidecar_max_size: optional(doc, "sidecar_max_size")?.value.trim() ?? "",
  keep_media_extension: checkedOf(optional(doc, "keep_media_extension")) ?? false,
  download_sidecars: checkedOf(optional(doc, "download_sidecars")) ?? true,
});

const regenerateStrm = async (button: HTMLButtonElement): Promise<void> => {
  button.disabled = true;
  try {
    const response = await fetcher("/api/strm/regenerate", { method: "POST" });
    if (!response.ok) {
      throw new Error(await response.text());
    }
    createToast("STRM regeneration started in the background", "success");
  } catch (error) {
    createToast(`STRM regenerate failed: ${describeError(error)}`, "error");
  } finally {
    button.disabled = false;
  }
};

export { collectStrm, populateStrm, regenerateStrm };
