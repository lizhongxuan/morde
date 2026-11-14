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

    func testCurrentTaskPlanIsRevisionFenced() throws {
        let currentJSON = #"{"id":"q1234567","conversation_id":"c1234567","objective":"demo","status":"running","current_revision":2,"step_count":0,"completed_step_count":0,"updated_at":"2026-11-04T00:00:00Z","plan":{"task_revision":2,"revision":3,"goal":"Prepare report","cost_estimate_state":"unknown","steps":[{"id":"collect","title":"Collect sources","description":"","depends_on":[],"outputs":["notes"],"completion_criteria":["sources cited"],"status":"in_progress"}]}}"#
        let staleJSON = currentJSON.replacingOccurrences(of: "\"task_revision\":2", with: "\"task_revision\":1")
        let current = try JSONDecoder().decode(MordeTask.self, from: Data(currentJSON.utf8))
        let stale = try JSONDecoder().decode(MordeTask.self, from: Data(staleJSON.utf8))
        XCTAssertEqual(MIFTaskPresentation.currentPlan(current)?.revision, 3)
        XCTAssertNil(MIFTaskPresentation.currentPlan(stale))
    }

    func testTaskBillingSeparatesSettledEstimateUnknownAndBYOK() throws {
        let json = #"{"id":"q1234567","conversation_id":"c1234567","objective":"demo","status":"completed","step_count":0,"completed_step_count":0,"updated_at":"2026-11-04T00:00:00Z","budget":{"active_seconds":600,"tool_calls":10,"model_tokens":1000},"usage":{"active_seconds":30,"tool_calls":2,"model_tokens":120},"billing":{"model_calls":4,"costs":[{"state":"settled","currency":"USD","calls":1,"rate_versions":[2],"reserved_micros":1000000,"actual_micros":750000},{"state":"estimated","currency":"USD","calls":1,"rate_versions":[3],"reserved_micros":250000,"actual_micros":null},{"state":"unknown","calls":1,"rate_versions":[],"reserved_micros":0,"actual_micros":null},{"state":"byok","calls":1,"rate_versions":[],"reserved_micros":0,"actual_micros":null}]}}"#
        let task = try JSONDecoder().decode(MordeTask.self, from: Data(json.utf8))
        let billing = try XCTUnwrap(MIFTaskPresentation.currentBilling(task))
        XCTAssertEqual(billing.model_calls, 4)
        XCTAssertEqual(billing.costs.map(\.state), ["settled", "estimated", "unknown", "byok"])

        let mismatch = try JSONDecoder().decode(MordeTask.self, from: Data(json.replacingOccurrences(of: "\"model_calls\":4", with: "\"model_calls\":5").utf8))
        XCTAssertNil(MIFTaskPresentation.currentBilling(mismatch))
    }

    func testTaskFileFetchPresentationSeparatesOfflineWaitFromContentAndCancellation() throws {
        let offline = try makeFileFetch([
            "status": "waiting_device", "waiting_reason": "device_offline",
        ])
        XCTAssertTrue(offline.isValid(for: "q1234567"))
        XCTAssertEqual(offline.sourceDirectoryLabel, AppLocalized("Not provided"))
        XCTAssertTrue(offline.canCancel)
        XCTAssertFalse(offline.canOpen)
        XCTAssertFalse(offline.canDeleteCopy)

        let retained = try makeFileFetch([
            "source_directory_name": "Project Docs",
            "status": "completed", "waiting_reason": NSNull(),
            "source_version": String(repeating: "a", count: 64),
            "source_sha256": String(repeating: "b", count: 64),
            "media_type": "application/pdf", "size_bytes": 31, "confirmed_bytes": 31,
            "content_id": "o2345678", "expires_at": "2026-11-18T00:00:00Z",
            "content_state": "retained", "parse_status": "raw_only",
        ])
        XCTAssertTrue(retained.isValid(for: "q1234567"))
        XCTAssertEqual(retained.sourceDirectoryLabel, "Project Docs")
        XCTAssertFalse(retained.canCancel)
        XCTAssertTrue(retained.canOpen)
        XCTAssertTrue(retained.canDeleteCopy)

        let completedRace = try makeFileFetch([
            "status": "completed", "waiting_reason": NSNull(),
            "source_sha256": String(repeating: "c", count: 64), "media_type": "text/plain",
            "size_bytes": 4, "confirmed_bytes": 4, "content_id": "o3456789",
            "content_state": "retained", "parse_status": "raw_only",
        ])
        XCTAssertTrue(completedRace.isValid(for: "q1234567"))
        XCTAssertFalse(completedRace.canCancel)

        let failedRace = try makeFileFetch([
            "status": "failed", "waiting_reason": NSNull(),
            "parse_status": "pending", "failure_reason": "source_changed",
        ])
        XCTAssertTrue(failedRace.isValid(for: "q1234567"))
        XCTAssertFalse(failedRace.canCancel)
        XCTAssertEqual(failedRace.failure_reason, "source_changed")
        XCTAssertFalse(try makeFileFetch(["failure_reason": "raw /Users/private/secret.txt"]).isValid(for: "q1234567"))
        XCTAssertFalse(try makeFileFetch(["status": "transferring", "failure_reason": "source_changed"]).isValid(for: "q1234567"))

        let deleted = try makeFileFetch([
            "status": "completed", "waiting_reason": NSNull(),
            "source_sha256": String(repeating: "b", count: 64), "media_type": "text/plain",
            "size_bytes": 4, "confirmed_bytes": 4, "content_id": "o2345678",
            "content_state": "deleted", "deleted_at": "2026-11-11T00:00:00Z",
            "parse_status": "extracted",
        ])
        XCTAssertTrue(deleted.isValid(for: "q1234567"))
        XCTAssertFalse(deleted.canOpen)
        XCTAssertFalse(deleted.canDeleteCopy)
    }

    func testTaskFileFetchRejectsUnsafeDirectoryPathAndInvalidProgress() throws {
        let valid = try makeFileFetch()
        XCTAssertTrue(valid.isValid(for: "q1234567"))
        XCTAssertFalse(valid.isValid(for: "q7654321"))

        let absolutePath = try makeFileFetch(["relative_path": "/Users/private/secret.txt"])
        XCTAssertFalse(absolutePath.isValid(for: "q1234567"))
        let traversalPath = try makeFileFetch(["relative_path": "../secret.txt"])
        XCTAssertFalse(traversalPath.isValid(for: "q1234567"))
        let directoryPath = try makeFileFetch(["source_directory_name": "Project/Private"])
        XCTAssertFalse(directoryPath.isValid(for: "q1234567"))
        let overLimit = try makeFileFetch(["size_bytes": 16 * 1024 * 1024 + 1])
        XCTAssertFalse(overLimit.isValid(for: "q1234567"))
        let overConfirmed = try makeFileFetch(["confirmed_bytes": 5])
        XCTAssertFalse(overConfirmed.isValid(for: "q1234567"))
        let badWaitReason = try makeFileFetch(["waiting_reason": "device_offline"])
        XCTAssertFalse(badWaitReason.isValid(for: "q1234567"))
    }

    private func makeFileFetch(_ overrides: [String: Any] = [:]) throws -> MordeTaskFileFetch {
        var payload: [String: Any] = [
            "fetch_id": "o1234567", "task_id": "q1234567",
            "source_installation_id": "i1234567", "source_device_name": "Work Mac",
            "source_directory_name": NSNull(), "relative_path": "docs/report.pdf",
            "task_revision": 1, "source_version": NSNull(), "source_sha256": NSNull(),
            "media_type": NSNull(), "size_bytes": NSNull(), "confirmed_bytes": 0,
            "status": "queued", "waiting_reason": NSNull(), "failure_reason": NSNull(), "content_id": NSNull(),
            "expires_at": NSNull(), "content_state": "none", "parse_status": "pending",
            "parse_error": NSNull(), "deleted_at": NSNull(),
        ]
        payload.merge(overrides) { _, replacement in replacement }
        return try JSONDecoder().decode(MordeTaskFileFetch.self,
            from: JSONSerialization.data(withJSONObject: payload))
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

    func testTaskArtifactListContainsOnlyCompleteVerifiedWorkspaceProofs() {
        let confirmed = makeArtifact(id: "a1234567")
        let unverified = makeArtifact(id: "a2345678", verified: false)
        let incomplete = makeArtifact(id: "a3456789", sha256: nil)
        let invalidWorkspace = makeArtifact(id: "a4567890", workspaceID: "workspace")

        XCTAssertEqual(MIFTaskPresentation.confirmedArtifacts(
            [confirmed, unverified, incomplete, invalidWorkspace]).map(\.id), ["a1234567"])
        XCTAssertTrue(MIFTaskPresentation.confirmedArtifacts(nil).isEmpty)
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

    func testOnlyExplicitlyRetryableFailedStepsWithExpectedVersionShowRetry() throws {
        func step(_ retryable: Bool?, attempt: Any = 1, revision: Any = 3, status: String = "failed") throws -> MordeTask.Step {
            let value: [String: Any] = [
                "id": "s1234567", "sequence": 1, "kind": "tool", "status": status,
                "title": "Local tool step", "summary": "Failed safely", "created_at": "2026-11-04T00:00:01Z",
                "attempt": attempt, "task_revision": revision,
            ].merging(retryable.map { ["retryable": $0] } ?? [:]) { _, new in new }
            return try JSONDecoder().decode(MordeTask.Step.self,
                from: JSONSerialization.data(withJSONObject: value))
        }

        XCTAssertTrue(MIFTaskPresentation.canRetryStep(try step(true)))
        XCTAssertFalse(MIFTaskPresentation.canRetryStep(try step(false)))
        XCTAssertFalse(MIFTaskPresentation.canRetryStep(try step(nil)))
        XCTAssertFalse(MIFTaskPresentation.canRetryStep(try step(true, attempt: 0)))
        XCTAssertTrue(MIFTaskPresentation.canRetryStep(try step(true, revision: 0)))
        XCTAssertFalse(MIFTaskPresentation.canRetryStep(try step(true, revision: -1)))
        XCTAssertFalse(MIFTaskPresentation.canRetryStep(try step(true, status: "running")))
    }

    func testNonRetryableStepFailureReasonsExplainKnownStatesAndHideUnknownValues() throws {
        let knownReasons = [
            ("not_dispatched", "Step was not dispatched. Refresh the task and confirm its state."),
            ("tool_failed_after_dispatch", "Step failed after dispatch. Check for partial results before retrying."),
            ("side_effect_unknown", "This step may already have changed something. Check its result before retrying."),
            ("authorization_changed", "Authorization changed. Refresh the task and check authorization before retrying."),
            ("resource_changed", "The target resource changed. Refresh the task and check its state before retrying."),
        ]
        for (reason, explanation) in knownReasons {
            let payload: [String: Any] = [
                "id": "s1234567", "sequence": 1, "kind": "tool", "status": "failed",
                "title": "Local tool step", "summary": "Failed safely", "created_at": "2026-11-04T00:00:01Z",
                "retryable": false, "attempt": 1, "task_revision": 3, "failure_reason": reason,
            ]
            let step = try JSONDecoder().decode(MordeTask.Step.self,
                from: JSONSerialization.data(withJSONObject: payload))
            XCTAssertEqual(MIFTaskPresentation.stepFailureExplanation(step), AppLocalized(explanation), reason)
            XCTAssertFalse(MIFTaskPresentation.canRetryStep(step), reason)
        }

        let unknownPayload: [String: Any] = [
            "id": "s1234567", "sequence": 1, "kind": "tool", "status": "failed",
            "title": "Local tool step", "summary": "Failed safely", "created_at": "2026-11-04T00:00:01Z",
            "retryable": false, "attempt": 1, "task_revision": 3, "failure_reason": "future_private_code",
        ]
        let unknown = try JSONDecoder().decode(MordeTask.Step.self,
            from: JSONSerialization.data(withJSONObject: unknownPayload))
        XCTAssertNil(MIFTaskPresentation.stepFailureExplanation(unknown))

        var retryablePayload = unknownPayload
        retryablePayload["failure_reason"] = "side_effect_unknown"
        retryablePayload["retryable"] = true
        let retryable = try JSONDecoder().decode(MordeTask.Step.self,
            from: JSONSerialization.data(withJSONObject: retryablePayload))
        XCTAssertNil(MIFTaskPresentation.stepFailureExplanation(retryable))
    }

    func testChildJobLabelsKeepRunningFailureAndUnknownDistinct() {
        XCTAssertEqual(MIFTaskPresentation.stepStatusLabel("queued"), AppLocalized("Queued"))
        XCTAssertEqual(MIFTaskPresentation.stepStatusLabel("running"), AppLocalized("Running"))
        XCTAssertEqual(MIFTaskPresentation.stepStatusLabel("failed"), AppLocalized("Failed"))
        XCTAssertEqual(MIFTaskPresentation.stepStatusLabel("unknown"), AppLocalized("Status unknown"))
        XCTAssertEqual(MIFTaskPresentation.stepStatusLabel("succeeded"), AppLocalized("Completed"))
        XCTAssertEqual(MIFTaskPresentation.agentStatusLabel("idle"), AppLocalized("Idle"))
        XCTAssertEqual(MIFTaskPresentation.agentStatusLabel("future_status"), AppLocalized("Status unknown"))
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

    func testEventMarkersDoNotClaimUnknownStatusesCompleted() {
        XCTAssertEqual(MIFTaskPresentation.eventChainMarker(eventStatus: "future_status", taskStatus: "completed",
                                                             index: 0, rowCount: 2), .unconfirmed)
        XCTAssertEqual(MIFTaskPresentation.eventChainMarker(eventStatus: "cancelled", taskStatus: "completed",
                                                             index: 0, rowCount: 2), .unconfirmed)
        XCTAssertEqual(MIFTaskPresentation.eventChainMarker(eventStatus: "failed", taskStatus: "running",
                                                             index: 0, rowCount: 2), .failed)
        XCTAssertEqual(MIFTaskPresentation.eventChainMarker(eventStatus: "waiting_user", taskStatus: "running",
                                                             index: 1, rowCount: 2), .current)
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

    private func makeArtifact(id: String, verified: Bool = true, workspaceID: String? = "wabc1234",
                              workspaceRevision: Int64? = 3, path: String? = "reports/output.docx",
                              sizeBytes: Int64? = 23, sha256: String? = String(repeating: "a", count: 64)) -> MordeTask.Artifact {
        MordeTask.Artifact(id: id, label: "Output.docx", media_type: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
                           verified: verified, workspace_id: workspaceID, workspace_revision: workspaceRevision,
                           path: path, size_bytes: sizeBytes, sha256: sha256,
                           created_at: "2026-11-04T00:00:00Z")
    }

    private func makeEvent(sequence: Int64, type: String, payload: String) throws -> MordeTaskTimelineEvent {
        let json = """
        {"task_id":"q1234567","sequence":\(sequence),"type":"\(type)","created_at":"2026-11-04T00:00:00Z","payload":\(payload)}
        """
        return try JSONDecoder().decode(MordeTaskTimelineEvent.self, from: Data(json.utf8))
    }
}
