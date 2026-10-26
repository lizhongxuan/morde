import { expect, test } from "@playwright/test";

test("creates, edits, disables, and inspects scheduled tasks and notifications", async ({ page }) => {
  const onceObjective = `一次性计划验收 ${Date.now()}`;
  const weeklyObjective = `每周计划验收 ${Date.now()}`;
  await page.goto("/");
  await page.getByLabel("用户名").fill("demo");
  await page.getByLabel("密码").fill("demo");
  await page.getByRole("button", { name: "登录" }).click();
  await page.getByRole("link", { name: "AI 助手", exact: true }).click();
  await expect(page.getByRole("heading", { name: "AI 助手" })).toBeVisible();
  await page.getByRole("combobox", { name: "AI 模型" }).selectOption("pmodel01");
  await expect(page.getByText("默认模型已保存。")).toBeVisible();
  await page.getByRole("button", { name: "创建我的云端电脑" }).click();
  await expect(page.getByRole("option", { name: /我的云端电脑 · 运行中/ })).toBeAttached({ timeout: 10_000 });
  await page.getByRole("link", { name: "任务与计划", exact: true }).click();
  await expect(page.getByRole("heading", { name: "定时计划" })).toBeVisible();

  await page.getByRole("button", { name: "＋ 新建计划" }).click();
  await page.getByLabel("任务目标").fill(onceObjective);
  await page.getByLabel("错过执行时间").selectOption("run_once");
  await page.getByRole("button", { name: "创建并启用" }).click();
  const onceCard = page.locator(".schedule-card").filter({ hasText: onceObjective });
  await expect(onceCard).toContainText("已启用");
  await expect(onceCard).toContainText("一次 ·");
  await onceCard.getByRole("button", { name: "运行记录" }).click();
  await expect(onceCard.getByText("还没有运行记录。")).toBeVisible();

  await onceCard.getByRole("button", { name: "编辑" }).click();
  await page.getByLabel("任务目标").fill(`${onceObjective}（已更新）`);
  await page.getByRole("button", { name: "保存计划" }).click();
  const editedCard = page.locator(".schedule-card").filter({ hasText: `${onceObjective}（已更新）` });
  await expect(editedCard).toContainText("版本 2");
  await editedCard.getByRole("button", { name: "停用" }).click();
  await expect(editedCard).toContainText("已停用");

  await page.getByRole("button", { name: "＋ 新建计划" }).click();
  await page.getByLabel("任务目标").fill(weeklyObjective);
  await page.getByLabel("每周", { exact: true }).check();
  await page.getByLabel("每周运行日").getByLabel("周六").check();
  await page.getByRole("button", { name: "创建并启用" }).click();
  const weeklyCard = page.locator(".schedule-card").filter({ hasText: weeklyObjective });
  await expect(weeklyCard).toContainText("每周 周一、周二、周三、周四、周五、周六");

  const activeCard = page.locator(".schedule-card").filter({ hasText: "整理本周项目变更并生成摘要" });
  await activeCard.getByRole("button", { name: "停用" }).click();
  const cancelDialog = page.getByRole("dialog", { name: "停用定时计划？" });
  await cancelDialog.getByRole("button", { name: "停用并请求取消" }).click();
  await expect(page.getByText("计划已停用；当前任务的取消仍在确认中。")).toBeVisible();
  await expect(activeCard).toContainText("已取消", { timeout: 10_000 });

  await page.getByRole("link", { name: "查看通知", exact: true }).click();
  await expect(page.getByRole("heading", { name: "通知", exact: true })).toBeVisible();
  const approvalNotice = page.locator(".notification-card").filter({ hasText: "任务等待确认：发送部署完成通知？" });
  await approvalNotice.getByRole("button", { name: "标记已读" }).click();
  await expect(approvalNotice).toContainText("已读");
  await approvalNotice.getByRole("link", { name: "打开任务详情" }).click();
  await expect(page.getByRole("heading", { name: "向客户发送部署完成通知" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "发送部署完成通知？" })).toBeVisible();
});
