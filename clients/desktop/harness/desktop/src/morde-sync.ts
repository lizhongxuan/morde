import { spawn, spawnSync, type ChildProcessWithoutNullStreams } from "node:child_process";
import { access, lstat, realpath, stat } from "node:fs/promises";
import { constants } from "node:fs";
import { basename, dirname, isAbsolute, join, parse, relative, resolve, sep } from "node:path";
import { randomUUID } from "node:crypto";
import type { Credentials } from "./morde-core";
import { checkedId, credentialExpired } from "./morde-core";

export type Mapping = { workspaceId: string; directory: string; dev: string; ino: string };
export type PreservedFile = { id: string; workspace_id: string; path: string; created_at: string; size: number };
type Progress = { state: string; stage?: string; confirmed_bytes?: number; planned_bytes?: number; session_id?: string; confirmed_revision?: number };
export type HandoffTarget = "desktop" | "cloud_workspace";
export type HandoffRecord = { id: string; workspaceId: string; scope: string[]; source: HandoffTarget; target: HandoffTarget; phase: "checking" | "needs_review" | "verified"; revision?: number };
export type SyncCheckpoint = { serviceInstanceID: string; installationId: string; workspaceId: string; directory: string; dev: string; ino: string; mode: "upload" | "download" | "bidirectional" | "live_sync" | "handoff_checkpoint"; scope: string[]; handoffTarget?: HandoffTarget; idempotencyKey: string; sessionId?: string; childPid?: number };

export function validateSyncScope(value: unknown): string[] {
  if (!Array.isArray(value) || value.length > 1000 || value.some((part) => typeof part !== "string" || !part || isAbsolute(part) || /^[A-Za-z]:/.test(part) || part.split(/[\\/]/).some((segment) => segment === ".." || segment === "." || !segment))) throw new Error("Invalid sync scope");
  return [...value];
}

export async function checkedDirectory(directory: string, others: Mapping[]): Promise<Mapping> {
  if (!isAbsolute(directory) || resolve(directory) !== directory) throw new Error("Choose an absolute folder");
  const canonical = await realpath(directory);
  if (canonical === parse(canonical).root) throw new Error("Choose a project folder, not a filesystem root");
  const info = await lstat(canonical, { bigint: true });
  if (!info.isDirectory() || info.ino === 0n) throw new Error("Choose a stable local folder");
  await access(canonical, constants.R_OK | constants.W_OK);
  const normalize = (value: string) => process.platform === "win32" ? value.toLocaleLowerCase("en-US") : process.platform === "darwin" ? value.normalize("NFD").toLocaleLowerCase("en-US") : value;
  const within = async (child: string, parent: string) => {
    const part = relative(process.platform === "win32" ? normalize(parent) : parent, process.platform === "win32" ? normalize(child) : child);
    if (!part || (part !== ".." && !part.startsWith(`..${sep}`) && !isAbsolute(part))) return true;
    if (process.platform !== "darwin") return false;
    const childRoot = parse(child).root, parentRoot = parse(parent).root;
    const childParts = child.slice(childRoot.length).split(sep).filter(Boolean), parentParts = parent.slice(parentRoot.length).split(sep).filter(Boolean);
    if (childRoot !== parentRoot || childParts.length < parentParts.length || !parentParts.every((value, index) => normalize(value) === normalize(childParts[index]!))) return false;
    const [parentInfo, prefixInfo] = await Promise.all([stat(parent), stat(join(childRoot, ...childParts.slice(0, parentParts.length)))]);
    return parentInfo.dev === prefixInfo.dev && parentInfo.ino === prefixInfo.ino;
  };
  for (const item of others) {
    const other = await realpath(item.directory);
    if (await within(canonical, other) || await within(other, canonical)) throw new Error("Workspace folders must not overlap");
    const otherInfo = await stat(other, { bigint: true });
    if (otherInfo.dev === info.dev && otherInfo.ino === info.ino) throw new Error("Workspace folders must not overlap");
  }
  return { workspaceId: "", directory: canonical, dev: info.dev.toString(), ino: info.ino.toString() };
}

export async function verifyMapping(mapping: Mapping): Promise<void> {
  const current = await checkedDirectory(mapping.directory, []);
  if (current.directory !== mapping.directory || current.dev !== mapping.dev || current.ino !== mapping.ino) throw new Error("Folder identity changed; select it again");
}

export async function pathsOverlap(leftPath: string, rightPath: string): Promise<boolean> {
  const canonical = async (value: string) => {
    try {
      const path = await realpath(value);
      const info = await stat(path, { bigint: true });
      return { path, dev: info.dev.toString(), ino: info.ino.toString() };
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
      const parent = await realpath(dirname(value));
      return { path: resolve(parent, basename(value)), dev: null, ino: null };
    }
  };
  const [left, right] = await Promise.all([canonical(leftPath), canonical(rightPath)]);
  const normalize = (value: string) => process.platform === "win32" ? value.toLocaleLowerCase("en-US") : process.platform === "darwin" ? value.normalize("NFD").toLocaleLowerCase("en-US") : value;
  const within = (child: string, parent: string) => {
    const part = relative(normalize(parent), normalize(child));
    return !part || (part !== ".." && !part.startsWith(`..${sep}`) && !isAbsolute(part));
  };
  return within(left.path, right.path) || within(right.path, left.path) ||
    (left.dev !== null && right.dev !== null && left.dev === right.dev && left.ino === right.ino);
}

function isPreservedFile(value: unknown): value is PreservedFile {
  if (!value || typeof value !== "object") return false;
  const file = value as Partial<PreservedFile>;
  return typeof file.id === "string" && /^[a-f0-9]{32}$/.test(file.id) &&
    typeof file.workspace_id === "string" && /^w[a-z0-9]{7}$/.test(file.workspace_id) &&
    typeof file.path === "string" && file.path.length > 0 && file.path.length <= 4096 &&
    !file.path.startsWith("/") && !file.path.includes("\\") &&
    file.path.split("/").every((part) => part !== "" && part !== "." && part !== "..") &&
    typeof file.created_at === "string" && Number.isFinite(Date.parse(file.created_at)) &&
    typeof file.size === "number" && Number.isSafeInteger(file.size) && file.size >= 0;
}

export class MordeSync {
  private child: ChildProcessWithoutNullStreams | null = null;
  private lastBinary: string | null = null;
  private requestId = 1;
  private stopping = false;
  private cancelId = 0;
  private cancelAcknowledged = false;
  private operationConfirmed = false;
  private localOperation = false;
  private state: "idle" | "running" | "shutdown_unknown" = "idle";
  private shutdownPid: number | null = null;
  constructor(private readonly binary: () => string, private readonly update: (state: string, progress?: Progress) => void, private readonly started?: (pid: number) => void) {}
  snapshot(): string { return this.state; }

  checkpointMayOwnDirectory(checkpoint: SyncCheckpoint | null): boolean {
    if (!checkpoint?.childPid) return false;
    try { process.kill(checkpoint.childPid, 0); }
    catch (error) { return (error as NodeJS.ErrnoException).code !== "ESRCH"; }
    try {
      const probe = spawnSync(this.lastBinary ?? this.binary(), ["--probe-operation-lock", checkpoint.directory], { encoding: "utf8", stdio: "ignore", timeout: 5_000, windowsHide: true });
      return Boolean(probe.error) || probe.status !== 0;
    } catch { return true; }
  }

  discardCheckpoint(checkpoint: SyncCheckpoint | null): boolean {
    if (this.child || this.localOperation || this.state === "running") return false;
    if (this.state === "shutdown_unknown" && (this.shutdownPid === null || checkpoint?.childPid !== this.shutdownPid)) return false;
    if (this.checkpointMayOwnDirectory(checkpoint)) return false;
    if (this.state === "shutdown_unknown") {
      this.state = "idle";
      this.shutdownPid = null;
      this.update("idle");
    }
    return true;
  }

  async listPreserved(mapping: Mapping, folderID?: string): Promise<PreservedFile[]> {
    checkedId("workspace", mapping.workspaceId);
    if (folderID !== undefined && !/^morde-[a-f0-9]{32}$/.test(folderID)) throw new Error("Invalid safety-copy folder ID");
    await verifyMapping(mapping);
    const result = await this.localRequest("list_preserved", { directory: mapping.directory, workspace_id: mapping.workspaceId, ...(folderID ? { folder_id: folderID } : {}) });
    if (!Array.isArray(result) || result.some((item) => !isPreservedFile(item) || item.workspace_id !== mapping.workspaceId)) throw new Error("Invalid safety-copy response");
    return result;
  }

  async restorePreserved(mapping: Mapping, id: string, folderID?: string): Promise<void> {
    checkedId("workspace", mapping.workspaceId);
    if (!/^[a-f0-9]{32}$/.test(id)) throw new Error("Invalid safety-copy ID");
    if (folderID !== undefined && !/^morde-[a-f0-9]{32}$/.test(folderID)) throw new Error("Invalid safety-copy folder ID");
    await verifyMapping(mapping);
    const result = await this.localRequest("restore_preserved", { directory: mapping.directory, workspace_id: mapping.workspaceId, id, ...(folderID ? { folder_id: folderID } : {}) });
    if (!result || typeof result !== "object" || (result as { restored?: unknown }).restored !== true) throw new Error("Invalid safety-copy restore response");
  }

  async deletePreserved(mapping: Mapping, id: string, folderID?: string): Promise<void> {
    checkedId("workspace", mapping.workspaceId);
    if (!/^[a-f0-9]{32}$/.test(id)) throw new Error("Invalid safety-copy ID");
    if (folderID !== undefined && !/^morde-[a-f0-9]{32}$/.test(folderID)) throw new Error("Invalid safety-copy folder ID");
    await verifyMapping(mapping);
    const result = await this.localRequest("delete_preserved", { directory: mapping.directory, workspace_id: mapping.workspaceId, id, ...(folderID ? { folder_id: folderID } : {}) });
    if (!result || typeof result !== "object" || (result as { deleted?: unknown }).deleted !== true) throw new Error("Invalid safety-copy delete response");
  }

  private async localRequest(method: "list_preserved" | "restore_preserved" | "delete_preserved", params: Record<string, string>): Promise<unknown> {
    if (this.child || this.localOperation || this.state !== "idle") throw new Error("Finish or verify the prior sync before accessing safety copies");
    this.localOperation = true;
    const binary = this.binary();
    this.lastBinary = binary;
    let child: ChildProcessWithoutNullStreams;
    try { child = spawn(binary, [], { shell: false, stdio: ["pipe", "pipe", "pipe"], windowsHide: true }); }
    catch { this.localOperation = false; throw new Error("Could not start the local safety-copy helper"); }
    child.stderr.resume();
    const id = this.requestId++;
    const request = { jsonrpc: "2.0", id, method, params };
    return await new Promise((resolveResult, rejectResult) => {
      let buffer = "";
      let result: unknown;
      let received = false;
      let failure: Error | null = null;
      let settled = false;
      const timeout = setTimeout(() => {
        failure = new Error("Local safety-copy operation timed out");
        child.kill();
      }, 30_000);
      const settle = (error?: Error) => {
        if (settled) return;
        settled = true;
        clearTimeout(timeout);
        this.localOperation = false;
        if (error) rejectResult(error);
        else resolveResult(result);
      };
      child.stdout.setEncoding("utf8");
      child.stdout.on("data", (chunk: string) => {
        buffer += chunk;
        if (buffer.length > 1_000_000) { failure = new Error("Safety-copy response too large"); child.kill(); return; }
        for (;;) {
          const end = buffer.indexOf("\n");
          if (end < 0) break;
          const line = buffer.slice(0, end); buffer = buffer.slice(end + 1);
          let message: Record<string, unknown>;
          try { message = JSON.parse(line) as Record<string, unknown>; }
          catch { failure = new Error("Invalid safety-copy response"); child.kill(); return; }
          if (message.id !== id) continue;
          received = true;
          if (message.error) failure = new Error(method === "list_preserved" ? "Could not read local safety copies" : method === "restore_preserved" ? "Could not restore the local safety copy; its archive remains in place" : "Could not delete the local safety copy");
          else result = message.result;
          try { child.stdin.end(); } catch { child.kill(); }
        }
      });
      child.on("error", () => { failure = new Error("Could not start the local safety-copy helper"); });
      child.on("close", () => {
        if (failure) settle(failure);
        else if (!received) settle(new Error("Safety-copy helper exited without a verified result"));
        else settle();
      });
      try { child.stdin.end(`${JSON.stringify(request)}\n`); }
      catch { failure = new Error("Could not request the local safety-copy operation"); child.kill(); }
    });
  }

  async run(auth: Credentials, mapping: Mapping, mode: "upload" | "download" | "bidirectional" | "live_sync" | "handoff_checkpoint", scope: string[] = [], checkpoint?: SyncCheckpoint, stillAuthorized: () => boolean = () => true, confirmMapping?: () => Promise<void>, rescanSeconds = 30): Promise<unknown> {
    if (this.child || this.localOperation) throw new Error("Sync is already active or its prior shutdown is unverified");
    checkedId("workspace", mapping.workspaceId);
    if (!auth.syncCredential || credentialExpired(auth.syncExpiresAt)) throw new Error("Workspace sync is not authorized");
    await verifyMapping(mapping);
    await confirmMapping?.();
    if (!stillAuthorized() || credentialExpired(auth.syncExpiresAt)) throw new Error("Workspace sync authorization changed");
    scope = validateSyncScope(scope);
    if (mode === "live_sync" && (!Number.isSafeInteger(rescanSeconds) || rescanSeconds < 5 || rescanSeconds > 3600)) throw new Error("Live sync scan interval must be between 5 and 3600 seconds");
    if (this.state === "shutdown_unknown" && !this.child && checkpoint?.childPid === this.shutdownPid && this.shutdownPid !== null) {
      if (!this.checkpointMayOwnDirectory(checkpoint)) {
        this.state = "idle";
        this.shutdownPid = null;
      }
    }
    if (this.child || this.state === "shutdown_unknown") throw new Error("Sync is already active or its prior shutdown is unverified");
    const params = { base_url: auth.origin, bearer_token: auth.syncCredential, workspace_id: mapping.workspaceId, directory: mapping.directory, scope };
    const method = mode === "live_sync" || mode === "handoff_checkpoint" ? mode : "sync";
    const data = method === "sync" ? { ...params, direction: mode, idempotency_key: checkpoint?.idempotencyKey ?? randomUUID(), ...(checkpoint?.sessionId ? { session_id: checkpoint.sessionId } : {}) } : method === "handoff_checkpoint" ? { ...params, idempotency_key: checkpoint?.idempotencyKey ?? randomUUID() } : { ...params, rescan_seconds: rescanSeconds };
    this.state = "running";
    this.stopping = false;
    this.cancelId = 0;
    this.cancelAcknowledged = false;
    this.operationConfirmed = false;
    this.update("running");
    let child: ChildProcessWithoutNullStreams;
    const binary = this.binary();
    this.lastBinary = binary;
    try { child = spawn(binary, [], { shell: false, stdio: ["pipe", "pipe", "pipe"], windowsHide: true }); }
    catch { this.state = "idle"; this.update("idle"); throw new Error("Could not start morde-sync"); }
    this.child = child;
    child.stderr.resume();
    const request = { jsonrpc: "2.0", id: this.requestId++, method, params: data };
    return await new Promise((resolveResult, rejectResult) => {
      let buffer = "";
      let settled = false;
      let received = false;
      let result: unknown;
      let failure: Error | null = null;
      let lockRejected = false;
      const settle = (error?: Error, result?: unknown) => {
        if (settled) return;
        settled = true;
        if (error) { if (this.state !== "shutdown_unknown") this.state = "idle"; this.update(this.state); rejectResult(error); }
        else { this.state = "idle"; this.update("idle"); resolveResult(result); }
      };
      const endOperation = (error?: Error, value?: unknown) => {
        if (received) return;
        received = true;
        failure = error ?? null;
        result = value;
        try { child.stdin.end(); } catch { child.kill(); }
      };
      child.stdout.setEncoding("utf8");
      child.stdout.on("data", (chunk: string) => {
        buffer += chunk;
        if (buffer.length > 1_000_000) { failure = new Error("Sync response too large"); child.kill(); return; }
        for (;;) {
          const end = buffer.indexOf("\n");
          if (end < 0) break;
          const line = buffer.slice(0, end); buffer = buffer.slice(end + 1);
          let message: Record<string, unknown>;
          try { message = JSON.parse(line) as Record<string, unknown>; } catch { failure = new Error("Invalid sync response"); child.kill(); return; }
          if (message.method === "progress") {
            const progress = message.params as Progress;
            this.update("running", progress);
          } else if (message.id === this.cancelId && message.result && typeof message.result === "object" && (message.result as { cancelled?: unknown }).cancelled === true) {
            this.cancelAcknowledged = true;
          } else if (message.id === request.id) {
            if (message.error) {
              const detail = (message.error as { message?: unknown }).message;
              lockRejected = detail === "another Morde sync operation is active for this directory";
              endOperation(new Error(lockRejected ? "This folder is already syncing in another Morde process" : "Sync failed; check workspace status"));
            } else if (!Object.prototype.hasOwnProperty.call(message, "result") || !message.result || typeof message.result !== "object" || Array.isArray(message.result)) {
              endOperation(new Error("Invalid sync response"));
            } else { this.operationConfirmed = true; endOperation(undefined, message.result); }
          }
        }
      });
      child.on("error", () => { failure = new Error("Could not start morde-sync"); });
      child.on("close", () => {
        this.child = null;
        if (this.stopping && !(this.cancelAcknowledged || this.operationConfirmed)) {
          this.state = "shutdown_unknown";
          this.shutdownPid = child.pid ?? null;
          this.update(this.state);
        }
        settle(failure ?? (received ? undefined : new Error("Sync exited without a verified result")), result);
      });
      try {
        if (child.pid) this.started?.(child.pid);
        child.stdin.write(`${JSON.stringify(request)}\n`);
      } catch (error) {
        failure = error instanceof Error ? error : new Error("Could not save sync recovery point");
        child.kill();
      }
    });
  }

  async stop(): Promise<boolean> {
    const child = this.child;
    if (!child) return this.state !== "shutdown_unknown";
    this.stopping = true;
    const id = this.requestId++;
    this.cancelId = id;
    try { child.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", id, method: "cancel", params: { request_id: id - 1 } })}\n`); child.stdin.end(); } catch { child.kill(); }
    let timeout: NodeJS.Timeout | undefined;
    const exited = await Promise.race([new Promise<boolean>((done) => child.once("close", () => done(true))), new Promise<boolean>((done) => { timeout = setTimeout(() => done(false), 15_000); })]);
    if (timeout) clearTimeout(timeout);
    if (!exited || !(this.cancelAcknowledged || this.operationConfirmed)) { child.kill(); this.state = "shutdown_unknown"; this.shutdownPid = child.pid ?? null; this.update(this.state); }
    else { this.child = null; this.state = "idle"; this.shutdownPid = null; this.update(this.state); }
    return exited && (this.cancelAcknowledged || this.operationConfirmed);
  }
}
