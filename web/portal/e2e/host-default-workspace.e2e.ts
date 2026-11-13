import { expect, test, type Page } from "@playwright/test";

const assistantMockStorageKey = "cloudhost:portal-mock-assistant-state-v3";
const firstHostID = "hdefault01";
const secondHostID = "hdefault02";

async function signInWithDefaultHostFixtures(page: Page) {
  await page.addInitScript(({ storageKey }) => {
    if (localStorage.getItem(storageKey)) return;
    const now = new Date().toISOString();
    const expiresAt = new Date(Date.now() + 30 * 86_400_000).toISOString();
    const workspace = (id: string, name: string) => ({
      id,
      name,
      revision: 1,
      used_bytes: 0,
      quota_bytes: 1_048_576,
      is_read_only: false,
      state: "ready",
      created_at: now,
      updated_at: now,
    });
    const binding = (workspaceValue: ReturnType<typeof workspace>) => ({
      workspace: {
        id: workspaceValue.id,
        name: workspaceValue.name,
        revision: workspaceValue.revision,
        used_bytes: workspaceValue.used_bytes,
        state: workspaceValue.state,
      },
      directory: `projects/${workspaceValue.id}`,
      state: "active",
      binding_version: 1,
      desired_revision: workspaceValue.revision,
      applied_revision: workspaceValue.revision,
      sync_state: "healthy",
      sync_conflicts: [],
    });
    const firstWorkspace = workspace("wdefault01", "默认电脑一 Workspace");
    const secondWorkspace = workspace("wdefault02", "默认电脑二 Workspace");
    const host = (id: string, name: string, workspaceValue: ReturnType<typeof workspace>) => ({
      id,
      name,
      owner_id: "u1a2b3c4",
      template_id: "std-1c2g",
      isolation: "container",
      state: "running",
      state_reason: "",
      runtime_generation: 1,
      control_version: 1,
      control_state: "ai",
      url: `https://${id}.hosts.mock.invalid`,
      workspaces: [binding(workspaceValue)],
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
      expires_at: expiresAt,
      last_active_at: now,
      created_at: now,
    });

    localStorage.setItem(storageKey, JSON.stringify({
      hosts: [
        host("hdefault01", "默认电脑一", firstWorkspace),
        host("hdefault02", "默认电脑二", secondWorkspace),
      ],
      workspaces: [firstWorkspace, secondWorkspace],
      workspaceOwners: [[firstWorkspace.id, "u1a2b3c4"], [secondWorkspace.id, "u1a2b3c4"]],
      userPreferences: [["u1a2b3c4", {
        assistant_name: "Morde",
        selected_model_id: null,
        default_host_id: "hdefault01",
        updated_at: now,
      }]],
      userResources: [{
        id: "u1a2b3c4",
        quota: { hosts: 20, cpu_milli: 4000, mem_mib: 8192 },
        default_host_resources: { cpu_milli: 1000, mem_mib: 2048 },
      }],
    }));
  }, { storageKey: assistantMockStorageKey });

  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto("/");
  await page.getByLabel("用户名").fill("demo");
  await page.getByLabel("密码").fill("demo");
  await page.getByRole("button", { name: "登录" }).click();
  await expect(page.getByRole("banner").getByText("demo", { exact: true })).toBeVisible();
}

test.describe("default computer and Workspace continuity", () => {
  test.skip(process.env.CH_REAL_E2E === "1", "local MSW fixture scenario is disabled for real-host runs");

  test("keeps the selected default computer and Workspace in sync with the Assistant sidebar", async ({ page }) => {
    await signInWithDefaultHostFixtures(page);
    await page.goto("/connections");

    const cloudComputerLink = page.getByRole("navigation", { name: "工作导航" }).getByRole("link", { name: "云端电脑", exact: true });
    await expect(cloudComputerLink).toBeVisible();
    await expect(cloudComputerLink).toHaveAttribute("href", "/hosts");
    await cloudComputerLink.click();
    await expect(page).toHaveURL(/\/hosts$/);
    await expect(cloudComputerLink).toHaveAttribute("aria-current", "page");

    const defaultComputer = page.getByRole("combobox", { name: "默认云端电脑" });
    await expect(defaultComputer).toHaveValue(firstHostID);
    await expect(page.locator(".host-default-workspace strong")).toHaveText("默认电脑一 Workspace");

    await defaultComputer.selectOption(secondHostID);
    await expect(defaultComputer).toHaveValue(secondHostID);
    await expect(page.locator(".host-default-workspace strong")).toHaveText("默认电脑二 Workspace");
    await expect(page.getByRole("link", { name: "管理 Workspace" }))
      .toHaveAttribute("href", `/workspaces?host=${secondHostID}`);

    await page.reload();
    await expect(page.getByRole("combobox", { name: "默认云端电脑" })).toHaveValue(secondHostID);
    await expect(page.locator(".host-default-workspace strong")).toHaveText("默认电脑二 Workspace");

    await page.goto("/assistant");
    await page.getByRole("button", { name: "打开侧边栏" }).click();
    const sidebar = page.getByRole("complementary", { name: "当前会话侧边栏" });
    await sidebar.getByRole("button", { name: "云端电脑" }).click();
    await expect(sidebar.getByRole("combobox", { name: "选择侧栏云端电脑" })).toHaveValue(secondHostID);
  });
});
