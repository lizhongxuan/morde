import XCTest

@MainActor
final class MordeRegistrationUITests: XCTestCase {
    func testApprovedDrawerDestinationsAreAvailableInMorde() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2),
           !app.textFields["morde.auth.username"].exists {
            signOutFromSettings(app)
        }
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("placeholder-pages-user", in: app)

        app.buttons["morde.shell.drawer"].tap()
        XCTAssertTrue(app.buttons["morde.shell.drawer.friends"].exists)
        XCTAssertTrue(app.buttons["morde.shell.toolChats"].exists)
        XCTAssertFalse(app.buttons["Devices"].exists)
        XCTAssertFalse(app.buttons["System files"].exists)
        app.terminate()
    }

    func testMIFPagesPassSystemAccessibilityAudit() throws {
        guard #available(iOS 17.0, *) else { throw XCTSkip("System accessibility audits require iOS 17.") }
        let app = try launchTaskActionFixture(extraArguments: ["-morde.localWorkItemFixture", "-morde.localResourceAcceptanceFixture",
                                                           "-morde.voiceOverUITest",
                                                           "-fontSettings.appBaseScale", "0"])
        try auditAccessibility(app, page: "chat")

        try relaunchTaskActionFixture(app)
        app.buttons["morde.shell.tab.tasks"].tap()
        let firstTask = app.buttons["morde.tasks.row.q0000001"]
        XCTAssertTrue(firstTask.waitForExistence(timeout: 15))
        try auditAccessibility(app, page: "task list")

        try relaunchTaskActionFixture(app)
        app.buttons["morde.shell.tab.tasks"].tap()
        app.buttons["morde.tasks.row.q0000001"].tap()
        XCTAssertTrue(app.staticTexts["morde.task.objective"].waitForExistence(timeout: 10))
        try auditAccessibility(app, page: "task detail")

        try relaunchTaskActionFixture(app)
        app.buttons["morde.shell.drawer"].tap()
        app.buttons["morde.shell.drawer.workItems"].tap()
        XCTAssertTrue(app.buttons["morde.workItems.projectPicker"].waitForExistence(timeout: 10))
        try auditAccessibility(app, page: "work items")

        try relaunchTaskActionFixture(app)
        let approvals = app.buttons["morde.shell.tab.approvals"]
        approvals.tap()
        XCTAssertTrue(app.staticTexts["mif.approval.count"].waitForExistence(timeout: 15))
        try auditAccessibility(app, page: "approvals")

        try relaunchTaskActionFixture(app)
        let resources = app.buttons["morde.shell.tab.resources"]
        resources.tap()
        XCTAssertTrue(app.buttons["morde.resources.add"].waitForExistence(timeout: 10))
        try auditAccessibility(app, page: "resources")

        try relaunchTaskActionFixture(app)
        app.buttons["morde.shell.drawer"].tap()
        app.buttons["morde.shell.drawer.library"].tap()
        XCTAssertTrue(app.descendants(matching: .any)
            .matching(identifier: "morde.library.sourcePicker").firstMatch.waitForExistence(timeout: 10))
        try auditAccessibility(app, page: "library")

        try relaunchTaskActionFixture(app)
        app.buttons["morde.shell.tab.chat"].tap()
        app.buttons["morde.shell.drawer"].tap()
        XCTAssertFalse(app.buttons["morde.shell.localHistory"].exists)
        XCTAssertTrue(app.buttons["morde.shell.drawer.friends"].exists)
        XCTAssertTrue(app.buttons["morde.shell.toolChats"].exists)
        app.terminate()
    }

    func testVerifiedTaskArtifactOpensLocalPreview() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskPagesFixture",
                              "-morde.localTaskArtifactPreviewFixture", "-AppleLanguages", "(en)",
                              "-AppleLocale", "en_US"]
        app.launch()
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2) {
            let chat = app.buttons["morde.shell.tab.chat"]
            if chat.exists && !chat.isSelected { chat.tap() }
            if !app.textFields["morde.auth.username"].waitForExistence(timeout: 3) {
                signOutFromSettings(app)
            }
        }
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("library-preview-user", in: app)
        app.terminate()
        app.launch()

        let tasks = app.buttons["morde.shell.tab.tasks"]
        XCTAssertTrue(tasks.waitForExistence(timeout: 10))
        tasks.tap()
        let task = app.buttons["morde.tasks.row.q0000001"]
        XCTAssertTrue(task.waitForExistence(timeout: 15))
        task.tap()
        XCTAssertTrue(app.descendants(matching: .any)
            .matching(identifier: "morde.task.artifact.a7654321").firstMatch.waitForExistence(timeout: 15))
        app.buttons["Back"].tap()

        let resources = app.buttons["morde.shell.tab.resources"]
        XCTAssertTrue(resources.waitForExistence(timeout: 10))
        resources.tap()
        app.buttons["morde.shell.drawer"].tap()
        app.buttons["morde.shell.drawer.library"].tap()
        let aiOutputs = app.buttons["morde.library.source.aiOutputs"]
        XCTAssertTrue(aiOutputs.waitForExistence(timeout: 10))
        aiOutputs.tap()

        let artifact = app.buttons["morde.library.task-artifact.preview.a7654321"]
        XCTAssertTrue(artifact.waitForExistence(timeout: 15))
        let metadataArtifact = app.descendants(matching: .any)
            .matching(identifier: "morde.library.task-artifact.a7654322").firstMatch
        XCTAssertTrue(metadataArtifact.exists)
        XCTAssertFalse(app.buttons["morde.library.task-artifact.preview.a7654322"].exists,
                       "Legacy artifacts without a complete proof stay metadata-only.")
        artifact.tap()
        XCTAssertTrue(app.descendants(matching: .any)
            .matching(NSPredicate(format: "label CONTAINS %@", "Local verified Task artifact preview"))
            .firstMatch.waitForExistence(timeout: 15))
        app.terminate()
    }

    func testResourcesShowLocalHostStatesAndControlActionFailures() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localResourceAcceptanceFixture",
                              "-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2) {
            signOutFromSettings(app)
        }
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("resource-acceptance-user", in: app)

        let resources = app.buttons["morde.shell.tab.resources"]
        XCTAssertTrue(resources.waitForExistence(timeout: 10))
        resources.tap()

        let running = app.buttons["morde.resources.host.open.h0000001"]
        XCTAssertTrue(running.waitForExistence(timeout: 15), "A running host with a ready binding can open.")
        let offlineOpen = app.buttons["morde.resources.host.open.h0000002"]
        XCTAssertTrue(offlineOpen.waitForExistence(timeout: 10))
        XCTAssertFalse(offlineOpen.isEnabled, "An offline host's open action is disabled.")

        let unreadyHost = app.buttons["morde.resources.host.open.h0000003"]
        for _ in 0..<10 where !unreadyHost.exists { app.swipeUp() }
        XCTAssertTrue(unreadyHost.exists, "A syncing binding host remains visible in the resource list.")
        XCTAssertFalse(unreadyHost.isEnabled)
        XCTAssertTrue(unreadyHost.label.contains("A ready Workspace binding is required to open this computer."),
                      "The disabled host row exposes its Workspace requirement to accessibility.")

        let unknown = app.staticTexts["Control status unknown"]
        for _ in 0..<10 where !unknown.exists { app.swipeUp() }
        XCTAssertTrue(unknown.exists)
        XCTAssertFalse(app.buttons["morde.resources.host.takeOver.h0000004"].exists)

        let expiredHost = app.buttons["morde.resources.host.open.h0000005"]
        for _ in 0..<10 where !expiredHost.isHittable { app.swipeUp() }
        XCTAssertTrue(expiredHost.label.contains("Control lease expired"),
                      "An expired lease must not be presented as active user control.")

        let shared = app.buttons["morde.resources.host.open.h0000008"]
        for _ in 0..<10 where !shared.exists { app.swipeUp() }
        XCTAssertTrue(shared.exists, "A shared Host with desktop:view is viewable.")
        XCTAssertFalse(app.buttons["morde.resources.host.takeOver.h0000008"].exists,
                       "Shared Hosts never expose control actions.")

        // nanoMuse: Resource rows may only release active user control; takeover lives in the host control screen.
        XCTAssertFalse(app.buttons["morde.resources.host.takeOver.h0000006"].exists)

        let returnToAI = app.buttons["morde.resources.host.returnToAI.h0000007"]
        let resourceScroll = app.scrollViews["morde.resources.scroll"]
        XCTAssertTrue(returnToAI.waitForExistence(timeout: 5))
        // XCTest may report a just-below-screen LazyVStack row as hittable. Move the
        // resource ScrollView once so the return action is physically on screen.
        resourceScroll.swipeUp()
        XCTAssertTrue(returnToAI.isHittable)
        returnToAI.tap()
        XCTAssertTrue(app.staticTexts["morde.resources.host.controlError.h0000007"].waitForExistence(timeout: 10),
                      "The failed return is surfaced while the server status is reloaded.")
        app.terminate()
    }

    func testLibraryShowsEmptyStateWhenNoTaskArtifactsAreDelivered() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskPagesFixture",
                              "-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2) {
            signOutFromSettings(app)
        }
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("library-empty-user", in: app)

        let resources = app.buttons["morde.shell.tab.resources"]
        XCTAssertTrue(resources.waitForExistence(timeout: 10))
        resources.tap()
        app.buttons["morde.shell.drawer"].tap()
        app.buttons["morde.shell.drawer.library"].tap()
        let aiOutputs = app.buttons["morde.library.source.aiOutputs"]
        XCTAssertTrue(aiOutputs.waitForExistence(timeout: 10))
        aiOutputs.tap()
        XCTAssertTrue(app.staticTexts["No delivered artifacts"].waitForExistence(timeout: 10))
        app.terminate()
    }

    func testLocalTaskPagesAndApprovalInbox() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskPagesFixture",
                              "-morde.localTaskPagesApprovalPaginationFixture",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        try signInToFixtureAccountIfNeeded("task-pages-user", in: app)

        let tasksTab = app.buttons["morde.shell.tab.tasks"]
        XCTAssertTrue(tasksTab.waitForExistence(timeout: 10))
        tasksTab.tap()

        let firstTask = app.buttons["morde.tasks.row.q0000001"]
        XCTAssertTrue(firstTask.waitForExistence(timeout: 10))
        firstTask.tap()

        XCTAssertTrue(app.staticTexts["morde.task.objective"].waitForExistence(timeout: 10))
        let firstStep = app.descendants(matching: .any)
            .matching(identifier: "morde.task.step.s0000001").firstMatch
        let firstEvent = app.descendants(matching: .any)
            .matching(identifier: "morde.task.event.1").firstMatch
        for _ in 0..<8 where !firstEvent.isHittable { app.swipeUp() }
        XCTAssertTrue(firstStep.waitForExistence(timeout: 10))
        XCTAssertTrue(firstEvent.waitForExistence(timeout: 10))
        XCTAssertFalse(app.descendants(matching: .any)
            .matching(identifier: "morde.task.step.s0000002").firstMatch.exists)
        XCTAssertFalse(app.descendants(matching: .any)
            .matching(identifier: "morde.task.event.2").firstMatch.exists)
        XCTAssertFalse(app.staticTexts["Private budget field"].exists)

        app.buttons["返回"].tap()
        let secondPageTask = app.buttons["morde.tasks.row.q0000103"]
        let approvalsTab = app.buttons["morde.shell.tab.approvals"]
        for _ in 0..<25 where !secondPageTask.exists { app.swipeUp() }
        XCTAssertTrue(secondPageTask.waitForExistence(timeout: 15), "Scrolling to the list end should load the next task page.")
        XCTAssertTrue(tasksTab.isSelected)
        for _ in 0..<10 where !secondPageTask.isHittable { app.swipeUp() }
        XCTAssertTrue(secondPageTask.waitForExistence(timeout: 10))

        XCTAssertTrue(approvalsTab.waitForExistence(timeout: 10))
        approvalsTab.tap()

        let approvalCount = app.staticTexts["mif.approval.count"]
        XCTAssertTrue(approvalCount.waitForExistence(timeout: 15))
        XCTAssertTrue(approvalCount.label.contains("1"))
        XCTAssertFalse(app.buttons["mif.approval.approve.q0000102:r0000002:1"].exists)
        XCTAssertFalse(app.descendants(matching: .any).matching(identifier: "mif.approval.partial").firstMatch.exists)

        let reject = app.buttons["mif.approval.reject.q0000101:r0000001:1"]
        XCTAssertTrue(reject.waitForExistence(timeout: 10))
        reject.tap()
        let emptyInbox = app.staticTexts["mif.approval.empty"]
        XCTAssertTrue(emptyInbox.waitForExistence(timeout: 15))
        XCTAssertTrue(approvalCount.label.contains("0"))
        app.terminate()
    }

    func testTaskListShowsConfirmedEmptyState() throws {
        let app = try launchTaskActionFixture(extraArguments: ["-morde.localTaskPagesEmptyFixture"])
        app.buttons["morde.shell.tab.tasks"].tap()
        XCTAssertTrue(app.descendants(matching: .any)
            .matching(identifier: "morde.tasks.empty").firstMatch.waitForExistence(timeout: 10))
        app.terminate()
    }

    func testTaskListCanRetryAfterLoadFailure() throws {
        let app = try launchTaskActionFixture(extraArguments: ["-morde.localTaskPagesRetryFixture"])
        app.buttons["morde.shell.tab.tasks"].tap()
        let error = app.descendants(matching: .any)
            .matching(identifier: "morde.tasks.loadError").firstMatch
        XCTAssertTrue(error.waitForExistence(timeout: 10))
        XCTAssertFalse(app.buttons["morde.tasks.loadError.retry"].exists,
                       "Task list errors use pull-to-refresh, not an extra retry button.")
        let taskList = app.scrollViews["morde.tasks.list"]
        XCTAssertTrue(taskList.waitForExistence(timeout: 5))
        // nanoMuse: Start at the scroll view's top edge so the short error state receives a pull gesture.
        let pullStart = taskList.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.05))
        let pullEnd = taskList.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.65))
        pullStart.press(forDuration: 0.05, thenDragTo: pullEnd)
        XCTAssertTrue(app.descendants(matching: .any)
            .matching(identifier: "morde.tasks.empty").firstMatch.waitForExistence(timeout: 15))
        app.terminate()
    }

    func testTaskListCanLoadMoreAfterAnEmptyIntermediatePage() throws {
        let app = try launchTaskActionFixture(extraArguments: ["-morde.localTaskPagesEmptyIntermediateFixture"])
        openTasksRootTab(app)

        let nextPageTask = app.buttons["morde.tasks.row.q0000001"]
        XCTAssertFalse(app.descendants(matching: .any)
            .matching(identifier: "morde.tasks.empty").firstMatch.exists)
        XCTAssertFalse(app.buttons["morde.tasks.loadMore"].exists,
                       "An empty cursor page continues automatically without a visible button.")
        for _ in 0..<10 where !nextPageTask.exists { app.swipeUp() }
        XCTAssertTrue(nextPageTask.waitForExistence(timeout: 15),
                      "Scrolling through an empty cursor page should continue to the next page.")
        app.terminate()
    }

    func testTaskTimelineReconnectsRecoversSnapshotAndDeduplicatesEvents() throws {
        let app = try launchTaskActionFixture(extraArguments: ["-morde.localTaskPagesTimelineRecoveryFixture"])
        openFirstTaskPage(in: app, taskID: "q0000101")

        let recovered = app.descendants(matching: .any)
            .matching(identifier: "morde.task.event.9").firstMatch
        XCTAssertTrue(recovered.waitForExistence(timeout: 20))
        for _ in 0..<12 where !recovered.isHittable { app.swipeUp() }
        XCTAssertEqual(app.staticTexts.matching(NSPredicate(format: "label == %@", "Recovered event")).count, 1)
        XCTAssertTrue(app.descendants(matching: .any)
            .matching(identifier: "morde.task.activity.expired").firstMatch.exists)
        app.terminate()
    }

    func testTaskTimelinePermissionRevocationClearsStaleDetails() throws {
        let app = try launchTaskActionFixture(extraArguments: ["-morde.localTaskPagesTimelinePermissionFixture"])
        openFirstTaskPage(in: app, taskID: "q0000101", waitForObjective: false)

        let error = app.descendants(matching: .any)
            .matching(identifier: "morde.task.loadError").firstMatch
        XCTAssertTrue(error.waitForExistence(timeout: 15))
        XCTAssertFalse(app.staticTexts["Local task timeline fixture"].exists,
                       "A revoked task must not leave its cached objective visible.")
        XCTAssertFalse(error.buttons["morde.task.loadError.retry"].exists,
                       "Task detail permission errors do not add a retry button; reopen the detail to reload.")
        app.terminate()
    }

    func testApprovalChainLinkOpensAtTaskActivityAfterNoHistoryResponse() throws {
        let app = try launchTaskActionFixture(extraArguments: ["-morde.localTaskPagesApprovalPaginationFixture"])
        app.buttons["morde.shell.tab.approvals"].tap()
        let chain = app.buttons["mif.approval.timeline.q0000101"]
        for _ in 0..<8 where !chain.isHittable { app.swipeUp() }
        XCTAssertTrue(chain.waitForExistence(timeout: 15))
        chain.tap()
        let emptyHistory = app.descendants(matching: .any)
            .matching(identifier: "morde.task.activity.emptyHistory").firstMatch
        XCTAssertTrue(emptyHistory.waitForExistence(timeout: 15))
        XCTAssertTrue(emptyHistory.isHittable)
        app.terminate()
    }

    func testApprovalInboxShowsPartialWhenTaskDetailCannotBeRead() throws {
        let app = try launchTaskActionFixture(extraArguments: ["-morde.localTaskPagesApprovalPartialFixture"])
        app.buttons["morde.shell.tab.approvals"].tap()

        let partial = app.descendants(matching: .any)
            .matching(identifier: "mif.approval.partial").firstMatch
        XCTAssertTrue(partial.waitForExistence(timeout: 15))
        XCTAssertTrue(app.staticTexts["mif.approval.count"].label.contains("1"))
        XCTAssertTrue(app.buttons["mif.approval.approve.q0000101:r0000001:1"].exists)
        XCTAssertFalse(app.staticTexts["mif.approval.empty"].exists)
        app.terminate()
    }

    func testTaskDetailsHandleApprovalAndClarificationRequests() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskPagesFixture",
                              "-morde.localTaskPagesTaskDetailRequestsFixture",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        try signInToFixtureAccountIfNeeded("task-detail-requests-user", in: app)

        let tasksTab = app.buttons["morde.shell.tab.tasks"]
        XCTAssertTrue(tasksTab.waitForExistence(timeout: 10))
        tasksTab.tap()

        let approvalRow = app.buttons["morde.tasks.row.q0000101"]
        XCTAssertTrue(approvalRow.waitForExistence(timeout: 10))
        approvalRow.tap()
        let approve = app.buttons["morde.task.approval.approve.r0000001"]
        XCTAssertTrue(approve.waitForExistence(timeout: 10))
        approve.tap()
        let approvalStatus = app.staticTexts["morde.task.status"]
        let approvalRefreshed = XCTNSPredicateExpectation(
            predicate: NSPredicate(format: "label CONTAINS %@", "Local decision refreshed"),
            object: approvalStatus)
        XCTAssertEqual(XCTWaiter.wait(for: [approvalRefreshed], timeout: 15), .completed)

        app.buttons["返回"].tap()
        let clarificationRow = app.buttons["morde.tasks.row.q0000102"]
        XCTAssertTrue(clarificationRow.waitForExistence(timeout: 10))
        clarificationRow.tap()
        XCTAssertTrue(app.staticTexts["morde.task.clarification.question"].waitForExistence(timeout: 10))

        let answer = app.descendants(matching: .any)
            .matching(identifier: "morde.task.clarification.answer").firstMatch
        let send = app.buttons["morde.task.clarification.submit"]
        XCTAssertTrue(answer.waitForExistence(timeout: 10))
        XCTAssertFalse(send.isEnabled)
        answer.tap()
        answer.typeText("Use a PDF.")
        XCTAssertTrue(send.isEnabled)
        send.tap()
        let clarificationStatus = app.staticTexts["morde.task.status"]
        let clarificationRefreshed = XCTNSPredicateExpectation(
            predicate: NSPredicate(format: "label CONTAINS %@", "Local clarification response received"),
            object: clarificationStatus)
        XCTAssertEqual(XCTWaiter.wait(for: [clarificationRefreshed], timeout: 15), .completed)
        app.terminate()
    }

    func testApprovalUnknownResultRefreshesCommittedDecision() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskPagesFixture",
                              "-morde.localTaskPagesApprovalUnknownRetryFixture",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        try signInToFixtureAccountIfNeeded("approval-unknown-retry-user", in: app)

        let approvalsTab = app.buttons["morde.shell.tab.approvals"]
        XCTAssertTrue(approvalsTab.waitForExistence(timeout: 15))
        approvalsTab.tap()
        let approve = app.buttons["mif.approval.approve.q0000101:r0000001:1"]
        XCTAssertTrue(approve.waitForExistence(timeout: 15))
        approve.tap()

        let approvalCount = app.staticTexts["mif.approval.count"]
        XCTAssertTrue(approvalCount.waitForExistence(timeout: 15))
        XCTAssertTrue(app.staticTexts["mif.approval.empty"].waitForExistence(timeout: 15))
        XCTAssertTrue(app.staticTexts["mif.approval.count"].label.contains("0"))
        XCTAssertFalse(approve.exists, "The committed local decision should disappear after task status refresh.")
        app.terminate()
    }

    func testApprovalUnknownResultCanRetryTheSameDecision() throws {
        let app = try launchTaskActionFixture(extraArguments: ["-morde.localTaskPagesApprovalRetryWithoutCommitFixture"])
        app.buttons["morde.shell.tab.approvals"].tap()

        let approve = app.buttons["mif.approval.approve.q0000101:r0000001:1"]
        XCTAssertTrue(approve.waitForExistence(timeout: 15))
        approve.tap()
        XCTAssertTrue(app.descendants(matching: .any)
            .matching(identifier: "mif.approval.action-result").firstMatch.waitForExistence(timeout: 15))
        XCTAssertTrue(app.staticTexts["mif.approval.count"].label.contains("1"))
        XCTAssertTrue(approve.isEnabled)

        approve.tap()
        XCTAssertTrue(app.staticTexts["mif.approval.empty"].waitForExistence(timeout: 15))
        XCTAssertTrue(app.staticTexts["mif.approval.count"].label.contains("0"))
        app.terminate()
    }

    func testApprovalDecisionButtonStaysDisabledWhileSubmitting() throws {
        let app = try launchTaskActionFixture(extraArguments: ["-morde.localTaskPagesApprovalDelayFixture"])
        app.buttons["morde.shell.tab.approvals"].tap()

        let approve = app.buttons["mif.approval.approve.q0000101:r0000001:1"]
        XCTAssertTrue(approve.waitForExistence(timeout: 15))
        approve.tap()
        XCTAssertFalse(approve.isEnabled)
        XCTAssertTrue(app.staticTexts["mif.approval.empty"].waitForExistence(timeout: 15))
        XCTAssertTrue(app.staticTexts["mif.approval.count"].label.contains("0"))
        app.terminate()
    }

    func testTaskActionUnknownResultRetriesWithSameIdempotencyKey() throws {
        let app = try launchTaskActionFixture(extraArguments: ["-morde.localTaskPagesTaskActionUnknownRetryFixture"])
        openFirstTaskPage(in: app)

        let pause = app.buttons["morde.task.action.pause"]
        XCTAssertTrue(pause.waitForExistence(timeout: 10))
        pause.tap()
        let unknown = app.staticTexts["任务操作结果未知；请重试相同操作或刷新 Task 状态确认。"]
        XCTAssertTrue(unknown.waitForExistence(timeout: 15), "A lost action response stays explicitly unknown.")
        XCTAssertTrue(pause.exists, "The stale task snapshot keeps the original action available for retry.")
        XCTAssertTrue(pause.isEnabled, "A failed request can be retried after its unknown result is reported.")
        XCTAssertTrue(app.staticTexts["morde.task.status"].label.contains("运行中"))

        pause.tap()
        let resume = app.buttons["morde.task.action.resume"]
        XCTAssertTrue(resume.waitForExistence(timeout: 15))
        XCTAssertTrue(app.staticTexts["morde.task.status"].label.contains("已暂停"))
        let currentStep = app.staticTexts.matching(
            NSPredicate(format: "label CONTAINS %@", "Local action requests: 2")).firstMatch
        XCTAssertTrue(currentStep.waitForExistence(timeout: 10))
        app.terminate()
    }

    func testTaskActionControlsStayDisabledWhileRequestIsInFlight() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskPagesFixture",
                              "-morde.localTaskPagesTaskActionDelayFixture",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        try signInToFixtureAccountIfNeeded("task-action-flight-user", in: app)

        openTasksRootTab(app)
        app.buttons["morde.tasks.row.q0000001"].tap()
        let pause = app.buttons["morde.task.action.pause"]
        let cancel = app.buttons["morde.task.action.cancel"]
        XCTAssertTrue(pause.waitForExistence(timeout: 15))
        pause.tap()
        XCTAssertFalse(pause.isEnabled)
        XCTAssertFalse(cancel.isEnabled, "A second Task action stays disabled while the first request is in flight.")

        XCTAssertTrue(app.buttons["morde.task.action.resume"].waitForExistence(timeout: 15))
        let currentStep = app.staticTexts.matching(
            NSPredicate(format: "label CONTAINS %@", "Local action requests: 1")).firstMatch
        XCTAssertTrue(currentStep.waitForExistence(timeout: 10), "Only one action request reached the local fixture.")
        app.terminate()
    }

    func testTaskActionRetryKeyIsIsolatedWhenServiceInstanceChanges() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskPagesFixture",
                              "-morde.localTaskPagesTaskActionServiceInstanceFixture",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        try signInToFixtureAccountIfNeeded("task-action-instance-user", in: app)
        openTasksRootTab(app)
        app.buttons["morde.tasks.row.q0000001"].tap()
        let pause = app.buttons["morde.task.action.pause"]
        XCTAssertTrue(pause.waitForExistence(timeout: 15))
        pause.tap()
        XCTAssertTrue(app.staticTexts["任务操作结果未知；请重试相同操作或刷新 Task 状态确认。"]
            .waitForExistence(timeout: 15))

        signOutFromSettings(app)
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("task-action-instance-user", in: app)
        XCTAssertTrue(app.buttons["morde.shell.drawer"].waitForExistence(timeout: 15))
        openTasksRootTab(app)
        app.buttons["morde.tasks.row.q0000001"].tap()
        let retry = app.buttons["morde.task.action.pause"]
        XCTAssertTrue(retry.waitForExistence(timeout: 15))
        retry.tap()

        XCTAssertTrue(app.buttons["morde.task.action.resume"].waitForExistence(timeout: 15),
                      "The new service instance accepts a fresh key for the same account and Task ID.")
        let currentStep = app.staticTexts.matching(
            NSPredicate(format: "label CONTAINS %@", "Local action requests: 2")).firstMatch
        XCTAssertTrue(currentStep.waitForExistence(timeout: 10))
        app.terminate()
    }

    func testAcceptedTaskCompletesOverSSEAfterSwitchingToTasksTab() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskSSEFixture",
                              "-morde.localTaskTabLifecycleFixture",
                              "-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        try signInToFixtureAccountIfNeeded("task-tab-lifecycle-user", in: app)

        openChatActions(in: app)
        app.buttons["morde.shell.taskOptions"].tap()
        let modelPicker = app.descendants(matching: .any).matching(identifier: "morde.task.model").firstMatch
        XCTAssertTrue(modelPicker.waitForExistence(timeout: 10))
        modelPicker.tap()
        let localModel = app.buttons.matching(NSPredicate(format: "label CONTAINS %@", "Local Test Tool")).firstMatch
        XCTAssertTrue(localModel.waitForExistence(timeout: 10))
        localModel.tap()
        app.buttons["Done"].tap()

        let draft = app.descendants(matching: .any).matching(identifier: "morde.chat.composer").firstMatch
        let send = app.buttons["morde.task.send"]
        XCTAssertTrue(draft.waitForExistence(timeout: 10))
        draft.tap()
        draft.typeText("Complete after changing tabs")
        XCTAssertTrue(send.isEnabled)
        send.tap()
        XCTAssertTrue(app.staticTexts["Task accepted before switching tabs"].waitForExistence(timeout: 10))

        openTasksRootTab(app)
        let acceptedTask = app.buttons["morde.tasks.row.q1234567"]
        XCTAssertTrue(acceptedTask.waitForExistence(timeout: 15))
        acceptedTask.tap()

        let result = app.staticTexts["Local Task completed after tab switch"]
        XCTAssertTrue(result.waitForExistence(timeout: 15),
                      "The task event stream completes and refreshes the accepted task after leaving chat.")
        XCTAssertTrue(app.staticTexts["morde.task.status"].label.localizedCaseInsensitiveContains("completed"))
        app.terminate()
    }

    func testRegistrationApprovalHubConfirmationAndSignOut() {
        let app = XCUIApplication()
        let localArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launchArguments = localArguments
        app.launch()
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2) {
            assertRemovedBoardTabsHidden(app)
            signOutFromSettings(app)
        }
        openMordeFromCurrentRoot(app)

        XCTAssertFalse(app.descendants(matching: .any).matching(identifier: "morde.auth.server").firstMatch.exists,
                       "The Portal is the default service; sign-in should not require server setup.")

        let createAccount = app.buttons["第一次使用 Morde？创建账号"]
        XCTAssertTrue(createAccount.waitForExistence(timeout: 10))
        createAccount.tap()

        XCTAssertTrue(app.textFields["morde.auth.username"].waitForExistence(timeout: 10))
        XCTAssertTrue(app.secureTextFields["morde.auth.password"].exists)
        XCTAssertTrue(app.secureTextFields["morde.auth.confirmPassword"].exists)
        XCTAssertEqual(app.secureTextFields.count, 2)

        let inviteOrSMS = app.descendants(matching: .any).matching(NSPredicate(
            format: "label CONTAINS[c] %@ OR label CONTAINS[c] %@ OR label CONTAINS[c] %@ OR label CONTAINS[c] %@",
            "邀请码", "Invite code", "短信", "SMS"
        ))
        XCTAssertEqual(inviteOrSMS.count, 0)

        app.textFields["morde.auth.username"].tap()
        app.textFields["morde.auth.username"].typeText("morde-test-user")
        app.secureTextFields["morde.auth.password"].tap()
        dismissStrongPasswordPromptIfPresent(in: app)
        app.secureTextFields["morde.auth.password"].typeText("MordeTestOnly!2468")
        app.secureTextFields["morde.auth.confirmPassword"].tap()
        dismissStrongPasswordPromptIfPresent(in: app)
        app.secureTextFields["morde.auth.confirmPassword"].typeText("MordeTestOnly!2468")
        app.buttons["morde.auth.submit"].tap()

        XCTAssertTrue(app.staticTexts["morde.registration.pending"].waitForExistence(timeout: 10))
        app.buttons["已有账号？登录"].tap()
        XCTAssertFalse(app.secureTextFields["morde.auth.confirmPassword"].exists)
        app.secureTextFields["morde.auth.password"].tap()
        app.secureTextFields["morde.auth.password"].typeText("MordeTestOnly!2468")
        app.buttons["morde.auth.submit"].tap()

        let pendingLogin = app.alerts.firstMatch
        XCTAssertTrue(pendingLogin.waitForExistence(timeout: 10))
        XCTAssertTrue(pendingLogin.staticTexts["账号正在等待管理员审核。"].exists)
        pendingLogin.buttons["确定"].tap()

        app.terminate()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localHubFixture",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        openMordeFromCurrentRoot(app)
        XCTAssertTrue(app.textFields["morde.auth.username"].waitForExistence(timeout: 10))
        app.textFields["morde.auth.username"].tap()
        app.textFields["morde.auth.username"].typeText("morde-test-user")
        app.secureTextFields["morde.auth.password"].tap()
        app.secureTextFields["morde.auth.password"].typeText("MordeTestOnly!2468")
        app.buttons["morde.auth.submit"].tap()

        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 10) {
            assertRemovedBoardTabsHidden(app)
        }

        let accountRow = app.buttons["morde.settings.account"]
        if !accountRow.waitForExistence(timeout: 2), app.navigationBars.buttons.firstMatch.exists {
            app.navigationBars.buttons.firstMatch.tap()
        }
        if !accountRow.waitForExistence(timeout: 2) {
            app.buttons["morde.shell.drawer"].tap()
            app.buttons["morde.shell.settings"].tap()
        }
        XCTAssertTrue(accountRow.waitForExistence(timeout: 10))
        accountRow.tap()
        let manageInstallations = app.buttons["morde.account.manageInstallations"]
        scrollUntilVisible(manageInstallations, in: app)
        XCTAssertTrue(manageInstallations.waitForExistence(timeout: 10))
        manageInstallations.tap()

        let relayURL = app.textFields["morde.hub.open.url"]
        XCTAssertTrue(relayURL.waitForExistence(timeout: 10))
        relayURL.tap()
        relayURL.typeText("https://example.invalid/safe")
        let openRequests = app.buttons.matching(identifier: "morde.hub.open.request")
        XCTAssertEqual(openRequests.count, 1)
        XCTAssertTrue(app.staticTexts["morde.hub.revokedDevice"].exists)
        let openRequest = openRequests.firstMatch
        openRequest.tap()
        let authorizationPrompt = app.alerts.firstMatch
        XCTAssertTrue(authorizationPrompt.waitForExistence(timeout: 10))
        XCTAssertTrue(authorizationPrompt.buttons["morde.hub.open.confirm"].exists)
        let cancelRemoteOpen = authorizationPrompt.buttons["morde.hub.open.cancel"].firstMatch
        XCTAssertTrue(cancelRemoteOpen.exists)
        cancelRemoteOpen.tap()
        XCTAssertFalse(authorizationPrompt.exists)

        app.navigationBars.buttons.firstMatch.tap()
        let signOut = app.buttons["morde.account.signOut"]
        scrollUntilVisible(signOut, in: app)
        XCTAssertTrue(signOut.waitForExistence(timeout: 10))
        signOut.tap()
        let confirmSignOut = app.descendants(matching: .any)
            .matching(identifier: "morde.account.signOut.confirm").firstMatch
        XCTAssertTrue(confirmSignOut.waitForExistence(timeout: 10))
        confirmSignOut.tap()
        let signedOut = expectation(for: NSPredicate(format: "exists == false"),
                                    evaluatedWith: manageInstallations)
        wait(for: [signedOut], timeout: 10)
        XCTAssertFalse(app.buttons["morde.account.signOut"].exists)
    }

    func testRejectedAndDisabledAccountsShowLocalizedMessages() {
        let app = XCUIApplication()
        assertLoginMessage(app, statusArgument: "-morde.localRegistrationRejected",
                          username: "rejected-local-user", message: "账号未通过审核，请联系管理员。")
        assertLoginMessage(app, statusArgument: "-morde.localRegistrationDisabled",
                          username: "disabled-local-user", message: "账号已停用，请联系管理员。")
    }

    func testRegistrationSubmitCannotBeRepeatedWhileRequestIsInFlight() {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationBusyFixture",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        openMordeFromCurrentRoot(app)
        let createAccount = app.buttons["第一次使用 Morde？创建账号"]
        XCTAssertTrue(createAccount.waitForExistence(timeout: 10))
        createAccount.tap()
        let username = app.textFields["morde.auth.username"]
        XCTAssertTrue(username.waitForExistence(timeout: 10))
        username.tap()
        username.typeText("morde-busy-fixture-user")
        app.secureTextFields["morde.auth.password"].tap()
        dismissStrongPasswordPromptIfPresent(in: app)
        app.secureTextFields["morde.auth.password"].typeText("MordeBusyFixture!2468")
        app.secureTextFields["morde.auth.confirmPassword"].tap()
        dismissStrongPasswordPromptIfPresent(in: app)
        app.secureTextFields["morde.auth.confirmPassword"].typeText("MordeBusyFixture!2468")

        let submit = app.buttons["morde.auth.submit"]
        submit.tap()
        XCTAssertFalse(submit.isEnabled)
        submit.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.5)).tap()
        XCTAssertTrue(app.staticTexts["morde.registration.pending"].waitForExistence(timeout: 8))
        XCTAssertFalse(app.alerts["请求失败"].waitForExistence(timeout: 4))
        app.terminate()
    }

    func testSwitchingAccountsDoesNotReuseConversationMessagesOrTasks() throws {
        // nanoMuse: Exercise account-scoped conversation and Task state only through the process-local Portal fixture.
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localAccountIsolationFixture",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2) {
            signOutFromSettings(app)
        }

        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("account-a", in: app)
        let accountAMessage = app.staticTexts["account-a-private-message"]
        XCTAssertTrue(accountAMessage.waitForExistence(timeout: 15))
        let accountATurnTwo = app.staticTexts["account-a-private-message-2"]
        for _ in 0..<8 where !accountATurnTwo.exists { app.swipeUp() }
        XCTAssertTrue(accountATurnTwo.exists)
        let accountAReplyTwo = app.staticTexts["account-a-private-reply-2"]
        for _ in 0..<8 where !accountAReplyTwo.exists { app.swipeUp() }
        XCTAssertTrue(accountAReplyTwo.exists)

        app.buttons["morde.shell.tab.tasks"].tap()
        let accountATask = app.staticTexts["account-a-private-task"]
        XCTAssertTrue(accountATask.waitForExistence(timeout: 15))
        signOutFromSettings(app)
        app.terminate()

        app.launch()
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("account-b", in: app)
        let accountBMessage = app.staticTexts["account-b-private-message"]
        XCTAssertTrue(accountBMessage.waitForExistence(timeout: 15))
        let accountBTurnTwo = app.staticTexts["account-b-private-message-2"]
        for _ in 0..<8 where !accountBTurnTwo.exists { app.swipeUp() }
        XCTAssertTrue(accountBTurnTwo.exists)
        let accountBReplyTwo = app.staticTexts["account-b-private-reply-2"]
        for _ in 0..<8 where !accountBReplyTwo.exists { app.swipeUp() }
        XCTAssertTrue(accountBReplyTwo.exists)
        XCTAssertFalse(accountAMessage.exists)
        XCTAssertFalse(accountATurnTwo.exists)

        app.buttons["morde.shell.tab.tasks"].tap()
        XCTAssertTrue(app.staticTexts["account-b-private-task"].waitForExistence(timeout: 15))
        XCTAssertFalse(accountATask.exists)
        signOutFromSettings(app)
        app.terminate()
    }

    func testLateTaskPageFromPreviousAccountCannotReplaceCurrentTasks() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localAccountIsolationFixture",
                              "-morde.localDelayedAccountResponseFixture",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        if app.buttons["morde.tasks.open"].waitForExistence(timeout: 2) {
            app.windows.firstMatch.coordinate(withNormalizedOffset: CGVector(dx: 0.96, dy: 0.5)).tap()
        }
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2) {
            signOutFromSettings(app)
        }

        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("account-a", in: app)
        XCTAssertTrue(app.buttons["morde.shell.drawer"].waitForExistence(timeout: 10))
        openTasksRootTab(app)
        XCTAssertTrue(app.staticTexts["account-a-private-task"].waitForExistence(timeout: 15))
        let loadingMore = app.descendants(matching: .any)
            .matching(identifier: "morde.tasks.loadingMore").firstMatch
        let requestStartedAt = Date.timeIntervalSinceReferenceDate
        for _ in 0..<20 where !loadingMore.exists { app.swipeUp() }
        XCTAssertTrue(loadingMore.waitForExistence(timeout: 10),
                      "Scrolling to the task list end should start the next-page request.")

        signOutFromSettings(app)
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("account-b", in: app)
        openTasksRootTab(app)
        let accountBTask = app.staticTexts["account-b-private-task"]
        XCTAssertTrue(accountBTask.waitForExistence(timeout: 15))

        // Let account A's already-started page request finish after account B has loaded.
        let remaining = max(13 - (Date.timeIntervalSinceReferenceDate - requestStartedAt), 0)
        Thread.sleep(forTimeInterval: remaining)
        XCTAssertTrue(accountBTask.exists)
        XCTAssertFalse(app.staticTexts["account-a-private-task"].exists)
        XCTAssertFalse(app.staticTexts["account-a-late-private-task"].exists)
        signOutFromSettings(app)
        app.terminate()
    }

    func testLateConversationMessagesFromPreviousAccountCannotReplaceCurrentChat() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localAccountIsolationFixture",
                              "-morde.localDelayedAccountChatResponseFixture",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2) {
            signOutFromSettings(app)
        }

        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("account-a", in: app)
        XCTAssertTrue(app.staticTexts["account-a-private-message"].waitForExistence(timeout: 15))
        app.buttons["morde.shell.drawer"].tap()
        let delayedChat = app.buttons["morde.shell.cloudConversation.c0000003"]
        XCTAssertTrue(delayedChat.waitForExistence(timeout: 10))
        let requestStartedAt = Date.timeIntervalSinceReferenceDate
        delayedChat.tap()
        let loadingMessages = app.staticTexts.matching(
            NSPredicate(format: "label == %@", "消息正在读取。")).firstMatch
        XCTAssertTrue(loadingMessages.waitForExistence(timeout: 10))
        XCTAssertFalse(app.staticTexts["account-a-delayed-private-message"].exists)

        signOutFromSettings(app)
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("account-b", in: app)
        let accountBMessage = app.staticTexts["account-b-private-message"]
        XCTAssertTrue(accountBMessage.waitForExistence(timeout: 15))

        let remaining = max(27 - (Date.timeIntervalSinceReferenceDate - requestStartedAt), 0)
        Thread.sleep(forTimeInterval: remaining)
        XCTAssertTrue(accountBMessage.exists)
        XCTAssertFalse(app.staticTexts["account-a-delayed-private-message"].exists)
        XCTAssertFalse(app.staticTexts["account-a-private-message"].exists)
        signOutFromSettings(app)
        app.terminate()
    }

    func testLocalTaskSSEUsesInjectedSession() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskSSEFixture",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        try signInToFixtureAccountIfNeeded("sse-fixture-user", in: app)
        XCTAssertTrue(app.staticTexts["Local SSE fixture reply"].waitForExistence(timeout: 15))
        app.terminate()
    }

    func testSequentialTaskSendsStayInTheSameConversation() throws {
        let app = XCUIApplication()
        // nanoMuse: Keep a valid local base URL so the fixture sign-in does not open the server-address prompt.
        let fixtureServer = "https://127.0.0.1:\(Int.random(in: 20000...60000))"
        app.launchArguments = ["-morde.server", fixtureServer, "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskSSEFixture",
                              "-morde.localTaskSequentialSendFixture",
                              "-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2) {
            signOutFromSettings(app)
        }
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("sequential-send-user", in: app)

        let draft = app.descendants(matching: .any).matching(identifier: "morde.chat.composer").firstMatch
        let send = app.buttons["morde.task.send"]
        XCTAssertTrue(draft.waitForExistence(timeout: 10))
        draft.tap()
        draft.typeText("Sequential prompt one")
        let firstSendEnabled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isEnabled == true"), object: send)
        wait(for: [firstSendEnabled], timeout: 10)
        XCTAssertTrue(send.isEnabled)
        send.tap()

        let firstReply = app.staticTexts["Local sequential reply 1"]
        let transcript = app.scrollViews["morde.chat.transcript"]
        XCTAssertTrue(transcript.waitForExistence(timeout: 10))
        let firstSendAlert = app.alerts.firstMatch
        if firstSendAlert.waitForExistence(timeout: 3) {
            XCTFail("First sequential send failed: \(firstSendAlert.staticTexts.allElementsBoundByIndex.map(\.label).joined(separator: " | "))")
            return
        }
        dismissKeyboard(in: app)
        for _ in 0..<8 where !firstReply.isHittable { transcript.swipeDown() }
        XCTAssertTrue(firstReply.waitForExistence(timeout: 15))
        XCTAssertTrue(app.staticTexts["Sequential prompt one"].exists)
        for _ in 0..<6 where !draft.isHittable { app.swipeUp() }
        draft.tap()
        draft.typeText("Sequential prompt two")
        let secondSendEnabled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isEnabled == true"), object: send)
        wait(for: [secondSendEnabled], timeout: 10)
        XCTAssertTrue(send.isEnabled)
        send.tap()
        let secondSendAlert = app.alerts.firstMatch
        if secondSendAlert.waitForExistence(timeout: 3) {
            XCTFail("Second sequential send failed: \(secondSendAlert.staticTexts.allElementsBoundByIndex.map(\.label).joined(separator: " | "))")
            return
        }
        dismissKeyboard(in: app)

        let secondPrompt = app.staticTexts["Sequential prompt two"]
        let secondReply = app.staticTexts["Local sequential reply 2"]
        let firstPrompt = app.staticTexts["Sequential prompt one"]
        for _ in 0..<5 where !firstReply.isHittable { transcript.swipeDown() }
        XCTAssertTrue(firstPrompt.waitForExistence(timeout: 15))
        XCTAssertTrue(firstReply.waitForExistence(timeout: 15))
        for _ in 0..<8 where !secondPrompt.isHittable { transcript.swipeUp() }
        XCTAssertTrue(secondPrompt.waitForExistence(timeout: 15))
        for _ in 0..<8 where !secondReply.isHittable { transcript.swipeUp() }
        XCTAssertTrue(secondReply.waitForExistence(timeout: 15))
        signOutFromSettings(app)
        app.terminate()
    }

    func testProjectOutboxVisibleAfterOfflineRestartWithoutPersonalConversation() throws {
        let app = XCUIApplication()
        let baseArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                             "-morde.localRegistrationApproved", "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launchArguments = baseArguments + ["-morde.localProjectQueueFixture"]
        app.launch()
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2) { signOutFromSettings(app) }
        openMordeFromCurrentRoot(app)
        let fixtureUser = "project-offline-" + String(UUID().uuidString.prefix(8)).lowercased()
        try signInToFixtureAccount(fixtureUser, in: app)
        let disclosure = app.descendants(matching: .any).matching(identifier: "morde.projectOutbox.disclosure").firstMatch
        XCTAssertTrue(disclosure.waitForExistence(timeout: 20))
        app.terminate()

        app.launchArguments = baseArguments + ["-morde.localProjectQueueOffline"]
        app.launch()
        let alert = app.alerts.firstMatch
        if alert.waitForExistence(timeout: 4) { alert.buttons.firstMatch.tap() }
        openMordeFromCurrentRoot(app)
        if alert.waitForExistence(timeout: 2) { alert.buttons.firstMatch.tap() }
        XCTAssertTrue(disclosure.waitForExistence(timeout: 15), "The scoped project queue must remain discoverable offline.")
        disclosure.tap()
        let message = app.staticTexts["Local offline project message"]
        XCTAssertTrue(message.waitForExistence(timeout: 5))
        app.buttons["重试发送"].tap()
        XCTAssertTrue(message.exists, "Retrying while offline must keep the message locally.")
        app.terminate()
    }

    func testIdentityPendingMessageSurvivesRestartAndNeedsExplicitRecovery() throws {
        let app = XCUIApplication()
        let baseArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                             "-morde.localRegistrationApproved", "-morde.localTaskSSEFixture",
                             "-morde.localTaskSequentialSendFixture", "-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launchArguments = baseArguments + ["-morde.localDelayIdentityFixture"]
        app.launch()
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2) { signOutFromSettings(app) }
        openMordeFromCurrentRoot(app)
        let fixtureUser = "identity-pending-" + String(UUID().uuidString.prefix(8)).lowercased()
        try signInToFixtureAccount(fixtureUser, in: app)
        let draft = app.descendants(matching: .any).matching(identifier: "morde.chat.composer").firstMatch
        XCTAssertTrue(draft.waitForExistence(timeout: 10))
        let text = "Local pending " + String(UUID().uuidString.prefix(8))
        draft.tap()
        draft.typeText(text)
        app.buttons["morde.task.send"].tap()
        XCTAssertTrue(app.staticTexts[text].waitForExistence(timeout: 10))
        XCTAssertTrue(app.staticTexts["账号或服务身份待确认；消息仅保存在本机。"].exists)
        app.terminate()

        app.launchArguments = baseArguments
        app.launch()
        openMordeFromCurrentRoot(app)
        XCTAssertTrue(app.staticTexts[text].waitForExistence(timeout: 20), "A restart must not lose or auto-send the local record.")
        let restore = app.buttons["确认并恢复"]
        XCTAssertTrue(restore.waitForExistence(timeout: 10))
        restore.tap()
        app.buttons["确认归属并恢复到输入框"].tap()
        let restored = expectation(for: NSPredicate(format: "value == %@", text), evaluatedWith: draft)
        wait(for: [restored], timeout: 10)
        XCTAssertTrue(app.staticTexts[text].exists, "Recovery must retain the local original until explicitly removed.")
        app.buttons["删除本机记录"].tap()
        app.buttons["morde.unconfirmed.delete.confirm"].firstMatch.tap()
        app.terminate()
    }

    func testAnotherMessageCanBeSentWhileFirstResponseIsPending() throws {
        let app = XCUIApplication()
        let fixtureServer = "https://127.0.0.1:\(Int.random(in: 20000...60000))"
        app.launchArguments = ["-morde.server", fixtureServer, "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskSSEFixture",
                              "-morde.localTaskSequentialSendFixture", "-morde.localDelayFirstMessageResponse",
                              "-morde.localAdjustmentFixture", "-morde.localFixtureDiagnostics",
                              "-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2) { signOutFromSettings(app) }
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("concurrent-send-user", in: app)
        let draft = app.descendants(matching: .any).matching(identifier: "morde.chat.composer").firstMatch
        let send = app.buttons["morde.task.send"]
        XCTAssertTrue(draft.waitForExistence(timeout: 10))
        draft.tap()
        draft.typeText("Sequential prompt one")
        XCTAssertTrue(send.isEnabled)
        send.tap()
        XCTAssertTrue(app.staticTexts["Sending"].waitForExistence(timeout: 5))
        draft.tap()
        draft.typeText("Sequential prompt two")
        XCTAssertTrue(send.isEnabled)
        let keyboard = app.keyboards.firstMatch
        XCTAssertTrue(keyboard.waitForExistence(timeout: 5))
        XCTAssertGreaterThanOrEqual(send.frame.width, 44)
        XCTAssertGreaterThanOrEqual(send.frame.height, 44)
        XCTAssertLessThanOrEqual(send.frame.maxY, keyboard.frame.minY + 1)
        let keyboardLayout = XCTAttachment(screenshot: app.screenshot())
        keyboardLayout.name = "Chat with keyboard and pending message"
        keyboardLayout.lifetime = .keepAlways
        add(keyboardLayout)
        send.tap()
        XCTAssertTrue(app.staticTexts["Sequential prompt two"].waitForExistence(timeout: 10))
        XCTAssertTrue(app.staticTexts["Sequential prompt one"].exists)
        XCTAssertTrue(app.staticTexts["Adjustment received; waiting to update"].waitForExistence(timeout: 10))
        let transcript = app.descendants(matching: .any).matching(identifier: "morde.chat.transcript").firstMatch
        XCTAssertTrue(transcript.waitForExistence(timeout: 5))
        let visibleReply = app.staticTexts["Local sequential reply 2"]
        XCTAssertTrue(visibleReply.waitForExistence(timeout: 5))
        XCTAssertTrue(visibleReply.isHittable)
        let composerSend = app.buttons["morde.task.send"]
        // nanoMuse: Attachments moved to the chat menu; measure against the persistent send control inside the composer pill.
        let keyboardPosition = XCTNSPredicateExpectation(predicate: NSPredicate { _, _ in
            visibleReply.frame.maxY + 10 <= composerSend.frame.minY - 20 - 8
        }, object: nil)
        let keyboardLayoutResult = XCTWaiter.wait(for: [keyboardPosition], timeout: 5)
        if keyboardLayoutResult != .completed {
            let screenshot = XCTAttachment(screenshot: app.screenshot())
            screenshot.name = "Latest reply and composer after layout"
            screenshot.lifetime = .keepAlways
            add(screenshot)
        }
        XCTAssertEqual(keyboardLayoutResult, .completed,
                       "The newest reply must stay above the composer while typing.")
        visibleReply.tap()
        XCTAssertTrue(keyboard.waitForNonExistence(timeout: 5))
        let chatTab = app.buttons["morde.shell.tab.chat"]
        XCTAssertTrue(chatTab.waitForExistence(timeout: 5))
        XCTAssertTrue(chatTab.isHittable)
        let latestReply = app.staticTexts["Local sequential reply 2"]
        XCTAssertTrue(latestReply.waitForExistence(timeout: 10))
        XCTAssertTrue(latestReply.isHittable)
        let restingPosition = XCTNSPredicateExpectation(predicate: NSPredicate { _, _ in
            latestReply.frame.maxY + 10 <= composerSend.frame.minY - 20 - 8
        }, object: nil)
        XCTAssertEqual(XCTWaiter.wait(for: [restingPosition], timeout: 5), .completed)
        let restingLayout = XCTAttachment(screenshot: app.screenshot())
        restingLayout.name = "Chat with bottom navigation"
        restingLayout.lifetime = .keepAlways
        add(restingLayout)
        app.terminate()
    }

    func testConfirmedRejectionCanBeBypassedWithoutLosingTheOriginal() throws {
        let app = XCUIApplication()
        let fixtureServer = "https://127.0.0.1:\(Int.random(in: 20000...60000))"
        let baseArguments = ["-morde.server", fixtureServer, "-morde.localRegistrationStub",
                             "-morde.localRegistrationApproved", "-morde.localTaskSSEFixture",
                             "-morde.localTaskSequentialSendFixture", "-morde.localIndependentSendFixture",
                             "-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launchArguments = baseArguments + ["-morde.localDelayFirstMessageResponse"]
        app.launch()
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2) { signOutFromSettings(app) }
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("independent-" + String(UUID().uuidString.prefix(8)).lowercased(), in: app)

        let draft = app.descendants(matching: .any).matching(identifier: "morde.chat.composer").firstMatch
        let send = app.buttons["morde.task.send"]
        XCTAssertTrue(draft.waitForExistence(timeout: 10))
        draft.tap(); draft.typeText("Synthetic rejected message"); send.tap()
        XCTAssertTrue(app.staticTexts["Synthetic permanent rejection"].waitForExistence(timeout: 10))
        draft.tap(); draft.typeText("Independent follow-up"); send.tap()
        let independent = app.buttons["Send this message independently"]
        XCTAssertTrue(independent.waitForExistence(timeout: 10))
        independent.tap()
        XCTAssertTrue(app.staticTexts["The failed message remains on this device. It will not be included in the AI task for this message."].waitForExistence(timeout: 5))
        let cancel = app.buttons["Cancel"].firstMatch
        if cancel.exists { cancel.tap() }
        else { app.otherElements["PopoverDismissRegion"].firstMatch.tap() }
        XCTAssertTrue(independent.exists, "Cancel must not change the predecessor or start sending.")
        XCTAssertTrue(app.staticTexts["Synthetic rejected message"].exists)
        app.terminate()

        app.launch()
        openMordeFromCurrentRoot(app)
        XCTAssertTrue(app.staticTexts["Synthetic rejected message"].waitForExistence(timeout: 15))
        XCTAssertTrue(independent.waitForExistence(timeout: 10), "Failure evidence must survive a restart.")
        independent.tap()
        app.buttons["morde.outbox.detach.confirm"].firstMatch.tap()
        XCTAssertTrue(app.staticTexts["Sending"].waitForExistence(timeout: 5))
        app.terminate()

        app.launchArguments = baseArguments
        app.launch()
        openMordeFromCurrentRoot(app)
        XCTAssertTrue(app.staticTexts["Independent follow-up"].waitForExistence(timeout: 15))
        XCTAssertTrue(app.staticTexts["Local sequential reply 1"].waitForExistence(timeout: 15))
        XCTAssertTrue(app.staticTexts["Synthetic rejected message"].exists, "The rejected original must remain local.")
        XCTAssertFalse(independent.exists, "The confirmed predecessor change must survive a restart.")
        draft.tap(); draft.typeText("Following message"); send.tap()
        XCTAssertTrue(app.staticTexts["Local sequential reply 2"].waitForExistence(timeout: 15),
                      "The next message must refer to the independently sent message.")
        XCTAssertTrue(app.staticTexts["Synthetic rejected message"].exists)
        app.terminate()
    }

    func testUnknownDeliveryAndFailedLookupCannotBeBypassed() throws {
        let app = XCUIApplication()
        let fixtureServer = "https://127.0.0.1:\(Int.random(in: 20000...60000))"
        app.launchArguments = ["-morde.server", fixtureServer, "-morde.localRegistrationStub",
                               "-morde.localRegistrationApproved", "-morde.localTaskSSEFixture",
                               "-morde.localTaskSequentialSendFixture", "-morde.localUncertainReconcileFixture",
                               "-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2) { signOutFromSettings(app) }
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("uncertain-" + String(UUID().uuidString.prefix(8)).lowercased(), in: app)
        let draft = app.descendants(matching: .any).matching(identifier: "morde.chat.composer").firstMatch
        let send = app.buttons["morde.task.send"]
        XCTAssertTrue(draft.waitForExistence(timeout: 10))
        draft.tap(); draft.typeText("Synthetic ambiguous message"); send.tap()
        XCTAssertTrue(app.staticTexts["Send status unknown"].waitForExistence(timeout: 10))
        draft.tap(); draft.typeText("Wait for verification"); send.tap()
        XCTAssertTrue(app.staticTexts["Synthetic lookup failure"].waitForExistence(timeout: 15))
        XCTAssertTrue(app.staticTexts["Synthetic ambiguous message"].exists)
        XCTAssertTrue(app.staticTexts["Wait for verification"].exists)
        XCTAssertTrue(app.staticTexts["Send status unknown"].exists)
        XCTAssertFalse(app.buttons["Send this message independently"].exists)
        XCTAssertFalse(app.staticTexts["Local sequential reply 1"].exists)
        app.terminate()
    }

    func testRoutingFailureCanBeRetriedWithoutResendingMessage() throws {
        let app = XCUIApplication()
        // nanoMuse: Keep a valid local base URL so the fixture login exercises routing retry instead of server setup.
        let fixtureServer = "https://127.0.0.1:\(Int.random(in: 20000...60000))"
        app.launchArguments = ["-morde.server", fixtureServer, "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskSSEFixture",
                              "-morde.localTaskSequentialSendFixture", "-morde.localRoutingFailureFixture",
                              "-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2) { signOutFromSettings(app) }
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("routing-retry-user", in: app)
        let draft = app.descendants(matching: .any).matching(identifier: "morde.chat.composer").firstMatch
        XCTAssertTrue(draft.waitForExistence(timeout: 10))
        draft.tap()
        draft.typeText("Sequential prompt one")
        app.buttons["morde.task.send"].tap()
        let failure = app.staticTexts["Model temporarily unavailable"]
        XCTAssertTrue(failure.waitForExistence(timeout: 15))
        let retry = app.buttons["Retry processing"]
        XCTAssertTrue(retry.waitForExistence(timeout: 5))
        retry.tap()
        XCTAssertTrue(failure.waitForNonExistence(timeout: 15))
        XCTAssertEqual(app.staticTexts.matching(identifier: "Sequential prompt one").count, 1)
        app.terminate()
    }

    func testMessageExceedingModelLimitKeepsTextAndDoesNotOfferUnchangedRetry() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskSSEFixture",
                              "-morde.localTaskSequentialSendFixture", "-morde.localMessageTooLongFixture",
                              "-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2) { signOutFromSettings(app) }
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("message-limit-user", in: app)
        let draft = app.descendants(matching: .any).matching(identifier: "morde.chat.composer").firstMatch
        XCTAssertTrue(draft.waitForExistence(timeout: 10))
        draft.tap()
        draft.typeText("Sequential prompt one")
        app.buttons["morde.task.send"].tap()
        let failure = app.staticTexts["Message and context exceed the model input limit; the original is retained. Shorten and send again."]
        XCTAssertTrue(failure.waitForExistence(timeout: 15))
        XCTAssertTrue(app.staticTexts["Sequential prompt one"].exists)
        XCTAssertFalse(app.buttons["Retry processing"].exists)
        draft.tap()
        draft.typeText("Sequential prompt two")
        app.buttons["morde.task.send"].tap()
        XCTAssertTrue(app.staticTexts["Sequential prompt two"].waitForExistence(timeout: 10))
        app.terminate()
    }

    func testChinesePinyinInputCanBeSent() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskSSEFixture",
                              "-morde.localTaskSequentialSendFixture", "-morde.localFixtureDiagnostics",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2) { signOutFromSettings(app) }
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("pinyin-test-user", in: app)
        let draft = app.descendants(matching: .any).matching(identifier: "morde.chat.composer").firstMatch
        XCTAssertTrue(draft.waitForExistence(timeout: 10))
        draft.tap()
        let keyboard = app.keyboards.firstMatch
        XCTAssertTrue(keyboard.waitForExistence(timeout: 5))
        for letter in ["n", "i", "h", "a", "o"] {
            let lowercase = keyboard.keys[letter]
            let key = lowercase.exists ? lowercase : keyboard.keys[letter.uppercased()]
            XCTAssertTrue(key.waitForExistence(timeout: 5), "Missing Pinyin key \(letter).")
            key.tap()
        }
        let composing = XCTAttachment(screenshot: app.screenshot())
        composing.name = "Chinese Pinyin composition"
        composing.lifetime = .keepAlways
        add(composing)
        let send = app.buttons["morde.task.send"]
        XCTAssertTrue(send.waitForExistence(timeout: 5))
        XCTAssertTrue(send.isEnabled, "Composing text must not disable sending.")
        let candidate = app.descendants(matching: .any).matching(NSPredicate(format: "label BEGINSWITH %@", "你好")).firstMatch
        XCTAssertTrue(candidate.waitForExistence(timeout: 5), "The simplified Pinyin keyboard should offer 你好.")
        candidate.tap()
        send.tap()
        XCTAssertTrue(app.staticTexts["你好"].waitForExistence(timeout: 15))
        app.terminate()
    }

    func testLocalCrossClientTextSync() throws {
        guard let path = UserDefaults.standard.string(forKey: "MordeCrossDeviceFixturePath") else {
            throw XCTSkip("No local cross-device test fixture is configured.")
        }
        let data = try Data(contentsOf: URL(fileURLWithPath: path))
        let fixture = try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [String: String])
        let server = try XCTUnwrap(fixture["url"])
        let username = try XCTUnwrap(fixture["username"])
        let password = try XCTUnwrap(fixture["password"])
        _ = try XCTUnwrap(fixture["conversation_id"])
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", server, "-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount(username, in: app, password: password)
        let browserText = "跨端验收：这条消息从浏览器发送。"
        XCTAssertTrue(app.staticTexts[browserText].waitForExistence(timeout: 30),
                      "The browser's message should appear in the same cloud conversation.")
        let draft = app.descendants(matching: .any).matching(identifier: "morde.chat.composer").firstMatch
        XCTAssertTrue(draft.waitForExistence(timeout: 10))
        draft.tap()
        let text = "Cross-device acceptance from iOS simulator (local fixture)."
        draft.typeText(text)
        let send = app.buttons["morde.task.send"]
        XCTAssertTrue(send.waitForExistence(timeout: 5))
        XCTAssertTrue(send.isEnabled)
        send.tap()
        XCTAssertTrue(app.staticTexts[text].waitForExistence(timeout: 15))
        app.terminate()
    }

    func testMessageCanReferenceTheTaskExplicitlyChosenByTheUser() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localMainChatFixture",
                              "-morde.localTaskSSEFixture", "-morde.localTaskQueueFixture",
                              "-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2) { signOutFromSettings(app) }
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("explicit-append-user", in: app)

        let target = app.buttons["morde.chat.taskTarget"]
        XCTAssertTrue(target.waitForExistence(timeout: 15))
        let task = app.staticTexts["Local active queue fixture"]
        XCTAssertTrue(task.waitForExistence(timeout: 15))
        target.tap()
        app.buttons["morde.chat.taskTarget.option.q1234567"].tap()
        let draft = app.descendants(matching: .any).matching(identifier: "morde.chat.composer").firstMatch
        draft.tap()
        draft.typeText("Additional task detail")
        let send = app.buttons["morde.task.send"]
        let enabled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isEnabled == true"), object: send)
        wait(for: [enabled], timeout: 10)
        send.tap()
        XCTAssertTrue(app.staticTexts["Additional task detail"].waitForExistence(timeout: 15))
        XCTAssertFalse(app.alerts.firstMatch.exists)
        app.terminate()
    }

    func testFreshLaunchOpensMainChatAndSideConversationsStayInDrawer() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localMainChatFixture",
                              "-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        let username = app.textFields["morde.auth.username"]
        if !username.waitForExistence(timeout: 2) {
            signOutFromSettings(app)
            app.terminate()
            app.launch()
        }

        XCTAssertTrue(app.buttons["morde.shell.tab.chat"].waitForExistence(timeout: 10))
        XCTAssertTrue(app.buttons["morde.shell.tab.chat"].isSelected)
        XCTAssertTrue(username.waitForExistence(timeout: 10), "Fresh launch should land directly in Morde chat sign-in.")
        XCTAssertFalse(app.buttons["Morde cloud tasks"].exists)
        XCTAssertFalse(app.textFields["morde.auth.server"].exists)

        try signInToFixtureAccount("main-chat-user", in: app)
        let mainMessage = app.staticTexts["Main chat is ready."]
        XCTAssertTrue(mainMessage.waitForExistence(timeout: 15), "Uninitialized accounts should be sent through main-conversation.ensure.")
        let composer = app.descendants(matching: .any).matching(identifier: "morde.chat.composer").firstMatch
        XCTAssertTrue(composer.waitForExistence(timeout: 10))
        let send = app.buttons["morde.task.send"]
        composer.tap()
        composer.typeText("Use the default model")
        let defaultModelReady = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isEnabled == true"), object: send)
        wait(for: [defaultModelReady], timeout: 10)
        XCTAssertTrue(send.isEnabled, "The first available cloud text model should be selected automatically.")
        dismissKeyboard(in: app)
        app.buttons["morde.shell.drawer"].tap()
        XCTAssertFalse(app.buttons["All routines"].exists)
        XCTAssertFalse(app.buttons["System files"].exists)
        app.coordinate(withNormalizedOffset: CGVector(dx: 0.95, dy: 0.5)).tap()
        openChatActions(in: app)
        app.buttons["morde.shell.taskOptions"].tap()
        let modelPicker = app.descendants(matching: .any).matching(identifier: "morde.task.model").firstMatch
        XCTAssertTrue(modelPicker.waitForExistence(timeout: 10))
        modelPicker.tap()
        let personalModel = app.buttons.matching(
            NSPredicate(format: "label CONTAINS %@", "Local Personal Model")).firstMatch
        XCTAssertTrue(personalModel.waitForExistence(timeout: 10))
        XCTAssertFalse(app.buttons.matching(
            NSPredicate(format: "label CONTAINS %@", "Non-default shared model")).firstMatch.exists,
            "Only the administrator default and checked personal models are selectable.")
        personalModel.tap()
        app.buttons["Done"].tap()
        XCTAssertTrue(send.isEnabled, "A checked personal text+tools model is selectable while the admin default is configured.")
        XCTAssertFalse(app.staticTexts["Side conversation is sidebar-only."].exists)

        app.buttons["morde.shell.drawer"].tap()
        let sideChat = app.buttons["morde.shell.cloudConversation.c2345678"]
        XCTAssertTrue(sideChat.waitForExistence(timeout: 10))
        sideChat.tap()
        XCTAssertTrue(app.staticTexts["Side conversation is sidebar-only."].waitForExistence(timeout: 10))

        app.buttons["morde.shell.drawer"].tap()
        app.buttons["morde.shell.mainChat"].tap()
        XCTAssertTrue(mainMessage.waitForExistence(timeout: 10))
        app.buttons["morde.shell.drawer"].tap()
        app.buttons["morde.shell.settings"].tap()
        XCTAssertTrue(app.buttons["morde.settings.account"].waitForExistence(timeout: 5),
                      "The existing Morde account page should remain available.")
        XCTAssertFalse(app.buttons["morde Cloud"].exists,
                       "Cloud-only settings should not offer the legacy OTP/password account.")
        XCTAssertFalse(app.buttons["morde.settings.cloudTasks"].exists,
                       "Cloud-only settings should not offer another Morde sign-in shell.")
        XCTAssertFalse(app.buttons["morde.settings.executionControl"].exists,
                       "Cloud-only settings should not show the legacy cloud execution control.")
        XCTAssertFalse(app.buttons["morde.settings.hands"].exists,
                       "Cloud-only settings should not show the iPhone-local Hands explanation.")
        XCTAssertFalse(app.buttons["morde.settings.legacyComputers"].exists,
                       "Cloud-only settings should use Morde Resources instead of the legacy Hub devices list.")
        for legacySetting in ["System files", "Skills", "Connectors", "Environment Variables", "iCloud Sync", "Network"] {
            XCTAssertFalse(app.buttons[legacySetting].exists,
                           "Morde should not expose an unported local Agent setting: \(legacySetting).")
        }
        signOutFromSettings(app)
        app.terminate()
    }

    func testChatAttachmentFixtureUploadsJPEGAndPDFAndSendsAllAttachmentIDs() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localMainChatFixture",
                              "-morde.localChatAttachmentFixture", "-morde.localChatAttachmentSendRetryFixture",
                              "-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        if !app.textFields["morde.auth.username"].waitForExistence(timeout: 2) {
            signOutFromSettings(app)
            app.terminate()
            app.launch()
        }
        try signInToFixtureAccount("chat-attachment-\(UUID().uuidString.prefix(8))", in: app)

        let pending = app.descendants(matching: .any)
            .matching(identifier: "morde.chat.attachments.pending").firstMatch
        XCTAssertTrue(pending.waitForExistence(timeout: 30), "The local picker fixture should prepare both files.")
        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].waitForExistence(timeout: 10))
        XCTAssertTrue(app.staticTexts["fixture.pdf"].exists)

        let composer = app.descendants(matching: .any).matching(identifier: "morde.chat.composer").firstMatch
        XCTAssertTrue(composer.waitForExistence(timeout: 10))
        composer.tap()
        composer.typeText("Summarize the attached image and PDF")
        let send = app.buttons["morde.task.send"]
        let ready = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isEnabled == true"), object: send)
        wait(for: [ready], timeout: 10)
        send.tap()

        let message = app.descendants(matching: .any)
            .matching(identifier: "morde.chat.message.m9999999").firstMatch
        XCTAssertTrue(app.staticTexts["Send status unknown"].waitForExistence(timeout: 15))
        let retry = app.buttons["Retry"].firstMatch
        XCTAssertTrue(retry.waitForExistence(timeout: 10))
        retry.tap()
        XCTAssertTrue(message.waitForExistence(timeout: 20),
                      "The retry must reconcile the accepted message and then link the same attachment placeholders.")
        XCTAssertTrue(app.staticTexts["Sent"].waitForExistence(timeout: 30),
                      "Personal PDF derivatives must link with the original so attachment processing does not stay pending.")
        XCTAssertTrue(app.staticTexts["Summarize the attached image and PDF"].exists)
        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].exists)
        XCTAssertTrue(app.staticTexts["fixture.pdf"].exists)
        // Internal derivatives stay hidden in the chat transcript; Sent is shown only after every
        // declared original and derived attachment slot has uploaded and linked successfully.

        let imageAttachment = app.buttons.matching(NSPredicate(format: "label CONTAINS[c] %@", "fixture-image.jpg"))
            .firstMatch
        XCTAssertTrue(imageAttachment.waitForExistence(timeout: 10))
        imageAttachment.tap()
        XCTAssertTrue(app.descendants(matching: .any)
            .matching(identifier: "morde.attachment.preview.image").firstMatch.waitForExistence(timeout: 15),
                      "The image attachment should open the read-only image preview after its Content-Length and ETag verify.")
        XCTAssertFalse(app.buttons["Share"].exists)
        XCTAssertFalse(app.buttons["Save to Files"].exists)
        app.buttons["morde.attachment.preview.back"].tap()

        let pdfAttachment = app.buttons.matching(NSPredicate(format: "label CONTAINS[c] %@", "fixture.pdf"))
            .firstMatch
        XCTAssertTrue(pdfAttachment.waitForExistence(timeout: 10))
        pdfAttachment.tap()
        XCTAssertTrue(app.descendants(matching: .any)
            .matching(identifier: "morde.attachment.preview.pdf").firstMatch.waitForExistence(timeout: 15),
                      "The PDF attachment should open without a system share or download action.")
        XCTAssertFalse(app.buttons["Share"].exists)
        XCTAssertFalse(app.buttons["Save to Files"].exists)
        app.buttons["morde.attachment.preview.back"].tap()

        let resources = app.buttons["morde.shell.tab.resources"]
        XCTAssertTrue(resources.waitForExistence(timeout: 10))
        resources.tap()
        app.buttons["morde.shell.drawer"].tap()
        app.buttons["morde.shell.drawer.library"].tap()
        let myUploads = app.buttons["morde.library.source.myUploads"]
        XCTAssertTrue(myUploads.waitForExistence(timeout: 15))
        myUploads.tap()
        let libraryImage = app.buttons.matching(NSPredicate(format: "label CONTAINS[c] %@", "fixture-image.jpg"))
            .firstMatch
        XCTAssertTrue(libraryImage.waitForExistence(timeout: 15),
                      "The library should reuse the existing personal file row as the preview target.")
        libraryImage.tap()
        XCTAssertTrue(app.descendants(matching: .any)
            .matching(identifier: "morde.attachment.preview.image").firstMatch.waitForExistence(timeout: 15))
        app.buttons["morde.attachment.preview.back"].tap()
        app.terminate()
    }

    func testChatAttachmentPreviewRejectsDownloadFailuresAndBadIntegrityHeaders() throws {
        for fixtureArgument in ["-morde.localChatAttachmentDownloadFailureFixture",
                                "-morde.localChatAttachmentDownloadBadLengthFixture",
                                "-morde.localChatAttachmentDownloadBadETagFixture"] {
            let app = try launchChatAttachmentMessageFixture(downloadFixtureArgument: fixtureArgument)
            let attachment = app.buttons.matching(NSPredicate(
                format: "identifier BEGINSWITH %@", "morde.chat.message.attachment.pa_"
            )).firstMatch
            XCTAssertTrue(attachment.waitForExistence(timeout: 10))
            attachment.tap()
            XCTAssertTrue(app.alerts.firstMatch.waitForExistence(timeout: 15),
                          "The fixture response must not show a preview when the request or integrity metadata is invalid.")
            XCTAssertFalse(app.buttons["morde.attachment.preview.back"].exists)
            app.alerts.firstMatch.buttons["确定"].tap()
            app.terminate()
        }
    }

    func testChatAttachmentPickerCanBeCancelledFromLongPressMenu() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localMainChatFixture",
                              "-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        if !app.textFields["morde.auth.username"].waitForExistence(timeout: 2) {
            signOutFromSettings(app)
            app.terminate()
            app.launch()
        }
        try signInToFixtureAccount("chat-cancel-\(UUID().uuidString.prefix(8))", in: app)
        openChatActions(in: app)
        app.buttons["morde.shell.attachments"].tap()

        let cancel = app.buttons["Cancel"]
        XCTAssertTrue(cancel.waitForExistence(timeout: 15), "The approved attachment menu entry should open the iOS file picker.")
        cancel.tap()
        XCTAssertFalse(app.descendants(matching: .any)
            .matching(identifier: "morde.chat.attachments.pending").firstMatch.exists,
                       "Cancelling file selection should leave the chat draft unchanged.")
        app.terminate()
    }

    func testChatAttachmentUploadFailureIsSurfacedWithoutPendingChip() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localMainChatFixture",
                              "-morde.localChatAttachmentFixture", "-morde.localChatAttachmentUploadFailureFixture",
                              "-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        if !app.textFields["morde.auth.username"].waitForExistence(timeout: 2) {
            signOutFromSettings(app)
            app.terminate()
            app.launch()
        }
        try signInToFixtureAccount("chat-upload-failure-\(UUID().uuidString.prefix(8))", in: app)

        let pending = app.descendants(matching: .any)
            .matching(identifier: "morde.chat.attachments.pending").firstMatch
        XCTAssertTrue(pending.waitForExistence(timeout: 30), "The local attachment picker should populate the draft.")
        let composer = app.descendants(matching: .any)
            .matching(identifier: "morde.chat.composer").firstMatch
        XCTAssertTrue(composer.waitForExistence(timeout: 10))
        composer.tap()
        composer.typeText("Send with a failing attachment upload")
        let send = app.buttons["morde.task.send"]
        let sendReady = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isEnabled == true"), object: send)
        XCTAssertEqual(XCTWaiter.wait(for: [sendReady], timeout: 10), .completed)
        send.tap()

        let message = app.descendants(matching: .any)
            .matching(identifier: "morde.chat.message.m9999999").firstMatch
        XCTAssertTrue(message.waitForExistence(timeout: 20), "The text is accepted before attachment processing finishes.")
        let uploadError = app.staticTexts.matching(
            NSPredicate(format: "label CONTAINS %@", "Synthetic attachment upload failed")).firstMatch
        XCTAssertTrue(uploadError.waitForExistence(timeout: 30), "The failed upload should remain visible on its message.")
        XCTAssertTrue(app.buttons["Retry"].firstMatch.exists, "The failed attachment batch should be safely retryable.")
        XCTAssertFalse(app.descendants(matching: .any)
            .matching(identifier: "morde.chat.attachments.pending").firstMatch.exists,
                       "After send, attachments belong to the queued message rather than the composer draft.")
        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].exists)
        XCTAssertTrue(app.staticTexts["fixture.pdf"].exists)
        app.terminate()
    }

    func testPendingChatAttachmentsDoNotLeakAcrossAccounts() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localAccountIsolationFixture",
                              "-morde.localChatAttachmentFixture",
                              "-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        if !app.textFields["morde.auth.username"].waitForExistence(timeout: 2) {
            signOutFromSettings(app)
            app.terminate()
            app.launch()
        }
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("account-a", in: app)
        let pending = app.descendants(matching: .any)
            .matching(identifier: "morde.chat.attachments.pending").firstMatch
        XCTAssertTrue(pending.waitForExistence(timeout: 30), "Account A should load its own local attachment draft.")
        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].exists)
        XCTAssertTrue(app.staticTexts["fixture.pdf"].exists)

        signOutFromSettings(app)
        app.terminate()
        app.launch()
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("account-b", in: app)
        XCTAssertTrue(app.staticTexts["account-b-private-message"].waitForExistence(timeout: 15))
        XCTAssertFalse(pending.exists, "Account A's uploaded attachments must not appear in Account B's draft.")
        XCTAssertFalse(app.staticTexts["fixture-image.jpg"].exists)
        XCTAssertFalse(app.staticTexts["fixture.pdf"].exists)
        app.terminate()
    }

    func testConversationHistoryLoadsEarlierPage() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localMainChatFixture",
                              "-morde.localConversationHistoryPaginationFixture",
                              "-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        signOutFromSettings(app)
        app.terminate()
        app.launch()

        try signInToFixtureAccount("history-user", in: app)

        let latestMessage = app.staticTexts["History message 040"]
        XCTAssertTrue(latestMessage.waitForExistence(timeout: 15), "The latest page should be displayed after recovery.")
        let transcript = app.scrollViews["morde.chat.transcript"]
        let loadEarlier = app.buttons["morde.chat.messages.loadEarlier"]
        for _ in 0..<10 where !loadEarlier.isHittable { transcript.swipeDown() }
        XCTAssertTrue(loadEarlier.waitForExistence(timeout: 10), "The response next_cursor should expose an earlier-page action.")
        loadEarlier.tap()

        let earliestMessage = app.staticTexts["History message 001"]
        for _ in 0..<10 where !earliestMessage.isHittable { transcript.swipeDown() }
        XCTAssertTrue(earliestMessage.isHittable,
                      "Loading older history should prepend the first page without snapping back to the latest message.")
        let finishedLoading = XCTNSPredicateExpectation(predicate: NSPredicate(format: "exists == false"),
                                                         object: loadEarlier)
        XCTAssertEqual(XCTWaiter.wait(for: [finishedLoading], timeout: 15), .completed,
                       "The final page should consume the cursor and remove the earlier-page action.")
        signOutFromSettings(app)
    }

    func testLocalHistoryArchiveHasNoDrawerEntry() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localLegacyHistoryFixture",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        openMordeFromCurrentRoot(app)
        if app.textFields["morde.auth.username"].waitForExistence(timeout: 3) {
            try signInToFixtureAccount("local-history-user", in: app)
        }

        app.buttons["morde.shell.drawer"].tap()
        XCTAssertFalse(app.buttons["morde.shell.localHistory"].exists,
                       "Archived local conversations stay stored but are not a design drawer destination.")
        XCTAssertTrue(app.buttons["morde.shell.drawer.friends"].exists)
        XCTAssertTrue(app.buttons["morde.shell.drawer.library"].exists)
        signOutFromSettings(app)
        app.terminate()
    }

    func testPersonalModelCanSendWithoutAdministratorDefaultModel() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localMainChatFixture",
                              "-morde.localNoAdminModelFixture",
                              "-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2) {
            signOutFromSettings(app)
            app.terminate()
            app.launch()
        }
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("no-admin-model-user", in: app)

        let composer = app.descendants(matching: .any).matching(identifier: "morde.chat.composer").firstMatch
        let send = app.buttons["morde.task.send"]
        XCTAssertTrue(composer.waitForExistence(timeout: 15))
        composer.tap()
        composer.typeText("A personal model can be used")
        let enabled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isEnabled == true"), object: send)
        wait(for: [enabled], timeout: 10)
        XCTAssertTrue(send.isEnabled, "A checked personal model should be usable without a platform default.")
        app.terminate()
    }

    func testLocalHistoryImportRequiresConfirmationAndSendsOnlyUserAssistantText() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localLegacyHistoryFixture",
                              "-morde.localHistoryImportFixture",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2) {
            signOutFromSettings(app)
        }
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("history-import-user", in: app)

        let localHistory = app.buttons["morde.localHistory.open"]
        for _ in 0..<6 where !localHistory.isHittable { app.swipeDown() }
        XCTAssertTrue(localHistory.waitForExistence(timeout: 10))
        localHistory.tap()
        let archivedConversation = app.staticTexts["Local import fixture conversation"]
        XCTAssertTrue(archivedConversation.waitForExistence(timeout: 10))
        archivedConversation.tap()
        XCTAssertTrue(app.staticTexts["Local import message"].waitForExistence(timeout: 10))
        XCTAssertTrue(app.staticTexts["Local import reply"].waitForExistence(timeout: 10))

        let importButton = app.buttons["morde.localHistory.import"]
        XCTAssertTrue(importButton.waitForExistence(timeout: 10))
        importButton.tap()
        let confirmImport = app.buttons["morde.localHistory.confirmImport"]
        XCTAssertTrue(confirmImport.waitForExistence(timeout: 5))
        app.buttons["Cancel"].tap()
        XCTAssertFalse(app.alerts["Morde history import"].exists)

        importButton.tap()
        XCTAssertTrue(confirmImport.waitForExistence(timeout: 5))
        confirmImport.tap()
        let importResult = app.alerts["Morde history import"]
        XCTAssertTrue(importResult.waitForExistence(timeout: 10))
        XCTAssertTrue(importResult.staticTexts[
            "Imported 2 text messages to Morde. The local conversation remains unchanged."
        ].exists)
        importResult.buttons["OK"].tap()
        XCTAssertTrue(app.staticTexts["Local import message"].exists)
        XCTAssertTrue(app.staticTexts["Local import reply"].exists)
        signOutFromSettings(app)
        app.terminate()
    }

    func testTaskMediaPreviewRequiresSaveDecisionAndConfirmsWorkspaceSave() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskSSEFixture",
                              "-morde.localTaskMediaFixture",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2) {
            signOutFromSettings(app)
        }
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("media-fixture-user", in: app)

        let conversation = app.buttons.matching(NSPredicate(format: "label CONTAINS %@", "Local SSE fixture")).firstMatch
        XCTAssertTrue(conversation.waitForExistence(timeout: 10))
        conversation.tap()
        let preview = app.buttons["morde.task.media.preview"]
        for _ in 0..<6 where !preview.isHittable { app.swipeUp() }
        XCTAssertTrue(preview.waitForExistence(timeout: 15))
        preview.tap()

        let save = app.buttons["morde.task.media.save"]
        XCTAssertTrue(save.waitForExistence(timeout: 15))
        XCTAssertTrue(app.buttons["morde.task.media.discard"].exists)
        save.tap()
        XCTAssertTrue(app.staticTexts["Generated media saved in Workspace"].waitForExistence(timeout: 15))

        signOutFromSettings(app)
        app.terminate()
    }

    func testTaskDetailMediaApprovalUsesTheSafePreviewFlow() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskSSEFixture",
                              "-morde.localTaskMediaFixture",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        try signInToFixtureAccountIfNeeded("task-page-media-fixture-user", in: app)

        let tasksTab = app.buttons["morde.shell.tab.tasks"]
        XCTAssertTrue(tasksTab.waitForExistence(timeout: 10))
        tasksTab.tap()
        let taskRow = app.buttons["morde.tasks.row.q1234567"]
        XCTAssertTrue(taskRow.waitForExistence(timeout: 10))
        taskRow.tap()

        let preview = app.buttons["morde.task.media.preview"]
        XCTAssertTrue(preview.waitForExistence(timeout: 10))
        preview.tap()
        let save = app.buttons["morde.task.media.save"]
        XCTAssertTrue(save.waitForExistence(timeout: 15))
        save.tap()
        XCTAssertTrue(app.staticTexts["Generated media saved in Workspace"].waitForExistence(timeout: 15))
        app.terminate()
    }

    func testTaskMediaApprovalStaysClosedWithoutCapability() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskSSEFixture",
                              "-morde.localTaskMediaFixture", "-morde.localTaskMediaCapabilityDisabled",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        try signInToFixtureAccountIfNeeded("media-disabled-fixture-user", in: app)

        let conversation = app.buttons.matching(NSPredicate(format: "label CONTAINS %@", "Local SSE fixture")).firstMatch
        XCTAssertTrue(conversation.waitForExistence(timeout: 10))
        conversation.tap()
        let unavailable = app.staticTexts["morde.task.media.unavailable"]
        for _ in 0..<6 where !unavailable.isHittable { app.swipeUp() }
        XCTAssertTrue(unavailable.waitForExistence(timeout: 10))
        XCTAssertFalse(app.buttons["morde.task.media.preview"].exists)
        XCTAssertFalse(app.buttons["morde.task.media.save"].exists)

        let discard = app.buttons["morde.task.media.discard"]
        XCTAssertTrue(discard.waitForExistence(timeout: 10))
        discard.tap()
        XCTAssertTrue(app.staticTexts["Generated media discarded"].waitForExistence(timeout: 15))
        app.terminate()
    }

    func testTaskSSERefreshesSnapshotOnSnapshotRequired() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskSSEFixture",
                              "-morde.localTaskSnapshotRequiredFixture",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        try signInToFixtureAccountIfNeeded("snapshot-fixture-user", in: app)
        XCTAssertTrue(app.staticTexts["Local snapshot-required fixture reply"].waitForExistence(timeout: 15))
        app.terminate()
    }

    func testTaskSSEReconnectsAfterTemporaryFailure() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskSSEFixture",
                              "-morde.localTaskDisconnectRecoveryFixture",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        try signInToFixtureAccountIfNeeded("disconnect-fixture-user", in: app)
        XCTAssertTrue(app.staticTexts["Local disconnect recovery fixture reply"].waitForExistence(timeout: 20))
        app.terminate()
    }

    func testAcceptedTaskIsRestoredAfterAppRelaunch() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskSSEFixture",
                              "-morde.localTaskRelaunchFixture",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        if app.buttons["morde.shell.drawer"].waitForExistence(timeout: 2) {
            signOutFromSettings(app)
        }
        openMordeFromCurrentRoot(app)
        try signInToFixtureAccount("relaunch-fixture-user", in: app)
        let runningTask = app.staticTexts.matching(identifier: "morde.chat.taskSummary")
            .matching(NSPredicate(format: "label == %@", "运行中")).firstMatch
        XCTAssertTrue(runningTask.waitForExistence(timeout: 15))

        app.terminate()
        app.launch()
        XCTAssertTrue(app.buttons["morde.shell.drawer"].waitForExistence(timeout: 15))
        XCTAssertTrue(runningTask.waitForExistence(timeout: 15))
        signOutFromSettings(app)
        app.terminate()
    }

    func testApprovalConflictRefreshesTaskAndHidesStaleControls() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskSSEFixture",
                              "-morde.localTaskApprovalConflictFixture",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        try signInToFixtureAccountIfNeeded("approval-conflict-user", in: app)

        app.buttons["morde.shell.tab.tasks"].tap()
        let taskRow = app.buttons["morde.tasks.row.q1234567"]
        XCTAssertTrue(taskRow.waitForExistence(timeout: 15))
        taskRow.tap()
        let approve = app.buttons["morde.task.approval.approve.r1234567"]
        scrollUntilVisible(approve, in: app)
        XCTAssertTrue(approve.waitForExistence(timeout: 15))
        approve.tap()
        let conflict = app.alerts.firstMatch
        XCTAssertTrue(conflict.waitForExistence(timeout: 10))
        XCTAssertTrue(conflict.staticTexts["任务状态已变化，已刷新最新状态。请核对后再操作。"].exists)
        conflict.buttons["确定"].tap()
        let completedTask = app.staticTexts["morde.task.status"]
        for _ in 0..<5 where !completedTask.exists { app.collectionViews.firstMatch.swipeDown() }
        XCTAssertTrue(completedTask.waitForExistence(timeout: 10))
        XCTAssertTrue(completedTask.label.contains("已完成"))
        XCTAssertFalse(app.buttons["morde.task.approval.approve.r1234567"].exists)
        XCTAssertFalse(app.buttons["morde.task.approval.reject.r1234567"].exists)
        app.terminate()
    }

    func testUnknownTaskStatusIsVisibleWithoutTaskControls() throws {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskSSEFixture",
                              "-morde.localUnknownTaskStatusFixture",
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        try signInToFixtureAccountIfNeeded("unknown-status-user", in: app)

        app.buttons["morde.shell.tab.tasks"].tap()
        let taskRow = app.buttons["morde.tasks.row.q1234567"]
        XCTAssertTrue(taskRow.waitForExistence(timeout: 15))
        taskRow.tap()
        let unknownStatus = app.staticTexts["morde.task.status"]
        XCTAssertTrue(unknownStatus.waitForExistence(timeout: 15))
        XCTAssertTrue(unknownStatus.label.contains("状态未知"))
        XCTAssertEqual(app.buttons.matching(NSPredicate(format: "identifier BEGINSWITH %@", "morde.task.action.")).count, 0)
        app.terminate()
    }

    private func assertLoginMessage(_ app: XCUIApplication, statusArgument: String,
                                   username: String, message: String) {
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub", statusArgument,
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        openMordeFromCurrentRoot(app)
        XCTAssertTrue(app.textFields["morde.auth.username"].waitForExistence(timeout: 10))
        app.textFields["morde.auth.username"].tap()
        app.textFields["morde.auth.username"].typeText(username)
        app.secureTextFields["morde.auth.password"].tap()
        app.secureTextFields["morde.auth.password"].typeText("LocalStatusTest!2468")
        app.buttons["morde.auth.submit"].tap()
        let alert = app.alerts.firstMatch
        XCTAssertTrue(alert.waitForExistence(timeout: 10))
        XCTAssertTrue(alert.staticTexts[message].exists)
        alert.buttons["确定"].tap()
        app.terminate()
    }

    private func dismissStrongPasswordPromptIfPresent(in app: XCUIApplication) {
        // iOS 27 presents AutoFill over new-password fields and consumes UI-test typing.
        if app.buttons["GenerateStrongPasswordButton"].waitForExistence(timeout: 1) {
            app.buttons["xmark"].tap()
        }
    }

    private func launchChatAttachmentMessageFixture(downloadFixtureArgument: String) throws -> XCUIApplication {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localMainChatFixture",
                              "-morde.localChatAttachmentFixture", downloadFixtureArgument,
                              "-AppleLanguages", "(en)", "-AppleLocale", "en_US"]
        app.launch()
        if !app.textFields["morde.auth.username"].waitForExistence(timeout: 2) {
            signOutFromSettings(app)
            app.terminate()
            app.launch()
        }
        try signInToFixtureAccount("chat-preview-\(UUID().uuidString.prefix(8))", in: app)

        let pending = app.descendants(matching: .any)
            .matching(identifier: "morde.chat.attachments.pending").firstMatch
        XCTAssertTrue(pending.waitForExistence(timeout: 30))
        let composer = app.descendants(matching: .any).matching(identifier: "morde.chat.composer").firstMatch
        XCTAssertTrue(composer.waitForExistence(timeout: 10))
        composer.tap()
        composer.typeText("Open the synthetic attachment")
        let send = app.buttons["morde.task.send"]
        let ready = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isEnabled == true"), object: send)
        wait(for: [ready], timeout: 10)
        send.tap()
        XCTAssertTrue(app.descendants(matching: .any)
            .matching(identifier: "morde.chat.message.m9999999").firstMatch.waitForExistence(timeout: 20))
        return app
    }

    private func signInToFixtureAccount(_ username: String, in app: XCUIApplication,
                                        password: String = "LocalAccountFixture!2468",
                                        expectedAlertMessage: String? = nil) throws {
        dismissLocalFixtureStartupError(in: app)
        let usernameField = app.textFields["morde.auth.username"]
        let voiceOverTest = app.launchArguments.contains("-morde.voiceOverUITest")
        _ = try XCTUnwrap(usernameField.waitForExistence(timeout: 10) ? usernameField : nil,
                          "The fixture sign-in form should appear before entering credentials.")
        if !voiceOverTest && !usernameField.isHittable {
            openMordeFromCurrentRoot(app)
        }
        if !voiceOverTest {
            let usernameReady = XCTNSPredicateExpectation(
                predicate: NSPredicate(format: "exists == true AND hittable == true"), object: usernameField)
            let ready = XCTWaiter.wait(for: [usernameReady], timeout: 5) == .completed
            _ = try XCTUnwrap(ready ? usernameField : nil,
                              "The fixture sign-in form should be hittable before entering credentials.")
        }
        if voiceOverTest { usernameField.doubleTap() } else { usernameField.tap() }
        let existingUsername = usernameField.value as? String ?? ""
        if !existingUsername.isEmpty && existingUsername != usernameField.placeholderValue {
            usernameField.typeText(String(repeating: XCUIKeyboardKey.delete.rawValue,
                                          count: existingUsername.count))
        }
        if !voiceOverTest { usernameField.tap() }
        usernameField.typeText(username)
        let usernameEntered = expectation(for: NSPredicate(format: "value == %@", username), evaluatedWith: usernameField)
        wait(for: [usernameEntered], timeout: 5)
        _ = try XCTUnwrap((usernameField.value as? String) == username ? usernameField : nil,
                          "The fixture username should be entered before submitting.")
        app.secureTextFields["morde.auth.password"].tap()
        app.secureTextFields["morde.auth.password"].typeText(password)
        app.buttons["morde.auth.submit"].tap()
        let loginFinished = XCTNSPredicateExpectation(predicate: NSPredicate(format: "exists == false"),
                                                       object: usernameField)
        let alert = app.alerts.firstMatch
        let expectedAlertShown: Bool
        if let expectedAlertMessage {
            expectedAlertShown = alert.waitForExistence(timeout: 20) && alert.staticTexts.matching(
                NSPredicate(format: "label CONTAINS %@", expectedAlertMessage)).firstMatch.exists
        } else {
            expectedAlertShown = false
        }
        let alertIsExpected = expectedAlertMessage == nil ? !alert.exists : expectedAlertShown
        let signedIn = XCTWaiter.wait(for: [loginFinished], timeout: 15) == .completed
            && app.buttons["morde.shell.drawer"].waitForExistence(timeout: 5) && alertIsExpected
        if !signedIn {
            let screenshot = XCTAttachment(screenshot: app.screenshot())
            screenshot.name = "Local fixture sign-in failure"
            screenshot.lifetime = .keepAlways
            add(screenshot)
        }
        _ = try XCTUnwrap(signedIn ? app.buttons["morde.shell.drawer"] : nil,
                          "The fixture sign-in must complete before page assertions run.")
        dismissKeyboard(in: app)
    }

    private func dismissLocalFixtureStartupError(in app: XCUIApplication) {
        let startupError = app.alerts.matching(
            NSPredicate(format: "label == %@ OR label == %@", "请求失败", "Request failed")
        ).firstMatch
        guard startupError.waitForExistence(timeout: 1) else { return }
        let confirm = startupError.buttons.matching(
            NSPredicate(format: "label == %@ OR label == %@", "确定", "OK")
        ).firstMatch
        if confirm.exists { confirm.tap() }
    }

    private func signInToFixtureAccountIfNeeded(_ username: String, in app: XCUIApplication) throws {
        dismissLocalFixtureStartupError(in: app)
        let usernameField = app.textFields["morde.auth.username"]
        if usernameField.waitForExistence(timeout: 5) {
            openMordeFromCurrentRoot(app)
            try signInToFixtureAccount(username, in: app)
        }
        let signedIn = app.buttons["morde.shell.drawer"].waitForExistence(timeout: 10)
            && !usernameField.exists && !app.alerts.firstMatch.exists
        _ = try XCTUnwrap(signedIn ? true : nil,
                          "Fixture setup must reach an authenticated Morde shell before testing pages.")
    }

    private func launchTaskActionFixture(extraArguments: [String]) throws -> XCUIApplication {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713",
                              "-morde.localRegistrationStub", "-morde.localRegistrationApproved",
                              "-morde.localTaskPagesFixture"] + extraArguments +
            ["-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        try signInToFixtureAccountIfNeeded("task-action-fixture", in: app)
        _ = try XCTUnwrap(app.buttons["morde.shell.tab.tasks"].waitForExistence(timeout: 15) ? app : nil,
                          "The authenticated fixture shell should expose the task tab.")
        return app
    }

    private func relaunchTaskActionFixture(_ app: XCUIApplication) throws {
        app.terminate()
        app.launch()
        try signInToFixtureAccountIfNeeded("task-action-fixture", in: app)
        _ = try XCTUnwrap(app.buttons["morde.shell.tab.chat"].waitForExistence(timeout: 10) ? app : nil,
                          "The authenticated fixture shell should expose the chat tab after relaunch.")
        _ = try XCTUnwrap(app.buttons["morde.shell.tab.tasks"].waitForExistence(timeout: 10) ? app : nil,
                          "The authenticated fixture shell should expose the task tab after relaunch.")
    }

    private func openFirstTaskPage(in app: XCUIApplication, taskID: String = "q0000001",
                                   waitForObjective: Bool = true) {
        openTasksRootTab(app)
        let firstTask = app.buttons["morde.tasks.row.\(taskID)"]
        XCTAssertTrue(firstTask.waitForExistence(timeout: 10))
        firstTask.tap()
        if waitForObjective { XCTAssertTrue(app.staticTexts["morde.task.objective"].waitForExistence(timeout: 10)) }
    }

    private func auditAccessibility(_ app: XCUIApplication, page: String) throws {
        var findings: [String] = []
        defer {
            if !findings.isEmpty {
                let attachment = XCTAttachment(string: findings.joined(separator: "\n\n"))
                attachment.name = "Accessibility audit elements: \(page)"
                attachment.lifetime = .keepAlways
                add(attachment)
            }
        }
        try app.performAccessibilityAudit { issue in
            let element = issue.element
            findings.append([
                "page: \(page)",
                "type: \(String(describing: issue.auditType))",
                "summary: \(issue.compactDescription)",
                "detail: \(issue.detailedDescription)",
                "element type: \(element.map { String(describing: $0.elementType) } ?? "none")",
                "identifier: \(element?.identifier ?? "")",
                "label: \(element?.label ?? "")",
                "value: \(element.map { String(describing: $0.value) } ?? "nil")",
                "frame: \(element.map { String(describing: $0.frame) } ?? "nil")",
                "debug: \(element?.debugDescription ?? "")",
            ].joined(separator: "\n"))
            return false
        }
    }

    private func signOutFromSettings(_ app: XCUIApplication) {
        dismissKeyboard(in: app)
        let usernameField = app.textFields["morde.auth.username"]
        if usernameField.waitForExistence(timeout: 2) {
            let drawer = app.buttons["morde.shell.drawer"]
            if !usernameField.isHittable && drawer.isHittable && app.buttons["morde.shell.settings"].exists {
                drawer.tap()
            }
            return
        }
        let signOut = app.buttons["morde.account.signOut"]
        let accountSettings = app.buttons["morde.settings.account"]
        if !signOut.exists {
            if !accountSettings.exists {
                let drawer = app.buttons["morde.shell.drawer"]
                if !app.buttons["morde.shell.settings"].exists {
                    XCTAssertTrue(drawer.waitForExistence(timeout: 5))
                    drawer.tap()
                }
                let settings = app.buttons["morde.shell.settings"]
                XCTAssertTrue(settings.waitForExistence(timeout: 5))
                let settingsVisible = expectation(for: NSPredicate(format: "isHittable == true"), evaluatedWith: settings)
                wait(for: [settingsVisible], timeout: 5)
                settings.tap()
                XCTAssertTrue(accountSettings.waitForExistence(timeout: 5))
            }
            if accountSettings.exists { accountSettings.tap() }
        }
        scrollUntilVisible(signOut, in: app)
        if signOut.waitForExistence(timeout: 3) {
            signOut.tap()
            let confirmation = app.descendants(matching: .any)
                .matching(identifier: "morde.account.signOut.confirm").firstMatch
            XCTAssertTrue(confirmation.waitForExistence(timeout: 5))
            confirmation.tap()
            let manageInstallations = app.buttons["morde.account.manageInstallations"]
            let signedOut = expectation(for: NSPredicate(format: "exists == false"),
                                        evaluatedWith: manageInstallations)
            wait(for: [signedOut], timeout: 10)
        } else {
            let snapshot = XCTAttachment(string: app.debugDescription)
            snapshot.name = "Morde account page when no sign-out action is visible"
            snapshot.lifetime = .keepAlways
            add(snapshot)
        }
        if !usernameField.isHittable, app.navigationBars.buttons.firstMatch.isHittable {
            app.navigationBars.buttons.firstMatch.tap()
        }
        let dismissSettings = app.buttons["chevron.left"]
        if !usernameField.isHittable, dismissSettings.isHittable { dismissSettings.tap() }
    }

    private func openMordeFromCurrentRoot(_ app: XCUIApplication) {
        let changedAccountAlert = app.alerts.firstMatch
        let changedAccountMessage = changedAccountAlert.staticTexts.matching(
            NSPredicate(format: "label CONTAINS %@", "账号已变化")
        ).firstMatch
        if changedAccountMessage.waitForExistence(timeout: 1) {
            changedAccountAlert.buttons["确定"].tap()
        }

        // nanoMuse: Fresh test simulators show a one-time provider-key migration notice over the sign-in form.
        let migrationNotice = app.alerts.matching(NSPredicate(format: "label CONTAINS[c] %@", "API")).firstMatch
        if migrationNotice.waitForExistence(timeout: 1) {
            let acknowledge = migrationNotice.buttons.matching(
                NSPredicate(format: "label == %@ OR label == %@", "Got it", "知道了")
            ).firstMatch
            if acknowledge.exists { acknowledge.tap() }
        }

        let usernameField = app.textFields["morde.auth.username"]
        if usernameField.waitForExistence(timeout: 2), usernameField.isHittable { return }
        let rootDrawer = app.buttons["morde.shell.drawer"]
        if rootDrawer.waitForExistence(timeout: 2), rootDrawer.isHittable, !usernameField.exists { return }
        let cloudTasks = app.buttons.matching(NSPredicate(
            format: "label CONTAINS[c] %@ OR label CONTAINS %@", "cloud tasks", "云端任务"
        )).firstMatch
        if cloudTasks.waitForExistence(timeout: 2), cloudTasks.isHittable {
            cloudTasks.tap()
            XCTAssertTrue(usernameField.waitForExistence(timeout: 10))
            return
        }
        let drawer = rootDrawer
        let drawerSettings = app.buttons["morde.shell.settings"]
        if usernameField.exists && !usernameField.isHittable && drawer.isHittable && drawerSettings.exists {
            drawer.tap()
        }
        for _ in 0..<3 {
            let usernameReady = XCTNSPredicateExpectation(
                predicate: NSPredicate(format: "exists == true AND hittable == true"), object: usernameField)
            if XCTWaiter.wait(for: [usernameReady], timeout: 2) == .completed { return }
            let navigationAction = app.navigationBars.buttons.firstMatch
            if navigationAction.exists && navigationAction.isHittable {
                navigationAction.tap()
                continue
            }
            let close = app.buttons.matching(NSPredicate(format: "label == %@ OR label == %@", "关闭", "Close")).firstMatch
            if close.exists && close.isHittable {
                close.tap()
                continue
            }
            let chatTab = app.buttons["morde.shell.tab.chat"]
            if chatTab.exists && chatTab.isHittable {
                if !chatTab.isSelected { chatTab.tap() }
                continue
            }
            break
        }
        if drawer.waitForExistence(timeout: 2), drawer.isHittable {
            if !drawerSettings.exists { drawer.tap() }
            let mainChat = app.buttons["morde.shell.mainChat"]
            if mainChat.waitForExistence(timeout: 2), mainChat.isHittable {
                mainChat.tap()
            } else if drawerSettings.exists {
                drawer.tap()
            }
            let usernameReady = XCTNSPredicateExpectation(
                predicate: NSPredicate(format: "exists == true AND hittable == true"), object: usernameField)
            XCTAssertEqual(XCTWaiter.wait(for: [usernameReady], timeout: 5), .completed,
                           "Morde sign-in should be visible after opening chat.")
        } else {
            XCTAssertTrue(usernameField.waitForExistence(timeout: 5), "Morde chat should be the default root.")
        }
    }

    private func openTasksRootTab(_ app: XCUIApplication) {
        dismissKeyboard(in: app)
        let tab = app.buttons["morde.shell.tab.tasks"]
        XCTAssertTrue(tab.waitForExistence(timeout: 5))
        if !tab.isSelected { tab.tap() }
    }

    private func dismissKeyboard(in app: XCUIApplication) {
        guard app.keyboards.firstMatch.exists else { return }
        let transcript = app.descendants(matching: .any).matching(identifier: "morde.chat.transcript").firstMatch
        if transcript.exists && transcript.isHittable {
            transcript.tap()
        } else {
            let returnKey = app.keyboards.buttons["Return"]
            if returnKey.exists && returnKey.isHittable { returnKey.tap() }
        }
    }

    private func openChatActions(in app: XCUIApplication) {
        let actions = app.descendants(matching: .any)
            .matching(identifier: "morde.shell.chatActions").firstMatch
        XCTAssertTrue(actions.waitForExistence(timeout: 10))
        actions.press(forDuration: 1)
    }

    private func scrollUntilVisible(_ element: XCUIElement, in app: XCUIApplication) {
        for _ in 0..<4 where !element.exists { app.swipeUp() }
    }

    private func assertRemovedBoardTabsHidden(_ app: XCUIApplication) {
        let chat = app.buttons["morde.shell.tab.chat"]
        let tasks = app.buttons["morde.shell.tab.tasks"]
        let approvals = app.buttons["morde.shell.tab.approvals"]
        let resources = app.buttons["morde.shell.tab.resources"]
        XCTAssertTrue(chat.waitForExistence(timeout: 10))
        XCTAssertTrue(tasks.exists)
        XCTAssertTrue(approvals.exists)
        XCTAssertTrue(resources.exists)
        for room in ["feed", "ideas", "goals"] {
            XCTAssertFalse(app.buttons["morde.shell.tab.\(room)"].exists)
        }
        resources.tap()
        XCTAssertTrue(resources.isSelected)
        chat.tap()
        XCTAssertTrue(chat.isSelected)
    }
}
