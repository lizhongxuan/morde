import assert from 'node:assert/strict';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { test } from 'node:test';
import { copyMordeMobileWeb } from './copy-morde-mobile.mjs';

function fixture() {
  const root = mkdtempSync(join(tmpdir(), 'morde-mobile-build-'));
  const webDist = join(root, 'web-dist');
  const outputDir = join(root, 'portal-dist', 'app');
  const legalDir = join(root, 'legal');
  mkdirSync(join(webDist, 'assets'), { recursive: true });
  mkdirSync(legalDir);
  writeFileSync(join(webDist, 'index.html'), '<script src="/app/assets/main.js"></script>');
  writeFileSync(join(webDist, 'assets', 'main.js'), 'Morde app');
  for (const file of ['LICENSE', 'NOTICE', 'THIRD_PARTY_NOTICES.md']) writeFileSync(join(legalDir, file), file);
  return { root, webDist, outputDir, legalDir };
}

test('copies the /app build and legal notices while removing stale files', (t) => {
  const value = fixture();
  t.after(() => rmSync(value.root, { recursive: true, force: true }));
  mkdirSync(value.outputDir, { recursive: true });
  writeFileSync(join(value.outputDir, 'stale.js'), 'stale');

  copyMordeMobileWeb(value.webDist, value.outputDir, value.legalDir);

  assert.equal(readFileSync(join(value.outputDir, 'assets', 'main.js'), 'utf8'), 'Morde app');
  assert.equal(readFileSync(join(value.outputDir, 'legal', 'LICENSE'), 'utf8'), 'LICENSE');
  assert.equal(existsSync(join(value.outputDir, 'stale.js')), false);
});

test('rejects incomplete or misbased output before replacing the existing app', (t) => {
  const value = fixture();
  t.after(() => rmSync(value.root, { recursive: true, force: true }));
  mkdirSync(value.outputDir, { recursive: true });
  writeFileSync(join(value.outputDir, 'current.js'), 'current');
  writeFileSync(join(value.webDist, 'index.html'), '<script src="/assets/main.js"></script>');

  assert.throws(() => copyMordeMobileWeb(value.webDist, value.outputDir, value.legalDir), /base path/);
  assert.equal(readFileSync(join(value.outputDir, 'current.js'), 'utf8'), 'current');
});
