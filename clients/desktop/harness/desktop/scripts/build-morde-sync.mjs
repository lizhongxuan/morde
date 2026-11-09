import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, unlinkSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { resolveSyncTarget } from './morde-target.mjs';

const root = resolve(import.meta.dirname, '../../../../../runtime/morde-sync');
if (!existsSync(join(root, 'go.mod'))) throw new Error('The Morde sync source is required beside nanoMuse');
const [targetPlatform = process.platform, targetArch = process.arch, ...unexpected] = process.argv.slice(2);
if (unexpected.length) throw new Error('Usage: build-morde-sync.mjs [darwin|windows|linux] [x64|arm64]');
const target = resolveSyncTarget(targetPlatform, targetArch);
const out = resolve(import.meta.dirname, '../bin');
mkdirSync(out, { recursive: true });
const binary = join(out, target.binaryName);
const otherBinary = join(out, target.binaryName === 'morde-sync.exe' ? 'morde-sync' : 'morde-sync.exe');
if (existsSync(otherBinary)) unlinkSync(otherBinary);
const result = spawnSync('go', ['build', '-trimpath', '-o', binary, './cmd/morde-sync'], {
  cwd: root,
  stdio: 'inherit',
  env: { ...process.env, GOOS: target.goos, GOARCH: target.goarch, CGO_ENABLED: '0' },
});
if (result.status !== 0) process.exit(result.status || 1);
const digest = createHash('sha256').update(readFileSync(binary)).digest('hex');
writeFileSync(join(out, 'morde-sync.sha256'), `${digest}  ${target.binaryName}\n`, { mode: 0o644 });
