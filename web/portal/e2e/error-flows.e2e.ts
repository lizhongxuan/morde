import { expect, test, type Browser, type BrowserContext, type Page } from "@playwright/test";

const portalUrl = process.env.CH_PORTAL_E2E_URL ?? "http://127.0.0.1:5183";
const user = {
  id: "u1234567",
  name: "demo",
  role: "member",
  quota: { hosts: 3, cpu_milli: 6000, mem_mib: 12288 },
  usage: { hosts: 0, cpu_milli: 0, mem_mib: 0 },
};
const resources = { cpu_milli: 500, mem_mib: 1536, cpu_limit_milli: 2000, mem_limit_mib: 4096 };
const template = {
  id: "std-test",
  name: "标准规格",
  description: "本地错误流程测试",
  isolation: "container",
  resources,
  workspace_size_mib: 1024,
  ttl_seconds: 3600,
  idle_timeout_seconds: 900,
  on_idle: "disabled",
  keep_workspace_default: true,
};
const detachedWorkspace = {
  id: "w1234567",
  name: "本地工作目录",
  revision: 0,
  used_bytes: 0,
  quota_bytes: 1024 * 1024,
  state: "ready",
  created_at: "2026-10-19T00:00:00Z",
  updated_at: "2026-10-19T00:00:00Z",
};

function json(body: unknown, status = 200) {
  return { status, contentType: "application/json", body: JSON.stringify(body) };
}

function apiError(code: string, message: string, status: number) {
  return json({ error: { code, message, details: {} } }, status);
}

async function signedInPage(
  browser: Browser,
  setupRoutes?: (context: BrowserContext) => Promise<void>,
): Promise<{ context: BrowserContext; page: Page }> {
  const context = await browser.newContext({ serviceWorkers: "block" });
  await context.route("**/src/main.tsx*", async (route) => {
    const response = await route.fetch();
    const source = await response.text();
    const withoutDevWorker = source.replace(
      'if (import.meta.env.DEV && import.meta.env.VITE_MOCK_API === "1")',
      "if (false)",
    );
    expect(withoutDevWorker).not.toBe(source);
    await route.fulfill({ response, body: withoutDevWorker });
  });
  await context.route("**/api/me", (route) => route.fulfill(apiError("unauthenticated", "请先登录", 401)));
  await context.route("**/api/auth/login", (route) => route.fulfill(json({ user })));
  await setupRoutes?.(context);

  const page = await context.newPage();
  await page.goto(`${portalUrl}/login`);
  await page.getByLabel("用户名").fill(user.name);
  await page.getByLabel("密码").fill("local-only");
  await page.getByRole("button", { name: "登录" }).click();
  await expect(page.getByRole("heading", { name: "AI 助手" })).toBeVisible();
  await page.getByRole("link", { name: "云端电脑", exact: true }).click();
  await expect(page.getByRole("heading", { name: "云端电脑", exact: true })).toBeVisible();
  return { context, page };
}

async function closeAlert(page: Page, text: string) {
  const alert = page.getByRole("alert").filter({ hasText: text });
  await expect(alert).toContainText(text);
  await alert.getByRole("button", { name: "关闭通知" }).click();
}

test.describe("Portal visible error mock flows", () => {
  test.skip(process.env.CH_REAL_E2E === "1", "mock scenario is disabled for a real-host run");

  test("[mock] shows quota, capacity, and workspace busy errors when creating a host", async ({ browser }) => {
    const createBodies: Array<{ workspace_ids: string[] }> = [];
    let createdWorkspace = 0;
    const failures = [
      apiError("quota_exceeded", "主机数已达上限 3", 409),
      apiError("capacity_exceeded", "集群容量已满", 409),
      apiError("workspace_busy", "工作目录已挂载", 409),
    ];
    const { context, page } = await signedInPage(browser, async (context) => {
      await context.route("**/api/templates", (route) => route.fulfill(json({ items: [template] })));
      await context.route("**/api/workspaces", (route) => {
        if (route.request().method() === "GET") return route.fulfill(json({ items: [detachedWorkspace] }));
        createdWorkspace += 1;
        return route.fulfill(json({
          ...detachedWorkspace,
          id: `wcreated${createdWorkspace}`,
          name: route.request().postDataJSON().name,
        }, 201));
      });
      await context.route("**/api/hosts", async (route) => {
        if (route.request().method() === "GET") return route.fulfill(json({ items: [] }));
        createBodies.push(route.request().postDataJSON());
        return route.fulfill(failures[createBodies.length - 1]);
      });
    });
    try {
      await page.getByRole("link", { name: "创建电脑" }).click();
      await page.getByRole("button", { name: "创建主机" }).click();
      await closeAlert(page, "已达到主机配额上限");

      await page.getByRole("button", { name: "创建主机" }).click();
      await closeAlert(page, "当前资源容量不足");

      await page.getByRole("radio", { name: "绑定已有工作区" }).check();
      await page.getByRole("checkbox", { name: /本地工作目录/ }).check();
      await page.getByRole("button", { name: "创建主机" }).click();
      await closeAlert(page, "工作目录正在被其他主机使用");
      expect(createBodies.map((body) => body.workspace_ids)).toEqual([
        ["wcreated1"],
        // Retries after a known Host rejection keep the already-created Workspace.
        ["wcreated1"],
        [detachedWorkspace.id],
      ]);
    } finally {
      await context.close();
    }
  });

  test("[mock] shows workspace busy when deletion loses a detach race", async ({ browser }) => {
    let deletePath = "";
    let boundDuringConfirmation = false;
    const hostAfterBinding = {
      id: "h1234567", name: "并发绑定电脑", owner_id: user.id, template_id: template.id,
      isolation: "container", state: "running", state_reason: "", runtime_generation: 1,
      control_version: 1, control_state: "ai", url: "", resources,
      expires_at: null, last_active_at: null, created_at: "2026-10-19T00:00:00Z",
      workspaces: [{
        workspace: { id: detachedWorkspace.id, name: detachedWorkspace.name, revision: detachedWorkspace.revision, used_bytes: detachedWorkspace.used_bytes, state: "ready" },
        directory: "projects/w1234567", state: "active", binding_version: 1,
        desired_revision: 0, applied_revision: 0, sync_state: "healthy", sync_conflicts: [],
      }],
    };
    const { context, page } = await signedInPage(browser, async (context) => {
      await context.route("**/api/templates", (route) => route.fulfill(json({ items: [template] })));
      await context.route("**/api/hosts", (route) => route.fulfill(json({ items: boundDuringConfirmation ? [hostAfterBinding] : [] })));
      await context.route("**/api/workspaces", (route) => route.fulfill(json({ items: [detachedWorkspace] })));
      await context.route("**/api/workspaces/w1234567", (route) => {
        deletePath = new URL(route.request().url()).pathname;
        boundDuringConfirmation = true;
        return route.fulfill(apiError("workspace_busy", "工作目录状态已变化", 409));
      });
    });
    try {
      await page.getByRole("link", { name: "工作目录" }).click();
      const workspace = page.getByRole("row").filter({ hasText: detachedWorkspace.id });
      await workspace.getByRole("button", { name: `删除工作区 ${detachedWorkspace.name}` }).click();
      await page.getByRole("dialog", { name: "删除工作区" }).getByRole("button", { name: "删除工作区" }).click();
      await expect(page.getByRole("alert")).toContainText("工作目录正在被其他主机使用");
      expect(deletePath).toBe(`/api/workspaces/${detachedWorkspace.id}`);
      await expect(page.getByRole("dialog", { name: "删除工作区" })).toHaveCount(0);
      await expect(workspace.getByText("并发绑定电脑 · 运行中")).toBeVisible();
      await expect(workspace.getByRole("button", { name: `删除工作区 ${detachedWorkspace.name}` })).toBeDisabled();
    } finally {
      await context.close();
    }
  });
});
