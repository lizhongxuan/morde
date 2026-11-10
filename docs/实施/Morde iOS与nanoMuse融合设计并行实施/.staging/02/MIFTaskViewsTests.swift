import XCTest
@testable import Minis

final class MIFTaskViewsTests: XCTestCase {
    func testPageMergePreservesOrderAndDropsDuplicateTaskIDs() {
        let first = makeTask(id: "q1234567", status: "running")
        let second = makeTask(id: "q2345678", status: "queued")
        let duplicate = makeTask(id: "q1234567", status: "completed")

        let merged = MIFTaskPageRules.merge(current: [first], incoming: [duplicate, second])

        XCTAssertEqual(merged.map(\.id), ["q1234567", "q2345678"])
        XCTAssertEqual(merged.first?.status, "completed")
    }

    func testPaginationRequiresAnAdvancingNonemptyCursorAndSingleRequest() {
        XCTAssertTrue(MIFTaskPageRules.canLoadMore(cursor: "next", isLoading: false))
        XCTAssertFalse(MIFTaskPageRules.canLoadMore(cursor: nil, isLoading: false))
        XCTAssertFalse(MIFTaskPageRules.canLoadMore(cursor: "", isLoading: false))
        XCTAssertFalse(MIFTaskPageRules.canLoadMore(cursor: "next", isLoading: true))
        XCTAssertTrue(MIFTaskPageRules.hasValidNextCursor(requested: "cursor-1", returned: "cursor-2"))
        XCTAssertTrue(MIFTaskPageRules.hasValidNextCursor(requested: "cursor-1", returned: nil))
        XCTAssertFalse(MIFTaskPageRules.hasValidNextCursor(requested: "cursor-1", returned: "cursor-1"))
        XCTAssertFalse(MIFTaskPageRules.hasValidNextCursor(requested: nil, returned: ""))
    }

    func testTaskActionsFollowOnlyKnownCurrentStates() {
        XCTAssertEqual(MIFTaskActionRules.options(for: makeTask(id: "q1234567", status: "running")).map(\.id),
                       ["pause", "cancel"])
        XCTAssertEqual(MIFTaskActionRules.options(for: makeTask(id: "q1234567", status: "paused")).map(\.id),
                       ["resume", "cancel"])
        XCTAssertEqual(MIFTaskActionRules.options(for: makeTask(id: "q1234567", status: "waiting_user")).map(\.id),
                       ["cancel"])
        XCTAssertTrue(MIFTaskActionRules.options(for: makeTask(id: "q1234567", status: "future_status")).isEmpty)
    }

    func testFailedTaskCanRetryOnlyFailedBatchItems() {
        let task = makeTask(id: "q1234567", status: "failed", batchItems: [
            MordeBatchItem(id: "b1234567", key: "first", status: "failed"),
            MordeBatchItem(id: "b2345678", key: "second", status: "completed"),
        ])

        let options = MIFTaskActionRules.options(for: task)

        XCTAssertEqual(options.map(\.id), ["retry:b1234567"])
        XCTAssertEqual(options.first?.kind, .retryBatchItem)
    }

    func testSnapshotStepsAreSortedAndBudgetStepsAreHidden() {
        let later = MordeTask.Step(id: "s2345678", sequence: 2, kind: "agent", status: "succeeded",
                                   title: "Second", summary: "Finished", created_at: "2026-11-04T00:00:02Z")
        let budget = MordeTask.Step(id: "s3456789", sequence: 3, kind: "budget", status: "succeeded",
                                    title: "Budget", summary: "Sensitive usage detail", created_at: "2026-11-04T00:00:03Z")
        let first = MordeTask.Step(id: "s1234567", sequence: 1, kind: "agent", status: "running",
                                   title: "First", summary: "Working", created_at: "2026-11-04T00:00:01Z")

        let visible = MIFTaskPresentation.visibleSteps([later, budget, first])

        XCTAssertEqual(visible.map(\.id), ["s1234567", "s2345678"])
        XCTAssertEqual(MIFTaskPresentation.stepStatusLabel("future_status"), AppLocalized("Status unknown"))
    }

    func testTimelineRowsSortDeduplicateAndSkipUndisplayableEvents() throws {
        let later = try makeEvent(sequence: 2, type: "task.status", payload: #"{"status":"running"}"#)
        let first = try makeEvent(sequence: 1, type: "task.status", payload: #"{"status":"queued"}"#)
        let duplicate = try makeEvent(sequence: 1, type: "task.status", payload: #"{"status":"completed"}"#)
        let budget = try makeEvent(sequence: 3, type: "task.step",
                                   payload: #"{"kind":"budget","title":"Private usage","summary":"Hidden"}"#)

        let rows = MIFTaskPresentation.timelineRows([later, duplicate, budget, first])

        XCTAssertEqual(rows.map(\.sequence), [1, 2])
        XCTAssertEqual(rows.map(\.id), ["event:1", "event:2"])
    }

    private func makeTask(id: String, status: String, batchItems: [MordeBatchItem]? = nil) -> MordeTask {
        MordeTask(owner_id: nil, initiator_id: nil, id: id, conversation_id: "c1234567", host_id: nil,
                  execution_location: "server", workspace_id: nil, input_revision: nil,
                  objective: "Synthetic task", status: status, event_sequence: 1, result: nil,
                  current_step: nil, waiting_reason: nil, stop_request: nil, pending_request: nil,
                  clarification_request: nil, batch_items: batchItems, steps: nil, artifacts: nil,
                  updated_at: "2026-11-04T00:00:00Z", assistant_message: nil)
    }

    private func makeEvent(sequence: Int64, type: String, payload: String) throws -> MordeTaskTimelineEvent {
        let json = """
        {"task_id":"q1234567","sequence":\(sequence),"type":"\(type)","created_at":"2026-11-04T00:00:00Z","payload":\(payload)}
        """
        return try JSONDecoder().decode(MordeTaskTimelineEvent.self, from: Data(json.utf8))
    }
}
