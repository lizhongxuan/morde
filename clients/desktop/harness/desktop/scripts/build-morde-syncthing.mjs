import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, unlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { resolveSyncTarget } from './morde-target.mjs';

const sourceCommit = 'ed38528d7fba3dd6a42c9c9fc6780af432dc15d2';
const f02PatchSHA256 = 'c6bfa517918183efac18b96eff0ca4ad6f844f42abe5601ab1c757ff06586539';
const safeInitPatchSHA256 = '972ffce777fb97c610647caac1dc4d834e4710fecea8e93f51d3fdb1291c89c6';
const sourceURL = 'https://github.com/syncthing/syncthing.git';
const expectedGoVersion = 'go1.27.1';
const scriptDirectory = import.meta.dirname;
const desktopDirectory = resolve(scriptDirectory, '..');
const patchFiles = [
  { path: resolve(desktopDirectory, 'patches/syncthing-f02.patch'), sha256: f02PatchSHA256 },
  { path: resolve(desktopDirectory, 'patches/syncthing-morde-safe-init.patch'), sha256: safeInitPatchSHA256 },
];

function run(command, args, options = {}) {
  const result = spawnSync(command, args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'inherit'], ...options });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`${command} ${args[0] ?? ''} failed (${result.status ?? 'signal'})`);
  return result.stdout;
}

function sha256(path) {
  return createHash('sha256').update(readFileSync(path)).digest('hex');
}

function checkGoVersion() {
  const version = run('go', ['version']).trim().split(/\s+/)[2];
  if (version !== expectedGoVersion) throw new Error(`Syncthing F02 builds require Go ${expectedGoVersion}; found ${version || 'unknown'}`);
}

function checkoutPinnedSource(directory) {
  const source = process.env.MORDE_SYNCTHING_SOURCE?.trim();
  const repository = source ? resolve(source) : sourceURL;
  if (source && !existsSync(join(repository, '.git'))) throw new Error('MORDE_SYNCTHING_SOURCE must be a Git checkout');
  run('git', ['init', '--quiet', directory]);
  run('git', ['-C', directory, 'remote', 'add', 'origin', repository]);
  run('git', ['-C', directory, 'fetch', '--quiet', '--depth=1', 'origin', sourceCommit]);
  run('git', ['-C', directory, 'checkout', '--quiet', '--detach', 'FETCH_HEAD']);
  const actual = run('git', ['-C', directory, 'rev-parse', 'HEAD']).trim();
  if (actual !== sourceCommit) throw new Error(`Syncthing source commit mismatch: ${actual}`);
}

function applyPinnedPatches(directory) {
  for (const patch of patchFiles) {
    if (sha256(patch.path) !== patch.sha256) throw new Error(`Syncthing patch digest mismatch: ${patch.name ?? patch.path}`);
    run('git', ['-C', directory, 'apply', '--check', patch.path]);
    run('git', ['-C', directory, 'apply', '--whitespace=error', patch.path]);
  }
  run('git', ['-C', directory, 'diff', '--check']);
}

const [platform = process.platform, arch = process.arch, ...unexpected] = process.argv.slice(2);
if (unexpected.length) throw new Error('Usage: build-morde-syncthing.mjs [darwin|windows|linux] [x64|arm64]');
const target = resolveSyncTarget(platform, arch);
const binaryName = target.goos === 'windows' ? 'morde-syncthing.exe' : 'morde-syncthing';
checkGoVersion();

const outputDirectory = resolve(desktopDirectory, 'bin');
mkdirSync(outputDirectory, { recursive: true });
const otherBinary = join(outputDirectory, binaryName === 'morde-syncthing.exe' ? 'morde-syncthing' : 'morde-syncthing.exe');
if (existsSync(otherBinary)) unlinkSync(otherBinary);
const buildDirectory = mkdtempSync(join(tmpdir(), 'morde-syncthing-f02-'));
const sourceDirectory = join(buildDirectory, 'source');
try {
  checkoutPinnedSource(sourceDirectory);
  applyPinnedPatches(sourceDirectory);
  const binary = join(outputDirectory, binaryName);
  run('go', ['build', '-tags', 'noassets', '-trimpath', '-buildvcs=false', '-o', binary, './cmd/syncthing'], {
    cwd: sourceDirectory,
    env: { ...process.env, GOOS: target.goos, GOARCH: target.goarch, CGO_ENABLED: '0', GOTOOLCHAIN: 'local' },
  });
  writeFileSync(join(outputDirectory, 'morde-syncthing.sha256'), `${sha256(binary)}  ${binaryName}\n`, { mode: 0o644 });
  process.stdout.write(`Built ${binaryName} from Syncthing ${sourceCommit} with patches ${patchFiles.map((patch) => patch.sha256).join(', ')}\n`);
} finally {
  rmSync(buildDirectory, { recursive: true, force: true });
}
