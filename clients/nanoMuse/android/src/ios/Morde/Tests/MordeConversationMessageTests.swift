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

    private func makeScope(instanceID: String = "11111111-1111-4111-8111-111111111111",
                           accountID: String = "user-1") throws -> MordeClientDataScope {
        try XCTUnwrap(MordeClientDataScope(serviceOrigin: "https://morde.example",
            serviceInstanceID: instanceID, accountID: accountID, installationID: "install-1"))
    }
}
