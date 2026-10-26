import { byId } from "#ui/lib/common/dom";
import { asArray, asRecord } from "#ui/lib/common/json";
import { appendCard, cardIndex, nextIndex } from "#ui/lib/config/cards";
import { assign, checkedOf, intOr, named, orDefault, requireNamed } from "#ui/lib/config/form";

const REQUIRED_FIELDS = [
  "host",
  "port",
  "username",
  "password",
  "backbone",
  "ssl",
  "max_connections",
  "priority",
] as const;

const STREAM_FIELDS = [
  "max_connections",
  "processing_max_connections",
  "read_ahead",
  "body_pipeline_depth",
  "processing_timeout",
  "conn_idle_timeout",
  "availability_sample_percent",
  "import_availability_sample_percent",
  "disk_path",
  "buffer_memory",
  "socket_read_buffer",
  "socket_write_buffer",
  "stream_backup_wait",
] as const;

const DEFAULT_PORT = 119;
const DEFAULT_PROVIDER_CONNECTIONS = 100;
const DEFAULT_CONNECTIONS = 15;
const DEFAULT_PIPELINE_DEPTH = 2;
const MAX_PIPELINE_DEPTH = 4;
const DEFAULT_SAMPLE_PERCENT = 10;

const providerContainer = (doc: Document): HTMLElement => byId("usenetProviders", HTMLElement, doc);

const addUsenetProvider = (doc: Document, data: Record<string, unknown> = {}): void => {
  const container = providerContainer(doc);
  const index = nextIndex(container);
  const card = appendCard(doc, container, {
    index,
    template: "config-usenet-provider",
    title: `Provider #${index + 1}`,
    names: {
      name: (field) => `usenet.providers[${index}].${field}`,
      id: (field) => `usenet_provider_${index}_${field}`,
    },
  });
  for (const [key, value] of Object.entries(data)) {
    assign(named(card, `usenet.providers[${index}].${key}`), value);
  }
};

const populateUsenet = (doc: Document, value: unknown): void => {
  if (value === undefined || value === null) {
    return;
  }
  const usenet = asRecord(value, "usenet");
  if (Array.isArray(usenet["providers"])) {
    for (const provider of asArray(usenet["providers"], "usenet.providers")) {
      addUsenetProvider(doc, asRecord(provider, "usenet provider"));
    }
  }
  const stream: Record<string, unknown> = {
    ...usenet,
    body_pipeline_depth: usenet["body_pipeline_depth"] ?? DEFAULT_PIPELINE_DEPTH,
  };
  for (const key of STREAM_FIELDS) {
    assign(named(doc, `usenet.${key}`), stream[key]);
  }
};

const providerFrom = (card: Element): Record<string, unknown> | undefined => {
  const index = cardIndex(card);
  const field = (key: string): ReturnType<typeof named> => named(card, `usenet.providers[${index}].${key}`);
  if (REQUIRED_FIELDS.some((key) => field(key) === undefined)) {
    return undefined;
  }
  return {
    host: field("host")?.value,
    port: intOr(field("port")?.value, DEFAULT_PORT),
    username: field("username")?.value,
    password: field("password")?.value,
    backbone: field("backbone")?.value.trim(),
    ssl: checkedOf(field("ssl")),
    max_connections: intOr(field("max_connections")?.value, DEFAULT_PROVIDER_CONNECTIONS),
    priority: intOr(field("priority")?.value, 0),
    backup: checkedOf(field("backup")) ?? false,
    tls_server_name: field("tls_server_name")?.value ?? "",
  };
};

const collectProviders = (doc: Document): Record<string, unknown>[] =>
  [...providerContainer(doc).querySelectorAll(".usenet-provider")]
    .map((card) => providerFrom(card))
    .filter(
      (provider): provider is Record<string, unknown> =>
        provider !== undefined && provider["host"] !== "" && provider["username"] !== "" && provider["password"] !== "",
    );

const pipelineDepth = (doc: Document): number => {
  const depth = intOr(named(doc, "usenet.body_pipeline_depth")?.value, DEFAULT_PIPELINE_DEPTH);
  return Math.min(MAX_PIPELINE_DEPTH, Math.max(1, depth));
};

const collectUsenet = (doc: Document): Record<string, unknown> => {
  const text = (key: string): string | undefined => named(doc, `usenet.${key}`)?.value;
  const connections = text("max_connections");
  return {
    providers: collectProviders(doc),
    max_connections: intOr(connections, DEFAULT_CONNECTIONS),
    processing_max_connections: intOr(text("processing_max_connections"), intOr(connections, DEFAULT_CONNECTIONS)),
    read_ahead: orDefault(requireNamed(doc, "usenet.read_ahead").value, "16MB"),
    body_pipeline_depth: pipelineDepth(doc),
    processing_timeout: orDefault(text("processing_timeout"), "5m"),
    conn_idle_timeout: orDefault(text("conn_idle_timeout"), ""),
    availability_sample_percent: intOr(text("availability_sample_percent"), DEFAULT_SAMPLE_PERCENT),
    import_availability_sample_percent: intOr(text("import_availability_sample_percent"), 1),
    disk_path: text("disk_path")?.trim() ?? "",
    buffer_memory: orDefault(text("buffer_memory"), ""),
    socket_read_buffer: text("socket_read_buffer") ?? "",
    socket_write_buffer: text("socket_write_buffer") ?? "",
    stream_backup_wait: text("stream_backup_wait") ?? "",
  };
};

export { addUsenetProvider, collectUsenet, populateUsenet };
