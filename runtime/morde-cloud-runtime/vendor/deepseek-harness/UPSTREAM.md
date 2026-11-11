# DSH source and update record

## Vendored source

- Upstream repository: `https://github.com/deepseek-ai/deepseek-harness`
- Runtime package: `@deepseek-ai/dsh-compaction-basic@0.2.2-rc.1`, MIT
- Source path: `packages/compaction/compaction-basic/src/`
- Pinned source commit: `fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159`
- Release tag commit: `dsh-v0.2.2-rc.1` / `66491b05e3f05ad35bcdd4f6cd7ed26041d9e3db`
- Morde copy: `vendor/deepseek-harness/packages/compaction/compaction-basic/`
- Local changes to copied upstream source: none. Source and manifest SHA-256 values are in that directory's `source.sha256`; `dist/index.js` is generated and ignored.

The pinned commit is the checkout in `project/deepseek-harness`. The source path and package manifest match the release tag. Run `scripts/verify-dsh-upstream.sh` from this Runtime directory to compare the local vendored source against the pinned Git objects and to verify the release path. Set `DSH_CHECKOUT` only if the checkout is elsewhere.

## Runtime package set

`runtime/morde-cloud-runtime/package.json` is the direct dependency authority; `package-lock.json` pins package versions, resolutions, and integrity values. All DSH packages below are MIT according to the locked package metadata.

| Runtime dependency | Version | Use |
| --- | --- | --- |
| `@deepseek-ai/cordis` | `4.1.0` | DSH plugin host |
| `@deepseek-ai/dsh-agent` | `0.2.2-rc.1` | Agent/session integration |
| `@deepseek-ai/dsh-agent-loop` | `0.2.2-rc.1` | DSH execution loop |
| `@deepseek-ai/dsh-compaction` | `0.2.2-rc.1` | Compaction service contract |
| `@deepseek-ai/dsh-llm` | `0.2.2-rc.1` | DSH model interface |
| `@deepseek-ai/dsh-mcp-client` | `0.2.2-rc.1` | DSH MCP client |
| `@deepseek-ai/dsh-session` | `0.2.2-rc.1` | Session runtime |
| `@deepseek-ai/dsh-session-checkpoint-policy` | `0.2.2-rc.1` | Checkpoint policy |
| `@deepseek-ai/dsh-session-persistence` | `0.2.2-rc.1` | Session persistence |
| `@deepseek-ai/dsh-session-projection` | `0.2.2-rc.1` | Session projection |
| `@deepseek-ai/dsh-system-prompt` | `0.2.2-rc.1` | Prompt service |
| `@deepseek-ai/dsh-token-meter` | `0.2.2-rc.1` | Token usage and compaction admission |
| `@deepseek-ai/dsh-tool-executor-native` | `0.2.2-rc.1` | Native tool execution adapter |
| `@deepseek-ai/dsh-tools` | `0.2.2-rc.1` | Tool contracts |

The reachable transitive DSH packages are `dsh-brand`, `dsh-timeout`, `dsh-typert-protocol`, `dsh-util-crypto`, and `dsh-util-values` at `0.2.2-rc.1`, plus `@deepseek-ai/cosmokit@1.9.0` and `@deepseek-ai/schemastery@3.19.0`; all are MIT. `@modelcontextprotocol/client` is locked to `2.4.0` by the Morde override (the DSH MCP package requests `2.2.0`). Other transitive package versions and integrity values remain in `package-lock.json` to avoid a second hand-maintained dependency list.

## Morde integration and evidence

Runtime builds the vendored TypeScript entry point with the already locked `esbuild` development dependency. `engine.mjs` loads upstream DSH compaction and token-meter plugins, mounts compaction with `auto: false`, and leaves the trigger and model dispatch to Morde's authorized route Worker and model gateway. Morde does not vendor a second agent loop or patch the upstream source.

Relevant verification:

- Source and release comparison: `sh scripts/verify-dsh-upstream.sh`
- Source checksums: `(cd vendor/deepseek-harness/packages/compaction/compaction-basic && shasum -a 256 -c source.sha256)`
- Runtime integration: `npm test`
- Morde persistence, authorization, and recovery: `go test ./internal/worker -run 'TestConversationRouteWorker|TestCloudDSHInitialPrompt'` and targeted PostgreSQL summary/Task recovery tests under `server/`
- Upstream behavior tests live at `project/deepseek-harness/packages/compaction/compaction-basic/tests/` and should be rerun from the DSH checkout when its dependencies are available.

## Upstream update procedure

1. Select and record an upstream commit and release tag. Compare the compaction package source, manifest, tests, runtime dependencies, and event/checkpoint format with this snapshot. Check whether any Morde patch was absorbed; currently there is no source patch.
2. Update the source commit, release commit, package version, checksum file, and verifier together. Rebuild the vendored entry point and pin all DSH runtime dependencies in `package.json` and `package-lock.json`; keep the MCP override only if the compatibility check still requires it.
3. Run the upstream compaction behavior tests, `npm test`, and Morde's session isolation, gateway accounting, checkpoint, cancellation, restart, and old-checkpoint recovery tests. Review lockfile integrity and license metadata.
4. Review the DSH event and checkpoint format before changing Morde's `ENGINE_VERSION`. DSH package version changes do not establish checkpoint compatibility. If an older checkpoint cannot be proven safe, stop the new runtime before rollout and use the prior runtime or the documented bounded-history recovery path; do not replay unknown model calls.

No dependency upgrade or source patch is inferred from the DSH checkout changing; source, lockfile, tests, and checkpoint compatibility are reviewed as one update.
