import { app, BrowserWindow, dialog, ipcMain, Menu, safeStorage, session, shell, Tray, WebContentsView, type Session, type WebContents } from "electron";
import { createHash, randomUUID } from "node:crypto";
import { existsSync, readFileSync, writeFileSync, renameSync, mkdirSync, rmSync } from "node:fs";
import { lstat, realpath } from "node:fs/promises";
import { basename, dirname, isAbsolute, join, posix, resolve, win32 } from "node:path";
import { MordeAPI, checkedId, credentialExpired, credentialExpiresSoon, gatewayURL, localGatewayOrigin, mordeUserDataDirectory, portalOrigin, type ClientLoginAttempt, type Credentials, type ProjectSyncFolder, type WebSession } from "./morde-core";
import { checkedDirectory, MordeSync, pathsOverlap, validateSyncScope, type HandoffRecord, type HandoffTarget, type Mapping, type PreservedFile, type SyncCheckpoint } from "./morde-sync";
import { MordeSyncthingNode, type ManagedWorkspaceFolder } from "./morde-node";

app.setName("Morde");
const userData = mordeUserDataDirectory(app.getPath("appData"), process.env.MORDE_TEST_USER_DATA);
mkdirSync(userData, { recursive: true, mode: 0o700 });
app.setPath("userData", userData);
const zh = (app.getLocale() || "").toLowerCase().startsWith("zh");
let mainWindow: BrowserWindow | null = null;
let tray: Tray | null = null;
let credentials: Credentials | null = null;
let lastOrigin: string | null = null;
let lastInstallation: { origin: string; serviceInstanceID?: string; name: string; id: string } | null = null;
type ClientLoginFence = {
  clientInstanceID: string;
  attemptGeneration: string;
  retiredClientInstanceIDs: string[];
  installationID: string | null;
  allowLegacyRenewal: boolean;
};
type WorkspaceSyncBindingState = "pending_node" | "active" | "revoking" | "revoked";
type LocalWorkspaceSyncBinding = {
  bindingID: string;
  folderID: string;
  deviceID: string;
  serverDeviceID: string;
  relayAddress: string;
  state: WorkspaceSyncBindingState;
  enabled: boolean;
};
type PendingWorkspaceSyncRegistration = {
  challengeID: string;
  signingPayload: string;
  signature: string;
  idempotencyKey: string;
  deviceID: string;
};
type WorkspaceSyncProfile = {
  version: 1;
  serviceInstanceID: string;
  installationId: string;
  deviceID: string | null;
  bindings: Record<string, LocalWorkspaceSyncBinding>;
  pending: Record<string, PendingWorkspaceSyncRegistration>;
};
type LiveSyncStatus = "stopped" | "starting" | "running" | "watching" | "scanning" | "transferring" | "reconnecting" | "stopping" | "conflict" | "failed" | "persisted_uncertain";
type LiveSyncRecord = {
  serviceInstanceID: string;
  installationId: string;
  workspaceId: string;
  directory: string;
  dev: string;
  ino: string;
  scope: string[];
  rescanSeconds: number;
  idempotencyKey: string;
  state: LiveSyncStatus;
  recoveryRequired: boolean;
  conflicts: { path: string; reason?: string }[];
  progress?: Record<string, string | number>;
  sessionId?: string;
  lastConfirmedRevision?: number;
  childPid?: number;
  message?: string;
};
type ProjectMapping = {
  selectionID: string;
  hostId: string;
  workspaceId: string;
  projectName: string;
  directory: string;
  dev: string;
  ino: string;
  folderId?: string;
  registrationKey: string;
};
type ProjectTarget = {
  hostId: string;
  hostName: string;
  hostState: "running" | "stopped";
  workspaceId: string;
  workspaceName: string;
  folders: { folderID: string; projectName: string; relativePath: string; state: string }[];
  blockedMessage?: string;
};
let clientLoginAttempts: Record<string, ClientLoginFence> = {};
let clientLoginAttemptsError = false;
let activeProfile: string | null = null;
let mappings: Mapping[] = [];
let projectMappings: ProjectMapping[] = [];
let syncCheckpoint: SyncCheckpoint | null = null;
type SyncRecoveryWarning = { state: "unknown" | "failed"; workspaceId?: string; mode?: SyncCheckpoint["mode"]; message: string };
let syncRecoveryWarning: SyncRecoveryWarning | null = null;
let handoffRecord: HandoffRecord | null = null;
let profileError: string | null = null;
let workspaceSyncProfile: WorkspaceSyncProfile | null = null;
let workspaceSyncProfileError: string | null = null;
let workspaceSyncError: string | null = null;
let liveSyncRecords: Record<string, LiveSyncRecord> = {};
let liveSyncStateError: string | null = null;
let liveSyncCapability: "unknown" | "enabled" | "disabled" = "unknown";
let liveSyncCapabilityMessage = "实时同步能力尚未核实；操作失败关闭。";
const liveSyncRunners = new Map<string, MordeSync>();
const liveSyncStartWaiters = new Map<string, { resolve: () => void; reject: (error: Error) => void }>();
const liveSyncStopping = new Set<string>();
let workspaceSyncOperationInProgress = false;
let syncChildPid: number | undefined;
let webView: WebContentsView | null = null;
let webPartition: Session | null = null;
let webAttached = false;
let webOpenTask: { window: BrowserWindow; promise: Promise<void> } | null = null;
let view: "web" | "sync" = "web";
let webRefreshTimer: NodeJS.Timeout | null = null;
let credentialExpiryTimer: NodeJS.Timeout | null = null;
let authorizationCheckTimer: NodeJS.Timeout | null = null;
let logoutInProgress = false;
let directorySelectionInProgress = false;
let projectDirectorySelectionInProgress = false;
let projectUploadInProgress = false;
let preservedOperationInProgress = false;
let exitReady = false;
let quitInProgress = false;
const hostWindows = new Set<BrowserWindow>();
const hostWorkbenchReturnHandlers = new Map<WebContents, () => boolean>();
const api = new MordeAPI(() => credentials, () => app.getVersion());
const syncBinary = () => app.isPackaged ? join(process.resourcesPath, "bin", process.platform === "win32" ? "morde-sync.exe" : "morde-sync") : process.env.MORDE_SYNC_BINARY || "";
const workspaceNode = new MordeSyncthingNode(() => app.isPackaged
  ? join(process.resourcesPath, "bin", process.platform === "win32" ? "morde-syncthing.exe" : "morde-syncthing")
  : process.env.MORDE_SYNCTHING_BINARY || join(__dirname, "../bin", process.platform === "win32" ? "morde-syncthing.exe" : "morde-syncthing"));
const sync = new MordeSync(syncBinary, (state, progress) => {
  if (syncCheckpoint && progress?.session_id && /^p[a-z0-9]{7}$/.test(progress.session_id) && progress.session_id !== syncCheckpoint.sessionId) {
    syncCheckpoint.sessionId = progress.session_id;
    try {
      savePrivate(profileFile("sync-checkpoint"), JSON.stringify(syncCheckpoint));
      syncRecoveryWarning = null;
    } catch {
      syncRecoveryWarning = { state: "unknown", workspaceId: syncCheckpoint.workspaceId, mode: syncCheckpoint.mode, message: "恢复点保存状态未知；已请求停止同步。" };
      void sync.stop();
      sendSyncState("shutdown_unknown", progress, "Could not save the sync recovery point");
      notifyState();
      return;
    }
  }
  sendSyncState(state, progress);
}, (pid) => {
  if (!syncCheckpoint) throw new Error("Sync recovery point is missing");
  syncChildPid = pid;
  syncCheckpoint.childPid = pid;
  try { savePrivate(profileFile("sync-checkpoint"), JSON.stringify(syncCheckpoint)); }
  catch {
    syncRecoveryWarning = { state: "unknown", workspaceId: syncCheckpoint.workspaceId, mode: syncCheckpoint.mode, message: "恢复点保存状态未知；同步进程正在停止。" };
    throw new Error("Could not save the sync recovery point; sync startup was interrupted");
  }
});

export function isSystemDirectory(candidate: string, root: string): boolean {
  const path = root.includes("\\") || /^[A-Za-z]:/.test(root) ? win32 : posix;
  if (candidate === root) return true;
  if (path.normalize(root) === path.parse(root).root) return false;
  const relativePath = path.relative(root, candidate);
  return relativePath !== ".." && !relativePath.startsWith(`..${path.sep}`) && !path.isAbsolute(relativePath);
}

function systemDirectoryRoots(): string[] {
  return process.platform === "win32"
    ? [process.env.SystemRoot, process.env.ProgramFiles].filter((value): value is string => Boolean(value))
    : ["/System", "/Applications", "/Library", "/private", "/usr", "/etc", "/var", "/bin", "/sbin", "/dev", "/proc", "/sys", "/run"];
}

async function checkedWorkspaceDirectory(directory: string, workspaceId: string): Promise<Mapping> {
  const appData = app.getPath("userData");
  const protectedMappings = [
    ...mappings.filter((item) => item.workspaceId !== workspaceId),
    ...projectMappings.map(projectAsMapping),
    { workspaceId: "", directory: appData, dev: "", ino: "" },
  ];
  const candidate = await checkedDirectory(directory, protectedMappings);
  const candidateRecoveryRoot = await checkedPreservedRoot(candidate.directory);
  if (await pathsOverlap(candidate.directory, candidateRecoveryRoot)) throw new Error("Workspace folder overlaps its safety-copy directory");
  for (const other of [
    ...protectedMappings,
    ...mappings.filter((item) => item.workspaceId === workspaceId),
  ]) {
    if (await pathsOverlap(candidateRecoveryRoot, other.directory)) throw new Error("Safety-copy directory overlaps a protected folder");
    if (other.workspaceId) {
      const otherRecoveryRoot = await checkedPreservedRoot(other.directory);
      if (await pathsOverlap(candidate.directory, otherRecoveryRoot)) throw new Error("Workspace folder overlaps another safety-copy directory");
    }
  }
  for (const root of systemDirectoryRoots()) {
    let canonicalRoot: string;
    try { canonicalRoot = await realpath(root); }
    catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") continue;
      throw new Error("Could not verify protected system directories");
    }
    if (isSystemDirectory(candidate.directory, canonicalRoot)) throw new Error("Choose a project folder outside system directories");
  }
  return candidate;
}

function projectAsMapping(value: ProjectMapping): Mapping {
  return { workspaceId: value.workspaceId, directory: value.directory, dev: value.dev, ino: value.ino };
}

function normalizedProjectName(value: unknown): string {
  if (typeof value !== "string") throw new Error("项目名无效");
  const name = value.normalize("NFC");
  if (!name || name.trim() !== name || [...name].length > 120 || name === "." || name === ".." ||
    /[\\/<>:"|?*\u0000-\u001f\u007f-\u009f]/u.test(name) || /[. ]$/.test(name)) throw new Error("项目名必须是一个有效的目录名");
  const base = name.split(".", 1)[0]!.toUpperCase();
  if (["CON", "PRN", "AUX", "NUL"].includes(base) || /^(?:COM|LPT)[1-9]$/.test(base)) throw new Error("项目名是系统保留名称");
  return name;
}

async function checkedProjectDirectory(directory: string, workspaceId: string, exceptSelectionID?: string): Promise<Mapping> {
  const protectedMappings = [
    ...mappings,
    ...projectMappings.filter((item) => item.selectionID !== exceptSelectionID).map(projectAsMapping),
    { workspaceId: "", directory: app.getPath("userData"), dev: "", ino: "" },
  ];
  const candidate = await checkedDirectory(directory, protectedMappings);
  const candidateRecoveryRoot = await checkedPreservedRoot(candidate.directory);
  if (await pathsOverlap(candidate.directory, candidateRecoveryRoot)) throw new Error("项目目录与本地安全副本目录重叠");
  for (const other of protectedMappings) {
    if (await pathsOverlap(candidateRecoveryRoot, other.directory)) throw new Error("本地安全副本目录与其他受保护目录重叠");
    if (other.workspaceId) {
      const otherRecoveryRoot = await checkedPreservedRoot(other.directory);
      if (await pathsOverlap(candidate.directory, otherRecoveryRoot)) throw new Error("项目目录与其他本地安全副本目录重叠");
    }
  }
  for (const root of systemDirectoryRoots()) {
    let canonicalRoot: string;
    try { canonicalRoot = await realpath(root); }
    catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") continue;
      throw new Error("无法验证受保护的系统目录");
    }
    if (isSystemDirectory(candidate.directory, canonicalRoot)) throw new Error("请选择系统目录之外的项目文件夹");
  }
  return { ...candidate, workspaceId };
}

async function confirmProjectMapping(mapping: ProjectMapping): Promise<void> {
  const current = await checkedProjectDirectory(mapping.directory, mapping.workspaceId, mapping.selectionID);
  if (current.directory !== mapping.directory || current.dev !== mapping.dev || current.ino !== mapping.ino) {
    throw new Error("项目文件夹身份已变化，请重新选择");
  }
}

async function checkedPreservedRoot(directory: string): Promise<string> {
  const root = join(dirname(directory), ".morde-preserved");
  try {
    const info = await lstat(root);
    if (!info.isDirectory() || info.isSymbolicLink()) throw new Error("Safety-copy directory is not a real directory");
    const canonical = await realpath(root);
    if (canonical !== root) throw new Error("Safety-copy directory changed while being checked");
    return canonical;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return root;
    throw error;
  }
}

async function confirmWorkspaceMapping(mapping: Mapping): Promise<void> {
  const current = await checkedWorkspaceDirectory(mapping.directory, mapping.workspaceId);
  if (current.directory !== mapping.directory || current.dev !== mapping.dev || current.ino !== mapping.ino) {
    throw new Error("Folder identity changed; select it again");
  }
}

function preservedOperationBlocked(): boolean {
  return preservedOperationInProgress || directorySelectionInProgress || projectDirectorySelectionInProgress || projectUploadInProgress || workspaceSyncOperationInProgress || workspaceNode.mayOwnWorkspace() ||
    sync.snapshot() !== "idle" || syncCheckpoint !== null || childMayStillRun(syncCheckpoint) || activeLiveSync();
}

async function withPreservedMapping<T>(workspaceId: unknown, operation: (mapping: Mapping) => Promise<T>): Promise<T> {
  if (preservedOperationBlocked()) throw new Error("Finish or verify the prior sync recovery point before accessing local safety copies");
  const auth = credentials;
  if (!auth || !activeProfile || activeProfile !== profileID(auth)) throw new Error("Log in before accessing local safety copies");
  if (profileError) throw new Error(profileError);
  const id = checkedId("workspace", workspaceId);
  const matches = mappings.filter((item) => item.workspaceId === id);
  if (matches.length !== 1) throw new Error("Choose a local folder for this Workspace first");
  const mapping = matches[0]!;
  preservedOperationInProgress = true;
  try {
    await confirmWorkspaceMapping(mapping);
    if (credentials !== auth || activeProfile !== profileID(auth) || profileError || mappings.filter((item) => item.workspaceId === id).length !== 1) {
      throw new Error("Account or Workspace mapping changed while checking local safety copies");
    }
    await assertNoLiveSyncForDirectory(mapping.directory);
    if (workspaceSyncOperationInProgress || workspaceNode.mayOwnWorkspace() || sync.snapshot() !== "idle" || syncCheckpoint !== null || childMayStillRun(syncCheckpoint)) {
      throw new Error("Finish or verify the prior sync recovery point before accessing local safety copies");
    }
    const result = await operation(mapping);
    if (credentials !== auth || activeProfile !== profileID(auth)) throw new Error("Account changed during local safety-copy operation");
    return result;
  } finally { preservedOperationInProgress = false; }
}

function childMayStillRun(checkpoint: SyncCheckpoint | null): boolean { return sync.checkpointMayOwnDirectory(checkpoint); }
function priorSyncChildMayStillRun(): boolean { return Boolean(syncCheckpoint?.childPid && syncCheckpoint.childPid !== syncChildPid && childMayStillRun(syncCheckpoint)); }
function clearConfirmedChild(): boolean {
  if (!syncCheckpoint?.childPid) return true;
  if (childMayStillRun(syncCheckpoint)) return false;
  const childPid = syncCheckpoint.childPid;
  const { childPid: _childPid, ...confirmed } = syncCheckpoint;
  try { savePrivate(profileFile("sync-checkpoint"), JSON.stringify(confirmed)); }
  catch {
    syncRecoveryWarning = { state: "unknown", workspaceId: syncCheckpoint.workspaceId, mode: syncCheckpoint.mode, message: "恢复点保存状态未知；请检查本机同步状态。" };
    throw new Error("Could not confirm the sync recovery point was updated");
  }
  syncCheckpoint = confirmed;
  syncRecoveryWarning = null;
  if (syncChildPid === childPid) syncChildPid = undefined;
  return true;
}

function publicDirectoryName(directory: string): string {
  const leaf = basename(directory.replace(/\\/g, "/").replace(/\/+$/, ""));
  return leaf && leaf !== "." && leaf !== ".." ? leaf.slice(-255) : "本机目录";
}

function publicMapping(mapping: Mapping): { workspaceId: string; directoryName: string } {
  return { workspaceId: mapping.workspaceId, directoryName: publicDirectoryName(mapping.directory) };
}

function publicProjectMapping(mapping: ProjectMapping): { selectionID: string; hostId: string; workspaceId: string; projectName: string; directoryName: string; folderId?: string } {
  return {
    selectionID: mapping.selectionID, hostId: mapping.hostId, workspaceId: mapping.workspaceId,
    projectName: mapping.projectName, directoryName: publicDirectoryName(mapping.directory),
    ...(mapping.folderId ? { folderId: mapping.folderId } : {}),
  };
}

function publicHandoff(record: HandoffRecord | null): Omit<HandoffRecord, "id"> | null {
  if (!record) return null;
  const { id: _id, ...publicRecord } = record;
  return { ...publicRecord, scope: [...record.scope] };
}

function publicSyncProgress(value: unknown): Record<string, string | number> | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
  const progress = value as Record<string, unknown>;
  const output: Record<string, string | number> = {};
  const states = new Set(["running", "idle", "scanning", "transferring", "conflict", "failed", "cancelled", "unknown", "reconnecting"]);
  const stages = new Set(["inventory", "hash", "planning", "transferring", "scanning", "watching", "reconnecting", "stopping", "scan", "transfer", "reconnect"]);
  if (typeof progress.state === "string" && states.has(progress.state)) output.state = progress.state;
  if (typeof progress.stage === "string" && stages.has(progress.stage)) output.stage = progress.stage;
  if (typeof progress.session_id === "string" && /^p[a-z0-9]{7}$/.test(progress.session_id)) output.session_id = progress.session_id;
  for (const key of ["confirmed_bytes", "planned_bytes", "file_count", "files_processed", "total_bytes", "bytes_processed"]) {
    const count = progress[key];
    if (Number.isSafeInteger(count) && (count as number) >= 0) output[key] = count as number;
  }
  if (Number.isSafeInteger(progress.confirmed_revision) && (progress.confirmed_revision as number) >= 0) output.confirmed_revision = progress.confirmed_revision as number;
  return Object.keys(output).length ? output : undefined;
}

function validLiveSyncRecord(value: unknown, auth: Credentials, workspaceID: string): value is LiveSyncRecord {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const record = value as Partial<LiveSyncRecord>;
  const states = new Set(["stopped", "starting", "running", "watching", "scanning", "transferring", "reconnecting", "stopping", "conflict", "failed", "persisted_uncertain"]);
  const conflicts = publicSyncConflicts(record.conflicts);
  try {
    return record.serviceInstanceID === auth.serviceInstanceID && record.installationId === auth.installationId && record.workspaceId === workspaceID &&
      typeof record.directory === "string" && record.directory.length <= 4096 && record.directory.includes("\0") === false && isAbsolute(record.directory) && resolve(record.directory) === record.directory &&
      typeof record.dev === "string" && /^\d+$/.test(record.dev) && typeof record.ino === "string" && /^\d+$/.test(record.ino) &&
      Number.isSafeInteger(record.rescanSeconds) && (record.rescanSeconds as number) >= 5 && (record.rescanSeconds as number) <= 3600 &&
      typeof record.idempotencyKey === "string" && /^[A-Za-z0-9._~-]{1,128}$/.test(record.idempotencyKey) &&
      typeof record.recoveryRequired === "boolean" && conflicts !== null && Array.isArray(record.conflicts) && record.conflicts.length === conflicts.length &&
      (record.sessionId === undefined || typeof record.sessionId === "string" && /^p[a-z0-9]{7}$/.test(record.sessionId)) &&
      (record.lastConfirmedRevision === undefined || Number.isSafeInteger(record.lastConfirmedRevision) && record.lastConfirmedRevision >= 0) &&
      (record.childPid === undefined || Number.isSafeInteger(record.childPid) && record.childPid > 0) &&
      typeof record.state === "string" && states.has(record.state) && Array.isArray(record.scope) && JSON.stringify(validateSyncScope(record.scope)) === JSON.stringify(record.scope) &&
      (record.progress === undefined || publicSyncProgress(record.progress) !== undefined) &&
      (record.message === undefined || typeof record.message === "string" && record.message.length <= 512);
  } catch { return false; }
}

function validLiveSyncStore(value: unknown, auth: Credentials): value is { version: 1; records: Record<string, LiveSyncRecord> } {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const store = value as { version?: unknown; records?: unknown };
  if (store.version !== 1 || !store.records || typeof store.records !== "object" || Array.isArray(store.records)) return false;
  return Object.entries(store.records).every(([workspaceID, record]) => /^w[a-z0-9]{7}$/.test(workspaceID) && validLiveSyncRecord(record, auth, workspaceID));
}

function saveLiveSyncRecords(): void {
  if (!credentials || !activeProfile || !safeStorageReady() || liveSyncStateError) throw new Error(liveSyncStateError ?? "Secure local Workspace sync storage is unavailable");
  savePrivate(profileFile("live-sync"), safeStorage.encryptString(JSON.stringify({ version: 1, records: liveSyncRecords })));
}

function publicLiveSyncRecord(record: LiveSyncRecord, capability = liveSyncCapability): Record<string, unknown> {
  return {
    workspaceId: record.workspaceId,
    state: record.state,
    capability,
    scope: record.scope,
    rescanSeconds: record.rescanSeconds,
    ...(record.sessionId ? { sessionId: record.sessionId } : {}),
    ...(record.lastConfirmedRevision === undefined ? {} : { lastConfirmedRevision: record.lastConfirmedRevision }),
    ...(record.progress ? { progress: publicSyncProgress(record.progress) } : {}),
    conflicts: record.conflicts,
    recoveryRequired: record.recoveryRequired,
    ...(record.message ? { message: redactPrivateValues(record.message) } : {}),
  };
}

function publicLiveSyncs(): Record<string, unknown> {
  const ids = new Set([...mappings.map((mapping) => mapping.workspaceId), ...Object.keys(liveSyncRecords)]);
  return Object.fromEntries([...ids].map((workspaceID) => {
    const record = liveSyncRecords[workspaceID];
    if (record) return [workspaceID, publicLiveSyncRecord(record)];
    const unavailable = liveSyncCapability !== "enabled" || liveSyncStateError !== null;
    return [workspaceID, {
      workspaceId: workspaceID,
      state: unavailable ? "unavailable" : "stopped",
      capability: unavailable ? "disabled" : "enabled",
      conflicts: [],
      recoveryRequired: false,
      ...(unavailable ? { message: liveSyncStateError ? "本机实时同步状态无法安全读取；操作失败关闭。" : liveSyncCapabilityMessage } : {}),
    }];
  }));
}

function notifyLiveSync(workspaceID: string): void {
  const record = liveSyncRecords[workspaceID];
  if (record) mainWindow?.webContents.send("morde:live-sync-state", { workspaceId: workspaceID, event: publicLiveSyncRecord(record) });
  notifyState();
}

function activeLiveSync(workspaceID?: string): boolean {
  const records = workspaceID ? [liveSyncRecords[workspaceID]].filter((value): value is LiveSyncRecord => Boolean(value)) : Object.values(liveSyncRecords);
  return records.some((record) => liveSyncRunners.get(record.workspaceId)?.snapshot() !== undefined && liveSyncRunners.get(record.workspaceId)?.snapshot() !== "idle" ||
    ["starting", "running", "watching", "scanning", "transferring", "reconnecting", "stopping"].includes(record.state));
}

function liveSyncCheckpoint(record: LiveSyncRecord): SyncCheckpoint {
  return {
    serviceInstanceID: record.serviceInstanceID,
    installationId: record.installationId,
    workspaceId: record.workspaceId,
    directory: record.directory,
    dev: record.dev,
    ino: record.ino,
    mode: "live_sync",
    scope: record.scope,
    idempotencyKey: record.idempotencyKey,
    ...(record.sessionId ? { sessionId: record.sessionId } : {}),
    ...(record.childPid ? { childPid: record.childPid } : {}),
  };
}

async function assertNoLiveSyncForDirectory(directory: string, workspaceID?: string): Promise<void> {
  for (const record of Object.values(liveSyncRecords)) {
    if (record.workspaceId === workspaceID || !record.recoveryRequired && !activeLiveSync(record.workspaceId) && record.state !== "conflict") continue;
    if (await pathsOverlap(directory, record.directory)) throw new Error("Another Workspace has a live sync operation or unresolved recovery state for this folder");
  }
}

function sendSyncState(state: string, progress?: unknown, message?: string): void {
  const checkpoint = syncCheckpoint;
  const safeState = ["running", "idle", "shutdown_unknown"].includes(state) ? state : "unknown";
  const safeProgress = publicSyncProgress(progress);
  const payload = {
    state: safeState,
    workspaceId: checkpoint?.workspaceId ?? null,
    mode: checkpoint?.mode ?? null,
    ...(safeProgress ? { progress: safeProgress } : {}),
    ...(message ? { message } : {}),
    ...(syncRecoveryWarning ? { recovery: syncRecoveryWarning } : {}),
  };
  mainWindow?.webContents.send("morde:sync-state", payload);
  const auth = credentials;
  if (auth && webView && isTrustedPortalBridgePage(webView.webContents.getURL(), auth.origin)) {
    const portalCheckpoint = checkpoint?.mode === "project_upload" ? checkpoint : null;
    const recovery = syncRecoveryView();
    webView.webContents.send("morde:portal-sync-state", {
      state: portalCheckpoint ? safeState : "idle",
      workspaceId: portalCheckpoint?.workspaceId ?? null,
      mode: portalCheckpoint ? "project_upload" : null,
      ...(portalCheckpoint && safeProgress ? { progress: safeProgress } : {}),
      ...(portalCheckpoint && message ? { message: safePortalProjectMessage(message) } : {}),
      ...(portalCheckpoint && recovery?.mode === "project_upload" ? {
        recovery: {
          state: recovery.state,
          mode: "project_upload",
          ...(recovery.workspaceId ? { workspaceId: recovery.workspaceId } : {}),
          ...(recovery.message ? { message: safePortalProjectMessage(recovery.message) } : {}),
        },
      } : {}),
    });
  }
}

function publicSyncConflicts(value: unknown): { path: string; reason?: string }[] | null {
  if (!Array.isArray(value) || value.length > 1000) return null;
  const conflicts: { path: string; reason?: string }[] = [];
  for (const item of value) {
    const path = typeof item === "string" ? item : item && typeof item === "object" && !Array.isArray(item) ? (item as { path?: unknown }).path : null;
    if (typeof path !== "string" || path.length > 4096 || /[\u0000-\u001f\u007f]/.test(path)) return null;
    try { validateSyncScope([path]); } catch { return null; }
    const reason = item && typeof item === "object" && !Array.isArray(item) ? (item as { reason?: unknown }).reason : undefined;
    if (reason !== undefined && (typeof reason !== "string" || !/^[a-z][a-z0-9_-]{0,63}$/.test(reason))) return null;
    conflicts.push({ path, ...(typeof reason === "string" ? { reason } : {}) });
  }
  return conflicts;
}

function syncRecoveryView(): { state: "pending" | "unknown" | "failed"; workspaceId?: string; mode?: SyncCheckpoint["mode"]; message?: string } | null {
  if (syncRecoveryWarning) return syncRecoveryWarning;
  if (syncCheckpoint) {
    if (sync.snapshot() === "shutdown_unknown" || childMayStillRun(syncCheckpoint)) {
      return { state: "unknown", workspaceId: syncCheckpoint.workspaceId, mode: syncCheckpoint.mode, message: "上次同步的停止状态未确认；检查点仍保留。" };
    }
    return { state: "pending", workspaceId: syncCheckpoint.workspaceId, mode: syncCheckpoint.mode };
  }
  if (profileError) return { state: "unknown", message: "本机同步状态无法读取；本机同步已暂停。" };
  return null;
}

function publicPortalProjectState(): unknown {
  if (credentials && credentialExpired(credentials.clientExpiresAt)) clearAuthorization();
  const projectUploadCheckpoint = credentials && syncCheckpoint?.mode === "project_upload" ? syncCheckpoint : null;
  const recovery = credentials ? syncRecoveryView() : null;
  const projectRecovery = recovery?.mode === "project_upload" ? {
    state: recovery.state,
    mode: "project_upload" as const,
    ...(recovery.workspaceId ? { workspaceId: recovery.workspaceId } : {}),
    ...(recovery.message ? { message: safePortalProjectMessage(recovery.message) } : {}),
  } : null;
  const snapshot = sync.snapshot();
  return {
    projectMappings: credentials ? projectMappings.map(publicProjectMapping) : [],
    syncState: projectUploadCheckpoint && ["idle", "running", "shutdown_unknown"].includes(snapshot) ? snapshot : "idle",
    syncCheckpoint: projectUploadCheckpoint ? {
      workspaceId: projectUploadCheckpoint.workspaceId,
      mode: "project_upload",
      ...(projectUploadCheckpoint.projectSelectionID ? { projectSelectionID: projectUploadCheckpoint.projectSelectionID } : {}),
      ...(projectUploadCheckpoint.projectName ? { projectName: projectUploadCheckpoint.projectName } : {}),
    } : null,
    syncRecovery: projectRecovery,
  };
}

function markSyncRecoveryUnknown(message: string): void {
  syncRecoveryWarning = {
    state: "unknown",
    ...(syncCheckpoint ? { workspaceId: syncCheckpoint.workspaceId, mode: syncCheckpoint.mode } : {}),
    message,
  };
  notifyState();
}

function removeSyncCheckpointFile(): void {
  try { rmSync(settingsPath(profileFile("sync-checkpoint")), { force: true }); }
  catch {
    markSyncRecoveryUnknown("本机同步结果已收到，但恢复点清理未确认；请检查本机同步状态。");
    throw new Error("Could not confirm the local sync recovery point was cleared");
  }
}

function settingsPath(name: string): string { return join(app.getPath("userData"), name); }
function accountProfileID(origin: string, accountName: string): string { return createHash("sha256").update(`${origin}\0${accountName}`).digest("hex"); }
function profileID(auth: Credentials): string {
  return createHash("sha256").update(`${auth.origin}\0${auth.serviceInstanceID}\0${auth.accountName}\0${auth.installationId}`).digest("hex");
}
function profileFile(name: "mappings" | "project-mappings" | "sync-checkpoint" | "workspace-sync" | "handoff" | "live-sync"): string {
  if (!activeProfile) throw new Error("Log in before changing local Workspace state");
  return `${name}-${activeProfile}.${name === "workspace-sync" || name === "live-sync" ? "bin" : "json"}`;
}
function savePrivate(name: string, value: string | Buffer): void {
  const target = settingsPath(name);
  mkdirSync(app.getPath("userData"), { recursive: true, mode: 0o700 });
  const pending = `${target}.${randomUUID()}.tmp`;
  writeFileSync(pending, value, { mode: 0o600 });
  renameSync(pending, target);
}

function validHandoffRecord(value: unknown): value is HandoffRecord {
  if (!value || typeof value !== "object") return false;
  const record = value as Partial<HandoffRecord>;
  if (typeof record.id !== "string" || !/^[A-Za-z0-9._~-]{1,128}$/.test(record.id) || typeof record.workspaceId !== "string" || !/^w[a-z0-9]{7}$/.test(record.workspaceId) ||
    !["desktop", "cloud_workspace"].includes(record.source as string) || !["desktop", "cloud_workspace"].includes(record.target as string) || record.source === record.target ||
    !["checking", "needs_review", "verified"].includes(record.phase as string) ||
    (record.revision !== undefined && (!Number.isSafeInteger(record.revision) || (record.revision as number) < 0)) ||
    (record.phase === "verified" && record.revision === undefined)) return false;
  try { validateSyncScope(record.scope); return true; }
  catch { return false; }
}

function validProjectMapping(value: unknown): value is ProjectMapping {
  if (!value || typeof value !== "object") return false;
  const mapping = value as Partial<ProjectMapping>;
  try { normalizedProjectName(mapping.projectName); }
  catch { return false; }
  return typeof mapping.selectionID === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(mapping.selectionID) &&
    typeof mapping.hostId === "string" && /^h[a-z0-9]{7}$/.test(mapping.hostId) &&
    typeof mapping.workspaceId === "string" && /^w[a-z0-9]{7}$/.test(mapping.workspaceId) &&
    typeof mapping.directory === "string" && isAbsolute(mapping.directory) && resolve(mapping.directory) === mapping.directory &&
    typeof mapping.dev === "string" && /^\d+$/.test(mapping.dev) && typeof mapping.ino === "string" && /^\d+$/.test(mapping.ino) &&
    (mapping.folderId === undefined || typeof mapping.folderId === "string" && /^morde-[a-f0-9]{32}$/.test(mapping.folderId)) &&
    typeof mapping.registrationKey === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(mapping.registrationKey);
}

function saveHandoffRecord(record: HandoffRecord | null): void {
  if (!record) {
    rmSync(settingsPath(profileFile("handoff")), { force: true });
    handoffRecord = null;
    return;
  }
  if (!validHandoffRecord(record)) throw new Error("Handoff record is invalid");
  const saved = { ...record, scope: validateSyncScope(record.scope) };
  savePrivate(profileFile("handoff"), JSON.stringify(saved));
  handoffRecord = saved;
}

function saveWorkspaceSyncProfile(next: WorkspaceSyncProfile): void {
  if (!credentials || !activeProfile || !safeStorageReady() || !validWorkspaceSyncProfile(next, credentials)) {
    throw new Error("Could not safely save Workspace sync state");
  }
  savePrivate(profileFile("workspace-sync"), safeStorage.encryptString(JSON.stringify(next)));
  workspaceSyncProfile = next;
}
function readOptional(name: string): string | null {
  try { return readFileSync(settingsPath(name), "utf8"); }
  catch (error) { if ((error as NodeJS.ErrnoException).code === "ENOENT") return null; throw error; }
}

function readOptionalBuffer(name: string): Buffer | null {
  try { return readFileSync(settingsPath(name)); }
  catch (error) { if ((error as NodeJS.ErrnoException).code === "ENOENT") return null; throw error; }
}

type PendingRevocation = { origin: string; serviceInstanceID: string; installationID: string; clientInstanceID: string | null; createdAt: string };

function readPendingRevocations(): PendingRevocation[] {
  const raw = readOptional("pending-revocations.json");
  if (raw === null) return [];
  const saved = JSON.parse(raw) as unknown;
  if (!Array.isArray(saved) || !saved.every((item) => item && typeof item === "object" &&
    typeof item.origin === "string" && item.origin === portalOrigin(item.origin) &&
    typeof item.serviceInstanceID === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(item.serviceInstanceID) &&
    typeof item.installationID === "string" && /^i[a-z0-9]{7}$/.test(item.installationID) &&
    (item.clientInstanceID === null || typeof item.clientInstanceID === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(item.clientInstanceID)) &&
    typeof item.createdAt === "string" && Number.isFinite(Date.parse(item.createdAt)))) throw new Error("Invalid pending revocation list");
  return saved as PendingRevocation[];
}

function sameRevocation(left: PendingRevocation, right: PendingRevocation): boolean {
  return left.origin === right.origin && left.serviceInstanceID === right.serviceInstanceID && left.installationID === right.installationID && left.clientInstanceID === right.clientInstanceID;
}

function rememberPendingRevocation(identity: Omit<PendingRevocation, "createdAt">): void {
  const entry = { ...identity, createdAt: new Date().toISOString() };
  const pending = readPendingRevocations().filter((item) => !sameRevocation(item, entry));
  savePrivate("pending-revocations.json", JSON.stringify([...pending, entry]));
}

function confirmPendingRevocation(identity: Omit<PendingRevocation, "createdAt">): void {
  const pending = readPendingRevocations().filter((item) => !sameRevocation(item, { ...identity, createdAt: "" }));
  if (pending.length) savePrivate("pending-revocations.json", JSON.stringify(pending));
  else rmSync(settingsPath("pending-revocations.json"), { force: true });
}

function loadClientLoginAttempts(): void {
  try {
    if (existsSync(settingsPath("client-login-attempts.invalid"))) {
      clientLoginAttemptsError = true;
      return;
    }
    const raw = readOptional("client-login-attempts.json");
    if (raw === null) return;
    // Keep the signed 64-bit JSON integer exact in JavaScript by parsing its decimal token as text.
    const saved = JSON.parse(raw.replace(/("attemptGeneration"\s*:\s*)(\d+)(?=\s*[,}])/g, '$1"$2"')) as Record<string, unknown>;
    if (!saved || Array.isArray(saved) || typeof saved !== "object") throw new Error("Invalid client login state");
    const validated: Record<string, ClientLoginFence> = {};
    for (const [key, value] of Object.entries(saved)) {
      const attempt = value as { clientInstanceID?: unknown; attemptGeneration?: unknown; retiredClientInstanceIDs?: unknown; installationID?: unknown; allowLegacyRenewal?: unknown };
      if (!/^[0-9a-f]{64}$/.test(key) || !attempt || typeof attempt !== "object" ||
        typeof attempt.clientInstanceID !== "string" || !/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(attempt.clientInstanceID) ||
        typeof attempt.attemptGeneration !== "string" || !/^(0|[1-9][0-9]*)$/.test(attempt.attemptGeneration) || BigInt(attempt.attemptGeneration) > 9223372036854775807n ||
        (attempt.retiredClientInstanceIDs !== undefined && (!Array.isArray(attempt.retiredClientInstanceIDs) || !attempt.retiredClientInstanceIDs.every((id) => typeof id === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(id)))) ||
        (attempt.retiredClientInstanceIDs as string[] | undefined)?.includes(attempt.clientInstanceID) ||
        (attempt.installationID !== undefined && attempt.installationID !== null && (typeof attempt.installationID !== "string" || !/^i[a-z0-9]{7}$/.test(attempt.installationID))) ||
        (attempt.allowLegacyRenewal !== undefined && typeof attempt.allowLegacyRenewal !== "boolean")) throw new Error("Invalid client login state");
      const retired = attempt.retiredClientInstanceIDs as string[] | undefined;
      validated[key] = {
        clientInstanceID: attempt.clientInstanceID, attemptGeneration: attempt.attemptGeneration,
        retiredClientInstanceIDs: retired ?? [], installationID: (attempt.installationID as string | null | undefined) ?? null,
        allowLegacyRenewal: attempt.allowLegacyRenewal === true,
      };
    }
    clientLoginAttempts = validated;
  } catch { clientLoginAttemptsError = true; }
}

function writeClientLoginAttempts(next: Record<string, ClientLoginFence>): void {
  if (clientLoginAttemptsError) throw new Error("Morde 登录恢复状态无法安全读取或写入，请修复本地状态文件后重试。");
  const serialized = JSON.stringify(next).replace(/("attemptGeneration"\s*:\s*)"(0|[1-9][0-9]*)"/g, "$1$2");
  try { savePrivate("client-login-attempts.json", serialized); }
  catch (error) {
    clientLoginAttemptsError = true;
    try { writeFileSync(settingsPath("client-login-attempts.invalid"), "Login fence persistence failed.", { mode: 0o600 }); } catch { /* Keep the in-memory fence closed if storage is unavailable. */ }
    throw new Error("Morde 登录恢复状态无法安全保存，本次无法安全登录。请修复本地状态文件后重试。", { cause: error });
  }
  clientLoginAttempts = next;
}

function nextClientLoginAttempt(origin: string, accountName: string): ClientLoginAttempt {
  if (clientLoginAttemptsError) throw new Error("Morde 登录恢复状态无法安全读取或写入，请修复本地状态文件后重试。");
  const key = accountProfileID(origin, accountName);
  const current = clientLoginAttempts[key] ?? {
    clientInstanceID: randomUUID(), attemptGeneration: "0", retiredClientInstanceIDs: [], installationID: null, allowLegacyRenewal: true,
  };
  if (BigInt(current.attemptGeneration) >= 9223372036854775807n) {
    throw new Error("登录尝试计数无法继续，请修复本地状态后重试。");
  }
  const next = { ...current, attemptGeneration: (BigInt(current.attemptGeneration) + 1n).toString() };
  writeClientLoginAttempts({ ...clientLoginAttempts, [key]: next });
  return { ...next, idempotencyKey: randomUUID() };
}

function isCurrentClientLoginAttempt(origin: string, accountName: string, attempt: ClientLoginAttempt): boolean {
  const current = clientLoginAttempts[accountProfileID(origin, accountName)];
  return current?.clientInstanceID === attempt.clientInstanceID && current.attemptGeneration === attempt.attemptGeneration;
}

function bindClientInstallation(origin: string, accountName: string, attempt: ClientLoginAttempt, installationID: string): void {
  const key = accountProfileID(origin, accountName);
  const current = clientLoginAttempts[key];
  if (!current || current.clientInstanceID !== attempt.clientInstanceID || current.attemptGeneration !== attempt.attemptGeneration) {
    throw new Error("较新的登录尝试已开始，已丢弃此响应。请使用最新尝试登录。");
  }
  writeClientLoginAttempts({ ...clientLoginAttempts, [key]: { ...current, installationID, allowLegacyRenewal: false } });
}

function rotateClientInstanceID(origin: string, accountName: string): void {
  if (clientLoginAttemptsError) throw new Error("Morde 登录恢复状态无法读取，请修复本地状态文件后重试。");
  const key = accountProfileID(origin, accountName);
  const old = clientLoginAttempts[key];
  let clientInstanceID = randomUUID();
  const retiredClientInstanceIDs = [...(old?.retiredClientInstanceIDs ?? []), ...(old ? [old.clientInstanceID] : [])];
  while (retiredClientInstanceIDs.includes(clientInstanceID)) clientInstanceID = randomUUID();
  writeClientLoginAttempts({ ...clientLoginAttempts, [key]: {
    clientInstanceID, attemptGeneration: "0", retiredClientInstanceIDs, installationID: null, allowLegacyRenewal: false,
  } });
}

function safeStorageReady(): boolean {
  if (!safeStorage.isEncryptionAvailable()) return false;
  return process.platform !== "linux" || ["gnome_libsecret", "kwallet", "kwallet5", "kwallet6"].includes(safeStorage.getSelectedStorageBackend());
}

function decodeWorkspaceSyncBase64(value: unknown, maxBytes: number): Buffer | null {
  if (typeof value !== "string" || !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(value)) return null;
  const decoded = Buffer.from(value, "base64");
  return decoded.length > 0 && decoded.length <= maxBytes && decoded.toString("base64") === value ? decoded : null;
}

function validWorkspaceSyncRelayAddress(value: unknown, expectedDeviceID: string): value is string {
  if (typeof value !== "string" || value.length > 2048) return false;
  try {
    const url = new URL(value), entries = [...url.searchParams.entries()];
    return url.protocol === "relay:" && !url.username && !url.password && Boolean(url.hostname) &&
      Number.isInteger(Number(url.port)) && Number(url.port) > 0 && Number(url.port) <= 65535 &&
      url.pathname === "/" && !url.hash && entries.length === 2 &&
      url.searchParams.getAll("id").length === 1 && url.searchParams.get("id") === expectedDeviceID &&
      url.searchParams.getAll("token").length === 1 && /^[A-Za-z0-9_-]{32,128}$/.test(url.searchParams.get("token") ?? "");
  } catch { return false; }
}

function emptyWorkspaceSyncProfile(auth: Credentials): WorkspaceSyncProfile {
  return { version: 1, serviceInstanceID: auth.serviceInstanceID, installationId: auth.installationId, deviceID: null, bindings: {}, pending: {} };
}

function validWorkspaceSyncProfile(value: unknown, auth: Credentials): value is WorkspaceSyncProfile {
  if (!value || typeof value !== "object") return false;
  const saved = value as Partial<WorkspaceSyncProfile>;
  if (saved.version !== 1 || saved.serviceInstanceID !== auth.serviceInstanceID || saved.installationId !== auth.installationId ||
    !(saved.deviceID === null || typeof saved.deviceID === "string" && /^[A-Z2-7]{7}(?:-[A-Z2-7]{7}){7}$/.test(saved.deviceID)) ||
    !saved.bindings || typeof saved.bindings !== "object" || Array.isArray(saved.bindings) ||
    !saved.pending || typeof saved.pending !== "object" || Array.isArray(saved.pending)) return false;
  const bindingStates = new Set(["pending_node", "active", "revoking", "revoked"]);
  const bindingsValid = Object.entries(saved.bindings).every(([workspaceID, value]) => Boolean(value) && /^w[a-z0-9]{7}$/.test(workspaceID) &&
    typeof value.bindingID === "string" && /^d[a-z0-9]{7}$/.test(value.bindingID) &&
    typeof value.folderID === "string" && /^morde-[a-f0-9]{32}$/.test(value.folderID) &&
    typeof value.deviceID === "string" && value.deviceID === saved.deviceID && /^[A-Z2-7]{7}(?:-[A-Z2-7]{7}){7}$/.test(value.deviceID) &&
    typeof value.serverDeviceID === "string" && /^[A-Z2-7]{7}(?:-[A-Z2-7]{7}){7}$/.test(value.serverDeviceID) &&
    validWorkspaceSyncRelayAddress(value.relayAddress, value.serverDeviceID) && bindingStates.has(value.state) && typeof value.enabled === "boolean");
  const pendingValid = Object.entries(saved.pending).every(([workspaceID, value]) => Boolean(value) && /^w[a-z0-9]{7}$/.test(workspaceID) &&
    typeof value.challengeID === "string" && /^n[a-z0-9]{7}$/.test(value.challengeID) &&
    Boolean(decodeWorkspaceSyncBase64(value.signingPayload, 16 * 1024)) && Boolean(decodeWorkspaceSyncBase64(value.signature, 1024)) &&
    typeof value.idempotencyKey === "string" && /^[A-Za-z0-9._~-]{1,128}$/.test(value.idempotencyKey) &&
    typeof value.deviceID === "string" && value.deviceID === saved.deviceID && /^[A-Z2-7]{7}(?:-[A-Z2-7]{7}){7}$/.test(value.deviceID));
  return bindingsValid && pendingValid && (saved.deviceID !== null || Object.keys(saved.bindings).length === 0 && Object.keys(saved.pending).length === 0);
}

function loadProfile(auth: Credentials): void {
  activeProfile = profileID(auth);
  mappings = [];
  projectMappings = [];
  syncCheckpoint = null;
  liveSyncRecords = {};
  liveSyncStateError = null;
  liveSyncCapability = "unknown";
  liveSyncCapabilityMessage = "实时同步能力尚未核实；操作失败关闭。";
  handoffRecord = null;
  profileError = null;
  syncRecoveryWarning = null;
  workspaceSyncProfile = emptyWorkspaceSyncProfile(auth);
  workspaceSyncProfileError = null;
  workspaceSyncError = null;
  const mappingFile = profileFile("mappings");
  const projectMappingFile = profileFile("project-mappings");
  const checkpointFile = profileFile("sync-checkpoint");
  try {
    const raw = readOptional(mappingFile);
    if (raw !== null) {
      const saved = JSON.parse(raw) as Mapping[];
      if (!Array.isArray(saved) || !saved.every((m) => m && /^w[a-z0-9]{7}$/.test(m.workspaceId) && typeof m.directory === "string" && typeof m.dev === "string" && typeof m.ino === "string") || new Set(saved.map((m) => m.workspaceId)).size !== saved.length) throw new Error("Invalid Workspace mapping");
      mappings = saved;
    }
  } catch { profileError = "本机同步配置无法读取；同步已暂停，请修复本机安全存储。"; }
  try {
    const raw = readOptional(projectMappingFile);
    if (raw !== null) {
      const saved = JSON.parse(raw) as ProjectMapping[];
      if (!Array.isArray(saved) || !saved.every(validProjectMapping) || new Set(saved.map((item) => item.selectionID)).size !== saved.length) throw new Error("Invalid project mapping");
      projectMappings = saved;
    }
  } catch { profileError ??= "本机项目同步配置无法读取；项目同步已暂停，请检查本机安全存储。"; }
  try {
    const raw = readOptional(checkpointFile);
    if (raw !== null) {
      const saved = JSON.parse(raw) as SyncCheckpoint;
      if (!saved || saved.serviceInstanceID !== auth.serviceInstanceID || saved.installationId !== auth.installationId || !/^w[a-z0-9]{7}$/.test(saved.workspaceId) || typeof saved.directory !== "string" || typeof saved.dev !== "string" || typeof saved.ino !== "string" || !/^[A-Za-z0-9._~-]{1,128}$/.test(saved.idempotencyKey) || !["upload", "download", "bidirectional", "live_sync", "handoff_checkpoint", "project_upload"].includes(saved.mode) || (saved.sessionId && !/^p[a-z0-9]{7}$/.test(saved.sessionId)) || (saved.childPid !== undefined && (!Number.isSafeInteger(saved.childPid) || saved.childPid <= 0)) || (saved.handoffTarget !== undefined && (saved.mode !== "handoff_checkpoint" || !["desktop", "cloud_workspace"].includes(saved.handoffTarget))) ||
        (saved.mode === "project_upload" && (typeof saved.projectSelectionID !== "string" || !/^[0-9a-f-]{36}$/i.test(saved.projectSelectionID) || typeof saved.hostId !== "string" || !/^h[a-z0-9]{7}$/.test(saved.hostId) || typeof saved.folderId !== "string" || !/^morde-[a-f0-9]{32}$/.test(saved.folderId) || typeof saved.projectName !== "string"))) throw new Error("Invalid sync recovery point");
      syncCheckpoint = { ...saved, scope: validateSyncScope(saved.scope === undefined ? [] : saved.scope), ...(saved.mode === "handoff_checkpoint" ? { handoffTarget: saved.handoffTarget ?? "cloud_workspace" } : {}) };
    }
  } catch {
    profileError ??= "本机同步恢复点无法读取；同步已暂停，请修复本机安全存储。";
    syncRecoveryWarning = { state: "unknown", message: "本机同步恢复点无法读取；同步已暂停。" };
  }
  const handoffFile = profileFile("handoff");
  try {
    const raw = readOptional(handoffFile);
    if (raw !== null) {
      const saved = JSON.parse(raw) as unknown;
      if (!validHandoffRecord(saved)) throw new Error("Invalid handoff record");
      handoffRecord = { ...saved, scope: validateSyncScope(saved.scope) };
    }
  } catch { profileError ??= "本机交接记录无法读取；本机同步已暂停，请修复本机安全存储。"; }
  const workspaceSyncFile = profileFile("workspace-sync");
  try {
    const raw = readOptionalBuffer(workspaceSyncFile);
    if (raw !== null) {
      if (!safeStorageReady()) throw new Error("Secure storage is unavailable");
      const saved = JSON.parse(safeStorage.decryptString(raw)) as unknown;
      if (!validWorkspaceSyncProfile(saved, auth)) throw new Error("Invalid Workspace sync state");
      workspaceSyncProfile = saved;
    }
  } catch { workspaceSyncProfileError = "本机 Workspace 节点状态无法读取；节点已暂停，请修复本机安全存储。"; }
  const liveSyncFile = profileFile("live-sync");
  try {
    const raw = readOptionalBuffer(liveSyncFile);
    if (raw !== null) {
      if (raw.byteLength > 4 * 1024 * 1024 || !safeStorageReady()) throw new Error("Secure storage is unavailable");
      const saved = JSON.parse(safeStorage.decryptString(raw)) as unknown;
      if (!validLiveSyncStore(saved, auth)) throw new Error("Invalid live sync profile");
      liveSyncRecords = saved.records;
      let recoveredUncertain = false;
      for (const record of Object.values(liveSyncRecords)) {
        if (["starting", "running", "watching", "scanning", "transferring", "reconnecting", "stopping"].includes(record.state)) {
          record.state = "persisted_uncertain";
          record.recoveryRequired = true;
          record.message = "上次退出时无法确认实时同步的最终结果；先核对原会话再恢复。";
          record.progress = undefined;
          recoveredUncertain = true;
        }
      }
      if (recoveredUncertain) saveLiveSyncRecords();
    }
  } catch {
    liveSyncStateError = "本机实时同步恢复状态无法安全读取；相关操作已暂停。";
  }
}

async function workspaceSyncManagedFolders(): Promise<ManagedWorkspaceFolder[]> {
  if (!workspaceSyncProfile) throw new Error("Workspace sync state is unavailable");
  const folders: ManagedWorkspaceFolder[] = [];
  for (const [workspaceID, binding] of Object.entries(workspaceSyncProfile.bindings)) {
    const matches = mappings.filter((item) => item.workspaceId === workspaceID);
    if (matches.length !== 1) throw new Error("Choose exactly one local folder for each registered Workspace");
    const mapping = matches[0]!;
    await confirmWorkspaceMapping(mapping);
    folders.push({
      workspaceID, folderID: binding.folderID, directory: mapping.directory, bindingState: binding.state,
      enabled: binding.enabled, serverDeviceID: binding.serverDeviceID, relayAddress: binding.relayAddress,
    });
  }
  return folders;
}

async function startWorkspaceNode(auth: Credentials): Promise<{ deviceID: string; certificateDER: Uint8Array }> {
  if (credentials !== auth || !activeProfile || workspaceSyncProfileError || !workspaceSyncProfile) throw new Error("Workspace sync profile is unavailable");
  await api.requireWorkspaceSyncCapability();
  const home = join(userData, "workspace-sync-node", activeProfile);
  const deviceID = workspaceNode.isRunning() ? workspaceNode.identity().then((identity) => identity.deviceID) :
    workspaceNode.start(home, () => {
      workspaceSyncError = "本机 Workspace 节点意外停止；同步状态未知。";
      notifyState();
    });
  const startedID = await deviceID;
  const identity = await workspaceNode.identity();
  if (identity.deviceID !== startedID) throw new Error("Syncthing device identity changed during startup");
  if (credentials !== auth || activeProfile !== profileID(auth)) throw new Error("Authorization changed while starting Workspace sync");
  if (workspaceSyncProfile.deviceID && workspaceSyncProfile.deviceID !== identity.deviceID) {
    throw new Error("This installation's Syncthing identity changed; its Workspace bindings need repair");
  }
  if (!workspaceSyncProfile.deviceID) saveWorkspaceSyncProfile({ ...workspaceSyncProfile, deviceID: identity.deviceID });
  return identity;
}

async function refreshLiveSyncCapability(auth = credentials): Promise<void> {
  if (!auth) {
    liveSyncCapability = "unknown";
    liveSyncCapabilityMessage = "请先登录后核验实时同步能力。";
    return;
  }
  try {
    await api.requireWorkspaceSyncCapability();
    if (credentials !== auth) return;
    liveSyncCapability = "enabled";
    liveSyncCapabilityMessage = "";
  } catch {
    if (credentials !== auth) return;
    liveSyncCapability = "disabled";
    liveSyncCapabilityMessage = "服务端未开放或暂时无法核验 Workspace 同步能力；实时同步已禁用。";
  }
  notifyState();
}

function liveSyncEventState(progress?: Record<string, string | number>): LiveSyncStatus {
  const state = progress?.state;
  const stage = progress?.stage;
  if (state === "conflict" || stage === "conflict") return "conflict";
  if (state === "reconnecting" || stage === "reconnect" || stage === "reconnecting") return "reconnecting";
  if (stage === "watching" || stage === "watch") return "watching";
  if (stage === "transfer" || stage === "transferring") return "transferring";
  if (stage === "scan" || stage === "scanning" || stage === "planning" || stage === "sync_planning") return "scanning";
  return "running";
}

function persistLiveSyncProgress(workspaceID: string, progressValue?: unknown): void {
  const record = liveSyncRecords[workspaceID];
  if (!record) return;
  const progress = publicSyncProgress(progressValue);
  if (!progress) return;
  let changed = false;
  if (progress.session_id !== undefined && progress.session_id !== record.sessionId) {
    record.sessionId = String(progress.session_id);
    changed = true;
  }
  if (typeof progress.confirmed_revision === "number" && progress.confirmed_revision !== record.lastConfirmedRevision) {
    record.lastConfirmedRevision = progress.confirmed_revision;
    changed = true;
  }
  const nextState = liveSyncEventState(progress);
  if (record.recoveryRequired && nextState === "watching" && typeof progress.confirmed_revision === "number") {
    record.recoveryRequired = false;
    record.message = "原实时同步检查点已核对；正在本机监控 Workspace。";
    changed = true;
  }
  if (record.state !== nextState) {
    record.state = nextState;
    changed = true;
  }
  record.progress = progress;
  if (changed) {
    try { saveLiveSyncRecords(); }
    catch {
      record.state = "persisted_uncertain";
      record.recoveryRequired = true;
      record.message = "实时同步恢复状态无法保存；已请求停止并保留工作目录。";
      void liveSyncRunners.get(workspaceID)?.stop();
    }
  }
  notifyLiveSync(workspaceID);
}

function createLiveSyncRunner(workspaceID: string): MordeSync {
  return new MordeSync(syncBinary, (_state, progress) => persistLiveSyncProgress(workspaceID, progress), (pid) => {
    const record = liveSyncRecords[workspaceID];
    if (!record) throw new Error("Live sync recovery point is missing");
    record.childPid = pid;
    record.state = "running";
    try { saveLiveSyncRecords(); }
    catch {
      record.state = "persisted_uncertain";
      record.recoveryRequired = true;
      record.message = "实时同步进程已启动但恢复点保存失败；正在停止。";
      void liveSyncRunners.get(workspaceID)?.stop();
      throw new Error("Could not save the live sync recovery point; startup was interrupted");
    }
    liveSyncStartWaiters.get(workspaceID)?.resolve();
    liveSyncStartWaiters.delete(workspaceID);
    notifyLiveSync(workspaceID);
  });
}

async function finishLiveSync(workspaceID: string, result: unknown): Promise<void> {
  const record = liveSyncRecords[workspaceID];
  if (!record) return;
  const outcome = result && typeof result === "object" ? result as { session?: { id?: unknown; workspace_id?: unknown; state?: unknown; target_revision?: unknown }; conflicts?: unknown } : null;
  const conflicts = publicSyncConflicts(outcome?.conflicts);
  const session = outcome?.session;
  if (session && typeof session.id === "string" && /^p[a-z0-9]{7}$/.test(session.id) && session.workspace_id === workspaceID && conflicts !== null) {
    record.sessionId = session.id;
    if (Number.isSafeInteger(session.target_revision) && Number(session.target_revision) >= 0) record.lastConfirmedRevision = Number(session.target_revision);
    record.conflicts = conflicts;
    if (conflicts.length || session.state === "conflict") {
      record.state = "conflict";
      record.recoveryRequired = true;
      record.message = "实时同步遇到冲突；两侧数据已保留，自动同步已暂停。请先人工核对冲突。";
    } else {
      record.state = "stopped";
      record.recoveryRequired = false;
      record.message = "实时同步已停止；最后确认 revision 已记录。";
    }
  } else if (!liveSyncStopping.has(workspaceID)) {
    record.state = "persisted_uncertain";
    record.recoveryRequired = true;
    record.message = "实时同步结束但最终结果未确认；请先核对原 session/revision。";
  }
  record.progress = undefined;
  try { saveLiveSyncRecords(); }
  catch {
    record.state = "persisted_uncertain";
    record.recoveryRequired = true;
    record.message = "实时同步结束但恢复状态无法保存；本机状态未知。";
  }
  if (liveSyncRunners.get(workspaceID)?.snapshot() === "idle" && !record.recoveryRequired) liveSyncRunners.delete(workspaceID);
  const waiter = liveSyncStartWaiters.get(workspaceID);
  if (waiter) {
    waiter.reject(new Error(record.message ?? "Live sync ended before startup was confirmed"));
    liveSyncStartWaiters.delete(workspaceID);
  }
  notifyLiveSync(workspaceID);
}

async function failLiveSync(workspaceID: string, error: unknown): Promise<void> {
  const record = liveSyncRecords[workspaceID];
  if (!record) return;
  if (!liveSyncStopping.has(workspaceID)) {
    record.state = "persisted_uncertain";
    record.recoveryRequired = true;
    record.message = "实时同步结果未知；本机恢复点已保留，未自动重试。";
    record.progress = undefined;
    try { saveLiveSyncRecords(); } catch { record.message = "实时同步结果与本机恢复点状态均未知；请保留目录并检查。"; }
  }
  const waiter = liveSyncStartWaiters.get(workspaceID);
  if (waiter) {
    waiter.reject(error instanceof Error ? error : new Error("Live sync could not start"));
    liveSyncStartWaiters.delete(workspaceID);
  }
  notifyLiveSync(workspaceID);
}

async function startLiveSync(workspaceIDInput: unknown, scopeInput: unknown, rescanSecondsInput: unknown, confirmRecoveryInput: unknown): Promise<unknown> {
  const workspaceID = checkedId("workspace", workspaceIDInput);
  const scope = validateSyncScope(scopeInput);
  if (!Number.isSafeInteger(rescanSecondsInput) || (rescanSecondsInput as number) < 5 || (rescanSecondsInput as number) > 3600) {
    throw new Error("Live sync scan interval must be an integer from 5 to 3600 seconds");
  }
  if (typeof confirmRecoveryInput !== "boolean") throw new Error("Live sync recovery confirmation is required");
  const rescanSeconds = rescanSecondsInput as number;
  const auth = credentials;
  if (!auth || !activeProfile || activeProfile !== profileID(auth)) throw new Error("Log in to Morde before starting live sync");
  if (profileError || liveSyncStateError || workspaceSyncProfileError || !workspaceSyncProfile) throw new Error("Local Workspace sync state is unavailable; live sync is paused");
  if (logoutInProgress || preservedOperationInProgress || workspaceSyncOperationInProgress || directorySelectionInProgress || projectDirectorySelectionInProgress || projectUploadInProgress) throw new Error("Another local Workspace operation is in progress");
  if (sync.snapshot() !== "idle" || syncCheckpoint || childMayStillRun(syncCheckpoint)) throw new Error("Resolve the existing sync recovery point before starting live sync");
  if (liveSyncStartWaiters.has(workspaceID) || activeLiveSync(workspaceID)) throw new Error("This Workspace already has a live sync operation");
  const previous = liveSyncRecords[workspaceID];
  if (previous?.state === "conflict") throw new Error("Resolve the reported live sync conflicts before restarting");
  if (previous && (previous.serviceInstanceID !== auth.serviceInstanceID || previous.installationId !== auth.installationId)) throw new Error("Live sync recovery belongs to another service or installation");
  if (previous?.recoveryRequired && confirmRecoveryInput !== true) throw new Error("Confirm recovery only after reviewing the previous live sync state");

  const matches = mappings.filter((mapping) => mapping.workspaceId === workspaceID);
  if (matches.length !== 1) throw new Error("Choose exactly one local folder for this Workspace first");
  const mapping = matches[0]!;
  await confirmWorkspaceMapping(mapping);
  if (previous && (previous.directory !== mapping.directory || previous.dev !== mapping.dev || previous.ino !== mapping.ino)) {
    throw new Error("The Workspace folder identity changed; preserve the old folder and review its recovery state before relinking");
  }
  if (previous && JSON.stringify(previous.scope) !== JSON.stringify(scope)) throw new Error("Resume live sync with its original scope; this directory has no automatic scope reset");
  if (workspaceSyncProfile.bindings[workspaceID]?.enabled) throw new Error("Pause the active Workspace sync binding before starting live sync");
  if (workspaceNode.mayOwnWorkspace()) throw new Error("The local Workspace sync node may still own this folder");
  await assertNoLiveSyncForDirectory(mapping.directory, workspaceID);
  if (credentials !== auth || activeProfile !== profileID(auth)) throw new Error("Authorization changed while checking live sync prerequisites");

  try {
    await api.requireWorkspaceSyncCapability();
    if (credentials !== auth || activeProfile !== profileID(auth)) throw new Error("Authorization changed while checking Workspace sync capability");
    liveSyncCapability = "enabled";
    liveSyncCapabilityMessage = "";
  } catch (error) {
    liveSyncCapability = "disabled";
    liveSyncCapabilityMessage = "服务端未开放或暂时无法核验 Workspace 同步能力；实时同步已禁用。";
    notifyState();
    throw error;
  }

  const workspace = await api.workspace(workspaceID) as { state?: string };
  if (credentials !== auth || workspace.state !== "ready") throw new Error("Workspace is not ready for live sync");
  const next: LiveSyncRecord = {
    ...(previous ?? {
      serviceInstanceID: auth.serviceInstanceID,
      installationId: auth.installationId,
      workspaceId: workspaceID,
      directory: mapping.directory,
      dev: mapping.dev,
      ino: mapping.ino,
      scope,
      rescanSeconds,
      idempotencyKey: randomUUID(),
      state: "stopped" as const,
      recoveryRequired: false,
      conflicts: [],
    }),
    scope,
    rescanSeconds,
    state: "starting",
    recoveryRequired: previous?.recoveryRequired ?? false,
    conflicts: [],
    progress: undefined,
    message: previous?.recoveryRequired ? "已获显式恢复确认；正在核对原 session/checkpoint。" : "正在启动本机实时同步。",
  };
  const old = liveSyncRecords[workspaceID];
  liveSyncRecords = { ...liveSyncRecords, [workspaceID]: next };
  try { saveLiveSyncRecords(); }
  catch (error) {
    const restore = { ...liveSyncRecords };
    if (old) restore[workspaceID] = old;
    else delete restore[workspaceID];
    liveSyncRecords = restore;
    throw error;
  }

  let resolveStarted!: () => void;
  let rejectStarted!: (error: Error) => void;
  const started = new Promise<void>((resolve, reject) => { resolveStarted = resolve; rejectStarted = reject; });
  liveSyncStartWaiters.set(workspaceID, { resolve: resolveStarted, reject: rejectStarted });
  let runner = liveSyncRunners.get(workspaceID);
  if (!runner) {
    runner = createLiveSyncRunner(workspaceID);
    liveSyncRunners.set(workspaceID, runner);
  }
  const checkpoint = liveSyncCheckpoint(next);
  if (runner.checkpointMayOwnDirectory(checkpoint)) {
    liveSyncStartWaiters.delete(workspaceID);
    next.state = "persisted_uncertain";
    next.recoveryRequired = true;
    next.message = "旧同步进程仍可能持有目录；实时同步未启动。";
    saveLiveSyncRecords();
    notifyLiveSync(workspaceID);
    throw new Error(next.message);
  }
  void runner.run(auth, mapping, "live_sync", scope, checkpoint,
    () => credentials === auth && !logoutInProgress && !liveSyncStopping.has(workspaceID) && liveSyncRecords[workspaceID]?.state === "starting",
    () => confirmWorkspaceMapping(mapping), rescanSeconds)
    .then((result) => finishLiveSync(workspaceID, result), (error) => failLiveSync(workspaceID, error))
    .catch(() => undefined);
  await started;
  return publicState();
}

async function stopLiveSync(workspaceIDInput: unknown): Promise<unknown> {
  const workspaceID = checkedId("workspace", workspaceIDInput);
  const record = liveSyncRecords[workspaceID];
  const runner = liveSyncRunners.get(workspaceID);
  if (!record || !runner || !activeLiveSync(workspaceID)) throw new Error("This Workspace has no confirmed active live sync process");
  if (liveSyncStopping.has(workspaceID)) throw new Error("Live sync stop is already in progress");
  liveSyncStopping.add(workspaceID);
  record.state = "stopping";
  record.message = "正在停止本机实时同步；最终 session 状态仍需核对。";
  try { saveLiveSyncRecords(); } catch { /* Continue stopping; the last durable recovery record remains. */ }
  notifyLiveSync(workspaceID);
  try {
    const stopped = await runner.stop();
    record.state = "persisted_uncertain";
    record.recoveryRequired = true;
    record.progress = undefined;
    record.message = stopped
      ? "本机停止已确认；云端最终 revision 尚未核对，恢复前需人工确认。"
      : "本机停止状态未确认；保留目录并先核对原 session/revision。";
    if (runner.snapshot() === "idle" && stopped) liveSyncRunners.delete(workspaceID);
    try { saveLiveSyncRecords(); } catch { record.message = "本机停止结果已返回，但恢复点保存失败；当前状态未知。"; }
    return publicLiveSyncRecord(record);
  } finally {
    liveSyncStopping.delete(workspaceID);
    notifyLiveSync(workspaceID);
  }
}

async function stopAllLiveSyncs(): Promise<boolean> {
  let confirmed = true;
  for (const workspaceID of [...liveSyncRunners.keys()]) {
    if (!activeLiveSync(workspaceID)) continue;
    try { await stopLiveSync(workspaceID); }
    catch { confirmed = false; }
  }
  return confirmed;
}

async function registerWorkspaceSyncBinding(auth: Credentials, workspaceID: string, deviceID: string): Promise<LocalWorkspaceSyncBinding> {
  if (!workspaceSyncProfile || credentials !== auth) throw new Error("Authorization changed during Workspace device registration");
  let pending = workspaceSyncProfile.pending[workspaceID];
  const createPending = async (): Promise<PendingWorkspaceSyncRegistration> => {
    const identity = await workspaceNode.identity();
    if (identity.deviceID !== deviceID) throw new Error("Syncthing device identity changed during registration");
    const challenge = await api.createWorkspaceSyncDeviceChallenge(workspaceID, deviceID, identity.certificateDER);
    const signature = await workspaceNode.signDeviceProof(challenge.signingPayload);
    const next: PendingWorkspaceSyncRegistration = {
      challengeID: challenge.id,
      signingPayload: Buffer.from(challenge.signingPayload).toString("base64"),
      signature: Buffer.from(signature).toString("base64"),
      idempotencyKey: randomUUID(),
      deviceID,
    };
    const current = workspaceSyncProfile!;
    saveWorkspaceSyncProfile({ ...current, pending: { ...current.pending, [workspaceID]: next } });
    return next;
  };
  if (pending && pending.deviceID !== deviceID) throw new Error("Pending Workspace registration belongs to another device identity");
  pending ??= await createPending();
  const register = async (value: PendingWorkspaceSyncRegistration) => {
    const payload = decodeWorkspaceSyncBase64(value.signingPayload, 16 * 1024);
    const signature = decodeWorkspaceSyncBase64(value.signature, 1024);
    if (!payload || !signature) throw new Error("Pending Workspace registration is invalid");
    return api.registerWorkspaceSyncDevice(workspaceID, value.challengeID, deviceID, signature, value.idempotencyKey);
  };
  let registered;
  try { registered = await register(pending); }
  catch (error) {
    if (!String(error).includes("HTTP 410")) throw error;
    const current = workspaceSyncProfile!;
    const { [workspaceID]: _expired, ...remaining } = current.pending;
    saveWorkspaceSyncProfile({ ...current, pending: remaining });
    pending = await createPending();
    registered = await register(pending);
  }
  if (credentials !== auth || activeProfile !== profileID(auth)) throw new Error("Authorization changed during Workspace device registration");
  const binding: LocalWorkspaceSyncBinding = {
    bindingID: registered.id, folderID: registered.folderID, deviceID: registered.deviceID,
    serverDeviceID: registered.serverDeviceID, relayAddress: registered.relayAddress, state: registered.state, enabled: false,
  };
  const current = workspaceSyncProfile!;
  const { [workspaceID]: _registered, ...remaining } = current.pending;
  saveWorkspaceSyncProfile({ ...current, bindings: { ...current.bindings, [workspaceID]: binding }, pending: remaining });
  return binding;
}

async function configureWorkspaceNode(): Promise<void> {
  try { await workspaceNode.configureWorkspaces(await workspaceSyncManagedFolders()); }
  catch (error) {
    if (!await workspaceNode.stop()) throw new Error("Workspace sync configuration failed and node shutdown was not confirmed; folders remain protected");
    throw error;
  }
}

async function restoreWorkspaceNode(auth: Credentials): Promise<void> {
  if (!workspaceSyncProfile || workspaceSyncProfileError || !Object.values(workspaceSyncProfile.bindings).some((binding) => binding.enabled) || workspaceSyncOperationInProgress) return;
  workspaceSyncOperationInProgress = true;
  try {
    await startWorkspaceNode(auth);
    await configureWorkspaceNode();
    workspaceSyncError = null;
  } catch (error) {
    workspaceSyncError = error instanceof Error ? error.message : "Could not restore Workspace sync";
    if (!await workspaceNode.stop()) workspaceSyncError = "本机 Workspace 节点启动失败且停止状态未确认；目录仍受保护。";
  } finally {
    workspaceSyncOperationInProgress = false;
    notifyState();
  }
}

async function setWorkspaceSyncEnabled(workspaceIDInput: unknown, enabled: unknown): Promise<unknown> {
  const workspaceID = checkedId("workspace", workspaceIDInput);
  if (typeof enabled !== "boolean") throw new Error("Workspace sync switch must be on or off");
  const auth = credentials;
  if (!auth || !workspaceSyncProfile || workspaceSyncProfileError || activeProfile !== profileID(auth)) {
    throw new Error(workspaceSyncProfileError ?? "Log in to Morde before enabling Workspace sync");
  }
  if (workspaceSyncOperationInProgress || preservedOperationInProgress || directorySelectionInProgress || projectDirectorySelectionInProgress || projectUploadInProgress) throw new Error("Another local Workspace operation is in progress");
  if (activeLiveSync(workspaceID) || enabled && (liveSyncRecords[workspaceID]?.recoveryRequired || liveSyncRecords[workspaceID]?.state === "conflict")) throw new Error("Stop or resolve this Workspace's live sync before changing its sync binding");
  if (sync.snapshot() !== "idle" || syncCheckpoint || childMayStillRun(syncCheckpoint)) throw new Error("Resolve the prior sync recovery point before enabling Workspace sync");
  const matches = mappings.filter((item) => item.workspaceId === workspaceID);
  if (matches.length !== 1) throw new Error("Choose exactly one local folder for this Workspace first");
  const mapping = matches[0]!;
  await confirmWorkspaceMapping(mapping);
  if (enabled) {
    const workspace = await api.workspace(workspaceID) as { state?: string };
    if (credentials !== auth || workspace.state !== "ready") throw new Error("Workspace is not ready for sync");
  }

  workspaceSyncOperationInProgress = true;
  try {
    let binding = workspaceSyncProfile.bindings[workspaceID];
    if (enabled) {
      const identity = await startWorkspaceNode(auth);
      binding ??= await registerWorkspaceSyncBinding(auth, workspaceID, identity.deviceID);
      if (binding.deviceID !== identity.deviceID || binding.state === "revoking" || binding.state === "revoked") {
        throw new Error("This Workspace device binding is no longer active; register it again in Morde");
      }
      await configureWorkspaceNode();
      const current = workspaceSyncProfile!;
      saveWorkspaceSyncProfile({ ...current, bindings: { ...current.bindings, [workspaceID]: { ...binding, enabled: true } } });
      try { await configureWorkspaceNode(); }
      catch (error) {
        const latest = workspaceSyncProfile!;
        saveWorkspaceSyncProfile({ ...latest, bindings: { ...latest.bindings, [workspaceID]: { ...latest.bindings[workspaceID]!, enabled: false } } });
        try { await configureWorkspaceNode(); }
        catch { await workspaceNode.stop(); }
        throw error;
      }
    } else if (binding) {
      const current = workspaceSyncProfile;
      saveWorkspaceSyncProfile({ ...current, bindings: { ...current.bindings, [workspaceID]: { ...binding, enabled: false } } });
      if (workspaceNode.isRunning()) await configureWorkspaceNode();
      if (!Object.values(workspaceSyncProfile!.bindings).some((item) => item.enabled) && !await workspaceNode.stop()) {
        throw new Error("Workspace sync node shutdown was not confirmed; its folder remains protected");
      }
    } else if (workspaceSyncProfile.pending[workspaceID]) {
      const current = workspaceSyncProfile;
      const { [workspaceID]: _pending, ...remaining } = current.pending;
      saveWorkspaceSyncProfile({ ...current, pending: remaining });
    }
    if (credentials !== auth || activeProfile !== profileID(auth)) throw new Error("Authorization changed during Workspace sync configuration");
    workspaceSyncError = null;
    return publicState();
  } catch (error) {
    workspaceSyncError = error instanceof Error ? error.message : "Workspace sync configuration failed";
    if (!Object.values(workspaceSyncProfile?.bindings ?? {}).some((item) => item.enabled)) await workspaceNode.stop();
    throw error;
  } finally {
    workspaceSyncOperationInProgress = false;
    notifyState();
  }
}

function loadState(): void {
  loadClientLoginAttempts();
  try { lastOrigin = portalOrigin(readFileSync(settingsPath("service-origin.txt"), "utf8").trim()); } catch { lastOrigin = null; }
  try {
    const saved = JSON.parse(readFileSync(settingsPath("installation.json"), "utf8")) as typeof lastInstallation;
    if (saved && saved.origin === portalOrigin(saved.origin) && (saved.serviceInstanceID === undefined || typeof saved.serviceInstanceID === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(saved.serviceInstanceID)) && typeof saved.name === "string" && saved.name && /^i[a-z0-9]{7}$/.test(saved.id)) lastInstallation = saved;
  } catch { lastInstallation = null; }
  try {
    if (safeStorageReady() && existsSync(settingsPath("credentials.bin"))) {
      const saved = JSON.parse(safeStorage.decryptString(readFileSync(settingsPath("credentials.bin")))) as { version?: number; credentials?: Credentials };
      const auth = saved.version === 2 ? saved.credentials : null;
      if (auth && typeof auth.serviceInstanceID === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(auth.serviceInstanceID) && /^i[a-z0-9]{7}$/.test(auth.installationId) && typeof auth.clientCredential === "string" && auth.clientCredential && !credentialExpired(auth.clientExpiresAt)) {
        auth.origin = portalOrigin(auth.origin);
        if (!auth.accountName && lastInstallation?.origin === auth.origin && lastInstallation.id === auth.installationId) auth.accountName = lastInstallation.name;
        if (typeof auth.accountName !== "string" || !auth.accountName) throw new Error("Local account identity is missing");
        credentials = auth;
        lastOrigin = auth.origin;
      } else rmSync(settingsPath("credentials.bin"), { force: true });
    }
  } catch { credentials = null; }
  if (credentials) loadProfile(credentials);
}

function publicState(): unknown {
  if (credentials && credentialExpired(credentials.clientExpiresAt)) clearAuthorization();
  return {
    authenticated: Boolean(credentials), accountName: credentials?.accountName ?? null, origin: credentials?.origin ?? lastOrigin,
    installationId: credentials?.installationId ?? null, expiresAt: credentials?.clientExpiresAt ?? null,
    expiresSoon: credentials ? credentialExpiresSoon(credentials.clientExpiresAt) : false,
    mappings: credentials ? mappings.map(publicMapping) : [], projectMappings: credentials ? projectMappings.map(publicProjectMapping) : [], syncState: sync.snapshot(),
    syncCheckpoint: credentials && syncCheckpoint ? { workspaceId: syncCheckpoint.workspaceId, mode: syncCheckpoint.mode, installationId: syncCheckpoint.installationId, scope: syncCheckpoint.scope,
      ...(syncCheckpoint.projectSelectionID ? { projectSelectionID: syncCheckpoint.projectSelectionID, projectName: syncCheckpoint.projectName } : {}),
      ...(syncCheckpoint.handoffTarget ? { handoffTarget: syncCheckpoint.handoffTarget } : {}) } : null,
    syncRecovery: credentials ? syncRecoveryView() : null,
    liveSyncCapability: credentials ? liveSyncCapability : "disabled",
    liveSyncCapabilityMessage: credentials ? liveSyncCapabilityMessage : "",
    liveSyncs: credentials ? publicLiveSyncs() : {},
    handoff: credentials ? publicHandoff(handoffRecord) : null,
    profileError: credentials ? profileError : null,
    workspaceSync: credentials ? {
      nodeState: workspaceNode.isRunning() ? "running" : workspaceNode.mayOwnWorkspace() ? "unknown" : "stopped",
      error: workspaceSyncProfileError || workspaceSyncError ? redactPrivateValues(workspaceSyncProfileError ?? workspaceSyncError ?? "") : null,
      bindings: Object.fromEntries(Object.entries(workspaceSyncProfile?.bindings ?? {}).map(([id, binding]) => [id, { enabled: binding.enabled, state: binding.state }])),
    } : null,
    storageReady: safeStorageReady(), view,
  };
}

function notifyState(): void {
  const state = publicState();
  mainWindow?.webContents.send("morde:state", state);
  const auth = credentials;
  if (auth && webView && isTrustedPortalBridgePage(webView.webContents.getURL(), auth.origin)) {
    webView.webContents.send("morde:portal-state", publicPortalProjectState());
  }
}
function clearTimer(timer: NodeJS.Timeout | null): null { if (timer) clearTimeout(timer); return null; }
function closeHostWindows(): void { for (const window of hostWindows) window.close(); }

type HostProjectUploadContext = { host: Record<string, unknown>; workspaceBinding: Record<string, unknown> };

async function checkedOwnedHostWorkspace(auth: Credentials, hostIDInput: unknown, workspaceIDInput: unknown): Promise<HostProjectUploadContext> {
  const hostID = checkedId("host", hostIDInput);
  const workspaceID = checkedId("workspace", workspaceIDInput);
  const [meValue, hostValue] = await Promise.all([api.me(), api.host(hostID)]) as [Record<string, unknown>, Record<string, unknown>];
  const workspaces = hostValue?.workspaces;
  if (credentials !== auth || activeProfile !== profileID(auth)) throw new Error("Authorization changed while checking the cloud computer");
  if (!meValue || typeof meValue.id !== "string" || hostValue?.id !== hostID || hostValue.owner_id !== meValue.id ||
    !["running", "stopped"].includes(String(hostValue.state)) || !Array.isArray(workspaces) || workspaces.length !== 1) {
    throw new Error("请选择自己拥有且已绑定工作区的云端电脑");
  }
  const binding = workspaces[0] as Record<string, unknown> | null;
  const workspace = binding?.workspace as Record<string, unknown> | null;
  if (!binding || binding.state !== "active" || !workspace || workspace.id !== workspaceID || workspace.state !== "ready") {
    throw new Error("云端电脑的工作区未就绪或与所选目标不匹配");
  }
  return { host: hostValue, workspaceBinding: binding };
}

async function listProjectUploadTargets(): Promise<ProjectTarget[]> {
  const auth = credentials;
  if (!auth || !activeProfile || activeProfile !== profileID(auth)) throw new Error("请先登录 Morde");
  if (profileError) throw new Error(profileError);
  await api.requireProjectUploadCapability();
  const [meValue, hostsValue, portalSession] = await Promise.all([api.me(), api.hosts(), api.webSession()]) as [Record<string, unknown>, { items?: unknown }, WebSession];
  if (credentials !== auth || activeProfile !== profileID(auth)) throw new Error("Authorization changed while loading cloud computers");
  if (!meValue || typeof meValue.id !== "string" || !hostsValue || !Array.isArray(hostsValue.items)) throw new Error("云端电脑列表无效");
  const targets: ProjectTarget[] = [];
  for (const rawHost of hostsValue.items) {
    const host = rawHost as Record<string, unknown> | null;
    if (!host || typeof host.id !== "string" || !/^h[a-z0-9]{7}$/.test(host.id) || host.owner_id !== meValue.id ||
      !["running", "stopped"].includes(String(host.state)) || !Array.isArray(host.workspaces) || host.workspaces.length !== 1) continue;
    const binding = host.workspaces[0] as Record<string, unknown> | null;
    const workspace = binding?.workspace as Record<string, unknown> | null;
    if (!binding || binding.state !== "active" || !workspace || typeof workspace.id !== "string" ||
      !/^w[a-z0-9]{7}$/.test(workspace.id) || workspace.state !== "ready") continue;
    const folders = await api.hostProjectFolders(host.id, portalSession);
    if (credentials !== auth || activeProfile !== profileID(auth)) throw new Error("Authorization changed while loading project folders");
    const rootFolder = folders.find((folder) => folder.folderKind === "workspace_root" && folder.state !== "revoked");
    const projectFolders = folders.filter((folder) => folder.folderKind === "project").map((folder) => ({
      folderID: folder.folderID, projectName: folder.projectName!, relativePath: folder.relativePath, state: folder.state,
    }));
    targets.push({
      hostId: host.id, hostName: typeof host.name === "string" && host.name ? host.name : host.id,
      hostState: host.state as "running" | "stopped",
      workspaceId: workspace.id, workspaceName: typeof workspace.name === "string" && workspace.name ? workspace.name : workspace.id,
      folders: projectFolders,
      ...(rootFolder ? { blockedMessage: "此云端工作区仍登记为整个工作区根目录，不能同时登记项目目录。" } : {}),
    });
  }
  return targets;
}

async function selectProjectDirectory(hostIDInput: unknown, workspaceIDInput: unknown): Promise<unknown> {
  if (preservedOperationInProgress || workspaceSyncOperationInProgress || projectDirectorySelectionInProgress || projectUploadInProgress) throw new Error("当前有本机文件操作正在运行");
  if (sync.snapshot() !== "idle" || syncCheckpoint || childMayStillRun(syncCheckpoint)) throw new Error("先完成或处理现有同步恢复点");
  const auth = credentials;
  if (!auth || !activeProfile || activeProfile !== profileID(auth)) throw new Error("请先登录 Morde");
  if (profileError) throw new Error(profileError);
  const hostID = checkedId("host", hostIDInput), workspaceID = checkedId("workspace", workspaceIDInput);
  await api.requireProjectUploadCapability();
  await checkedOwnedHostWorkspace(auth, hostID, workspaceID);
  const portalSession = await api.webSession();
  const folders = await api.hostProjectFolders(hostID, portalSession);
  if (folders.some((folder) => folder.folderKind === "workspace_root" && folder.state !== "revoked")) {
    throw new Error("此云端工作区仍使用整个工作区根目录同步，暂不能登记独立项目目录");
  }
  if (workspaceSyncProfile?.bindings[workspaceID]?.enabled) throw new Error("先暂停此工作区已有的固定同步绑定");
  if (directorySelectionInProgress) throw new Error("另一项本机目录选择正在进行");
  projectDirectorySelectionInProgress = true;
  try {
    const picked = await dialog.showOpenDialog(mainWindow!, { properties: ["openDirectory"] });
    if (picked.canceled || !picked.filePaths[0]) return null;
    const projectName = normalizedProjectName(basename(picked.filePaths[0].replace(/\\/g, "/").replace(/\/+$/, "")));
    const previous = projectMappings.find((item) => item.hostId === hostID && item.workspaceId === workspaceID && item.projectName === projectName);
    const candidate = await checkedProjectDirectory(picked.filePaths[0], workspaceID, previous?.selectionID);
    if (credentials !== auth || sync.snapshot() !== "idle" || syncCheckpoint) throw new Error("本机授权或同步状态已变化，请重新选择");
    await assertNoLiveSyncForDirectory(candidate.directory, workspaceID);
    const mapping: ProjectMapping = {
      selectionID: previous?.selectionID ?? randomUUID(), hostId: hostID, workspaceId: workspaceID, projectName,
      directory: candidate.directory, dev: candidate.dev, ino: candidate.ino,
      ...(previous?.folderId ? { folderId: previous.folderId } : {}),
      registrationKey: previous?.registrationKey ?? randomUUID(),
    };
    const next = [...projectMappings.filter((item) => item.selectionID !== mapping.selectionID), mapping];
    savePrivate(profileFile("project-mappings"), JSON.stringify(next));
    projectMappings = next;
    notifyState();
    return publicProjectMapping(mapping);
  } finally { projectDirectorySelectionInProgress = false; }
}

async function ensureProjectFolder(auth: Credentials, mapping: ProjectMapping, projectName: string): Promise<ProjectSyncFolder> {
  const { host } = await checkedOwnedHostWorkspace(auth, mapping.hostId, mapping.workspaceId);
  if (credentials !== auth) throw new Error("Authorization changed before project folder registration");
  const session = await api.webSession();
  const folders = await api.hostProjectFolders(mapping.hostId, session);
  if (folders.some((folder) => folder.folderKind === "workspace_root" && folder.state !== "revoked")) {
    throw new Error("目标工作区仍登记为整体同步目录；项目目录同步已停止");
  }
  let folder: ProjectSyncFolder | undefined;
  if (mapping.folderId) {
    folder = folders.find((item) => item.folderID === mapping.folderId);
    if (!folder || folder.folderKind !== "project" || folder.workspaceID !== mapping.workspaceId || folder.projectName !== projectName ||
      folder.relativePath !== "project/" + projectName || folder.state === "revoked" || folder.syncAvailable) {
      throw new Error("登记的项目目录已变化或撤销；请检查云端目录后重新选择");
    }
  } else {
    const matching = folders.filter((item) => item.folderKind === "project" && item.projectName === projectName);
    if (matching.some((item) => item.state === "revoked")) throw new Error("同名云端项目目录已撤销，请先在云端处理该目录");
    folder = matching.find((item) => item.state === "active" || item.state === "pending_node");
    if (!folder) folder = await api.registerHostProjectFolder(mapping.hostId, projectName, session, mapping.registrationKey);
    if (folder.workspaceID !== mapping.workspaceId || folder.folderKind !== "project" || folder.projectName !== projectName ||
      folder.relativePath !== "project/" + projectName || folder.state === "revoked" || folder.syncAvailable) {
      throw new Error("云端返回的项目目录与所选电脑不匹配");
    }
  }
  if (credentials !== auth || host.id !== mapping.hostId) throw new Error("Authorization changed while registering the project folder");
  const updated = { ...mapping, projectName, folderId: folder.folderID };
  const next = projectMappings.map((item) => item.selectionID === mapping.selectionID ? updated : item);
  savePrivate(profileFile("project-mappings"), JSON.stringify(next));
  projectMappings = next;
  return folder;
}

async function projectHostApplyStatus(auth: Credentials, hostID: string, workspaceID: string): Promise<{ state: "applied" | "pending_apply"; desiredRevision?: number; appliedRevision?: number }> {
  try {
    const host = await api.host(hostID) as Record<string, unknown>;
    if (credentials !== auth || host.id !== hostID || !Array.isArray(host.workspaces)) return { state: "pending_apply" };
    const binding = host.workspaces.map((item) => item as Record<string, unknown>).find((item) => (item.workspace as Record<string, unknown> | undefined)?.id === workspaceID);
    const desired = binding?.desired_revision, applied = binding?.applied_revision;
    const valid = Number.isSafeInteger(desired) && Number(desired) >= 0 && Number.isSafeInteger(applied) && Number(applied) >= 0;
    return {
      state: host.state === "running" && binding?.state === "active" && valid && Number(applied) >= Number(desired) ? "applied" : "pending_apply",
      ...(valid ? { desiredRevision: Number(desired), appliedRevision: Number(applied) } : {}),
    };
  } catch { return { state: "pending_apply" }; }
}

async function uploadProject(selectionIDInput: unknown, projectNameInput: unknown): Promise<unknown> {
  if (projectUploadInProgress) throw new Error("A project upload is already being prepared or transferred");
  projectUploadInProgress = true;
  try { return await performProjectUpload(selectionIDInput, projectNameInput); }
  finally { projectUploadInProgress = false; }
}

async function performProjectUpload(selectionIDInput: unknown, projectNameInput: unknown): Promise<unknown> {
  if (preservedOperationInProgress || directorySelectionInProgress || projectDirectorySelectionInProgress || workspaceSyncOperationInProgress) throw new Error("Finish the local file operation before uploading a project");
  if (profileError) throw new Error(profileError);
  const selectionID = typeof selectionIDInput === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(selectionIDInput)
    ? selectionIDInput : "";
  const projectName = normalizedProjectName(projectNameInput);
  const auth = credentials;
  const mapping = projectMappings.find((item) => item.selectionID === selectionID);
  if (!auth || !activeProfile || activeProfile !== profileID(auth) || !mapping) throw new Error("先选择本机项目文件夹");
  if (mapping.folderId && mapping.projectName !== projectName) throw new Error("项目名已绑定到云端目录；如需更名，请重新选择文件夹并创建新项目目录");
  if (sync.snapshot() !== "idle" || childMayStillRun(syncCheckpoint)) throw new Error("先停止并核对已有的同步操作");
  if (syncCheckpoint && (syncCheckpoint.mode !== "project_upload" || syncCheckpoint.projectSelectionID !== selectionID ||
    syncCheckpoint.hostId !== mapping.hostId || syncCheckpoint.folderId !== mapping.folderId || syncCheckpoint.projectName !== projectName ||
    syncCheckpoint.workspaceId !== mapping.workspaceId || syncCheckpoint.directory !== mapping.directory ||
    syncCheckpoint.dev !== mapping.dev || syncCheckpoint.ino !== mapping.ino)) {
    throw new Error("另一个同步恢复点仍待处理，请先恢复或明确放弃");
  }
  await api.requireProjectUploadCapability();
  const confirmation = await dialog.showMessageBox(mainWindow!, {
    type: "warning", buttons: ["上传项目", "取消"], defaultId: 1, cancelId: 1,
    title: "单向上传项目文件夹",
    message: "将“" + publicDirectoryName(mapping.directory) + "”上传到“" + projectName + "”",
    detail: "目标：~/workspace/project/" + projectName + "\n范围：上传所选本机文件夹的全部文件和子目录，并保留相对路径。只从本机上传，云端变化不会下载到本机。继续后，只有上次成功同步的内容才可能按同步基线处理删除与冲突。",
    noLink: true,
  });
  if (confirmation.response !== 0) return { cancelled: true };
  if (credentials !== auth) throw new Error("Authorization changed before project upload");
  await confirmProjectMapping(mapping);
  await assertNoLiveSyncForDirectory(mapping.directory, mapping.workspaceId);
  if (workspaceSyncProfile?.bindings[mapping.workspaceId]?.enabled) throw new Error("先暂停此工作区已有的固定同步绑定");
  const folder = await ensureProjectFolder(auth, mapping, projectName);
  const currentMapping = projectMappings.find((item) => item.selectionID === selectionID);
  if (!currentMapping || currentMapping.folderId !== folder.folderID) throw new Error("本机项目目录映射保存状态未确认");
  const checked = await checkedProjectDirectory(currentMapping.directory, currentMapping.workspaceId, currentMapping.selectionID);
  if (checked.directory !== currentMapping.directory || checked.dev !== currentMapping.dev || checked.ino !== currentMapping.ino) throw new Error("项目文件夹身份已变化，请重新选择");
  await checkedOwnedHostWorkspace(auth, currentMapping.hostId, currentMapping.workspaceId);
  if (syncCheckpoint && syncCheckpoint.folderId !== folder.folderID) throw new Error("当前恢复点属于其他项目目录");
  if (!syncCheckpoint) {
    syncCheckpoint = {
      serviceInstanceID: auth.serviceInstanceID, installationId: auth.installationId, workspaceId: currentMapping.workspaceId,
      directory: currentMapping.directory, dev: currentMapping.dev, ino: currentMapping.ino, mode: "project_upload", scope: [],
      projectSelectionID: selectionID, hostId: currentMapping.hostId, folderId: folder.folderID, projectName,
      idempotencyKey: randomUUID(),
    };
    try { savePrivate(profileFile("sync-checkpoint"), JSON.stringify(syncCheckpoint)); }
    catch {
      syncRecoveryWarning = { state: "failed", workspaceId: currentMapping.workspaceId, mode: "project_upload", message: "本机恢复点未能保存；上传未启动。" };
      syncCheckpoint = null;
      notifyState();
      throw new Error("Could not save the project upload recovery point; upload was not started");
    }
    syncRecoveryWarning = null;
  }
  let result: unknown;
  try {
    result = await sync.run(auth, projectAsMapping(currentMapping), "project_upload", [], syncCheckpoint,
      () => credentials === auth && !logoutInProgress,
      () => confirmProjectMapping(currentMapping));
  } finally { if (sync.snapshot() === "idle" && !childMayStillRun(syncCheckpoint)) clearConfirmedChild(); }
  const outcome = result && typeof result === "object" ? result as { session?: { id?: unknown; workspace_id?: unknown; folder_id?: unknown; state?: unknown }; conflicts?: unknown } : null;
  const session = outcome?.session;
  const conflicts = publicSyncConflicts(outcome?.conflicts);
  if (!session || typeof session.id !== "string" || !/^p[a-z0-9]{7}$/.test(session.id) || session.workspace_id !== currentMapping.workspaceId ||
    session.folder_id !== folder.folderID || !["running", "completed", "conflict", "cancelled", "failed", "expired"].includes(String(session.state)) || conflicts === null) {
    throw new Error("Project upload result is unknown; the recovery point was retained");
  }
  if (session.state === "completed" && conflicts.length === 0) {
    removeSyncCheckpointFile();
    syncCheckpoint = null;
    syncRecoveryWarning = null;
    notifyState();
    const hostStatus = await projectHostApplyStatus(auth, currentMapping.hostId, currentMapping.workspaceId);
    return {
      session: { id: session.id, workspace_id: currentMapping.workspaceId, folder_id: folder.folderID, state: "completed" }, conflicts,
      cloudStatus: "confirmed", hostStatus: hostStatus.state,
      ...(hostStatus.desiredRevision !== undefined ? { desiredRevision: hostStatus.desiredRevision, appliedRevision: hostStatus.appliedRevision } : {}),
    };
  }
  if (session.state !== "running") {
    // morde-sync removes local session metadata after a terminal response. Keep
    // only non-terminal checkpoints so the UI never offers an impossible resume.
    removeSyncCheckpointFile();
    syncCheckpoint = null;
    syncRecoveryWarning = null;
    notifyState();
  }
  return { session: { id: session.id, workspace_id: currentMapping.workspaceId, folder_id: folder.folderID, state: String(session.state) }, conflicts, cloudStatus: "pending" };
}

function detachWeb(): void {
  closeHostWindows();
  if (webView) {
    if (mainWindow && !mainWindow.isDestroyed() && webAttached) mainWindow.contentView.removeChildView(webView);
    webAttached = false;
    if (!webView.webContents.isDestroyed()) webView.webContents.close();
    webView = null;
  }
  if (webPartition) void webPartition.clearStorageData({ storages: ["cookies", "localstorage"] });
  webPartition = null;
  webRefreshTimer = clearTimer(webRefreshTimer);
}

function clearAuthorization(stopRuntime = true): void {
  if (!credentials) return;
  if (stopRuntime) {
    for (const [workspaceID, runner] of liveSyncRunners) {
      const record = liveSyncRecords[workspaceID];
      if (!record || !activeLiveSync(workspaceID)) continue;
      liveSyncStopping.add(workspaceID);
      record.state = "persisted_uncertain";
      record.recoveryRequired = true;
      record.message = "本机授权结束时实时同步停止状态待核对；恢复点已保留。";
      try { saveLiveSyncRecords(); } catch { /* The prior durable checkpoint remains and is treated as uncertain on next login. */ }
      void runner.stop().finally(() => {
        liveSyncStopping.delete(workspaceID);
        if (runner.snapshot() === "idle") liveSyncRunners.delete(workspaceID);
        notifyState();
      }).catch(() => undefined);
    }
  }
  credentials = null;
  liveSyncCapability = "unknown";
  detachWeb();
  credentialExpiryTimer = clearTimer(credentialExpiryTimer);
  authorizationCheckTimer = clearTimer(authorizationCheckTimer);
  rmSync(settingsPath("credentials.bin"), { force: true });
  if (stopRuntime) {
    void sync.stop().then((stopped) => { if (stopped) clearConfirmedChild(); }).catch(() => undefined);
    void workspaceNode.stop().then((stopped) => {
      if (!stopped) workspaceSyncError = "本机 Workspace 节点停止状态未确认，目录仍受保护。";
      notifyState();
    }).catch(() => { workspaceSyncError = "本机 Workspace 节点停止状态未确认，目录仍受保护。"; notifyState(); });
  }
  notifyState();
}

function scheduleCredentialExpiry(): void {
  credentialExpiryTimer = clearTimer(credentialExpiryTimer);
  const auth = credentials;
  if (!auth) return;
  const expiry = Date.parse(auth.clientExpiresAt);
  if (!Number.isFinite(expiry) || expiry <= Date.now()) { clearAuthorization(); return; }
  credentialExpiryTimer = setTimeout(() => {
    if (credentials !== auth) return;
    if (credentialExpired(auth.clientExpiresAt)) clearAuthorization();
    else scheduleCredentialExpiry();
  }, Math.min(expiry - Date.now(), 2_147_483_647));
  credentialExpiryTimer.unref();
}

function scheduleAuthorizationCheck(): void {
  authorizationCheckTimer = clearTimer(authorizationCheckTimer);
  const auth = credentials;
  if (!auth) return;
  authorizationCheckTimer = setTimeout(() => {
    void api.me().catch((error: unknown) => { if (credentials === auth && String(error).includes("Authorization expired or revoked")) clearAuthorization(); })
      .finally(() => { if (credentials === auth) scheduleAuthorizationCheck(); });
  }, 60_000);
  authorizationCheckTimer.unref();
}

function sameOriginURL(value: string, origin: string): URL | null {
  try { const url = new URL(value); return url.origin === origin && !url.username && !url.password ? url : null; } catch { return null; }
}

function isTrustedPortalBridgePage(value: string, origin: string): boolean {
  const url = sameOriginURL(value, origin);
  if (!url || !["/hosts", "/workspaces"].includes(url.pathname)) return false;
  if (url.pathname === "/hosts") {
    return !url.search && (!url.hash || /^#host-h[a-z0-9]{7}$/.test(url.hash));
  }
  return !url.hash && [...url.searchParams.keys()].every((key) => key === "host") && url.searchParams.getAll("host").length <= 1 &&
    (!url.searchParams.has("host") || /^h[a-z0-9]{7}$/.test(url.searchParams.get("host") ?? ""));
}

function isTrustedHostWorkbenchPage(value: string, origin: string): boolean {
  const url = sameOriginURL(value, origin);
  if (!url || url.pathname !== "/__ch/wb/" || url.hash) return false;
  return [...url.searchParams.keys()].every((key) => key === "tab") && url.searchParams.getAll("tab").length <= 1 &&
    (!url.searchParams.has("tab") || url.searchParams.get("tab") === "workspace");
}

function preloadArguments(mode: "portal" | "host-workbench", inherited?: string[]): string[] {
  return [...(inherited ?? []).filter((argument) => !argument.startsWith("--morde-preload-mode=")), `--morde-preload-mode=${mode}`];
}

function trustedPortalBridgeEvent(event: { sender: WebContents; senderFrame: { url: string } | null }, auth: Credentials): boolean {
  const contents = webView?.webContents;
  return Boolean(contents && !contents.isDestroyed() && credentials === auth && event.sender === contents &&
    event.senderFrame === event.sender.mainFrame && isTrustedPortalBridgePage(event.sender.getURL(), auth.origin));
}

const documentLoadOptions = { extraHeaders: "Accept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8" };
function installDocumentErrorRecovery(partition: Session, origin?: string): (contents: WebContents, fallbackURL: string, fallbackLabel: string) => (value: string) => boolean {
  type RecoveryPage = { contents: WebContents; fallbackURL: string; fallbackLabel: string; recoveryPageURL: string | null; blockedDocumentURL: string | null };
  const pages = new Map<number, RecoveryPage>();
  partition.webRequest.onHeadersReceived({ urls: origin ? [`${origin}/*`] : ["<all_urls>"] }, (details, callback) => {
    const contentType = Object.entries(details.responseHeaders ?? {})
      .find(([name]) => name.toLowerCase() === "content-type")?.[1].join(";").toLowerCase() ?? "";
    const isJSONDocument = contentType.includes("application/json") || contentType.includes("+json");
    const page = details.webContentsId === undefined
      ? [...pages.values()].find((candidate) => details.webContents === candidate.contents)
      : pages.get(details.webContentsId);
    if (page && details.resourceType === "mainFrame" && (details.statusCode >= 400 || isJSONDocument)) {
      page.blockedDocumentURL = details.url;
      callback({ cancel: true });
      return;
    }
    callback({});
  });
  return (contents, fallbackURL, fallbackLabel) => {
    const page: RecoveryPage = { contents, fallbackURL, fallbackLabel, recoveryPageURL: null, blockedDocumentURL: null };
    pages.set(contents.id, page);
    contents.on("destroyed", () => pages.delete(contents.id));
    const showRecoveryPage = () => {
      if (contents.isDestroyed()) return;
      const scriptString = (value: string) => JSON.stringify(value).replace(/</g, "\\u003c");
      const html = `<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Morde</title><style>*{box-sizing:border-box}body{min-height:100vh;margin:0;padding:24px;display:grid;place-items:center;background:#f5f7fb;color:#1b2932;font:16px/1.6 Inter,system-ui,-apple-system,"Segoe UI",sans-serif}main{width:min(560px,100%);padding:44px 32px;border:1px solid #e3e9ed;border-radius:18px;background:#fff;box-shadow:0 14px 46px #24425417;text-align:center}.mark{width:48px;height:48px;margin:0 auto 18px;display:grid;place-items:center;border:1px solid #f1c9c5;border-radius:50%;color:#b42318;background:#fff4f2;font-size:24px;font-weight:750}h1{margin:0 0 10px;font-size:24px;line-height:1.3}p{margin:0 auto 24px;color:#637784}.actions{display:flex;justify-content:center;flex-wrap:wrap;gap:10px}button{min-height:38px;padding:8px 14px;border:1px solid #c5d0dc;border-radius:9px;background:#fff;color:#34465c;font:600 14px/1.2 Inter,system-ui,sans-serif;cursor:pointer}button.primary{border-color:#2e74a6;background:#2e74a6;color:#fff}</style><body><main role="alert"><div class="mark" aria-hidden="true">!</div><h1>页面暂时无法显示</h1><p>服务没有返回可显示的页面。你可以返回上一页，或回到安全页面继续使用。</p><div class="actions"><button id="back">返回上一页</button><button id="home" class="primary">${fallbackLabel}</button></div></main><script>const fallbackURL=${scriptString(fallbackURL)};document.getElementById('back').onclick=()=>history.length>1?history.back():location.replace(fallbackURL);document.getElementById('home').onclick=()=>location.replace(fallbackURL);</script></body></html>`;
      page.recoveryPageURL = `data:text/html;charset=utf-8,${encodeURIComponent(html)}`;
      void contents.loadURL(page.recoveryPageURL).catch(() => undefined);
    };
    contents.on("did-fail-load", (_event, errorCode, _description, failedURL, isMainFrame) => {
      if (!isMainFrame) return;
      if (page.blockedDocumentURL === failedURL) { page.blockedDocumentURL = null; showRecoveryPage(); return; }
      if (errorCode !== -3 && (origin ? sameOriginURL(failedURL, origin) : /^https?:\/\//i.test(failedURL))) showRecoveryPage();
    });
    return (value) => value === page.recoveryPageURL;
  };
}
function hostEntryURL(value: string, origin: string): { id: string; target: string | null } | null {
  const url = sameOriginURL(value, origin);
  if (!url || url.hash || !/^\/host-entry\/h[a-z0-9]{7}$/.test(url.pathname) || [...url.searchParams.keys()].some((key) => key !== "target") || url.searchParams.getAll("target").length > 1) return null;
  const target = url.searchParams.get("target");
  if (target && target !== "desktop" && target !== "terminal") return null;
  return { id: url.pathname.slice("/host-entry/".length), target };
}

function taskObservationTarget(value: string, serviceOrigin: string): { id: string; url: URL } | null {
  let url: URL;
  try { url = new URL(value); } catch { return null; }
  const id = url.hostname.split(".")[0] ?? "";
  const ticket = url.searchParams.get("ticket");
  if (!/^h[a-z0-9]{7}$/.test(id) || (url.protocol !== "https:" && !localGatewayOrigin(url, serviceOrigin)) || url.username || url.password || url.hash ||
    url.pathname !== "/__ch/task-auth" || url.searchParams.getAll("ticket").length !== 1 || !/^[A-Za-z0-9_-]{40,128}$/.test(ticket ?? "") ||
    [...url.searchParams.keys()].some((key) => key !== "ticket")) return null;
  return { id, url };
}

function taskObservationPage(value: string, origin: string): boolean {
  const url = sameOriginURL(value, origin);
  if (!url || url.hash) return false;
  if (url.pathname === "/__ch/task-auth") {
    const ticket = url.searchParams.get("ticket");
    return url.searchParams.getAll("ticket").length === 1 && /^[A-Za-z0-9_-]{40,128}$/.test(ticket ?? "") &&
      [...url.searchParams.keys()].every((key) => key === "ticket");
  }
  if (url.pathname !== "/__ch/wb/") return false;
  const taskID = url.searchParams.get("task_id");
  return url.searchParams.getAll("task_id").length === 1 && /^q[a-z0-9]{7}$/.test(taskID ?? "") &&
    [...url.searchParams.keys()].every((key) => key === "task_id");
}

function guardTaskObservationWindow(win: BrowserWindow, auth: Credentials, isRecoveryPage: (value: string) => boolean): void {
  let allowedGateway: string | null = null;
  let authorizing = false;
  win.webContents.setWindowOpenHandler(() => ({ action: "deny" }));
  win.webContents.on("will-navigate", (event, value) => {
    if (isRecoveryPage(value) || value === "about:blank" || (allowedGateway && taskObservationPage(value, allowedGateway))) return;
    const target = taskObservationTarget(value, auth.origin);
    event.preventDefault();
    if (!target || authorizing || credentials !== auth) {
      if (!win.isDestroyed()) win.close();
      return;
    }
    authorizing = true;
    void hostOrigin(target.id, false, auth.origin).then((expected) => {
      if (expected !== target.url.origin || credentials !== auth || win.isDestroyed()) throw new Error("Task observation Host changed");
      allowedGateway = expected;
      return win.loadURL(target.url.href, documentLoadOptions).catch(() => undefined);
    }).catch(() => { if (!win.isDestroyed()) win.close(); }).finally(() => { authorizing = false; });
  });
  win.webContents.on("will-redirect", (event, value) => {
    if (!allowedGateway || !taskObservationPage(value, allowedGateway)) event.preventDefault();
  });
}

async function hostOrigin(id: string, shared: boolean, serviceOrigin: string): Promise<string> {
  const value = shared ? (await api.sharedHosts() as { items?: Array<{ id: string; url: string }> }).items?.find((item) => item.id === id) : await api.host(id) as { url?: string };
  if (!value?.url) throw new Error("Host is unavailable");
  const url = new URL(value.url);
  if ((url.protocol !== "https:" && !localGatewayOrigin(url, serviceOrigin)) || url.username || url.password) throw new Error("Invalid Host origin");
  return url.origin;
}

function guardHostWindow(win: BrowserWindow, entry: { id: string; target: string | null }, auth: Credentials, isRecoveryPage: (value: string) => boolean): void {
  hostWindows.add(win);
  let allowedGateway: string | null = null;
  const returnToMain = () => {
    if (!win.isDestroyed()) win.close();
    if (credentials === auth && mainWindow && !mainWindow.isDestroyed()) { showView("web"); mainWindow.focus(); }
  };
  hostWorkbenchReturnHandlers.set(win.webContents, () => {
    if (credentials !== auth || !allowedGateway || !isTrustedHostWorkbenchPage(win.webContents.getURL(), allowedGateway)) return false;
    returnToMain();
    return true;
  });
  win.on("closed", () => {
    hostWindows.delete(win);
    hostWorkbenchReturnHandlers.delete(win.webContents);
  });
  win.webContents.setWindowOpenHandler(() => ({ action: "deny" }));
  win.webContents.on("will-navigate", (event, value) => {
    if (isRecoveryPage(value)) return;
    const same = sameOriginURL(value, auth.origin);
    if (same?.pathname === `/host-entry/${entry.id}`) return;
    if (same?.pathname === "/hosts") { event.preventDefault(); returnToMain(); return; }
    try { if (allowedGateway && new URL(value).origin === allowedGateway) return; } catch { /* Reject malformed navigation. */ }
    let target: string;
    try { target = gatewayURL(value, auth.origin); } catch { event.preventDefault(); return; }
    const origin = new URL(target).origin;
    if (allowedGateway === origin && credentials === auth) return;
    event.preventDefault();
    void hostOrigin(entry.id, Boolean(entry.target), auth.origin).then((expected) => {
      if (expected !== origin || credentials !== auth || win.isDestroyed()) throw new Error("Host ticket target changed");
      allowedGateway = origin;
      return win.loadURL(target, documentLoadOptions).catch(() => undefined);
    }).catch((error: unknown) => { if (!win.isDestroyed()) void dialog.showMessageBox(win, { type: "error", message: String(error) }); });
  });
  win.webContents.on("will-redirect", (event, value) => {
    if (isRecoveryPage(value)) return;
    const url = sameOriginURL(value, auth.origin);
    if (url?.pathname === "/hosts") { event.preventDefault(); returnToMain(); return; }
    const portalEntry = url?.pathname === `/host-entry/${entry.id}`;
    try { if (!portalEntry && new URL(value).origin !== allowedGateway) event.preventDefault(); }
    catch { event.preventDefault(); }
  });
  win.webContents.on("did-navigate-in-page", (_event, value) => {
    if (sameOriginURL(value, auth.origin)?.pathname === "/hosts") returnToMain();
  });
}

function installDownloadAndClipboard(partition: Session, auth: Credentials): void {
  partition.on("will-download", (_event, item, contents) => {
    const source = contents.getURL();
    if (!sameOriginURL(source, auth.origin) && ![...hostWindows].some((win) => win.webContents === contents)) { item.cancel(); return; }
    item.setSaveDialogOptions({ title: "保存 Morde 文件" });
  });
  partition.setPermissionRequestHandler((contents, permission, callback) => {
    const trusted = Boolean(sameOriginURL(contents.getURL(), auth.origin)) || [...hostWindows].some((win) => win.webContents === contents);
    callback(trusted && permission === "clipboard-sanitized-write");
  });
}

function positionWeb(): void {
  if (!mainWindow || !webView) return;
  const bounds = mainWindow.getContentBounds();
  webView.setBounds({ x: 0, y: 0, width: bounds.width, height: bounds.height });
}
function showView(next: "web" | "sync"): void {
  view = next;
  if (mainWindow && webView) {
    if (next === "web" && !webAttached) { mainWindow.contentView.addChildView(webView); webAttached = true; positionWeb(); webView.webContents.focus(); }
    if (next === "sync" && webAttached) { mainWindow.contentView.removeChildView(webView); webAttached = false; }
  }
  notifyState();
}

async function refreshWebSession(auth: Credentials, initial?: WebSession): Promise<void> {
  if (credentials !== auth || !webPartition) return;
  try {
    const next = initial ?? await api.webSession();
    if (credentials !== auth || !webPartition) return;
    await webPartition.cookies.set({ url: auth.origin, name: "ch_portal", value: next.credential, path: "/", httpOnly: true, secure: auth.origin.startsWith("https:"), sameSite: "strict", expirationDate: Date.parse(next.expiresAt) / 1000 });
    webRefreshTimer = clearTimer(webRefreshTimer);
    webRefreshTimer = setTimeout(() => { void refreshWebSession(auth).catch(() => undefined); }, Math.max(1000, Math.min(Date.parse(next.expiresAt) - Date.now() - 60_000, 2_147_483_647)));
    webRefreshTimer.unref();
  } catch (error) {
    if (credentials !== auth) return;
    if (String(error).includes("Authorization expired or revoked")) { clearAuthorization(); return; }
    webRefreshTimer = clearTimer(webRefreshTimer);
    webRefreshTimer = setTimeout(() => { void refreshWebSession(auth).catch(() => undefined); }, 30_000);
    webRefreshTimer.unref();
    throw error;
  }
}

async function openWeb(initial: WebSession | undefined, owner: BrowserWindow): Promise<void> {
  const auth = credentials;
  if (!auth) throw new Error("Log in to Morde first");
  if (mainWindow !== owner || owner.isDestroyed()) return;
  const assistantURL = `${auth.origin}/assistant`;
  if (!webPartition) {
    webPartition = session.fromPartition(`morde-user-${randomUUID()}`);
    installDownloadAndClipboard(webPartition, auth);
    webPartition.webRequest.onCompleted({ urls: [`${auth.origin}/api/auth/logout`] }, (details) => {
      if (credentials === auth && details.statusCode === 204 && details.method === "POST") clearAuthorization();
    });
  }
  const partition = webPartition;
  if (!partition) return;
  await refreshWebSession(auth, initial);
  if (credentials !== auth || mainWindow !== owner || owner.isDestroyed() || webPartition !== partition) return;
  if (!webView) {
    const content = new WebContentsView({ webPreferences: {
      session: partition, contextIsolation: true, nodeIntegration: false, sandbox: true, webSecurity: true,
      preload: join(__dirname, "morde-preload.js"), additionalArguments: preloadArguments("portal"),
    } });
    webView = content;
    const registerDocumentRecovery = installDocumentErrorRecovery(partition);
    const isRecoveryPage = registerDocumentRecovery(content.webContents, assistantURL, "返回助手");
    const restoreFromLogin = () => {
      void refreshWebSession(auth).then(() => {
        if (credentials === auth && mainWindow === owner && webPartition === partition && !content.webContents.isDestroyed()) return content.webContents.loadURL(assistantURL, documentLoadOptions).catch(() => undefined);
      }).catch(() => { if (credentials === auth && mainWindow === owner) showView("sync"); });
    };
    content.webContents.setWindowOpenHandler((details) => {
      const entry = hostEntryURL(details.url, auth.origin);
      if (!entry || credentials !== auth) {
        if (details.url === "about:blank" && credentials === auth) return { action: "allow", createWindow: (options) => {
          const win = new BrowserWindow({ ...options, width: 960, height: 720, title: "Morde Task Observation", webPreferences: { ...options.webPreferences, preload: undefined, additionalArguments: [], session: partition, contextIsolation: true, nodeIntegration: false, sandbox: true, webSecurity: true } });
          const isObservationRecoveryPage = registerDocumentRecovery(win.webContents, assistantURL, "返回助手");
          guardTaskObservationWindow(win, auth, isObservationRecoveryPage);
          return win.webContents;
        } };
        const target = sameOriginURL(details.url, auth.origin);
        if (target?.pathname.startsWith("/admin")) void shell.openExternal(target.href);
        return { action: "deny" };
      }
      return { action: "allow", createWindow: (options) => {
        const win = new BrowserWindow({ ...options, width: 1280, height: 800, title: "Morde Host", webPreferences: {
          ...options.webPreferences,
          preload: join(__dirname, "morde-preload.js"),
          additionalArguments: preloadArguments("host-workbench", options.webPreferences?.additionalArguments),
          session: webPartition!, contextIsolation: true, nodeIntegration: false, sandbox: true, webSecurity: true,
        } });
        const isHostRecoveryPage = registerDocumentRecovery(win.webContents, `${auth.origin}/hosts`, "返回助手");
        guardHostWindow(win, entry, auth, isHostRecoveryPage);
        return win.webContents;
      } };
    });
    const guardNavigation = (event: { preventDefault(): void }, value: string) => {
      if (isRecoveryPage(value)) return;
      const target = sameOriginURL(value, auth.origin);
      if (!target || target.pathname.startsWith("/admin") || target.pathname === "/login") {
        event.preventDefault();
        if (target?.pathname.startsWith("/admin")) void shell.openExternal(target.href);
        else if (target?.pathname === "/login") restoreFromLogin();
      }
    };
    content.webContents.on("will-navigate", guardNavigation);
    content.webContents.on("will-redirect", guardNavigation);
    content.webContents.on("did-navigate-in-page", (_event, value) => {
      const target = sameOriginURL(value, auth.origin);
      if (credentials !== auth) return;
      if (target?.pathname === "/login") restoreFromLogin();
      if (target?.pathname.startsWith("/admin")) { void shell.openExternal(target.href); void content.webContents.loadURL(assistantURL, documentLoadOptions).catch(() => undefined); }
    });
    await content.webContents.loadURL(assistantURL, documentLoadOptions).catch(() => undefined);
  } else if (webView.webContents.getURL() !== assistantURL) {
    await webView.webContents.loadURL(assistantURL, documentLoadOptions).catch(() => undefined);
  }
  if (credentials !== auth || mainWindow !== owner || owner.isDestroyed() || webPartition !== partition) return;
  showView("web");
}

function openAssistant(initial?: WebSession): Promise<void> {
  const owner = mainWindow;
  if (!owner || owner.isDestroyed()) return Promise.reject(new Error("Morde window is unavailable"));
  if (webOpenTask?.window === owner) return webOpenTask.promise;
  const task = openWeb(initial, owner);
  const promise = task.finally(() => { if (webOpenTask?.promise === promise) webOpenTask = null; });
  webOpenTask = { window: owner, promise };
  return promise;
}

async function login(origin: unknown, name: unknown, password: unknown): Promise<unknown> {
  if (preservedOperationInProgress || workspaceSyncOperationInProgress || workspaceNode.mayOwnWorkspace() || activeLiveSync()) throw new Error("Stop local Workspace operations before changing accounts");
  if (credentials) throw new Error("Sign out before changing accounts");
  if (sync.snapshot() !== "idle" || childMayStillRun(syncCheckpoint)) throw new Error("Finish the previous Workspace sync before changing accounts");
  if (!safeStorageReady()) throw new Error("Secure operating system storage is unavailable");
  const normalizedOrigin = portalOrigin(origin);
  const normalizedName = typeof name === "string" ? name.trim() : "";
  const existingFence = clientLoginAttempts[accountProfileID(normalizedOrigin, normalizedName)];
  const mayRenewInstallation = existingFence ?
    (existingFence.installationID !== null ? existingFence.installationID === lastInstallation?.id : existingFence.allowLegacyRenewal) : true;
  const previousID = lastInstallation?.serviceInstanceID && lastInstallation.origin === normalizedOrigin && lastInstallation.name === normalizedName && mayRenewInstallation
    ? { installationID: lastInstallation.id, serviceInstanceID: lastInstallation.serviceInstanceID } : undefined;
  const attempt = nextClientLoginAttempt(normalizedOrigin, normalizedName);
  let result: Awaited<ReturnType<typeof api.login>>;
  try { result = await api.login(normalizedOrigin, name, password, attempt, previousID); }
  catch (error) {
    if (!isCurrentClientLoginAttempt(normalizedOrigin, normalizedName, attempt)) {
      throw new Error("较新的登录尝试已开始，已丢弃此响应。请使用最新尝试登录。");
    }
    throw error;
  }
  if (!isCurrentClientLoginAttempt(normalizedOrigin, normalizedName, attempt)) {
    throw new Error("较新的登录尝试已开始，已丢弃此响应。请使用最新尝试登录。");
  }
  const previousInstallation = lastInstallation;
  const previousProfile = activeProfile, previousMappings = mappings, previousCheckpoint = syncCheckpoint, previousLiveSyncRecords = liveSyncRecords, previousLiveSyncStateError = liveSyncStateError, previousCapability = liveSyncCapability, previousCapabilityMessage = liveSyncCapabilityMessage, previousHandoffRecord = handoffRecord, previousProfileError = profileError, previousChildPid = syncChildPid;
  credentials = result.credentials;
  try {
    bindClientInstallation(normalizedOrigin, normalizedName, attempt, result.credentials.installationId);
    loadProfile(result.credentials);
    if (activeProfile !== previousProfile) syncChildPid = undefined;
    savePrivate("credentials.bin", safeStorage.encryptString(JSON.stringify({ version: 2, credentials: result.credentials })));
    savePrivate("service-origin.txt", result.credentials.origin);
    savePrivate("installation.json", JSON.stringify({ origin: result.credentials.origin, serviceInstanceID: result.credentials.serviceInstanceID, name: normalizedName, id: result.credentials.installationId }));
    lastOrigin = credentials.origin;
    lastInstallation = { origin: credentials.origin, serviceInstanceID: credentials.serviceInstanceID, name: normalizedName, id: credentials.installationId };
    scheduleCredentialExpiry();
    scheduleAuthorizationCheck();
    void refreshLiveSyncCapability(result.credentials);
    await openAssistant(result.webSession);
    void restoreWorkspaceNode(result.credentials);
    return publicState();
  } catch (error) {
    try {
      rotateClientInstanceID(normalizedOrigin, normalizedName);
      await new MordeAPI(() => result.credentials).logout();
    } catch { clientLoginAttemptsError = true; }
    clearAuthorization();
    activeProfile = previousProfile;
    mappings = previousMappings;
    syncCheckpoint = previousCheckpoint;
    liveSyncRecords = previousLiveSyncRecords;
    liveSyncStateError = previousLiveSyncStateError;
    liveSyncCapability = previousCapability;
    liveSyncCapabilityMessage = previousCapabilityMessage;
    handoffRecord = previousHandoffRecord;
    profileError = previousProfileError;
    syncChildPid = previousChildPid;
    lastInstallation = previousInstallation;
    try {
      if (previousInstallation) savePrivate("installation.json", JSON.stringify(previousInstallation));
      else rmSync(settingsPath("installation.json"), { force: true });
    } catch { /* Login still fails closed when local metadata cannot be restored. */ }
    throw error;
  }
}

async function logout(): Promise<string | null> {
  if (preservedOperationInProgress || workspaceSyncOperationInProgress || projectDirectorySelectionInProgress || projectUploadInProgress) throw new Error("Finish the local Workspace operation before signing out");
  if (!credentials) return null;
  if (logoutInProgress) throw new Error("Sign-out is already in progress");
  const auth = credentials;
  const activeFence = clientLoginAttempts[accountProfileID(auth.origin, auth.accountName)];
  const revokeIdentity = {
    origin: auth.origin, serviceInstanceID: auth.serviceInstanceID, installationID: auth.installationId,
    clientInstanceID: activeFence?.clientInstanceID ?? null,
  };
  logoutInProgress = true;
  let warning: string | null = null;
  const addWarning = (message: string) => { warning = warning ? `${warning} ${message}` : message; };
  try {
    // Retire the instance before logout so an uncertain response cannot cause its reuse.
    try { rotateClientInstanceID(auth.origin, auth.accountName); }
    catch {
      clientLoginAttemptsError = true;
      addWarning("本机已退出，但登录设备状态无法保存，后续登录已禁止。请修复本机登录状态文件后重试。");
    }
    let pendingStored = false;
    try { rememberPendingRevocation(revokeIdentity); pendingStored = true; } catch { /* Revoke still runs; report marker failure if it is not confirmed. */ }
    const liveStopsConfirmed = await stopAllLiveSyncs();
    if (!liveStopsConfirmed) addWarning("本机实时同步停止状态未确认；恢复点和工作目录已保留。");
    const syncStop = priorSyncChildMayStillRun() ? Promise.resolve(false) : sync.stop();
    const workspaceStop = workspaceNode.stop();
    try { clearAuthorization(false); }
    catch {
      addWarning("本机授权已从当前会话清除，但授权文件无法删除，请检查本机存储。");
      notifyState();
    }
    lastInstallation = null;
    try { rmSync(settingsPath("installation.json"), { force: true }); }
    catch { addWarning("本机安装信息无法清理，请检查本机存储。"); }

    const remoteRevoke = new MordeAPI(() => auth).logout();
    let syncStopConfirmed = false;
    try {
      const stopped = await syncStop;
      if (stopped) {
        try {
          if (clearConfirmedChild()) syncStopConfirmed = true;
          else addWarning("同步进程仍可能持有目录；恢复点已保留。");
        } catch { addWarning("同步恢复点保存失败，请检查本机目录。"); }
      } else addWarning("同步进程停止状态未确认；恢复点已保留。");
    } catch { addWarning("同步进程停止状态未确认；恢复点已保留。"); }
    let workspaceStopConfirmed = false;
    try {
      workspaceStopConfirmed = await workspaceStop;
      if (!workspaceStopConfirmed) addWarning("本机 Workspace 节点停止状态未确认；目录仍受保护。");
    } catch { addWarning("本机 Workspace 节点停止状态未确认；目录仍受保护。"); }
    if (syncStopConfirmed && workspaceStopConfirmed && liveStopsConfirmed) {
      activeProfile = null;
      mappings = [];
      projectMappings = [];
      syncCheckpoint = null;
      liveSyncRecords = {};
      liveSyncRunners.clear();
      liveSyncStateError = null;
      handoffRecord = null;
      profileError = null;
      workspaceSyncProfile = null;
      workspaceSyncProfileError = null;
      workspaceSyncError = null;
      syncChildPid = undefined;
      notifyState();
    }

    let revokeConfirmed = false;
    try { await remoteRevoke; revokeConfirmed = true; }
    catch {
      if (pendingStored) addWarning("服务器撤销尚未确认。本机凭证和会话运行状态已清除；未确认的撤销标识已保存在本机，工作目录及本机恢复数据未删除。");
      else addWarning("服务器撤销尚未确认，且本机无法保存撤销标识。请在 Portal 的客户端列表中撤销此设备。");
    }
    if (revokeConfirmed && pendingStored) {
      try { confirmPendingRevocation(revokeIdentity); }
      catch { addWarning("服务端已确认撤销，但本机的旧撤销记录无法更新。"); }
    }
    return warning;
  } finally { logoutInProgress = false; }
}

function redactPrivateValues(value: string): string {
  let message = value;
  const privateValues = [
    app.getPath("userData"),
    ...mappings.map((mapping) => mapping.directory),
    credentials?.clientCredential,
    credentials?.syncCredential,
  ].filter((value): value is string => typeof value === "string" && value.length > 0).sort((left, right) => right.length - left.length);
  for (const value of privateValues) message = message.split(value).join("[本机路径或凭证已隐藏]");
  return message;
}

function safeRendererErrorMessage(error: unknown): string {
  return redactPrivateValues(error instanceof Error ? error.message : "Morde request failed");
}

function safePortalProjectMessage(value: string): string {
  let message = safeRendererErrorMessage(new Error(value));
  const privateValues = [
    credentials?.origin,
    credentials?.accountName,
    credentials?.installationId,
    ...projectMappings.map((mapping) => mapping.directory),
    ...(syncCheckpoint?.mode === "project_upload" ? [syncCheckpoint.directory] : []),
  ].filter((item): item is string => typeof item === "string" && item.length > 0).sort((left, right) => right.length - left.length);
  for (const item of privateValues) message = message.split(item).join("[本机路径或标识已隐藏]");
  return message;
}

function sendError(error: unknown): never {
  const message = safeRendererErrorMessage(error);
  if (message.includes("Authorization expired or revoked")) clearAuthorization();
  else if (message.includes("HTTP 403")) void sync.stop();
  throw new Error(message);
}
function handler<T extends unknown[]>(channel: string, fn: (...args: T) => Promise<unknown> | unknown): void {
  ipcMain.handle(channel, async (event, ...args: T) => {
    if (!mainWindow || event.sender !== mainWindow.webContents) throw new Error("Invalid desktop window");
    if (logoutInProgress && channel !== "morde:logout") throw new Error("Sign-out is in progress");
    if (credentials && credentialExpired(credentials.clientExpiresAt)) clearAuthorization();
    try { return await fn(...args); } catch (error) { return sendError(error); }
  });
}

function portalBridgeHandler(channel: string, fn: (...args: unknown[]) => Promise<unknown> | unknown): void {
  ipcMain.handle(channel, async (event, ...args: unknown[]) => {
    const auth = credentials;
    if (!auth || !trustedPortalBridgeEvent(event, auth)) throw new Error("Invalid Portal window or route");
    if (logoutInProgress) throw new Error("Sign-out is in progress");
    if (credentialExpired(auth.clientExpiresAt)) { clearAuthorization(); throw new Error("Authorization expired or revoked"); }
    try { return await fn(...args); } catch (error) { return sendError(error); }
  });
}

function register(): void {
  ipcMain.handle("morde:return-to-portal-hosts", async (event) => {
    const returnToPortalHosts = hostWorkbenchReturnHandlers.get(event.sender);
    if (!returnToPortalHosts || event.senderFrame !== event.sender.mainFrame || event.sender.isDestroyed()) {
      throw new Error("Invalid Host Workbench window");
    }
    if (!returnToPortalHosts()) throw new Error("Host Workbench is not on a trusted page or its authorization changed");
    return { returned: true };
  });
  handler("morde:state", () => publicState());
  handler("morde:login", login);
  handler("morde:logout", logout);
  handler("morde:public-page", (path: unknown, requestedOrigin?: unknown) => {
    if (path !== "register" && path !== "download") throw new Error("Invalid public page");
    const origin = credentials?.origin ?? (requestedOrigin ? portalOrigin(requestedOrigin) : lastOrigin);
    if (!origin) throw new Error("Enter a service address and log in first");
    const partition = session.fromPartition(`morde-public-${randomUUID()}`);
    partition.on("will-download", (_event, item) => item.setSaveDialogOptions({ title: "保存 Morde 安装包" }));
    partition.setPermissionRequestHandler((_contents, _permission, callback) => callback(false));
    const window = new BrowserWindow({ width: 960, height: 720, title: "Morde", webPreferences: { session: partition, contextIsolation: true, nodeIntegration: false, sandbox: true, webSecurity: true } });
    const registerDocumentRecovery = installDocumentErrorRecovery(partition, origin);
    const isRecoveryPage = registerDocumentRecovery(window.webContents, `${origin}/`, "返回首页");
    window.webContents.setWindowOpenHandler(() => ({ action: "deny" }));
    window.webContents.on("will-navigate", (event, value) => {
      if (isRecoveryPage(value)) return;
      const target = sameOriginURL(value, origin);
      if (target?.pathname === "/login") { event.preventDefault(); window.close(); mainWindow?.focus(); return; }
      if (!target || (target.pathname !== "/" && target.pathname !== "/register" && target.pathname !== "/download")) { event.preventDefault(); if (value.startsWith("https://")) void shell.openExternal(value); }
    });
    window.webContents.on("will-redirect", (event, value) => {
      if (isRecoveryPage(value)) return;
      const target = sameOriginURL(value, origin);
      if (!target || (target.pathname !== "/" && target.pathname !== "/register" && target.pathname !== "/download")) event.preventDefault();
      if (target?.pathname === "/login") { window.close(); mainWindow?.focus(); }
    });
    window.webContents.on("did-navigate-in-page", (_event, value) => {
      if (sameOriginURL(value, origin)?.pathname === "/login") { window.close(); mainWindow?.focus(); }
    });
    void window.loadURL(`${origin}/${path}`, documentLoadOptions).catch(() => undefined);
  });
  handler("morde:workspaces", () => api.workspaces());
  handler("morde:project-upload-targets", listProjectUploadTargets);
  handler("morde:project-select-directory", selectProjectDirectory);
  handler("morde:project-upload", uploadProject);
  portalBridgeHandler("morde:portal-state", () => publicPortalProjectState());
  portalBridgeHandler("morde:portal-project-upload-targets", listProjectUploadTargets);
  portalBridgeHandler("morde:portal-project-select-directory", selectProjectDirectory);
  portalBridgeHandler("morde:portal-project-upload", uploadProject);
  handler("morde:workspace-sync-toggle", setWorkspaceSyncEnabled);
  handler("morde:live-sync-start", startLiveSync);
  handler("morde:live-sync-stop", stopLiveSync);
  handler("morde:workspace-sync-status", async (workspaceIDInput: unknown) => {
    const workspaceID = checkedId("workspace", workspaceIDInput);
    const auth = credentials;
    if (!auth || !activeProfile || activeProfile !== profileID(auth) || workspaceSyncProfileError || !workspaceSyncProfile) {
      throw new Error(workspaceSyncProfileError ?? "Log in to Morde before checking Workspace sync");
    }
    if (workspaceSyncOperationInProgress || !workspaceNode.isRunning()) throw new Error("The local Workspace sync node is not ready");
    const binding = workspaceSyncProfile.bindings[workspaceID];
    const matches = mappings.filter((item) => item.workspaceId === workspaceID);
    if (!binding || !binding.enabled || binding.state !== "active" || matches.length !== 1) throw new Error("Enable an active Workspace sync binding first");
    await confirmWorkspaceMapping(matches[0]!);
    const status = await workspaceNode.workspaceStatus(binding.folderID);
    const currentBinding = workspaceSyncProfile?.bindings[workspaceID];
    const currentMappings = mappings.filter((item) => item.workspaceId === workspaceID);
    if (credentials !== auth || credentialExpired(auth.clientExpiresAt) || activeProfile !== profileID(auth) || !workspaceNode.isRunning() ||
      !currentBinding?.enabled || currentBinding.state !== "active" || currentBinding.folderID !== binding.folderID ||
      currentMappings.length !== 1 || currentMappings[0]!.directory !== matches[0]!.directory ||
      currentMappings[0]!.dev !== matches[0]!.dev || currentMappings[0]!.ino !== matches[0]!.ino) {
      throw new Error("Authorization or Workspace sync state changed while checking status");
    }
    await confirmWorkspaceMapping(currentMappings[0]!);
    return { ...status, observedAt: new Date().toISOString() };
  });
  handler("morde:choose-directory", async (workspaceId: unknown) => {
    if (preservedOperationInProgress || workspaceSyncOperationInProgress || workspaceNode.mayOwnWorkspace()) throw new Error("Stop local Workspace operations before changing folders");
    const auth = credentials;
    if (!auth) throw new Error("Log in to Morde first");
    if (profileError) throw new Error(profileError);
    const id = checkedId("workspace", workspaceId);
    const liveRecord = liveSyncRecords[id];
    if (activeLiveSync(id) || liveRecord?.recoveryRequired || liveRecord?.state === "conflict") throw new Error("Stop or resolve this Workspace's live sync before changing folders");
    if (sync.snapshot() !== "idle" || syncCheckpoint) throw new Error("Finish or discard the prior sync recovery point before changing folders");
    if (workspaceSyncProfile?.bindings[id]?.enabled) throw new Error("Pause Workspace sync before changing its local folder");
    const workspace = await api.workspace(id) as { state?: string };
    if (workspace.state !== "ready") throw new Error("Workspace is not ready for sync");
    if (directorySelectionInProgress || projectDirectorySelectionInProgress || projectUploadInProgress) throw new Error("Another folder selection is in progress");
    directorySelectionInProgress = true;
    try {
      const picked = await dialog.showOpenDialog(mainWindow!, { properties: ["openDirectory"] });
      if (picked.canceled || !picked.filePaths[0]) return null;
      const candidate = await checkedWorkspaceDirectory(picked.filePaths[0], id);
      if (credentials !== auth) throw new Error("Authorization changed while selecting a folder");
      if (workspaceSyncOperationInProgress || workspaceNode.mayOwnWorkspace() || sync.snapshot() !== "idle" || syncCheckpoint) throw new Error("Finish or discard the prior sync recovery point before changing folders");
      await assertNoLiveSyncForDirectory(candidate.directory, id);
      const mapping = { ...candidate, workspaceId: id };
      const previousLiveRecord = liveSyncRecords[id];
      if (previousLiveRecord && (previousLiveRecord.directory !== mapping.directory || previousLiveRecord.dev !== mapping.dev || previousLiveRecord.ino !== mapping.ino)) {
        const nextRecords = { ...liveSyncRecords };
        delete nextRecords[id];
        liveSyncRecords = nextRecords;
        try { saveLiveSyncRecords(); }
        catch (error) { liveSyncRecords = { ...liveSyncRecords, [id]: previousLiveRecord }; throw error; }
      }
      const nextMappings = [...mappings.filter((item) => item.workspaceId !== id), mapping];
      savePrivate(profileFile("mappings"), JSON.stringify(nextMappings));
      mappings = nextMappings;
      notifyState();
      return publicMapping(mapping);
    } finally { directorySelectionInProgress = false; }
  });
  handler("morde:sync", async (workspaceId: unknown, mode: Parameters<MordeSync["run"]>[2], scopeInput: unknown = [], confirmedQuiesced: unknown = false, handoffTargetInput: unknown = "cloud_workspace") => {
    if (preservedOperationInProgress) throw new Error("Finish the local safety-copy operation before starting sync");
    if (directorySelectionInProgress || projectDirectorySelectionInProgress || projectUploadInProgress) throw new Error("Finish selecting the Workspace folder before starting sync");
    if (profileError) throw new Error(profileError);
    if (!["upload", "download", "bidirectional", "live_sync", "handoff_checkpoint"].includes(mode)) throw new Error("Unsupported sync mode");
    const scope = validateSyncScope(scopeInput);
    const id = checkedId("workspace", workspaceId);
    const auth = credentials;
    const mapping = mappings.find((item) => item.workspaceId === id);
    if (!auth || !mapping) throw new Error("Log in and choose a folder first");
    await assertNoLiveSyncForDirectory(mapping.directory);
    if (workspaceSyncProfileError || !workspaceSyncProfile) throw new Error(workspaceSyncProfileError ?? "Workspace sync state is unavailable");
    if (mode === "live_sync") {
      try {
        await api.requireWorkspaceSyncCapability();
        if (credentials !== auth) throw new Error("Authorization changed while checking Workspace sync capability");
        liveSyncCapability = "enabled";
        liveSyncCapabilityMessage = "";
      } catch (error) {
        liveSyncCapability = "disabled";
        liveSyncCapabilityMessage = "服务端未开放或暂时无法核验 Workspace 同步能力；实时同步已禁用。";
        notifyState();
        throw error;
      }
    }
    if (workspaceSyncProfile.bindings[id]?.enabled) throw new Error("Pause Workspace sync before starting a manual sync or handoff checkpoint");
    if (workspaceSyncOperationInProgress) throw new Error("Finish configuring Workspace sync before starting a manual sync");
    let handoffTarget: HandoffTarget | undefined;
    if (mode === "handoff_checkpoint") {
      if (handoffTargetInput !== "desktop" && handoffTargetInput !== "cloud_workspace") throw new Error("Choose where to continue after the handoff checkpoint");
      handoffTarget = handoffTargetInput;
    }
    if (mode === "handoff_checkpoint" && confirmedQuiesced !== true) throw new Error("Confirm that Workspace writers are paused before creating a handoff checkpoint");
    const workspace = await api.workspace(id) as { state?: string };
    if (credentials !== auth || workspace.state !== "ready") throw new Error("Workspace is not ready for sync");
    if (workspaceSyncOperationInProgress || workspaceSyncProfile?.bindings[id]?.enabled) throw new Error("Pause Workspace sync before starting a manual sync or handoff checkpoint");
    if (childMayStillRun(syncCheckpoint)) throw new Error("A prior sync process may still hold this folder. Stop it before recovery");
    if (syncCheckpoint && (syncCheckpoint.serviceInstanceID !== auth.serviceInstanceID || syncCheckpoint.installationId !== auth.installationId || syncCheckpoint.workspaceId !== id || syncCheckpoint.mode !== mode || syncCheckpoint.directory !== mapping.directory || syncCheckpoint.dev !== mapping.dev || syncCheckpoint.ino !== mapping.ino || (mode === "handoff_checkpoint" && syncCheckpoint.handoffTarget !== handoffTarget))) throw new Error("An earlier sync needs recovery or explicit discard before starting another");
    if (syncCheckpoint && JSON.stringify(syncCheckpoint.scope) !== JSON.stringify(scope)) throw new Error("Resume this sync with its original scope or explicitly discard its recovery point");
    if (!syncCheckpoint) {
      syncCheckpoint = { serviceInstanceID: auth.serviceInstanceID, installationId: auth.installationId, workspaceId: id, directory: mapping.directory, dev: mapping.dev, ino: mapping.ino, mode, scope, ...(handoffTarget ? { handoffTarget } : {}), idempotencyKey: randomUUID() };
      try { savePrivate(profileFile("sync-checkpoint"), JSON.stringify(syncCheckpoint)); }
      catch {
        syncRecoveryWarning = { state: "failed", workspaceId: id, mode, message: "本机恢复点未能保存；同步未启动。" };
        syncCheckpoint = null;
        notifyState();
        throw new Error("Could not save the sync recovery point; sync was not started");
      }
      syncRecoveryWarning = null;
    }
    if (mode === "handoff_checkpoint" && syncCheckpoint.handoffTarget) {
      const target = syncCheckpoint.handoffTarget;
      try { saveHandoffRecord({ id: syncCheckpoint.idempotencyKey, workspaceId: id, scope: syncCheckpoint.scope, source: target === "desktop" ? "cloud_workspace" : "desktop", target, phase: "checking" }); }
      catch {
        syncRecoveryWarning = { state: "unknown", workspaceId: id, mode, message: "本机交接记录的保存状态未知；同步尚未启动。" };
        notifyState();
        throw new Error("Could not confirm the local handoff record was saved; sync was not started");
      }
    }
    let result: unknown;
    try { result = await sync.run(auth, mapping, mode, scope, syncCheckpoint, () => credentials === auth && !logoutInProgress, () => confirmWorkspaceMapping(mapping)); }
    catch (error) {
      if (mode === "handoff_checkpoint" && handoffRecord?.id === syncCheckpoint.idempotencyKey) {
        const { revision: _revision, ...record } = handoffRecord;
        try { saveHandoffRecord({ ...record, phase: "needs_review" }); }
        catch { markSyncRecoveryUnknown("本机交接记录保存状态未知；恢复点已保留。"); }
      }
      throw error;
    }
    finally { if (sync.snapshot() === "idle" && !childMayStillRun(syncCheckpoint)) clearConfirmedChild(); }
    const outcome = result && typeof result === "object" ? result as { revision?: unknown; session?: { id?: unknown; workspace_id?: unknown; state?: unknown }; conflicts?: unknown } : null;
    if (mode === "handoff_checkpoint") {
      if (!outcome || !Number.isSafeInteger(outcome.revision) || Number(outcome.revision) < 0) {
        if (handoffRecord?.id === syncCheckpoint.idempotencyKey) {
          const { revision: _revision, ...record } = handoffRecord;
          try { saveHandoffRecord({ ...record, phase: "needs_review" }); }
          catch { markSyncRecoveryUnknown("本机交接记录保存状态未知；恢复点已保留。"); }
        }
        markSyncRecoveryUnknown("交接检查点结果未知；本机恢复点已保留。");
        throw new Error("Workspace checkpoint result is unknown; the recovery point was retained");
      }
      if (!handoffRecord || handoffRecord.id !== syncCheckpoint.idempotencyKey) {
        markSyncRecoveryUnknown("本机交接记录缺失；恢复点已保留。");
        throw new Error("Handoff record is missing; the recovery point was retained");
      }
      try { saveHandoffRecord({ ...handoffRecord, phase: "verified", revision: Number(outcome.revision) }); }
      catch {
        markSyncRecoveryUnknown("交接检查点已核验，但本机交接记录保存状态未知；恢复点已保留。");
        throw new Error("Could not confirm the local handoff record was saved; the recovery point was retained");
      }
      removeSyncCheckpointFile();
      syncCheckpoint = null;
      syncRecoveryWarning = null;
      notifyState();
      return { revision: Number(outcome.revision) };
    } else {
      const session = outcome?.session;
      const conflicts = publicSyncConflicts(outcome?.conflicts);
      if (!session || typeof session.id !== "string" || !/^p[a-z0-9]{7}$/.test(session.id) || session.workspace_id !== id ||
        !["running", "completed", "conflict", "cancelled", "failed", "expired"].includes(String(session.state)) || conflicts === null) {
        throw new Error("Workspace sync result is unknown; the recovery point was retained");
      }
      if (session.state === "completed" && conflicts.length === 0) {
        removeSyncCheckpointFile();
        syncCheckpoint = null;
        syncRecoveryWarning = null;
        notifyState();
      }
      return { session: { id: session.id, workspace_id: id, state: String(session.state) }, conflicts };
    }
  });
  handler("morde:sync-stop", async () => {
    if (preservedOperationInProgress) throw new Error("Finish the local safety-copy operation before stopping sync");
    if (priorSyncChildMayStillRun()) throw new Error("A prior sync process may still hold this folder. Its recovery point was kept");
    const stopped = await sync.stop();
    if (stopped && syncCheckpoint?.mode === "handoff_checkpoint" && handoffRecord?.id === syncCheckpoint.idempotencyKey) {
      const { revision: _revision, ...record } = handoffRecord;
      try { saveHandoffRecord({ ...record, phase: "needs_review" }); }
      catch {
        markSyncRecoveryUnknown("本机交接记录保存状态未知；恢复点已保留。");
        throw new Error("Could not confirm the local handoff record was updated");
      }
    }
    if (stopped && !clearConfirmedChild()) throw new Error("A prior sync process may still hold this folder. Its recovery point was kept");
    return stopped;
  });
  handler("morde:sync-discard", () => {
    if (preservedOperationInProgress) throw new Error("Finish the local safety-copy operation before changing the recovery point");
    if (profileError) throw new Error(profileError);
    if (sync.snapshot() === "running") throw new Error("Stop sync before discarding its recovery point");
    if (childMayStillRun(syncCheckpoint)) throw new Error("A prior sync process may still hold this folder");
    if (!sync.discardCheckpoint(syncCheckpoint)) throw new Error("A prior sync process may still hold this folder");
    if (syncCheckpoint?.mode === "handoff_checkpoint" && handoffRecord?.id === syncCheckpoint.idempotencyKey) {
      try { saveHandoffRecord(null); }
      catch {
        markSyncRecoveryUnknown("本机交接记录清理未确认；同步恢复点仍保留。");
        throw new Error("Could not confirm the local handoff record was discarded");
      }
    }
    try { rmSync(settingsPath(profileFile("sync-checkpoint")), { force: true }); }
    catch {
      markSyncRecoveryUnknown("本机同步恢复点清理未确认；请检查本机同步状态。");
      throw new Error("Could not confirm the local sync recovery point was discarded");
    }
    syncCheckpoint = null;
    syncRecoveryWarning = null;
    notifyState();
    return publicState();
  });
  handler("morde:preserved-list", async (workspaceId: unknown) => withPreservedMapping(workspaceId, (mapping): Promise<PreservedFile[]> =>
    sync.listPreserved(mapping, workspaceSyncProfile?.bindings[mapping.workspaceId]?.folderID)));
  handler("morde:preserved-restore", async (workspaceId: unknown, preservedId: unknown) => {
    if (typeof preservedId !== "string" || !/^[a-f0-9]{32}$/.test(preservedId)) throw new Error("Invalid safety-copy ID");
    await withPreservedMapping(workspaceId, async (mapping) => {
      await sync.restorePreserved(mapping, preservedId, workspaceSyncProfile?.bindings[mapping.workspaceId]?.folderID);
    });
    return { restored: true };
  });
  handler("morde:preserved-delete", async (workspaceId: unknown, preservedId: unknown) => {
    if (typeof preservedId !== "string" || !/^[a-f0-9]{32}$/.test(preservedId)) throw new Error("Invalid safety-copy ID");
    await withPreservedMapping(workspaceId, async (mapping) => {
      await sync.deletePreserved(mapping, preservedId, workspaceSyncProfile?.bindings[mapping.workspaceId]?.folderID);
    });
    return { deleted: true };
  });
}

function makeWindow(): BrowserWindow {
  const win = new BrowserWindow({ width: 1120, height: 760, minWidth: 720, minHeight: 520, title: "Morde", show: false, frame: true, webPreferences: { contextIsolation: true, nodeIntegration: false, sandbox: true, webSecurity: true, preload: join(__dirname, "morde-preload.js") } });
  win.webContents.setWindowOpenHandler(() => ({ action: "deny" }));
  win.webContents.on("will-navigate", (event) => event.preventDefault());
  win.on("resize", positionWeb);
  win.on("maximize", positionWeb);
  win.on("unmaximize", positionWeb);
  win.on("restore", positionWeb);
  win.on("enter-full-screen", positionWeb);
  win.on("leave-full-screen", positionWeb);
  void win.loadFile(join(__dirname, "../resources/morde.html"));
  win.once("ready-to-show", () => win.show());
  win.on("closed", () => { detachWeb(); mainWindow = null; });
  return win;
}
function showWindow(nextView: "web" | "sync" = "web"): void {
  const created = !mainWindow;
  if (created) mainWindow = makeWindow();
  if (credentials) {
    if (nextView === "sync") {
      void refreshLiveSyncCapability();
      showView("sync");
    } else if (webView) showView("web");
    else {
      const owner = mainWindow;
      void openAssistant().catch(() => {
        if (!owner || mainWindow !== owner || owner.isDestroyed()) return;
        owner.webContents.send("morde:sync-state", { state: "offline", message: "Morde 云端页面暂时无法连接；本机同步状态未改变。" });
        showView("sync");
      });
    }
  }
  mainWindow?.show();
  mainWindow?.focus();
}

function signOutFromMenu(): void {
  void logout().then((warning) => {
    if (warning && mainWindow && !mainWindow.isDestroyed()) void dialog.showMessageBox(mainWindow, { type: "warning", message: warning });
  }).catch((error: unknown) => {
    if (mainWindow && !mainWindow.isDestroyed()) void dialog.showMessageBox(mainWindow, { type: "error", message: safeRendererErrorMessage(error) });
  });
}

function menuSettings() {
  return [
    { label: zh ? "本机同步" : "Local Sync", click: () => showWindow("sync") },
    { label: zh ? "退出此电脑" : "Sign Out This PC", click: signOutFromMenu },
  ];
}

if (!app.requestSingleInstanceLock()) app.quit();
else {
  app.on("second-instance", () => showWindow());
  app.whenReady().then(() => {
    loadState();
    scheduleCredentialExpiry();
    scheduleAuthorizationCheck();
    register();
    Menu.setApplicationMenu(Menu.buildFromTemplate([
      { label: "Morde", submenu: [
        { label: zh ? "打开 Morde" : "Open Morde", click: () => showWindow() },
        { label: zh ? "设置" : "Settings", submenu: menuSettings() },
        { type: "separator" },
        { label: zh ? "退出 Morde" : "Quit Morde", click: () => app.quit() },
      ] },
      { role: "editMenu" },
    ]));
    showWindow();
    if (credentials) {
      void restoreWorkspaceNode(credentials);
    }
    try { tray = new Tray(join(__dirname, "../resources/morde-icon.png")); tray.setToolTip("Morde"); tray.setContextMenu(Menu.buildFromTemplate([
      { label: zh ? "打开 Morde" : "Open Morde", click: () => showWindow() },
      ...menuSettings(),
      { type: "separator" },
      { label: zh ? "退出 Morde" : "Quit Morde", click: () => app.quit() },
    ])); tray.on("double-click", () => showWindow()); } catch { tray = null; }
  });
  app.on("activate", () => showWindow());
  app.on("window-all-closed", () => { if (process.platform !== "darwin" && !tray) app.quit(); });
  app.on("before-quit", (event) => {
    if (exitReady) return;
    event.preventDefault();
    if (quitInProgress) return;
    quitInProgress = true;
    void (async () => {
      try {
        if (!await stopAllLiveSyncs()) throw new Error("Live sync process shutdown was not confirmed");
        const stopped = !priorSyncChildMayStillRun() && await sync.stop();
        if (!stopped && !sync.discardCheckpoint(syncCheckpoint)) throw new Error("Sync process may still be running");
        if (!await workspaceNode.stop()) throw new Error("Workspace sync node may still be running");
        if (!clearConfirmedChild()) throw new Error("Sync recovery point could not be saved");
        exitReady = true;
        app.quit();
      } catch {
        await dialog.showMessageBox({
          type: "warning",
          message: zh
            ? "同步进程或 Workspace 节点尚未确认退出，Morde 将保持打开。请等待进程停止后再次退出；恢复点已保留。"
            : "Sync or Workspace node shutdown was not confirmed. Morde will stay open; wait for the process to stop, then try quitting again. The recovery point was kept.",
        });
      } finally { quitInProgress = false; }
    })();
  });
}
