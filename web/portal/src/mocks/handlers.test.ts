import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { setupServer } from "msw/node";
import { handlers, resetMockState, setMockWorkspaceSyncConflict } from "./handlers";

const server = setupServer(...handlers);
function createApiClient() {
  let cookie = "";
  return async (path: string, init?: RequestInit) => {
    const headers = new Headers(init?.headers);
    if (cookie) headers.set("cookie", cookie);
    const response = await fetch(new URL(path, globalThis.location.href), { ...init, headers });
    const setCookie = response.headers.get("set-cookie");
    if (setCookie) {
      const pair = setCookie.split(";", 1)[0] ?? "";
      cookie = pair.endsWith("=") ? "" : pair;
    }
    return response;
  };
}
let api = createApiClient();
const json = (method: string, body: unknown): RequestInit => ({ method, headers: { "content-type": "application/json" }, body: JSON.stringify(body) });
const wait = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));
let workspaceSequence = 0;
let hostSequence = 0;
const createWorkspace = (name: string) => api("/api/workspaces", {
  ...json("POST", { name }), headers: { "content-type": "application/json", "Idempotency-Key": `workspace-test-${++workspaceSequence}` },
});
const createHostRequest = (body: unknown, key = `host-test-${++hostSequence}`) => api("/api/hosts", {
  method: "POST", headers: { "content-type": "application/json", "Idempotency-Key": key }, body: JSON.stringify(body),
});

beforeAll(() => server.listen());
beforeEach(() => { resetMockState(); api = createApiClient(); workspaceSequence = 0; hostSequence = 0; });
afterEach(() => { server.resetHandlers(); vi.restoreAllMocks(); });
afterAll(() => server.close());

describe("portal mock cookie sessions", () => {
  it("approves and revokes only the current user's client without exposing the code in audit", async () => {
    const owner = createApiClient();
    const other = createApiClient();
    await owner("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    await other("/api/auth/login", json("POST", { name: "member", password: "member" }));
    expect((await other("/api/client-authorizations/inspect", json("POST", { user_code: "ABCD2345" }))).status).toBe(404);
    expect((await owner("/api/client-authorizations/inspect", json("POST", { user_code: "ABCD2345" }))).status).toBe(200);
    const approved = await owner("/api/client-authorizations/approve", json("POST", { user_code: "ABCD2345", workspace_ids: [] }));
    expect(approved.status).toBe(201);
    const client = await approved.json() as { id: string };
    resetMockState({ restoreAssistantState: true });
    expect((await (await owner("/api/clients")).json()).items).toHaveLength(1);
    expect((await (await other("/api/clients")).json()).items).toHaveLength(0);
    expect((await other(`/api/clients/${client.id}`, { method: "DELETE" })).status).toBe(404);
    expect((await owner(`/api/clients/${client.id}`, { method: "DELETE" })).status).toBe(204);
    expect((await (await owner("/api/clients")).json()).items[0].revoked).toBe(true);
    expect(JSON.stringify(await (await owner("/api/admin/audit")).json())).not.toContain("ABCD2345");
  });

  it("rejects unauthenticated protected requests without creating resources", async () => {
    const createHost = await createHostRequest({ name: "unauthenticated", template_id: "std-2c4g", workspace_ids: [] });
    const createKey = await api("/api/apikeys", json("POST", { name: "unauthenticated" }));
    expect(createHost.status).toBe(401);
    expect(createKey.status).toBe(401);
    expect((await api("/api/me")).status).toBe(401);
    expect((await api("/api/hosts")).status).toBe(401);
    expect((await api("/api/templates")).status).toBe(401);
    expect((await api("/api/admin/users", json("POST", { name: "unauthenticated", password: "secret" }))).status).toBe(401);

    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    expect((await (await api("/api/hosts")).json()).items).toEqual([]);
    expect((await (await api("/api/apikeys")).json()).items).toEqual([]);
  });

  it("requires and replays an exact host creation idempotency key", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const body = { name: "retry host", template_id: "std-2c4g", workspace_ids: [] };
    expect((await api("/api/hosts", json("POST", body))).status).toBe(400);

    const first = await createHostRequest(body, "host-retry-key-001");
    const second = await createHostRequest(body, "host-retry-key-001");
    expect(first.status).toBe(201);
    expect(second.status).toBe(201);
    expect(await first.json()).toMatchObject({ id: (await second.json() as { id: string }).id });

    const changed = await createHostRequest({ ...body, name: "different" }, "host-retry-key-001");
    expect(changed.status).toBe(409);
    expect((await changed.json()).error.code).toBe("idempotency_conflict");
    expect((await (await api("/api/hosts")).json()).items).toHaveLength(1);
  });

  it("keeps identities isolated by cookie and logout revokes only that session", async () => {
    const admin = createApiClient();
    const member = createApiClient();
    const adminLogin = await admin("/api/auth/login", json("POST", { name: "admin", password: "admin" }));
    const memberLogin = await member("/api/auth/login", json("POST", { name: "member", password: "member" }));
    const adminCookie = adminLogin.headers.get("set-cookie") ?? "";
    const memberCookie = memberLogin.headers.get("set-cookie") ?? "";
    expect(adminCookie).toMatch(/^ch_mock_session=.+; Path=\/; SameSite=Lax$/);
    expect(memberCookie).not.toBe(adminCookie);

    expect(await (await admin("/api/me")).json()).toMatchObject({ name: "admin", role: "admin" });
    expect(await (await member("/api/me")).json()).toMatchObject({ name: "member", role: "user" });
    expect((await member("/api/admin/users")).status).toBe(403);

    const logout = await admin("/api/auth/logout", { method: "POST" });
    expect(logout.headers.get("set-cookie")).toContain("Max-Age=0");
    expect((await admin("/api/me")).status).toBe(401);
    expect(await (await member("/api/me")).json()).toMatchObject({ name: "member", role: "user" });
  });
});

describe("portal mock Host sharing", () => {
  it("grants selected scopes, issues observation tickets, and revokes access", async () => {
    const owner = createApiClient();
    const member = createApiClient();
    await owner("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const created = await owner("/api/hosts", {
      method: "POST", headers: { "content-type": "application/json", "Idempotency-Key": "share-host-key-001" },
      body: JSON.stringify({ name: "共享测试电脑", template_id: "std-2c4g", workspace_ids: [] }),
    });
    const host = await created.json() as { id: string };
    await wait(950);

    const unsupported = await owner(`/api/hosts/${host.id}/members`, json("POST", { user_id: "umember1", permissions: ["workspace:sync"] }));
    expect(unsupported.status).toBe(400);
    const invited = await owner(`/api/hosts/${host.id}/members`, json("POST", { user_id: "umember1", permissions: ["desktop:control", "terminal:control", "mcp:control"] }));
    expect(invited.status).toBe(201);
    expect(await invited.json()).toMatchObject({ user_id: "umember1", permissions: ["desktop:view", "desktop:control", "terminal:control", "mcp:control"] });
    expect(await (await owner(`/api/hosts/${host.id}/members`)).json()).toMatchObject({ items: [{ user_id: "umember1" }] });

    await member("/api/auth/login", json("POST", { name: "member", password: "member" }));
    expect(await (await member("/api/shared-hosts")).json()).toMatchObject({ items: [{ id: host.id, url: expect.stringContaining("hosts.mock.invalid"), permissions: ["desktop:view", "desktop:control", "terminal:control", "mcp:control"] }] });
    expect((await member(`/api/hosts/${host.id}`)).status).toBe(404);
    expect((await member(`/api/hosts/${host.id}/members`)).status).toBe(404);
    expect(await (await member(`/api/hosts/${host.id}/observation-ticket`, { method: "POST" })).json()).toMatchObject({ url: expect.stringContaining("/__ch/auth?t=") });
    const terminalTicket = await member(`/api/hosts/${host.id}/observation-ticket?target=terminal`, { method: "POST" });
    expect(await terminalTicket.json()).toMatchObject({ url: expect.stringContaining("hosts.mock.invalid/__ch/auth?t=") });
    expect((await member(`/api/hosts/${host.id}/observation-ticket?target=terminal&target=desktop`, { method: "POST" })).status).toBe(400);
    const sharedMCPKey = await member("/api/apikeys", json("POST", { name: "共享主机 MCP", host_ids: [host.id], scopes: ["computer:operate"] }));
    expect(sharedMCPKey.status).toBe(201);

    expect((await owner(`/api/hosts/${host.id}/members/umember1`, { method: "DELETE" })).status).toBe(204);
    expect(await (await member("/api/shared-hosts")).json()).toEqual({ items: [] });
    expect((await member(`/api/hosts/${host.id}/observation-ticket`, { method: "POST" })).status).toBe(404);
  });

  it("gives a terminal-only member a terminal ticket without desktop access", async () => {
    const owner = createApiClient();
    const member = createApiClient();
    await owner("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const created = await owner("/api/hosts", {
      method: "POST", headers: { "content-type": "application/json", "Idempotency-Key": "terminal-share-key-001" },
      body: JSON.stringify({ name: "终端专用共享", template_id: "std-2c4g", workspace_ids: [] }),
    });
    const host = await created.json() as { id: string };
    await wait(950);
    expect((await owner(`/api/hosts/${host.id}/members`, json("POST", { user_id: "umember1", permissions: ["terminal:control"] }))).status).toBe(201);
    await member("/api/auth/login", json("POST", { name: "member", password: "member" }));

    const terminal = await member(`/api/hosts/${host.id}/observation-ticket?target=terminal`, { method: "POST" });
    expect(terminal.status).toBe(200);
    expect(await terminal.json()).toMatchObject({ url: expect.stringContaining("next=%2F__ch%2Fterminal%2F") });
    expect((await member(`/api/hosts/${host.id}/observation-ticket`, { method: "POST" })).status).toBe(404);
    expect((await member("/api/apikeys", json("POST", { name: "无 MCP 权限", host_ids: [host.id], scopes: ["computer:operate"] }))).status).toBe(404);
  });
});

describe("portal mock admin workspaces", () => {
  it("returns workspace metadata only to admins", async () => {
    const admin = createApiClient();
    const member = createApiClient();
    await admin("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const created = await admin("/api/workspaces", {
      ...json("POST", { name: "管理员可见工作区" }),
      headers: { "content-type": "application/json", "Idempotency-Key": "admin-workspace-create-01" },
    });
    const workspace = await created.json() as { id: string };

    const listed = await admin("/api/admin/workspaces");
    expect(listed.status).toBe(200);
    expect(await listed.json()).toMatchObject({ items: [{ id: workspace.id, owner_id: "u1a2b3c4", owner_name: "demo", name: "管理员可见工作区", bindings: [] }], next_cursor: null });
    const detail = await admin(`/api/admin/workspaces/${workspace.id}`);
    expect(detail.status).toBe(200);
    const detailText = await detail.text();
    expect(detailText).toContain('"name":"管理员可见工作区"');
    expect(detailText).not.toContain("directory");

    await member("/api/auth/login", json("POST", { name: "member", password: "member" }));
    expect((await member("/api/admin/workspaces")).status).toBe(403);
    expect((await member(`/api/admin/workspaces/${workspace.id}`)).status).toBe(403);
    expect((await admin("/api/admin/workspaces/wmissing1")).status).toBe(404);
  });
});

describe("portal mock conversation messages", () => {
  it("pages messages from newest to older without duplicating the boundary message", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));

    const firstResponse = await api("/api/conversations/cdemo001/messages?limit=2");
    expect(firstResponse.status).toBe(200);
    const first = await firstResponse.json() as { items: Array<{ id: string }>; next_cursor: string | null };
    expect(first.items.map((message) => message.id)).toEqual(["mdemo003", "mdemo004"]);
    expect(first.next_cursor).toBeTruthy();

    const secondResponse = await api(`/api/conversations/cdemo001/messages?limit=2&cursor=${encodeURIComponent(first.next_cursor!)}`);
    const second = await secondResponse.json() as { items: Array<{ id: string }>; next_cursor: string | null };
    expect(second.items.map((message) => message.id)).toEqual(["mdemo001", "mdemo002"]);
    expect(second.next_cursor).toBeNull();
    expect(new Set([...first.items, ...second.items].map((message) => message.id)).size).toBe(4);

    expect((await api("/api/conversations/cdemo001/messages?cursor=invalid")).status).toBe(400);
  });
});

describe("portal mock tasks", () => {
  it("confirms task pause and cancellation asynchronously and persists the confirmed state", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const pauseHeaders = { "content-type": "application/json", "Idempotency-Key": "task-pause-001" };
    const pause = await api("/api/tasks/qdemo001/actions", { ...json("POST", { action: "pause", expected_status: "running" }), headers: pauseHeaders });
    expect(await pause.json()).toMatchObject({ status: "pausing" });
    await wait(1050);
    expect(await (await api("/api/tasks/qdemo001")).json()).toMatchObject({ status: "paused", stop_request: null });

    resetMockState({ restoreAssistantState: true });
    api = createApiClient();
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    expect(await (await api("/api/tasks/qdemo001")).json()).toMatchObject({ status: "paused" });

    const resumeHeaders = { "content-type": "application/json", "Idempotency-Key": "task-resume-001" };
    const resume = await api("/api/tasks/qdemo001/actions", { ...json("POST", { action: "resume", expected_status: "paused" }), headers: resumeHeaders });
    expect(await resume.json()).toMatchObject({ status: "queued" });
    const cancelHeaders = { "content-type": "application/json", "Idempotency-Key": "task-cancel-001" };
    const cancel = await api("/api/tasks/qdemo001/actions", { ...json("POST", { action: "cancel", expected_status: "queued" }), headers: cancelHeaders });
    expect(await cancel.json()).toMatchObject({ status: "cancelling" });
    await wait(1050);
    expect(await (await api("/api/tasks/qdemo001")).json()).toMatchObject({ status: "cancelled", stop_request: null });
  });

  it("exposes task progress and applies only an exact, idempotent approval response", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const list = await api("/api/tasks");
    expect(list.status).toBe(200);
    expect((await list.json()).items).toEqual(expect.arrayContaining([
      expect.objectContaining({ id: "qdemo001", status: "running" }),
      expect.objectContaining({ id: "qreview1", status: "waiting_user" }),
      expect.objectContaining({ id: "qclarif1", status: "waiting_user" }),
    ]));

    const detail = await api("/api/tasks/qreview1");
    expect(await detail.json()).toMatchObject({
      id: "qreview1", waiting_reason: "approval_required",
      pending_request: { id: "rapprove", version: 1, kind: "approval" },
    });

    const body = { action: "respond", pending_id: "rapprove", pending_version: 1, decision: "approve" };
    const missingKey = await api("/api/tasks/qreview1/actions", json("POST", body));
    expect(missingKey.status).toBe(400);

    const headers = { "content-type": "application/json", "Idempotency-Key": "approval-key-001" };
    const response = await api("/api/tasks/qreview1/actions", { ...json("POST", body), headers });
    expect(response.status).toBe(202);
    expect(await response.json()).toMatchObject({ task_id: "qreview1", action: "respond", status: "queued" });

    const replay = await api("/api/tasks/qreview1/actions", { ...json("POST", body), headers });
    expect(replay.status).toBe(202);
    expect(await replay.json()).toMatchObject({ task_id: "qreview1", status: "queued" });
    const reusedKey = await api("/api/tasks/qreview1/actions", { ...json("POST", { ...body, decision: "reject" }), headers });
    expect(reusedKey.status).toBe(409);

    expect(await (await api("/api/tasks/qreview1")).json()).toMatchObject({ status: "queued", waiting_reason: null, pending_request: null });
    const staleApproval = await api("/api/tasks/qreview1/actions", { ...json("POST", body), headers: { "content-type": "application/json", "Idempotency-Key": "approval-key-002" } });
    expect(staleApproval.status).toBe(409);

    const clarification = await api("/api/tasks/qclarif1");
    expect(await clarification.json()).toMatchObject({
      id: "qclarif1", waiting_reason: "clarification_required",
      clarification_request: { id: "rclarify", version: 1, question: expect.any(String) },
      pending_request: null,
    });
    const invalidAnswer = await api("/api/tasks/qclarif1/actions", { ...json("POST", { action: "answer_clarification", clarification_id: "rclarify", clarification_version: 1, answer: "   " }), headers: { "content-type": "application/json", "Idempotency-Key": "clarify-key-001" } });
    expect(invalidAnswer.status).toBe(400);
    const answer = await api("/api/tasks/qclarif1/actions", { ...json("POST", { action: "answer_clarification", clarification_id: "rclarify", clarification_version: 1, answer: "周一到周五，并包括风险和计划。" }), headers: { "content-type": "application/json", "Idempotency-Key": "clarify-key-002" } });
    expect(answer.status).toBe(202);
    expect(await answer.json()).toMatchObject({ task_id: "qclarif1", action: "answer_clarification", status: "queued" });
    expect(await (await api("/api/tasks/qclarif1")).json()).toMatchObject({ status: "queued", waiting_reason: null, clarification_request: null });
  });
});

describe("portal mock Assistant flow", () => {
  it("persists a user task as a conversation and supports an exact idempotent retry", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    expect((await (await api("/api/model-connections")).json()).items).toEqual(expect.arrayContaining([
      expect.objectContaining({ id: "pmodel01", shared: true, capabilities: { text: true, tools: true, vision: false } }),
    ]));
    const createHost = await createHostRequest({ name: "Assistant 测试电脑", template_id: "std-2c4g", workspace_ids: [] });
    const host = await createHost.json();
    await wait(950);
    const body = { host_id: host.id, model_id: "pmodel01", objective: "整理测试目录并生成摘要" };
    const headers = { "content-type": "application/json", "Idempotency-Key": "assistant-submit-001" };
    const first = await api("/api/tasks", { ...json("POST", body), headers });
    expect(first.status).toBe(202);
    const task = await first.json();
    const replay = await api("/api/tasks", { ...json("POST", body), headers });
    expect(replay.status).toBe(200);
    expect(await replay.json()).toMatchObject({ id: task.id, conversation_id: task.conversation_id });

    const conversationList = await api("/api/conversations");
    expect(await conversationList.json()).toMatchObject({ items: expect.arrayContaining([expect.objectContaining({ id: task.conversation_id, host_id: host.id })]) });
    const history = await api(`/api/conversations/${task.conversation_id}/messages`);
    expect(await history.json()).toMatchObject({ items: [expect.objectContaining({ content: body.objective, task_id: task.id, role: "user" })] });

    const nextBody = { host_id: host.id, model_id: "pmodel01", content: "同时列出需要我确认的风险" };
    const next = await api(`/api/conversations/${task.conversation_id}/messages`, { ...json("POST", nextBody), headers: { ...headers, "Idempotency-Key": "assistant-followup-001" } });
    expect(next.status).toBe(202);
    expect(await next.json()).toMatchObject({ message: { content: nextBody.content }, task: { conversation_id: task.conversation_id, objective: nextBody.content } });
    expect((await (await api("/api/tasks")).json()).items).toEqual(expect.arrayContaining([
      expect.objectContaining({ id: task.id, conversation_id: task.conversation_id }),
    ]));
  });

  it("never returns a personal model API key in the model connection response", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const secret = "test-only-not-a-real-provider-secret";
    const response = await api("/api/model-connections", json("POST", { name: "本地合成模型", base_url: "https://models.example.test/v1", model_name: "mock-model", api_key: secret, shared: false }));
    expect(response.status).toBe(201);
    const created = await response.json();
    expect(created).toMatchObject({ name: "本地合成模型", has_api_key: true, shared: false });
    expect(JSON.stringify(created)).not.toContain(secret);
  });
});

describe("portal mock schedules and notifications", () => {
  it("persists a versioned schedule, keeps cancellation pending until the task confirms, and marks notifications read only", async () => {
    const admin = createApiClient();
    const member = createApiClient();
    await admin("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    await member("/api/auth/login", json("POST", { name: "member", password: "member" }));

    const hostResponse = await admin("/api/hosts", { ...(json("POST", { name: "Schedule 测试电脑", template_id: "std-2c4g", workspace_ids: [] })), headers: { "content-type": "application/json", "Idempotency-Key": `host-test-${++hostSequence}` } });
    const host = await hostResponse.json() as { id: string };
    await wait(950);
    const body = {
      host_id: host.id, model_id: "pmodel01", objective: "定时汇总新文件",
      timing: { kind: "once", run_at: new Date(Date.now() + 5 * 60_000).toISOString(), time_zone: "Asia/Shanghai" },
      budget: { active_seconds: 900, tool_calls: 40, model_tokens: 20000 },
      misfire_policy: "run_once", notification_events: ["failed", "waiting_user"],
    };
    const createHeaders = { "content-type": "application/json", "Idempotency-Key": "schedule-create-001" };
    const created = await admin("/api/schedules", { ...json("POST", body), headers: createHeaders });
    expect(created.status).toBe(201);
    const schedule = await created.json() as { id: string; version: number };
    const replay = await admin("/api/schedules", { ...json("POST", body), headers: createHeaders });
    expect(replay.status).toBe(201);
    expect(await replay.json()).toMatchObject({ id: schedule.id });
    expect((await (await admin(`/api/schedules/${schedule.id}`)).json())).toMatchObject({ status: "enabled", version: 1, objective: body.objective, misfire_policy: "run_once" });

    const updated = await admin(`/api/schedules/${schedule.id}`, json("PATCH", { expected_version: 1, objective: "定时整理新文件" }));
    expect(await updated.json()).toMatchObject({ version: 2, objective: "定时整理新文件" });
    const stale = await admin(`/api/schedules/${schedule.id}`, json("PATCH", { expected_version: 1, objective: "覆盖旧版本" }));
    expect(stale.status).toBe(409);
    expect((await (await admin(`/api/schedules/${schedule.id}/runs`)).json()).items).toEqual([]);
    expect((await member(`/api/schedules/${schedule.id}`)).status).toBe(404);

    const disable = await admin("/api/schedules/jdemo001/actions", { ...json("POST", { action: "disable", expected_version: 1, current_run_action: "cancel" }), headers: { "content-type": "application/json", "Idempotency-Key": "schedule-cancel-001" } });
    expect(disable.status).toBe(202);
    expect(await disable.json()).toMatchObject({ schedule: { status: "disabled" }, current_run: { status: "stopping", task_id: "qdemo001" } });
    expect(await (await admin("/api/tasks/qdemo001")).json()).toMatchObject({ status: "cancelling" });
    await wait(1050);
    expect(await (await admin("/api/tasks/qdemo001")).json()).toMatchObject({ status: "cancelled" });
    expect(await (await admin("/api/schedules/jdemo001")).json()).toMatchObject({ last_run: { status: "cancelled" } });

    const markRead = await admin("/api/notifications/ndemo001/read", { method: "POST" });
    expect(markRead.status).toBe(204);
    await admin("/api/notifications/ndemo001/read", { method: "POST" });
    expect(await (await admin("/api/notifications")).json()).toMatchObject({ items: expect.arrayContaining([expect.objectContaining({ id: "ndemo001", read_at: expect.any(String), actionable: true })]) });
    expect(await (await admin("/api/tasks/qreview1")).json()).toMatchObject({ status: "waiting_user", pending_request: { id: "rapprove" } });
    expect((await member("/api/notifications/ndemo001/read", { method: "POST" })).status).toBe(404);
  });
});

describe("portal mock owner execution gate", () => {
  it("stops owner tasks durably, keeps owners isolated, and does not revive cancelled work", async () => {
    const admin = createApiClient();
    const member = createApiClient();
    await admin("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    await member("/api/auth/login", json("POST", { name: "member", password: "member" }));

    expect(await (await admin("/api/execution")).json()).toMatchObject({ state: "enabled", epoch: 1, pending_task_count: 0 });
    const key = { "content-type": "application/json", "Idempotency-Key": "mock-execution-stop-01" };
    const stop = await admin("/api/execution/stop-all", { ...json("POST", { expected_epoch: 1 }), headers: key });
    expect(stop.status).toBe(200);
    expect(await stop.json()).toMatchObject({ state: "stopping", epoch: 2, pending_task_count: 3 });
    expect(await (await member("/api/execution")).json()).toMatchObject({ state: "enabled", epoch: 1, pending_task_count: 0 });
    expect((await admin("/api/execution/enable", { ...json("POST", { expected_epoch: 2 }), headers: { ...key, "Idempotency-Key": "mock-execution-enable-01" } })).status).toBe(409);

    const stoppingTasks = await (await admin("/api/tasks")).json() as { items: Array<{ id: string; status: string }> };
    expect(stoppingTasks.items.filter((item) => item.status === "cancelling")).toHaveLength(3);
    await wait(3100);
    expect(await (await admin("/api/execution")).json()).toMatchObject({ state: "disabled", epoch: 3, pending_task_count: 0 });

    const enable = await admin("/api/execution/enable", { ...json("POST", { expected_epoch: 3 }), headers: { ...key, "Idempotency-Key": "mock-execution-enable-02" } });
    expect(enable.status).toBe(200);
    expect(await enable.json()).toMatchObject({ state: "enabled", epoch: 4 });
    const tasksAfterEnable = await (await admin("/api/tasks")).json() as { items: Array<{ id: string; status: string }> };
    expect(tasksAfterEnable.items.filter((item) => item.status === "cancelled")).toHaveLength(3);
  });
});

describe("portal mock quota and capacity", () => {
  it("derives usage and allocation from hosts and rejects quota and cluster overages", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const initialCapacity = await (await api("/api/admin/capacity")).json();
    expect(initialCapacity.allocated).toEqual({ cpu_milli: 0, mem_mib: 0 });

    await api("/api/admin/users/u1a2b3c4", json("PATCH", { quota: { hosts: 1, cpu_milli: 500, mem_mib: 1536 } }));
    const created = await createHostRequest({ name: "quota test", template_id: "std-2c4g", workspace_ids: [] });
    expect(created.status).toBe(201);
    const me = await (await api("/api/me")).json();
    expect(me.usage).toEqual({ hosts: 1, cpu_milli: 500, mem_mib: 1536 });
    const creatingCapacity = await (await api("/api/admin/capacity")).json();
    expect(creatingCapacity.allocated).toEqual({ cpu_milli: 500, mem_mib: 1536 });
    expect(creatingCapacity.hosts_by_state).toEqual({ creating: 1 });

    const quotaRejected = await createHostRequest({ name: "over quota", template_id: "std-2c4g", workspace_ids: [] });
    expect(quotaRejected.status).toBe(409);
    expect((await quotaRejected.json()).error).toMatchObject({ code: "quota_exceeded", details: { dimension: "hosts", limit: 1 } });

    await api("/api/admin/users/u1a2b3c4", json("PATCH", { quota: { hosts: 100, cpu_milli: 50000, mem_mib: 200000 } }));
    for (let index = 1; index < 64; index += 1) {
      const response = await createHostRequest({ name: `capacity test ${index}`, template_id: "std-2c4g", workspace_ids: [] });
      expect(response.status).toBe(201);
    }
    const capacityRejected = await createHostRequest({ name: "over capacity", template_id: "std-2c4g", workspace_ids: [] });
    expect(capacityRejected.status).toBe(409);
    expect((await capacityRejected.json()).error).toMatchObject({ code: "capacity_exceeded", details: { dimension: "mem_mib", limit: 98304 } });
  });

  it("stores admin-created user credentials and blocks disabled accounts", async () => {
    await api("/api/auth/login", json("POST", { name: "admin", password: "admin" }));
    const created = await api("/api/admin/users", json("POST", { name: "local-new-user", password: "local-password", role: "user" }));
    expect(created.status).toBe(201);
    const user = await created.json();
    expect(user).toMatchObject({ name: "local-new-user", role: "user", disabled: false });
    expect(user).not.toHaveProperty("password");

    await api("/api/auth/logout", { method: "POST" });
    expect((await api("/api/auth/login", json("POST", { name: "local-new-user", password: "local-password" }))).status).toBe(200);
    await api("/api/auth/login", json("POST", { name: "admin", password: "admin" }));
    expect((await api(`/api/admin/users/${user.id}`, json("PATCH", { disabled: true }))).status).toBe(200);
    await api("/api/auth/logout", { method: "POST" });
    expect((await api("/api/auth/login", json("POST", { name: "local-new-user", password: "local-password" }))).status).toBe(401);
  });

  it("filters synthetic audit records and applies the requested result limit", async () => {
    await api("/api/auth/login", json("POST", { name: "admin", password: "admin" }));
    const since = encodeURIComponent(new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString());
    const filtered = await api(`/api/admin/audit?actor=u1a2b3c4&action=host.create&target=h-audit-demo&since=${since}&limit=1`);
    expect(filtered.status).toBe(200);
    expect((await filtered.json()).items).toEqual([expect.objectContaining({ action: "host.create", actor_id: "u1a2b3c4", target: "h-audit-demo" })]);

    const limited = await api("/api/admin/audit?limit=2");
    expect((await limited.json()).items).toHaveLength(2);
  });

  it("hides another user's host, workspace, and API Key by ID", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const workspaceResponse = await createWorkspace("private workspace");
    const workspace = await workspaceResponse.json();
    const hostResponse = await createHostRequest({ name: "private host", template_id: "std-2c4g", workspace_ids: [workspace.id] });
    const host = await hostResponse.json();
    const keyResponse = await api("/api/apikeys", json("POST", { name: "owner key", host_ids: [], scopes: [] }));
    const key = await keyResponse.json();
    expect(hostResponse.status).toBe(201);
    expect(keyResponse.status).toBe(201);

    await api("/api/auth/login", json("POST", { name: "member", password: "member" }));
    expect((await (await api("/api/hosts")).json()).items).toEqual([]);
    expect((await (await api("/api/workspaces")).json()).items).toEqual([]);
    expect((await (await api("/api/apikeys")).json()).items).toEqual([]);
    for (const [path, init] of [
      [`/api/hosts/${host.id}`, undefined],
      [`/api/hosts/${host.id}`, { method: "DELETE" }],
      [`/api/hosts/${host.id}/renew`, { method: "POST" }],
      [`/api/hosts/${host.id}/enter`, { method: "POST" }],
      [`/api/workspaces/${workspace.id}`, { method: "DELETE" }],
      [`/api/apikeys/${key.id}`, { method: "DELETE" }],
    ] as Array<[string, RequestInit | undefined]>) expect((await api(path, init)).status).toBe(404);

    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    expect((await api(`/api/hosts/${host.id}`)).status).toBe(200);
    expect((await (await api("/api/apikeys")).json()).items).toContainEqual(expect.objectContaining({ id: key.id }));
    expect((await api(`/api/apikeys/${key.id}`, { method: "DELETE" })).status).toBe(204);
  });

  it("returns a one-time workbench entry ticket when entering an owned Host", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const created = await createHostRequest({ name: "entry ticket", template_id: "std-2c4g" });
    const host = await created.json() as { id: string; url: string; expires_at: string };

    const response = await api(`/api/hosts/${host.id}/enter`, { method: "POST" });
    const checkedAt = Date.now();
    const entry = await response.json() as { url: string; expires_at: string };
    const url = new URL(entry.url);

    expect(response.status).toBe(200);
    expect(url.origin).toBe(new URL(host.url).origin);
    expect(url.pathname).toBe("/__ch/auth");
    expect(url.searchParams.get("t")).toMatch(/^mock-/);
    expect(Date.parse(entry.expires_at)).toBeGreaterThan(checkedAt);
    expect(Date.parse(entry.expires_at)).toBeLessThanOrEqual(checkedAt + 61_000);
  });

  it("deletes the Host while retaining its independent Workspace, then deletes the Workspace separately", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const workspaceResponse = await createWorkspace("retained workspace");
    const workspace = await workspaceResponse.json();
    const created = await createHostRequest({ name: "retained workspace", template_id: "std-2c4g", workspace_ids: [workspace.id] });
    const host = await created.json();
    const hostId = host.id as string;
    const workspaceId = workspace.id as string;
    const timer = vi.spyOn(globalThis, "setTimeout");
    const deleted = await api(`/api/hosts/${hostId}`, { method: "DELETE" });
    expect(timer).toHaveBeenCalledWith(expect.any(Function), 900);
    timer.mockRestore();
    const deletingHost = await deleted.json();
    expect(deleted.status).toBe(202);
    expect(deletingHost.state).toBe("deleting");
    expect(deletingHost.workspaces[0]).toMatchObject({ workspace: { id: workspaceId, state: "ready" }, state: "active" });
    expect((await (await api(`/api/hosts/${hostId}`)).json()).state).toBe("deleting");
    expect((await (await api("/api/hosts")).json()).items).toContainEqual(expect.objectContaining({ id: hostId, state: "deleting" }));
    expect((await (await api("/api/workspaces")).json()).items).toContainEqual(expect.objectContaining({ id: workspaceId, state: "ready" }));
    expect((await api(`/api/hosts/${hostId}`, { method: "DELETE" })).status).toBe(409);
    expect((await (await api("/api/me")).json()).usage.hosts).toBe(1);

    await wait(950);
    const workspacesAfterNavigation = await (await api("/api/workspaces")).json();
    expect(workspacesAfterNavigation.items).toContainEqual(expect.objectContaining({ id: workspaceId, state: "ready" }));
    expect((await api(`/api/hosts/${hostId}`)).status).toBe(404);
    expect((await (await api("/api/hosts")).json()).items).not.toContainEqual(expect.objectContaining({ id: hostId }));
    expect((await (await api("/api/me")).json()).usage.hosts).toBe(0);

    const workspaceTimer = vi.spyOn(globalThis, "setTimeout");
    const deletedWorkspace = await api(`/api/workspaces/${workspaceId}`, { method: "DELETE" });
    expect(workspaceTimer).toHaveBeenCalledWith(expect.any(Function), 900);
    workspaceTimer.mockRestore();
    expect(deletedWorkspace.status).toBe(202);
    expect((await deletedWorkspace.json()).state).toBe("deleting");
    expect((await (await api("/api/workspaces")).json()).items).toContainEqual(expect.objectContaining({ id: workspaceId, state: "deleting" }));
    expect((await api(`/api/workspaces/${workspaceId}`, { method: "DELETE" })).status).toBe(409);
    await wait(950);
    expect((await (await api("/api/workspaces")).json()).items).not.toContainEqual(expect.objectContaining({ id: workspaceId }));
  });

  it("resumes a pending Workspace deletion after mock state is restored", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const workspace = await (await createWorkspace("resumed deletion")).json() as { id: string };
    const deleted = await api(`/api/workspaces/${workspace.id}`, { method: "DELETE" });
    expect((await deleted.json()).state).toBe("deleting");

    resetMockState({ restoreAssistantState: true });
    api = createApiClient();
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    expect((await (await api("/api/workspaces")).json()).items).toContainEqual(expect.objectContaining({ id: workspace.id, state: "deleting" }));

    await wait(950);
    expect((await (await api("/api/workspaces")).json()).items).not.toContainEqual(expect.objectContaining({ id: workspace.id }));
    resetMockState();
  });

  it("keeps a Host allocated while deletion finishes when no Workspace is bound", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const created = await createHostRequest({ name: "deletion accounting", template_id: "std-2c4g", workspace_ids: [] });
    expect(created.status).toBe(201);
    const host = await created.json();
    const before = await (await api("/api/me")).json();

    const deleted = await api(`/api/hosts/${host.id}`, { method: "DELETE" });
    const deletingHost = await deleted.json();
    expect(deleted.status).toBe(202);
    expect(deletingHost.state).toBe("deleting");
    expect(deletingHost.workspaces).toEqual([]);
    expect((await (await api(`/api/hosts/${host.id}`)).json()).state).toBe("deleting");
    expect((await (await api("/api/workspaces")).json()).items).toEqual([]);
    expect((await (await api("/api/me")).json()).usage.hosts).toBe(before.usage.hosts);

    await wait(950);
    expect((await api(`/api/hosts/${host.id}`)).status).toBe(404);
    expect((await (await api("/api/me")).json()).usage.hosts).toBe(before.usage.hosts - 1);
  });

  it("releases compute and restores the same Host with the same workspace and a new generation", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const workspaceResponse = await createWorkspace("persistent workspace");
    const workspace = await workspaceResponse.json();
    const created = await createHostRequest({ name: "persistent computer", template_id: "std-2c4g", workspace_ids: [workspace.id] });
    expect(created.status).toBe(201);
    const initial = await created.json();
    expect(initial.runtime_generation).toBe(1);

    await wait(950);
    expect((await (await api("/api/hosts")).json()).items[0].state).toBe("running");
    const released = await api(`/api/hosts/${initial.id}/release`, { method: "POST" });
    expect(released.status).toBe(202);
    expect((await released.json()).state).toBe("stopping");
    expect((await (await api("/api/me")).json()).usage.hosts).toBe(1);

    await wait(950);
    const stopped = await (await api(`/api/hosts/${initial.id}`)).json();
    expect(stopped).toMatchObject({ id: initial.id, state: "stopped", runtime_generation: 1 });
    expect(stopped.workspaces[0].workspace.id).toBe(workspace.id);
    expect((await (await api("/api/me")).json()).usage.hosts).toBe(0);

    const restored = await api(`/api/hosts/${initial.id}/restore`, { method: "POST" });
    expect(restored.status).toBe(202);
    expect(await restored.json()).toMatchObject({ id: initial.id, state: "creating", runtime_generation: 2 });
    await wait(950);
    const running = await (await api(`/api/hosts/${initial.id}`)).json();
    expect(running).toMatchObject({ id: initial.id, state: "running", runtime_generation: 2 });
    expect(running.workspaces[0].workspace.id).toBe(workspace.id);
    expect((await (await api("/api/me")).json()).usage.hosts).toBe(1);
  });
});

describe("portal mock workspace bindings", () => {
  it("binds an owned workspace idempotently and exposes only empty owner-scoped device progress", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const workspaceResponse = await createWorkspace("bindable workspace");
    const workspace = await workspaceResponse.json() as { id: string; revision: number };
    const hostResponse = await createHostRequest({ name: "workspace host", template_id: "std-2c4g", workspace_ids: [] });
    const host = await hostResponse.json() as { id: string };
    await wait(950);

    const headers = { "content-type": "application/json", "Idempotency-Key": "workspace-bind-001" };
    const stale = await api(`/api/hosts/${host.id}/workspace-bindings`, { ...json("POST", { workspace_id: workspace.id, expected_revision: workspace.revision + 1, expected_binding_version: 0 }), headers });
    expect(stale.status).toBe(409);
    expect((await stale.json()).error.code).toBe("workspace_conflict");

    const request = { workspace_id: workspace.id, expected_revision: workspace.revision, expected_binding_version: 0 };
    const bound = await api(`/api/hosts/${host.id}/workspace-bindings`, { ...json("POST", request), headers });
    const binding = await bound.json();
    expect(bound.status).toBe(201);
    expect(binding).toMatchObject({ workspace: { id: workspace.id, revision: workspace.revision }, state: "active", desired_revision: workspace.revision, applied_revision: workspace.revision });

    const replay = await api(`/api/hosts/${host.id}/workspace-bindings`, { ...json("POST", request), headers });
    expect(replay.status).toBe(201);
    expect(await replay.json()).toEqual(binding);
    const hostAfterBinding = await (await api(`/api/hosts/${host.id}`)).json();
    expect(hostAfterBinding.workspaces).toHaveLength(1);

    const staleRetry = await api(`/api/hosts/${host.id}/workspace-bindings`, { ...json("POST", { ...request, expected_revision: workspace.revision }), headers: { ...headers, "Idempotency-Key": "workspace-bind-002" } });
    expect(staleRetry.status).toBe(409);
    expect((await staleRetry.json()).error.code).toBe("workspace_conflict");

    const progress = await api(`/api/workspaces/${workspace.id}/sync-progress`);
    expect(await progress.json()).toEqual({ items: [] });
    const changedReplay = await api(`/api/hosts/${host.id}/workspace-bindings`, { ...json("POST", { workspace_id: "w-other", expected_revision: 0, expected_binding_version: 0 }), headers });
    expect(changedReplay.status).toBe(409);
    expect((await changedReplay.json()).error.code).toBe("idempotency_conflict");

    await api("/api/auth/login", json("POST", { name: "member", password: "member" }));
    expect((await api(`/api/workspaces/${workspace.id}/sync-progress`)).status).toBe(404);
  });

  it("accepts unbinding only with current version and writer confirmation while retaining workspace data", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const workspaceResponse = await createWorkspace("retained workspace");
    const workspace = await workspaceResponse.json() as { id: string; revision: number; state: string };
    const hostResponse = await createHostRequest({ name: "unbind host", template_id: "std-2c4g", workspace_ids: [] });
    const host = await hostResponse.json() as { id: string };
    await wait(950);
    const bindHeaders = { "content-type": "application/json", "Idempotency-Key": "workspace-unbind-bind-01" };
    const bindBody = { workspace_id: workspace.id, expected_revision: workspace.revision, expected_binding_version: 0 };
    await api(`/api/hosts/${host.id}/workspace-bindings`, { ...json("POST", bindBody), headers: bindHeaders });

    const path = `/api/hosts/${host.id}/workspace-bindings/${workspace.id}`;
    const unbindHeaders = { "content-type": "application/json", "Idempotency-Key": "workspace-unbind-001" };
    const missingConfirmation = await api(path, { ...json("DELETE", { expected_binding_version: 1, writers_stopped: false }), headers: unbindHeaders });
    expect(missingConfirmation.status).toBe(400);
    const staleVersion = await api(path, { ...json("DELETE", { expected_binding_version: 2, writers_stopped: true }), headers: unbindHeaders });
    expect(staleVersion.status).toBe(409);
    expect((await staleVersion.json()).error.code).toBe("workspace_conflict");

    const request = { expected_binding_version: 1, writers_stopped: true };
    const unbound = await api(path, { ...json("DELETE", request), headers: unbindHeaders });
    expect(unbound.status).toBe(202);
    const accepted = await unbound.json();
    expect(accepted).toMatchObject({ state: "unbinding", binding_version: 2, workspace: { id: workspace.id } });
    const replay = await api(path, { ...json("DELETE", request), headers: unbindHeaders });
    expect(replay.status).toBe(202);
    expect(await replay.json()).toEqual(accepted);
    expect((await (await api(`/api/hosts/${host.id}`)).json()).workspaces[0].state).toBe("unbinding");
    expect(await (await api(`/api/workspaces`)).json()).toMatchObject({ items: [{ id: workspace.id, state: "ready" }] });
  });

  it("retries an owner Host sync with compare-and-set semantics and idempotent healthy replay", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const workspaceResponse = await createWorkspace("sync conflict workspace");
    const workspace = await workspaceResponse.json() as { id: string; revision: number };
    const hostResponse = await createHostRequest({ name: "sync conflict host", template_id: "std-2c4g", workspace_ids: [] });
    const host = await hostResponse.json() as { id: string };
    await wait(950);
    const bindHeaders = { "content-type": "application/json", "Idempotency-Key": "workspace-sync-bind-01" };
    const boundResponse = await api(`/api/hosts/${host.id}/workspace-bindings`, {
      ...json("POST", { workspace_id: workspace.id, expected_revision: workspace.revision, expected_binding_version: 0 }), headers: bindHeaders,
    });
    expect(boundResponse.status).toBe(201);
    const bound = await boundResponse.json() as { binding_version: number };
    const conflicts = ["src/main.go"];
    setMockWorkspaceSyncConflict(host.id, workspace.id, conflicts);
    expect(await (await api(`/api/hosts/${host.id}`)).json()).toMatchObject({ workspaces: [{ sync_state: "conflict", sync_conflicts: conflicts }] });

    const path = `/api/hosts/${host.id}/workspace-bindings/${workspace.id}/retry-sync`;
    const stale = await api(path, json("POST", { expected_binding_version: bound.binding_version + 1 }));
    expect(stale.status).toBe(409);
    expect((await stale.json()).error.code).toBe("workspace_conflict");
    expect((await (await api(`/api/hosts/${host.id}`)).json()).workspaces[0].sync_state).toBe("conflict");

    const request = json("POST", { expected_binding_version: bound.binding_version });
    const retried = await api(path, request);
    expect(retried.status).toBe(200);
    const healthy = await retried.json();
    expect(healthy).toMatchObject({ sync_state: "healthy", sync_conflicts: [] });
    const replay = await api(path, request);
    expect(replay.status).toBe(200);
    expect(await replay.json()).toEqual(healthy);
    const staleReplay = await api(path, json("POST", { expected_binding_version: bound.binding_version + 1 }));
    expect(staleReplay.status).toBe(409);
    expect((await staleReplay.json()).error.code).toBe("workspace_conflict");
    expect((await (await api(`/api/hosts/${host.id}`)).json()).workspaces[0].sync_state).toBe("healthy");

    await api("/api/auth/login", json("POST", { name: "member", password: "member" }));
    expect((await api(path, request)).status).toBe(404);
  });
});
