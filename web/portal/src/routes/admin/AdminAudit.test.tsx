import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../../app/api';
import { getDesktopDownloads, type DesktopDownloads } from '../../app/desktopDownloads';
import { AdminAudit } from './AdminAudit';

vi.mock('../../app/desktopDownloads', () => ({
  desktopCapabilityNames: { install: '原生安装', launch_idle_network: '启动与闲置网络', user_web_workbench: 'PC 客户端与云端电脑', workspace_sync_recovery: 'Workspace 同步与恢复', update: '更新', uninstall: '卸载' },
  getDesktopDownloads: vi.fn(),
}));

const downloads: DesktopDownloads = { version: '0.1.0', artifacts: [{
  platform: 'macos', architecture: 'arm64', filename: 'morde-pc-0.1.0-macos-arm64.dmg',
  download_url: '/downloads/files/morde-pc-0.1.0-macos-arm64.dmg', bytes: 1, sha256: 'a'.repeat(64),
  acceptance: { status: 'verified', verified_at: '2026-11-15T00:00:00.000Z', evidence_ref: 'local-acceptance-001',
    signature_status: 'signed_and_notarized', capabilities: ['install', 'launch_idle_network', 'user_web_workbench', 'workspace_sync_recovery', 'update', 'uninstall'] },
}] };

describe('admin desktop release downloads', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.clearAllMocks(); });

  it('exposes a local link for a verified package without starting a download', async () => {
    vi.spyOn(portalApi, 'audit').mockResolvedValue({ items: [] });
    vi.mocked(getDesktopDownloads).mockResolvedValue(downloads);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><AdminAudit /></QueryClientProvider>);

    const link = await screen.findByRole('link', { name: '下载 morde-pc-0.1.0-macos-arm64.dmg' });
    expect(link).toHaveAttribute('href', '/downloads/files/morde-pc-0.1.0-macos-arm64.dmg');
    expect(link).toHaveAttribute('download', 'morde-pc-0.1.0-macos-arm64.dmg');
  });
});
