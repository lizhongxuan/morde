import XCTest
@testable import Minis

final class MordeConversationTasksPolicyTests: XCTestCase {
    private func task(_ id: String, conversationID: String = "c1234567", sequence: Int? = 1) throws -> MordeTask {
        var value: [String: Any] = [
            "id": id, "conversation_id": conversationID, "objective": "Synthetic task",
            "status": "running", "step_count": 0, "completed_step_count": 0,
            "updated_at": "2026-11-06T00:00:00Z"
        ]
        if let sequence { value["event_sequence"] = sequence }
        return try JSONDecoder().decode(MordeTask.self, from: JSONSerialization.data(withJSONObject: value))
    }

    func testIndependentTasksRemainScopedToConversation() throws {
        let first = try task("q1234567", sequence: 1)
        let second = try task("q2345678", sequence: 12)
        XCTAssertTrue(MordeConversationTasksPolicy.valid([first, second], conversationID: "c1234567"))
        XCTAssertFalse(MordeConversationTasksPolicy.valid([first, first], conversationID: "c1234567"))
        XCTAssertFalse(MordeConversationTasksPolicy.valid([first, try task("q2345678", conversationID: "c2345678")],
                                                           conversationID: "c1234567"))
    }

    func testTaskListRequiresNonnegativeEventCursor() throws {
        XCTAssertFalse(MordeConversationTasksPolicy.valid([try task("q1234567", sequence: nil)],
                                                           conversationID: "c1234567"))
        XCTAssertFalse(MordeConversationTasksPolicy.valid([try task("q1234567", sequence: -1)],
                                                           conversationID: "c1234567"))
    }

    func testRepliesFromParallelTasksStayWithTheirOwnTaskAndConversation() {
        let first = MordeClientMessage(id: "m1234567", conversation_id: "c1234567", task_id: "q1234567",
                                       role: "assistant", content: "First reply", status: "streaming")
        let second = MordeClientMessage(id: "m2345678", conversation_id: "c1234567", task_id: "q2345678",
                                        role: "assistant", content: "Second reply", status: "streaming")
        let firstUpdate = MordeClientMessage(id: first.id, conversation_id: "c1234567", task_id: "q1234567",
                                             role: "assistant", content: "First completed", status: "completed")
        let initial = MordeTaskSnapshotMessageMerge.merge([], snapshot: first, taskID: "q1234567", conversationID: "c1234567")
        let both = MordeTaskSnapshotMessageMerge.merge(initial, snapshot: second, taskID: "q2345678", conversationID: "c1234567")
        let updated = MordeTaskSnapshotMessageMerge.merge(both, snapshot: firstUpdate, taskID: "q1234567", conversationID: "c1234567")
        XCTAssertEqual(updated.map(\.content), ["First completed", "Second reply"])
        XCTAssertEqual(MordeTaskSnapshotMessageMerge.merge(updated, snapshot: second, taskID: "q1234567",
                                                            conversationID: "c1234567"), updated)
        XCTAssertEqual(MordeTaskSnapshotMessageMerge.merge(updated, snapshot: second, taskID: "q2345678",
                                                            conversationID: "c8765432"), updated)
    }

    func testCombinedReplySourcesKeepOrderAndResolveTheirUserMessages() {
        let firstMessage = MordeClientMessage(id: "m1111111", conversation_id: "c1234567", sequence: 1,
            role: "user", content: "帮我整理第一项")
        let secondMessage = MordeClientMessage(id: "m2222222", conversation_id: "c1234567", sequence: 2,
            role: "user", content: "再核对第二项")
        let sources = [
            MordeClientMessageSource(user_message_id: firstMessage.id, task_id: "q1111111", task_revision: 2,
                                     event_sequence: 5, content: "第一项已完成。"),
            MordeClientMessageSource(user_message_id: secondMessage.id, task_id: "q2222222", task_revision: 1,
                                     event_sequence: 8, content: "第二项已核对。"),
        ]

        let lines = MordeConversationMessageSourcePresentation.lines(sources, userMessages: [firstMessage, secondMessage])

        XCTAssertEqual(lines.map(\.source.content), ["第一项已完成。", "第二项已核对。"])
        XCTAssertEqual(lines.map(\.originLabel), ["关联消息：帮我整理第一项", "关联消息：再核对第二项"])
        XCTAssertEqual(lines.map(\.source.task_id), ["q1111111", "q2222222"])
    }

    func testStreamingSnapshotFollowsTaskTerminalState() {
        let snapshot = MordeClientMessage(id: "m1234567", conversation_id: "c1234567", task_id: "q1234567",
            role: "assistant", content: "Final answer", status: "completed", output_status: "streaming")

        let running = MordeTaskSnapshotMessageMerge.reconcilingStreamingSnapshot(snapshot, taskStatus: "running")
        let runningTranscript = MordeTaskSnapshotMessageMerge.merge([], snapshot: running,
            taskID: "q1234567", conversationID: "c1234567")
        XCTAssertEqual(runningTranscript.first?.displayStatus, "streaming")

        let completed = MordeTaskSnapshotMessageMerge.reconcilingStreamingSnapshot(snapshot, taskStatus: "completed")
        let completedTranscript = MordeTaskSnapshotMessageMerge.merge([], snapshot: completed,
            taskID: "q1234567", conversationID: "c1234567")
        XCTAssertEqual(completedTranscript.first?.displayStatus, "completed")
        XCTAssertEqual(completedTranscript.first?.content, snapshot.content)

        let failed = MordeTaskSnapshotMessageMerge.reconcilingStreamingSnapshot(snapshot, taskStatus: "failed")
        let failedTranscript = MordeTaskSnapshotMessageMerge.merge([], snapshot: failed,
            taskID: "q1234567", conversationID: "c1234567")
        XCTAssertEqual(failedTranscript.first?.displayStatus, "interrupted")
        let cancelled = MordeTaskSnapshotMessageMerge.reconcilingStreamingSnapshot(snapshot, taskStatus: "cancelled")
        let cancelledTranscript = MordeTaskSnapshotMessageMerge.merge([], snapshot: cancelled,
            taskID: "q1234567", conversationID: "c1234567")
        XCTAssertEqual(cancelledTranscript.first?.displayStatus, "interrupted")
    }

    func testCombinedReplySourceWithoutLoadedUserMessageKeepsStableMessageID() {
        let source = MordeClientMessageSource(user_message_id: "m9999999", task_id: "q1111111", task_revision: 1,
                                              event_sequence: 3, content: "结果片段")
        let line = MordeConversationMessageSourcePresentation.lines([source], userMessages: []).first

        XCTAssertEqual(line?.originLabel, "关联消息 m9999999")
        XCTAssertEqual(line?.source.content, "结果片段")
    }

    func testLongMultilingualMessageChunkingReassemblesExactUTF8Bytes() throws {
        let text = String(repeating: "中文abc🙂", count: 7000) + "尾部完整"
        let bytes = Data(text.utf8)
        let count = MordeMessageChunking.count(for: bytes)
        let chunks = try (0..<count).map { sequence -> Data in
            try XCTUnwrap(MordeMessageChunking.chunk(bytes, sequence: sequence))
        }

        XCTAssertGreaterThan(bytes.count, MordeMessageChunking.directLimit)
        XCTAssertLessThanOrEqual(bytes.count, MordeMessageChunking.messageLimit)
        XCTAssertGreaterThan(count, 2)
        XCTAssertTrue(chunks.allSatisfy { !$0.isEmpty && $0.count <= MordeMessageChunking.chunkSize })
        XCTAssertEqual(Data(chunks.flatMap(Array.init)), bytes)
        XCTAssertEqual(String(data: Data(chunks.flatMap(Array.init)), encoding: .utf8), text)
        XCTAssertEqual(MordeMessageChunking.chunk(bytes, sequence: -1), nil)
        XCTAssertEqual(MordeMessageChunking.chunk(bytes, sequence: count), nil)
        XCTAssertEqual(MordeMessageChunking.count(for: Data()), 0)
        XCTAssertNil(MordeMessageChunking.chunk(Data(), sequence: 0))
    }
}
