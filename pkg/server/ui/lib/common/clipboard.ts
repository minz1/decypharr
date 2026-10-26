import { createToast } from "#ui/lib/common/toast";

const copyToClipboard = async (text: string): Promise<boolean> => {
  try {
    await navigator.clipboard.writeText(text);
    createToast("Copied to clipboard", "success");
    return true;
  } catch {
    createToast("Failed to copy to clipboard", "error");
    return false;
  }
};

export { copyToClipboard };
