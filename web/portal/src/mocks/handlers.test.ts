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
});

describe("admin console mock routes", () => {
  it("records invitation attribution without bypassing review and keeps admin defaults safe", async () => {
    await api("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    const settings = await api("/api/admin/settings");
    expect(await settings.json()).toMatchObject({
      registration_review_required: true, automatic_new_user_model_budget: false,
      automatic_invitation_reward: false, automatic_contribution_reward: false,
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
    expect((await api("/api/model-connections", json("POST", {
      name: "未授权连接", base_url: "https://models.example.test/v1", model_name: "test-model", api_key: "local-test-secret", shared: true,
    }))).status).toBe(403);
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

  it("gives a Workspace-only member a fixed read-only entry without implying desktop access", async () => {
    const owner = createApiClient();
    const member = createApiClient();
    await owner("/api/auth/login", json("POST", { name: "demo", password: "demo" }));
    await allowHosts(2, owner);
    const created = await owner("/api/hosts", {
      method: "POST", headers: { "content-type": "application/json", "Idempotency-Key": "workspace-share-key-001" },
      body: JSON.stringify({ name: "工作区只读共享", workspace_ids: [] }),
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
    const created = await createHostRequest({ name: "控制切换测试", workspace_ids: [] });
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
    expect(await history.json()).toMatchObject({ items: [{ content: "你好", role: "user", task_id: saved.id }] });
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
      remaining_bytes: Math.max(0, workspace.quota_bytes - workspace.used_bytes), quota_status: "available", items: [], quota_pauses: [] });
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
