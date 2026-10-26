import { fetcher } from "#ui/lib/common/http";
import { createToast } from "#ui/lib/common/toast";
import { joinURL } from "#ui/lib/common/url";

interface Registration {
  username: string;
  password: string;
  confirmPassword: string;
}

interface RegisterElements {
  form: HTMLFormElement;
  username: HTMLInputElement;
  password: HTMLInputElement;
  confirmPassword: HTMLInputElement;
  skip: HTMLButtonElement;
}

interface RegisterDeps {
  navigate: (url: string) => void;
  urlBase: string;
}

const describeError = (error: unknown): string => (error instanceof Error ? error.message : String(error));

const registrationForm = (registration: Registration): FormData => {
  const form = new FormData();
  form.append("username", registration.username);
  form.append("password", registration.password);
  form.append("confirmPassword", registration.confirmPassword);
  return form;
};

const goHome = (deps: RegisterDeps): void => {
  deps.navigate(joinURL(deps.urlBase, "/"));
};

const register = async (registration: Registration, deps: RegisterDeps): Promise<void> => {
  try {
    const response = await fetcher("/register", { method: "POST", body: registrationForm(registration) });
    if (response.ok) {
      goHome(deps);
      return;
    }
    const text = await response.text();
    createToast(text === "" ? "Registration failed" : text, "error");
  } catch (error) {
    createToast(`Registration failed: ${describeError(error)}`, "error");
  }
};

const skipAuth = async (deps: RegisterDeps): Promise<void> => {
  try {
    const response = await fetcher("/skip-auth", { method: "POST" });
    if (!response.ok) {
      throw new Error("Failed to skip authentication");
    }
    goHome(deps);
  } catch (error) {
    createToast(`Error: ${describeError(error)}`, "error");
  }
};

const bindRegisterPage = (elements: RegisterElements, deps: RegisterDeps): void => {
  elements.form.addEventListener("submit", (event) => {
    event.preventDefault();
    const registration = {
      username: elements.username.value,
      password: elements.password.value,
      confirmPassword: elements.confirmPassword.value,
    };
    if (registration.password !== registration.confirmPassword) {
      createToast("Passwords do not match!", "error");
      return;
    }
    void register(registration, deps);
  });
  elements.skip.addEventListener("click", () => {
    void skipAuth(deps);
  });
};

export { bindRegisterPage, registrationForm };
