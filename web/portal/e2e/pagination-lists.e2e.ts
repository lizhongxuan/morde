import { expect, test, type Page } from "@playwright/test";

const assistantMockStorageKey = "cloudhost:portal-mock-assistant-state-v3";

async function signInWithLongListFixtures(page: Page, account = { username: "demo", password: "demo", ownerID: "u1a2b3c4" }) {
  await page.addInitScript(({ storageKey, ownerID }) => {
    const now = Date.now();
    const createdAt = new Date(now - 60_000).toISOString();
    const expiresAt = new Date(now + 30 * 86_400_000).toISOString();
    const clients = Array.from({ length: 12 }, (_, index) => ({
      id: `cli_e2e${String(index + 1).padStart(4, "0")}`,
      platform: index % 2 === 0 ? "desktop" : "ios",
      display_name: `验收设备 ${String(index + 1).padStart(2, "0")}`,
      expires_at: expiresAt,
      revoked: false,
      created_at: createdAt,
      last_used_at: null,
    }));
    const apiKeys = Array.from({ length: 10 }, (_, index) => ({
      id: `t_e2e${String(index + 1).padStart(4, "0")}`,
      name: `验收密钥 ${String(index + 1).padStart(2, "0")}`,
      owner_id: ownerID,
      expires_at: expiresAt,
      last_used_at: null,
      revoked: false,
      created_at: createdAt,
    }));
    const hosts = Array.from({ length: 12 }, (_, index) => ({
      id: `h${String(index + 1).padStart(7, "0")}`,
      name: `分页电脑 ${String(index + 1).padStart(2, "0")}`,
      owner_id: ownerID,
      template_id: "std-1c2g",
      isolation: "container",
      state: "stopped",
      state_reason: "",
      runtime_generation: 1,
      control_version: 1,
      control_state: "ai",
      url: "",
      workspaces: [],
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
      expires_at: null,
      last_active_at: null,
      created_at: createdAt,
    }));
    const workspaces = Array.from({ length: 12 }, (_, index) => ({
      id: `w${String(index + 1).padStart(7, "0")}`,
      name: `分页工作区 ${String(index + 1).padStart(2, "0")}`,
      revision: 1,
      used_bytes: 0,
      quota_bytes: 1_048_576,
      is_read_only: false,
      state: "ready",
      created_at: createdAt,
      updated_at: createdAt,
    }));

    localStorage.setItem(storageKey, JSON.stringify({
      apiKeys,
      clients,
      clientOwners: clients.map(({ id }) => [id, ownerID]),
      hosts,
      workspaces,
      workspaceOwners: workspaces.map(({ id }) => [id, ownerID]),
      userPreferences: [],
      userResources: [{ id: ownerID, quota: { hosts: 20, cpu_milli: 4000, mem_mib: 8192 },
        default_host_resources: { cpu_milli: 1000, mem_mib: 2048 } }],
    }));
  }, { storageKey: assistantMockStorageKey, ownerID: account.ownerID });
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto("/");
  await page.getByLabel("用户名").fill(account.username);
  await page.getByLabel("密码").fill(account.password);
  await page.getByRole("button", { name: "登录" }).click();
  await expect(page.getByRole("banner").getByText(account.username, { exact: true })).toBeVisible();
}

async function seedProjectAndGoalFixtures(page: Page) {
  const statuses = await page.evaluate(async () => {
    const projects = await Promise.all(Array.from({ length: 12 }, (_, index) => fetch("/api/projects", {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name: `验收项目 ${String(index + 1).padStart(2, "0")}`, description: "本地分页验收数据" }),
    }).then((response) => response.status)));
    const goals = await Promise.all(Array.from({ length: 12 }, (_, index) => fetch("/api/goals", {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json", "Idempotency-Key": `pagination-goal-${String(index + 1).padStart(2, "0")}` },
      body: JSON.stringify({ id: `pagination_goal_${String(index + 1).padStart(2, "0")}`, body: {
        title: `验收目标 ${String(index + 1).padStart(2, "0")}`,
        description: "",
        status: "active",
        steps: [],
        related_material_ids: [],
      } }),
    }).then((response) => response.status)));
    return { projects, goals };
  });
  expect(statuses.projects.every((status) => status === 201), JSON.stringify(statuses.projects)).toBe(true);
  expect(statuses.goals.every((status) => status === 201), JSON.stringify(statuses.goals)).toBe(true);
}

async function seedConnectionFixtures(page: Page) {
  const statuses = await page.evaluate(async () => {
    const models: number[] = [];
    const teamModels: number[] = [];
    const connectors: number[] = [];
    for (let index = 11; index >= 0; index -= 1) {
      const number = String(index + 1).padStart(2, "0");
      models.push(await fetch("/api/model-connections", {
        method: "POST",
        credentials: "include",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name: `验收模型 ${number}`, base_url: `https://model-${number}.example.invalid/v1`, model_name: `local-test-${number}`, api_key: "local-e2e-dummy-only", shared: false }),
      }).then((response) => response.status));
    }
    for (let index = 11; index >= 0; index -= 1) {
      const number = String(index + 1).padStart(2, "0");
      teamModels.push(await fetch("/api/model-connections", {
        method: "POST",
        credentials: "include",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name: `验收团队模型 ${number}`, base_url: `https://team-model-${number}.example.invalid/v1`, model_name: `local-team-test-${number}`, api_key: "local-e2e-dummy-only", shared: true }),
      }).then((response) => response.status));
    }
    for (let index = 11; index >= 0; index -= 1) {
      const number = String(index + 1).padStart(2, "0");
      connectors.push(await fetch("/api/mcp/connectors", {
        method: "POST",
        credentials: "include",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name: `验收工具 ${number}`, endpoint_url: `https://mcp-${number}.example.invalid/mcp` }),
      }).then((response) => response.status));
    }
    const [modelListResponse, connectorListResponse] = await Promise.all([
      fetch("/api/model-connections", { credentials: "include" }),
      fetch("/api/mcp/connectors", { credentials: "include" }),
    ]);
    const modelList = await modelListResponse.json() as { items: Array<{ name: string }> };
    const connectorList = await connectorListResponse.json() as { items: Array<{ name: string }> };
    return { models, teamModels, connectors, modelList: modelList.items.map((item) => item.name), connectorList: connectorList.items.map((item) => item.name) };
  });
  for (const list of ["models", "teamModels", "connectors"] as const) {
    expect(statuses[list].every((status) => status === 201), `${list}: ${JSON.stringify(statuses[list])}`).toBe(true);
  }
  expect(statuses.modelList).toHaveLength(25);
  expect(statuses.connectorList).toHaveLength(12);
}

async function navigateWithinPortal(page: Page, pathname: string) {
  await page.evaluate((path) => {
    window.history.pushState({}, "", path);
    window.dispatchEvent(new PopStateEvent("popstate"));
  }, pathname);
}

async function expectPageFitsViewport(page: Page) {
  const dimensions = await page.evaluate(() => {
    const pageContent = document.querySelector<HTMLElement>("#main-content");
    if (!pageContent) throw new Error("页面内容容器不存在");
    return {
      documentHeight: document.documentElement.scrollHeight,
      viewportHeight: document.documentElement.clientHeight,
      pageContentHeight: pageContent.scrollHeight,
      pageViewportHeight: pageContent.clientHeight,
    };
  });
  expect(dimensions.documentHeight).toBeLessThanOrEqual(dimensions.viewportHeight + 1);
  expect(dimensions.pageContentHeight).toBeLessThanOrEqual(dimensions.pageViewportHeight + 1);
}

test.describe("screen pagination for business lists", () => {
  test.skip(process.env.CH_REAL_E2E === "1", "local MSW fixture scenario is disabled for real-host runs");

  test("paginates client installations without adding page-level vertical scrolling", async ({ page }) => {
    await signInWithLongListFixtures(page);
    await page.goto("/clients");

    const pagination = page.getByRole("navigation", { name: "已登录客户端分页" });
    const list = page.locator(".screen-pagination__viewport");
    await expect(page.getByRole("heading", { name: "已登录设备" })).toBeVisible();
    await expect(pagination.getByText(/1 \/ \d+/)).toBeVisible();
    await expect(list.getByRole("heading", { name: "验收设备 01" })).toBeVisible();
    await expect(list.getByRole("heading", { name: "验收设备 02" })).toBeVisible();
    await expect(list.getByRole("heading", { name: "验收设备 03" })).toBeVisible();
    await expect(list.getByRole("heading", { name: "验收设备 04" })).toBeHidden();
    await expectPageFitsViewport(page);

    await pagination.getByRole("button", { name: "下一页" }).click();
    await expect(pagination.getByText(/2 \/ \d+/)).toBeVisible();
    await expect(list.getByRole("heading", { name: "验收设备 04" })).toBeVisible();
    await expect(list.getByRole("heading", { name: "验收设备 01" })).toBeHidden();
    await expectPageFitsViewport(page);
  });

  test("paginates API Key metadata without adding page-level vertical scrolling", async ({ page }) => {
    await signInWithLongListFixtures(page);
    await page.goto("/apikeys");

    const pagination = page.getByRole("navigation", { name: "API Key 列表分页" });
    await expect(page.getByRole("heading", { name: "我的密钥" })).toBeVisible();
    await expect(pagination.getByText(/1 \/ \d+/)).toBeVisible();
    await expect(page.getByRole("article", { name: "API Key 验收密钥 01" })).toBeVisible();
    await expect(page.getByRole("article", { name: "API Key 验收密钥 02" })).toBeVisible();
    await expect(page.getByRole("article", { name: "API Key 验收密钥 03" })).toBeHidden();
    await expectPageFitsViewport(page);

    await pagination.getByRole("button", { name: "下一页" }).click();
    await expect(pagination.getByText(/2 \/ \d+/)).toBeVisible();
    await expect(page.getByRole("article", { name: "API Key 验收密钥 03" })).toBeVisible();
    await expect(page.getByRole("article", { name: "API Key 验收密钥 01" })).toBeHidden();
    await expectPageFitsViewport(page);
  });

  test("paginates owned cloud computers without adding page-level vertical scrolling", async ({ page }) => {
    await signInWithLongListFixtures(page);
    await page.goto("/hosts");

    const pagination = page.getByRole("navigation", { name: "我的电脑分页" });
    await expect(page.getByRole("button", { name: /分页电脑 01/ })).toBeVisible();
    await expect(page.getByRole("button", { name: /分页电脑 12/ })).toBeHidden();
    await expectPageFitsViewport(page);

    const lastHost = page.getByRole("button", { name: /分页电脑 12/ });
    for (let pageIndex = 0; pageIndex < 12 && !(await lastHost.isVisible()); pageIndex += 1) {
      await pagination.getByRole("button", { name: "下一页" }).click();
    }
    await expect(page.getByRole("button", { name: /分页电脑 01/ })).toBeHidden();
    await expect(lastHost).toBeVisible();
    await expectPageFitsViewport(page);
  });

  test("paginates cloud Workspace cards without adding page-level vertical scrolling", async ({ page }) => {
    await signInWithLongListFixtures(page);
    await page.goto("/workspaces");

    const pagination = page.getByRole("region", { name: "云端工作区" });
    const mockWorkspace = page.getByRole("article", { name: "工作区 本地差异演示工作区" });
    const lastWorkspace = page.getByRole("article", { name: "工作区 分页工作区 12" });
    await expect(mockWorkspace).toBeVisible();
    await expect(lastWorkspace).toBeHidden();
    await expectPageFitsViewport(page);

    for (let pageIndex = 0; pageIndex < 12 && !(await lastWorkspace.isVisible()); pageIndex += 1) {
      await pagination.getByRole("button", { name: "下一页" }).click();
    }
    await expect(mockWorkspace).toBeHidden();
    await expect(lastWorkspace).toBeVisible();
    await expectPageFitsViewport(page);
  });

  test("paginates projects without adding page-level vertical scrolling", async ({ page }) => {
    await signInWithLongListFixtures(page, { username: "member", password: "member", ownerID: "umember1" });
    await seedProjectAndGoalFixtures(page);
    await navigateWithinPortal(page, "/projects");

    const pagination = page.getByRole("navigation", { name: "项目列表分页" });
    const firstProject = page.getByRole("heading", { name: "验收项目 01" });
    const lastProject = page.getByRole("heading", { name: "验收项目 12" });
    await expect(firstProject).toBeVisible();
    await expect(lastProject).toBeHidden();
    await expectPageFitsViewport(page);

    for (let pageIndex = 0; pageIndex < 12 && !(await lastProject.isVisible()); pageIndex += 1) {
      await pagination.getByRole("button", { name: "下一页" }).click();
    }
    await expect(firstProject).toBeHidden();
    await expect(lastProject).toBeVisible();
    await expectPageFitsViewport(page);
  });

  test("paginates goals without adding page-level vertical scrolling", async ({ page }) => {
    await signInWithLongListFixtures(page);
    await seedProjectAndGoalFixtures(page);
    await navigateWithinPortal(page, "/goals");

    const pagination = page.getByRole("navigation", { name: "目标列表分页" });
    const firstGoal = page.getByRole("heading", { name: "验收目标 01" });
    const lastGoal = page.getByRole("heading", { name: "验收目标 12" });
    await expect(firstGoal).toBeVisible();
    await expect(lastGoal).toBeHidden();
    await expectPageFitsViewport(page);

    for (let pageIndex = 0; pageIndex < 12 && !(await lastGoal.isVisible()); pageIndex += 1) {
      await pagination.getByRole("button", { name: "下一页" }).click();
    }
    await expect(firstGoal).toBeHidden();
    await expect(lastGoal).toBeVisible();
    await expectPageFitsViewport(page);
  });

  test("paginates personal and team models plus MCP connections without page-level scrolling", async ({ page }) => {
    await signInWithLongListFixtures(page);
    await navigateWithinPortal(page, "/connections");
    await expect(page.getByRole("heading", { name: "我的模型" })).toBeVisible();
    await seedConnectionFixtures(page);

    await page.getByRole("button", { name: /添加个人模型/ }).click();
    await page.getByLabel("显示名称").fill("本地刷新触发模型");
    await page.getByLabel("模型名").fill("local-refresh-trigger");
    await page.getByLabel("服务地址").fill("https://refresh-trigger.example.invalid/v1");
    await page.getByLabel("API 密钥").fill("local-e2e-dummy-only");
    await page.getByRole("button", { name: "保存个人模型" }).click();

    const personalPagination = page.getByRole("navigation", { name: "个人模型分页" });
    const firstPersonalModel = page.getByRole("heading", { name: "验收模型 01" });
    const lastPersonalModel = page.getByRole("heading", { name: "验收模型 12" });
    await expect(firstPersonalModel).toBeVisible();
    await expect(lastPersonalModel).toBeHidden();
    await expectPageFitsViewport(page);
    for (let pageIndex = 0; pageIndex < 12 && !(await lastPersonalModel.isVisible()); pageIndex += 1) {
      await personalPagination.getByRole("button", { name: "下一页" }).click();
    }
    await expect(firstPersonalModel).toBeHidden();
    await expect(lastPersonalModel).toBeVisible();
    await expectPageFitsViewport(page);

    await page.getByRole("button", { name: /^团队模型/ }).click();
    const teamPagination = page.getByRole("navigation", { name: "团队模型分页" });
    const firstTeamModel = page.getByRole("heading", { name: "验收团队模型 01" });
    const lastTeamModel = page.getByRole("heading", { name: "验收团队模型 12" });
    await expect(firstTeamModel).toBeVisible();
    await expect(lastTeamModel).toBeHidden();
    await expectPageFitsViewport(page);
    for (let pageIndex = 0; pageIndex < 12 && !(await lastTeamModel.isVisible()); pageIndex += 1) {
      await teamPagination.getByRole("button", { name: "下一页" }).click();
    }
    await expect(firstTeamModel).toBeHidden();
    await expect(lastTeamModel).toBeVisible();
    await expectPageFitsViewport(page);

    await page.getByRole("button", { name: "MCP 工具" }).click();
    await page.getByRole("button", { name: /添加工具连接/ }).click();
    await page.getByLabel("连接名称").fill("本地刷新触发工具");
    await page.getByLabel("MCP 服务地址").fill("https://refresh-trigger.example.invalid/mcp");
    await page.getByRole("button", { name: "保存连接" }).click();
    const connectorPagination = page.getByRole("navigation", { name: "MCP 工具连接列表分页" });
    const firstConnector = page.getByRole("heading", { name: "验收工具 01" });
    const lastConnector = page.getByRole("heading", { name: "验收工具 12" });
    await expect(firstConnector).toBeVisible();
    await expect(lastConnector).toBeHidden();
    await expectPageFitsViewport(page);
    for (let pageIndex = 0; pageIndex < 12 && !(await lastConnector.isVisible()); pageIndex += 1) {
      await connectorPagination.getByRole("button", { name: "下一页" }).click();
    }
    await expect(firstConnector).toBeHidden();
    await expect(lastConnector).toBeVisible();
    await expectPageFitsViewport(page);
  });
});
