import { cloneTemplate } from "#ui/lib/common/dom";
import { statusBadge } from "#ui/lib/reacquire/badge";
import { arrKind, formatTime } from "#ui/lib/reacquire/format";
import type { Binding, Job, Mutation } from "#ui/lib/reacquire/types";
import { cloneRows, field, fillFields } from "#ui/lib/repair/dom";

type DetailValue = Node | string;

const code = (value: string): DetailValue => {
  if (value === "") {
    return "-";
  }
  const element = document.createElement("code");
  element.classList.add("text-xs");
  element.textContent = value;
  return element;
};

const arrValue = (job: Job): DetailValue[] => {
  if (!arrKind(job.arrName, job.arrType)) {
    return [job.arrName === "" ? "-" : job.arrName];
  }
  const type = document.createElement("span");
  type.classList.add("opacity-60");
  type.textContent = `(${job.arrType})`;
  return [`${job.arrName} `, type];
};

const detailRows = (job: Job, now: number): (readonly [string, DetailValue[]])[] => [
  ["Status", [statusBadge(job.status)]],
  ["Arr", arrValue(job)],
  ["Source", [job.cause === "" ? "-" : job.cause]],
  ["Strategy", [job.strategy === "" ? "-" : job.strategy]],
  ["Broken download", [code(job.downloadId)]],
  ["Replacement", [code(job.replacementDownloadId)]],
  ["Created", [formatTime(job.createdAt, now)]],
  ["Updated", [formatTime(job.updatedAt, now)]],
  ["Completed", [job.completedAt === "" ? "-" : formatTime(job.completedAt, now)]],
];

const detailRow = (label: string, value: readonly DetailValue[]): DocumentFragment => {
  const row = cloneTemplate("reacquire-detail-row");
  field(row, "label").textContent = label;
  field(row, "value").replaceChildren(...value);
  return row;
};

const bindingRow = (binding: Binding): DocumentFragment => {
  const row = cloneRows("reacquire-detail-binding");
  fillFields(row, {
    file: binding.entryFileName === "" ? binding.entryFileId : binding.entryFileName,
    "arr-file": binding.arrFileId === 0 ? "-" : String(binding.arrFileId),
    match: binding.confidence === "" ? "-" : binding.confidence,
  });
  return row;
};

const mutationRow = (mutation: Mutation, now: number): DocumentFragment => {
  const row = cloneRows("reacquire-detail-mutation");
  const confirmed = mutation.state === "confirmed";
  fillFields(row, {
    kind: mutation.kind,
    state: confirmed ? "confirmed" : "intent",
    attempts: String(mutation.attempts),
    confirmed: mutation.confirmedAt === "" ? "-" : formatTime(mutation.confirmedAt, now),
  });
  field(row, "state").classList.add(confirmed ? "badge-success" : "badge-warning");
  return row;
};

const fillSection = (section: HTMLElement, rows: readonly DocumentFragment[]): void => {
  if (rows.length === 0) {
    section.remove();
    return;
  }
  field(section, "rows").replaceChildren(...rows);
};

const jobDetail = (job: Job, now: number): DocumentFragment => {
  const detail = cloneTemplate("reacquire-job-detail");
  field(detail, "grid").replaceChildren(...detailRows(job, now).map(([label, value]) => detailRow(label, value)));
  const error = field(detail, "error");
  if (job.lastError === "") {
    error.remove();
  } else {
    field(error, "message").textContent = job.lastError;
  }
  fillSection(
    field(detail, "files"),
    job.bindings.map((binding) => bindingRow(binding)),
  );
  fillSection(
    field(detail, "actions"),
    job.mutations.map((mutation) => mutationRow(mutation, now)),
  );
  return detail;
};

const acknowledgeButton = (): HTMLButtonElement => {
  const button = document.createElement("button");
  button.classList.add("btn", "btn-warning", "btn-sm");
  button.textContent = "Acknowledge Arr actions";
  return button;
};

export { acknowledgeButton, jobDetail };
