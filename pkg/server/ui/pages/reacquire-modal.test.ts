import { describe, expect, it, onTestFinished, vi } from "vitest";

import { click, element, mutations, settle, toasts } from "#ui/lib/testing/api";
import { XSS, binding, bootReacquire } from "#ui/lib/testing/reacquire";

type Routes = Parameters<typeof bootReacquire>[0];

const BINDINGS = [binding(XSS), binding("movie", { movieId: 5, arrFileId: 0 })];

const openModal = async (routes: Routes = {}): Promise<Awaited<ReturnType<typeof bootReacquire>>> => {
  const requests = await bootReacquire({ "GET /api/arr/bindings?": () => Response.json(BINDINGS), ...routes });
  await click("#reacquireBtn");
  return requests;
};

const resultRows = (): string[] =>
  [...document.querySelectorAll("#bindingResults tr")].map((row) => row.textContent.replaceAll(/\s+/gu, " ").trim());

const choose = async (): Promise<void> => {
  await click("#bindingResults tr td:nth-child(2)");
  element("#reacquireStrategy", HTMLSelectElement).value = "command_search";
  element("#reacquireSubmit", HTMLButtonElement).click();
  await click("#reacquireSubmit");
  await settle();
};

const typeQuery = (value: string): void => {
  const query = element("#reacquireQuery", HTMLInputElement);
  query.value = value;
  query.dispatchEvent(new Event("input"));
  query.dispatchEvent(new Event("input"));
};

describe("the binding index", () => {
  it("shows each Arr's summary and fills the Arr filter", async () => {
    await bootReacquire();
    expect({
      cards: [...document.querySelectorAll("#indexSummary > div")].map((card) =>
        card.textContent.replaceAll(/\s+/gu, " ").trim(),
      ),
      arrs: [...element("#reacquireArr", HTMLSelectElement).options].map((option) => option.value),
    }).toStrictEqual({
      cards: ["Sonarr 4K sonarr 12 files bound 9 can be replaced · updated -"],
      arrs: ["", "Sonarr 4K"],
    });
  });

  it("says when nothing is indexed", async () => {
    await bootReacquire({ "GET /api/arr/index": () => Response.json([]) });
    expect(element("#indexSummary", HTMLElement).textContent.replaceAll(/\s+/gu, " ").trim()).toBe(
      "Nothing is indexed yet. Only Sonarr and Radarr instances are indexed; rebuild once they hold imported files.",
    );
  });

  it("queues a rebuild once and rereads the index after 3 s", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    onTestFinished(() => {
      vi.useRealTimers();
    });
    const requests = await bootReacquire({ "POST /api/arr/index/refresh": () => Response.json({}) });
    element("#refreshIndexBtn", HTMLButtonElement).click();
    await click("#refreshIndexBtn");
    await vi.advanceTimersByTimeAsync(3000);
    expect({ posted: mutations(requests), toasts: toasts("success"), last: requests.at(-1)?.url }).toStrictEqual({
      posted: [{ url: "/api/arr/index/refresh", method: "POST", body: "" }],
      toasts: ["Index rebuild queued"],
      last: "/api/arr/index",
    });
  });

  it("reports a failed rebuild", async () => {
    await bootReacquire({ "POST /api/arr/index/refresh": () => new Response("", { status: 503 }) });
    await click("#refreshIndexBtn");
    expect(toasts("error")).toStrictEqual(["Could not queue the index rebuild"]);
  });
});

describe("a new search", () => {
  it("forgets the chosen file when a new search replaces the list", async () => {
    const requests = await openModal({ "GET /api/arr/bindings?arr=Sonarr+4K": () => Response.json(BINDINGS) });
    await click("#bindingResults tr td:nth-child(2)");
    const arr = element("#reacquireArr", HTMLSelectElement);
    arr.value = "Sonarr 4K";
    arr.dispatchEvent(new Event("change"));
    await vi.waitFor(() => {
      expect(requests.at(-1)?.url).toBe("/api/arr/bindings?arr=Sonarr+4K");
    });
    await settle();
    element("#reacquireSubmit", HTMLButtonElement).click();
    await settle();
    expect({
      disabled: element("#reacquireSubmit", HTMLButtonElement).disabled,
      posted: mutations(requests),
    }).toStrictEqual({ disabled: true, posted: [] });
  });
});

describe("searching the binding index", () => {
  it("lists the bindings as text when the modal opens", async () => {
    const requests = await openModal();
    expect({
      open: element("#reacquireModal", HTMLDialogElement).open,
      url: requests.at(-1)?.url,
      rows: resultRows(),
      disabled: [...document.querySelectorAll<HTMLInputElement>("#bindingResults input")].map(
        (radio) => radio.disabled,
      ),
      images: document.querySelectorAll("#bindingResults img").length,
      submit: element("#reacquireSubmit", HTMLButtonElement).disabled,
    }).toStrictEqual({
      open: true,
      url: "/api/arr/bindings?",
      rows: [`${XSS}.mkv Show ${XSS} sonarr series 3 · season 1`, "movie.mkv Show movie sonarr movie 5"],
      disabled: [false, true],
      images: 0,
      submit: true,
    });
  });

  it("searches by name once typing stops, then by Arr", async () => {
    const requests = await openModal({
      "GET /api/arr/bindings?q=Show+1": () => Response.json(BINDINGS),
      "GET /api/arr/bindings?q=Show+1&arr=Sonarr+4K": () => Response.json([]),
    });
    typeQuery("Show 1");
    await vi.waitFor(() => {
      expect(requests.at(-1)?.url).toBe("/api/arr/bindings?q=Show+1");
    });
    const arr = element("#reacquireArr", HTMLSelectElement);
    arr.value = "Sonarr 4K";
    arr.dispatchEvent(new Event("change"));
    await vi.waitFor(() => {
      expect(resultRows()).toStrictEqual(["No indexed file matches."]);
    });
    expect(requests.map((request) => request.url).filter((url) => url.includes("q="))).toStrictEqual([
      "/api/arr/bindings?q=Show+1",
      "/api/arr/bindings?q=Show+1&arr=Sonarr+4K",
    ]);
  });

  it("says when the search fails", async () => {
    await openModal({ "GET /api/arr/bindings?": () => new Response("down", { status: 503 }) });
    expect(resultRows()).toStrictEqual(["Search failed."]);
  });
});

describe("reacquiring a file", () => {
  it("posts the chosen file once and reloads the jobs", async () => {
    const requests = await openModal({ "POST /api/arr/reacquire": () => Response.json({ id: "j" }) });
    await choose();
    expect({
      posted: mutations(requests),
      open: element("#reacquireModal", HTMLDialogElement).open,
      toasts: toasts("success"),
      last: requests.at(-1)?.url,
    }).toStrictEqual({
      posted: [
        {
          url: "/api/arr/reacquire",
          method: "POST",
          body: `{"entryId":"entry-${XSS}","fileId":"file-${XSS}","cause":"manual","strategy":"command_search"}`,
        },
      ],
      open: false,
      toasts: ["Reacquisition queued"],
      last: "/api/arr/reacquire/jobs",
    });
  });

  it("shows the server's refusal in the modal", async () => {
    await openModal({ "POST /api/arr/reacquire": () => Response.json({ error: XSS }, { status: 409 }) });
    await choose();
    const error = element("#reacquireError", HTMLElement);
    expect({
      text: error.textContent,
      hidden: error.classList.contains("hidden"),
      images: error.querySelectorAll("img").length,
    }).toStrictEqual({ text: XSS, hidden: false, images: 0 });
  });

  it("closes with Cancel", async () => {
    await openModal();
    await click("#reacquireCancelBtn");
    expect({ open: element("#reacquireModal", HTMLDialogElement).open }).toStrictEqual({ open: false });
  });
});
