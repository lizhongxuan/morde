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
        let data = Data(#"{"id":"l0000001","installation_id":"i0000001","profile_key":"profile-a","thread_id":"native-thread-12345678","title":"Example","cwd_label":"project","updated_at":"2026-11-14T00:00:00Z","status":"in_progress","scopes":["read","live"],"last_sequence":42}"#.utf8)
        let session = try JSONDecoder().decode(MordeCodexSession.self, from: data)
        XCTAssertEqual(session.current_sequence, 42)
        XCTAssertEqual(session.profile_key, "profile-a")
        XCTAssertEqual(session.shortThreadID, "12345678")
        XCTAssertNotNil(session.updatedAtDate)
        XCTAssertTrue(MordeCodexEventPolicy.isValidSessionID(session.id))
        XCTAssertFalse(MordeCodexEventPolicy.isValidSessionID("cx1234567"))
    }

    func testLegacySessionWithoutProfileKeyRemainsReadable() throws {
        let data = Data(#"{"id":"l0000001","installation_id":"i0000001","thread_id":"native-thread-12345678","title":"Example","cwd_label":"project","status":"idle","scopes":["read"],"last_sequence":0}"#.utf8)
        let session = try JSONDecoder().decode(MordeCodexSession.self, from: data)
        XCTAssertNil(session.profile_key)
        XCTAssertEqual(MordeCodexSessionScopePolicy.sessions([session], profileKey: nil), [session])
    }

    func testSessionCursorRejectsEmptyImmediateAndRepeatedPageTokens() {
        XCTAssertTrue(MordeCodexSessionScopePolicy.acceptsNextCursor("next", after: nil, seen: []))
        XCTAssertTrue(MordeCodexSessionScopePolicy.acceptsNextCursor(nil, after: "current", seen: ["prior"]))
        XCTAssertFalse(MordeCodexSessionScopePolicy.acceptsNextCursor("", after: nil, seen: []))
        XCTAssertFalse(MordeCodexSessionScopePolicy.acceptsNextCursor("current", after: "current", seen: []))
        XCTAssertFalse(MordeCodexSessionScopePolicy.acceptsNextCursor("prior", after: "current", seen: ["prior"]))
    }

    func testSessionRefreshOnlyRemovesMissingSessionsAfterFinalPage() {
        let previous: Set<String> = ["l0000001", "l0000002"]
        let observed: Set<String> = ["l0000001"]
        XCTAssertEqual(MordeCodexSessionScopePolicy.sessionsMissingFromCompletedRefresh(
            previous: previous, observed: observed, finished: false), [])
        XCTAssertEqual(MordeCodexSessionScopePolicy.sessionsMissingFromCompletedRefresh(
            previous: previous, observed: observed, finished: true), ["l0000002"])
    }

    func testSessionProfilesCanBeFilteredWithinAnInstallation() throws {
        func session(_ id: String, profileKey: String) throws -> MordeCodexSession {
            let data = try JSONSerialization.data(withJSONObject: [
                "id": id, "installation_id": "i0000001", "profile_key": profileKey,
                "thread_id": "thread-\(profileKey)", "title": profileKey, "cwd_label": "project",
                "status": "idle", "scopes": ["read", "live"], "last_sequence": 0,
            ])
            return try JSONDecoder().decode(MordeCodexSession.self, from: data)
        }
        let sessions = [try session("l0000001", profileKey: "profile-a"),
                        try session("l0000002", profileKey: "profile-b")]
        XCTAssertEqual(MordeCodexSessionScopePolicy.profileKeys(in: sessions), ["profile-a", "profile-b"])
        XCTAssertEqual(MordeCodexSessionScopePolicy.sessions(sessions, profileKey: "profile-b").map(\.id),
                       ["l0000002"])
        XCTAssertEqual(MordeCodexSessionScopePolicy.sessions(sessions, profileKey: nil), sessions)
    }

    func testProfileConnectionDecodesDiscoveryAndHistoryProgress() throws {
        let data = Data(#"{"installation_id":"i0000001","profile_key":"profile-a","scopes":["read","live","send"],"grant_version":2,"connected":true,"updated_at":"2026-11-14T00:00:00Z","session_discovery":{"state":"partial","error_code":"connection_lost","updated_at":"2026-11-14T00:01:00Z"},"history_sync":{"discovered_sessions":4,"history_complete_sessions":2,"pending_upload_event_count":5}}"#.utf8)
        let connection = try JSONDecoder().decode(MordeCodexProfileConnection.self, from: data)
        XCTAssertTrue(MordeCodexProfileConnectionPolicy.isValid(connection, installationID: "i0000001"))
        XCTAssertEqual(connection.session_discovery?.state, "partial")
        XCTAssertEqual(connection.session_discovery?.error_code, "connection_lost")
        XCTAssertEqual(connection.history_sync?.discovered_sessions, 4)
        XCTAssertEqual(connection.history_sync?.history_complete_sessions, 2)
        XCTAssertEqual(connection.history_sync?.pending_upload_event_count, 5)
        XCTAssertFalse(MordeCodexProfileConnectionPolicy.isValid(connection, installationID: "i0000002"))
    }

    func testProfileConnectionWithoutReportedStatusIsValidFirstSyncState() throws {
        let data = Data(#"{"installation_id":"i0000001","profile_key":"profile-a","scopes":["read","live"],"grant_version":1,"connected":true,"updated_at":"2026-11-14T00:00:00Z","session_discovery":null,"history_sync":null}"#.utf8)
        let connection = try JSONDecoder().decode(MordeCodexProfileConnection.self, from: data)
        XCTAssertTrue(MordeCodexProfileConnectionPolicy.isValid(connection, installationID: "i0000001"))
        XCTAssertNil(connection.session_discovery)
        XCTAssertNil(connection.history_sync)
    }

    func testProfileConnectionRejectsInconsistentHistoryCounts() throws {
        let data = Data(#"{"installation_id":"i0000001","profile_key":"profile-a","scopes":["read","live"],"grant_version":1,"connected":true,"updated_at":"2026-11-14T00:00:00Z","session_discovery":{"state":"synced","error_code":null,"updated_at":"2026-11-14T00:01:00Z"},"history_sync":{"discovered_sessions":1,"history_complete_sessions":2,"pending_upload_event_count":0}}"#.utf8)
        let connection = try JSONDecoder().decode(MordeCodexProfileConnection.self, from: data)
        XCTAssertFalse(MordeCodexProfileConnectionPolicy.isValid(connection, installationID: "i0000001"))
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

    func testReasoningSummaryIsExpandableAndPrivateReasoningIsNotExposed() {
        let summary = MordeCodexEventPolicy.timelineItems([
            event(1, "summary-1", kind: "reasoning_summary", text: "Public reasoning summary")
        ]).first
        XCTAssertEqual(summary?.kind, .reasoning)
        XCTAssertNil(summary?.text)
        XCTAssertEqual(summary?.details, [MordeCodexTimelineDetail(
            field: .reasoning, text: "Public reasoning summary", state: .complete)])

        let privateReasoning = MordeCodexEventPolicy.timelineItems([
            event(2, "private-1", kind: "reasoning", text: "private model reasoning")
        ]).first
        XCTAssertEqual(privateReasoning?.kind, .unsupported)
        XCTAssertNil(privateReasoning?.text)
        XCTAssertTrue(privateReasoning?.details.isEmpty == true)
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

    func testToolCallGroupsInputAndOutputPartsByCallIDInPartOrder() {
        let events = [
            payloadEvent(1, "call-1", kind: "tool_call", payload: [
                "call_id": .string("call-1"), "tool": .string("shell"),
                "input": .string("second"), "input_part": .integer(1), "input_parts": .integer(2),
            ]),
            payloadEvent(2, "call-2", kind: "tool_call", payload: [
                "call_id": .string("call-1"), "summary": .string("Run command"),
                "input": .string("first"), "input_part": .integer(0), "input_parts": .integer(2),
            ]),
            payloadEvent(3, "result-1", kind: "tool_result", payload: [
                "call_id": .string("call-1"), "output": .string(" world"),
                "output_part": .integer(1), "output_parts": .integer(2),
            ]),
            payloadEvent(4, "result-2", kind: "tool_result", payload: [
                "call_id": .string("call-1"), "output": .string("Hello"),
                "output_part": .integer(0), "output_parts": .integer(2),
            ], status: "completed"),
        ]

        let items = MordeCodexEventPolicy.timelineItems(events)
        XCTAssertEqual(items.count, 1)
        let item = items[0]
        XCTAssertEqual(item.id, "tool-call-1")
        XCTAssertEqual(item.sequence, 1)
        XCTAssertEqual(item.text, "Run command")
        XCTAssertEqual(item.status, "completed")
        XCTAssertEqual(item.details.map(\.field), [.input, .output])
        XCTAssertEqual(item.details[0].text, "firstsecond")
        XCTAssertEqual(item.details[0].state, .complete)
        XCTAssertEqual(item.details[1].text, "Hello world")
        XCTAssertEqual(item.details[1].state, .complete)
    }

    func testDeclaredOutputSnapshotReplacesOverlappingStaleDeltas() {
        let events = [
            payloadEvent(1, "call-1", kind: "tool_call", payload: [
                "call_id": .string("call-1"), "tool": .string("shell"),
            ]),
            payloadEvent(2, "delta-0", kind: "tool_output_delta", payload: [
                "call_id": .string("call-1"), "output": .string("stale delta"), "output_part": .integer(0),
            ]),
            payloadEvent(3, "snapshot-0", kind: "tool_result", payload: [
                "call_id": .string("call-1"), "output": .string("final "),
                "output_part": .integer(0), "output_parts": .integer(2),
            ]),
            payloadEvent(4, "snapshot-1", kind: "tool_result", payload: [
                "call_id": .string("call-1"), "output": .string("output"),
                "output_part": .integer(1), "output_parts": .integer(2),
            ]),
        ]

        let output = MordeCodexEventPolicy.timelineItems(events).first?.details
            .first(where: { $0.field == .output })
        XCTAssertEqual(output?.text, "final output")
        XCTAssertEqual(output?.state, .complete)
    }

    func testIncompleteToolInputAndUnsealedOutputAreMarkedInTimeline() {
        let call = payloadEvent(1, "call-1", kind: "tool_call", payload: [
            "call_id": .string("call-1"), "tool": .string("mcp"),
            "input": .string("partial"), "input_part": .integer(0), "input_parts": .integer(2),
        ])
        let outputDelta = payloadEvent(2, "result-1", kind: "tool_result", payload: [
            "call_id": .string("call-1"), "output": .string("still arriving"), "output_part": .integer(0),
        ])

        let item = MordeCodexEventPolicy.timelineItems([call, outputDelta]).first
        XCTAssertEqual(item?.details.first(where: { $0.field == .input })?.state, .incomplete)
        XCTAssertEqual(item?.details.first(where: { $0.field == .output })?.state, .streaming)
        XCTAssertEqual(item?.details.first(where: { $0.field == .output })?.text, "still arriving")
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

    func testLateReadResponsesRequireCurrentSessionInstallationAndScopes() throws {
        func session(scopes: [String]) throws -> MordeCodexSession {
            let data = try JSONSerialization.data(withJSONObject: [
                "id": "l0000001", "installation_id": "i0000001", "thread_id": "thread-1",
                "title": "Example", "cwd_label": "project", "status": "idle",
                "scopes": scopes, "last_sequence": 1,
            ])
            return try JSONDecoder().decode(MordeCodexSession.self, from: data)
        }
        let readable = try session(scopes: ["read", "live"])
        XCTAssertTrue(MordeCodexSessionScopePolicy.permitsReadResponse(readable,
            selectedSessionID: readable.id, installationAuthorized: true, revoked: false))
        XCTAssertFalse(MordeCodexSessionScopePolicy.permitsReadResponse(readable,
            selectedSessionID: "l0000002", installationAuthorized: true, revoked: false))
        XCTAssertFalse(MordeCodexSessionScopePolicy.permitsReadResponse(readable,
            selectedSessionID: readable.id, installationAuthorized: false, revoked: false))
        XCTAssertFalse(MordeCodexSessionScopePolicy.permitsReadResponse(readable,
            selectedSessionID: readable.id, installationAuthorized: true, revoked: true))
        let unreadable = try session(scopes: ["send"])
        let readOnly = try session(scopes: ["read"])
        XCTAssertFalse(MordeCodexSessionScopePolicy.permitsReadResponse(unreadable,
            selectedSessionID: "l0000001", installationAuthorized: true, revoked: false))
        XCTAssertFalse(MordeCodexSessionScopePolicy.permitsReadResponse(readOnly,
            selectedSessionID: "l0000001", installationAuthorized: true, revoked: false, requireLive: true))
    }

    func testLateCommandResponsesRequireCurrentReadAndActionGrants() throws {
        let data = try JSONSerialization.data(withJSONObject: [
            "id": "l0000001", "installation_id": "i0000001", "thread_id": "thread-1",
            "title": "Example", "cwd_label": "project", "status": "idle",
            "scopes": ["read", "send"], "last_sequence": 1,
        ])
        let sendable = try JSONDecoder().decode(MordeCodexSession.self, from: data)
        XCTAssertTrue(MordeCodexSessionScopePolicy.permitsCommandResponse(sendable,
            installationAuthorized: true, revoked: false, requiredScope: "send"))
        XCTAssertFalse(MordeCodexSessionScopePolicy.permitsCommandResponse(sendable,
            installationAuthorized: false, revoked: false, requiredScope: "send"))
        XCTAssertFalse(MordeCodexSessionScopePolicy.permitsCommandResponse(sendable,
            installationAuthorized: true, revoked: true, requiredScope: "send"))
        XCTAssertFalse(MordeCodexSessionScopePolicy.permitsCommandResponse(sendable,
            installationAuthorized: true, revoked: false, requiredScope: "approval"))

        let noReadData = try JSONSerialization.data(withJSONObject: [
            "id": "l0000001", "installation_id": "i0000001", "thread_id": "thread-1",
            "title": "Example", "cwd_label": "project", "status": "idle",
            "scopes": ["send"], "last_sequence": 1,
        ])
        let noRead = try JSONDecoder().decode(MordeCodexSession.self, from: noReadData)
        XCTAssertFalse(MordeCodexSessionScopePolicy.permitsCommandResponse(noRead,
            installationAuthorized: true, revoked: false, requiredScope: "send"))
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

    private func payloadEvent(_ sequence: Int64, _ providerID: String, kind: String,
                             payload: [String: MordeCodexJSONValue], status: String? = nil) -> MordeCodexEvent {
        MordeCodexEvent(session_id: "l0000001", sequence: sequence, provider_event_id: providerID,
                        kind: kind, occurred_at: "2026-11-14T00:00:00Z", turn_id: "turn-1", item_id: nil,
                        parent_item_id: nil, status: status, payload: payload,
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
