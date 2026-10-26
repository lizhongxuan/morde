export type DesktopDownload = {
  platform: 'macos' | 'windows' | 'linux';
  architecture: 'arm64' | 'x64';
  filename: string;
  download_url: string;
  bytes: number;
  sha256: string;
  acceptance: {
    status: 'verified';
    verified_at: string;
    evidence_ref: string;
    signature_status: 'signed_and_notarized' | 'signed' | 'not_applicable';
    capabilities: DesktopCapability[];
  };
};

export type DesktopCapability = 'install' | 'launch_idle_network' | 'task_approval_host' | 'workspace_sync_recovery' | 'update' | 'uninstall';
export type DesktopDownloads = { version: string | null; artifacts: DesktopDownload[] };

const emptyDownloads: DesktopDownloads = { version: null, artifacts: [] };
const versionPattern = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/;
const capabilities: DesktopCapability[] = ['install', 'launch_idle_network', 'task_approval_host', 'workspace_sync_recovery', 'update', 'uninstall'];
export const desktopCapabilityNames: Record<DesktopCapability, string> = {
  install: '原生安装',
  launch_idle_network: '启动与闲置网络',
  task_approval_host: '任务、审批与 Host',
  workspace_sync_recovery: 'Workspace 同步与恢复',
  update: '更新',
  uninstall: '卸载',
};
const targets: Record<string, Record<string, string>> = {
  macos: { arm64: '.dmg', x64: '.dmg' },
  windows: { x64: '.exe' },
  linux: { x64: '.AppImage' },
};

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function parseDownload(value: unknown, version: string): DesktopDownload | null {
  if (!isRecord(value)) return null;
  const { platform, architecture, filename, download_url: downloadURL, bytes, sha256, acceptance } = value;
  if (!isRecord(acceptance) || !['verified', 'pending', 'failed'].includes(String(acceptance.status))) return null;
  if (acceptance.status !== 'verified') return null;
  if (typeof platform !== 'string' || !Object.hasOwn(targets, platform) || typeof architecture !== 'string') return null;
  const extension = targets[platform]?.[architecture];
  if (!extension) return null;
  const expectedFilename = `morde-desktop-${version}-${platform}-${architecture}${extension}`;
  const expectedSignature = platform === 'macos' ? 'signed_and_notarized' : platform === 'windows' ? 'signed' : 'not_applicable';
  const verifiedAt = acceptance.verified_at;
  const evidenceRef = acceptance.evidence_ref;
  const verifiedCapabilities = acceptance.capabilities;
  const validTimestamp = typeof verifiedAt === 'string' && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/.test(verifiedAt) && !Number.isNaN(Date.parse(verifiedAt)) && new Date(verifiedAt).toISOString() === verifiedAt;
  const validEvidenceRef = typeof evidenceRef === 'string' && /^[A-Za-z0-9][A-Za-z0-9._/-]{2,159}$/.test(evidenceRef) && !evidenceRef.includes('..');
  const validCapabilities = Array.isArray(verifiedCapabilities)
    && verifiedCapabilities.length === capabilities.length
    && new Set(verifiedCapabilities).size === capabilities.length
    && capabilities.every((capability) => verifiedCapabilities.includes(capability));
  if (
    filename !== expectedFilename
    || downloadURL !== `/downloads/files/${expectedFilename}`
    || typeof bytes !== 'number' || !Number.isSafeInteger(bytes) || bytes < 1
    || typeof sha256 !== 'string' || !/^[a-f0-9]{64}$/.test(sha256)
    || !validTimestamp || !validEvidenceRef || !validCapabilities
    || acceptance.signature_status !== expectedSignature
  ) return null;
  return {
    platform: platform as DesktopDownload['platform'], architecture: architecture as DesktopDownload['architecture'],
    filename: expectedFilename, download_url: downloadURL, bytes, sha256,
    acceptance: {
      status: 'verified', verified_at: verifiedAt, evidence_ref: evidenceRef,
      signature_status: expectedSignature, capabilities: [...capabilities],
    },
  };
}

export function parseDesktopDownloads(value: unknown, currentOrigin: string): DesktopDownloads {
  if (!isRecord(value) || value.schema_version !== 2 || typeof value.version !== 'string' || !versionPattern.test(value.version) || typeof value.portal_url !== 'string' || !Array.isArray(value.artifacts)) {
    throw new Error('安装包发布清单格式无效');
  }
  let portalURL: URL;
  try {
    portalURL = new URL(value.portal_url);
  } catch {
    throw new Error('安装包发布地址无效');
  }
  if (portalURL.pathname !== '/' || portalURL.search || portalURL.hash || portalURL.username || portalURL.password || portalURL.origin !== currentOrigin) {
    throw new Error('安装包配置的 Morde 地址与当前页面不一致，请管理员重新打包');
  }
  const downloads: DesktopDownload[] = [];
  for (const item of value.artifacts) {
    if (isRecord(item) && isRecord(item.acceptance) && ['pending', 'failed'].includes(String(item.acceptance.status))) continue;
    const artifact = parseDownload(item, value.version);
    if (!artifact) throw new Error('安装包发布清单包含无效条目');
    downloads.push(artifact);
  }
  if (new Set(downloads.map(({ platform, architecture }) => `${platform}:${architecture}`)).size !== downloads.length) {
    throw new Error('安装包发布清单包含重复平台');
  }
  return { version: downloads.length ? value.version : null, artifacts: downloads };
}

export async function getDesktopDownloads(fetcher: typeof fetch = fetch, currentOrigin = window.location.origin): Promise<DesktopDownloads> {
  const response = await fetcher('/downloads/manifest.json', { cache: 'no-cache' });
  if (response.status === 404) return emptyDownloads;
  if (!response.ok) throw new Error('暂时无法读取安装包列表，请重试');
  const contentType = response.headers.get('content-type')?.split(';', 1)[0]?.trim();
  if (contentType === 'text/html') return emptyDownloads;
  if (contentType !== 'application/json') throw new Error('安装包发布清单格式无效');
  return parseDesktopDownloads(await response.json(), currentOrigin);
}
