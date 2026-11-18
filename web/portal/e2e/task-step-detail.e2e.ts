import { expect, test } from "@playwright/test";

test.describe("Assistant Task step detail", () => {
  test.skip(process.env.CH_REAL_E2E === "1", "synthetic detail scenario uses local MSW only");

  test("reads safe tool details only when opened and renders output as text", async ({ page }) => {
    let detailReads = 0;
    let taskSnapshotLeakedDetail = false;
    page.on("request", (request) => {
      if (new URL(request.url()).pathname === "/api/tasks/qdetail01/steps/sdetail01") detailReads += 1;
    });
    page.on("response", async (response) => {
      const path = new URL(response.url()).pathname;
      if (!(path === "/api/tasks" || path.startsWith("/api/tasks/") || path.endsWith("/tasks")) || path.includes("/steps/")) return;
      const body = await response.json().catch(() => null);
      if (JSON.stringify(body).includes("<script>not executable</script>")) taskSnapshotLeakedDetail = true;
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
      const objective = "任务步骤详情合成验收";
      const task = {
        id: "qdetail01",
        owner_id: "u1a2b3c4",
        initiator_id: "u1a2b3c4",
        execution_location: "server",
        conversation_id: conversationID,
        model_id: "pmodel01",
        objective,
        status: "completed",
        queue_priority: "normal",
        event_sequence: 2,
        current_revision: 1,
        desired_revision: 1,
        current_step: null,
        step_count: 1,
        completed_step_count: 1,
        budget: { active_seconds: 100, tool_calls: 10, model_tokens: 1000 },
        usage: { active_seconds: 2, tool_calls: 1, model_tokens: 30 },
        dependencies: [],
        child_jobs: [],
        result_review: null,
        created_at: timestamp,
        updated_at: timestamp,
        finished_at: timestamp,
        result: "任务完成。",
        waiting_reason: null,
        steps: [{ id: "sdetail01", sequence: 1, attempt: 1, retryable: false, kind: "tool", status: "succeeded",
          title: "读取项目文件", summary: "读取成功。", created_at: timestamp,
          mock_detail: { task_id: "qdetail01", step_id: "sdetail01", event_sequence: 2, tool_name: "morde_workspace_read",
            detail_state: "available", object: "README.md", input: "读取项目说明",
            result: "<script>not executable</script>", truncated: true } }],
        batch_items: [],
        artifacts: [],
        pending_request: null,
        clarification_request: null,
        stop_request: null,
        assistant_message: null,
      };
      state.conversationOwners = [...(state.conversationOwners ?? []).filter(([id]) => id !== conversationID),
        [conversationID, "u1a2b3c4"]];
      state.conversationMessages = [...(state.conversationMessages ?? []).filter((message) => message.id !== "mdetail01"), {
        id: "mdetail01",
        conversation_id: conversationID,
        sequence: Math.max(0, ...(state.conversationMessages ?? [])
          .filter((message) => message.conversation_id === conversationID)
          .map((message) => typeof message.sequence === "number" ? message.sequence : 0)) + 1,
        role: "user",
        content: objective,
        task_id: "qdetail01",
        routing_status: "routed",
        delivery_status: "applied",
        created_at: timestamp,
      }];
      state.tasks = [...(state.tasks ?? []).filter((item) => item.id !== "qdetail01"), task];
      localStorage.setItem(storageKey, JSON.stringify(state));
      return { id: task.id, objective };
    });

    await page.reload();
    await page.getByRole("button", { name: "打开侧边栏" }).click();
    await page.getByRole("button", { name: /^最近结束/ }).click();
    await page.getByRole("button", { name: new RegExp(`查看详情：${task.objective}`) }).click();
    const activity = page.locator(`.task-panel-detail .task-activity[data-task-id="${task.id}"]`);
    expect(detailReads).toBe(0);
    await activity.getByText("更多信息").click();
    await activity.getByRole("button", { name: /读取项目文件/ }).click();
    const detail = activity.getByRole("region", { name: "动作详情：读取项目文件" });
    await expect(detail).toContainText("morde_workspace_read");
    await expect(detail).toContainText("README.md");
    await expect(detail).toContainText("<script>not executable</script>");
    await expect(detail).toContainText("内容已截断。");
    await expect(activity.locator("script")).toHaveCount(0);
    expect(detailReads).toBe(1);
    expect(taskSnapshotLeakedDetail).toBe(false);
  });
});
