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

    func testSessionDecodesProposedLastSequenceField() throws {
        let data = Data(#"{"id":"l0000001","installation_id":"i0000001","title":"Example","cwd_label":"project","updated_at":null,"status":"in_progress","scopes":["read","live"],"last_sequence":42}"#.utf8)
        let session = try JSONDecoder().decode(MordeCodexSession.self, from: data)
        XCTAssertEqual(session.current_sequence, 42)
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
}
