import { onAction } from "#ui/lib/common/dom";
import { fetcher } from "#ui/lib/common/http";
import { asRecord, optStr, readJSON } from "#ui/lib/common/json";
import { createToast } from "#ui/lib/common/toast";
import { joinURL } from "#ui/lib/common/url";
import { parseSetupConfig } from "#ui/lib/setup/config";
import { setupOverview } from "#ui/lib/setup/overview";
import {
  STEP_ORDER,
  emptySetupState,
  providerConfigured,
  readAuthStep,
  readDebridStep,
  readDownloadStep,
  readMountStep,
  readUsenetStep,
  setupPayload,
} from "#ui/lib/setup/steps";
import type { SetupState, StepName, StepOutcome } from "#ui/lib/setup/types";
import { fillFields, setFinishBusy, setupView, showError, showOverview, showStep } from "#ui/lib/setup/view";

type SetupView = ReturnType<typeof setupView>;

interface WizardDeps {
  navigate: (url: string) => void;
  urlBase: string;
}

interface Wizard {
  view: SetupView;
  state: SetupState;
  deps: WizardDeps;
}

type StateKey = keyof SetupState;

const REDIRECT_DELAY_MS = 1500;
const FINISH_FAILED = "Failed to complete setup";
const PROVIDER_REQUIRED = "Please configure at least one Debrid or Usenet provider before continuing.";

const describeError = (error: unknown): string => (error instanceof Error ? error.message : String(error));

const stepAfter = (step: StepName): StepName => STEP_ORDER[STEP_ORDER.indexOf(step) + 1] ?? "overview";

const goToStep = (wizard: Wizard, step: StepName): void => {
  showStep(wizard.view, step);
  if (step === "overview") {
    showOverview(wizard.view, setupOverview(wizard.state));
  }
};

const ensureProvider = (wizard: Wizard): boolean => {
  if (providerConfigured(wizard.state)) {
    return true;
  }
  showError(wizard.view, PROVIDER_REQUIRED);
  return false;
};

const advance = <Key extends StateKey>(wizard: Wizard, key: Key, outcome: StepOutcome<SetupState[Key]>): void => {
  if ("error" in outcome) {
    showError(wizard.view, outcome.error);
    return;
  }
  wizard.state[key] = outcome.step;
  if (key !== "usenet" || ensureProvider(wizard)) {
    goToStep(wizard, stepAfter(key));
  }
};

const authNext = (wizard: Wizard): void => {
  const { input } = wizard.view;
  const outcome = readAuthStep({
    username: input("auth-username").value,
    password: input("auth-password").value,
    confirmPassword: input("auth-confirm-password").value,
  });
  advance(wizard, "auth", outcome);
};

const debridNext = (wizard: Wizard): void => {
  const outcome = readDebridStep({
    provider: wizard.view.provider.value,
    apiKey: wizard.view.input("debrid-api-key").value,
  });
  advance(wizard, "debrid", outcome);
};

const usenetNext = (wizard: Wizard): void => {
  const { input } = wizard.view;
  const outcome = readUsenetStep({
    host: input("usenet-host").value,
    port: input("usenet-port").value,
    username: input("usenet-username").value,
    password: input("usenet-password").value,
    maxConnections: input("usenet-max-connections").value,
    readerConnections: input("usenet-max-connections-per-reader").value,
    ssl: input("usenet-ssl").checked,
  });
  advance(wizard, "usenet", outcome);
};

const mountNext = (wizard: Wizard): void => {
  const { input } = wizard.view;
  const checked = wizard.view.mountTypes.find((radio) => radio.checked);
  const outcome = readMountStep({
    mountType: checked?.value ?? "",
    mountPath: input("mount-path").value,
    cacheDir: input("cache-dir").value,
    rcloneBufferSize: input("rclone-buffer-size").value,
  });
  advance(wizard, "mount", outcome);
};

const toggleMountOptions = (view: SetupView): void => {
  const hidden = view.input("mount-type-dfs").checked || view.input("mount-type-none").checked;
  view.panel("rclone-options").classList.toggle("hidden", hidden);
};

const parsedRefusal = (body: string): string => {
  const refusal = asRecord(JSON.parse(body), "response");
  return optStr(refusal, "error") ?? FINISH_FAILED;
};

const refusalMessage = async (response: Response): Promise<string> => {
  const body = await response.text();
  try {
    return parsedRefusal(body);
  } catch {
    return FINISH_FAILED;
  }
};

const completeSetup = async (wizard: Wizard): Promise<string> => {
  const response = await fetcher("/api/setup/complete", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(setupPayload(wizard.state)),
  });
  if (!response.ok) {
    throw new Error(await refusalMessage(response));
  }
  const result = asRecord(await readJSON(response), "response");
  return optStr(result, "api_token") ?? "";
};

const goHomeSoon = (wizard: Wizard): void => {
  createToast("Setup completed successfully! Redirecting...", "success");
  setTimeout(() => {
    wizard.deps.navigate(joinURL(wizard.deps.urlBase, "/"));
  }, REDIRECT_DELAY_MS);
};

const showToken = (wizard: Wizard, token: string): void => {
  const { tokenDialog, input } = wizard.view;
  tokenDialog.addEventListener(
    "close",
    () => {
      goHomeSoon(wizard);
    },
    { once: true },
  );
  input("setup-token-value").value = token;
  tokenDialog.showModal();
  input("setup-token-value").select();
};

const submit = async (wizard: Wizard): Promise<void> => {
  try {
    const token = await completeSetup(wizard);
    if (token === "") {
      goHomeSoon(wizard);
    } else {
      showToken(wizard, token);
    }
  } catch (error) {
    const message = describeError(error);
    showError(wizard.view, message === "" ? FINISH_FAILED : message);
    setFinishBusy(wizard.view, false);
  }
};

const finish = async (wizard: Wizard): Promise<void> => {
  setFinishBusy(wizard.view, true);
  if (!ensureProvider(wizard)) {
    setFinishBusy(wizard.view, false);
    return;
  }
  await submit(wizard);
};

const isStep = (name: string | undefined): name is StepName => STEP_ORDER.some((step) => step === name);

const ACTIONS: Readonly<Record<string, (wizard: Wizard, target: HTMLElement) => void>> = {
  "auth-next": authNext,
  "skip-auth": (wizard) => {
    advance(wizard, "auth", { step: { skip_auth: true } });
  },
  "token-only": (wizard) => {
    advance(wizard, "auth", { step: { token_only: true } });
  },
  "debrid-next": debridNext,
  "skip-debrid": (wizard) => {
    advance(wizard, "debrid", { step: { skip_debrid: true } });
  },
  "usenet-next": usenetNext,
  "skip-usenet": (wizard) => {
    advance(wizard, "usenet", { step: { skip_usenet: true } });
  },
  "download-next": (wizard) => {
    advance(wizard, "download", readDownloadStep(wizard.view.input("download-folder").value));
  },
  "mount-next": mountNext,
  "go-to": (wizard, target) => {
    const { step } = target.dataset;
    if (isStep(step)) {
      goToStep(wizard, step);
    }
  },
  finish: (wizard) => {
    void finish(wizard);
  },
  "close-token": (wizard) => {
    wizard.view.tokenDialog.close();
  },
};

const bindWizard = (wizard: Wizard): void => {
  const bound = Object.entries(ACTIONS).map(([name, action]): [string, (target: HTMLElement) => void] => [
    name,
    (target: HTMLElement): void => {
      action(wizard, target);
    },
  ]);
  onAction(wizard.view.root, "click", Object.fromEntries(bound));
  onAction(wizard.view.root, "change", {
    "mount-type": () => {
      toggleMountOptions(wizard.view);
    },
  });
};

const fetchConfig = async (): Promise<Response | undefined> => {
  try {
    return await fetcher("/api/config");
  } catch {
    return undefined;
  }
};

const loadConfig = async (view: SetupView): Promise<void> => {
  const response = await fetchConfig();
  if (response?.ok !== true) {
    return;
  }
  try {
    fillFields(view, parseSetupConfig(await readJSON(response)));
  } catch (error) {
    createToast(`Failed to load config: ${describeError(error)}`, "error");
  }
};

const startSetupWizard = async (deps: WizardDeps): Promise<void> => {
  const wizard: Wizard = { view: setupView(), state: emptySetupState(), deps };
  await loadConfig(wizard.view);
  wizard.view.panel("loading-state").classList.add("hidden");
  goToStep(wizard, "auth");
  bindWizard(wizard);
};

export { startSetupWizard };
