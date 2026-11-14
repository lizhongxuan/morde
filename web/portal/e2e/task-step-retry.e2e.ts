import { expect, test } from "@playwright/test";

test.describe("Assistant Task step retry", () => {
  test.skip(process.env.CH_REAL_E2E === "1", "synthetic retry scenario uses local MSW only");

  test("shows retry safety reasons and renders the accepted next attempt", async ({ page }) => {
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

      const timestamp = new Date().toISOString();
      const taskID = "qretry01";
      const retryableStepID = "sretry01";
      const blockedStepID = "sunsafe1";
      const objective = "PC 步骤重试合成验收";
      const task = {
        id: taskID,
        owner_id: "u1a2b3c4",
        initiator_id: "u1a2b3c4",
        execution_location: "server",
        conversation_id: conversationID,
        model_id: "pmodel01",
        objective,
        status: "failed",
        event_sequence: 1,
        current_revision: 4,
        desired_revision: 4,
        current_step: retryableStepID,
        step_count: 2,
        completed_step_count: 0,
        budget: { active_seconds: 1800, tool_calls: 20, model_tokens: 5000 },
        usage: { active_seconds: 2, tool_calls: 1, model_tokens: 100 },
        dependencies: [],
        child_jobs: [],
        result_review: null,
        created_at: timestamp,
        updated_at: timestamp,
        result: null,
        waiting_reason: null,
        steps: [
          { id: retryableStepID, sequence: 1, attempt: 1, kind: "tool", status: "failed", title: "搜索资料",
            summary: "Host 确认调用未派发。", failure_reason: "not_dispatched", retryable: true,
            task_revision: 4, created_at: timestamp },
          { id: blockedStepID, sequence: 2, attempt: 1, kind: "tool", status: "failed", title: "提交远端操作",
            summary: "执行结果待确认。", failure_reason: "side_effect_unknown", retryable: false,
            task_revision: 4, created_at: timestamp },
        ],
        batch_items: [],
        artifacts: [],
        pending_request: null,
        clarification_request: null,
        stop_request: null,
        assistant_message: null,
        finished_at: timestamp,
      };
      const message = {
        id: "mretry01",
        conversation_id: conversationID,
        sequence: Math.max(0, ...(state.conversationMessages ?? [])
          .filter((item) => item.conversation_id === conversationID)
          .map((item) => typeof item.sequence === "number" ? item.sequence : 0)) + 1,
        role: "user",
        content: objective,
        task_id: taskID,
        routing_status: "routed",
        delivery_status: "applied",
        created_at: timestamp,
      };
      state.conversationOwners = [...(state.conversationOwners ?? []).filter(([id]) => id !== conversationID),
        [conversationID, "u1a2b3c4"]];
      state.conversationMessages = [...(state.conversationMessages ?? []).filter((item) => item.id !== message.id), message];
      state.tasks = [...(state.tasks ?? []).filter((item) => item.id !== taskID), task];
      localStorage.setItem(storageKey, JSON.stringify(state));
      return { id: taskID, retryableStepID, objective };
    });

    await page.reload();
    await page.getByRole("button", { name: "打开侧边栏" }).click();
    await page.getByRole("button", { name: /^最近结束/ }).click();
    await page.getByRole("button", { name: new RegExp(`查看详情：${task.objective}`) }).click();
    const activity = page.locator(`.task-panel-detail .task-activity[data-task-id="${task.id}"]`);
    const retryButton = activity.getByRole("button", { name: "重试步骤 搜索资料" });
    await expect(retryButton).toBeVisible();
    await expect(activity.getByText("因操作副作用状态未知，不能安全重试。")).toBeVisible();
    await expect(activity.getByRole("button", { name: "重试步骤 提交远端操作" })).toHaveCount(0);

    const retryResponsePromise = page.waitForResponse((response) => response.request().method() === "POST" &&
      new URL(response.url()).pathname === `/api/tasks/${task.id}/steps/${task.retryableStepID}/retries`);
    await retryButton.click();
    const retryResponse = await retryResponsePromise;
    expect(retryResponse.status()).toBe(202);
    expect(await retryResponse.json()).toMatchObject({
      task_id: task.id,
      source_step_id: task.retryableStepID,
      attempt: 2,
      status: "queued",
      replayed: false,
    });
    await expect(activity.getByText("第 2 次尝试")).toBeVisible();
    await expect(retryButton).toHaveCount(0);
    await expect(activity.locator(".task-activity-status")).toHaveText("等待执行");
  });
});
