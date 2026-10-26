import { expect, test } from "@playwright/test";

test("[mock] answers a clarification and returns the task to the queue", async ({ page }) => {
  await page.goto("/");
  await page.getByLabel("用户名").fill("demo");
  const password = page.getByLabel("密码");
  await password.fill("demo");
  await password.press("Enter");

  await page.getByRole("link", { name: "AI 助手", exact: true }).click();
  await expect(page.getByRole("heading", { name: "AI 助手" })).toBeVisible();
  await page.getByRole("link", { name: "任务与计划", exact: true }).click();
  await page.getByRole("link", { name: "查看任务 生成本周项目进度报告" }).click();

  await expect(page.getByRole("heading", { name: "任务等你回答" })).toBeVisible();
  await expect(page.getByText("报告需要包含哪几天？是否要加入风险和下周计划？")).toBeVisible();
  await page.getByLabel("你的回答").fill("本周工作日，并包含风险和下周计划。");
  await page.getByRole("button", { name: "提交回答" }).click();

  await expect(page.getByRole("status")).toContainText("已提交补充信息，任务已重新排队。");
  await expect(page.locator(".task-detail-heading .status")).toHaveText("排队中");
  await expect(page.getByRole("heading", { name: "任务等你回答" })).toHaveCount(0);
});

test("[mock] stops AI tasks, then re-enables without reviving work", async ({ page }) => {
  await page.goto("/");
  await page.getByLabel("用户名").fill("demo");
  await page.getByLabel("密码").fill("demo");
  await page.getByRole("button", { name: "登录" }).click();
  await page.getByRole("link", { name: "AI 助手", exact: true }).click();
  await expect(page.getByRole("heading", { name: "AI 助手" })).toBeVisible();
  await page.getByRole("link", { name: "任务与计划", exact: true }).click();

  await expect(page.getByRole("heading", { name: "AI 执行" })).toBeVisible();
  await page.getByRole("button", { name: "停止所有 AI 执行" }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("MCP 电脑操作会请求运行端停止");
  await dialog.getByRole("button", { name: "停止 AI 执行" }).click();

  await expect(page.getByText("停止中")).toBeVisible();
  await expect(page.getByText(/剩余 3 个执行项/)).toBeVisible();
  await expect(page.getByRole("button", { name: "重新开启" })).toBeVisible({ timeout: 8_000 });
  await page.getByRole("button", { name: "重新开启" }).click();
  await expect(page.getByText("已开启")).toBeVisible();
  await expect(page.getByText("重新开启只允许新任务运行，不会恢复已取消的任务。")).toHaveCount(0);
});
