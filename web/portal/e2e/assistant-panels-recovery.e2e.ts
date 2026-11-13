import { expect, test } from "@playwright/test";

test.describe("Assistant panel recovery", () => {
  test.skip(process.env.CH_REAL_E2E === "1", "mock panel scenario is disabled for real-host runs");

  test("recovers a task detail after a temporary local network failure", async ({ page }) => {
    await page.route(/^https?:\/\//, async (route) => {
      const requestURL = new URL(route.request().url());
      const localOrigin = new URL("http://127.0.0.1:5183").origin;
      if (requestURL.origin !== localOrigin) {
        await route.abort();
        return;
      }
      await route.continue();
    });

    await page.addInitScript(() => {
      const nativeFetch = window.fetch.bind(window);
      window.fetch = (input, init) => {
        const requestURL = typeof input === "string" ? input : input instanceof Request ? input.url : input.href;
        const method = init?.method ?? (input instanceof Request ? input.method : "GET");
        const path = new URL(requestURL, location.href).pathname;
        if (method.toUpperCase() === "GET" && path === "/api/tasks/qrecov01" &&
          sessionStorage.getItem("e2e-task-detail-recovered") !== "1") {
          return Promise.reject(new TypeError("Failed to fetch"));
        }
        return nativeFetch(input, init);
      };
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
      if (!stored) throw new Error("本地演示状态缺失");
      const state = JSON.parse(stored) as {
        conversations: Array<Record<string, unknown>>;
        mainConversations: Array<[string, { conversation_id?: string | null }]>;
        tasks?: Array<Record<string, unknown>>;
      };
      const conversationID = state.mainConversations.find(([userID]) => userID === "u1a2b3c4")?.[1].conversation_id
        ?? state.conversations.find((conversation) => conversation.is_main === true)?.id;
      if (typeof conversationID !== "string") throw new Error("主会话尚未初始化");

      const timestamp = new Date().toISOString();
      const task = {
        id: "qrecov01",
        owner_id: "u1a2b3c4",
        initiator_id: "u1a2b3c4",
        execution_location: "server",
        conversation_id: conversationID,
        model_id: "pmodel01",
        objective: "分页验收任务详情恢复",
        status: "running",
        current_step: "srecov01",
        step_count: 1,
        completed_step_count: 0,
        event_sequence: 1,
        current_revision: 1,
        desired_revision: 1,
        budget: { active_seconds: 1800, tool_calls: 100, model_tokens: 50000 },
        usage: { active_seconds: 0, tool_calls: 0, model_tokens: null },
        dependencies: [],
        child_jobs: [],
        result_review: null,
        created_at: timestamp,
        updated_at: timestamp,
        result: null,
        waiting_reason: null,
        steps: [{ id: "srecov01", sequence: 1, kind: "tool", status: "running", title: "读取验收状态", summary: "", created_at: timestamp }],
        batch_items: [],
        artifacts: [],
        pending_request: null,
        clarification_request: null,
        stop_request: null,
        assistant_message: null,
        finished_at: null,
      };
      state.tasks = [...(state.tasks ?? []).filter((item) => item.id !== task.id), task];
      localStorage.setItem(storageKey, JSON.stringify(state));
      return task;
    });

    await page.reload();
    await page.getByRole("button", { name: "打开侧边栏" }).click();
    const taskCard = page.getByRole("button", { name: /查看详情：分页验收任务详情/ });
    await expect(taskCard).toBeVisible();
    await taskCard.click();

    const detailError = page.getByRole("alert").filter({ hasText: "任务详情暂不可用，当前状态未确认。" });
    await expect(detailError).toBeVisible();
    await expect(page.getByRole("textbox", { name: "消息内容" })).toBeVisible();
    await expect(page).toHaveURL(/\/assistant$/);

    await page.evaluate(() => sessionStorage.setItem("e2e-task-detail-recovered", "1"));
    await detailError.getByRole("button", { name: "重试" }).click();
    await expect(page.getByRole("region", { name: `任务详情：${task.objective}` })).toBeVisible();
    await expect(page.getByRole("region", { name: "当前进展" }).getByText("读取验收状态")).toBeVisible();
    await expect(page.getByRole("textbox", { name: "消息内容" })).toBeVisible();
  });
});
