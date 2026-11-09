import XCTest
@testable import MordeTaskPolicies

final class MordeTaskEventIsolationTests: XCTestCase {
    func testSameConversationTaskCursorsResumeIndependently() async {
        let first = MordeTaskEventCursor(taskID: "q1234567", afterSequence: 3)
        let second = MordeTaskEventCursor(taskID: "q2345678", afterSequence: 11)

        async let firstAdvanced = replay(first, eventIDs: ["4", "5"])
        async let secondAdvanced = replay(second, eventIDs: ["12", "14"])
        var firstResult = await firstAdvanced
        let secondResult = await secondAdvanced

        XCTAssertEqual(firstResult.queryValue, "5")
        XCTAssertEqual(secondResult.queryValue, "14")
        XCTAssertTrue(firstResult.resume(eventID: "6", snapshotSequence: 9))
        XCTAssertEqual(firstResult.queryValue, "9")
        XCTAssertEqual(secondResult.queryValue, "14", "Recovering one task snapshot must not move the other task cursor.")
        XCTAssertFalse(firstResult.advance(eventID: "15", for: second.taskID))
        XCTAssertEqual(firstResult.queryValue, "9")
    }

    func testStoppingSelectedTaskTargetsOnlyThatTaskInSameConversation() throws {
        let taskIDs: Set<String> = ["q1234567", "q2345678"]
        let firstTarget = MordeConversationTaskStopPolicy.targetTaskID(
            taskID: "q1234567", taskConversationID: "c1234567", currentConversationID: "c1234567",
            trackedTaskIDs: taskIDs, status: "running")

        XCTAssertEqual(firstTarget, "q1234567")
        let firstTaskID = try XCTUnwrap(firstTarget)
        XCTAssertEqual(MordeTaskResponsePolicy.actionPath(taskID: firstTaskID), "/api/tasks/q1234567/actions")
        XCTAssertEqual(MordeConversationTaskStopPolicy.targetTaskID(
            taskID: "q2345678", taskConversationID: "c1234567", currentConversationID: "c1234567",
            trackedTaskIDs: taskIDs, status: "running"), "q2345678",
            "A stop request for the first task must leave its sibling independently addressable.")
        XCTAssertNil(MordeConversationTaskStopPolicy.targetTaskID(
            taskID: "q1234567", taskConversationID: "c1234567", currentConversationID: "c8765432",
            trackedTaskIDs: taskIDs, status: "running"))
    }

    private func replay(_ initial: MordeTaskEventCursor, eventIDs: [String]) async -> MordeTaskEventCursor {
        var cursor = initial
        for eventID in eventIDs {
            await Task.yield()
            XCTAssertTrue(cursor.advance(eventID: eventID, for: initial.taskID))
        }
        return cursor
    }
}
