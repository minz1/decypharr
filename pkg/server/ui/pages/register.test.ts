import { describe, expect, it, vi } from "vitest";

import { stubFormAPI } from "#ui/lib/testing/api";
import { loadFixture } from "#ui/lib/testing/fixture";
import { toasts, waitForAnyToast } from "#ui/lib/testing/toast";

interface Booted {
  requests: ReturnType<typeof stubFormAPI>;
  navigations: string[];
}

const bootRegister = async (
  fixture: string,
  respond: () => Promise<Response>,
  prepare?: () => void,
): Promise<Booted> => {
  vi.resetModules();
  loadFixture(fixture);
  prepare?.();
  const requests = stubFormAPI(respond);
  const navigations: string[] = [];
  vi.spyOn(globalThis.location, "assign").mockImplementation((url: string | URL) => {
    navigations.push(String(url));
  });
  await import("#ui/pages/register");
  return { requests, navigations };
};

const reply =
  (body: string, status: number): (() => Promise<Response>) =>
  async () =>
    Promise.resolve(new Response(body, { status }));

const input = (id: string): HTMLInputElement => {
  const found = document.querySelector(`#${id}`);
  if (!(found instanceof HTMLInputElement)) {
    throw new TypeError(`the register fixture has no #${id}`);
  }
  return found;
};

const fill = (username: string, password: string, confirmPassword: string): void => {
  input("username").value = username;
  input("password").value = password;
  input("confirmPassword").value = confirmPassword;
};

const submit = (): void => {
  document.querySelector("#authForm")?.dispatchEvent(new Event("submit", { cancelable: true }));
};

const skip = (): void => {
  document.querySelector<HTMLButtonElement>("#skipAuthBtn")?.click();
};

describe("registering", () => {
  it("posts the form once to /register and goes home", async () => {
    const booted = await bootRegister("register", reply("", 200));
    fill("admin", "secret", "secret");
    submit();
    await vi.waitFor(() => {
      expect(booted.navigations).toStrictEqual(["/"]);
    });
    expect(booted.requests).toStrictEqual([
      {
        url: "/register",
        method: "POST",
        fields: [
          ["username", "admin"],
          ["password", "secret"],
          ["confirmPassword", "secret"],
        ],
      },
    ]);
  });

  it("posts and goes home under the URL base", async () => {
    const booted = await bootRegister("register@base", reply("", 200));
    fill("admin", "secret", "secret");
    submit();
    await vi.waitFor(() => {
      expect(booted.navigations).toStrictEqual(["/decypharr/"]);
    });
    expect(booted.requests.map((request) => request.url)).toStrictEqual(["/decypharr/register"]);
  });
});

describe("a registration in flight", () => {
  it("disables Save so a second submit posts nothing", async () => {
    const pending = Promise.withResolvers<Response>();
    const booted = await bootRegister("register", async () => pending.promise);
    fill("admin", "secret", "secret");
    submit();
    submit();
    const save = document.querySelector<HTMLButtonElement>("#saveAuthBtn");
    expect({ requests: booted.requests.length, disabled: save?.disabled }).toStrictEqual({
      requests: 1,
      disabled: true,
    });
    pending.resolve(new Response("taken", { status: 400 }));
    await waitForAnyToast("error");
    expect({ disabled: save?.disabled, label: save?.textContent }).toStrictEqual({ disabled: false, label: "Save" });
  });
});

describe("a registration that does not go through", () => {
  it("blocks a mismatched confirmation without posting", async () => {
    const booted = await bootRegister("register", reply("", 200));
    fill("admin", "secret", "other");
    submit();
    await waitForAnyToast("error");
    expect({ toasts: toasts("error"), requests: booted.requests }).toStrictEqual({
      toasts: ["Passwords do not match!"],
      requests: [],
    });
  });

  it("shows the server's refusal as text and stays", async () => {
    const booted = await bootRegister("register", reply("<img src=x onerror=alert(1)>", 400));
    fill("admin", "secret", "secret");
    submit();
    await waitForAnyToast("error");
    expect({
      toasts: toasts("error"),
      images: document.querySelectorAll(".toast-container img").length,
      navigations: booted.navigations,
    }).toStrictEqual({ toasts: ["<img src=x onerror=alert(1)>"], images: 0, navigations: [] });
  });

  it("falls back to Registration failed when the refusal has no text", async () => {
    await bootRegister("register", reply("", 400));
    fill("admin", "secret", "secret");
    submit();
    await waitForAnyToast("error");
    expect(toasts("error")).toStrictEqual(["Registration failed"]);
  });

  it("reports a request that never reached the server", async () => {
    await bootRegister("register", async () => Promise.reject(new Error("offline")));
    fill("admin", "secret", "secret");
    submit();
    await waitForAnyToast("error");
    expect(toasts("error")).toStrictEqual(["Registration failed: offline"]);
  });
});

describe("skipping authentication", () => {
  it("posts to /skip-auth under the URL base and goes home", async () => {
    const booted = await bootRegister("register@base", reply("", 200));
    skip();
    await vi.waitFor(() => {
      expect(booted.navigations).toStrictEqual(["/decypharr/"]);
    });
    expect(booted.requests).toStrictEqual([{ url: "/decypharr/skip-auth", method: "POST", fields: [] }]);
  });

  it("reports a refused skip and stays", async () => {
    const booted = await bootRegister("register", reply("nope", 500));
    skip();
    await waitForAnyToast("error");
    expect({ toasts: toasts("error"), navigations: booted.navigations }).toStrictEqual({
      toasts: ["Error: Failed to skip authentication"],
      navigations: [],
    });
  });

  it("reports a skip that never reached the server", async () => {
    await bootRegister("register", async () => Promise.reject(new Error("offline")));
    skip();
    await waitForAnyToast("error");
    expect(toasts("error")).toStrictEqual(["Error: offline"]);
  });
});

describe("a template out of step with the page", () => {
  it("names the missing element in an error toast", async () => {
    await bootRegister("register", reply("", 200), () => {
      document.querySelector("#skipAuthBtn")?.remove();
    });
    await waitForAnyToast("error");
    expect(toasts("error")).toStrictEqual(["register: missing #skipAuthBtn"]);
  });
});
