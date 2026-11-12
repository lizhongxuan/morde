import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { createHash } from "node:crypto";
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
const waitForHostState = async (hostID: string, expected: string) => {
  for (let attempt = 0; attempt < 30; attempt += 1) {
    const response = await api(`/api/hosts/${hostID}`);
    if (response.ok && (await response.json() as { state?: string }).state === expected) return;
    await wait(100);
  }
  throw new Error(`Host ${hostID} did not reach ${expected}`);
};
let workspaceSequence = 0;
let hostSequence = 0;
const createWorkspace = (name: string) => api("/api/workspaces", {
  ...json("POST", { name }), headers: { "content-type": "application/json", "Idempotency-Key": `workspace-test-${++workspaceSequence}` },
});
const createHostRequest = (body: unknown, key = `host-test-${++hostSequence}`) => api("/api/hosts", {
  method: "POST", headers: { "content-type": "application/json", "Idempotency-Key": key }, body: JSON.stringify(body),
});
const allowHosts = async (count: number, client: typeof api = api, cpuMilli = 4000, memMiB = 8192) => {
  const updated = await client("/api/admin/users/u1a2b3c4", json("PATCH", {
    expected_status: "approved", quota: { hosts: count, cpu_milli: cpuMilli, mem_mib: memMiB },
  }));
  expect(updated.status).toBe(200);
};

beforeAll(() => server.listen());
beforeEach(() => { resetMockState(); api = createApiClient(); workspaceSequence = 0; hostSequence = 0; });
afterEach(() => { server.resetHandlers(); vi.restoreAllMocks(); vi.unstubAllEnvs(); });
afterAll(() => server.close());

describe("local MCP connector mock routes", () => {
  it("owner-scopes connectors, checks revisions, and never returns submitted credentials", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const credential = { kind: "headers", headers: { Authorization: "Bearer mock-only-secret" } };
    const createdResponse = await api("/api/mcp/connectors", json("POST", {
      name: "日历工具", endpoint_url: "https://mcp.example.test/mcp", credential,
    }));
    expect(createdResponse.status).toBe(201);
    const created = await createdResponse.json() as Record<string, unknown>;
    expect(created).toMatchObject({ name: "日历工具", enabled: false, has_credential: true, credential_version: 1, revision: 1 });
    expect(JSON.stringify(created)).not.toContain("mock-only-secret");

    const memberAPI = createApiClient();
    await memberAPI("/api/auth/login", json("POST", { name: "member", password: "member" }));
    expect(await (await memberAPI("/api/mcp/connectors")).json()).toEqual({ items: [] });
    const connectorID = String(created.id);
    const enabled = await api(`/api/mcp/connectors/${connectorID}/enabled`, json("PUT", { enabled: true, expected_revision: 1 }));
    expect(enabled.status).toBe(200);
    expect(await enabled.json()).toMatchObject({ enabled: true, revision: 2 });
    const stale = await api(`/api/mcp/connectors/${connectorID}/enabled`, json("PUT", { enabled: false, expected_revision: 1 }));
    expect(stale.status).toBe(409);
    expect((await (await api("/api/mcp/connectors")).json() as { items: unknown[] }).items).toHaveLength(1);
  });
});

describe("portal personal-data mock reads", () => {
  it("serves an empty account snapshot, quota, changes, and metadata-only record page", async () => {
    expect((await api("/api/sync/snapshot")).status).toBe(401);
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));

    const snapshot = await api("/api/sync/snapshot?limit=100");
    expect(await snapshot.json()).toEqual({ watermark: 0, items: [], next_cursor: null });
    const changes = await api("/api/sync/changes?after_sequence=4&limit=100");
    expect(await changes.json()).toEqual({ items: [], through_sequence: 4, next_after_sequence: 4, has_more: false });
    const quota = await api("/api/personal-data/quota");
    expect(await quota.json()).toMatchObject({ owner_id: "u1a2b3c4", used_bytes: 0, remaining_bytes: 5_368_709_120, version: 1 });
    const records = await api("/api/me/records?limit=50");
    expect(await records.json()).toEqual({ items: [], next_before: "" });
    const captureStatuses = await api("/api/me/record-captures?limit=50");
    expect(await captureStatuses.json()).toEqual({ items: [], next_before: "" });
  });

  it("imports only the selected text messages and returns the same conversation for follow-up batches", async () => {
    expect((await api("/api/personal-data/history/import", json("POST", {}))).status).toBe(401);
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const body = {
      source_id: "nanomuse-export-v1", device_account_id: "nanomuse-local-v1", source_conversation_id: "main",
      title: "Imported chat", messages: [{ source_message_id: "e_user01", role: "user", content: "hello" }],
    };
    const first = await api("/api/personal-data/history/import", json("POST", body));
    const responseBody = await first.json() as { conversation_id: string; messages: Array<{ role: string; content: string }> };
    expect(first.status).toBe(200);
    expect(responseBody).toMatchObject({ title: "Imported chat", messages: [{ role: "user", content: "hello" }] });
    expect(JSON.stringify(responseBody)).not.toContain("task_id");
    const next = await api("/api/personal-data/history/import", json("POST", { ...body, target_conversation_id: responseBody.conversation_id }));
    expect(await next.json()).toMatchObject({ conversation_id: responseBody.conversation_id });
  });

  it("writes selected rooms memories and goals with owner scope and exact idempotent retries", async () => {
    expect((await api("/api/personal-data/memory/m_1234abcd", json("PUT", { base_revision: 0, body: { content: "Selected memory" } }))).status).toBe(401);
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const memoryPath = "/api/personal-data/memory/m_1234abcd";
    const memoryRequest = { base_revision: 0, body: { content: "Selected memory" } };
    const memoryHeaders = { "content-type": "application/json", "Idempotency-Key": "legacy-rooms-memory-test-001" };
    const memoryWrite = await api(memoryPath, { ...json("PUT", memoryRequest), headers: memoryHeaders });
    expect(memoryWrite.status).toBe(200);
    expect(await memoryWrite.json()).toMatchObject({ object_type: "memory", object_id: "m_1234abcd", result_revision: 1, replay: false });
    const memoryReplay = await api(memoryPath, { ...json("PUT", memoryRequest), headers: memoryHeaders });
    expect(await memoryReplay.json()).toMatchObject({ result_revision: 1, account_change_sequence: 1, replay: true });
    expect((await api(memoryPath, { ...json("PUT", { ...memoryRequest, body: { content: "Different" } }), headers: memoryHeaders })).status).toBe(409);
    expect(await (await api(memoryPath)).json()).toMatchObject({ revision: 1, body: { content: "Selected memory" } });

    const goal = {
      id: "legacy_goal_test001",
      body: { title: "Selected goal", description: "Imported", status: "active", steps: [{ id: "legacy_step_1", title: "First", status: "pending" }], related_material_ids: [] },
    };
    const goalHeaders = { "content-type": "application/json", "Idempotency-Key": "legacy-rooms-goal-test-001" };
    const goalWrite = await api("/api/goals", { ...json("POST", goal), headers: goalHeaders });
    expect(goalWrite.status).toBe(201);
    expect(await goalWrite.json()).toMatchObject({ object_type: "goal", object_id: goal.id, result_revision: 1, replay: false });
    expect(await (await api("/api/goals", { ...json("POST", goal), headers: goalHeaders })).json()).toMatchObject({ result_revision: 1, replay: true });
    expect((await api(`/api/goals/${goal.id}`)).status).toBe(200);
    expect(await (await api("/api/goals")).json()).toMatchObject({ items: [{ object_id: goal.id, body: { title: "Selected goal" } }] });
    expect(await (await api("/api/sync/snapshot")).json()).toMatchObject({ watermark: 2, items: [{ object_id: "m_1234abcd" }, { object_id: goal.id }] });
    expect(await (await api("/api/sync/changes?after_sequence=0&limit=100")).json()).toMatchObject({
      items: [{ change_sequence: 1, object_type: "memory" }, { change_sequence: 2, object_type: "goal" }],
      through_sequence: 2, next_after_sequence: 2, has_more: false,
    });

    const member = createApiClient();
    await member("/api/auth/login", json("POST", { name: "member", password: "member" }));
    expect((await member(memoryPath)).status).toBe(404);
    expect((await member(`/api/goals/${goal.id}`)).status).toBe(404);
    expect(await (await member("/api/goals")).json()).toEqual({ items: [], next_cursor: null });
    expect(await (await member("/api/sync/snapshot")).json()).toEqual({ watermark: 0, items: [], next_cursor: null });
  });

  it("accepts the default dragon profile saved by the Portal editor", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const body = { name: "本机验收用户", avatar: "dragon", emoji: "", color: "", description: "", style: "" };
    const response = await api("/api/personal-data/profile/default", {
      ...json("PUT", { base_revision: 0, body }),
      headers: { "content-type": "application/json", "Idempotency-Key": "profile-editor-save-001" },
    });
    expect(response.status).toBe(200);
    expect(await response.json()).toMatchObject({ object_type: "profile", object_id: "default", result_revision: 1 });
    expect(await (await api("/api/personal-data/profile/default")).json()).toMatchObject({ body });
  });

  it("writes only contract-shaped legacy personal objects with scoped revision and idempotency", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const feedBody = { title: "Imported note", type: "research", body: "Plain text", created_at: "2026-11-04T00:00:00.000Z", liked: true };
    const feedHeaders = { "content-type": "application/json", "Idempotency-Key": "legacy-feed-post-test-001" };
    const feedPath = "/api/personal-data/feed_post/legacy_feed_post_abc123";
    const feed = await api(feedPath, { ...json("PUT", { base_revision: 0, body: feedBody }), headers: feedHeaders });
    expect(feed.status).toBe(200);
    expect(await feed.json()).toMatchObject({ object_type: "feed_post", object_id: "legacy_feed_post_abc123", result_revision: 1, replay: false });
    expect(await (await api(feedPath, { ...json("PUT", { base_revision: 0, body: feedBody }), headers: feedHeaders })).json()).toMatchObject({ replay: true });
    expect((await api("/api/personal-data/idea_mark/train-watch", json("PUT", {
      base_revision: 0, body: { state: "saved", session_id: "must-not-transfer" },
    }))).status).toBe(400);
    expect((await api("/api/personal-data/library_item/legacy_library_abc123", json("PUT", {
      base_revision: 0, body: { name: "Index only", kind: "artifact", path: "/private/file" },
    }))).status).toBe(400);

    const libraryHeaders = { "content-type": "application/json", "Idempotency-Key": "legacy-library-test-001" };
    const libraryPath = "/api/personal-data/library_item/legacy_library_abc123";
    const libraryBody = { name: "Index only", kind: "artifact" };
    expect((await api(libraryPath, { ...json("PUT", { base_revision: 0, body: libraryBody }), headers: libraryHeaders })).status).toBe(200);
    expect(await (await api(libraryPath)).json()).toMatchObject({ object_type: "library_item", body: libraryBody });

    const soulPath = "/api/personal-data/soul/legacy_soul_abc123";
    const soulBody = { name: "nanoMuse", lang: "zh-CN", content: "Be thoughtful." };
    expect((await api(soulPath, { ...json("PUT", { base_revision: 0, body: soulBody }), headers: { ...libraryHeaders, "Idempotency-Key": "legacy-soul-test-001" } })).status).toBe(200);
    expect(await (await api("/api/sync/snapshot")).json()).toMatchObject({ watermark: 3, items: [
      { object_type: "feed_post", body: feedBody }, { object_type: "library_item", body: libraryBody }, { object_type: "soul", body: soulBody },
    ] });

    const member = createApiClient();
    await member("/api/auth/login", json("POST", { name: "member", password: "member" }));
    expect((await member(feedPath)).status).toBe(404);
    expect((await member(libraryPath)).status).toBe(404);
    expect(await (await member("/api/sync/snapshot")).json()).toMatchObject({ watermark: 0, items: [] });
  });
});

describe("local project mock routes", () => {
  it("shares project history but reconciles each sender's message only for that sender", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const projectID = "pdem0001";
    const created = await api(`/api/projects/${projectID}/conversations`, {
      ...json("POST", { title: "本机合成对话" }), headers: { "content-type": "application/json", "Idempotency-Key": "project-create-1" },
    });
    const conversation = await created.json() as { id: string };
    const path = `/api/projects/${projectID}/conversations/${conversation.id}/messages`;
    const headers = { "content-type": "application/json", "Idempotency-Key": "project-append-test-1",
      "X-Morde-Service-Instance-ID": "00000000-0000-4000-8000-000000000001", "X-Morde-Actor-ID": "u1a2b3c4" };
    const body = { client_message_id: "project-append-test-1", model_id: "pmodel01", content: "只受理此消息" };
    const first = await api(path, { ...json("POST", body), headers });
    const receipt = await first.json() as { message: { id: string } };
    expect(first.status).toBe(202);
    expect(receipt).toMatchObject({ message: { owner_id: "u1a2b3c4", actor_id: "u1a2b3c4", routing_status: "pending" } });
    expect(receipt).not.toHaveProperty("task");
    expect(await (await api(path, { ...json("POST", body), headers })).json()).toMatchObject({ message: { id: receipt.message.id } });
    expect((await api(path, { ...json("POST", { ...body, content: "不同内容" }), headers })).status).toBe(409);
    expect((await api(path, { ...json("POST", body), headers: { ...headers, "X-Morde-Service-Instance-ID": "other" } })).status).toBe(409);

    const member = createApiClient();
    await member("/api/auth/login", json("POST", { name: "member", password: "member" }));
    expect((await member(path)).status).toBe(404);
    await member("/api/project-invitations/rinvite1/accept", { method: "POST", headers: { "Idempotency-Key": "project-member-accept-1" } });
    expect(await (await member(path)).json()).toMatchObject({ items: [{ id: receipt.message.id }] });
    expect((await member(`${path}?client_message_id=${body.client_message_id}`, { headers })).status).toBe(409);
    const memberHeaders = { ...headers, "X-Morde-Actor-ID": "umember1" };
    expect(await (await member(`${path}?client_message_id=${body.client_message_id}`, { headers: memberHeaders })).json()).toEqual({ items: [], next_cursor: null });
    const memberBody = { ...body, client_message_id: "member-message-1" };
    const wrongAccount = await member(path, { ...json("POST", memberBody), headers: { ...headers, "Idempotency-Key": memberBody.client_message_id } });
    expect(wrongAccount.status).toBe(409);
    expect((await (await member(path)).json()).items).toHaveLength(1);
    const second = await member(path, { ...json("POST", memberBody), headers: { ...memberHeaders, "Idempotency-Key": memberBody.client_message_id } });
    expect(await second.json()).toMatchObject({ message: { owner_id: "u1a2b3c4", actor_id: "umember1", routing_status: "pending" } });
    expect((await (await member(path)).json()).items).toHaveLength(2);
  });

  it("keeps project access behind invitation acceptance and checks work-item versions", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const projectPage = await api("/api/projects");
    const project = (await projectPage.json() as { items: Array<{ id: string; version: number }> }).items[0];
    if (!project) throw new Error("local project fixture is missing");

    const member = createApiClient();
    await member("/api/auth/login", json("POST", { name: "member", password: "member" }));
    expect((await member(`/api/projects/${project.id}`)).status).toBe(404);
    const invitations = await member("/api/me/project-invitations");
    const invitation = (await invitations.json() as { items: Array<{ id: string; state: string }> }).items[0];
    expect(invitation?.state).toBe("pending");

    const accepted = await member(`/api/project-invitations/${invitation!.id}/accept`, { method: "POST", headers: { "Idempotency-Key": "project-accept-test" } });
    expect(accepted.status).toBe(201);
    expect(await accepted.json()).toMatchObject({ project_id: project.id, user_id: "umember1", role: "member" });
    const acceptedProject = await member(`/api/projects/${project.id}`);
    expect(acceptedProject.status).toBe(200);

    const transfer = await api(`/api/projects/${project.id}/owner-transfer`, {
      ...json("POST", { user_id: "umember1", expected_version: project.version }),
      headers: { "content-type": "application/json", "Idempotency-Key": "project-transfer-test" },
    });
    expect(await transfer.json()).toMatchObject({ owner_id: "umember1", version: 2 });

    const blocked = await member(`/api/projects/${project.id}/work-items/jdem0001/block`, json("POST", { blocked_reason: "等待确认", expected_version: 1 }));
    expect(await blocked.json()).toMatchObject({ state: "blocked", blocked_reason: "等待确认", version: 2 });
    const stale = await member(`/api/projects/${project.id}/work-items/jdem0001/resume`, json("POST", { expected_version: 1 }));
    expect(stale.status).toBe(409);
    const resumed = await member(`/api/projects/${project.id}/work-items/jdem0001/resume`, json("POST", { expected_version: 2 }));
    expect(await resumed.json()).toMatchObject({ state: "open", blocked_reason: "", version: 3 });
  });

  it("keeps unassigned items hidden from legacy lists and uses versioned idempotent claims", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const projectID = "pdem0001";
    const member = createApiClient();
    await member("/api/auth/login", json("POST", { name: "member", password: "member" }));
    const invitation = await member("/api/me/project-invitations");
    const invitationID = (await invitation.json() as { items: Array<{ id: string }> }).items[0]!.id;
    expect((await member(`/api/project-invitations/${invitationID}/accept`, {
      method: "POST", headers: { "Idempotency-Key": "work-item-claim-accept-01" },
    })).status).toBe(201);

    const create = await api(`/api/projects/${projectID}/work-items`, {
      ...json("POST", { title: "新增待认领缺陷", category: "bug", unassigned: true }),
      headers: { "content-type": "application/json", "Idempotency-Key": "work-item-unassigned-create-01" },
    });
    const item = await create.json() as { id: string; assignee_id: string | null; category: string; version: number };
    expect(create.status).toBe(201);
    expect(item).toMatchObject({ assignee_id: null, category: "bug", version: 1 });

    const legacyList = await (await api(`/api/projects/${projectID}/work-items`)).json() as { items: Array<{ id: string }> };
    expect(legacyList.items.some((candidate) => candidate.id === item.id)).toBe(false);
    const v2List = await (await api(`/api/projects/${projectID}/work-items?include_unassigned=true`)).json() as { items: Array<{ id: string; assignee_id: string | null; category: string }> };
    expect(v2List.items).toEqual(expect.arrayContaining([expect.objectContaining({ id: item.id, assignee_id: null, category: "bug" })]));

    const path = `/api/projects/${projectID}/work-items/${item.id}/claim`;
    const ownerClaim = api(path, { ...json("POST", { expected_version: item.version }), headers: { "content-type": "application/json", "Idempotency-Key": "work-item-claim-owner-01" } });
    const memberClaim = member(path, { ...json("POST", { expected_version: item.version }), headers: { "content-type": "application/json", "Idempotency-Key": "work-item-claim-member-01" } });
    const claims = await Promise.all([ownerClaim, memberClaim]);
    const winner = claims.find((response) => response.status === 200)!;
    const loser = claims.find((response) => response.status === 409)!;
    const claimed = await winner.clone().json() as { assignee_id: string; version: number };
    expect(claims.map((response) => response.status).sort()).toEqual([200, 409]);
    expect(claimed).toMatchObject({ version: 2 });
    expect(["u1a2b3c4", "umember1"]).toContain(claimed.assignee_id);
    expect(await loser.json()).toMatchObject({ error: { code: "conflict" } });

    const winnerKey = winner === claims[0] ? "work-item-claim-owner-01" : "work-item-claim-member-01";
    const winnerClient = winner === claims[0] ? api : member;
    const replay = await winnerClient(path, {
      ...json("POST", { expected_version: item.version }), headers: { "content-type": "application/json", "Idempotency-Key": winnerKey },
    });
    expect(await replay.json()).toMatchObject({ assignee_id: claimed.assignee_id, version: 2 });
  });

  it("keeps project Workspace directory reads behind both resource linkage and direct Workspace access", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const workspaceResponse = await createWorkspace("项目源工作区");
    const workspace = await workspaceResponse.json() as { id: string };
    const projectID = "pdem0001";
    const linked = await api(`/api/projects/${projectID}/resources`, {
      ...json("POST", { kind: "workspace", resource_id: workspace.id, expected_version: 1 }),
      headers: { "content-type": "application/json", "Idempotency-Key": "project-workspace-link-01" },
    });
    expect(linked.status).toBe(201);

    const ownerManifest = await api(`/api/projects/${projectID}/workspaces/${workspace.id}/manifest`);
    expect(ownerManifest.status).toBe(200);
    expect(await ownerManifest.json()).toEqual({
      project_id: projectID, workspace_id: workspace.id, revision: 0,
      files: [], empty_directories: [], next_cursor: null,
    });
    expect((await api(`/api/projects/${projectID}/workspaces/wmiss001/manifest`)).status).toBe(404);

    const member = createApiClient();
    await member("/api/auth/login", json("POST", { name: "member", password: "member" }));
    await member("/api/project-invitations/rinvite1/accept", {
      method: "POST", headers: { "Idempotency-Key": "project-workspace-member-01" },
    });
    expect((await member(`/api/projects/${projectID}/workspaces/${workspace.id}/manifest`)).status).toBe(404);

    await allowHosts(2);
    const hostResponse = await api("/api/hosts", {
      method: "POST", headers: { "content-type": "application/json", "Idempotency-Key": "project-workspace-host-01" },
      body: JSON.stringify({ name: "Workspace 只读授权", workspace_ids: [workspace.id] }),
    });
    const host = await hostResponse.json() as { id: string };
    expect(hostResponse.status).toBe(201);
    const shared = await api(`/api/hosts/${host.id}/members`, json("POST", { user_id: "umember1", permissions: ["workspace:file:read"] }));
    expect(shared.status).toBe(201);

    expect(await (await member(`/api/projects/${projectID}/resources`)).json()).toMatchObject({
      items: [{ project_id: projectID, kind: "workspace", resource_id: workspace.id }],
    });
    const memberManifest = await member(`/api/projects/${projectID}/workspaces/${workspace.id}/manifest`);
    expect(memberManifest.status).toBe(200);
    expect(await memberManifest.json()).toMatchObject({ project_id: projectID, workspace_id: workspace.id, revision: 0, files: [] });
  });
});

describe("local main-conversation mock routes", () => {
  it("shares one server-owned main ID across clients and rejects a stale concurrent update", async () => {
    const firstClient = createApiClient();
    const secondClient = createApiClient();
    await firstClient("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const createTask = (client: typeof firstClient, key: string, objective: string) => client("/api/tasks", {
      ...json("POST", { model_id: "pmodel01", objective }),
      headers: { "content-type": "application/json", "Idempotency-Key": key },
    });
    const firstTask = await createTask(firstClient, "main-task-one-001", "第一个历史会话");
    const secondTask = await createTask(firstClient, "main-task-two-001", "第二个历史会话");
    const firstConversationID = (await firstTask.json() as { conversation_id: string }).conversation_id;
    const secondConversationID = (await secondTask.json() as { conversation_id: string }).conversation_id;
    await secondClient("/api/auth/login", json("POST", { name: "demo", password: "demo" }));

    const selectionRequired = await firstClient("/api/me/main-conversation/ensure", {
      method: "POST", headers: { "Idempotency-Key": "main-ensure-test-001" },
    });
    expect(await selectionRequired.json()).toEqual({ status: "selection_required", conversation_id: "", version: 0 });
    const selected = await firstClient("/api/me/main-conversation", {
      ...json("PUT", { conversation_id: firstConversationID, expected_version: 0 }),
      headers: { "content-type": "application/json", "Idempotency-Key": "main-select-test-001" },
    });
    expect(await selected.json()).toEqual({ status: "ready", conversation_id: firstConversationID, version: 1 });
    expect(await (await secondClient("/api/me/main-conversation")).json()).toEqual({ status: "ready", conversation_id: firstConversationID, version: 1 });

    const stale = await secondClient("/api/me/main-conversation", {
      ...json("PUT", { conversation_id: secondConversationID, expected_version: 0 }),
      headers: { "content-type": "application/json", "Idempotency-Key": "main-select-stale-001" },
    });
    expect(stale.status).toBe(409);
    const tasksAfterConflict = await firstClient("/api/tasks");
    expect(await tasksAfterConflict.json()).toMatchObject({ items: [{ id: expect.any(String) }, { id: expect.any(String) }] });
  });
});

describe("workspace manifest metadata mock", () => {
  it("returns owner-scoped revision metadata without file content", async () => {
    expect((await api("/api/workspaces/w1234567/manifest?revision=0")).status).toBe(401);
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const created = await createWorkspace("manifest demo");
    const workspace = await created.json() as { id: string; revision: number };

    const manifest = await api(`/api/workspaces/${workspace.id}/manifest?revision=0&limit=1000`);
    expect(await manifest.json()).toEqual({ workspace_id: workspace.id, revision: 0, files: [], empty_directories: [], next_cursor: null });
    expect((await api(`/api/workspaces/${workspace.id}/manifest?revision=invalid`)).status).toBe(400);
    expect((await api(`/api/workspaces/${workspace.id}/manifest?revision=1`)).status).toBe(404);

    const member = createApiClient();
    await member("/api/auth/login", json("POST", { name: "member", password: "member" }));
    expect((await member(`/api/workspaces/${workspace.id}/manifest?revision=0`)).status).toBe(404);
  });
});

describe("workspace diff and restore mock", () => {
  it("returns owner-scoped synthetic diffs and applies an idempotent compare-and-set restore", async () => {
    vi.stubEnv("VITE_MOCK_API", "1");
    resetMockState();
    expect((await api("/api/workspaces/wlocal01/diff?from_revision=1&to_revision=2")).status).toBe(401);
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));

    const diff = await api("/api/workspaces/wlocal01/diff?from_revision=1&to_revision=2");
    expect(await diff.json()).toMatchObject({
      workspace_id: "wlocal01", from_revision: 1, to_revision: 2,
      changes: [{ path: "README.md", display_status: "text", before_text: "本地合成 revision v1", after_text: "本地合成 revision v2" }],
    });
    const restoreBody = { target_revision: 1, expected_current_revision: 2, paths: ["README.md"] };
    const restore = await api("/api/workspaces/wlocal01/restore", {
      ...json("POST", restoreBody), headers: { "content-type": "application/json", "Idempotency-Key": "restore-test-key-001" },
    });
    expect(await restore.json()).toEqual({ workspace_id: "wlocal01", target_revision: 1, expected_current_revision: 2, revision: 3, replayed: false });
    const replay = await api("/api/workspaces/wlocal01/restore", {
      ...json("POST", restoreBody), headers: { "content-type": "application/json", "Idempotency-Key": "restore-test-key-001" },
    });
    expect(await replay.json()).toEqual({ workspace_id: "wlocal01", target_revision: 1, expected_current_revision: 2, revision: 3, replayed: true });
    expect((await api("/api/workspaces/wlocal01/restore", {
      ...json("POST", { ...restoreBody, paths: ["../outside"] }), headers: { "content-type": "application/json", "Idempotency-Key": "restore-test-key-002" },
    })).status).toBe(400);
    expect((await api("/api/workspaces/wlocal01/restore", {
      ...json("POST", { ...restoreBody, expected_current_revision: 2 }), headers: { "content-type": "application/json", "Idempotency-Key": "restore-test-key-003" },
    })).status).toBe(409);

    const member = createApiClient();
    await member("/api/auth/login", json("POST", { name: "member", password: "member" }));
    expect((await member("/api/workspaces/wlocal01/diff?from_revision=1&to_revision=2")).status).toBe(404);
  });
});

describe("schedule mock routes", () => {
  it("creates and lists an every-N-hours schedule", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const hosts = await api("/api/hosts");
    const models = await api("/api/available-models");
    const host = (await hosts.json() as { items: Array<{ id: string }> }).items[0];
    const model = (await models.json() as { items: Array<{ id: string }> }).items[0];
    if (!host || !model) throw new Error("mock schedule dependencies are missing");
    const timing = { kind: "every_n_hours", interval_hours: 6, time_zone: "America/New_York" };

    const created = await api("/api/schedules", {
      ...json("POST", {
        host_id: host.id, model_id: model.id, objective: "每六小时检查一次", timing,
        budget: { active_seconds: 900, tool_calls: 20, model_tokens: 10000 },
      }),
      headers: { "content-type": "application/json", "Idempotency-Key": "mock-schedule-create-0001" },
    });
    expect(created.status).toBe(201);
    expect(await created.json()).toMatchObject({ timing, objective: "每六小时检查一次", status: "enabled" });

    const listed = await api("/api/schedules");
    expect(await listed.json()).toMatchObject({ items: [{ timing, objective: "每六小时检查一次" }] });
  });

  it("creates a hostless text schedule and rejects hostless observation", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const models = await api("/api/available-models");
    const model = (await models.json() as { items: Array<{ id: string }> }).items[0];
    if (!model) throw new Error("mock schedule model is missing");
    const body = {
      host_id: null, model_id: model.id, objective: "仅文本计划", timing: { kind: "daily", starts_on: "2026-11-04", local_time: "09:00", time_zone: "UTC" },
      budget: { active_seconds: 60, tool_calls: 2, model_tokens: 500 }, observation: null,
    };

    const created = await api("/api/schedules", {
      ...json("POST", body), headers: { "content-type": "application/json", "Idempotency-Key": "mock-hostless-schedule-001" },
    });
    expect(created.status).toBe(201);
    expect(await created.json()).toMatchObject({ host_id: null, title: body.objective, objective: body.objective, status: "enabled" });
    const rejectedObservation = await api("/api/schedules", {
      ...json("POST", { ...body, observation: { url: "https://example.test", selector: "h1", field: "text" } }),
      headers: { "content-type": "application/json", "Idempotency-Key": "mock-hostless-schedule-002" },
    });
    expect(rejectedObservation.status).toBe(400);
  });
});

describe("admin console mock routes", () => {
  it("records invitation attribution without bypassing review and keeps admin defaults safe", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const settings = await api("/api/admin/settings");
    expect(await settings.json()).toMatchObject({
      registration_review_required: true, automatic_new_user_model_budget: true,
      automatic_invitation_reward: false, automatic_contribution_reward: false,
      default_model_budget: { configured: false, token_limit: 0, max_concurrent: 0, version: 0 },
    });
    const codeResponse = await api("/api/me/invite-code");
    const { code } = await codeResponse.json() as { code: string };
    expect(code).toMatch(/^[a-f0-9]{32}$/);

    const invalid = await createApiClient()("/api/auth/register", json("POST", { name: "bad-invite", password: "secret-pass", invite_code: "invalid" }));
    expect(invalid.status).toBe(400);
    const registered = await createApiClient()("/api/auth/register", json("POST", {
      name: "invited-user", password: "secret-pass", invite_code: code,
    }));
    expect(registered.status).toBe(201);
    expect(registered.headers.get("set-cookie")).toBeNull();
    expect(await registered.json()).toMatchObject({ user: { name: "invited-user", status: "pending" } });

    const invites = await api("/api/admin/invites?search=invited-user&limit=10&offset=0");
    expect(await invites.json()).toMatchObject({ total: 1, total_inviters: 1, items: [{ inviter_name: "demo", invitee_name: "invited-user", invitee_status: "pending", inviter_invitees: 1 }] });
    const sites = await api("/api/admin/sites");
    expect(await sites.json()).toMatchObject({ integrations: { demo: "not_configured", github_stats: "not_configured", traffic: "not_collected" }, total: 0 });

    const initialPolicy = await api("/api/admin/nudges");
    expect(await initialPolicy.json()).toMatchObject({ enabled: false, version: 1 });
    const enabled = await api("/api/admin/nudges", json("PUT", {
      expected_version: 1, enabled: true, url: "https://github.com/acme/project", text_zh: "查看项目", text_en: "View the project",
    }));
    expect(await enabled.json()).toMatchObject({ enabled: true, version: 2 });
    expect(await (await createApiClient()("/api/nudges")).json()).toMatchObject({ enabled: true, url: "https://github.com/acme/project" });
    const stale = await api("/api/admin/nudges", json("PUT", {
      expected_version: 1, enabled: false, url: "", text_zh: "", text_en: "",
    }));
    expect(stale.status).toBe(409);
    const audit = await api("/api/admin/audit?limit=20&offset=0");
    expect(await audit.text()).toContain("admin.nudge.policy.update");
  });

  it("reads and compare-and-set updates isolated personal-data quotas", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const initial = await api("/api/admin/users/u1a2b3c4/personal-data-quota");
    expect(initial.status).toBe(200);
    expect(await initial.json()).toMatchObject({
      used_bytes: 0, reserved_bytes: 0, quota_bytes: 5 * 1024 ** 3,
      max_attachment_bytes: 300 * 1024 ** 2, version: 1,
      default_quota_bytes: 5 * 1024 ** 3, default_max_attachment_bytes: 300 * 1024 ** 2,
    });

    const update = await api("/api/admin/users/u1a2b3c4/personal-data-quota", json("PATCH", {
      expected_version: 1, quota_bytes: 6 * 1024 ** 3, max_attachment_bytes: 400 * 1024 ** 2,
    }));
    expect(update.status).toBe(200);
    expect(await update.json()).toMatchObject({ quota_bytes: 6 * 1024 ** 3, max_attachment_bytes: 400 * 1024 ** 2, version: 2 });
    const replayedRead = await api("/api/admin/users/u1a2b3c4/personal-data-quota");
    expect(await replayedRead.json()).toMatchObject({ quota_bytes: 6 * 1024 ** 3, version: 2 });

    const stale = await api("/api/admin/users/u1a2b3c4/personal-data-quota", json("PATCH", {
      expected_version: 1, quota_bytes: 7 * 1024 ** 3, max_attachment_bytes: 500 * 1024 ** 2,
    }));
    expect(stale.status).toBe(409);
  });

  it("serves the admin views and never returns or audits a shared model key", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const overview = await api("/api/admin/overview");
    expect(overview.status).toBe(200);
    expect(await overview.json()).toMatchObject({ health: { ready: true }, online_installations: 0, agents_by_state: {} });
    const stats = await api("/api/admin/stats");
    expect(await stats.json()).toMatchObject({ users: { total: 3 }, platforms: [{ platform: "desktop", installations: 1 }], version_tracking_available: false });

    const created = await api("/api/model-connections", json("POST", {
      name: "本地测试连接", base_url: "https://models.example.test/v1", model_name: "test-model", api_key: "local-test-secret", shared: true,
    }));
    expect(created.status).toBe(201);
    const createdText = await created.text();
    expect(createdText).not.toContain("local-test-secret");
    const connection = JSON.parse(createdText) as { id: string; shared: boolean };
    expect(connection.shared).toBe(true);
    const page = await api("/api/admin/model-connections");
    const pageBody = await page.json();
    expect(pageBody.items).toEqual(expect.arrayContaining([expect.objectContaining({ id: connection.id, shared: true, has_api_key: true })]));
    const audit = await api(`/api/admin/audit?target=${connection.id}`);
    const auditText = await audit.text();
    expect(auditText).toContain("model.connection.create");
    expect(auditText).not.toContain("local-test-secret");
  });

  it("creates, reads, checks and owner-fences a personal model without returning its key", async () => {
    await api("/api/auth/login", json("POST", { name: "member", password: "member" }));
    const secret = "private-local-test-secret";
    const created = await api("/api/model-connections", json("POST", {
      name: "个人模型", base_url: "https://models.example.test/v1", model_name: "private-test-model", api_key: secret, shared: false,
    }));
    expect(created.status).toBe(201);
    const createdText = await created.text();
    expect(createdText).not.toContain(secret);
    const connection = JSON.parse(createdText) as { id: string; shared: boolean; has_api_key: boolean };
    expect(connection).toMatchObject({ shared: false, has_api_key: true });

    const listed = await api("/api/model-connections");
    expect(await listed.json()).toMatchObject({ items: expect.arrayContaining([expect.objectContaining({ id: connection.id, shared: false })]) });
    expect((await api(`/api/model-connections/${connection.id}`)).status).toBe(200);
    const check = await api(`/api/model-connections/${connection.id}/check`, { method: "POST", headers: { "Idempotency-Key": "personal-model-check-001" } });
    expect(await check.json()).toEqual({ text: true, tools: true, vision: false, text_stream: false });
    const catalog = await api("/api/available-models");
    expect(await catalog.json()).toMatchObject({ items: expect.arrayContaining([expect.objectContaining({ id: connection.id })]), default_model_configured: true });

    const preferences = await api("/api/me/preferences", json("PUT", { assistant_name: "Morde", selected_model_id: connection.id }));
    expect(await preferences.json()).toMatchObject({ selected_model_id: connection.id });

    const ownerTask = await api("/api/tasks", {
      ...json("POST", { model_id: connection.id, objective: "使用自己的模型" }),
      headers: { "content-type": "application/json", "Idempotency-Key": "personal-model-task-001" },
    });
    expect(ownerTask.status).toBe(202);
    expect(await ownerTask.json()).toMatchObject({ model_id: connection.id, objective: "使用自己的模型" });

    const otherAccount = createApiClient();
    await otherAccount("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    expect((await otherAccount(`/api/model-connections/${connection.id}`)).status).toBe(404);
    expect((await otherAccount(`/api/model-connections/${connection.id}/check`, { method: "POST", headers: { "Idempotency-Key": "foreign-model-check-001" } })).status).toBe(404);
    const otherModels = await otherAccount("/api/model-connections");
    expect(JSON.stringify(await otherModels.json())).not.toContain(connection.id);
    const otherCatalog = await otherAccount("/api/available-models");
    expect(JSON.stringify(await otherCatalog.json())).not.toContain(connection.id);
    const foreignTask = await otherAccount("/api/tasks", {
      ...json("POST", { model_id: connection.id, objective: "不能使用其他账号的模型" }),
      headers: { "content-type": "application/json", "Idempotency-Key": "foreign-model-task-001" },
    });
    expect(foreignTask.status).toBe(404);

    const invalidURL = await api("/api/model-connections", json("POST", {
      name: "不安全模型", base_url: "http://models.example.test/v1", model_name: "bad-model", api_key: secret, shared: false,
    }));
    expect(invalidURL.status).toBe(400);
    expect(await (await api("/api/model-connections")).text()).not.toContain(secret);
  });

  it("sets and clears the versioned platform default using only a checked shared text-and-tools model", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const initial = await api("/api/admin/settings");
    expect(await initial.json()).toMatchObject({ default_model: { configured: true, model_id: "pmodel01", version: 1 } });

    const invalid = await api("/api/admin/settings/model-default", json("PUT", { expected_version: 1, model_id: "missing" }));
    expect(invalid.status).toBe(400);
    const personal = await api("/api/model-connections", json("POST", {
      name: "个人候选", base_url: "https://private.example.test/v1", model_name: "private-test-model", api_key: "private-test-secret", shared: false,
    }));
    const personalID = (await personal.json() as { id: string }).id;
    const forbiddenDefault = await api("/api/admin/settings/model-default", json("PUT", { expected_version: 1, model_id: personalID }));
    expect(forbiddenDefault.status).toBe(400);
    const unverifiedShared = await api("/api/model-connections", json("POST", {
      name: "未检查共享模型", base_url: "https://unverified.example.test/v1", model_name: "unverified", api_key: "shared-test-secret", shared: true,
    }));
    const unverifiedID = (await unverifiedShared.json() as { id: string }).id;
    const unverifiedDefault = await api("/api/admin/settings/model-default", json("PUT", { expected_version: 1, model_id: unverifiedID }));
    expect(unverifiedDefault.status).toBe(400);
    const stale = await api("/api/admin/settings/model-default", json("PUT", { expected_version: 0, model_id: "pmodel01" }));
    expect(stale.status).toBe(409);

    const selected = await api("/api/admin/settings/model-default", json("PUT", { expected_version: 1, model_id: "pmodel01" }));
    expect(selected.status).toBe(200);
    expect(await selected.json()).toMatchObject({ configured: true, model_id: "pmodel01", version: 2 });

    const cleared = await api("/api/admin/settings/model-default", json("PUT", { expected_version: 2, model_id: "" }));
    expect(await cleared.json()).toMatchObject({ configured: false, model_id: null, version: 3 });
    expect(await (await api("/api/available-models")).json()).toMatchObject({ default_model_configured: false, default_model_id: null });
    expect(await (await api("/api/admin/settings")).json()).toMatchObject({ default_model: { configured: false, model_id: null, version: 3 } });
    const taskWithoutDefault = await api("/api/tasks", {
      ...json("POST", { model_id: "pmodel01", objective: "使用已验证模型" }),
      headers: { "content-type": "application/json", "Idempotency-Key": "no-default-task-001" },
    });
    expect(taskWithoutDefault.status).toBe(202);
  });

  it("serves only aggregate activity and explicit unavailable quality/place states", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const quality = await api("/api/admin/quality?days=30");
    expect(await quality.json()).toMatchObject({ days: 30, available: false });
    const activity = await api("/api/admin/activity?days=7");
    const body = await activity.text();
    expect(JSON.parse(body)).toMatchObject({ days: 7, by_day: expect.any(Array), hub_operations: [], task_events: [], audit_events: [] });
    expect(body).not.toMatch(/owner_id|installation_id|payload|ip|user_agent|hash|address/i);
    const places = await api("/api/admin/places");
    expect(await places.json()).toMatchObject({ available: false });
    expect((await api("/api/admin/activity?days=8")).status).toBe(400);
  });

  it("evaluates notify-only rules into the local receiver and enforces versions", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const created = await api("/api/admin/controls/rules", json("POST", { threshold: 3, note: "本地增长检查" }));
    expect(created.status).toBe(201);
    const rule = await created.json() as { id: number; version: number };
    const fired = await api("/api/admin/controls/evaluate", json("POST", {}));
    expect(await fired.json()).toMatchObject({ accounts_total: 3, fired: [{ id: rule.id, last_fired_at: expect.any(String) }] });
    expect((await (await api("/api/admin/controls/evaluate", json("POST", {}))).json()).fired).toEqual([]);
    expect((await api(`/api/admin/controls/rules/${rule.id}`, json("PUT", { expected_version: rule.version, enabled: false }))).status).toBe(409);
    const inbox = await api("/api/admin/controls/notify-test", json("POST", {}));
    expect(await inbox.json()).toMatchObject({ sent: true, configured: true });
    const state = await api("/api/admin/controls");
    expect(await state.json()).toMatchObject({ notifications: [{ kind: "test" }, { kind: "rule_fired", rule_id: rule.id }], notify_configured: true });
    const audit = await api("/api/admin/audit");
    expect(await audit.text()).toMatch(/admin\.control\.(rule\.create|rule\.fire|evaluate|notify_test)/);
  });

  it("keeps record consent off by default and separates owner consent from admin grants", async () => {
    const owner = createApiClient();
    await owner("/api/auth/login", json("POST", { name: "member", password: "member" }));
    const initialConsent = await owner("/api/me/record-consents");
    expect(await initialConsent.json()).toMatchObject({ items: [
      { purpose: "full_record", enabled: false, version: 0 },
      { purpose: "training_contribution", enabled: false, version: 0 },
    ] });
    const consent = await owner("/api/me/record-consents", json("PUT", { purpose: "full_record", enabled: true }));
    expect(await consent.json()).toMatchObject({ purpose: "full_record", enabled: true, version: 1 });

    const superAdmin = createApiClient();
    await superAdmin("/api/auth/login", json("POST", { name: "admin", password: "admin" }));
    expect((await superAdmin("/api/admin/users/umember1/record-settings")).status).toBe(403);
    await superAdmin("/api/me/password", json("POST", { current_password: "admin", new_password: "local-only-password" }));
    await superAdmin("/api/auth/login", json("POST", { name: "admin", password: "local-only-password" }));

    const settings = await superAdmin("/api/admin/users/umember1/record-settings");
    expect(await settings.json()).toMatchObject({ owner_id: "umember1", quota: { version: 1, retention_days: 30 } });
    const updatedQuota = await superAdmin("/api/admin/users/umember1/record-quota", json("PUT", {
      expected_version: 1, retention_days: 60, limit_bytes: 2 * 1024 ** 3,
    }));
    expect(await updatedQuota.json()).toMatchObject({ version: 2, retention_days: 60 });
    expect((await superAdmin("/api/admin/users/umember1/record-quota", json("PUT", {
      expected_version: 1, retention_days: 90, limit_bytes: 3 * 1024 ** 3,
    }))).status).toBe(409);
    expect((await superAdmin("/api/admin/users/umember1/record-grants", json("PUT", {
      admin_id: "u1a2b3c4", capability: "view", granted: true,
    }))).status).toBe(200);

    const administrator = createApiClient();
    await administrator("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    expect((await administrator("/api/admin/users/umember1/record-settings")).status).toBe(403);
    expect(await (await administrator("/api/admin/users/umember1/record-access")).json()).toMatchObject({
      capabilities: ["view"], consents: expect.arrayContaining([expect.objectContaining({ purpose: "full_record", enabled: true, version: 1 })]),
    });
    expect(await (await administrator("/api/admin/users/umember1/records?purpose=full_record")).json()).toEqual({ items: [], next_before: "" });
    const audit = await administrator("/api/admin/audit?action=record");
    expect(await audit.text()).toMatch(/record\.(consent|quota|grant|access|list)/);
  });

  it("rejects non-admin console reads and shared model writes", async () => {
    await api("/api/auth/login", json("POST", { name: "member", password: "member" }));
    expect((await api("/api/admin/overview")).status).toBe(403);
    expect((await api("/api/model-connections")).status).toBe(200);
    const models = await (await api("/api/model-connections")).json() as { items: Array<{ shared: boolean }> };
    expect(models.items.every((item) => item.shared)).toBe(true);
    expect((await api("/api/model-connections", json("POST", {
      name: "未授权连接", base_url: "https://models.example.test/v1", model_name: "test-model", api_key: "local-test-secret", shared: true,
    }))).status).toBe(403);
    expect((await api("/api/model-connections", json("POST", {
      name: "个人连接", base_url: "https://models.example.test/v1", model_name: "test-model", api_key: "local-test-secret", shared: false,
    }))).status).toBe(201);
  });
});

describe("portal mock cookie sessions", () => {
  it("allocates one stopped 1-core 2-GiB computer on login and refuses another", async () => {
    resetMockState();
    api = createApiClient();
    const login = await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    expect(login.status).toBe(200);
    expect((await login.json()).user.quota).toEqual({ hosts: 1, cpu_milli: 4000, mem_mib: 8192 });
    const first = await (await api("/api/hosts")).json();
    expect(first.items).toHaveLength(1);
    expect(first.items[0]).toMatchObject({ state: "stopped", resources: { cpu_milli: 1000, mem_mib: 2048, cpu_limit_milli: 1000, mem_limit_mib: 2048 } });
    const blocked = await createHostRequest({ name: "second computer" });
    expect(blocked.status).toBe(409);
    expect((await blocked.json()).error).toMatchObject({ code: "quota_exceeded", message: "达到云端电脑上限,无法再创建" });
    expect((await createHostRequest({ name: "custom computer", cpu_milli: 2000 })).status).toBe(400);
    expect((await createHostRequest({ name: "custom computer", template_id: "std-2c4g" })).status).toBe(400);
    const defaultHost = await api("/api/me/default-host", json("POST", {}));
    expect(await defaultHost.json()).toMatchObject({ created: false, host: { id: first.items[0].id, state: "stopped" } });
    expect((await (await api("/api/workspaces")).json()).items).toEqual([]);
    await api("/api/auth/logout", { method: "POST" });
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    expect((await (await api("/api/hosts")).json()).items).toHaveLength(1);
  });

  it("allows a second default-size computer after an admin raises the host limit", async () => {
    resetMockState();
    api = createApiClient();
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const updated = await api("/api/admin/users/u1a2b3c4", json("PATCH", {
      expected_status: "approved", quota: { hosts: 2, cpu_milli: 4000, mem_mib: 8192 },
    }));
    expect(updated.status).toBe(200);
    const created = await createHostRequest({ name: "second computer" });
    expect(created.status).toBe(201);
    expect((await created.json()).resources).toMatchObject({ cpu_milli: 1000, mem_mib: 2048 });
    const me = await (await api("/api/me")).json();
    expect(me.usage).toMatchObject({ hosts: 2, cpu_milli: 1000, mem_mib: 2048 });
  });

  it("keeps a restored computer running and retains an admin's online resource change after reload", async () => {
    resetMockState();
    api = createApiClient();
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const host = (await (await api("/api/hosts")).json()).items[0] as { id: string };
    const workspace = await createWorkspace("legacy host workspace");
    const { id: workspaceID, revision } = await workspace.json() as { id: string; revision: number };
    await api(`/api/hosts/${host.id}/workspace-bindings`, {
      ...json("POST", { workspace_id: workspaceID, expected_revision: revision, expected_binding_version: 0 }),
      headers: { "content-type": "application/json", "Idempotency-Key": "legacy-restore-bind-01" },
    });
    expect((await api(`/api/hosts/${host.id}/restore`, { method: "POST" })).status).toBe(202);
    await wait(950);
    expect((await (await api(`/api/hosts/${host.id}`)).json()).state).toBe("running");
    await allowHosts(1, api, 1000, 2048);
    expect((await api(`/api/admin/hosts/${host.id}/resources`, json("PATCH", {
      cpu_milli: 1500, mem_mib: 3072, expected_version: 1,
    }))).status).toBe(409);
    expect((await (await api(`/api/admin/hosts/${host.id}/resources`)).json()).resources).toEqual({ cpu_milli: 1000, mem_mib: 2048 });
    await allowHosts(1);
    expect((await api(`/api/admin/hosts/${host.id}/resources`, json("PATCH", {
      cpu_milli: 1500, mem_mib: 3072, expected_version: 1,
    }))).status).toBe(200);

    resetMockState({ restoreAssistantState: true });
    api = createApiClient();
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    expect((await (await api(`/api/hosts/${host.id}`)).json())).toMatchObject({
      state: "running", resources: { cpu_milli: 1500, mem_mib: 3072 },
    });
  });

  it("keeps admin resource changes after the mock state reloads", async () => {
    resetMockState();
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const resources = { cpu_milli: 1500, mem_mib: 3072 };
    const quota = { hosts: 2, cpu_milli: 4000, mem_mib: 8192 };
    expect((await api("/api/admin/users/umember1", json("PATCH", {
      expected_status: "approved", quota, default_host_resources: resources,
    }))).status).toBe(200);

    resetMockState({ restoreAssistantState: true });
    api = createApiClient();
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const restored = await (await api("/api/admin/users")).json();
    expect(restored.items.find((user: { id: string }) => user.id === "umember1")).toMatchObject({ quota, default_host_resources: resources });
  });

  it("registers a pending user without logging in and only reveals the review state after a correct password", async () => {
    const account = createApiClient();
    const created = await account("/api/auth/register", json("POST", { name: "fresh-user", password: "secret-pass" }));
    expect(created.status).toBe(201);
    expect(created.headers.get("set-cookie")).toBeNull();
    expect(await created.json()).toMatchObject({ user: { name: "fresh-user", role: "user", status: "pending", must_change_password: false } });
    expect((await account("/api/me")).status).toBe(401);
    expect((await account("/api/auth/register", json("POST", { name: "fresh-user", password: "secret-pass" }))).status).toBe(409);
    expect((await account("/api/auth/register", json("POST", { name: "admin", password: "secret-pass" }))).status).toBe(409);
    expect((await account("/api/auth/register", json("POST", { name: "AdMiN", password: "secret-pass" }))).status).toBe(409);
    const wrongPassword = await account("/api/auth/login", json("POST", { name: "fresh-user", password: "wrong-pass" }));
    expect(wrongPassword.status).toBe(401);
    expect((await wrongPassword.json()).error.code).toBe("unauthenticated");
    const pendingLogin = await account("/api/auth/login", json("POST", { name: "fresh-user", password: "secret-pass" }));
    expect(pendingLogin.status).toBe(403);
    expect((await pendingLogin.json()).error.code).toBe("account_pending");
    expect((await account("/api/admin/users")).status).toBe(401);
  });

  it("lets admins approve, promote and disable accounts using expected status", async () => {
    const admin = createApiClient();
    const account = createApiClient();
    await account("/api/auth/register", json("POST", { name: "review-me", password: "secret-pass" }));
    await admin("/api/auth/login", json("POST", { name: "demo", password: "demo" }));

    const pending = await admin("/api/admin/users?status=pending");
    const pendingList = await pending.json() as { items: Array<{ id: string; name: string; role: string; status: string }> };
    expect(pendingList.items).toEqual(expect.arrayContaining([expect.objectContaining({ name: "review-me", status: "pending", role: "user" })]));
    const user = pendingList.items.find((item) => item.name === "review-me")!;
    const stale = await admin(`/api/admin/users/${user.id}`, json("PATCH", { expected_status: "approved", status: "disabled" }));
    expect(stale.status).toBe(409);
    expect((await stale.json()).error.code).toBe("conflict");

    const unbudgetedAccount = await createApiClient()("/api/auth/register", json("POST", { name: "no-budget-user", password: "secret-pass" }));
    const unbudgeted = (await unbudgetedAccount.json() as { user: { id: string } }).user;
    const unconfiguredApproval = await admin(`/api/admin/users/${unbudgeted.id}`, json("PATCH", { expected_status: "pending", status: "approved" }));
    expect(unconfiguredApproval.status).toBe(200);
    expect(await (await admin(`/api/admin/users/${unbudgeted.id}/model-usage-budget`)).json()).toMatchObject({ configured: false });

    const invalidDefault = await admin("/api/admin/settings/model-budget-default", json("PUT", { expected_version: 0, token_limit: 0, max_concurrent: 101 }));
    expect(invalidDefault.status).toBe(400);

    const configured = await admin("/api/admin/settings/model-budget-default", json("PUT", { expected_version: 0, token_limit: 100_000, max_concurrent: 4 }));
    expect(configured.status).toBe(200);
    expect(await configured.json()).toMatchObject({ configured: true, token_limit: 100_000, max_concurrent: 4, version: 1 });
    const staleDefault = await admin("/api/admin/settings/model-budget-default", json("PUT", { expected_version: 0, token_limit: 200_000, max_concurrent: 8 }));
    expect(staleDefault.status).toBe(409);
    expect((await staleDefault.json()).error.code).toBe("conflict");

    const approved = await admin(`/api/admin/users/${user.id}`, json("PATCH", { expected_status: "pending", status: "approved" }));
    expect(approved.status).toBe(200);
    expect(await (await admin(`/api/admin/users/${user.id}/model-usage-budget`)).json()).toMatchObject({ configured: true, token_limit: 100_000, max_concurrent: 4 });
    const nextDefault = await admin("/api/admin/settings/model-budget-default", json("PUT", { expected_version: 1, token_limit: 200_000, max_concurrent: 8 }));
    expect(nextDefault.status).toBe(200);
    expect(await (await admin(`/api/admin/users/${user.id}/model-usage-budget`)).json()).toMatchObject({ configured: true, token_limit: 100_000, max_concurrent: 4 });
    const promoted = await admin(`/api/admin/users/${user.id}`, json("PATCH", { expected_status: "approved", role: "admin" }));
    expect(await promoted.json()).toMatchObject({ role: "admin", status: "approved" });
    await account("/api/auth/login", json("POST", { name: "review-me", password: "secret-pass" }));
    expect((await account("/api/admin/users")).status).toBe(200);

    const disabled = await admin(`/api/admin/users/${user.id}`, json("PATCH", { expected_status: "approved", status: "disabled" }));
    expect(await disabled.json()).toMatchObject({ status: "disabled", role: "admin" });
    expect((await account("/api/me")).status).toBe(401);
    expect((await account("/api/auth/login", json("POST", { name: "review-me", password: "wrong-pass" }))).status).toBe(401);
    const disabledLogin = await account("/api/auth/login", json("POST", { name: "review-me", password: "secret-pass" }));
    expect(disabledLogin.status).toBe(403);
    expect((await disabledLogin.json()).error.code).toBe("account_disabled");
    expect((await admin(`/api/admin/users/${user.id}`, json("PATCH", { expected_status: "disabled", status: "approved" }))).status).toBe(200);
    expect(await (await admin(`/api/admin/users/${user.id}/model-usage-budget`)).json()).toMatchObject({ configured: true, token_limit: 100_000, max_concurrent: 4 });

    const rejectedRegistration = await account("/api/auth/register", json("POST", { name: "rejected-me", password: "secret-pass" }));
    const rejectedUser = (await rejectedRegistration.json() as { user: { id: string } }).user;
    expect((await admin(`/api/admin/users/${rejectedUser.id}`, json("PATCH", { expected_status: "pending", status: "rejected" }))).status).toBe(200);
    expect((await account("/api/auth/login", json("POST", { name: "rejected-me", password: "wrong-pass" }))).status).toBe(401);
    const rejectedLogin = await account("/api/auth/login", json("POST", { name: "rejected-me", password: "secret-pass" }));
    expect(rejectedLogin.status).toBe(403);
    expect((await rejectedLogin.json()).error.code).toBe("account_rejected");
    const reapproved = await admin(`/api/admin/users/${rejectedUser.id}`, json("PATCH", { expected_status: "rejected", status: "approved" }));
    expect(reapproved.status).toBe(200);
    expect(await (await admin(`/api/admin/users/${rejectedUser.id}/model-usage-budget`)).json()).toMatchObject({ configured: true, token_limit: 200_000, max_concurrent: 8 });
  });

  it("protects the super administrator and restricts its initial default-password session", async () => {
    const admin = createApiClient();
    const initialLogin = await admin("/api/auth/login", json("POST", { name: "admin", password: "admin" }));
    expect(await initialLogin.json()).toMatchObject({ user: { name: "admin", role: "super_admin", must_change_password: true } });
    expect(await (await admin("/api/me")).json()).toMatchObject({ role: "super_admin", must_change_password: true });
    expect((await admin("/api/admin/users")).status).toBe(403);
    expect((await admin("/api/me/preferences")).status).toBe(403);

    const changed = await admin("/api/me/password", json("POST", { current_password: "admin", new_password: "admin-new-pass" }));
    expect(changed.status).toBe(204);
    expect(changed.headers.get("set-cookie")).toContain("Max-Age=0");
    expect((await admin("/api/me")).status).toBe(401);
    expect((await admin("/api/auth/login", json("POST", { name: "admin", password: "admin" }))).status).toBe(401);
    const nextLogin = await admin("/api/auth/login", json("POST", { name: "admin", password: "admin-new-pass" }));
    expect(await nextLogin.json()).toMatchObject({ user: { role: "super_admin", must_change_password: false } });
    expect((await admin("/api/admin/users")).status).toBe(200);
  });

  it("does not allow an admin to change their own role or operate on admin", async () => {
    const admin = createApiClient();
    await admin("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const self = await admin("/api/admin/users/u1a2b3c4", json("PATCH", { expected_status: "approved", role: "user" }));
    expect(self.status).toBe(403);
    const protectedAdmin = await admin("/api/admin/users/uadmin01", json("PATCH", { expected_status: "approved", status: "disabled" }));
    expect(protectedAdmin.status).toBe(403);
  });

  it("lists and revokes only the current user's client", async () => {
    const owner = createApiClient();
    const other = createApiClient();
    await owner("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    await other("/api/auth/login", json("POST", { name: "member", password: "member" }));
    const clients = (await (await owner("/api/clients")).json()).items as Array<{ id: string }>;
    expect(clients).toHaveLength(1);
    expect((await (await other("/api/clients")).json()).items).toHaveLength(0);
    expect((await other(`/api/clients/${clients[0]!.id}`, { method: "DELETE" })).status).toBe(404);
    expect((await owner(`/api/clients/${clients[0]!.id}`, { method: "DELETE" })).status).toBe(204);
    expect((await (await owner("/api/clients")).json()).items[0].revoked).toBe(true);
    expect(JSON.stringify(await (await owner("/api/admin/audit")).json())).toContain('client.revoke');
  });

  it("rejects unauthenticated protected requests without creating resources", async () => {
    const createHost = await createHostRequest({ name: "unauthenticated" });
    const createKey = await api("/api/apikeys", json("POST", { name: "unauthenticated" }));
    expect(createHost.status).toBe(401);
    expect(createKey.status).toBe(401);
    expect((await api("/api/me")).status).toBe(401);
    expect((await api("/api/hosts")).status).toBe(401);
    expect((await api("/api/templates")).status).toBe(401);
    expect((await api("/api/admin/users", json("POST", { name: "unauthenticated", password: "secret" }))).status).toBe(401);

    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    expect((await (await api("/api/hosts")).json()).items).toMatchObject([{ state: "stopped" }]);
    expect((await (await api("/api/apikeys")).json()).items).toEqual([]);
  });

  it("requires and replays an exact host creation idempotency key", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    await allowHosts(2);
    const body = { name: "retry host" };
    expect((await api("/api/hosts", json("POST", body))).status).toBe(400);
    expect((await createHostRequest({ name: "empty Workspace list", workspace_ids: [] })).status).toBe(400);

    const first = await createHostRequest(body, "host-retry-key-001");
    const second = await createHostRequest(body, "host-retry-key-001");
    expect(first.status).toBe(201);
    expect(second.status).toBe(201);
    expect(await first.json()).toMatchObject({ id: (await second.json() as { id: string }).id });

    const changed = await createHostRequest({ ...body, name: "different" }, "host-retry-key-001");
    expect(changed.status).toBe(409);
    expect((await changed.json()).error.code).toBe("idempotency_conflict");
    expect((await (await api("/api/hosts")).json()).items).toHaveLength(2);
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

    expect(await (await admin("/api/me")).json()).toMatchObject({ name: "admin", role: "super_admin", must_change_password: true });
    expect(await (await member("/api/me")).json()).toMatchObject({ name: "member", role: "user" });
    expect((await member("/api/admin/users")).status).toBe(403);

    const logout = await admin("/api/auth/logout", { method: "POST" });
    expect(logout.headers.get("set-cookie")).toContain("Max-Age=0");
    expect((await admin("/api/me")).status).toBe(401);
    expect(await (await member("/api/me")).json()).toMatchObject({ name: "member", role: "user" });
  });
});

describe("portal mock Host sharing", () => {
  it("grants shared host permissions, issues observation tickets, and revokes access", async () => {
    const owner = createApiClient();
    const member = createApiClient();
    await owner("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    await allowHosts(2, owner);
    const created = await owner("/api/hosts", {
      method: "POST", headers: { "content-type": "application/json", "Idempotency-Key": "share-host-key-001" },
      body: JSON.stringify({ name: "共享测试电脑" }),
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
    const sharedMCPKey = await member("/api/apikeys", json("POST", { name: "共享主机 MCP" }));
    expect(sharedMCPKey.status).toBe(201);

    expect((await owner(`/api/hosts/${host.id}/members/umember1`, { method: "DELETE" })).status).toBe(204);
    expect(await (await member("/api/shared-hosts")).json()).toEqual({ items: [] });
    expect((await member(`/api/hosts/${host.id}/observation-ticket`, { method: "POST" })).status).toBe(404);
  });

  it("gives a terminal-only member a terminal ticket without desktop access", async () => {
    const owner = createApiClient();
    const member = createApiClient();
    await owner("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    await allowHosts(2, owner);
    const created = await owner("/api/hosts", {
      method: "POST", headers: { "content-type": "application/json", "Idempotency-Key": "terminal-share-key-001" },
      body: JSON.stringify({ name: "终端专用共享" }),
    });
    const host = await created.json() as { id: string };
    await wait(950);
    expect((await owner(`/api/hosts/${host.id}/members`, json("POST", { user_id: "umember1", permissions: ["terminal:control"] }))).status).toBe(201);
    await member("/api/auth/login", json("POST", { name: "member", password: "member" }));

    const terminal = await member(`/api/hosts/${host.id}/observation-ticket?target=terminal`, { method: "POST" });
    expect(terminal.status).toBe(200);
    expect(await terminal.json()).toMatchObject({ url: expect.stringContaining("next=%2F__ch%2Fterminal%2F") });
    expect((await member(`/api/hosts/${host.id}/observation-ticket`, { method: "POST" })).status).toBe(404);
    expect((await member("/api/apikeys", json("POST", { name: "账号权限 Key" }))).status).toBe(201);
  });

  it("gives a Workspace-only member a fixed read-only entry without implying desktop access", async () => {
    const owner = createApiClient();
    const member = createApiClient();
    await owner("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    await allowHosts(2, owner);
    const created = await owner("/api/hosts", {
      method: "POST", headers: { "content-type": "application/json", "Idempotency-Key": "workspace-share-key-001" },
      body: JSON.stringify({ name: "工作区只读共享" }),
    });
    const host = await created.json() as { id: string };
    await wait(950);
    const grant = await owner(`/api/hosts/${host.id}/members`, json("POST", { user_id: "umember1", permissions: ["workspace:file:read"] }));
    expect(grant.status).toBe(201);
    expect(await grant.json()).toMatchObject({ permissions: ["workspace:file:read"] });

    await member("/api/auth/login", json("POST", { name: "member", password: "member" }));
    expect(await (await member("/api/shared-hosts")).json()).toMatchObject({ items: [{ id: host.id, permissions: ["workspace:file:read"] }] });
    const workspaceTicket = await member(`/api/hosts/${host.id}/observation-ticket?target=workspace`, { method: "POST" });
    expect(workspaceTicket.status).toBe(200);
    expect(await workspaceTicket.json()).toMatchObject({ url: expect.stringContaining("next=%2F__ch%2Fwb%2F%3Ftab%3Dworkspace") });
    expect((await member(`/api/hosts/${host.id}/observation-ticket`, { method: "POST" })).status).toBe(404);
    expect((await member(`/api/hosts/${host.id}/observation-ticket?target=workspace&target=desktop`, { method: "POST" })).status).toBe(400);

    expect((await owner(`/api/hosts/${host.id}/members/umember1`, { method: "DELETE" })).status).toBe(204);
    expect((await member(`/api/hosts/${host.id}/observation-ticket?target=workspace`, { method: "POST" })).status).toBe(404);
  });
});

describe("portal mock Host control", () => {
  it("keeps the Host fenced while tools stop, resumes the same request, and refreshes the returned version", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    await allowHosts(2);
    const created = await createHostRequest({ name: "控制切换测试" });
    const host = await created.json() as { id: string };
    await wait(950);
    await api("/api/hosts"); // Materialize the mock Host as running.

    const initial = await api(`/api/hosts/${host.id}/control`);
    expect(await initial.json()).toMatchObject({ host_id: host.id, control_state: "ai", control_version: 0, active_transition: null });
    const takeOver = { action: "take_over", request_id: "host-control-retry-001", expected_control_version: 0 };
    const pending = await api(`/api/hosts/${host.id}/control/actions`, json("POST", takeOver));
    expect(pending.status).toBe(202);
    expect(await pending.json()).toMatchObject({ control_state: "taking_over", control_version: 1, pending_operation_ids: [`op-${host.id}`] });
    expect(await (await api(`/api/hosts/${host.id}/control`)).json()).toMatchObject({ control_state: "taking_over", active_transition: { request_id: takeOver.request_id, failure_reason: "operations_pending" } });

    const completedTakeOver = await api(`/api/hosts/${host.id}/control/actions`, json("POST", takeOver));
    expect(completedTakeOver.status).toBe(200);
    expect(await completedTakeOver.json()).toMatchObject({ control_state: "user", control_version: 2, active_transition: null, pending_operation_ids: [] });
    expect(await (await api(`/api/hosts/${host.id}/control/actions`, json("POST", takeOver))).json()).toMatchObject({ control_state: "user", control_version: 2 });

    const release = { action: "release", request_id: "host-control-release-001", expected_control_version: 2 };
    expect((await api(`/api/hosts/${host.id}/control/actions`, json("POST", release))).status).toBe(202);
    const completedRelease = await api(`/api/hosts/${host.id}/control/actions`, json("POST", release));
    expect(completedRelease.status).toBe(200);
    expect(await completedRelease.json()).toMatchObject({ control_state: "ai", control_version: 4 });
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

describe("portal mock durable tasks", () => {
  it("accepts personal messages before routing and replays exact retries without creating a Task", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const blocked = await api("/api/conversations", { ...json("POST", {}),
      headers: { "content-type": "application/json", "Idempotency-Key": "conversation-create-01",
        "X-Morde-Service-Instance-ID": "00000000-0000-4000-8000-000000000001", "X-Morde-Actor-ID": "other-account" } });
    expect(blocked.status).toBe(409);
    const created = await api("/api/conversations", { ...json("POST", {}),
      headers: { "content-type": "application/json", "Idempotency-Key": "conversation-create-01",
        "X-Morde-Service-Instance-ID": "00000000-0000-4000-8000-000000000001", "X-Morde-Actor-ID": "u1a2b3c4" } });
    const conversation = await created.json() as { id: string };
    const path = `/api/conversations/${conversation.id}/messages`;
    const headers = { "content-type": "application/json", "Idempotency-Key": "first-message-1",
      "X-Morde-Service-Instance-ID": "00000000-0000-4000-8000-000000000001", "X-Morde-Actor-ID": "u1a2b3c4" };
    const body = { client_message_id: "first-message-1", content: "第一条" };
    const first = await api(path, { ...json("POST", body), headers });
    const result = await first.json() as { message: { id: string; sequence: number; task_id?: string; client_message_id: string; delivery_status: string; routing_status: string }; replayed: boolean };
    expect(first.status).toBe(202);
    expect(result).toMatchObject({ message: { client_message_id: body.client_message_id, delivery_status: "pending", routing_status: "pending" }, replayed: false });
    expect(result.message.task_id).toBeUndefined();
    const replay = await api(path, { ...json("POST", body), headers });
    expect(replay.status).toBe(202);
    expect(await replay.json()).toMatchObject({ message: { id: result.message.id }, replayed: true });
    expect((await api(path, { ...json("POST", { ...body, content: "变化" }), headers })).status).toBe(409);
    expect((await api(path, { ...json("POST", { ...body, mode: "new", model_id: "pmodel01" }), headers })).status).toBe(400);
    expect((await api(path, { ...json("POST", { ...body, input_revision: 5 }), headers })).status).toBe(400);
    expect((await api(`${path}?client_message_id=${body.client_message_id}`, { headers })).status).toBe(200);
    const secondBody = { client_message_id: "second-message-1", content: "第二条" };
    const second = await api(path, { ...json("POST", secondBody), headers: { ...headers, "Idempotency-Key": secondBody.client_message_id } });
    expect(second.status).toBe(202);
    const secondResult = await second.json() as { message: { id: string; task_id?: string; sequence: number } };
    expect(secondResult.message.task_id).toBeUndefined();
    expect(secondResult.message.sequence).toBeGreaterThan(result.message.sequence);
    expect((await (await api(path)).json()).items).toHaveLength(2);
    expect((await (await api(`/api/conversations/${conversation.id}/tasks`)).json()).items).toHaveLength(0);
    const member = createApiClient();
    await member("/api/auth/login", json("POST", { name: "member", password: "member" }));
    expect((await member(path)).status).toBe(404);
  });

  it("resumes and commits multilingual personal and project message uploads exactly once", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const serviceID = "00000000-0000-4000-8000-000000000001";
    const identityHeaders = { "X-Morde-Service-Instance-ID": serviceID, "X-Morde-Actor-ID": "u1a2b3c4" };
    const personalConversation = await api("/api/conversations", {
      ...json("POST", {}), headers: { ...identityHeaders, "content-type": "application/json", "Idempotency-Key": "upload-conversation-01" },
    });
    expect(personalConversation.status).toBe(201);
    const personalConversationID = (await personalConversation.json() as { id: string }).id;
    const projectConversation = await api("/api/projects/pdem0001/conversations", {
      ...json("POST", { title: "上传协议回归" }), headers: { "content-type": "application/json", "Idempotency-Key": "upload-project-conversation-01" },
    });
    expect(projectConversation.status).toBe(201);
    const projectConversationID = (await projectConversation.json() as { id: string }).id;
    const content = "长消息🙂内容".repeat(7_000);
    const bytes = new TextEncoder().encode(content);
    const chunkSize = 16 * 1024;
    const totalChunks = Math.ceil(bytes.length / chunkSize);

    for (const target of [
      { path: `/api/conversations/${personalConversationID}`, messages: `/api/conversations/${personalConversationID}/messages`, clientID: "upload-personal-01" },
      { path: `/api/projects/pdem0001/conversations/${projectConversationID}`, messages: `/api/projects/pdem0001/conversations/${projectConversationID}/messages`, clientID: "upload-project-01" },
    ]) {
      const uploadPath = `${target.path}/message-uploads/${target.clientID}`;
      const headers = { ...identityHeaders, "content-type": "application/json", "Idempotency-Key": target.clientID };
      const init = {
        total_bytes: bytes.length, total_chunks: totalChunks, sha256: createHash("sha256").update(bytes).digest("hex"),
        previous_client_message_id: "previous-message-01", model_id: "pmodel01",
      };
      const initialized = await api(uploadPath, { ...json("PUT", init), headers });
      expect(initialized.status).toBe(200);
      expect(await initialized.json()).toMatchObject({ client_message_id: target.clientID, state: "uploading", received_sequences: [] });
      const initReplay = await api(uploadPath, { ...json("PUT", init), headers });
      expect(initReplay.status).toBe(200);
      expect((await initReplay.json() as { received_sequences: number[] }).received_sequences).toEqual([]);
      expect((await api(uploadPath, { ...json("PUT", { ...init, model_id: "different-model" }), headers })).status).toBe(409);

      const sendChunk = async (sequence: number) => {
        const chunk = bytes.subarray(sequence * chunkSize, Math.min((sequence + 1) * chunkSize, bytes.length));
        return api(`${uploadPath}/chunks/${sequence}`, {
          ...json("PUT", { content_base64: Buffer.from(chunk).toString("base64"), sha256: createHash("sha256").update(chunk).digest("hex") }),
          headers: identityHeaders,
        });
      };
      expect((await sendChunk(totalChunks - 1)).status).toBe(204);
      expect((await sendChunk(totalChunks - 1)).status).toBe(204);
      const progress = await api(uploadPath, { headers: identityHeaders });
      expect(await progress.json()).toMatchObject({ state: "uploading", received_sequences: [totalChunks - 1] });
      expect((await api(`${uploadPath}/commit`, { method: "POST", headers: { ...identityHeaders, "Idempotency-Key": target.clientID } })).status).toBe(409);
      for (let sequence = 0; sequence < totalChunks - 1; sequence += 1) expect((await sendChunk(sequence)).status).toBe(204);

      const committed = await api(`${uploadPath}/commit`, { method: "POST", headers: { ...identityHeaders, "Idempotency-Key": target.clientID } });
      expect(committed.status).toBe(202);
      const receipt = await committed.json() as { message: { id: string; content: string; client_message_id: string; delivery_status: string } };
      expect(receipt.message).toMatchObject({ content, client_message_id: target.clientID, delivery_status: "pending" });
      const replay = await api(`${uploadPath}/commit`, { method: "POST", headers: { ...identityHeaders, "Idempotency-Key": target.clientID } });
      expect(await replay.json()).toEqual(receipt);
      const listed = await api(`${target.messages}?client_message_id=${target.clientID}`, { headers: identityHeaders });
      const persisted = (await listed.json() as { items: Array<{ id: string; content: string }> }).items;
      expect(persisted).toHaveLength(1);
      expect(persisted[0]).toMatchObject({ id: receipt.message.id, content });
    }
  });

  it("persists a queued Task, replays an exact retry, and protects conversation ownership", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const headers = { "content-type": "application/json", "Idempotency-Key": "task-submit-retry-001" };
    const body = { model_id: "pmodel01", objective: "你好", host_id: "h1234567", workspace_id: "w1234567", input_revision: 5 };
    const first = await api("/api/tasks", { ...json("POST", body), headers });
    expect(first.status).toBe(202);
    const saved = await first.json() as { id: string; conversation_id: string; status: string };
    expect(saved).toMatchObject({
      conversation_id: expect.any(String), status: "queued", host_id: "h1234567", workspace_id: "w1234567", input_revision: 5,
    });
    const replay = await api("/api/tasks", { ...json("POST", body), headers });
    expect(replay.status).toBe(202);
    expect(await replay.json()).toMatchObject({ id: saved.id, conversation_id: saved.conversation_id });
    const history = await api(`/api/conversations/${saved.conversation_id}/messages`);
    expect(await history.json()).toMatchObject({ items: [{ content: "你好", role: "user", task_id: saved.id, delivery_status: "applied" }] });
    expect(await (await api(`/api/tasks/${saved.id}`)).json()).toMatchObject({
      id: saved.id, status: "queued", host_id: "h1234567", workspace_id: "w1234567", input_revision: 5,
    });
    expect(await (await api("/api/conversations")).json()).toMatchObject({ items: [{ id: saved.conversation_id, title: "你好" }] });
    const member = createApiClient();
    await member("/api/auth/login", json("POST", { name: "member", password: "member" }));
    expect((await member(`/api/conversations/${saved.conversation_id}/messages`)).status).toBe(404);
    expect((await member(`/api/tasks/${saved.id}`)).status).toBe(404);

    resetMockState({ restoreAssistantState: true });
    api = createApiClient();
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const restored = await api(`/api/conversations/${saved.conversation_id}/messages`);
    expect((await restored.json()).items).toHaveLength(1);
    expect((await (await api(`/api/tasks/${saved.id}`)).json()).status).toBe("queued");
  });

  it("issues a read-only observation ticket only for the current owner and matching Host Task", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const hostList = await api("/api/hosts");
    const host = (await hostList.json() as { items: Array<{ id: string }> }).items[0]!;
    const workspaceResponse = await createWorkspace("Task observation workspace");
    const workspace = await workspaceResponse.json() as { id: string };
    const bound = await api(`/api/hosts/${host.id}/workspace-bindings`, {
      ...json("POST", { workspace_id: workspace.id, expected_revision: 0, expected_binding_version: 0 }),
      headers: { "content-type": "application/json", "Idempotency-Key": "task-observe-bind-001" },
    });
    expect(bound.status).toBe(201);
    await api(`/api/hosts/${host.id}/restore`, { method: "POST" });
    await wait(950);
    await api("/api/hosts");
    const created = await api("/api/tasks", {
      ...json("POST", { model_id: "pmodel01", objective: "observe", host_id: host.id, workspace_id: workspace.id, input_revision: 0 }),
      headers: { "content-type": "application/json", "Idempotency-Key": "task-observe-submit-01" },
    });
    expect(created.status).toBe(202);
    const task = await created.json() as { id: string; host_id: string };
    const ticketResponse = await api(`/api/tasks/${task.id}/workbench-observation-ticket`, { method: "POST" });
    expect(ticketResponse.status).toBe(200);
    expect(await ticketResponse.json()).toMatchObject({
      task_id: task.id, host_id: task.host_id, ticket: expect.stringContaining("mock-one-time-task-ticket"),
      expires_at: expect.any(String),
    });

    const member = createApiClient();
    await member("/api/auth/login", json("POST", { name: "member", password: "member" }));
    expect((await member(`/api/tasks/${task.id}/workbench-observation-ticket`, { method: "POST" })).status).toBe(404);

    const noHost = await api("/api/tasks", {
      ...json("POST", { model_id: "pmodel01", objective: "cloud only" }),
      headers: { "content-type": "application/json", "Idempotency-Key": "task-observe-cloud-01" },
    });
    const cloudTask = await noHost.json() as { id: string };
    expect((await api(`/api/tasks/${cloudTask.id}/workbench-observation-ticket`, { method: "POST" })).status).toBe(404);
  });

  it("records a pending cancellation, replays the same action, and rejects stale status", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const taskResponse = await api("/api/tasks", {
      ...json("POST", { model_id: "pmodel01", objective: "停止任务测试" }),
      headers: { "content-type": "application/json", "Idempotency-Key": "task-stop-submit-01" },
    });
    const task = await taskResponse.json() as { id: string };
    const actionBody = { action: "cancel", expected_status: "queued" };
    const headers = { "content-type": "application/json", "Idempotency-Key": "task-stop-action-01" };

    const response = await api(`/api/tasks/${task.id}/actions`, { ...json("POST", actionBody), headers });
    expect(response.status).toBe(202);
    const accepted = await response.json();
    expect(accepted).toMatchObject({ task_id: task.id, action: "cancel", status: "cancelling", unsettled_operation_ids: [] });
    expect(await (await api(`/api/tasks/${task.id}`)).json()).toMatchObject({
      status: "cancelling", stop_request: { action: "cancel", status: "pending", unsettled_operation_ids: [] },
    });
    const replay = await api(`/api/tasks/${task.id}/actions`, { ...json("POST", actionBody), headers });
    expect(replay.status).toBe(202);
    expect(await replay.json()).toEqual(accepted);
    const stale = await api(`/api/tasks/${task.id}/actions`, {
      ...json("POST", actionBody), headers: { ...headers, "Idempotency-Key": "task-stop-action-02" },
    });
    expect(stale.status).toBe(409);
    expect((await stale.json()).error).toMatchObject({ code: "conflict" });
  });

  it("atomically compares task revision fences and replays the exact fenced action", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const createTask = async (objective: string, key: string) => {
      const response = await api("/api/tasks", {
        ...json("POST", { model_id: "pmodel01", objective }),
        headers: { "content-type": "application/json", "Idempotency-Key": key },
      });
      expect(response.status).toBe(202);
      return await response.json() as { id: string };
    };

    for (const staleField of ["expected_current_revision", "expected_desired_revision"] as const) {
      const task = await createTask(`修订条件测试 ${staleField}`, `task-stop-${staleField}-submit`);
      const initialResponse = await api(`/api/tasks/${task.id}`);
      const initial = await initialResponse.json() as {
        status: string; event_sequence: number; current_revision?: number; desired_revision?: number;
      };
      const currentRevision = initial.current_revision ?? 0;
      const desiredRevision = initial.desired_revision ?? 0;
      const expected = { expected_current_revision: currentRevision, expected_desired_revision: desiredRevision };
      const key = `task-stop-${staleField}-fence`;

      const invalidPair = await api(`/api/tasks/${task.id}/actions`, {
        ...json("POST", { action: "cancel", expected_status: "queued", expected_current_revision: currentRevision }),
        headers: { "content-type": "application/json", "Idempotency-Key": key },
      });
      expect(invalidPair.status).toBe(400);
      const stalePair = {
        ...expected,
        [staleField]: expected[staleField] + 1,
      };
      const stale = await api(`/api/tasks/${task.id}/actions`, {
        ...json("POST", { action: "cancel", expected_status: "queued", ...stalePair }),
        headers: { "content-type": "application/json", "Idempotency-Key": key },
      });
      expect(stale.status).toBe(409);
      expect((await stale.json()).error).toMatchObject({ code: "conflict" });

      const unchangedResponse = await api(`/api/tasks/${task.id}`);
      const unchanged = await unchangedResponse.json();
      expect(unchanged).toMatchObject({
        status: "queued", event_sequence: initial.event_sequence,
        current_revision: currentRevision, desired_revision: desiredRevision,
      });

      const matchingBody = { action: "cancel", expected_status: "queued", ...expected };
      const accepted = await api(`/api/tasks/${task.id}/actions`, {
        ...json("POST", matchingBody), headers: { "content-type": "application/json", "Idempotency-Key": key },
      });
      expect(accepted.status).toBe(202);
      const acceptedBody = await accepted.json();
      expect(acceptedBody).toMatchObject({ task_id: task.id, action: "cancel", status: "cancelling" });
      const replay = await api(`/api/tasks/${task.id}/actions`, {
        ...json("POST", matchingBody), headers: { "content-type": "application/json", "Idempotency-Key": key },
      });
      expect(replay.status).toBe(202);
      expect(await replay.json()).toEqual(acceptedBody);
      expect(await (await api(`/api/tasks/${task.id}`)).json()).toMatchObject({
        status: "cancelling", event_sequence: initial.event_sequence + 1,
      });
    }
  });
});

describe("portal mock quota and capacity", () => {
  it("counts stopped hosts toward the host limit and rejects resource and cluster overages", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const templateResponse = await api("/api/templates");
    const availableTemplates = await templateResponse.json() as { items: Array<{ id: string; resources: { cpu_limit_milli: number; mem_limit_mib: number } }> };
    expect(availableTemplates.items.map((template) => template.id)).toEqual(["std-2c4g", "std-1c2g", "std-2c2g"]);
    expect(availableTemplates.items.slice(1).map((template) => [template.resources.cpu_limit_milli, template.resources.mem_limit_mib])).toEqual([[1000, 2048], [2000, 2048]]);
    const initialCapacity = await (await api("/api/admin/capacity")).json();
    expect(initialCapacity.allocated).toEqual({ cpu_milli: 0, mem_mib: 0 });
    const me = await (await api("/api/me")).json();
    expect(me.usage).toEqual({ hosts: 1, cpu_milli: 0, mem_mib: 0 });
    const secondHost = await createHostRequest({ name: "beyond host-count quota" });
    expect(secondHost.status).toBe(409);
    expect((await secondHost.json()).error).toMatchObject({ code: "quota_exceeded", message: "达到云端电脑上限,无法再创建" });

    await allowHosts(2, api, 2000, 4096);
    expect((await createHostRequest({ name: "within raised quota" })).status).toBe(201);
    const afterSecondHost = await (await api("/api/me")).json();
    expect(afterSecondHost.usage).toEqual({ hosts: 2, cpu_milli: 1000, mem_mib: 2048 });
    const creatingCapacity = await (await api("/api/admin/capacity")).json();
    expect(creatingCapacity.allocated).toEqual({ cpu_milli: 1000, mem_mib: 2048 });
    expect(creatingCapacity.hosts_by_state).toEqual({ creating: 1, stopped: 1 });
    await allowHosts(3, api, 1500, 3072);
    const resourceQuotaRejected = await createHostRequest({ name: "over resource quota" });
    expect(resourceQuotaRejected.status).toBe(409);
    expect((await resourceQuotaRejected.json()).error).toMatchObject({ code: "quota_exceeded", details: { dimension: "cpu_milli", limit: 1500 } });

    await allowHosts(100, api, 100000, 200000);
    for (let index = 1; index < 48; index += 1) {
      const response = await createHostRequest({ name: `capacity test ${index}` });
      expect(response.status).toBe(201);
    }
    const capacityRejected = await createHostRequest({ name: "over capacity" });
    expect(capacityRejected.status).toBe(409);
    expect((await capacityRejected.json()).error).toMatchObject({ code: "capacity_exceeded", details: { dimension: "cpu_milli", limit: 48000 } });
  });

  it("filters synthetic audit records and applies the requested result limit", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const since = encodeURIComponent(new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString());
    const filtered = await api(`/api/admin/audit?actor=u1a2b3c4&action=host.create&target=h-audit-demo&since=${since}&limit=1`);
    expect(filtered.status).toBe(200);
    expect((await filtered.json()).items).toEqual([expect.objectContaining({ action: "host.create", actor_id: "u1a2b3c4", target: "h-audit-demo" })]);

    const limited = await api("/api/admin/audit?limit=2");
    expect((await limited.json()).items).toHaveLength(2);
  });

  it("hides another user's host, workspace, and API Key by ID", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    await allowHosts(2);
    const workspaceResponse = await createWorkspace("private workspace");
    const workspace = await workspaceResponse.json();
    const hostResponse = await createHostRequest({ name: "private host", workspace_ids: [workspace.id] });
    const host = await hostResponse.json();
    const keyResponse = await api("/api/apikeys", json("POST", { name: "owner key" }));
    const key = await keyResponse.json();
    expect(hostResponse.status).toBe(201);
    expect(keyResponse.status).toBe(201);

    await api("/api/auth/login", json("POST", { name: "member", password: "member" }));
    expect((await (await api("/api/hosts")).json()).items).toMatchObject([{ state: "stopped" }]);
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
    await allowHosts(2);
    const created = await createHostRequest({ name: "entry ticket" });
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
    await allowHosts(2);
    const workspaceResponse = await createWorkspace("retained workspace");
    const workspace = await workspaceResponse.json();
    const created = await createHostRequest({ name: "retained workspace", workspace_ids: [workspace.id] });
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
    expect((await (await api("/api/me")).json()).usage.hosts).toBe(2);

    await wait(950);
    const workspacesAfterNavigation = await (await api("/api/workspaces")).json();
    expect(workspacesAfterNavigation.items).toContainEqual(expect.objectContaining({ id: workspaceId, state: "ready" }));
    expect((await api(`/api/hosts/${hostId}`)).status).toBe(404);
    expect((await (await api("/api/hosts")).json()).items).not.toContainEqual(expect.objectContaining({ id: hostId }));
    expect((await (await api("/api/me")).json()).usage.hosts).toBe(1);

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

  it("keeps a Host allocated during deletion and retains its Workspace", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    await allowHosts(2);
    const created = await createHostRequest({ name: "deletion accounting" });
    expect(created.status).toBe(201);
    const host = await created.json() as { id: string; workspaces: Array<{ workspace: { id: string } }> };
    expect(host.workspaces).toHaveLength(1);
    const workspaceID = host.workspaces[0]!.workspace.id;
    const before = await (await api("/api/me")).json();

    const deleted = await api(`/api/hosts/${host.id}`, { method: "DELETE" });
    const deletingHost = await deleted.json();
    expect(deleted.status).toBe(202);
    expect(deletingHost.state).toBe("deleting");
    expect(deletingHost.workspaces).toHaveLength(1);
    expect(deletingHost.workspaces[0].workspace.id).toBe(workspaceID);
    expect((await (await api(`/api/hosts/${host.id}`)).json()).state).toBe("deleting");
    expect((await (await api("/api/workspaces")).json()).items).toContainEqual(expect.objectContaining({ id: workspaceID, state: "ready" }));
    expect((await (await api("/api/me")).json()).usage.hosts).toBe(before.usage.hosts);

    await wait(950);
    expect((await api(`/api/hosts/${host.id}`)).status).toBe(404);
    expect((await (await api("/api/me")).json()).usage.hosts).toBe(before.usage.hosts - 1);
  });

  it("releases compute and restores the same Host with the same workspace and a new generation", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    await allowHosts(2);
    const workspaceResponse = await createWorkspace("persistent workspace");
    const workspace = await workspaceResponse.json();
    const created = await createHostRequest({ name: "persistent computer", workspace_ids: [workspace.id] });
    expect(created.status).toBe(201);
    const initial = await created.json();
    expect(initial.runtime_generation).toBe(1);

    await wait(950);
    expect((await (await api("/api/hosts")).json()).items[0].state).toBe("running");
    const released = await api(`/api/hosts/${initial.id}/release`, { method: "POST" });
    expect(released.status).toBe(202);
    expect((await released.json()).state).toBe("stopping");
    expect((await (await api("/api/me")).json()).usage.hosts).toBe(2);

    await wait(950);
    const stopped = await (await api(`/api/hosts/${initial.id}`)).json();
    expect(stopped).toMatchObject({ id: initial.id, state: "stopped", runtime_generation: 1 });
    expect(stopped.workspaces[0].workspace.id).toBe(workspace.id);
    expect((await (await api("/api/me")).json()).usage.hosts).toBe(2);

    const restored = await api(`/api/hosts/${initial.id}/restore`, { method: "POST" });
    expect(restored.status).toBe(202);
    expect(await restored.json()).toMatchObject({ id: initial.id, state: "creating", runtime_generation: 2 });
    await wait(950);
    const running = await (await api(`/api/hosts/${initial.id}`)).json();
    expect(running).toMatchObject({ id: initial.id, state: "running", runtime_generation: 2 });
    expect(running.workspaces[0].workspace.id).toBe(workspace.id);
    expect((await (await api("/api/me")).json()).usage.hosts).toBe(2);
  });
});

describe("portal mock workspace bindings", () => {
  it("binds an owned workspace idempotently and exposes only empty owner-scoped device progress", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    await allowHosts(2);
    const workspaceResponse = await createWorkspace("bindable workspace");
    const workspace = await workspaceResponse.json() as { id: string; revision: number; used_bytes: number; quota_bytes: number };
    const hostResponse = await createHostRequest({ name: "workspace host", workspace_ids: [workspace.id] });
    const host = await hostResponse.json() as { id: string };
    await wait(950);

    const headers = { "content-type": "application/json", "Idempotency-Key": "workspace-bind-001" };
    const stale = await api(`/api/hosts/${host.id}/workspace-bindings`, { ...json("POST", { workspace_id: workspace.id, expected_revision: workspace.revision + 1, expected_binding_version: 1 }), headers });
    expect(stale.status).toBe(409);
    expect((await stale.json()).error.code).toBe("workspace_conflict");

    const request = { workspace_id: workspace.id, expected_revision: workspace.revision, expected_binding_version: 1 };
    const bound = await api(`/api/hosts/${host.id}/workspace-bindings`, { ...json("POST", request), headers });
    const binding = await bound.json();
    expect(bound.status).toBe(201);
    expect(binding).toMatchObject({ workspace: { id: workspace.id, revision: workspace.revision }, state: "active", desired_revision: workspace.revision, applied_revision: workspace.revision });

    const replay = await api(`/api/hosts/${host.id}/workspace-bindings`, { ...json("POST", request), headers });
    expect(replay.status).toBe(201);
    expect(await replay.json()).toEqual(binding);
    const hostAfterBinding = await (await api(`/api/hosts/${host.id}`)).json();
    expect(hostAfterBinding.workspaces).toHaveLength(1);

    const otherWorkspaceResponse = await createWorkspace("replacement workspace");
    const otherWorkspace = await otherWorkspaceResponse.json() as { id: string; revision: number };
    const replacement = await api(`/api/hosts/${host.id}/workspace-bindings`, {
      ...json("POST", { workspace_id: otherWorkspace.id, expected_revision: otherWorkspace.revision, expected_binding_version: 0 }),
      headers: { ...headers, "Idempotency-Key": "workspace-bind-other-01" },
    });
    expect(replacement.status).toBe(409);
    expect((await replacement.json()).error.code).toBe("workspace_conflict");

    const staleRetry = await api(`/api/hosts/${host.id}/workspace-bindings`, { ...json("POST", { ...request, expected_binding_version: 0 }), headers: { ...headers, "Idempotency-Key": "workspace-bind-002" } });
    expect(staleRetry.status).toBe(409);
    expect((await staleRetry.json()).error.code).toBe("workspace_conflict");

    const progress = await api(`/api/workspaces/${workspace.id}/sync-progress`);
    expect(await progress.json()).toEqual({ revision: workspace.revision, used_bytes: workspace.used_bytes, quota_bytes: workspace.quota_bytes,
      remaining_bytes: Math.max(0, workspace.quota_bytes - workspace.used_bytes), quota_status: "available", items: [], quota_pauses: [] });
    const changedReplay = await api(`/api/hosts/${host.id}/workspace-bindings`, { ...json("POST", { workspace_id: "w-other", expected_revision: 0, expected_binding_version: 0 }), headers });
    expect(changedReplay.status).toBe(409);
    expect((await changedReplay.json()).error.code).toBe("idempotency_conflict");

    await api("/api/auth/login", json("POST", { name: "member", password: "member" }));
    expect((await api(`/api/workspaces/${workspace.id}/sync-progress`)).status).toBe(404);
  });

  it("keeps the single Workspace on a stopped Host through restore", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    await allowHosts(2);
    const workspaceResponse = await createWorkspace("stopped host workspace");
    const workspace = await workspaceResponse.json() as { id: string; revision: number };
    const hostResponse = await createHostRequest({ name: "stopped workspace host", workspace_ids: [workspace.id] });
    const host = await hostResponse.json() as { id: string };
    await waitForHostState(host.id, "running");
    await api(`/api/hosts/${host.id}/release`, json("POST", { confirm_process_termination: true }));
    await waitForHostState(host.id, "stopped");

    const restore = await api(`/api/hosts/${host.id}/restore`, json("POST", {}));
    expect(restore.status).toBe(202);
    await wait(950);
    expect((await (await api(`/api/hosts/${host.id}`)).json()).workspaces[0]).toMatchObject({ state: "active", applied_revision: workspace.revision });
  });

  it("accepts unbinding only with current version and writer confirmation while retaining workspace data", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    await allowHosts(2);
    const workspaceResponse = await createWorkspace("retained workspace");
    const workspace = await workspaceResponse.json() as { id: string; revision: number; state: string };
    const hostResponse = await createHostRequest({ name: "unbind host", workspace_ids: [workspace.id] });
    const host = await hostResponse.json() as { id: string };
    await wait(950);
    const bindHeaders = { "content-type": "application/json", "Idempotency-Key": "workspace-unbind-bind-01" };
    const bindBody = { workspace_id: workspace.id, expected_revision: workspace.revision, expected_binding_version: 1 };
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
    await allowHosts(2);
    const workspaceResponse = await createWorkspace("sync conflict workspace");
    const workspace = await workspaceResponse.json() as { id: string; revision: number };
    const hostResponse = await createHostRequest({ name: "sync conflict host", workspace_ids: [workspace.id] });
    const host = await hostResponse.json() as { id: string };
    await wait(950);
    const bindHeaders = { "content-type": "application/json", "Idempotency-Key": "workspace-sync-bind-01" };
    const boundResponse = await api(`/api/hosts/${host.id}/workspace-bindings`, {
      ...json("POST", { workspace_id: workspace.id, expected_revision: workspace.revision, expected_binding_version: 1 }), headers: bindHeaders,
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
