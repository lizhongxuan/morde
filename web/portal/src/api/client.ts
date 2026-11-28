import createClient from "openapi-fetch";
import type { components, paths } from "./schema";

export class ApiError extends Error {
  readonly code: string;
  readonly status: number;
  readonly retryAfter?: number;
  readonly details?: Record<string, unknown>;

  constructor(input: { message: string; code: string; status: number; retryAfter?: number; details?: Record<string, unknown> }) {
    super(input.message);
    this.name = "ApiError";
    this.code = input.code;
    this.status = input.status;
    this.retryAfter = input.retryAfter;
    this.details = input.details;
  }
}

export type ServerSentEvent = { event: string; data: string; id: string };

export async function consumeServerSentEvents(response: Response, onEvent: (event: ServerSentEvent) => void,
  signal?: AbortSignal): Promise<void> {
  if (!response.body) throw new TypeError("Event stream response has no body");
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let eventName = "";
  let eventID = "";
  let data: string[] = [];
  const dispatch = () => {
    if (data.length) onEvent({ event: eventName || "message", data: data.join("\n"), id: eventID });
    eventName = "";
    data = [];
  };
  const processLine = (line: string) => {
    if (line === "") { dispatch(); return; }
    if (line.startsWith(":")) return;
    const separator = line.indexOf(":");
    const field = separator < 0 ? line : line.slice(0, separator);
    let value = separator < 0 ? "" : line.slice(separator + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    if (field === "event") eventName = value;
    else if (field === "data") data.push(value);
    else if (field === "id" && !value.includes("\0")) eventID = value;
  };
  const processLines = (final: boolean) => {
    while (true) {
      const separator = buffer.search(/[\r\n]/);
      if (separator < 0 || (!final && buffer[separator] === "\r" && separator === buffer.length - 1)) break;
      const line = buffer.slice(0, separator);
      const width = buffer[separator] === "\r" && buffer[separator + 1] === "\n" ? 2 : 1;
      buffer = buffer.slice(separator + width);
      processLine(line);
    }
    if (final) {
      if (buffer) processLine(buffer);
      buffer = "";
      dispatch();
    }
  };
  const cancel = () => { void reader.cancel().catch(() => undefined); };
  signal?.addEventListener("abort", cancel, { once: true });
  try {
    if (signal?.aborted) cancel();
    while (!signal?.aborted) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      processLines(false);
    }
    if (!signal?.aborted) {
      buffer += decoder.decode();
      processLines(true);
    }
  } finally {
    signal?.removeEventListener("abort", cancel);
    reader.releaseLock();
  }
}

function requestId() {
  return globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

function randomHex(byteLength: number) {
  const bytes = new Uint8Array(byteLength);
  if (globalThis.crypto?.getRandomValues) globalThis.crypto.getRandomValues(bytes);
  else for (let index = 0; index < bytes.length; index++) bytes[index] = Math.floor(Math.random() * 256);
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
}

function traceparent() {
  let traceId = randomHex(16);
  let spanId = randomHex(8);
  if (/^0+$/.test(traceId)) traceId = `${traceId.slice(0, -1)}1`;
  if (/^0+$/.test(spanId)) spanId = `${spanId.slice(0, -1)}1`;
  return `00-${traceId}-${spanId}-01`;
}

export async function requestFetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
  const headers = new Headers(input instanceof Request ? input.headers : undefined);
  new Headers(init?.headers).forEach((value, key) => headers.set(key, value));
  headers.set("X-Request-Id", headers.get("X-Request-Id") ?? requestId());
  headers.set("traceparent", headers.get("traceparent") ?? traceparent());
  const method = (init?.method ?? (input instanceof Request ? input.method : "GET")).toUpperCase();
  if (method !== "GET" && method !== "HEAD" && !(init?.body instanceof FormData)) {
    headers.set("Content-Type", headers.get("Content-Type") ?? "application/json");
  }
  let response: Response;
  try {
    response = await fetch(input, { ...init, headers, credentials: "include" });
  } catch (error) {
    if (!init?.signal?.aborted && error instanceof TypeError) window.dispatchEvent(new CustomEvent("morde:api-network-error", { detail: { source: "request" } }));
    throw error;
  }
  const requestTraceID = headers.get("traceparent")?.split("-")[1] ?? "";
  const headerTraceID = response.headers.get("X-Trace-Id");
  const responseTraceID = headerTraceID && /^[0-9a-f]{32}$/.test(headerTraceID) ? headerTraceID : requestTraceID;
  window.dispatchEvent(new CustomEvent("morde:api-trace", {
    detail: { request_id: headers.get("X-Request-Id"), trace_id: responseTraceID, status: response.status },
  }));
  window.dispatchEvent(new CustomEvent(response.status >= 500 ? "morde:api-network-error" : "morde:api-network-recovered", {
    detail: { source: "request" },
  }));
  const path = new URL(typeof input === "string" || input instanceof URL ? input : input.url, window.location.origin).pathname;
  if (response.status === 401 && path !== "/api/auth/login" && path !== "/api/me" && window.location.pathname !== "/login") {
    const next = `${window.location.pathname}${window.location.search}`;
    window.location.assign(`/login?next=${encodeURIComponent(next)}`);
  }
  return response;
}

export const api = createClient<paths>({ baseUrl: window.location.origin, fetch: requestFetch });

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

export function unwrapApiResponse<T>(result: { data?: T; error?: unknown; response: Response }): T {
  if (result.error !== undefined) {
    const outer = isRecord(result.error) ? result.error : {};
    const raw: Partial<components["schemas"]["ErrorResponse"]["error"]> = isRecord(outer.error)
      ? outer.error as Partial<components["schemas"]["ErrorResponse"]["error"]>
      : {};
    const retryText = result.response.headers.get("Retry-After");
    const retry = retryText === null ? undefined : Number(retryText);
    const requestID = result.response.headers.get("X-Request-Id");
    const traceID = result.response.headers.get("X-Trace-Id");
    const serverDetails = isRecord(raw.details) ? { ...raw.details } : {};
    delete serverDetails.request_id;
    delete serverDetails.trace_id;
    throw new ApiError({
      message: typeof raw.message === "string" ? raw.message : "请求失败，请稍后重试",
      code: typeof raw.code === "string" ? raw.code : "internal",
      status: result.response.status,
      retryAfter: retry !== undefined && Number.isFinite(retry) ? retry : undefined,
      details: {
        ...serverDetails,
        ...(requestID && /^[A-Za-z0-9._:-]{1,128}$/.test(requestID) ? { request_id: requestID } : {}),
        ...(traceID && /^[0-9a-f]{32}$/.test(traceID) ? { trace_id: traceID } : {}),
      },
    });
  }
  if (result.data === undefined && result.response.status !== 204) {
    throw new ApiError({ message: "服务器返回了空响应", code: "internal", status: result.response.status });
  }
  return result.data as T;
}
