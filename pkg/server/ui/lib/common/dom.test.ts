import { describe, expect, it, vi } from "vitest";

import { bootPage, byId, cloneTemplate, onAction } from "#ui/lib/common/dom";

const page = (): void => {
  document.body.innerHTML = `<div class="toast-container"></div>
    <form id="form"><input id="name"><button id="go" type="button">Go</button></form>
    <template id="row"><li class="row"><span data-field="name"></span></li></template>
    <ul id="list">
      <li id="open-one" data-action="open" data-id="1"><span id="inner">one</span></li>
      <li id="unknown" data-action="nope">two</li>
      <li id="plain">three</li>
    </ul>
    <div id="outside" data-action="open"><span id="outside-inner">x</span></div>`;
};

const errorToasts = (): (string | null)[] =>
  [...document.querySelectorAll(".toast-container .alert-error")].map((toast) => toast.textContent);

const recordActions = (): string[] => {
  page();
  const seen: string[] = [];
  onAction(byId("list", HTMLUListElement), "click", {
    open: (target, event) => {
      seen.push(`${target.dataset["id"] ?? ""}:${event.type}`);
    },
  });
  return seen;
};

const parsedValue = (text: string): unknown => JSON.parse(text);

const click = (id: string): void => {
  byId(id, HTMLElement).click();
};

describe("looking up an element", () => {
  it("returns the element with its type", () => {
    page();
    const input = byId("name", HTMLInputElement);
    input.value = "typed";
    expect(input.value).toBe("typed");
  });

  it("throws naming the id when it is missing", () => {
    page();
    expect(() => byId("absent", HTMLElement)).toThrow(new TypeError("missing #absent"));
  });

  it("throws naming the id when the element has another type", () => {
    page();
    expect(() => byId("go", HTMLInputElement)).toThrow(new TypeError("missing #go"));
  });

  it("looks in the given document", () => {
    page();
    const doc = document.implementation.createHTMLDocument();
    doc.body.innerHTML = '<p id="elsewhere"></p>';
    expect(byId("elsewhere", HTMLParagraphElement, doc).tagName).toBe("P");
    expect(() => byId("elsewhere", HTMLElement)).toThrow(new TypeError("missing #elsewhere"));
  });

  it("finds an id that needs escaping in a selector", () => {
    document.body.innerHTML = '<p id="a.b:c"></p>';
    expect(byId("a.b:c", HTMLParagraphElement).id).toBe("a.b:c");
  });
});

describe("cloning a template", () => {
  it("returns a fresh copy of the template's content each time", () => {
    page();
    const first = cloneTemplate("row");
    const second = cloneTemplate("row");
    expect(first.querySelector(".row")).not.toBeNull();
    expect(first.querySelector(".row")).not.toBe(second.querySelector(".row"));
    expect(document.querySelectorAll("#list .row")).toHaveLength(0);
  });

  it("throws naming the template when it is missing", () => {
    page();
    expect(() => cloneTemplate("absent")).toThrow(new TypeError("missing template #absent"));
  });

  it("throws when the id is not a template", () => {
    page();
    expect(() => cloneTemplate("list")).toThrow(new TypeError("missing template #list"));
  });
});

describe("delegating actions", () => {
  it("calls the handler of the closest data-action element", () => {
    const seen = recordActions();
    click("inner");
    click("open-one");
    expect(seen).toStrictEqual(["1:click", "1:click"]);
  });

  it("ignores unknown actions and elements without one", () => {
    const seen = recordActions();
    click("unknown");
    click("plain");
    click("list");
    expect(seen).toStrictEqual([]);
  });

  it("ignores a data-action element outside the container", () => {
    const seen = recordActions();
    document.body.dataset["action"] = "open";
    click("plain");
    click("outside-inner");
    expect(seen).toStrictEqual([]);
    delete document.body.dataset["action"];
  });
});

describe("booting a page", () => {
  it("runs start at once", () => {
    page();
    const started: string[] = [];
    bootPage("page", () => {
      started.push("page");
    });
    expect(started).toStrictEqual(["page"]);
    expect(errorToasts()).toStrictEqual([]);
  });

  it("shows one error toast naming the page and the selector when start throws", async () => {
    page();
    bootPage("browse", () => {
      byId("absent", HTMLElement);
    });
    await vi.waitFor(() => {
      expect(errorToasts()).toStrictEqual(["browse: missing #absent"]);
    });
  });

  it("shows one error toast when start rejects", async () => {
    page();
    bootPage("stats", async () => Promise.reject(new Error("offline")));
    await vi.waitFor(() => {
      expect(errorToasts()).toStrictEqual(["stats: offline"]);
    });
  });

  it("names a thrown value that is not an Error", async () => {
    page();
    bootPage("page", () => {
      throw parsedValue('"plain"');
    });
    await vi.waitFor(() => {
      expect(errorToasts()).toStrictEqual(["page: plain"]);
    });
  });
});

describe("a page whose boot failed", () => {
  it("leaves the rest of the page working", async () => {
    page();
    bootPage("broken", () => {
      byId("absent", HTMLElement);
    });
    const clicks: string[] = [];
    bootPage("working", () => {
      byId("go", HTMLButtonElement).addEventListener("click", () => {
        clicks.push("go");
      });
    });
    click("go");
    expect(clicks).toStrictEqual(["go"]);
    await vi.waitFor(() => {
      expect(errorToasts()).toStrictEqual(["broken: missing #absent"]);
    });
  });
});
