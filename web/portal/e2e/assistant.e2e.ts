import { expect, test } from "@playwright/test";

test.describe("Assistant durable tasks", () => {
  test.skip(process.env.CH_REAL_E2E === "1", "mock chat scenario is disabled for real-host runs");

  test("starts with one stopped 1-core 2-GiB computer and shows the quota message", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.getByRole("link", { name: "云端电脑", exact: true }).click();
    await expect(page.locator(".host-card").first()).toContainText("1 核 / 2 GiB");
    await expect(page.locator(".host-card").first()).toContainText("已停止");
    await page.getByRole("button", { name: "＋ 创建电脑", exact: true }).click();
    await expect(page.getByText("达到云端电脑上限,无法再创建")).toBeVisible();
  });

  test("starts separate durable tasks in one conversation and restores them", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.getByRole("link", { name: "AI 助手", exact: true }).click();
    await expect(page.getByRole("link", { name: "任务与计划", exact: true })).toHaveCount(0);

    await page.getByRole("button", { name: "＋ 新对话" }).click();
    const firstPrompt = `独立任务一 ${Date.now()}`;
    const secondPrompt = `独立任务二 ${Date.now()}`;
    await page.getByRole("combobox", { name: "选择 AI 模型" }).selectOption("pmodel01");
    await page.getByRole("textbox", { name: "消息内容" }).fill(firstPrompt);
    await page.getByRole("button", { name: "发送", exact: true }).click();
    await expect(page.locator(".chat-message-user p").filter({ hasText: firstPrompt })).toBeVisible();
    await expect(page.locator(".task-activity")).toHaveCount(1);
    const taskActivity = page.locator(".task-activity").first();
    await expect(taskActivity).toContainText("正在等待开始");
    await expect(page.getByRole("button", { name: "停止任务" })).toBeVisible();
    await expect(page.getByRole("button", { name: "暂停任务" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "设为主会话" })).toHaveCount(0);

    await page.getByRole("textbox", { name: "消息内容" }).fill(secondPrompt);
    await page.getByRole("button", { name: "发送", exact: true }).click();
    await expect(page.locator(".chat-message-user p").filter({ hasText: secondPrompt })).toBeVisible();
    await expect(page.locator(".task-activity")).toHaveCount(2);

    await page.reload();
    await page.getByRole("button", { name: new RegExp(firstPrompt) }).click();
    await expect(page.locator(".chat-message-user p").filter({ hasText: firstPrompt })).toBeVisible();
    await expect(page.locator(".chat-message-user p").filter({ hasText: secondPrompt })).toBeVisible();
    await expect(page.locator(".task-activity")).toHaveCount(2);
  });

  test("keeps chat controls usable at desktop and enlarged text widths", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.getByRole("link", { name: "AI 助手", exact: true }).click();

    for (const scenario of [{ width: 1280, height: 720, fontSize: "16px" }, { width: 640, height: 360, fontSize: "16px" }]) {
      await page.setViewportSize({ width: scenario.width, height: scenario.height });
      await page.evaluate((fontSize) => { document.documentElement.style.fontSize = fontSize; }, scenario.fontSize);
      const dimensions = await page.evaluate(() => ({ width: document.documentElement.clientWidth, scrollWidth: document.documentElement.scrollWidth }));
      expect(dimensions.scrollWidth).toBeLessThanOrEqual(dimensions.width);
      await expect(page.getByRole("textbox", { name: "消息内容" })).toBeVisible();
      await expect(page.getByRole("button", { name: "发送", exact: true })).toBeVisible();
      const layout = page.locator(".assistant-layout");
      const beforeHeight = await layout.evaluate((element) => element.getBoundingClientRect().height);
      await page.getByRole("textbox", { name: "消息内容" }).fill("测试输入不会拉长页面。".repeat(400));
      const afterHeight = await layout.evaluate((element) => element.getBoundingClientRect().height);
      const inputHeight = await page.getByRole("textbox", { name: "消息内容" }).evaluate((element) => element.getBoundingClientRect().height);
      expect(afterHeight).toBe(beforeHeight);
      expect(inputHeight).toBeLessThanOrEqual(120);
    }
  });
});
