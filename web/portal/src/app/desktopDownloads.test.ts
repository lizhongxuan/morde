import { describe, expect, it } from 'vitest';
import { getDesktopDownloads, parseDesktopDownloads } from './desktopDownloads';

const portalURL = 'https://morde.example/';
const artifact = {
  platform: 'macos', architecture: 'arm64', filename: 'morde-desktop-0.1.0-macos-arm64.dmg',
  download_url: '/downloads/files/morde-desktop-0.1.0-macos-arm64.dmg', bytes: 42, sha256: 'a'.repeat(64),
};

describe('desktop downloads manifest', () => {
  it('accepts only an installer published for this Portal origin', () => {
    expect(parseDesktopDownloads({ schema_version: 1, version: '0.1.0', portal_url: portalURL, artifacts: [artifact] }, 'https://morde.example'))
      .toEqual({ version: '0.1.0', artifacts: [artifact] });
    expect(() => parseDesktopDownloads({ schema_version: 1, version: '0.1.0', portal_url: 'https://other.example/', artifacts: [artifact] }, 'https://morde.example'))
      .toThrow('与当前页面不一致');
  });

  it('rejects unknown targets, duplicate packages, and non-download URLs', () => {
    const manifest = (artifacts: unknown[]) => ({ schema_version: 1, version: '0.1.0', portal_url: portalURL, artifacts });
    expect(() => parseDesktopDownloads(manifest([{ ...artifact, platform: 'toString' }]), 'https://morde.example')).toThrow('无效条目');
    expect(() => parseDesktopDownloads(manifest([artifact, artifact]), 'https://morde.example')).toThrow('重复平台');
    expect(() => parseDesktopDownloads(manifest([{ ...artifact, download_url: 'https://attacker.example/setup' }]), 'https://morde.example')).toThrow('无效条目');
  });

  it('shows no packages until a manifest has been published', async () => {
    const fetcher = async () => new Response(null, { status: 404 });
    await expect(getDesktopDownloads(fetcher, 'https://morde.example')).resolves.toEqual({ version: null, artifacts: [] });
  });
});
