import { setButtonLoading } from "#ui/lib/common/button";
import { copyToClipboard } from "#ui/lib/common/clipboard";
import { byId } from "#ui/lib/common/dom";
import { describeError } from "#ui/lib/common/errors";
import { fetcher } from "#ui/lib/common/http";
import { asRecord, optBool, optStr, readJSON, str } from "#ui/lib/common/json";
import { createToast } from "#ui/lib/common/toast";
import { orDefault, withId } from "#ui/lib/config/form";

const CREDENTIAL_FIELDS = ["auth-username", "auth-password", "auth-password-confirm"] as const;

const input = (doc: Document, id: string): HTMLInputElement => byId(id, HTMLInputElement, doc);

const toggleTokenOnly = (doc: Document): void => {
  const tokenOnly = input(doc, "auth-token-only").checked;
  for (const id of CREDENTIAL_FIELDS) {
    const field = withId(doc, id);
    if (field !== undefined) {
      field.disabled = tokenOnly;
      if (tokenOnly) {
        field.value = "";
      }
    }
  }
};

const populateToken = (doc: Document, config: Record<string, unknown>): void => {
  const token = withId(doc, "api-token-display");
  if (token !== undefined) {
    token.value = orDefault(optStr(config, "api_token"), "****");
  }
};

const populateAuth = (doc: Document, value: unknown): void => {
  const config = asRecord(value, "config");
  populateToken(doc, config);
  const username = withId(doc, "auth-username");
  const name = optStr(config, "auth_username") ?? "";
  if (username !== undefined && name !== "") {
    username.value = name;
  }
  const tokenOnly = withId(doc, "auth-token-only");
  if (tokenOnly instanceof HTMLInputElement) {
    tokenOnly.checked = optBool(config, "auth_token_only") === true;
    toggleTokenOnly(doc);
  }
};

const showPasswordMatch = (doc: Document): void => {
  const password = input(doc, "auth-password").value;
  const confirm = input(doc, "auth-password-confirm").value;
  const indicator = byId("password-match-indicator", HTMLElement, doc);
  if (password === "" && confirm === "") {
    indicator.textContent = "";
    indicator.className = "label-text-alt";
  } else if (password === confirm) {
    indicator.textContent = "✓ Passwords match";
    indicator.className = "label-text-alt text-success";
  } else {
    indicator.textContent = "✗ Passwords do not match";
    indicator.className = "label-text-alt text-error";
  }
};

const requestToken = async (): Promise<Record<string, unknown>> => {
  const response = await fetcher("/api/refresh-token", { method: "POST" });
  if (!response.ok) {
    throw new Error("Failed to refresh token");
  }
  return asRecord(await readJSON(response), "token");
};

const refreshAPIToken = async (doc: Document): Promise<void> => {
  const button = byId("refresh-token-btn", HTMLButtonElement, doc);
  setButtonLoading(button, true);
  try {
    const data = await requestToken();
    input(doc, "api-token-display").value = str(data, "token");
    createToast(orDefault(optStr(data, "message"), "Token refreshed successfully"), "success");
  } catch (error) {
    createToast(`Failed to refresh token: ${describeError(error)}`, "error");
  } finally {
    setButtonLoading(button, false);
  }
};

const copyAPIToken = async (doc: Document): Promise<void> => {
  const token = input(doc, "api-token-display").value;
  if (token === "" || token === "No token generated") {
    createToast("No token to copy. Please refresh the token first.", "warning");
    return;
  }
  await copyToClipboard(token);
};

const authBody = (doc: Document): string =>
  JSON.stringify({
    username: input(doc, "auth-username").value,
    password: input(doc, "auth-password").value,
    confirm_password: input(doc, "auth-password-confirm").value,
    token_only: input(doc, "auth-token-only").checked,
  });

const sendAuth = async (doc: Document): Promise<Record<string, unknown>> => {
  const response = await fetcher("/api/update-auth", { method: "POST", body: authBody(doc) });
  if (!response.ok) {
    const reason = await response.text();
    throw new Error(orDefault(reason, "Failed to update authentication settings"));
  }
  return asRecord(await readJSON(response), "auth");
};

const applyAuth = (doc: Document, data: Record<string, unknown>): void => {
  const warned = Boolean(data["warnings"]);
  createToast(optStr(data, "message") ?? "", warned ? "warning" : "success");
  const token = optStr(data, "token") ?? "";
  if (token !== "") {
    input(doc, "api-token-display").value = token;
  }
  input(doc, "auth-password").value = "";
  input(doc, "auth-password-confirm").value = "";
};

const updateAuthSettings = async (doc: Document): Promise<void> => {
  if (
    !input(doc, "auth-token-only").checked &&
    input(doc, "auth-password").value !== input(doc, "auth-password-confirm").value
  ) {
    createToast("Passwords do not match", "error");
    return;
  }
  const button = byId("update-auth-btn", HTMLButtonElement, doc);
  setButtonLoading(button, true);
  try {
    applyAuth(doc, await sendAuth(doc));
  } catch (error) {
    createToast(`Failed to update authentication: ${describeError(error)}`, "error");
  } finally {
    setButtonLoading(button, false);
  }
};

export { copyAPIToken, populateAuth, refreshAPIToken, showPasswordMatch, toggleTokenOnly, updateAuthSettings };
