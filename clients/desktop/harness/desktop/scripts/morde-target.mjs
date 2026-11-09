const GOOS = { darwin: 'darwin', macos: 'darwin', win32: 'windows', windows: 'windows', linux: 'linux' };
const GOARCH = { x64: 'amd64', amd64: 'amd64', arm64: 'arm64' };

export function resolveSyncTarget(platform, arch) {
  const goos = Object.hasOwn(GOOS, platform) ? GOOS[platform] : undefined;
  const goarch = Object.hasOwn(GOARCH, arch) ? GOARCH[arch] : undefined;
  if (!goos || !goarch) throw new Error(`Unsupported morde-sync target: ${platform}/${arch}`);
  return { goos, goarch, binaryName: goos === 'windows' ? 'morde-sync.exe' : 'morde-sync' };
}
