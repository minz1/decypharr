import { asRecord, optBool, optStr } from "#ui/lib/common/json";
import { checkedOf, setChecked, textList, withId } from "#ui/lib/config/form";

const EVENTS = 'input[name="notifications.events[]"]';

const eventBoxes = (doc: Document): HTMLInputElement[] => [...doc.querySelectorAll<HTMLInputElement>(EVENTS)];

const setIfGiven = (doc: Document, id: string, value: string | undefined): void => {
  const control = withId(doc, id);
  if (control !== undefined && value !== undefined && value !== "") {
    control.value = value;
  }
};

const populateNotifications = (doc: Document, value: unknown): void => {
  if (value === undefined || value === null) {
    return;
  }
  const notifications = asRecord(value, "notifications");
  setChecked(withId(doc, "notifications.enabled"), optBool(notifications, "enabled") ?? false);
  setIfGiven(doc, "notifications.webhook_url", optStr(notifications, "webhook_url"));
  setIfGiven(doc, "notifications.callback_url", optStr(notifications, "callback_url"));
  const events = new Set(textList(notifications["events"], "notifications.events"));
  for (const box of eventBoxes(doc)) {
    if (events.has(box.value)) {
      box.checked = true;
    }
  }
};

const collectNotifications = (doc: Document): Record<string, unknown> => ({
  enabled: checkedOf(withId(doc, "notifications.enabled")) ?? false,
  webhook_url: withId(doc, "notifications.webhook_url")?.value ?? "",
  callback_url: withId(doc, "notifications.callback_url")?.value ?? "",
  events: eventBoxes(doc)
    .filter((box) => box.checked)
    .map((box) => box.value),
});

export { collectNotifications, populateNotifications };
