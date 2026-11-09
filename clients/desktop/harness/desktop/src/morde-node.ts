import { createPrivateKey, randomBytes, sign, verify, X509Certificate } from "node:crypto";
import { spawn, type ChildProcess, type SpawnOptions, type StdioOptions } from "node:child_process";
import { createServer } from "node:net";
import { chmod, lstat, mkdir, readFile } from "node:fs/promises";
import { isAbsolute, join, resolve } from "node:path";

const deviceIDPattern = /^[A-Z2-7]{7}(?:-[A-Z2-7]{7}){7}$/;
const readinessTimeout = 20_000;
const stopTimeout = 8_000;

type SpawnProcess = (command: string, args: string[], options: SpawnOptions) => ChildProcess;
type NodeStatus = { myID?: unknown };
export type WorkspaceFolderStatus = { state: string; needBytes: number; needFiles: number; pullErrors: number };

export type ManagedWorkspaceFolder = {
  workspaceID: string;
  folderID: string;
  directory: string;
  bindingState: "pending_node" | "active" | "revoking" | "revoked";
  enabled: boolean;
  serverDeviceID: string;
  relayAddress: string;
};

const workspaceIDPattern = /^w[a-z0-9]{7}$/;
const folderIDPattern = /^morde-[a-f0-9]{32}$/;
const relayDevicePattern = /^[A-Z2-7]{7}(?:-[A-Z2-7]{7}){7}$/;
const maxSingleFileBytes = 314_572_800;

export type SyncthingIdentity = { deviceID: string; certificateDER: Uint8Array };

export function syncthingEnvironment(base: NodeJS.ProcessEnv, guiAddress: string, apiKey: string): NodeJS.ProcessEnv {
  const env = Object.fromEntries(Object.entries(base).filter(([name]) => !name.startsWith("ST")));
  return { ...env, STGUIADDRESS: guiAddress, STGUIAPIKEY: apiKey };
}

export class MordeSyncthingNode {
  private child: ChildProcess | null = null;
  private exited: Promise<void> | null = null;
  private apiKey: string | null = null;
  private guiAddress: string | null = null;
  private home: string | null = null;
  private deviceID: string | null = null;
  private startPromise: Promise<string> | null = null;
  private stopping = false;

  constructor(
    private readonly binary: () => string,
    private readonly spawnProcess: SpawnProcess = spawn,
    private readonly fetcher: typeof fetch = fetch,
    private readonly choosePort: () => Promise<number> = loopbackPort,
    private readonly environment: () => NodeJS.ProcessEnv = () => process.env,
  ) {}

  isRunning(): boolean { return this.child !== null && this.deviceID !== null; }
  mayOwnWorkspace(): boolean { return this.child !== null; }

  start(home: string, onUnexpectedExit?: () => void): Promise<string> {
    if (this.startPromise) return this.startPromise;
    if (this.child) return Promise.reject(new Error("Syncthing is already running or stopping"));
    this.startPromise = this.startInner(home, onUnexpectedExit).finally(() => { this.startPromise = null; });
    return this.startPromise;
  }

  private async startInner(homeInput: string, onUnexpectedExit?: () => void): Promise<string> {
    const home = resolve(homeInput);
    const binary = this.binary();
    if (!homeInput || home !== homeInput || !binary || !isAbsolute(binary) || resolve(binary) !== binary) {
      throw new Error("Syncthing node paths are invalid");
    }
    await mkdir(home, { recursive: true, mode: 0o700 });
    await secureDirectory(home);
    const initEnvironment = syncthingEnvironment(this.environment(), "http://127.0.0.1:8384", "");
    await this.runCommand(binary, [`--home=${home}`, "generate", "--morde-desktop", "--no-port-probing"], initEnvironment);

    const port = await this.choosePort();
    if (!Number.isSafeInteger(port) || port < 1 || port > 65535) throw new Error("Could not reserve local Syncthing control port");
    const guiAddress = `http://127.0.0.1:${port}`;
    const apiKey = randomBytes(32).toString("hex");
    const env = syncthingEnvironment(this.environment(), guiAddress, apiKey);
    const child = this.spawnProcess(binary, [
      `--home=${home}`, "serve", "--no-browser", "--no-upgrade", "--no-restart", "--no-port-probing", "--paused",
    ], { cwd: home, env, shell: false, stdio: "ignore" as StdioOptions, windowsHide: true });
    this.child = child;
    this.apiKey = apiKey;
    this.guiAddress = guiAddress;
    this.home = home;
    this.stopping = false;
    this.exited = new Promise<void>((resolveExit) => {
      child.once("exit", () => {
        const unexpected = this.child === child && !this.stopping;
        this.clearChild(child);
        resolveExit();
        if (unexpected) onUnexpectedExit?.();
      });
      child.once("error", () => {
        const unexpected = this.child === child && !this.stopping;
        this.clearChild(child);
        resolveExit();
        if (unexpected) onUnexpectedExit?.();
      });
    });

    try {
      this.deviceID = await this.waitUntilReady();
      return this.deviceID;
    } catch {
      await this.stopChild();
      throw new Error("Could not start the private Syncthing node");
    }
  }

  async request(path: string, method: "GET" | "PUT" | "POST" | "PATCH" | "DELETE" = "GET", body?: unknown): Promise<unknown> {
    if (!this.child || !this.apiKey || !this.guiAddress || !path.startsWith("/rest/") || path.includes("\\") || /%2e/i.test(path)) {
      throw new Error("Syncthing local control is unavailable");
    }
    const url = new URL(path, this.guiAddress);
    if (url.origin !== this.guiAddress || !url.pathname.startsWith("/rest/")) throw new Error("Invalid Syncthing local control path");
    let response: Response;
    try {
      response = await this.fetcher(url, {
        method,
        headers: {
          "X-API-Key": this.apiKey,
          accept: "application/json",
          ...(body === undefined ? {} : { "content-type": "application/json" }),
        },
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
        redirect: "error",
        signal: AbortSignal.timeout(10_000),
      });
    } catch { throw new Error("Syncthing local control request failed"); }
    if (!response.ok) throw new Error("Syncthing local control request failed");
    if (response.status === 204) return null;
    try {
      const body = await response.text();
      return body.trim() ? JSON.parse(body) as unknown : null;
    }
    catch { throw new Error("Invalid Syncthing local control response"); }
  }

  async configureWorkspaces(items: ManagedWorkspaceFolder[]): Promise<void> {
    if (!this.child || !this.deviceID || !Array.isArray(items) || items.some((item) =>
      !item || !workspaceIDPattern.test(item.workspaceID) || !folderIDPattern.test(item.folderID) ||
      !isAbsolute(item.directory) || resolve(item.directory) !== item.directory ||
      !["pending_node", "active", "revoking", "revoked"].includes(item.bindingState) || typeof item.enabled !== "boolean" ||
      !relayDevicePattern.test(item.serverDeviceID) || !validRelayAddress(item.relayAddress, item.serverDeviceID)) ||
      new Set(items.map((item) => item.workspaceID)).size !== items.length || new Set(items.map((item) => item.folderID)).size !== items.length) {
      throw new Error("Workspace folder configuration is invalid");
    }
    const localDeviceID = this.deviceID;

    const [options, foldersValue, devicesValue, defaultFolder, defaultDevice] = await Promise.all([
      this.request("/rest/config/options"),
      this.request("/rest/config/folders"),
      this.request("/rest/config/devices"),
      this.request("/rest/config/defaults/folder"),
      this.request("/rest/config/defaults/device"),
    ]) as [Record<string, unknown>, Array<Record<string, unknown>>, Array<Record<string, unknown>>, Record<string, unknown>, Record<string, unknown>];
    if (!safeNetworkOptions(options) || !Array.isArray(foldersValue) || !Array.isArray(devicesValue) ||
      !defaultFolder || typeof defaultFolder !== "object" || !defaultDevice || typeof defaultDevice !== "object") {
      throw new Error("Syncthing configuration is not using Morde's private-node defaults");
    }
    const knownFolders = new Set(items.map((item) => item.folderID));
    const knownDevices = new Set(items.map((item) => item.serverDeviceID));
    const enabledItems = items.filter((item) => item.enabled && item.bindingState === "active");
    const peers = new Map<string, { relayAddress: string; workspaceIDs: Set<string> }>();
    for (const item of enabledItems) {
      const peer = peers.get(item.serverDeviceID) ?? { relayAddress: "", workspaceIDs: new Set<string>() };
      peer.relayAddress = peer.relayAddress ? `${peer.relayAddress}\n${item.relayAddress}` : item.relayAddress;
      peer.workspaceIDs.add(item.workspaceID);
      peers.set(item.serverDeviceID, peer);
    }
    if (foldersValue.some((folder) => typeof folder.id !== "string" || !knownFolders.has(folder.id)) ||
      devicesValue.some((device) => typeof device.deviceID !== "string" || (device.deviceID !== localDeviceID && !knownDevices.has(device.deviceID)))) {
      throw new Error("Syncthing contains an unmanaged Workspace folder or device");
    }
    const selfDevice = devicesValue.find((device) => device.deviceID === localDeviceID);
    if (!selfDevice || devicesValue.filter((device) => device.deviceID === localDeviceID).length !== 1) {
      throw new Error("Syncthing local device configuration is invalid");
    }

    const devices = [selfDevice, ...[...peers].map(([deviceID, peer]) => ({
      ...defaultDevice,
      deviceID,
      name: "Morde Workspace",
      addresses: [...new Set(peer.relayAddress.split("\n"))],
      introducer: false,
      skipIntroductionRemovals: false,
      introducedBy: "",
      paused: false,
      allowedNetworks: [],
      autoAcceptFolders: false,
      untrusted: false,
    }))];
    const folders = items.map((item) => ({
      ...defaultFolder,
      id: item.folderID,
      label: `Morde Workspace ${item.workspaceID}`,
      path: item.directory,
      filesystemType: "basic",
      type: "sendreceive",
      devices: [...(enabledItems.includes(item) ? [{ deviceID: item.serverDeviceID }] : []), { deviceID: localDeviceID }],
      paused: !enabledItems.includes(item),
      mordeMaxSingleFileBytes: maxSingleFileBytes,
      mordeQuotaSocket: "",
      mordeWorkspaceId: "",
    }));

    await this.request("/rest/config/options", "PATCH", { relaysEnabled: peers.size > 0 });
    await this.request("/rest/config/devices", "PUT", devices);
    await this.request("/rest/config/folders", "PUT", folders);
    const [configuredOptions, configuredDevices, configuredFolders] = await Promise.all([
      this.request("/rest/config/options"),
      this.request("/rest/config/devices"),
      this.request("/rest/config/folders"),
    ]) as [Record<string, unknown>, Array<Record<string, unknown>>, Array<Record<string, unknown>>];
    if (!safeNetworkOptions(configuredOptions) || configuredOptions.relaysEnabled !== (peers.size > 0) ||
      !Array.isArray(configuredDevices) || configuredDevices.length !== devices.length || devices.some((expected) => {
        const actual = configuredDevices.find((device) => device.deviceID === expected.deviceID);
        return !actual || !sameStringList(actual.addresses, expected.addresses);
      }) || !Array.isArray(configuredFolders) || configuredFolders.length !== folders.length || folders.some((expected) => {
      const actual = configuredFolders.find((folder) => folder.id === expected.id);
      return !actual || actual.path !== expected.path || actual.paused !== expected.paused ||
        actual.filesystemType !== "basic" || actual.mordeMaxSingleFileBytes !== maxSingleFileBytes ||
        actual.type !== "sendreceive" || !sameDeviceIDs(actual.devices, expected.devices);
    })) throw new Error("Syncthing did not apply the Workspace folder configuration");
    await this.request("/rest/system/resume", "POST");
  }

  async workspaceStatus(folderID: string): Promise<WorkspaceFolderStatus> {
    if (!folderIDPattern.test(folderID)) throw new Error("Workspace folder ID is invalid");
    // shortcut: status refresh is manual and local-only; replace it when a trusted cross-device status contract exists.
    const value = await this.request(`/rest/db/status?folder=${encodeURIComponent(folderID)}`) as Record<string, unknown> | null;
    if (!value || typeof value.state !== "string" || !/^[A-Za-z0-9_-]{1,32}$/.test(value.state) ||
      !["needBytes", "needFiles", "pullErrors"].every((key) => Number.isSafeInteger(value[key]) && (value[key] as number) >= 0)) {
      throw new Error("Invalid Syncthing Workspace status");
    }
    return { state: value.state, needBytes: value.needBytes as number, needFiles: value.needFiles as number, pullErrors: value.pullErrors as number };
  }

  async identity(): Promise<SyncthingIdentity> {
    if (!this.home || !this.deviceID || !this.child) throw new Error("Syncthing node is not running");
    const certificatePath = join(this.home, "cert.pem");
    const keyPath = join(this.home, "key.pem");
    const [certificateInfo, keyInfo] = await Promise.all([lstat(certificatePath), lstat(keyPath)]);
    if (certificateInfo.isSymbolicLink() || !certificateInfo.isFile() || keyInfo.isSymbolicLink() || !keyInfo.isFile()) {
      throw new Error("Syncthing device identity is unavailable");
    }
    await chmod(keyPath, 0o600);
    const securedKeyInfo = await lstat(keyPath);
    if (!securedKeyInfo.isFile() || securedKeyInfo.isSymbolicLink() || (process.platform !== "win32" && (securedKeyInfo.mode & 0o077) !== 0)) {
      throw new Error("Syncthing private key permissions are unsafe");
    }
    try {
      const certificate = new X509Certificate(await readFile(certificatePath));
      const privateKey = createPrivateKey(await readFile(keyPath));
      if (certificate.raw.length === 0 || certificate.raw.length > 16 * 1024 ||
        certificate.publicKey.asymmetricKeyType !== "ed25519" || privateKey.asymmetricKeyType !== "ed25519") {
        throw new Error("invalid key type");
      }
      const probe = randomBytes(32);
      if (!verify(null, probe, certificate.publicKey, sign(null, probe, privateKey))) throw new Error("key mismatch");
      return { deviceID: this.deviceID, certificateDER: new Uint8Array(certificate.raw) };
    } catch { throw new Error("Syncthing device identity is invalid"); }
  }

  async signDeviceProof(payload: Uint8Array): Promise<Uint8Array> {
    if (!this.home || !this.child || !(payload instanceof Uint8Array) || payload.length === 0 || payload.length > 16 * 1024) {
      throw new Error("Syncthing device proof is invalid");
    }
    const keyPath = join(this.home, "key.pem");
    try {
      const info = await lstat(keyPath);
      if (info.isSymbolicLink() || !info.isFile()) throw new Error("invalid key file");
      await chmod(keyPath, 0o600);
      const privateKey = createPrivateKey(await readFile(keyPath));
      if (privateKey.asymmetricKeyType !== "ed25519") throw new Error("invalid key type");
      return new Uint8Array(sign(null, payload, privateKey));
    } catch { throw new Error("Syncthing device proof is unavailable"); }
  }

  async stop(): Promise<boolean> {
    const starting = this.startPromise;
    if (starting) await starting.catch(() => undefined);
    return this.stopChild();
  }

  private async waitUntilReady(): Promise<string> {
    const deadline = Date.now() + readinessTimeout;
    while (Date.now() < deadline) {
      if (!this.child || this.child.exitCode !== null || this.child.signalCode !== null) throw new Error("Syncthing exited before becoming ready");
      try {
        const status = await this.request("/rest/system/status") as NodeStatus | null;
        if (typeof status?.myID === "string" && deviceIDPattern.test(status.myID)) return status.myID;
      } catch { /* The local GUI can take a moment to bind. */ }
      await new Promise((resolveDelay) => setTimeout(resolveDelay, 200));
    }
    throw new Error("Syncthing local control did not become ready");
  }

  private async runCommand(binary: string, args: string[], env: NodeJS.ProcessEnv): Promise<void> {
    let child: ChildProcess;
    try { child = this.spawnProcess(binary, args, { env, shell: false, stdio: "ignore", windowsHide: true }); }
    catch { throw new Error("Could not initialize the private Syncthing node"); }
    await new Promise<void>((resolveCommand, rejectCommand) => {
      let settled = false;
      const settle = (error?: Error) => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        if (error) rejectCommand(error);
        else resolveCommand();
      };
      const timer = setTimeout(() => {
        child.kill();
        settle(new Error("Syncthing initialization timed out"));
      }, 30_000);
      child.once("error", () => settle(new Error("Could not initialize the private Syncthing node")));
      child.once("exit", (code) => code === 0 ? settle() : settle(new Error("Syncthing initialization failed")));
    });
  }

  private async stopChild(): Promise<boolean> {
    const child = this.child;
    if (!child) return true;
    this.stopping = true;
    const exited = this.exited;
    child.kill();
    if (exited) await Promise.race([exited, delay(stopTimeout)]);
    if (this.child === child) {
      child.kill("SIGKILL");
      if (exited) await Promise.race([exited, delay(2_000)]);
    }
    const stopped = this.child !== child;
    if (stopped) this.stopping = false;
    return stopped;
  }

  private clearChild(child: ChildProcess): void {
    if (this.child !== child) return;
    this.child = null;
    this.exited = null;
    this.apiKey = null;
    this.guiAddress = null;
    this.deviceID = null;
    this.home = null;
  }
}

async function loopbackPort(): Promise<number> {
  const server = createServer();
  await new Promise<void>((resolveListen, rejectListen) => {
    server.once("error", rejectListen);
    server.listen(0, "127.0.0.1", resolveListen);
  });
  const address = server.address();
  await new Promise<void>((resolveClose, rejectClose) => server.close((error) => error ? rejectClose(error) : resolveClose()));
  if (!address || typeof address === "string") throw new Error("Could not reserve local Syncthing control port");
  return address.port;
}

async function secureDirectory(directory: string): Promise<void> {
  const info = await lstat(directory);
  if (info.isSymbolicLink() || !info.isDirectory()) throw new Error("Syncthing node home is not a real directory");
  if (process.platform === "win32") return;
  await chmod(directory, 0o700);
  const secured = await lstat(directory);
  if (secured.isSymbolicLink() || !secured.isDirectory() || (secured.mode & 0o077) !== 0) throw new Error("Syncthing node home permissions are unsafe");
}

function safeNetworkOptions(options: Record<string, unknown>): boolean {
  return Array.isArray(options.listenAddresses) && options.listenAddresses.length === 1 && options.listenAddresses[0] === "" &&
    options.globalAnnounceEnabled === false && options.localAnnounceEnabled === false && options.natEnabled === false &&
    options.announceLANAddresses === false && options.crashReportingEnabled === false && options.autoUpgradeIntervalH === 0 &&
    options.startBrowser === false;
}

function sameStringList(actual: unknown, expected: unknown): boolean {
  return Array.isArray(actual) && Array.isArray(expected) &&
    actual.every((item) => typeof item === "string") && expected.every((item) => typeof item === "string") &&
    JSON.stringify([...actual].sort()) === JSON.stringify([...expected].sort());
}

function sameDeviceIDs(actual: unknown, expected: Array<{ deviceID: string }>): boolean {
  if (!Array.isArray(actual) || actual.some((item) => !item || typeof item !== "object" || typeof item.deviceID !== "string")) return false;
  return sameStringList(actual.map((item) => item.deviceID), expected.map((item) => item.deviceID));
}

function validRelayAddress(value: unknown, expectedDeviceID: string): value is string {
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

function delay(milliseconds: number): Promise<void> {
  return new Promise((resolveDelay) => setTimeout(resolveDelay, milliseconds));
}
