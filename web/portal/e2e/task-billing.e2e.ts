import { expect, test } from "@playwright/test";

test.describe("Assistant Task billing", () => {
  test.skip(process.env.CH_REAL_E2E === "1", "synthetic billing scenario uses local MSW only");

  test("shows settled, reserved, unknown, multi-rate, and BYOK totals in task details", async ({ page }) => {
    await page.route(/^https?:\/\//, async (route) => {
      const requestURL = new URL(route.request().url());
      if (requestURL.origin !== new URL("http://127.0.0.1:5183").origin) {
        await route.abort();
        return;
      }
      await route.continue();
    });

    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.goto("/assistant");
    await expect(page.getByRole("button", { name: /^主会话/ })).toBeVisible();

    const task = await page.evaluate(() => {
      const storageKey = "cloudhost:portal-mock-assistant-state-v3";
      const stored = localStorage.getItem(storageKey);
      if (!stored) throw new Error("本地 MSW 演示状态缺失");
      const state = JSON.parse(stored) as {
        conversations: Array<Record<string, unknown>>;
        mainConversations: Array<[string, { conversation_id?: string | null }]>;
        conversationMessages?: Array<Record<string, unknown>>;
        conversationOwners?: Array<[string, string]>;
        tasks?: Array<Record<string, unknown>>;
      };
      const conversationID = state.mainConversations.find(([userID]) => userID === "u1a2b3c4")?.[1].conversation_id
        ?? state.conversations.find((conversation) => conversation.is_main === true)?.id;
      if (typeof conversationID !== "string") throw new Error("本地主会话尚未初始化");

      const taskID = "qbill001";
      const objective = "Task 费用与用量验收";
      const timestamp = new Date().toISOString();
      const task = {
        id: taskID,
        owner_id: "u1a2b3c4",
        initiator_id: "u1a2b3c4",
        execution_location: "server",
        conversation_id: conversationID,
        model_id: "pmodel01",
        objective,
        status: "completed",
        event_sequence: 4,
        current_revision: 1,
        desired_revision: 1,
        budget: { active_seconds: 100, tool_calls: 10, model_tokens: 1000 },
        usage: { active_seconds: 12, tool_calls: 2, model_tokens: 73 },
        billing: { model_calls: 4, costs: [
          { state: "settled", currency: "USD", calls: 1, rate_versions: [2], reserved_micros: 1_000_000, actual_micros: 750_000 },
          { state: "estimated", currency: "USD", calls: 1, rate_versions: [3], reserved_micros: 250_000, actual_micros: null },
          { state: "unknown", calls: 1, rate_versions: [], reserved_micros: 0, actual_micros: null },
          { state: "byok", calls: 1, rate_versions: [], reserved_micros: 0, actual_micros: null },
        ] },
        dependencies: [],
        child_jobs: [],
        result_review: null,
        created_at: timestamp,
        updated_at: timestamp,
        finished_at: timestamp,
        result: "完成费用投影验收。",
        waiting_reason: null,
        steps: [],
        batch_items: [],
        artifacts: [],
        pending_request: null,
        clarification_request: null,
        stop_request: null,
        assistant_message: null,
      };
      const sequence = Math.max(0, ...(state.conversationMessages ?? [])
        .filter((message) => message.conversation_id === conversationID)
        .map((message) => typeof message.sequence === "number" ? message.sequence : 0)) + 1;
      state.conversationOwners = [...(state.conversationOwners ?? []).filter(([id]) => id !== conversationID),
        [conversationID, "u1a2b3c4"]];
      state.conversationMessages = [...(state.conversationMessages ?? []).filter((message) => message.id !== "mbill001"), {
        id: "mbill001",
        conversation_id: conversationID,
        sequence,
        role: "user",
        content: objective,
        task_id: taskID,
        routing_status: "routed",
        delivery_status: "applied",
        created_at: timestamp,
      }];
      state.tasks = [...(state.tasks ?? []).filter((item) => item.id !== taskID), task];
      localStorage.setItem(storageKey, JSON.stringify(state));
      return { id: taskID, objective };
    });

    await page.reload();
    await page.getByRole("button", { name: "打开侧边栏" }).click();
    await page.getByRole("button", { name: /^最近结束/ }).click();
    await page.getByRole("button", { name: new RegExp(`查看详情：${task.objective}`) }).click();

    const activity = page.locator(`.task-panel-detail .task-activity[data-task-id="${task.id}"]`);
    await activity.getByText("更多信息").click();
    const billing = activity.getByRole("region", { name: "用量与费用" });
    await expect(billing).toBeVisible();
    await expect(billing).toContainText("模型调用 4 次 · Token 73 / 1000");
    await expect(billing).toContainText("工具调用 2 / 10 · 活跃时间 12 / 100 秒");
    await expect(billing).toContainText("已结算 USD 0.75");
    await expect(billing).toContainText("预留估算 USD 0.25（非实际费用）");
    await expect(billing).toContainText("费用未知");
    await expect(billing).toContainText("自带模型 · Morde 未计费");
    await expect(billing).toContainText("费率快照：v2");
    await expect(billing).toContainText("费率快照：v3");
  });
});
