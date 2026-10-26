declare global {
  var urlBase: string;
  var decypharrUtils: DecypharrUtils;
}

export interface DecypharrUtils {
  fetcher: (url: string, init?: RequestInit) => Promise<Response>;
  createToast: (message: string, type: "success" | "error" | "warning" | "info") => void;
  setButtonLoading: (button: HTMLButtonElement | null, loading: boolean) => void;
}
