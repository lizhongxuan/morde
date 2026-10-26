export type DesktopDownload = {
  platform: 'macos' | 'windows' | 'linux';
  architecture: 'arm64' | 'x64';
  filename: string;
  download_url: string;
  bytes: number;
  sha256: string;
};

export type DesktopDownloads = { version: string | null; artifacts: DesktopDownload[] };

const emptyDownloads: DesktopDownloads = { version: null, artifacts: [] };
const versionPattern = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/;
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
  const { platform, architecture, filename, download_url: downloadURL, bytes, sha256 } = value;
  if (typeof platform !== 'string' || !Object.hasOwn(targets, platform) || typeof architecture !== 'string') return null;
  const extension = targets[platform]?.[architecture];
  if (!extension) return null;
  const expectedFilename = `morde-desktop-${version}-${platform}-${architecture}${extension}`;
  if (
    filename !== expectedFilename
    || downloadURL !== `/downloads/files/${expectedFilename}`
    || typeof bytes !== 'number' || !Number.isSafeInteger(bytes) || bytes < 1
    || typeof sha256 !== 'string' || !/^[a-f0-9]{64}$/.test(sha256)
  ) return null;
  return { platform: platform as DesktopDownload['platform'], architecture: architecture as DesktopDownload['architecture'], filename: expectedFilename, download_url: downloadURL, bytes, sha256 };
}

export function parseDesktopDownloads(value: unknown, currentOrigin: string): DesktopDownloads {
  if (!isRecord(value) || value.schema_version !== 1 || typeof value.version !== 'string' || !versionPattern.test(value.version) || typeof value.portal_url !== 'string' || !Array.isArray(value.artifacts)) {
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
  const artifacts = value.artifacts.map((item) => parseDownload(item, value.version as string));
  if (artifacts.some((item) => item === null)) throw new Error('安装包发布清单包含无效条目');
  const downloads = artifacts as DesktopDownload[];
  if (new Set(downloads.map(({ platform, architecture }) => `${platform}:${architecture}`)).size !== downloads.length) {
    throw new Error('安装包发布清单包含重复平台');
  }
  return { version: value.version, artifacts: downloads };
}

export async function getDesktopDownloads(fetcher: typeof fetch = fetch, currentOrigin = window.location.origin): Promise<DesktopDownloads> {
  const response = await fetcher('/downloads/manifest.json', { cache: 'no-cache' });
  if (response.status === 404) return emptyDownloads;
  if (!response.ok) throw new Error('暂时无法读取安装包列表，请重试');
  return parseDesktopDownloads(await response.json(), currentOrigin);
}
