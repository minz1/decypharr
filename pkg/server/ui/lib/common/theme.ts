const TRANSITION_MS = 300;
const DARK_QUERY = "(prefers-color-scheme: dark)";

const themeSetter =
  (doc: Document, view: Window, toggle: HTMLInputElement) =>
  (theme: string): void => {
    doc.documentElement.dataset["theme"] = theme;
    toggle.checked = theme === "dark";
    doc.body.style.transition = "background-color 0.3s ease, color 0.3s ease";
    view.setTimeout(() => {
      doc.body.style.transition = "";
    }, TRANSITION_MS);
  };

const setupThemeToggle = (doc: Document): void => {
  const toggle = doc.querySelector("#themeToggle");
  const view = doc.defaultView;
  if (!(toggle instanceof HTMLInputElement) || view === null) {
    return;
  }
  const setTheme = themeSetter(doc, view, toggle);
  setTheme(view.localStorage.getItem("theme") ?? (view.matchMedia(DARK_QUERY).matches ? "dark" : "light"));
  toggle.addEventListener("change", () => {
    const theme = doc.documentElement.dataset["theme"] === "dark" ? "light" : "dark";
    view.localStorage.setItem("theme", theme);
    setTheme(theme);
  });
  view.matchMedia(DARK_QUERY).addEventListener("change", (event) => {
    if (view.localStorage.getItem("theme") === null) {
      setTheme(event.matches ? "dark" : "light");
    }
  });
};

export { setupThemeToggle };
