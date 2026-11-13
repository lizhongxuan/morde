import { expect, test } from "@playwright/test";

const mockStorageKey = "cloudhost:portal-mock-assistant-state-v3";

async function signInWithLargeClientList(page: import("@playwright/test").Page) {
  await page.addInitScript((storageKey) => {
    const now = Date.now();
    const clients = Array.from({ length: 24 }, (_, index) => ({
      id: `cli_zoom${String(index + 1).padStart(4, "0")}`,
      platform: "desktop",
      display_name: index === 0 ? "放大文字首项" : index === 23 ? "放大文字末项" : `放大文字验收设备 ${String(index + 1).padStart(2, "0")}`,
      expires_at: new Date(now + 30 * 86_400_000).toISOString(),
      revoked: false,
      created_at: new Date(now - 60_000).toISOString(),
      last_used_at: null,
    }));
    localStorage.setItem(storageKey, JSON.stringify({
      apiKeys: [],
      clients,
      clientOwners: clients.map(({ id }) => [id, "u1a2b3c4"]),
      hosts: [],
      workspaces: [],
      workspaceOwners: [],
      userPreferences: [],
    }));
  }, mockStorageKey);
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/");
  await page.getByLabel("用户名").fill("demo");
  await page.getByLabel("密码").fill("demo");
  await page.getByRole("button", { name: "登录" }).click();
  await page.goto("/clients");
}

async function expectPageFitsViewport(page: import("@playwright/test").Page) {
  const dimensions = await page.evaluate(() => {
    const content = document.querySelector<HTMLElement>("#main-content");
    if (!content) throw new Error("页面内容容器不存在");
    return {
      documentHeight: document.documentElement.scrollHeight,
      viewportHeight: document.documentElement.clientHeight,
      contentHeight: content.scrollHeight,
      contentViewportHeight: content.clientHeight,
    };
  });
  expect(dimensions.documentHeight).toBeLessThanOrEqual(dimensions.viewportHeight + 1);
  expect(dimensions.contentHeight).toBeLessThanOrEqual(dimensions.contentViewportHeight + 1);
}

test.describe("screen pagination at enlarged text size", () => {
  test.skip(process.env.CH_REAL_E2E === "1", "local MSW fixture scenario is disabled for real-host runs");

  test("keeps a long client list paginated and hidden actions unfocusable at 32px text", async ({ page }) => {
    await signInWithLargeClientList(page);
    await page.addStyleTag({ content: ".device-manager-card, .device-manager-card * { font-size: 32px !important; line-height: 1.35 !important; }" });

    const rows = page.locator(".screen-pagination__item");
    const pagination = page.getByRole("navigation", { name: "已登录客户端分页" });
    const status = pagination.locator(".screen-pagination__status");
    await expect(rows).toHaveCount(24);
    await expect(page.getByRole("heading", { name: "放大文字首项" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "放大文字末项" })).toBeHidden();
    expect(await page.locator(".device-manager-card h2").first().evaluate((element) => getComputedStyle(element).fontSize)).toBe("32px");
    await expect(status).toHaveText(/^1 \/ \d+$/);
    await expectPageFitsViewport(page);

    const firstHiddenRow = page.locator(".screen-pagination__item[hidden]").first();
    const hiddenAction = firstHiddenRow.locator("button");
    await expect(hiddenAction).not.toBeVisible();
    await hiddenAction.evaluate((element) => (element as HTMLButtonElement).focus());
    expect(await hiddenAction.evaluate((element) => document.activeElement === element)).toBe(false);

    const firstPageCount = await rows.evaluateAll((items) => items.filter((item) => !item.hasAttribute("hidden")).length);
    expect(firstPageCount).toBeGreaterThan(0);
    expect(firstPageCount).toBeLessThan(24);
    await page.locator(".screen-pagination__viewport").focus();
    await page.keyboard.press("PageDown");
    await expect(status).toHaveText(/^2 \/ \d+$/);
    await expect(rows.first()).toHaveAttribute("hidden", "");
    await expectPageFitsViewport(page);

    let pagePosition = (await status.innerText()).split("/").map((value) => Number(value.trim()));
    for (let index = 0; pagePosition[0] < pagePosition[1] && index < 40; index += 1) {
      await pagination.getByRole("button", { name: "下一页" }).click();
      pagePosition = (await status.innerText()).split("/").map((value) => Number(value.trim()));
    }
    expect(pagePosition[0]).toBe(pagePosition[1]);
    await expect(page.getByRole("heading", { name: "放大文字首项" })).toBeHidden();
    await expect(page.getByRole("heading", { name: "放大文字末项" })).toBeVisible();
    await expectPageFitsViewport(page);
  });

});
