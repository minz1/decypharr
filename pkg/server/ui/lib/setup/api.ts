import { fetcher } from "#ui/lib/common/http";
import { asRecord, optStr, readJSON } from "#ui/lib/common/json";
import { parseSetupConfig } from "#ui/lib/setup/config";
import { setupPayload } from "#ui/lib/setup/steps";
import type { SetupState } from "#ui/lib/setup/types";

const FINISH_FAILED = "Failed to complete setup";

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

const completeSetup = async (state: SetupState): Promise<string> => {
  const response = await fetcher("/api/setup/complete", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(setupPayload(state)),
  });
  if (!response.ok) {
    throw new Error(await refusalMessage(response));
  }
  const result = asRecord(await readJSON(response), "response");
  return optStr(result, "api_token") ?? "";
};

const fetchConfig = async (): Promise<Response | undefined> => {
  try {
    return await fetcher("/api/config");
  } catch {
    return undefined;
  }
};

const readSetupConfig = async (response: Response): Promise<ReturnType<typeof parseSetupConfig>> =>
  parseSetupConfig(await readJSON(response));

export { FINISH_FAILED, completeSetup, fetchConfig, readSetupConfig };
