const SECURITY = ["-webkit-text-security", "text-security"];

const toggleTextarea = (field: HTMLTextAreaElement, icon: Element): void => {
  const hidden = field.dataset["passwordVisible"] !== "true";
  for (const property of SECURITY) {
    field.style.setProperty(property, hidden ? "none" : "disc");
  }
  field.dataset["passwordVisible"] = String(hidden);
  icon.className = hidden ? "bi bi-eye-slash" : "bi bi-eye";
};

const toggleInput = (field: HTMLInputElement, icon: Element): void => {
  const hidden = field.type === "password";
  field.type = hidden ? "text" : "password";
  icon.className = hidden ? "bi bi-eye-slash" : "bi bi-eye";
};

const toggleField = (button: Element): void => {
  const field = button.closest(".password-toggle-container")?.querySelector("input, textarea");
  const icon = button.querySelector("i");
  if (icon === null) {
    return;
  }
  if (field instanceof HTMLTextAreaElement) {
    toggleTextarea(field, icon);
  } else if (field instanceof HTMLInputElement) {
    toggleInput(field, icon);
  }
};

const setupPasswordToggles = (doc: Document): void => {
  doc.addEventListener("click", (event) => {
    if (!(event.target instanceof Element)) {
      return;
    }
    const button = event.target.closest(".password-toggle-btn");
    if (button !== null) {
      event.preventDefault();
      event.stopPropagation();
      toggleField(button);
    }
  });
};

export { setupPasswordToggles };
