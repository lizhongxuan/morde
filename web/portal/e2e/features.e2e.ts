import { expect, test, type Page } from "@playwright/test";

const portalUrl = process.env.CH_PORTAL_E2E_URL ?? "http://127.0.0.1:5183";

async function signIn(page: Page, username: "demo" | "admin" = "demo") {
  await page.goto("/");
  await submitLogin(page, username, username);
  await page.getByRole("link", { name: "AI 助手", exact: true }).click();
  await expect(page.getByRole("heading", { name: "AI 助手" })).toBeVisible();
  await page.getByRole("link", { name: "云端电脑", exact: true }).click();
  await expect(page.getByRole("heading", { name: "云端电脑", exact: true })).toBeVisible();
}

async function submitLogin(page: Page, username: string, password: string) {
  await page.getByLabel("用户名").fill(username);
  await page.getByLabel("密码").fill(password);
  await page.getByRole("button", { name: "登录" }).click();
}

async function allowMoreHosts(page: Page) {
  const status = await page.evaluate(async () => (await fetch('/api/admin/users/u1a2b3c4', {
    method: 'PATCH', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ expected_status: 'approved', quota: { hosts: 3, cpu_milli: 4000, mem_mib: 8192 } }),
  })).status);
  expect(status).toBe(200);
  await page.reload();
  await expect(page.getByRole('link', { name: '创建电脑' })).toBeVisible();
}

test.describe("Portal feature mock flows", () => {
  test.skip(process.env.CH_REAL_E2E === "1", "mock scenario is disabled for a real-host run");

  test("[mock] restores an uncertain Host create after browser reload with the same idempotency key", async ({ browser }) => {
    const user = { id: "u1234567", name: "demo", role: "member", quota: { hosts: 3, cpu_milli: 6000, mem_mib: 12288 }, usage: { hosts: 0, cpu_milli: 0, mem_mib: 0 } };
    const workspace = { id: "w7654321", owner_id: user.id, name: "接力项目工作区", revision: 0, used_bytes: 0, quota_bytes: 1048576, state: "ready", created_at: "", updated_at: "" };
    const hostKeys: string[] = [];
    const workspaceKeys: string[] = [];
    let workspaceCreates = 0;
    const context = await browser.newContext({ serviceWorkers: "block" });
    await context.route("**/src/main.tsx*", async (route) => {
      const response = await route.fetch();
      const source = await response.text();
      const withoutMockWorker = source.replace('if (import.meta.env.DEV && import.meta.env.VITE_MOCK_API === "1")', "if (false)");
      expect(withoutMockWorker).not.toBe(source);
      await route.fulfill({ response, body: withoutMockWorker });
    });
    await context.route("**/api/me", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(user) }));
    await context.route("**/api/templates", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [{
      id: "std", name: "标准规格", description: "", isolation: "container",
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
      workspace_size_mib: 1024, ttl_seconds: 3600, idle_timeout_seconds: 0, on_idle: "disabled", keep_workspace_default: true,
    }] }) }));
    await context.route("**/api/workspaces", async (route) => {
      if (route.request().method() === "GET") return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [] }) });
      workspaceCreates++;
      workspaceKeys.push(route.request().headers()["idempotency-key"] ?? "");
      return route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify(workspace) });
    });
    await context.route("**/api/hosts", async (route) => {
      if (route.request().method() === "GET") return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [] }) });
      hostKeys.push(route.request().headers()["idempotency-key"] ?? "");
      if (hostKeys.length === 1) return route.abort("failed");
      return route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: "h1234567", state: "creating" }) });
    });
    const page = await context.newPage();
    try {
      await page.goto(`${portalUrl}/hosts/new`);
      await expect(page.getByRole("heading", { name: "创建电脑" })).toBeVisible();
      await page.getByLabel(/主机名称/).fill("待恢复创建");
      await page.getByRole("button", { name: "创建主机" }).click();
      await expect(page.getByRole("button", { name: "继续上次创建" })).toBeVisible();
      await page.reload();
      await expect(page.getByRole("status").filter({ hasText: "上次创建结果尚未确认" })).toBeVisible();
      await expect(page.getByLabel(/主机名称/)).toHaveValue("待恢复创建");
      await page.getByRole("button", { name: "继续上次创建" }).click();
      await expect.poll(() => hostKeys.length).toBe(2);
      expect(hostKeys[1]).toBe(hostKeys[0]);
      expect(workspaceKeys).toHaveLength(1);
      expect(workspaceKeys[0]).toMatch(/^[A-Za-z0-9._~-]{8,128}$/);
    } finally {
      await context.close();
    }
  });

  test("[mock] detects a new Host sync conflict and resumes it after an owner retry", async ({ browser }) => {
    const user = {
      id: "u1234567", name: "demo", role: "member",
      quota: { hosts: 3, cpu_milli: 6000, mem_mib: 12288 }, usage: { hosts: 0, cpu_milli: 0, mem_mib: 0 },
    };
    const workspace = {
      id: "w1234567", owner_id: user.id, name: "接力项目", state: "ready", revision: 2,
      used_bytes: 4096, quota_bytes: 1024 * 1024, last_error: "", created_at: "2026-10-24T00:00:00Z", updated_at: "2026-10-24T00:00:00Z",
    };
    let conflict = false;
    let retryBody: { expected_binding_version: number } | undefined;
    const host = {
      id: "h1234567", name: "接力电脑", owner_id: user.id, template_id: "std-2c4g", isolation: "container", state: "running",
      state_reason: "", runtime_generation: 1, control_version: 1, control_state: "ai", url: "http://127.0.0.1:5184",
      workspaces: [{
        workspace: { id: workspace.id, name: workspace.name, revision: workspace.revision, used_bytes: workspace.used_bytes, state: workspace.state },
        directory: `projects/${workspace.id}`, state: "active", binding_version: 7, desired_revision: 2, applied_revision: 2,
        sync_state: "healthy", sync_conflicts: [],
      }],
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
      expires_at: null, last_active_at: null, created_at: "2026-10-24T00:00:00Z",
    };

    const context = await browser.newContext({ serviceWorkers: "block" });
    await context.route("**/src/main.tsx*", async (route) => {
      const response = await route.fetch();
      const source = await response.text();
      const withoutMockWorker = source.replace(
        'if (import.meta.env.DEV && import.meta.env.VITE_MOCK_API === "1")',
        "if (false)",
      );
      expect(withoutMockWorker).not.toBe(source);
      await route.fulfill({ response, body: withoutMockWorker });
    });
    await context.route("**/api/me", (route) => route.fulfill({ status: 401, contentType: "application/json", body: JSON.stringify({ error: { code: "unauthenticated", message: "请先登录", details: {} } }) }));
    await context.route("**/api/auth/login", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ user }) }));
    await context.route("**/api/workspaces", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [workspace] }) }));
    await context.route("**/api/hosts", (route) => route.fulfill({
      status: 200, contentType: "application/json",
      body: JSON.stringify({ items: [{ ...host, workspaces: host.workspaces.map((binding) => ({
        ...binding, sync_state: conflict ? "conflict" : "healthy", sync_conflicts: conflict ? ["src/main.go"] : [],
      })) }] }),
    }));
    await context.route("**/retry-sync", async (route) => {
      retryBody = route.request().postDataJSON() as { expected_binding_version: number };
      conflict = false;
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(host.workspaces[0]) });
    });

    const page = await context.newPage();
    try {
      await page.goto(`${portalUrl}/login?next=%2Fworkspaces`);
      await page.getByLabel("用户名").fill(user.name);
      await page.getByLabel("密码").fill("local-only");
      await page.getByRole("button", { name: "登录" }).click();
      await expect(page.getByRole("heading", { name: "工作区", exact: true })).toBeVisible();
      await expect(page.getByText("同步：已同步 v2")).toBeVisible();
      await expect.poll(async () => {
        conflict = true;
        return page.getByText("Host 工作区同步冲突").isVisible();
      }, { timeout: 5000 }).toBe(true);
      await expect(page.getByText("src/main.go")).toBeVisible();
      await page.getByRole("button", { name: "重试 Host 同步 接力项目 到 接力电脑" }).click();
      await expect.poll(() => retryBody).toEqual({ expected_binding_version: 7 });
      await expect(page.getByText("Host 工作区同步冲突")).toHaveCount(0);
      await expect(page.getByText("同步：已同步 v2")).toBeVisible();
    } finally {
      await context.close();
    }
  });

  test("[mock] removes host token routes and shows the Codex platform MCP config", async ({ page }) => {
    test.skip(true, "Codex integration is outside the current MVP scope");
    await signIn(page);
    await page.goto("/hosts/h1234567/tokens");
    await expect(page.getByRole("heading", { name: "找不到这个页面" })).toBeVisible();
    await page.getByRole("link", { name: "返回助手" }).click();
    await page.getByRole("link", { name: "连接设置" }).click();
    await page.getByRole("link", { name: /创建或管理 API Key/ }).click();
    await expect(page.getByRole("heading", { name: "Codex 平台 MCP" })).toBeVisible();
    await expect(page.locator(".config-snippet")).toContainText("mcp_servers.cloudhost");
    await expect(page.locator(".config-snippet")).toContainText("CH_CLOUDHOST_API_KEY");
    await expect(page.locator(".config-snippet")).toContainText(`${new URL(page.url()).origin}/mcp`);
  });

  test("[mock] keeps the default workspace on host deletion, then deletes the detached workspace", async ({ page }) => {
    const hostName = `mock-workspace-host-${Date.now()}`;

    await signIn(page);
    await allowMoreHosts(page);
    await page.context().route('https://workbench.mock.invalid/**', (route) => route.fulfill({
      status: 200, contentType: 'text/html', body: '<!doctype html><meta charset="utf-8"><h1>本地测试主机</h1>',
    }));
    await page.getByRole("link", { name: "创建电脑" }).click();
    await page.getByLabel(/主机名称/).fill(hostName);
    await page.getByRole("button", { name: "创建主机" }).click();

    const host = page.locator(".host-card").filter({ hasText: hostName });
    await expect(host.getByText("运行中")).toBeVisible({ timeout: 10_000 });
    await expect(host.getByRole("button", { name: /暂停主机|恢复主机|管理主机令牌/ })).toHaveCount(0);
    const workspaceId = await host.locator(".host-facts > div").nth(2).locator("strong").innerText();

    const workbenchPromise = page.waitForEvent("popup");
    await host.getByRole("button", { name: `进入主机 ${hostName}` }).click();
    const workbench = await workbenchPromise;
    await expect(workbench.getByText("本地测试主机")).toBeVisible();

    await host.getByRole("button", { name: `删除主机 ${hostName}` }).click();
    const deleteDialog = page.getByRole("dialog", { name: `删除主机 ${hostName}` });
    await expect(deleteDialog).toContainText("绑定的独立工作区会自动解绑并保留在服务端，不随主机删除");
    await expect(deleteDialog).toContainText("正在运行的终端命令及服务会中止");
    await deleteDialog.getByRole("button", { name: "删除主机" }).click();
    await expect(host).toHaveCount(0);
    await workbench.close();

    await page.getByRole("link", { name: "工作目录" }).click();
    const workspace = page.getByRole("row").filter({ hasText: workspaceId });
    await expect(workspace).toBeVisible();
    await expect(workspace.getByText("未绑定")).toBeVisible();
    await workspace.getByRole("button", { name: `删除工作区 ${workspaceId}` }).click();
    const deleteWorkspaceDialog = page.getByRole("dialog", { name: "删除工作区" });
    await expect(deleteWorkspaceDialog).toContainText("永久删除 Morde 云端工作区及其已同步数据");
    await expect(deleteWorkspaceDialog).toContainText("本地文件夹和未同步的文件会保留");
    await deleteWorkspaceDialog.getByRole("button", { name: "删除工作区" }).click();
    await expect(workspace).toHaveCount(0, { timeout: 10_000 });
  });

  test("[mock] releases and restores the same cloud computer without losing its workspace", async ({ page }) => {
    const hostName = `mock-release-host-${Date.now()}`;
    await signIn(page);
    await allowMoreHosts(page);
    await page.getByRole("link", { name: "创建电脑" }).click();
    await page.getByLabel(/主机名称/).fill(hostName);
    await page.getByRole("button", { name: "创建主机" }).click();

    const host = page.locator(".host-card").filter({ hasText: hostName });
    await expect(host.getByText("运行中")).toBeVisible({ timeout: 10_000 });
    const hostIdentity = await host.locator(".mono").innerText();
    const workspaceId = await host.locator(".host-facts > div").nth(2).locator("strong").innerText();

    await host.getByRole("button", { name: `释放运行资源 ${hostName}` }).click();
    const releaseDialog = page.getByRole("dialog", { name: `释放主机 ${hostName} 的运行资源` });
    await expect(releaseDialog).toContainText("文件会保留");
    await releaseDialog.getByRole("button", { name: "释放运行资源" }).click();
    await expect(host.getByText("已停止")).toBeVisible({ timeout: 10_000 });
    await expect(host.locator(".mono")).toHaveText(hostIdentity);
    await expect(host.locator(".host-facts > div").nth(2).locator("strong")).toHaveText(workspaceId);

    await host.getByRole("button", { name: `恢复主机 ${hostName}` }).click();
    const restoreDialog = page.getByRole("dialog", { name: `恢复主机 ${hostName}` });
    await expect(restoreDialog).toContainText("保留原有文件");
    await restoreDialog.getByRole("button", { name: "恢复主机" }).click();
    await expect(host.getByText("运行中")).toBeVisible({ timeout: 10_000 });
    await expect(host.locator(".mono")).toHaveText(hostIdentity);
    await expect(host.locator(".host-facts > div").nth(2).locator("strong")).toHaveText(workspaceId);
    const restored = await page.evaluate(async (id) => (await fetch(`/api/hosts/${id}`)).json(), hostIdentity.split(" · ")[0]);
    expect(restored.runtime_generation).toBe(2);
  });

  test("[mock] remounts a detached workspace and disables deletion while attached", async ({ page }) => {
    const sourceName = `mock-workspace-source-${Date.now()}`;
    const targetName = `mock-workspace-target-${Date.now()}`;

    await signIn(page);
    await allowMoreHosts(page);
    await page.getByRole("link", { name: "创建电脑" }).click();
    await page.getByLabel(/主机名称/).fill(sourceName);
    await page.getByRole("button", { name: "创建主机" }).click();

    const sourceHost = page.locator(".host-card").filter({ hasText: sourceName });
    await expect(sourceHost.getByText("运行中")).toBeVisible({ timeout: 10_000 });
    const workspaceId = await sourceHost.locator(".host-facts > div").nth(2).locator("strong").innerText();
    await sourceHost.getByRole("button", { name: `删除主机 ${sourceName}` }).click();
    const sourceDeleteDialog = page.getByRole("dialog", { name: `删除主机 ${sourceName}` });
    await sourceDeleteDialog.getByRole("button", { name: "删除主机" }).click();

    await page.getByRole("link", { name: "工作目录" }).click();
    const workspace = page.getByRole("row").filter({ hasText: workspaceId });
    await expect(workspace.getByText("未绑定")).toBeVisible();

    await page.getByRole("link", { name: "云端电脑", exact: true }).click();
    await page.getByRole("link", { name: "创建电脑" }).click();
    await page.getByLabel(/主机名称/).fill(targetName);
    await page.getByRole("radio", { name: "绑定已有工作区" }).check();
    await page.getByRole("checkbox", { name: new RegExp(sourceName) }).check();
    await page.getByRole("button", { name: "创建主机" }).click();

    const targetHost = page.locator(".host-card").filter({ hasText: targetName });
    await expect(targetHost.getByText("运行中")).toBeVisible({ timeout: 10_000 });
    await expect(targetHost.locator(".host-facts > div").nth(2).locator("strong")).toHaveText(workspaceId);
    await page.getByRole("link", { name: "工作目录" }).click();
    const attachedWorkspace = page.getByRole("row").filter({ hasText: workspaceId });
    await expect(attachedWorkspace).toContainText(targetName);
    await expect(attachedWorkspace).toContainText("应用：已应用");
    await expect(attachedWorkspace.getByRole("button", { name: `删除工作区 ${workspaceId}` })).toBeDisabled();

    await page.getByRole("link", { name: "云端电脑", exact: true }).click();
    await targetHost.getByRole("button", { name: `删除主机 ${targetName}` }).click();
    await page.getByRole("dialog", { name: `删除主机 ${targetName}` }).getByRole("button", { name: "删除主机" }).click();
    await page.getByRole("link", { name: "工作目录" }).click();
    const detachedWorkspace = page.getByRole("row").filter({ hasText: workspaceId });
    await expect(detachedWorkspace.getByText("未绑定")).toBeVisible();
    await detachedWorkspace.getByRole("button", { name: `删除工作区 ${workspaceId}` }).click();
    await page.getByRole("dialog", { name: "删除工作区" }).getByRole("button", { name: "删除工作区" }).click();
    await expect(detachedWorkspace).toHaveCount(0, { timeout: 10_000 });
  });

  test("[mock] admin can open and view each management page", async ({ page }) => {
    await signIn(page);
    await page.getByRole("link", { name: "管理后台" }).click();
    await expect(page.getByRole("heading", { name: "总览", exact: true })).toBeVisible();
    await expect(page.getByRole("heading", { name: "主机状态" })).toBeVisible();

    const adminNav = page.getByRole("navigation", { name: "管理导航" });
    await adminNav.getByRole("link", { name: "用户" }).click();
    await expect(page.getByRole("row").filter({ hasText: "demo" })).toBeVisible();
    await adminNav.getByRole("link", { name: "总览" }).click();
    await expect(page.getByRole("heading", { name: "主机状态" })).toBeVisible();
    await adminNav.getByRole("link", { name: "审计" }).click();
    await expect(page.getByRole("row").filter({ hasText: "h-audit-demo" })).toBeVisible();
  });

  test("[mock] redirects and clears admin data after the server revokes access", async ({ browser }) => {
    const admin = {
      id: "u1234567", name: "admin", role: "admin",
      quota: { hosts: 3, cpu_milli: 6000, mem_mib: 12288 }, usage: { hosts: 0, cpu_milli: 0, mem_mib: 0 },
    };
    const adminRecord = {
      ...admin, disabled: false, created_at: "2026-10-26T00:00:00Z",
    };
    let revoked = false;
    const context = await browser.newContext({ serviceWorkers: "block" });
    await context.route("**/src/main.tsx*", async (route) => {
      const response = await route.fetch();
      const source = await response.text();
      const withoutMockWorker = source.replace('if (import.meta.env.DEV && import.meta.env.VITE_MOCK_API === "1")', "if (false)");
      expect(withoutMockWorker).not.toBe(source);
      await route.fulfill({ response, body: withoutMockWorker });
    });
    await context.route("**/src/admin-main.tsx*", async (route) => {
      const response = await route.fetch();
      const source = await response.text();
      const withoutMockWorker = source.replace('if (import.meta.env.DEV && import.meta.env.VITE_MOCK_API === "1")', "if (false)");
      expect(withoutMockWorker).not.toBe(source);
      await route.fulfill({ response, body: withoutMockWorker });
    });
    await context.route("**/api/me", (route) => route.fulfill({
      status: 200, contentType: "application/json", body: JSON.stringify({ ...admin, role: revoked ? "user" : "admin" }),
    }));
    await context.route("**/api/admin/users", (route) => route.fulfill(revoked
      ? { status: 403, contentType: "application/json", body: JSON.stringify({ error: { code: "forbidden", message: "没有执行此操作的权限", details: {} } }) }
      : { status: 200, contentType: "application/json", body: JSON.stringify({ items: [adminRecord] }) }));

    const page = await context.newPage();
    try {
      await page.goto(`${portalUrl}/admin/users`);
      await expect(page.getByRole("row").filter({ hasText: "admin" })).toBeVisible();
      revoked = true;
      const adminNav = page.getByRole("navigation", { name: "管理导航" });
      await adminNav.getByRole("link", { name: "总览" }).click();
      await adminNav.getByRole("link", { name: "用户" }).click();

      await expect(page.getByRole("heading", { name: "没有管理权限" })).toBeVisible();
      await expect(page.getByRole("row").filter({ hasText: "admin" })).toHaveCount(0);
      await expect(page).toHaveURL(/\/admin\/access-denied$/);
    } finally {
      await context.close();
    }
  });

  test("[mock] creates and disables a user, then filters audit records", async ({ page }) => {
    const userName = `local-user-${Date.now()}`;
    const password = "local-only-password";

    await signIn(page);
    await page.getByRole("link", { name: "管理后台" }).click();
    await expect(page.getByRole('heading', { name: '主机状态' })).toBeVisible();
    const registrationStatus = await page.evaluate(async ({ name, password: secret }) => (await fetch('/api/auth/register', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name, password: secret }),
    })).status, { name: userName, password });
    expect(registrationStatus).toBe(201);
    const adminNav = page.getByRole("navigation", { name: "管理导航" });
    await adminNav.getByRole("link", { name: "用户" }).click();
    const userRow = page.getByRole("row").filter({ hasText: userName });
    await expect(userRow).toContainText("普通用户 · 待审核");
    await userRow.getByRole('button', { name: '通过审核' }).click();
    await expect(userRow).toContainText('普通用户 · 已通过');

    await userRow.getByRole('button', { name: '禁用账号' }).click();
    await expect(userRow).toContainText("已禁用");

    await page.getByRole("button", { name: "退出" }).click();
    await expect(page.getByLabel("用户名", { exact: true })).toBeVisible();
    await submitLogin(page, userName, password);
    await expect(page.getByRole("alert")).toHaveText("账号已被禁用，请联系管理员。");

    await submitLogin(page, "demo", "demo");
    const auditNav = page.getByRole("navigation", { name: "管理导航" });
    await auditNav.getByRole("link", { name: "审计" }).click();
    const since = await page.evaluate(() => {
      const cutoff = new Date(Date.now() - 2 * 60 * 60 * 1000);
      return new Date(cutoff.getTime() - cutoff.getTimezoneOffset() * 60_000).toISOString().slice(0, 16);
    });
    await page.getByLabel("操作人").fill("u1a2b3c4");
    await page.getByLabel("操作类型").fill("host.create");
    await page.getByLabel("目标").fill("h-audit-demo");
    await page.getByLabel("开始时间").fill(since);
    await page.getByRole("button", { name: "筛选" }).click();
    const auditRow = page.getByRole("row").filter({ hasText: "h-audit-demo" });
    await expect(auditRow).toContainText("host.create");
    await expect(page.getByRole("row").filter({ hasText: "t-audit-demo" })).toHaveCount(0);
  });
});
