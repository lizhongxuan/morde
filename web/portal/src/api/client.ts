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

function requestId() {
  return globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

export async function requestFetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
  const headers = new Headers(input instanceof Request ? input.headers : undefined);
  new Headers(init?.headers).forEach((value, key) => headers.set(key, value));
  headers.set("X-Request-Id", headers.get("X-Request-Id") ?? requestId());
  const method = (init?.method ?? (input instanceof Request ? input.method : "GET")).toUpperCase();
  if (method !== "GET" && method !== "HEAD" && !(init?.body instanceof FormData)) {
    headers.set("Content-Type", headers.get("Content-Type") ?? "application/json");
  }
  const response = await fetch(input, { ...init, headers, credentials: "include" });
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
    throw new ApiError({
      message: typeof raw.message === "string" ? raw.message : "请求失败，请稍后重试",
      code: typeof raw.code === "string" ? raw.code : "internal",
      status: result.response.status,
      retryAfter: retry !== undefined && Number.isFinite(retry) ? retry : undefined,
      details: isRecord(raw.details) ? raw.details : {},
    });
  }
  if (result.data === undefined && result.response.status !== 204) {
    throw new ApiError({ message: "服务器返回了空响应", code: "internal", status: result.response.status });
  }
  return result.data as T;
}
