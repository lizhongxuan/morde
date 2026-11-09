const { spawnSync } = require('node:child_process');
const { renameSync, writeFileSync } = require('node:fs');
const { join } = require('node:path');

module.exports = async ({ electronPlatformName, appOutDir, packager }) => {
  if (electronPlatformName === 'linux') {
    const executable = join(appOutDir, packager.appInfo.productFilename);
    renameSync(executable, `${executable}-bin`);
    // shortcut: block the observed Chromium GVT endpoints; audit again on Electron upgrades.
    writeFileSync(executable, '#!/bin/sh\nHERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)\nexec "$HERE/morde-client-bin" --disable-background-networking --disable-component-update --host-resolver-rules="MAP *.gvt1.com ~NOTFOUND,MAP *.gvt1-cn.com ~NOTFOUND" "$@"\n', { mode: 0o755 });
    return;
  }
  if (electronPlatformName !== 'darwin') return;
  const plist = join(appOutDir, `${packager.appInfo.productFilename}.app`, 'Contents', 'Info.plist');
  for (const key of [
    'NSAppTransportSecurity',
    'NSAudioCaptureUsageDescription',
    'NSBluetoothAlwaysUsageDescription',
    'NSBluetoothPeripheralUsageDescription',
    'NSCameraUsageDescription',
    'NSMicrophoneUsageDescription',
  ]) {
    const result = spawnSync('plutil', ['-remove', key, plist], { encoding: 'utf8' });
    if (result.status !== 0) throw new Error(`Could not remove unused macOS permission ${key}: ${result.stderr}`);
  }
};
