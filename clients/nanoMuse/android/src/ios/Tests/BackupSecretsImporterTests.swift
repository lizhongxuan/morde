import Foundation

// Minimal Codable backup models keep this host-side harness independent of app frameworks.
struct BackupSecrets: Codable {
    var v: Int
    var providers: [ProviderSecret]
    var envVars: [EnvVarSecret]
    var mcpOAuth: [MCPOAuthSecret]

    struct ProviderSecret: Codable {
        let instanceId: String
        let label: String?
        let providerType: String?
        var apiKey: String?
        var manualOAuthToken: String?
        var oauthToken: String?
        var oauthEmail: String?
        var oauthGcpProject: String?
    }

    struct EnvVarSecret: Codable {
        let name: String
        let value: String
    }

    struct MCPOAuthSecret: Codable {
        let serverId: String
        let token: String
        var clientSecret: String?
    }
}

struct AppLogger {
    init(category: String) {}
    func info(_ message: String) {}
    func warning(_ message: String) {}
    func error(_ message: String) {}
}

enum ProviderKeychainHelper {
    struct OAuthAddress: Hashable {
        let instanceId: String
        let account: String
    }

    static var apiKeys: [String: String] = [:]
    static var apiKeyWrites = 0
    static var oauthStrings: [OAuthAddress: String] = [:]
    static var rawOAuthTokens: [String: Data] = [:]

    static func saveAPIKey(_ key: String, instanceId: String, caller: String = "test") {
        apiKeyWrites += 1
        apiKeys[instanceId] = key
    }

    static func loadOAuthString(instanceId: String, account: String) -> String? {
        oauthStrings[OAuthAddress(instanceId: instanceId, account: account)]
    }

    static func saveOAuthString(_ value: String, instanceId: String, account: String) {
        oauthStrings[OAuthAddress(instanceId: instanceId, account: account)] = value
    }

    static func loadRawOAuthToken(instanceId: String) -> Data? {
        rawOAuthTokens[instanceId]
    }

    static func saveRawOAuthToken(_ data: Data, instanceId: String) {
        rawOAuthTokens[instanceId] = data
    }
}

enum EnvVarStore {
    static var values: [String: String] = [:]

    static func loadValueSync(forKey key: String) -> String? {
        values[key]
    }

    static func saveValueSync(_ value: String, forKey key: String) {
        values[key] = value
    }
}

enum MCPOAuthController {
    struct StoredTokens: Codable, Equatable {
        let accessToken: String
        let refreshToken: String?
        let expiresAt: String?
    }

    static var storedTokens: [String: StoredTokens] = [:]
    static var clientSecrets: [String: String] = [:]

    static func tokens(server: String) -> StoredTokens? {
        storedTokens[server]
    }

    static func restoreTokens(_ tokens: StoredTokens, server: String) {
        storedTokens[server] = tokens
    }

    static func clientSecret(server: String) -> String? {
        clientSecrets[server]
    }

    static func setClientSecret(_ secret: String, server: String) {
        clientSecrets[server] = secret
    }
}

@main
enum BackupSecretsImporterTests {
    @MainActor
    static func main() {
        let newProviderID = "provider-new"
        let existingProviderID = "provider-existing"
        let existingManualOAuth = "keep-local-manual-oauth"
        let existingRawOAuth = Data(#"{"accessToken":"keep-local-oauth"}"#.utf8)
        let existingAPIKey = "keep-local-api-key"
        ProviderKeychainHelper.apiKeys[existingProviderID] = existingAPIKey
        ProviderKeychainHelper.oauthStrings[.init(
            instanceId: existingProviderID, account: "manual-oauth-token"
        )] = existingManualOAuth
        ProviderKeychainHelper.rawOAuthTokens[existingProviderID] = existingRawOAuth

        EnvVarStore.values["EXISTING_ENV"] = "keep-local-env"

        let existingTokens = MCPOAuthController.StoredTokens(
            accessToken: "keep-local-access", refreshToken: "keep-local-refresh", expiresAt: nil
        )
        MCPOAuthController.storedTokens["existing-server"] = existingTokens
        MCPOAuthController.clientSecrets["existing-server"] = "keep-local-client-secret"

        let restoredTokens = MCPOAuthController.StoredTokens(
            accessToken: "restored-access", refreshToken: "restored-refresh", expiresAt: "2030-01-01T00:00:00Z"
        )
        let restoredOAuthBlob = Data(#"{"accessToken":"restored-oauth"}"#.utf8)
        let payload: [String: Any] = [
            "v": 1,
            "providers": [
                [
                    "instanceId": newProviderID,
                    "label": "New provider",
                    "providerType": "anthropic",
                    "apiKey": b64("removed-api-key"),
                    "manualOAuthToken": b64("restored-manual-oauth"),
                    "oauthEmail": b64("restored@example.test"),
                    "oauthGcpProject": b64("restored-project"),
                    "oauthToken": restoredOAuthBlob.base64EncodedString(),
                    "futureCredentialField": "ignored"
                ],
                [
                    "instanceId": existingProviderID,
                    "label": "Existing provider",
                    "providerType": "anthropic",
                    "apiKey": b64("must-not-overwrite-api-key"),
                    "manualOAuthToken": b64("must-not-overwrite-manual-oauth"),
                    "oauthToken": Data(#"{"accessToken":"must-not-overwrite-oauth"}"#.utf8).base64EncodedString()
                ]
            ],
            "envVars": [
                ["name": "NEW_ENV", "value": b64("restored-env")],
                ["name": "EXISTING_ENV", "value": b64("must-not-overwrite-env"), "futureEnvField": true]
            ],
            "mcpOAuth": [
                [
                    "serverId": "new-server",
                    "token": try! JSONEncoder().encode(restoredTokens).base64EncodedString(),
                    "clientSecret": b64("restored-client-secret")
                ],
                [
                    "serverId": "existing-server",
                    "token": try! JSONEncoder().encode(restoredTokens).base64EncodedString(),
                    "clientSecret": b64("must-not-overwrite-client-secret"),
                    "futureMCPField": "ignored"
                ]
            ],
            "futureTopLevelField": 42
        ]
        let data = try! JSONSerialization.data(withJSONObject: payload, options: [.sortedKeys])
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("backup-secrets-importer-tests-\(UUID().uuidString)", isDirectory: true)
        try! FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        try! data.write(to: root.appendingPathComponent("secrets.json"))

        guard let result = BackupSecretsImporter.restore(from: root) else {
            fatalError("Importer did not return a result for secrets.json")
        }

        precondition(!result.credentialSectionUnreadable)
        precondition(result.providerAPIKeysSkippedByPolicy == 2)
        precondition(ProviderKeychainHelper.apiKeyWrites == 0)
        precondition(ProviderKeychainHelper.apiKeys[newProviderID] == nil)
        precondition(ProviderKeychainHelper.apiKeys[existingProviderID] == existingAPIKey)

        precondition(ProviderKeychainHelper.loadOAuthString(
            instanceId: newProviderID, account: "manual-oauth-token"
        ) == "restored-manual-oauth")
        precondition(ProviderKeychainHelper.loadOAuthString(
            instanceId: newProviderID, account: "oauth-email"
        ) == "restored@example.test")
        precondition(ProviderKeychainHelper.loadOAuthString(
            instanceId: newProviderID, account: "oauth-gcp-project"
        ) == "restored-project")
        precondition(ProviderKeychainHelper.rawOAuthTokens[newProviderID] == restoredOAuthBlob)
        precondition(ProviderKeychainHelper.loadOAuthString(
            instanceId: existingProviderID, account: "manual-oauth-token"
        ) == existingManualOAuth)
        precondition(ProviderKeychainHelper.rawOAuthTokens[existingProviderID] == existingRawOAuth)

        precondition(EnvVarStore.values["NEW_ENV"] == "restored-env")
        precondition(EnvVarStore.values["EXISTING_ENV"] == "keep-local-env")
        precondition(result.envVarsRestored == 1 && result.envVarsSkippedExisting == 1)

        precondition(MCPOAuthController.storedTokens["new-server"] == restoredTokens)
        precondition(MCPOAuthController.clientSecrets["new-server"] == "restored-client-secret")
        precondition(MCPOAuthController.storedTokens["existing-server"] == existingTokens)
        precondition(MCPOAuthController.clientSecrets["existing-server"] == "keep-local-client-secret")
        precondition(result.mcpOAuthRestored == 1 && result.mcpOAuthSkippedExisting == 1)

        precondition(result.unknownFieldsIgnored == 4)
        print("BackupSecretsImporter restore harness passed")
    }

    private static func b64(_ value: String) -> String {
        Data(value.utf8).base64EncodedString()
    }
}
