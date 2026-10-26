import { createHash } from 'node:crypto';
import { readFileSync, statSync } from 'node:fs';
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

const capabilitySet = ['install', 'launch_idle_network', 'task_approval_host', 'workspace_sync_recovery', 'update', 'uninstall'];
const targets = { macos: { arm64: '.dmg', x64: '.dmg' }, windows: { x64: '.exe' }, linux: { x64: '.AppImage' } };
const versionPattern = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/;

function record(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function validAcceptance(acceptance, platform) {
  const signature = platform === 'macos' ? 'signed_and_notarized' : platform === 'windows' ? 'signed' : 'not_applicable';
  return record(acceptance)
    && acceptance.status === 'verified'
    && typeof acceptance.verified_at === 'string'
    && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/.test(acceptance.verified_at)
    && !Number.isNaN(Date.parse(acceptance.verified_at))
    && new Date(acceptance.verified_at).toISOString() === acceptance.verified_at
    && typeof acceptance.evidence_ref === 'string'
    && /^[A-Za-z0-9][A-Za-z0-9._/-]{2,159}$/.test(acceptance.evidence_ref)
    && !acceptance.evidence_ref.includes('..')
    && acceptance.signature_status === signature
    && Array.isArray(acceptance.capabilities)
    && acceptance.capabilities.length === capabilitySet.length
    && new Set(acceptance.capabilities).size === capabilitySet.length
    && capabilitySet.every((capability) => acceptance.capabilities.includes(capability));
}

export function validateDesktopRelease(manifestPath, publicRoot) {
  const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'));
  if (!record(manifest) || manifest.schema_version !== 2 || typeof manifest.version !== 'string' || !versionPattern.test(manifest.version) || !Array.isArray(manifest.artifacts) || manifest.artifacts.length === 0) {
    throw new Error('桌面发行清单无效或没有已验收安装包');
  }
  const seen = new Set();
  for (const artifact of manifest.artifacts) {
    if (!record(artifact) || artifact.acceptance?.status !== 'verified' || typeof artifact.platform !== 'string' || !Object.hasOwn(targets, artifact.platform) || typeof artifact.architecture !== 'string') {
      throw new Error('桌面发行清单含未验收或无效条目');
    }
    const target = targets[artifact.platform];
    const extension = Object.hasOwn(target, artifact.architecture) ? target[artifact.architecture] : undefined;
    const filename = `morde-desktop-${manifest.version}-${artifact.platform}-${artifact.architecture}${extension ?? ''}`;
    const key = `${artifact.platform}:${artifact.architecture}`;
    if (!extension || seen.has(key) || artifact.filename !== filename || artifact.download_url !== `/downloads/files/${filename}`
      || !Number.isSafeInteger(artifact.bytes) || artifact.bytes < 1 || typeof artifact.sha256 !== 'string' || !/^[a-f0-9]{64}$/.test(artifact.sha256)
      || !validAcceptance(artifact.acceptance, artifact.platform)) {
      throw new Error(`桌面发行条目无效：${key}`);
    }
    seen.add(key);
    const path = resolve(publicRoot, 'downloads', 'files', filename);
    const stat = statSync(path);
    if (!stat.isFile() || stat.size !== artifact.bytes) throw new Error(`安装包字节数不匹配：${filename}`);
    const sha256 = createHash('sha256').update(readFileSync(path)).digest('hex');
    if (sha256 !== artifact.sha256) throw new Error(`安装包 SHA-256 不匹配：${filename}`);
  }
  return `已校验 ${seen.size} 个 ${manifest.version} 桌面安装包的验收信息、字节数和 SHA-256`;
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  const manifestPath = resolve(process.argv[2] ?? 'public/downloads/manifest.json');
  const publicRoot = resolve(process.argv[3] ?? 'public');
  try {
    process.stdout.write(`${validateDesktopRelease(manifestPath, publicRoot)}\n`);
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : '桌面发行校验失败'}\n`);
    process.exitCode = 1;
  }
}
