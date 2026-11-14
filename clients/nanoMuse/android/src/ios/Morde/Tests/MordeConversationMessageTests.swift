import XCTest
@testable import MordeTaskPolicies

final class MordeConversationMessageTests: XCTestCase {
    func testSelectedModelPreferenceWinsOnlyWhenItIsAvailable() {
        let available = Set(["personal", "admin-default"])

        XCTAssertEqual(MordePreferredModelPolicy.resolve(
            selectedModelID: "personal", availableModelIDs: available,
            administratorDefaultModelID: "admin-default", singleAvailableModelID: nil), "personal")
        XCTAssertEqual(MordePreferredModelPolicy.resolve(
            selectedModelID: "unavailable", availableModelIDs: available,
            administratorDefaultModelID: "admin-default", singleAvailableModelID: nil), "admin-default")
        XCTAssertEqual(MordePreferredModelPolicy.resolve(
            selectedModelID: nil, availableModelIDs: Set(["personal"]),
            administratorDefaultModelID: nil, singleAvailableModelID: "personal"), "personal")
        XCTAssertNil(MordePreferredModelPolicy.resolve(
            selectedModelID: "unavailable", availableModelIDs: [],
            administratorDefaultModelID: nil, singleAvailableModelID: nil))
    }

    func testClientMessageDecodesWireFields() throws {
        let data = Data(#"{"id":"m1234567","conversation_id":"c1234567","sequence":2,"task_id":"q1234567","role":"user","content":"继续处理","attachment_ids":["pa_12345678901234567890123456789012"],"delivery_status":"pending","created_at":"2026-11-09T12:00:00Z","client_message_id":"client-message-1","routing_status":"pending"}"#.utf8)

        let message = try JSONDecoder().decode(MordeClientMessage.self, from: data)

        XCTAssertEqual(message.id, "m1234567")
        XCTAssertEqual(message.conversation_id, "c1234567")
        XCTAssertEqual(message.sequence, 2)
        XCTAssertEqual(message.task_id, "q1234567")
        XCTAssertEqual(message.role, "user")
        XCTAssertEqual(message.content, "继续处理")
        XCTAssertEqual(message.attachment_ids, ["pa_12345678901234567890123456789012"])
        XCTAssertEqual(message.delivery_status, "pending")
        XCTAssertEqual(message.client_message_id, "client-message-1")
        XCTAssertEqual(message.routing_status, "pending")
    }

    func testClientMessageDecodesVariantProjection() throws {
        let data = Data(#"{"id":"m1234567","conversation_id":"c1234567","sequence":8,"role":"assistant","content":"second answer","variant_group_id":"m7654321","variant_version":2,"variant_action":"regenerate_text","output_status":"completed"}"#.utf8)

        let message = try JSONDecoder().decode(MordeClientMessage.self, from: data)

        XCTAssertEqual(message.variant_group_id, "m7654321")
        XCTAssertEqual(message.variant_version, 2)
        XCTAssertEqual(message.variant_action, "regenerate_text")
        XCTAssertTrue(MordeConversationVariantPresentation.hasValidProjection(message))
    }

    func testVariantPresentationKeepsOldContentAndDefaultsToNewestVersion() {
        let versionOneUser = MordeClientMessage(id: "m0000001", conversation_id: "c1234567", sequence: 1,
            role: "user", content: "original prompt", variant_group_id: "m0000001", variant_version: 1,
            variant_action: "original")
        let versionOneAssistant = MordeClientMessage(id: "m0000002", conversation_id: "c1234567", sequence: 2,
            role: "assistant", content: "original answer", output_status: "completed",
            variant_group_id: "m0000001", variant_version: 1, variant_action: "original")
        let versionTwoUser = MordeClientMessage(id: "m0000003", conversation_id: "c1234567", sequence: 3,
            role: "user", content: "revised prompt", variant_group_id: "m0000001", variant_version: 2,
            variant_action: "edit_resend")
        let versionTwoAssistant = MordeClientMessage(id: "m0000004", conversation_id: "c1234567", sequence: 4,
            role: "assistant", content: "revised answer", output_status: "completed",
            variant_group_id: "m0000001", variant_version: 2, variant_action: "edit_resend")
        let otherGroup = MordeClientMessage(id: "m0000005", conversation_id: "c1234567", sequence: 5,
            role: "user", content: "another turn", variant_group_id: "m0000005", variant_version: 1)
        let messages = [versionOneUser, versionOneAssistant, versionTwoUser, versionTwoAssistant, otherGroup]

        XCTAssertEqual(MordeConversationVariantPresentation.latestVersion(in: messages, groupID: "m0000001"), 2)
        XCTAssertEqual(MordeConversationVariantPresentation.visibleMessages(messages, selectedVersions: [:]).map(\.id),
                       ["m0000003", "m0000004", "m0000005"])
        XCTAssertEqual(MordeConversationVariantPresentation.visibleMessages(messages,
            selectedVersions: ["m0000001": 1]).map(\.content), ["original prompt", "original answer", "another turn"])
        XCTAssertEqual(versionOneAssistant.content, "original answer")
    }

    func testVariantActionsRequireCorrectMessageRoleAndRecoverableOutput() {
        let user = MordeClientMessage(id: "m0000001", conversation_id: "c1234567", role: "user",
            content: "prompt", variant_group_id: "m0000001", variant_version: 1)
        let complete = MordeClientMessage(id: "m0000002", conversation_id: "c1234567", task_id: "q1234567",
            role: "assistant", content: "answer", status: "completed",
            variant_group_id: "m0000001", variant_version: 1)
        let interrupted = MordeClientMessage(id: "m0000003", conversation_id: "c1234567", task_id: "q1234567",
            role: "assistant", content: "partial", status: "interrupted",
            variant_group_id: "m0000001", variant_version: 1)
        let streaming = MordeClientMessage(id: "m0000004", conversation_id: "c1234567", task_id: "q1234567",
            role: "assistant", content: "partial", status: "streaming",
            variant_group_id: "m0000001", variant_version: 1)

        XCTAssertTrue(MordeConversationVariantPresentation.allows(.editResend, for: user))
        XCTAssertFalse(MordeConversationVariantPresentation.allows(.regenerateText, for: user))
        XCTAssertTrue(MordeConversationVariantPresentation.allows(.regenerateText, for: complete))
        XCTAssertFalse(MordeConversationVariantPresentation.allows(.continueGeneration, for: complete))
        XCTAssertTrue(MordeConversationVariantPresentation.allows(.continueGeneration, for: interrupted))
        XCTAssertFalse(MordeConversationVariantPresentation.allows(.continueGeneration, for: streaming))
    }

    func testVariantRequestAndOperationReceiptMatchContract() throws {
        let edit = MordeConversationVariantOperationRequest(action: .editResend, expectedVersion: 3,
                                                           content: "new prompt")
        XCTAssertTrue(edit.isValid)
        XCTAssertEqual(edit.fields["action"] as? String, "edit_resend")
        XCTAssertEqual(edit.fields["expected_version"] as? Int64, 3)
        XCTAssertEqual(edit.fields["content"] as? String, "new prompt")
        XCTAssertFalse(MordeConversationVariantOperationRequest(action: .editResend, expectedVersion: 1,
                                                                content: " \n ").isValid)
        XCTAssertFalse(MordeConversationVariantOperationRequest(action: .regenerateText, expectedVersion: 1,
                                                                content: "not allowed").isValid)

        let data = Data(#"{"operation_id":"variant-op-1","variant_group_id":"m1234567","variant_version":4,"action":"edit_resend","state":"accepted","task_id":"q1234567","user_message_id":"m1234568","assistant_message_id":"m1234569"}"#.utf8)
        let receipt = try JSONDecoder().decode(MordeConversationVariantOperationResponse.self, from: data)
        XCTAssertTrue(receipt.isValid)
        XCTAssertTrue(receipt.hasKnownOutcome)
        XCTAssertEqual(receipt.variant_version, 4)
        XCTAssertEqual(receipt.action, .editResend)

        let unknownData = Data(String(decoding: data, as: UTF8.self)
            .replacingOccurrences(of: #""state":"accepted""#, with: #""state":"unknown""#).utf8)
        let unknownReceipt = try JSONDecoder().decode(MordeConversationVariantOperationResponse.self, from: unknownData)
        XCTAssertTrue(unknownReceipt.isValid)
        XCTAssertFalse(unknownReceipt.hasKnownOutcome)

        let invalidData = Data(String(decoding: data, as: UTF8.self)
            .replacingOccurrences(of: #""state":"accepted""#, with: #""state":"queued""#).utf8)
        XCTAssertFalse(try JSONDecoder().decode(MordeConversationVariantOperationResponse.self, from: invalidData).isValid)
    }

    func testVariantEditContentUsesServerUnicodeScalarLimit() {
        let maximum = String(repeating: "你", count: 32_768)
        let overLimit = maximum + "你"

        XCTAssertEqual(maximum.unicodeScalars.count, MordeConversationVariantOperationRequest.maximumContentScalars)
        XCTAssertTrue(MordeConversationVariantOperationRequest(action: .editResend, expectedVersion: 1,
                                                               content: maximum).isValid)
        XCTAssertFalse(MordeConversationVariantOperationRequest(action: .editResend, expectedVersion: 1,
                                                                content: overLimit).isValid)
    }

    func testClientMessageDecodesDuplicateAssociationAndKeepsItsTask() throws {
        let data = Data(#"{"id":"m1234567","conversation_id":"c1234567","sequence":3,"role":"user","content":"相同请求","delivery_status":"pending","created_at":"2026-11-13T12:00:00Z","client_message_id":"client-message-3","routing_status":"waiting_user","duplicate_of_message_id":"m7654321","related_task_id":"q1234567"}"#.utf8)

        let message = try JSONDecoder().decode(MordeClientMessage.self, from: data)

        XCTAssertEqual(message.duplicate_of_message_id, "m7654321")
        XCTAssertEqual(message.related_task_id, "q1234567")
        XCTAssertEqual(MordeRoutingPresentation.duplicateMessageLocalizationKey(
            originalMessageID: message.duplicate_of_message_id!, taskID: message.related_task_id),
            "Repeated request · related task q1234567")
    }

    func testRoutineRoutingStatusesStayHiddenButAdjustmentRemainsVisible() {
        XCTAssertNil(MordeRoutingPresentation.localizationKey("pending"))
        XCTAssertNil(MordeRoutingPresentation.localizationKey("processing"))
        XCTAssertNil(MordeRoutingPresentation.localizationKey("waiting_user"))
        XCTAssertNil(MordeRoutingPresentation.localizationKey("failed"))
        XCTAssertEqual(MordeRoutingPresentation.localizationKey("waiting_adjustment"),
                       "Adjustment received; waiting to update")
    }

    func testTaskStartedAcknowledgementMarksOneLatestEligibleMessagePerTask() {
        let messages = [
            MordeClientMessage(id: "m0000008", sequence: 8, task_id: "q5678901",
                               role: "user", content: "independent task"),
            MordeClientMessage(id: "m0000007", sequence: 7, task_id: "q4567890",
                               role: "user", content: "not started"),
            MordeClientMessage(id: "m0000006", sequence: 6, task_id: "q3456789",
                               role: "user", content: "waiting for revision"),
            MordeClientMessage(id: "m0000005", sequence: 5, task_id: "q2345678",
                               role: "user", content: "failed", routing_status: "failed"),
            MordeClientMessage(id: "m0000004", sequence: 4, task_id: "q1234567",
                               role: "assistant", content: "reply"),
            MordeClientMessage(id: "m0000003", sequence: 3, role: "user", content: "exact repeat",
                               duplicate_of_message_id: "m0000001", related_task_id: "q1234567"),
            MordeClientMessage(id: "m0000002", sequence: 2, task_id: "q1234567",
                               role: "user", content: "same task follow-up"),
            MordeClientMessage(id: "m0000001", sequence: 1, task_id: "q1234567",
                               role: "user", content: "task root"),
        ]
        let tasks = [
            MordeTaskStartedAcknowledgementState(taskID: "q1234567", currentRevision: 2,
                desiredRevision: 2, currentRevisionStarted: true),
            MordeTaskStartedAcknowledgementState(taskID: "q2345678", currentRevision: 1,
                desiredRevision: 1, currentRevisionStarted: true),
            MordeTaskStartedAcknowledgementState(taskID: "q3456789", currentRevision: 1,
                desiredRevision: 2, currentRevisionStarted: true),
            MordeTaskStartedAcknowledgementState(taskID: "q4567890", currentRevision: 1,
                desiredRevision: 1, currentRevisionStarted: false),
            MordeTaskStartedAcknowledgementState(taskID: "q5678901", currentRevision: 1,
                desiredRevision: 1, currentRevisionStarted: true),
        ]

        XCTAssertEqual(MordeTaskStartedAcknowledgementPolicy.messageIDs(messages: messages, tasks: tasks),
                       Set(["m0000002", "m0000008"]))
    }

    func testPublishedTaskResultIsShownOnceWithItsSource() {
        let snapshot = MordeClientMessage(
            id: "m0000001", conversation_id: "c1234567", task_id: "q1234567",
            role: "assistant", content: "A result", output_status: "completed",
            output_task_revision: 2, output_result_event_sequence: 7)
        let published = MordeClientMessage(
            id: "m0000002", conversation_id: "c1234567", task_id: "q1234567",
            role: "assistant", content: "The main assistant reply", status: "completed",
            sources: [MordeClientMessageSource(
                user_message_id: "m1234567", task_id: "q1234567", task_revision: 2,
                event_sequence: 7, content: "A result")])

        let merged = MordeTaskSnapshotMessageMerge.merge(
            [published], snapshot: snapshot, taskID: "q1234567", conversationID: "c1234567")

        XCTAssertEqual(merged.map(\.id), ["m0000002"])
        XCTAssertFalse(MordeTaskSnapshotMessageMerge.shouldShowTaskResult(
            resultMessagePending: false, resultMessagePublished: true))
        XCTAssertEqual(merged.first?.sources?.first?.task_revision, 2)
        XCTAssertEqual(merged.first?.sources?.first?.event_sequence, 7)
    }

    func testExpiredCredentialIsRejected() throws {
        let now = try XCTUnwrap(ISO8601DateFormatter().date(from: "2026-11-09T12:00:00Z"))

        XCTAssertEqual(MordeCredentialExpiry.status(expiresAt: "2026-11-09T11:59:59Z", now: now), .expired)
        XCTAssertNil(MordeCredentialExpiry.validCredential(
            "expired-credential", expiresAt: "2026-11-09T11:59:59Z", now: now))
        XCTAssertEqual(MordeCredentialExpiry.validCredential(
            "current-credential", expiresAt: "2026-11-10T12:00:00Z", now: now), "current-credential")
    }

    func testChangedServiceInstanceCannotReadExistingAccountOutbox() throws {
        let firstScope = try makeScope(instanceID: "11111111-1111-4111-8111-111111111111")
        let changedScope = try makeScope(instanceID: "22222222-2222-4222-8222-222222222222")
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("morde-instance-outbox-\(UUID().uuidString)", isDirectory: true)
        defer { try? FileManager.default.removeItem(at: root) }

        let outbox = MordeMessageOutbox(root: root)
        let queued = try outbox.enqueue(scope: firstScope, conversationID: "c1234567", content: "仅属于旧服务实例",
            modelID: nil, attachmentIDs: [])

        XCTAssertEqual(try outbox.messages(scope: firstScope), [queued])
        XCTAssertTrue(try MordeMessageOutbox(root: root).messages(scope: changedScope).isEmpty)
        XCTAssertNotEqual(firstScope.messageOutboxKey, changedScope.messageOutboxKey)
        XCTAssertFalse(firstScope.hasSameAccount(as: changedScope))
    }

    func testChangedAccountCannotReadExistingOutbox() throws {
        let firstScope = try makeScope(accountID: "user-old")
        let changedScope = try makeScope(accountID: "user-new")
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("morde-account-outbox-\(UUID().uuidString)", isDirectory: true)
        defer { try? FileManager.default.removeItem(at: root) }

        let outbox = MordeMessageOutbox(root: root)
        let queued = try outbox.enqueue(scope: firstScope, conversationID: "c1234567", content: "旧账号待发内容",
            modelID: nil, attachmentIDs: [])

        XCTAssertEqual(try outbox.messages(scope: firstScope), [queued])
        XCTAssertTrue(try MordeMessageOutbox(root: root).messages(scope: changedScope).isEmpty)
        XCTAssertNotEqual(firstScope.messageOutboxKey, changedScope.messageOutboxKey)
        XCTAssertFalse(firstScope.hasSameAccount(as: changedScope))
    }

    func testVariantOperationOutboxReusesExactRequestAcrossRestartAndVersions() throws {
        let scope = try makeScope()
        let otherAccount = try makeScope(accountID: "user-other")
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("morde-variant-operation-\(UUID().uuidString)", isDirectory: true)
        defer { try? FileManager.default.removeItem(at: root) }

        let key = MordeConversationVariantPresentation.attemptStorageKey(scope: scope, projectID: "p1234567",
            conversationID: "c1234567", groupID: "m1234567", action: .editResend)
        let proposed = MordeConversationVariantOperationAttempt(idempotencyKey: "variant-attempt-0001",
            action: .editResend, expectedVersion: 2, projectID: "p1234567", conversationID: "c1234567",
            messageID: "m1234567", groupID: "m1234567", content: "edited")
        let outbox = MordeMessageOutbox(root: root)
        let attempt = try outbox.beginVariantOperation(storageKey: key, proposed: proposed, scope: scope)
        XCTAssertEqual(try MordeMessageOutbox(root: root).pendingVariantOperation(storageKey: key, scope: scope), proposed)

        let staleRetry = MordeConversationVariantOperationAttempt(idempotencyKey: "newly-generated-key",
            action: .editResend, expectedVersion: 3, projectID: "p1234567", conversationID: "c1234567",
            messageID: "m1234568", groupID: "m1234567", content: "different text")
        XCTAssertEqual(try MordeMessageOutbox(root: root).beginVariantOperation(storageKey: key,
            proposed: staleRetry, scope: scope), attempt)
        XCTAssertEqual(attempt.expectedVersion, 2)
        XCTAssertEqual(attempt.content, "edited")

        let accountScopedKey = MordeConversationVariantPresentation.attemptStorageKey(scope: otherAccount,
            projectID: "p1234567", conversationID: "c1234567", groupID: "m1234567", action: .editResend)
        let otherAccountAttempt = try outbox.beginVariantOperation(storageKey: accountScopedKey,
            proposed: MordeConversationVariantOperationAttempt(idempotencyKey: "other-attempt-0001",
                action: .editResend, expectedVersion: 2, projectID: "p1234567", conversationID: "c1234567",
                messageID: "m1234567", groupID: "m1234567", content: "edited"), scope: otherAccount)
        XCTAssertNotEqual(otherAccountAttempt.idempotencyKey, attempt.idempotencyKey)

        try outbox.finishVariantOperation(storageKey: key, attempt: attempt, scope: scope)
        XCTAssertNil(try outbox.pendingVariantOperation(storageKey: key, scope: scope))
    }

    func testVariantSelectionPersistsPerAccountAndProjectConversation() throws {
        let scope = try makeScope()
        let otherAccount = try makeScope(accountID: "user-other")
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("morde-variant-selection-\(UUID().uuidString)", isDirectory: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let outbox = MordeMessageOutbox(root: root)

        try outbox.setVariantSelection(storageKey: "p1234567:c1234567:m1234567", version: 1, scope: scope)
        try outbox.setVariantSelection(storageKey: "personal:c1234567:m1234567", version: 3, scope: scope)

        let reopened = MordeMessageOutbox(root: root)
        XCTAssertEqual(try reopened.variantSelections(scope: scope)["p1234567:c1234567:m1234567"], 1)
        XCTAssertEqual(try reopened.variantSelections(scope: scope)["personal:c1234567:m1234567"], 3)
        XCTAssertTrue(try reopened.variantSelections(scope: otherAccount).isEmpty)
    }

    func testOutboxRestartPreservesClientAndPredecessorMessageIDs() throws {
        let scope = try makeScope(instanceID: "11111111-1111-4111-8111-111111111111")
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("morde-message-restart-\(UUID().uuidString)", isDirectory: true)
        defer { try? FileManager.default.removeItem(at: root) }

        let originalOutbox = MordeMessageOutbox(root: root)
        var first = try originalOutbox.enqueue(scope: scope, conversationID: "c1234567", content: "第一条",
            modelID: nil, attachmentIDs: [])
        first.state = .sending
        first.submissionStarted = true
        first.submissionUncertain = true
        try originalOutbox.replace(first, scope: scope)

        var second = try originalOutbox.enqueue(scope: scope, conversationID: "c1234567", content: "第二条",
            modelID: nil, attachmentIDs: [])
        second.state = .sending
        second.submissionStarted = true
        second.submissionUncertain = true
        try originalOutbox.replace(second, scope: scope)
        XCTAssertEqual(second.previousClientMessageID, first.id)

        let reopenedOutbox = MordeMessageOutbox(root: root)
        let recovered = try reopenedOutbox.recoverInterruptedMessages(scope: scope)
        XCTAssertEqual(recovered.map(\.id), [first.id, second.id])
        XCTAssertTrue(recovered.allSatisfy { $0.state == .unknown })

        let recoveredFirst = try XCTUnwrap(recovered.first)
        let recoveredSecond = try XCTUnwrap(recovered.last)
        XCTAssertEqual(recoveredFirst.id, first.id)
        XCTAssertEqual(recoveredSecond.id, second.id)
        XCTAssertEqual(recoveredSecond.previousClientMessageID, recoveredFirst.id)
        if let timeZone = recoveredSecond.timeZone {
            XCTAssertTrue(TimeZone.knownTimeZoneIdentifiers.contains(timeZone))
        }

        let payload = MordeConversationMessagePayload.direct(recoveredSecond)
        XCTAssertEqual(payload["client_message_id"] as? String, recoveredSecond.id)
        XCTAssertEqual(payload["previous_client_message_id"] as? String, recoveredFirst.id)
        XCTAssertEqual(payload["time_zone"] as? String, recoveredSecond.timeZone)
        let uploadPayload = MordeConversationMessagePayload.uploadInitialization(recoveredSecond,
            bytes: Data(recoveredSecond.content.utf8))
        XCTAssertEqual(uploadPayload["time_zone"] as? String, recoveredSecond.timeZone)

        var legacyFields = try XCTUnwrap(JSONSerialization.jsonObject(with: JSONEncoder().encode(recoveredSecond)) as? [String: Any])
        legacyFields.removeValue(forKey: "timeZone")
        let legacy = try JSONDecoder().decode(MordeOutboxMessage.self,
            from: JSONSerialization.data(withJSONObject: legacyFields))
        XCTAssertNil(legacy.timeZone)
        XCTAssertNil(MordeConversationMessagePayload.direct(legacy)["time_zone"])

        let next = try reopenedOutbox.enqueue(scope: scope, conversationID: "c1234567", content: "第三条",
            modelID: nil, attachmentIDs: [])
        XCTAssertEqual(next.previousClientMessageID, recoveredSecond.id)
    }

    func testMessagePagesRejectOtherOrMissingConversations() {
        let first = MordeClientMessage(id: "m0000001", conversation_id: "c1234567", role: "user", content: "第一会话")
        let second = MordeClientMessage(id: "m0000002", conversation_id: "c7654321", role: "user", content: "第二会话")
        let missing = MordeClientMessage(id: "m0000003", role: "assistant", content: "未标识会话")

        XCTAssertTrue(MordeConversationMessagePagePolicy.matches([], conversationID: "c1234567"))
        XCTAssertTrue(MordeConversationMessagePagePolicy.matches([first], conversationID: "c1234567"))
        XCTAssertFalse(MordeConversationMessagePagePolicy.matches([first, second], conversationID: "c1234567"))
        XCTAssertFalse(MordeConversationMessagePagePolicy.matches([missing], conversationID: "c1234567"))
    }

    func testAuthorizationLossDoesNotClaimAnUncertainSendFailed() {
        let rejected = MordeOutboxFailurePolicy.confirmsRejection(
            submissionBegan: true, firstSubmission: true, hadUncertainSubmission: false,
            domain: "Morde", code: 401)
        XCTAssertFalse(rejected)
        XCTAssertEqual(MordeOutboxFailurePolicy.state(mayHaveBeenAccepted: true,
            confirmedRejection: rejected, domain: "Morde", code: 401), .unknown)
        XCTAssertEqual(MordeOutboxFailurePolicy.state(mayHaveBeenAccepted: false,
            confirmedRejection: false, domain: "Morde", code: 401), .pending)
    }

    func testOutboxReauthenticationKeepsTwoConversationQueuesIndependent() throws {
        let scope = try makeScope()
        let renewedScope = try XCTUnwrap(MordeClientDataScope(serviceOrigin: "https://morde.example",
            serviceInstanceID: scope.serviceInstanceID, accountID: scope.accountID, installationID: "install-2"))
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("morde-conversation-queues-\(UUID().uuidString)", isDirectory: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let outbox = MordeMessageOutbox(root: root)
        var first = try outbox.enqueue(scope: scope, conversationID: "c1234567", content: "会话一首条",
            modelID: nil, attachmentIDs: [])
        let second = try outbox.enqueue(scope: scope, conversationID: "c7654321", content: "会话二首条",
            modelID: nil, attachmentIDs: [])
        let firstFollowup = try outbox.enqueue(scope: scope, conversationID: "c1234567", content: "会话一后续",
            modelID: nil, attachmentIDs: [])
        first.state = .sending
        first.submissionStarted = true
        first.submissionUncertain = true
        try outbox.replace(first, scope: scope)

        let reopened = MordeMessageOutbox(root: root)
        let recovered = try reopened.recoverInterruptedMessages(scope: renewedScope)
        XCTAssertEqual(recovered.map(\.id), [first.id, second.id, firstFollowup.id])
        XCTAssertEqual(recovered.first?.state, .unknown)
        XCTAssertNil(second.previousClientMessageID)
        XCTAssertEqual(firstFollowup.previousClientMessageID, first.id)
        XCTAssertEqual(MordeOutboxScheduling.readyIDs(recovered, scope: renewedScope, inFlight: []),
                       [first.id, second.id])
        XCTAssertEqual(MordeConversationMessagePayload.direct(recovered[0])["client_message_id"] as? String, first.id)
        XCTAssertEqual(MordeConversationMessagePayload.direct(recovered[2])["previous_client_message_id"] as? String, first.id)
    }

    func testLocalConversationMappingAndMessageIdentitySurviveRestart() throws {
        let scope = try makeScope()
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("morde-local-conversation-\(UUID().uuidString)", isDirectory: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let localID = "local-12345678-1234-4234-8234-123456789012"
        let outbox = MordeMessageOutbox(root: root)
        let first = try outbox.enqueue(scope: scope, conversationID: localID, content: "待创建会话",
            modelID: nil, attachmentIDs: [])
        try outbox.resolveLocalConversation(localID, serverID: "c1234567", scope: scope)

        let reopened = MordeMessageOutbox(root: root)
        XCTAssertEqual(try reopened.resolvedConversationID(localID, scope: scope), "c1234567")
        try reopened.resolveLocalConversation(localID, serverID: "c1234567", scope: scope)
        XCTAssertThrowsError(try reopened.resolveLocalConversation(localID, serverID: "c7654321", scope: scope))
        let restored = try XCTUnwrap(reopened.messages(scope: scope).first)
        XCTAssertEqual(restored.id, first.id)
        XCTAssertEqual(restored.conversationID, "c1234567")
        let followup = try reopened.enqueue(scope: scope, conversationID: "c1234567", content: "后续消息",
            modelID: nil, attachmentIDs: [])
        XCTAssertEqual(followup.previousClientMessageID, first.id)
    }

    private func makeScope(instanceID: String = "11111111-1111-4111-8111-111111111111",
                           accountID: String = "user-1") throws -> MordeClientDataScope {
        try XCTUnwrap(MordeClientDataScope(serviceOrigin: "https://morde.example",
            serviceInstanceID: instanceID, accountID: accountID, installationID: "install-1"))
    }
}
