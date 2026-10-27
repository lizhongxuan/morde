import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { validateDesktopRelease } from './validate-desktop-release.mjs';

function fixture() {
  const root = mkdtempSync(join(tmpdir(), 'morde-release-'));
  const fileRoot = join(root, 'public', 'downloads', 'files');
  mkdirSync(fileRoot, { recursive: true });
  const filename = 'morde-desktop-0.1.0-macos-arm64.dmg';
  const bytes = Buffer.from('test package');
  writeFileSync(join(fileRoot, filename), bytes);
  const manifestPath = join(root, 'public', 'downloads', 'manifest.json');
  mkdirSync(join(root, 'public', 'downloads'), { recursive: true });
  writeFileSync(manifestPath, JSON.stringify({
    schema_version: 3,
    version: '0.1.0',
    portal_url: 'https://morde.example/',
    artifacts: [{
      platform: 'macos', architecture: 'arm64', filename,
      download_url: `/downloads/files/${filename}`, bytes: bytes.length,
      sha256: createHash('sha256').update(bytes).digest('hex'),
      acceptance: {
        status: 'verified', verified_at: '2026-10-26T00:00:00.000Z', evidence_ref: 'T22.10/macos-arm64/2026-10-26',
        signature_status: 'signed_and_notarized',
        capabilities: ['install', 'launch_idle_network', 'user_web_workbench', 'workspace_sync_recovery', 'update', 'uninstall'],
      },
    }],
  }));
  return { root, fileRoot, filename, manifestPath, publicRoot: join(root, 'public') };
}

test('validates package metadata and the actual file hash', (t) => {
  const value = fixture();
  t.after(() => rmSync(value.root, { recursive: true, force: true }));
  assert.match(validateDesktopRelease(value.manifestPath, value.publicRoot), /已校验 1 个/);
});

test('rejects package bytes that differ from the manifest', (t) => {
  const value = fixture();
  t.after(() => rmSync(value.root, { recursive: true, force: true }));
  writeFileSync(join(value.fileRoot, value.filename), 'changed data');
  assert.throws(() => validateDesktopRelease(value.manifestPath, value.publicRoot), /字节数不匹配|SHA-256 不匹配/);
});

test('rejects packages that have not passed platform acceptance', (t) => {
  const value = fixture();
  t.after(() => rmSync(value.root, { recursive: true, force: true }));
  const manifest = JSON.parse(readFileSync(value.manifestPath, 'utf8'));
  manifest.artifacts[0].acceptance.status = 'pending';
  writeFileSync(value.manifestPath, JSON.stringify(manifest));
  assert.throws(() => validateDesktopRelease(value.manifestPath, value.publicRoot), /未验收/);
});

test('rejects the retired task and approval acceptance contract', (t) => {
  const value = fixture();
  t.after(() => rmSync(value.root, { recursive: true, force: true }));
  const manifest = JSON.parse(readFileSync(value.manifestPath, 'utf8'));
  manifest.artifacts[0].acceptance.capabilities[2] = 'task_approval_host';
  writeFileSync(value.manifestPath, JSON.stringify(manifest));
  assert.throws(() => validateDesktopRelease(value.manifestPath, value.publicRoot), /桌面发行条目无效/);
});
