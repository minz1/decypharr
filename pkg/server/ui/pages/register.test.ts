import { describe, expect, it, vi } from "vitest";

import { loadFixture } from "#ui/lib/testing/fixture";

interface Request {
  url: string;
  method: string;
  fields: [string, FormDataEntryValue][];
}

interface Booted {
  requests: Request[];
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
  const requests: Request[] = [];
  const navigations: string[] = [];
  vi.stubGlobal("fetch", async (url: string, init: RequestInit): Promise<Response> => {
    const fields = init.body instanceof FormData ? [...init.body.entries()] : [];
    requests.push({ url, method: init.method ?? "GET", fields });
    return respond();
  });
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

const errorToasts = (): string[] =>
  [...document.querySelectorAll(".toast-container .alert-error [data-toast-message]")].map(
    (toast) => toast.textContent,
  );

const waitForToast = async (): Promise<void> => {
  await vi.waitFor(() => {
    expect(errorToasts().length).toBeGreaterThan(0);
  });
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

describe("a registration that does not go through", () => {
  it("blocks a mismatched confirmation without posting", async () => {
    const booted = await bootRegister("register", reply("", 200));
    fill("admin", "secret", "other");
    submit();
    await waitForToast();
    expect({ toasts: errorToasts(), requests: booted.requests }).toStrictEqual({
      toasts: ["Passwords do not match!"],
      requests: [],
    });
  });

  it("shows the server's refusal as text and stays", async () => {
    const booted = await bootRegister("register", reply("<img src=x onerror=alert(1)>", 400));
    fill("admin", "secret", "secret");
    submit();
    await waitForToast();
    expect({
      toasts: errorToasts(),
      images: document.querySelectorAll(".toast-container img").length,
      navigations: booted.navigations,
    }).toStrictEqual({ toasts: ["<img src=x onerror=alert(1)>"], images: 0, navigations: [] });
  });

  it("falls back to Registration failed when the refusal has no text", async () => {
    await bootRegister("register", reply("", 400));
    fill("admin", "secret", "secret");
    submit();
    await waitForToast();
    expect(errorToasts()).toStrictEqual(["Registration failed"]);
  });

  it("reports a request that never reached the server", async () => {
    await bootRegister("register", async () => Promise.reject(new Error("offline")));
    fill("admin", "secret", "secret");
    submit();
    await waitForToast();
    expect(errorToasts()).toStrictEqual(["Registration failed: offline"]);
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
    await waitForToast();
    expect({ toasts: errorToasts(), navigations: booted.navigations }).toStrictEqual({
      toasts: ["Error: Failed to skip authentication"],
      navigations: [],
    });
  });

  it("reports a skip that never reached the server", async () => {
    await bootRegister("register", async () => Promise.reject(new Error("offline")));
    skip();
    await waitForToast();
    expect(errorToasts()).toStrictEqual(["Error: offline"]);
  });
});

describe("a template out of step with the page", () => {
  it("names the missing element in an error toast", async () => {
    await bootRegister("register", reply("", 200), () => {
      document.querySelector("#skipAuthBtn")?.remove();
    });
    await waitForToast();
    expect(errorToasts()).toStrictEqual(["register: missing #skipAuthBtn"]);
  });
});
