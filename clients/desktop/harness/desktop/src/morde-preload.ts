import { contextBridge, ipcRenderer } from "electron";

const call = (channel: string, ...args: unknown[]) => ipcRenderer.invoke(channel, ...args);
const listen = (channel: string, listener: (value: unknown) => void) => {
  if (typeof listener !== "function") return () => undefined;
  const callback = (_event: Electron.IpcRendererEvent, value: unknown) => listener(value);
  ipcRenderer.on(channel, callback);
  return () => ipcRenderer.off(channel, callback);
};

const mode = process.argv.find((argument) => argument.startsWith("--morde-preload-mode="))?.slice("--morde-preload-mode=".length);

if (mode === "codex") {
  contextBridge.exposeInMainWorld("morde", {
    state: () => call("morde:codex-state"),
    addProfile: () => call("morde:codex-add-profile"),
    selectProfile: (profileKey: string) => call("morde:codex-select-profile", profileKey),
    authorizeProfile: (profileKey: string, scopes: string[]) => call("morde:codex-authorize-profile", profileKey, scopes),
    revokeProfile: (profileKey: string) => call("morde:codex-revoke-profile", profileKey),
    listThreads: (cursor: string | null) => call("morde:codex-list-threads", cursor),
    authorizeThread: (threadID: string, scopes: string[]) => call("morde:codex-authorize-thread", threadID, scopes),
    updateScopes: (sessionID: string, scopes: string[]) => call("morde:codex-update-scopes", sessionID, scopes),
    revoke: (sessionID: string) => call("morde:codex-revoke", sessionID),
    onState: (listener: (value: unknown) => void) => listen("morde:codex-state", listener),
  });
} else if (mode === "portal") {
  contextBridge.exposeInMainWorld("morde", {
    projectUploadTargets: () => call("morde:portal-project-upload-targets"),
    selectProjectDirectory: (hostId: string, workspaceId: string) => call("morde:portal-project-select-directory", hostId, workspaceId),
    uploadProject: (selectionId: string, projectName: string) => call("morde:portal-project-upload", selectionId, projectName),
    stopProjectSync: (selectionId: string) => call("morde:portal-project-stop", selectionId),
    legacySyncState: () => call("morde:portal-legacy-state"),
    legacyStopSync: (workspaceId: string) => call("morde:portal-legacy-stop", workspaceId),
    legacyResumeSync: (workspaceId: string) => call("morde:portal-legacy-resume", workspaceId),
    legacyDiscardSync: (workspaceId: string) => call("morde:portal-legacy-discard", workspaceId),
    legacyPauseSync: (workspaceId: string) => call("morde:portal-legacy-pause", workspaceId),
    legacyStopLiveSync: (workspaceId: string) => call("morde:portal-legacy-live-stop", workspaceId),
    preserved: (workspaceId: string) => call("morde:portal-preserved-list", workspaceId),
    restorePreserved: (workspaceId: string, id: string) => call("morde:portal-preserved-restore", workspaceId, id),
    deletePreserved: (workspaceId: string, id: string) => call("morde:portal-preserved-delete", workspaceId, id),
    codexState: () => call("morde:codex-state"),
    codexAddProfile: () => call("morde:codex-add-profile"),
    codexSelectProfile: (profileKey: string) => call("morde:codex-select-profile", profileKey),
    codexListThreads: (cursor: string | null) => call("morde:codex-list-threads", cursor),
    codexAuthorizeProfile: (profileKey: string, scopes: string[]) => call("morde:codex-authorize-profile", profileKey, scopes),
    codexRevokeProfile: (profileKey: string) => call("morde:codex-revoke-profile", profileKey),
    codexAuthorizeThread: (threadID: string, scopes: string[]) => call("morde:codex-authorize-thread", threadID, scopes),
    codexUpdateScopes: (sessionID: string, scopes: string[]) => call("morde:codex-update-scopes", sessionID, scopes),
    codexRevoke: (sessionID: string) => call("morde:codex-revoke", sessionID),
    state: () => call("morde:portal-state"),
    onState: (listener: (value: unknown) => void) => listen("morde:portal-state", listener),
    onSync: (listener: (value: unknown) => void) => listen("morde:portal-sync-state", listener),
    onCodexState: (listener: (value: unknown) => void) => listen("morde:codex-state", listener),
  });
} else if (mode === "host-workbench") {
  contextBridge.exposeInMainWorld("morde", {
    returnToPortalHosts: () => call("morde:return-to-portal-hosts"),
  });
} else {
const bridge = {
  state: () => call("morde:state"),
  openAssistant: () => call("morde:open-assistant"),
  codexAddProfile: () => call("morde:codex-add-profile"),
  codexSelectProfile: (profileKey: string) => call("morde:codex-select-profile", profileKey),
  codexListThreads: (cursor: string | null) => call("morde:codex-list-threads", cursor),
  codexAuthorizeThread: (threadID: string, scopes: string[]) => call("morde:codex-authorize-thread", threadID, scopes),
  codexUpdateScopes: (sessionID: string, scopes: string[]) => call("morde:codex-update-scopes", sessionID, scopes),
  codexRevoke: (sessionID: string) => call("morde:codex-revoke", sessionID),
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
