import { expect, test } from "@playwright/test";

test.describe("Assistant instant chat", () => {
  test.skip(process.env.CH_REAL_E2E === "1", "mock chat scenario is disabled for real-host runs");

  test("starts with one stopped 1-core 2-GiB computer and shows the quota message", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.getByRole("link", { name: "云端电脑", exact: true }).click();
    await expect(page.locator(".host-card").first()).toContainText("1 核 / 2 GiB");
    await expect(page.locator(".host-card").first()).toContainText("已停止");
    await page.getByRole("button", { name: "创建电脑", exact: true }).click();
    await expect(page.getByText("达到云端电脑上限,无法再创建")).toBeVisible();
  });

  test("sends a message, restores the conversation, and has no task entry", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.getByRole("link", { name: "AI 助手", exact: true }).click();
    await expect(page.getByRole("link", { name: "任务与计划", exact: true })).toHaveCount(0);

    const prompt = `即时对话验收 ${Date.now()}`;
    await page.getByRole("combobox", { name: "选择 AI 模型" }).selectOption("pmodel01");
    await page.getByRole("textbox", { name: "消息内容" }).fill(prompt);
    await page.getByRole("button", { name: "发送", exact: true }).click();
    await expect(page.getByText(`已收到：${prompt}`)).toBeVisible();

    await page.reload();
    await page.getByRole("button", { name: new RegExp(prompt) }).click();
    await expect(page.getByText(`已收到：${prompt}`)).toBeVisible();
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
    }
  });
});
