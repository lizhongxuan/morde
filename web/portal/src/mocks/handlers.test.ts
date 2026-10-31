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
const allowHosts = async (count: number, client: typeof api = api, cpuMilli = 4000, memMiB = 8192) => {
  const updated = await client("/api/admin/users/u1a2b3c4", json("PATCH", {
    expected_status: "approved", quota: { hosts: count, cpu_milli: cpuMilli, mem_mib: memMiB },
  }));
  expect(updated.status).toBe(200);
};

beforeAll(() => server.listen());
beforeEach(() => { resetMockState(); api = createApiClient(); workspaceSequence = 0; hostSequence = 0; });
afterEach(() => { server.resetHandlers(); vi.restoreAllMocks(); });
afterAll(() => server.close());

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

    const approved = await admin(`/api/admin/users/${user.id}`, json("PATCH", { expected_status: "pending", status: "approved" }));
    expect(approved.status).toBe(200);
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

    const rejectedRegistration = await account("/api/auth/register", json("POST", { name: "rejected-me", password: "secret-pass" }));
    const rejectedUser = (await rejectedRegistration.json() as { user: { id: string } }).user;
    expect((await admin(`/api/admin/users/${rejectedUser.id}`, json("PATCH", { expected_status: "pending", status: "rejected" }))).status).toBe(200);
    expect((await account("/api/auth/login", json("POST", { name: "rejected-me", password: "wrong-pass" }))).status).toBe(401);
    const rejectedLogin = await account("/api/auth/login", json("POST", { name: "rejected-me", password: "secret-pass" }));
    expect(rejectedLogin.status).toBe(403);
    expect((await rejectedLogin.json()).error.code).toBe("account_rejected");
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
    const createHost = await createHostRequest({ name: "unauthenticated", workspace_ids: [] });
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
    const body = { name: "retry host", workspace_ids: [] };
    expect((await api("/api/hosts", json("POST", body))).status).toBe(400);

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
      body: JSON.stringify({ name: "共享测试电脑", workspace_ids: [] }),
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
      body: JSON.stringify({ name: "终端专用共享", workspace_ids: [] }),
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
  it("persists a queued Task, replays an exact retry, and protects conversation ownership", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const headers = { "content-type": "application/json", "Idempotency-Key": "task-submit-retry-001" };
    const body = { model_id: "pmodel01", objective: "你好" };
    const first = await api("/api/tasks", { ...json("POST", body), headers });
    expect(first.status).toBe(202);
    const saved = await first.json() as { id: string; conversation_id: string; status: string };
    expect(saved).toMatchObject({ conversation_id: expect.any(String), status: "queued" });
    const replay = await api("/api/tasks", { ...json("POST", body), headers });
    expect(replay.status).toBe(202);
    expect(await replay.json()).toMatchObject({ id: saved.id, conversation_id: saved.conversation_id });
    const history = await api(`/api/conversations/${saved.conversation_id}/messages`);
    expect(await history.json()).toMatchObject({ items: [{ content: "你好", role: "user", task_id: saved.id }] });
    expect(await (await api(`/api/tasks/${saved.id}`)).json()).toMatchObject({ id: saved.id, status: "queued" });
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
    const secondHost = await createHostRequest({ name: "beyond host-count quota", workspace_ids: [] });
    expect(secondHost.status).toBe(409);
    expect((await secondHost.json()).error).toMatchObject({ code: "quota_exceeded", message: "达到云端电脑上限,无法再创建" });

    await allowHosts(2, api, 2000, 4096);
    expect((await createHostRequest({ name: "within raised quota", workspace_ids: [] })).status).toBe(201);
    const afterSecondHost = await (await api("/api/me")).json();
    expect(afterSecondHost.usage).toEqual({ hosts: 2, cpu_milli: 1000, mem_mib: 2048 });
    const creatingCapacity = await (await api("/api/admin/capacity")).json();
    expect(creatingCapacity.allocated).toEqual({ cpu_milli: 1000, mem_mib: 2048 });
    expect(creatingCapacity.hosts_by_state).toEqual({ creating: 1, stopped: 1 });
    await allowHosts(3, api, 1500, 3072);
    const resourceQuotaRejected = await createHostRequest({ name: "over resource quota", workspace_ids: [] });
    expect(resourceQuotaRejected.status).toBe(409);
    expect((await resourceQuotaRejected.json()).error).toMatchObject({ code: "quota_exceeded", details: { dimension: "cpu_milli", limit: 1500 } });

    await allowHosts(100, api, 100000, 200000);
    for (let index = 1; index < 48; index += 1) {
      const response = await createHostRequest({ name: `capacity test ${index}`, workspace_ids: [] });
      expect(response.status).toBe(201);
    }
    const capacityRejected = await createHostRequest({ name: "over capacity", workspace_ids: [] });
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

  it("keeps a Host allocated while deletion finishes when no Workspace is bound", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    await allowHosts(2);
    const created = await createHostRequest({ name: "deletion accounting", workspace_ids: [] });
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
    const hostResponse = await createHostRequest({ name: "workspace host", workspace_ids: [] });
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
    expect(await progress.json()).toEqual({ revision: workspace.revision, used_bytes: workspace.used_bytes, quota_bytes: workspace.quota_bytes,
      remaining_bytes: Math.max(0, workspace.quota_bytes - workspace.used_bytes), quota_status: "available", items: [] });
    const changedReplay = await api(`/api/hosts/${host.id}/workspace-bindings`, { ...json("POST", { workspace_id: "w-other", expected_revision: 0, expected_binding_version: 0 }), headers });
    expect(changedReplay.status).toBe(409);
    expect((await changedReplay.json()).error.code).toBe("idempotency_conflict");

    await api("/api/auth/login", json("POST", { name: "member", password: "member" }));
    expect((await api(`/api/workspaces/${workspace.id}/sync-progress`)).status).toBe(404);
  });

  it("accepts unbinding only with current version and writer confirmation while retaining workspace data", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    await allowHosts(2);
    const workspaceResponse = await createWorkspace("retained workspace");
    const workspace = await workspaceResponse.json() as { id: string; revision: number; state: string };
    const hostResponse = await createHostRequest({ name: "unbind host", workspace_ids: [] });
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
    await allowHosts(2);
    const workspaceResponse = await createWorkspace("sync conflict workspace");
    const workspace = await workspaceResponse.json() as { id: string; revision: number };
    const hostResponse = await createHostRequest({ name: "sync conflict host", workspace_ids: [] });
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
