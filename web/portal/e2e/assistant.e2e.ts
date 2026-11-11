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

  test("accepts consecutive messages and restores independent Task workchains", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.getByRole("link", { name: "moder", exact: true }).click();
    await expect(page.getByRole("link", { name: "任务与计划", exact: true })).toHaveCount(0);

    await page.getByRole("button", { name: "新建旁路会话" }).click();
    await page.getByRole("combobox", { name: "选择目标电脑" }).selectOption("");
    const firstPrompt = `普通文字问题一 ${Date.now()}`;
    const secondPrompt = `普通文字问题二 ${Date.now()}`;
    await page.getByRole("textbox", { name: "消息内容" }).fill(firstPrompt);
    await page.getByRole("button", { name: "发送", exact: true }).click();
    await expect(page.locator(".chat-message-user p").filter({ hasText: firstPrompt })).toBeVisible();
    await expect(page.locator(".chat-message-user").filter({ hasText: "已受理，等待处理" })).toContainText(firstPrompt);

    await page.getByRole("textbox", { name: "消息内容" }).fill(secondPrompt);
    await page.getByRole("button", { name: "发送", exact: true }).click();
    await expect(page.locator(".chat-message-user p").filter({ hasText: secondPrompt })).toBeVisible();
    await expect(page.locator(".chat-message-user").filter({ hasText: "已受理，等待处理" })).toHaveCount(2);
    await expect(page.getByRole("button", { name: "任务动态" })).toHaveAttribute("aria-expanded", "false");
    await expect(page.locator(".task-activity")).toHaveCount(0);

    const taskIDs = await page.evaluate(({ firstPrompt, secondPrompt }) => {
      const storageKey = "cloudhost:portal-mock-assistant-state-v3";
      const stored = localStorage.getItem(storageKey);
      if (!stored) throw new Error("本地演示状态缺失");
      const state = JSON.parse(stored) as {
        conversationMessages: Array<Record<string, unknown>>;
        tasks?: Array<Record<string, unknown>>;
      };
      const first = state.conversationMessages.find((message) => message.content === firstPrompt);
      const second = state.conversationMessages.find((message) => message.content === secondPrompt);
      if (!first || !second || first.conversation_id !== second.conversation_id) throw new Error("演示消息不在同一对话");
      const conversationID = String(first.conversation_id);
      const timestamp = new Date().toISOString();
      const firstTaskID = "qdemo001";
      const secondTaskID = "qdemo002";
      const firstReplyID = "mdemo001";
      const secondReplyID = "mdemo002";
      const firstReplyText = "第一项任务已完成首轮建议。";
      const secondReplyText = "第二项任务正在读取网页。";
      Object.assign(first, { sequence: 1, task_id: firstTaskID, routing_status: "routed", delivery_status: "applied" });
      Object.assign(second, { sequence: 3, task_id: secondTaskID, routing_status: "routed", delivery_status: "applied" });
      const firstReply = { id: firstReplyID, conversation_id: conversationID, sequence: 2, role: "assistant", content: firstReplyText,
        task_id: firstTaskID, delivery_status: "applied", output_status: "completed", output_task_revision: 1, created_at: timestamp };
      const secondReply = { id: secondReplyID, conversation_id: conversationID, sequence: 4, role: "assistant", content: secondReplyText,
        task_id: secondTaskID, delivery_status: "applied", output_status: "streaming", output_task_revision: 1, created_at: timestamp };
      const makeTask = (id: string, objective: string, status: "waiting_user" | "running", stepID: string, stepTitle: string,
        replyID: string, sequence: number, replyText: string, replyStatus: "completed" | "streaming") => ({
        id, owner_id: "u1a2b3c4", initiator_id: "u1a2b3c4", execution_location: "server", conversation_id: conversationID,
        model_id: "pmodel01", objective, status, current_step: stepID, event_sequence: 5,
        current_revision: 1, desired_revision: 1,
        budget: { active_seconds: 1800, tool_calls: 100, model_tokens: 50000 },
        usage: { active_seconds: 0, tool_calls: 0, model_tokens: null }, created_at: timestamp, updated_at: timestamp,
        result: null, waiting_reason: status === "waiting_user" ? "approval" : null,
        steps: [{ id: stepID, sequence: 1, kind: "tool", status: status === "waiting_user" ? "waiting_user" : "running",
          title: stepTitle, summary: "", created_at: timestamp }],
        batch_items: [], artifacts: [],
        pending_request: status === "waiting_user" ? { id: "kdem0001", version: 1, kind: "approval", title: "写入文件",
          object: "报告.md", content: "准备创建报告。", impact: "会新增一个文件。" } : null,
        clarification_request: null, stop_request: null,
        assistant_message: { id: replyID, conversation_id: conversationID, sequence, task_id: id, task_revision: 1,
          role: "assistant", content: replyText, status: replyStatus, created_at: timestamp },
        finished_at: null,
      });
      const firstTask = makeTask(firstTaskID, firstPrompt, "waiting_user", "sdemo001",
        "Run cloud dsh MCP tool: morde_workspace_write", firstReplyID, 2, firstReplyText, "completed");
      const secondTask = makeTask(secondTaskID, secondPrompt, "running", "sdemo002",
        "Run cloud dsh MCP tool: morde_web_read", secondReplyID, 4, secondReplyText, "streaming");
      state.conversationMessages = [
        ...state.conversationMessages.filter((message) => message.conversation_id !== conversationID),
        first, firstReply, second, secondReply,
      ];
      state.tasks = [
        ...(state.tasks ?? []).filter((task) => task.conversation_id !== conversationID), firstTask, secondTask,
      ];
      localStorage.setItem(storageKey, JSON.stringify(state));
      return [firstTaskID, secondTaskID] as const;
    }, { firstPrompt, secondPrompt });

    await page.reload();
    await page.getByRole("button", { name: /新对话 本机最近查看/ }).click();
    await expect(page.locator(".chat-message-user p").filter({ hasText: firstPrompt })).toBeVisible();
    await expect(page.locator(".chat-message-user p").filter({ hasText: secondPrompt })).toBeVisible();
    const assistantReply = page.locator(".chat-message-assistant").filter({ hasText: "第一项任务已完成首轮建议。" });
    const replyTime = assistantReply.locator("..").locator(".assistant-reply-time");
    await expect(replyTime).toHaveCSS("opacity", "0");
    await assistantReply.hover();
    await expect(replyTime).toHaveCSS("opacity", "1");
    await expect(replyTime).toHaveAttribute("datetime", /.+/);
    await page.getByRole("button", { name: "任务动态" }).click();
    const firstPanel = page.locator(`.task-activity[aria-label="任务 ${taskIDs[0]} 的工作过程"]`);
    const secondPanel = page.locator(`.task-activity[aria-label="任务 ${taskIDs[1]} 的工作过程"]`);
    await expect(firstPanel).toContainText("任务正在等待你的确认或补充信息。");
    await expect(secondPanel).toContainText("AI 正在处理…");
    await expect(firstPanel).toContainText("保存工作区文件");
    await expect(secondPanel).toContainText("读取网页");
    await expect(page.getByText("第一项任务已完成首轮建议。")).toBeVisible();
    await expect(page.getByText("第二项任务正在读取网页。")).toBeVisible();
    await expect(page.locator(".message-list article").first()).toHaveClass(/chat-message-user/);
    await expect(page.locator(".message-list .task-activity")).toHaveCount(0);

    await expect(firstPanel.getByRole("button", { name: "暂停任务" })).toBeVisible();
    await expect(secondPanel.getByRole("button", { name: "暂停任务" })).toBeVisible();
    await firstPanel.getByRole("button", { name: "停止任务" }).click();
    await expect(firstPanel).toContainText("正在等待服务端确认停止结果。");
    await expect(secondPanel).toContainText("AI 正在处理…");
    await expect(page.locator(".assistant-composer").getByRole("button", { name: "停止任务" })).toHaveCount(0);
    await expect(page.locator(".assistant-composer").getByRole("button", { name: "暂停任务" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "设为主会话" })).toHaveCount(0);
    await expect(page.locator(".assistant-composer .assistant-progress")).toHaveCount(0);
    await expect(page.locator(".assistant-composer")).not.toContainText("AI 正在处理…");
  });

  test("accepts a long multilingual message through resumable upload", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.getByRole("link", { name: "moder", exact: true }).click();
    await page.getByRole("button", { name: "新建旁路会话" }).click();
    await page.getByRole("combobox", { name: "选择目标电脑" }).selectOption("");

    const prompt = "长消息中文🙂".repeat(4_500);
    expect(new TextEncoder().encode(prompt).length).toBeGreaterThan(32 * 1024);
    await page.getByRole("textbox", { name: "消息内容" }).fill(prompt);
    await page.getByRole("button", { name: "发送", exact: true }).click();
    const message = page.locator(".chat-message-user").filter({ hasText: prompt.slice(0, 24) }).last();
    await expect(message).toContainText("已受理，等待处理");
    await expect(page.getByRole("textbox", { name: "消息内容" })).toHaveValue("");
    const saved = await page.evaluate((prefix) => {
      const state = JSON.parse(localStorage.getItem("cloudhost:portal-mock-assistant-state-v3") ?? "null") as {
        conversationMessages?: Array<{ content: string; client_message_id?: string }>;
      } | null;
      const matches = (state?.conversationMessages ?? []).filter((item) => item.content.startsWith(prefix));
      return { count: matches.length, contentLength: matches[0]?.content.length, clientMessageID: matches[0]?.client_message_id };
    }, prompt.slice(0, 24));
    expect(saved).toMatchObject({ count: 1, contentLength: prompt.length });
    expect(saved.clientMessageID).toBeTruthy();
  });

  test("keeps chat controls usable at desktop and enlarged text widths", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.getByRole("link", { name: "moder", exact: true }).click();

    for (const scenario of [{ width: 1280, height: 720, fontSize: "16px" }, { width: 640, height: 360, fontSize: "16px" }]) {
      await page.setViewportSize({ width: scenario.width, height: scenario.height });
      await page.evaluate((fontSize) => { document.documentElement.style.fontSize = fontSize; }, scenario.fontSize);
      const dimensions = await page.evaluate(() => ({ width: document.documentElement.clientWidth, scrollWidth: document.documentElement.scrollWidth }));
      expect(dimensions.scrollWidth).toBeLessThanOrEqual(dimensions.width);
      await expect(page.getByRole("textbox", { name: "消息内容" })).toBeVisible();
      await expect(page.getByRole("button", { name: "发送", exact: true })).toBeVisible();
      const layout = page.locator(".assistant-layout");
      const beforeHeight = await layout.evaluate((element) => element.getBoundingClientRect().height);
      const composer = page.locator(".assistant-composer");
      const beforeBottom = await composer.evaluate((element) => element.getBoundingClientRect().bottom);
      await page.getByRole("textbox", { name: "消息内容" }).fill("测试输入不会拉长页面。".repeat(400));
      const afterHeight = await layout.evaluate((element) => element.getBoundingClientRect().height);
      const inputHeight = await page.getByRole("textbox", { name: "消息内容" }).evaluate((element) => element.getBoundingClientRect().height);
      const afterBottom = await composer.evaluate((element) => element.getBoundingClientRect().bottom);
      expect(afterHeight).toBe(beforeHeight);
      expect(inputHeight).toBeLessThanOrEqual(120);
      expect(afterBottom).toBe(beforeBottom);
      await page.getByRole("button", { name: "任务动态" }).click();
      const sendIsUsable = await page.getByRole("button", { name: "发送", exact: true }).evaluate((button) => {
        const rect = button.getBoundingClientRect();
        const target = document.elementFromPoint(rect.left + rect.width / 2, rect.top + rect.height / 2);
        return rect.top >= 0 && rect.bottom <= window.innerHeight && (target === button || button.contains(target));
      });
      expect(sendIsUsable).toBeTruthy();
      await page.getByRole("button", { name: "关闭任务动态" }).click();
    }
  });
});
