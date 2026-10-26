import { expect, vi } from "vitest";

const realSetTimeout = globalThis.setTimeout;

const element = <Wanted extends Element>(selector: string, type: abstract new () => Wanted): Wanted => {
  const found = document.querySelector(selector);
  if (!(found instanceof type)) {
    throw new TypeError(`the page has no ${selector}`);
  }
  return found;
};

const settle = async (): Promise<void> => {
  const tick = { done: false };
  realSetTimeout(() => {
    tick.done = true;
  }, 0);
  await vi.waitFor(() => {
    expect(tick.done).toBe(true);
  });
};

const check = (box: HTMLInputElement, checked = true): void => {
  box.checked = checked;
  box.dispatchEvent(new Event("change", { bubbles: true }));
};

export { check, element, settle };
