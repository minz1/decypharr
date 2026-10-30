import { describeError } from "#ui/lib/common/errors";
import { createToast } from "#ui/lib/common/toast";

type ActionHandler = (target: HTMLElement, event: Event) => void;

const byId = <Wanted extends HTMLElement>(
  id: string,
  type: abstract new () => Wanted,
  root: Document = document,
): Wanted => {
  const found = root.querySelector(`#${CSS.escape(id)}`);
  if (!(found instanceof type)) {
    throw new TypeError(`missing #${id}`);
  }
  return found;
};

const cloneTemplate = (id: string, root: Document = document): DocumentFragment => {
  const template = root.querySelector(`#${CSS.escape(id)}`);
  if (!(template instanceof HTMLTemplateElement)) {
    throw new TypeError(`missing template #${id}`);
  }
  return root.importNode(template.content, true);
};

const onAction = (
  container: HTMLElement,
  eventType: string,
  handlers: Readonly<Record<string, ActionHandler>>,
): void => {
  container.addEventListener(eventType, (event) => {
    if (!(event.target instanceof Element)) {
      return;
    }
    const origin = event.target.closest("[data-action]");
    if (origin instanceof HTMLElement && container.contains(origin)) {
      handlers[origin.dataset["action"] ?? ""]?.(origin, event);
    }
  });
};

const bootPage = (name: string, start: () => void | Promise<void>): void => {
  const run = async (): Promise<void> => {
    try {
      await start();
    } catch (error) {
      createToast(`${name}: ${describeError(error)}`, "error");
    }
  };
  void run();
};

const field = (root: ParentNode, name: string): HTMLElement => {
  const found = root.querySelector(`[data-field="${name}"]`);
  if (!(found instanceof HTMLElement)) {
    throw new TypeError(`missing [data-field="${name}"]`);
  }
  return found;
};

const cloneRow = (id: string): HTMLTableRowElement => {
  const row = cloneTemplate(id).querySelector("tr");
  if (row === null) {
    throw new TypeError(`template #${id} has no row`);
  }
  return row;
};

const onClick = (button: HTMLButtonElement, action: () => unknown): void => {
  button.addEventListener("click", () => {
    void action();
  });
};

export { bootPage, byId, cloneRow, cloneTemplate, field, onAction, onClick };
