import { contextBridge, ipcRenderer } from "electron";

const call = (channel: string, ...args: unknown[]) => ipcRenderer.invoke(channel, ...args);
const listen = (channel: string, listener: (value: unknown) => void) => {
  if (typeof listener !== "function") return () => undefined;
  const callback = (_event: Electron.IpcRendererEvent, value: unknown) => listener(value);
  ipcRenderer.on(channel, callback);
  return () => ipcRenderer.off(channel, callback);
};

const mode = process.argv.find((argument) => argument.startsWith("--morde-preload-mode="))?.slice("--morde-preload-mode=".length);

if (mode === "portal") {
  contextBridge.exposeInMainWorld("morde", {
    projectUploadTargets: () => call("morde:portal-project-upload-targets"),
    selectProjectDirectory: (hostId: string, workspaceId: string) => call("morde:portal-project-select-directory", hostId, workspaceId),
    uploadProject: (selectionId: string, projectName: string) => call("morde:portal-project-upload", selectionId, projectName),
    state: () => call("morde:portal-state"),
    onState: (listener: (value: unknown) => void) => listen("morde:portal-state", listener),
    onSync: (listener: (value: unknown) => void) => listen("morde:portal-sync-state", listener),
  });
} else if (mode === "host-workbench") {
  contextBridge.exposeInMainWorld("morde", {
    returnToPortalHosts: () => call("morde:return-to-portal-hosts"),
  });
} else {
const bridge = {
  state: () => call("morde:state"),
  login: (origin: string, name: string, password: string) => call("morde:login", origin, name, password),
  logout: () => call("morde:logout"),
  publicPage: (path: "register" | "download", origin?: string) => call("morde:public-page", path, origin),
  workspaces: () => call("morde:workspaces"),
  projectUploadTargets: () => call("morde:project-upload-targets"),
  selectProjectDirectory: (hostId: string, workspaceId: string) => call("morde:project-select-directory", hostId, workspaceId),
  uploadProject: (selectionId: string, projectName: string) => call("morde:project-upload", selectionId, projectName),
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
  onState: (listener: (value: unknown) => void) => listen("morde:state", listener),
  onLiveSync: (listener: (value: unknown) => void) => listen("morde:live-sync-state", listener),
  onSync: (listener: (value: unknown) => void) => listen("morde:sync-state", listener),
};

contextBridge.exposeInMainWorld("morde", bridge);
}
