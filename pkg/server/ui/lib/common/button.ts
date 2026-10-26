const saved = new WeakMap<HTMLButtonElement, Node[]>();

const spinner = (): HTMLSpanElement => {
  const span = document.createElement("span");
  span.classList.add("loading", "loading-spinner", "loading-sm");
  return span;
};

const setButtonLoading = (button: HTMLButtonElement | null, loading: boolean): void => {
  if (button === null) {
    return;
  }
  button.disabled = loading;
  if (loading) {
    if (!saved.has(button)) {
      saved.set(button, [...button.childNodes]);
    }
    button.replaceChildren(spinner(), "Processing...");
    return;
  }
  button.replaceChildren(...(saved.get(button) ?? ["Submit"]));
  saved.delete(button);
};

export { setButtonLoading };
