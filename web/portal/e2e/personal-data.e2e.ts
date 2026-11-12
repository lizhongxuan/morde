import { expect, test } from "@playwright/test";

test.describe("personal data auto-save", () => {
  test.skip(process.env.CH_REAL_E2E === "1", "local MSW fixture scenario is disabled for real-host runs");

  test("submits profile changes automatically and keeps the account page free of manual sync actions", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.goto("/personal");

    await expect(page.getByRole("region", { name: "个人资料" })).toBeVisible();
    await expect(page.getByText("保存后自动更新云端")).toBeVisible();
    await expect(page.getByRole("button", { name: /立即同步|立即更新/ })).toHaveCount(0);
    await page.getByLabel("名称").fill("自动保存验收用户");
    const saveResponse = page.waitForResponse((response) =>
      response.url().endsWith("/api/personal-data/profile/default") && response.request().method() === "PUT",
    );
    await page.getByRole("button", { name: "保存个人资料" }).click();
    expect((await saveResponse).status()).toBe(200);

    await expect(page.getByText("0 项待提交 · 0 条冲突草稿")).toBeVisible();
    const saved = await page.evaluate(async () => {
      const response = await fetch("/api/personal-data/profile/default", { credentials: "include" });
      if (!response.ok) throw new Error(`个人资料读取失败：${response.status}`);
      return response.json() as Promise<{ object_type: string; object_id: string; revision: number; body: Record<string, unknown> }>;
    });
    expect(saved).toMatchObject({
      object_type: "profile",
      object_id: "default",
      revision: 1,
      body: { name: "自动保存验收用户" },
    });
  });
});
