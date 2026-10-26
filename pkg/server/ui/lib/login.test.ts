import { describe, expect, it, vi } from "vitest";

import type { DecypharrUtils } from "#ui/globals";
import { bindLoginForm } from "#ui/lib/login";

interface Recorder {
  utils: DecypharrUtils;
  requests: { url: string; init: RequestInit | undefined }[];
  toasts: { message: string; type: string }[];
  loading: boolean[];
  redirects: string[];
}

const record = (response: Response): Recorder => {
  const requests: Recorder["requests"] = [];
  const toasts: Recorder["toasts"] = [];
  const loading: boolean[] = [];
  return {
    utils: {
      fetcher: async (url, init) => {
        requests.push({ url, init });
        return Promise.resolve(response);
      },
      createToast: (message, type) => {
        toasts.push({ message, type });
      },
      setButtonLoading: (_button, isLoading) => {
        loading.push(isLoading);
      },
    },
    requests,
    toasts,
    loading,
    redirects: [],
  };
};

const loginForm = (withUsername: boolean): HTMLFormElement => {
  document.body.innerHTML = `<form id="loginForm">
    ${withUsername ? '<input id="username" value="u">' : ""}
    <input id="password" type="password" value="p">
    <button type="submit">Login</button>
  </form>`;
  const form = document.querySelector<HTMLFormElement>("#loginForm");
  if (form === null) {
    throw new Error("fixture has no form");
  }
  return form;
};

const submit = async (rec: Recorder, form: HTMLFormElement, urlBase: string): Promise<void> => {
  vi.useFakeTimers();
  bindLoginForm(form, {
    utils: rec.utils,
    redirect: (url) => {
      rec.redirects.push(url);
    },
    urlBase,
  });
  form.dispatchEvent(new Event("submit", { cancelable: true }));
  await vi.advanceTimersByTimeAsync(0);
};

const posted = (body: string): Recorder["requests"] => [
  { url: "/login", init: { method: "POST", headers: { "Content-Type": "application/json" }, body } },
];

describe("submitting the login form", () => {
  it("posts the username and password as JSON to /login", async () => {
    const rec = record(new Response("", { status: 200 }));
    await submit(rec, loginForm(true), "/");
    expect(rec.requests).toStrictEqual(posted('{"username":"u","password":"p"}'));
  });

  it("sends an empty username when the form has none", async () => {
    const rec = record(new Response("", { status: 200 }));
    await submit(rec, loginForm(false), "/");
    expect(rec.requests).toStrictEqual(posted('{"username":"","password":"p"}'));
  });
});

describe("a successful login", () => {
  it("toasts success and redirects to urlBase after a second", async () => {
    const rec = record(new Response("", { status: 200 }));
    await submit(rec, loginForm(true), "/x/");
    expect(rec.toasts).toStrictEqual([{ message: "Login successful! Redirecting...", type: "success" }]);
    await vi.advanceTimersByTimeAsync(999);
    expect(rec.redirects).toStrictEqual([]);
    await vi.advanceTimersByTimeAsync(1);
    expect(rec.redirects).toStrictEqual(["/x/"]);
    expect(rec.loading).toStrictEqual([false]);
  });

  it("redirects to / when urlBase is empty", async () => {
    const rec = record(new Response("", { status: 200 }));
    await submit(rec, loginForm(true), "");
    await vi.advanceTimersByTimeAsync(1000);
    expect(rec.redirects).toStrictEqual(["/"]);
  });
});

describe("a rejected login", () => {
  it("toasts the server's text and does not redirect", async () => {
    const rec = record(new Response("bad", { status: 401 }));
    await submit(rec, loginForm(true), "/");
    await vi.advanceTimersByTimeAsync(1000);
    expect(rec.toasts).toStrictEqual([{ message: "bad", type: "error" }]);
    expect(rec.redirects).toStrictEqual([]);
    expect(rec.loading).toStrictEqual([false]);
  });

  it("falls back to Invalid credentials when the server sends no text", async () => {
    const rec = record(new Response("", { status: 401 }));
    await submit(rec, loginForm(true), "/");
    expect(rec.toasts).toStrictEqual([{ message: "Invalid credentials", type: "error" }]);
  });

  it("toasts the error when the request fails, and resets the button", async () => {
    const rec = record(new Response("", { status: 200 }));
    rec.utils.fetcher = async (): Promise<Response> => Promise.reject(new Error("offline"));
    await submit(rec, loginForm(true), "/");
    expect(rec.toasts).toStrictEqual([{ message: "offline", type: "error" }]);
    expect(rec.loading).toStrictEqual([false]);
  });
});
