import XCTest
@testable import Minis

final class MIFTaskViewsTests: XCTestCase {
    func testTaskDecodesAggregationFlagsWithRequiredStepCounts() throws {
        let legacy = #"{"id":"q1234567","conversation_id":"c1234567","objective":"demo","status":"completed","step_count":0,"completed_step_count":0,"updated_at":"2026-11-04T00:00:00Z"}"#
        let pending = #"{"id":"q1234567","conversation_id":"c1234567","objective":"demo","status":"completed","step_count":0,"completed_step_count":0,"updated_at":"2026-11-04T00:00:00Z","result_message_pending":true,"result_message_published":false}"#
        let oldTask = try JSONDecoder().decode(MordeTask.self, from: Data(legacy.utf8))
        let newTask = try JSONDecoder().decode(MordeTask.self, from: Data(pending.utf8))
        XCTAssertNil(oldTask.result_message_pending)
        XCTAssertNil(oldTask.result_message_published)
        XCTAssertEqual(newTask.result_message_pending, true)
        XCTAssertEqual(newTask.result_message_published, false)
        let missingCounts = #"{"id":"q1234567","conversation_id":"c1234567","objective":"demo","status":"completed","updated_at":"2026-11-04T00:00:00Z"}"#
        XCTAssertThrowsError(try JSONDecoder().decode(MordeTask.self, from: Data(missingCounts.utf8)))
    }

    func testNewRevisionWaitsForItsOwnPersistedStart() throws {
        func task(started: Bool?, current: Int64, desired: Int64, status: String = "running") throws -> MordeTask {
            var payload: [String: Any] = ["id": "q1234567", "conversation_id": "c1234567", "objective": "demo",
                "status": status, "step_count": 0, "completed_step_count": 0,
                "updated_at": "2026-11-04T00:00:00Z", "current_revision": current,
                "desired_revision": desired, "current_step": "开始处理"]
            if let started { payload["current_revision_started"] = started }
            return try JSONDecoder().decode(MordeTask.self, from: JSONSerialization.data(withJSONObject: payload))
        }
        let pending = try task(started: false, current: 1, desired: 1)
        XCTAssertTrue(pending.isWaitingForRevisionStart)
        XCTAssertEqual(pending.displayedCurrentStep, AppLocalized("Waiting to start"))
        XCTAssertEqual(pending.statusLabel, AppLocalized("Waiting to start"))
        XCTAssertTrue(try task(started: true, current: 1, desired: 2).isWaitingForRevisionStart)
        XCTAssertFalse(try task(started: true, current: 2, desired: 2).isWaitingForRevisionStart)
        XCTAssertFalse(try task(started: nil, current: 1, desired: 2).isWaitingForRevisionStart)
        XCTAssertFalse(try task(started: false, current: 1, desired: 1, status: "failed").isWaitingForRevisionStart)
        let oldStep = MordeTask.Step(id: "s1234567", sequence: 1, kind: "progress", status: "succeeded",
            title: "开始处理", summary: "正在处理你的请求。", created_at: "2026-11-04T00:00:01Z")
        XCTAssertTrue(MIFTaskPresentation.visibleSteps([oldStep], waitingForRevisionStart: true).isEmpty)
        XCTAssertEqual(MIFTaskPresentation.visibleSteps([oldStep], waitingForRevisionStart: false).count, 1)
    }

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

    func testEmptyPageWithNextCursorIsNotFinalEmptyState() {
        XCTAssertFalse(MIFTaskPageRules.isFinalEmptyPage(itemCount: 0, didLoadInitial: true,
                                                         nextCursor: "next"))
        XCTAssertTrue(MIFTaskPageRules.isFinalEmptyPage(itemCount: 0, didLoadInitial: true,
                                                        nextCursor: nil))
        XCTAssertFalse(MIFTaskPageRules.isFinalEmptyPage(itemCount: 0, didLoadInitial: false,
                                                         nextCursor: nil))
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

    func testConversationHeaderMoodReflectsTheCurrentTaskState() {
        XCTAssertEqual(MIFTaskPresentation.headerMood(for: makeTask(id: "q1234567", status: "running")), .working)
        XCTAssertEqual(MIFTaskPresentation.headerMood(for: makeTask(id: "q1234567", status: "waiting_user")), .waiting)
        XCTAssertEqual(MIFTaskPresentation.headerMood(for: makeTask(id: "q1234567", status: "completed")), .happy)
        XCTAssertEqual(MIFTaskPresentation.headerMood(for: makeTask(id: "q1234567", status: "failed")), .error)
        XCTAssertEqual(MIFTaskPresentation.headerMood(for: nil), .idle)
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

    func testToolStepsUseReadableNamesAndKeepTheirRealStatuses() throws {
        let tool = MordeTask.Step(id: "s1234567", sequence: 1, kind: "tool", status: "failed",
            title: "Run cloud dsh tool: morde_web_search",
            summary: "Checking current authorization, budget, and Host control.", created_at: "2026-11-04T00:00:01Z")
        XCTAssertEqual(MIFTaskPresentation.stepDescription(tool), "\(AppLocalized("Search the web")) · \(AppLocalized("Failed"))")
        XCTAssertEqual(tool.status, "failed")
        let event = try makeEvent(sequence: 1, type: "task.step",
            payload: #"{"kind":"tool","title":"Run cloud dsh tool: morde_web_search","status":"failed","summary":"Checking current authorization, budget, and Host control."}"#)
        XCTAssertEqual(event.display?.title, AppLocalized("Search the web"))
        XCTAssertEqual(event.display?.summary, AppLocalized("Failed"))
        XCTAssertEqual(event.status, "failed")
    }

    func testStartedProgressIsVisibleWithoutClaimingTheTaskFinished() throws {
        let step = MordeTask.Step(id: "s1234567", sequence: 1, kind: "progress", status: "succeeded",
            title: "开始处理", summary: "正在处理你的请求。", created_at: "2026-11-04T00:00:01Z")
        XCTAssertEqual(MIFTaskPresentation.visibleSteps([step]).map(\.id), [step.id])
        XCTAssertEqual(MIFTaskPresentation.stepStatusLabel(step.status, kind: step.kind, title: step.title),
                       AppLocalized("Task started"))
        XCTAssertEqual(step.status, "succeeded")
        let event = try makeEvent(sequence: 1, type: "task.step",
            payload: #"{"kind":"progress","title":"开始处理","status":"succeeded","summary":"正在处理你的请求。"}"#)
        XCTAssertEqual(event.display?.title, AppLocalized("Starting the task"))
        XCTAssertEqual(event.display?.summary, "\(AppLocalized("Task started")) · \(AppLocalized("Processing your request."))")
        XCTAssertEqual(event.status, "succeeded")
    }

    func testProjectRecoveryWaitExplainsTheFailureInTaskAndEvent() throws {
        let json = #"{"id":"q1234567","conversation_id":"c1234567","objective":"demo","status":"waiting_user","waiting_reason":"project_recovery_unavailable","step_count":0,"completed_step_count":0,"updated_at":"2026-11-04T00:00:00Z"}"#
        let task = try JSONDecoder().decode(MordeTask.self, from: Data(json.utf8))
        let explanation = "原项目权限/上下文变化，未安全恢复，请重新发送。"
        XCTAssertEqual(task.statusLabel, explanation)
        XCTAssertEqual(MIFTaskPresentation.headerMood(for: task), .waiting)

        let event = try makeEvent(sequence: 1, type: "task.waiting_user",
            payload: #"{"status":"waiting_user","reason":"project_recovery_unavailable"}"#)
        XCTAssertEqual(event.display?.summary, explanation)
        XCTAssertEqual(event.status, "waiting_user")
        XCTAssertEqual(MIFTaskPresentation.eventChainMarker(eventStatus: event.status, taskStatus: task.status,
                                                             index: 0, rowCount: 1), .current)

        let statusEvent = try makeEvent(sequence: 2, type: "task.status",
            payload: #"{"status":"waiting_user","reason":"project_recovery_unavailable"}"#)
        XCTAssertEqual(statusEvent.display?.summary, explanation)
    }

    func testTaskListDetailUsesServerStepCountsAndKeepsCurrentDetail() {
        let running = MordeTask.Step(id: "s2345678", sequence: 2, kind: "agent", status: "running",
                                    title: "Second", summary: "Deploying", created_at: "2026-11-04T00:00:02Z")
        let budget = MordeTask.Step(id: "s3456789", sequence: 3, kind: "budget", status: "succeeded",
                                   title: "Private budget", summary: "Private usage detail",
                                   created_at: "2026-11-04T00:00:03Z")
        let completed = MordeTask.Step(id: "s1234567", sequence: 1, kind: "agent", status: "succeeded",
                                       title: "First", summary: "Finished", created_at: "2026-11-04T00:00:01Z")
        let task = makeTask(id: "q1234567", status: "running", currentStep: "Deploying", stepCount: 2,
                            completedStepCount: 1,
                            steps: [running, budget, completed])

        let detail = MIFTaskPresentation.taskListDetail(task)

        XCTAssertEqual(detail, String(format: AppLocalized("%1$d steps · %2$d done"), 2, 1) + " · Deploying")
        XCTAssertFalse(detail?.localizedCaseInsensitiveContains("budget") == true)
    }

    func testTaskListDetailDoesNotInferCountsFromSteps() {
        let currentStep = makeTask(id: "q1234567", status: "running", currentStep: "Checking")
        let result = makeTask(id: "q2345678", status: "completed", result: "Delivered")
        let completedWithStaleStep = makeTask(id: "q4567890", status: "completed",
                                              currentStep: "Old step", result: "Delivered")
        let budget = MordeTask.Step(id: "s3456789", sequence: 1, kind: "budget", status: "succeeded",
                                    title: "Private budget", summary: "Private usage detail",
                                    created_at: "2026-11-04T00:00:01Z")
        let hiddenBudget = makeTask(id: "q3456789", status: "completed", stepCount: 0,
                                    completedStepCount: 0, steps: [budget])
        let serverReported = makeTask(id: "q5678901", status: "running", stepCount: 7,
                                      completedStepCount: 3, steps: [])

        XCTAssertEqual(MIFTaskPresentation.taskListDetail(currentStep), "Checking")
        XCTAssertEqual(MIFTaskPresentation.taskListDetail(result), "Delivered")
        XCTAssertEqual(MIFTaskPresentation.taskListDetail(completedWithStaleStep), "Delivered")
        XCTAssertNil(MIFTaskPresentation.taskListDetail(hiddenBudget))
        XCTAssertEqual(MIFTaskPresentation.taskListDetail(serverReported),
                       String(format: AppLocalized("%1$d steps · %2$d done"), 7, 3))
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

    func testEmptyTimelineDistinguishesLoadingKnownEmptyAndUnconfirmed() {
        XCTAssertEqual(MIFTaskPresentation.timelineEmptyState(isLoading: true, noHistory: false), .loading)
        XCTAssertEqual(MIFTaskPresentation.timelineEmptyState(isLoading: false, noHistory: true), .noHistory)
        XCTAssertEqual(MIFTaskPresentation.timelineEmptyState(isLoading: false, noHistory: false), .unconfirmed)
    }

    func testTimelineMarkersReflectOnlyTheActualLatestTaskState() {
        XCTAssertEqual(MIFTaskPresentation.timelineMarker(taskStatus: "running", index: 0, rowCount: 2), .completed)
        XCTAssertEqual(MIFTaskPresentation.timelineMarker(taskStatus: "running", index: 1, rowCount: 2), .current)
        XCTAssertEqual(MIFTaskPresentation.timelineMarker(taskStatus: "failed", index: 0, rowCount: 1), .failed)
        XCTAssertEqual(MIFTaskPresentation.timelineMarker(taskStatus: "completed", index: 0, rowCount: 1), .completed)
        XCTAssertEqual(MIFTaskPresentation.timelineMarker(taskStatus: "future_status", index: 0, rowCount: 1), .unconfirmed)
    }

    private func makeTask(id: String, status: String, batchItems: [MordeBatchItem]? = nil,
                          currentStep: String? = nil, result: String? = nil,
                          stepCount: Int64 = 0, completedStepCount: Int64 = 0,
                          steps: [MordeTask.Step]? = nil) -> MordeTask {
        // nanoMuse: model_id has no memberwise default, so fixture tasks provide nil explicitly.
        MordeTask(owner_id: nil, initiator_id: nil, id: id, conversation_id: "c1234567", host_id: nil,
                  execution_location: "server", model_id: nil, workspace_id: nil, input_revision: nil,
                  objective: "Synthetic task", status: status, event_sequence: 1,
                  current_revision: nil, desired_revision: nil, current_revision_started: nil,
                  result: result, result_message_pending: nil,
                  result_message_published: nil,
                  current_step: currentStep, waiting_reason: nil, stop_request: nil,
                  step_count: stepCount, completed_step_count: completedStepCount, pending_request: nil,
                  clarification_request: nil, batch_items: batchItems, steps: steps, artifacts: nil,
                  updated_at: "2026-11-04T00:00:00Z", assistant_message: nil)
    }

    private func makeEvent(sequence: Int64, type: String, payload: String) throws -> MordeTaskTimelineEvent {
        let json = """
        {"task_id":"q1234567","sequence":\(sequence),"type":"\(type)","created_at":"2026-11-04T00:00:00Z","payload":\(payload)}
        """
        return try JSONDecoder().decode(MordeTaskTimelineEvent.self, from: Data(json.utf8))
    }
}
