import XCTest
@testable import MordeTaskPolicies

final class MordeTaskEventIsolationTests: XCTestCase {
    func testEventsOlderThanConversationSnapshotAreNotPresented() {
        XCTAssertFalse(MordeTaskEventCursor.shouldPresent(sequence: 7, initialSequence: 8))
        XCTAssertTrue(MordeTaskEventCursor.shouldPresent(sequence: 8, initialSequence: 8))
        XCTAssertTrue(MordeTaskEventCursor.shouldPresent(sequence: 9, initialSequence: 8))
    }

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

    func testStepDetailRouteRejectsInvalidTaskAndStepIDs() {
        XCTAssertEqual(MordeTaskResponsePolicy.stepDetailPath(taskID: "q1234567", stepID: "s1234567"),
                       "/api/tasks/q1234567/steps/s1234567")
        XCTAssertNil(MordeTaskResponsePolicy.stepDetailPath(taskID: "../q1234567", stepID: "s1234567"))
        XCTAssertNil(MordeTaskResponsePolicy.stepDetailPath(taskID: "q1234567", stepID: "../secret"))
    }

    func testStepTimeLabelParsesFractionalAndWholeSecondTimestamps() {
        XCTAssertNotNil(MordeTaskResponsePolicy.stepTimeLabel("2026-11-13T04:44:29.123Z"))
        XCTAssertNotNil(MordeTaskResponsePolicy.stepTimeLabel("2026-11-13T04:44:29Z"))
        XCTAssertNil(MordeTaskResponsePolicy.stepTimeLabel("not-a-timestamp"))
    }

    func testStepDetailValidatesIdentitySequenceAndSizeButPreservesUnknownState() throws {
        let json = #"{"task_id":"q1234567","step_id":"s1234567","event_sequence":12,"tool_name":"read_file","detail_state":"future_state","object":null,"input":"path.txt","result":null,"truncated":false}"#
        let detail = try JSONDecoder().decode(MordeTaskStepDetail.self, from: Data(json.utf8))

        XCTAssertTrue(detail.isValid(taskID: "q1234567", stepID: "s1234567", atLeast: 11))
        XCTAssertFalse(detail.isValid(taskID: "q2345678", stepID: "s1234567", atLeast: 11))
        XCTAssertFalse(detail.isValid(taskID: "q1234567", stepID: "s1234567", atLeast: 13))

        let oversized = String(repeating: "x", count: 8193)
        let oversizedJSON = #"{"task_id":"q1234567","step_id":"s1234567","event_sequence":12,"tool_name":"read_file","detail_state":"available","object":null,"input":""# + oversized + #"","result":null,"truncated":false}"#
        let oversizedDetail = try JSONDecoder().decode(MordeTaskStepDetail.self, from: Data(oversizedJSON.utf8))
        XCTAssertFalse(oversizedDetail.isValid(taskID: "q1234567", stepID: "s1234567", atLeast: 12))
    }

    func testAgentActivityGroupsStepsBySessionAndDelegationOrder() throws {
        let json = #"[{"id":"main-session","parent_id":null,"step_id":null,"task_revision":3,"role":"main","status":"running","observed_at":null},{"id":"subagent-b","parent_id":"main-session","step_id":"s0000002","task_revision":3,"role":"subagent","status":"idle","observed_at":null},{"id":"subagent-a","parent_id":"main-session","step_id":"s0000001","task_revision":3,"role":"subagent","status":"new_status","observed_at":null},{"id":"nested-agent","parent_id":"subagent-a","step_id":"s0000003","task_revision":3,"role":"subagent","status":"running","observed_at":null}]"#
        let agents = try JSONDecoder().decode([MordeTaskAgentExecution].self, from: Data(json.utf8))
        let steps = [
            MordeTaskAgentActivityStep(id: "s0000004", sequence: 4, agentID: "unresolved-session"),
            MordeTaskAgentActivityStep(id: "s0000002", sequence: 2, agentID: "subagent-b"),
            MordeTaskAgentActivityStep(id: "s0000001", sequence: 1, agentID: "main-session"),
            MordeTaskAgentActivityStep(id: "s0000003", sequence: 3, agentID: "nested-agent"),
        ]

        let groups = MordeTaskAgentActivityPolicy.groups(agents: agents, steps: steps)

        XCTAssertEqual(groups.map(\.title), ["MAIN", "SUBAGENT 01", "SUBAGENT 01.01", "SUBAGENT 02", "Attribution unconfirmed"])
        XCTAssertEqual(groups.map(\.stepIDs), [["s0000001"], [], ["s0000003"], ["s0000002"], ["s0000004"]])
        XCTAssertEqual(groups.map(\.taskRevision), [3, 3, 3, 3, nil])
        XCTAssertEqual(groups[2].status, "running")
        XCTAssertEqual(agents[2].status, "new_status", "Unknown agent status values remain available for the UI fallback.")
    }

    func testInvalidAgentCycleLeavesStepsUnassigned() throws {
        let json = #"[{"id":"main-session","parent_id":null,"step_id":null,"task_revision":0,"role":"main","status":"running","observed_at":null},{"id":"cycle-a","parent_id":"cycle-b","step_id":null,"task_revision":0,"role":"subagent","status":"idle","observed_at":null},{"id":"cycle-b","parent_id":"cycle-a","step_id":null,"task_revision":0,"role":"subagent","status":"idle","observed_at":null}]"#
        let agents = try JSONDecoder().decode([MordeTaskAgentExecution].self, from: Data(json.utf8))
        let steps = [MordeTaskAgentActivityStep(id: "s1234567", sequence: 1, agentID: "main-session")]

        XCTAssertEqual(MordeTaskAgentActivityPolicy.groups(agents: agents, steps: steps), [
            MordeTaskAgentActivityGroup(id: "unassigned", title: "Attribution unconfirmed", status: nil,
                                        taskRevision: nil, depth: 0, stepIDs: ["s1234567"]),
        ])
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
