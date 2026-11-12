import { expect, test } from "@playwright/test";

test("keeps the administration shell and navigation usable at supported desktop widths", async ({ page }) => {
  await page.goto("/admin/login");
  await page.getByLabel("用户名").fill("demo");
  await page.getByLabel("密码").fill("demo");
  await page.getByRole("button", { name: "登录" }).click();
  await expect(page.locator(".morde-admin-shell")).toBeVisible();

  for (const viewport of [{ width: 1440, height: 900 }, { width: 1024, height: 768 }, { width: 800, height: 600 }]) {
    await page.setViewportSize(viewport);
    for (const route of ["/admin/", "/admin/users", "/admin/settings", "/admin/hosts", "/admin/audit"]) {
      await page.goto(route);
      await expect(page.locator(".morde-admin-shell")).toBeVisible();
      const dimensions = await page.evaluate(() => ({
        width: document.documentElement.clientWidth,
        scrollWidth: document.documentElement.scrollWidth,
      }));
      expect(dimensions.scrollWidth, `${route} at ${viewport.width}px`).toBeLessThanOrEqual(dimensions.width + 1);
      await expect(page.getByRole("navigation", { name: "管理导航" })).toBeVisible();
    }
  }
});
