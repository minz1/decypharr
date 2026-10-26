import { inject } from "vitest";

const loadFixture = (name: string): void => {
  const html = inject("fixtures")[name];
  if (typeof html !== "string") {
    throw new TypeError(`unknown UI fixture ${name}`);
  }
  const parsed = new DOMParser().parseFromString(html, "text/html");
  for (const element of parsed.querySelectorAll("script, link")) {
    element.remove();
  }
  document.documentElement.remove();
  document.append(document.importNode(parsed.documentElement, true));
};

export { loadFixture };
