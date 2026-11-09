import { contextBridge, ipcRenderer } from "electron";

const call = (channel: string, ...args: unknown[]) => ipcRenderer.invoke(channel, ...args);
const bridge = {
  state: () => call("morde:state"),
  login: (origin: string, name: string, password: string) => call("morde:login", origin, name, password),
  logout: () => call("morde:logout"),
  showWeb: () => call("morde:show-web", "assistant"),
  showAccount: () => call("morde:show-web", "account"),
  showSync: () => call("morde:show-sync"),
  publicPage: (path: "register" | "download", origin?: string) => call("morde:public-page", path, origin),
  workspaces: () => call("morde:workspaces"),
  chooseDirectory: (id: string) => call("morde:choose-directory", id),
  setWorkspaceSyncEnabled: (id: string, enabled: boolean) => call("morde:workspace-sync-toggle", id, enabled),
  workspaceSyncStatus: (id: string) => call("morde:workspace-sync-status", id),
  startLiveSync: (workspaceId: string, scope: string[], rescanSeconds: number, confirmRecovery: boolean) => call("morde:live-sync-start", workspaceId, scope, rescanSeconds, confirmRecovery),
  stopLiveSync: (workspaceId: string) => call("morde:live-sync-stop", workspaceId),
  sync: (workspaceId: string, mode: "upload" | "download" | "bidirectional" | "live_sync" | "handoff_checkpoint", scope: string[], confirmedQuiesced: boolean, handoffTarget?: "desktop" | "cloud_workspace") => handoffTarget === undefined ? call("morde:sync", workspaceId, mode, scope, confirmedQuiesced) : call("morde:sync", workspaceId, mode, scope, confirmedQuiesced, handoffTarget),
  stopSync: () => call("morde:sync-stop"),
  discardSync: () => call("morde:sync-discard"),
  preserved: (id: string) => call("morde:preserved-list", id),
  restorePreserved: (id: string, preservedId: string) => call("morde:preserved-restore", id, preservedId),
  deletePreserved: (id: string, preservedId: string) => call("morde:preserved-delete", id, preservedId),
  onState: (listener: (value: unknown) => void) => { const cb = (_event: Electron.IpcRendererEvent, value: unknown) => listener(value); ipcRenderer.on("morde:state", cb); return () => ipcRenderer.off("morde:state", cb); },
  onLiveSync: (listener: (value: unknown) => void) => { const cb = (_event: Electron.IpcRendererEvent, value: unknown) => listener(value); ipcRenderer.on("morde:live-sync-state", cb); return () => ipcRenderer.off("morde:live-sync-state", cb); },
  onSync: (listener: (value: unknown) => void) => { const cb = (_event: Electron.IpcRendererEvent, value: unknown) => listener(value); ipcRenderer.on("morde:sync-state", cb); return () => ipcRenderer.off("morde:sync-state", cb); },
};

contextBridge.exposeInMainWorld("morde", bridge);
