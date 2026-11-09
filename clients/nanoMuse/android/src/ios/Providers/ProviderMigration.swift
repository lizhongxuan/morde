import Foundation
import Security
import os.log

private let logger = AppLogger(category: "ProviderMigration")

/// Migrates existing provider configuration (AuthMode, API keys, OAuth state)
/// into the new ProviderInstance / ModelEntry / ModelGroup system.
@MainActor
enum ProviderMigration {

    private static let migrationKey = "io.github.nanomuse.app.provider-migration-v1-done"
    private static let oauthMigrationKey = "io.github.nanomuse.app.provider-migration-oauth-v2-done"

    /// Run migration if it hasn't been performed yet.
    static func migrateIfNeeded(store: ProviderConfigStore) {
        if !UserDefaults.standard.bool(forKey: migrationKey) {
            logger.info("Starting provider migration from legacy config")
            migrate(store: store)
            UserDefaults.standard.set(true, forKey: migrationKey)
            logger.info("Provider migration complete")
        }

        if !UserDefaults.standard.bool(forKey: oauthMigrationKey) {
            if migrateOAuthTokens(store: store) {
                UserDefaults.standard.set(true, forKey: oauthMigrationKey)
            } else {
                logger.error("OAuth token migration is incomplete; legacy credentials were retained for retry")
            }
        }

        // nanoMuse: Keep local provider API keys until the MI-05 deletion scope is approved.
    }

    // MARK: - V2: Migrate singleton OAuth tokens → per-instance storage

    private static func migrateOAuthTokens(store: ProviderConfigStore) -> Bool {
        logger.info("Starting OAuth token migration to per-instance storage")

        let oauthInstances = store.instances.filter { $0.credentialType == .oauth }
        var claudeToken = ClaudeOAuthManager.loadLegacyToken()
        var geminiToken = GeminiOAuthManager.loadLegacyToken()
        var codexToken = CodexOAuthManager.loadLegacyToken()
        var geminiEmail = UserDefaults.standard.string(forKey: GeminiOAuthManager.legacyEmailKey)
        var geminiProjectID = UserDefaults.standard.string(forKey: GeminiOAuthManager.legacyProjectIDKey)
        var migrationSucceeded = true

        // Keep legacy values and retry on a later launch if there is no destination
        // instance yet. The V1 config migration normally creates these instances first.
        if claudeToken != nil && !oauthInstances.contains(where: { $0.providerType == .anthropic }) {
            migrationSucceeded = false
        }
        if (geminiToken != nil || geminiEmail != nil || geminiProjectID != nil)
            && !oauthInstances.contains(where: { $0.providerType == .gemini }) {
            migrationSucceeded = false
        }
        if codexToken != nil && !oauthInstances.contains(where: { $0.providerType == .openAI }) {
            migrationSucceeded = false
        }

        for instance in oauthInstances {
            switch instance.providerType {
            case .anthropic:
                if ProviderKeychainHelper.loadOAuthToken(instanceId: instance.id, as: ClaudeTokenStorage.self) == nil,
                   let token = claudeToken {
                    if ProviderKeychainHelper.saveOAuthToken(token, instanceId: instance.id) {
                        ClaudeOAuthManager.deleteLegacyToken()
                        claudeToken = nil
                        logger.info("Migrated Claude OAuth token to instance \(instance.id)")
                    } else {
                        migrationSucceeded = false
                        logger.error("Could not save Claude OAuth token to instance \(instance.id); legacy token retained")
                    }
                }

            case .gemini:
                if ProviderKeychainHelper.loadOAuthToken(instanceId: instance.id, as: GeminiTokenStorage.self) == nil,
                   let token = geminiToken {
                    if ProviderKeychainHelper.saveOAuthToken(token, instanceId: instance.id) {
                        GeminiOAuthManager.deleteLegacyToken()
                        geminiToken = nil
                        logger.info("Migrated Gemini OAuth token to instance \(instance.id)")
                    } else {
                        migrationSucceeded = false
                        logger.error("Could not save Gemini OAuth token to instance \(instance.id); legacy token retained")
                    }
                }
                // Migrate email and project ID independently of whether the token
                // already exists, so a previous partial migration can be retried.
                if let email = geminiEmail,
                   ProviderKeychainHelper.loadOAuthString(instanceId: instance.id, account: "oauth-email") == nil {
                    if ProviderKeychainHelper.saveOAuthString(email, instanceId: instance.id, account: "oauth-email") {
                        UserDefaults.standard.removeObject(forKey: GeminiOAuthManager.legacyEmailKey)
                        geminiEmail = nil
                    } else {
                        migrationSucceeded = false
                        logger.error("Could not save Gemini OAuth email to instance \(instance.id); legacy value retained")
                    }
                }
                if let projectID = geminiProjectID,
                   ProviderKeychainHelper.loadOAuthString(instanceId: instance.id, account: "oauth-gcp-project") == nil {
                    if ProviderKeychainHelper.saveOAuthString(projectID, instanceId: instance.id, account: "oauth-gcp-project") {
                        UserDefaults.standard.removeObject(forKey: GeminiOAuthManager.legacyProjectIDKey)
                        geminiProjectID = nil
                    } else {
                        migrationSucceeded = false
                        logger.error("Could not save Gemini OAuth project ID to instance \(instance.id); legacy value retained")
                    }
                }

            case .openAI:
                if ProviderKeychainHelper.loadOAuthToken(instanceId: instance.id, as: CodexTokenStorage.self) == nil,
                   let token = codexToken {
                    if ProviderKeychainHelper.saveOAuthToken(token, instanceId: instance.id) {
                        CodexOAuthManager.deleteLegacyToken()
                        codexToken = nil
                        logger.info("Migrated Codex OAuth token to instance \(instance.id)")
                    } else {
                        migrationSucceeded = false
                        logger.error("Could not save Codex OAuth token to instance \(instance.id); legacy token retained")
                    }
                }

            case .antigravity:
                // No legacy tokens to migrate for Antigravity (new provider)
                break
            case .openRouter:
                // No legacy tokens to migrate for OpenRouter (new provider)
                break
            case .openAIResponses:
                break
            case .xAI:
                // xAI is a new provider; no legacy singleton tokens to migrate.
                break
            case .kimiCode:
                // Kimi is a new provider; no legacy singleton tokens to migrate.
                break
            case .unsupported:
                break
            }
        }

        if migrationSucceeded {
            logger.info("OAuth token migration complete")
        } else {
            logger.error("OAuth token migration remains incomplete; legacy values are retained")
        }
        return migrationSucceeded
    }

    // MARK: - V1: Legacy migration

    private static func migrate(store: ProviderConfigStore) {
        var config = ProviderConfig.empty
        var firstGroupEntryIds: [String] = []

        // MARK: - Anthropic

        // API Key
        if let key = readLegacyKeychain(service: "io.github.nanomuse.app.anthropic-api-key") {
            let instance = ProviderInstance(
                label: "Anthropic API Key",
                providerType: .anthropic,
                credentialType: .apiKey
            )
            config.instances.append(instance)
            let entries = LLMModel.allAnthropic.map {
                ModelEntry(providerInstanceId: instance.id, model: $0)
            }
            config.modelEntries.append(contentsOf: entries)
            // Save key to new keychain location
            ProviderKeychainHelper.saveAPIKey(key, instanceId: instance.id)

            if isLegacyActiveProvider("API Key") {
                firstGroupEntryIds = entries.map(\.id)
            }
        }

        // OAuth — check legacy singleton keychain directly
        if ClaudeOAuthManager.loadLegacyToken() != nil {
            let instance = ProviderInstance(
                label: "Claude OAuth",
                providerType: .anthropic,
                credentialType: .oauth
            )
            config.instances.append(instance)
            let entries = LLMModel.allAnthropic.map {
                ModelEntry(providerInstanceId: instance.id, model: $0)
            }
            config.modelEntries.append(contentsOf: entries)

            if isLegacyActiveProvider("OAuth") {
                firstGroupEntryIds = entries.map(\.id)
            }
        }

        // MARK: - Gemini

        // API Key
        if let key = readLegacyKeychain(service: "io.github.nanomuse.app.gemini-api-key") {
            let instance = ProviderInstance(
                label: "Gemini API Key",
                providerType: .gemini,
                credentialType: .apiKey
            )
            config.instances.append(instance)
            let entries = LLMModel.allGemini.map {
                ModelEntry(providerInstanceId: instance.id, model: $0)
            }
            config.modelEntries.append(contentsOf: entries)
            ProviderKeychainHelper.saveAPIKey(key, instanceId: instance.id)

            if isLegacyActiveProvider("Gemini API Key") {
                firstGroupEntryIds = entries.map(\.id)
            }
        }

        // OAuth
        if GeminiOAuthManager.loadLegacyToken() != nil {
            let instance = ProviderInstance(
                label: "Gemini OAuth",
                providerType: .gemini,
                credentialType: .oauth
            )
            config.instances.append(instance)
            let entries = LLMModel.allGemini.map {
                ModelEntry(providerInstanceId: instance.id, model: $0)
            }
            config.modelEntries.append(contentsOf: entries)

            if isLegacyActiveProvider("Gemini OAuth") {
                firstGroupEntryIds = entries.map(\.id)
            }
        }

        // MARK: - OpenAI

        // API Key
        if let key = readLegacyKeychain(service: "io.github.nanomuse.app.openai-api-key") {
            let instance = ProviderInstance(
                label: "OpenAI API Key",
                providerType: .openAI,
                credentialType: .apiKey
            )
            config.instances.append(instance)
            let entries = LLMModel.allOpenAI.map {
                ModelEntry(providerInstanceId: instance.id, model: $0)
            }
            config.modelEntries.append(contentsOf: entries)
            ProviderKeychainHelper.saveAPIKey(key, instanceId: instance.id)

            if isLegacyActiveProvider("OpenAI API Key") {
                firstGroupEntryIds = entries.map(\.id)
            }
        }

        // Codex OAuth
        if CodexOAuthManager.loadLegacyToken() != nil {
            let instance = ProviderInstance(
                label: "Codex OAuth",
                providerType: .openAI,
                credentialType: .oauth
            )
            config.instances.append(instance)
            let entries = LLMModel.allOpenAI.map {
                ModelEntry(providerInstanceId: instance.id, model: $0)
            }
            config.modelEntries.append(contentsOf: entries)

            if isLegacyActiveProvider("Codex OAuth") {
                firstGroupEntryIds = entries.map(\.id)
            }
        }

        // MARK: - Default Group

        // If we found an active provider, narrow the group to the last selected model if possible
        if !firstGroupEntryIds.isEmpty {
            // Try to match legacy AgentModelSettings primary model IDs
            let legacySettings = Self.loadLegacyAgentModelSettings()
            let primaryIds = legacySettings.primaryModelIds

            // Filter to just entries matching the legacy primary model IDs
            let matchedEntries = firstGroupEntryIds.filter { entryId in
                primaryIds.contains(where: { entryId.hasSuffix(":\($0)") })
            }

            let groupMembers = matchedEntries.isEmpty ? firstGroupEntryIds : matchedEntries

            let defaultGroup = ModelGroup(
                name: "Default",
                memberEntryIds: groupMembers,
                strategy: groupMembers.count > 1 ? .fallback : .fallback
            )
            config.modelGroups.append(defaultGroup)
            config.defaultPrimaryGroupId = defaultGroup.id

            // Sub-model group from legacy settings
            let subIds = legacySettings.subModelIds.isEmpty ? legacySettings.primaryModelIds : legacySettings.subModelIds
            if subIds != primaryIds {
                let subEntries = firstGroupEntryIds.filter { entryId in
                    subIds.contains(where: { entryId.hasSuffix(":\($0)") })
                }
                if !subEntries.isEmpty {
                    let subGroup = ModelGroup(
                        name: "Sub Tasks",
                        memberEntryIds: subEntries,
                        strategy: .fallback
                    )
                    config.modelGroups.append(subGroup)
                    config.defaultSubGroupId = subGroup.id
                }
            }
        }

        store.applyConfig(config)

        logger.info("Migration created \(config.instances.count) instances, \(config.modelEntries.count) entries, \(config.modelGroups.count) groups")
    }

    // MARK: - Legacy Helpers

    private static func readLegacyKeychain(service: String) -> String? {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: "api-key",
            kSecReturnData as String: true,
            kSecMatchLimit as String: kSecMatchLimitOne,
        ]
        var result: AnyObject?
        guard SecItemCopyMatching(query as CFDictionary, &result) == errSecSuccess,
              let data = result as? Data else { return nil }
        return String(data: data, encoding: .utf8)
    }

    /// Legacy AgentModelSettings shape (for migration only).
    private struct LegacyAgentModelSettings: Codable {
        var primaryModelIds: [String]
        var subModelIds: [String]
    }

    private static func loadLegacyAgentModelSettings() -> LegacyAgentModelSettings {
        let key = "io.github.nanomuse.app.agent-model-settings"
        guard let data = UserDefaults.standard.data(forKey: key),
              let settings = try? JSONDecoder().decode(LegacyAgentModelSettings.self, from: data)
        else {
            return LegacyAgentModelSettings(primaryModelIds: [LLMModel.claudeSonnet46.id], subModelIds: [])
        }
        return settings
    }

    /// Check if a given raw auth mode string matches the legacy active provider keychain entry.
    private static func isLegacyActiveProvider(_ rawValue: String) -> Bool {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: "io.github.nanomuse.app.active-provider",
            kSecAttrAccount as String: "auth-mode",
            kSecReturnData as String: true,
            kSecMatchLimit as String: kSecMatchLimitOne,
        ]
        var result: AnyObject?
        guard SecItemCopyMatching(query as CFDictionary, &result) == errSecSuccess,
              let data = result as? Data,
              let raw = String(data: data, encoding: .utf8) else { return false }
        return raw == rawValue
    }
}
