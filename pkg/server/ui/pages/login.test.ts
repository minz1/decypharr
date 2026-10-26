import { describe, expect, it, vi } from "vitest";

import { loadFixture } from "#ui/lib/testing/fixture";

interface Request {
  url: string;
  method: string;
  body: string;
}

const bootLogin = async (fixture: string, response: Response): Promise<Request[]> => {
  vi.resetModules();
  loadFixture(fixture);
  const requests: Request[] = [];
  vi.stubGlobal("fetch", async (url: string, init: RequestInit): Promise<Response> => {
    requests.push({ url, method: init.method ?? "GET", body: typeof init.body === "string" ? init.body : "" });
    return Promise.resolve(response);
  });
  await import("#ui/pages/login");
  return requests;
};

const submit = async (username: string, password: string): Promise<void> => {
  const user = document.querySelector<HTMLInputElement>("#username");
  const pass = document.querySelector<HTMLInputElement>("#password");
  const form = document.querySelector<HTMLFormElement>("#loginForm");
  if (!user || !pass || !form) {
    throw new Error("the login fixture has no form");
  }
  user.value = username;
  pass.value = password;
  form.dispatchEvent(new Event("submit", { cancelable: true }));
  await vi.waitFor(() => {
    expect(document.querySelectorAll(".toast-container .alert").length).toBeGreaterThan(0);
  });
};

const toastText = (): string[] =>
  [...document.querySelectorAll(".toast-container .alert")].map((toast) => toast.textContent.trim());

describe("the login page", () => {
  it("posts the credentials once to /login", async () => {
    const requests = await bootLogin("login", new Response("", { status: 200 }));
    await submit("admin", "<img src=x onerror=alert(1)>");
    expect(requests).toStrictEqual([
      { url: "/login", method: "POST", body: '{"username":"admin","password":"<img src=x onerror=alert(1)>"}' },
    ]);
  });

  it("posts under the URL base", async () => {
    const requests = await bootLogin("login@base", new Response("", { status: 200 }));
    await submit("admin", "secret");
    expect(requests.map((request) => request.url)).toStrictEqual(["/decypharr/login"]);
  });

  it("shows the server's refusal as text", async () => {
    await bootLogin("login", new Response("<b>Invalid credentials</b>", { status: 401 }));
    await submit("admin", "wrong");
    expect({ toasts: toastText(), bold: document.querySelectorAll(".toast-container b").length }).toStrictEqual({
      toasts: ["<b>Invalid credentials</b>"],
      bold: 0,
    });
  });
});
