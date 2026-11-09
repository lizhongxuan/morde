import { test } from 'node:test';
import assert from 'node:assert/strict';
import { resolveSyncTarget } from '../scripts/morde-target.mjs';

test('sync build targets use Go architecture names and platform executables', () => {
  assert.deepEqual(resolveSyncTarget('windows', 'x64'), { goos: 'windows', goarch: 'amd64', binaryName: 'morde-sync.exe' });
  assert.deepEqual(resolveSyncTarget('linux', 'x64'), { goos: 'linux', goarch: 'amd64', binaryName: 'morde-sync' });
  assert.deepEqual(resolveSyncTarget('macos', 'x64'), { goos: 'darwin', goarch: 'amd64', binaryName: 'morde-sync' });
  assert.deepEqual(resolveSyncTarget('macos', 'arm64'), { goos: 'darwin', goarch: 'arm64', binaryName: 'morde-sync' });
  assert.throws(() => resolveSyncTarget('windows', 'arm64el'), /Unsupported morde-sync target/);
  assert.throws(() => resolveSyncTarget('__proto__', 'x64'), /Unsupported morde-sync target/);
});
