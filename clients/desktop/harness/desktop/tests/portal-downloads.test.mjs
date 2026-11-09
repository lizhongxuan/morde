import assert from "node:assert/strict";
import { createHash, generateKeyPairSync, sign } from "node:crypto";
import { spawnSync } from "node:child_process";
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { stagePortalDownloads } from "../scripts/portal-downloads.mjs";

const version = "0.1.0";
const portalURL = "https://portal.example.test";
const sidecarSuffix = ".morde-acceptance.json";
const sidecarFormat = "morde-desktop-acceptance-v2";
const envelopeFormat = "morde-desktop-acceptance-envelope-v1";
const keyID = "test-release-key";
const capabilities = ["install", "launch_idle_network", "user_web_workbench", "workspace_sync_recovery", "update", "uninstall"];
const desktopRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const targets = [
  { platform: "macos", architecture: "arm64", extension: ".dmg", signature_status: "signed_and_notarized" },
  { platform: "macos", architecture: "x64", extension: ".dmg", signature_status: "signed_and_notarized" },
  { platform: "windows", architecture: "x64", extension: ".exe", signature_status: "signed" },
  { platform: "linux", architecture: "x64", extension: ".AppImage", signature_status: "not_applicable" },
];

async function fixture(t) {
  const root = await mkdtemp(path.join(os.tmpdir(), "morde-client-downloads-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const distDir = path.join(root, "dist");
  const outputDir = path.join(root, "portal", "downloads");
  await mkdir(distDir, { recursive: true });
  return { distDir, outputDir };
}

function filename(target) {
  return `morde-desktop-${version}-${target.platform}-${target.architecture}${target.extension}`;
}

function createTestKeyPair() {
  const { privateKey, publicKey } = generateKeyPairSync("ed25519");
  return { privateKey, publicKeyPEM: publicKey.export({ type: "spki", format: "pem" }) };
}

function trustKeys(t, keys) {
  const previous = process.env.MORDE_PORTAL_TRUSTED_KEYS_JSON;
  if (keys === undefined) delete process.env.MORDE_PORTAL_TRUSTED_KEYS_JSON;
  else process.env.MORDE_PORTAL_TRUSTED_KEYS_JSON = JSON.stringify(keys);
  t.after(() => {
    if (previous === undefined) delete process.env.MORDE_PORTAL_TRUSTED_KEYS_JSON;
    else process.env.MORDE_PORTAL_TRUSTED_KEYS_JSON = previous;
  });
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

async function writeArtifact(distDir, target, privateKey, content = `${target.platform} ${target.architecture}`) {
  const name = filename(target);
  const bytes = Buffer.from(content);
  const sha256 = createHash("sha256").update(bytes).digest("hex");
  await writeFile(path.join(distDir, name), bytes);
  const record = {
    format: sidecarFormat,
    key_id: keyID,
    version,
    platform: target.platform,
    architecture: target.architecture,
    filename: name,
    bytes: bytes.length,
    sha256,
    source_commit: "a".repeat(40),
    build_id: "desktop-build-2026-11-03-001",
    evidence_sha256: createHash("sha256").update(`evidence:${target.platform}:${target.architecture}`).digest("hex"),
    acceptance: {
      verified_at: "2026-10-27T06:43:45.000Z",
      evidence_ref: `release/${target.platform}-${target.architecture}/run-42`,
      signature_status: target.signature_status,
      capabilities: [...capabilities],
    },
  };
  record.signature = sign(null, Buffer.from(JSON.stringify(acceptanceEnvelope(record))), privateKey).toString("base64");
  await writeFile(path.join(distDir, `${name}${sidecarSuffix}`), JSON.stringify(record));
  return { record, bytes };
}

async function assertNotStaged(outputDir, target) {
  await assert.rejects(readFile(path.join(outputDir, "files", filename(target))), { code: "ENOENT" });
}

test("stages current Morde-client filenames only after Ed25519 verification", async (t) => {
  const { distDir, outputDir } = await fixture(t);
  const { privateKey, publicKeyPEM } = createTestKeyPair();
  trustKeys(t, { [keyID]: publicKeyPEM });
  const built = [];
  for (const target of targets) built.push({ target, ...await writeArtifact(distDir, target, privateKey) });

  const manifest = await stagePortalDownloads({ distDir, outputDir, version, portalURL });
  assert.equal(manifest.schema_version, 3);
  assert.equal(manifest.portal_url, `${portalURL}/`);
  assert.deepEqual(manifest.artifacts.map(({ platform, architecture }) => [platform, architecture]), [
    ["macos", "arm64"], ["macos", "x64"], ["windows", "x64"], ["linux", "x64"],
  ]);
  for (const { target, record, bytes } of built) {
    const artifact = manifest.artifacts.find((item) => item.platform === target.platform && item.architecture === target.architecture);
    assert.equal(artifact.filename, filename(target));
    assert.equal(artifact.download_url, `/downloads/files/${filename(target)}`);
    assert.equal(artifact.bytes, bytes.length);
    assert.equal(artifact.sha256, record.sha256);
    assert.deepEqual(artifact.acceptance, {
      status: "verified",
      verified_at: record.acceptance.verified_at,
      evidence_ref: record.acceptance.evidence_ref,
      signature_status: target.signature_status,
      capabilities,
    });
    const staged = await readFile(path.join(outputDir, "files", filename(target)));
    assert.deepEqual(staged, bytes);
  }
  assert.deepEqual(JSON.parse(await readFile(path.join(outputDir, "manifest.json"), "utf8")), manifest);
});

test("without acceptance sidecars writes an empty manifest and copies no installers without trusted keys", async (t) => {
  const { distDir, outputDir } = await fixture(t);
  trustKeys(t, undefined);
  for (const target of targets) await writeFile(path.join(distDir, filename(target)), "unverified package");

  const manifest = await stagePortalDownloads({ distDir, outputDir, version, portalURL });
  assert.deepEqual(manifest, { schema_version: 3, version, portal_url: `${portalURL}/`, artifacts: [] });
  await assertNotStaged(outputDir, targets[0]);
  assert.deepEqual(JSON.parse(await readFile(path.join(outputDir, "manifest.json"), "utf8")), manifest);
});

test("rejects a signed sidecar when the local trusted key is missing or does not recognize its key id", async (t) => {
  const keyPair = createTestKeyPair();
  const { distDir, outputDir } = await fixture(t);
  await writeArtifact(distDir, targets[0], keyPair.privateKey);

  trustKeys(t, undefined);
  await assert.rejects(stagePortalDownloads({ distDir, outputDir, version, portalURL }), /Trusted Ed25519 keys are not configured/);
  await assertNotStaged(outputDir, targets[0]);

  const otherPair = createTestKeyPair();
  trustKeys(t, { "different-release-key": otherPair.publicKeyPEM });
  await assert.rejects(stagePortalDownloads({ distDir, outputDir, version, portalURL }), /uses an unknown key/);
  await assertNotStaged(outputDir, targets[0]);
});

test("rejects tampered signed metadata and installer bytes before copying", async (t) => {
  const { privateKey, publicKeyPEM } = createTestKeyPair();
  trustKeys(t, { [keyID]: publicKeyPEM });

  const metadataFixture = await fixture(t);
  const target = targets[0];
  const { record } = await writeArtifact(metadataFixture.distDir, target, privateKey);
  const sidecarPath = path.join(metadataFixture.distDir, `${filename(target)}${sidecarSuffix}`);
  await writeFile(sidecarPath, JSON.stringify({ ...record, build_id: "changed-build-identifier" }));
  await assert.rejects(stagePortalDownloads({ ...metadataFixture, version, portalURL }), /Acceptance signature is invalid/);
  await assertNotStaged(metadataFixture.outputDir, target);

  const packageFixture = await fixture(t);
  const packageRecord = await writeArtifact(packageFixture.distDir, target, privateKey);
  await writeFile(path.join(packageFixture.distDir, filename(target)), Buffer.concat([packageRecord.bytes, Buffer.from("tampered")]));
  await assert.rejects(stagePortalDownloads({ ...packageFixture, version, portalURL }), /does not match its installer/);
  await assertNotStaged(packageFixture.outputDir, target);
});

test("rejects legacy self-asserted verified sidecars and dirty v2 sidecars", async (t) => {
  const { privateKey, publicKeyPEM } = createTestKeyPair();
  trustKeys(t, { [keyID]: publicKeyPEM });
  const legacyFixture = await fixture(t);
  const target = targets[0];
  const { bytes } = await writeArtifact(legacyFixture.distDir, target, privateKey);
  const legacy = {
    format: "morde-desktop-acceptance-v1",
    version,
    platform: target.platform,
    architecture: target.architecture,
    filename: filename(target),
    bytes: bytes.length,
    sha256: createHash("sha256").update(bytes).digest("hex"),
    acceptance: {
      status: "verified",
      verified_at: "2026-10-27T06:43:45.000Z",
      evidence_ref: "release/macos-arm64/run-42",
      signature_status: target.signature_status,
      capabilities: [...capabilities],
    },
  };
  const sidecarPath = path.join(legacyFixture.distDir, `${filename(target)}${sidecarSuffix}`);
  await writeFile(sidecarPath, JSON.stringify(legacy));
  await assert.rejects(stagePortalDownloads({ ...legacyFixture, version, portalURL }), /contains missing or unexpected fields/);
  await assertNotStaged(legacyFixture.outputDir, target);

  const dirtyFixture = await fixture(t);
  const { record } = await writeArtifact(dirtyFixture.distDir, target, privateKey);
  await writeFile(path.join(dirtyFixture.distDir, `${filename(target)}${sidecarSuffix}`), JSON.stringify({ ...record, verified: true }));
  await assert.rejects(stagePortalDownloads({ ...dirtyFixture, version, portalURL }), /contains missing or unexpected fields/);
  await assertNotStaged(dirtyFixture.outputDir, target);
});

test("stage:portal forwards the URL and stages only verified files in local test directories", async (t) => {
  const { distDir, outputDir } = await fixture(t);
  const { privateKey, publicKeyPEM } = createTestKeyPair();
  await writeArtifact(distDir, targets[0], privateKey);
  const env = {
    ...process.env,
    MORDE_PORTAL_TRUSTED_KEYS_JSON: JSON.stringify({ [keyID]: publicKeyPEM }),
    MORDE_PORTAL_STAGE_DIST_DIR: distDir,
    MORDE_PORTAL_STAGE_OUTPUT_DIR: outputDir,
  };
  const result = spawnSync("npm", ["run", "stage:portal", "--", portalURL], { cwd: desktopRoot, env, encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr || result.stdout);
  const manifest = JSON.parse(await readFile(path.join(outputDir, "manifest.json"), "utf8"));
  assert.equal(manifest.portal_url, `${portalURL}/`);
  assert.deepEqual(manifest.artifacts.map(({ filename: staged }) => staged), [filename(targets[0])]);
  assert.deepEqual(await readFile(path.join(outputDir, "files", filename(targets[0]))), Buffer.from("macos arm64"));
});

test("stage:portal fails closed for a missing URL or untrusted sidecar without creating output", async (t) => {
  const missingArgFixture = await fixture(t);
  const baseEnv = {
    ...process.env,
    MORDE_PORTAL_STAGE_DIST_DIR: missingArgFixture.distDir,
    MORDE_PORTAL_STAGE_OUTPUT_DIR: missingArgFixture.outputDir,
  };
  const missingArg = spawnSync("npm", ["run", "stage:portal"], { cwd: desktopRoot, env: baseEnv, encoding: "utf8" });
  assert.notEqual(missingArg.status, 0);
  assert.match(`${missingArg.stdout}\n${missingArg.stderr}`, /Usage: npm run stage:portal -- https:\/\/portal\.example/);
  await assert.rejects(readFile(path.join(missingArgFixture.outputDir, "manifest.json")), { code: "ENOENT" });

  const invalidFixture = await fixture(t);
  const { privateKey } = createTestKeyPair();
  await writeArtifact(invalidFixture.distDir, targets[0], privateKey);
  const invalid = spawnSync("npm", ["run", "stage:portal", "--", portalURL], {
    cwd: desktopRoot,
    env: { ...baseEnv, MORDE_PORTAL_STAGE_DIST_DIR: invalidFixture.distDir, MORDE_PORTAL_STAGE_OUTPUT_DIR: invalidFixture.outputDir },
    encoding: "utf8",
  });
  assert.notEqual(invalid.status, 0);
  assert.match(`${invalid.stdout}\n${invalid.stderr}`, /Trusted Ed25519 keys are not configured/);
  await assert.rejects(readFile(path.join(invalidFixture.outputDir, "manifest.json")), { code: "ENOENT" });
  await assertNotStaged(invalidFixture.outputDir, targets[0]);
});
