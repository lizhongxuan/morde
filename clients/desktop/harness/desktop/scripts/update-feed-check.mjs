// Consumes the Windows update assets the way the app's updater will: electron-updater itself
// (NsisUpdater) reads latest.yml from a loopback feed seeded with what electron-builder
// generated, finds the candidate version, downloads the installer it names and verifies it;
// and refuses an installer whose bytes do not match latest.yml's SHA-512, or whose file is
// missing. This is the "build assets → the update library can take and verify them" check
// release maintenance relies on (the release's update assets and SHA256SUMS-desktop.txt);
// it asserts the behaviour, not the build configuration.
//
// The library's whole download path (latest.yml parsing, the file the metadata names, the
// SHA-512 digest transform that decides "downloaded") is its own; only the transport socket
// is ours, a thin HttpExecutor over node:http where the app would use electron's net (the
// base class drives redirects, timeouts and the digest the same way either way). That keeps
// the check a plain Node run: no window, no GPU, nothing GUI to depend on.
//
// Run after `npm run dist` on Windows (`.github/workflows/desktop-app.yml` does, and CI keeps
// the run green from there): `node scripts/update-feed-check.mjs`; dist is the build output
// beside this script; `--dist=<path>` points it elsewhere.
//
// Isolation: the feed is a throwaway HTTP server on 127.0.0.1 serving the dist directory
// as-is; the updater's cache and config live in a fresh temp directory per scenario; the app
// adapter it gets is a stub with no real app behind it. autoDownload and autoInstallOnAppQuit
// are off and quit() is recorded: a run of this check must never install anything, whatever
// it finds or fails to find. Nothing is uploaded anywhere: consuming the assets locally is not
// a licence to publish them (that stays the release recipe's job).

import { createHash } from "node:crypto";
import http, { createServer } from "node:http";
import https from "node:https";
import { createReadStream, existsSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { HttpExecutor, configureRequestOptions, configureRequestUrl } from "builder-util-runtime";
import { NsisUpdater } from "electron-updater";

const dist = resolve(
  dirname(fileURLToPath(import.meta.url)),
  "..",
  process.argv.find((a) => a.startsWith("--dist="))?.slice("--dist=".length) ?? "dist",
);
const YML = join(dist, "latest.yml");
const TIMEOUT_MS = 10 * 60 * 1000; // the installer is a couple of hundred megabytes over loopback

/** latest.yml, electron-builder's update metadata: the version and the files it promises. */
function readMetadata() {
  if (!existsSync(YML)) throw new Error(`no update metadata: ${YML} is missing; the win publish config did not take`);
  const text = readFileSync(YML, "utf8");
  const version = /^version:\s*(\S+)\s*$/m.exec(text)?.[1];
  const files = /^files:\s*$/m.exec(text);
  const entry = files ? /^  - url:\s*(\S+)\s*\n    sha512:\s*(\S+)\s*\n    size:\s*(\d+)\s*$/m.exec(text) : null;
  if (!version || !entry) {
    throw new Error(`latest.yml is not in the shape electron-updater writes (version ${JSON.stringify(version)}, files entry ${Boolean(entry)})`);
  }
  return { version, installer: entry[1], sha512: entry[2], size: Number(entry[3]) };
}

function hash(path, algorithm) {
  return new Promise((resolveHash, reject) => {
    const h = createHash(algorithm);
    createReadStream(path)
      .on("data", (block) => h.update(block))
      .on("end", () => resolveHash(h.digest(algorithm === "sha512" ? "base64" : "hex")))
      .on("error", reject);
  });
}

/** The transport the app's updater would get from electron's net, over Node's own http(s):
 * createRequest for every request, and the file download the base class drives through it. */
class NodeHttpExecutor extends HttpExecutor {
  createRequest(options, callback) {
    // electron's net refuses a Host header (it moves it into host); node takes it either way,
    // but keep the two paths identical
    if (options.headers && options.headers.Host) {
      options.host = options.headers.Host;
      delete options.headers.Host;
    }
    return (options.protocol === "http:" ? http : https).request(options, callback);
  }

  download(url, destination, options) {
    return options.cancellationToken.createPromise((resolve, reject, onCancel) => {
      const requestOptions = { headers: options.headers || undefined, redirect: "manual" };
      configureRequestUrl(url, requestOptions);
      configureRequestOptions(requestOptions);
      this.doDownload(
        requestOptions,
        {
          destination,
          options,
          onCancel,
          callback: (error) => (error == null ? resolve(destination) : reject(error)),
          responseHandler: null,
        },
        0,
      );
    });
  }
}

/** The app adapter electron-updater gets: a stub (no window, no real app, a temp home). */
class CheckApp {
  constructor(base) {
    this.base = base;
    this.quitCalls = 0;
    this.relaunchCalls = 0;
  }
  get version() {
    // deliberately older than anything a release carries: the candidate must then be "an update"
    return "0.0.1";
  }
  get name() {
    return "nanomuse-update-feed-check";
  }
  get isPackaged() {
    return true;
  }
  get appUpdateConfigPath() {
    return join(this.base, "app-update.yml");
  }
  get userDataPath() {
    return this.base;
  }
  get baseCachePath() {
    return this.base;
  }
  whenReady() {
    return Promise.resolve();
  }
  quit() {
    this.quitCalls += 1;
  }
  relaunch() {
    this.relaunchCalls += 1;
  }
  onQuit() {
    // a stub has no quit to observe
  }
}

/**
 * A loopback feed serving dist/, with the scenario's twist on the installer's bytes: a
 * function over them (a corruption), or null (the file is not there).
 */
function feedServer(root, twist) {
  const server = createServer((req, res) => {
    const name = decodeURIComponent(new URL(req.url, "http://feed").pathname).replace(/^\/+/, "");
    const path = join(root, name);
    let data = null;
    try {
      data = readFileSync(path);
    } catch {
      res.writeHead(404, { "content-length": 0 });
      res.end();
      return;
    }
    if (name.endsWith(".exe") && twist) {
      const served = twist(data);
      if (served === null) {
        res.writeHead(404, { "content-length": 0 });
        res.end();
        return;
      }
      data = served;
    }
    res.writeHead(200, { "content-length": data.length, "content-type": name.endsWith(".yml") ? "text/yaml" : "application/octet-stream" });
    res.end(data);
  });
  return new Promise((resolveListen) => {
    server.listen(0, "127.0.0.1", () => resolveListen({ server, url: `http://127.0.0.1:${server.address().port}/` }));
  });
}

/** A real NsisUpdater over the stub app: no auto anything, its own temp cache. */
function makeUpdater(feedUrl, base) {
  const appAdapter = new CheckApp(base);
  // app-update.yml for electron-updater's own config reads; setFeedURL below is what it uses.
  writeFileSync(join(base, "app-update.yml"), `provider: generic\nurl: ${feedUrl}\n`);
  const updater = new NsisUpdater(null, appAdapter); // (options, app): the stub app, no feed options here
  updater.httpExecutor = new NodeHttpExecutor();
  updater.setFeedURL({ provider: "generic", url: feedUrl });
  updater.autoDownload = false;
  updater.autoInstallOnAppQuit = false;
  updater.allowDowngrade = false;
  return { updater, appAdapter };
}

function settle(promise, what, ms = TIMEOUT_MS) {
  let timer;
  const timeout = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(`${what}: timed out after ${Math.round(ms / 1000)}s`)), ms);
  });
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer));
}

/**
 * One scenario: check, then download, through the library, against the twisted feed.
 * Returns what happened: the candidate found, whether the download completed and was
 * verified, and what the updater tried to do to the (stub) app.
 */
async function scenario(name, twist, meta) {
  const base = mkdtempSync(join(tmpdir(), "nanomuse-update-feed-check-"));
  const { server, url } = await feedServer(dist, twist);
  const result = { name, candidate: null, available: false, resolvedInstaller: false, downloaded: false, verified: null, downloadError: null, quitCalls: 0, relaunchCalls: 0 };
  try {
    const { updater, appAdapter } = makeUpdater(url, base);
    const errors = [];
    updater.on("error", (e) => errors.push(String(e?.message ?? e)));
    updater.on("update-downloaded", () => {
      result.downloaded = true;
    });
    const check = await settle(updater.checkForUpdates(), `${name}: checkForUpdates`);
    result.available = check?.isUpdateAvailable === true;
    result.candidate = check?.updateInfo?.version ?? null;
    if (!result.available) throw new Error(`${name}: no update available on the feed (candidate ${JSON.stringify(result.candidate)})`);
    result.resolvedInstaller = (check.updateInfo?.files ?? [])[0]?.url === meta.installer;
    try {
      const downloaded = await settle(updater.downloadUpdate(), `${name}: downloadUpdate`);
      const file = downloaded?.[0];
      result.verified =
        result.downloaded && typeof file === "string" && existsSync(file) && (await hash(file, "sha256")) === (await hash(join(dist, meta.installer), "sha256"));
    } catch (e) {
      result.downloadError = String(e?.message ?? e);
    }
    result.errors = errors;
    result.quitCalls = appAdapter.quitCalls;
    result.relaunchCalls = appAdapter.relaunchCalls;
  } finally {
    server.close();
    server.closeAllConnections?.();
    rmSync(base, { recursive: true, force: true });
  }
  return result;
}

const failures = [];
function expect(ok, message) {
  console.log(`  ${ok ? "ok" : "FAIL"}: ${message}`);
  if (!ok) failures.push(message);
}

function shorten(text) {
  return String(text ?? "").replace(/\s+/g, " ").slice(0, 120);
}

const meta = readMetadata();
const installerPath = join(dist, meta.installer);
const blockmapPath = `${installerPath}.blockmap`;
const shellVersion = JSON.parse(readFileSync(join(dist, "..", "package.json"), "utf8")).version;

console.log(`update feed check: dist ${dist}`);
console.log(`  metadata: version ${meta.version}, installer ${meta.installer}, sha512 ${meta.sha512.slice(0, 12)}…, ${meta.size} bytes`);

// The set belongs to one version: the metadata, the installer it names and the blockmap the
// update library uses for differential downloads, all of this build.
expect(meta.version === shellVersion, `latest.yml's version (${meta.version}) is the shell's version (${shellVersion})`);
expect(meta.installer.includes(meta.version), `the installer's name carries the same version (${meta.installer})`);
expect(existsSync(installerPath), `the installer is in the build output (${meta.installer})`);
expect(existsSync(blockmapPath), `the installer's blockmap is in the build output (${meta.installer}.blockmap)`);
expect(statSync(installerPath).size === meta.size, `the installer's size matches the metadata (${meta.size} bytes)`);
expect((await hash(installerPath, "sha512")) === meta.sha512, "the installer's SHA-512 matches the metadata");

// 1. A complete feed: the library discovers the candidate, downloads the installer and
//    verifies it; nothing is installed and the stub app is never asked to quit or relaunch.
const complete = await scenario("complete feed", null, meta);
expect(complete.candidate === meta.version, `the library found candidate ${complete.candidate} on the feed`);
expect(complete.resolvedInstaller, "the metadata names the installer the feed serves");
expect(complete.verified === true, "the download completed and verified (byte-equal to the built installer)");
expect(complete.quitCalls === 0 && complete.relaunchCalls === 0, `nothing was installed (quit ${complete.quitCalls}, relaunch ${complete.relaunchCalls})`);

// 2. A corrupted installer: the library must refuse it (a SHA-512 mismatch, not "downloaded").
const corrupt = await scenario("corrupted installer", (data) => ((data[data.length >> 1] ^= 0xff), data), meta);
expect(corrupt.downloadError !== null, `the corrupted download was refused (${shorten(corrupt.downloadError)})`);
expect(/checksum|mismatch|sha/i.test(corrupt.downloadError ?? ""), "the refusal names the checksum");
expect(corrupt.downloaded === false && corrupt.verified !== true, "the corrupted download was never marked as downloaded");
expect(corrupt.quitCalls === 0 && corrupt.relaunchCalls === 0, `nothing was installed from it (quit ${corrupt.quitCalls}, relaunch ${corrupt.relaunchCalls})`);

// 3. A partial feed (the metadata is there, the installer is not): the library must fail, not
//    call anything downloaded.
const missing = await scenario("missing installer", () => null, meta);
expect(missing.downloadError !== null, `the missing installer failed the download (${shorten(missing.downloadError)})`);
expect(missing.downloaded === false && missing.verified !== true, "the missing installer was never marked as downloaded");

console.log(failures.length === 0 ? "update feed check: ok" : `update feed check: ${failures.length} failing`);
process.exitCode = failures.length === 0 ? 0 : 1;
