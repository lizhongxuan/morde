import Foundation

private let logger = AppLogger(category: "Backup")

/// Restores eligible fields from a recognized app backup's credential section
/// (docs/backup-restore-design.md §5.4). Provider API keys are deliberately
/// omitted by the Morde migration policy; OAuth and other supported fields keep
/// the existing merge behavior.
///
/// [T-ios-backup-credential-restore] Before this existed, `secrets.json` was
/// collected, base64'd, encrypted, decrypted on restore — and then **discarded**,
/// because nothing read it. The restore UI meanwhile said providers "may need
/// to be re-authenticated if their tokens have expired", when in truth they
/// always did. That is precisely the "provider list intact, every request 401"
/// half-restored state design §3.3 merged Providers+Credentials to prevent, and
/// it made device migration — the feature's primary purpose — silently fail.
///
/// Merge semantics match every other category (§8.2): a supported credential
/// already present locally is NOT overwritten. This importer is only called
/// for the app's `.minisbak` format; standalone provider exports use their own
/// explicit import flow and do not pass through this migration policy.
enum BackupSecretsImporter {

    struct Result {
        var providersRestored = 0
        var providersSkippedExisting = 0
        var providerAPIKeysSkippedByPolicy = 0
        var unknownFieldsIgnored = 0
        var credentialSectionUnreadable = false
        var envVarsRestored = 0
        var envVarsSkippedExisting = 0
        var mcpOAuthRestored = 0
        var mcpOAuthSkippedExisting = 0

        var total: Int { providersRestored + envVarsRestored + mcpOAuthRestored }
    }

    /// Read `secrets.json` from an unpacked package root and apply it.
    ///
    /// Returns nil when the package carries no credential section at all —
    /// which is the normal case for a "share copy" export (§3.3).
    @MainActor
    static func restore(from root: URL) -> Result? {
        let url = root.appendingPathComponent("secrets.json")
        guard FileManager.default.fileExists(atPath: url.path) else { return nil }
        let data: Data
        do {
            data = try Data(contentsOf: url)
        } catch {
            logger.error("[Restore] secrets.json could not be read — credentials NOT restored: \(error.localizedDescription)")
            return Result(credentialSectionUnreadable: true)
        }
        let secrets: BackupSecrets
        do {
            secrets = try JSONDecoder().decode(BackupSecrets.self, from: data)
        } catch {
            logger.error("[Restore] secrets.json could not be decoded — credentials NOT restored: \(error.localizedDescription)")
            return Result(credentialSectionUnreadable: true)
        }

        var result = Result()
        result.unknownFieldsIgnored = MordeBackupAPIKeyRestorePolicy.unknownFieldCount(in: data)
        if result.unknownFieldsIgnored > 0 {
            logger.warning("[Restore] secrets.json contained \(result.unknownFieldsIgnored) unrecognized field(s); this app version ignored them")
        }

        for p in secrets.providers {
            var wrote = false
            // Provider API keys from app backups must not resurrect credentials
            // removed during migration. Other supported fields restore independently.
            switch MordeBackupAPIKeyRestorePolicy.disposition(hasAPIKey: p.apiKey != nil) {
            case .absent:
                break
            case .removedByMigration:
                result.providerAPIKeysSkippedByPolicy += 1
            }
            if let b64 = p.manualOAuthToken, let value = decode(b64),
               ProviderKeychainHelper.loadOAuthString(
                   instanceId: p.instanceId, account: "manual-oauth-token") == nil {
                ProviderKeychainHelper.saveOAuthString(
                    value, instanceId: p.instanceId, account: "manual-oauth-token")
                wrote = true
            }
            if let b64 = p.oauthEmail, let value = decode(b64),
               ProviderKeychainHelper.loadOAuthString(
                   instanceId: p.instanceId, account: "oauth-email") == nil {
                ProviderKeychainHelper.saveOAuthString(
                    value, instanceId: p.instanceId, account: "oauth-email")
                wrote = true
            }
            if let b64 = p.oauthGcpProject, let value = decode(b64),
               ProviderKeychainHelper.loadOAuthString(
                   instanceId: p.instanceId, account: "oauth-gcp-project") == nil {
                ProviderKeychainHelper.saveOAuthString(
                    value, instanceId: p.instanceId, account: "oauth-gcp-project")
                wrote = true
            }
            // The structured OAuth blob is stored under a per-provider-type
            // Codable. It is written back as raw JSON under the same Keychain
            // account, so the importer doesn't have to switch on every provider
            // type (and stays correct when a new one is added).
            if let b64 = p.oauthToken, let raw = Data(base64Encoded: b64) {
                if ProviderKeychainHelper.loadRawOAuthToken(instanceId: p.instanceId) == nil {
                    ProviderKeychainHelper.saveRawOAuthToken(raw, instanceId: p.instanceId)
                    wrote = true
                }
            }
            if wrote { result.providersRestored += 1 }
        }

        for e in secrets.envVars {
            guard let value = decode(e.value) else { continue }
            if EnvVarStore.loadValueSync(forKey: e.name) == nil {
                EnvVarStore.saveValueSync(value, forKey: e.name)
                result.envVarsRestored += 1
            } else {
                result.envVarsSkippedExisting += 1
            }
        }

        for m in secrets.mcpOAuth {
            if MCPOAuthController.tokens(server: m.serverId) == nil,
               let raw = Data(base64Encoded: m.token),
               let tokens = try? JSONDecoder().decode(
                   MCPOAuthController.StoredTokens.self, from: raw) {
                MCPOAuthController.restoreTokens(tokens, server: m.serverId)
                result.mcpOAuthRestored += 1
            } else {
                result.mcpOAuthSkippedExisting += 1
            }
            if let b64 = m.clientSecret, let secret = decode(b64),
               MCPOAuthController.clientSecret(server: m.serverId) == nil {
                MCPOAuthController.setClientSecret(secret, server: m.serverId)
            }
        }

        logger.info("[Restore] credentials: providers=\(result.providersRestored) (kept \(result.providersSkippedExisting)) providerAPIKeysSkipped=\(result.providerAPIKeysSkippedByPolicy) unknownFieldsIgnored=\(result.unknownFieldsIgnored) envVars=\(result.envVarsRestored) mcpOAuth=\(result.mcpOAuthRestored)")
        return result
    }

    private static func decode(_ b64: String) -> String? {
        guard let data = Data(base64Encoded: b64) else { return nil }
        return String(data: data, encoding: .utf8)
    }
}
