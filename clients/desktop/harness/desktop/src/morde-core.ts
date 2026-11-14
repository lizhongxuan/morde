import { isAbsolute, join, resolve } from "node:path";

export type Credentials = {
  origin: string;
  serviceInstanceID: string;
  accountName: string;
  installationId: string;
  clientCredential: string;
  clientExpiresAt: string;
  syncCredential?: string;
  syncExpiresAt?: string;
};

export type WebSession = { credential: string; expiresAt: string };
export type ClientLoginAttempt = { clientInstanceID: string; attemptGeneration: string; idempotencyKey: string };
export type ProjectSyncFolder = { folderID: string; workspaceID: string; folderKind: "workspace_root" | "project"; projectName?: string; relativePath: string; state: "pending_node" | "active" | "revoked"; syncAvailable: boolean };
export type CodexScope = "read" | "live" | "send" | "interrupt" | "approval";
export type CodexDiscoverInput = {
  profile_key: string;
  thread_id: string;
  title: string;
  cwd_label: string;
  updated_at: string | null;
  status: "idle" | "in_progress" | "waiting_approval" | "completed" | "failed" | "unknown";
  scopes: CodexScope[];
};
export type CodexQueuedCommand = {
  id: string;
  session_id: string;
  profile_key: string;
  thread_id: string;
  grant_version: number;
  type: "send" | "interrupt" | "approval" | "answer_question";
  input: { text?: string; turn_id?: string; approval_id?: string; approval_version?: string; decision?: "approve" | "reject"; question_request_id?: string; question_version?: string; answers?: Record<string, string[]> };
};

const IDS = { host: /^h[a-z0-9]{7}$/, workspace: /^w[a-z0-9]{7}$/ };
const SYNC_DEVICE_ID = /^[A-Z2-7]{7}(?:-[A-Z2-7]{7}){7}$/;
const SYNC_FOLDER_ID = /^morde-[a-f0-9]{32}$/;
const SYNC_DEVICE_STATES = new Set(["pending_node", "active", "revoking", "revoked"]);
const EXPIRY_WARNING_MS = 7 * 24 * 60 * 60 * 1000;

export type WorkspaceSyncDeviceChallenge = {
  id: string;
  deviceID: string;
  nonce: Uint8Array;
  signingPayload: Uint8Array;
  expiresAt: string;
};

export type WorkspaceSyncDeviceRegistration = {
  id: string;
  workspaceID: string;
  clientInstallationID: string;
  deviceID: string;
  folderID: string;
  serverDeviceID: string;
  relayAddress: string;
  state: "pending_node" | "active" | "revoking" | "revoked";
  createdAt: string;
};

export function credentialExpiresSoon(expiresAt: string, now = Date.now()): boolean {
  const remaining = Date.parse(expiresAt) - now;
  return Number.isFinite(remaining) && remaining > 0 && remaining <= EXPIRY_WARNING_MS;
}

export function credentialExpired(expiresAt: unknown, now = Date.now()): boolean {
  if (typeof expiresAt !== "string") return true;
  const expiry = Date.parse(expiresAt);
  return !Number.isFinite(expiry) || expiry <= now;
}

export function checkedId(kind: keyof typeof IDS, value: unknown): string {
  if (typeof value !== "string" || !IDS[kind].test(value)) throw new Error(`Invalid ${kind} ID`);
  return value;
}

export function portalOrigin(input: unknown): string {
  if (typeof input !== "string") throw new Error("Enter a Morde service address");
  const url = new URL(input.trim());
  const local = ["localhost", "portal.localhost", "127.0.0.1", "[::1]"].includes(url.hostname);
  if ((url.protocol !== "https:" && !(local && url.protocol === "http:")) || url.username || url.password ||
    url.pathname !== "/" || url.search || url.hash) {
    throw new Error("Use an HTTPS service origin, or HTTP on localhost/portal.localhost for development");
  }
  return url.origin;
}

export function mordeUserDataDirectory(appDataDirectory: string, testDirectory?: string): string {
  const override = typeof testDirectory === "string" ? testDirectory.trim() : "";
  if (!override) return join(appDataDirectory, "Morde Client");
  if (!isAbsolute(override)) throw new Error("MORDE_TEST_USER_DATA must be an absolute path");
  return resolve(override);
}

export function desktopDownloadsURL(origin: unknown): string {
  return `${portalOrigin(origin)}/download`;
}

export function localGatewayOrigin(url: URL, serviceOrigin: string): boolean {
  const service = new URL(serviceOrigin);
  return service.protocol === "http:" && ["localhost", "portal.localhost", "127.0.0.1", "[::1]"].includes(service.hostname) &&
    url.protocol === "http:" && /^h[a-z0-9]{7}\.hosts\.localhost$/.test(url.hostname) && !url.port;
}

export function gatewayURL(value: unknown, serviceOrigin = ""): string {
  if (typeof value !== "string") throw new Error("Invalid Host ticket URL");
  const url = new URL(value);
  const next = url.searchParams.get("next");
  if ((url.protocol !== "https:" && !localGatewayOrigin(url, serviceOrigin)) || url.username || url.password || url.hash || url.pathname !== "/__ch/auth" ||
    url.searchParams.getAll("t").length !== 1 || !url.searchParams.get("t") ||
    url.searchParams.getAll("next").length > 1 ||
    [...url.searchParams.keys()].some((key) => key !== "t" && key !== "next") ||
    (next !== null && (!next.startsWith("/") || next.startsWith("//") || next.includes("\\")))) {
    throw new Error("Invalid Host ticket URL");
  }
  return url.href;
}

function validSession(value: unknown): WebSession {
  const response = value as Record<string, unknown>;
  if (!response || typeof response.web_session_credential !== "string" || !response.web_session_credential ||
    typeof response.web_session_expires_at !== "string" || credentialExpired(response.web_session_expires_at)) {
    throw new Error("Invalid Morde Web session response");
  }
  return { credential: response.web_session_credential, expiresAt: response.web_session_expires_at };
}

function validProjectFolderName(value: string): boolean {
  if (!value || value.trim() !== value || [...value].length > 120 || value === "." || value === ".." || /[\\/<>:"|?*\u0000-\u001f\u007f-\u009f]/u.test(value) || /[. ]$/.test(value)) return false;
  const base = value.split(".", 1)[0]!.toUpperCase();
  return !new Set(["CON", "PRN", "AUX", "NUL"]).has(base) && !/^(?:COM|LPT)[1-9]$/.test(base);
}

function parseProjectSyncFolder(value: unknown): ProjectSyncFolder {
  const folder = value as Record<string, unknown> | null;
  if (!folder || typeof folder.folder_id !== "string" || !SYNC_FOLDER_ID.test(folder.folder_id) ||
    typeof folder.workspace_id !== "string" || !/^w[a-z0-9]{7}$/.test(folder.workspace_id) ||
    (folder.folder_kind !== "workspace_root" && folder.folder_kind !== "project") ||
    typeof folder.relative_path !== "string" || folder.relative_path.startsWith("/") || folder.relative_path.includes("\\") ||
    !["pending_node", "active", "revoked"].includes(String(folder.state)) || typeof folder.sync_available !== "boolean" ||
    (folder.folder_kind === "project" && (typeof folder.project_name !== "string" || !validProjectFolderName(folder.project_name)))) {
    throw new Error("Invalid Morde project folder response");
  }
  const expectedPath = folder.folder_kind === "project" ? `project/${folder.project_name}` : ".";
  if (folder.relative_path !== expectedPath) throw new Error("Invalid Morde project folder path");
  return {
    folderID: folder.folder_id, workspaceID: folder.workspace_id, folderKind: folder.folder_kind,
    ...(folder.folder_kind === "project" ? { projectName: folder.project_name as string } : {}),
    relativePath: folder.relative_path, state: folder.state as ProjectSyncFolder["state"], syncAvailable: folder.sync_available,
  };
}

function decodeBase64(value: unknown, maxBytes: number): Uint8Array | null {
  if (typeof value !== "string" || !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(value)) return null;
  const bytes = Buffer.from(value, "base64");
  return bytes.length > 0 && bytes.length <= maxBytes && bytes.toString("base64") === value ? bytes : null;
}

function validWorkspaceSyncRelayAddress(value: unknown, expectedDeviceID: string): value is string {
  if (typeof value !== "string" || value.length > 2048) return false;
  try {
    const url = new URL(value);
    const entries = [...url.searchParams.entries()];
    return url.protocol === "relay:" && !url.username && !url.password && Boolean(url.hostname) &&
      Number.isInteger(Number(url.port)) && Number(url.port) > 0 && Number(url.port) <= 65535 &&
      url.pathname === "/" && !url.hash && entries.length === 2 &&
      url.searchParams.getAll("id").length === 1 && url.searchParams.get("id") === expectedDeviceID &&
      url.searchParams.getAll("token").length === 1 && /^[A-Za-z0-9_-]{32,128}$/.test(url.searchParams.get("token") ?? "");
  } catch { return false; }
}

function versionParts(value: unknown): { core: number[]; pre: string[] } | null {
  if (typeof value !== "string") return null;
  const match = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/.exec(value);
  if (!match) return null;
  const core = match.slice(1, 4).map(Number);
  return core.every(Number.isSafeInteger) ? { core, pre: match[4]?.split(".") ?? [] } : null;
}

function meetsMinimumVersion(current: unknown, minimum: unknown): boolean {
  const actual = versionParts(current), required = versionParts(minimum);
  if (!actual || !required) return false;
  for (let i = 0; i < 3; i++) {
    if (actual.core[i] !== required.core[i]) return actual.core[i]! > required.core[i]!;
  }
  if (!actual.pre.length || !required.pre.length) return !actual.pre.length;
  for (let i = 0; i < Math.max(actual.pre.length, required.pre.length); i++) {
    const a = actual.pre[i], b = required.pre[i];
    if (a === undefined || b === undefined) return b === undefined;
    if (a === b) continue;
    const an = /^(0|[1-9]\d*)$/.test(a), bn = /^(0|[1-9]\d*)$/.test(b);
    if (an && bn) return BigInt(a) > BigInt(b);
    if (an !== bn) return !an;
    return a > b;
  }
  return true;
}

export class MordeAPI {
  constructor(private readonly credentials: () => Credentials | null, private readonly clientVersion: () => string = () => "") {}

  private async checkCapabilities(origin: string): Promise<{ serviceInstanceID: string; supportedFeatures: string[] }> {
    let response: Response;
    try {
      response = await fetch(`${origin}/api/capabilities`, {
        headers: { accept: "application/json" }, cache: "no-store", redirect: "error", signal: AbortSignal.timeout(20_000),
      });
    } catch { throw new Error("无法确认 Morde 服务版本，请检查网络和服务地址后重试。未发送账号信息。"); }
    if (!response.ok) throw new Error("Morde 服务不支持客户端登录或暂不可用，请升级服务后重试。未发送账号信息。");
    const value = await response.json().catch(() => null) as Record<string, unknown> | null;
    const features = value?.supported_features;
    if (!value || typeof value.service_instance_id !== "string" ||
      !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value.service_instance_id) ||
      value.api_version !== "1" ||
      !(value.hub_version === null || typeof value.hub_version === "string") ||
      !(value.event_version === null || typeof value.event_version === "string") ||
      !(value.minimum_client_version === null || typeof value.minimum_client_version === "string") ||
      !Array.isArray(features) || !features.every((feature) => typeof feature === "string") ||
      typeof value.default_max_request_bytes !== "number" || !Number.isSafeInteger(value.default_max_request_bytes) || value.default_max_request_bytes <= 0 ||
      !["password_login", "client_installations", "ordered_client_login"].every((feature) => features.includes(feature))) {
      throw new Error("Morde 服务版本不兼容或未开放客户端登录，本次未发送账号信息。");
    }
    if (value.minimum_client_version !== null && !meetsMinimumVersion(this.clientVersion(), value.minimum_client_version)) {
      throw new Error("当前 Morde 客户端版本过低，请更新客户端后再登录。未发送账号信息。");
    }
    return { serviceInstanceID: value.service_instance_id, supportedFeatures: features as string[] };
  }

  async requireWorkspaceSyncCapability(): Promise<void> {
    const auth = this.credentials();
    if (!auth || credentialExpired(auth.clientExpiresAt)) throw new Error("Log in to Morde again");
    const capabilities = await this.checkCapabilities(auth.origin);
    if (capabilities.serviceInstanceID !== auth.serviceInstanceID) throw new Error("Morde service identity changed; log in again");
    if (!capabilities.supportedFeatures.includes("workspace_sync")) {
      throw new Error("Morde Workspace sync is not enabled for this service");
    }
  }

  async requireProjectUploadCapability(): Promise<void> {
    const auth = this.credentials();
    if (!auth || credentialExpired(auth.clientExpiresAt)) throw new Error("Log in to Morde again");
    const capabilities = await this.checkCapabilities(auth.origin);
    if (capabilities.serviceInstanceID !== auth.serviceInstanceID) throw new Error("Morde service identity changed; log in again");
    if (!capabilities.supportedFeatures.includes("workspace_sync") || !capabilities.supportedFeatures.includes("project_upload")) {
      throw new Error("此 Morde 服务尚未开放云端电脑项目上传");
    }
  }

  async login(originInput: unknown, name: unknown, password: unknown, attempt: ClientLoginAttempt, renewal?: { installationID: string; serviceInstanceID: string }): Promise<{ credentials: Credentials; webSession: WebSession }> {
    const origin = portalOrigin(originInput);
    if (typeof name !== "string" || !name.trim() || typeof password !== "string" || !password ||
      !attempt || !/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(attempt.clientInstanceID) ||
      typeof attempt.attemptGeneration !== "string" || !/^[1-9][0-9]*$/.test(attempt.attemptGeneration) || BigInt(attempt.attemptGeneration) > 9223372036854775807n ||
      (renewal !== undefined && (!/^i[a-z0-9]{7}$/.test(renewal.installationID) || !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(renewal.serviceInstanceID))) ||
      !/^[A-Za-z0-9._~-]{1,128}$/.test(attempt.idempotencyKey)) throw new Error("Enter your Morde account and password");
    const { serviceInstanceID } = await this.checkCapabilities(origin);
    const renewalInstallationID = renewal?.serviceInstanceID === serviceInstanceID ? renewal.installationID : undefined;
    let response: Response;
    try {
      response = await fetch(`${origin}/api/auth/client-login`, {
        method: "POST",
        headers: { "content-type": "application/json", accept: "application/json", "Idempotency-Key": attempt.idempotencyKey },
        body: (() => {
          const body = JSON.stringify({ name: name.trim(), password, platform: "desktop", display_name: "Morde Desktop",
            client_instance_id: attempt.clientInstanceID,
            ...(renewalInstallationID ? { renewal_installation_id: renewalInstallationID } : {}) });
          return `${body.slice(0, -1)},"attempt_generation":${attempt.attemptGeneration}}`;
        })(),
        redirect: "error", signal: AbortSignal.timeout(20_000),
      });
    } catch { throw new Error("无法连接 Morde 服务，请检查网络和服务地址后重试。"); }
    if (!response.ok) {
      const body = await response.json().catch(() => null) as { error?: { code?: string } } | null;
      const messages: Record<string, string> = {
        unauthenticated: "用户名或密码错误，请重试。",
        account_pending: "账号正在等待管理员审核，通过后才能登录。",
        account_rejected: "账号审核未通过，请联系管理员。",
        account_disabled: "账号已被禁用，请联系管理员。",
        rate_limited: "登录尝试过于频繁，请稍后重试。",
        forbidden: "请先在管理后台修改密码，再登录客户端。",
      };
      throw new Error(messages[body?.error?.code ?? ""] ?? (response.status === 401 ? messages.unauthenticated : `Morde 登录失败（HTTP ${response.status}）。`));
    }
    const value = await response.json() as Record<string, unknown>;
    if (typeof value.installation_id !== "string" || !/^i[a-z0-9]{7}$/.test(value.installation_id) ||
      typeof value.client_credential !== "string" || !value.client_credential ||
      typeof value.client_expires_at !== "string" || credentialExpired(value.client_expires_at) ||
      typeof value.sync_credential !== "string" || !value.sync_credential ||
      typeof value.sync_expires_at !== "string" || credentialExpired(value.sync_expires_at)) {
      throw new Error("Invalid Morde client login response");
    }
    return {
      credentials: {
        origin, serviceInstanceID, accountName: name.trim(), installationId: value.installation_id, clientCredential: value.client_credential,
        clientExpiresAt: value.client_expires_at, syncCredential: value.sync_credential,
        syncExpiresAt: value.sync_expires_at,
      },
      webSession: validSession(value),
    };
  }

  private async request(path: string, method = "GET", body?: unknown, idempotencyKey?: string): Promise<unknown> {
    const auth = this.credentials();
    if (!auth || credentialExpired(auth.clientExpiresAt)) throw new Error("Log in to Morde again");
    const response = await fetch(`${auth.origin}${path}`, {
      method, headers: {
        authorization: `Bearer ${auth.clientCredential}`, accept: "application/json",
        ...(body === undefined ? {} : { "content-type": "application/json" }),
        ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}),
      },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      redirect: "error", signal: AbortSignal.timeout(20_000),
    });
    if (!response.ok) throw new Error(response.status === 401 ? "Authorization expired or revoked" : `Morde returned HTTP ${response.status} (${method} ${path})`);
    return response.status === 204 ? null : response.json();
  }

  private async codexRequest(path: string, method = "GET", body?: unknown): Promise<unknown> {
    const auth = this.credentials();
    if (!auth || credentialExpired(auth.clientExpiresAt)) throw new Error("Log in to Morde again");
    if (new URL(auth.origin).protocol !== "https:") throw new Error("Codex synchronization requires an HTTPS Morde origin");
    return this.request(path, method, body);
  }

  async webSession(): Promise<WebSession> {
    return validSession(await this.request("/api/auth/client-web-session", "POST"));
  }

  async logout(): Promise<void> {
    await this.request("/api/auth/client-logout", "POST");
  }

  async codexDiscover(input: CodexDiscoverInput): Promise<{ id: string; scopes: CodexScope[]; grant_version: number }> {
    const response = await this.codexRequest("/api/codex/sessions/discover", "POST", input) as Record<string, unknown> | null;
    const value = response && typeof response.session === "object" && response.session !== null
      ? response.session as Record<string, unknown>
      : response;
    if (!value || typeof value.id !== "string" || !/^l[a-z0-9]{7}$/.test(value.id) ||
      !Array.isArray(value.scopes) || !value.scopes.every((scope) => ["read", "live", "send", "interrupt", "approval"].includes(String(scope))) ||
      !Number.isSafeInteger(value.grant_version) || (value.grant_version as number) < 1) {
      throw new Error("Invalid Morde Codex session response");
    }
    return { id: value.id, scopes: value.scopes as CodexScope[], grant_version: value.grant_version as number };
  }

  async codexClaim(): Promise<CodexQueuedCommand[]> {
    const value = await this.codexRequest("/api/codex/commands/claim", "POST", {}) as unknown;
    const rows = Array.isArray(value) ? value : (value as { items?: unknown; commands?: unknown } | null)?.items ?? (value as { commands?: unknown } | null)?.commands;
    if (!Array.isArray(rows) || rows.length > 64) throw new Error("Invalid Morde Codex command response");
    return rows.map((value) => {
      const row = value as Record<string, unknown> | null;
      const input = row?.input as Record<string, unknown> | null;
      if (!row || typeof row.id !== "string" || !/^g[a-z0-9]{7}$/.test(row.id) ||
        typeof row.session_id !== "string" || !/^l[a-z0-9]{7}$/.test(row.session_id) ||
        typeof row.profile_key !== "string" || !/^[A-Za-z0-9_-]{1,128}$/.test(row.profile_key) ||
        typeof row.thread_id !== "string" || !row.thread_id || row.thread_id.length > 512 ||
        !Number.isSafeInteger(row.grant_version) || !["send", "interrupt", "approval", "answer_question"].includes(String(row.type)) ||
        !input || typeof input !== "object") throw new Error("Invalid Morde Codex command");
      const normalized: CodexQueuedCommand["input"] = {};
      if (input.text !== undefined) {
        if (typeof input.text !== "string" || !input.text.trim() || input.text.length > 32768) throw new Error("Invalid Morde Codex send command");
        normalized.text = input.text;
      }
      if (input.turn_id !== undefined) {
        if (typeof input.turn_id !== "string" || !input.turn_id || input.turn_id.length > 512) throw new Error("Invalid Morde Codex interrupt command");
        normalized.turn_id = input.turn_id;
      }
      if (input.approval_id !== undefined) {
        if (typeof input.approval_id !== "string" || !input.approval_id || input.approval_id.length > 512) throw new Error("Invalid Morde Codex approval command");
        normalized.approval_id = input.approval_id;
      }
      if (input.approval_version !== undefined) {
        if (typeof input.approval_version !== "string" || !input.approval_version || input.approval_version.length > 256) throw new Error("Invalid Morde Codex approval command");
        normalized.approval_version = input.approval_version;
      }
      if (input.decision !== undefined) {
        if (input.decision !== "approve" && input.decision !== "reject") throw new Error("Invalid Morde Codex approval decision");
        normalized.decision = input.decision;
      }
      if (input.question_request_id !== undefined) {
        if (typeof input.question_request_id !== "string" || !input.question_request_id.trim() ||
          Buffer.byteLength(input.question_request_id) > 512 || /[\u0000-\u001f\u007f-\u009f]/.test(input.question_request_id)) {
          throw new Error("Invalid Morde Codex question request ID");
        }
        normalized.question_request_id = input.question_request_id;
      }
      if (input.question_version !== undefined) {
        if (typeof input.question_version !== "string" || !/^[a-f0-9]{64}$/.test(input.question_version)) {
          throw new Error("Invalid Morde Codex question version");
        }
        normalized.question_version = input.question_version;
      }
      if (input.answers !== undefined) {
        const answers = input.answers as Record<string, unknown> | null;
        const entries = answers && typeof answers === "object" && !Array.isArray(answers) ? Object.entries(answers) : [];
        if (entries.length < 1 || entries.length > 64) throw new Error("Invalid Morde Codex question answers");
        const normalizedAnswers: Record<string, string[]> = {};
        for (const [questionID, value] of entries) {
          if (!questionID.trim() || Buffer.byteLength(questionID) > 512 || /[\u0000-\u001f\u007f-\u009f]/.test(questionID) ||
            !Array.isArray(value) || value.length < 1 || value.length > 64 ||
            !value.every((answer) => typeof answer === "string" && answer.trim().length > 0 && Buffer.byteLength(answer) <= 4096 &&
              !/[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f-\u009f]/.test(answer))) {
            throw new Error("Invalid Morde Codex question answers");
          }
          Object.defineProperty(normalizedAnswers, questionID, { value: value as string[], enumerable: true, configurable: true, writable: true });
        }
        normalized.answers = normalizedAnswers;
      }
      const complete = row.type === "send" ? typeof normalized.text === "string" : row.type === "interrupt" ? Boolean(normalized.turn_id) :
        row.type === "approval" ? Boolean(normalized.approval_id && normalized.approval_version && normalized.decision) :
        Boolean(normalized.question_request_id && normalized.question_version && normalized.answers);
      if (!complete) throw new Error("Incomplete Morde Codex command");
      return {
        id: row.id, session_id: row.session_id, profile_key: row.profile_key, thread_id: row.thread_id,
        grant_version: row.grant_version as number, type: row.type as CodexQueuedCommand["type"], input: normalized,
      };
    });
  }

  async codexCommandResult(commandID: string, result: { status: "forwarded" | "running" | "waiting_approval" | "completed" | "failed" | "unknown"; provider_request_id?: string; result_code?: string }): Promise<void> {
    if (!/^g[a-z0-9]{7}$/.test(commandID)) throw new Error("Invalid Morde Codex command ID");
    await this.codexRequest(`/api/codex/commands/${commandID}/result`, "POST", result);
  }

  async codexEvents(sessionID: string, events: unknown[]): Promise<{ provider_event_ids: string[]; last_sequence: number }> {
    if (!/^l[a-z0-9]{7}$/.test(sessionID) || !Array.isArray(events) || events.length < 1 || events.length > 128) {
      throw new Error("Invalid Morde Codex event batch");
    }
    const value = await this.codexRequest(`/api/codex/sessions/${sessionID}/events`, "POST", { events }) as Record<string, unknown> | null;
    if (!value || !Array.isArray(value.items) || !Number.isSafeInteger(value.last_sequence)) throw new Error("Invalid Morde Codex event acknowledgement");
    const ids = value.items.map((item) => {
      const row = item as Record<string, unknown> | null;
      if (!row || typeof row.provider_event_id !== "string" || !Number.isSafeInteger(row.sequence)) throw new Error("Invalid Morde Codex event acknowledgement");
      return row.provider_event_id;
    });
    return { provider_event_ids: ids, last_sequence: value.last_sequence as number };
  }

  private async portalRequest(path: string, session: WebSession, method = "GET", body?: unknown, idempotencyKey?: string): Promise<unknown> {
    const auth = this.credentials();
    if (!auth || credentialExpired(auth.clientExpiresAt)) throw new Error("Log in to Morde again");
    if (!session || typeof session.credential !== "string" || !session.credential || credentialExpired(session.expiresAt)) {
      throw new Error("Portal session expired; retry the operation");
    }
    const response = await fetch(`${auth.origin}${path}`, {
      method, headers: {
        cookie: `ch_portal=${session.credential}`, accept: "application/json",
        ...(body === undefined ? {} : { "content-type": "application/json" }),
        ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}),
      },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      cache: "no-store", redirect: "error", signal: AbortSignal.timeout(20_000),
    });
    if (!response.ok) throw new Error(response.status === 401 ? "Portal authorization expired or revoked" : `Morde returned HTTP ${response.status} (${method} ${path})`);
    return response.status === 204 ? null : response.json();
  }

  async createWorkspaceSyncDeviceChallenge(workspaceID: unknown, deviceID: unknown, certificateDER: Uint8Array): Promise<WorkspaceSyncDeviceChallenge> {
    const id = checkedId("workspace", workspaceID);
    if (typeof deviceID !== "string" || !SYNC_DEVICE_ID.test(deviceID) || !(certificateDER instanceof Uint8Array) ||
      certificateDER.length === 0 || certificateDER.length > 16 * 1024) throw new Error("Invalid Workspace sync device challenge request");
    const value = await this.request(`/api/workspaces/${id}/sync-device-challenges`, "POST", {
      device_id: deviceID, certificate_der: Buffer.from(certificateDER).toString("base64"),
    }) as Record<string, unknown> | null;
    const nonce = decodeBase64(value?.nonce, 32);
    const signingPayload = decodeBase64(value?.signing_payload, 16 * 1024);
    if (!value || typeof value.id !== "string" || !/^n[a-z0-9]{7}$/.test(value.id) || value.device_id !== deviceID ||
      !nonce || nonce.length !== 32 || !signingPayload || typeof value.expires_at !== "string" || credentialExpired(value.expires_at)) {
      throw new Error("Invalid Morde Workspace sync device challenge response");
    }
    return { id: value.id, deviceID, nonce, signingPayload, expiresAt: value.expires_at };
  }

  async registerWorkspaceSyncDevice(workspaceID: unknown, challengeID: unknown, deviceID: unknown, signature: Uint8Array, idempotencyKey: unknown): Promise<WorkspaceSyncDeviceRegistration> {
    const id = checkedId("workspace", workspaceID);
    const auth = this.credentials();
    if (typeof challengeID !== "string" || !/^n[a-z0-9]{7}$/.test(challengeID) || typeof deviceID !== "string" ||
      !SYNC_DEVICE_ID.test(deviceID) || !(signature instanceof Uint8Array) ||
      signature.length === 0 || signature.length > 16 * 1024 || typeof idempotencyKey !== "string" ||
      !/^[A-Za-z0-9._~-]{1,128}$/.test(idempotencyKey)) throw new Error("Invalid Workspace sync device registration request");
    const value = await this.request(`/api/workspaces/${id}/sync-devices`, "POST", {
      challenge_id: challengeID, signature: Buffer.from(signature).toString("base64"),
    }, idempotencyKey) as Record<string, unknown> | null;
    if (!value || typeof value.id !== "string" || !/^d[a-z0-9]{7}$/.test(value.id) || value.workspace_id !== id ||
      !auth || value.client_installation_id !== auth.installationId || value.device_id !== deviceID ||
      typeof value.folder_id !== "string" || !SYNC_FOLDER_ID.test(value.folder_id) || typeof value.server_device_id !== "string" ||
      !SYNC_DEVICE_ID.test(value.server_device_id) || !validWorkspaceSyncRelayAddress(value.relay_address, value.server_device_id) ||
      typeof value.state !== "string" || !SYNC_DEVICE_STATES.has(value.state) || typeof value.created_at !== "string" || !Number.isFinite(Date.parse(value.created_at))) {
      throw new Error("Invalid Morde Workspace sync device registration response");
    }
    return {
      id: value.id, workspaceID: id, clientInstallationID: auth.installationId, deviceID,
      folderID: value.folder_id, serverDeviceID: value.server_device_id, relayAddress: value.relay_address,
      state: value.state as WorkspaceSyncDeviceRegistration["state"], createdAt: value.created_at,
    };
  }

  workspaces(): Promise<unknown> {
    return this.request("/api/workspaces");
  }

  hosts(): Promise<unknown> {
    return this.request("/api/hosts");
  }

  workspace(id: unknown): Promise<unknown> {
    return this.request(`/api/workspaces/${checkedId("workspace", id)}`);
  }

  me(): Promise<unknown> {
    return this.request("/api/me");
  }

  host(id: unknown): Promise<unknown> {
    return this.request(`/api/hosts/${checkedId("host", id)}`);
  }

  async hostProjectFolders(hostID: unknown, session: WebSession): Promise<ProjectSyncFolder[]> {
    const host = checkedId("host", hostID);
    const value = await this.portalRequest(`/api/hosts/${host}/sync-folders`, session) as { items?: unknown } | null;
    if (!value || !Array.isArray(value.items)) throw new Error("Invalid Morde project folder response");
    return value.items.map((item) => parseProjectSyncFolder(item));
  }

  async registerHostProjectFolder(hostID: unknown, projectName: unknown, session: WebSession, idempotencyKey: unknown): Promise<ProjectSyncFolder> {
    const host = checkedId("host", hostID);
    if (typeof projectName !== "string" || !validProjectFolderName(projectName) || typeof idempotencyKey !== "string" || !/^[A-Za-z0-9._~-]{1,128}$/.test(idempotencyKey)) {
      throw new Error("Invalid Morde project folder registration request");
    }
    const value = await this.portalRequest(`/api/hosts/${host}/sync-folders`, session, "POST", { project_name: projectName }, idempotencyKey);
    return parseProjectSyncFolder(value);
  }

  sharedHosts(): Promise<unknown> {
    return this.request("/api/shared-hosts");
  }
}
