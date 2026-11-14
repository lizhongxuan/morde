import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import { basename, isAbsolute, resolve } from "node:path";
import type { CodexDiscoverInput, CodexQueuedCommand, CodexScope, MordeAPI } from "./morde-core";

// Dependency-free protocol handling adapted from MIT-licensed T3code sources:
// project/T3code/packages/effect-codex-app-server/src/protocol.ts and client.ts.
// Event projections below follow project/T3code/apps/server/src/orchestration-v2/Adapters/CodexAdapterV2.ts.

export type JsonObject = Record<string, unknown>;
export type CodexThread = {
  id: string;
  name: string;
  cwd: string;
  updatedAt: string | null;
  status: string;
};
export type CodexTurn = {
  id: string;
  status: string;
  startedAt: number | null;
  completedAt: number | null;
  items: JsonObject[];
  itemsView: string;
};
export type CodexSnapshot = {
  thread: CodexThread;
  turns: CodexTurn[];
  complete: boolean;
  anchored: boolean;
  nextCursor: string | null;
};

type PendingRequest = { resolve: (value: unknown) => void; reject: (error: Error) => void; timer: NodeJS.Timeout };
type JsonRpcMessage = { id?: number | string; method?: string; params?: unknown; result?: unknown; error?: { message?: string; code?: number } };

const MAX_LINE_BYTES = 8 * 1024 * 1024;
const MAX_TURN_PAGES = 10;
const TURN_PAGE_SIZE = 20;
const MAX_PENDING_REQUESTS = 64;

export type ParsedJsonRpcFrame =
  | { type: "response"; id: number | string; result?: unknown; error?: { code: number; message: string } }
  | { type: "notification"; method: string; params?: unknown }
  | { type: "request"; id: number | string; method: string; params?: unknown };

/** Strict frame routing for Codex's versionless JSON-RPC wire, without T3code's Effect runtime. */
export function parseJsonRpcFrame(line: string): ParsedJsonRpcFrame {
  const parsed = object(JSON.parse(line));
  if (!parsed || Object.hasOwn(parsed, "jsonrpc") && parsed.jsonrpc !== "2.0") throw new Error("Invalid Codex JSON-RPC envelope");
  const hasMethod = Object.hasOwn(parsed, "method");
  const hasResult = Object.hasOwn(parsed, "result");
  const hasError = Object.hasOwn(parsed, "error");
  if (hasMethod) {
    if (typeof parsed.method !== "string" || !/^[a-zA-Z][A-Za-z0-9/_-]{0,127}$/.test(parsed.method) || hasResult || hasError) {
      throw new Error("Invalid Codex JSON-RPC method frame");
    }
    const hasID = Object.hasOwn(parsed, "id");
    if (hasID && !(typeof parsed.id === "string" && parsed.id.length <= 512 || typeof parsed.id === "number" && Number.isSafeInteger(parsed.id))) {
      throw new Error("Invalid Codex JSON-RPC request id");
    }
    return hasID
      ? { type: "request", id: parsed.id as number | string, method: parsed.method, ...(Object.hasOwn(parsed, "params") ? { params: parsed.params } : {}) }
      : { type: "notification", method: parsed.method, ...(Object.hasOwn(parsed, "params") ? { params: parsed.params } : {}) };
  }
  if (!(typeof parsed.id === "string" && parsed.id.length <= 512 || typeof parsed.id === "number" && Number.isSafeInteger(parsed.id)) || hasResult === hasError) {
    throw new Error("Invalid Codex JSON-RPC response frame");
  }
  if (hasError) {
    const error = object(parsed.error);
    if (!error || typeof error.code !== "number" || !Number.isSafeInteger(error.code) || typeof error.message !== "string") {
      throw new Error("Invalid Codex JSON-RPC error response");
    }
    return { type: "response", id: parsed.id as number | string, error: { code: error.code, message: error.message } };
  }
  return { type: "response", id: parsed.id as number | string, result: parsed.result };
}

export function encodeJsonRpcFrame(message: JsonObject): string {
  return `${JSON.stringify(message)}\n`;
}

function object(value: unknown): JsonObject | null {
  return value !== null && typeof value === "object" && !Array.isArray(value) ? value as JsonObject : null;
}

function boundedString(value: unknown, maximum = 4096): string | null {
  return typeof value === "string" && value.length <= maximum && !value.includes("\u0000") ? value : null;
}

function visibleLabel(value: unknown, maximum: number): string {
  return (typeof value === "string" ? value : "").replace(/[\u0000-\u001f\u007f-\u009f]/g, " ").replace(/\s+/g, " ").trim().slice(0, maximum);
}
function safeCwdLabel(path: string): string { return visibleLabel(path ? basename(path) : "未知目录", 256) || "未知目录"; }

function unixSecondsISO(value: unknown): string | null {
  if (typeof value !== "number" || !Number.isFinite(value)) return null;
  const date = new Date(value * 1000);
  return Number.isFinite(date.getTime()) ? date.toISOString() : null;
}

function stableID(value: unknown): value is string {
  return typeof value === "string" && value.length > 0 && value.length <= 512 && !/[\u0000-\u001f\u007f]/.test(value);
}

function threadFrom(value: unknown): CodexThread | null {
  const row = object(value);
  if (!row || !stableID(row.id)) return null;
  const updated = typeof row.updatedAt === "number" ? unixSecondsISO(row.updatedAt) :
    typeof row.updated_at === "string" && Number.isFinite(Date.parse(row.updated_at)) ? new Date(row.updated_at).toISOString() : null;
  const cwd = boundedString(row.cwd, 4096) ?? "";
  return {
    id: row.id,
    name: visibleLabel(boundedString(row.name, 4096) ?? boundedString(row.preview, 4096) ?? "Codex conversation", 512) || "Codex conversation",
    cwd,
    updatedAt: updated,
    status: boundedString(object(row.status)?.type, 128) ?? boundedString(row.status, 128) ?? "unknown",
  };
}

function turnFrom(value: unknown): CodexTurn | null {
  const row = object(value);
  if (!row || !stableID(row.id) || typeof row.status !== "string" || !Array.isArray(row.items) ||
    (row.itemsView !== undefined && typeof row.itemsView !== "string")) return null;
  const items: JsonObject[] = [];
  for (const value of row.items) {
    const item = object(value);
    if (!item || !stableID(item.id) || typeof item.type !== "string") return null;
    items.push(item);
  }
  return {
    id: row.id, status: row.status,
    startedAt: typeof row.startedAt === "number" && Number.isFinite(row.startedAt) ? row.startedAt : null,
    completedAt: typeof row.completedAt === "number" && Number.isFinite(row.completedAt) ? row.completedAt : null,
    items, itemsView: typeof row.itemsView === "string" ? row.itemsView : "unknown",
  };
}

function childEnvironment(codeHome: string): NodeJS.ProcessEnv {
  const allow = ["PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TERM", "TMPDIR", "TMP", "TEMP", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "SystemRoot", "WINDIR", "HOMEDRIVE", "HOMEPATH"];
  const env: NodeJS.ProcessEnv = { CODEX_HOME: codeHome };
  for (const key of allow) {
    const value = process.env[key];
    if (typeof value === "string") env[key] = value;
  }
  return env;
}

export class MordeCodexProvider {
  private child: ChildProcessWithoutNullStreams | null = null;
  private carry = Buffer.alloc(0);
  private nextID = 0;
  private readonly pending = new Map<number, PendingRequest>();
  private closed = false;
  private startup: Promise<void> | null = null;
  private onNotice: (method: string, params: unknown) => void = () => undefined;
  private onRequest: (id: number | string, method: string, params: unknown) => void = () => undefined;
  private onExit: (message: string) => void = () => undefined;

  setHandlers(handlers: {
    notification?: (method: string, params: unknown) => void;
    request?: (id: number | string, method: string, params: unknown) => void;
    exit?: (message: string) => void;
  }): void {
    this.onNotice = handlers.notification ?? (() => undefined);
    this.onRequest = handlers.request ?? (() => undefined);
    this.onExit = handlers.exit ?? (() => undefined);
  }

  async start(executable: string, codeHome: string): Promise<void> {
    if (this.child && !this.closed) return;
    if (this.startup) return this.startup;
    this.startup = this.startChild(executable, codeHome).finally(() => { this.startup = null; });
    return this.startup;
  }

  private async startChild(executable: string, codeHome: string): Promise<void> {
    this.closed = false;
    this.carry = Buffer.alloc(0);
    this.nextID = 0;
    const child = spawn(executable, ["app-server", "--stdio"], {
      env: childEnvironment(codeHome),
      shell: false,
      windowsHide: true,
      stdio: ["pipe", "pipe", "pipe"],
    });
    this.child = child;
    child.stdout.on("data", (chunk: Buffer) => this.acceptOutput(chunk));
    child.stderr.on("data", () => undefined);
    child.once("error", () => this.failAll("Codex App Server failed to start"));
    child.once("exit", () => {
      const wasClosed = this.closed;
      this.child = null;
      this.failAll("Codex App Server exited");
      if (!wasClosed) this.onExit("Codex App Server exited");
    });
    try {
      const initialized = object(await this.request("initialize", {
        clientInfo: { name: "morde", title: "Morde Client", version: "1" },
        capabilities: { experimentalApi: true },
      }, 20_000));
      const receivedHome = typeof initialized?.codexHome === "string" ? initialized.codexHome : "";
      const normalizeHome = (value: string) => process.platform === "win32" ? resolve(value).toLowerCase() : resolve(value);
      if (!initialized || !receivedHome || normalizeHome(receivedHome) !== normalizeHome(codeHome) ||
        typeof initialized.platformFamily !== "string" || typeof initialized.platformOs !== "string" || typeof initialized.userAgent !== "string") {
        throw new Error("Codex App Server did not confirm the selected CODEX_HOME profile");
      }
      this.notify("initialized");
    } catch (error) {
      await this.stop();
      throw error;
    }
  }

  private acceptOutput(chunk: Buffer): void {
    this.carry = Buffer.concat([this.carry, chunk]);
    if (this.carry.byteLength > MAX_LINE_BYTES && !this.carry.includes(0x0a)) {
      this.failAll("Codex App Server response exceeded the size limit");
      void this.stop();
      return;
    }
    while (true) {
      const newline = this.carry.indexOf(0x0a);
      if (newline < 0) break;
      if (newline > MAX_LINE_BYTES) {
        this.failAll("Codex App Server response exceeded the size limit");
        void this.stop();
        return;
      }
      const line = this.carry.subarray(0, newline).toString("utf8").trim();
      this.carry = this.carry.subarray(newline + 1);
      if (!line) continue;
      let message: ParsedJsonRpcFrame;
      try { message = parseJsonRpcFrame(line); }
      catch {
        this.failAll("Codex App Server returned invalid JSON-RPC");
        void this.stop();
        return;
      }
      if (message.type === "response") {
        if (typeof message.id !== "number") continue;
        const pending = this.pending.get(message.id);
        if (!pending) continue;
        clearTimeout(pending.timer);
        this.pending.delete(message.id);
        if (message.error) pending.reject(new Error("Codex App Server request failed"));
        else pending.resolve(message.result);
      } else if (message.type === "request") {
        this.onRequest(message.id, message.method, message.params);
      } else {
        this.onNotice(message.method, message.params);
      }
    }
    if (this.carry.byteLength > MAX_LINE_BYTES) {
      this.failAll("Codex App Server response exceeded the size limit");
      void this.stop();
    }
  }

  private write(message: JsonObject): void {
    if (!this.child || this.closed || this.child.stdin.destroyed) throw new Error("Codex App Server is unavailable");
    const line = encodeJsonRpcFrame(message);
    if (Buffer.byteLength(line) > 64 * 1024) throw new Error("Codex App Server request exceeds the size limit");
    this.child.stdin.write(line);
  }

  request(method: string, params: JsonObject, timeoutMs = 20_000): Promise<unknown> {
    if (!/^[a-zA-Z][A-Za-z0-9/_-]{0,127}$/.test(method)) return Promise.reject(new Error("Invalid Codex protocol method"));
    if (this.pending.size >= MAX_PENDING_REQUESTS) return Promise.reject(new Error("Too many Codex App Server requests are pending"));
    const id = ++this.nextID;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new Error(`Codex App Server request timed out (${method})`));
      }, timeoutMs);
      this.pending.set(id, { resolve, reject, timer });
      try { this.write({ id, method, params }); }
      catch (error) {
        clearTimeout(timer);
        this.pending.delete(id);
        reject(error instanceof Error ? error : new Error("Codex App Server is unavailable"));
      }
    });
  }

  notify(method: string, params?: JsonObject): void {
    this.write({ method, ...(params === undefined ? {} : { params }) });
  }

  respond(id: number | string, result: JsonObject): void {
    this.write({ id, result });
  }

  respondError(id: number | string, code: number, message: string): void {
    this.write({ id, error: { code, message } });
  }

  async listThreads(cursor: string | null = null): Promise<{ items: CodexThread[]; nextCursor: string | null }> {
    if (!(cursor === null || typeof cursor === "string" && cursor.length <= 4096 && !cursor.includes("\u0000"))) throw new Error("Invalid Codex thread cursor");
    const result = object(await this.request("thread/list", {
      cursor, limit: 100, sortKey: "updated_at", sortDirection: "desc", useStateDbOnly: true,
    }));
    if (!result || !Array.isArray(result.data)) throw new Error("Codex thread list is unavailable");
    const threads = result.data.map(threadFrom).filter((thread): thread is CodexThread => thread !== null);
    const rawNext = result.nextCursor;
    if (rawNext !== undefined && rawNext !== null && (typeof rawNext !== "string" || rawNext.length > 4096 || rawNext.includes("\u0000") || rawNext === cursor)) {
      throw new Error("Codex thread pagination is invalid");
    }
    const next = typeof rawNext === "string" ? rawNext : null;
    return { items: threads, nextCursor: next };
  }

  async readSnapshot(threadID: string, knownTurnIDs: ReadonlySet<string> = new Set(), startCursor: string | null = null): Promise<CodexSnapshot> {
    if (!stableID(threadID)) throw new Error("Invalid Codex thread ID");
    const before = object(await this.request("thread/read", { threadId: threadID, includeTurns: false }));
    const first = threadFrom(before?.thread);
    if (!first || first.id !== threadID) throw new Error("Codex thread is unavailable");
    const turns: CodexTurn[] = [];
    let cursor: string | null = startCursor;
    let nextCursor: string | null = null;
    let complete = false;
    let anchored = false;
    for (let page = 0; page < MAX_TURN_PAGES; page++) {
      const result = object(await this.request("thread/turns/list", {
        threadId: threadID, cursor, limit: TURN_PAGE_SIZE, itemsView: "full", sortDirection: "desc",
      }));
      if (!result || !Array.isArray(result.data)) throw new Error("Codex thread history is unavailable");
      for (const value of result.data) {
        const turn = turnFrom(value);
        if (!turn) throw new Error("Codex thread history is incomplete");
        turns.push(turn);
      }
      const rawNext = result.nextCursor;
      if (rawNext !== undefined && rawNext !== null && (typeof rawNext !== "string" || rawNext.length > 4096 || rawNext.includes("\u0000") || rawNext === cursor)) {
        throw new Error("Codex thread history pagination is invalid");
      }
      const next = typeof rawNext === "string" ? rawNext : null;
      if (!next) { complete = true; nextCursor = null; break; }
      nextCursor = next;
      if (knownTurnIDs.size && turns.some((turn) => knownTurnIDs.has(turn.id))) { anchored = true; break; }
      cursor = next;
    }
    const after = object(await this.request("thread/read", { threadId: threadID, includeTurns: false }));
    const second = threadFrom(after?.thread);
    if (!second || second.id !== threadID) throw new Error("Codex thread is unavailable");
    if (first.updatedAt !== second.updatedAt) throw new Error("Codex thread changed during snapshot");
    return { thread: second, turns, complete, anchored, nextCursor };
  }

  async resume(threadID: string): Promise<void> {
    if (!stableID(threadID)) throw new Error("Invalid Codex thread ID");
    const result = object(await this.request("thread/resume", { threadId: threadID, excludeTurns: true }));
    const thread = threadFrom(result?.thread);
    if (!thread || thread.id !== threadID) throw new Error("Codex thread could not be resumed");
  }

  async startTurn(threadID: string, text: string, clientMessageID = randomUUID()): Promise<string> {
    if (!stableID(threadID) || typeof text !== "string" || !text.trim() || text.length > 32768) throw new Error("Invalid Codex message");
    const result = object(await this.request("turn/start", {
      threadId: threadID, input: [{ type: "text", text }], clientUserMessageId: clientMessageID,
    }, 20_000));
    const turn = object(result?.turn);
    if (!turn || !stableID(turn.id)) throw new Error("Codex did not confirm the turn request");
    return turn.id;
  }

  async interrupt(threadID: string, turnID: string): Promise<void> {
    if (!stableID(threadID) || !stableID(turnID)) throw new Error("Invalid Codex turn");
    await this.request("turn/interrupt", { threadId: threadID, turnId: turnID });
  }

  async stop(): Promise<void> {
    this.closed = true;
    const child = this.child;
    this.child = null;
    this.failAll("Codex App Server stopped");
    if (!child) return;
    await new Promise<void>((resolve) => {
      const timer = setTimeout(() => { child.kill("SIGKILL"); resolve(); }, 1500);
      child.once("exit", () => { clearTimeout(timer); resolve(); });
      child.kill("SIGTERM");
    });
  }

  get isRunning(): boolean { return this.child !== null && !this.closed; }

  private failAll(message: string): void {
    for (const item of this.pending.values()) { clearTimeout(item.timer); item.reject(new Error(message)); }
    this.pending.clear();
  }
}

export function codexProviderEventID(kind: string, turnID: string, itemID: string): string {
  return `appserver:${kind}:${hash(`${turnID}\0${itemID}`)}`;
}

export function codexItemEventID(kind: string, turnID: string, itemID: string): string {
  return `item:${kind}:${hash(`${turnID}\0${itemID}`)}`;
}

export function codexSnapshotItemFingerprint(item: JsonObject): string {
  return hash(JSON.stringify(item));
}

export type CodexLocalProfile = { profileKey: string; homeDirectory: string; executable: string; createdAt: string };
export type CodexEventInput = {
  provider_event_id: string;
  kind: "user_message" | "assistant_message" | "assistant_delta" | "reasoning_summary" | "plan" | "turn_started" | "turn_completed" | "turn_failed" | "turn_cancelled" | "tool_call" | "tool_result" | "file_change" | "child_agent" | "approval_request" | "approval_status" | "unsupported";
  occurred_at: string;
  turn_id?: string;
  item_id?: string;
  status?: string;
  payload: Record<string, unknown>;
  source: "snapshot" | "live" | "approval";
};
export type CodexGrant = {
  id: string;
  profileKey: string;
  threadId: string;
  title: string;
  cwdLabel: string;
  updatedAt: string | null;
  status: string;
  scopes: CodexScope[];
  grantVersion: number;
  snapshotState: "pending" | "available" | "limited" | "unknown" | "disabled";
  historyComplete: boolean;
  historyCursor: string | null;
  snapshotMessage?: string;
  candidateTurns: Record<string, string>;
  knownItems: Record<string, string>;
  pendingEvents: CodexEventInput[];
  approvals: Record<string, CodexApprovalRequest>;
};
export type CodexApprovalRequest = {
  id: string;
  rpcID: number | string;
  version: string;
  profileKey: string;
  threadId: string;
  grantVersion: number;
  method: string;
  params: JsonObject;
  kind: string;
  summary: string;
  createdAt: string;
};
export type CodexCommandLedger = { status: string; providerRequestID?: string; resultCode?: string; resultPending?: boolean };
export type CodexPersistentState = {
  version: 1;
  serviceInstanceID: string;
  installationId: string;
  profiles: CodexLocalProfile[];
  selectedProfileKey: string | null;
  grants: CodexGrant[];
  commands: Record<string, CodexCommandLedger>;
};

export type CodexRuntimeState = {
  profiles: { profileKey: string; label: string; executableLabel: string; selected: boolean; state: string; error: string | null }[];
  selectedProfileKey: string | null;
  sessions: { id: string; profileKey: string; threadId: string; title: string; cwdLabel: string; updatedAt: string | null; status: string; scopes: CodexScope[]; snapshotState: string; snapshotMessage?: string; pendingEvents: number; blockedEvents: number }[];
  realtimeCapability: "available" | "unverified";
  running: boolean;
  error: string | null;
};

type CodexAPI = Pick<MordeAPI, "codexDiscover" | "codexClaim" | "codexCommandResult" | "codexEvents">;
type RuntimePersistence = { load: () => unknown | null; save: (value: CodexPersistentState) => void };

const SCOPES = new Set<CodexScope>(["read", "live", "send", "interrupt", "approval"]);
const FINAL_COMMAND_STATUSES = new Set(["forwarded", "running", "waiting_approval", "completed", "failed", "unknown"]);
const MAX_LOCAL_EVENTS = 3000;
const MAX_LOCAL_STATE_BYTES = 8 * 1024 * 1024;
const MAX_VISIBLE_MESSAGE_BYTES = 2 * 1024 * 1024;

function isCodexScope(value: unknown): value is CodexScope { return typeof value === "string" && SCOPES.has(value as CodexScope); }

function eventInputValid(value: unknown): value is CodexEventInput {
  const row = object(value);
  const payload = object(row?.payload);
  const allowedKeys: Record<string, Set<string>> = {
    user_message: new Set(["text", "segment_id"]), assistant_message: new Set(["text", "segment_id"]),
    assistant_delta: new Set(["text", "segment_id", "delta_index"]), reasoning_summary: new Set(["text"]),
    plan: new Set(["summary", "items"]), tool_call: new Set(["tool", "summary", "call_id", "status"]),
    tool_result: new Set(["tool", "summary", "output", "exit_code", "status"]),
    file_change: new Set(["path_label", "change", "summary"]), child_agent: new Set(["agent_label", "status", "summary"]),
    unsupported: new Set(["provider_type", "diagnostic"]), turn_started: new Set(["status"]),
    turn_completed: new Set(["status", "summary"]), turn_failed: new Set(["status", "error_code", "summary"]),
    turn_cancelled: new Set(["status", "summary"]), approval_request: new Set(["approval_id", "approval_version", "kind", "summary", "object", "detail", "impact"]),
    approval_status: new Set(["approval_id", "approval_version", "status", "summary"]),
  };
  if (!row || !stableID(row.provider_event_id) || typeof row.kind !== "string" || !allowedKeys[row.kind] ||
    !payload || Object.keys(payload).some((key) => !allowedKeys[row.kind as string]!.has(key)) ||
    !Object.values(payload).every((item) => typeof item === "string" && Buffer.byteLength(item) <= 32 * 1024 && !item.includes("\u0000") ||
      typeof item === "number" && Number.isFinite(item) || typeof item === "boolean" || Array.isArray(item) && item.length <= 256 && item.every((part) => typeof part === "string" && Buffer.byteLength(part) <= 4096 && !part.includes("\u0000"))) ||
    typeof row.occurred_at !== "string" || !Number.isFinite(Date.parse(row.occurred_at)) ||
    (row.status !== undefined && (typeof row.status !== "string" || row.status.length > 128 || /[\u0000-\u001f\u007f-\u009f]/.test(row.status))) ||
    (row.source !== "snapshot" && row.source !== "live" && row.source !== "approval") ||
    (row.turn_id !== undefined && !stableID(row.turn_id)) || (row.item_id !== undefined && !stableID(row.item_id))) return false;
  return true;
}

function validPersistentState(value: unknown, serviceInstanceID: string, installationId: string): value is CodexPersistentState {
  const state = object(value);
  if (!state || state.version !== 1 || state.serviceInstanceID !== serviceInstanceID || state.installationId !== installationId ||
    !Array.isArray(state.profiles) || state.profiles.length > 20 || !Array.isArray(state.grants) || state.grants.length > 500 ||
    !object(state.commands) || Object.keys(state.commands).length > 10000 ||
    !(state.selectedProfileKey === null || typeof state.selectedProfileKey === "string")) return false;
  const keys = new Set<string>();
  for (const value of state.profiles) {
    const profile = object(value);
    if (!profile || typeof profile.profileKey !== "string" || !/^[A-Za-z0-9_-]{1,128}$/.test(profile.profileKey) || keys.has(profile.profileKey) ||
      typeof profile.homeDirectory !== "string" || !(isAbsolute(profile.homeDirectory) || /^[A-Za-z]:[\\/]/.test(profile.homeDirectory)) ||
      typeof profile.executable !== "string" || !(isAbsolute(profile.executable) || /^[A-Za-z]:[\\/]/.test(profile.executable)) ||
      typeof profile.createdAt !== "string" || !Number.isFinite(Date.parse(profile.createdAt))) return false;
    keys.add(profile.profileKey);
  }
  if (state.selectedProfileKey !== null && !keys.has(state.selectedProfileKey)) return false;
  for (const value of state.grants) {
    const grant = object(value);
    if (!grant || typeof grant.id !== "string" || !/^l[a-z0-9]{7}$/.test(grant.id) || typeof grant.profileKey !== "string" || !keys.has(grant.profileKey) ||
      !stableID(grant.threadId) || typeof grant.title !== "string" || grant.title.length > 512 || typeof grant.cwdLabel !== "string" || grant.cwdLabel.length > 256 ||
      !(grant.updatedAt === null || typeof grant.updatedAt === "string" && Number.isFinite(Date.parse(grant.updatedAt))) || typeof grant.status !== "string" ||
      !Array.isArray(grant.scopes) || !grant.scopes.every(isCodexScope) || new Set(grant.scopes).size !== grant.scopes.length ||
      !Number.isSafeInteger(grant.grantVersion) || !["pending", "available", "limited", "unknown", "disabled"].includes(String(grant.snapshotState)) || typeof grant.historyComplete !== "boolean" ||
      !(grant.historyCursor === null || typeof grant.historyCursor === "string" && grant.historyCursor.length <= 4096) ||
      !object(grant.candidateTurns) || !object(grant.knownItems) || !Array.isArray(grant.pendingEvents) || !grant.pendingEvents.every(eventInputValid) ||
      grant.pendingEvents.length > MAX_LOCAL_EVENTS || !object(grant.approvals)) return false;
    for (const [id, approvalValue] of Object.entries(grant.approvals)) {
      const approval = object(approvalValue);
      if (!approval || id !== approval.id || !/^\d{1,24}$/.test(id) && !/^[A-Za-z0-9._~-]{1,128}$/.test(id) ||
        !(typeof approval.rpcID === "string" || typeof approval.rpcID === "number" && Number.isSafeInteger(approval.rpcID)) ||
        typeof approval.version !== "string" || !/^[a-f0-9]{64}$/.test(approval.version) || approval.profileKey !== grant.profileKey || approval.threadId !== grant.threadId ||
        approval.grantVersion !== grant.grantVersion || typeof approval.method !== "string" || !parseApprovalMethod(approval.method) ||
        !object(approval.params) || typeof approval.kind !== "string" || typeof approval.summary !== "string" || typeof approval.createdAt !== "string" || !Number.isFinite(Date.parse(approval.createdAt))) return false;
    }
  }
  for (const [id, value] of Object.entries(state.commands)) {
    const item = object(value);
    if (!/^g[a-z0-9]{7}$/.test(id) || !item || typeof item.status !== "string" || item.status.length > 32 ||
      (item.providerRequestID !== undefined && !stableID(item.providerRequestID)) ||
      (item.resultCode !== undefined && typeof item.resultCode !== "string") || (item.resultPending !== undefined && typeof item.resultPending !== "boolean")) return false;
  }
  return Buffer.byteLength(JSON.stringify(state)) <= MAX_LOCAL_STATE_BYTES;
}

function migratePersistentState(value: unknown): unknown {
  const state = object(value);
  if (state?.version !== 1 || !Array.isArray(state.grants)) return value;
  return {
    ...state,
    grants: state.grants.map((grantValue) => {
      const grant = object(grantValue);
      return grant && !Object.hasOwn(grant, "historyCursor") ? { ...grant, historyCursor: null } : grantValue;
    }),
  };
}

function terminalTurn(status: string): boolean { return ["completed", "failed", "interrupted"].includes(status); }
function snapshotTurnEvent(status: string): { kind: "turn_completed" | "turn_failed" | "turn_cancelled"; id: string } | null {
  if (status === "completed") return { kind: "turn_completed", id: status };
  if (status === "failed") return { kind: "turn_failed", id: status };
  if (status === "interrupted") return { kind: "turn_cancelled", id: status };
  return null;
}
function turnEventID(status: string, turnID: string): string { return `turn:${status}:${hash(turnID)}`; }
function turnCompletedAt(turn: CodexTurn): string { return unixSecondsISO(turn.completedAt) ?? new Date().toISOString(); }

function visibleText(item: JsonObject): string | null {
  if (item.type === "agentMessage") return typeof item.text === "string" ? item.text : null;
  if (item.type !== "userMessage" || !Array.isArray(item.content)) return null;
  const parts = item.content.flatMap((value) => {
    const part = object(value);
    return part?.type === "text" && typeof part.text === "string" ? [part.text] : [];
  });
  return parts.length ? parts.join("") : null;
}

function hash(value: string): string { return createHash("sha256").update(value).digest("hex"); }

function boundedMessageChunks(value: string): string[] {
  const chunks: string[] = [];
  let current = "";
  let currentBytes = 0;
  for (const codePoint of value) {
    const bytes = Buffer.byteLength(codePoint, "utf8");
    if (currentBytes + bytes > 24 * 1024) { chunks.push(current); current = ""; currentBytes = 0; }
    current += codePoint;
    currentBytes += bytes;
  }
  if (current || chunks.length === 0) chunks.push(current);
  return chunks;
}

function eventText(value: unknown, maximumBytes = 24 * 1024): string {
  const raw = typeof value === "string" ? value : "";
  const clean = raw.replace(/[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f-\u009f]/g, " ");
  const chunks = boundedMessageChunks(clean);
  let result = "";
  for (const part of chunks) {
    if (Buffer.byteLength(result + part) > maximumBytes) break;
    result += part;
  }
  return result;
}

function nativeStatus(value: unknown): string {
  return typeof value === "string" && /^[A-Za-z0-9_-]{1,64}$/.test(value) ? value : "unknown";
}

function itemOutput(item: JsonObject): string {
  if (typeof item.aggregatedOutput === "string") return item.aggregatedOutput;
  if (typeof item.output === "string") return item.output;
  const contentItems = Array.isArray(item.contentItems) ? item.contentItems : Array.isArray(item.output) ? item.output :
    Array.isArray(object(item.result)?.content) ? object(item.result)!.content as unknown[] : [];
  const texts = contentItems.flatMap((value) => {
    const row = object(value);
    return typeof value === "string" ? [value] : row && typeof row.text === "string" ? [row.text] : row && typeof row.title === "string" ? [row.title] : [];
  });
  if (typeof object(item.result)?.error === "string") texts.push(String(object(item.result)!.error));
  if (Array.isArray(item.results)) {
    for (const value of item.results.slice(0, 128)) {
      const row = object(value);
      if (!row) continue;
      const summary = [row.title, row.url, row.snippet, row.description].filter((part): part is string => typeof part === "string").join("\n");
      if (summary) texts.push(summary);
    }
  }
  return texts.join("\n");
}

function itemEvent(
  kind: CodexEventInput["kind"], turn: CodexTurn, item: JsonObject, suffix: string,
  payload: Record<string, unknown>, status?: string,
): CodexEventInput {
  const event: CodexEventInput = {
    provider_event_id: codexItemEventID(kind, turn.id, `${item.id}:${suffix}`), kind,
    occurred_at: turnCompletedAt(turn), turn_id: turn.id, item_id: String(item.id), payload, source: "snapshot",
  };
  if (status !== undefined) event.status = nativeStatus(status);
  return event;
}

/** Stable, bounded projection of the user-visible Codex items persisted by thread/turns/list. */
export function codexSnapshotItemEvents(turn: CodexTurn, item: JsonObject): CodexEventInput[] {
  const id = stableID(item.id) ? item.id : "unknown-item";
  const at = turn.completedAt ?? turn.startedAt ?? Date.now() / 1000;
  const itemTurn = { ...turn, id: turn.id, completedAt: at };
  const events: CodexEventInput[] = [];
  const pushText = (kind: "user_message" | "assistant_message", text: string) => {
    const chunks = boundedMessageChunks(text);
    chunks.forEach((chunk, index) => events.push(itemEvent(kind, itemTurn, { ...item, id }, `message:${index}`, {
      text: chunk, ...(chunks.length > 1 ? { segment_id: `${id}:${index}` } : {}),
    })));
  };
  const pushOutput = (tool: string, summary: string, output: string, status: string, exitCode?: number) => {
    const chunks = boundedMessageChunks(output);
    chunks.forEach((chunk, index) => events.push(itemEvent("tool_result", itemTurn, { ...item, id }, `result:${index}`, {
      tool: eventText(tool, 128) || "codex_tool", summary: eventText(summary, 12 * 1024), output: chunk,
      ...(typeof exitCode === "number" && Number.isFinite(exitCode) ? { exit_code: exitCode } : {}),
      status: nativeStatus(status),
    }, status)));
  };
  const pushTool = (tool: string, summary: string, status: string) => events.push(itemEvent("tool_call", itemTurn, { ...item, id }, "call", {
    tool: eventText(tool, 128) || "codex_tool", summary: eventText(summary), call_id: id, status: nativeStatus(status),
  }, status));

  const text = visibleText(item);
  if (item.type === "userMessage" && text !== null) pushText("user_message", text);
  else if (item.type === "agentMessage" && text !== null) pushText("assistant_message", text);

  if (item.type === "reasoning") {
    // Codex exposes both `summary` and private `content`; only its user-visible summary is projected.
    const summary = Array.isArray(item.summary) ? item.summary.filter((part): part is string => typeof part === "string").join("\n") : "";
    if (summary) events.push(itemEvent("reasoning_summary", itemTurn, { ...item, id }, "summary", { text: eventText(summary) }));
  } else if (item.type === "plan") {
    const summary = typeof item.text === "string" ? item.text : "";
    const steps = summary.split(/\r?\n/).map((line) => eventText(line, 4096)).filter(Boolean).slice(0, 256);
    events.push(itemEvent("plan", itemTurn, { ...item, id }, "plan", { summary: eventText(summary), items: steps }));
  } else if (item.type === "commandExecution") {
    const status = nativeStatus(item.status);
    const command = eventText(item.command, 24 * 1024);
    pushTool("command", command, status);
    pushOutput("command", command, typeof item.aggregatedOutput === "string" ? item.aggregatedOutput : "", status,
      typeof item.exitCode === "number" ? item.exitCode : undefined);
  } else if (item.type === "fileChange") {
    const changes = Array.isArray(item.changes) ? item.changes : [];
    for (const [index, value] of changes.entries()) {
      const change = object(value);
      const kind = object(change?.kind);
      if (typeof change?.path !== "string" || typeof kind?.type !== "string") continue;
      const pathParts = change.path.split(/[\\/]+/).filter(Boolean);
      const pathLabel = eventText(pathParts.slice(-2).join("/"), 256) || "文件";
      const action = ["add", "update", "delete"].includes(kind.type) ? kind.type : "change";
      events.push(itemEvent("file_change", itemTurn, { ...item, id }, `file:${index}`, {
        path_label: pathLabel, change: action, summary: `Codex ${action}`,
      }, nativeStatus(item.status)));
    }
  } else if (item.type === "mcpToolCall" || item.type === "dynamicToolCall") {
    const status = nativeStatus(item.status);
    const tool = item.type === "mcpToolCall" ? `${String(item.server ?? "mcp")}/${String(item.tool ?? "tool")}` : String(item.tool ?? "tool");
    pushTool(tool, tool, status);
    pushOutput(tool, tool, itemOutput(item), status);
  } else if (item.type === "webSearch") {
    const query = eventText(item.query, 12 * 1024);
    pushTool("web_search", query, "completed");
    pushOutput("web_search", query, itemOutput(item), "completed");
  } else if (item.type === "collabAgentToolCall") {
    const states = object(item.agentsStates) ?? {};
    const entries = Object.entries(states);
    if (entries.length) {
      for (const [index, [label, value]] of entries.entries()) {
        const state = object(value);
        const status = nativeStatus(state?.status);
        events.push(itemEvent("child_agent", itemTurn, { ...item, id }, `agent:${hash(label).slice(0, 12)}`, {
          agent_label: `agent-${hash(label).slice(0, 8)}`, status,
          summary: eventText(state?.message ?? `${String(item.tool ?? "subagent")} ${status}`, 12 * 1024),
        }, status));
      }
    } else {
      const status = nativeStatus(item.status);
      events.push(itemEvent("child_agent", itemTurn, { ...item, id }, "agent", {
        agent_label: `agent-${hash(String(item.receiverThreadIds ?? item.id)).slice(0, 8)}`, status,
        summary: eventText(`${String(item.tool ?? "subagent")} ${status}`, 12 * 1024),
      }, status));
    }
  } else if (item.type === "subAgentActivity") {
    const status = nativeStatus(item.kind);
    events.push(itemEvent("child_agent", itemTurn, { ...item, id }, "activity", {
      agent_label: `agent-${hash(String(item.agentThreadId ?? item.agentPath ?? item.id)).slice(0, 8)}`, status,
      summary: eventText(`Subagent ${status}`, 12 * 1024),
    }, status));
  } else if (item.type === "agentMessage" && Array.isArray(item.questions)) {
    for (const [index, value] of item.questions.entries()) {
      const question = object(value);
      if (!question) continue;
      const diagnostic = [
        typeof question.header === "string" ? question.header : "Codex question",
        typeof question.question === "string" ? question.question : "",
        ...(Array.isArray(question.options) ? question.options.flatMap((option) => {
          const row = object(option);
          return typeof row?.label === "string" ? [`• ${row.label}`] : typeof option === "string" ? [`• ${option}`] : [];
        }) : []),
        "Morde 当前只同步问题内容，不提供问题回答协议。",
      ].filter(Boolean).join("\n");
      events.push(itemEvent("unsupported", itemTurn, { ...item, id }, `question:${index}`, {
        provider_type: "user_question", diagnostic: eventText(diagnostic),
      }));
    }
  } else if (["imageView", "imageGeneration", "functionCallOutput", "sleep"].includes(String(item.type))) {
    const label = String(item.type);
    const details = item.type === "functionCallOutput" ? itemOutput(item) : `${label} item recorded by Codex.`;
    pushTool(label, label, nativeStatus(item.status ?? "completed"));
    pushOutput(label, label, details, nativeStatus(item.status ?? "completed"));
  } else if (item.type === "hookPrompt" || item.type === "contextCompaction") {
    events.push(itemEvent("unsupported", itemTurn, { ...item, id }, String(item.type), {
      provider_type: String(item.type), diagnostic: `Codex history contains a ${String(item.type)} item; no readable text projection is available.`,
    }));
  }
  return events;
}

function parseApprovalMethod(method: string): { kind: string; decision: (approved: boolean, params?: JsonObject) => JsonObject } | null {
  if (method === "item/commandExecution/requestApproval") return { kind: "command", decision: (approved) => ({ decision: approved ? "accept" : "decline" }) };
  if (method === "item/fileChange/requestApproval") return { kind: "file_change", decision: (approved) => ({ decision: approved ? "accept" : "decline" }) };
  if (method === "item/permissions/requestApproval") return { kind: "permissions", decision: (approved, params) => ({ permissions: approved ? object(params?.permissions) ?? {} : {} }) };
  return null;
}

function approvalSummary(params: JsonObject): string {
  const command = boundedString(params.command, 160);
  const reason = boundedString(params.reason, 160);
  const permissionNames = object(params.permissions) ? Object.keys(object(params.permissions)!).filter((key) => /^[a-zA-Z0-9_-]{1,40}$/.test(key)).join(", ") : "";
  return command ?? reason ?? (permissionNames ? `Codex 权限请求：${permissionNames}` : "Codex 请求审批");
}

function approvalPathLabel(value: unknown): string | null {
  const path = typeof value === "string" ? value : (() => {
    const row = object(value);
    return row?.type === "path" ? boundedString(row.path, 4096) : row?.type === "glob_pattern" ? boundedString(row.pattern, 4096) : null;
  })();
  const parts = path?.split(/[\\/]+/).filter(Boolean) ?? [];
  return parts.length ? visibleLabel(parts.slice(-2).join("/"), 256) : null;
}

function approvalPermissionDetail(params: JsonObject): string | null {
  const permissions = object(params.permissions);
  const fileSystem = object(permissions?.fileSystem);
  const requested: string[] = [];
  for (const access of ["read", "write"] as const) {
    const paths = Array.isArray(fileSystem?.[access]) ? fileSystem[access] as unknown[] : [];
    for (const path of paths.slice(0, 20)) {
      const label = approvalPathLabel(path);
      if (label) requested.push(`${access}: ${label}`);
    }
  }
  const entries = Array.isArray(fileSystem?.entries) ? fileSystem.entries : [];
  for (const value of entries.slice(0, 20)) {
    const entry = object(value);
    const label = entry && approvalPathLabel(entry.path);
    if (entry && ["read", "write", "deny"].includes(String(entry.access)) && label) requested.push(`${entry.access}: ${label}`);
    else if (entry && ["read", "write", "deny"].includes(String(entry.access)) && object(entry.path)?.type === "special") requested.push(`${entry.access}: special path`);
  }
  if (object(permissions?.network)?.enabled === true) requested.push("network access");
  return requested.length ? visibleLabel(requested.slice(0, 32).join("; "), 4096) : null;
}

export function codexApprovalPresentation(kind: string, params: JsonObject): { summary: string; object?: string; detail?: string; impact?: string } {
  const summary = approvalSummary(params);
  const reason = visibleLabel(params.reason, 4096);
  const command = visibleLabel(params.command, 4096);
  const cwd = approvalPathLabel(params.cwd);
  const grantRoot = approvalPathLabel(params.grantRoot);
  const permissionDetail = approvalPermissionDetail(params);
  const approvalObject = kind === "command" ? command ?? cwd : kind === "file_change" ? grantRoot : cwd ?? permissionDetail;
  const detail = kind === "command" ? cwd : kind === "permissions" ? permissionDetail : reason && reason !== summary ? reason : undefined;
  const impact = reason && reason !== summary ? reason : undefined;
  return {
    summary,
    ...(approvalObject ? { object: approvalObject } : {}),
    ...(detail ? { detail } : {}),
    ...(impact ? { impact } : {}),
  };
}

export class MordeCodexRuntime {
  private data: CodexPersistentState | null = null;
  private readonly providers = new Map<string, MordeCodexProvider>();
  private readonly providerStates = new Map<string, { state: string; error: string | null }>();
  private claimTimer: NodeJS.Timeout | null = null;
  private snapshotTimer: NodeJS.Timeout | null = null;
  private flushTimer: NodeJS.Timeout | null = null;
  private claimInProgress = false;
  private snapshotInProgress = false;
  private flushInProgress = false;
  private selectedGrantIndex = 0;
  private active = false;
  private loadFailed = false;
  private lastError: string | null = null;
  private deltaIndices = new Map<string, number>();
  private completedTurns = new Set<string>();
  private generation = 0;

  constructor(private readonly api: CodexAPI, private readonly persistence: RuntimePersistence, private readonly onChange: () => void) {}

  load(serviceInstanceID: string, installationId: string): void {
    this.generation++;
    this.providerStates.clear();
    this.deltaIndices.clear();
    this.completedTurns.clear();
    this.loadFailed = false;
    this.data = {
      version: 1, serviceInstanceID, installationId, profiles: [], selectedProfileKey: null, grants: [], commands: {},
    };
    this.lastError = null;
    try {
      const value = migratePersistentState(this.persistence.load());
      if (value !== null) {
        if (!validPersistentState(value, serviceInstanceID, installationId)) throw new Error("Invalid Codex profile state");
        this.data = value;
      }
    } catch {
      this.loadFailed = true;
      this.lastError = "Codex 本机授权状态无法安全读取；Provider 已暂停。";
      this.data = { version: 1, serviceInstanceID, installationId, profiles: [], selectedProfileKey: null, grants: [], commands: {} };
    }
  }

  async start(): Promise<void> {
    if (!this.data || this.active || this.loadFailed) return;
    this.active = true;
    for (const profile of this.data.profiles) {
      try { await this.ensureProvider(profile); }
      catch (error) { this.lastError = safeError(error); }
    }
    this.claimTimer = setInterval(() => { void this.claimCommands(); }, 2000);
    this.flushTimer = setInterval(() => { void this.flushPending(); }, 1500);
    this.snapshotTimer = setInterval(() => { void this.snapshotOne(); }, 2_500);
    await this.recoverUnknownCommands();
    this.onChange();
  }

  async stop(): Promise<void> {
    this.generation++;
    this.active = false;
    if (this.claimTimer) clearInterval(this.claimTimer);
    if (this.flushTimer) clearInterval(this.flushTimer);
    if (this.snapshotTimer) clearInterval(this.snapshotTimer);
    this.claimTimer = this.flushTimer = this.snapshotTimer = null;
    const providers = [...this.providers.values()];
    this.providers.clear();
    for (const item of Object.values(this.data?.commands ?? {})) {
      if (["dispatching", "claimed", "running", "forwarded", "waiting_approval"].includes(item.status)) {
        item.status = "unknown";
        item.resultCode = "interrupted_by_restart";
        item.resultPending = true;
      }
    }
    try { this.persist(); } catch { this.lastError = "Codex 命令恢复状态未能保存；操作结果可能未知。"; }
    for (const provider of providers) await provider.stop().catch(() => undefined);
    for (const profile of this.data?.profiles ?? []) this.providerStates.set(profile.profileKey, { state: "stopped", error: null });
    this.onChange();
  }

  publicState(): CodexRuntimeState {
    const state = this.data;
    return {
      profiles: (state?.profiles ?? []).map((profile) => {
        const provider = this.providerStates.get(profile.profileKey);
        return {
          profileKey: profile.profileKey, label: basename(profile.homeDirectory), executableLabel: basename(profile.executable),
          selected: profile.profileKey === state?.selectedProfileKey,
          state: provider?.state ?? "stopped", error: provider?.error ?? null,
        };
      }),
      selectedProfileKey: state?.selectedProfileKey ?? null,
      sessions: (state?.grants ?? []).map((grant) => ({
        id: grant.id, profileKey: grant.profileKey, threadId: grant.threadId, title: grant.title, cwdLabel: grant.cwdLabel,
        updatedAt: grant.updatedAt, status: grant.status, scopes: grant.scopes, snapshotState: grant.snapshotState,
        pendingEvents: grant.pendingEvents.length,
        blockedEvents: grant.pendingEvents.filter((event) => event.source === "snapshot" ? !grant.scopes.includes("read") : event.source === "approval" ? !grant.scopes.includes("approval") : !grant.scopes.includes("live")).length,
        ...(grant.snapshotMessage ? { snapshotMessage: grant.snapshotMessage } : {}),
      })),
      realtimeCapability: "unverified",
      running: this.active && !this.loadFailed,
      error: this.lastError,
    };
  }

  listThreads(cursor: string | null = null): Promise<{ items: { id: string; title: string; cwdLabel: string; updatedAt: string | null; status: string; authorized: boolean }[]; nextCursor: string | null }> {
    const selected = this.data?.profiles.find((profile) => profile.profileKey === this.data?.selectedProfileKey);
    if (!selected) throw new Error("请先选择 CODEX_HOME profile");
    return this.ensureProvider(selected).then(async (provider) => {
      const result = await provider.listThreads(cursor);
      return { items: result.items.map((thread) => ({
        id: thread.id, title: thread.name, cwdLabel: safeCwdLabel(thread.cwd), updatedAt: thread.updatedAt,
        status: thread.status, authorized: Boolean(this.data?.grants.some((grant) => grant.profileKey === selected.profileKey && grant.threadId === thread.id)),
      })), nextCursor: result.nextCursor };
    });
  }

  async addProfile(homeDirectory: string, executable: string): Promise<void> {
    const state = this.requireState();
    const existing = state.profiles.find((profile) => profile.homeDirectory === homeDirectory && profile.executable === executable);
    if (existing) { state.selectedProfileKey = existing.profileKey; await this.ensureProvider(existing); this.persist(); this.onChange(); return; }
    if (state.profiles.length >= 20) throw new Error("最多支持 20 个 Codex profile");
    const profile: CodexLocalProfile = { profileKey: randomUUID().replace(/-/g, ""), homeDirectory, executable, createdAt: new Date().toISOString() };
    state.profiles.push(profile);
    state.selectedProfileKey = profile.profileKey;
    this.persist();
    try { await this.ensureProvider(profile); }
    catch (error) { this.lastError = safeError(error); this.onChange(); throw error; }
    this.onChange();
  }

  selectProfile(profileKey: string): void {
    const state = this.requireState();
    const profile = state.profiles.find((item) => item.profileKey === profileKey);
    if (!profile) throw new Error("Codex profile not found");
    state.selectedProfileKey = profileKey;
    this.persist();
    if (this.active) void this.ensureProvider(profile).catch((error) => { this.lastError = safeError(error); this.onChange(); });
    this.onChange();
  }

  async authorize(threadID: string, scopesValue: unknown): Promise<{ id: string; grantVersion: number }> {
    if (!Array.isArray(scopesValue) || !scopesValue.every(isCodexScope) || new Set(scopesValue).size !== scopesValue.length || !scopesValue.length) {
      throw new Error("请选择至少一个 Codex 授权范围");
    }
    const state = this.requireState();
    const profile = state.profiles.find((item) => item.profileKey === state.selectedProfileKey);
    if (!profile) throw new Error("请先选择 CODEX_HOME profile");
    const old = state.grants.find((grant) => grant.profileKey === profile.profileKey && grant.threadId === threadID);
    const provider = await this.ensureProvider(profile);
    const listed = await provider.listThreads();
    if (!listed.some((thread) => thread.id === threadID)) throw new Error("线程不在当前 Codex profile 的可授权列表中");
    if (old) {
      await this.updateScopes(old.id, scopesValue);
      return { id: old.id, grantVersion: old.grantVersion };
    }
    const snapshot = await provider.readSnapshot(threadID, new Set(Object.keys(old?.candidateTurns ?? {})));
    const cwdLabel = safeCwdLabel(snapshot.thread.cwd);
    const discover: CodexDiscoverInput = {
      profile_key: profile.profileKey, thread_id: snapshot.thread.id, title: snapshot.thread.name.slice(0, 512),
      cwd_label: cwdLabel.slice(0, 256), updated_at: snapshot.thread.updatedAt,
      status: normalizeSessionStatus(snapshot.thread.status), scopes: scopesValue as CodexScope[],
    };
    const response = await this.api.codexDiscover(discover);
    const grant: CodexGrant = old ?? {
      id: response.id, profileKey: profile.profileKey, threadId, title: discover.title, cwdLabel, updatedAt: discover.updated_at,
      status: discover.status, scopes: response.scopes, grantVersion: response.grant_version,
      snapshotState: "pending", historyComplete: false, historyCursor: null, candidateTurns: {}, knownItems: {}, pendingEvents: [], approvals: {},
    };
    grant.id = response.id;
    grant.title = discover.title;
    grant.cwdLabel = cwdLabel;
    grant.updatedAt = discover.updated_at;
    grant.status = discover.status;
    grant.scopes = response.scopes;
    grant.grantVersion = response.grant_version;
    if (!grant.scopes.includes("read")) {
      grant.snapshotState = "disabled";
      grant.historyComplete = false;
      grant.historyCursor = null;
      grant.candidateTurns = {};
      grant.knownItems = {};
    }
    if (!old) state.grants.push(grant);
    this.persist();
    if (grant.scopes.includes("read")) this.reconcileSnapshot(grant, snapshot);
    this.onChange();
    return { id: grant.id, grantVersion: grant.grantVersion };
  }

  async updateScopes(sessionID: string, scopesValue: unknown): Promise<void> {
    if (!Array.isArray(scopesValue) || !scopesValue.every(isCodexScope) || new Set(scopesValue).size !== scopesValue.length) throw new Error("Invalid Codex scope selection");
    const grant = this.requireGrant(sessionID);
    const profile = this.requireProfile(grant.profileKey);
    const requested = scopesValue as CodexScope[];
    grant.scopes = grant.scopes.filter((scope) => requested.includes(scope));
    if (!requested.includes("read")) {
      grant.snapshotState = "disabled";
      grant.snapshotMessage = "读取范围已移除；未确认事件保留在本机并暂停上传。";
      grant.historyComplete = false;
      grant.historyCursor = null;
      grant.candidateTurns = {};
      grant.knownItems = {};
    }
    if (!requested.includes("approval")) {
      const provider = this.providers.get(profile.profileKey);
      if (provider) for (const approval of Object.values(grant.approvals)) {
        const capability = parseApprovalMethod(approval.method);
        if (capability) try { provider.respond(approval.rpcID, capability.decision(false, approval.params)); } catch { /* Local and cloud grants still close. */ }
      }
      grant.approvals = {};
    }
    this.persist();
    this.onChange();

    let snapshot: CodexSnapshot | null = null;
    let input: CodexDiscoverInput;
    if (requested.includes("read")) {
      const provider = await this.ensureProvider(profile);
      snapshot = await provider.readSnapshot(grant.threadId, new Set(Object.keys(grant.candidateTurns)));
      input = {
        profile_key: profile.profileKey, thread_id: grant.threadId, title: snapshot.thread.name.slice(0, 512),
        cwd_label: safeCwdLabel(snapshot.thread.cwd),
        updated_at: snapshot.thread.updatedAt, status: normalizeSessionStatus(snapshot.thread.status), scopes: requested,
      };
    } else {
      input = {
        profile_key: profile.profileKey, thread_id: grant.threadId, title: grant.title, cwd_label: grant.cwdLabel,
        updated_at: grant.updatedAt, status: normalizeSessionStatus(grant.status), scopes: requested,
      };
    }
    let response: Awaited<ReturnType<MordeAPI["codexDiscover"]>>;
    try { response = await this.api.codexDiscover(input); }
    catch (error) { this.onChange(); throw error; }
    grant.scopes = response.scopes;
    grant.grantVersion = response.grant_version;
    grant.title = input.title;
    grant.cwdLabel = input.cwd_label;
    grant.updatedAt = input.updated_at;
    grant.status = input.status;
    if (!grant.scopes.includes("read")) {
      grant.snapshotState = "disabled";
    } else if (snapshot) this.reconcileSnapshot(grant, snapshot, !grant.historyComplete && grant.historyCursor === null);
    for (const [id, approval] of Object.entries(grant.approvals)) {
      if (!grant.scopes.includes("approval") || approval.grantVersion !== grant.grantVersion) {
        const provider = this.providers.get(profile.profileKey);
        const capability = parseApprovalMethod(approval.method);
        if (provider && capability) try { provider.respond(approval.rpcID, capability.decision(false, approval.params)); } catch { /* Stale approval is closed. */ }
        delete grant.approvals[id];
      }
    }
    this.persist();
    this.onChange();
  }

  async revoke(sessionID: string): Promise<void> { await this.updateScopes(sessionID, []); }

  private async ensureProvider(profile: CodexLocalProfile): Promise<MordeCodexProvider> {
    let provider = this.providers.get(profile.profileKey);
    if (!provider) {
      provider = new MordeCodexProvider();
      const generation = this.generation;
      provider.setHandlers({
        notification: (method, params) => { if (this.generation === generation && this.active) void this.handleNotification(profile.profileKey, method, params); },
        request: (id, method, params) => { if (this.generation === generation && this.active) this.handleServerRequest(profile.profileKey, id, method, params); },
        exit: (error) => { if (this.generation === generation) { this.providerStates.set(profile.profileKey, { state: "unavailable", error }); this.onChange(); } },
      });
      this.providers.set(profile.profileKey, provider);
    }
    if (!provider.isRunning) {
      this.providerStates.set(profile.profileKey, { state: "connecting", error: null });
      this.onChange();
      try {
        await provider.start(profile.executable, profile.homeDirectory);
        this.providerStates.set(profile.profileKey, { state: "connected", error: null });
      } catch (error) {
        this.providerStates.set(profile.profileKey, { state: "unavailable", error: safeError(error) });
        this.providers.delete(profile.profileKey);
        await provider.stop().catch(() => undefined);
        throw error;
      }
    }
    return provider;
  }

  private async handleNotification(profileKey: string, method: string, value: unknown): Promise<void> {
    const params = object(value);
    const threadID = params?.threadId;
    if (!stableID(threadID)) return;
    const grant = this.data?.grants.find((item) => item.profileKey === profileKey && item.threadId === threadID);
    if (!grant) return;
    const turn = object(params?.turn);
    if (method === "turn/completed" && stableID(turn?.id)) {
      const turnKey = `${profileKey}\0${threadID}\0${turn.id}`;
      this.completedTurns.add(turnKey);
      const ledger = Object.values(this.requireState().commands).find((item) => item.providerRequestID === turn.id && item.status === "running");
      if (ledger) {
        ledger.status = turn.status === "completed" || turn.status === "interrupted" ? "completed" : "failed";
        if (turn.status === "interrupted") ledger.resultCode = "turn_interrupted";
        ledger.resultPending = true;
        try { this.persist(); } catch { this.lastError = "Codex turn 结果无法保存；状态未知。"; }
        void this.flushResults();
      }
    }
    if (!grant.scopes.includes("live")) return;
    const now = new Date().toISOString();
    let event: CodexEventInput | null = null;
    if (method === "turn/started") {
      if (!stableID(turn?.id)) return;
      event = { provider_event_id: turnEventID("turn_started", turn.id), kind: "turn_started", occurred_at: now, turn_id: turn.id, payload: { status: "in_progress" }, source: "live" };
    } else if (method === "turn/completed") {
      if (!stableID(turn?.id)) return;
      const status = typeof turn.status === "string" ? turn.status : "unknown";
      const kind = status === "completed" ? "turn_completed" : status === "interrupted" ? "turn_cancelled" : "turn_failed";
      event = { provider_event_id: turnEventID(kind, turn.id), kind, occurred_at: now, turn_id: turn.id, status, payload: { status }, source: "live" };
      } else if (method === "item/agentMessage/delta") {
      const turnID = params.turnId, itemID = params.itemId, delta = boundedString(params.delta, 24 * 1024);
      if (!stableID(turnID) || !stableID(itemID) || delta === null) return;
      const key = `${threadID}\0${turnID}\0${itemID}`;
      const index = this.deltaIndices.get(key) ?? 0;
      this.deltaIndices.set(key, index + 1);
      event = {
        provider_event_id: codexProviderEventID("assistant_delta", turnID, `${itemID}:${index}`), kind: "assistant_delta",
        occurred_at: now, turn_id: turnID, item_id: itemID, payload: { text: delta, segment_id: itemID, delta_index: index }, source: "live",
      };
      } else if (method === "item/started" || method === "item/completed") {
        const turnID = params.turnId, item = object(params.item);
        if (!stableID(turnID) || !item || !stableID(item.id) || typeof item.type !== "string") return;
        const projected = codexSnapshotItemEvents({ id: turnID, status: method === "item/completed" ? "completed" : "inProgress", startedAt: Date.now() / 1000, completedAt: Date.now() / 1000, items: [], itemsView: "full" }, item);
        for (const itemEvent of projected) {
          if (method === "item/started" && itemEvent.kind === "tool_result") continue;
          this.queueEvent(grant, {
            ...itemEvent, occurred_at: now, source: "live",
          });
        }
        return;
      } else if (method === "item/reasoning/summaryTextDelta" || method === "item/plan/delta" || method === "item/commandExecution/outputDelta") {
        const turnID = params.turnId, itemID = params.itemId;
        const delta = boundedString(params.delta, 24 * 1024);
        if (!stableID(turnID) || !stableID(itemID) || delta === null) return;
        const key = `${threadID}\0${turnID}\0${itemID}:${method}`;
        const index = this.deltaIndices.get(key) ?? 0;
        this.deltaIndices.set(key, index + 1);
        const kind = method === "item/reasoning/summaryTextDelta" ? "reasoning_summary" : method === "item/plan/delta" ? "plan" : "tool_result";
        const payload = kind === "reasoning_summary" ? { text: delta } : kind === "plan" ? { summary: delta, items: [] } : {
          tool: "command", summary: "Codex command output", output: delta, status: "inProgress",
        };
        event = {
          provider_event_id: codexProviderEventID(kind, turnID, `${itemID}:${method}:${index}`), kind,
          occurred_at: now, turn_id: turnID, item_id: itemID, status: "in_progress", payload, source: "live",
        };
      } else if (method === "turn/plan/updated") {
        const turnID = params.turnId;
        if (!stableID(turnID) || !Array.isArray(params.plan)) return;
        const items = params.plan.flatMap((value) => {
          const step = object(value);
          return typeof step?.step === "string" ? [eventText(step.step, 4096)] : [];
        }).slice(0, 256);
        const summary = typeof params.explanation === "string" ? eventText(params.explanation) : items.join("\n");
        event = { provider_event_id: codexProviderEventID("plan", turnID, `turn-plan:${hash(JSON.stringify({ summary, items }))}`), kind: "plan", occurred_at: now,
          turn_id: turnID, payload: { summary, items }, source: "live" };
    }
    if (!event) return;
    this.queueEvent(grant, event);
  }

  private handleServerRequest(profileKey: string, rpcID: number | string, method: string, value: unknown): void {
    const params = object(value);
    const threadID = params?.threadId;
    const grant = stableID(threadID) ? this.data?.grants.find((item) => item.profileKey === profileKey && item.threadId === threadID) : undefined;
    const capability = parseApprovalMethod(method);
    const provider = this.providers.get(profileKey);
    if (!provider) return;
    if (method === "item/tool/requestUserInput") {
      if (grant && grant.scopes.includes("approval") && params && Array.isArray(params.questions)) {
        const diagnostic = params.questions.flatMap((value) => {
          const question = object(value);
          return question && typeof question.question === "string" ? [eventText(question.question, 4096)] : [];
        }).slice(0, 16).join("\n");
        this.queueEvent(grant, {
          provider_event_id: `appserver:question:${hash(`${threadID}:${String(rpcID)}:${diagnostic}`)}`,
          kind: "unsupported", occurred_at: new Date().toISOString(),
          turn_id: stableID(params.turnId) ? params.turnId : undefined,
          item_id: stableID(params.itemId) ? params.itemId : undefined,
          payload: { provider_type: "user_question", diagnostic: eventText(`Codex 请求用户输入：\n${diagnostic}\nMorde 当前仅同步问题内容，不能提交回答。`) },
          source: "approval",
        });
      }
      // Morde's command contract currently has approve/reject only; never invent an answer.
      provider.respondError(rpcID, -32000, "Morde cannot answer this Codex user-input request");
      return;
    }
    if (!params || !grant || !capability || !grant.scopes.includes("approval")) {
      if (capability) provider.respond(rpcID, capability.decision(false, params ?? undefined));
      else provider.respondError(rpcID, -32000, "Approval request is unavailable or not authorized by Morde");
      return;
    }
    const id = String(rpcID);
    if (!stableID(id) || Buffer.byteLength(id) > 128) { provider.respondError(rpcID, -32600, "Invalid approval request id"); return; }
    const version = hash(JSON.stringify({ method, params }));
    const presentation = codexApprovalPresentation(capability.kind, params);
    const record: CodexApprovalRequest = {
      id, rpcID, version, profileKey, threadId: threadID, grantVersion: grant.grantVersion,
      method, params, kind: capability.kind, summary: presentation.summary, createdAt: new Date().toISOString(),
    };
    if (Object.keys(grant.approvals).length >= 100) {
      provider.respond(rpcID, capability.decision(false, params));
      return;
    }
    grant.approvals[id] = record;
    const queued = this.queueEvent(grant, {
      provider_event_id: `appserver:approval:${hash(`${id}:${version}`)}`, kind: "approval_request", occurred_at: record.createdAt,
      turn_id: stableID(params.turnId) ? params.turnId : undefined, item_id: stableID(params.itemId) ? params.itemId : undefined,
      payload: { approval_id: id, approval_version: version, kind: capability.kind, ...presentation }, source: "approval",
    });
    if (!queued) {
      delete grant.approvals[id];
      provider.respond(rpcID, capability.decision(false, params));
    }
  }

  private async claimCommands(): Promise<void> {
    if (!this.active || this.claimInProgress || !this.data || !this.data.profiles.length) return;
    this.claimInProgress = true;
    const generation = this.generation;
    try {
      await this.flushResults();
      const commands = await this.api.codexClaim();
      if (generation !== this.generation || !this.active) return;
      for (const command of commands) {
        if (generation !== this.generation || !this.active) break;
        await this.executeCommand(command);
      }
    } catch (error) {
      this.lastError = safeError(error);
      if (String(error).includes("Authorization expired or revoked")) await this.stop();
      this.onChange();
    } finally { this.claimInProgress = false; }
  }

  private async executeCommand(command: CodexQueuedCommand): Promise<void> {
    const state = this.requireState();
    const generation = this.generation;
    const existing = state.commands[command.id];
    if (existing) {
      if (!FINAL_COMMAND_STATUSES.has(existing.status)) {
        existing.status = "unknown";
        existing.resultCode = "previous_execution_state_unknown";
        existing.resultPending = true;
        this.persist();
      }
      return;
    }
    const grant = state.grants.find((item) => item.id === command.session_id && item.profileKey === command.profile_key && item.threadId === command.thread_id);
    const required: CodexScope = command.type === "send" ? "send" : command.type === "interrupt" ? "interrupt" : "approval";
    if (!grant || grant.grantVersion !== command.grant_version || !grant.scopes.includes(required)) {
      state.commands[command.id] = { status: "failed", resultCode: "scope_unavailable", resultPending: true };
      this.persist();
      return;
    }
    const profile = state.profiles.find((item) => item.profileKey === command.profile_key);
    if (!profile) {
      state.commands[command.id] = { status: "failed", resultCode: "profile_unavailable", resultPending: true };
      this.persist();
      return;
    }
    state.commands[command.id] = { status: "dispatching" };
    this.persist();
    try {
      const provider = await this.ensureProvider(profile);
      if (generation !== this.generation || !this.active) return;
      if (grant.grantVersion !== command.grant_version || !grant.scopes.includes(required)) {
        state.commands[command.id] = { status: "failed", resultCode: "scope_revoked_before_dispatch", resultPending: true };
        this.persist();
        await this.flushResults();
        return;
      }
      if (command.type === "send") {
        await provider.resume(command.thread_id);
        if (generation !== this.generation || !this.active) return;
        if (grant.grantVersion !== command.grant_version || !grant.scopes.includes("send")) {
          state.commands[command.id] = { status: "failed", resultCode: "scope_revoked_before_dispatch", resultPending: true };
          this.persist();
          await this.flushResults();
          return;
        }
        const turnID = await provider.startTurn(command.thread_id, command.input.text!);
        if (generation !== this.generation || !this.active) {
          const interrupted = state.commands[command.id];
          if (interrupted) interrupted.providerRequestID = turnID;
          try { this.persist(); } catch { /* The persisted unknown ledger remains authoritative. */ }
          return;
        }
        const turnKey = `${profile.profileKey}\0${command.thread_id}\0${turnID}`;
        const completed = this.completedTurns.has(turnKey);
        state.commands[command.id] = { status: completed ? "completed" : "running", providerRequestID: turnID, resultPending: true };
      } else if (command.type === "interrupt") {
        await provider.interrupt(command.thread_id, command.input.turn_id!);
        if (generation !== this.generation || !this.active) return;
        state.commands[command.id] = { status: "forwarded", providerRequestID: command.input.turn_id, resultPending: true };
      } else {
        const approval = grant.approvals[command.input.approval_id!];
        if (!approval || approval.version !== command.input.approval_version || approval.grantVersion !== grant.grantVersion || approval.profileKey !== profile.profileKey || approval.threadId !== grant.threadId) {
          state.commands[command.id] = { status: "failed", resultCode: "approval_stale", resultPending: true };
        } else {
          const capability = parseApprovalMethod(approval.method);
          if (!capability) throw new Error("Codex approval type is unavailable");
          delete grant.approvals[approval.id];
          this.persist();
          provider.respond(approval.rpcID, capability.decision(command.input.decision === "approve", approval.params));
          state.commands[command.id] = { status: "forwarded", providerRequestID: approval.id, resultPending: true };
          this.queueEvent(grant, {
            provider_event_id: `morde:approval:${hash(`${approval.id}:${approval.version}:${command.input.decision}`)}`,
            kind: "approval_status", occurred_at: new Date().toISOString(),
            payload: { approval_id: approval.id, approval_version: approval.version, status: command.input.decision === "approve" ? "approved" : "rejected" }, source: "approval",
          });
        }
      }
      this.persist();
    } catch (error) {
      state.commands[command.id] = {
        status: "unknown", resultCode: "provider_result_unknown", resultPending: true,
      };
      this.lastError = safeError(error);
      this.persist();
    }
    await this.flushResults();
    this.onChange();
  }

  private async recoverUnknownCommands(): Promise<void> {
    if (!this.data) return;
    let changed = false;
    for (const command of Object.values(this.data.commands)) {
      if (["dispatching", "claimed", "running", "forwarded", "waiting_approval"].includes(command.status)) {
        command.status = "unknown";
        command.resultCode = "previous_execution_state_unknown";
        command.resultPending = true;
        changed = true;
      }
    }
    if (changed) this.persist();
    await this.flushResults();
  }

  private async flushResults(): Promise<void> {
    if (!this.data) return;
    for (const [id, command] of Object.entries(this.data.commands)) {
      if (!command.resultPending || !FINAL_COMMAND_STATUSES.has(command.status)) continue;
      try {
        await this.api.codexCommandResult(id, {
          status: command.status as "forwarded" | "running" | "waiting_approval" | "completed" | "failed" | "unknown",
          ...(command.providerRequestID ? { provider_request_id: command.providerRequestID } : {}),
          ...(command.resultCode ? { result_code: command.resultCode } : {}),
        });
        command.resultPending = false;
        this.persist();
      } catch { this.lastError = "Codex 命令结果等待 Morde 确认；不会重新执行命令。"; return; }
    }
  }

  private async flushPending(): Promise<void> {
    if (!this.active || this.flushInProgress || !this.data) return;
    this.flushInProgress = true;
    const generation = this.generation;
    let changed = false;
    try {
      for (const grant of this.data.grants) {
        if (generation !== this.generation || !this.active) return;
        if (!grant.pendingEvents.length) continue;
        const allowed = (event: CodexEventInput) => event.source === "snapshot" ? grant.scopes.includes("read") : event.source === "approval" ? grant.scopes.includes("approval") : grant.scopes.includes("live");
        const sendable = grant.pendingEvents.filter(allowed);
        if (!sendable.length) continue;
        const payload: CodexEventInput[] = [];
        const ids: string[] = [];
        let bytes = 16;
        for (const pending of sendable) {
          const nextBytes = Buffer.byteLength(JSON.stringify(pending)) + 1;
          if (payload.length >= 128 || bytes + nextBytes > 480 * 1024) break;
          payload.push(pending);
          ids.push(pending.provider_event_id);
          bytes += nextBytes;
        }
        if (!payload.length) continue;
        const ack = await this.api.codexEvents(grant.id, payload);
        if (generation !== this.generation || !this.active) return;
        const acknowledged = new Set(ack.provider_event_ids);
        if (acknowledged.size !== payload.length || ids.some((id) => !acknowledged.has(id))) {
          this.lastError = "Codex 事件确认不完整；本机缓冲已保留。";
          continue;
        }
        grant.pendingEvents = grant.pendingEvents.filter((event) => !acknowledged.has(event.provider_event_id));
        this.persist();
        changed = true;
      }
    } catch (error) { this.lastError = safeError(error); changed = true; }
    finally { this.flushInProgress = false; if (changed) this.onChange(); }
  }

  private async snapshotOne(): Promise<void> {
    if (!this.active || this.snapshotInProgress || !this.data) return;
    const grants = this.data.grants.filter((grant) => grant.scopes.includes("read"));
    if (!grants.length) return;
    const now = Date.now();
    const activeGrants = grants.filter((grant) => grant.status === "in_progress" || grant.status === "waiting_approval");
    const recentGrants = grants.filter((grant) => grant.updatedAt && now - Date.parse(grant.updatedAt) < 60_000);
    const priority = [...new Set([...activeGrants, ...recentGrants])];
    const pool = priority.length ? priority : grants;
    const grant = pool[this.selectedGrantIndex % pool.length]!;
    this.selectedGrantIndex = (this.selectedGrantIndex + 1) % pool.length;
    const profile = this.data.profiles.find((item) => item.profileKey === grant.profileKey);
    if (!profile) return;
    // Drain the previous bounded page before expanding the durable local event buffer.
    if (grant.pendingEvents.length >= 256) return;
    this.snapshotInProgress = true;
    const generation = this.generation;
    try {
      const provider = await this.ensureProvider(profile);
      const cursor = grant.historyComplete ? null : grant.historyCursor;
      let snapshot = await provider.readSnapshot(grant.threadId, cursor === null ? new Set(Object.keys(grant.candidateTurns)) : new Set(), cursor);
      let advanceHistory = !grant.historyComplete;
      if (cursor !== null && grant.updatedAt && snapshot.thread.updatedAt && grant.updatedAt !== snapshot.thread.updatedAt) {
        // Reconcile the new head first; preserve the older-history cursor for the next idle pass.
        snapshot = await provider.readSnapshot(grant.threadId, new Set(Object.keys(grant.candidateTurns)), null);
        advanceHistory = false;
      }
      if (generation !== this.generation || !this.active) return;
      this.reconcileSnapshot(grant, snapshot, advanceHistory);
      this.lastError = null;
    } catch (error) {
      if (generation !== this.generation || !this.active) return;
      grant.snapshotState = "unknown";
      grant.snapshotMessage = "thread/read 快照不稳定或不可用；外部 GUI 变化状态未知。";
      this.lastError = safeError(error);
      try { this.persist(); } catch { this.lastError = "thread/read 状态未知，且本机安全存储无法保存该状态。"; }
    } finally { this.snapshotInProgress = false; this.onChange(); }
  }

  private reconcileSnapshot(grant: CodexGrant, snapshot: CodexSnapshot, advanceHistory = true): void {
    const turns = snapshot.turns;
    const nextCandidates: Record<string, string> = {};
    const nextKnownItems = { ...grant.knownItems };
    const sorted = [...turns].reverse();
    const appended: CodexEventInput[] = [];
    let bytes = 0;
    let unstable = false;
    let awaitingStable = false;
    let externalWrite = false;
    for (const turn of sorted) {
      if (turn.itemsView !== "full") { unstable = true; break; }
      const signature = hash(JSON.stringify({ id: turn.id, status: turn.status, items: turn.items }));
      const priorSignature = grant.candidateTurns[turn.id];
      nextCandidates[turn.id] = signature;
      if (!terminalTurn(turn.status)) {
        if (turn.status === "inProgress" || turn.status === "unknown") externalWrite = true;
        continue;
      }
      if (priorSignature === undefined) { awaitingStable = true; continue; }
      if (priorSignature !== signature) { externalWrite = true; continue; }
      for (const item of turn.items) {
        const key = `${turn.id}:${item.id}`;
        const fingerprint = codexSnapshotItemFingerprint(item);
        const existingFingerprint = grant.knownItems[key];
        if (existingFingerprint !== undefined) {
          if (existingFingerprint !== fingerprint) { unstable = true; break; }
          continue;
        }
        const mapped = codexSnapshotItemEvents(turn, item);
        if (!mapped.length) mapped.push(itemEvent("unsupported", turn, item, "unsupported", {
          provider_type: eventText(item.type, 128) || "unknown_item",
          diagnostic: `Codex history item type ${eventText(item.type, 128) || "unknown"} has no projection.`,
        }));
        for (const event of mapped) {
          bytes += Buffer.byteLength(JSON.stringify(event));
          if (bytes > MAX_VISIBLE_MESSAGE_BYTES || grant.pendingEvents.length + appended.length >= MAX_LOCAL_EVENTS) { unstable = true; break; }
          appended.push(event);
        }
        if (unstable) break;
        nextKnownItems[key] = fingerprint;
      }
      if (unstable) break;
      const turnEvent = snapshotTurnEvent(turn.status);
      if (turnEvent) {
        const key = `turn:${turn.id}`;
        const fingerprint = hash(turn.status);
        const existingFingerprint = grant.knownItems[key];
        if (existingFingerprint !== undefined && existingFingerprint !== fingerprint) { unstable = true; break; }
        if (existingFingerprint === undefined) {
          const event: CodexEventInput = {
            provider_event_id: turnEventID(turnEvent.kind, turn.id), kind: turnEvent.kind,
            occurred_at: turnCompletedAt(turn), turn_id: turn.id, status: turn.status, payload: { status: turn.status }, source: "snapshot",
          };
          bytes += Buffer.byteLength(JSON.stringify(event));
          if (bytes > MAX_VISIBLE_MESSAGE_BYTES || grant.pendingEvents.length + appended.length >= MAX_LOCAL_EVENTS) { unstable = true; break; }
          appended.push(event);
        }
        nextKnownItems[key] = fingerprint;
      }
    }
    if (unstable) {
      grant.snapshotState = "unknown";
      grant.snapshotMessage = "线程项在相邻快照间发生变化或超出本地缓冲；该次对账未确认完成。";
      try { this.persist(); } catch { this.lastError = "线程项状态未知，且本机安全存储无法保存该状态。"; }
      return;
    }
    const previous = { candidateTurns: grant.candidateTurns, knownItems: grant.knownItems, pendingEvents: grant.pendingEvents,
      updatedAt: grant.updatedAt, status: grant.status, historyComplete: grant.historyComplete, historyCursor: grant.historyCursor,
      snapshotState: grant.snapshotState, snapshotMessage: grant.snapshotMessage };
    grant.pendingEvents = [...grant.pendingEvents, ...appended.filter((event) => !grant.pendingEvents.some((pending) => pending.provider_event_id === event.provider_event_id))];
    grant.candidateTurns = nextCandidates;
    // Keep recheck fingerprints bounded; stable provider IDs plus server ACK handle older duplicates.
    const knownEntries = Object.entries(nextKnownItems);
    grant.knownItems = Object.fromEntries(knownEntries.slice(-10_000));
    if (advanceHistory && !awaitingStable && !externalWrite) {
      grant.historyCursor = snapshot.nextCursor;
      if (snapshot.complete) grant.historyComplete = true;
    }
    grant.updatedAt = snapshot.thread.updatedAt;
    grant.status = normalizeSessionStatus(snapshot.thread.status);
    grant.snapshotState = externalWrite ? "unknown" : awaitingStable ? "pending" : grant.historyComplete ? "available" : "limited";
    grant.snapshotMessage = externalWrite ? "发现正在写入或刚变化的 turn；等待稳定快照后再对账。" : awaitingStable ? "等待相邻 thread/read 快照一致。" : grant.historyComplete ? undefined : `历史回填进行中：每批最多 ${snapshot.turns.length} 轮；更早历史尚未同步。`;
    try { this.persist(); }
    catch {
      grant.candidateTurns = previous.candidateTurns;
      grant.knownItems = previous.knownItems;
      grant.pendingEvents = previous.pendingEvents;
      grant.updatedAt = previous.updatedAt;
      grant.status = previous.status;
      grant.historyComplete = previous.historyComplete;
      grant.historyCursor = previous.historyCursor;
      grant.snapshotState = "unknown";
      grant.snapshotMessage = "快照事件无法写入本机安全存储；对账状态未知。";
      this.lastError = grant.snapshotMessage;
    }
  }

  private queueEvent(grant: CodexGrant, event: CodexEventInput): boolean {
    if (grant.pendingEvents.some((item) => item.provider_event_id === event.provider_event_id)) return true;
    if (grant.pendingEvents.length >= MAX_LOCAL_EVENTS) {
      grant.snapshotState = "unknown";
      grant.snapshotMessage = "本机未确认事件缓冲已满；等待服务端确认前停止缓存新事件。";
      this.lastError = grant.snapshotMessage;
      this.onChange();
      return false;
    }
    const previousEvents = grant.pendingEvents;
    grant.pendingEvents = [...previousEvents, event];
    try { this.persist(); }
    catch {
      grant.pendingEvents = previousEvents;
      this.lastError = "Codex 未确认事件无法保存到系统安全存储；已暂停转发。";
      grant.snapshotState = "unknown";
      this.onChange();
      return false;
    }
    this.onChange();
    return true;
  }

  private requireState(): CodexPersistentState { if (!this.data) throw new Error("Morde Codex profile is unavailable"); return this.data; }
  private requireGrant(sessionID: string): CodexGrant {
    const grant = this.requireState().grants.find((item) => item.id === sessionID);
    if (!grant) throw new Error("Codex session authorization not found");
    return grant;
  }
  private requireProfile(profileKey: string): CodexLocalProfile {
    const profile = this.requireState().profiles.find((item) => item.profileKey === profileKey);
    if (!profile) throw new Error("Codex profile not found");
    return profile;
  }
  private persist(): void {
    if (this.loadFailed) throw new Error("Codex encrypted profile state could not be read; refusing to overwrite it");
    const state = this.requireState();
    if (!validPersistentState(state, state.serviceInstanceID, state.installationId)) throw new Error("Codex profile state is invalid");
    this.persistence.save(state);
  }
}

function normalizeSessionStatus(value: string): CodexDiscoverInput["status"] {
  if (["inProgress", "in_progress", "busy", "running"].includes(value)) return "in_progress";
  if (value === "waiting_approval") return "waiting_approval";
  if (value === "completed") return "completed";
  if (value === "failed") return "failed";
  if (value === "interrupted") return "idle";
  if (["idle", "ready"].includes(value)) return "idle";
  return "unknown";
}

function safeError(error: unknown): string {
  const value = error instanceof Error ? error.message : String(error);
  return value.replace(/(?:\/[^\s:]+){2,}/g, "[本地路径]").slice(0, 240);
}
