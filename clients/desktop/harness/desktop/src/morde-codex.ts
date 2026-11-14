import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import { basename, isAbsolute, resolve } from "node:path";
import type { CodexDiscoverInput, CodexProfileSyncStatus, CodexQueuedCommand, CodexScope, MordeAPI } from "./morde-core";

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
  archived: boolean;
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
  metadataStable: boolean;
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

class CodexAppServerRejection extends Error {
  constructor(readonly code: number) {
    super("Codex App Server rejected the request");
    this.name = "CodexAppServerRejection";
  }
}

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

function threadFrom(value: unknown, archived = false): CodexThread | null {
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
    archived,
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
        if (message.error) pending.reject(new CodexAppServerRejection(message.error.code));
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

  private async listThreadPage(cursor: string | null, archived: boolean): Promise<{ items: CodexThread[]; nextCursor: string | null }> {
    const result = object(await this.request("thread/list", {
      cursor, limit: 100, sortKey: "updated_at", sortDirection: "desc", useStateDbOnly: false, archived,
    }));
    if (!result || !Array.isArray(result.data)) throw new Error("Codex thread list is unavailable");
    const threads = result.data.map((thread) => threadFrom(thread, archived)).filter((thread): thread is CodexThread => thread !== null);
    const rawNext = result.nextCursor;
    if (rawNext !== undefined && rawNext !== null && (typeof rawNext !== "string" || rawNext.length > 4096 || rawNext.includes("\u0000") || rawNext === cursor)) {
      throw new Error("Codex thread pagination is invalid");
    }
    const next = typeof rawNext === "string" ? rawNext : null;
    return { items: threads, nextCursor: next };
  }

  async listThreads(cursor: string | null = null): Promise<{ items: CodexThread[]; nextCursor: string | null }> {
    if (!(cursor === null || typeof cursor === "string" && cursor.length <= 8192 && !cursor.includes("\u0000"))) throw new Error("Invalid Codex thread cursor");
    const phase = cursor?.startsWith("archived:") ? "archived" : "active";
    if (cursor !== null && !cursor.startsWith("active:") && !cursor.startsWith("archived:")) throw new Error("Invalid Codex thread cursor");
    const prefix = `${phase}:`;
    const serverCursor = cursor === null ? null : cursor.slice(prefix.length) || null;
    const page = await this.listThreadPage(serverCursor, phase === "archived");
    if (page.nextCursor) return { items: page.items, nextCursor: `${phase}:${page.nextCursor}` };
    if (phase === "archived") return page;
    const archivedPage = await this.listThreadPage(null, true);
    return {
      items: [...page.items, ...archivedPage.items],
      nextCursor: archivedPage.nextCursor ? `archived:${archivedPage.nextCursor}` : null,
    };
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
    // Thread metadata often changes while an external Codex GUI is writing an active turn.
    // Keep its bounded visible item snapshot for best-effort reconciliation, while callers
    // retain a separate stability gate for terminal turns.
    return { thread: second, turns, complete, anchored, nextCursor, metadataStable: first.updatedAt === second.updatedAt };
  }

  async resume(threadID: string): Promise<void> {
    if (!stableID(threadID)) throw new Error("Invalid Codex thread ID");
    const result = object(await this.request("thread/resume", { threadId: threadID, excludeTurns: true }));
    const thread = threadFrom(result?.thread);
    if (!thread || thread.id !== threadID) throw new Error("Codex thread could not be resumed");
  }

  async startTurn(threadID: string, text: string, clientMessageID: string = randomUUID()): Promise<string> {
    const dispatch = codexTurnDispatch(threadID, text, null, clientMessageID);
    const result = object(await this.request(dispatch.method, dispatch.params, 20_000));
    const turn = object(result?.turn);
    if (!turn || !stableID(turn.id)) throw new Error("Codex did not confirm the turn request");
    return turn.id;
  }

  async activeTurnID(threadID: string): Promise<string | null> {
    if (!stableID(threadID)) throw new Error("Invalid Codex thread ID");
    const result = object(await this.request("thread/turns/list", {
      threadId: threadID, cursor: null, limit: 1, itemsView: "notLoaded", sortDirection: "desc",
    }));
    if (!result || !Array.isArray(result.data)) throw new Error("Codex active turn state is unavailable");
    const active = result.data.map(object).find((turn) => turn !== null && turn.status === "inProgress" && stableID(turn.id));
    const turnID = active?.id;
    return stableID(turnID) ? turnID : null;
  }

  async steerTurn(threadID: string, turnID: string, text: string, clientMessageID: string): Promise<string> {
    const dispatch = codexTurnDispatch(threadID, text, turnID, clientMessageID);
    const result = object(await this.request(dispatch.method, dispatch.params, 20_000));
    const confirmedTurnID = result?.turnId;
    if (!stableID(confirmedTurnID)) throw new Error("Codex did not confirm the steered turn");
    return confirmedTurnID;
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

export type CodexLocalProfile = { profileKey: string; homeDirectory: string; executable: string; createdAt: string; connectionScopes?: CodexScope[] };
export type CodexEventInput = {
  provider_event_id: string;
  kind: "user_message" | "assistant_message" | "assistant_delta" | "reasoning_summary" | "plan" | "turn_started" | "turn_completed" | "turn_failed" | "turn_cancelled" | "tool_call" | "tool_result" | "file_change" | "child_agent" | "question" | "approval_request" | "approval_status" | "unsupported";
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
  questions: Record<string, CodexUserInputRequest>;
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
export type CodexUserInputOption = { label: string; description: string };
export type CodexUserInputQuestion = {
  id: string;
  header: string;
  question: string;
  options: CodexUserInputOption[];
  is_secret: boolean;
  is_other: boolean;
};
export type CodexUserInputRequest = {
  id: string;
  rpcID: number | string;
  version: string;
  profileKey: string;
  threadId: string;
  turnId: string;
  itemId: string;
  grantVersion: number;
  questions: CodexUserInputQuestion[];
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
  profiles: { profileKey: string; label: string; executableLabel: string; selected: boolean; state: string; error: string | null; connectionAuthorized: boolean; scopes: CodexScope[]; syncState: "idle" | "syncing" | "synced" | "partial"; syncTotal: number; syncDone: number; syncError: string | null }[];
  selectedProfileKey: string | null;
  sessions: { id: string; profileKey: string; threadId: string; title: string; cwdLabel: string; updatedAt: string | null; status: string; scopes: CodexScope[]; snapshotState: string; snapshotMessage?: string; historyComplete: boolean; pendingEvents: number; blockedEvents: number }[];
  realtimeCapability: "available" | "unverified";
  running: boolean;
  error: string | null;
};

type CodexAPI = Pick<MordeAPI, "codexDiscover" | "codexClaim" | "codexCommandResult" | "codexEvents" | "codexAuthorizeConnection" | "codexRevokeConnection" | "codexReportSessionDiscoveryStatus">;
type RuntimePersistence = { load: () => unknown | null; save: (value: CodexPersistentState) => void };
type ProfileSyncProgress = { state: "idle" | "syncing" | "synced" | "partial"; total: number; done: number; error: string | null };

const SCOPES = new Set<CodexScope>(["read", "live", "send", "interrupt", "approval"]);
const FINAL_COMMAND_STATUSES = new Set(["forwarded", "running", "waiting_approval", "completed", "failed", "unknown"]);
const PROFILE_EXIT_COMMAND_STATUSES = new Set(["dispatching", "claimed", "forwarded", "running", "waiting_approval"]);
const MAX_LOCAL_EVENTS = 3000;
const MAX_LOCAL_STATE_BYTES = 64 * 1024 * 1024;
const MAX_VISIBLE_MESSAGE_BYTES = 16 * 1024 * 1024;

function isCodexScope(value: unknown): value is CodexScope { return typeof value === "string" && SCOPES.has(value as CodexScope); }

function validRpcID(value: unknown): value is number | string {
  return typeof value === "number" ? Number.isSafeInteger(value) : typeof value === "string" && value.length > 0 &&
    Buffer.byteLength(value) <= 512 && !/[\u0000-\u001f\u007f-\u009f]/.test(value);
}

function validQuestionRequestID(value: unknown): value is string {
  return typeof value === "string" && value.trim().length > 0 && Buffer.byteLength(value) <= 512 && !/[\u0000-\u001f\u007f-\u009f]/.test(value);
}

function validQuestionVersion(value: unknown): value is string {
  return typeof value === "string" && /^[a-f0-9]{64}$/.test(value);
}

function codexUserInputQuestionValid(value: unknown): value is CodexUserInputQuestion {
  const question = object(value);
  return !!question && Object.keys(question).every((key) => ["id", "header", "question", "options", "is_secret", "is_other"].includes(key)) &&
    typeof question.id === "string" && question.id.trim().length > 0 && Buffer.byteLength(question.id) <= 512 && !/[\u0000-\u001f\u007f-\u009f]/.test(question.id) &&
    typeof question.header === "string" && Buffer.byteLength(question.header) <= 4096 && !question.header.includes("\u0000") &&
    typeof question.question === "string" && Buffer.byteLength(question.question) <= 4096 && !question.question.includes("\u0000") &&
    Array.isArray(question.options) && question.options.length <= 64 && question.options.every((value) => {
      const option = object(value);
      return !!option && Object.keys(option).length === 2 && typeof option.label === "string" && Buffer.byteLength(option.label) <= 4096 &&
        typeof option.description === "string" && Buffer.byteLength(option.description) <= 4096 &&
        !option.label.includes("\u0000") && !option.description.includes("\u0000");
    }) && typeof question.is_secret === "boolean" && typeof question.is_other === "boolean";
}

export function codexUserInputQuestions(value: unknown): CodexUserInputQuestion[] | null {
  if (!Array.isArray(value) || value.length < 1 || value.length > 64) return null;
  const questions: CodexUserInputQuestion[] = [];
  const ids = new Set<string>();
  for (const questionValue of value) {
    const row = object(questionValue);
    if (!row || typeof row.id !== "string" || !validRpcID(row.id) || row.id.trim().length === 0 || ids.has(row.id) ||
      typeof row.header !== "string" || Buffer.byteLength(row.header) > 4096 ||
      typeof row.question !== "string" || Buffer.byteLength(row.question) > 4096 ||
      row.isSecret !== undefined && typeof row.isSecret !== "boolean" || row.isOther !== undefined && typeof row.isOther !== "boolean") return null;
    ids.add(row.id);
    const rawOptions = row.options === undefined || row.options === null ? [] : row.options;
    if (!Array.isArray(rawOptions) || rawOptions.length > 64) return null;
    const options: CodexUserInputOption[] = [];
    for (const optionValue of rawOptions) {
      const option = object(optionValue);
      if (!option || typeof option.label !== "string" || Buffer.byteLength(option.label) > 4096 ||
        typeof option.description !== "string" && option.description !== undefined ||
        typeof option.description === "string" && Buffer.byteLength(option.description) > 4096) return null;
      options.push({ label: eventText(option.label, 4096), description: eventText(option.description ?? "", 4096) });
    }
    questions.push({
      id: row.id,
      header: eventText(row.header, 4096),
      question: eventText(row.question, 4096),
      options,
      is_secret: row.isSecret === true,
      is_other: row.isOther === true,
    });
  }
  return Buffer.byteLength(JSON.stringify(questions)) <= 48 * 1024 && questions.every(codexUserInputQuestionValid) ? questions : null;
}

function codexQuestionEventPayloadValid(payload: JsonObject): boolean {
  if (!validQuestionRequestID(payload.question_request_id) || !validQuestionVersion(payload.question_version)) return false;
  if (payload.status === "pending") return Object.keys(payload).length === 4 && Array.isArray(payload.questions) &&
    payload.questions.length > 0 && payload.questions.every(codexUserInputQuestionValid) &&
    new Set(payload.questions.map((question) => (question as CodexUserInputQuestion).id)).size === payload.questions.length;
  return Object.keys(payload).length === 3 && (payload.status === "answered" || payload.status === "stale") && payload.questions === undefined;
}

export function codexUserInputVersion(epoch: string, nonce: string,
  context: Pick<CodexUserInputRequest, "profileKey" | "threadId" | "grantVersion" | "rpcID" | "turnId" | "itemId" | "questions">): string {
  return hash(JSON.stringify({ epoch, nonce, profileKey: context.profileKey, threadId: context.threadId, grantVersion: context.grantVersion,
    rpcID: context.rpcID, turnId: context.turnId, itemId: context.itemId, questions: context.questions }));
}

export function codexQuestionRequestMatches(request: CodexUserInputRequest, profileKey: string, threadId: string,
  grantVersion: number, requestID: string, version: string): boolean {
  return validQuestionRequestID(requestID) && validQuestionVersion(version) && request.id === requestID && request.version === version &&
    request.profileKey === profileKey && request.threadId === threadId && request.grantVersion === grantVersion;
}

export function codexConsumeQuestionRequest(requests: Record<string, CodexUserInputRequest>, profileKey: string, threadId: string,
  grantVersion: number, requestID: string, version: string): CodexUserInputRequest | null {
  if (!Object.hasOwn(requests, requestID)) return null;
  const request = requests[requestID];
  if (!request || !codexQuestionRequestMatches(request, profileKey, threadId, grantVersion, requestID, version)) return null;
  delete requests[requestID];
  return request;
}

export function codexQuestionAnswerResponse(questions: CodexUserInputQuestion[], value: unknown): JsonObject | null {
  const answers = object(value);
  if (!answers || questions.length < 1 || questions.length > 64 || new Set(questions.map((question) => question.id)).size !== questions.length ||
    Object.keys(answers).length !== questions.length) return null;
  const response: [string, { answers: string[] }][] = [];
  for (const question of questions) {
    if (!Object.hasOwn(answers, question.id)) return null;
    const answer = answers[question.id];
    if (!Array.isArray(answer) || answer.length < 1 || answer.length > 64 ||
      !answer.every((item) => typeof item === "string" && item.trim().length > 0 && Buffer.byteLength(item) <= 4096 &&
        !/[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f-\u009f]/.test(item))) return null;
    response.push([question.id, { answers: answer as string[] }]);
  }
  return response.length === Object.keys(answers).length ? { answers: Object.fromEntries(response) } : null;
}

export function codexQuestionEvent(request: CodexUserInputRequest, status: "pending" | "answered" | "stale", occurredAt = new Date().toISOString()): CodexEventInput {
  if (status !== "pending") return codexQuestionStatusEvent(request.id, request.version, status, occurredAt, request.turnId, request.itemId);
  return {
    provider_event_id: `appserver:question:${hash(`${request.id}:${request.version}:${status}`)}`,
    kind: "question", occurred_at: occurredAt, turn_id: request.turnId, item_id: request.itemId,
    payload: {
      question_request_id: request.id, question_version: request.version, status, questions: request.questions,
    },
    source: "live",
  };
}

export function codexQuestionStatusEvent(requestID: string, version: string, status: "answered" | "stale", occurredAt = new Date().toISOString(),
  turnId?: string, itemId?: string): CodexEventInput {
  return {
    provider_event_id: `appserver:question:${hash(`${requestID}:${version}:${status}`)}`,
    kind: "question", occurred_at: occurredAt, ...(stableID(turnId) ? { turn_id: turnId } : {}), ...(stableID(itemId) ? { item_id: itemId } : {}),
    payload: { question_request_id: requestID, question_version: version, status }, source: "live",
  };
}

export function codexQuestionAnswerEvent(request: CodexUserInputRequest, answersValue: unknown, occurredAt = new Date().toISOString()): CodexEventInput | null {
  const answers = object(codexQuestionAnswerResponse(request.questions, answersValue)?.answers);
  if (!answers) return null;
  const text = request.questions.flatMap((question) => {
    if (question.is_secret) return [];
    const answer = object(answers[question.id])?.answers;
    return Array.isArray(answer) && answer.every((item) => typeof item === "string") && answer.length
      ? [`${question.header || question.question}: ${(answer as string[]).join(", ")}`] : [];
  }).join("\n");
  if (!text) return null;
  return {
    provider_event_id: `appserver:question_answer:${hash(`${request.id}:${request.version}`)}`,
    kind: "user_message", occurred_at: occurredAt, turn_id: request.turnId, item_id: request.itemId,
    payload: { text: eventText(text, 32 * 1024) }, source: "live",
  };
}

export function codexCommandsForExitedProfile(commandProfiles: ReadonlyMap<string, string>, commands: Record<string, CodexCommandLedger>, profileKey: string): string[] {
  return [...commandProfiles.entries()]
    .filter(([id, profile]) => profile === profileKey && PROFILE_EXIT_COMMAND_STATUSES.has(commands[id]?.status ?? ""))
    .map(([id]) => id);
}

function eventInputValid(value: unknown): value is CodexEventInput {
  const row = object(value);
  const payload = object(row?.payload);
  const allowedKeys: Record<string, Set<string>> = {
    user_message: new Set(["text", "segment_id"]), assistant_message: new Set(["text", "segment_id"]),
    assistant_delta: new Set(["text", "segment_id", "delta_index"]), reasoning_summary: new Set(["text"]),
    plan: new Set(["summary", "items"]), tool_call: new Set(["tool", "summary", "call_id", "status", "input", "input_part", "input_parts"]),
    tool_result: new Set(["tool", "summary", "output", "call_id", "output_part", "output_parts", "exit_code", "status"]),
    file_change: new Set(["path_label", "change", "summary"]), child_agent: new Set(["agent_label", "status", "summary"]),
    unsupported: new Set(["provider_type", "diagnostic"]), turn_started: new Set(["status"]),
    turn_completed: new Set(["status", "summary"]), turn_failed: new Set(["status", "error_code", "summary"]),
    turn_cancelled: new Set(["status", "summary"]), question: new Set(["question_request_id", "question_version", "status", "questions"]),
    approval_request: new Set(["approval_id", "approval_version", "kind", "summary", "object", "detail", "impact"]),
    approval_status: new Set(["approval_id", "approval_version", "status", "summary"]),
  };
  if (!row || !stableID(row.provider_event_id) || typeof row.kind !== "string" || !allowedKeys[row.kind] ||
    !payload || Object.keys(payload).some((key) => !allowedKeys[row.kind as string]!.has(key)) ||
    !Object.entries(payload).every(([key, item]) => row.kind === "question" && key === "questions" ? Array.isArray(item) && item.every(codexUserInputQuestionValid) :
      typeof item === "string" && Buffer.byteLength(item) <= 32 * 1024 && !item.includes("\u0000") ||
      typeof item === "number" && Number.isFinite(item) || typeof item === "boolean" || Array.isArray(item) && item.length <= 256 && item.every((part) => typeof part === "string" && Buffer.byteLength(part) <= 4096 && !part.includes("\u0000"))) ||
    typeof row.occurred_at !== "string" || !Number.isFinite(Date.parse(row.occurred_at)) ||
    (row.status !== undefined && (typeof row.status !== "string" || row.status.length > 128 || /[\u0000-\u001f\u007f-\u009f]/.test(row.status))) ||
    (row.source !== "snapshot" && row.source !== "live" && row.source !== "approval") ||
    (row.turn_id !== undefined && !stableID(row.turn_id)) || (row.item_id !== undefined && !stableID(row.item_id)) ||
    row.kind === "question" && (row.source !== "live" || !codexQuestionEventPayloadValid(payload)) ||
    row.kind === "tool_call" && (payload.input !== undefined || payload.input_part !== undefined || payload.input_parts !== undefined) &&
      (typeof payload.input !== "string" || Buffer.byteLength(payload.input) > 24 * 1024 || !stableID(payload.call_id) ||
        !Number.isSafeInteger(payload.input_part) || !Number.isSafeInteger(payload.input_parts) ||
        (payload.input_part as number) < 0 || (payload.input_parts as number) < 1 || (payload.input_part as number) >= (payload.input_parts as number)) ||
    row.kind === "tool_result" && payload.call_id !== undefined && !stableID(payload.call_id) ||
    row.kind === "tool_result" && (payload.output_part !== undefined || payload.output_parts !== undefined) &&
      (typeof payload.output !== "string" || Buffer.byteLength(payload.output) > 24 * 1024 || !stableID(payload.call_id) ||
        !Number.isSafeInteger(payload.output_part) || (payload.output_part as number) < 0 ||
        payload.output_parts !== undefined && (!Number.isSafeInteger(payload.output_parts) || (payload.output_parts as number) <= (payload.output_part as number)))) return false;
  return true;
}

function validPersistentState(value: unknown, serviceInstanceID: string, installationId: string): value is CodexPersistentState {
  const state = object(value);
  const commands = object(state?.commands);
  if (!state || state.version !== 1 || state.serviceInstanceID !== serviceInstanceID || state.installationId !== installationId ||
    !Array.isArray(state.profiles) || state.profiles.length > 20 || !Array.isArray(state.grants) ||
    !commands || Object.keys(commands).length > 10000 ||
    !(state.selectedProfileKey === null || typeof state.selectedProfileKey === "string")) return false;
  const keys = new Set<string>();
  for (const value of state.profiles) {
    const profile = object(value);
    if (!profile || typeof profile.profileKey !== "string" || !/^[A-Za-z0-9_-]{1,128}$/.test(profile.profileKey) || keys.has(profile.profileKey) ||
      typeof profile.homeDirectory !== "string" || !(isAbsolute(profile.homeDirectory) || /^[A-Za-z]:[\\/]/.test(profile.homeDirectory)) ||
      typeof profile.executable !== "string" || !(isAbsolute(profile.executable) || /^[A-Za-z]:[\\/]/.test(profile.executable)) ||
      typeof profile.createdAt !== "string" || !Number.isFinite(Date.parse(profile.createdAt)) ||
      profile.connectionScopes !== undefined && (!Array.isArray(profile.connectionScopes) || !profile.connectionScopes.every(isCodexScope) || new Set(profile.connectionScopes).size !== profile.connectionScopes.length)) return false;
    keys.add(profile.profileKey);
  }
  if (state.selectedProfileKey !== null && !keys.has(state.selectedProfileKey)) return false;
  for (const value of state.grants) {
    const grant = object(value);
    const approvals = object(grant?.approvals);
    const questions = object(grant?.questions);
    if (!grant || typeof grant.id !== "string" || !/^l[a-z0-9]{7}$/.test(grant.id) || typeof grant.profileKey !== "string" || !keys.has(grant.profileKey) ||
      !stableID(grant.threadId) || typeof grant.title !== "string" || grant.title.length > 512 || typeof grant.cwdLabel !== "string" || grant.cwdLabel.length > 256 ||
      !(grant.updatedAt === null || typeof grant.updatedAt === "string" && Number.isFinite(Date.parse(grant.updatedAt))) || typeof grant.status !== "string" ||
      !Array.isArray(grant.scopes) || !grant.scopes.every(isCodexScope) || new Set(grant.scopes).size !== grant.scopes.length ||
      !Number.isSafeInteger(grant.grantVersion) || !["pending", "available", "limited", "unknown", "disabled"].includes(String(grant.snapshotState)) || typeof grant.historyComplete !== "boolean" ||
      !(grant.historyCursor === null || typeof grant.historyCursor === "string" && grant.historyCursor.length <= 4096) ||
      !object(grant.candidateTurns) || !object(grant.knownItems) || !Array.isArray(grant.pendingEvents) || !grant.pendingEvents.every(eventInputValid) ||
      grant.pendingEvents.length > MAX_LOCAL_EVENTS || !approvals || !questions) return false;
    for (const [id, approvalValue] of Object.entries(approvals)) {
      const approval = object(approvalValue);
      if (!approval || id !== approval.id || !/^\d{1,24}$/.test(id) && !/^[A-Za-z0-9._~-]{1,128}$/.test(id) ||
        !(typeof approval.rpcID === "string" || typeof approval.rpcID === "number" && Number.isSafeInteger(approval.rpcID)) ||
        typeof approval.version !== "string" || !/^[a-f0-9]{64}$/.test(approval.version) || approval.profileKey !== grant.profileKey || approval.threadId !== grant.threadId ||
        approval.grantVersion !== grant.grantVersion || typeof approval.method !== "string" || !parseApprovalMethod(approval.method) ||
        !object(approval.params) || typeof approval.kind !== "string" || typeof approval.summary !== "string" || typeof approval.createdAt !== "string" || !Number.isFinite(Date.parse(approval.createdAt))) return false;
    }
    if (Object.keys(questions).length > 100) return false;
    for (const [id, requestValue] of Object.entries(questions)) {
      const request = object(requestValue);
      if (!request || id !== request.id || !validRpcID(request.rpcID) || request.id !== String(request.rpcID) ||
        typeof request.version !== "string" || !/^[a-f0-9]{64}$/.test(request.version) || request.profileKey !== grant.profileKey ||
        request.threadId !== grant.threadId || !grant.scopes.includes("live") || !grant.scopes.includes("send") ||
        request.turnId === undefined || !stableID(request.turnId) ||
        request.itemId === undefined || !stableID(request.itemId) || request.grantVersion !== grant.grantVersion ||
        !Array.isArray(request.questions) || request.questions.length < 1 || request.questions.length > 64 ||
        !request.questions.every(codexUserInputQuestionValid) || new Set(request.questions.map((question) => (question as CodexUserInputQuestion).id)).size !== request.questions.length ||
        typeof request.createdAt !== "string" || !Number.isFinite(Date.parse(request.createdAt))) return false;
    }
  }
  for (const [id, value] of Object.entries(commands)) {
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
      return grant ? {
        ...(!Object.hasOwn(grant, "historyCursor") ? { historyCursor: null } : {}),
        ...(!Object.hasOwn(grant, "questions") ? { questions: {} } : {}),
        ...grant,
      } : grantValue;
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

export function codexTurnDispatch(threadID: string, text: string, activeTurnID: string | null, clientMessageID: string): { method: "turn/start" | "turn/steer"; params: JsonObject } {
  if (!stableID(threadID) || typeof text !== "string" || !text.trim() || text.length > 32768 || !stableID(clientMessageID) ||
    !(activeTurnID === null || stableID(activeTurnID))) throw new Error("Invalid Codex message");
  const input = [{ type: "text", text }];
  return activeTurnID === null
    ? { method: "turn/start", params: { threadId: threadID, input, clientUserMessageId: clientMessageID } }
    : { method: "turn/steer", params: { threadId: threadID, expectedTurnId: activeTurnID, input, clientUserMessageId: clientMessageID } };
}

export function codexApprovalVersion(epoch: string, method: string, params: JsonObject): string {
  return hash(JSON.stringify({ epoch, method, params }));
}

export function codexApprovalStatusEvent(approval: Pick<CodexApprovalRequest, "id" | "version" | "params">, status: string, summary?: string): CodexEventInput {
  const turnID = approval.params.turnId;
  return {
    provider_event_id: `morde:approval:${hash(`${approval.id}:${approval.version}:${status}`)}`,
    kind: "approval_status", occurred_at: new Date().toISOString(),
    ...(stableID(turnID) ? { turn_id: turnID } : {}),
    payload: { approval_id: approval.id, approval_version: approval.version, status, ...(summary ? { summary: eventText(summary, 4096) } : {}) },
    source: "approval",
  };
}

export function codexUnsupportedNotificationEvent(method: string, threadID: string, params: JsonObject, occurredAt = new Date().toISOString()): CodexEventInput {
  const turnID = stableID(params.turnId) ? params.turnId : undefined;
  const itemID = stableID(params.itemId) ? params.itemId : undefined;
  const safeMethod = /^[a-zA-Z][A-Za-z0-9/_-]{0,127}$/.test(method) ? method : "unknown";
  return {
    provider_event_id: `appserver:unsupported:${hash(`${threadID}\0${safeMethod}\0${turnID ?? ""}\0${itemID ?? ""}`)}`,
    kind: "unsupported", occurred_at: occurredAt,
    ...(turnID ? { turn_id: turnID } : {}), ...(itemID ? { item_id: itemID } : {}),
    payload: { provider_type: "app_server_notification", diagnostic: `Morde 暂不支持 Codex App Server 通知：${safeMethod}` },
    source: "live",
  };
}

export function codexErrorNotificationEvent(threadID: string, params: JsonObject, occurredAt = new Date().toISOString()): { turnID: string; willRetry: boolean; event: CodexEventInput } | null {
  const turnID = params.turnId;
  const error = object(params.error);
  if (!stableID(turnID) || typeof params.willRetry !== "boolean" || typeof error?.message !== "string") return null;
  const additionalDetails = typeof error.additionalDetails === "string" ? error.additionalDetails.trim() : "";
  const detail = eventText(additionalDetails || error.message, 4096) || "Codex 返回了错误";
  return params.willRetry
    ? {
        turnID, willRetry: true,
        event: {
          provider_event_id: codexProviderEventID("app_server_retry", turnID, `error:${threadID}`), kind: "unsupported", occurred_at: occurredAt,
          turn_id: turnID, payload: { provider_type: "app_server_error", diagnostic: eventText(`Codex 遇到临时错误，App Server 将自动重试：${detail}`, 4096) }, source: "live",
        },
      }
    : {
        turnID, willRetry: false,
        event: {
          provider_event_id: codexProviderEventID("turn_failed", turnID, `error:${threadID}`), kind: "turn_failed", occurred_at: occurredAt,
          turn_id: turnID, status: "failed", payload: { status: "failed", error_code: "codex_error", summary: detail }, source: "live",
        },
      };
}

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
    for (const value of item.results) {
      const row = object(value);
      if (!row) continue;
      const summary = [row.title, row.url, row.snippet, row.description].filter((part): part is string => typeof part === "string").join("\n");
      if (summary) texts.push(summary);
    }
  }
  return texts.join("\n");
}

function hasUnprojectedMedia(item: JsonObject): boolean {
  const result = object(item.result);
  const arrays = [item.content, item.contentItems, Array.isArray(item.output) ? item.output : undefined, result?.content]
    .filter(Array.isArray) as unknown[][];
  return arrays.some((items) => items.some((value) => {
    if (typeof value === "string") return false;
    const row = object(value);
    return !row || typeof row.text !== "string" && typeof row.title !== "string";
  }));
}

function itemEvent(
  kind: CodexEventInput["kind"], turn: CodexTurn, item: JsonObject, suffix: string,
  payload: Record<string, unknown>, status?: string, fingerprint = codexSnapshotItemFingerprint(item),
): CodexEventInput {
  const event: CodexEventInput = {
    // Including the source item fingerprint gives each visible version a stable ID.
    // Cloud dedupe can then collapse identical live/snapshot/final projections without
    // dropping a changed active snapshot.
    provider_event_id: codexItemEventID(kind, turn.id, `${item.id}:${suffix}:${fingerprint}`), kind,
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
  const normalizedItem = { ...item, id };
  const fingerprint = codexSnapshotItemFingerprint(normalizedItem);
  const snapshotEvent = (kind: CodexEventInput["kind"], suffix: string, payload: Record<string, unknown>, status?: string) =>
    itemEvent(kind, itemTurn, normalizedItem, suffix, payload, status, fingerprint);
  const events: CodexEventInput[] = [];
  const pushText = (kind: "user_message" | "assistant_message", text: string) => {
    const chunks = boundedMessageChunks(text);
    chunks.forEach((chunk, index) => {
      // Snapshot chunks stay in the assistant_message family. iOS groups each stable
      // segment_id and keeps the newest version of that segment before joining chunks.
      events.push(snapshotEvent(kind, `message:${index}`, {
        text: chunk,
        ...(chunks.length > 1 || kind === "assistant_message" ? { segment_id: `${id}:${index}` } : {}),
      }));
    });
  };
  const pushOutput = (tool: string, summary: string, output: string, status: string, exitCode?: number) => {
    const chunks = boundedMessageChunks(output);
    chunks.forEach((chunk, index) => events.push(snapshotEvent("tool_result", `result:${index}`, {
      tool: eventText(tool, 128) || "codex_tool", summary: eventText(summary, 12 * 1024), output: chunk,
      call_id: id, output_part: index, output_parts: chunks.length,
      ...(typeof exitCode === "number" && Number.isFinite(exitCode) ? { exit_code: exitCode } : {}),
      status: nativeStatus(status),
    }, status)));
  };
  const pushTool = (tool: string, summary: string, status: string, input: unknown = {}) => {
    const encodedInput = typeof input === "string" ? JSON.stringify({ input }) : JSON.stringify(input);
    const chunks = boundedMessageChunks(encodedInput);
    chunks.forEach((chunk, index) => events.push(snapshotEvent("tool_call", `call:${index}`, {
      tool: eventText(tool, 128) || "codex_tool", summary: eventText(summary), call_id: id, status: nativeStatus(status),
      input: chunk, input_part: index, input_parts: chunks.length,
    }, status)));
  };

  const text = visibleText(item);
  if (item.type === "userMessage" && text !== null) pushText("user_message", text);
  else if (item.type === "agentMessage" && text !== null) pushText("assistant_message", text);
  if (item.type === "userMessage" && hasUnprojectedMedia(item)) {
    events.push(snapshotEvent("unsupported", "attachment", {
      provider_type: "attachment", diagnostic: "此 Codex 用户消息包含未同步的图片或附件；当前 App Server 历史接口未提供可读取的附件内容。",
    }));
  }

  if (item.type === "reasoning") {
    // Codex exposes both `summary` and private `content`; only its user-visible summary is projected.
    const summary = Array.isArray(item.summary) ? item.summary.filter((part): part is string => typeof part === "string").join("\n") : "";
    boundedMessageChunks(summary).forEach((chunk, index) => {
      if (summary) events.push(snapshotEvent("reasoning_summary", `summary:${index}`, { text: chunk }));
    });
    if (!summary && Array.isArray(item.content) && item.content.length > 0) events.push(snapshotEvent("unsupported", "reasoning_summary_unavailable", {
      provider_type: "reasoning_summary", diagnostic: "Codex 未提供可展示的推理摘要；私有推理内容不会同步。",
    }));
  } else if (item.type === "plan") {
    const summary = typeof item.text === "string" ? item.text : "";
    boundedMessageChunks(summary).forEach((chunk, index) => {
      events.push(snapshotEvent("plan", `plan:${index}`, { summary: chunk, items: [] }));
    });
  } else if (item.type === "commandExecution") {
    const status = nativeStatus(item.status);
    const command = eventText(item.command, 24 * 1024);
    pushTool("command", command, status, { command: item.command ?? "", cwd: item.cwd ?? "" });
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
      events.push(snapshotEvent("file_change", `file:${index}`, {
        path_label: pathLabel, change: action, summary: `Codex ${action}`,
      }, nativeStatus(item.status)));
    }
  } else if (item.type === "mcpToolCall" || item.type === "dynamicToolCall") {
    const status = nativeStatus(item.status);
    const tool = item.type === "mcpToolCall" ? `${String(item.server ?? "mcp")}/${String(item.tool ?? "tool")}` : String(item.tool ?? "tool");
    pushTool(tool, tool, status, item.arguments ?? item.input ?? item.params ?? {});
    pushOutput(tool, tool, itemOutput(item), status);
    if (hasUnprojectedMedia(item)) events.push(snapshotEvent("unsupported", "tool_media", {
      provider_type: "tool_media", diagnostic: "此 Codex 工具结果包含未同步的图片或附件；当前 App Server 历史接口未提供可读取的附件内容。",
    }));
  } else if (item.type === "webSearch") {
    const query = eventText(item.query, 12 * 1024);
    pushTool("web_search", query, "completed", { query: item.query ?? "" });
    pushOutput("web_search", query, itemOutput(item), "completed");
  } else if (item.type === "collabAgentToolCall") {
    const states = object(item.agentsStates) ?? {};
    const entries = Object.entries(states);
    if (entries.length) {
      for (const [index, [label, value]] of entries.entries()) {
        const state = object(value);
        const status = nativeStatus(state?.status);
        events.push(snapshotEvent("child_agent", `agent:${hash(label).slice(0, 12)}`, {
          agent_label: `agent-${hash(label).slice(0, 8)}`, status,
          summary: eventText(state?.message ?? `${String(item.tool ?? "subagent")} ${status}`, 12 * 1024),
        }, status));
      }
    } else {
      const status = nativeStatus(item.status);
      events.push(snapshotEvent("child_agent", "agent", {
        agent_label: `agent-${hash(String(item.receiverThreadIds ?? item.id)).slice(0, 8)}`, status,
        summary: eventText(`${String(item.tool ?? "subagent")} ${status}`, 12 * 1024),
      }, status));
    }
  } else if (item.type === "subAgentActivity") {
    const status = nativeStatus(item.kind);
    events.push(snapshotEvent("child_agent", "activity", {
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
        "thread/read 快照只包含问题内容，不包含对应的原始 RPC handle；若问题由其他 Codex App Server 发起，Morde 无法通过此快照回答，请在原 Codex 客户端处理。",
      ].filter(Boolean).join("\n");
      events.push(snapshotEvent("unsupported", `question:${index}`, {
        provider_type: "user_question", diagnostic: eventText(diagnostic),
      }));
    }
  } else if (item.type === "imageView" || item.type === "imageGeneration") {
    events.push(snapshotEvent("unsupported", "image", {
      provider_type: "image", diagnostic: "Codex 会话包含图片内容，但当前 App Server 历史接口未提供可读取的图片数据。",
    }));
  } else if (["functionCallOutput", "sleep"].includes(String(item.type))) {
    const label = String(item.type);
    const details = item.type === "functionCallOutput" ? itemOutput(item) : `${label} item recorded by Codex.`;
    pushTool(label, label, nativeStatus(item.status ?? "completed"), item.arguments ?? item.input ?? {});
    pushOutput(label, label, details, nativeStatus(item.status ?? "completed"));
  } else if (item.type === "hookPrompt" || item.type === "contextCompaction") {
    events.push(snapshotEvent("unsupported", String(item.type), {
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
  const command = visibleLabel(params.command, 160) || null;
  const reason = visibleLabel(params.reason, 160) || null;
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
    const paths = fileSystem?.[access];
    for (const path of (Array.isArray(paths) ? paths : []).slice(0, 20)) {
      const label = approvalPathLabel(path);
      if (label) requested.push(`${access}: ${label}`);
    }
  }
  const entries = fileSystem?.entries;
  for (const value of (Array.isArray(entries) ? entries : []).slice(0, 20)) {
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
  const reason = visibleLabel(params.reason, 4096) || null;
  const command = visibleLabel(params.command, 4096) || null;
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
  private discoveryTimer: NodeJS.Timeout | null = null;
  private readonly profileSyncStates = new Map<string, ProfileSyncProgress>();
  private readonly profilesSyncing = new Set<string>();
  private readonly profileSyncEpochs = new Map<string, number>();
  private readonly profileMutationTails = new Map<string, Promise<void>>();
  private readonly profileStatusTimers = new Map<string, NodeJS.Timeout>();
  private readonly profileStatusLatest = new Map<string, CodexProfileSyncStatus>();
  private readonly profileStatusInFlight = new Map<string, number>();
  private readonly profileStatusReported = new Map<string, string>();
  private claimInProgress = false;
  private snapshotInProgress = false;
  private flushInProgress = false;
  private selectedGrantIndex = 0;
  private active = false;
  private loadFailed = false;
  private lastError: string | null = null;
  private deltaIndices = new Map<string, number>();
  private completedTurns = new Set<string>();
  private failedTurns = new Set<string>();
  private activeCommandProfiles = new Map<string, string>();
  private generation = 0;
  private approvalEpoch = randomUUID();

  constructor(private readonly api: CodexAPI, private readonly persistence: RuntimePersistence, private readonly onChange: () => void) {}

  load(serviceInstanceID: string, installationId: string): void {
    this.generation++;
    this.active = false;
    if (this.claimTimer) clearInterval(this.claimTimer);
    if (this.flushTimer) clearInterval(this.flushTimer);
    if (this.snapshotTimer) clearInterval(this.snapshotTimer);
    if (this.discoveryTimer) clearInterval(this.discoveryTimer);
    this.claimTimer = this.flushTimer = this.snapshotTimer = this.discoveryTimer = null;
    for (const timer of this.profileStatusTimers.values()) clearTimeout(timer);
    this.profileStatusTimers.clear();
    this.profileStatusLatest.clear();
    this.profileStatusInFlight.clear();
    this.profileStatusReported.clear();
    this.profileSyncStates.clear();
    this.profilesSyncing.clear();
    this.profileSyncEpochs.clear();
    const oldProviders = new Map(this.providers);
    this.activeCommandProfiles.clear();
    for (const grant of this.data?.grants ?? []) {
      this.invalidateGrantApprovals(grant, oldProviders.get(grant.profileKey), true);
      this.invalidateGrantQuestions(grant, oldProviders.get(grant.profileKey), true);
    }
    this.providers.clear();
    for (const provider of oldProviders.values()) void provider.stop().catch(() => undefined);
    this.approvalEpoch = randomUUID();
    this.providerStates.clear();
    this.deltaIndices.clear();
    this.completedTurns.clear();
    this.failedTurns.clear();
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
    for (const grant of this.data.grants) {
      this.invalidateGrantApprovals(grant, undefined, false);
      this.invalidateGrantQuestions(grant, undefined, false);
    }
  }

  async start(): Promise<void> {
    if (!this.data || this.active || this.loadFailed) return;
    this.approvalEpoch = randomUUID();
    this.active = true;
    for (const profile of this.data.profiles) {
      try { await this.ensureProvider(profile); }
      catch (error) { this.lastError = safeError(error); }
    }
    this.claimTimer = setInterval(() => { void this.claimCommands(); }, 2000);
    this.flushTimer = setInterval(() => { void this.flushPending(); }, 1500);
    this.snapshotTimer = setInterval(() => { void this.snapshotOne(); }, 500);
    this.discoveryTimer = setInterval(() => { void this.discoverConnectedProfiles(); }, 30_000);
    void this.discoverConnectedProfiles();
    await this.recoverUnknownCommands();
    this.onChange();
  }

  async stop(): Promise<void> {
    this.generation++;
    this.active = false;
    this.deltaIndices.clear();
    this.completedTurns.clear();
    this.failedTurns.clear();
    if (this.claimTimer) clearInterval(this.claimTimer);
    if (this.flushTimer) clearInterval(this.flushTimer);
    if (this.snapshotTimer) clearInterval(this.snapshotTimer);
    if (this.discoveryTimer) clearInterval(this.discoveryTimer);
    this.claimTimer = this.flushTimer = this.snapshotTimer = this.discoveryTimer = null;
    for (const timer of this.profileStatusTimers.values()) clearTimeout(timer);
    this.profileStatusTimers.clear();
    this.profileStatusLatest.clear();
    this.profilesSyncing.clear();
    const providers = new Map(this.providers);
    for (const grant of this.data?.grants ?? []) {
      this.invalidateGrantApprovals(grant, providers.get(grant.profileKey), true);
      this.invalidateGrantQuestions(grant, providers.get(grant.profileKey), true);
    }
    this.providers.clear();
    for (const item of Object.values(this.data?.commands ?? {})) {
      if (["dispatching", "claimed", "running", "forwarded", "waiting_approval"].includes(item.status)) {
        item.status = "unknown";
        item.resultCode = "interrupted_by_restart";
        item.resultPending = true;
      }
    }
    this.activeCommandProfiles.clear();
    try { this.persist(); } catch { this.lastError = "Codex 命令恢复状态未能保存；操作结果可能未知。"; }
    for (const provider of providers.values()) await provider.stop().catch(() => undefined);
    for (const profile of this.data?.profiles ?? []) this.providerStates.set(profile.profileKey, { state: "stopped", error: null });
    this.onChange();
  }

  publicState(): CodexRuntimeState {
    const state = this.data;
    return {
      profiles: (state?.profiles ?? []).map((profile) => {
        const provider = this.providerStates.get(profile.profileKey);
        const sync = this.profileSyncStates.get(profile.profileKey);
        return {
          profileKey: profile.profileKey, label: basename(profile.homeDirectory), executableLabel: basename(profile.executable),
          selected: profile.profileKey === state?.selectedProfileKey,
          state: provider?.state ?? "stopped", error: provider?.error ?? null,
          connectionAuthorized: Boolean(profile.connectionScopes?.length), scopes: profile.connectionScopes ?? [],
          syncState: sync?.state ?? "idle", syncTotal: sync?.total ?? 0, syncDone: sync?.done ?? 0, syncError: sync?.error ?? null,
        };
      }),
      selectedProfileKey: state?.selectedProfileKey ?? null,
      sessions: (state?.grants ?? []).map((grant) => ({
        id: grant.id, profileKey: grant.profileKey, threadId: grant.threadId, title: grant.title, cwdLabel: grant.cwdLabel,
        updatedAt: grant.updatedAt, status: grant.status, scopes: grant.scopes, snapshotState: grant.snapshotState,
        historyComplete: grant.historyComplete,
        pendingEvents: grant.pendingEvents.length,
        blockedEvents: grant.pendingEvents.filter((event) => event.source === "snapshot" ? !grant.scopes.includes("read") : event.source === "approval" ? !grant.scopes.includes("approval") : !grant.scopes.includes("live")).length,
        ...(grant.snapshotMessage ? { snapshotMessage: grant.snapshotMessage } : {}),
      })),
      realtimeCapability: "unverified",
      running: this.active && !this.loadFailed,
      error: this.lastError,
    };
  }

  listThreads(cursor: string | null = null): Promise<{ items: { id: string; title: string; cwdLabel: string; updatedAt: string | null; status: string; authorized: boolean; archived: boolean }[]; nextCursor: string | null }> {
    const selected = this.data?.profiles.find((profile) => profile.profileKey === this.data?.selectedProfileKey);
    if (!selected) throw new Error("请先选择 CODEX_HOME profile");
    return this.ensureProvider(selected).then(async (provider) => {
      const result = await provider.listThreads(cursor);
      return { items: result.items.map((thread) => ({
        id: thread.id, title: thread.name, cwdLabel: safeCwdLabel(thread.cwd), updatedAt: thread.updatedAt,
        status: thread.status, authorized: Boolean(this.data?.grants.some((grant) => grant.profileKey === selected.profileKey && grant.threadId === thread.id)),
        archived: thread.archived,
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
    const provider = await this.ensureProvider(profile);
    let cursor: string | null = null;
    let thread: CodexThread | undefined;
    do {
      const page = await provider.listThreads(cursor);
      thread = page.items.find((item) => item.id === threadID);
      cursor = thread ? null : page.nextCursor;
    } while (!thread && cursor);
    if (!thread) throw new Error("线程不在当前 Codex profile 的可授权列表中");
    const authorized = await this.authorizeListedThread(profile, thread, scopesValue as CodexScope[]);
    if (!authorized) throw new Error("Codex profile authorization changed before the session was added");
    return authorized;
  }

  async authorizeProfile(profileKey: string, scopesValue: unknown): Promise<void> {
    return this.serializeProfileMutation(profileKey, async () => {
      if (!Array.isArray(scopesValue) || !scopesValue.every(isCodexScope) || new Set(scopesValue).size !== scopesValue.length ||
        !scopesValue.includes("read") || !scopesValue.includes("live")) throw new Error("连接 Codex 需要授权读取历史和同步新事件");
      const profile = this.requireProfile(profileKey);
      const generation = this.generation;
      const epoch = this.bumpProfileSyncEpoch(profileKey);
      const connection = await this.api.codexAuthorizeConnection(profileKey, scopesValue as CodexScope[]);
      if (generation !== this.generation) return;
      if (this.profileSyncEpochs.get(profileKey) !== epoch) {
        await this.api.codexRevokeConnection(profileKey).catch(() => undefined);
        throw new Error("Codex 配置授权已被后续操作取代");
      }
      const previousScopes = profile.connectionScopes;
      profile.connectionScopes = connection.scopes;
      this.profileSyncStates.set(profileKey, { state: "syncing", total: 0, done: 0, error: null });
      try { this.persist(); }
      catch (error) {
        profile.connectionScopes = previousScopes;
        await this.api.codexRevokeConnection(profileKey).catch(() => undefined);
        throw error;
      }
      this.onChange();
      this.reportProfileSessionDiscovery(profileKey, true);
      void this.syncProfileThreads(profile, true);
    });
  }

  async revokeProfile(profileKey: string): Promise<void> {
    return this.serializeProfileMutation(profileKey, async () => {
      const profile = this.requireProfile(profileKey);
      const generation = this.generation;
      const epoch = this.bumpProfileSyncEpoch(profileKey);
      await this.api.codexRevokeConnection(profileKey);
      if (generation !== this.generation) return;
      if (this.profileSyncEpochs.get(profileKey) !== epoch) return;
      profile.connectionScopes = [];
      this.profileSyncStates.set(profileKey, { state: "idle", total: 0, done: 0, error: null });
      const statusTimer = this.profileStatusTimers.get(profileKey);
      if (statusTimer) clearTimeout(statusTimer);
      this.profileStatusTimers.delete(profileKey);
      this.profileStatusLatest.delete(profileKey);
      this.profileStatusReported.delete(profileKey);
      for (const grant of this.requireState().grants.filter((item) => item.profileKey === profileKey)) {
        this.invalidateGrantApprovals(grant, this.providers.get(profileKey), true);
        this.invalidateGrantQuestions(grant, this.providers.get(profileKey), true);
        grant.scopes = [];
        grant.snapshotState = "disabled";
        grant.snapshotMessage = "Codex 配置授权已撤销；本机未确认事件保留且暂停上传。";
      }
      this.persist();
      this.onChange();
    });
  }

  private async serializeProfileMutation<T>(profileKey: string, mutation: () => Promise<T>): Promise<T> {
    const previous = this.profileMutationTails.get(profileKey) ?? Promise.resolve();
    let release!: () => void;
    const current = new Promise<void>((resolve) => { release = resolve; });
    this.profileMutationTails.set(profileKey, current);
    await previous;
    try { return await mutation(); }
    finally {
      release();
      if (this.profileMutationTails.get(profileKey) === current) this.profileMutationTails.delete(profileKey);
    }
  }

  private async authorizeListedThread(profile: CodexLocalProfile, thread: CodexThread, scopes: CodexScope[], syncFence?: { epoch: number; scopesKey: string }): Promise<{ id: string; grantVersion: number } | null> {
    if (syncFence && !this.profileSyncCurrent(profile, syncFence.epoch, syncFence.scopesKey)) return null;
    const state = this.requireState();
    const old = state.grants.find((grant) => grant.profileKey === profile.profileKey && grant.threadId === thread.id);
    if (old) {
      if (scopes.length !== old.scopes.length || scopes.some((scope) => !old.scopes.includes(scope))) await this.updateScopes(old.id, scopes, syncFence);
      if (syncFence && !this.profileSyncCurrent(profile, syncFence.epoch, syncFence.scopesKey)) return null;
      return { id: old.id, grantVersion: old.grantVersion };
    }
    const provider = await this.ensureProvider(profile);
    if (syncFence && !this.profileSyncCurrent(profile, syncFence.epoch, syncFence.scopesKey)) return null;
    const snapshot = await provider.readSnapshot(thread.id);
    if (syncFence && !this.profileSyncCurrent(profile, syncFence.epoch, syncFence.scopesKey)) return null;
    const cwdLabel = safeCwdLabel(snapshot.thread.cwd);
    const discover: CodexDiscoverInput = {
      profile_key: profile.profileKey, thread_id: snapshot.thread.id, title: snapshot.thread.name.slice(0, 512),
      cwd_label: cwdLabel.slice(0, 256), updated_at: snapshot.thread.updatedAt,
      status: normalizeSessionStatus(snapshot.thread.status), scopes,
    };
    const response = await this.api.codexDiscover(discover);
    if (syncFence && !this.profileSyncCurrent(profile, syncFence.epoch, syncFence.scopesKey)) return null;
    const grant: CodexGrant = old ?? {
      id: response.id, profileKey: profile.profileKey, threadId: thread.id, title: discover.title, cwdLabel, updatedAt: discover.updated_at,
      status: discover.status, scopes: response.scopes, grantVersion: response.grant_version,
      snapshotState: "pending", historyComplete: false, historyCursor: null, candidateTurns: {}, knownItems: {}, pendingEvents: [], approvals: {}, questions: {},
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

  private async discoverConnectedProfiles(): Promise<void> {
    if (!this.active || !this.data) return;
    for (const profile of this.data.profiles) {
      if (profile.connectionScopes?.length) void this.syncProfileThreads(profile, false);
    }
  }

  private async syncProfileThreads(profile: CodexLocalProfile, updateExisting: boolean): Promise<void> {
    if (!this.active || !profile.connectionScopes?.length || this.profilesSyncing.has(profile.profileKey)) return;
    const generation = this.generation;
    const scopes = [...profile.connectionScopes];
    const scopesKey = JSON.stringify(scopes);
    const epoch = this.profileSyncEpochs.get(profile.profileKey) ?? 0;
    const fence = { epoch, scopesKey };
    const progress: ProfileSyncProgress = { state: "syncing", total: 0, done: 0, error: null };
    this.profileSyncStates.set(profile.profileKey, progress);
    this.profilesSyncing.add(profile.profileKey);
    this.onChange();
    this.reportProfileSessionDiscovery(profile.profileKey, true);
    try {
      const provider = await this.ensureProvider(profile);
      let cursor: string | null = null;
      const seen = new Set<string>();
      let failures = 0;
      do {
        if (!this.profileSyncCurrent(profile, epoch, scopesKey, generation)) return;
        const page = await provider.listThreads(cursor);
        if (!this.profileSyncCurrent(profile, epoch, scopesKey, generation)) return;
        for (const thread of page.items) {
          if (seen.has(thread.id)) continue;
          seen.add(thread.id);
          progress.total++;
          const old = this.data?.grants.find((grant) => grant.profileKey === profile.profileKey && grant.threadId === thread.id);
          try {
            if (!old || updateExisting && (old.scopes.length !== scopes.length || scopes.some((scope) => !old.scopes.includes(scope)))) {
              await this.authorizeListedThread(profile, thread, scopes, fence);
            }
          } catch (error) {
            if (!this.profileSyncCurrent(profile, epoch, scopesKey, generation)) return;
            failures++;
            progress.error = `${thread.id}: ${safeError(error)}`;
          }
          if (!this.profileSyncCurrent(profile, epoch, scopesKey, generation)) return;
          progress.done++;
          if (progress.done % 10 === 0) {
            this.onChange();
            this.reportProfileSessionDiscovery(profile.profileKey);
          }
        }
        cursor = page.nextCursor;
      } while (cursor);
      progress.state = failures ? "partial" : "synced";
    } catch (error) {
      if (this.profileSyncCurrent(profile, epoch, scopesKey, generation)) {
        progress.state = "partial";
        progress.error = safeError(error);
        this.lastError = progress.error;
      }
    } finally {
      if (generation === this.generation) {
        this.profilesSyncing.delete(profile.profileKey);
        const current = this.profileSyncEpochs.get(profile.profileKey) ?? 0;
        const connected = Boolean(profile.connectionScopes?.length);
        const superseded = current !== epoch || JSON.stringify(profile.connectionScopes ?? []) !== scopesKey;
        if (current === epoch) {
          this.profileSyncStates.set(profile.profileKey, progress);
          if (connected) this.reportProfileSessionDiscovery(profile.profileKey, true);
        }
        this.onChange();
        if (this.active && connected && superseded) {
          void this.syncProfileThreads(profile, true);
        }
      }
    }
  }

  private bumpProfileSyncEpoch(profileKey: string): number {
    const epoch = (this.profileSyncEpochs.get(profileKey) ?? 0) + 1;
    this.profileSyncEpochs.set(profileKey, epoch);
    return epoch;
  }

  private reportProfileSessionDiscovery(profileKey: string, immediate = false): void {
    if (typeof this.api.codexReportSessionDiscoveryStatus !== "function") return;
    const state = this.data;
    const profile = state?.profiles.find((item) => item.profileKey === profileKey);
    if (!this.active || !state || !profile?.connectionScopes?.length) return;
    const grants = state.grants.filter((item) => item.profileKey === profileKey && item.scopes.includes("read"));
    const progress = this.profileSyncStates.get(profileKey);
    const status: CodexProfileSyncStatus = {
      session_discovery: {
        state: progress?.state === "partial" ? "partial" : progress?.state === "synced" ? "synced" : "syncing",
        error_code: progress?.state === "partial" && progress.error ? "unknown" : null,
      },
      history_sync: {
        discovered_sessions: Math.max(progress?.total ?? 0, grants.length),
        history_complete_sessions: grants.filter((item) => item.historyComplete).length,
        pending_upload_event_count: grants.reduce((count, item) => count + item.pendingEvents.length, 0),
      },
    };
    const signature = JSON.stringify(status);
    if (this.profileStatusReported.get(profileKey) === signature) return;
    this.profileStatusLatest.set(profileKey, status);
    if (this.profileStatusInFlight.has(profileKey)) return;
    const existing = this.profileStatusTimers.get(profileKey);
    if (existing && !immediate) return;
    if (existing) clearTimeout(existing);
    const reportGeneration = this.generation;
    const reportState = state;
    const timer = setTimeout(() => {
      this.profileStatusTimers.delete(profileKey);
      const latest = this.profileStatusLatest.get(profileKey);
      if (!latest || !this.active || this.generation !== reportGeneration || this.data !== reportState || !profile.connectionScopes?.length) return;
      const sentSignature = JSON.stringify(latest);
      this.profileStatusInFlight.set(profileKey, reportGeneration);
      void this.api.codexReportSessionDiscoveryStatus(profileKey, latest).then(() => {
        if (this.generation === reportGeneration && this.data === reportState) this.profileStatusReported.set(profileKey, sentSignature);
      }).catch(() => undefined).finally(() => {
        if (this.profileStatusInFlight.get(profileKey) !== reportGeneration) return;
        this.profileStatusInFlight.delete(profileKey);
        if (this.generation !== reportGeneration || this.data !== reportState || !this.active) return;
        const next = this.profileStatusLatest.get(profileKey);
        if (next && JSON.stringify(next) !== sentSignature) this.reportProfileSessionDiscovery(profileKey);
        else if (this.profileStatusReported.get(profileKey) !== sentSignature) this.profileStatusLatest.delete(profileKey);
      });
    }, immediate ? 0 : 5000);
    this.profileStatusTimers.set(profileKey, timer);
  }

  private profileSyncCurrent(profile: CodexLocalProfile, epoch: number, scopesKey: string, generation = this.generation): boolean {
    return this.active && generation === this.generation && this.profileSyncEpochs.get(profile.profileKey) === epoch &&
      JSON.stringify(profile.connectionScopes ?? []) === scopesKey && Boolean(profile.connectionScopes?.length);
  }

  async updateScopes(sessionID: string, scopesValue: unknown, syncFence?: { epoch: number; scopesKey: string }): Promise<void> {
    if (!Array.isArray(scopesValue) || !scopesValue.every(isCodexScope) || new Set(scopesValue).size !== scopesValue.length) throw new Error("Invalid Codex scope selection");
    const grant = this.requireGrant(sessionID);
    const profile = this.requireProfile(grant.profileKey);
    if (syncFence && !this.profileSyncCurrent(profile, syncFence.epoch, syncFence.scopesKey)) return;
    const requested = scopesValue as CodexScope[];
    grant.scopes = grant.scopes.filter((scope) => requested.includes(scope));
    this.invalidateGrantQuestions(grant, this.providers.get(profile.profileKey), true);
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
      if (syncFence && !this.profileSyncCurrent(profile, syncFence.epoch, syncFence.scopesKey)) return;
      snapshot = await provider.readSnapshot(grant.threadId, new Set(Object.keys(grant.candidateTurns)));
      if (syncFence && !this.profileSyncCurrent(profile, syncFence.epoch, syncFence.scopesKey)) return;
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
    if (syncFence && !this.profileSyncCurrent(profile, syncFence.epoch, syncFence.scopesKey)) return;
    let response: Awaited<ReturnType<MordeAPI["codexDiscover"]>>;
    try { response = await this.api.codexDiscover(input); }
    catch (error) { this.onChange(); throw error; }
    if (syncFence && !this.profileSyncCurrent(profile, syncFence.epoch, syncFence.scopesKey)) return;
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
        exit: (error) => {
          if (this.generation !== generation) return;
          this.approvalEpoch = randomUUID();
          this.markProfileCommandsUnknown(profile.profileKey);
          for (const grant of this.data?.grants ?? []) {
            if (grant.profileKey === profile.profileKey) {
              this.invalidateGrantApprovals(grant, undefined, false);
              this.invalidateGrantQuestions(grant, undefined, false);
            }
          }
          this.providerStates.set(profile.profileKey, { state: "unavailable", error });
          this.onChange();
        },
      });
      this.providers.set(profile.profileKey, provider);
    }
    if (!provider.isRunning) {
      this.approvalEpoch = randomUUID();
      for (const grant of this.data?.grants ?? []) {
        if (grant.profileKey === profile.profileKey) this.invalidateGrantQuestions(grant, provider, true);
      }
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
    if (!params) return;
    const threadID = params?.threadId;
    if (!stableID(threadID)) return;
    const grant = this.data?.grants.find((item) => item.profileKey === profileKey && item.threadId === threadID);
    if (!grant) return;
    const turn = object(params?.turn);
    const now = new Date().toISOString();
    if (method === "error") {
      const projection = codexErrorNotificationEvent(threadID, params, now);
      if (!projection) {
        if (grant.scopes.includes("live")) this.queueEvent(grant, codexUnsupportedNotificationEvent(method, threadID, params, now));
        return;
      }
      if (!projection.willRetry) {
        const turnKey = `${profileKey}\0${threadID}\0${projection.turnID}`;
        this.failedTurns.add(turnKey);
        const ledgers = Object.entries(this.requireState().commands).filter(([id, item]) =>
          this.activeCommandProfiles.get(id) === profileKey && item.providerRequestID === projection.turnID && ["running", "waiting_approval", "forwarded"].includes(item.status));
        for (const [id, ledger] of ledgers) {
          if (ledger.status === "forwarded") {
            ledger.status = "completed";
            delete ledger.resultCode;
          } else {
            ledger.status = "failed";
            ledger.resultCode = "codex_error";
          }
          ledger.resultPending = true;
          this.activeCommandProfiles.delete(id);
        }
        if (ledgers.length) {
          try { this.persist(); } catch { this.lastError = "Codex turn 结果无法保存；状态未知。"; }
          void this.flushResults();
        }
      }
      if (!grant.scopes.includes("live")) return;
      this.queueEvent(grant, projection.event);
      return;
    }
    if (method === "turn/completed" && stableID(turn?.id)) {
      const turnKey = `${profileKey}\0${threadID}\0${turn.id}`;
      const completed = turn.status === "completed" || turn.status === "interrupted";
      if (completed) this.completedTurns.add(turnKey);
      else this.failedTurns.add(turnKey);
      const ledgers = Object.entries(this.requireState().commands).filter(([id, item]) =>
        this.activeCommandProfiles.get(id) === profileKey && item.providerRequestID === turn.id && ["running", "waiting_approval", "forwarded"].includes(item.status));
      for (const [id, ledger] of ledgers) {
        ledger.status = ledger.status === "forwarded" ? "completed" : completed ? "completed" : "failed";
        if (ledger.status === "completed" && turn.status === "interrupted") ledger.resultCode = "turn_interrupted";
        else if (ledger.status === "failed") ledger.resultCode = "turn_failed";
        else delete ledger.resultCode;
        ledger.resultPending = true;
        this.activeCommandProfiles.delete(id);
      }
      if (ledgers.length) {
        try { this.persist(); } catch { this.lastError = "Codex turn 结果无法保存；状态未知。"; }
        void this.flushResults();
      }
    }
    if (!grant.scopes.includes("live")) return;
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
      if (!projected.length) {
        const itemType = /^[A-Za-z][A-Za-z0-9_-]{0,63}$/.test(item.type) ? item.type : "unknown";
        this.queueEvent(grant, {
          provider_event_id: codexProviderEventID("unsupported_item", turnID, item.id), kind: "unsupported", occurred_at: now,
          turn_id: turnID, item_id: item.id,
          payload: { provider_type: "app_server_item", diagnostic: `Morde 暂不支持 Codex item 类型：${itemType}` }, source: "live",
        });
        return;
      }
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
        tool: "command", summary: "Codex command output", output: delta, call_id: itemID, output_part: index, status: "inProgress",
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
    if (!event) event = codexUnsupportedNotificationEvent(method, threadID, params, now);
    this.queueEvent(grant, event);
  }

  private invalidateGrantApprovals(grant: CodexGrant, provider: MordeCodexProvider | undefined, respond: boolean): void {
    for (const [id, approval] of Object.entries(grant.approvals)) {
      if (respond && provider) {
        const capability = parseApprovalMethod(approval.method);
        if (capability) try { provider.respond(approval.rpcID, capability.decision(false, approval.params)); } catch { /* Old process may already be gone. */ }
      }
      delete grant.approvals[id];
      this.queueEvent(grant, codexApprovalStatusEvent(approval, "stale", "Codex App Server 已停止，待处理审批已失效。"));
    }
    if (Object.keys(grant.approvals).length === 0) {
      try { this.persist(); } catch { this.lastError = "Stale Codex approvals could not be saved; approval responses are disabled."; }
    }
  }

  private invalidateGrantQuestions(grant: CodexGrant, provider: MordeCodexProvider | undefined, respond: boolean): void {
    const requests = Object.values(grant.questions);
    if (!requests.length) return;
    const staleEvents: CodexEventInput[] = [];
    for (const request of requests) {
      if (respond && provider) {
        try { provider.respondError(request.rpcID, -32000, "Morde user-input request is stale"); } catch { /* Old process may already be gone. */ }
      }
      if (grant.scopes.includes("live")) staleEvents.push(codexQuestionEvent(request, "stale"));
    }
    grant.questions = {};
    if (staleEvents.length && this.queueEvents(grant, staleEvents)) return;
    try { this.persist(); }
    catch { this.lastError = "Stale Codex user-input requests could not be saved; answers remain disabled."; }
  }

  private markProfileCommandsUnknown(profileKey: string): void {
    const state = this.data;
    if (!state) return;
    const commandIDs = codexCommandsForExitedProfile(this.activeCommandProfiles, state.commands, profileKey);
    if (!commandIDs.length) return;
    for (const id of commandIDs) {
      const command = state.commands[id];
      if (!command) continue;
      command.status = "unknown";
      command.resultCode = "provider_process_exited";
      command.resultPending = true;
      this.activeCommandProfiles.delete(id);
    }
    try { this.persist(); } catch { this.lastError = "Codex App Server 已退出，命令结果未能保存。"; }
    void this.flushResults();
    this.onChange();
  }

  private commandWasInvalidatedByProviderExit(id: string): boolean {
    const command = this.data?.commands[id];
    return command?.status === "unknown" && command.resultCode === "provider_process_exited";
  }

  private handleServerRequest(profileKey: string, rpcID: number | string, method: string, value: unknown): void {
    const params = object(value);
    const threadID = stableID(params?.threadId) ? params.threadId : null;
    const grant = stableID(threadID) ? this.data?.grants.find((item) => item.profileKey === profileKey && item.threadId === threadID) : undefined;
    const capability = parseApprovalMethod(method);
    const provider = this.providers.get(profileKey);
    if (!provider) return;
    if (method === "item/tool/requestUserInput") {
      // A live scope is required before any question content is sent to Morde.
      if (!grant || !params || !grant.scopes.includes("live")) {
        provider.respondError(rpcID, -32000, "Codex user-input request is unavailable or not authorized by Morde");
        return;
      }
      const questions = codexUserInputQuestions(params.questions);
      const turnId = params.turnId;
      const itemId = params.itemId;
      if (!validRpcID(rpcID) || !String(rpcID).trim() || !questions || !stableID(turnId) || !stableID(itemId)) {
        provider.respondError(rpcID, -32600, "Invalid Codex user-input request");
        return;
      }
      const id = String(rpcID);
      if (Object.hasOwn(grant.questions, id)) {
        provider.respondError(rpcID, -32600, "Duplicate Codex user-input request id");
        return;
      }
      const base = { profileKey, threadId: grant.threadId, grantVersion: grant.grantVersion, rpcID, turnId, itemId, questions };
      const record: CodexUserInputRequest = {
        id, rpcID, profileKey, threadId: grant.threadId, turnId, itemId, grantVersion: grant.grantVersion,
        questions, version: codexUserInputVersion(this.approvalEpoch, randomUUID(), base), createdAt: new Date().toISOString(),
      };
      if (!grant.scopes.includes("send")) {
        if (this.queueEvents(grant, [codexQuestionEvent(record, "pending"), codexQuestionEvent(record, "stale")])) {
          provider.respondError(rpcID, -32000, "Codex user-input request cannot be answered without send authorization");
        } else provider.respondError(rpcID, -32000, "Codex user-input request could not be saved");
        return;
      }
      if (Object.keys(grant.questions).length >= 100) {
        provider.respondError(rpcID, -32000, "Too many pending Codex user-input requests");
        return;
      }
      grant.questions = Object.assign(Object.create(null) as Record<string, CodexUserInputRequest>, grant.questions);
      grant.questions[id] = record;
      if (!this.queueEvent(grant, codexQuestionEvent(record, "pending"))) {
        delete grant.questions[id];
        provider.respondError(rpcID, -32000, "Codex user-input request could not be saved");
      }
      return;
    }
    if (!params || !threadID || !grant || !capability || !grant.scopes.includes("approval")) {
      if (capability) provider.respond(rpcID, capability.decision(false, params ?? undefined));
      else provider.respondError(rpcID, -32000, "Approval request is unavailable or not authorized by Morde");
      return;
    }
    const id = String(rpcID);
    if (!stableID(id) || Buffer.byteLength(id) > 128) { provider.respondError(rpcID, -32600, "Invalid approval request id"); return; }
    const version = codexApprovalVersion(this.approvalEpoch, method, params);
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
    } else if (stableID(params.turnId)) {
      const ledgers = Object.entries(this.requireState().commands).filter(([id, item]) =>
        this.activeCommandProfiles.get(id) === profileKey && item.providerRequestID === params.turnId && item.status === "running");
      for (const [, ledger] of ledgers) {
        ledger.status = "waiting_approval";
        ledger.resultPending = true;
      }
      if (ledgers.length) {
        try { this.persist(); } catch { this.lastError = "Codex approval wait state could not be saved; result is unknown."; }
        void this.flushResults();
      }
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
    const required: CodexScope = command.type === "send" || command.type === "answer_question" ? "send" : command.type === "interrupt" ? "interrupt" : "approval";
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
    this.activeCommandProfiles.set(command.id, profile.profileKey);
    try {
      let sendDispatchAttempted = false;
      try {
        const provider = await this.ensureProvider(profile);
        if (this.commandWasInvalidatedByProviderExit(command.id)) return;
        if (generation !== this.generation || !this.active) return;
        if (grant.grantVersion !== command.grant_version || !grant.scopes.includes(required)) {
          state.commands[command.id] = { status: "failed", resultCode: "scope_revoked_before_dispatch", resultPending: true };
          this.persist();
          await this.flushResults();
          return;
        }
        if (command.type === "send") {
          await provider.resume(command.thread_id);
          if (this.commandWasInvalidatedByProviderExit(command.id)) return;
          if (generation !== this.generation || !this.active) return;
          if (grant.grantVersion !== command.grant_version || !grant.scopes.includes("send")) {
            state.commands[command.id] = { status: "failed", resultCode: "scope_revoked_before_dispatch", resultPending: true };
            this.persist();
            await this.flushResults();
            return;
          }
          const activeTurnID = await provider.activeTurnID(command.thread_id);
          if (this.commandWasInvalidatedByProviderExit(command.id)) return;
          if (generation !== this.generation || !this.active) return;
          if (grant.grantVersion !== command.grant_version || !grant.scopes.includes("send")) {
            state.commands[command.id] = { status: "failed", resultCode: "scope_revoked_before_dispatch", resultPending: true };
            this.persist();
            await this.flushResults();
            return;
          }
          sendDispatchAttempted = true;
          const turnID = activeTurnID
            ? await provider.steerTurn(command.thread_id, activeTurnID, command.input.text!, command.id)
            : await provider.startTurn(command.thread_id, command.input.text!, command.id);
          if (this.commandWasInvalidatedByProviderExit(command.id)) return;
          if (generation !== this.generation || !this.active) {
            const interrupted = state.commands[command.id];
            if (interrupted) interrupted.providerRequestID = turnID;
            try { this.persist(); } catch { /* The persisted unknown ledger remains authoritative. */ }
            return;
          }
          const turnKey = `${profile.profileKey}\0${command.thread_id}\0${turnID}`;
          const failed = this.failedTurns.has(turnKey);
          const completed = this.completedTurns.has(turnKey);
          const waitingApproval = Object.values(grant.approvals).some((approval) => approval.params.turnId === turnID);
          state.commands[command.id] = {
            status: failed ? "failed" : completed ? "completed" : waitingApproval ? "waiting_approval" : "running",
            providerRequestID: turnID,
            ...(failed ? { resultCode: "codex_error" } : {}),
            resultPending: true,
          };
        } else if (command.type === "interrupt") {
          await provider.interrupt(command.thread_id, command.input.turn_id!);
          if (this.commandWasInvalidatedByProviderExit(command.id)) return;
          if (generation !== this.generation || !this.active) return;
          state.commands[command.id] = { status: "forwarded", providerRequestID: command.input.turn_id, resultPending: true };
        } else if (command.type === "answer_question") {
          const requestID = command.input.question_request_id!;
          const version = command.input.question_version!;
          const request = grant.questions[requestID];
          if (!request || !grant.scopes.includes("live") || !grant.scopes.includes("send") ||
            !codexQuestionRequestMatches(request, profile.profileKey, grant.threadId, grant.grantVersion, requestID, version)) {
            state.commands[command.id] = { status: "failed", resultCode: "question_stale", resultPending: true };
            if (grant.scopes.includes("live")) {
              const staleEvents: CodexEventInput[] = [];
              if (request) {
                delete grant.questions[request.id];
                try { provider.respondError(request.rpcID, -32000, "Morde user-input request is stale"); } catch { /* The original process may have exited. */ }
                staleEvents.push(codexQuestionEvent(request, "stale"));
                if (validQuestionRequestID(requestID) && validQuestionVersion(version) && version !== request.version) {
                  staleEvents.push(codexQuestionStatusEvent(requestID, version, "stale"));
                }
              } else if (validQuestionRequestID(requestID) && validQuestionVersion(version)) {
                staleEvents.push(codexQuestionStatusEvent(requestID, version, "stale"));
              }
              if (staleEvents.length) this.queueEvents(grant, staleEvents);
            } else if (request) {
              delete grant.questions[request.id];
              try { provider.respondError(request.rpcID, -32000, "Morde user-input request is stale"); } catch { /* The original process may have exited. */ }
            }
          } else {
            const response = codexQuestionAnswerResponse(request.questions, command.input.answers);
            const answerEvent = codexQuestionAnswerEvent(request, command.input.answers);
            if (!response) {
              state.commands[command.id] = { status: "failed", resultCode: "question_invalid_answers", resultPending: true };
            } else {
              const events = [codexQuestionEvent(request, "answered"), ...(answerEvent ? [answerEvent] : [])];
              const knownEventIds = new Set(grant.pendingEvents.map((event) => event.provider_event_id));
              const newEventCount = events.filter((event) => {
                if (knownEventIds.has(event.provider_event_id)) return false;
                knownEventIds.add(event.provider_event_id);
                return true;
              }).length;
              if (grant.pendingEvents.length + newEventCount > MAX_LOCAL_EVENTS) {
                state.commands[command.id] = { status: "failed", resultCode: "event_buffer_full", resultPending: true };
              } else {
                const consumed = codexConsumeQuestionRequest(grant.questions, profile.profileKey, grant.threadId,
                  grant.grantVersion, requestID, version);
                if (!consumed) {
                  state.commands[command.id] = { status: "failed", resultCode: "question_stale", resultPending: true };
                  if (grant.scopes.includes("live")) this.queueEvent(grant, codexQuestionEvent(request, "stale"));
                } else {
                  try { provider.respond(consumed.rpcID, response); }
                  catch (error) {
                    state.commands[command.id] = { status: "unknown", resultCode: "question_response_unknown", resultPending: true };
                    const staleEvents = grant.scopes.includes("live") ? [codexQuestionEvent(consumed, "stale")] : [];
                    if (!this.queueEvents(grant, staleEvents)) {
                      try { this.persist(); } catch { /* Dispatching remains a no-replay fence on disk. */ }
                    }
                    this.lastError = safeError(error);
                  }
                  if (state.commands[command.id]?.status !== "unknown") {
                    state.commands[command.id] = { status: "completed", providerRequestID: request.id, resultPending: true };
                    if (!this.queueEvents(grant, events)) {
                      this.lastError = "Codex answered the pending question, but its confirmation event could not be saved.";
                      try { this.persist(); } catch { /* The persisted dispatching ledger prevents replay after restart. */ }
                    }
                  }
                }
              }
            }
          }
        } else {
          const approval = grant.approvals[command.input.approval_id!];
          if (!approval || approval.version !== command.input.approval_version || approval.grantVersion !== grant.grantVersion || approval.profileKey !== profile.profileKey || approval.threadId !== grant.threadId) {
            state.commands[command.id] = { status: "failed", resultCode: "approval_stale", resultPending: true };
          } else {
            const capability = parseApprovalMethod(approval.method);
            if (!capability) throw new Error("Codex approval type is unavailable");
            provider.respond(approval.rpcID, capability.decision(command.input.decision === "approve", approval.params));
            delete grant.approvals[approval.id];
            this.persist();
            state.commands[command.id] = { status: "forwarded", providerRequestID: approval.id, resultPending: true };
            this.queueEvent(grant, codexApprovalStatusEvent(approval, command.input.decision === "approve" ? "approved" : "rejected"));
            if (stableID(approval.params.turnId)) {
              const stillWaiting = Object.values(grant.approvals).some((pending) => pending.params.turnId === approval.params.turnId);
              if (!stillWaiting) {
                for (const [id, ledger] of Object.entries(state.commands)) {
                  if (this.activeCommandProfiles.get(id) === profile.profileKey && ledger.providerRequestID === approval.params.turnId && ledger.status === "waiting_approval") {
                    ledger.status = "running";
                    delete ledger.resultCode;
                    ledger.resultPending = true;
                  }
                }
              }
            }
          }
        }
        this.persist();
      } catch (error) {
        if (!this.commandWasInvalidatedByProviderExit(command.id)) {
          const rejection = error instanceof CodexAppServerRejection ? error : null;
          const knownSendFailure = command.type === "send" && (rejection !== null || !sendDispatchAttempted);
          state.commands[command.id] = knownSendFailure
            ? { status: "failed", resultCode: rejection ? `app_server_rejected_${Math.abs(rejection.code)}` : "provider_unavailable", resultPending: true }
            : { status: "unknown", resultCode: "provider_result_unknown", resultPending: true };
          this.lastError = safeError(error);
          this.persist();
        }
      }
      if (!this.commandWasInvalidatedByProviderExit(command.id)) await this.flushResults();
      this.onChange();
    } finally {
      const ledger = state.commands[command.id];
      if (!ledger || !PROFILE_EXIT_COMMAND_STATUSES.has(ledger.status)) this.activeCommandProfiles.delete(command.id);
    }
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
    finally {
      this.flushInProgress = false;
      if (changed) {
        this.onChange();
        for (const profile of this.data?.profiles ?? []) this.reportProfileSessionDiscovery(profile.profileKey);
      }
    }
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
    const count = Math.min(16, pool.length);
    const selected = Array.from({ length: count }, (_, offset) => pool[(this.selectedGrantIndex + offset) % pool.length]!);
    this.selectedGrantIndex = (this.selectedGrantIndex + count) % pool.length;
    this.snapshotInProgress = true;
    const generation = this.generation;
    try { await Promise.all(selected.map((grant) => this.snapshotGrant(grant, generation))); }
    finally {
      this.snapshotInProgress = false;
      for (const profile of this.data?.profiles ?? []) this.reportProfileSessionDiscovery(profile.profileKey);
      this.onChange();
    }
  }

  private async snapshotGrant(grant: CodexGrant, generation: number): Promise<void> {
    if (!this.active || generation !== this.generation || grant.pendingEvents.length >= 256 || !this.data) return;
    const profile = this.data.profiles.find((item) => item.profileKey === grant.profileKey);
    if (!profile) return;
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
      grant.snapshotMessage = "thread/read 快照不可用；外部 Codex 更新和终态可能尚未同步。";
      this.lastError = safeError(error);
      try { this.persist(); } catch { this.lastError = "thread/read 状态未知，且本机安全存储无法保存该状态。"; }
    }
  }

  private reconcileSnapshot(grant: CodexGrant, snapshot: CodexSnapshot, advanceHistory = true): void {
    const turns = snapshot.turns;
    const nextCandidates: Record<string, string> = {};
    const nextKnownItems = { ...grant.knownItems };
    const sorted = [...turns].reverse();
    const appended: CodexEventInput[] = [];
    const queuedIDs = new Set(grant.pendingEvents.map((event) => event.provider_event_id));
    let bytes = 0;
    let unstable = false;
    let bufferLimited = false;
    let awaitingStable = false;
    let externalWrite = snapshot.metadataStable === false;
    for (const turn of sorted) {
      if (turn.itemsView !== "full") { unstable = true; break; }
      const signature = hash(JSON.stringify({ id: turn.id, status: turn.status, items: turn.items }));
      const priorSignature = grant.candidateTurns[turn.id];
      const isTerminal = terminalTurn(turn.status);
      if (!isTerminal) {
        // Active App Server turns are useful even while their contents are changing.
        // Emit their bounded visible projection now; the terminal turn event still waits
        // for a stable, terminal snapshot below.
        nextCandidates[turn.id] = signature;
        externalWrite = true;
      } else {
        if (snapshot.metadataStable === false) {
          // A thread metadata race must not discard active projections, but also cannot
          // count as one of the matching snapshots required to finalize this turn.
          if (priorSignature !== undefined) nextCandidates[turn.id] = priorSignature;
          externalWrite = true;
          continue;
        }
        nextCandidates[turn.id] = signature;
        if (priorSignature === undefined) { awaitingStable = true; continue; }
        if (priorSignature !== signature) { externalWrite = true; continue; }
      }
      for (const item of turn.items) {
        const key = `${turn.id}:${item.id}`;
        const fingerprint = codexSnapshotItemFingerprint(item);
        const existingFingerprint = nextKnownItems[key];
        if (existingFingerprint === fingerprint) continue;
        const mapped = codexSnapshotItemEvents(turn, item);
        if (!mapped.length) mapped.push(itemEvent("unsupported", turn, item, "unsupported", {
          provider_type: eventText(item.type, 128) || "unknown_item",
          diagnostic: `Codex history item type ${eventText(item.type, 128) || "unknown"} has no projection.`,
        }, undefined, fingerprint));
        const progressPrefix = `${fingerprint}:partial:`;
        const savedProgress = existingFingerprint?.startsWith(progressPrefix) ? Number(existingFingerprint.slice(progressPrefix.length)) : 0;
        const startAt = Number.isSafeInteger(savedProgress) && savedProgress > 0 && savedProgress <= mapped.length ? savedProgress : 0;
        let projectedThrough = startAt;
        for (let index = startAt; index < mapped.length; index++) {
          const event = mapped[index]!;
          if (queuedIDs.has(event.provider_event_id)) { projectedThrough = index + 1; continue; }
          bytes += Buffer.byteLength(JSON.stringify(event));
          if (bytes > MAX_VISIBLE_MESSAGE_BYTES || grant.pendingEvents.length + appended.length >= MAX_LOCAL_EVENTS) { bufferLimited = true; break; }
          appended.push(event);
          queuedIDs.add(event.provider_event_id);
          projectedThrough = index + 1;
        }
        nextKnownItems[key] = bufferLimited ? `${progressPrefix}${projectedThrough}` : fingerprint;
        if (bufferLimited) break;
      }
      if (unstable || bufferLimited) break;
      if (!isTerminal) continue;
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
          if (!queuedIDs.has(event.provider_event_id)) {
            bytes += Buffer.byteLength(JSON.stringify(event));
            if (bytes > MAX_VISIBLE_MESSAGE_BYTES || grant.pendingEvents.length + appended.length >= MAX_LOCAL_EVENTS) { bufferLimited = true; break; }
            appended.push(event);
            queuedIDs.add(event.provider_event_id);
          }
        }
        nextKnownItems[key] = fingerprint;
      }
      if (bufferLimited) break;
    }
    if (unstable) {
      grant.snapshotState = "unknown";
      grant.snapshotMessage = "线程项在快照间变化或超出本地缓冲；本次未确认终态，后续会继续对账。";
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
    if (advanceHistory && !awaitingStable && !externalWrite && !bufferLimited) {
      grant.historyCursor = snapshot.nextCursor;
      if (snapshot.complete) grant.historyComplete = true;
    }
    grant.updatedAt = snapshot.thread.updatedAt;
    grant.status = normalizeSessionStatus(snapshot.thread.status);
    grant.snapshotState = bufferLimited ? "limited" : externalWrite ? "unknown" : awaitingStable ? "pending" : grant.historyComplete ? "available" : "limited";
    grant.snapshotMessage = bufferLimited ? "本机事件缓冲达到单批上限；已保存的部分会先上传，确认后继续回填。" : externalWrite ? "活动 turn 的可见项已同步；thread/read 不含外部审批或问题 RPC handle，无法通过此快照回答；等待稳定终态快照。" : awaitingStable ? "等待相邻 thread/read 快照一致。" : grant.historyComplete ? undefined : `历史回填进行中：每批最多 ${snapshot.turns.length} 轮；更早历史尚未同步。`;
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
    return this.queueEvents(grant, [event]);
  }

  private queueEvents(grant: CodexGrant, events: CodexEventInput[]): boolean {
    if (events.some((event) => event.kind === "question" && !grant.scopes.includes("live"))) return false;
    const known = new Set(grant.pendingEvents.map((event) => event.provider_event_id));
    const additions = events.filter((event) => {
      if (known.has(event.provider_event_id)) return false;
      known.add(event.provider_event_id);
      return true;
    });
    if (grant.pendingEvents.length + additions.length > MAX_LOCAL_EVENTS) {
      grant.snapshotState = "unknown";
      grant.snapshotMessage = "本机未确认事件缓冲已满；等待服务端确认前停止缓存新事件。";
      this.lastError = grant.snapshotMessage;
      this.onChange();
      return false;
    }
    const previousEvents = grant.pendingEvents;
    grant.pendingEvents = [...previousEvents, ...additions];
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
