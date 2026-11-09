import { createHash, createPublicKey, randomUUID, verify } from "node:crypto";
import { createReadStream } from "node:fs";
import { copyFile, lstat, mkdir, readFile, readdir, realpath, rename, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const desktopRoot = path.resolve(scriptDir, "..");
const packageInfo = JSON.parse(await readFile(path.join(desktopRoot, "package.json"), "utf8"));
const sidecarSuffix = ".morde-acceptance.json";
const acceptanceFormat = "morde-desktop-acceptance-v2";
const envelopeFormat = "morde-desktop-acceptance-envelope-v1";
const capabilities = ["install", "launch_idle_network", "user_web_workbench", "workspace_sync_recovery", "update", "uninstall"];
const targets = [
  { platform: "macos", architecture: "arm64", extension: ".dmg", signatureStatus: "signed_and_notarized" },
  { platform: "macos", architecture: "x64", extension: ".dmg", signatureStatus: "signed_and_notarized" },
  { platform: "windows", architecture: "x64", extension: ".exe", signatureStatus: "signed" },
  { platform: "linux", architecture: "x64", extension: ".AppImage", signatureStatus: "not_applicable" },
];

function expectedFilename(target, version) {
  return `morde-desktop-${version}-${target.platform}-${target.architecture}${target.extension}`;
}

function targetKeys(target) { return `${target.platform}:${target.architecture}`; }

async function sha256File(filePath) {
  const hash = createHash("sha256");
  for await (const chunk of createReadStream(filePath)) hash.update(chunk);
  return hash.digest("hex");
}

async function requireRegularFile(filePath, label) {
  let info;
  try { info = await lstat(filePath); }
  catch (error) { throw new Error(`${label} is missing or unreadable`, { cause: error }); }
  if (!info.isFile() || info.isSymbolicLink()) throw new Error(`${label} must be a regular file`);
  return info;
}

async function requireDirectory(directoryPath, label, { create = false } = {}) {
  if (create) await mkdir(directoryPath, { recursive: true, mode: 0o755 });
  let info;
  try { info = await lstat(directoryPath); }
  catch (error) { throw new Error(`${label} is missing or unreadable`, { cause: error }); }
  if (!info.isDirectory() || info.isSymbolicLink()) throw new Error(`${label} must be a real directory`);
  return realpath(directoryPath);
}

async function writeAtomic(filePath, contents) {
  const temporary = `${filePath}.${randomUUID()}.tmp`;
  try {
    await writeFile(temporary, contents, { flag: "wx", mode: 0o644 });
    await rename(temporary, filePath);
  } finally { await rm(temporary, { force: true }); }
}

function exactKeys(value, expected, label) {
  if (!value || typeof value !== "object" || Array.isArray(value) ||
      Object.keys(value).length !== expected.length || expected.some((key) => !Object.hasOwn(value, key))) {
    throw new Error(`${label} contains missing or unexpected fields`);
  }
}

function checkedPortalURL(input) {
  let url;
  try { url = new URL(input); }
  catch { throw new Error("Portal URL is invalid"); }
  if (!input || !["http:", "https:"].includes(url.protocol) || url.username || url.password ||
      url.pathname !== "/" || url.search || url.hash) throw new Error("Portal URL must be an origin");
  return `${url.origin}/`;
}

function loadTrustedPublicKeys() {
  const raw = process.env.MORDE_PORTAL_TRUSTED_KEYS_JSON;
  if (!raw) throw new Error("Trusted Ed25519 keys are not configured");
  let configured;
  try { configured = JSON.parse(raw); }
  catch (error) { throw new Error("Trusted Ed25519 key configuration is invalid", { cause: error }); }
  if (!configured || typeof configured !== "object" || Array.isArray(configured) || !Object.keys(configured).length) {
    throw new Error("Trusted Ed25519 key configuration is invalid");
  }
  const keys = new Map();
  for (const [keyID, pem] of Object.entries(configured)) {
    if (!/^[A-Za-z0-9][A-Za-z0-9._-]{2,63}$/.test(keyID) || typeof pem !== "string" ||
        !pem.includes("-----BEGIN PUBLIC KEY-----") || pem.includes("PRIVATE KEY-----")) {
      throw new Error("Trusted Ed25519 key configuration is invalid");
    }
    let key;
    try { key = createPublicKey(pem); }
    catch (error) { throw new Error("Trusted Ed25519 key configuration is invalid", { cause: error }); }
    if (key.asymmetricKeyType !== "ed25519") throw new Error("Trusted key is not Ed25519");
    keys.set(keyID, key);
  }
  return keys;
}

function acceptanceEnvelope(record) {
  return {
    format: envelopeFormat,
    key_id: record.key_id,
    version: record.version,
    platform: record.platform,
    architecture: record.architecture,
    filename: record.filename,
    bytes: record.bytes,
    sha256: record.sha256,
    source_commit: record.source_commit,
    build_id: record.build_id,
    evidence_sha256: record.evidence_sha256,
    acceptance: {
      verified_at: record.acceptance.verified_at,
      evidence_ref: record.acceptance.evidence_ref,
      signature_status: record.acceptance.signature_status,
      capabilities: [...capabilities],
    },
  };
}

function validateAcceptance(record, target, filename, bytes, sha256, version, trustedKeys) {
  exactKeys(record, ["format", "key_id", "signature", "version", "platform", "architecture", "filename", "bytes", "sha256", "source_commit", "build_id", "evidence_sha256", "acceptance"], "Acceptance sidecar");
  if (record.format !== acceptanceFormat || !/^[A-Za-z0-9][A-Za-z0-9._-]{2,63}$/.test(record.key_id) ||
      record.version !== version || record.platform !== target.platform || record.architecture !== target.architecture ||
      record.filename !== filename || record.bytes !== bytes || record.sha256 !== sha256 ||
      typeof record.source_commit !== "string" || !/^(?:[a-f0-9]{40}|[a-f0-9]{64})$/.test(record.source_commit) ||
      typeof record.build_id !== "string" || !/^[A-Za-z0-9][A-Za-z0-9._-]{2,127}$/.test(record.build_id) ||
      typeof record.evidence_sha256 !== "string" || !/^[a-f0-9]{64}$/.test(record.evidence_sha256)) {
    throw new Error(`Acceptance sidecar does not match its installer: ${filename}`);
  }
  const publicKey = trustedKeys.get(record.key_id);
  if (!publicKey) throw new Error(`Acceptance sidecar uses an unknown key: ${record.key_id}`);
  const acceptance = record.acceptance;
  exactKeys(acceptance, ["verified_at", "evidence_ref", "signature_status", "capabilities"], "Acceptance evidence");
  const verifiedAt = acceptance.verified_at;
  const evidenceRef = acceptance.evidence_ref;
  const validTimestamp = typeof verifiedAt === "string" &&
    /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/.test(verifiedAt) &&
    !Number.isNaN(Date.parse(verifiedAt)) && new Date(verifiedAt).toISOString() === verifiedAt;
  const validEvidenceRef = typeof evidenceRef === "string" &&
    /^[A-Za-z0-9][A-Za-z0-9._/-]{2,159}$/.test(evidenceRef) && !evidenceRef.includes("..");
  const verifiedCapabilities = acceptance.capabilities;
  const validCapabilities = Array.isArray(verifiedCapabilities) &&
    verifiedCapabilities.length === capabilities.length && new Set(verifiedCapabilities).size === capabilities.length &&
    capabilities.every((capability) => verifiedCapabilities.includes(capability));
  if (!validTimestamp || !validEvidenceRef || !validCapabilities ||
      acceptance.signature_status !== target.signatureStatus) {
    throw new Error(`Acceptance evidence is incomplete or invalid: ${filename}`);
  }
  const signature = typeof record.signature === "string" && /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(record.signature)
    ? Buffer.from(record.signature, "base64") : Buffer.alloc(0);
  if (!signature.length || signature.length !== 64 || signature.toString("base64") !== record.signature ||
      !verify(null, Buffer.from(JSON.stringify(acceptanceEnvelope(record))), publicKey, signature)) {
    throw new Error(`Acceptance signature is invalid: ${filename}`);
  }
  return {
    status: "verified",
    verified_at: verifiedAt,
    evidence_ref: evidenceRef,
    signature_status: target.signatureStatus,
    capabilities: [...capabilities],
  };
}

async function readVerifiedArtifacts(distDir, version) {
  const distRoot = await requireDirectory(distDir, "Morde-client dist directory");
  const entries = await readdir(distRoot, { withFileTypes: true });
  const sidecars = entries.filter((entry) => entry.name.endsWith(sidecarSuffix));
  if (!sidecars.length) return [];
  const trustedKeys = loadTrustedPublicKeys();
  const knownTargets = new Map(targets.map((target) => [
    `${expectedFilename(target, version)}${sidecarSuffix}`,
    target,
  ]));
  const artifacts = [];
  const seenTargets = new Set();

  for (const sidecar of sidecars) {
    const target = knownTargets.get(sidecar.name);
    if (!target) throw new Error(`Unexpected or stale acceptance sidecar: ${sidecar.name}`);
    if (seenTargets.has(targetKeys(target))) throw new Error(`Duplicate acceptance sidecar for ${targetKeys(target)}`);
    seenTargets.add(targetKeys(target));
    const sidecarPath = path.join(distRoot, sidecar.name);
    await requireRegularFile(sidecarPath, "Acceptance sidecar");
    let record;
    try { record = JSON.parse(await readFile(sidecarPath, "utf8")); }
    catch (error) { throw new Error(`Could not read acceptance sidecar: ${sidecar.name}`, { cause: error }); }

    const filename = expectedFilename(target, version);
    const sourcePath = path.join(distRoot, filename);
    const sourceInfo = await requireRegularFile(sourcePath, "Morde-client installer");
    const sha256 = await sha256File(sourcePath);
    const acceptance = validateAcceptance(record, target, filename, sourceInfo.size, sha256, version, trustedKeys);
    artifacts.push({
      platform: target.platform,
      architecture: target.architecture,
      filename,
      download_url: `/downloads/files/${filename}`,
      bytes: sourceInfo.size,
      sha256,
      acceptance,
      sourcePath,
    });
  }

  return artifacts.sort((left, right) =>
    targets.findIndex((target) => targetKeys(target) === `${left.platform}:${left.architecture}`) -
    targets.findIndex((target) => targetKeys(target) === `${right.platform}:${right.architecture}`));
}

export async function stagePortalDownloads({ distDir, outputDir, version, portalURL }) {
  if (typeof version !== "string" || !/^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/.test(version)) throw new Error("Morde-client version is invalid");
  const normalizedPortalURL = checkedPortalURL(portalURL);
  const artifacts = await readVerifiedArtifacts(distDir, version);
  const outputRoot = await requireDirectory(outputDir, "Portal download directory", { create: true });
  const published = [];

  if (artifacts.length) {
    const filesDir = await requireDirectory(path.join(outputRoot, "files"), "Portal download files directory", { create: true });
    for (const artifact of artifacts) {
      const destination = path.join(filesDir, artifact.filename);
      try { await requireRegularFile(destination, "Existing Portal installer"); }
      catch (error) {
        if (!error.message.startsWith("Existing Portal installer is missing")) throw error;
      }
      const temporary = `${destination}.${randomUUID()}.tmp`;
      try {
        await copyFile(artifact.sourcePath, temporary);
        const copiedInfo = await requireRegularFile(temporary, "Staged Portal installer");
        if (copiedInfo.size !== artifact.bytes || await sha256File(temporary) !== artifact.sha256) {
          throw new Error(`Staged installer checksum did not match: ${artifact.filename}`);
        }
        await rename(temporary, destination);
      } finally { await rm(temporary, { force: true }); }
      const { sourcePath: _sourcePath, ...manifestArtifact } = artifact;
      published.push(manifestArtifact);
    }
  }

  const manifest = { schema_version: 3, version, portal_url: normalizedPortalURL, artifacts: published };
  await writeAtomic(path.join(outputRoot, "manifest.json"), `${JSON.stringify(manifest, null, 2)}\n`);
  return manifest;
}

async function main(args) {
  const [command, portalURL] = args;
  if (command !== "stage" || args.length !== 2) {
    throw new Error("Usage: npm run stage:portal -- https://portal.example");
  }
  const distDir = path.resolve(process.env.MORDE_PORTAL_STAGE_DIST_DIR || path.join(desktopRoot, "dist"));
  const outputDir = path.resolve(process.env.MORDE_PORTAL_STAGE_OUTPUT_DIR || path.resolve(desktopRoot, "../../../../web/portal/public/downloads"));
  const manifest = await stagePortalDownloads({
    distDir,
    outputDir,
    version: packageInfo.version,
    portalURL,
  });
  console.log(`Staged ${manifest.artifacts.length} verified Morde-client installer(s) for ${manifest.portal_url}`);
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
