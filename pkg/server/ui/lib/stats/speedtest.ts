import { cloneTemplate } from "#ui/lib/common/dom";
import { fetcher } from "#ui/lib/common/http";
import { asRecord, readJSON } from "#ui/lib/common/json";
import { cloneElement, field } from "#ui/lib/stats/dom";
import { errorMessage } from "#ui/lib/stats/format";
import { count, text } from "#ui/lib/stats/read";

type Protocol = "nntp" | "debrid";

interface SpeedTestResult {
  error: string;
  latencyMs: number;
  speedMbps: number;
  testedAt: string;
}

interface DebridSpeedParts {
  latency: string;
  speed: string;
  tested: string;
}

const BITS_PER_BYTE = 8;

const parseSpeedTest = (value: unknown): SpeedTestResult => {
  const record = asRecord(value, "speed test");
  return {
    error: text(record, "error"),
    latencyMs: count(record, "latency_ms"),
    speedMbps: count(record, "speed_mbps"),
    testedAt: text(record, "tested_at"),
  };
};

const requestSpeedTest = async (protocol: Protocol, provider: string): Promise<SpeedTestResult> => {
  const response = await fetcher("/debug/speedtest", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ protocol, provider }),
  });
  if (!response.ok) {
    throw new Error(`HTTP ${response.status}`);
  }
  return parseSpeedTest(await readJSON(response));
};

const testedAtText = (testedAt: string): string => (testedAt === "" ? "" : new Date(testedAt).toLocaleTimeString());

const debridSpeedParts = (result: SpeedTestResult): DebridSpeedParts => ({
  latency: result.latencyMs ? `${result.latencyMs}ms` : "",
  speed: result.speedMbps ? `${(result.speedMbps * BITS_PER_BYTE).toFixed(1)} Mbps` : "",
  tested: `Tested: ${testedAtText(result.testedAt)}`,
});

const withText = (element: HTMLElement, value: string): HTMLElement => {
  element.append(` ${value}`);
  return element;
};

const speedError = (message: string): HTMLElement => withText(cloneElement("stats-speed-error"), message);

const optionalPart = (root: ParentNode, name: string, value: string): void => {
  const part = field(root, `${name}-part`);
  if (value === "") {
    part.remove();
  } else {
    field(part, name).textContent = value;
  }
};

const debridSpeed = (result: SpeedTestResult): DocumentFragment => {
  const parts = debridSpeedParts(result);
  const fragment = cloneTemplate("stats-debrid-speed");
  optionalPart(fragment, "latency", parts.latency);
  optionalPart(fragment, "speed", parts.speed);
  field(fragment, "tested").textContent = parts.tested;
  return fragment;
};

const nntpSpeed = (result: SpeedTestResult): DocumentFragment => {
  const fragment = cloneTemplate("stats-nntp-speed");
  field(fragment, "latency").textContent = `${result.latencyMs}ms`;
  field(fragment, "tested").textContent = `Tested: ${testedAtText(result.testedAt)}`;
  return fragment;
};

const spinner = (): HTMLSpanElement => {
  const span = document.createElement("span");
  span.classList.add("loading", "loading-spinner", "loading-xs");
  return span;
};

const testingNote = (): HTMLSpanElement => {
  const span = document.createElement("span");
  span.classList.add("text-base-content/50");
  span.textContent = "Testing...";
  return span;
};

const resultSlot = (button: HTMLElement): HTMLElement | undefined => {
  const slot = button.closest("[data-speed-scope]")?.querySelector('[data-field="speed-result"]');
  return slot instanceof HTMLElement ? slot : undefined;
};

const showTesting = (button: HTMLButtonElement, slot: HTMLElement, protocol: Protocol): void => {
  button.disabled = true;
  if (protocol === "debrid") {
    button.replaceChildren(spinner());
    slot.replaceChildren(testingNote());
  } else {
    button.replaceChildren(spinner(), " Testing...");
    slot.replaceChildren();
  }
};

const runSpeedTest = async (
  button: HTMLButtonElement,
  protocol: Protocol,
  show: (result: SpeedTestResult) => Node,
): Promise<void> => {
  const slot = resultSlot(button);
  if (!slot || button.disabled) {
    return;
  }
  showTesting(button, slot, protocol);
  try {
    const result = await requestSpeedTest(protocol, button.dataset["provider"] ?? "");
    slot.replaceChildren(result.error === "" ? show(result) : speedError(result.error));
  } catch (error) {
    slot.replaceChildren(speedError(errorMessage(error)));
  } finally {
    button.disabled = false;
    button.textContent = "Test Speed";
  }
};

const runDebridSpeedTest = async (button: HTMLButtonElement): Promise<void> =>
  runSpeedTest(button, "debrid", debridSpeed);

const runNNTPSpeedTest = async (button: HTMLButtonElement): Promise<void> => runSpeedTest(button, "nntp", nntpSpeed);

export type { SpeedTestResult };
export { debridSpeed, debridSpeedParts, parseSpeedTest, runDebridSpeedTest, runNNTPSpeedTest, testedAtText, withText };
