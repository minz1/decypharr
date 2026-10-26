import { fetcher } from "#ui/lib/common/http";
import { readJSON } from "#ui/lib/common/json";
import type { PageLocation, StatsElements } from "#ui/lib/stats/elements";
import { errorMessage } from "#ui/lib/stats/format";
import { parseSnapshot, renderSnapshot } from "#ui/lib/stats/snapshot";

type View = "loading" | "content" | "error";

const show = (elements: StatsElements, view: View): void => {
  elements.loading.style.display = view === "loading" ? "block" : "none";
  elements.content.style.display = view === "content" ? "block" : "none";
  elements.error.style.display = view === "error" ? "flex" : "none";
};

const fetchSnapshot = async (): Promise<ReturnType<typeof parseSnapshot>> => {
  const response = await fetcher("/debug/stats");
  if (!response.ok) {
    throw new Error(`HTTP ${response.status}: ${response.statusText}`);
  }
  return parseSnapshot(await readJSON(response));
};

const loadStats = async (elements: StatsElements, where: PageLocation): Promise<void> => {
  show(elements, "loading");
  try {
    renderSnapshot(elements, await fetchSnapshot(), where);
    show(elements, "content");
  } catch (error) {
    const message = errorMessage(error);
    show(elements, "error");
    elements.errorMessage.textContent = message === "" ? "Failed to load statistics" : message;
  }
};

export { loadStats };
