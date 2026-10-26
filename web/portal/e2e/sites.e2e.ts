import { expect, test } from '@playwright/test';

const portalUrl = process.env.CH_PORTAL_E2E_URL ?? 'http://127.0.0.1:5183';
const userID = 'u1234567';
const hostID = 'h1234567';
const workspaceID = 'w1234567';
const siteID = 'site_0123456789abcdef';
const releaseID = 'rel_0123456789abcdef';
const timestamp = '2026-10-22T08:00:00.000Z';

test('[mock] creates a private Site snapshot from a pinned Workspace revision', async ({ browser }) => {
  test.skip(true, 'Site snapshot UI is outside the current MVP scope');
  const context = await browser.newContext({ serviceWorkers: 'block' });
  const calls: { path: string; method: string; body?: unknown; idempotencyKey?: string }[] = [];
  let siteCreated = false;
  let candidateCreated = false;
  let materialized = false;
  const workspace = {
    id: workspaceID, name: '博客项目', revision: 4, used_bytes: 2048, quota_bytes: 1048576,
    state: 'ready', created_at: timestamp, updated_at: timestamp,
  };
  const site = {
    id: siteID, host_id: hostID, workspace_id: workspaceID, state: 'active', created_at: timestamp, updated_at: timestamp,
  };
  const candidate = {
    id: releaseID, site_id: siteID, workspace_id: workspaceID, workspace_revision: 4,
    manifest_sha256: 'a'.repeat(64), service_port: 3000, state: 'candidate', artifact_file_count: 0,
    artifact_bytes: 0, materialized_at: null, created_at: timestamp, updated_at: timestamp,
  };
  const release = {
    ...candidate, artifact_file_count: 3, artifact_bytes: 1536, materialized_at: timestamp,
  };

  try {
    await context.route('**/src/main.tsx*', async (route) => {
      const response = await route.fetch();
      const source = await response.text();
      const withoutMockWorker = source.replace('if (import.meta.env.DEV && import.meta.env.VITE_MOCK_API === "1")', 'if (false)');
      expect(withoutMockWorker).not.toBe(source);
      await route.fulfill({ response, body: withoutMockWorker });
    });
    await context.route('**/api/me', (route) => route.fulfill({
      status: 200, contentType: 'application/json', body: JSON.stringify({ id: userID, name: 'demo', role: 'user' }),
    }));
    await context.route('**/api/hosts', (route) => route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ items: [{
        id: hostID, name: '开发电脑', owner_id: userID, state: 'running',
        workspaces: [{ workspace: { id: workspaceID }, state: 'active', desired_revision: 4, applied_revision: 4 }],
      }] }),
    }));
    await context.route('**/api/workspaces', (route) => route.fulfill({
      status: 200, contentType: 'application/json', body: JSON.stringify({ items: [workspace] }),
    }));
    await context.route(/\/api\/sites\?limit=100$/, (route) => {
      if (route.request().method() !== 'GET') return route.fallback();
      return route.fulfill({
        status: 200, contentType: 'application/json', body: JSON.stringify({ items: siteCreated ? [site] : [], next_cursor: null }),
      });
    });
    await context.route('**/api/sites', async (route) => {
      if (route.request().method() !== 'POST') return route.fallback();
      const request = route.request();
      const body = request.postDataJSON();
      calls.push({ path: new URL(request.url()).pathname, method: request.method(), body, idempotencyKey: request.headers()['idempotency-key'] });
      expect(body).toEqual({ host_id: hostID, workspace_id: workspaceID });
      siteCreated = true;
      await route.fulfill({ status: 201, contentType: 'application/json', body: JSON.stringify(site) });
    });
    await context.route(new RegExp(`/api/sites/${siteID}/releases\\?limit=100$`), (route) => route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ items: candidateCreated ? [materialized ? release : candidate] : [], next_cursor: null }),
    }));
    await context.route(new RegExp(`/api/sites/${siteID}/releases$`), async (route) => {
      if (route.request().method() !== 'POST') return route.fallback();
      const request = route.request();
      const body = request.postDataJSON();
      calls.push({ path: new URL(request.url()).pathname, method: request.method(), body, idempotencyKey: request.headers()['idempotency-key'] });
      expect(body).toEqual({ workspace_revision: 4, service_port: 3000 });
      expect(body).not.toHaveProperty('manifest_sha256');
      candidateCreated = true;
      await route.fulfill({ status: 201, contentType: 'application/json', body: JSON.stringify(candidate) });
    });
    await context.route(new RegExp(`/api/sites/${siteID}/releases/${releaseID}/materialize$`), async (route) => {
      if (route.request().method() !== 'POST') return route.fallback();
      calls.push({ path: new URL(route.request().url()).pathname, method: route.request().method() });
      materialized = true;
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(release) });
    });

    const page = await context.newPage();
    await page.goto(`${portalUrl}/sites`);
    await expect(page.getByRole('heading', { name: '私有快照，不执行、不发布、不生成公开 URL' })).toBeVisible();
    await page.getByRole('button', { name: '创建站点' }).click();
    await expect(page.getByText(`站点 ${siteID} 已创建。`)).toBeVisible();
    await expect(page.getByRole('heading', { name: new RegExp(siteID) })).toBeVisible();
    await page.getByRole('button', { name: '创建候选快照' }).click();
    await expect(page.getByRole('cell').getByText('候选快照', { exact: true })).toBeVisible();
    await page.getByRole('button', { name: `保存私有快照 ${releaseID}` }).click();
    await expect(page.getByText('私有快照已保存')).toBeVisible();
    await expect(page.getByRole('link', { name: /访问|打开站点/ })).toHaveCount(0);
    expect(calls).toHaveLength(3);
    expect(calls.slice(0, 2).every((call) => call.idempotencyKey)).toBe(true);

    await page.reload();
    await page.getByRole('button', { name: '查看版本' }).click();
    await expect(page.getByText('v4', { exact: true })).toBeVisible();
    await expect(page.getByText(/1,536 B/)).toBeVisible();
    await expect(page.getByText('私有快照已保存')).toBeVisible();
    await expect(page.getByRole('link', { name: /访问|打开站点/ })).toHaveCount(0);
  } finally {
    await context.close();
  }
});
