import { expect, test } from "@playwright/test";

test("keeps the administration shell and navigation usable at supported desktop widths", async ({ page }) => {
  const logoResponse = page.waitForResponse((response) => response.url().includes("morde-logo-reference.png"));
  await page.goto("/admin/login");
  await page.getByLabel("用户名").fill("demo");
  await page.getByLabel("密码").fill("demo");
  await page.getByRole("button", { name: "登录" }).click();
  await expect(page.locator(".morde-admin-shell")).toBeVisible();
  expect((await logoResponse).ok()).toBe(true);

  for (const viewport of [{ width: 1440, height: 900 }, { width: 1024, height: 768 }, { width: 800, height: 600 }]) {
    await page.setViewportSize(viewport);
    for (const route of ["/admin/", "/admin/users", "/admin/users/u1a2b3c4", "/admin/settings", "/admin/hosts", "/admin/hosts/h-missing01", "/admin/audit"]) {
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
  await page.goto("/admin/users/u1a2b3c4");
  await expect(page.getByRole("heading", { name: "demo" })).toBeVisible();
  await page.goto("/admin/hosts/h-missing01");
  await expect(page.getByRole("heading", { name: "找不到这台电脑" })).toBeVisible();
});
