export type Host = {
  id: string; name: string; state: string; owner_id: string; url: string;
  workspaces: { workspace: { id: string; name: string } }[];
};
export type SharedHost = { id: string; name: string; state: string; url: string; permissions: string[] };
export type Workspace = { id: string; name: string; revision: number; state: string };
export type Model = { id: string; name: string; model_name: string; capabilities: { text: boolean; tools: boolean; vision: boolean } };
export type Conversation = { id: string; title: string; model_id: string; updated_at: string };
export type Message = { id: string; role: "user" | "assistant"; content: string };
export type Notification = { id: string; summary: string | null; kind: string; read_at: string | null; task_id: string | null; actionable: boolean };

export class ApiError extends Error {
  constructor(readonly status: number, message: string) { super(message); }
}

export const newKey = () => crypto.randomUUID();

export function validateHostTicketUrl(ticketUrl: string, hostUrl: string, pageUrl: string): URL {
  let ticket: URL;
  let host: URL;
  let page: URL;
  try {
    ticket = new URL(ticketUrl);
    host = new URL(hostUrl);
    page = new URL(pageUrl);
  } catch {
    throw new Error("主机地址无效");
  }
  const localHost = (hostname: string) => {
    const octets = hostname.split(".");
    const loopbackV4 = octets.length === 4 && octets[0] === "127" &&
      octets.every((part) => /^(0|[1-9]\d{0,2})$/.test(part) && Number(part) <= 255);
    return hostname === "localhost" || hostname.endsWith(".localhost") || hostname === "[::1]" || loopbackV4;
  };
  const localHttp = host.protocol === "http:" && page.protocol === "http:" &&
    localHost(host.hostname) && localHost(page.hostname);
  const ticketValues = ticket.searchParams.getAll("t");
  if ((host.protocol !== "https:" && !localHttp) || host.username || host.password || host.pathname !== "/" ||
      host.search || host.hash || ticket.origin !== host.origin || ticket.pathname !== "/__ch/auth" ||
      ticket.username || ticket.password || ticket.hash || ticketValues.length !== 1 || !ticketValues[0]?.trim()) {
    throw new Error("主机地址无效");
  }
  return ticket;
}

async function request<T>(path: string, method = "GET", body?: unknown, key?: string): Promise<T> {
  const headers: Record<string, string> = { "X-Request-Id": newKey() };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (key) headers["Idempotency-Key"] = key;
  const response = await fetch(path, {
    method, headers, body: body === undefined ? undefined : JSON.stringify(body),
    credentials: "same-origin", cache: "no-store",
  });
  if (!response.ok) {
    const payload = await response.json().catch(() => null) as { error?: { message?: string } } | null;
    throw new ApiError(response.status, payload?.error?.message ?? `请求失败 (${response.status})`);
  }
  if (response.status === 204) return undefined as T;
  return await response.json() as T;
}

const path = (id: string) => encodeURIComponent(id);
const page = <T>(url: string) => request<{ items: T[] }>(url).then((data) => data.items);

export const morde = {
  me: () => request<{ id: string; name: string }>("/api/me"),
  hosts: () => page<Host>("/api/hosts"),
  sharedHosts: () => page<SharedHost>("/api/shared-hosts"),
  workspaces: () => page<Workspace>("/api/workspaces"),
  models: () => page<Model>("/api/available-models"),
  conversations: () => page<Conversation>("/api/instant-chat/conversations"),
  messages: (id: string) => page<Message>(`/api/instant-chat/conversations/${path(id)}/messages`),
  notifications: () => page<Notification>("/api/notifications?limit=100"),
  send: (modelId: string, content: string, conversationId: string | null, key: string) =>
    request<{ conversation: Conversation; message: Message; reply: Message }>("/api/instant-chat/messages", "POST", {
      model_id: modelId, content, ...(conversationId ? { conversation_id: conversationId } : {}),
    }, key),
  markRead: (id: string) => request<unknown>(`/api/notifications/${path(id)}/read`, "POST"),
  ticket: (hostId: string, shared: boolean) => request<{ url: string; expires_at: string }>(
    shared ? `/api/hosts/${path(hostId)}/observation-ticket` : `/api/hosts/${path(hostId)}/enter`, "POST"),
};
