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
    throw new Error(`missing #${id}`);
  }
  return found;
};

const cloneTemplate = (id: string, root: Document = document): DocumentFragment => {
  const template = root.querySelector(`#${CSS.escape(id)}`);
  if (!(template instanceof HTMLTemplateElement)) {
    throw new Error(`missing template #${id}`);
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

export { bootPage, byId, cloneTemplate, onAction };
