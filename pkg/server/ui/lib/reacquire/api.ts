import { fetcher } from "#ui/lib/common/http";
import { asArray, asRecord, optNum, optStr, readJSON } from "#ui/lib/common/json";
import type { Binding, IndexSummary, Job, Mutation } from "#ui/lib/reacquire/types";

type JSONRecord = Record<string, unknown>;

const listOrEmpty = (value: unknown, path: string): readonly unknown[] =>
  value === null || value === undefined ? [] : asArray(value, path);

const text = (record: JSONRecord, key: string): string => optStr(record, key) ?? "";

const count = (record: JSONRecord, key: string): number => optNum(record, key) ?? 0;

const getJSON = async (endpoint: string): Promise<unknown> => readJSON(await fetcher(endpoint));

const parseBinding = (value: unknown): Binding => {
  const binding = asRecord(value, "binding");
  return {
    arrName: text(binding, "arrName"),
    arrType: text(binding, "arrType"),
    entryId: text(binding, "entryId"),
    entryName: text(binding, "entryName"),
    entryFileId: text(binding, "entryFileId"),
    entryFileName: text(binding, "entryFileName"),
    arrFileId: count(binding, "arrFileId"),
    seriesId: count(binding, "seriesId"),
    seasonNumber: optNum(binding, "seasonNumber"),
    movieId: count(binding, "movieId"),
    confidence: text(binding, "confidence"),
  };
};

const parseMutation = (value: unknown): Mutation => {
  const mutation = asRecord(value, "mutation");
  return {
    kind: text(mutation, "kind"),
    state: text(mutation, "state"),
    attempts: count(mutation, "attempts"),
    confirmedAt: text(mutation, "confirmedAt"),
  };
};

const parseJob = (value: unknown): Job => {
  const job = asRecord(value, "job");
  return {
    id: text(job, "id"),
    status: text(job, "status"),
    cause: text(job, "cause"),
    strategy: text(job, "strategy"),
    arrName: text(job, "arrName"),
    arrType: text(job, "arrType"),
    fileId: text(job, "fileId"),
    downloadId: text(job, "downloadId"),
    replacementDownloadId: text(job, "replacementDownloadId"),
    lastError: text(job, "lastError"),
    createdAt: text(job, "createdAt"),
    updatedAt: text(job, "updatedAt"),
    completedAt: text(job, "completedAt"),
    bindings: listOrEmpty(job["bindings"], "bindings").map((item) => parseBinding(item)),
    mutations: listOrEmpty(job["mutations"], "mutations").map((item) => parseMutation(item)),
  };
};

const parseJobs = (value: unknown): Job[] => listOrEmpty(value, "jobs").map((item) => parseJob(item));

const parseBindings = (value: unknown): Binding[] => listOrEmpty(value, "bindings").map((item) => parseBinding(item));

const parseSummary = (value: unknown): IndexSummary => {
  const summary = asRecord(value, "summary");
  return {
    arrName: text(summary, "arrName"),
    arrType: text(summary, "arrType"),
    bindings: count(summary, "bindings"),
    actionable: count(summary, "actionable"),
    updatedAt: text(summary, "updatedAt"),
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
  getJSON,
  parseBindings,
  parseIndex,
  parseJobs,
  reacquireRequest,
};
