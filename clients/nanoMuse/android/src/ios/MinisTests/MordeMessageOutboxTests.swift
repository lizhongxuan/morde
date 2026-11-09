import XCTest
@testable import Minis

final class MordeMessageOutboxTests: XCTestCase {
    // nanoMuse: Keep restart recovery and the sole model-configuration send gate covered in the active MinisTests target.
    func testInterruptedSendsRecoverToReconciliationStates() throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("morde-outbox-recovery-\(UUID().uuidString)", isDirectory: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let scope = try XCTUnwrap(MordeClientDataScope(serviceOrigin: "https://morde.example",
            serviceInstanceID: "11111111-1111-4111-8111-111111111111", accountID: "user-1", installationID: "install-1"))
        let outbox = MordeMessageOutbox(root: root)

        var unknownDelivery = try outbox.enqueue(scope: scope, conversationID: "c1234567", content: "check delivery",
            modelID: nil, attachmentIDs: [])
        unknownDelivery.state = .sending
        unknownDelivery.submissionStarted = true
        unknownDelivery.submissionUncertain = true
        try outbox.replace(unknownDelivery, scope: scope)

        var acceptedAttachment = try outbox.enqueue(scope: scope, conversationID: "c7654321", content: "upload attachment",
            modelID: nil, attachmentIDs: [])
        acceptedAttachment.state = .sending
        acceptedAttachment.serverMessageID = "m1234567"
        try outbox.replace(acceptedAttachment, scope: scope)

        var activeSend = try outbox.enqueue(scope: scope, conversationID: "c3456789", content: "still in flight",
            modelID: nil, attachmentIDs: [])
        activeSend.state = .sending
        try outbox.replace(activeSend, scope: scope)

        let recovered = try outbox.recoverInterruptedMessages(scope: scope, excludingInFlight: [activeSend.id])
        XCTAssertEqual(recovered.first(where: { $0.id == unknownDelivery.id })?.state, .unknown)
        XCTAssertEqual(recovered.first(where: { $0.id == unknownDelivery.id })?.content, "check delivery")
        XCTAssertEqual(recovered.first(where: { $0.id == unknownDelivery.id })?.submissionUncertain, true)
        XCTAssertEqual(recovered.first(where: { $0.id == acceptedAttachment.id })?.state, .accepted)
        XCTAssertEqual(recovered.first(where: { $0.id == acceptedAttachment.id })?.serverMessageID, "m1234567")
        XCTAssertEqual(recovered.first(where: { $0.id == activeSend.id })?.state, .sending)
        XCTAssertEqual(try outbox.messages(scope: scope), recovered)
    }

    func testMissingModelBlocksOnlyUnacceptedMessages() throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("morde-outbox-model-gate-\(UUID().uuidString)", isDirectory: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let scope = try XCTUnwrap(MordeClientDataScope(serviceOrigin: "https://morde.example",
            serviceInstanceID: "11111111-1111-4111-8111-111111111111", accountID: "user-1", installationID: "install-1"))
        let outbox = MordeMessageOutbox(root: root)
        let pending = try outbox.enqueue(scope: scope, conversationID: "c1234567", content: "new message",
            modelID: nil, attachmentIDs: [])
        var unknownDelivery = try outbox.enqueue(scope: scope, conversationID: "c2345678", content: "reconcile old delivery",
            modelID: nil, attachmentIDs: [])
        unknownDelivery.state = .unknown
        unknownDelivery.submissionStarted = true
        unknownDelivery.submissionUncertain = true
        var accepted = try outbox.enqueue(scope: scope, conversationID: "c7654321", content: "finish attachment upload",
            modelID: nil, attachmentIDs: [])
        accepted.state = .accepted
        accepted.serverMessageID = "m7654321"

        let now = Date(timeIntervalSince1970: 1_800_000_000)
        let confirmedAbsent = !MordeCloudModelConfiguration.allowsSubmission(status: "not_configured", loaded: true,
            checkedAt: now, now: now)
        XCTAssertEqual(MordeOutboxScheduling.readyIDs([pending, unknownDelivery, accepted], scope: scope, inFlight: [],
            allowsNewSubmissions: !confirmedAbsent), [unknownDelivery.id, accepted.id])
        let lookupStillLoadingAllowsSubmission = MordeCloudModelConfiguration.allowsSubmission(status: "unknown",
            loaded: false, checkedAt: nil, now: now)
        XCTAssertEqual(MordeOutboxScheduling.readyIDs([pending, unknownDelivery, accepted], scope: scope, inFlight: [],
            allowsNewSubmissions: lookupStillLoadingAllowsSubmission), [pending.id, unknownDelivery.id, accepted.id])
        XCTAssertEqual(MordeOutboxScheduling.readyIDs([pending, unknownDelivery, accepted], scope: scope, inFlight: [],
            allowsNewSubmissions: true), [pending.id, unknownDelivery.id, accepted.id])
    }

    func testOnlyRecentConfirmedModelAbsenceBlocksNewOutboxSubmissions() {
        let now = Date(timeIntervalSince1970: 1_800_000_000)
        XCTAssertTrue(MordeCloudModelConfiguration.allowsSubmission(status: "unknown", loaded: false,
            checkedAt: nil, now: now), "A model lookup that is still loading is not a send gate.")
        XCTAssertTrue(MordeCloudModelConfiguration.allowsSubmission(status: "unknown", loaded: true,
            checkedAt: now, now: now), "An explicit unknown result must allow sending.")
        XCTAssertTrue(MordeCloudModelConfiguration.allowsSubmission(status: "not_configured", loaded: false,
            checkedAt: nil, now: now), "A failed lookup must not be treated as confirmed absence.")
        XCTAssertFalse(MordeCloudModelConfiguration.allowsSubmission(status: "not_configured", loaded: true,
            checkedAt: now, now: now), "A fresh successful absence check is the sole configuration gate.")
        XCTAssertTrue(MordeCloudModelConfiguration.allowsSubmission(status: "not_configured", loaded: true,
            checkedAt: now.addingTimeInterval(-301), now: now), "An expired absence result is unknown again.")

        XCTAssertFalse(MordeResourceLoadState.loading.isResolved)
        XCTAssertTrue(MordeResourceLoadState.failed("offline").isResolved)
        XCTAssertTrue(MordeResourceLoadState.loaded.isResolved)
    }

    func testPersonalAttachmentUploadResumeSkipsPersistedChunks() throws {
        let received = try XCTUnwrap(MordePersonalAttachmentUploadRecovery.receivedSequences([0, 2], chunkCount: 3))
        XCTAssertEqual(MordePersonalAttachmentUploadRecovery.missingSequences(received, chunkCount: 3), [1])
        XCTAssertEqual(MordePersonalAttachmentUploadRecovery.uploadedBytes(received,
            sizeBytes: Int64(MordePersonalAttachmentHash.chunkBytes * 2 + 1)),
            Int64(MordePersonalAttachmentHash.chunkBytes + 1))
        XCTAssertEqual(MordePersonalAttachmentUploadRecovery.receivedSequences(nil, chunkCount: 2), [])
        XCTAssertNil(MordePersonalAttachmentUploadRecovery.receivedSequences([1, 1], chunkCount: 2))
        XCTAssertNil(MordePersonalAttachmentUploadRecovery.receivedSequences([2], chunkCount: 2))
    }

    func testLongMultilingualMessageAndIdentitySurviveOutboxReopen() throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("morde-outbox-long-message-\(UUID().uuidString)", isDirectory: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let scope = try XCTUnwrap(MordeClientDataScope(serviceOrigin: "https://morde.example",
            serviceInstanceID: "11111111-1111-4111-8111-111111111111", accountID: "user-1", installationID: "install-1"))
        let content = String(repeating: "中文abc🙂", count: 7000) + "尾部完整"
        let original = MordeMessageOutbox(root: root)
        var queued = try original.enqueue(scope: scope, conversationID: "c1234567", content: content,
            modelID: nil, attachmentIDs: [])
        queued.state = .sending
        queued.submissionStarted = true
        queued.submissionUncertain = true
        try original.replace(queued, scope: scope)

        let reopened = MordeMessageOutbox(root: root)
        let recovered = try reopened.recoverInterruptedMessages(scope: scope, excludingInFlight: [])
        let restored = try XCTUnwrap(recovered.first)
        XCTAssertEqual(restored.id, queued.id)
        XCTAssertEqual(restored.content, content)
        XCTAssertGreaterThan(restored.content.utf8.count, MordeMessageChunking.directLimit)
        XCTAssertEqual(restored.submissionUncertain, true)
        XCTAssertEqual(try reopened.messages(scope: scope), recovered)
    }
}
