import { expect, vi } from "vitest";

type ToastType = "error" | "info" | "success" | "warning";

interface ShownToast {
  type: string;
  message: string;
}

const shownToasts = (): ShownToast[] =>
  [...document.querySelectorAll(".toast-container .alert")].map((toast) => ({
    type: [...toast.classList].find((name) => name.startsWith("alert-"))?.slice("alert-".length) ?? "",
    message: toast.querySelector("[data-toast-message]")?.textContent ?? "",
  }));

const toasts = (type?: ToastType): string[] =>
  shownToasts()
    .filter((toast) => type === undefined || toast.type === type)
    .map((toast) => toast.message);

const labelledToasts = (): string[] => shownToasts().map((toast) => `alert-${toast.type} ${toast.message.trim()}`);

const waitForAnyToast = async (type?: ToastType): Promise<void> => {
  await vi.waitFor(() => {
    expect(toasts(type).length).toBeGreaterThan(0);
  });
};

export type { ShownToast, ToastType };
export { labelledToasts, shownToasts, toasts, waitForAnyToast };
