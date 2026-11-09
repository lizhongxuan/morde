import { afterEach, expect, it, vi } from "vitest";
import { morde, validateHostTicketUrl } from "./api";

afterEach(() => { vi.unstubAllGlobals(); });

it("sends an instant chat message with the caller's retry key and same-origin cookie", async () => {
  const exchange = { conversation: { id: "c1" }, message: { id: "m1" }, reply: { id: "m2" } };
  const fetch = vi.fn().mockResolvedValue(Response.json(exchange));
  vi.stubGlobal("fetch", fetch);
  vi.stubGlobal("crypto", { randomUUID: () => "request-1" });

  expect(await morde.send("model-1", "你好", null, "send-key-1")).toEqual(exchange);
  const [url, options] = fetch.mock.calls[0];
  expect(url).toBe("/api/instant-chat/messages");
  expect(options.credentials).toBe("same-origin");
  expect(options.headers["Idempotency-Key"]).toBe("send-key-1");
  expect(JSON.parse(options.body)).toEqual({ model_id: "model-1", content: "你好" });
});

it("reuses an instant chat conversation without submitting a task", async () => {
  const fetch = vi.fn().mockResolvedValue(Response.json({ conversation: { id: "c1" }, message: {}, reply: {} }));
  vi.stubGlobal("fetch", fetch);
  vi.stubGlobal("crypto", { randomUUID: () => "request-1" });
  await morde.send("model-1", "继续", "c1", "send-key-2");
  expect(JSON.parse(fetch.mock.calls[0][1].body)).toEqual({ model_id: "model-1", content: "继续", conversation_id: "c1" });
});

it("uses the Host ticket endpoints and marks notifications read without Cloud credentials", async () => {
  const fetch = vi.fn().mockResolvedValueOnce(Response.json({ url: "https://host.test/__ch/auth?t=one-time", expires_at: "2026-10-26T00:00:00Z" }))
    .mockResolvedValueOnce(Response.json({ url: "https://host.test/__ch/auth?t=observe", expires_at: "2026-10-26T00:00:00Z" }))
    .mockResolvedValueOnce(new Response(null, { status: 204 }));
  vi.stubGlobal("fetch", fetch);
  vi.stubGlobal("crypto", { randomUUID: () => "request-1" });

  await morde.ticket("host-1", false);
  await morde.ticket("host-1", true);
  await morde.markRead("notice-1");

  expect(fetch.mock.calls.map(([url]) => url)).toEqual([
    "/api/hosts/host-1/enter", "/api/hosts/host-1/observation-ticket", "/api/notifications/notice-1/read",
  ]);
  for (const [, options] of fetch.mock.calls) {
    expect(options.credentials).toBe("same-origin");
    expect(options.headers.Authorization).toBeUndefined();
  }
});

it("accepts a Host ticket only for the selected Host origin and auth route", () => {
  expect(validateHostTicketUrl(
    "https://host.test/__ch/auth?t=one-time", "https://host.test", "https://portal.test/app/",
  ).href).toBe("https://host.test/__ch/auth?t=one-time");
});

it.each([
  ["cross Host origin", "https://other.test/__ch/auth?t=one-time", "https://host.test", "https://portal.test/"],
  ["wrong Host port", "https://host.test:8443/__ch/auth?t=one-time", "https://host.test", "https://portal.test/"],
  ["wrong path", "https://host.test/other?t=one-time", "https://host.test", "https://portal.test/"],
  ["missing ticket", "https://host.test/__ch/auth", "https://host.test", "https://portal.test/"],
  ["blank ticket", "https://host.test/__ch/auth?t=%20", "https://host.test", "https://portal.test/"],
  ["ambiguous ticket", "https://host.test/__ch/auth?t=one&t=two", "https://host.test", "https://portal.test/"],
  ["ticket credentials", "https://user@host.test/__ch/auth?t=one-time", "https://host.test", "https://portal.test/"],
  ["ticket fragment", "https://host.test/__ch/auth?t=one-time#other", "https://host.test", "https://portal.test/"],
  ["relative ticket URL", "/__ch/auth?t=one-time", "https://host.test", "https://portal.test/"],
  ["HTTP Host from HTTPS portal", "http://host.localhost/__ch/auth?t=one-time", "http://host.localhost", "https://portal.test/"],
  ["remote HTTP Host", "http://host.test/__ch/auth?t=one-time", "http://host.test", "http://127.0.0.1:5173/app/"],
  ["lookalike loopback domain", "http://127.evil.test/__ch/auth?t=one-time", "http://127.evil.test", "http://127.evil.test/"],
])("rejects %s", (_name, ticket, host, page) => {
  expect(() => validateHostTicketUrl(ticket, host, page)).toThrow("主机地址无效");
});

it("allows an HTTP Host only from a local HTTP portal during local development", () => {
  expect(validateHostTicketUrl(
    "http://host.localhost/__ch/auth?t=one-time", "http://host.localhost", "http://127.0.0.1:5173/app/",
  ).href).toBe("http://host.localhost/__ch/auth?t=one-time");
  expect(validateHostTicketUrl(
    "http://127.255.255.255/__ch/auth?t=one-time", "http://127.255.255.255", "http://127.0.0.1:5173/app/",
  ).href).toBe("http://127.255.255.255/__ch/auth?t=one-time");
});
