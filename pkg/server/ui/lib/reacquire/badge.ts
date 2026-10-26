import { cloneTemplate } from "#ui/lib/common/dom";
import { isActive, statusMeta } from "#ui/lib/reacquire/format";

const statusBadge = (status: string): DocumentFragment => {
  const meta = statusMeta(status);
  const badge = cloneTemplate("reacquire-status-badge");
  badge.querySelector("span")?.classList.add(meta.badge);
  const icon = badge.querySelector("i");
  icon?.classList.add(meta.icon, ...(isActive(status) ? ["animate-spin"] : []));
  icon?.after(meta.label);
  return badge;
};

export { statusBadge };
