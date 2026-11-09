import http from "node:http";
import https from "node:https";
import { createHash } from "node:crypto";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { HttpExecutor, configureRequestOptions, configureRequestUrl } from "builder-util-runtime";
import { WindowsUpdates, RestartNsisUpdater } from "../../out/windows-updates.js";
import { ProtectedExit } from "../../out/protected-exit.js";

// Only Electron's transport and installation side effect are replaced. Its metadata parsing,
// downloads, digest verification and disk cache remain the real library's implementation.
class NodeHttpExecutor extends HttpExecutor {
  createRequest(options, callback) {
    return (options.protocol === "http:" ? http : https).request(options, callback);
  }
  download(url, destination, options) {
    return options.cancellationToken.createPromise((resolve, reject, onCancel) => {
      const request = { headers: options.headers || undefined, redirect: "manual" };
      configureRequestUrl(url, request);
      configureRequestOptions(request);
      this.doDownload(request, { destination, options, onCancel,
        callback: (error) => error ? reject(error) : resolve(destination), responseHandler: null }, 0);
    });
  }
}

export async function updateFeed(t, { current = "1.0.3", version = "1.0.4", slow = false, confirm = false, busy = [], realInstall = false } = {}) {
  const root = mkdtempSync(join(tmpdir(), "nanomuse-update-flow-"));
  const bytes = Buffer.alloc(1024 * 128, 0x31);
  const digest = createHash("sha512").update(bytes).digest("base64");
  const seen = [];
  const states = [];
  const installs = [];
  const quits = [];
  const questions = [];
  const lines = [];
  const cacheFiles = [];
  let gate = true;
  let on = busy;
  const answers = { mirrorPackage: bytes, githubPackage: bytes, mirrorMetadata: version, githubMetadata: version,
    mirrorCheck: version, githubCheck: version, githubDigest: digest, stopError: false, installError: false };
  const metadata = (v) => `version: ${v}\nfiles:\n  - url: nanoMuse-Desktop-${v}-win-x64.exe\n    sha512: ${digest}\n    size: ${bytes.length}\npath: nanoMuse-Desktop-${v}-win-x64.exe\nsha512: ${digest}\nreleaseDate: '2026-10-12T00:00:00.000Z'\n`;
  const server = http.createServer((req, res) => {
    const path = new URL(req.url, "http://feed").pathname;
    seen.push(path);
    const mirror = path.startsWith("/mirror/");
    const data = path.endsWith(".yml")
      ? (path === "/mirror/latest.yml" ? answers.mirrorCheck : path === "/github/latest.yml" ? answers.githubCheck : mirror ? answers.mirrorMetadata : answers.githubMetadata)
      : mirror ? answers.mirrorPackage : answers.githubPackage;
    if (data === null) { res.writeHead(404).end(); return; }
    const body = path.endsWith(".yml") ? Buffer.from(metadata(data).replaceAll(digest, mirror ? digest : answers.githubDigest)) : data;
    res.writeHead(200, { "content-length": body.length });
    if (slow && path.endsWith(".exe")) {
      const third = Math.floor(body.length / 3);
      res.write(body.subarray(0, third));
      setTimeout(() => res.write(body.subarray(third, 2 * third)), 600);
      setTimeout(() => res.end(body.subarray(2 * third)), 1200);
    } else res.end(body);
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const base = `http://127.0.0.1:${server.address().port}/`;
  writeFileSync(join(root, "app-update.yml"), `provider: generic\nurl: ${base}mirror/\nupdaterCacheDirName: cache\n`);
  const work = {
    busy: async () => on,
    setGate: async (open) => { gate = open; },
    stopAll: async () => { if (answers.stopError) throw new Error("work did not end"); on = []; },
  };
  const guard = new ProtectedExit({ work, dialog: { confirm: async (items, intent) => {
    questions.push({ items: [...items], intent });
    return typeof confirm === "function" ? confirm() : confirm;
  } }, finish: () => quits.push({ gateOpen: gate, busy: [...on] }), log: (line) => lines.push(line) });
  const createUpdater = (canQuit = () => true) => {
    const app = { version: current, name: "nanomuse-update-flow", isPackaged: true, appUpdateConfigPath: join(root, "app-update.yml"),
      userDataPath: root, baseCachePath: root, whenReady: async () => undefined,
      quit: () => { if (!realInstall) throw new Error("real quit attempted"); quits.push({ gateOpen: gate, busy: [...on] }); }, relaunch: () => { throw new Error("real relaunch attempted"); },
      onQuit: () => { throw new Error("automatic quit installation enabled"); } };
    const updater = new RestartNsisUpdater(canQuit, app);
    updater.httpExecutor = new NodeHttpExecutor();
    updater.disableDifferentialDownload = true;
    updater.logger = { info: (line) => lines.push(line), warn: (line) => lines.push(line), error: (line) => lines.push(line) };
    updater.on("update-downloaded", (event) => cacheFiles.push(event.downloadedFile));
    if (realInstall) updater.spawnLog = async (_path, args) => {
      if (answers.installError) throw Object.assign(new Error("installer could not start"), { code: "EIO" });
      installs.push({ args, gateOpen: gate, busy: [...on] });
      return true;
    };
    else updater.quitAndInstall = (silent, reopen) => {
      if (answers.installError === "late") { setImmediate(() => updater.emit("error", new Error("installer could not start"))); return; }
      if (answers.installError) { updater.emit("error", new Error("installer could not start")); return; }
      installs.push({ silent, reopen, gateOpen: gate, busy: [...on] });
    };
    return updater;
  };
  const flow = new WindowsUpdates({ currentVersion: current, supported: true, createUpdater,
    sources: { mirrorCheck: `${base}mirror/`, githubCheck: `${base}github/`,
      mirrorVersion: (v) => `${base}mirror/v${v}/`, githubVersion: (v) => `${base}github/v${v}/` },
    handoff: (intent, install) => guard.requestExit(intent, install), cancelHandoff: () => guard.cancelHandoff() });
  flow.subscribe((state) => states.push(state));
  t.after(async () => {
    flow.dispose();
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
    rmSync(root, { recursive: true, force: true });
  });
  return { flow, guard, work, questions, answers, seen, states, installs, quits, lines, cacheFiles, bytes, root, gateOpen: () => gate };
}
