import Foundation

private func fixtureObject(_ value: Any?, label: String) -> [String: Any] {
    guard let object = value as? [String: Any] else { fatalError("Missing fixture object: \(label)") }
    return object
}

private func fixtureString(_ value: Any?, label: String) -> String {
    guard let string = value as? String else { fatalError("Missing fixture string: \(label)") }
    return string
}

private func fixtureInt(_ value: Any?, label: String) -> Int {
    guard let integer = value as? Int else { fatalError("Missing fixture integer: \(label)") }
    return integer
}

private func parseSSEFixture(_ raw: String) -> (id: String, event: String, data: [String: Any]) {
    var id: String?
    var event: String?
    var payload: [String: Any]?
    for line in raw.components(separatedBy: .newlines) where !line.isEmpty {
        if line.hasPrefix("id:") {
            id = line.dropFirst(3).trimmingCharacters(in: .whitespaces)
        } else if line.hasPrefix("event:") {
            event = line.dropFirst(6).trimmingCharacters(in: .whitespaces)
        } else if line.hasPrefix("data:") {
            let json = line.dropFirst(5).trimmingCharacters(in: .whitespaces)
            guard let data = json.data(using: .utf8),
                  let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else {
                fatalError("Invalid SSE fixture data")
            }
            payload = object
        }
    }
    guard let id, let event, let payload else { fatalError("Incomplete SSE fixture frame") }
    return (id, event, payload)
}

@main
enum MordeCredentialExpiryTests {
    static func main() {
        let registration = MordeRegistrationPayload.make(name: "  alice  ", password: "password123")
        precondition(registration?.jsonObject == ["name": "alice", "password": "password123"])
        precondition(MordeRegistrationPayload.make(name: "  ", password: "password123") == nil)
        precondition(MordeRegistrationPayload.make(name: "alice", password: "") == nil)
        precondition(MordeLoginStateStore.serviceOrigin("https://MORDE.example:443") == "https://morde.example")
#if DEBUG
        precondition(MordeLoginStateStore.serviceOrigin("http://localhost:7080") == "http://localhost:7080")
        precondition(MordeLoginStateStore.serviceOrigin("http://127.0.0.1:7080") == "http://127.0.0.1:7080")
        precondition(MordeLoginStateStore.serviceOrigin("http://[::1]:7080") == "http://[::1]:7080")
        precondition(MordeLoginStateStore.serviceOrigin("http://localhost.example:7080") == nil)
#else
        precondition(MordeLoginStateStore.serviceOrigin("http://localhost:7080") == nil)
#endif
        precondition(!MordeFirstRunPolicy.isNeeded(
            signedIn: false, hasProviders: false, hasSessions: false, done: false, mordeSignedIn: true))
        precondition(MordeFirstRunPolicy.isNeeded(
            signedIn: false, hasProviders: false, hasSessions: false, done: false, mordeSignedIn: false))
        precondition(MordeFirstRunPolicy.isNeeded(
            signedIn: true, hasProviders: false, hasSessions: false, done: false, mordeSignedIn: false))
        precondition(!MordeFirstRunPolicy.isNeeded(
            signedIn: true, hasProviders: false, hasSessions: true, done: false, mordeSignedIn: false))
        precondition(!MordeFirstRunPolicy.isNeeded(
            signedIn: true, hasProviders: false, hasSessions: false, done: true, mordeSignedIn: false))
        precondition(!MordeFirstRunPolicy.isNeeded(
            signedIn: false, hasProviders: true, hasSessions: true, done: false, mordeSignedIn: false))
        precondition(!MordeLocalExecutionPolicy.isEnabled)
        precondition(!MordeLocalExecutionPolicy.disabledMessage.isEmpty)
        precondition(MordeLocalExecutionDisabledError().errorDescription == MordeLocalExecutionPolicy.disabledMessage)
        precondition(MordeTaskStepPresentation.titleLocalizationKey("Run cloud dsh tool: morde_web_search") == "Search the web")
        precondition(MordeTaskStepPresentation.titleLocalizationKey("Run cloud dsh tool: morde_web_read") == "Read a web page")
        precondition(MordeTaskStepPresentation.titleLocalizationKey("Run Hostless Workspace tool: morde_workspace_write") == "Update a workspace file")
        precondition(MordeTaskStepPresentation.titleLocalizationKey("Run cloud dsh tool: desktop_click") == "Use the cloud computer")
        precondition(MordeTaskStepPresentation.titleLocalizationKey("Run cloud dsh tool: an_unknown_tool") == "Process the task")
        precondition(MordeTaskStepPresentation.titleLocalizationKey("A user-facing step") == nil)
        precondition(MordeTaskStepPresentation.summary("Checking the task checkpoint and Workspace revision.",
            title: "Run Hostless Workspace tool: morde_workspace_read") == nil)
        precondition(MordeTaskStepPresentation.summary("The file could not be read.",
            title: "Run Hostless Workspace tool: morde_workspace_read") == "The file could not be read.")
        precondition(MordeTaskStepPresentation.isStartedProgress(kind: "progress", title: "开始处理", status: "succeeded"))
        precondition(!MordeTaskStepPresentation.isStartedProgress(kind: "progress", title: "开始处理", status: "failed"))
        precondition(MordeTaskStartPresentation.isWaiting(status: "running", currentRevision: 1, desiredRevision: 1, currentRevisionStarted: false))
        precondition(MordeTaskStartPresentation.isWaiting(status: "running", currentRevision: 1, desiredRevision: 2, currentRevisionStarted: true))
        precondition(!MordeTaskStartPresentation.isWaiting(status: "running", currentRevision: 2, desiredRevision: 2, currentRevisionStarted: true))
        precondition(!MordeTaskStartPresentation.isWaiting(status: "running", currentRevision: 1, desiredRevision: 2, currentRevisionStarted: nil))
        precondition(!MordeTaskStartPresentation.isWaiting(status: "failed", currentRevision: 1, desiredRevision: 2, currentRevisionStarted: false))
        let projectAttachmentID = "pca_" + String(repeating: "a", count: 32)
        precondition(MordeProjectAttachmentContract.isValidAttachmentID(projectAttachmentID))
        let chineseFilename = String(repeating: "文", count: 85)
        precondition(chineseFilename.utf8.count == 255 && MordeProjectAttachmentContract.isValidName(chineseFilename))
        precondition(!MordeProjectAttachmentContract.isValidName(chineseFilename + "a"))
        precondition(!MordeProjectAttachmentContract.isValidAttachmentID("pa_" + String(repeating: "a", count: 32)))
        precondition(MordeProjectAttachmentContract.chunkCount(size: 0) == 0)
        precondition(MordeProjectAttachmentContract.chunkCount(size: Int64(MordeProjectAttachmentContract.chunkBytes) + 1) == 2)
        precondition(MordeProjectAttachmentContract.chunkCount(size: MordeProjectAttachmentContract.maxBytes + 1) == nil)
        precondition(MordeProjectAttachmentContract.chunkRange(size: 0, sequence: 0) == nil)
        precondition(MordeProjectAttachmentContract.chunkRange(size: 1, sequence: -1) == nil)
        let localText = Data("Synthetic local PDF text".utf8)
        let boundedText = MordePDFTextLimit.appendingNote(String(repeating: "😀", count: 20_000),
                                                          note: "\n[truncated]", limit: 48 * 1024)
        precondition(boundedText.utf8.count <= 48 * 1024 && boundedText.hasSuffix("\n[truncated]"))
        precondition(String(data: Data(boundedText.utf8), encoding: .utf8) == boundedText)
        precondition(MordePDFTextLimit.appendingNote("x", note: "long note", limit: 2).isEmpty)
        let textDerivative = MordeProjectDerivativeFile(kind: "extracted_text", page: 0, name: "text.txt",
            media_type: "text/plain", sha256: MordeMessageChunking.digest(localText),
            content_base64: localText.base64EncodedString())
        let localImage = Data(repeating: 42, count: 512 * 1024)
        let imageDigest = MordeMessageChunking.digest(localImage)
        let previews = (1...10).map { page in
            MordeProjectDerivativeFile(kind: "page_preview", page: page, name: "page-\(page).jpg",
                media_type: "image/jpeg", sha256: imageDigest, content_base64: localImage.base64EncodedString())
        }
        let derivativeRequest = MordeProjectDerivativeRequest(outcome: "completed", files: [textDerivative] + previews)
        let derivativePayload = derivativeRequest.encoded(mode: "pdf")!
        precondition(derivativePayload.count <= 8 * 1024 * 1024)
        precondition(try! JSONDecoder().decode(MordeProjectDerivativeRequest.self, from: derivativePayload) == derivativeRequest)
        precondition(derivativeRequest.encoded(mode: "pdf") == derivativePayload)
        precondition(MordeProjectDerivativeRequest(outcome: "completed", files: [textDerivative, previews[0], previews[0]])
            .encoded(mode: "pdf") == nil)
        precondition(MordeProjectDerivativeRequest(outcome: "skipped", files: []).encoded(mode: "pdf") != nil)
        precondition(MordeProjectDerivativeRequest(outcome: "skipped", files: [textDerivative]).encoded(mode: "pdf") == nil)
        precondition(MordeProjectDerivativeRequest(outcome: "completed", files: []).encoded(mode: "pdf") == nil)
        let wrongDigest = MordeProjectDerivativeFile(kind: "image_conversion", page: 0, name: "image.jpg",
            media_type: "image/jpeg", sha256: String(repeating: "0", count: 64),
            content_base64: localImage.base64EncodedString())
        precondition(MordeProjectDerivativeRequest(outcome: "completed", files: [wrongDigest]).encoded(mode: "image") == nil)
        let validImage = MordeProjectDerivativeFile(kind: "image_conversion", page: 0, name: "image.jpg",
            media_type: "image/jpeg", sha256: imageDigest, content_base64: localImage.base64EncodedString())
        precondition(MordeProjectDerivativeRequest(outcome: "completed", files: [validImage]).encoded(mode: "image") != nil)
        var syntheticBytes = Data(repeating: 0x61, count: MordeProjectAttachmentContract.chunkBytes + 7)
        syntheticBytes.replaceSubrange(MordeProjectAttachmentContract.chunkBytes..<syntheticBytes.count,
                                       with: Data("testing".utf8))
        let attachmentDigest = MordeMessageChunking.digest(syntheticBytes)
        let ranges = (0..<2).map { MordeProjectAttachmentContract.chunkRange(size: Int64(syntheticBytes.count), sequence: $0)! }
        let inMemoryChunks = ranges.map { syntheticBytes.subdata(in: Int($0.lowerBound)..<Int($0.upperBound)) }
        precondition(inMemoryChunks.map(\.count) == [MordeProjectAttachmentContract.chunkBytes, 7])
        precondition(inMemoryChunks.reduce(into: Data()) { $0.append($1) } == syntheticBytes)
        precondition(MordeMessageChunking.digest(inMemoryChunks[1]) == MordeMessageChunking.digest(Data("testing".utf8)))
        let partialUpload = MordeProjectAttachmentUploadStatus(attachment_id: projectAttachmentID, name: "local.pdf",
            media_type: "application/pdf", size_bytes: Int64(syntheticBytes.count),
            sha256: attachmentDigest, state: "uploading", total_chunks: 2, received_sequences: [1], replay: true)
        precondition(partialUpload.isValid(name: "local.pdf", mediaType: "application/pdf", size: partialUpload.size_bytes,
                                           digest: attachmentDigest, existingID: projectAttachmentID))
        precondition(partialUpload.missingSequences == [0])
        precondition(!partialUpload.isValid(name: "local.pdf", mediaType: "application/pdf", size: partialUpload.size_bytes,
                                            digest: attachmentDigest, existingID: projectAttachmentID, derivativeMode: "pdf"))
        var derivativesUpload = partialUpload
        derivativesUpload.derivative_mode = "pdf"
        derivativesUpload.derivative_state = "awaiting"
        precondition(derivativesUpload.isValid(name: "local.pdf", mediaType: "application/pdf", size: partialUpload.size_bytes,
                                               digest: attachmentDigest, existingID: projectAttachmentID, derivativeMode: "pdf"))
        precondition(!derivativesUpload.isValid(name: "local.pdf", mediaType: "application/pdf", size: partialUpload.size_bytes,
                                                digest: attachmentDigest, existingID: projectAttachmentID, derivativeMode: "image"))
        precondition(!derivativesUpload.isValid(name: "local.pdf", mediaType: "application/pdf", size: partialUpload.size_bytes,
                                                digest: attachmentDigest, existingID: projectAttachmentID, derivativeMode: "none"))
        let derivativeStatusJSON: [String: Any] = ["attachment_id": projectAttachmentID, "name": "local.pdf",
            "media_type": "application/pdf", "size_bytes": partialUpload.size_bytes, "sha256": attachmentDigest,
            "state": "uploading", "total_chunks": 2, "received_sequences": [1], "replay": true,
            "derivative_mode": "pdf", "derivative_state": "awaiting"]
        let decodedStatus = try! JSONDecoder().decode(MordeProjectAttachmentUploadStatus.self,
            from: JSONSerialization.data(withJSONObject: derivativeStatusJSON))
        precondition(decodedStatus.derivative_mode == "pdf" && decodedStatus.derivative_state == "awaiting")
        var simulatedRemoteChunks = [1: inMemoryChunks[1]]
        for sequence in partialUpload.missingSequences { simulatedRemoteChunks[sequence] = inMemoryChunks[sequence] }
        precondition([0, 1].compactMap { simulatedRemoteChunks[$0] }.reduce(into: Data()) { $0.append($1) } == syntheticBytes)
        precondition(!partialUpload.isValid(name: "other.pdf", mediaType: "application/pdf", size: partialUpload.size_bytes,
                                            digest: attachmentDigest, existingID: projectAttachmentID))
        precondition(!partialUpload.isValid(name: "local.pdf", mediaType: "application/pdf", size: partialUpload.size_bytes,
                                            digest: attachmentDigest, existingID: "pca_" + String(repeating: "c", count: 32)))
        let incompleteCommit = MordeProjectAttachmentUploadStatus(attachment_id: projectAttachmentID, name: "local.pdf",
            media_type: "application/pdf", size_bytes: partialUpload.size_bytes, sha256: attachmentDigest,
            state: "committed", total_chunks: 2, received_sequences: [1], replay: true)
        precondition(!incompleteCommit.isValid(name: "local.pdf", mediaType: "application/pdf", size: partialUpload.size_bytes,
                                              digest: attachmentDigest, existingID: projectAttachmentID))
        let duplicateChunks = MordeProjectAttachmentUploadStatus(attachment_id: projectAttachmentID, name: "local.pdf",
            media_type: "application/pdf", size_bytes: partialUpload.size_bytes, sha256: attachmentDigest,
            state: "uploading", total_chunks: 2, received_sequences: [1, 1], replay: true)
        precondition(!duplicateChunks.isValid(name: "local.pdf", mediaType: "application/pdf", size: partialUpload.size_bytes,
                                             digest: attachmentDigest, existingID: projectAttachmentID))
        let expiredWithChunks = MordeProjectAttachmentUploadStatus(attachment_id: projectAttachmentID, name: "local.pdf",
            media_type: "application/pdf", size_bytes: partialUpload.size_bytes, sha256: attachmentDigest,
            state: "expired", total_chunks: 2, received_sequences: [1], replay: true)
        precondition(!expiredWithChunks.isValid(name: "local.pdf", mediaType: "application/pdf", size: partialUpload.size_bytes,
                                               digest: attachmentDigest, existingID: projectAttachmentID))
        let emptyDigest = MordeMessageChunking.digest(Data())
        let emptyCommit = MordeProjectAttachmentUploadStatus(attachment_id: projectAttachmentID, name: "empty.pdf",
            media_type: "application/pdf", size_bytes: 0, sha256: emptyDigest,
            state: "committed", total_chunks: 0, received_sequences: [], replay: true)
        precondition(emptyCommit.isValid(name: "empty.pdf", mediaType: "application/pdf", size: 0,
                                         digest: emptyDigest, existingID: nil))
        do {
            try MordeLocalExecutionPolicy.requireEnabled()
            preconditionFailure("local execution must stay disabled")
        } catch is MordeLocalExecutionDisabledError {
        } catch {
            preconditionFailure("unexpected local execution error: \(error)")
        }
        precondition(!MordeCloudKitSyncPolicy.isEnabled)
        precondition(!MordeCloudKitSyncPolicy.disabledMessage.isEmpty)
        precondition(MordeCloudKitSyncDisabledError().errorDescription == MordeCloudKitSyncPolicy.disabledMessage)
        precondition(!MordeCloudKitSyncPolicy.shouldRemoveLegacyCache(userSyncEnabled: true))
        precondition(!MordeCloudKitSyncPolicy.shouldRemoveLegacyCache(userSyncEnabled: false))
        precondition(!MordeLegacyServicePolicy.isEnabled)
        precondition(MordeLegacyServiceDisabledError().errorDescription == MordeLegacyServicePolicy.disabledMessage)
        precondition(MordeAPIKeyDeletionOutcome(legacyStatus: 0, synchronizableStatus: 0).isComplete)
        precondition(MordeAPIKeyDeletionOutcome(legacyStatus: -25_300, synchronizableStatus: -25_300).isComplete)
        precondition(MordeAPIKeyDeletionOutcome(legacyStatus: 0, synchronizableStatus: -25_300).isComplete)
        let partialAPIKeyDeletion = MordeAPIKeyDeletionOutcome(legacyStatus: 0, synchronizableStatus: -50)
        precondition(!partialAPIKeyDeletion.isComplete)
        precondition(partialAPIKeyDeletion.failedStatuses == [-50])

        let providerCleanupRows = [
            MordeProviderKeyCleanupDescriptor(instanceID: "openai-1", providerType: "openAI", credentialType: "apiKey"),
            MordeProviderKeyCleanupDescriptor(instanceID: "cloud-relay-1", providerType: "openAI", credentialType: "apiKey"),
            MordeProviderKeyCleanupDescriptor(instanceID: "oauth-1", providerType: "anthropic", credentialType: "oauth"),
            MordeProviderKeyCleanupDescriptor(instanceID: "unknown-1", providerType: "futureProvider", credentialType: "apiKey"),
            MordeProviderKeyCleanupDescriptor(instanceID: " ", providerType: "gemini", credentialType: "apiKey"),
        ]
        precondition(MordeProviderKeyCleanupPlan.candidateInstanceIDs(from: providerCleanupRows) == ["cloud-relay-1", "openai-1"])
        precondition(MordeProviderKeyCleanupPlan.reviewRequiredInstanceIDs(from: providerCleanupRows) == ["unknown-1"])
        precondition(MordeProviderKeyCleanupPlan.shouldShowNotice(storedValue: nil, cleanupComplete: true))
        precondition(MordeProviderKeyCleanupPlan.shouldShowNotice(storedValue: true, cleanupComplete: false))
        precondition(!MordeProviderKeyCleanupPlan.shouldShowNotice(storedValue: false, cleanupComplete: true))
        precondition(!MordeProviderKeyCleanupPlan.shouldShowNotice(storedValue: nil, cleanupComplete: false))
        let syntheticService = "io.github.nanomuse.app.provider.synthetic"
        var syntheticKeychain: [[String: Any]] = [
            ["class": "generic-password", "service": syntheticService, "account": "api-key", "synchronizable": false, "value": "synthetic-legacy-key"],
            ["class": "generic-password", "service": syntheticService, "account": "api-key", "synchronizable": true, "value": "synthetic-synced-key"],
            ["class": "generic-password", "service": syntheticService, "account": "oauth-token", "synchronizable": false, "value": "synthetic-oauth-token"],
            ["class": "generic-password", "service": "another.provider", "account": "api-key", "synchronizable": true, "value": "synthetic-other-key"],
        ]
        var deletionQueries: [[String: Any]] = []
        func syntheticMatch(_ item: [String: Any], query: [String: Any]) -> Bool {
            query.allSatisfy { key, expected in
                if let bool = expected as? Bool { return item[key] as? Bool == bool }
                if let string = expected as? String { return item[key] as? String == string }
                return false
            }
        }
        let syntheticOutcome = MordeProviderKeyCleanupPlan.deleteVariants(
            baseQuery: ["class": "generic-password", "service": syntheticService, "account": "api-key"],
            synchronizableAttribute: "synchronizable"
        ) { query in
            deletionQueries.append(query)
            let found = syntheticKeychain.contains { syntheticMatch($0, query: query) }
            syntheticKeychain.removeAll { syntheticMatch($0, query: query) }
            return found ? MordeAPIKeyDeletionOutcome.successStatus : MordeAPIKeyDeletionOutcome.itemNotFoundStatus
        }
        precondition(syntheticOutcome.isComplete)
        precondition(deletionQueries.count == 2)
        precondition(deletionQueries[0]["synchronizable"] as? Bool == false)
        precondition(deletionQueries[1]["synchronizable"] as? Bool == true)
        precondition(syntheticKeychain.count == 2)
        precondition(syntheticKeychain.contains { $0["account"] as? String == "oauth-token" })
        precondition(syntheticKeychain.contains { $0["service"] as? String == "another.provider" })
        var partialDeleteStatuses: [Int32] = [MordeAPIKeyDeletionOutcome.successStatus, -50]
        let partialDelete = MordeProviderKeyCleanupPlan.deleteVariants(
            baseQuery: [:],
            synchronizableAttribute: "synchronizable"
        ) { _ in partialDeleteStatuses.removeFirst() }
        precondition(!partialDelete.isComplete && partialDelete.failedStatuses == [-50])
        let priorCleanup = MordeProviderKeyCleanupLedger(
            version: MordeProviderKeyCleanupPlan.version,
            status: .pending,
            pendingInstanceIDs: ["cloud-relay-1", "retry-1"],
            pendingLegacyServices: [MordeProviderKeyCleanupPlan.legacyKeychainServices[0]],
            reviewRequiredInstanceIDs: []
        )
        let pendingCleanup = MordeProviderKeyCleanupPlan.begin(
            previous: priorCleanup,
            candidateInstanceIDs: MordeProviderKeyCleanupPlan.candidateInstanceIDs(from: providerCleanupRows),
            reviewRequiredInstanceIDs: MordeProviderKeyCleanupPlan.reviewRequiredInstanceIDs(from: providerCleanupRows),
            preservedInstanceIDs: ["cloud-relay-1"]
        )
        precondition(pendingCleanup.status == .pending)
        precondition(pendingCleanup.pendingInstanceIDs == ["openai-1", "retry-1"])
        precondition(!pendingCleanup.pendingInstanceIDs.contains("cloud-relay-1"), "a Cloud relay key in a pending ledger must not be deleted")
        precondition(pendingCleanup.pendingInstanceIDs.contains("openai-1"), "ordinary OpenAI API keys remain in the cleanup plan")
        precondition(pendingCleanup.pendingLegacyServices == MordeProviderKeyCleanupPlan.legacyKeychainServices)
        precondition(pendingCleanup.reviewRequiredInstanceIDs == ["unknown-1"])
        let failedCleanup = MordeProviderKeyCleanupPlan.finish(
            pendingCleanup,
            failedInstanceIDs: ["retry-1"],
            failedLegacyServices: [MordeProviderKeyCleanupPlan.legacyKeychainServices[0]]
        )
        precondition(failedCleanup.status == .pending)
        precondition(failedCleanup.pendingInstanceIDs == ["retry-1"])
        precondition(failedCleanup.pendingLegacyServices == [MordeProviderKeyCleanupPlan.legacyKeychainServices[0]])
        let completedCleanup = MordeProviderKeyCleanupPlan.finish(failedCleanup, failedInstanceIDs: [], failedLegacyServices: [])
        precondition(completedCleanup.status == .complete)
        precondition(completedCleanup.pendingInstanceIDs.isEmpty && completedCleanup.pendingLegacyServices.isEmpty)
        precondition(completedCleanup.reviewRequiredInstanceIDs == ["unknown-1"])
        let cleanupLedgerData = try! JSONEncoder().encode(completedCleanup)
        precondition(try! JSONDecoder().decode(MordeProviderKeyCleanupLedger.self, from: cleanupLedgerData) == completedCleanup)
        precondition(MordeBackupAPIKeyRestorePolicy.disposition(hasAPIKey: true) == .removedByMigration)
        precondition(MordeBackupAPIKeyRestorePolicy.disposition(hasAPIKey: false) == .absent)
        let syntheticBackupSecrets = Data(#"{"futureTopLevel":"ignored","providers":[{"instanceId":"synthetic","apiKey":"synthetic-key","futureCredential":"ignored"}],"envVars":[{"name":"SYNTHETIC","value":"eA==","futureEnv":"ignored"}]}"#.utf8)
        precondition(MordeBackupAPIKeyRestorePolicy.unknownFieldCount(in: syntheticBackupSecrets) == 3)

        let now = Date(timeIntervalSince1970: 1_793_548_800)
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime]
        let nowString = formatter.string(from: now)
        let soonString = formatter.string(from: now.addingTimeInterval(7 * 24 * 60 * 60))
        let futureString = formatter.string(from: now.addingTimeInterval(7 * 24 * 60 * 60 + 1))

        precondition(MordeCredentialExpiry.status(expiresAt: nowString, now: now) == .expired)
        precondition(MordeCredentialExpiry.status(expiresAt: soonString, now: now) == .expiringSoon)
        precondition(MordeCredentialExpiry.status(expiresAt: futureString, now: now) == .valid)
        precondition(MordeCredentialExpiry.secondsUntilExpiry(expiresAt: nowString, now: now) == 0)
        precondition(MordeCredentialExpiry.secondsUntilExpiry(expiresAt: futureString, now: now) == 7 * 24 * 60 * 60 + 1)
        precondition(MordeCredentialExpiry.secondsUntilExpiry(expiresAt: nil, now: now) == nil)
        precondition(MordeCredentialExpiry.validCredential("secret", expiresAt: nowString, now: now) == nil)
        precondition(MordeCredentialExpiry.validCredential("secret", expiresAt: nil, now: now) == nil)
        precondition(MordeCredentialExpiry.validCredential(nil, expiresAt: futureString, now: now) == nil)
        precondition(MordeCredentialExpiry.validCredential("secret", expiresAt: futureString, now: now) == "secret")
        precondition(MordeCredentialExpiry.status(expiresAt: "invalid", now: now) == nil)
        precondition(MordeTaskStatusPresentation.label(status: "queued") == "排队中")
        precondition(MordeTaskStatusPresentation.label(status: "running") == "运行中")
        precondition(MordeTaskStatusPresentation.label(status: "waiting_user", waitingReason: "approval_required") == "等你审批")
        precondition(MordeTaskStatusPresentation.label(status: "waiting_user", waitingReason: "clarification_required") == "等待你补充信息")
        precondition(MordeTaskStatusPresentation.label(status: "waiting_user", waitingReason: "mcp_result_unknown") == "操作结果待核对")
        precondition(MordeTaskStatusPresentation.label(status: "waiting_user", waitingReason: "workspace_host_sync") == "等待电脑同步")
        precondition(MordeTaskStatusPresentation.label(status: "waiting_user", waitingReason: "workspace_input_changed") == "工作区已变化，等待确认")
        precondition(MordeTaskStatusPresentation.label(status: "waiting_user", waitingReason: "desktop_focus_changed") == "电脑焦点已变化，等待确认")
        precondition(MordeTaskStatusPresentation.label(status: "waiting_user", waitingReason: "mcp_result_unsupported") == "当前操作不受支持，等待处理")
        precondition(MordeTaskStatusPresentation.label(status: "waiting_user", waitingReason: "mcp_connector_revoked") == "连接器已撤销，等待处理")
        precondition(MordeTaskStatusPresentation.label(status: "waiting_user", waitingReason: "host_fence_changed") == "电脑状态已变化，等待处理")
        precondition(MordeTaskStatusPresentation.label(status: "waiting_user", waitingReason: "tool_call_budget") == "等待任务额度处理")
        precondition(MordeTaskStatusPresentation.label(status: "waiting_user", waitingReason: "project_recovery_unavailable") ==
            "原项目权限/上下文变化，未安全恢复，请重新发送。")
        precondition(MordeTaskStatusPresentation.localizationKey(status: "waiting_user", waitingReason: "project_recovery_unavailable") ==
            "原项目权限/上下文变化，未安全恢复，请重新发送。")
        precondition(MordeTaskStatusPresentation.label(status: "waiting_user", waitingReason: "future_reason") == "等待你处理")
        precondition(MordeTaskStatusPresentation.label(status: "pausing") == "正在暂停")
        precondition(MordeTaskStatusPresentation.label(status: "paused") == "已暂停")
        precondition(MordeTaskStatusPresentation.label(status: "cancelling", stopRequestStatus: "pending") == "正在停止，等待服务端确认")
        precondition(MordeTaskStatusPresentation.label(status: "cancelling", stopRequestStatus: "completed") == "停止请求已处理，等待任务状态更新")
        precondition(MordeTaskStatusPresentation.label(status: "cancelling") == "停止状态待确认")
        precondition(MordeTaskStatusPresentation.label(status: "completed") == "已完成")
        precondition(MordeTaskStatusPresentation.label(status: "failed") == "失败")
        precondition(MordeTaskStatusPresentation.label(status: "cancelled") == "已取消")
        precondition(MordeTaskStatusPresentation.label(status: "future_status") == "状态未知")
        let statusLocalizationCases: [(String, String?, String?, String)] = [
            ("queued", nil, nil, "Queued"),
            ("running", nil, nil, "Running"),
            ("waiting_user", "approval_required", nil, "Waiting for approval"),
            ("waiting_user", "clarification_required", nil, "Waiting for your input"),
            ("waiting_user", "workspace_host_sync", nil, "Waiting for computer sync"),
            ("waiting_user", "workspace_input_changed", nil, "Workspace changed; awaiting confirmation"),
            ("waiting_user", "desktop_focus_changed", nil, "Computer focus changed; awaiting confirmation"),
            ("waiting_user", "mcp_result_unknown", nil, "Operation result needs review"),
            ("waiting_user", "mcp_result_unsupported", nil, "Operation not supported; action needed"),
            ("waiting_user", "mcp_connector_revoked", nil, "Connector revoked; action needed"),
            ("waiting_user", "host_fence_changed", nil, "Computer state changed; action needed"),
            ("waiting_user", "tool_call_budget", nil, "Waiting for task quota"),
            ("waiting_user", "future_reason", nil, "Waiting for you"),
            ("pausing", nil, nil, "Pausing"),
            ("paused", nil, nil, "Paused"),
            ("cancelling", nil, "pending", "Stopping; awaiting server confirmation"),
            ("cancelling", nil, "completed", "Stop request processed; task update pending"),
            ("cancelling", nil, nil, "Stop status needs confirmation"),
            ("completed", nil, nil, "Completed"),
            ("failed", nil, nil, "Task failed"),
            ("cancelled", nil, nil, "Cancelled"),
            ("future_status", nil, nil, "Status unknown"),
        ]
        for (status, reason, stop, key) in statusLocalizationCases {
            precondition(MordeTaskStatusPresentation.localizationKey(
                status: status, waitingReason: reason, stopRequestStatus: stop) == key)
        }
        precondition(MordeTaskStatusPresentation.notificationLabel(kind: "completed") == "任务完成")
        precondition(MordeTaskStatusPresentation.notificationLabel(kind: "failed") == "任务失败")
        precondition(MordeTaskStatusPresentation.notificationLabel(kind: "waiting_user") == "需要你处理")
        precondition(MordeTaskStatusPresentation.notificationLabel(kind: "future_kind") == "任务通知")
        precondition(MordeTaskStatusPresentation.notificationLocalizationKey(kind: "completed") == "Task completed")
        precondition(MordeTaskStatusPresentation.notificationLocalizationKey(kind: "waiting_user") == "Action needed")
        precondition(MordeTaskStatusPresentation.notificationLocalizationKey(kind: "future_kind") == "Task notification")
        precondition(!MordeLoginContract.supportsOrderedClientLogin(["password_login", "client_installations"]))
        precondition(MordeLoginContract.supportsOrderedClientLogin(["password_login", "client_installations", "ordered_client_login"]))
        precondition(MordeAccountLoginFailure.fromAPIErrorCode("account_pending") == .pending)
        precondition(MordeAccountLoginFailure.fromAPIErrorCode("account_rejected") == .rejected)
        precondition(MordeAccountLoginFailure.fromAPIErrorCode("account_disabled") == .disabled)
        precondition(MordeAccountLoginFailure.fromAPIErrorCode("unauthenticated") == nil)
        precondition(MordeClientInstallationPolicy.isValidID("i1234567"))
        precondition(!MordeClientInstallationPolicy.isValidID("../api/me"))
        precondition(MordeClientInstallationPolicy.canRevoke("i7654321", currentInstallationID: "i1234567"))
        precondition(!MordeClientInstallationPolicy.canRevoke("i1234567", currentInstallationID: "i1234567"))
        precondition(!MordeClientInstallationPolicy.canRevoke("i7654321", currentInstallationID: nil))
        precondition(MordeClientInstallationPolicy.revokePath(
            for: "i7654321", currentInstallationID: "i1234567") == "/api/clients/i7654321")
        precondition(MordeClientInstallationPolicy.revokePath(
            for: "../api/me", currentInstallationID: "i1234567") == nil)
        let networkCapabilityFailure = MordeCapabilitiesFailure.classify(domain: NSURLErrorDomain,
                                                                           code: URLError.notConnectedToInternet.rawValue)
        precondition(networkCapabilityFailure == .network)
        precondition(networkCapabilityFailure.message(underlying: "offline").contains("无法连接 Morde 服务"))
        precondition(!networkCapabilityFailure.message().contains("版本不兼容"))
        let forbiddenCapabilityFailure = MordeCapabilitiesFailure.classify(domain: "Morde", code: 403)
        precondition(forbiddenCapabilityFailure == .http(statusCode: 403))
        precondition(forbiddenCapabilityFailure.message(underlying: "forbidden").contains("HTTP 403"))
        precondition(!forbiddenCapabilityFailure.message().contains("版本不兼容"))
        let missingCapabilitiesFailure = MordeCapabilitiesFailure.classify(domain: "Morde", code: 404)
        precondition(missingCapabilitiesFailure == .http(statusCode: 404))
        precondition(missingCapabilitiesFailure.message().contains("未提供能力接口"))
        precondition(MordeCapabilitiesFailure.classify(domain: NSCocoaErrorDomain, code: 3840) == .invalidResponse)
        precondition(MordeCapabilitiesFailure.incompatible.message().contains("版本不兼容"))
        precondition(MordeCapabilitiesFailure.minimumVersion.message().contains("版本过低"))
        precondition(MordeTaskCapability.cloudWorkerIsAvailable(["cloud_tasks"]))
        precondition(MordeTaskCapability.taskAPIIsAvailable(["cloud_tasks"]))
        precondition(!MordeTaskCapability.cloudWorkerIsAvailable(["task_api"]))
        precondition(MordeTaskCapability.taskAPIIsAvailable(["task_api"]))
        precondition(!MordeTaskCapability.taskAPIIsAvailable([]))
        precondition(MordeTaskCapability.mediaGenerationIsAvailable(["task_media_generation"]))
        precondition(!MordeTaskCapability.mediaGenerationIsAvailable(["task_api"]))
        precondition(MordeResourcePresentation.emptyListMessage(
            resource: .models, state: .loaded, itemCount: 0, emptyMessage: "没有模型。") == "没有模型。")
        let loadingModelsMessage = MordeResourcePresentation.emptyListMessage(
            resource: .models, state: .loading, itemCount: 0, emptyMessage: "没有模型。")
        precondition(loadingModelsMessage?.contains("正在读取") == true)
        precondition(loadingModelsMessage?.contains("没有模型") == false)
        let failedModelsMessage = MordeResourcePresentation.emptyListMessage(
            resource: .models, state: .failed("HTTP 403"), itemCount: 0, emptyMessage: "没有模型。")
        precondition(failedModelsMessage?.contains("未成功读取") == true)
        precondition(failedModelsMessage?.contains("HTTP 403") == true)
        precondition(MordeResourcePresentation.emptyListMessage(
            resource: .models, state: .failed("HTTP 403"), itemCount: 2, emptyMessage: "没有模型。") == nil)
        let approvedModelIDs: Set<String> = ["text-with-tools"]
        precondition(MordeTaskModelSelection.canSubmit(modelID: "text-with-tools", resourceState: .loaded,
                                                       allowedModelIDs: approvedModelIDs))
        precondition(!MordeTaskModelSelection.canSubmit(modelID: "text-with-tools", resourceState: .loading,
                                                        allowedModelIDs: approvedModelIDs))
        precondition(!MordeTaskModelSelection.canSubmit(modelID: "text-with-tools", resourceState: .failed("offline"),
                                                        allowedModelIDs: approvedModelIDs))
        precondition(!MordeTaskModelSelection.canSubmit(modelID: "removed-model", resourceState: .loaded,
                                                        allowedModelIDs: approvedModelIDs))
        precondition(!MordeTaskModelSelection.canSubmit(modelID: "", resourceState: .loaded,
                                                        allowedModelIDs: approvedModelIDs))
        precondition(MordeConversationTaskState.belongs(taskConversationID: "conversation-2",
                                                        toSelectedConversationID: "conversation-2"))
        precondition(!MordeConversationTaskState.belongs(taskConversationID: "conversation-1",
                                                         toSelectedConversationID: "conversation-2"))
        precondition(!MordeConversationTaskState.belongs(taskConversationID: "conversation-1",
                                                        toSelectedConversationID: nil))

        guard let fixturePath = ProcessInfo.processInfo.environment["MORDE_CLIENT_CONTRACT_FIXTURE"] else {
            fatalError("MORDE_CLIENT_CONTRACT_FIXTURE must point to the shared API contract fixture")
        }
        let fixtureData = try! Data(contentsOf: URL(fileURLWithPath: fixturePath))
        let contract = fixtureObject(try! JSONSerialization.jsonObject(with: fixtureData), label: "root")
        let capabilities = fixtureObject(contract["capabilities"], label: "capabilities")
        let queuedTaskAPI = fixtureObject(capabilities["queued_task_api"], label: "queued_task_api")
        let cloudExecution = fixtureObject(capabilities["cloud_execution"], label: "cloud_execution")
        let unsupported = fixtureObject(capabilities["unsupported"], label: "unsupported")
        let unsupportedVersion = fixtureObject(capabilities["unsupported_version"], label: "unsupported_version")
        func supportedFeatures(_ value: [String: Any], label: String) -> [String] {
            guard let features = value["supported_features"] as? [String] else {
                fatalError("Missing fixture feature list: \(label)")
            }
            return features
        }

        let queuedFeatures = supportedFeatures(queuedTaskAPI, label: "queued_task_api")
        precondition(fixtureString(queuedTaskAPI["api_version"], label: "queued_task_api.api_version") == "1")
        precondition(MordeLoginContract.supportsOrderedClientLogin(queuedFeatures))
        precondition(MordeTaskCapability.taskAPIIsAvailable(queuedFeatures))
        precondition(!MordeTaskCapability.cloudWorkerIsAvailable(queuedFeatures))

        let cloudFeatures = supportedFeatures(cloudExecution, label: "cloud_execution")
        precondition(MordeLoginContract.supportsOrderedClientLogin(cloudFeatures))
        precondition(MordeTaskCapability.taskAPIIsAvailable(cloudFeatures))
        precondition(MordeTaskCapability.cloudWorkerIsAvailable(cloudFeatures))

        let unsupportedFeatures = supportedFeatures(unsupported, label: "unsupported")
        precondition(MordeLoginContract.supportsOrderedClientLogin(unsupportedFeatures))
        precondition(!MordeTaskCapability.taskAPIIsAvailable(unsupportedFeatures))
        let unsupportedVersionFeatures = supportedFeatures(unsupportedVersion, label: "unsupported_version")
        precondition(fixtureString(unsupportedVersion["api_version"], label: "unsupported_version.api_version") != "1")
        precondition(MordeLoginContract.supportsOrderedClientLogin(unsupportedVersionFeatures))
        precondition(MordeTaskCapability.taskAPIIsAvailable(unsupportedVersionFeatures))
        let fixtureMinimumVersion = fixtureString(unsupportedVersion["minimum_client_version"],
                                                  label: "unsupported_version.minimum_client_version")
        precondition(!MordeClientVersion.meetsMinimum(current: "1.0.0", minimum: fixtureMinimumVersion))

        guard let errors = contract["errors"] as? [[String: Any]] else { fatalError("Missing fixture errors") }
        let expectedErrorStatuses = [
            "unauthenticated": 401, "forbidden": 403, "not_found": 404,
            "event_cursor_expired": 409, "unsupported": 422,
            "host_not_ready": 503, "upstream_timeout": 504,
        ]
        precondition(errors.count == expectedErrorStatuses.count)
        for error in errors {
            let name = fixtureString(error["name"], label: "error.name")
            let status = fixtureInt(error["status"], label: "error.status")
            precondition(expectedErrorStatuses[name] == status)
            let body = fixtureObject(error["body"], label: "error.body")
            let envelope = fixtureObject(body["error"], label: "error.body.error")
            precondition(fixtureString(envelope["code"], label: "error.body.error.code") == name)
            precondition(!fixtureString(envelope["message"], label: "error.body.error.message").isEmpty)
            precondition(envelope["details"] is [String: Any])
            precondition(MordeCapabilitiesFailure.classify(domain: "Morde", code: status) == .http(statusCode: status))
        }
        let hostNotReady = errors.first { $0["name"] as? String == "host_not_ready" }!
        precondition(fixtureString(hostNotReady["retry_after"], label: "host_not_ready.retry_after") == "5")
        let nonJSONError = fixtureObject(contract["non_json_error"], label: "non_json_error")
        precondition(fixtureInt(nonJSONError["status"], label: "non_json_error.status") == 502)
        precondition(fixtureString(nonJSONError["content_type"], label: "non_json_error.content_type") == "text/html")
        precondition(fixtureString(nonJSONError["body"], label: "non_json_error.body").hasPrefix("<!doctype"))
        precondition(MordeCapabilitiesFailure.classify(domain: "Morde", code: 502) == .http(statusCode: 502))
        let networkError = fixtureObject(contract["network_error"], label: "network_error")
        precondition(fixtureString(networkError["name"], label: "network_error.name") == "TypeError")
        precondition(MordeCapabilitiesFailure.classify(domain: NSURLErrorDomain,
                                                       code: URLError.notConnectedToInternet.rawValue) == .network)

        let taskSnapshot = fixtureObject(contract["task_snapshot"], label: "task_snapshot")
        let snapshotSequence = Int64(fixtureInt(taskSnapshot["event_sequence"], label: "task_snapshot.event_sequence"))
        let frames = fixtureObject(contract["task_event_frames"], label: "task_event_frames")
        let assistantDelta = parseSSEFixture(fixtureString(frames["assistant_delta"], label: "assistant_delta"))
        precondition(assistantDelta.id == "7" && assistantDelta.event == "task.message")
        precondition(fixtureInt(assistantDelta.data["sequence"], label: "assistant_delta.data.sequence") == 7)
        precondition(MordeTaskSnapshotCursor.resumeCursor(eventID: assistantDelta.id,
                                                          snapshotSequence: snapshotSequence) == "8")
        let snapshotRequired = parseSSEFixture(fixtureString(frames["snapshot_required"], label: "snapshot_required"))
        precondition(snapshotRequired.id == "8" && snapshotRequired.event == "task.snapshot_required")
        precondition(fixtureInt(snapshotRequired.data["sequence"], label: "snapshot_required.data.sequence") == 8)
        precondition(snapshotSequence >= Int64(snapshotRequired.id)!)
        precondition(MordeTaskSnapshotCursor.resumeCursor(eventID: snapshotRequired.id,
                                                          snapshotSequence: snapshotSequence) == "8")

        let serviceA = "11111111-1111-4111-8111-111111111111"
        let serviceB = "22222222-2222-4222-8222-222222222222"
        precondition(MordeLoginContract.mayRenewInstallation(
            installationOrigin: "https://morde.example", installationServiceInstanceID: serviceA,
            selectedServer: "https://morde.example:443", selectedServiceInstanceID: serviceA.uppercased(),
            legacyServer: nil, installationAccount: "alice", accountName: "alice"))
        precondition(MordeLoginContract.mayRenewInstallation(
            installationOrigin: nil, installationServiceInstanceID: nil,
            selectedServer: "https://morde.example", selectedServiceInstanceID: serviceA,
            legacyServer: "https://morde.example", installationAccount: "alice", accountName: "alice"))
        precondition(!MordeLoginContract.mayRenewInstallation(
            installationOrigin: "https://other.example", installationServiceInstanceID: serviceA,
            selectedServer: "https://morde.example", selectedServiceInstanceID: serviceA,
            legacyServer: "https://morde.example", installationAccount: "alice", accountName: "alice"))
        precondition(!MordeLoginContract.mayRenewInstallation(
            installationOrigin: "https://morde.example", installationServiceInstanceID: serviceA,
            selectedServer: "https://morde.example", selectedServiceInstanceID: serviceA,
            legacyServer: nil, installationAccount: "alice", accountName: "bob"))
        precondition(!MordeLoginContract.mayRenewInstallation(
            installationOrigin: "https://morde.example", installationServiceInstanceID: serviceB,
            selectedServer: "https://morde.example", selectedServiceInstanceID: serviceA,
            legacyServer: nil, installationAccount: "alice", accountName: "alice"))
        let suiteName = "MordeLoginStateStoreTests-\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suiteName)!
        defaults.removePersistentDomain(forName: suiteName)
        defer { defaults.removePersistentDomain(forName: suiteName) }
        let firstStore = MordeLoginStateStore(defaults: defaults)
        let restartedStore = MordeLoginStateStore(defaults: defaults)
        let firstIntent = try! firstStore.registerIntent(origin: "https://MORDE.example:443/", accountName: " alice ")
        let firstAttempt = try! firstStore.beginAttempt(for: firstIntent, serviceInstanceID: serviceA)!
        precondition(firstAttempt.origin == "https://morde.example")
        precondition(firstAttempt.attemptGeneration == 1)

        let secondIntent = try! restartedStore.registerIntent(origin: "https://morde.example", accountName: "alice")
        let secondAttempt = try! restartedStore.beginAttempt(for: secondIntent, serviceInstanceID: serviceA)!
        precondition(secondAttempt.attemptGeneration == 2)
        precondition(secondAttempt.clientInstanceID == firstAttempt.clientInstanceID)
        precondition(secondAttempt.idempotencyKey != firstAttempt.idempotencyKey)

        let persistedState = String(decoding: defaults.data(forKey: MordeLoginStateStore.defaultsKey)!, as: UTF8.self)
        precondition(!persistedState.contains("password"))
        precondition(!persistedState.contains(secondAttempt.idempotencyKey))
        var staleCredentialWasWritten = false
        precondition(try! !firstStore.commitIfCurrent(firstAttempt) { staleCredentialWasWritten = true })
        precondition(!staleCredentialWasWritten)
        var currentCredentialWasWritten = false
        precondition(try! restartedStore.commitIfCurrent(secondAttempt) { currentCredentialWasWritten = true })
        precondition(currentCredentialWasWritten)

        let thirdIntent = try! firstStore.registerIntent(origin: "https://morde.example", accountName: "alice")
        let thirdAttempt = try! firstStore.beginAttempt(for: thirdIntent, serviceInstanceID: serviceA)!
        precondition(thirdAttempt.attemptGeneration == 3)
        precondition(thirdAttempt.clientInstanceID == firstAttempt.clientInstanceID)
        precondition(try! firstStore.commitIfCurrent(thirdAttempt) {})

        let rebuiltInstanceIntent = try! restartedStore.registerIntent(origin: "https://morde.example", accountName: "alice")
        let rebuiltInstanceAttempt = try! restartedStore.beginAttempt(for: rebuiltInstanceIntent,
                                                                       serviceInstanceID: serviceB)!
        precondition(rebuiltInstanceAttempt.attemptGeneration == 1)
        precondition(rebuiltInstanceAttempt.clientInstanceID != thirdAttempt.clientInstanceID)
        precondition(try! restartedStore.commitIfCurrent(rebuiltInstanceAttempt) {})

        let otherServiceIntent = try! restartedStore.registerIntent(origin: "https://other.example", accountName: "alice")
        let otherServiceAttempt = try! restartedStore.beginAttempt(for: otherServiceIntent, serviceInstanceID: serviceA)!
        precondition(otherServiceAttempt.attemptGeneration == 1)
        precondition(otherServiceAttempt.clientInstanceID != thirdAttempt.clientInstanceID)
        precondition(try! restartedStore.commitIfCurrent(otherServiceAttempt) {})

        let otherAccountIntent = try! restartedStore.registerIntent(origin: "https://morde.example", accountName: "bob")
        let otherAccountAttempt = try! restartedStore.beginAttempt(for: otherAccountIntent, serviceInstanceID: serviceA)!
        precondition(otherAccountAttempt.attemptGeneration == 1)
        precondition(otherAccountAttempt.clientInstanceID != thirdAttempt.clientInstanceID)
        precondition(try! restartedStore.commitIfCurrent(otherAccountAttempt) {})

        let supersededIntent = try! firstStore.registerIntent(origin: "https://morde.example", accountName: "alice")
        let latestIntent = try! restartedStore.registerIntent(origin: "https://morde.example", accountName: "alice")
        precondition(try! firstStore.beginAttempt(for: supersededIntent, serviceInstanceID: serviceA) == nil)
        let pendingAtLogout = try! restartedStore.beginAttempt(for: latestIntent, serviceInstanceID: serviceA)!
        try! firstStore.rotateClientInstanceID(origin: "https://morde.example", serviceInstanceID: serviceA, accountName: "alice")
        precondition(!restartedStore.isCurrent(pendingAtLogout))
        let afterLogoutIntent = try! restartedStore.registerIntent(origin: "https://morde.example", accountName: "alice")
        let afterLogoutAttempt = try! restartedStore.beginAttempt(for: afterLogoutIntent, serviceInstanceID: serviceA)!
        precondition(afterLogoutAttempt.attemptGeneration == 1)
        precondition(afterLogoutAttempt.clientInstanceID != firstAttempt.clientInstanceID)
        precondition(afterLogoutAttempt.clientInstanceID != otherAccountAttempt.clientInstanceID)
        try! firstStore.rotateClientInstanceID(origin: "https://morde.example", serviceInstanceID: serviceA, accountName: "alice")
        let secondLogoutIntent = try! restartedStore.registerIntent(origin: "https://morde.example", accountName: "alice")
        let afterSecondLogoutAttempt = try! restartedStore.beginAttempt(for: secondLogoutIntent, serviceInstanceID: serviceA)!
        precondition(afterSecondLogoutAttempt.attemptGeneration == 1)
        precondition(afterSecondLogoutAttempt.clientInstanceID != firstAttempt.clientInstanceID)
        precondition(afterSecondLogoutAttempt.clientInstanceID != afterLogoutAttempt.clientInstanceID)

        let parallelSuite = "MordeLoginStateStoreParallelTests-\(UUID().uuidString)"
        let parallelDefaults = UserDefaults(suiteName: parallelSuite)!
        parallelDefaults.removePersistentDomain(forName: parallelSuite)
        defer { parallelDefaults.removePersistentDomain(forName: parallelSuite) }
        let resultLock = NSLock()
        var parallelGenerations: [Int64] = []
        DispatchQueue.concurrentPerform(iterations: 16) { _ in
            let store = MordeLoginStateStore(defaults: parallelDefaults)
            let intent = try! store.registerIntent(origin: "https://parallel.example", accountName: "alice")
            if let ticket = try! store.beginAttempt(for: intent, serviceInstanceID: serviceA) {
                resultLock.lock()
                parallelGenerations.append(ticket.attemptGeneration)
                resultLock.unlock()
            }
        }
        precondition(!parallelGenerations.isEmpty)
        precondition(Set(parallelGenerations).count == parallelGenerations.count)
        let taskAttempt = MordeTaskAttempt.fingerprint(conversationID: nil, hostID: "host-1", modelID: "model-1", objective: "private prompt")
        precondition(taskAttempt.count == 64)
        precondition(MordeTaskAttempt.canContinuePendingAttempt(nil, requestedFingerprint: taskAttempt))
        precondition(MordeTaskAttempt.canContinuePendingAttempt(taskAttempt, requestedFingerprint: taskAttempt))
        precondition(!MordeTaskAttempt.canContinuePendingAttempt("different", requestedFingerprint: taskAttempt))
        precondition(taskAttempt == MordeTaskAttempt.fingerprint(conversationID: nil, hostID: "host-1", modelID: "model-1", objective: "private prompt"))
        precondition(taskAttempt != MordeTaskAttempt.fingerprint(conversationID: nil, hostID: "host-1", modelID: "model-1", objective: "changed prompt"))
        precondition(taskAttempt != MordeTaskAttempt.fingerprint(conversationID: nil, hostID: "host-1", workspaceID: "workspace-1", modelID: "model-1", objective: "private prompt"))
        precondition(MordeTaskTargetPolicy.hostIsReady(state: "running", controlState: "ai"))
        precondition(!MordeTaskTargetPolicy.hostIsReady(state: "running", controlState: "user"))
        precondition(!MordeTaskTargetPolicy.hostIsReady(state: "stopped", controlState: "ai"))
        precondition(!MordeTaskTargetPolicy.hostIsReady(state: "running", controlState: nil))
        precondition(MordeTaskTargetPolicy.workspaceIsReady(bindingState: "active", workspaceState: "ready",
                                                            syncState: "healthy", appliedRevision: 4,
                                                            desiredRevision: 4, workspaceRevision: 4))
        precondition(!MordeTaskTargetPolicy.workspaceIsReady(bindingState: "preparing", workspaceState: "ready",
                                                             syncState: "healthy", appliedRevision: 4,
                                                             desiredRevision: 4, workspaceRevision: 4))
        precondition(!MordeTaskTargetPolicy.workspaceIsReady(bindingState: "active", workspaceState: "archived",
                                                             syncState: "healthy", appliedRevision: 4,
                                                             desiredRevision: 4, workspaceRevision: 4))
        precondition(!MordeTaskTargetPolicy.workspaceIsReady(bindingState: "active", workspaceState: "ready",
                                                             syncState: "conflict", appliedRevision: 4,
                                                             desiredRevision: 4, workspaceRevision: 4))
        precondition(!MordeTaskTargetPolicy.workspaceIsReady(bindingState: "active", workspaceState: "ready",
                                                             syncState: "healthy", appliedRevision: 3,
                                                             desiredRevision: 4, workspaceRevision: 4))
        precondition(!MordeTaskTargetPolicy.workspaceIsReady(bindingState: "active", workspaceState: "ready",
                                                             syncState: "healthy", appliedRevision: 4,
                                                             desiredRevision: 4, workspaceRevision: 5))
        precondition(MordeTaskTargetPolicy.targetIsUnavailable(hostID: "host-1", workspaceID: "", workspaceBindingCount: 0,
                                                              hostExists: false, hostReady: false,
                                                              workspaceExists: false, workspaceReady: false))
        precondition(!MordeTaskTargetPolicy.targetIsUnavailable(hostID: "", workspaceID: "", workspaceBindingCount: 0,
                                                               hostExists: false, hostReady: false,
                                                               workspaceExists: false, workspaceReady: false))
        precondition(MordeTaskTargetPolicy.targetIsUnavailable(hostID: "host-1", workspaceID: "", workspaceBindingCount: 0,
                                                              hostExists: true, hostReady: false,
                                                              workspaceExists: false, workspaceReady: false))
        precondition(MordeTaskTargetPolicy.targetIsUnavailable(hostID: "host-1", workspaceID: "workspace-1", workspaceBindingCount: 1,
                                                              hostExists: true, hostReady: true,
                                                              workspaceExists: false, workspaceReady: false))
        precondition(MordeTaskTargetPolicy.targetIsUnavailable(hostID: "", workspaceID: "workspace-1", workspaceBindingCount: 0,
                                                              hostExists: false, hostReady: false,
                                                              workspaceExists: true, workspaceReady: true))
        precondition(!MordeTaskTargetPolicy.targetIsUnavailable(hostID: "host-1", workspaceID: "workspace-1", workspaceBindingCount: 1,
                                                               hostExists: true, hostReady: true,
                                                               workspaceExists: true, workspaceReady: true))
        precondition(MordeTaskTargetPolicy.targetIsUnavailable(hostID: "host-1", workspaceID: "workspace-1", workspaceBindingCount: 2,
                                                              hostExists: true, hostReady: true,
                                                              workspaceExists: true, workspaceReady: true))
        precondition(MordeTaskTargetPolicy.canRestoreHost(state: "stopped", isOwner: true,
                                                          workspaceBindingCount: 1, workspaceReady: true))
        precondition(!MordeTaskTargetPolicy.canRestoreHost(state: "stopped", isOwner: false,
                                                           workspaceBindingCount: 1, workspaceReady: true))
        precondition(!MordeTaskTargetPolicy.canRestoreHost(state: "stopped", isOwner: true,
                                                           workspaceBindingCount: 2, workspaceReady: true))
        precondition(!MordeTaskTargetPolicy.canRestoreHost(state: "running", isOwner: true,
                                                           workspaceBindingCount: 1, workspaceReady: true))
        precondition(MordeWorkspaceSyncCapability.isAvailable(["workspace_sync"]))
        precondition(!MordeWorkspaceSyncCapability.isAvailable(["hub", "task_api"]))
        precondition(!MordeWorkspaceSyncCapability.isAvailable(["task_api"]))
        precondition(!MordeWorkspaceSyncCapability.isAvailable(nil))
        precondition(MordeConversationSelectionPolicy.shouldFollowMain(currentlySelected: nil,
                                                                       wasFollowingMain: false))
        precondition(MordeConversationSelectionPolicy.shouldFollowMain(currentlySelected: "c1234567",
                                                                       wasFollowingMain: true))
        precondition(!MordeConversationSelectionPolicy.shouldFollowMain(currentlySelected: "c1234567",
                                                                        wasFollowingMain: false))
        let scopeGeneration = UUID()
        precondition(MordeScopedRequestPolicy.isCurrent(scope: "account-a", generation: scopeGeneration,
                                                         activeScope: "account-a", activeGeneration: scopeGeneration))
        precondition(!MordeScopedRequestPolicy.isCurrent(scope: "account-a", generation: scopeGeneration,
                                                          activeScope: "account-b", activeGeneration: scopeGeneration))
        precondition(!MordeScopedRequestPolicy.isCurrent(scope: "account-a", generation: scopeGeneration,
                                                          activeScope: "account-a", activeGeneration: UUID()))
        precondition(MordeHostControlPolicy.canTakeOver(state: "ai", actorID: "", leaseActive: false, transitioning: false))
        precondition(!MordeHostControlPolicy.canTakeOver(state: "ai", actorID: nil, leaseActive: false, transitioning: false))
        precondition(!MordeHostControlPolicy.canTakeOver(state: "ai", actorID: "u1234567", leaseActive: false, transitioning: false))
        precondition(MordeHostControlPolicy.canRelease(state: "user", actorID: "u1234567", currentUserID: "u1234567",
                                                        leaseActive: true, transitioning: false))
        precondition(!MordeHostControlPolicy.canRelease(state: "user", actorID: "u7654321", currentUserID: "u1234567",
                                                         leaseActive: true, transitioning: false))
        precondition(MordeHostControlPolicy.canRelease(state: "user", actorID: nil, currentUserID: "u1234567",
                                                        leaseActive: false, transitioning: false))
        precondition(!MordeHostControlPolicy.canRelease(state: "user", actorID: "u7654321", currentUserID: "u1234567",
                                                         leaseActive: false, transitioning: false))
        precondition(!MordeHostControlPolicy.canRenew(state: "user", actorID: "u1234567", currentUserID: "u1234567",
                                                       leaseActive: false, transitioning: false))
        precondition(MordeProjectPolicy.hasValidID("j1234567"))
        precondition(!MordeProjectPolicy.hasValidID("../project"))
        precondition(MordeProjectPolicy.hasValidInvitationID("r1234567"))
        precondition(MordeProjectPolicy.hasValidConversationID("c1234567"))
        precondition(!MordeProjectPolicy.hasValidConversationID("../tasks"))
        precondition(MordeProjectPolicy.hasValidUserID("u1234567"))
        precondition(MordeProjectPolicy.hasValidTaskID("q1234567"))
        precondition(!MordeProjectPolicy.hasValidTaskID("../tasks"))
        precondition(MordeProjectPolicy.restorableTaskID(
            ownerID: "u1234567", currentUserID: "u1234567",
            messages: [(actorID: "u1234567", taskID: "q1234567"),
                       (actorID: "u7654321", taskID: "q7654321"),
                       (actorID: "u1234567", taskID: "q7654321")]) == "q7654321")
        precondition(MordeProjectPolicy.restorableTaskID(
            ownerID: "u7654321", currentUserID: "u1234567",
            messages: [(actorID: "u1234567", taskID: "q1234567")]) == nil)
        precondition(MordeProjectPolicy.restorableTaskID(
            ownerID: "u1234567", currentUserID: "u1234567",
            messages: [(actorID: "u1234567", taskID: "q1234567"),
                       (actorID: "u1234567", taskID: "../tasks")]) == "q1234567")
        precondition(MordeProjectPolicy.hasValidResourceID(kind: "workspace", id: "w1234567"))
        precondition(!MordeProjectPolicy.hasValidResourceID(kind: "workspace", id: "h1234567"))
        precondition(MordeProjectPolicy.isOwner(ownerID: "u1234567", currentUserID: "u1234567"))
        precondition(!MordeProjectPolicy.isOwner(ownerID: "u1234567", currentUserID: "u7654321"))
        precondition(MordeProjectPolicy.canAcceptInvitation(
            state: "pending", inviteeID: "u1234567", currentUserID: "u1234567"))
        precondition(!MordeProjectPolicy.canAcceptInvitation(
            state: "pending", inviteeID: "u1234567", currentUserID: "u7654321"))
        precondition(MordeProjectPolicy.canTransferOwnership(
            ownerID: "u1234567", currentUserID: "u1234567", targetUserID: "u7654321", targetRole: "member"))
        precondition(!MordeProjectPolicy.canTransferOwnership(
            ownerID: "u1234567", currentUserID: "u1234567", targetUserID: "u7654321", targetRole: "owner"))
        precondition(MordeProjectPolicy.canContinuePendingMutation(nil, requestedFingerprint: "create"))
        precondition(MordeProjectPolicy.canContinuePendingMutation("create", requestedFingerprint: "create"))
        precondition(!MordeProjectPolicy.canContinuePendingMutation("create", requestedFingerprint: "delete"))
        precondition(MordeProjectPolicy.canManageWorkItems(role: "owner"))
        precondition(MordeProjectPolicy.canManageWorkItems(role: "member"))
        precondition(!MordeProjectPolicy.canManageWorkItems(role: "viewer"))
        precondition(MordeProjectPolicy.canLinkTask(role: "member", state: "open", taskID: "q1234567", taskIDs: []))
        precondition(!MordeProjectPolicy.canLinkTask(role: "viewer", state: "open", taskID: "q1234567", taskIDs: []))
        precondition(!MordeProjectPolicy.canLinkTask(role: "member", state: "delivered", taskID: "q1234567", taskIDs: []))
        precondition(!MordeProjectPolicy.canLinkTask(role: "member", state: "open", taskID: "q1234567", taskIDs: ["q1234567"]))
        precondition(MordeProjectPolicy.canRecordDelivery(role: "owner", state: "open", taskID: "q1234567",
                                                           taskIDs: ["q1234567"], workspaceID: "w1234567", revision: 2))
        precondition(!MordeProjectPolicy.canRecordDelivery(role: "viewer", state: "open", taskID: "q1234567",
                                                            taskIDs: ["q1234567"], workspaceID: "w1234567", revision: 2))
        precondition(!MordeProjectPolicy.canRecordDelivery(role: "member", state: "open", taskID: "q1234567",
                                                            taskIDs: [], workspaceID: "w1234567", revision: 2))
        precondition(!MordeProjectPolicy.canRecordDelivery(role: "member", state: "open", taskID: "q1234567",
                                                            taskIDs: ["q1234567"], workspaceID: "w1234567", revision: 0))
        precondition(MordeProjectPolicy.canSetBudget(role: "owner", currency: "USD", limitMicros: 1_000_000))
        precondition(!MordeProjectPolicy.canSetBudget(role: "member", currency: "USD", limitMicros: 1_000_000))
        precondition(!MordeProjectPolicy.canSetBudget(role: "owner", currency: "usd", limitMicros: 1_000_000))
        precondition(!MordeProjectPolicy.canSetBudget(role: "owner", currency: "USD", limitMicros: 0))
        precondition(MordeProjectExecutionCapability.isAvailable(["project_budget_execution"]))
        precondition(!MordeProjectExecutionCapability.isAvailable(["task_api"]))
        precondition(!MordeProjectExecutionCapability.isAvailable(nil))
        precondition(MordeProjectExecutionCapability.canContinueTask(status: "running"))
        precondition(MordeProjectExecutionCapability.canContinueTask(status: "waiting_user"))
        precondition(MordeProjectExecutionCapability.canContinueTask(status: "waiting_user", waitingReason: "approval_required"))
        precondition(!MordeProjectExecutionCapability.canContinueTask(status: "waiting_user", waitingReason: "project_recovery_unavailable"))
        precondition(MordeProjectExecutionCapability.canContinueTask(status: "running", waitingReason: "project_recovery_unavailable"))
        precondition(!MordeProjectExecutionCapability.canContinueTask(status: "completed"))
        var seenPersonalSnapshotCursors = Set<String>()
        precondition(!MordePersonalSnapshotCursorPolicy.repeats("page-1", seen: &seenPersonalSnapshotCursors))
        precondition(!MordePersonalSnapshotCursorPolicy.repeats("page-2", seen: &seenPersonalSnapshotCursors))
        precondition(MordePersonalSnapshotCursorPolicy.repeats("page-1", seen: &seenPersonalSnapshotCursors))
        precondition(!MordePersonalSnapshotCursorPolicy.repeats(nil, seen: &seenPersonalSnapshotCursors))
        precondition(MordeNotificationPresentationPolicy.summary("任务完成", redacted: false) == "任务完成")
        precondition(MordeNotificationPresentationPolicy.summary("敏感摘要", redacted: true) == "不可查看")
        precondition(MordeNotificationPresentationPolicy.summary("旧服务端摘要", redacted: nil) == "不可查看")
        precondition(MordeNotificationPresentationPolicy.summary(nil, redacted: false) == "不可查看")
        precondition(MordeNotificationPresentationPolicy.canOpenTask(kind: "completed", redacted: false, actionable: false))
        precondition(MordeNotificationPresentationPolicy.canOpenTask(kind: "waiting_user", redacted: false, actionable: true))
        precondition(!MordeNotificationPresentationPolicy.canOpenTask(kind: "waiting_user", redacted: false, actionable: false))
        precondition(!MordeNotificationPresentationPolicy.canOpenTask(kind: "completed", redacted: nil, actionable: true))
        precondition(MordeNotificationPresentationPolicy.canAppendPage(currentIDs: ["n1"], pageIDs: ["n2"]))
        precondition(!MordeNotificationPresentationPolicy.canAppendPage(currentIDs: ["n1"], pageIDs: ["n1"]))
        let mainConversation = try! JSONDecoder().decode(MordeMainConversation.self,
            from: Data(#"{"status":"ready","conversation_id":"c1234567","version":1}"#.utf8))
        precondition(mainConversation.isValid)
        precondition(!(try! JSONDecoder().decode(MordeMainConversation.self,
            from: Data(#"{"status":"ready","conversation_id":"","version":0}"#.utf8))).isValid)
        precondition((try! JSONDecoder().decode(MordeMainConversation.self,
            from: Data(#"{"status":"selection_required","conversation_id":"","version":0}"#.utf8))).isValid)
        precondition(MordeMessageDeliveryPresentation.label("pending") == "已受理，等待进入 Agent")
        precondition(MordeMessageDeliveryPresentation.label("applied").contains("任务仍可能运行"))
        precondition(MordeMessageDeliveryPresentation.label("unknown").contains("不要重复提交"))
        precondition(MordeMessageDeliveryPresentation.label(nil).contains("未提供"))
        let deliveryMessageData = Data(#"{"id":"m1234567","conversation_id":"c1234567","task_id":"t1234567","role":"user","content":"hello","delivery_status":"pending"}"#.utf8)
        let deliveryMessage = try! JSONDecoder().decode(MordeClientMessage.self, from: deliveryMessageData)
        precondition(deliveryMessage.delivery_status == "pending")
        let conflictData = Data(#"{"error":{"code":"conflict","message":"conflict","details":{"reason":"personal_object_revision_changed","base_revision":2,"current_revision":3,"deleted":false}}}"#.utf8)
        precondition(MordePersonalRevisionConflict.decode(conflictData)?.currentRevision == 3)
        let fixedTarget = MordeTaskTargetPresentation.fixedTarget(
            executionLocation: "server", hostID: "host-1", hostName: "Mac mini", hostState: "running",
            workspaceID: "workspace-1", workspaceName: "Project", inputRevision: 8)
        precondition(fixedTarget == "云端 Agent Runtime · Mac mini（运行中） · Project v8")
        let inaccessibleTarget = MordeTaskTargetPresentation.fixedTarget(
            executionLocation: "server", hostID: "host-1", hostName: nil, hostState: nil,
            workspaceID: "workspace-1", workspaceName: nil, inputRevision: nil)
        precondition(inaccessibleTarget.contains("Host host-1（当前不可访问）"))
        precondition(inaccessibleTarget.contains("workspace-1"))
        let serverTaskWithoutHost = MordeTaskTargetPresentation.fixedTarget(
            executionLocation: "server", hostID: nil, hostName: nil, hostState: nil,
            workspaceID: nil, workspaceName: nil, inputRevision: nil)
        precondition(serverTaskWithoutHost == "云端 Agent Runtime · 无电脑目标 · 无 Workspace")
        precondition(MordeTaskTargetPresentation.fixedTarget(
            executionLocation: nil, hostID: nil, hostName: nil, hostState: nil,
            workspaceID: nil, workspaceName: nil, inputRevision: nil).hasPrefix("执行位置待核验"))

        precondition(MordeTaskResponsePolicy.hasValidTaskID("q1234567"))
        precondition(!MordeTaskResponsePolicy.hasValidTaskID("qbad"))
        precondition(MordeTaskResponsePolicy.hasValidRequestID("r1234567"))
        precondition(!MordeTaskResponsePolicy.hasValidRequestID("pending-1"))
        precondition(MordeTaskResponsePolicy.actionPath(taskID: "q1234567") == "/api/tasks/q1234567/actions")
        precondition(MordeTaskResponsePolicy.approvalPath(taskID: "q1234567", approvalID: "r1234567") ==
                     "/api/tasks/q1234567/approvals/r1234567/responses")
        precondition(MordeTaskResponsePolicy.approvalPath(taskID: "q1234567", approvalID: "../actions") == nil)
        precondition(MordeTaskResponsePolicy.inputPath(taskID: "q1234567") == "/api/tasks/q1234567/inputs")
        precondition(MordeTaskResponsePolicy.hasValidOperationID("o1234567"))
        precondition(!MordeTaskResponsePolicy.hasValidOperationID("../tasks"))
        precondition(MordeTaskResponsePolicy.mediaPreviewPath(taskID: "q1234567", operationID: "o1234567") ==
                     "/api/tasks/q1234567/media/o1234567/preview")
        precondition(MordeTaskResponsePolicy.mediaPreviewPath(taskID: "q1234567", operationID: "../tasks") == nil)
        precondition(MordeTaskResponsePolicy.hasValidAnswer("Use staging"))
        precondition(!MordeTaskResponsePolicy.hasValidAnswer("  \n"))
        precondition(!MordeTaskResponsePolicy.hasValidAnswer("a\0b"))
        precondition(!MordeTaskResponsePolicy.hasValidAnswer(String(repeating: "a", count: 16_385)))

        let approvalEndpoint = "approvals/r1234567/responses"
        let approveAction: [String: Any] = ["version": 3, "decision": "approve"]
        let approveFingerprint = MordeActionRetryAttempt.fingerprint(
            taskID: "q1234567", endpoint: approvalEndpoint, body: approveAction)!
        precondition(approveFingerprint.count == 64)
        precondition(approveFingerprint == MordeActionRetryAttempt.fingerprint(
            taskID: "q1234567", endpoint: approvalEndpoint, body: approveAction))
        precondition(approveFingerprint != MordeActionRetryAttempt.fingerprint(
            taskID: "q1234568", endpoint: approvalEndpoint, body: approveAction))
        precondition(approveFingerprint != MordeActionRetryAttempt.fingerprint(
            taskID: "q1234567", endpoint: "inputs", body: approveAction))
        let firstActionAttempt = MordeActionRetryAttempt(fingerprint: approveFingerprint, idempotencyKey: "action-retry-key")
        let repeatedActionAttempt = MordeActionRetryAttempt.reusing(firstActionAttempt, for: approveFingerprint)
        precondition(repeatedActionAttempt.idempotencyKey == "action-retry-key")
        let changedApproval = ["version": 3, "decision": "reject"] as [String: Any]
        let changedFingerprint = MordeActionRetryAttempt.fingerprint(
            taskID: "q1234567", endpoint: approvalEndpoint, body: changedApproval)!
        precondition(changedFingerprint != approveFingerprint)
        precondition(MordeTaskAttempt.canContinuePendingAttempt(
            firstActionAttempt.fingerprint, requestedFingerprint: approveFingerprint))
        precondition(!MordeTaskAttempt.canContinuePendingAttempt(
            firstActionAttempt.fingerprint, requestedFingerprint: changedFingerprint))
        precondition(MordeActionRetryAttempt.reusing(firstActionAttempt, for: changedFingerprint).idempotencyKey != "action-retry-key")

        precondition(MordeClientVersion.meetsMinimum(current: "1.2.0", minimum: "1.1.9"))
        precondition(MordeClientVersion.meetsMinimum(current: "1.2.0", minimum: "1.2.0"))
        precondition(!MordeClientVersion.meetsMinimum(current: "1.1.9", minimum: "1.2.0"))
        precondition(!MordeClientVersion.meetsMinimum(current: "1.2.0-beta.1", minimum: "1.2.0"))
        precondition(MordeClientVersion.meetsMinimum(current: "1.2.0", minimum: "1.2.0-beta.9"))
        precondition(MordeClientVersion.meetsMinimum(current: "1.2.0-beta.12", minimum: "1.2.0-beta.9"))
        precondition(MordeClientVersion.meetsMinimum(current: "1.2.3+build.8", minimum: "1.2.3"))
        precondition(!MordeClientVersion.meetsMinimum(current: "invalid", minimum: "1.2.0"))
        precondition(!MordeClientVersion.meetsMinimum(current: "", minimum: "1.2.0"))

        precondition(MordeTaskSnapshotCursor.resumeCursor(eventID: "7", snapshotSequence: 9) == "9")
        precondition(MordeTaskSnapshotCursor.resumeCursor(eventID: "9", snapshotSequence: 7) == nil)
        precondition(MordeTaskSnapshotCursor.resumeCursor(eventID: "-1", snapshotSequence: 0) == nil)
        let assistantStatusValues = Set((contract["assistant_message_statuses"] as? [String]) ?? [])
        precondition(assistantStatusValues == MordeTaskSnapshotMessageMerge.statuses)
        precondition(MordeTaskSnapshotMessageMerge.shouldIncludeSnapshot(resultMessagePending: nil, resultMessagePublished: nil, status: "completed"))
        precondition(!MordeTaskSnapshotMessageMerge.shouldIncludeSnapshot(resultMessagePending: true, resultMessagePublished: false, status: "completed"))
        precondition(!MordeTaskSnapshotMessageMerge.shouldIncludeSnapshot(resultMessagePending: false, resultMessagePublished: true, status: "completed"))
        precondition(MordeTaskSnapshotMessageMerge.shouldIncludeSnapshot(resultMessagePending: false, resultMessagePublished: false, status: "completed"))
        precondition(MordeTaskSnapshotMessageMerge.shouldIncludeSnapshot(resultMessagePending: true, resultMessagePublished: false, status: "streaming"))
        precondition(!MordeTaskSnapshotMessageMerge.shouldIncludeSnapshot(resultMessagePending: false, resultMessagePublished: true, status: "streaming"))
        precondition(MordeTaskSnapshotMessageMerge.shouldIncludeSnapshot(resultMessagePending: true, resultMessagePublished: false, status: "interrupted"))
        precondition(MordeTaskSnapshotMessageMerge.shouldIncludeSnapshot(resultMessagePending: false, resultMessagePublished: true, status: "interrupted"))
        let interruptedRevision = MordeClientMessage(id: "old-interrupted", conversation_id: "c1234567",
            task_id: "q1234567", role: "assistant", content: "Earlier partial reply", status: "interrupted",
            task_revision: 1, result_event_sequence: 10)
        let currentRevision = MordeClientMessage(id: "new-completed", conversation_id: "c1234567",
            task_id: "q1234567", role: "assistant", content: "Current reply", status: "completed", task_revision: 2)
        precondition(!MordeTaskSnapshotMessageMerge.shouldDiscardPublishedSynthetic(interruptedRevision,
            taskID: "q1234567", currentRevision: 2, published: true))
        precondition(MordeTaskSnapshotMessageMerge.shouldDiscardPublishedSynthetic(currentRevision,
            taskID: "q1234567", currentRevision: 2, published: true))
        precondition(!MordeTaskSnapshotMessageMerge.shouldDiscardPublishedSynthetic(currentRevision,
            taskID: "q1234567", currentRevision: 3, published: true))
        precondition(!MordeTaskSnapshotMessageMerge.shouldDiscardPublishedSynthetic(currentRevision,
            taskID: "q1234567", currentRevision: nil, published: true))
        let withBothRevisions = MordeTaskSnapshotMessageMerge.merge([interruptedRevision], snapshot: currentRevision,
            taskID: "q1234567", conversationID: "c1234567")
        precondition(Set(withBothRevisions.map(\.id)) == ["old-interrupted", "new-completed"])
        let newSource = MordeClientMessageSource(user_message_id: "m1234567", task_id: "q1234567",
                                                 task_revision: 2, event_sequence: 20, content: "Current reply")
        let newGrouped = MordeClientMessage(id: "new-grouped", conversation_id: "c1234567", sequence: 5,
            role: "assistant", content: "Current reply", sources: [newSource])
        let keptDuringRefresh = MordeTaskSnapshotMessageMerge.preservingInterrupted([interruptedRevision],
            in: [newGrouped], conversationID: "c1234567")
        precondition(Set(keptDuringRefresh.map(\.id)) == ["old-interrupted", "new-grouped"])
        let oldSource = MordeClientMessageSource(user_message_id: "m1234567", task_id: "q1234567",
                                                 task_revision: 1, event_sequence: 10, content: "Earlier partial reply")
        let oldGrouped = MordeClientMessage(id: "old-grouped", conversation_id: "c1234567", sequence: 6,
            role: "assistant", content: "Earlier partial reply", sources: [oldSource])
        let coveredDuringRefresh = MordeTaskSnapshotMessageMerge.preservingInterrupted([interruptedRevision],
            in: [oldGrouped], conversationID: "c1234567")
        precondition(Set(coveredDuringRefresh.map(\.id)) == ["old-interrupted", "old-grouped"])
        let canonicalOld = MordeClientMessage(id: "old-interrupted", conversation_id: "c1234567", sequence: 7,
            task_id: "q1234567", role: "assistant", content: "Authoritative reply")
        precondition(MordeTaskSnapshotMessageMerge.preservingInterrupted([interruptedRevision], in: [canonicalOld],
            conversationID: "c1234567").first?.content == "Authoritative reply")
        let persistedStream = MordeClientMessage(id: "mstream1", conversation_id: "c1234567", sequence: 8,
            task_id: "q1234567", role: "assistant", content: "First", output_status: "streaming", output_task_revision: 2)
        let wireOutput = Data(#"{"id":"mstream1","conversation_id":"c1234567","sequence":8,"task_id":"q1234567","role":"assistant","content":"Final","output_status":"completed","output_task_revision":2,"output_result_event_sequence":20}"#.utf8)
        let decodedOutput = try! JSONDecoder().decode(MordeClientMessage.self, from: wireOutput)
        precondition(decodedOutput.displayStatus == "completed" && decodedOutput.displayTaskRevision == 2 &&
                     decodedOutput.displayResultEventSequence == 20 && decodedOutput.sequence == 8)
        let streamSnapshot = MordeClientMessage(id: "mstream1", conversation_id: "c1234567",
            task_id: "q1234567", role: "assistant", content: "First and second", status: "streaming", task_revision: 2)
        let mergedStream = MordeTaskSnapshotMessageMerge.merge([persistedStream], snapshot: streamSnapshot,
            taskID: "q1234567", conversationID: "c1234567")
        precondition(mergedStream.count == 1 && mergedStream[0].id == "mstream1" && mergedStream[0].sequence == 8 &&
                     mergedStream[0].content == "First and second" && mergedStream[0].displayStatus == "streaming" &&
                     mergedStream[0].displayTaskRevision == 2)
        let wrongConversation = MordeClientMessage(id: "mstream1", conversation_id: "c7654321",
            task_id: "q1234567", role: "assistant", content: "First and secret", status: "streaming", task_revision: 2)
        precondition(MordeTaskSnapshotMessageMerge.merge([persistedStream], snapshot: wrongConversation,
            taskID: "q1234567", conversationID: "c1234567") == [persistedStream])
        let sameIDUser = MordeClientMessage(id: "mstream1", conversation_id: "c1234567", sequence: 8,
            role: "user", content: "Original user message")
        precondition(MordeTaskSnapshotMessageMerge.merge([sameIDUser], snapshot: streamSnapshot,
            taskID: "q1234567", conversationID: "c1234567") == [sameIDUser])
        let divergentSnapshot = MordeClientMessage(id: "mstream1", conversation_id: "c1234567",
            task_id: "q1234567", role: "assistant", content: "Different text", status: "streaming", task_revision: 2)
        precondition(MordeTaskSnapshotMessageMerge.merge([persistedStream], snapshot: divergentSnapshot,
            taskID: "q1234567", conversationID: "c1234567")[0].content == "First")
        let unversionedSnapshot = MordeClientMessage(id: "mstream1", conversation_id: "c1234567",
            task_id: "q1234567", role: "assistant", content: "First and second", status: "streaming")
        precondition(MordeTaskSnapshotMessageMerge.merge([persistedStream], snapshot: unversionedSnapshot,
            taskID: "q1234567", conversationID: "c1234567")[0].content == "First")
        let finalStream = MordeClientMessage(id: "mstream1", conversation_id: "c1234567", sequence: 8,
            task_id: "q1234567", role: "assistant", content: "Final", output_status: "completed",
            output_task_revision: 2, output_result_event_sequence: 20)
        let staleStream = MordeTaskSnapshotMessageMerge.merge([finalStream], snapshot: streamSnapshot,
            taskID: "q1234567", conversationID: "c1234567")
        precondition(staleStream.count == 1 && staleStream[0].content == "Final" && staleStream[0].displayStatus == "completed")
        let interruptedStream = MordeClientMessage(id: "mstream1", conversation_id: "c1234567", sequence: 8,
            task_id: "q1234567", role: "assistant", content: "Interrupted", output_status: "interrupted", output_task_revision: 2)
        let staleInterrupted = MordeTaskSnapshotMessageMerge.merge([interruptedStream], snapshot: streamSnapshot,
            taskID: "q1234567", conversationID: "c1234567")
        precondition(staleInterrupted.count == 1 && staleInterrupted[0].content == "Interrupted" &&
                     staleInterrupted[0].displayStatus == "interrupted")
        precondition(MordeTaskSnapshotMessageMerge.sourceCoversSnapshot(newSource, snapshot: finalStream, taskID: "q1234567"))
        precondition(!MordeTaskSnapshotMessageMerge.sourceCoversSnapshot(oldSource, snapshot: finalStream, taskID: "q1234567"))
        precondition(MordeTaskSnapshotMessageMerge.deduplicatingPublished([finalStream, newGrouped]).map(\.id) == ["new-grouped"])
        precondition(MordeTaskSnapshotMessageMerge.deduplicatingPublished([finalStream, oldGrouped]).count == 2)
        precondition(MordeTaskSnapshotMessageMerge.deduplicatingPublished([interruptedStream, newGrouped]).count == 2)
        precondition(MordeTaskSnapshotMessageMerge.shouldShowTaskResult(resultMessagePending: nil, resultMessagePublished: nil))
        precondition(!MordeTaskSnapshotMessageMerge.shouldShowTaskResult(resultMessagePending: true, resultMessagePublished: false))
        precondition(!MordeTaskSnapshotMessageMerge.shouldShowTaskResult(resultMessagePending: false, resultMessagePublished: true))
        precondition(MordeTaskSnapshotMessageMerge.shouldShowTaskResult(resultMessagePending: false, resultMessagePublished: false))
        let assistantSnapshotFixture = fixtureObject(taskSnapshot["assistant_message"], label: "task_snapshot.assistant_message")
        let assistantSnapshotData = try! JSONSerialization.data(withJSONObject: assistantSnapshotFixture)
        let streamingAssistant = try! JSONDecoder().decode(MordeClientMessage.self, from: assistantSnapshotData)
        precondition(streamingAssistant.status == "streaming")
        let chronologicalMessages = MordeConversationMessageOrder.ascending([
            MordeClientMessage(id: "m2345678", sequence: 2, role: "assistant", content: "Reply",
                               created_at: "2026-11-06T09:00:00Z"),
            MordeClientMessage(id: "m1234567", sequence: 1, role: "user", content: "Question",
                               created_at: "2026-11-06T09:01:00Z"),
        ], sequence: { $0.sequence }, createdAt: { $0.created_at }, id: { $0.id })
        precondition(chronologicalMessages.map(\.sequence) == [1, 2],
                     "Conversation history must follow server sequence, not reverse-page or timestamp order")
        let serverFirst = MordeClientMessage(id: "m1111111", sequence: 1, role: "user", content: "first",
                                             created_at: "2026-11-06T09:02:00Z")
        let serverSecond = MordeClientMessage(id: "m2222222", sequence: 2, role: "assistant", content: "second",
                                              created_at: "2026-11-06T09:00:00Z")
        let localMiddle = MordeClientMessage(id: "pending-1", role: "user", content: "pending",
                                             created_at: "2026-11-06T09:01:00Z")
        let skewedInputs = [[serverFirst, serverSecond, localMiddle], [localMiddle, serverFirst, serverSecond],
                            [serverSecond, localMiddle, serverFirst], [serverSecond, serverFirst, localMiddle],
                            [serverFirst, localMiddle, serverSecond], [localMiddle, serverSecond, serverFirst]]
        for input in skewedInputs {
            let ordered = MordeConversationMessageOrder.ascending(input,
                sequence: { $0.sequence }, createdAt: { $0.created_at }, id: { $0.id })
            precondition(ordered.map(\.id) == ["pending-1", "m1111111", "m2222222"])
        }
        let sameTimeLocal = [MordeClientMessage(id: "pending-z", role: "user", content: "first",
                                                created_at: "2026-11-06T09:00:00.000Z"),
                             MordeClientMessage(id: "pending-a", role: "user", content: "second",
                                                created_at: "2026-11-06T09:00:00.000Z")]
        precondition(MordeConversationMessageOrder.ascending(sameTimeLocal,
            sequence: { $0.sequence }, createdAt: { $0.created_at }, id: { $0.id }).map(\.id) == ["pending-z", "pending-a"])
        let streamingMessages = MordeTaskSnapshotMessageMerge.merge(
            [], snapshot: streamingAssistant, taskID: "q1234567", conversationID: "c1234567")
        precondition(streamingMessages == [streamingAssistant])
        let nextDelta = MordeClientMessage(
            id: streamingAssistant.id, conversation_id: "c1234567", task_id: "q1234567",
            role: "assistant", content: "Hello world", status: "streaming", created_at: streamingAssistant.created_at)
        let accumulatedMessages = MordeTaskSnapshotMessageMerge.merge(
            streamingMessages, snapshot: nextDelta, taskID: "q1234567", conversationID: "c1234567")
        precondition(accumulatedMessages.count == 1 && accumulatedMessages[0].content == "Hello world")
        let completedAssistant = MordeClientMessage(
            id: streamingAssistant.id, conversation_id: "c1234567", task_id: "q1234567",
            role: "assistant", content: "Hello world!", status: "completed", created_at: streamingAssistant.created_at)
        let completedMessages = MordeTaskSnapshotMessageMerge.merge(
            accumulatedMessages, snapshot: completedAssistant, taskID: "q1234567", conversationID: "c1234567")
        precondition(completedMessages.count == 1 && completedMessages[0].id == streamingAssistant.id)
        precondition(completedMessages[0].content == "Hello world!" && completedMessages[0].status == "completed")
        let deliveredCanonical = MordeClientMessage(
            id: streamingAssistant.id, conversation_id: "c1234567", task_id: "q1234567",
            role: "assistant", content: "Canonical final text", delivery_status: "applied",
            created_at: streamingAssistant.created_at)
        let completedSnapshotMerge = MordeTaskSnapshotMessageMerge.merge(
            [deliveredCanonical], snapshot: completedAssistant, taskID: "q1234567", conversationID: "c1234567")
        precondition(completedSnapshotMerge[0].content == "Hello world!" &&
                     completedSnapshotMerge[0].status == "completed" &&
                     completedSnapshotMerge[0].delivery_status == "applied")
        let canonicalMessage = MordeClientMessage(
            id: streamingAssistant.id, conversation_id: "c1234567", task_id: "q1234567",
            role: "assistant", content: "Canonical final text", created_at: streamingAssistant.created_at)
        let staleSnapshotMerge = MordeTaskSnapshotMessageMerge.merge(
            [canonicalMessage], snapshot: streamingAssistant, taskID: "q1234567", conversationID: "c1234567")
        precondition(staleSnapshotMerge.count == 1 && staleSnapshotMerge[0].content == "Canonical final text")
        precondition(staleSnapshotMerge[0].status == "completed")
        let interruptedAssistant = MordeClientMessage(
            id: "m7654321", conversation_id: "c1234567", task_id: "q1234567",
            role: "assistant", content: "Partial reply", status: "interrupted", created_at: streamingAssistant.created_at)
        let interruptedMessages = MordeTaskSnapshotMessageMerge.merge(
            [], snapshot: interruptedAssistant, taskID: "q1234567", conversationID: "c1234567")
        precondition(interruptedMessages.count == 1 && interruptedMessages[0].status == "interrupted")
        let wrongTaskMerge = MordeTaskSnapshotMessageMerge.merge(
            [], snapshot: streamingAssistant, taskID: "q7654321", conversationID: "c1234567")
        precondition(wrongTaskMerge.isEmpty)

        let groupedSource = MordeClientMessageSource(user_message_id: "m0000001", task_id: "q1234567",
                                                    task_revision: 2, event_sequence: 7, content: "完成")
        let groupedReply = MordeClientMessage(id: "m0000003", conversation_id: "c1234567", role: "assistant",
                                              content: "完成", sources: [groupedSource])
        let coveredSnapshot = MordeClientMessage(id: "m0000002", conversation_id: "c1234567", task_id: "q1234567",
            role: "assistant", content: "完成", status: "completed", task_revision: 2, result_event_sequence: 7)
        let coveredMerge = MordeTaskSnapshotMessageMerge.merge([groupedReply], snapshot: coveredSnapshot,
                                                              taskID: "q1234567", conversationID: "c1234567")
        precondition(coveredMerge == [groupedReply])
        let newerSnapshot = MordeClientMessage(id: "m0000004", conversation_id: "c1234567", task_id: "q1234567",
            role: "assistant", content: "新结果", status: "completed", task_revision: 3, result_event_sequence: 8)
        precondition(MordeTaskSnapshotMessageMerge.merge([groupedReply], snapshot: newerSnapshot,
            taskID: "q1234567", conversationID: "c1234567").count == 2)
        let unprovenSnapshot = MordeClientMessage(id: "m0000005", conversation_id: "c1234567", task_id: "q1234567",
            role: "assistant", content: "未核对", status: "completed")
        precondition(MordeTaskSnapshotMessageMerge.merge([groupedReply], snapshot: unprovenSnapshot,
            taskID: "q1234567", conversationID: "c1234567").count == 2)

        let scope = MordeClientDataScope(serviceOrigin: "https://morde.example", serviceInstanceID: serviceA,
                                         accountID: "user-1", installationID: "install-1")!
        let pendingSend = MordeRetryAttempt(scope: scope, fingerprint: taskAttempt, key: "retry-key")
        let restoredSend = try! JSONDecoder().decode(MordeRetryAttempt.self, from: JSONEncoder().encode(pendingSend))
        precondition(restoredSend == pendingSend)
        precondition(restoredSend.matches(scope: scope, fingerprint: taskAttempt))
        let otherAccountScope = MordeClientDataScope(serviceOrigin: "https://morde.example", serviceInstanceID: serviceA,
                                                      accountID: "user-2", installationID: "install-1")!
        let rebuiltServiceScope = MordeClientDataScope(serviceOrigin: "https://morde.example", serviceInstanceID: serviceB,
                                                       accountID: "user-1", installationID: "install-1")!
        let otherInstallationScope = MordeClientDataScope(serviceOrigin: "https://morde.example", serviceInstanceID: serviceA,
                                                          accountID: "user-1", installationID: "install-2")!
        precondition(MordeOfflineConversationPolicy.mayRestore(scope, hasActiveInstallation: true,
            origin: scope.serviceOrigin, instanceID: scope.serviceInstanceID, accountID: scope.accountID))
        precondition(!MordeOfflineConversationPolicy.mayRestore(scope, hasActiveInstallation: true,
            origin: scope.serviceOrigin, instanceID: scope.serviceInstanceID, accountID: "user-2"))
        precondition(MordeOfflineConversationPolicy.mayRestore(scope, hasActiveInstallation: true,
            origin: scope.serviceOrigin, instanceID: scope.serviceInstanceID, accountID: scope.accountID))
        precondition(!MordeOfflineConversationPolicy.mayRestore(scope, hasActiveInstallation: true,
            origin: scope.serviceOrigin, instanceID: UUID().uuidString, accountID: scope.accountID))
        precondition(!MordeOfflineConversationPolicy.mayRestore(scope, hasActiveInstallation: true,
            origin: scope.serviceOrigin, instanceID: scope.serviceInstanceID, accountID: nil))
        precondition(MordeOfflineConversationPolicy.mayRestore(scope, hasActiveInstallation: false,
            origin: nil, instanceID: nil, accountID: nil))
        let outboxRoot = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        defer { try? FileManager.default.removeItem(at: outboxRoot) }
        let outboxSuite = "MordeTaskOutboxTests-\(UUID().uuidString)"
        let outboxDefaults = UserDefaults(suiteName: outboxSuite)!
        outboxDefaults.removePersistentDomain(forName: outboxSuite)
        defer { outboxDefaults.removePersistentDomain(forName: outboxSuite) }
        let messageOutbox = MordeMessageOutbox(root: outboxRoot)
        let unconfirmedStore = MordeUnconfirmedMessageStore(root: outboxRoot)
        let invalidAttachment = MordeOutboxAttachment(id: "local-slot-001", attachmentID: nil,
                                                      localPath: "/local/synthetic.txt", linked: false)
        let unconfirmed = MordeUnconfirmedMessage(id: "unconfirmed-001", serviceOrigin: scope.serviceOrigin,
            serviceInstanceID: scope.serviceInstanceID, accountName: "alice", accountID: scope.accountID,
            conversationID: "c1234567", projectID: nil, content: "本机保存的文字", modelID: nil,
            referenceTaskID: nil, hostID: nil, workspaceID: nil,
            attachmentIDs: ["bad-id", "bad-id"], stagedAttachments: [invalidAttachment],
            pendingUploadPaths: ["/local/synthetic.txt"], createdAt: "2026-11-07T00:00:00.000Z",
            reason: "附件待处理")
        try! unconfirmedStore.append(unconfirmed)
        precondition(try! MordeUnconfirmedMessageStore(root: outboxRoot).messages() == [unconfirmed])
        let updatedUnconfirmed = try! unconfirmedStore.updateAttachmentStatus(id: unconfirmed.id, status: "附件保存失败")
        precondition(updatedUnconfirmed.first?.content == "本机保存的文字")
        precondition(updatedUnconfirmed.first?.localAttachmentStatus == "附件保存失败")
        precondition(updatedUnconfirmed.first?.stagedAttachments == [invalidAttachment])
        precondition(unconfirmed.belongsTo(origin: scope.serviceOrigin, accountName: "alice", accountID: scope.accountID))
        precondition(!unconfirmed.belongsTo(origin: scope.serviceOrigin, accountName: "bob", accountID: "other-user"))
        precondition(unconfirmed.belongsTo(origin: scope.serviceOrigin, accountName: "alice", accountID: scope.accountID))
        precondition(unconfirmed.matchesInstance(scope.serviceInstanceID))
        precondition(!unconfirmed.matchesInstance(UUID().uuidString.lowercased()))
        precondition(!unconfirmed.belongsTo(origin: "https://different.example", accountName: "alice", accountID: scope.accountID))
        let unknownIdentity = MordeUnconfirmedMessage(id: "unconfirmed-002", serviceOrigin: scope.serviceOrigin,
            serviceInstanceID: scope.serviceInstanceID, accountName: "alice", accountID: nil, installationID: "install-a",
            conversationID: nil, projectID: nil, content: "未认领文字", modelID: nil,
            referenceTaskID: nil, hostID: nil, workspaceID: nil, attachmentIDs: [], stagedAttachments: [],
            pendingUploadPaths: [], createdAt: unconfirmed.createdAt, reason: "身份待确认")
        try! unconfirmedStore.append(unknownIdentity)
        precondition(unknownIdentity.belongsTo(origin: scope.serviceOrigin, accountName: "alice", accountID: nil))
        precondition(!unknownIdentity.belongsTo(origin: scope.serviceOrigin, accountName: "bob", accountID: "other-user"))
        precondition(unknownIdentity.belongsTo(origin: scope.serviceOrigin, accountName: "alice", accountID: scope.accountID))
        precondition(!unknownIdentity.matchesInstance(UUID().uuidString.lowercased()))
        precondition(unknownIdentity.matchesInstallation("install-a"))
        precondition(!unknownIdentity.matchesInstallation("install-b"))
        precondition(unknownIdentity.matchesInstallation("install-a"))
        precondition(unconfirmed.matchesInstallation("renewed-installation"))
        precondition(try! MordeUnconfirmedMessageStore(root: outboxRoot).messages() == updatedUnconfirmed + [unknownIdentity])
        precondition(!MordeMessageAttachmentValidation.isValid(attachmentIDs: unconfirmed.attachmentIDs,
            stagedAttachments: [invalidAttachment], projectID: nil, personalAttachmentIsValid: { $0.hasPrefix("pa_") }))
        precondition(!MordeMessageAttachmentValidation.isValid(attachmentIDs: [],
            stagedAttachments: Array(repeating: invalidAttachment, count: 17), projectID: nil, personalAttachmentIsValid: { _ in true }))
        precondition(!MordeMessageAttachmentValidation.isValid(attachmentIDs: [],
            stagedAttachments: [invalidAttachment, invalidAttachment], projectID: "j1234567", personalAttachmentIsValid: { _ in true }))
        precondition(MordeMessageAttachmentValidation.isValid(attachmentIDs: [],
            stagedAttachments: [invalidAttachment], projectID: "j1234567", personalAttachmentIsValid: { _ in true }))
        precondition(try! messageOutbox.messages(scope: scope).isEmpty)
        try! unconfirmedStore.remove(id: unconfirmed.id)
        precondition(try! MordeUnconfirmedMessageStore(root: outboxRoot).messages() == [unknownIdentity])
        try! unconfirmedStore.remove(id: unknownIdentity.id)
        precondition(try! MordeUnconfirmedMessageStore(root: outboxRoot).messages().isEmpty)
        let configurationCheck = Date(timeIntervalSince1970: 1_000)
        let adjustment = try! JSONDecoder().decode(MordeClientMessage.self, from: Data(
            #"{"id":"m1234567","role":"user","content":"change","routing_status":"waiting_adjustment"}"#.utf8))
        precondition(MordeRoutingPresentation.localizationKey(adjustment.routing_status) == "Adjustment received; waiting to update")
        precondition(MordeRoutingPresentation.localizationKey("waiting_user") == nil)
        precondition(MordeRoutingPresentation.localizationKey("routed") == nil)
        let failedMessage = try! JSONDecoder().decode(MordeClientMessage.self, from: Data(
            #"{"id":"m1234567","role":"user","content":"change","routing_status":"failed","routing_failure_reason":"model_unavailable"}"#.utf8))
        precondition(MordeRoutingPresentation.failureLocalizationKey(failedMessage.routing_failure_reason) == "Model temporarily unavailable")
        precondition(MordeRoutingPresentation.failureLocalizationKey("routing_context_unavailable") == "Task context unavailable")
        precondition(MordeRoutingPresentation.failureLocalizationKey("routing_action_unavailable") == "Unable to update the task")
        precondition(MordeRoutingPresentation.failureLocalizationKey("message_too_long") ==
            "Message and context exceed the model input limit; the original is retained. Shorten and send again.")
        precondition(!MordeRoutingPresentation.canRetryProcessing("message_too_long"))
        precondition(MordeRoutingPresentation.canRetryProcessing("model_unavailable"))
        precondition(MordeTaskStatusPresentation.label(status: "waiting_user", waitingReason: "model_context_limit") ==
            "消息及上下文超过当前模型输入上限，原文已保留；请缩短后重新发送。")
        precondition(MordeTaskStatusPresentation.localizationKey(status: "waiting_user", waitingReason: "model_context_limit") ==
            "Message and context exceed the model input limit; the original is retained. Shorten and send again.")
        precondition(MordeTaskStatusPresentation.localizationKey(status: "waiting_user", waitingReason: "computer_target_missing") ==
            "Choose a computer to continue this task")
        precondition(MordeTaskStatusPresentation.localizationKey(status: "waiting_user", waitingReason: "computer_unavailable") ==
            "Required cloud computer is unavailable")
        precondition(MordeTaskStatusPresentation.localizationKey(status: "waiting_user", waitingReason: "workspace_unavailable") ==
            "Required workspace is unavailable")
        precondition(MordeTaskStatusPresentation.localizationKey(status: "waiting_user", waitingReason: "target_result_unknown") ==
            "Computer operation result needs review")
        precondition(MordeTaskStatusPresentation.label(status: "waiting_user", waitingReason: "computer_target_changed") ==
            "任务已绑定其他云端电脑，无法安全切换；请为新目标另发起任务。")
        precondition(MordeTaskStatusPresentation.label(status: "waiting_user", waitingReason: "workspace_target_changed") ==
            "任务已绑定其他工作区，无法安全切换；请为新目标另发起任务。")
        precondition(MordeRoutingPresentation.failureLocalizationKey(nil) == "Message processing failed")
        let retryStorage = MordeRoutingRetryAttempt.storageKey(scope: scope, projectID: nil,
                                                              conversationID: "c1234567", messageID: "m1234567")
        let retryAttempt = try! messageOutbox.beginRoutingRetry(storageKey: retryStorage, scope: scope)
        precondition(try! messageOutbox.beginRoutingRetry(storageKey: retryStorage, scope: otherInstallationScope) == retryAttempt)
        precondition(retryStorage != MordeRoutingRetryAttempt.storageKey(scope: otherAccountScope, projectID: nil,
                                                                        conversationID: "c1234567", messageID: "m1234567"))
        precondition(retryStorage != MordeRoutingRetryAttempt.storageKey(scope: rebuiltServiceScope, projectID: nil,
                                                                        conversationID: "c1234567", messageID: "m1234567"))
        precondition(retryStorage == MordeRoutingRetryAttempt.storageKey(scope: otherInstallationScope, projectID: nil,
                                                                        conversationID: "c1234567", messageID: "m1234567"))
        try! messageOutbox.finishRoutingRetry(storageKey: retryStorage, attempt: "other-attempt", scope: scope)
        precondition(try! messageOutbox.beginRoutingRetry(storageKey: retryStorage, scope: scope) == retryAttempt)
        let reopenedRetryStore = MordeMessageOutbox(root: outboxRoot)
        precondition(try! reopenedRetryStore.beginRoutingRetry(storageKey: retryStorage, scope: scope) == retryAttempt)
        try! messageOutbox.finishRoutingRetry(storageKey: retryStorage, attempt: retryAttempt, scope: scope)
        precondition(try! messageOutbox.beginRoutingRetry(storageKey: retryStorage, scope: scope) != retryAttempt)
        precondition(MordeCloudModelConfiguration.isConfirmedAbsent(status: "not_configured", loaded: true,
                                                                   checkedAt: configurationCheck, now: configurationCheck))
        precondition(!MordeCloudModelConfiguration.isConfirmedAbsent(status: "unknown", loaded: true,
                                                                    checkedAt: configurationCheck, now: configurationCheck))
        precondition(!MordeCloudModelConfiguration.isConfirmedAbsent(status: "not_configured", loaded: false,
                                                                    checkedAt: configurationCheck, now: configurationCheck))
        precondition(!MordeCloudModelConfiguration.isConfirmedAbsent(status: "not_configured", loaded: true,
                                                                    checkedAt: configurationCheck,
                                                                    now: configurationCheck.addingTimeInterval(301)))
        let longMixedText = String(repeating: "中文a", count: 10_000)
        let longBytes = Data(longMixedText.utf8)
        precondition(longBytes.count > MordeMessageChunking.directLimit)
        let chunks = (0..<MordeMessageChunking.count(for: longBytes)).map {
            MordeMessageChunking.chunk(longBytes, sequence: $0)!
        }
        precondition(chunks.allSatisfy { $0.count <= MordeMessageChunking.chunkSize })
        precondition(chunks.reduce(into: Data()) { $0.append($1) } == longBytes)
        precondition(MordeMessageChunking.chunk(longBytes, sequence: -1) == nil)
        precondition(MordeMessageChunking.chunk(longBytes, sequence: chunks.count) == nil)
        precondition(MordeMessageChunking.digest(Data("abc".utf8)) ==
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
        let wireOutbox = MordeMessageOutbox(root: outboxRoot.appendingPathComponent("wire-fixture"))
        let wireFirst = try! wireOutbox.enqueue(scope: scope, conversationID: "c1234567", content: "第一条",
                                                modelID: nil, attachmentIDs: [])
        try! wireOutbox.update(id: wireFirst.id, scope: scope) { $0.state = .sending }
        let wireSecond = try! wireOutbox.enqueue(scope: scope, conversationID: "c1234567", content: "  第二条\n保留原文  ",
            modelID: "p1234567", attachmentIDs: [], referenceTaskID: "q1234567", hostID: "h1234567",
            workspaceID: "w1234567", stagedAttachments: [MordeOutboxAttachment(id: "local-slot-001", attachmentID: nil, linked: false)])
        let wireLong = try! wireOutbox.enqueue(scope: scope, conversationID: "c1234567", content: longMixedText,
            modelID: "p1234567", attachmentIDs: [], referenceTaskID: "q1234567", hostID: "h1234567",
            workspaceID: "w1234567", stagedAttachments: [MordeOutboxAttachment(id: "local-slot-002", attachmentID: nil, linked: false)])
        let otherConversation = try! wireOutbox.enqueue(scope: scope, conversationID: "c7654321", content: "另一个会话",
                                                       modelID: nil, attachmentIDs: [])
        let delayed = try! wireOutbox.messages(scope: scope)
        precondition(delayed.map(\.id) == [wireFirst.id, wireSecond.id, wireLong.id, otherConversation.id])
        precondition(MordeOutboxScheduling.readyIDs(delayed, scope: scope, inFlight: [wireFirst.id]) == [otherConversation.id])
        precondition(MordeOutboxScheduling.waitingForEarlierMessage(wireSecond, in: delayed) == "Waiting for the previous message to be received.")
        precondition(MordeOutboxScheduling.readyIDs(delayed, scope: otherAccountScope, inFlight: []).isEmpty)

        try! wireOutbox.update(id: wireFirst.id, scope: scope) {
            $0.state = .accepted
            $0.serverMessageID = "m1234567"
        }
        let firstReceived = try! wireOutbox.messages(scope: scope)
        precondition(MordeOutboxScheduling.readyIDs(firstReceived, scope: scope, inFlight: [wireFirst.id]) ==
            [wireSecond.id, otherConversation.id])
        precondition(MordeOutboxScheduling.waitingForEarlierMessage(wireSecond, in: firstReceived) == nil)
        try! wireOutbox.update(id: wireSecond.id, scope: scope) {
            $0.state = .accepted
            $0.serverMessageID = "m2345678"
        }
        let secondReceivedWithAttachmentPending = try! wireOutbox.messages(scope: scope)
        precondition(secondReceivedWithAttachmentPending[1].attachments.first?.linked == false)
        precondition(MordeOutboxScheduling.readyIDs(secondReceivedWithAttachmentPending, scope: scope,
            inFlight: [wireFirst.id, wireSecond.id]) == [wireLong.id, otherConversation.id])

        var failedFirst = delayed[0]
        failedFirst.state = .failed
        failedFirst.confirmedNotAccepted = true
        let failedQueue = [failedFirst, wireSecond, wireLong, otherConversation]
        precondition(MordeOutboxScheduling.readyIDs(failedQueue, scope: scope, inFlight: []) == [otherConversation.id])
        precondition(MordeOutboxScheduling.waitingForEarlierMessage(wireSecond, in: failedQueue) ==
            "The previous message failed to send. Please handle it first.")
        var unverifiedFirst = failedFirst
        unverifiedFirst.confirmedNotAccepted = nil
        precondition(MordeOutboxScheduling.waitingForEarlierMessage(wireSecond, in: [unverifiedFirst, wireSecond]) ==
            "The previous message’s delivery could not be verified. Retry or check it first.")
        var unknownFirst = delayed[0]
        unknownFirst.state = .unknown
        let uncertainQueue = [unknownFirst, wireSecond, wireLong, otherConversation]
        precondition(MordeOutboxScheduling.readyIDs(uncertainQueue, scope: scope, inFlight: []) ==
            [wireFirst.id, otherConversation.id])
        precondition(MordeOutboxScheduling.waitingForEarlierMessage(wireSecond, in: uncertainQueue) ==
            "The previous message’s delivery could not be verified. Retry or check it first.")
        for code in [400, 403, 404, 413, 422] {
            precondition(MordeOutboxFailurePolicy.confirmsRejection(submissionBegan: true, firstSubmission: true,
                hadUncertainSubmission: false, domain: "Morde", code: code))
        }
        for code in [401, 408, 409, 425, 429, 500] {
            precondition(!MordeOutboxFailurePolicy.confirmsRejection(submissionBegan: true, firstSubmission: true,
                hadUncertainSubmission: false, domain: "Morde", code: code))
        }
        precondition(!MordeOutboxFailurePolicy.confirmsRejection(submissionBegan: false, firstSubmission: true,
            hadUncertainSubmission: false, domain: "Morde", code: 413))
        precondition(!MordeOutboxFailurePolicy.confirmsRejection(submissionBegan: true, firstSubmission: false,
            hadUncertainSubmission: false, domain: "Morde", code: 413))
        precondition(!MordeOutboxFailurePolicy.confirmsRejection(submissionBegan: true, firstSubmission: false,
            hadUncertainSubmission: true, domain: "Morde", code: 413))
        precondition(!MordeOutboxFailurePolicy.confirmsRejection(submissionBegan: true, firstSubmission: true,
            hadUncertainSubmission: false, domain: NSURLErrorDomain, code: 413))
        precondition(MordeOutboxFailurePolicy.state(mayHaveBeenAccepted: true, confirmedRejection: true,
            domain: "Morde", code: 422) == .failed)
        precondition(MordeOutboxFailurePolicy.state(mayHaveBeenAccepted: true, confirmedRejection: false,
            domain: "Morde", code: 422) == .unknown)
        precondition(MordeOutboxFailurePolicy.state(mayHaveBeenAccepted: true, confirmedRejection: false,
            domain: "Morde", code: 404) == .unknown)
        precondition(MordeOutboxFailurePolicy.state(mayHaveBeenAccepted: false, confirmedRejection: false,
            domain: "Morde", code: 404) == .failed)
        precondition(MordeOutboxFailurePolicy.state(mayHaveBeenAccepted: false, confirmedRejection: false,
            domain: NSURLErrorDomain, code: -1009) == .pending)

        let branchRoot = outboxRoot.appendingPathComponent("confirmed-failure-fixture")
        let branchOutbox = MordeMessageOutbox(root: branchRoot)
        let prior = try! branchOutbox.enqueue(scope: scope, conversationID: "c3456789", content: "已受理的前序",
                                              modelID: nil, attachmentIDs: [])
        try! branchOutbox.update(id: prior.id, scope: scope) {
            $0.state = .accepted; $0.serverMessageID = "m3456789"
        }
        try! branchOutbox.remove(id: prior.id, scope: scope)
        let rejected = try! branchOutbox.enqueue(scope: scope, conversationID: "c3456789", content: "完整的失败原文",
                                                 modelID: nil, attachmentIDs: [])
        try! branchOutbox.update(id: rejected.id, scope: scope) {
            $0.state = .failed; $0.error = "明确拒绝"; $0.submissionStarted = true
            $0.submissionUncertain = false; $0.confirmedNotAccepted = true
        }
        let independent = try! branchOutbox.enqueue(scope: scope, conversationID: "c3456789", content: "独立消息",
                                                    modelID: nil, attachmentIDs: [])
        let following = try! branchOutbox.enqueue(scope: scope, conversationID: "c3456789", content: "之后的消息",
                                                  modelID: nil, attachmentIDs: [])
        let branchBefore = try! branchOutbox.messages(scope: scope)
        precondition(independent.previousClientMessageID == rejected.id && following.previousClientMessageID == independent.id)
        precondition(MordeOutboxScheduling.bypassablePredecessor(of: independent, in: branchBefore, inFlight: [])?.id == rejected.id)
        precondition(MordeOutboxScheduling.bypassablePredecessor(of: following, in: branchBefore, inFlight: []) == nil)
        precondition(MordeOutboxScheduling.bypassablePredecessor(of: independent, in: branchBefore, inFlight: [rejected.id]) == nil)
        precondition(MordeOutboxScheduling.bypassablePredecessor(of: independent, in: branchBefore, inFlight: [independent.id]) == nil)
        for invalidPrevious in [MordeOutboxState.unknown, .sending, .accepted, .pending] {
            var invalid = branchBefore
            invalid[0].state = invalidPrevious
            precondition(MordeOutboxScheduling.bypassablePredecessor(of: independent, in: invalid, inFlight: []) == nil)
        }
        var uncertainRejection = branchBefore
        uncertainRejection[0].submissionUncertain = true
        precondition(MordeOutboxScheduling.bypassablePredecessor(of: independent, in: uncertainRejection, inFlight: []) == nil)
        var legacyRejection = branchBefore
        legacyRejection[0].confirmedNotAccepted = nil
        precondition(MordeOutboxScheduling.bypassablePredecessor(of: independent, in: legacyRejection, inFlight: []) == nil)
        var projectPayload = try! JSONSerialization.jsonObject(with: JSONEncoder().encode(independent)) as! [String: Any]
        projectPayload["projectID"] = "p1234567"
        let differentProject = try! JSONDecoder().decode(MordeOutboxMessage.self,
            from: JSONSerialization.data(withJSONObject: projectPayload))
        precondition(MordeOutboxScheduling.bypassablePredecessor(of: differentProject, in: branchBefore, inFlight: []) == nil)
        var legacyPayload = try! JSONSerialization.jsonObject(with: JSONEncoder().encode(independent)) as! [String: Any]
        for key in ["submissionStarted", "submissionUncertain", "confirmedNotAccepted", "bypassedPredecessorIDs"] {
            legacyPayload.removeValue(forKey: key)
        }
        let legacyPending = try! JSONDecoder().decode(MordeOutboxMessage.self,
            from: JSONSerialization.data(withJSONObject: legacyPayload))
        precondition(legacyPending.submissionStarted == nil && legacyPending.submissionUncertain == nil)
        precondition(MordeOutboxScheduling.bypassablePredecessor(of: legacyPending, in: branchBefore, inFlight: []) == nil)
        var alreadySubmitted = independent
        alreadySubmitted.submissionStarted = true
        precondition(MordeOutboxScheduling.bypassablePredecessor(of: alreadySubmitted, in: branchBefore, inFlight: []) == nil)
        alreadySubmitted.submissionStarted = nil
        precondition(MordeOutboxScheduling.bypassablePredecessor(of: alreadySubmitted, in: branchBefore, inFlight: []) == nil)
        alreadySubmitted.submissionStarted = false
        alreadySubmitted.submissionUncertain = true
        precondition(MordeOutboxScheduling.bypassablePredecessor(of: alreadySubmitted, in: branchBefore, inFlight: []) == nil)
        alreadySubmitted.submissionUncertain = false
        alreadySubmitted.serverMessageID = "m9999999"
        precondition(MordeOutboxScheduling.bypassablePredecessor(of: alreadySubmitted, in: branchBefore, inFlight: []) == nil)
        precondition(!(try! branchOutbox.detachFailedPredecessor(messageID: independent.id,
            predecessorID: rejected.id, scope: otherAccountScope)))
        let differentInstance = MordeClientDataScope(serviceOrigin: scope.serviceOrigin,
            serviceInstanceID: UUID().uuidString, accountID: scope.accountID, installationID: scope.installationID)!
        precondition(!(try! branchOutbox.detachFailedPredecessor(messageID: independent.id,
            predecessorID: rejected.id, scope: differentInstance)))
        precondition(try! branchOutbox.detachFailedPredecessor(messageID: independent.id,
            predecessorID: rejected.id, scope: scope))
        let branchReopened = MordeMessageOutbox(root: branchRoot)
        let detached = try! branchReopened.messages(scope: scope)
        precondition(detached[0] == branchBefore[0], "The rejected original must remain unchanged.")
        precondition(detached[1].previousClientMessageID == prior.id && detached[2].previousClientMessageID == independent.id)
        precondition(detached[1].bypassedPredecessorIDs == [rejected.id])
        precondition(MordeConversationMessagePayload.direct(detached[1])["previous_client_message_id"] as? String == prior.id)
        precondition(MordeOutboxScheduling.readyIDs(detached, scope: scope, inFlight: []) == [independent.id])
        precondition(MordeOutboxScheduling.waitingForEarlierMessage(detached[1], in: detached) == nil)
        precondition(MordeOutboxScheduling.waitingForEarlierMessage(detached[2], in: detached) != nil)
        try! branchOutbox.update(id: rejected.id, scope: scope) { $0.state = .unknown }
        let retryWhileLaterInFlight = try! branchOutbox.messages(scope: scope)
        precondition(MordeOutboxScheduling.readyIDs(retryWhileLaterInFlight, scope: scope, inFlight: []) == [independent.id])
        precondition(MordeOutboxScheduling.waitingForEarlierMessage(retryWhileLaterInFlight[0], in: retryWhileLaterInFlight) ==
            "Retry will wait until the independent message is received.")
        precondition(MordeOutboxScheduling.readyIDs(retryWhileLaterInFlight, scope: scope, inFlight: [independent.id]).isEmpty)
        try! branchOutbox.update(id: independent.id, scope: scope) {
            $0.state = .accepted; $0.serverMessageID = "m4567890"
        }
        let independentAccepted = try! branchReopened.messages(scope: scope)
        precondition(MordeOutboxScheduling.readyIDs(independentAccepted, scope: scope, inFlight: [independent.id]) == [rejected.id])
        try! branchOutbox.update(id: rejected.id, scope: scope) { $0.state = .failed }
        let afterAcceptance = try! branchReopened.messages(scope: scope)
        precondition(MordeOutboxScheduling.readyIDs(afterAcceptance, scope: scope, inFlight: [independent.id]) == [following.id])
        let newest = try! branchOutbox.enqueue(scope: scope, conversationID: "c3456789", content: "再之后",
                                               modelID: nil, attachmentIDs: [])
        precondition(newest.previousClientMessageID == following.id)

        let staleRoot = outboxRoot.appendingPathComponent("stale-failure-fixture")
        let staleOutbox = MordeMessageOutbox(root: staleRoot)
        let staleFirst = try! staleOutbox.enqueue(scope: scope, conversationID: "c4567890", content: "失败",
                                                  modelID: nil, attachmentIDs: [])
        try! staleOutbox.update(id: staleFirst.id, scope: scope) {
            $0.state = .failed; $0.submissionStarted = true; $0.submissionUncertain = false; $0.confirmedNotAccepted = true
        }
        let staleSecond = try! staleOutbox.enqueue(scope: scope, conversationID: "c4567890", content: "等待",
                                                   modelID: nil, attachmentIDs: [])
        try! staleOutbox.update(id: staleFirst.id, scope: scope) { $0.state = .unknown }
        precondition(!(try! staleOutbox.detachFailedPredecessor(messageID: staleSecond.id,
            predecessorID: staleFirst.id, scope: scope)))
        precondition((try! staleOutbox.messages(scope: scope))[1].previousClientMessageID == staleFirst.id)

        let multiRoot = outboxRoot.appendingPathComponent("multiple-rejections-fixture")
        let multiOutbox = MordeMessageOutbox(root: multiRoot)
        let olderFailure = try! multiOutbox.enqueue(scope: scope, conversationID: "c5678901", content: "早先失败",
                                                    modelID: nil, attachmentIDs: [])
        let laterFailure = try! multiOutbox.enqueue(scope: scope, conversationID: "c5678901", content: "后续失败",
                                                    modelID: nil, attachmentIDs: [])
        let multiTarget = try! multiOutbox.enqueue(scope: scope, conversationID: "c5678901", content: "独立处理",
                                                   modelID: nil, attachmentIDs: [])
        for id in [olderFailure.id, laterFailure.id] {
            try! multiOutbox.update(id: id, scope: scope) {
                $0.state = .failed; $0.submissionUncertain = false; $0.confirmedNotAccepted = true
            }
        }
        precondition(try! multiOutbox.detachFailedPredecessor(messageID: multiTarget.id,
            predecessorID: laterFailure.id, scope: scope))
        precondition(try! multiOutbox.detachFailedPredecessor(messageID: multiTarget.id,
            predecessorID: olderFailure.id, scope: scope))
        try! multiOutbox.update(id: olderFailure.id, scope: scope) { $0.state = .unknown }
        try! multiOutbox.update(id: laterFailure.id, scope: scope) { $0.state = .unknown }
        let multiRestored = try! MordeMessageOutbox(root: multiRoot).messages(scope: scope)
        precondition(multiRestored[2].bypassedPredecessorIDs == [laterFailure.id, olderFailure.id])
        precondition(multiRestored[2].previousClientMessageID == nil)
        precondition(MordeOutboxScheduling.readyIDs(multiRestored, scope: scope, inFlight: []) == [multiTarget.id])
        let firstFields = MordeConversationMessagePayload.direct(wireFirst)
        precondition(Set(firstFields.keys) == ["client_message_id", "content"])
        precondition(firstFields["client_message_id"] as? String == wireFirst.id)
        precondition(firstFields["content"] as? String == wireFirst.content)
        let directFields = MordeConversationMessagePayload.direct(wireSecond)
        precondition(Set(directFields.keys) == ["client_message_id", "content", "previous_client_message_id",
            "reference_task_id", "model_id", "host_id", "workspace_id", "client_attachment_ids"])
        precondition(directFields["client_message_id"] as? String == wireSecond.id)
        precondition(directFields["previous_client_message_id"] as? String == wireFirst.id)
        precondition(directFields["reference_task_id"] as? String == "q1234567")
        precondition(directFields["model_id"] as? String == "p1234567")
        precondition(directFields["host_id"] as? String == "h1234567")
        precondition(directFields["workspace_id"] as? String == "w1234567")
        precondition(directFields["client_attachment_ids"] as? [String] == ["local-slot-001"])
        precondition(directFields["mode"] == nil && directFields["expected_task_id"] == nil)
        let serializedFields = try! JSONSerialization.data(withJSONObject: directFields)
        let decodedFields = try! JSONSerialization.jsonObject(with: serializedFields) as! [String: Any]
        precondition(decodedFields["content"] as? String == wireSecond.content)
        let uploadFields = MordeConversationMessagePayload.uploadInitialization(wireLong, bytes: longBytes)
        precondition(uploadFields["previous_client_message_id"] as? String == wireSecond.id)
        precondition(uploadFields["reference_task_id"] as? String == "q1234567")
        precondition(uploadFields["client_attachment_ids"] as? [String] == ["local-slot-002"])
        precondition(uploadFields["total_bytes"] as? Int == longBytes.count)
        precondition(uploadFields["total_chunks"] as? Int == chunks.count)
        precondition(uploadFields["sha256"] as? String == MordeMessageChunking.digest(longBytes))
        precondition(uploadFields["content"] == nil && uploadFields["client_message_id"] == nil &&
            uploadFields["mode"] == nil && uploadFields["expected_task_id"] == nil)
        let receiptMessage: [String: Any] = ["id": "m1234567", "conversation_id": "c1234567", "role": "user",
            "content": wireFirst.content, "client_message_id": wireFirst.id, "routing_status": "pending"]
        for replayed in [false, true] {
            let data = try! JSONSerialization.data(withJSONObject: ["message": receiptMessage, "replayed": replayed])
            let receipt = try! JSONDecoder().decode(MordeConversationMessageReceipt.self, from: data)
            precondition(receipt.message.task_id == nil && receipt.message.client_message_id == wireFirst.id)
            precondition(receipt.message.content == wireFirst.content && receipt.replayed == replayed)
            precondition(MordeConversationMessageReceiptPolicy.matches(receipt.message, clientMessageID: wireFirst.id,
                conversationID: wireFirst.conversationID, content: wireFirst.content))
        }
        let uploadReceipt = try! JSONDecoder().decode(MordeConversationMessageReceipt.self,
            from: JSONSerialization.data(withJSONObject: ["message": receiptMessage]))
        precondition(uploadReceipt.message.task_id == nil && uploadReceipt.replayed == nil)
        let routedReceipt = MordeClientMessage(id: "m1234567", conversation_id: "c1234567", task_id: "q1234567",
            role: "user", content: wireFirst.content, client_message_id: wireFirst.id)
        precondition(MordeConversationMessageReceiptPolicy.matches(routedReceipt, clientMessageID: wireFirst.id,
            conversationID: wireFirst.conversationID, content: wireFirst.content))
        precondition(!MordeConversationMessageReceiptPolicy.matches(routedReceipt, clientMessageID: "other-id",
            conversationID: wireFirst.conversationID, content: wireFirst.content))
        precondition(!MordeConversationMessageReceiptPolicy.matches(routedReceipt, clientMessageID: wireFirst.id,
            conversationID: "c7654321", content: wireFirst.content))
        precondition(!MordeConversationMessageReceiptPolicy.matches(routedReceipt, clientMessageID: wireFirst.id,
            conversationID: wireFirst.conversationID, content: "不同内容"))
        let wrongRole = MordeClientMessage(id: "m1234567", conversation_id: "c1234567", role: "assistant",
            content: wireFirst.content, client_message_id: wireFirst.id)
        precondition(!MordeConversationMessageReceiptPolicy.matches(wrongRole, clientMessageID: wireFirst.id,
            conversationID: wireFirst.conversationID, content: wireFirst.content))
        let normalizedReceipt = MordeClientMessage(id: "m1234567", conversation_id: "c1234567", role: "user",
            content: "é", client_message_id: wireFirst.id)
        precondition(!MordeConversationMessageReceiptPolicy.matches(normalizedReceipt, clientMessageID: wireFirst.id,
            conversationID: wireFirst.conversationID, content: "e\u{301}"))
        let firstPending = try! messageOutbox.enqueue(scope: scope, conversationID: "c1234567", content: "第一条",
                                                      modelID: nil, attachmentIDs: [])
        var secondPending = try! messageOutbox.enqueue(scope: scope, conversationID: "c1234567", content: "第二条",
                                                       modelID: "p1234567", attachmentIDs: ["pa_test"])
        precondition(secondPending.previousClientMessageID == firstPending.id)
        precondition(try! messageOutbox.messages(scope: scope) == [firstPending, secondPending])
        precondition(try! messageOutbox.messages(scope: otherAccountScope).isEmpty)
        precondition(try! messageOutbox.messages(scope: rebuiltServiceScope).isEmpty)
        precondition(try! messageOutbox.messages(scope: otherInstallationScope) == [firstPending, secondPending])
        secondPending.state = .unknown
        try! messageOutbox.replace(secondPending, scope: otherInstallationScope)
        precondition((try! messageOutbox.messages(scope: scope)).last?.state == .unknown)
        precondition((try? messageOutbox.replace(secondPending, scope: otherAccountScope)) == nil)
        try! messageOutbox.remove(id: firstPending.id, scope: scope)
        let thirdPending = try! messageOutbox.enqueue(scope: scope, conversationID: "c1234567", content: "第三条",
                                                      modelID: nil, attachmentIDs: [])
        precondition(thirdPending.previousClientMessageID == secondPending.id)
        let reopenedOutbox = MordeMessageOutbox(root: outboxRoot)
        precondition(try! reopenedOutbox.messages(scope: scope) == [secondPending, thirdPending])
        let staged = MordeOutboxAttachment(id: "attachment-id-001", attachmentID: nil,
            localPath: "/local/synthetic.pdf", linked: false, sourceBookmark: Data("local-bookmark".utf8),
            sourcePath: "/original/synthetic.pdf", derivedAssociationPending: true)
        let uploadPending = try! messageOutbox.enqueue(scope: scope, conversationID: "c7654321", content: "附件",
            modelID: nil, attachmentIDs: [], awaitingAttachmentBatch: true, stagedAttachments: [staged],
            pendingUploadPaths: ["/local/source.pdf"])
        let restoredUpload = (try! reopenedOutbox.messages(scope: scope)).last!
        precondition(restoredUpload == uploadPending && restoredUpload.awaitingAttachmentBatch == true)
        precondition(restoredUpload.attachments.first?.id == "attachment-id-001")
        precondition(restoredUpload.attachments.first?.sourceBookmark == Data("local-bookmark".utf8))
        precondition(restoredUpload.attachments.first?.derivedAssociationPending == true)
        precondition(restoredUpload.pendingUploadPaths == ["/local/source.pdf"])
        var acceptedWithPendingDerivatives = restoredUpload
        acceptedWithPendingDerivatives.awaitingAttachmentBatch = false
        acceptedWithPendingDerivatives.state = .accepted
        acceptedWithPendingDerivatives.attachments[0].linked = true
        precondition(acceptedWithPendingDerivatives.statusLocalizationKey == "Sent; uploading attachments")
        acceptedWithPendingDerivatives.error = "解析失败"
        precondition(acceptedWithPendingDerivatives.statusLocalizationKey == "Sent; attachments need attention")
        let projectPending = try! messageOutbox.enqueue(scope: scope, conversationID: "c2345678", content: "",
            modelID: nil, attachmentIDs: [], projectID: "j1234567", stagedAttachments: [staged])
        func projectMessageCopy(id: String) -> MordeOutboxMessage {
            var value = try! JSONSerialization.jsonObject(with: JSONEncoder().encode(projectPending)) as! [String: Any]
            value["id"] = id
            value["createdAt"] = "2026-11-07T00:00:00.000Z"
            return try! JSONDecoder().decode(MordeOutboxMessage.self, from: JSONSerialization.data(withJSONObject: value))
        }
        let sameTimeProjectMessages = [projectMessageCopy(id: "local-z"), projectMessageCopy(id: "local-a")]
        precondition(MordeOfflineProjectQueue.messages(sameTimeProjectMessages, scope: scope).map(\.id) == ["local-z", "local-a"])
        let projectOnlyRestart = try! MordeMessageOutbox(root: outboxRoot).messages(scope: scope)
        precondition(MordeOfflineProjectQueue.messages(projectOnlyRestart, scope: scope).map(\.id).contains(projectPending.id))
        let otherProjectAccountScope = MordeClientDataScope(serviceOrigin: scope.serviceOrigin,
            serviceInstanceID: scope.serviceInstanceID, accountID: "user-2", installationID: "install-b")!
        let otherInstanceScope = MordeClientDataScope(serviceOrigin: scope.serviceOrigin,
            serviceInstanceID: UUID().uuidString, accountID: scope.accountID, installationID: "install-a")!
        precondition(MordeOfflineProjectQueue.messages(projectOnlyRestart, scope: otherProjectAccountScope).isEmpty)
        precondition(MordeOfflineProjectQueue.messages(projectOnlyRestart, scope: otherInstanceScope).isEmpty)
        precondition(MordeOfflineProjectQueue.messages(projectOnlyRestart, scope: scope).map(\.id).contains(projectPending.id))
        try! messageOutbox.update(id: projectPending.id, scope: scope) { message in
            message.serverMessageID = "m1234567"
            message.state = .accepted
            message.attachments[0].attachmentID = projectAttachmentID
        }
        let resumedProject = try! MordeMessageOutbox(root: outboxRoot).messages(scope: scope)
            .first { $0.id == projectPending.id }!
        precondition(resumedProject.projectID == "j1234567" && resumedProject.serverMessageID == "m1234567")
        precondition(resumedProject.attachments[0].id == staged.id &&
                     resumedProject.attachments[0].attachmentID == projectAttachmentID && !resumedProject.attachments[0].linked)
        precondition((try! reopenedOutbox.messages(scope: otherAccountScope)).allSatisfy { $0.id != projectPending.id })
        let localID = "local-01234567-89ab-cdef-0123-456789abcdef"
        let localFirst = try! messageOutbox.enqueue(scope: scope, conversationID: localID, content: "离线一",
                                                    modelID: nil, attachmentIDs: [])
        let localSecond = try! messageOutbox.enqueue(scope: scope, conversationID: localID, content: "离线二",
                                                     modelID: nil, attachmentIDs: [])
        try! messageOutbox.resolveLocalConversation(localID, serverID: "c-new-server", scope: scope)
        let resolved = try! reopenedOutbox.messages(scope: scope).filter { $0.conversationID == "c-new-server" }
        precondition(resolved.map(\.id) == [localFirst.id, localSecond.id])
        precondition(resolved.last?.previousClientMessageID == localFirst.id)
        precondition(try! reopenedOutbox.resolvedConversationID(localID, scope: otherInstallationScope) == "c-new-server")
        precondition(try! reopenedOutbox.resolvedConversationID(localID, scope: otherAccountScope) == localID)
        precondition((try? messageOutbox.resolveLocalConversation(localID, serverID: "c-other", scope: scope)) == nil)
        let afterResolution = try! messageOutbox.enqueue(scope: scope, conversationID: "c-new-server", content: "第三条",
                                                         modelID: nil, attachmentIDs: [])
        precondition(afterResolution.previousClientMessageID == localSecond.id)
        let brokenRoot = outboxRoot.appendingPathComponent("not-a-directory")
        try! Data("file".utf8).write(to: brokenRoot)
        let brokenOutbox = MordeMessageOutbox(root: brokenRoot)
        precondition((try? brokenOutbox.enqueue(scope: scope, conversationID: "c1234567", content: "保留输入",
                                               modelID: nil, attachmentIDs: [])) == nil)
        precondition(!restoredSend.matches(scope: otherAccountScope, fingerprint: taskAttempt))
        precondition(!restoredSend.matches(scope: rebuiltServiceScope, fingerprint: taskAttempt))
        precondition(!restoredSend.matches(scope: otherInstallationScope, fingerprint: taskAttempt))
        precondition(MordeUnauthorizedResponsePolicy.shouldClearAuthorization(
            requestCredential: "old-token", currentCredential: "old-token", requestScope: scope,
            currentScope: scope, hasPendingLoginIntent: false, isCancelled: false))
        precondition(!MordeUnauthorizedResponsePolicy.shouldClearAuthorization(
            requestCredential: "old-token", currentCredential: "new-token", requestScope: scope,
            currentScope: scope, hasPendingLoginIntent: false, isCancelled: false))
        precondition(!MordeUnauthorizedResponsePolicy.shouldClearAuthorization(
            requestCredential: "old-token", currentCredential: "old-token", requestScope: scope,
            currentScope: otherAccountScope, hasPendingLoginIntent: false, isCancelled: false))
        precondition(!MordeUnauthorizedResponsePolicy.shouldClearAuthorization(
            requestCredential: "old-token", currentCredential: "old-token", requestScope: scope,
            currentScope: rebuiltServiceScope, hasPendingLoginIntent: false, isCancelled: false))
        precondition(!MordeUnauthorizedResponsePolicy.shouldClearAuthorization(
            requestCredential: "old-token", currentCredential: "old-token", requestScope: scope,
            currentScope: otherInstallationScope, hasPendingLoginIntent: false, isCancelled: false))
        precondition(!MordeUnauthorizedResponsePolicy.shouldClearAuthorization(
            requestCredential: "old-token", currentCredential: "old-token", requestScope: scope,
            currentScope: nil, hasPendingLoginIntent: false, isCancelled: false))
        precondition(!MordeUnauthorizedResponsePolicy.shouldClearAuthorization(
            requestCredential: "old-token", currentCredential: "old-token", requestScope: scope,
            currentScope: scope, hasPendingLoginIntent: true, isCancelled: false))
        precondition(!MordeUnauthorizedResponsePolicy.shouldClearAuthorization(
            requestCredential: "old-token", currentCredential: "old-token", requestScope: scope,
            currentScope: scope, hasPendingLoginIntent: false, isCancelled: true))
        precondition(Set([scope.storageKey, otherAccountScope.storageKey, rebuiltServiceScope.storageKey,
                          otherInstallationScope.storageKey]).count == 4)

        let recent = MordeScopedRecentConversation(scope: scope, conversationID: "conversation-1")
        outboxDefaults.set(try! JSONEncoder().encode(recent), forKey: "morde.recentConversation.\(scope.storageKey)")
        let restartedRecent = try! JSONDecoder().decode(
            MordeScopedRecentConversation.self,
            from: outboxDefaults.data(forKey: "morde.recentConversation.\(scope.storageKey)")!)
        precondition(restartedRecent.matches(scope: scope))
        precondition(!restartedRecent.matches(scope: otherAccountScope))
        precondition(!restartedRecent.matches(scope: rebuiltServiceScope))
        precondition(!restartedRecent.matches(scope: otherInstallationScope))

        let revocationSuite = "MordeRevocationStoreTests-\(UUID().uuidString)"
        let revocationDefaults = UserDefaults(suiteName: revocationSuite)!
        revocationDefaults.removePersistentDomain(forName: revocationSuite)
        defer { revocationDefaults.removePersistentDomain(forName: revocationSuite) }
        let revocation = MordeRevocationRecord(scope: scope, clientInstanceID: firstAttempt.clientInstanceID,
                                               confirmed: false)
        try! MordeRevocationStore(defaults: revocationDefaults).record(revocation)
        let restartedRevocations = MordeRevocationStore(defaults: revocationDefaults).pending()
        precondition(restartedRevocations == [revocation])
        let revocationJSON = String(decoding: revocationDefaults.data(forKey: MordeRevocationStore.defaultsKey)!, as: UTF8.self)
        precondition(revocationJSON.contains("install-1"))
        precondition(revocationJSON.contains(firstAttempt.clientInstanceID.uuidString))
        precondition(!revocationJSON.contains("credential"))
        precondition(!revocationJSON.contains("password"))
        try! MordeRevocationStore(defaults: revocationDefaults).record(
            MordeRevocationRecord(scope: scope, clientInstanceID: firstAttempt.clientInstanceID, confirmed: true))
        precondition(MordeRevocationStore(defaults: revocationDefaults).pending().isEmpty)

        print("Morde login fence, scoped task recovery, target readiness and pinning, revocation persistence, credential, version, cursor, task status, and notification checks passed")
    }
}
