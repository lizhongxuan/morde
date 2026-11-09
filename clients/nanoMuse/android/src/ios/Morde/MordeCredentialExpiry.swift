import Foundation
import CryptoKit

enum MordePersonalJSONValue: Codable, Equatable, Sendable {
    case object([String: MordePersonalJSONValue])
    case array([MordePersonalJSONValue])
    case string(String)
    case integer(Int64)
    case number(Double)
    case bool(Bool)
    case null

    init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        if container.decodeNil() { self = .null; return }
        if let value = try? container.decode([String: MordePersonalJSONValue].self) { self = .object(value); return }
        if let value = try? container.decode([MordePersonalJSONValue].self) { self = .array(value); return }
        if let value = try? container.decode(String.self) { self = .string(value); return }
        if let value = try? container.decode(Bool.self) { self = .bool(value); return }
        if let value = try? container.decode(Int64.self) { self = .integer(value); return }
        if let value = try? container.decode(Double.self), value.isFinite { self = .number(value); return }
        throw DecodingError.dataCorruptedError(in: container, debugDescription: "Unsupported personal-data JSON value")
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.singleValueContainer()
        switch self {
        case .object(let value): try container.encode(value)
        case .array(let value): try container.encode(value)
        case .string(let value): try container.encode(value)
        case .integer(let value): try container.encode(value)
        case .number(let value): try container.encode(value)
        case .bool(let value): try container.encode(value)
        case .null: try container.encodeNil()
        }
    }

    func decode<T: Decodable>(_ type: T.Type) throws -> T {
        try JSONDecoder().decode(type, from: JSONEncoder().encode(self))
    }
}

struct MordePersonalRevisionConflict: LocalizedError, Equatable, Sendable {
    let baseRevision: Int64
    let currentRevision: Int64
    let deleted: Bool
    let message: String

    var errorDescription: String? { message }

    static func decode(_ data: Data) -> MordePersonalRevisionConflict? {
        guard let envelope = try? JSONDecoder().decode(MordePersonalErrorEnvelope.self, from: data),
              envelope.error.code == "conflict",
              let details = envelope.error.details else { return nil }
        let recognizedReason = details["reason"] == .string("personal_object_revision_changed") ||
            (details["reason"] == nil && details.count == 3)
        guard recognizedReason,
              case .integer(let baseRevision)? = details["base_revision"],
              case .integer(let currentRevision)? = details["current_revision"],
              case .bool(let deleted)? = details["deleted"],
              baseRevision >= 0, currentRevision >= 0 else { return nil }
        // Older local API builds omit `reason` but retain this exact revision-conflict shape.
        return MordePersonalRevisionConflict(baseRevision: baseRevision, currentRevision: currentRevision,
                                             deleted: deleted, message: envelope.error.message)
    }
}

private struct MordePersonalErrorEnvelope: Decodable {
    struct ErrorBody: Decodable {
        let code: String
        let message: String
        let details: [String: MordePersonalJSONValue]?
    }

    let error: ErrorBody
}

enum MordeFirstRunPolicy {
    static func isNeeded(
        signedIn: Bool,
        hasProviders: Bool,
        hasSessions: Bool,
        done: Bool,
        mordeSignedIn: Bool
    ) -> Bool {
        !mordeSignedIn && ((!signedIn && !hasProviders) || (!hasSessions && !done))
    }
}

enum MordeLocalExecutionPolicy {
    static let isEnabled = false
    static var disabledMessage: String {
        String(localized: "Local Agent execution and direct model calls are disabled. Open Morde cloud tasks to continue.")
    }

    static func requireEnabled() throws {
        guard isEnabled else { throw MordeLocalExecutionDisabledError() }
    }
}

/// CloudKit sync is an independent product path from local Agent execution.
/// Keep it fail-closed until the old iCloud data flow is explicitly reviewed.
enum MordeCloudKitSyncPolicy {
    static let isEnabled = false
    static var disabledMessage: String {
        String(localized: "Legacy iCloud business sync is paused.")
    }

    static func shouldRemoveLegacyCache(userSyncEnabled: Bool) -> Bool {
        isEnabled && userSyncEnabled
    }
}

struct MordeCloudKitSyncDisabledError: LocalizedError {
    var errorDescription: String? { MordeCloudKitSyncPolicy.disabledMessage }
}

/// Keep preserved nanoMuse Cloud and Relay code from making requests in the Morde app.
enum MordeLegacyServicePolicy {
    static let isEnabled = false
    static var disabledMessage: String {
        String(localized: "Legacy morde services are disabled in morde.")
    }
}

struct MordeLegacyServiceDisabledError: LocalizedError {
    var errorDescription: String? { MordeLegacyServicePolicy.disabledMessage }
}

struct MordeLocalExecutionDisabledError: LocalizedError {
    var errorDescription: String? { MordeLocalExecutionPolicy.disabledMessage }
}

struct MordeAPIKeyDeletionOutcome: Equatable {
    static let successStatus: Int32 = 0
    static let itemNotFoundStatus: Int32 = -25_300

    let legacyStatus: Int32
    let synchronizableStatus: Int32

    var isComplete: Bool {
        Self.isSuccessful(legacyStatus) && Self.isSuccessful(synchronizableStatus)
    }

    var failedStatuses: [Int32] {
        [legacyStatus, synchronizableStatus].filter { !Self.isSuccessful($0) }
    }

    private static func isSuccessful(_ status: Int32) -> Bool {
        status == successStatus || status == itemNotFoundStatus
    }
}

struct MordeProviderKeyCleanupDescriptor: Equatable {
    let instanceID: String
    let providerType: String
    let credentialType: String
}

struct MordeProviderKeyCleanupLedger: Codable, Equatable {
    enum Status: String, Codable {
        case pending
        case complete
    }

    let version: Int
    var status: Status
    var pendingInstanceIDs: [String]
    var pendingLegacyServices: [String]
    var reviewRequiredInstanceIDs: [String]
}

enum MordeProviderKeyCleanupPlan {
    static let version = 1
    static let defaultsKey = "morde.provider-own-api-key-cleanup.v1"
    static let noticePendingDefaultsKey = "morde.provider-own-api-key-cleanup-notice-pending.v1"
    static let legacyKeychainServices = [
        "io.github.nanomuse.app.anthropic-api-key",
        "io.github.nanomuse.app.gemini-api-key",
        "io.github.nanomuse.app.openai-api-key",
    ]

    static func shouldShowNotice(storedValue: Bool?, cleanupComplete: Bool) -> Bool {
        storedValue ?? cleanupComplete
    }

    /// Delete both Keychain variants with an explicit synchronization scope. The injected
    /// closure lets the host-side test use synthetic entries instead of the real Keychain.
    static func deleteVariants(
        baseQuery: [String: Any],
        synchronizableAttribute: String,
        deleteItem: ([String: Any]) -> Int32
    ) -> MordeAPIKeyDeletionOutcome {
        var legacyQuery = baseQuery
        legacyQuery[synchronizableAttribute] = false
        var synchronizableQuery = baseQuery
        synchronizableQuery[synchronizableAttribute] = true
        return MordeAPIKeyDeletionOutcome(
            legacyStatus: deleteItem(legacyQuery),
            synchronizableStatus: deleteItem(synchronizableQuery)
        )
    }

    private static let knownKeyProviderTypes: Set<String> = [
        "openAI", "anthropic", "gemini", "antigravity", "openRouter",
        "openAIResponses", "xAI", "kimiCode",
    ]

    static func candidateInstanceIDs(from descriptors: [MordeProviderKeyCleanupDescriptor]) -> [String] {
        Array(Set(descriptors.compactMap { descriptor in
            guard descriptor.credentialType == "apiKey",
                  knownKeyProviderTypes.contains(descriptor.providerType),
                  !descriptor.instanceID.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
                return nil
            }
            return descriptor.instanceID
        })).sorted()
    }

    static func reviewRequiredInstanceIDs(from descriptors: [MordeProviderKeyCleanupDescriptor]) -> [String] {
        Array(Set(descriptors.compactMap { descriptor in
            guard descriptor.credentialType == "apiKey",
                  !knownKeyProviderTypes.contains(descriptor.providerType),
                  !descriptor.instanceID.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
                return nil
            }
            return descriptor.instanceID
        })).sorted()
    }

    static func begin(
        previous: MordeProviderKeyCleanupLedger?,
        candidateInstanceIDs: [String],
        reviewRequiredInstanceIDs: [String],
        preservedInstanceIDs: Set<String> = []
    ) -> MordeProviderKeyCleanupLedger {
        if let previous, previous.version == version, previous.status == .complete {
            var completed = previous
            completed.pendingInstanceIDs.removeAll { preservedInstanceIDs.contains($0) }
            completed.reviewRequiredInstanceIDs.removeAll { preservedInstanceIDs.contains($0) }
            return completed
        }
        return MordeProviderKeyCleanupLedger(
            version: version,
            status: .pending,
            // nanoMuse: A pending cleanup retry must not delete the relay key after its provider row was reclassified.
            pendingInstanceIDs: Array(Set((previous?.pendingInstanceIDs ?? []) + candidateInstanceIDs)
                .filter { !preservedInstanceIDs.contains($0) }).sorted(),
            pendingLegacyServices: Array(Set((previous?.pendingLegacyServices ?? []) + legacyKeychainServices)).sorted(),
            reviewRequiredInstanceIDs: Array(Set((previous?.reviewRequiredInstanceIDs ?? []) + reviewRequiredInstanceIDs)
                .filter { !preservedInstanceIDs.contains($0) }).sorted()
        )
    }

    static func finish(
        _ ledger: MordeProviderKeyCleanupLedger,
        failedInstanceIDs: [String],
        failedLegacyServices: [String]
    ) -> MordeProviderKeyCleanupLedger {
        let failedInstances = Array(Set(failedInstanceIDs)).sorted()
        let failedServices = Array(Set(failedLegacyServices)).sorted()
        return MordeProviderKeyCleanupLedger(
            version: version,
            status: failedInstances.isEmpty && failedServices.isEmpty ? .complete : .pending,
            pendingInstanceIDs: failedInstances,
            pendingLegacyServices: failedServices,
            reviewRequiredInstanceIDs: ledger.reviewRequiredInstanceIDs
        )
    }
}

enum MordeBackupAPIKeyRestorePolicy {
    enum Disposition: Equatable {
        case absent
        case removedByMigration
    }

    static func disposition(hasAPIKey: Bool) -> Disposition {
        hasAPIKey ? .removedByMigration : .absent
    }

    static func unknownFieldCount(in data: Data) -> Int {
        guard let root = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else { return 0 }
        var count = unknownKeys(in: root, allowed: ["v", "providers", "envVars", "mcpOAuth"])
        for provider in root["providers"] as? [[String: Any]] ?? [] {
            count += unknownKeys(in: provider, allowed: [
                "instanceId", "label", "providerType", "apiKey", "manualOAuthToken",
                "oauthToken", "oauthEmail", "oauthGcpProject",
            ])
        }
        for envVar in root["envVars"] as? [[String: Any]] ?? [] {
            count += unknownKeys(in: envVar, allowed: ["name", "value"])
        }
        for server in root["mcpOAuth"] as? [[String: Any]] ?? [] {
            count += unknownKeys(in: server, allowed: ["serverId", "token", "clientSecret"])
        }
        return count
    }

    private static func unknownKeys(in object: [String: Any], allowed: Set<String>) -> Int {
        object.keys.reduce(into: 0) { count, key in
            if !allowed.contains(key) { count += 1 }
        }
    }
}

struct MordeLoginIntent: Equatable {
    let id: UUID
    let origin: String
    let accountName: String
    fileprivate let scopeKey: String
}

enum MordeLoginContract {
    private static let requiredFeatures: Set<String> = [
        "password_login", "client_installations", "ordered_client_login"
    ]

    static func supportsOrderedClientLogin(_ features: [String]) -> Bool {
        requiredFeatures.isSubset(of: Set(features))
    }

    static func mayRenewInstallation(installationOrigin: String?, installationServiceInstanceID: String?,
                                     selectedServer: String, selectedServiceInstanceID: String,
                                     legacyServer: String?, installationAccount: String,
                                     accountName: String) -> Bool {
        guard installationAccount == accountName,
              let selectedOrigin = MordeLoginStateStore.serviceOrigin(selectedServer),
              let storedOrigin = MordeLoginStateStore.serviceOrigin(installationOrigin ?? legacyServer ?? "") else {
            return false
        }
        guard storedOrigin == selectedOrigin else { return false }
        guard let installationServiceInstanceID else { return true }
        guard let storedInstanceID = UUID(uuidString: installationServiceInstanceID),
              let selectedInstanceID = UUID(uuidString: selectedServiceInstanceID) else { return false }
        return storedInstanceID == selectedInstanceID
    }
}

enum MordeAccountLoginFailure: Equatable {
    case pending, rejected, disabled

    static func fromAPIErrorCode(_ code: String?) -> Self? {
        switch code {
        case "account_pending": .pending
        case "account_rejected": .rejected
        case "account_disabled": .disabled
        default: nil
        }
    }
}

// nanoMuse: Registration is a pending username/password request, never a login credential.
struct MordeRegistrationPayload: Equatable {
    let name: String
    let password: String

    static func make(name: String, password: String) -> Self? {
        let name = name.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !name.isEmpty, !password.isEmpty else { return nil }
        return Self(name: name, password: password)
    }

    var jsonObject: [String: String] { ["name": name, "password": password] }
}

enum MordeTaskSpeechInputPolicy {
    static func onDeviceRequirement(forced: Bool, supported: Bool) -> Bool? {
        guard !forced || supported else { return nil }
        return forced || supported
    }
}

enum MordeCapabilitiesFailure: Equatable {
    case network
    case http(statusCode: Int)
    case invalidResponse
    case incompatible
    case minimumVersion

    static func classify(domain: String, code: Int) -> MordeCapabilitiesFailure {
        if domain == NSURLErrorDomain { return .network }
        if domain == "Morde", (400..<600).contains(code) { return .http(statusCode: code) }
        return .invalidResponse
    }

    func message(underlying: String? = nil) -> String {
        let summary: String
        switch self {
        case .network:
            summary = "无法连接 Morde 服务，未发送账号信息。"
        case .http(let statusCode) where statusCode == 401 || statusCode == 403:
            summary = "无权读取 Morde 服务能力（HTTP \(statusCode)），未发送账号信息。"
        case .http(statusCode: 404):
            summary = "Morde 服务未提供能力接口（HTTP 404），未发送账号信息。"
        case .http(let statusCode):
            summary = "Morde 服务返回 HTTP \(statusCode)，无法读取服务能力，未发送账号信息。"
        case .invalidResponse:
            summary = "Morde 服务能力响应无效，未发送账号信息。"
        case .incompatible:
            summary = "Morde 服务版本不兼容或未开放有序客户端登录，未发送账号信息。"
        case .minimumVersion:
            summary = "当前 Morde 客户端版本过低，请更新后再登录，未发送账号信息。"
        }
        guard let underlying, !underlying.isEmpty else { return summary }
        return "\(summary)\n\(underlying)"
    }
}

enum MordeTaskCapability {
    static func cloudWorkerIsAvailable(_ features: [String]) -> Bool {
        features.contains("cloud_tasks")
    }

    // nanoMuse: Treat legacy cloud_tasks as Task API support during capability migration.
    static func taskAPIIsAvailable(_ features: [String]) -> Bool {
        features.contains("task_api") || cloudWorkerIsAvailable(features)
    }

    static func mediaGenerationIsAvailable(_ features: [String]) -> Bool {
        features.contains("task_media_generation")
    }
}

enum MordeWorkspaceSyncCapability {
    static func isAvailable(_ features: [String]?) -> Bool {
        features?.contains("workspace_sync") == true
    }
}

enum MordeConversationSelectionPolicy {
    static func shouldFollowMain(currentlySelected: String?, wasFollowingMain: Bool) -> Bool {
        wasFollowingMain || currentlySelected == nil
    }
}

enum MordeScopedRequestPolicy {
    static func isCurrent(scope: String?, generation: UUID, activeScope: String?, activeGeneration: UUID) -> Bool {
        guard let scope else { return false }
        return scope == activeScope && generation == activeGeneration
    }
}

enum MordePersonalSnapshotCursorPolicy {
    static func repeats(_ cursor: String?, seen: inout Set<String>) -> Bool {
        guard let cursor, !cursor.isEmpty else { return false }
        return !seen.insert(cursor).inserted
    }
}

enum MordeHostControlPolicy {
    static func canTakeOver(state: String, actorID: String?, leaseActive: Bool?, transitioning: Bool) -> Bool {
        state == "ai" && actorID == "" && leaseActive == false && !transitioning
    }

    static func canRelease(state: String, actorID: String?, currentUserID: String?,
                           leaseActive: Bool?, transitioning: Bool) -> Bool {
        state == "user" && !currentUserID.isNilOrEmpty && !transitioning &&
            ((actorID == currentUserID && leaseActive == true) || (actorID.isNilOrEmpty && leaseActive == false))
    }

    static func canRenew(state: String, actorID: String?, currentUserID: String?,
                         leaseActive: Bool?, transitioning: Bool) -> Bool {
        canRelease(state: state, actorID: actorID, currentUserID: currentUserID,
                   leaseActive: leaseActive, transitioning: transitioning)
    }
}

enum MordeProjectPolicy {
    static func hasValidID(_ id: String) -> Bool {
        id.range(of: #"^j[a-z0-9]{7}$"#, options: .regularExpression) != nil
    }

    static func hasValidInvitationID(_ id: String) -> Bool {
        id.range(of: #"^r[a-z0-9]{7}$"#, options: .regularExpression) != nil
    }

    static func hasValidConversationID(_ id: String) -> Bool {
        id.range(of: #"^c[a-z0-9]{7}$"#, options: .regularExpression) != nil
    }

    static func hasValidUserID(_ id: String) -> Bool {
        id.range(of: #"^u[a-z0-9]{7}$"#, options: .regularExpression) != nil
    }

    static func hasValidTaskID(_ id: String) -> Bool {
        id.range(of: #"^q[a-z0-9]{7}$"#, options: .regularExpression) != nil
    }

    static func restorableTaskID(ownerID: String, currentUserID: String?,
                                 messages: [(actorID: String, taskID: String?)]) -> String? {
        guard isOwner(ownerID: ownerID, currentUserID: currentUserID),
              let userID = currentUserID else { return nil }
        return messages.reversed().compactMap { message in
            message.actorID == userID ? message.taskID : nil
        }.first(where: hasValidTaskID)
    }

    static func isOwner(ownerID: String, currentUserID: String?) -> Bool {
        hasValidUserID(ownerID) && ownerID == currentUserID
    }

    static func canAcceptInvitation(state: String, inviteeID: String, currentUserID: String?) -> Bool {
        state == "pending" && hasValidUserID(inviteeID) && inviteeID == currentUserID
    }

    static func canTransferOwnership(ownerID: String, currentUserID: String?, targetUserID: String,
                                     targetRole: String) -> Bool {
        isOwner(ownerID: ownerID, currentUserID: currentUserID) && hasValidUserID(targetUserID) &&
            targetUserID != currentUserID && ["member", "viewer"].contains(targetRole)
    }

    static func canContinuePendingMutation(_ pendingFingerprint: String?, requestedFingerprint: String) -> Bool {
        pendingFingerprint == nil || pendingFingerprint == requestedFingerprint
    }

    static func canManageWorkItems(role: String?) -> Bool {
        role == "owner" || role == "member"
    }

    static func canLinkTask(role: String?, state: String, taskID: String, taskIDs: [String]) -> Bool {
        canManageWorkItems(role: role) && state != "delivered" && taskIDs.count < 100 &&
            hasValidTaskID(taskID) && !taskIDs.contains(taskID)
    }

    static func canRecordDelivery(role: String?, state: String, taskID: String, taskIDs: [String],
                                  workspaceID: String, revision: Int64) -> Bool {
        canManageWorkItems(role: role) && state == "open" && taskIDs.contains(taskID) &&
            hasValidTaskID(taskID) && hasValidResourceID(kind: "workspace", id: workspaceID) && revision > 0
    }

    static func canSetBudget(role: String?, currency: String, limitMicros: Int64) -> Bool {
        role == "owner" && currency.range(of: "^[A-Z]{3}$", options: .regularExpression) != nil &&
            limitMicros > 0 && limitMicros < Int64.max
    }

    static func hasValidResourceID(kind: String, id: String) -> Bool {
        switch kind {
        case "host": return id.range(of: #"^h[a-z0-9]{7}$"#, options: .regularExpression) != nil
        case "workspace": return id.range(of: #"^w[a-z0-9]{7}$"#, options: .regularExpression) != nil
        default: return false
        }
    }
}

enum MordeProjectExecutionCapability {
    static func isAvailable(_ features: [String]?) -> Bool {
        features?.contains("project_budget_execution") == true
    }

    static func canContinueTask(status: String, waitingReason: String? = nil) -> Bool {
        ["queued", "running", "waiting_user"].contains(status) &&
            !(status == "waiting_user" && waitingReason == "project_recovery_unavailable")
    }
}

private extension Optional where Wrapped == String {
    var isNilOrEmpty: Bool { self?.isEmpty != false }
}

enum MordeNotificationPresentationPolicy {
    static func summary(_ summary: String?, redacted: Bool?) -> String {
        guard redacted == false, let summary else { return "不可查看" }
        return summary
    }

    static func canOpenTask(kind: String, redacted: Bool?, actionable: Bool?) -> Bool {
        redacted == false && (kind != "waiting_user" || actionable == true)
    }

    static func canAppendPage(currentIDs: [String], pageIDs: [String]) -> Bool {
        let ids = currentIDs + pageIDs
        return Set(ids).count == ids.count
    }
}

enum MordeTaskResponsePolicy {
    static func hasValidTaskID(_ id: String) -> Bool {
        id.range(of: #"^q[a-z0-9]{7}$"#, options: .regularExpression) != nil
    }

    static func hasValidRequestID(_ id: String) -> Bool {
        id.range(of: #"^r[a-z0-9]{7}$"#, options: .regularExpression) != nil
    }

    static func hasValidOperationID(_ id: String) -> Bool {
        id.range(of: #"^o[a-z0-9]{7}$"#, options: .regularExpression) != nil
    }

    static func hasValidAnswer(_ answer: String) -> Bool {
        !answer.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && answer.utf8.count <= 16384 && !answer.contains("\0")
    }

    static func actionPath(taskID: String) -> String? {
        hasValidTaskID(taskID) ? "/api/tasks/\(taskID)/actions" : nil
    }

    static func approvalPath(taskID: String, approvalID: String) -> String? {
        guard hasValidTaskID(taskID), hasValidRequestID(approvalID) else { return nil }
        return "/api/tasks/\(taskID)/approvals/\(approvalID)/responses"
    }

    static func mediaPreviewPath(taskID: String, operationID: String) -> String? {
        guard hasValidTaskID(taskID), hasValidOperationID(operationID) else { return nil }
        return "/api/tasks/\(taskID)/media/\(operationID)/preview"
    }

    static func inputPath(taskID: String) -> String? {
        hasValidTaskID(taskID) ? "/api/tasks/\(taskID)/inputs" : nil
    }
}

struct MordeMainConversation: Decodable, Equatable {
    let status: String
    let conversation_id: String
    let version: Int64

    var isValid: Bool {
        ["ready", "selection_required", "uninitialized"].contains(status) &&
            (status == "ready"
                ? (conversation_id.range(of: "^c[a-z0-9]{7}$", options: .regularExpression) != nil && version > 0)
                : (conversation_id.isEmpty && version == 0))
    }
}

enum MordeMessageDeliveryPresentation {
    static func label(_ status: String?) -> String {
        switch status {
        case "pending": return "已受理，等待进入 Agent"
        case "applied": return "已进入 Agent；任务仍可能运行"
        case "unknown": return "投递状态未知；请刷新确认，不要重复提交"
        default: return "服务端未提供投递状态"
        }
    }
}

enum MordeClientInstallationPolicy {
    static func isValidID(_ id: String) -> Bool {
        id.range(of: "^i[a-z0-9]{7}$", options: .regularExpression) != nil
    }

    static func canRevoke(_ targetID: String, currentInstallationID: String?) -> Bool {
        guard isValidID(targetID), let currentInstallationID, isValidID(currentInstallationID) else {
            return false
        }
        return targetID != currentInstallationID
    }

    static func revokePath(for targetID: String, currentInstallationID: String?) -> String? {
        guard canRevoke(targetID, currentInstallationID: currentInstallationID) else { return nil }
        return "/api/clients/\(targetID)"
    }
}

enum MordeResource: String, CaseIterable, Hashable {
    case hosts
    case sharedHosts
    case workspaces
    case models
    case conversations
    case tasks
    case notifications
    case messages
    case taskDetail

    var label: String {
        switch self {
        case .hosts: return "电脑列表"
        case .sharedHosts: return "共享电脑列表"
        case .workspaces: return "Workspace 列表"
        case .models: return "模型列表"
        case .conversations: return "对话列表"
        case .tasks: return "Task 列表"
        case .notifications: return "通知列表"
        case .messages: return "消息"
        case .taskDetail: return "Task 状态"
        }
    }
}

enum MordeResourceLoadState: Equatable {
    case loading
    case loaded
    case failed(String)

    var isLoaded: Bool {
        if case .loaded = self { return true }
        return false
    }

    var isFailed: Bool {
        if case .failed = self { return true }
        return false
    }

    // nanoMuse: A failed model lookup is unknown, not an active loading gate; queued chat must resume after it settles.
    var isResolved: Bool { isLoaded || isFailed }

    var failureMessage: String? {
        if case .failed(let message) = self { return message }
        return nil
    }
}

enum MordeResourcePresentation {
    static func emptyListMessage(resource: MordeResource, state: MordeResourceLoadState,
                                 itemCount: Int, emptyMessage: String) -> String? {
        guard itemCount == 0 else { return nil }
        switch state {
        case .loading:
            return "\(resource.label)正在读取。"
        case .loaded:
            return emptyMessage
        case .failed(let message):
            let detail = message.isEmpty ? "暂时不可用" : message
            return "\(resource.label)未成功读取：\(detail)"
        }
    }
}

enum MordeTaskModelSelection {
    static func canSubmit(modelID: String, resourceState: MordeResourceLoadState,
                          allowedModelIDs: Set<String>) -> Bool {
        resourceState.isLoaded && !modelID.isEmpty && allowedModelIDs.contains(modelID)
    }
}

enum MordeConversationTaskState {
    static func belongs(taskConversationID: String?, toSelectedConversationID: String?) -> Bool {
        guard let taskConversationID, let toSelectedConversationID else { return false }
        return taskConversationID == toSelectedConversationID
    }
}

struct MordeClientDataScope: Codable, Equatable, Hashable {
    let serviceOrigin: String
    let serviceInstanceID: String
    let accountID: String
    let installationID: String

    init?(serviceOrigin: String, serviceInstanceID: String, accountID: String, installationID: String) {
        guard let origin = MordeLoginStateStore.serviceOrigin(serviceOrigin),
              let instanceID = UUID(uuidString: serviceInstanceID),
              !accountID.isEmpty, !installationID.isEmpty else { return nil }
        self.serviceOrigin = origin
        self.serviceInstanceID = instanceID.uuidString.lowercased()
        self.accountID = accountID
        self.installationID = installationID
    }

    var storageKey: String { Self.digest([serviceOrigin, serviceInstanceID, accountID, installationID]) }

    // A new installation of the same account may resume locally saved messages after sign-in.
    var messageOutboxKey: String { Self.digest([serviceOrigin, serviceInstanceID, accountID]) }

    func hasSameAccount(as other: MordeClientDataScope) -> Bool {
        serviceOrigin == other.serviceOrigin && serviceInstanceID == other.serviceInstanceID &&
            accountID == other.accountID
    }

    static func installationLookupKey(origin: String, serviceInstanceID: String, accountName: String) -> String? {
        guard let canonicalOrigin = MordeLoginStateStore.serviceOrigin(origin),
              let instanceID = UUID(uuidString: serviceInstanceID), !accountName.isEmpty else { return nil }
        return digest([canonicalOrigin, instanceID.uuidString.lowercased(), accountName])
    }

    private static func digest(_ parts: [String]) -> String {
        SHA256.hash(data: Data(parts.joined(separator: "\u{0}").utf8))
            .map { String(format: "%02x", $0) }.joined()
    }
}

enum MordeOfflineConversationPolicy {
    static func mayRestore(_ saved: MordeClientDataScope, hasActiveInstallation: Bool,
                           origin: String?, instanceID: String?, accountID: String?) -> Bool {
        guard hasActiveInstallation else { return true }
        guard let accountID, let instanceID else { return false }
        return saved.serviceOrigin == origin && saved.serviceInstanceID == instanceID.lowercased() &&
            saved.accountID == accountID
    }
}

enum MordeUnauthorizedResponsePolicy {
    static func shouldClearAuthorization(requestCredential: String, currentCredential: String?,
                                         requestScope: MordeClientDataScope,
                                         currentScope: MordeClientDataScope?,
                                         hasPendingLoginIntent: Bool, isCancelled: Bool) -> Bool {
        !isCancelled && !hasPendingLoginIntent && requestCredential == currentCredential &&
            requestScope == currentScope
    }
}

struct MordeScopedRecentConversation: Codable, Equatable {
    let scope: MordeClientDataScope
    let conversationID: String?

    func matches(scope: MordeClientDataScope) -> Bool { self.scope == scope }
}

struct MordeLoginAttemptTicket: Equatable {
    let intentID: UUID
    let origin: String
    let serviceInstanceID: String
    let accountName: String
    let clientInstanceID: UUID
    let attemptGeneration: Int64
    let idempotencyKey: String
    fileprivate let intentScopeKey: String
    fileprivate let identityScopeKey: String
}

private struct MordeLoginClientIdentity: Codable {
    let origin: String
    let serviceInstanceID: String
    let accountName: String
    var clientInstanceID: UUID
    var attemptGeneration: Int64
    var retiredInstanceIDs: [UUID]
}

private struct MordeActiveLoginAttempt: Codable, Equatable {
    let intentID: UUID
    let intentScopeKey: String
    let identityScopeKey: String
    let serviceInstanceID: String
    let clientInstanceID: UUID
    let attemptGeneration: Int64
}

private struct MordeLoginStateSnapshot: Codable {
    var identities: [String: MordeLoginClientIdentity] = [:]
    var latestIntents: [String: MordeLoginIntentRecord] = [:]
    var activeAttempts: [String: MordeActiveLoginAttempt] = [:]
    var retiredInstanceIDs: [UUID] = []
}

private struct MordeLegacyLoginClientIdentity: Codable {
    let clientInstanceID: UUID
    let retiredInstanceIDs: [UUID]?
}

private struct MordeLegacyLoginStateSnapshot: Codable {
    let identities: [String: MordeLegacyLoginClientIdentity]
}

private struct MordeLoginIntentRecord: Codable {
    let id: UUID
    let origin: String
    let accountName: String
}

enum MordeLoginStateStoreError: Error {
    case invalidOrigin
    case invalidServiceInstance
    case emptyAccount
}

final class MordeLoginStateStore {
    static let defaultsKey = "morde.clientLoginState.v2"
    private static let legacyDefaultsKey = "morde.clientLoginState.v1"
    private static let lock = NSLock()
    private let defaults: UserDefaults

    init(defaults: UserDefaults = .standard) { self.defaults = defaults }

    static func serviceOrigin(_ server: String) -> String? {
        guard var components = URLComponents(string: server),
              let scheme = components.scheme?.lowercased(),
              let host = components.host?.lowercased(), !host.isEmpty,
              components.user == nil, components.password == nil else { return nil }
        let secure = scheme == "https"
#if DEBUG
        let localHTTP = scheme == "http" && ["localhost", "127.0.0.1", "[::1]"].contains(host)
#else
        let localHTTP = false
#endif
        guard secure || localHTTP else { return nil }
        components.scheme = scheme
        components.host = host
        if (scheme == "https" && components.port == 443) || (scheme == "http" && components.port == 80) {
            components.port = nil
        }
        components.path = ""
        components.query = nil
        components.fragment = nil
        return components.string
    }

    func registerIntent(origin rawOrigin: String, accountName: String) throws -> MordeLoginIntent {
        guard let origin = Self.serviceOrigin(rawOrigin) else { throw MordeLoginStateStoreError.invalidOrigin }
        let accountName = accountName.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !accountName.isEmpty else { throw MordeLoginStateStoreError.emptyAccount }
        let intentScopeKey = Self.intentScopeKey(origin: origin, accountName: accountName)
        Self.lock.lock()
        defer { Self.lock.unlock() }
        var snapshot = loadSnapshot()
        let intent = MordeLoginIntent(id: UUID(), origin: origin, accountName: accountName, scopeKey: intentScopeKey)
        snapshot.latestIntents[intentScopeKey] = MordeLoginIntentRecord(id: intent.id, origin: origin, accountName: accountName)
        snapshot.activeAttempts.removeValue(forKey: intentScopeKey)
        try saveSnapshot(snapshot)
        return intent
    }

    func beginAttempt(for intent: MordeLoginIntent, serviceInstanceID rawServiceInstanceID: String) throws -> MordeLoginAttemptTicket? {
        guard let serviceInstanceID = UUID(uuidString: rawServiceInstanceID)?.uuidString.lowercased() else {
            throw MordeLoginStateStoreError.invalidServiceInstance
        }
        Self.lock.lock()
        defer { Self.lock.unlock() }
        var snapshot = loadSnapshot()
        guard snapshot.latestIntents[intent.scopeKey]?.id == intent.id else { return nil }

        let identityScopeKey = Self.identityScopeKey(origin: intent.origin, serviceInstanceID: serviceInstanceID,
                                                     accountName: intent.accountName)
        var identity = snapshot.identities[identityScopeKey]
        if identity?.origin != intent.origin || identity?.serviceInstanceID != serviceInstanceID ||
            identity?.accountName != intent.accountName { identity = nil }
        var nextGeneration: Int64 = 1
        var instanceID: UUID
        if var current = identity {
            if current.attemptGeneration == Int64.max {
                retire(current.clientInstanceID, from: &current)
                retire(current.clientInstanceID, in: &snapshot)
                current.clientInstanceID = newInstanceID(in: snapshot)
                nextGeneration = 1
            } else {
                nextGeneration = current.attemptGeneration + 1
            }
            instanceID = current.clientInstanceID
            current.attemptGeneration = nextGeneration
            identity = current
        } else {
            instanceID = newInstanceID(in: snapshot)
            identity = MordeLoginClientIdentity(origin: intent.origin, serviceInstanceID: serviceInstanceID,
                                                accountName: intent.accountName,
                                                clientInstanceID: instanceID, attemptGeneration: nextGeneration,
                                                retiredInstanceIDs: [])
        }

        guard let identity else { return nil }
        snapshot.identities[identityScopeKey] = identity
        let active = MordeActiveLoginAttempt(intentID: intent.id, intentScopeKey: intent.scopeKey,
                                             identityScopeKey: identityScopeKey, serviceInstanceID: serviceInstanceID,
                                             clientInstanceID: instanceID, attemptGeneration: nextGeneration)
        snapshot.activeAttempts[intent.scopeKey] = active
        try saveSnapshot(snapshot)
        return MordeLoginAttemptTicket(intentID: intent.id, origin: intent.origin,
                                       serviceInstanceID: serviceInstanceID, accountName: intent.accountName,
                                       clientInstanceID: instanceID,
                                       attemptGeneration: nextGeneration, idempotencyKey: UUID().uuidString,
                                       intentScopeKey: intent.scopeKey, identityScopeKey: identityScopeKey)
    }

    func isLatest(_ intent: MordeLoginIntent) -> Bool {
        Self.lock.lock()
        defer { Self.lock.unlock() }
        return loadSnapshot().latestIntents[intent.scopeKey]?.id == intent.id
    }

    func finish(_ intent: MordeLoginIntent) throws -> Bool {
        Self.lock.lock()
        defer { Self.lock.unlock() }
        var snapshot = loadSnapshot()
        guard snapshot.latestIntents[intent.scopeKey]?.id == intent.id else { return false }
        snapshot.latestIntents.removeValue(forKey: intent.scopeKey)
        snapshot.activeAttempts.removeValue(forKey: intent.scopeKey)
        try saveSnapshot(snapshot)
        return true
    }

    func hasPendingIntent(origin rawOrigin: String, accountName: String) -> Bool {
        guard let origin = Self.serviceOrigin(rawOrigin) else { return false }
        let accountName = accountName.trimmingCharacters(in: .whitespacesAndNewlines)
        Self.lock.lock()
        defer { Self.lock.unlock() }
        return loadSnapshot().latestIntents[Self.intentScopeKey(origin: origin, accountName: accountName)] != nil
    }

    func isCurrent(_ ticket: MordeLoginAttemptTicket) -> Bool {
        Self.lock.lock()
        defer { Self.lock.unlock() }
        return isCurrent(ticket, in: loadSnapshot())
    }

    func commitIfCurrent(_ ticket: MordeLoginAttemptTicket, persist: () throws -> Void) throws -> Bool {
        Self.lock.lock()
        defer { Self.lock.unlock() }
        var snapshot = loadSnapshot()
        guard isCurrent(ticket, in: snapshot) else { return false }
        try persist()
        snapshot.latestIntents.removeValue(forKey: ticket.intentScopeKey)
        snapshot.activeAttempts.removeValue(forKey: ticket.intentScopeKey)
        try saveSnapshot(snapshot)
        return true
    }

    func rotateClientInstanceID(origin rawOrigin: String, serviceInstanceID rawServiceInstanceID: String,
                                accountName: String) throws {
        guard let origin = Self.serviceOrigin(rawOrigin) else { throw MordeLoginStateStoreError.invalidOrigin }
        guard let serviceInstanceID = UUID(uuidString: rawServiceInstanceID)?.uuidString.lowercased() else {
            throw MordeLoginStateStoreError.invalidServiceInstance
        }
        let accountName = accountName.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !accountName.isEmpty else { throw MordeLoginStateStoreError.emptyAccount }
        let intentScopeKey = Self.intentScopeKey(origin: origin, accountName: accountName)
        let identityScopeKey = Self.identityScopeKey(origin: origin, serviceInstanceID: serviceInstanceID,
                                                     accountName: accountName)
        Self.lock.lock()
        defer { Self.lock.unlock() }
        var snapshot = loadSnapshot()
        var identity = snapshot.identities[identityScopeKey] ?? MordeLoginClientIdentity(
            origin: origin, serviceInstanceID: serviceInstanceID, accountName: accountName,
            clientInstanceID: newInstanceID(in: snapshot),
            attemptGeneration: 0, retiredInstanceIDs: [])
        if identity.origin != origin || identity.serviceInstanceID != serviceInstanceID || identity.accountName != accountName {
            identity = MordeLoginClientIdentity(origin: origin, serviceInstanceID: serviceInstanceID, accountName: accountName,
                                                clientInstanceID: newInstanceID(in: snapshot),
                                                attemptGeneration: 0, retiredInstanceIDs: [])
        }
        retire(identity.clientInstanceID, from: &identity)
        retire(identity.clientInstanceID, in: &snapshot)
        identity.clientInstanceID = newInstanceID(in: snapshot)
        identity.attemptGeneration = 0
        snapshot.identities[identityScopeKey] = identity
        snapshot.latestIntents.removeValue(forKey: intentScopeKey)
        snapshot.activeAttempts.removeValue(forKey: intentScopeKey)
        try saveSnapshot(snapshot)
    }

    func invalidatePendingAttempts(origin rawOrigin: String? = nil, accountName: String? = nil) throws {
        let origin = rawOrigin.flatMap(Self.serviceOrigin)
        Self.lock.lock()
        defer { Self.lock.unlock() }
        var snapshot = loadSnapshot()
        let keys = snapshot.latestIntents.compactMap { key, intent -> String? in
            if rawOrigin != nil && (origin == nil || intent.origin != origin) { return nil }
            if let accountName, intent.accountName != accountName { return nil }
            return key
        }
        for key in keys {
            snapshot.latestIntents.removeValue(forKey: key)
            snapshot.activeAttempts.removeValue(forKey: key)
        }
        try saveSnapshot(snapshot)
    }

    private func isCurrent(_ ticket: MordeLoginAttemptTicket, in snapshot: MordeLoginStateSnapshot) -> Bool {
        let active = MordeActiveLoginAttempt(intentID: ticket.intentID, intentScopeKey: ticket.intentScopeKey,
                                             identityScopeKey: ticket.identityScopeKey,
                                             serviceInstanceID: ticket.serviceInstanceID,
                                             clientInstanceID: ticket.clientInstanceID,
                                             attemptGeneration: ticket.attemptGeneration)
        guard snapshot.latestIntents[ticket.intentScopeKey]?.id == ticket.intentID,
              snapshot.activeAttempts[ticket.intentScopeKey] == active,
              let identity = snapshot.identities[ticket.identityScopeKey] else { return false }
        return identity.origin == ticket.origin && identity.serviceInstanceID == ticket.serviceInstanceID &&
            identity.accountName == ticket.accountName &&
            identity.clientInstanceID == ticket.clientInstanceID && identity.attemptGeneration == ticket.attemptGeneration
    }

    private func loadSnapshot() -> MordeLoginStateSnapshot {
        if let data = defaults.data(forKey: Self.defaultsKey),
           let snapshot = try? JSONDecoder().decode(MordeLoginStateSnapshot.self, from: data) {
            return snapshot
        }
        guard let legacyData = defaults.data(forKey: Self.legacyDefaultsKey),
              let legacy = try? JSONDecoder().decode(MordeLegacyLoginStateSnapshot.self, from: legacyData) else {
            return MordeLoginStateSnapshot()
        }
        let retiredIDs = legacy.identities.values.flatMap { ($0.retiredInstanceIDs ?? []) + [$0.clientInstanceID] }
        return MordeLoginStateSnapshot(retiredInstanceIDs: Array(Set(retiredIDs)))
    }

    private func saveSnapshot(_ snapshot: MordeLoginStateSnapshot) throws {
        defaults.set(try JSONEncoder().encode(snapshot), forKey: Self.defaultsKey)
    }

    private static func intentScopeKey(origin: String, accountName: String) -> String {
        digest([origin, accountName])
    }

    private static func identityScopeKey(origin: String, serviceInstanceID: String, accountName: String) -> String {
        digest([origin, serviceInstanceID, accountName])
    }

    private static func digest(_ parts: [String]) -> String {
        let data = Data(parts.joined(separator: "\u{0}").utf8)
        return SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
    }

    private func newInstanceID(in snapshot: MordeLoginStateSnapshot) -> UUID {
        let usedIDs = Set(snapshot.retiredInstanceIDs + snapshot.identities.values.flatMap {
            $0.retiredInstanceIDs + [$0.clientInstanceID]
        })
        var candidate = UUID()
        while usedIDs.contains(candidate) { candidate = UUID() }
        return candidate
    }

    private func retire(_ instanceID: UUID, from identity: inout MordeLoginClientIdentity) {
        if !identity.retiredInstanceIDs.contains(instanceID) { identity.retiredInstanceIDs.append(instanceID) }
    }

    private func retire(_ instanceID: UUID, in snapshot: inout MordeLoginStateSnapshot) {
        if !snapshot.retiredInstanceIDs.contains(instanceID) { snapshot.retiredInstanceIDs.append(instanceID) }
    }
}

enum MordeTaskAttempt {
    static func fingerprint(conversationID: String?, hostID: String, workspaceID: String? = nil,
                            modelID: String, objective: String, attachmentIDs: [String] = []) -> String {
        let fields = [conversationID ?? "", hostID, workspaceID ?? "", modelID, objective] + attachmentIDs
        let input = Data(fields.joined(separator: "\u{0}").utf8)
        return SHA256.hash(data: input).map { String(format: "%02x", $0) }.joined()
    }

    static func canContinuePendingAttempt(_ pendingFingerprint: String?, requestedFingerprint: String) -> Bool {
        pendingFingerprint == nil || pendingFingerprint == requestedFingerprint
    }
}

enum MordeTaskTargetPolicy {
    static func hostIsReady(state: String, controlState: String?) -> Bool {
        state == "running" && controlState == "ai"
    }

    static func canRestoreHost(state: String, isOwner: Bool, workspaceBindingCount: Int,
                               workspaceReady: Bool) -> Bool {
        state == "stopped" && isOwner && workspaceBindingCount == 1 && workspaceReady
    }

    static func workspaceIsReady(bindingState: String, workspaceState: String, syncState: String,
                                 appliedRevision: Int64, desiredRevision: Int64,
                                 workspaceRevision: Int64) -> Bool {
        bindingState == "active" && workspaceState == "ready" && syncState == "healthy" &&
            appliedRevision == desiredRevision && appliedRevision == workspaceRevision
    }

    static func targetIsUnavailable(hostID: String, workspaceID: String, workspaceBindingCount: Int,
                                    hostExists: Bool,
                                    hostReady: Bool, workspaceExists: Bool,
                                    workspaceReady: Bool) -> Bool {
        if !hostID.isEmpty && (workspaceID.isEmpty || workspaceBindingCount != 1 || !hostExists || !hostReady) {
            return true
        }
        if !workspaceID.isEmpty && (hostID.isEmpty || !workspaceExists || !workspaceReady) { return true }
        return false
    }
}

enum MordeTaskTargetPresentation {
    static func hostState(_ state: String) -> String {
        switch state {
        case "creating": return "创建中"
        case "running": return "运行中"
        case "stopping": return "停止中"
        case "stopped": return "已停止"
        case "failed": return "失败"
        case "deleting": return "删除中"
        case "deleted": return "已删除"
        default: return "状态未知"
        }
    }

    static func hostControlState(_ state: String?) -> String {
        switch state {
        case "ai": return "AI 控制"
        case "taking_over": return "正在接管"
        case "user": return "人工控制"
        case "returning": return "正在交还 AI"
        default: return "控制状态待核验"
        }
    }

    static func fixedTarget(executionLocation: String?, hostID: String?, hostName: String?,
                            hostState: String?, workspaceID: String?, workspaceName: String?,
                            inputRevision: Int64?) -> String {
        let runtime: String
        switch executionLocation {
        case "server": runtime = "云端 Agent Runtime"
        case "legacy": runtime = "旧客户端 Runtime"
        default: runtime = "执行位置待核验"
        }
        let computer: String
        if let hostID, !hostID.isEmpty {
            if let hostName {
                computer = "\(hostName)（\(hostState.map(hostStateLabel) ?? "状态未知")）"
            } else {
                computer = "Host \(hostID)（当前不可访问）"
            }
        } else {
            computer = "无电脑目标"
        }
        let workspace: String
        if let workspaceID, !workspaceID.isEmpty {
            let name = workspaceName ?? workspaceID
            workspace = inputRevision.map { "\(name) v\($0)" } ?? name
        } else {
            workspace = "无 Workspace"
        }
        return "\(runtime) · \(computer) · \(workspace)"
    }

    private static func hostStateLabel(_ state: String) -> String { hostState(state) }
}

// nanoMuse: hash action payloads so retries reuse a key without retaining clarification text.
struct MordeActionRetryAttempt: Equatable {
    let fingerprint: String
    let idempotencyKey: String

    init(fingerprint: String, idempotencyKey: String = UUID().uuidString) {
        self.fingerprint = fingerprint
        self.idempotencyKey = idempotencyKey
    }

    func matches(fingerprint: String) -> Bool { self.fingerprint == fingerprint }

    static func reusing(_ current: MordeActionRetryAttempt?, for fingerprint: String) -> MordeActionRetryAttempt {
        guard let current, current.matches(fingerprint: fingerprint) else {
            return MordeActionRetryAttempt(fingerprint: fingerprint)
        }
        return current
    }

    static func fingerprint(taskID: String, endpoint: String, body: [String: Any]) -> String? {
        guard JSONSerialization.isValidJSONObject(body),
              let encodedBody = try? JSONSerialization.data(withJSONObject: body, options: [.sortedKeys]) else {
            return nil
        }
        var input = Data(taskID.utf8)
        input.append(0)
        input.append(contentsOf: endpoint.utf8)
        input.append(0)
        input.append(encodedBody)
        return SHA256.hash(data: input).map { String(format: "%02x", $0) }.joined()
    }
}

enum MordeClientVersion {
    private struct Parsed: Equatable {
        let major: Int
        let minor: Int
        let patch: Int
        let prerelease: [String]
    }

    static func meetsMinimum(current: String?, minimum: String?) -> Bool {
        guard let current, let actual = parse(current), let minimum, let required = parse(minimum) else { return false }
        if actual.major != required.major { return actual.major > required.major }
        if actual.minor != required.minor { return actual.minor > required.minor }
        if actual.patch != required.patch { return actual.patch > required.patch }
        if actual.prerelease.isEmpty || required.prerelease.isEmpty { return actual.prerelease.isEmpty }
        for index in 0..<max(actual.prerelease.count, required.prerelease.count) {
            guard index < actual.prerelease.count else { return false }
            guard index < required.prerelease.count else { return true }
            let left = actual.prerelease[index]
            let right = required.prerelease[index]
            if left == right { continue }
            let leftIsNumber = left.utf8.allSatisfy { (48...57).contains($0) }
            let rightIsNumber = right.utf8.allSatisfy { (48...57).contains($0) }
            if leftIsNumber && rightIsNumber {
                if left.count != right.count { return left.count > right.count }
                return left > right
            }
            if leftIsNumber { return false }
            if rightIsNumber { return true }
            return left < right
        }
        return true
    }

    private static func parse(_ raw: String) -> Parsed? {
        let buildParts = raw.split(separator: "+", maxSplits: 1, omittingEmptySubsequences: false)
        guard let version = buildParts.first, buildParts.count <= 2, !version.isEmpty else { return nil }
        if buildParts.count == 2 {
            let buildIdentifiers = buildParts[1].split(separator: ".", omittingEmptySubsequences: false)
            guard !buildIdentifiers.isEmpty, buildIdentifiers.allSatisfy(validIdentifier) else { return nil }
        }
        let versionAndPrerelease = version.split(separator: "-", maxSplits: 1, omittingEmptySubsequences: false)
        guard versionAndPrerelease.count <= 2 else { return nil }
        let core = versionAndPrerelease[0].split(separator: ".", omittingEmptySubsequences: false)
        guard core.count == 3,
              core.allSatisfy({ $0 == "0" || ($0.first != "0" && $0.allSatisfy(\.isNumber)) }),
              let major = Int(core[0]), let minor = Int(core[1]), let patch = Int(core[2]) else { return nil }
        let prerelease: [String]
        if versionAndPrerelease.count == 2 {
            let parts = versionAndPrerelease[1].split(separator: ".", omittingEmptySubsequences: false)
            guard !parts.isEmpty, parts.allSatisfy(validIdentifier),
                  parts.allSatisfy({ !($0.count > 1 && $0.allSatisfy(\.isNumber) && $0.first == "0") }) else { return nil }
            prerelease = parts.map(String.init)
        } else {
            prerelease = []
        }
        return Parsed(major: major, minor: minor, patch: patch, prerelease: prerelease)
    }

    private static func validIdentifier(_ value: Substring) -> Bool {
        !value.isEmpty && value.allSatisfy { $0.isASCII && ($0.isLetter || $0.isNumber || $0 == "-") }
    }
}

enum MordeTaskSnapshotCursor {
    /// A snapshot may advance beyond the event that triggered it; resume after the snapshot's
    /// sequence so older events cannot replay stale state over the newer snapshot.
    static func resumeCursor(eventID: String, snapshotSequence: Int64) -> String? {
        guard let observed = Int64(eventID), observed >= 0, snapshotSequence >= observed else { return nil }
        return String(snapshotSequence)
    }
}

/// Each task event stream owns one cursor value; task IDs prevent one stream from advancing another.
struct MordeTaskEventCursor: Equatable, Sendable {
    let taskID: String
    private(set) var afterSequence: Int64

    init(taskID: String, afterSequence: Int64) {
        self.taskID = taskID
        self.afterSequence = max(0, afterSequence)
    }

    var queryValue: String? { afterSequence > 0 ? String(afterSequence) : nil }

    mutating func advance(eventID: String, for taskID: String) -> Bool {
        guard taskID == self.taskID, let sequence = Int64(eventID), sequence > 0 else { return false }
        afterSequence = max(afterSequence, sequence)
        return true
    }

    mutating func resume(eventID: String, snapshotSequence: Int64) -> Bool {
        guard let value = MordeTaskSnapshotCursor.resumeCursor(eventID: eventID,
                                                               snapshotSequence: snapshotSequence),
              let sequence = Int64(value), sequence >= afterSequence else { return false }
        afterSequence = sequence
        return true
    }
}

enum MordeConversationTaskStopPolicy {
    private static let stoppableStatuses: Set<String> = ["queued", "running", "paused", "waiting_user"]

    static func targetTaskID(taskID: String, taskConversationID: String,
                             currentConversationID: String?, trackedTaskIDs: Set<String>,
                             status: String) -> String? {
        guard MordeTaskResponsePolicy.hasValidTaskID(taskID),
              taskConversationID == currentConversationID,
              trackedTaskIDs.contains(taskID), stoppableStatuses.contains(status) else { return nil }
        return taskID
    }
}

struct MordeClientMessageSource: Decodable, Equatable {
    let user_message_id: String
    let task_id: String
    let task_revision: Int64
    let event_sequence: Int64
    let content: String
}

struct MordeClientMessage: Decodable, Equatable, Identifiable {
    let id: String
    let conversation_id: String?
    let sequence: Int64?
    let task_id: String?
    let role: String
    let content: String
    let attachment_ids: [String]?
    let status: String?
    let delivery_status: String?
    let created_at: String?
    let client_message_id: String?
    let routing_status: String?
    let routing_failure_reason: String?
    let local_send_status: String?
    let owner_id: String?
    let actor_id: String?
    let task_revision: Int64?
    let result_event_sequence: Int64?
    let sources: [MordeClientMessageSource]?
    let output_status: String?
    let output_task_revision: Int64?
    let output_result_event_sequence: Int64?

    var displayStatus: String? { output_status ?? status }
    var displayTaskRevision: Int64? { output_task_revision ?? task_revision }
    var displayResultEventSequence: Int64? { output_result_event_sequence ?? result_event_sequence }

    init(id: String, conversation_id: String? = nil, sequence: Int64? = nil, task_id: String? = nil,
         role: String, content: String, attachment_ids: [String]? = nil, status: String? = nil, delivery_status: String? = nil,
         created_at: String? = nil, client_message_id: String? = nil, routing_status: String? = nil,
         routing_failure_reason: String? = nil,
         local_send_status: String? = nil, owner_id: String? = nil, actor_id: String? = nil,
         task_revision: Int64? = nil, result_event_sequence: Int64? = nil,
         sources: [MordeClientMessageSource]? = nil, output_status: String? = nil,
         output_task_revision: Int64? = nil, output_result_event_sequence: Int64? = nil) {
        self.id = id
        self.conversation_id = conversation_id
        self.sequence = sequence
        self.task_id = task_id
        self.role = role
        self.content = content
        self.attachment_ids = attachment_ids
        self.status = status
        self.delivery_status = delivery_status
        self.created_at = created_at
        self.client_message_id = client_message_id
        self.routing_status = routing_status
        self.routing_failure_reason = routing_failure_reason
        self.local_send_status = local_send_status
        self.owner_id = owner_id
        self.actor_id = actor_id
        self.task_revision = task_revision
        self.result_event_sequence = result_event_sequence
        self.sources = sources
        self.output_status = output_status
        self.output_task_revision = output_task_revision
        self.output_result_event_sequence = output_result_event_sequence
    }
}

struct MordeConversationMessageSourceLine: Equatable, Identifiable {
    let source: MordeClientMessageSource
    let originatingMessageContent: String?

    var id: String {
        "\(source.user_message_id):\(source.task_id):\(source.task_revision):\(source.event_sequence)"
    }

    var originLabel: String {
        guard let originatingMessageContent else { return "关联消息 \(source.user_message_id)" }
        let normalized = originatingMessageContent.split(whereSeparator: \.isWhitespace).joined(separator: " ")
        let excerpt = String(normalized.prefix(72))
        return "关联消息：\(excerpt)\(normalized.count > excerpt.count ? "…" : "")"
    }
}

enum MordeConversationMessageSourcePresentation {
    static func lines(_ sources: [MordeClientMessageSource]?, userMessages: [MordeClientMessage]) -> [MordeConversationMessageSourceLine] {
        (sources ?? []).map { source in
            let originatingMessage = userMessages.first {
                $0.id == source.user_message_id && $0.role == "user"
            }
            return MordeConversationMessageSourceLine(
                source: source,
                originatingMessageContent: originatingMessage?.content
            )
        }
    }
}

struct MordeConversationMessageReceipt: Decodable {
    let message: MordeClientMessage
    // Text-upload commits may omit this field; a replay has the same message semantics.
    let replayed: Bool?
}

enum MordeConversationMessageReceiptPolicy {
    static func matches(_ message: MordeClientMessage, clientMessageID: String,
                        conversationID: String, content: String) -> Bool {
        message.role == "user" && !message.id.isEmpty && message.client_message_id == clientMessageID &&
            message.conversation_id == conversationID && message.content.utf8.elementsEqual(content.utf8)
    }
}

enum MordeConversationMessageOrder {
    static func ascending<Message>(
        _ messages: [Message],
        sequence: (Message) -> Int64?,
        createdAt: (Message) -> String?,
        id: (Message) -> String
    ) -> [Message] {
        let indexed = Array(messages.enumerated())
        let sequenced = indexed.filter { sequence($0.element) != nil }.sorted { lhs, rhs in
            let left = sequence(lhs.element)!
            let right = sequence(rhs.element)!
            if left != right { return left < right }
            let leftDate = createdAt(lhs.element) ?? ""
            let rightDate = createdAt(rhs.element) ?? ""
            if leftDate != rightDate { return leftDate < rightDate }
            let leftID = id(lhs.element), rightID = id(rhs.element)
            return leftID == rightID ? lhs.offset < rhs.offset : leftID < rightID
        }
        let local = indexed.filter { sequence($0.element) == nil }.sorted { lhs, rhs in
            let leftDate = createdAt(lhs.element) ?? ""
            let rightDate = createdAt(rhs.element) ?? ""
            return leftDate == rightDate ? lhs.offset < rhs.offset : leftDate < rightDate
        }
        var ordered: [Message] = []
        ordered.reserveCapacity(messages.count)
        var serverIndex = 0, localIndex = 0
        while serverIndex < sequenced.count && localIndex < local.count {
            let server = sequenced[serverIndex].element
            let pending = local[localIndex].element
            if (createdAt(pending) ?? "") < (createdAt(server) ?? "") {
                ordered.append(pending)
                localIndex += 1
            } else {
                ordered.append(server)
                serverIndex += 1
            }
        }
        ordered.append(contentsOf: sequenced[serverIndex...].map(\.element))
        ordered.append(contentsOf: local[localIndex...].map(\.element))
        return ordered
    }
}

enum MordeTaskSnapshotMessageMerge {
    /// Reconcile a task's assistant snapshot with its current task state before presenting it.
    /// A late snapshot can still say `streaming` after the task has already reached a terminal
    /// state; keeping that status would make the chat claim that generation is still in progress.
    static func reconcilingStreamingSnapshot(_ snapshot: MordeClientMessage?, taskStatus: String) -> MordeClientMessage? {
        guard let snapshot, snapshot.role == "assistant", snapshot.displayStatus == "streaming" else {
            return snapshot
        }
        let resolvedStatus: String
        switch taskStatus {
        case "running":
            return snapshot
        case "completed":
            resolvedStatus = "completed"
        case "failed", "cancelled":
            resolvedStatus = "interrupted"
        default:
            return snapshot
        }
        return MordeClientMessage(
            id: snapshot.id, conversation_id: snapshot.conversation_id, sequence: snapshot.sequence,
            task_id: snapshot.task_id, role: snapshot.role, content: snapshot.content,
            attachment_ids: snapshot.attachment_ids, status: resolvedStatus,
            delivery_status: snapshot.delivery_status, created_at: snapshot.created_at,
            client_message_id: snapshot.client_message_id, routing_status: snapshot.routing_status,
            routing_failure_reason: snapshot.routing_failure_reason, local_send_status: snapshot.local_send_status,
            owner_id: snapshot.owner_id, actor_id: snapshot.actor_id, task_revision: snapshot.task_revision,
            result_event_sequence: snapshot.result_event_sequence, sources: snapshot.sources,
            output_status: nil, output_task_revision: snapshot.output_task_revision,
            output_result_event_sequence: snapshot.output_result_event_sequence)
    }

    static func shouldIncludeSnapshot(resultMessagePending: Bool?, resultMessagePublished: Bool?, status: String?) -> Bool {
        if status == "interrupted" { return true }
        return resultMessagePublished != true && (resultMessagePending != true || status == "streaming")
    }

    static func shouldDiscardPublishedSynthetic(_ message: MordeClientMessage, taskID: String,
                                                currentRevision: Int64?, published: Bool?) -> Bool {
        guard published == true, message.role == "assistant", message.task_id == taskID,
              message.output_status == nil, message.status != nil, message.displayStatus != "interrupted",
              message.sources?.isEmpty ?? true,
              let currentRevision, let revision = message.displayTaskRevision else { return false }
        return revision == currentRevision
    }

    static func shouldShowTaskResult(resultMessagePending: Bool?, resultMessagePublished: Bool?) -> Bool {
        resultMessagePending != true && resultMessagePublished != true
    }

    static let statuses: Set<String> = ["streaming", "completed", "interrupted"]

    static func sourceCoversSnapshot(_ source: MordeClientMessageSource, snapshot: MordeClientMessage,
                                     taskID: String) -> Bool {
        guard snapshot.displayStatus == "completed", let revision = snapshot.displayTaskRevision,
              let eventSequence = snapshot.displayResultEventSequence else { return false }
        return source.task_id == taskID && source.task_revision == revision && source.event_sequence == eventSequence
    }

    static func preservingInterrupted(_ previous: [MordeClientMessage], in current: [MordeClientMessage],
                                      conversationID: String) -> [MordeClientMessage] {
        var values = current
        for snapshot in previous where snapshot.role == "assistant" && snapshot.displayStatus == "interrupted" &&
            snapshot.output_status == nil && snapshot.conversation_id == conversationID {
            guard let taskID = snapshot.task_id, !values.contains(where: { $0.id == snapshot.id }) else { continue }
            values = merge(values, snapshot: snapshot, taskID: taskID, conversationID: conversationID)
        }
        return values
    }

    static func deduplicatingPublished(_ messages: [MordeClientMessage]) -> [MordeClientMessage] {
        let sources = messages.flatMap { $0.sources ?? [] }
        return messages.filter { message in
            guard message.role == "assistant", message.displayStatus == "completed",
                  message.sources?.isEmpty ?? true, let taskID = message.task_id else { return true }
            return !sources.contains { sourceCoversSnapshot($0, snapshot: message, taskID: taskID) }
        }
    }

    static func merge(_ messages: [MordeClientMessage], snapshot: MordeClientMessage?,
                      taskID: String, conversationID: String) -> [MordeClientMessage] {
        guard let snapshot,
              snapshot.role == "assistant",
              snapshot.task_id == taskID,
              snapshot.conversation_id == conversationID,
              let status = snapshot.displayStatus,
              statuses.contains(status) else { return messages }

        if messages.contains(where: { message in
            message.sources?.contains(where: { sourceCoversSnapshot($0, snapshot: snapshot, taskID: taskID) }) == true
        }) {
            return messages.filter { $0.id != snapshot.id || $0.sources?.isEmpty == false }
        }

        var merged = messages
        if let index = merged.firstIndex(where: { $0.id == snapshot.id }) {
            let existing = merged[index]
            if existing.sources?.isEmpty == false { return merged }
            guard existing.role == "assistant", existing.conversation_id.map({ $0 == conversationID }) ?? true,
                  existing.task_id.map({ $0 == taskID }) ?? true else { return merged }
            if let existingRevision = existing.displayTaskRevision, let snapshotRevision = snapshot.displayTaskRevision,
               existingRevision != snapshotRevision { return merged }
            if existing.sequence != nil && existing.output_status != nil {
                // A persisted final/interrupted message is authoritative. A streaming message may
                // advance only with a longer snapshot for the same revision and exact byte prefix.
                guard existing.displayStatus == "streaming", let existingRevision = existing.output_task_revision,
                      let snapshotRevision = snapshot.displayTaskRevision, existingRevision == snapshotRevision,
                      snapshot.content.utf8.count > existing.content.utf8.count,
                      snapshot.content.utf8.starts(with: existing.content.utf8) else { return merged }
            }
            if existing.sequence != nil && existing.output_status == nil && existing.status == nil { return merged }
            // Legacy conversation messages lacked sequence/status. Keep their final text when a
            // stale streaming snapshot happens to have the same ID.
            if status == "streaming", existing.output_status == nil, existing.displayStatus == nil {
                merged[index] = MordeClientMessage(
                    id: existing.id, conversation_id: existing.conversation_id,
                    sequence: existing.sequence, task_id: existing.task_id, role: existing.role, content: existing.content,
                    attachment_ids: existing.attachment_ids,
                    status: "completed", delivery_status: existing.delivery_status,
                    created_at: existing.created_at, client_message_id: existing.client_message_id,
                    routing_status: existing.routing_status, routing_failure_reason: existing.routing_failure_reason,
                    local_send_status: existing.local_send_status,
                    owner_id: existing.owner_id, actor_id: existing.actor_id, task_revision: existing.task_revision,
                    result_event_sequence: existing.result_event_sequence, sources: existing.sources,
                    output_status: existing.output_status, output_task_revision: existing.output_task_revision,
                    output_result_event_sequence: existing.output_result_event_sequence)
            } else {
                merged[index] = MordeClientMessage(
                    id: snapshot.id, conversation_id: snapshot.conversation_id ?? existing.conversation_id,
                    sequence: existing.sequence ?? snapshot.sequence,
                    task_id: snapshot.task_id ?? existing.task_id, role: snapshot.role, content: snapshot.content,
                    attachment_ids: snapshot.attachment_ids ?? existing.attachment_ids,
                    status: snapshot.status, delivery_status: snapshot.delivery_status ?? existing.delivery_status,
                    created_at: snapshot.created_at ?? existing.created_at,
                    client_message_id: snapshot.client_message_id ?? existing.client_message_id,
                    routing_status: snapshot.routing_status ?? existing.routing_status,
                    routing_failure_reason: snapshot.routing_failure_reason ?? existing.routing_failure_reason,
                    local_send_status: existing.local_send_status,
                    owner_id: snapshot.owner_id ?? existing.owner_id, actor_id: snapshot.actor_id ?? existing.actor_id,
                    task_revision: snapshot.task_revision,
                    result_event_sequence: snapshot.result_event_sequence ?? existing.result_event_sequence,
                    sources: snapshot.sources ?? existing.sources,
                    output_status: existing.output_status == nil ? snapshot.output_status : snapshot.displayStatus,
                    output_task_revision: snapshot.output_task_revision ?? existing.output_task_revision,
                    output_result_event_sequence: snapshot.output_result_event_sequence ?? existing.output_result_event_sequence)
            }
        } else {
            merged.append(snapshot)
        }
        return MordeConversationMessageOrder.ascending(
            merged, sequence: { $0.sequence }, createdAt: { $0.created_at }, id: { $0.id })
    }
}

enum MordeTaskStepPresentation {
    // The server retains the original tool call in its audit trail; these labels are only for display.
    static func titleLocalizationKey(_ title: String) -> String? {
        if title == "开始处理" { return "Starting the task" }
        let prefixes = ["Run cloud dsh tool: ", "Run Hostless Workspace tool: "]
        guard let prefix = prefixes.first(where: { title.hasPrefix($0) }) else { return nil }
        let tool = String(title.dropFirst(prefix.count)).lowercased()
        switch tool {
        case "morde_web_search": return "Search the web"
        case "morde_web_read": return "Read a web page"
        case "morde_workspace_list": return "Browse workspace files"
        case "morde_workspace_read": return "Read a workspace file"
        case "morde_workspace_write": return "Update a workspace file"
        case "morde_read_task_attachment": return "Read an attachment"
        default:
            if tool.contains("computer") || tool.contains("desktop") || tool.contains("host") {
                return "Use the cloud computer"
            }
            if tool.hasPrefix("morde_workspace_") || prefix == "Run Hostless Workspace tool: " {
                return "Work in the workspace"
            }
            return "Process the task"
        }
    }

    static func title(_ title: String) -> String {
        guard let key = titleLocalizationKey(title) else { return title }
        return NSLocalizedString(key, comment: "Task step shown to the user")
    }

    static func summary(_ summary: String, title: String) -> String? {
        if summary.isEmpty { return nil }
        if title == "开始处理" && summary == "正在处理你的请求。" {
            return NSLocalizedString("Processing your request.", comment: "Task has started")
        }
        if titleLocalizationKey(title) != nil &&
            ["Checking current authorization, budget, and Host control.",
             "Checking the task checkpoint and Workspace revision."].contains(summary) { return nil }
        return summary
    }

    static func isStartedProgress(kind: String?, title: String, status: String) -> Bool {
        kind == "progress" && title == "开始处理" && status == "succeeded"
    }
}

enum MordeTaskStartPresentation {
    static func isWaiting(status: String, currentRevision: Int64?, desiredRevision: Int64?,
                          currentRevisionStarted: Bool?) -> Bool {
        guard ["queued", "running"].contains(status), let started = currentRevisionStarted else { return false }
        guard started, let currentRevision, let desiredRevision else { return true }
        return currentRevision != desiredRevision
    }
}

enum MordeTaskStatusPresentation {
    // nanoMuse: Keep the localized string lookup keys beside the wire status mapping.
    static func localizationKey(status: String, waitingReason: String? = nil,
                                stopRequestStatus: String? = nil) -> String {
        switch label(status: status, waitingReason: waitingReason, stopRequestStatus: stopRequestStatus) {
        case "排队中": return "Queued"
        case "运行中": return "Running"
        case "等你审批": return "Waiting for approval"
        case "等待你补充信息": return "Waiting for your input"
        case "等待电脑同步": return "Waiting for computer sync"
        case "工作区已变化，等待确认": return "Workspace changed; awaiting confirmation"
        case "电脑焦点已变化，等待确认": return "Computer focus changed; awaiting confirmation"
        case "操作结果待核对": return "Operation result needs review"
        case "当前操作不受支持，等待处理": return "Operation not supported; action needed"
        case "连接器已撤销，等待处理": return "Connector revoked; action needed"
        case "电脑状态已变化，等待处理": return "Computer state changed; action needed"
        case "等待任务额度处理": return "Waiting for task quota"
        case "需要选择电脑才能继续任务": return "Choose a computer to continue this task"
        case "所需云端电脑暂不可用": return "Required cloud computer is unavailable"
        case "所需工作区暂不可用": return "Required workspace is unavailable"
        case "电脑操作结果待核对": return "Computer operation result needs review"
        case "任务已绑定其他云端电脑，无法安全切换；请为新目标另发起任务。":
            return "Task is bound to another cloud computer; start a new task for the new target."
        case "任务已绑定其他工作区，无法安全切换；请为新目标另发起任务。":
            return "Task is bound to another workspace; start a new task for the new target."
        case "原项目权限/上下文变化，未安全恢复，请重新发送。":
            return "原项目权限/上下文变化，未安全恢复，请重新发送。"
        case "消息及上下文超过当前模型输入上限，原文已保留；请缩短后重新发送。":
            return "Message and context exceed the model input limit; the original is retained. Shorten and send again."
        case "等待你处理": return "Waiting for you"
        case "正在暂停": return "Pausing"
        case "已暂停": return "Paused"
        case "正在停止，等待服务端确认": return "Stopping; awaiting server confirmation"
        case "停止请求已处理，等待任务状态更新": return "Stop request processed; task update pending"
        case "停止状态待确认": return "Stop status needs confirmation"
        case "已完成": return "Completed"
        case "失败": return "Task failed"
        case "已取消": return "Cancelled"
        default: return "Status unknown"
        }
    }

    static func notificationLocalizationKey(kind: String) -> String {
        switch kind {
        case "completed": return "Task completed"
        case "failed": return "Task failed"
        case "waiting_user": return "Action needed"
        default: return "Task notification"
        }
    }

    static func label(status: String, waitingReason: String? = nil, stopRequestStatus: String? = nil) -> String {
        switch status {
        case "queued": return "排队中"
        case "running": return "运行中"
        case "waiting_user":
            if waitingReason == "approval_required" { return "等你审批" }
            if waitingReason == "clarification_required" { return "等待你补充信息" }
            switch waitingReason {
            case "workspace_host_sync": return "等待电脑同步"
            case "workspace_input_changed": return "工作区已变化，等待确认"
            case "desktop_focus_changed": return "电脑焦点已变化，等待确认"
            case "mcp_result_unknown", "dsh_recovery_unresolved": return "操作结果待核对"
            case "mcp_result_unsupported": return "当前操作不受支持，等待处理"
            case "mcp_connector_revoked": return "连接器已撤销，等待处理"
            case "host_fence_changed": return "电脑状态已变化，等待处理"
            case "tool_call_budget": return "等待任务额度处理"
            case "model_context_limit": return "消息及上下文超过当前模型输入上限，原文已保留；请缩短后重新发送。"
            case "computer_target_missing": return "需要选择电脑才能继续任务"
            case "computer_unavailable": return "所需云端电脑暂不可用"
            case "workspace_unavailable": return "所需工作区暂不可用"
            case "target_result_unknown": return "电脑操作结果待核对"
            case "computer_target_changed": return "任务已绑定其他云端电脑，无法安全切换；请为新目标另发起任务。"
            case "workspace_target_changed": return "任务已绑定其他工作区，无法安全切换；请为新目标另发起任务。"
            case "project_recovery_unavailable": return "原项目权限/上下文变化，未安全恢复，请重新发送。"
            default: break
            }
            if waitingReason?.localizedCaseInsensitiveContains("unknown") == true { return "操作结果待核对" }
            return "等待你处理"
        case "pausing": return "正在暂停"
        case "paused": return "已暂停"
        case "cancelling":
            if stopRequestStatus == "pending" { return "正在停止，等待服务端确认" }
            if stopRequestStatus == "completed" { return "停止请求已处理，等待任务状态更新" }
            return "停止状态待确认"
        case "completed": return "已完成"
        case "failed": return "失败"
        case "cancelled": return "已取消"
        default: return "状态未知"
        }
    }

    static func notificationLabel(kind: String) -> String {
        switch kind {
        case "completed": return "任务完成"
        case "failed": return "任务失败"
        case "waiting_user": return "需要你处理"
        default: return "任务通知"
        }
    }
}

struct MordeRetryAttempt: Codable, Equatable {
    let scope: MordeClientDataScope
    let fingerprint: String
    let key: String

    init(scope: MordeClientDataScope, fingerprint: String, key: String) {
        self.scope = scope
        self.fingerprint = fingerprint
        self.key = key
    }

    func matches(scope: MordeClientDataScope, fingerprint: String) -> Bool {
        self.scope == scope && self.fingerprint == fingerprint
    }

}

enum MordeRoutingPresentation {
    static func localizationKey(_ status: String?) -> String? {
        status == "waiting_adjustment" ? "Adjustment received; waiting to update" : nil
    }

    static func failureLocalizationKey(_ reason: String?) -> String {
        switch reason {
        case "model_unavailable": return "Model temporarily unavailable"
        case "routing_context_unavailable": return "Task context unavailable"
        case "routing_action_unavailable": return "Unable to update the task"
        case "message_too_long": return "Message and context exceed the model input limit; the original is retained. Shorten and send again."
        default: return "Message processing failed"
        }
    }

    static func canRetryProcessing(_ reason: String?) -> Bool { reason != "message_too_long" }
}

enum MordeRoutingRetryAttempt {
    static func storageKey(scope: MordeClientDataScope, projectID: String?, conversationID: String, messageID: String) -> String {
        "morde.routingRetry.\(scope.messageOutboxKey).\(projectID ?? "personal").\(conversationID).\(messageID)"
    }
}

enum MordeOutboxState: String, Codable {
    case pending, sending, unknown, failed, accepted
}

enum MordeOutboxFailurePolicy {
    static func confirmsRejection(submissionBegan: Bool, firstSubmission: Bool, hadUncertainSubmission: Bool,
                                  domain: String, code: Int) -> Bool {
        submissionBegan && firstSubmission && !hadUncertainSubmission && domain == "Morde" && (400..<500).contains(code) &&
            ![401, 408, 409, 425, 429].contains(code)
    }

    static func state(mayHaveBeenAccepted: Bool, confirmedRejection: Bool,
                      domain: String, code: Int) -> MordeOutboxState {
        if mayHaveBeenAccepted && !confirmedRejection { return .unknown }
        if domain == "Morde" && (400..<500).contains(code) && code != 401 && code != 429 { return .failed }
        return .pending
    }
}

enum MordeCloudModelConfiguration {
    static func isConfirmedAbsent(status: String, loaded: Bool, checkedAt: Date?, now: Date = Date()) -> Bool {
        guard status == "not_configured", loaded, let checkedAt else { return false }
        let age = now.timeIntervalSince(checkedAt)
        return age >= 0 && age < 300
    }

    static func allowsSubmission(status: String, loaded: Bool, checkedAt: Date?, now: Date = Date()) -> Bool {
        !isConfirmedAbsent(status: status, loaded: loaded, checkedAt: checkedAt, now: now)
    }
}

struct MordeProjectAttachmentUploadStatus: Decodable {
    let attachment_id: String
    let name: String
    let media_type: String
    let size_bytes: Int64
    let sha256: String
    let state: String
    let total_chunks: Int
    let received_sequences: [Int]
    let replay: Bool
    var derivative_mode: String? = nil
    var derivative_state: String? = nil

    var missingSequences: [Int] {
        let received = Set(received_sequences)
        return (0..<max(0, total_chunks)).filter { !received.contains($0) }
    }

    func isValid(name expectedName: String, mediaType: String, size: Int64, digest: String,
                 existingID: String?, derivativeMode: String? = nil) -> Bool {
        guard MordeProjectAttachmentContract.isValidAttachmentID(attachment_id),
              existingID.map({ $0 == attachment_id }) ?? true,
              name == expectedName, media_type == mediaType, size_bytes == size, sha256 == digest,
              digest.range(of: "^[0-9a-f]{64}$", options: .regularExpression) != nil,
              let count = MordeProjectAttachmentContract.chunkCount(size: size), total_chunks == count,
              ["uploading", "committed", "expired"].contains(state),
              Set(received_sequences).count == received_sequences.count,
              received_sequences.allSatisfy({ $0 >= 0 && $0 < count }) else { return false }
        if let derivativeMode {
            if let actual = derivative_mode {
                guard actual == derivativeMode else { return false }
                if derivativeMode == "none" {
                    guard derivative_state == "none" else { return false }
                } else {
                    guard ["awaiting", "completed", "skipped"].contains(derivative_state ?? "") else { return false }
                }
            } else if derivativeMode != "none" {
                return false
            }
        }
        if state == "committed" { return received_sequences.count == count }
        if state == "expired" { return received_sequences.isEmpty }
        return true
    }
}

struct MordeProjectDerivativeFile: Codable, Equatable {
    let kind: String
    let page: Int
    let name: String
    let media_type: String
    let sha256: String
    let content_base64: String
}

struct MordeProjectDerivativeRequest: Codable, Equatable {
    let outcome: String
    let files: [MordeProjectDerivativeFile]

    func encoded(mode: String) -> Data? {
        guard ["pdf", "image"].contains(mode), ["completed", "skipped"].contains(outcome) else { return nil }
        if outcome == "skipped" {
            guard files.isEmpty else { return nil }
        } else {
            if mode == "pdf" {
                guard files.filter({ $0.kind == "extracted_text" && $0.page == 0 }).count == 1,
                      files.count <= 11,
                      files.allSatisfy({ ($0.kind == "extracted_text" && $0.page == 0) ||
                          ($0.kind == "page_preview" && (1...10).contains($0.page)) }) else { return nil }
            } else {
                guard files.count == 1, files[0].kind == "image_conversion", files[0].page == 0 else { return nil }
            }
            guard Set(files.map { "\($0.kind):\($0.page)" }).count == files.count else { return nil }
            for file in files {
                guard MordeProjectAttachmentContract.isValidName(file.name),
                      let data = Data(base64Encoded: file.content_base64),
                      MordeMessageChunking.digest(data) == file.sha256 else { return nil }
                if file.kind == "extracted_text" {
                    guard file.media_type == "text/plain", data.count <= 48 * 1024,
                          String(data: data, encoding: .utf8) != nil else { return nil }
                } else {
                    guard file.media_type == "image/jpeg", data.count <= 512 * 1024 else { return nil }
                }
            }
        }
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        guard let data = try? encoder.encode(self), data.count <= 8 * 1024 * 1024 else { return nil }
        return data
    }
}

enum MordeProjectAttachmentContract {
    static let chunkBytes = 4 * 1024 * 1024
    static let maxBytes: Int64 = 300 * 1024 * 1024

    static func isValidName(_ name: String) -> Bool {
        !name.isEmpty && name.utf8.count <= 255 && name != "." && name != ".." &&
            !name.contains("/") && !name.contains("\\") &&
            !name.unicodeScalars.contains(where: CharacterSet.controlCharacters.contains)
    }

    static func isValidAttachmentID(_ id: String) -> Bool {
        id.range(of: "^pca_[0-9a-f]{32}$", options: .regularExpression) != nil
    }

    static func chunkCount(size: Int64) -> Int? {
        guard size >= 0, size <= maxBytes else { return nil }
        return size == 0 ? 0 : Int((size - 1) / Int64(chunkBytes) + 1)
    }

    static func chunkRange(size: Int64, sequence: Int) -> Range<Int64>? {
        guard let count = chunkCount(size: size), sequence >= 0, sequence < count else { return nil }
        let offset = Int64(sequence) * Int64(chunkBytes)
        return offset..<min(size, offset + Int64(chunkBytes))
    }
}

enum MordePDFTextLimit {
    static func appendingNote(_ text: String, note: String, limit: Int) -> String {
        guard limit >= note.utf8.count else { return "" }
        let budget = limit - note.utf8.count
        var prefix = String.UnicodeScalarView()
        var count = 0
        for scalar in text.unicodeScalars {
            let length = String(scalar).utf8.count
            if count + length > budget { break }
            prefix.append(scalar)
            count += length
        }
        return String(prefix) + note
    }
}

enum MordeMessageChunking {
    static let directLimit = 32 * 1024
    static let chunkSize = 16 * 1024
    static let messageLimit = 8 * 1024 * 1024

    static func count(for data: Data) -> Int {
        (data.count + chunkSize - 1) / chunkSize
    }

    static func chunk(_ data: Data, sequence: Int) -> Data? {
        guard sequence >= 0, sequence < count(for: data) else { return nil }
        let start = sequence * chunkSize
        return data.subdata(in: start..<min(start + chunkSize, data.count))
    }

    static func digest(_ data: Data) -> String {
        SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
    }
}

struct MordeOutboxAttachment: Codable, Equatable, Identifiable, Sendable {
    let id: String
    var attachmentID: String?
    let localPath: String?
    var linked: Bool
    var sourceBookmark: Data?
    var sourcePath: String?
    var derivedLocalPaths: [String]?
    var derivedAssociationPending: Bool?
    var projectDerivativeMode: String?
    var projectDerivativePayloadPath: String?
    var projectDerivativePayloadDigest: String?
    var originalFilename: String?

    init(id: String, attachmentID: String?, localPath: String? = nil, linked: Bool,
         sourceBookmark: Data? = nil, sourcePath: String? = nil,
         derivedLocalPaths: [String]? = nil, derivedAssociationPending: Bool? = nil,
         projectDerivativeMode: String? = nil, projectDerivativePayloadPath: String? = nil,
         projectDerivativePayloadDigest: String? = nil, originalFilename: String? = nil) {
        self.id = id
        self.attachmentID = attachmentID
        self.localPath = localPath
        self.linked = linked
        self.sourceBookmark = sourceBookmark
        self.sourcePath = sourcePath
        self.derivedLocalPaths = derivedLocalPaths
        self.derivedAssociationPending = derivedAssociationPending
        self.projectDerivativeMode = projectDerivativeMode
        self.projectDerivativePayloadPath = projectDerivativePayloadPath
        self.projectDerivativePayloadDigest = projectDerivativePayloadDigest
        self.originalFilename = originalFilename
    }
}

struct MordeOutboxMessage: Codable, Equatable, Identifiable {
    let id: String
    let scope: MordeClientDataScope
    var conversationID: String
    let projectID: String?
    var previousClientMessageID: String?
    let content: String
    let modelID: String?
    let referenceTaskID: String?
    let hostID: String?
    let workspaceID: String?
    let createdAt: String
    var attachments: [MordeOutboxAttachment]
    var awaitingAttachmentBatch: Bool?
    var pendingUploadPaths: [String]?
    var preparedBatchAttachmentIDs: [String]?
    var serverMessageID: String?
    var state: MordeOutboxState
    var error: String?
    var submissionStarted: Bool? = nil
    var submissionUncertain: Bool? = nil
    var confirmedNotAccepted: Bool? = nil
    var bypassedPredecessorIDs: [String]? = nil

    var statusLocalizationKey: String {
        if awaitingAttachmentBatch == true {
            return state == .failed ? "Attachments need attention" : "Waiting for attachments"
        }
        if state == .accepted && attachments.contains(where: { !$0.linked || $0.derivedAssociationPending == true }) {
            return error == nil
                ? "Sent; uploading attachments" : "Sent; attachments need attention"
        }
        switch state {
        case .pending: return "Waiting to send"
        case .sending: return "Sending"
        case .unknown: return "Send status unknown"
        case .failed: return "Send failed"
        case .accepted: return "Sent"
        }
    }
}

enum MordeOutboxScheduling {
    private struct Conversation: Hashable {
        let projectID: String?
        let conversationID: String
    }

    private struct Dependency: Hashable {
        let conversation: Conversation
        let predecessorID: String
    }

    private static func conversation(_ message: MordeOutboxMessage) -> Conversation {
        Conversation(projectID: message.projectID, conversationID: message.conversationID)
    }

    private static func dependencies(_ messages: [MordeOutboxMessage]) -> Set<Dependency> {
        Set(messages.compactMap { message in
            guard message.serverMessageID == nil, let previous = message.previousClientMessageID else { return nil }
            return Dependency(conversation: conversation(message), predecessorID: previous)
        })
    }

    private static func independentRetryBlocks(_ messages: [MordeOutboxMessage]) -> Set<Dependency> {
        var blocked = Set<Dependency>()
        for message in messages where message.serverMessageID == nil {
            for skipped in message.bypassedPredecessorIDs ?? [] {
                blocked.insert(Dependency(conversation: conversation(message), predecessorID: skipped))
            }
        }
        return blocked
    }

    static func bypassablePredecessor(of message: MordeOutboxMessage, in messages: [MordeOutboxMessage],
                                      inFlight: Set<String>) -> MordeOutboxMessage? {
        guard message.state == .pending, message.serverMessageID == nil, message.submissionStarted == false,
              message.submissionUncertain == false,
              !inFlight.contains(message.id), let previousID = message.previousClientMessageID,
              !inFlight.contains(previousID),
              let index = messages.firstIndex(where: { $0.id == message.id }),
              let previous = messages[..<index].first(where: { $0.id == previousID }),
              previous.scope.hasSameAccount(as: message.scope), conversation(previous) == conversation(message),
              previous.state == .failed, previous.serverMessageID == nil,
              previous.confirmedNotAccepted == true, previous.submissionUncertain == false else { return nil }
        return previous
    }

    static func readyIDs(_ messages: [MordeOutboxMessage], scope: MordeClientDataScope,
                         inFlight: Set<String>, allowsNewSubmissions: Bool = true) -> [String] {
        let scoped = messages.filter { $0.scope.hasSameAccount(as: scope) }
        let required = dependencies(scoped)
        let independentBlocks = independentRetryBlocks(scoped)
        let occupied = Set(scoped.filter { $0.serverMessageID == nil && inFlight.contains($0.id) }.map(conversation))
        var awaitingReceipt = Set<Conversation>()
        var ready: [String] = []
        for message in scoped {
            let conversation = conversation(message)
            // nanoMuse: Unknown prior deliveries still need reconciliation; callers may hold untouched sends only on confirmed absence.
            guard allowsNewSubmissions || message.serverMessageID != nil || message.state != .pending ||
                    message.submissionStarted != false else { continue }
            if message.serverMessageID == nil {
                if independentBlocks.contains(Dependency(conversation: conversation, predecessorID: message.id)) { continue }
                if message.state == .failed && !required.contains(Dependency(conversation: conversation, predecessorID: message.id)) {
                    continue
                }
                if !awaitingReceipt.insert(conversation).inserted || occupied.contains(conversation) { continue }
            }
            guard message.state != .failed, !inFlight.contains(message.id) else { continue }
            ready.append(message.id)
        }
        return ready
    }

    static func waitingForEarlierMessage(_ message: MordeOutboxMessage,
                                         in messages: [MordeOutboxMessage]) -> String? {
        guard message.serverMessageID == nil, let index = messages.firstIndex(where: { $0.id == message.id }) else { return nil }
        let scoped = messages.filter { $0.scope.hasSameAccount(as: message.scope) }
        if independentRetryBlocks(scoped).contains(Dependency(conversation: conversation(message), predecessorID: message.id)) {
            return "Retry will wait until the independent message is received."
        }
        let required = dependencies(scoped)
        guard let earlier = messages[..<index].first(where: {
            $0.scope.hasSameAccount(as: message.scope) && conversation($0) == conversation(message) &&
                $0.serverMessageID == nil &&
                ($0.state != .failed || required.contains(Dependency(conversation: conversation($0), predecessorID: $0.id)))
        }) else { return nil }
        if earlier.state == .unknown {
            return "The previous message’s delivery could not be verified. Retry or check it first."
        }
        if earlier.state == .failed {
            return earlier.confirmedNotAccepted == true && earlier.submissionUncertain == false
                ? "The previous message failed to send. Please handle it first."
                : "The previous message’s delivery could not be verified. Retry or check it first."
        }
        return "Waiting for the previous message to be received."
    }
}

enum MordeConversationMessagePayload {
    private static func routingFields(_ message: MordeOutboxMessage) -> [String: Any] {
        var fields: [String: Any] = [:]
        if let previous = message.previousClientMessageID { fields["previous_client_message_id"] = previous }
        if let model = message.modelID { fields["model_id"] = model }
        if let target = message.referenceTaskID { fields["reference_task_id"] = target }
        if let host = message.hostID { fields["host_id"] = host }
        if let workspace = message.workspaceID { fields["workspace_id"] = workspace }
        if !message.attachments.isEmpty { fields["client_attachment_ids"] = message.attachments.map(\.id) }
        return fields
    }

    static func direct(_ message: MordeOutboxMessage) -> [String: Any] {
        var fields = routingFields(message)
        fields["client_message_id"] = message.id
        fields["content"] = message.content
        return fields
    }

    static func uploadInitialization(_ message: MordeOutboxMessage, bytes: Data) -> [String: Any] {
        var fields = routingFields(message)
        fields["total_bytes"] = bytes.count
        fields["total_chunks"] = MordeMessageChunking.count(for: bytes)
        fields["sha256"] = MordeMessageChunking.digest(bytes)
        return fields
    }
}

enum MordeOfflineProjectQueue {
    static func messages(_ values: [MordeOutboxMessage], scope: MordeClientDataScope) -> [MordeOutboxMessage] {
        values.filter { $0.projectID != nil && $0.scope.hasSameAccount(as: scope) }
    }
}

/// A message written before its account can be verified, or with attachments that need repair.
/// These records are never read by the network outbox and can only be recovered explicitly.
struct MordeUnconfirmedMessage: Codable, Equatable, Identifiable {
    let id: String
    let serviceOrigin: String
    let serviceInstanceID: String?
    let accountName: String
    let accountID: String?
    var installationID: String? = nil
    let conversationID: String?
    let projectID: String?
    let content: String
    let modelID: String?
    let referenceTaskID: String?
    let hostID: String?
    let workspaceID: String?
    let attachmentIDs: [String]
    let stagedAttachments: [MordeOutboxAttachment]
    let pendingUploadPaths: [String]
    let createdAt: String
    let reason: String
    var localAttachmentStatus: String? = nil

    func belongsTo(origin: String?, accountName: String?, accountID: String?) -> Bool {
        guard let origin, serviceOrigin == origin else { return false }
        // A known account ID must never become visible to a different account with the same name.
        if let expected = self.accountID { return expected == accountID }
        guard let accountName, !self.accountName.isEmpty else { return false }
        return self.accountName == accountName
    }

    func matchesInstance(_ instanceID: String?) -> Bool {
        serviceInstanceID != nil && serviceInstanceID == instanceID
    }

    func matchesInstallation(_ activeInstallationID: String?) -> Bool {
        accountID != nil || (installationID != nil && installationID == activeInstallationID)
    }
}

enum MordeMessageAttachmentValidation {
    static func isValid(attachmentIDs: [String], stagedAttachments: [MordeOutboxAttachment], projectID: String?,
                        personalAttachmentIsValid: (String) -> Bool) -> Bool {
        (projectID == nil || attachmentIDs.isEmpty) && attachmentIDs.count + stagedAttachments.count <= 16 &&
            Set(attachmentIDs).count == attachmentIDs.count &&
            attachmentIDs.allSatisfy(personalAttachmentIsValid) &&
            Set(stagedAttachments.map(\.id)).count == stagedAttachments.count &&
            stagedAttachments.allSatisfy {
                $0.id.range(of: "^[A-Za-z0-9._~-]{8,128}$", options: .regularExpression) != nil
            }
    }
}

final class MordeUnconfirmedMessageStore {
    private static let lock = NSLock()
    private let root: URL
    private let files: FileManager
    init(root: URL, files: FileManager = .default) { self.root = root; self.files = files }

    func messages() throws -> [MordeUnconfirmedMessage] {
        Self.lock.lock(); defer { Self.lock.unlock() }
        return try read()
    }

    func append(_ message: MordeUnconfirmedMessage) throws {
        Self.lock.lock(); defer { Self.lock.unlock() }
        var values = try read()
        guard !values.contains(where: { $0.id == message.id }) else { throw CocoaError(.fileWriteFileExists) }
        values.append(message)
        try write(values)
    }

    func remove(id: String) throws {
        Self.lock.lock(); defer { Self.lock.unlock() }
        var values = try read()
        values.removeAll { $0.id == id }
        try write(values)
    }

    func updateAttachmentStatus(id: String, status: String) throws -> [MordeUnconfirmedMessage] {
        Self.lock.lock(); defer { Self.lock.unlock() }
        var values = try read()
        guard let index = values.firstIndex(where: { $0.id == id }) else { return values }
        values[index].localAttachmentStatus = status
        try write(values)
        return values
    }

    private var location: URL { root.appendingPathComponent("unconfirmed-messages.json") }
    private func read() throws -> [MordeUnconfirmedMessage] {
        guard files.fileExists(atPath: location.path) else { return [] }
        return try JSONDecoder().decode([MordeUnconfirmedMessage].self, from: Data(contentsOf: location))
    }
    private func write(_ values: [MordeUnconfirmedMessage]) throws {
        try files.createDirectory(at: root, withIntermediateDirectories: true)
#if os(iOS)
        try files.setAttributes([.protectionKey: FileProtectionType.completeUntilFirstUserAuthentication],
                                ofItemAtPath: root.path)
        try JSONEncoder().encode(values).write(to: location, options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
#else
        try JSONEncoder().encode(values).write(to: location, options: .atomic)
#endif
    }
}

private struct MordeOutboxSnapshot: Codable {
    var messages: [MordeOutboxMessage] = []
    var lastMessageIDs: [String: String] = [:]
    var resolvedConversations: [String: String]? = nil
    var routingRetryAttempts: [String: String]? = nil
}

/// Each account's messages are written atomically before the composer is cleared.
final class MordeMessageOutbox {
    private static let lock = NSLock()
    private let root: URL
    private let files: FileManager

    init(root: URL, files: FileManager = .default) {
        self.root = root
        self.files = files
    }

    func messages(scope: MordeClientDataScope) throws -> [MordeOutboxMessage] {
        Self.lock.lock()
        defer { Self.lock.unlock() }
        return try read(scope: scope).messages
    }

    // nanoMuse: A process restart invalidates in-flight UI work; reconcile old sends by their original client message IDs.
    func recoverInterruptedMessages(scope: MordeClientDataScope,
                                    excludingInFlight: Set<String> = []) throws -> [MordeOutboxMessage] {
        Self.lock.lock()
        defer { Self.lock.unlock() }
        var snapshot = try read(scope: scope)
        var changed = false
        for index in snapshot.messages.indices where snapshot.messages[index].state == .sending &&
                !excludingInFlight.contains(snapshot.messages[index].id) {
            snapshot.messages[index].state = snapshot.messages[index].serverMessageID == nil ? .unknown : .accepted
            changed = true
        }
        if changed { try write(snapshot, scope: scope) }
        return snapshot.messages
    }

    func resolvedConversationID(_ id: String, scope: MordeClientDataScope) throws -> String {
        Self.lock.lock()
        defer { Self.lock.unlock() }
        return try read(scope: scope).resolvedConversations?[id] ?? id
    }

    func beginRoutingRetry(storageKey: String, scope: MordeClientDataScope) throws -> String {
        Self.lock.lock()
        defer { Self.lock.unlock() }
        var snapshot = try read(scope: scope)
        if let existing = snapshot.routingRetryAttempts?[storageKey] { return existing }
        let attempt = UUID().uuidString.lowercased()
        var attempts = snapshot.routingRetryAttempts ?? [:]
        attempts[storageKey] = attempt
        snapshot.routingRetryAttempts = attempts
        try write(snapshot, scope: scope)
        return attempt
    }

    func finishRoutingRetry(storageKey: String, attempt: String, scope: MordeClientDataScope) throws {
        Self.lock.lock()
        defer { Self.lock.unlock() }
        var snapshot = try read(scope: scope)
        guard snapshot.routingRetryAttempts?[storageKey] == attempt else { return }
        snapshot.routingRetryAttempts?.removeValue(forKey: storageKey)
        try write(snapshot, scope: scope)
    }

    func resolveLocalConversation(_ localID: String, serverID: String,
                                  scope: MordeClientDataScope) throws {
        Self.lock.lock()
        defer { Self.lock.unlock() }
        var snapshot = try read(scope: scope)
        if let existing = snapshot.resolvedConversations?[localID], existing != serverID {
            throw CocoaError(.fileWriteInvalidFileName)
        }
        var mappings = snapshot.resolvedConversations ?? [:]
        mappings[localID] = serverID
        snapshot.resolvedConversations = mappings
        for index in snapshot.messages.indices where snapshot.messages[index].conversationID == localID {
            snapshot.messages[index].conversationID = serverID
        }
        if let previous = snapshot.lastMessageIDs.removeValue(forKey: localID) {
            snapshot.lastMessageIDs[serverID] = previous
        }
        try write(snapshot, scope: scope)
    }

    func enqueue(scope: MordeClientDataScope, conversationID: String, content: String,
                 modelID: String?, attachmentIDs: [String], projectID: String? = nil,
                 referenceTaskID: String? = nil, hostID: String? = nil,
                 workspaceID: String? = nil, awaitingAttachmentBatch: Bool = false,
                 stagedAttachments: [MordeOutboxAttachment] = [], pendingUploadPaths: [String] = []) throws -> MordeOutboxMessage {
        Self.lock.lock()
        defer { Self.lock.unlock() }
        var snapshot = try read(scope: scope)
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        let message = MordeOutboxMessage(
            id: UUID().uuidString.lowercased(), scope: scope, conversationID: conversationID, projectID: projectID,
            previousClientMessageID: snapshot.lastMessageIDs[conversationID], content: content,
            modelID: modelID, referenceTaskID: referenceTaskID, hostID: hostID, workspaceID: workspaceID,
            createdAt: formatter.string(from: Date()),
            attachments: attachmentIDs.map {
                MordeOutboxAttachment(id: UUID().uuidString.lowercased(), attachmentID: $0, linked: false)
            } + stagedAttachments, awaitingAttachmentBatch: awaitingAttachmentBatch,
            pendingUploadPaths: pendingUploadPaths, preparedBatchAttachmentIDs: nil,
            serverMessageID: nil, state: .pending, error: nil,
            submissionStarted: false, submissionUncertain: false, confirmedNotAccepted: false)
        snapshot.messages.append(message)
        snapshot.lastMessageIDs[conversationID] = message.id
        try write(snapshot, scope: scope)
        return message
    }

    func replace(_ message: MordeOutboxMessage, scope: MordeClientDataScope) throws {
        Self.lock.lock()
        defer { Self.lock.unlock() }
        guard message.scope.hasSameAccount(as: scope) else { throw CocoaError(.fileWriteNoPermission) }
        var snapshot = try read(scope: scope)
        guard let index = snapshot.messages.firstIndex(where: { $0.id == message.id }) else {
            throw CocoaError(.fileNoSuchFile)
        }
        snapshot.messages[index] = message
        try write(snapshot, scope: scope)
    }

    func detachFailedPredecessor(messageID: String, predecessorID: String,
                                 scope: MordeClientDataScope) throws -> Bool {
        Self.lock.lock()
        defer { Self.lock.unlock() }
        var snapshot = try read(scope: scope)
        guard let index = snapshot.messages.firstIndex(where: { $0.id == messageID }),
              let previous = MordeOutboxScheduling.bypassablePredecessor(of: snapshot.messages[index],
                  in: snapshot.messages, inFlight: []), previous.id == predecessorID else { return false }
        snapshot.messages[index].previousClientMessageID = previous.previousClientMessageID
        var skipped = snapshot.messages[index].bypassedPredecessorIDs ?? []
        if !skipped.contains(previous.id) { skipped.append(previous.id) }
        snapshot.messages[index].bypassedPredecessorIDs = skipped
        try write(snapshot, scope: scope)
        return true
    }

    @discardableResult
    func update(id: String, scope: MordeClientDataScope,
                _ change: (inout MordeOutboxMessage) -> Void) throws -> MordeOutboxMessage {
        Self.lock.lock()
        defer { Self.lock.unlock() }
        var snapshot = try read(scope: scope)
        guard let index = snapshot.messages.firstIndex(where: { $0.id == id }) else {
            throw CocoaError(.fileNoSuchFile)
        }
        change(&snapshot.messages[index])
        let message = snapshot.messages[index]
        try write(snapshot, scope: scope)
        return message
    }

    func remove(id: String, scope: MordeClientDataScope) throws {
        Self.lock.lock()
        defer { Self.lock.unlock() }
        var snapshot = try read(scope: scope)
        snapshot.messages.removeAll { $0.id == id }
        try write(snapshot, scope: scope)
    }

    private func location(scope: MordeClientDataScope) -> URL {
        root.appendingPathComponent("\(scope.messageOutboxKey).json")
    }

    private func read(scope: MordeClientDataScope) throws -> MordeOutboxSnapshot {
        let url = location(scope: scope)
        guard files.fileExists(atPath: url.path) else { return MordeOutboxSnapshot() }
        let snapshot = try JSONDecoder().decode(MordeOutboxSnapshot.self, from: Data(contentsOf: url))
        guard snapshot.messages.allSatisfy({ $0.scope.hasSameAccount(as: scope) }) else {
            throw CocoaError(.fileReadNoPermission)
        }
        return snapshot
    }

    private func write(_ snapshot: MordeOutboxSnapshot, scope: MordeClientDataScope) throws {
        try files.createDirectory(at: root, withIntermediateDirectories: true)
#if os(iOS)
        try files.setAttributes([.protectionKey: FileProtectionType.completeUntilFirstUserAuthentication],
                                ofItemAtPath: root.path)
#endif
        let url = location(scope: scope)
        let data = try JSONEncoder().encode(snapshot)
#if os(iOS)
        try data.write(to: url, options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
#else
        try data.write(to: url, options: .atomic)
#endif
    }
}

struct MordeRevocationRecord: Codable, Equatable {
    let scope: MordeClientDataScope
    let clientInstanceID: UUID?
    let confirmed: Bool
}

private struct MordeRevocationSnapshot: Codable {
    var records: [String: MordeRevocationRecord] = [:]
}

final class MordeRevocationStore {
    static let defaultsKey = "morde.pendingRevocations.v1"
    private static let lock = NSLock()
    private let defaults: UserDefaults

    init(defaults: UserDefaults = .standard) { self.defaults = defaults }

    func record(_ revocation: MordeRevocationRecord) throws {
        Self.lock.lock()
        defer { Self.lock.unlock() }
        var snapshot = loadSnapshot()
        snapshot.records[revocation.scope.storageKey] = revocation
        defaults.set(try JSONEncoder().encode(snapshot), forKey: Self.defaultsKey)
    }

    func confirm(_ revocation: MordeRevocationRecord) throws {
        try record(MordeRevocationRecord(scope: revocation.scope,
                                         clientInstanceID: revocation.clientInstanceID,
                                         confirmed: true))
    }

    func pending() -> [MordeRevocationRecord] {
        Self.lock.lock()
        defer { Self.lock.unlock() }
        return loadSnapshot().records.values.filter { !$0.confirmed }
    }

    private func loadSnapshot() -> MordeRevocationSnapshot {
        guard let data = defaults.data(forKey: Self.defaultsKey),
              let snapshot = try? JSONDecoder().decode(MordeRevocationSnapshot.self, from: data) else {
            return MordeRevocationSnapshot()
        }
        return snapshot
    }
}

enum MordeRevocationRetryPolicy {
    static func candidates(_ pending: [MordeRevocationRecord], currentScope: MordeClientDataScope,
                           currentInstallationID: String?, accountName: String) -> [MordeRevocationRecord] {
        pending.filter { record in
            !record.confirmed &&
                record.scope.serviceOrigin == currentScope.serviceOrigin &&
                record.scope.serviceInstanceID == currentScope.serviceInstanceID &&
                (record.scope.accountID == currentScope.accountID || record.scope.accountID == accountName) &&
                MordeClientInstallationPolicy.canRevoke(
                    record.scope.installationID, currentInstallationID: currentInstallationID)
        }
    }
}

enum MordeCredentialExpiryStatus: Equatable {
    case expired
    case expiringSoon
    case valid
}

enum MordeCredentialExpiry {
    static func parse(_ raw: String) -> Date? {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let date = formatter.date(from: raw) { return date }
        formatter.formatOptions = [.withInternetDateTime]
        return formatter.date(from: raw)
    }

    static func status(expiresAt raw: String, now: Date = Date()) -> MordeCredentialExpiryStatus? {
        guard let expiry = parse(raw) else { return nil }
        if expiry <= now { return .expired }
        if expiry <= now.addingTimeInterval(7 * 24 * 60 * 60) { return .expiringSoon }
        return .valid
    }

    static func validCredential(_ credential: String?, expiresAt: String?, now: Date = Date()) -> String? {
        guard let credential, !credential.isEmpty, let expiresAt,
              let status = status(expiresAt: expiresAt, now: now), status != .expired else { return nil }
        return credential
    }

    static func secondsUntilExpiry(expiresAt raw: String?, now: Date = Date()) -> TimeInterval? {
        guard let raw, let expiry = parse(raw) else { return nil }
        return max(0, expiry.timeIntervalSince(now))
    }
}
