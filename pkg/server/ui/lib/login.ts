import type { setButtonLoading } from "#ui/lib/common/button";
import type { fetcher } from "#ui/lib/common/http";
import type { createToast } from "#ui/lib/common/toast";

interface LoginUtils {
  fetcher: typeof fetcher;
  createToast: typeof createToast;
  setButtonLoading: typeof setButtonLoading;
}

interface LoginDeps {
  utils: LoginUtils;
  redirect: (url: string) => void;
  urlBase: string;
}

const REDIRECT_DELAY_MS = 1000;

const fieldValue = (form: HTMLFormElement, selector: string): string =>
  form.querySelector<HTMLInputElement>(selector)?.value ?? "";

const report = async (response: Response, deps: LoginDeps): Promise<void> => {
  if (response.ok) {
    deps.utils.createToast("Login successful! Redirecting...", "success");
    setTimeout(() => {
      deps.redirect(deps.urlBase === "" ? "/" : deps.urlBase);
    }, REDIRECT_DELAY_MS);
    return;
  }
  const text = await response.text();
  deps.utils.createToast(text === "" ? "Invalid credentials" : text, "error");
};

const submit = async (form: HTMLFormElement, deps: LoginDeps): Promise<void> => {
  const body = JSON.stringify({ username: fieldValue(form, "#username"), password: fieldValue(form, "#password") });
  try {
    const response = await deps.utils.fetcher("/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body,
    });
    await report(response, deps);
  } catch (error) {
    deps.utils.createToast(error instanceof Error ? error.message : "Login failed. Please try again.", "error");
  } finally {
    deps.utils.setButtonLoading(form.querySelector<HTMLButtonElement>('button[type="submit"]'), false);
  }
};

const bindLoginForm = (form: HTMLFormElement, deps: LoginDeps): void => {
  form.addEventListener("submit", (event) => {
    event.preventDefault();
    void submit(form, deps);
  });
};

export { bindLoginForm };
