import { asArray, asRecord, numOrZero, optNum, strOrEmpty } from "#ui/lib/common/json";
import type { Binding, IndexSummary, Job, Mutation } from "#ui/lib/reacquire/types";

type JSONRecord = Record<string, unknown>;

const listOrEmpty = (value: unknown, path: string): readonly unknown[] =>
  value === null || value === undefined ? [] : asArray(value, path);

const parseBinding = (value: unknown): Binding => {
  const binding = asRecord(value, "binding");
  return {
    arrName: strOrEmpty(binding, "arrName"),
    arrType: strOrEmpty(binding, "arrType"),
    entryId: strOrEmpty(binding, "entryId"),
    entryName: strOrEmpty(binding, "entryName"),
    entryFileId: strOrEmpty(binding, "entryFileId"),
    entryFileName: strOrEmpty(binding, "entryFileName"),
    arrFileId: numOrZero(binding, "arrFileId"),
    seriesId: numOrZero(binding, "seriesId"),
    seasonNumber: optNum(binding, "seasonNumber"),
    movieId: numOrZero(binding, "movieId"),
    confidence: strOrEmpty(binding, "confidence"),
  };
};

const parseMutation = (value: unknown): Mutation => {
  const mutation = asRecord(value, "mutation");
  return {
    kind: strOrEmpty(mutation, "kind"),
    state: strOrEmpty(mutation, "state"),
    attempts: numOrZero(mutation, "attempts"),
    confirmedAt: strOrEmpty(mutation, "confirmedAt"),
  };
};

const parseJob = (value: unknown): Job => {
  const job = asRecord(value, "job");
  return {
    id: strOrEmpty(job, "id"),
    status: strOrEmpty(job, "status"),
    cause: strOrEmpty(job, "cause"),
    strategy: strOrEmpty(job, "strategy"),
    arrName: strOrEmpty(job, "arrName"),
    arrType: strOrEmpty(job, "arrType"),
    fileId: strOrEmpty(job, "fileId"),
    downloadId: strOrEmpty(job, "downloadId"),
    replacementDownloadId: strOrEmpty(job, "replacementDownloadId"),
    lastError: strOrEmpty(job, "lastError"),
    createdAt: strOrEmpty(job, "createdAt"),
    updatedAt: strOrEmpty(job, "updatedAt"),
    completedAt: strOrEmpty(job, "completedAt"),
    bindings: listOrEmpty(job["bindings"], "bindings").map((item) => parseBinding(item)),
    mutations: listOrEmpty(job["mutations"], "mutations").map((item) => parseMutation(item)),
  };
};

const parseJobs = (value: unknown): Job[] => listOrEmpty(value, "jobs").map((item) => parseJob(item));

const parseBindings = (value: unknown): Binding[] => listOrEmpty(value, "bindings").map((item) => parseBinding(item));

const parseSummary = (value: unknown): IndexSummary => {
  const summary = asRecord(value, "summary");
  return {
    arrName: strOrEmpty(summary, "arrName"),
    arrType: strOrEmpty(summary, "arrType"),
    bindings: numOrZero(summary, "bindings"),
    actionable: numOrZero(summary, "actionable"),
    updatedAt: strOrEmpty(summary, "updatedAt"),
  };
};

const parseIndex = (value: unknown): IndexSummary[] => listOrEmpty(value, "index").map((item) => parseSummary(item));

const bindingsEndpoint = (query: string, arrName: string): string => {
  const params = new URLSearchParams();
  if (query !== "") {
    params.set("q", query);
  }
  if (arrName !== "") {
    params.set("arr", arrName);
  }
  return `/api/arr/bindings?${params.toString()}`;
};

const jsonRequest = (method: string, body: unknown): RequestInit => ({
  method,
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify(body),
});

const deleteJobsRequest = (ids: readonly string[]): RequestInit => jsonRequest("DELETE", { ids });

const reacquireRequest = (binding: Binding, strategy: string): RequestInit =>
  jsonRequest("POST", {
    entryId: binding.entryId,
    fileId: binding.entryFileId,
    cause: "manual",
    strategy: strategy === "" ? "history_failed" : strategy,
  });

const looseRecord = (body: string): JSONRecord => {
  try {
    const data: unknown = JSON.parse(body);
    return typeof data === "object" && data !== null && !Array.isArray(data) ? asRecord(data, "response") : {};
  } catch {
    return {};
  }
};

const looseString = (record: JSONRecord, key: string): string => {
  const value = record[key];
  return typeof value === "string" ? value : "";
};

const errorDetail = (body: string, status: number): string => {
  const error = looseString(looseRecord(body), "error");
  return error === "" ? `HTTP ${status}` : error;
};

const deletedCount = (body: string, requested: number): number => {
  const { deleted } = looseRecord(body);
  return typeof deleted === "number" ? deleted : requested;
};

export {
  bindingsEndpoint,
  deleteJobsRequest,
  deletedCount,
  errorDetail,
  parseBindings,
  parseIndex,
  parseJobs,
  reacquireRequest,
};
