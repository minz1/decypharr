interface AuthStep {
  username?: string;
  password?: string;
  skip_auth?: boolean;
  token_only?: boolean;
}

interface DebridStep {
  provider?: string;
  api_key?: string;
  skip_debrid?: boolean;
}

interface UsenetStep {
  host?: string;
  port?: number;
  username?: string;
  password?: string;
  max_connections?: number;
  reader_connections?: number;
  ssl?: boolean;
  skip_usenet?: boolean;
}

interface DownloadStep {
  download_folder?: string;
}

interface MountStep {
  mount_type?: string;
  mount_path?: string;
  cache_dir?: string;
  rclone_buffer_size?: string;
}

interface SetupState {
  auth: AuthStep;
  debrid: DebridStep;
  usenet: UsenetStep;
  download: DownloadStep;
  mount: MountStep;
}

type StepName = "auth" | "debrid" | "usenet" | "download" | "mount" | "overview";

type StepOutcome<Step> = { step: Step } | { error: string };

export type { AuthStep, DebridStep, DownloadStep, MountStep, SetupState, StepName, StepOutcome, UsenetStep };
