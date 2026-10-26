import { expect, test, type Browser } from '@playwright/test';

const portalUrl = process.env.CH_PORTAL_E2E_URL ?? 'http://127.0.0.1:5183';
const manifest = {
  schema_version: 2,
  version: '0.1.0',
  portal_url: portalUrl.endsWith('/') ? portalUrl : `${portalUrl}/`,
  artifacts: [
    {
      platform: 'macos', architecture: 'arm64', filename: 'morde-desktop-0.1.0-macos-arm64.dmg',
      download_url: '/downloads/files/morde-desktop-0.1.0-macos-arm64.dmg', bytes: 42, sha256: 'a'.repeat(64),
      acceptance: {
        status: 'verified', verified_at: '2026-10-26T00:00:00.000Z', evidence_ref: 'T22.10/macos-arm64/2026-10-26',
        signature_status: 'signed_and_notarized',
        capabilities: ['install', 'launch_idle_network', 'task_approval_host', 'workspace_sync_recovery', 'update', 'uninstall'],
      },
    },
    {
      platform: 'windows', architecture: 'x64', filename: 'morde-desktop-0.1.0-windows-x64.exe',
      download_url: '/downloads/files/morde-desktop-0.1.0-windows-x64.exe', bytes: 42, sha256: 'b'.repeat(64),
      acceptance: { status: 'pending' },
    },
  ],
};

async function openDownloadPage(browser: Browser) {
  const context = await browser.newContext({ serviceWorkers: 'block' });
  await context.route('**/src/main.tsx*', async (route) => {
    const response = await route.fetch();
    const source = await response.text();
    const withoutMockWorker = source.replace('if (import.meta.env.DEV && import.meta.env.VITE_MOCK_API === "1")', 'if (false)');
    expect(withoutMockWorker).not.toBe(source);
    await route.fulfill({ response, body: withoutMockWorker });
  });
  await context.route('**/api/me', (route) => route.fulfill({ status: 401, contentType: 'application/json', body: JSON.stringify({ error: { code: 'unauthenticated', message: '请先登录', details: {} } }) }));
  return { context, page: await context.newPage() };
}

test('[mock] explains that desktop packages have not been published yet', async ({ browser }) => {
  const { context, page } = await openDownloadPage(browser);
  await context.route('**/downloads/manifest.json', (route) => route.fulfill({ status: 404 }));
  try {
    await page.goto(`${portalUrl}/download`);
    await expect(page.getByRole('heading', { name: '下载 Morde Desktop' })).toBeVisible();
    await expect(page.getByRole('heading', { name: '当前还没有可下载版本' })).toBeVisible();
    await expect(page.getByRole('link', { name: /^下载 / })).toHaveCount(0);
    await page.goto(`${portalUrl}/login`);
    await page.getByRole('link', { name: '下载 Morde Desktop' }).click();
    await expect(page).toHaveURL(`${portalUrl}/download`);
  } finally {
    await context.close();
  }
});

test('[mock] downloads the package selected in the published manifest', async ({ browser }) => {
  const { context, page } = await openDownloadPage(browser);
  await context.route('**/downloads/manifest.json', (route) => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(manifest) }));
  await context.route('**/downloads/files/**', (route) => route.fulfill({ status: 200, contentType: 'application/octet-stream', headers: { 'content-disposition': 'attachment; filename="morde-desktop-0.1.0-macos-arm64.dmg"' }, body: 'local fixture' }));
  try {
    await page.goto(`${portalUrl}/download`);
    await expect(page.getByText('版本 0.1.0')).toBeVisible();
    await expect(page.getByRole('heading', { name: 'macOS · Apple 芯片' })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Windows · 64 位' })).toHaveCount(0);
    const downloadPromise = page.waitForEvent('download');
    await page.getByRole('link', { name: '下载 macOS · Apple 芯片' }).click();
    const download = await downloadPromise;
    expect(download.suggestedFilename()).toBe('morde-desktop-0.1.0-macos-arm64.dmg');
  } finally {
    await context.close();
  }
});
