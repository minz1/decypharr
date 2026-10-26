interface DecypharrUtils {
  fetcher(url: string, init?: RequestInit): Promise<Response>;
  createToast(message: string, type: "success" | "error" | "warning" | "info"): void;
  setButtonLoading(button: HTMLButtonElement | null, loading: boolean): void;
}

interface Window {
  urlBase: string;
  decypharrUtils: DecypharrUtils;
}
