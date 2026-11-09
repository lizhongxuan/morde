import assert from 'node:assert/strict';
import { test } from 'node:test';
import { chmodSync, existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import afterPack from '../scripts/after-pack.cjs';

test('Linux package launcher disables Chromium background networking and preserves arguments', async () => {
  const output = mkdtempSync(join(tmpdir(), 'morde-after-pack-'));
  const executable = join(output, 'morde-client');
  const received = join(output, 'arguments.txt');
  writeFileSync(executable, '#!/bin/sh\nprintf "%s\\n" "$@" > "$MORDE_ARGS_OUT"\n');
  chmodSync(executable, 0o755);

  try {
    await afterPack({ electronPlatformName: 'linux', appOutDir: output, packager: { appInfo: { productFilename: 'morde-client' } } });
    assert.equal(existsSync(join(output, 'morde-client-bin')), true);
    assert.match(readFileSync(executable, 'utf8'), /--disable-background-networking --disable-component-update --host-resolver-rules=/);
    const result = spawnSync(executable, ['--user-data-dir=/tmp/test', 'argument with spaces'], {
      encoding: 'utf8',
      env: { ...process.env, MORDE_ARGS_OUT: received },
    });
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(readFileSync(received, 'utf8').trim().split('\n'), [
      '--disable-background-networking',
      '--disable-component-update',
      '--host-resolver-rules=MAP *.gvt1.com ~NOTFOUND,MAP *.gvt1-cn.com ~NOTFOUND',
      '--user-data-dir=/tmp/test',
      'argument with spaces',
    ]);
  } finally {
    rmSync(output, { recursive: true, force: true });
  }
});

test('desktop packages include the morde-sync source license', () => {
  const builder = readFileSync(new URL('../electron-builder.yml', import.meta.url), 'utf8');
  const notice = readFileSync(new URL('../resources/THIRD_PARTY_NOTICES.morde-sync.md', import.meta.url), 'utf8');
  const license = readFileSync(new URL('../../../../../runtime/morde-sync/LICENSE', import.meta.url), 'utf8');

  assert.match(builder, /from: \.\.\/\.\.\/\.\.\/\.\.\/runtime\/morde-sync\/LICENSE\s+to: legal\/LICENSE\.morde-sync/);
  assert.match(notice, /licensed under GNU GPL version 3 or later/);
  assert.match(license, /GNU GENERAL PUBLIC LICENSE\s+Version 3/);
});
