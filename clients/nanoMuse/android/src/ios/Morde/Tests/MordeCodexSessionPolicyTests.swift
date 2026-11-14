import XCTest
@testable import MordeTaskPolicies

final class MordeCodexSessionPolicyTests: XCTestCase {
    func testMergeSortsAndDeduplicatesEventsBySequenceAndProviderID() throws {
        let expected = [event(1, "one"), event(2, "two")]
        let merged = try MordeCodexEventPolicy.merge([], incoming: [expected[1], expected[0], expected[1]],
                                                       sessionID: "l0000001")
        XCTAssertEqual(merged, expected)
    }

    func testMergeRejectsSequenceGapsAndConflictingDuplicates() {
        XCTAssertThrowsError(try MordeCodexEventPolicy.merge([], incoming: [event(1, "one"), event(3, "three")],
                                                              sessionID: "l0000001"))
        XCTAssertThrowsError(try MordeCodexEventPolicy.merge([], incoming: [event(1, "one"), event(1, "other")],
                                                              sessionID: "l0000001"))
        XCTAssertThrowsError(try MordeCodexEventPolicy.merge([], incoming: [event(1, "one"), event(2, "one")],
                                                              sessionID: "l0000001"))
    }

    func testSessionDecodesThreadIDLastSequenceAndActivityDate() throws {
        let data = Data(#"{"id":"l0000001","installation_id":"i0000001","thread_id":"native-thread-12345678","title":"Example","cwd_label":"project","updated_at":"2026-11-14T00:00:00Z","status":"in_progress","scopes":["read","live"],"last_sequence":42}"#.utf8)
        let session = try JSONDecoder().decode(MordeCodexSession.self, from: data)
        XCTAssertEqual(session.current_sequence, 42)
        XCTAssertEqual(session.shortThreadID, "12345678")
        XCTAssertNotNil(session.updatedAtDate)
        XCTAssertTrue(MordeCodexEventPolicy.isValidSessionID(session.id))
        XCTAssertFalse(MordeCodexEventPolicy.isValidSessionID("cx1234567"))
    }

    func testCursorReportsDuplicatesAndMissingSequence() {
        var cursor = MordeCodexEventCursor(afterSequence: 4)
        XCTAssertEqual(cursor.accept(event(4, "four"), sessionID: "l0000001"), .duplicate)
        XCTAssertEqual(cursor.accept(event(6, "six"), sessionID: "l0000001"),
                       .gap(expected: 5, received: 6))
        XCTAssertEqual(cursor.accept(event(5, "five"), sessionID: "l0000001"), .accepted)
        XCTAssertEqual(cursor.sequence, 5)
    }

    func testAssistantDeltasMergeAndFinalTextReplacesPartialText() throws {
        let deltas = [event(1, "one", kind: "assistant_delta", itemID: "item", text: "Hel"),
                      event(2, "two", kind: "assistant_delta", itemID: "item", text: "lo")]
        let partial = MordeCodexEventPolicy.timelineItems(deltas)
        XCTAssertEqual(partial.count, 1)
        XCTAssertEqual(partial.first?.text, "Hello")

        let final = event(3, "three", kind: "assistant_message", itemID: "item", text: "Hello!")
        XCTAssertEqual(MordeCodexEventPolicy.timelineItems(deltas + [final]).first?.text, "Hello!")
    }

    func testUnknownEventsDoNotExposeUnclassifiedPayloadText() {
        let unknown = event(1, "one", kind: "internal_tool_payload", text: "sensitive raw payload")
        let item = MordeCodexEventPolicy.timelineItems([unknown]).first
        XCTAssertEqual(item?.kind, .unsupported)
        XCTAssertNil(item?.text)
    }

    func testUnsupportedEventShowsOnlySafeDiagnosticField() {
        func unsupported(_ sequence: Int64, payload: [String: MordeCodexJSONValue]) -> MordeCodexEvent {
            MordeCodexEvent(session_id: "l0000001", sequence: sequence,
                provider_event_id: "unsupported-\(sequence)", kind: "unsupported",
                occurred_at: "2026-11-14T00:00:00Z", turn_id: nil, item_id: nil,
                parent_item_id: nil, status: nil, payload: payload, stored_at: "2026-11-14T00:00:00Z")
        }
        let safe = unsupported(1, payload: ["provider_type": .string("notification"),
                                             "diagnostic": .string("This Codex question cannot be answered on iOS.")])
        let arbitraryText = unsupported(2, payload: ["text": .string("private raw payload")])
        let rawJSON = unsupported(3, payload: ["diagnostic": .string(#"{"raw":"payload"}"#)])
        let longSafeDiagnostic = String(repeating: "question details ", count: 500)
        let longSafe = unsupported(4, payload: ["diagnostic": .string(longSafeDiagnostic)])
        let oversized = unsupported(5, payload: ["diagnostic": .string(String(repeating: "x", count: 24 * 1024 + 1))])

        let items = MordeCodexEventPolicy.timelineItems([safe, arbitraryText, rawJSON, longSafe, oversized])
        XCTAssertEqual(items[0].kind, .unsupported)
        XCTAssertEqual(items[0].text, "This Codex question cannot be answered on iOS.")
        XCTAssertNil(items[1].text)
        XCTAssertNil(items[2].text)
        XCTAssertEqual(items[3].text, longSafeDiagnostic)
        XCTAssertNil(items[4].text)
    }

    func testCommandStatusEventRestoresVisibleStatusWithoutCommandBody() {
        let statusEvent = MordeCodexEvent(session_id: "l0000001", sequence: 1,
            provider_event_id: "command-status-1", kind: "command_status",
            occurred_at: "2026-11-14T00:00:00Z", turn_id: nil, item_id: nil,
            parent_item_id: nil, status: nil,
            payload: ["command_id": .string("g0000001"), "type": .string("send"),
                      "status": .string("claimed")],
            stored_at: "2026-11-14T00:00:00Z")
        let item = MordeCodexEventPolicy.timelineItems([statusEvent]).first
        XCTAssertEqual(item?.kind, .commandStatus)
        XCTAssertEqual(item?.status, "claimed")
        XCTAssertEqual(item?.text, "send")
    }

    func testProviderToolEventDoesNotExposeRawJSONPayload() {
        let rawSnapshot = event(1, "one", kind: "tool_result", text: #"{"snapshot":{"pixels":"private"}}"#)
        XCTAssertNil(MordeCodexEventPolicy.timelineItems([rawSnapshot]).first?.text)
        let summarized = event(2, "two", kind: "tool_result", text: "Read the page title.")
        XCTAssertEqual(MordeCodexEventPolicy.timelineItems([summarized]).first?.text, "Read the page title.")
    }

    func testPendingApprovalWithoutOptionalExpiryCanBeAnsweredWithServerVersionCheck() {
        let approval = MordeCodexApproval(id: "approval-1", version: "v1", title: "Review change",
            object: nil, detail: nil, impact: nil, status: "pending", expiresAt: nil)
        XCTAssertTrue(approval.canRespond())
        XCTAssertFalse(MordeCodexApproval(id: "approval-1", version: "v1", title: "Review change",
            object: nil, detail: nil, impact: nil, status: "unknown", expiresAt: nil).canRespond())
    }

    func testPendingApprovalWithInvalidExpiryFailsClosed() {
        let approval = MordeCodexApproval(id: "approval-1", version: "v1", title: "Review change",
            object: nil, detail: nil, impact: nil, status: "pending", expiresAt: "not-a-date")
        XCTAssertFalse(approval.canRespond(at: Date(timeIntervalSince1970: 0)))
    }

    func testApprovalRequestDecodesProviderObjectDetailAndImpact() throws {
        let data = Data(#"{"session_id":"l0000001","sequence":1,"provider_event_id":"approval-1","kind":"approval_request","occurred_at":"2026-11-14T00:00:00Z","payload":{"approval_id":"approval-1","approval_version":"v1","kind":"command","summary":"Run project checks","object":"npm run check","detail":"work/project","impact":"Run the requested check"},"stored_at":"2026-11-14T00:00:00Z"}"#.utf8)
        let event = try JSONDecoder().decode(MordeCodexEvent.self, from: data)
        XCTAssertEqual(event.approval?.object, "npm run check")
        XCTAssertEqual(event.approval?.detail, "work/project")
        XCTAssertEqual(event.approval?.impact, "Run the requested check")
    }

    func testQuestionRequestPreservesOpaqueQuestionIDAndSecretFlags() throws {
        let data = Data(#"{"session_id":"l0000001","sequence":1,"provider_event_id":"question-1","kind":"question","occurred_at":"2026-11-14T00:00:00Z","payload":{"question_request_id":"100","question_version":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","status":"pending","questions":[{"id":" choice ","header":"Choice","question":"Which option?","options":[{"label":"A","description":"Option A"}],"is_secret":true,"is_other":true}]},"stored_at":"2026-11-14T00:00:00Z"}"#.utf8)
        let event = try JSONDecoder().decode(MordeCodexEvent.self, from: data)
        let question = try XCTUnwrap(event.questionRequest)
        XCTAssertEqual(question.id, "100")
        XCTAssertEqual(question.questions.first?.id, " choice ")
        XCTAssertTrue(question.questions.first?.isSecret == true)
        XCTAssertTrue(question.questions.first?.isOther == true)
        XCTAssertTrue(question.accepts([" choice ": ["A"]]))
        XCTAssertFalse(question.accepts(["choice": ["A"]]))
        XCTAssertFalse(question.accepts([" choice ": [" \t "]]))
        XCTAssertFalse(question.accepts([" choice ": ["A\u{0000}"]]))
    }

    func testQuestionTimelineAppliesLatestStatusToPendingCard() {
        let pending = questionEvent(1, "question-pending", status: "pending", questions: [
            MordeCodexJSONValue.object(["id": .string("q1"), "header": .string("Choice"),
                "question": .string("Which option?"), "options": .array([])])
        ])
        let stale = questionEvent(2, "question-stale", status: "stale")
        let items = MordeCodexEventPolicy.timelineItems([pending, stale])
        XCTAssertEqual(items.count, 1)
        XCTAssertEqual(items.first?.kind, .question)
        XCTAssertEqual(items.first?.question?.status, "stale")
        XCTAssertEqual(items.first?.question?.questions.first?.id, "q1")
    }

    func testSegmentedUserAndAssistantSnapshotsAreReassembledInIndexOrder() {
        let events = [
            textEvent(1, "assistant-segment-1", kind: "assistant_message", itemID: "assistant-item",
                      segmentID: "assistant-item:1", text: "answer"),
            textEvent(2, "user-segment-1", kind: "user_message", itemID: "user-item",
                      segmentID: "user-item:1", text: "world"),
            textEvent(3, "assistant-segment-0", kind: "assistant_message", itemID: "assistant-item",
                      segmentID: "assistant-item:0", text: "A complete "),
            textEvent(4, "user-segment-0", kind: "user_message", itemID: "user-item",
                      segmentID: "user-item:0", text: "hello ")
        ]

        let items = MordeCodexEventPolicy.timelineItems(events)
        XCTAssertEqual(items.count, 2)
        XCTAssertEqual(items.first(where: { $0.kind == .assistant })?.text, "A complete answer")
        XCTAssertEqual(items.first(where: { $0.kind == .user })?.text, "hello world")
    }

    func testAssistantSnapshotReplacesLiveDeltasWithoutDroppingSnapshotChunks() {
        let delta0 = textEvent(1, "delta-0", kind: "assistant_delta", itemID: "item",
                              text: "old ", deltaIndex: 0)
        let delta1 = textEvent(2, "delta-1", kind: "assistant_delta", itemID: "item",
                              text: "partial", deltaIndex: 1)
        let snapshot0 = textEvent(3, "snapshot-0", kind: "assistant_message", itemID: "item",
                                  segmentID: "item:0", text: "complete ")
        let snapshot1 = textEvent(4, "snapshot-1", kind: "assistant_message", itemID: "item",
                                  segmentID: "item:1", text: "answer")

        let items = MordeCodexEventPolicy.timelineItems([delta0, delta1, snapshot0, snapshot1])
        XCTAssertEqual(items.count, 1)
        XCTAssertEqual(items.first?.text, "complete answer")
    }

    func testVersionedSnapshotSegmentUpdatesOnlyItsMatchingChunk() {
        let original0 = textEvent(1, "snapshot-v1-0", kind: "assistant_message", itemID: "item",
                                  segmentID: "item:v1:0", text: "old ")
        let original1 = textEvent(2, "snapshot-v1-1", kind: "assistant_message", itemID: "item",
                                  segmentID: "item:v1:1", text: "ending")
        let updated0 = textEvent(3, "snapshot-v2-0", kind: "assistant_message", itemID: "item",
                                 segmentID: "item:v2:0", text: "new ")

        let items = MordeCodexEventPolicy.timelineItems([original0, original1, updated0])
        XCTAssertEqual(items.first?.text, "new ending")
    }

    func testReplyLocksEndOnlyForMatchingTerminalEventOrCommandResult() throws {
        let approval = approvalEvent(1, "approval-pending", status: "pending")
        let approvalIdentity = try XCTUnwrap(approval.replyIdentity)
        XCTAssertNil(MordeCodexReplyPolicy.terminalReplyIdentity(for: approval))
        XCTAssertEqual(MordeCodexReplyPolicy.terminalReplyIdentity(
            for: approvalEvent(2, "approval-resolved", status: "approved")), approvalIdentity)

        let pendingQuestion = questionEvent(3, "question-pending", status: "pending", questions: [
            MordeCodexJSONValue.object(["id": .string("q1"), "header": .string("Choice"),
                "question": .string("Which option?"), "options": .array([])])
        ])
        let questionIdentity = try XCTUnwrap(pendingQuestion.replyIdentity)
        XCTAssertNil(MordeCodexReplyPolicy.terminalReplyIdentity(for: pendingQuestion))
        XCTAssertEqual(MordeCodexReplyPolicy.terminalReplyIdentity(
            for: questionEvent(4, "question-answered", status: "answered")), questionIdentity)

        let waiting = commandStatusEvent(5, status: "waiting_pc", commandID: "g0000001")
        XCTAssertNil(MordeCodexReplyPolicy.terminalCommandID(for: waiting))
        XCTAssertEqual(MordeCodexReplyPolicy.terminalCommandID(
            for: commandStatusEvent(6, status: "failed", commandID: "g0000001")), "g0000001")
        XCTAssertTrue(MordeCodexReplyPolicy.isTerminalCommandStatus("completed"))
        XCTAssertFalse(MordeCodexReplyPolicy.isTerminalCommandStatus("waiting_pc"))
    }

    func testLiveOnlyScopeRevocationPreservesReadHistory() {
        XCTAssertFalse(MordeCodexSessionScopePolicy.shouldClearReadHistory(afterRefreshing: ["read", "send"]))
        XCTAssertTrue(MordeCodexSessionScopePolicy.shouldClearReadHistory(afterRefreshing: ["send"]))
        XCTAssertFalse(MordeCodexSessionScopePolicy.shouldClearReadHistory(afterRefreshing: nil))
    }

    func testQuestionRequestRejectsDuplicateQuestionIDs() {
        let duplicate = MordeCodexJSONValue.object(["id": .string("q1"), "header": .string("Choice"),
            "question": .string("Which option?"), "options": .array([])])
        let event = questionEvent(1, "question-duplicate", status: "pending", questions: [duplicate, duplicate])
        XCTAssertNil(event.questionRequest)
        XCTAssertNotEqual(MordeCodexEventPolicy.timelineItems([event]).first?.kind, .question)
    }

    func testQuestionRequestRequiresLowercaseSHA256Version() {
        let value = MordeCodexJSONValue.object(["id": .string("q1"), "header": .string("Choice"),
            "question": .string("Which option?"), "options": .array([])])
        XCTAssertNotNil(questionEvent(1, "question-valid-version", status: "pending", questions: [value]).questionRequest)
        XCTAssertNil(questionEvent(2, "question-short-version", status: "pending", version: "v1",
            questions: [value]).questionRequest)
        XCTAssertNil(questionEvent(3, "question-uppercase-version", status: "pending",
            version: String(repeating: "A", count: 64), questions: [value]).questionRequest)
    }

    func testSSEParserKeepsFrameBoundariesAndMultiLineData() throws {
        var parser = MordeCodexSSEParser()
        let wire = "id: 7\nevent: codex.event\ndata: {\"text\":\ndata: \"hello\"}\n\n"
        var frames: [MordeCodexSSEFrame] = []
        for byte in wire.utf8 {
            if let frame = try parser.append(byte) { frames.append(frame) }
        }
        XCTAssertEqual(frames, [.init(id: "7", event: "codex.event", data: "{\"text\":\n\"hello\"}")])
    }

    private func event(_ sequence: Int64, _ providerID: String, kind: String = "assistant_message",
                       itemID: String? = nil, text: String = "") -> MordeCodexEvent {
        MordeCodexEvent(session_id: "l0000001", sequence: sequence, provider_event_id: providerID,
                        kind: kind, occurred_at: "2026-11-14T00:00:00Z", turn_id: nil, item_id: itemID,
                        parent_item_id: nil, status: nil, payload: ["text": .string(text)],
                        stored_at: "2026-11-14T00:00:00Z")
    }

    private func textEvent(_ sequence: Int64, _ providerID: String, kind: String, itemID: String,
                          segmentID: String? = nil, text: String, deltaIndex: Int? = nil) -> MordeCodexEvent {
        var payload: [String: MordeCodexJSONValue] = ["text": .string(text)]
        if let segmentID { payload["segment_id"] = .string(segmentID) }
        if let deltaIndex { payload["delta_index"] = .integer(Int64(deltaIndex)) }
        return MordeCodexEvent(session_id: "l0000001", sequence: sequence, provider_event_id: providerID,
            kind: kind, occurred_at: "2026-11-14T00:00:00Z", turn_id: "turn-1", item_id: itemID,
            parent_item_id: nil, status: nil, payload: payload, stored_at: "2026-11-14T00:00:00Z")
    }

    private func approvalEvent(_ sequence: Int64, _ providerID: String, status: String) -> MordeCodexEvent {
        MordeCodexEvent(session_id: "l0000001", sequence: sequence, provider_event_id: providerID,
            kind: status == "pending" ? "approval_request" : "approval_status",
            occurred_at: "2026-11-14T00:00:00Z", turn_id: nil, item_id: nil, parent_item_id: nil,
            status: nil, payload: ["approval_id": .string("approval-1"), "approval_version": .string("v1"),
                                  "approval_status": .string(status)], stored_at: "2026-11-14T00:00:00Z")
    }

    private func commandStatusEvent(_ sequence: Int64, status: String, commandID: String) -> MordeCodexEvent {
        MordeCodexEvent(session_id: "l0000001", sequence: sequence,
            provider_event_id: "command-status-\(sequence)", kind: "command_status",
            occurred_at: "2026-11-14T00:00:00Z", turn_id: nil, item_id: nil, parent_item_id: nil,
            status: status, payload: ["command_id": .string(commandID)], stored_at: "2026-11-14T00:00:00Z")
    }

    private func questionEvent(_ sequence: Int64, _ providerID: String, status: String,
                               version: String = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
                               questions: [MordeCodexJSONValue] = []) -> MordeCodexEvent {
        var payload: [String: MordeCodexJSONValue] = [
            "question_request_id": .string("100"), "question_version": .string(version), "status": .string(status)
        ]
        if !questions.isEmpty { payload["questions"] = .array(questions) }
        return MordeCodexEvent(session_id: "l0000001", sequence: sequence, provider_event_id: providerID,
            kind: "question", occurred_at: "2026-11-14T00:00:00Z", turn_id: nil, item_id: "item-1",
            parent_item_id: nil, status: nil, payload: payload, stored_at: "2026-11-14T00:00:00Z")
    }
}
