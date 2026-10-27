import { describe, expect, it } from 'vitest';
import { getDesktopDownloads, parseDesktopDownloads } from './desktopDownloads';

const portalURL = 'https://morde.example/';
const artifact = {
  platform: 'macos', architecture: 'arm64', filename: 'morde-desktop-0.1.0-macos-arm64.dmg',
  download_url: '/downloads/files/morde-desktop-0.1.0-macos-arm64.dmg', bytes: 42, sha256: 'a'.repeat(64),
  acceptance: {
    status: 'verified', verified_at: '2026-10-26T00:00:00.000Z', evidence_ref: 'T22.10/macos-arm64/2026-10-26',
    signature_status: 'signed_and_notarized',
    capabilities: ['install', 'launch_idle_network', 'user_web_workbench', 'workspace_sync_recovery', 'update', 'uninstall'],
  },
};
const manifest = (artifacts: unknown[]) => ({ schema_version: 3, version: '0.1.0', portal_url: portalURL, artifacts });

describe('desktop downloads manifest', () => {
  it('accepts only an installer published for this Portal origin', () => {
    expect(parseDesktopDownloads(manifest([artifact]), 'https://morde.example'))
      .toEqual({ version: '0.1.0', artifacts: [artifact] });
    expect(() => parseDesktopDownloads({ ...manifest([artifact]), portal_url: 'https://other.example/' }, 'https://morde.example'))
      .toThrow('与当前页面不一致');
    expect(() => parseDesktopDownloads({ ...manifest([artifact]), schema_version: 2 }, 'https://morde.example'))
      .toThrow('格式无效');
  });

  it('rejects unknown targets, duplicate packages, and non-download URLs', () => {
    expect(() => parseDesktopDownloads(manifest([{ ...artifact, platform: 'toString' }]), 'https://morde.example')).toThrow('无效条目');
    expect(() => parseDesktopDownloads(manifest([artifact, artifact]), 'https://morde.example')).toThrow('重复平台');
    expect(() => parseDesktopDownloads(manifest([{ ...artifact, download_url: 'https://attacker.example/setup' }]), 'https://morde.example')).toThrow('无效条目');
  });

  it('shows only verified artifacts and rejects incomplete acceptance evidence', () => {
    const pending = { ...artifact, platform: 'windows', filename: 'morde-desktop-0.1.0-windows-x64.exe', download_url: '/downloads/files/morde-desktop-0.1.0-windows-x64.exe', acceptance: { status: 'pending' } };
    expect(parseDesktopDownloads(manifest([artifact, pending]), 'https://morde.example').artifacts).toEqual([artifact]);
    expect(parseDesktopDownloads(manifest([pending]), 'https://morde.example')).toEqual({ version: null, artifacts: [] });
    expect(() => parseDesktopDownloads(manifest([{ ...artifact, acceptance: { ...artifact.acceptance, capabilities: ['install'] } }]), 'https://morde.example')).toThrow('无效条目');
    expect(() => parseDesktopDownloads(manifest([{ ...artifact, acceptance: { ...artifact.acceptance, capabilities: ['install', 'launch_idle_network', 'task_approval_host', 'workspace_sync_recovery', 'update', 'uninstall'] } }]), 'https://morde.example')).toThrow('无效条目');
    expect(() => parseDesktopDownloads(manifest([{ ...artifact, acceptance: { ...artifact.acceptance, verified_at: 'yesterday' } }]), 'https://morde.example')).toThrow('无效条目');
  });

  it('shows no packages until a manifest has been published', async () => {
    const fetcher = async () => new Response(null, { status: 404 });
    await expect(getDesktopDownloads(fetcher, 'https://morde.example')).resolves.toEqual({ version: null, artifacts: [] });
  });

  it('treats the local SPA fallback as an unpublished manifest', async () => {
    const fetcher = async () => new Response('<!doctype html>', { status: 200, headers: { 'content-type': 'text/html' } });
    await expect(getDesktopDownloads(fetcher, 'https://morde.example')).resolves.toEqual({ version: null, artifacts: [] });
  });
});
