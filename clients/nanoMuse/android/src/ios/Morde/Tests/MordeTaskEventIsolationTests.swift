import XCTest
@testable import MordeTaskPolicies

final class MordeTaskEventIsolationTests: XCTestCase {
    func testEventsOlderThanConversationSnapshotAreNotPresented() {
        XCTAssertFalse(MordeTaskEventCursor.shouldPresent(sequence: 7, initialSequence: 8))
        XCTAssertTrue(MordeTaskEventCursor.shouldPresent(sequence: 8, initialSequence: 8))
        XCTAssertTrue(MordeTaskEventCursor.shouldPresent(sequence: 9, initialSequence: 8))
    }

    func testRevokedTaskStaysHiddenUntilANewerAuthorizedRead() {
        var fence = MordeTaskAccessFence()
        let stalePageToken = fence.token
        fence.deny("q1234567")
        fence.confirm(["q1234567"], requestToken: stalePageToken)
        XCTAssertEqual(fence.deniedTaskIDs, ["q1234567"])
        let currentToken = fence.token
        fence.confirm(["q2345678"], requestToken: currentToken)
        XCTAssertEqual(fence.deniedTaskIDs, ["q1234567"])
        fence.confirm(["q1234567"], requestToken: currentToken)
        XCTAssertTrue(fence.deniedTaskIDs.isEmpty)
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

    func testEventCursorIsNotCommittedUntilSnapshotRecoverySucceeds() throws {
        var cursor = MordeTaskEventCursor(taskID: "q1234567", afterSequence: 3)
        let candidate = try XCTUnwrap(cursor.advancing(eventID: "4", for: cursor.taskID))
        XCTAssertEqual(cursor.queryValue, "3", "A failed snapshot fetch must retry from the old cursor.")
        var snapshotCursor = candidate
        XCTAssertFalse(snapshotCursor.resume(eventID: "4", snapshotSequence: 3))
        XCTAssertEqual(cursor.queryValue, "3")
        XCTAssertTrue(snapshotCursor.resume(eventID: "4", snapshotSequence: 9))
        cursor = candidate
        XCTAssertEqual(cursor.queryValue, "4", "An ahead-of-stream snapshot must not skip available events on reconnect.")
        var expiredCursor = cursor
        XCTAssertTrue(expiredCursor.resume(eventID: "5", snapshotSequence: 9))
        cursor = expiredCursor
        XCTAssertEqual(cursor.queryValue, "9")
        XCTAssertNil(cursor.advancing(eventID: "10", for: "q2345678"))
    }

    func testKnownEventsRefreshSnapshotAndUnknownEventsCanAdvance() throws {
        for type in ["task.status", "task.step", "task.operation", "task.message", "task.batch_item",
                     "task.artifact", "task.waiting_user", "task.response", "task.result"] {
            XCTAssertTrue(MordeTaskEventSnapshotPolicy.requiresRefresh(type: type), type)
        }
        XCTAssertFalse(MordeTaskEventSnapshotPolicy.requiresRefresh(type: "task.future_safe_event"))
        let cursor = MordeTaskEventCursor(taskID: "q1234567", afterSequence: 3)
        let unknownEvent = try XCTUnwrap(cursor.advancing(eventID: "4", for: cursor.taskID))
        XCTAssertEqual(unknownEvent.queryValue, "4")
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

    func testStepRetryContractUsesSafePathRevisionAttemptAndReusableIdempotencyKey() throws {
        XCTAssertEqual(MordeTaskResponsePolicy.stepRetryPath(taskID: "q1234567", stepID: "s1234567"),
                       "/api/tasks/q1234567/steps/s1234567/retries")
        XCTAssertNil(MordeTaskResponsePolicy.stepRetryPath(taskID: "../q1234567", stepID: "s1234567"))
        XCTAssertNil(MordeTaskResponsePolicy.stepRetryPath(taskID: "q1234567", stepID: "../secret"))

        let body = try XCTUnwrap(MordeTaskResponsePolicy.stepRetryBody(taskRevision: 3, attempt: 2))
        XCTAssertEqual(Set(body.keys), ["expected_task_revision", "expected_attempt"])
        XCTAssertEqual(body["expected_task_revision"] as? Int64, 3)
        XCTAssertEqual(body["expected_attempt"] as? Int, 2)
        XCTAssertEqual(MordeTaskResponsePolicy.stepRetryBody(taskRevision: 0, attempt: 2)?["expected_task_revision"] as? Int64, 0)
        XCTAssertNil(MordeTaskResponsePolicy.stepRetryBody(taskRevision: -1, attempt: 2))
        XCTAssertNil(MordeTaskResponsePolicy.stepRetryBody(taskRevision: 3, attempt: 0))
        XCTAssertNil(MordeTaskResponsePolicy.stepRetryBody(taskRevision: nil, attempt: 2))
        XCTAssertTrue(MordeTaskResponsePolicy.validStepRetryReceipt(
            sourceStepID: "s1234567", sourceAttempt: 1, responseSourceStepID: "s1234567",
            attemptStepID: "s1234568", attempt: 2, status: "queued", replayed: false))
        XCTAssertTrue(MordeTaskResponsePolicy.validStepRetryReceipt(
            sourceStepID: "s1234567", sourceAttempt: 1, responseSourceStepID: "s1234567",
            attemptStepID: "s1234568", attempt: 2, status: "queued", replayed: true))
        XCTAssertFalse(MordeTaskResponsePolicy.validStepRetryReceipt(
            sourceStepID: "s1234567", sourceAttempt: 1, responseSourceStepID: "s1234566",
            attemptStepID: "s1234568", attempt: 2, status: "queued", replayed: false))
        XCTAssertFalse(MordeTaskResponsePolicy.validStepRetryReceipt(
            sourceStepID: "s1234567", sourceAttempt: 1, responseSourceStepID: "s1234567",
            attemptStepID: "../step", attempt: 2, status: "queued", replayed: false))
        XCTAssertFalse(MordeTaskResponsePolicy.validStepRetryReceipt(
            sourceStepID: "s1234567", sourceAttempt: 1, responseSourceStepID: "s1234567",
            attemptStepID: "s1234568", attempt: 1, status: "queued", replayed: false))
        XCTAssertFalse(MordeTaskResponsePolicy.validStepRetryReceipt(
            sourceStepID: "s1234567", sourceAttempt: 1, responseSourceStepID: "s1234567",
            attemptStepID: "s1234568", attempt: 2, status: "replayed", replayed: true))
        XCTAssertFalse(MordeTaskResponsePolicy.validStepRetryReceipt(
            sourceStepID: "s1234567", sourceAttempt: 1, responseSourceStepID: "s1234567",
            attemptStepID: "s1234568", attempt: 2, status: "queued", replayed: nil))

        let path = try XCTUnwrap(MordeTaskResponsePolicy.stepRetryPath(taskID: "q1234567", stepID: "s1234567"))
        let fingerprint = try XCTUnwrap(MordeActionRetryAttempt.fingerprint(taskID: "q1234567", endpoint: path, body: body))
        let first = MordeActionRetryAttempt.reusing(nil, for: fingerprint)
        let replay = MordeActionRetryAttempt.reusing(first, for: fingerprint)
        XCTAssertEqual(replay.idempotencyKey, first.idempotencyKey)
    }

    func testStepRetryErrorsAreIsolatedByTaskAndStep() {
        var errors = MordeTaskStepRetryErrorStore()
        errors.set("Retry step A failed", taskID: "q1234567", stepID: "s1234567")
        errors.set("Retry step B failed", taskID: "q1234567", stepID: "s1234568")
        errors.set("Same step ID in another task", taskID: "q1234568", stepID: "s1234567")

        XCTAssertEqual(errors.message(taskID: "q1234567", stepID: "s1234567"), "Retry step A failed")
        XCTAssertEqual(errors.message(taskID: "q1234567", stepID: "s1234568"), "Retry step B failed")
        XCTAssertEqual(errors.message(taskID: "q1234568", stepID: "s1234567"), "Same step ID in another task")

        errors.remove(taskID: "q1234567", stepID: "s1234567")
        XCTAssertNil(errors.message(taskID: "q1234567", stepID: "s1234567"))
        XCTAssertEqual(errors.message(taskID: "q1234567", stepID: "s1234568"), "Retry step B failed")
        XCTAssertEqual(errors.message(taskID: "q1234568", stepID: "s1234567"), "Same step ID in another task")

        errors.removeAll(taskID: "q1234567")
        XCTAssertNil(errors.message(taskID: "q1234567", stepID: "s1234568"))
        XCTAssertEqual(errors.message(taskID: "q1234568", stepID: "s1234567"), "Same step ID in another task")
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
