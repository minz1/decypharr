import type {
  AuthStep,
  DebridStep,
  DownloadStep,
  MountStep,
  SetupState,
  StepName,
  StepOutcome,
  UsenetStep,
} from "#ui/lib/setup/types";

interface AuthInput {
  username: string;
  password: string;
  confirmPassword: string;
}

interface UsenetInput {
  host: string;
  port: string;
  username: string;
  password: string;
  maxConnections: string;
  readerConnections: string;
  ssl: boolean;
}

interface MountInput {
  mountType: string;
  mountPath: string;
  cacheDir: string;
  rcloneBufferSize: string;
}

const STEP_ORDER: readonly StepName[] = ["auth", "debrid", "usenet", "download", "mount", "overview"];

const MIN_PASSWORD_LENGTH = 6;
const MAX_PORT = 65_535;
const MAX_CONNECTIONS = 100;
const MAX_READER_CONNECTIONS = 50;
const DEFAULT_MAX_CONNECTIONS = 30;
const DEFAULT_READER_CONNECTIONS = 15;
const DECIMAL = 10;

const emptySetupState = (): SetupState => ({ auth: {}, debrid: {}, usenet: {}, download: {}, mount: {} });

const firstFailure = (checks: readonly (readonly [failed: boolean, error: string])[]): string | undefined =>
  checks.find(([failed]) => failed)?.[1];

const readAuthStep = (input: AuthInput): StepOutcome<AuthStep> => {
  const username = input.username.trim();
  if (username === "" && input.password === "" && input.confirmPassword === "") {
    return { step: { skip_auth: true } };
  }
  const error = firstFailure([
    [username === "", "Username is required"],
    [input.password === "", "Password is required"],
    [input.password !== input.confirmPassword, "Passwords do not match"],
    [input.password.length < MIN_PASSWORD_LENGTH, "Password must be at least 6 characters"],
  ]);
  return error === undefined ? { step: { username, password: input.password, skip_auth: false } } : { error };
};

const readDebridStep = (input: { provider: string; apiKey: string }): StepOutcome<DebridStep> => {
  const apiKey = input.apiKey.trim();
  if (input.provider === "") {
    return { error: "Please select a debrid provider" };
  }
  if (apiKey === "") {
    return { error: "API key is required" };
  }
  return { step: { provider: input.provider, api_key: apiKey } };
};

const inRange = (value: number, max: number): boolean => !Number.isNaN(value) && value >= 1 && value <= max;

const countOr = (value: string, fallback: number): number =>
  value === "" ? fallback : Number.parseInt(value, DECIMAL);

const outOfRange = (value: string, max: number): boolean => !inRange(Number.parseInt(value, DECIMAL), max);

const usenetError = (fields: UsenetInput): string | undefined =>
  firstFailure([
    [fields.host === "", "Usenet server host is required"],
    [fields.port === "", "Usenet server port is required"],
    [fields.username === "", "Usenet username is required"],
    [fields.password === "", "Usenet password is required"],
    [outOfRange(fields.port, MAX_PORT), "Please enter a valid port number (1-65535)"],
    [
      fields.maxConnections !== "" && outOfRange(fields.maxConnections, MAX_CONNECTIONS),
      "Max connections must be between 1 and 100",
    ],
    [
      fields.readerConnections !== "" && outOfRange(fields.readerConnections, MAX_READER_CONNECTIONS),
      "Connections per stream must be between 1 and 50",
    ],
  ]);

const trimUsenet = (input: UsenetInput): UsenetInput => ({
  host: input.host.trim(),
  port: input.port.trim(),
  username: input.username.trim(),
  password: input.password.trim(),
  maxConnections: input.maxConnections.trim(),
  readerConnections: input.readerConnections.trim(),
  ssl: input.ssl,
});

const readUsenetStep = (input: UsenetInput): StepOutcome<UsenetStep> => {
  const fields = trimUsenet(input);
  if (fields.host === "" && fields.port === "" && fields.username === "" && fields.password === "" && !fields.ssl) {
    return { step: { skip_usenet: true } };
  }
  const error = usenetError(fields);
  if (error !== undefined) {
    return { error };
  }
  return {
    step: {
      host: fields.host,
      port: Number.parseInt(fields.port, DECIMAL),
      username: fields.username,
      password: fields.password,
      max_connections: countOr(fields.maxConnections, DEFAULT_MAX_CONNECTIONS),
      reader_connections: countOr(fields.readerConnections, DEFAULT_READER_CONNECTIONS),
      ssl: fields.ssl,
      skip_usenet: false,
    },
  };
};

const readDownloadStep = (folder: string): StepOutcome<DownloadStep> => {
  const downloadFolder = folder.trim();
  return downloadFolder === ""
    ? { error: "Download folder is required" }
    : { step: { download_folder: downloadFolder } };
};

const readMountStep = (input: MountInput): StepOutcome<MountStep> => {
  const mountPath = input.mountPath.trim();
  const bufferSize = input.rcloneBufferSize.trim();
  if (mountPath === "" && input.mountType !== "none") {
    return { error: "Mount path is required" };
  }
  const step: MountStep = { mount_type: input.mountType, mount_path: mountPath, cache_dir: input.cacheDir.trim() };
  if (input.mountType === "rclone" && bufferSize !== "") {
    step.rclone_buffer_size = bufferSize;
  }
  return { step };
};

const filled = (value: string | undefined): boolean => value !== undefined && value !== "";

const providerConfigured = (state: SetupState): boolean => {
  const { debrid, usenet } = state;
  const hasDebrid = debrid.skip_debrid !== true && filled(debrid.provider) && filled(debrid.api_key);
  const hasUsenet =
    usenet.skip_usenet !== true &&
    filled(usenet.host) &&
    usenet.port !== undefined &&
    usenet.port !== 0 &&
    filled(usenet.username) &&
    filled(usenet.password);
  return hasDebrid || hasUsenet;
};

const setupPayload = (state: SetupState): Readonly<SetupState> => ({
  auth: state.auth,
  debrid: state.debrid,
  usenet: state.usenet,
  download: state.download,
  mount: state.mount,
});

export {
  STEP_ORDER,
  emptySetupState,
  providerConfigured,
  readAuthStep,
  readDebridStep,
  readDownloadStep,
  readMountStep,
  readUsenetStep,
  setupPayload,
};
