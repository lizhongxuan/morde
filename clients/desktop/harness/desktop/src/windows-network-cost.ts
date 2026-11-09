import { execFile } from "node:child_process";
import { join } from "node:path";
import type { NetworkCost } from "./windows-updates";

// Windows' internet profile is the source of truth, never the connection's Wi-Fi name.
// https://learn.microsoft.com/en-us/uwp/api/windows.networking.connectivity.connectionprofile
const SCRIPT = `
$ErrorActionPreference = 'Stop'
try {
  $profile = [Windows.Networking.Connectivity.NetworkInformation,Windows,ContentType=WindowsRuntime]::GetInternetConnectionProfile()
  if (!$profile -or $profile.GetNetworkConnectivityLevel().ToString() -ne 'InternetAccess') { 'unknown'; exit }
  $cost = $profile.GetConnectionCost()
  if ($cost.NetworkCostType.ToString() -eq 'Unknown') { 'unknown' }
  elseif ($cost.NetworkCostType.ToString() -eq 'Unrestricted' -and !$cost.Roaming -and !$cost.OverDataLimit -and !$cost.ApproachingDataLimit -and !$cost.BackgroundDataUsageRestricted) { 'unmetered' }
  else { 'metered' }
} catch { 'unknown' }
`;

/** Read only the cost classification; unavailable Windows APIs fail closed. */
export function windowsNetworkCost(): Promise<NetworkCost> {
  if (process.platform !== "win32") return Promise.resolve("unknown");
  return new Promise((resolve) => {
    const powershell = join(process.env.SystemRoot ?? "C:\\Windows", "System32", "WindowsPowerShell", "v1.0", "powershell.exe");
    execFile(powershell, ["-NoLogo", "-NoProfile", "-NonInteractive", "-Command", SCRIPT],
      { windowsHide: true, timeout: 10_000, maxBuffer: 1024 }, (error, stdout) => {
        const cost = stdout.trim();
        resolve(!error && (cost === "unmetered" || cost === "metered") ? cost : "unknown");
      });
  });
}
