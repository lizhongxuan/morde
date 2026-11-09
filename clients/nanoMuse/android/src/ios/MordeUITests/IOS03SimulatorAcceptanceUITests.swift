import XCTest

final class IOS03SimulatorAcceptanceUITests: XCTestCase {
    func testLoginAndReadWorkspaceResources() throws {
        guard let account = ProcessInfo.processInfo.environment["MORDE_IOS03_TEST_USER"],
              let password = ProcessInfo.processInfo.environment["MORDE_IOS03_TEST_PASSWORD"],
              let serverAddress = ProcessInfo.processInfo.environment["MORDE_IOS03_TEST_SERVER"] else {
            throw XCTSkip("Set local IOS-03 test credentials in the test action environment.")
        }

        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        app.launchArguments = ["-morde.server", ""]
        app.launch()

        let server = app.textFields["https://morde.example"]
        XCTAssertTrue(server.waitForExistence(timeout: 10))
        server.tap()
        server.typeText(serverAddress)

        let accountField = app.textFields["账号"]
        accountField.tap()
        accountField.typeText(account)

        let passwordField = app.secureTextFields["密码"]
        passwordField.tap()
        passwordField.typeText(password)
        app.buttons["登录"].tap()

        let projects = app.tabBars.buttons["项目"]
        guard projects.waitForExistence(timeout: 15) else {
            let alert = app.alerts.firstMatch
            let message = alert.exists ? alert.staticTexts.allElementsBoundByIndex.last?.label ?? "alert" : "no alert"
            XCTFail("Sign-in did not reach the resource tabs: \(message)")
            return
        }
        projects.tap()
        XCTAssertTrue(app.staticTexts["IOS-03 本地合成验收 Workspace"].waitForExistence(timeout: 20))

        let computers = app.tabBars.buttons["电脑"]
        XCTAssertTrue(computers.exists)
        computers.tap()
        XCTAssertTrue(app.staticTexts["标准 1 核 2G"].waitForExistence(timeout: 10))
        XCTAssertTrue(app.staticTexts["已停止 · AI 控制"].exists)
    }

    func testMordeDesignNavigationRoutes() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localTaskPagesFixture", "-morde.localExecutionFixture"])
        let menu = app.buttons["morde.nav.menu"]
        XCTAssertTrue(app.buttons["morde.nav.tab.chat"].isSelected)
        XCTAssertFalse(app.buttons["morde.chat.taskTarget"].exists,
                       "Task target selection must stay in the approved long-press/menu entry.")
        attachScreen(app, named: "MDA-01-chat-bottom-navigation")
        for tab in ["chat", "tasks", "approvals", "resources"] {
            XCTAssertTrue(app.buttons["morde.nav.tab.\(tab)"].exists, "Missing \(tab) tab")
        }
        XCTAssertFalse(app.buttons["项目"].exists)
        XCTAssertFalse(app.buttons["通知"].exists)

        app.buttons["morde.nav.tab.tasks"].tap()
        XCTAssertTrue(app.buttons["morde.tasks.newTask"].waitForExistence(timeout: 15))
        XCTAssertTrue(app.buttons["morde.nav.tab.tasks"].isSelected)
        attachScreen(app, named: "MDA-01-tasks-bottom-navigation")
        menu.tap()
        XCTAssertTrue(app.buttons["morde.nav.drawer.eventChain"].waitForExistence(timeout: 5))
        app.buttons["morde.nav.drawer.eventChain"].tap()
        XCTAssertTrue(app.descendants(matching: .any)["morde.eventChain.empty"].waitForExistence(timeout: 10) ||
                      app.descendants(matching: .any)["morde.eventChain.taskTitle"].waitForExistence(timeout: 10))
        for tab in ["chat", "tasks", "approvals", "resources"] {
            XCTAssertFalse(app.buttons["morde.nav.tab.\(tab)"].isSelected,
                           "The event-chain page does not select a primary tab.")
        }

        menu.tap()
        app.buttons["morde.nav.drawer.friends"].tap()
        XCTAssertTrue(app.descendants(matching: .any)["morde.placeholder.friends"].waitForExistence(timeout: 10))
        for tab in ["chat", "tasks", "approvals", "resources"] {
            XCTAssertFalse(app.buttons["morde.nav.tab.\(tab)"].isSelected,
                           "The friends placeholder does not select a primary tab.")
        }
        XCTAssertFalse(app.buttons["morde.friends.add"].exists,
                       "Friend actions stay hidden until the feature is implemented.")
        attachScreen(app, named: "MDA-09-friends-placeholder")
        menu.tap()
        app.buttons["morde.nav.drawer.toolChats"].tap()
        XCTAssertTrue(app.descendants(matching: .any)["morde.placeholder.toolChats"].waitForExistence(timeout: 10))
        for tab in ["chat", "tasks", "approvals", "resources"] {
            XCTAssertFalse(app.buttons["morde.nav.tab.\(tab)"].isSelected,
                           "The AI tool chat placeholder does not select a primary tab.")
        }
        attachScreen(app, named: "MDA-10-tool-chats-placeholder")
        app.buttons["morde.toolChats.back"].tap()
        XCTAssertTrue(menu.waitForExistence(timeout: 5))

        menu.tap()
        let settings = app.buttons["设置"]
        XCTAssertTrue(settings.waitForExistence(timeout: 5))
        settings.tap()
        for row in ["account", "connectors", "devices", "credentials", "permissions",
                    "approvalRules", "notifications", "appearance", "about"] {
            XCTAssertTrue(app.descendants(matching: .any)["morde.settings.row.\(row)"].waitForExistence(timeout: 5),
                          "Missing settings row \(row)")
        }
        XCTAssertTrue(app.buttons["morde.settings.executionControl"].exists)
        for tab in ["chat", "tasks", "approvals", "resources"] {
            XCTAssertFalse(app.buttons["morde.nav.tab.\(tab)"].isSelected,
                           "The settings page does not select a primary tab.")
        }
        attachScreen(app, named: "MDA-13 settings root")
    }

    func testMordeDrawerGeometrySearchAndConversationPagination() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localTaskPagesFixture"])

        let menu = app.buttons["morde.nav.menu"]
        XCTAssertTrue(menu.waitForExistence(timeout: 10))
        XCTAssertEqual(menu.frame.minX, 22, accuracy: 1)
        XCTAssertEqual(menu.frame.width, 38, accuracy: 1)

        let tabFrames = ["chat", "tasks", "approvals", "resources"].map {
            app.buttons["morde.nav.tab.\($0)"].frame
        }
        XCTAssertEqual(tabFrames.map(\.width).max()! - tabFrames.map(\.width).min()!, 0, accuracy: 1)
        XCTAssertTrue(zip(tabFrames, tabFrames.dropFirst()).allSatisfy { $0.0.minX < $0.1.minX })

        menu.tap()
        let settings = app.buttons["morde.nav.drawer.settings"]
        let newChat = app.buttons["morde.nav.drawer.newChat"]
        let search = app.textFields["morde.nav.drawer.search"]
        let searchContainer = app.descendants(matching: .any)["morde.nav.drawer.search.container"]
        XCTAssertTrue(settings.waitForExistence(timeout: 5))
        XCTAssertTrue(newChat.exists)
        XCTAssertTrue(searchContainer.exists)
        attachScreen(app, named: "MDA-01-drawer-geometry-open")
        XCTAssertEqual(settings.frame.width, 38, accuracy: 1)
        XCTAssertEqual(newChat.frame.width, 38, accuracy: 1)
        XCTAssertEqual(searchContainer.frame.height, 38, accuracy: 1)

        let firstPageConversation = app.buttons["morde.nav.drawer.conversation-c2345678"]
        XCTAssertTrue(firstPageConversation.waitForExistence(timeout: 10))
        XCTAssertEqual(firstPageConversation.images.count, 0,
                       "Side chats use the text-only rows shown in the design.")
        let secondPageConversation = app.buttons["morde.nav.drawer.conversation-c3456789"]
        let drawerScroll = app.scrollViews["morde.nav.drawer.scroll"]
        XCTAssertTrue(drawerScroll.exists)
        for _ in 0..<5 where !secondPageConversation.exists {
            drawerScroll.swipeUp()
        }
        XCTAssertTrue(secondPageConversation.waitForExistence(timeout: 15),
                      "Opening the drawer should load the next conversation page when its last row appears.")
        XCTAssertFalse(app.buttons["morde.nav.drawer.projects"].exists,
                       "The legacy project-management route is not part of the approved design drawer.")
        XCTAssertFalse(app.buttons["项目"].exists)
        XCTAssertFalse(app.buttons["通知"].exists)

        search.tap()
        search.typeText("Needle")
        for _ in 0..<5 where !secondPageConversation.exists {
            drawerScroll.swipeUp()
        }
        XCTAssertTrue(secondPageConversation.waitForExistence(timeout: 5))
        XCTAssertFalse(firstPageConversation.exists, "Search should filter conversation rows as text changes.")
        search.typeText("\n")
        XCTAssertTrue(settings.exists, "Submitting the live filter must not dismiss the drawer.")
    }

    func testMordeTaskListClearsStaleRowsAfterPermissionRevocationAndRefreshRecovers() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesPermissionRecoveryFixture",
        ], loginName: "task-permission-recovery-user", resetExistingAccount: true)
        app.buttons["morde.nav.tab.tasks"].tap()

        let task = app.buttons["morde.tasks.row.q0000001"]
        XCTAssertTrue(task.waitForExistence(timeout: 15))
        XCTAssertTrue(task.label.contains("Permission scoped local task"))

        let taskList = app.scrollViews["morde.tasks.list"]
        XCTAssertTrue(taskList.exists)
        let permissionError = app.descendants(matching: .any)["morde.tasks.loadError"]
        for _ in 0..<15 where !permissionError.exists {
            taskList.swipeUp()
            _ = permissionError.waitForExistence(timeout: 2)
        }
        XCTAssertTrue(permissionError.waitForExistence(timeout: 15),
                      "Loading the next page after the initial rows should surface the revoked permission.")
        XCTAssertFalse(task.exists,
                       "A 403 while loading a later page clears task rows from the account after access is revoked.")
        XCTAssertFalse(app.buttons["Retry loading"].exists,
                       "The task list recovers through pull to refresh without a separate retry button.")
        attachScreen(app, named: "MDA-03 task list after permission revocation")

        let retryStart = taskList.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.05))
        let retryEnd = taskList.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.65))
        retryStart.press(forDuration: 0.05, thenDragTo: retryEnd)
        XCTAssertTrue(task.waitForExistence(timeout: 15),
                      "A later successful refresh should reload tasks after permission is restored.")
        XCTAssertFalse(permissionError.exists)
        attachScreen(app, named: "MDA-03 task list after permission restoration")
    }

    func testMordeTaskDetailShowsRealStepsWithoutBudget() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localTaskPagesFixture"])
        app.buttons["morde.nav.tab.tasks"].tap()

        let task = app.buttons["morde.tasks.row.q0000001"]
        XCTAssertTrue(task.waitForExistence(timeout: 15))
        attachScreen(app, named: "MDA-03-task-list-with-synthetic-running-task")
        task.tap()

        XCTAssertTrue(app.staticTexts["morde.task.objective"].waitForExistence(timeout: 15))
        XCTAssertTrue(app.descendants(matching: .any)["morde.task.step.s0000001"].waitForExistence(timeout: 15))
        XCTAssertTrue(app.descendants(matching: .any)["morde.task.step.s0000003"].waitForExistence(timeout: 15),
                      "The current step should render a current timeline marker.")
        for tab in ["chat", "tasks", "approvals", "resources"] {
            XCTAssertFalse(app.buttons["morde.nav.tab.\(tab)"].isSelected,
                           "Task details do not select a primary tab in the design.")
        }
        let completedEvent = app.descendants(matching: .any)["morde.task.event.1"]
        XCTAssertTrue(completedEvent.waitForExistence(timeout: 15))
        XCTAssertEqual(completedEvent.value as? String, "已完成",
                       "A completed event stays completed even while its Task is still running.")
        XCTAssertTrue(app.buttons["morde.task.action.pause"].exists)
        XCTAssertFalse(app.staticTexts["Private budget field"].exists)
        attachScreen(app, named: "MDA-04 task detail with aligned title and summary spacing")
    }

    func testMordeTaskTerminalDetailsKeepStatusStepsAndApprovedActions() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localTaskPagesStatusFixture"])
        app.buttons["morde.nav.tab.tasks"].tap()

        let completedTask = app.buttons["morde.tasks.row.q0000005"]
        XCTAssertTrue(completedTask.waitForExistence(timeout: 15))
        completedTask.tap()

        let completedStatus = app.staticTexts["morde.task.status"]
        XCTAssertTrue(completedStatus.waitForExistence(timeout: 15))
        XCTAssertTrue(completedStatus.label.contains("已完成"))
        XCTAssertTrue(completedStatus.label.contains("Local completed result"))
        XCTAssertTrue(app.descendants(matching: .any)["morde.task.step.s0000005"].exists)
        XCTAssertTrue(app.descendants(matching: .any)["morde.task.activity.emptyHistory"].waitForExistence(timeout: 10))
        XCTAssertFalse(app.staticTexts["Private budget field"].exists)
        XCTAssertFalse(app.buttons["morde.task.action.pause"].exists)
        XCTAssertFalse(app.buttons["morde.task.action.cancel"].exists)
        attachScreen(app, named: "MDA-04 completed task detail")

        app.buttons["morde.task.back"].tap()
        let failedTask = app.buttons["morde.tasks.row.q0000006"]
        XCTAssertTrue(failedTask.waitForExistence(timeout: 10))
        failedTask.tap()

        let failedStatus = app.staticTexts["morde.task.status"]
        XCTAssertTrue(failedStatus.waitForExistence(timeout: 15))
        XCTAssertTrue(failedStatus.label.contains("失败"))
        XCTAssertTrue(failedStatus.label.contains("Review failed output"))
        XCTAssertTrue(app.descendants(matching: .any)["morde.task.step.s0000007"].exists)
        XCTAssertTrue(app.descendants(matching: .any)["morde.task.activity.emptyHistory"].waitForExistence(timeout: 10))
        XCTAssertTrue(app.buttons["morde.task.action.retry:b0000001"].waitForExistence(timeout: 10),
                      "A failed batch item keeps the existing retry action in Task detail.")
        XCTAssertFalse(app.buttons["morde.task.action.pause"].exists)
        XCTAssertFalse(app.buttons["morde.task.action.cancel"].exists)
        XCTAssertFalse(app.staticTexts["Private budget field"].exists)
        attachScreen(app, named: "MDA-04 failed task detail with existing retry action")

        app.buttons["morde.task.back"].tap()
        let cancelledTask = app.buttons["morde.tasks.row.q0000007"]
        XCTAssertTrue(cancelledTask.waitForExistence(timeout: 10))
        cancelledTask.tap()

        let cancelledStatus = app.staticTexts["morde.task.status"]
        XCTAssertTrue(cancelledStatus.waitForExistence(timeout: 15))
        XCTAssertTrue(cancelledStatus.label.contains("已取消"))
        XCTAssertTrue(cancelledStatus.label.contains("Local cancelled result"))
        let cancelledStep = app.descendants(matching: .any)["morde.task.step.s0000009"]
        XCTAssertTrue(cancelledStep.exists)
        XCTAssertTrue((cancelledStep.value as? String ?? "").contains("已取消"),
                      "A cancelled task step must not be presented as an unknown status.")
        XCTAssertTrue(app.descendants(matching: .any)["morde.task.activity.emptyHistory"].waitForExistence(timeout: 10))
        XCTAssertFalse(app.buttons["morde.task.action.pause"].exists)
        XCTAssertFalse(app.buttons["morde.task.action.resume"].exists)
        XCTAssertFalse(app.buttons["morde.task.action.cancel"].exists)
        XCTAssertFalse(app.buttons["morde.task.action.retry:b0000001"].exists)
        XCTAssertFalse(app.staticTexts["Private budget field"].exists)
        attachScreen(app, named: "MDA-04 cancelled task detail without task actions")
    }

    func testMordeTaskWaitingDetailsShowApprovalAndClarificationControls() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesFixture",
            "-morde.localTaskPagesTaskDetailRequestsFixture",
        ])
        app.buttons["morde.nav.tab.tasks"].tap()

        let approvalTask = app.buttons["morde.tasks.row.q0000101"]
        XCTAssertTrue(approvalTask.waitForExistence(timeout: 15))
        approvalTask.tap()

        let approvalStatus = app.staticTexts["morde.task.status"]
        XCTAssertTrue(approvalStatus.waitForExistence(timeout: 15))
        XCTAssertTrue(approvalStatus.label.contains("等你审批"))
        XCTAssertTrue(app.staticTexts["Review local change"].exists)
        XCTAssertTrue(app.buttons["morde.task.approval.reject.r0000001"].exists)
        XCTAssertTrue(app.buttons["morde.task.approval.approve.r0000001"].exists)
        XCTAssertTrue(app.buttons["morde.task.action.cancel"].exists)
        XCTAssertFalse(app.buttons["morde.task.clarification.submit"].exists)
        attachScreen(app, named: "MDA-04 waiting approval task detail")

        app.buttons["morde.task.back"].tap()
        let clarificationTask = app.buttons["morde.tasks.row.q0000102"]
        XCTAssertTrue(clarificationTask.waitForExistence(timeout: 10))
        clarificationTask.tap()

        let clarificationStatus = app.staticTexts["morde.task.status"]
        XCTAssertTrue(clarificationStatus.waitForExistence(timeout: 15))
        XCTAssertTrue(clarificationStatus.label.contains("等待你补充信息"))
        let question = app.staticTexts["morde.task.clarification.question"]
        XCTAssertTrue(question.waitForExistence(timeout: 10))
        XCTAssertEqual(question.label, "A synthetic clarification, not an approval.")
        let answer = app.textFields["morde.task.clarification.answer"]
        let submit = app.buttons["morde.task.clarification.submit"]
        XCTAssertTrue(answer.exists)
        XCTAssertTrue(submit.exists)
        XCTAssertFalse(submit.isEnabled, "An empty answer must not be submitted.")
        XCTAssertTrue(app.buttons["morde.task.action.cancel"].exists)
        XCTAssertFalse(app.buttons["morde.task.approval.approve.r0000001"].exists)
        XCTAssertFalse(app.buttons["morde.task.approval.reject.r0000001"].exists)
        answer.tap()
        answer.typeText("Synthetic response")
        XCTAssertTrue(submit.isEnabled, "A nonempty answer should enable the existing submit action.")
        attachScreen(app, named: "MDA-04 waiting clarification task detail")
    }

    func testMordeTaskWaitingOtherReasonDoesNotShowApprovalControls() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesFixture",
            "-morde.localTaskPagesTaskDetailRequestsFixture",
            "-morde.localTaskPagesOtherWaitReasonFixture",
        ])
        app.buttons["morde.nav.tab.tasks"].tap()

        let task = app.buttons["morde.tasks.row.q0000101"]
        XCTAssertTrue(task.waitForExistence(timeout: 15))
        task.tap()

        let status = app.staticTexts["morde.task.status"]
        XCTAssertTrue(status.waitForExistence(timeout: 15))
        XCTAssertTrue(status.label.contains("操作结果待核对"))
        XCTAssertTrue(status.label.contains("Review operation result"))
        XCTAssertTrue(app.buttons["morde.task.action.cancel"].exists)
        XCTAssertFalse(app.buttons["morde.task.approval.approve.r0000001"].exists)
        XCTAssertFalse(app.buttons["morde.task.approval.reject.r0000001"].exists)
        XCTAssertFalse(app.buttons["morde.task.clarification.submit"].exists)
        attachScreen(app, named: "MDA-04 task waiting for operation result review")
    }

    func testMordeTaskDetailShowsPermissionErrorWithoutTaskActions() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesFixture",
            "-morde.localTaskPagesTaskDetailRequestsFixture",
            "-morde.localTaskPagesTaskDetailReentryFixture",
        ])
        app.buttons["morde.nav.tab.tasks"].tap()

        let task = app.buttons["morde.tasks.row.q0000102"]
        XCTAssertTrue(task.waitForExistence(timeout: 15))
        task.tap()

        let loadError = app.descendants(matching: .any)["morde.task.loadError"]
        XCTAssertTrue(loadError.waitForExistence(timeout: 15))
        XCTAssertFalse(app.buttons["morde.task.loadError.retry"].exists,
                       "A failed detail read has no extra retry button.")
        XCTAssertFalse(app.buttons["Retry loading"].exists)
        XCTAssertFalse(app.buttons["morde.task.action.pause"].exists)
        XCTAssertFalse(app.buttons["morde.task.action.cancel"].exists)
        XCTAssertFalse(app.buttons["morde.task.approval.approve.r0000001"].exists)
        XCTAssertFalse(app.buttons["morde.task.clarification.submit"].exists)
        attachScreen(app, named: "MDA-04 task detail permission denied")

        app.buttons["morde.task.back"].tap()
        XCTAssertTrue(task.waitForExistence(timeout: 10))
        task.tap()
        XCTAssertTrue(app.staticTexts["morde.task.clarification.question"].waitForExistence(timeout: 15),
                      "Reentering the detail must perform a fresh request and recover the transient failure.")
        XCTAssertFalse(app.buttons["morde.task.loadError.retry"].exists)
        XCTAssertFalse(app.buttons["Retry loading"].exists)
        attachScreen(app, named: "MDA-04 task detail recovered after reentry")
    }

    func testMordeTaskDetailTimelineFailureHasNoRetryButtonAndRecoversAutomatically() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesFixture",
            "-morde.localTaskPagesTimelineRecoveryFixture",
        ])
        app.buttons["morde.nav.tab.tasks"].tap()

        let task = app.buttons["morde.tasks.row.q0000101"]
        XCTAssertTrue(task.waitForExistence(timeout: 15))
        task.tap()

        let timelineError = app.descendants(matching: .any)["morde.task.activity.timelineError"]
        XCTAssertTrue(timelineError.waitForExistence(timeout: 15))
        XCTAssertFalse(app.buttons["Retry loading"].exists,
                       "A task event connection failure must not add a retry button.")
        let recoveredEvent = app.staticTexts["Recovered event"]
        XCTAssertTrue(recoveredEvent.waitForExistence(timeout: 25),
                      "The event stream already reconnects automatically after a transient failure.")
        XCTAssertFalse(app.buttons["Retry loading"].exists)
        attachScreen(app, named: "MDA-04 task detail timeline recovered without retry button")
    }

    func testMordeTaskDetailUnconfirmedHistoryHasNoRetryButton() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesFixture",
            "-morde.localTaskPagesTimelineRecoveryFixture",
            "-morde.localTaskPagesTimelineUnconfirmedFixture",
        ])
        app.buttons["morde.nav.tab.tasks"].tap()

        let task = app.buttons["morde.tasks.row.q0000101"]
        XCTAssertTrue(task.waitForExistence(timeout: 15))
        task.tap()

        let history = app.descendants(matching: .any)["morde.task.activity.unconfirmedHistory"]
        XCTAssertTrue(history.waitForExistence(timeout: 15))
        XCTAssertFalse(app.buttons["Retry loading"].exists,
                       "Unconfirmed history remains an informational state without an extra button.")
        attachScreen(app, named: "MDA-04 unconfirmed task history without retry button")
    }

    func testMordeTaskApprovalLocksTaskActionsWhileSubmitting() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesFixture",
            "-morde.localTaskPagesTaskDetailRequestsFixture",
            "-morde.localTaskPagesApprovalDelayFixture",
        ])
        app.buttons["morde.nav.tab.tasks"].tap()

        let task = app.buttons["morde.tasks.row.q0000101"]
        XCTAssertTrue(task.waitForExistence(timeout: 15))
        task.tap()

        let approve = app.buttons["morde.task.approval.approve.r0000001"]
        let reject = app.buttons["morde.task.approval.reject.r0000001"]
        let cancel = app.buttons["morde.task.action.cancel"]
        XCTAssertTrue(approve.waitForExistence(timeout: 15))
        XCTAssertTrue(reject.exists)
        XCTAssertTrue(cancel.exists)
        attachScreen(app, named: "MDA-04 waiting approval actions before submission")

        approve.tap()
        XCTAssertFalse(approve.isEnabled, "Approve must lock while its request is in flight.")
        XCTAssertFalse(reject.isEnabled, "Reject must lock while its request is in flight.")
        XCTAssertFalse(cancel.isEnabled, "Task cancellation must lock while its approval is in flight.")
        attachScreen(app, named: "MDA-04 approval submission locks competing task actions")

        let settled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "exists == false"), object: approve)
        XCTAssertEqual(XCTWaiter.wait(for: [settled], timeout: 15), .completed,
                       "The confirmed approval must refresh the task detail and remove its pending decision.")
        XCTAssertTrue(app.staticTexts["morde.task.status"].label.contains("已完成"))
        XCTAssertFalse(cancel.exists, "A completed task must no longer expose cancellation.")
        attachScreen(app, named: "MDA-04 approved task detail after refresh")
    }

    func testMordeTaskClarificationLocksTaskActionsWhileSubmitting() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesFixture",
            "-morde.localTaskPagesTaskDetailRequestsFixture",
            "-morde.localTaskPagesClarificationDelayFixture",
        ])
        app.buttons["morde.nav.tab.tasks"].tap()

        let task = app.buttons["morde.tasks.row.q0000102"]
        XCTAssertTrue(task.waitForExistence(timeout: 15))
        task.tap()

        let answer = app.textFields["morde.task.clarification.answer"]
        let submit = app.buttons["morde.task.clarification.submit"]
        let cancel = app.buttons["morde.task.action.cancel"]
        XCTAssertTrue(answer.waitForExistence(timeout: 15))
        answer.tap()
        answer.typeText("Synthetic response")
        XCTAssertTrue(submit.isEnabled)
        XCTAssertTrue(cancel.exists)
        attachScreen(app, named: "MDA-04 waiting clarification actions before submission")

        submit.tap()
        XCTAssertFalse(submit.isEnabled, "Clarification submission must lock while its request is in flight.")
        XCTAssertFalse(answer.isEnabled, "The answer must not change while its request is in flight.")
        XCTAssertFalse(cancel.isEnabled, "Task cancellation must lock while clarification is in flight.")
        attachScreen(app, named: "MDA-04 clarification submission locks competing task actions")

        let settled = XCTNSPredicateExpectation(
            predicate: NSPredicate(format: "exists == false"),
            object: app.staticTexts["morde.task.clarification.question"])
        XCTAssertEqual(XCTWaiter.wait(for: [settled], timeout: 15), .completed,
                       "The confirmed answer must refresh the task detail and clear its clarification request.")
        XCTAssertTrue(app.staticTexts["morde.task.status"].label.contains("已完成"))
        XCTAssertFalse(cancel.exists, "A completed task must no longer expose cancellation.")
        attachScreen(app, named: "MDA-04 clarified task detail after refresh")
    }

    func testMordeTaskDetailDropsLateResponseAfterAccountSwitch() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localAccountIsolationFixture",
            "-morde.localEventChainAccountIsolationFixture",
            "-morde.localTaskDetailLateDeliveryFixture",
        ], loginName: "account-a", resetExistingAccount: true)
        app.buttons["morde.nav.tab.tasks"].tap()

        let accountATask = app.buttons["morde.tasks.row.q0000001"]
        XCTAssertTrue(accountATask.waitForExistence(timeout: 15))
        accountATask.tap()
        XCTAssertTrue(app.staticTexts["正在加载任务…"].waitForExistence(timeout: 10),
                      "Account A's detail response should remain in flight during the account switch.")
        XCTAssertFalse(app.staticTexts["account-a-private-task-detail"].exists)
        attachScreen(app, named: "MDA-04 account A task detail loading before account switch")

        app.buttons["morde.task.back"].tap()
        signOutCurrentMordeAccount(app)
        let objective = app.staticTexts["morde.task.objective"]
        let username = app.textFields["morde.auth.username"]
        XCTAssertTrue(username.waitForExistence(timeout: 10))
        username.tap()
        username.typeText("account-b")
        let password = app.secureTextFields["morde.auth.password"]
        password.tap()
        password.typeText("local-password")
        app.buttons["morde.auth.submit"].tap()

        let tasksTab = app.buttons["morde.nav.tab.tasks"]
        XCTAssertTrue(tasksTab.waitForExistence(timeout: 20), "Account B should restore an authenticated route.")
        if !objective.exists {
            tasksTab.tap()
            let accountBTask = app.buttons["morde.tasks.row.q0000001"]
            XCTAssertTrue(accountBTask.waitForExistence(timeout: 15))
            accountBTask.tap()
        }
        XCTAssertTrue(objective.waitForExistence(timeout: 15))
        XCTAssertEqual(objective.label, "account-b-private-task-detail")
        XCTAssertFalse(app.staticTexts["account-a-private-task-detail"].exists)

        let lateResponseReleased = expectation(description: "Account A's late task detail response was released")
        DispatchQueue.global().asyncAfter(deadline: .now() + 20) { lateResponseReleased.fulfill() }
        wait(for: [lateResponseReleased], timeout: 22)
        XCTAssertEqual(objective.label, "account-b-private-task-detail",
                       "A late detail response from Account A must not replace Account B's task.")
        XCTAssertFalse(app.staticTexts["account-a-private-task-detail"].exists)
        attachScreen(app, named: "MDA-04 account B task detail after account A late response")
    }

    func testMordeTaskDetailPauseLocksActionsAndRefreshesStatus() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesFixture",
            "-morde.localTaskPagesTaskActionDelayFixture",
        ])
        app.buttons["morde.nav.tab.tasks"].tap()

        let task = app.buttons["morde.tasks.row.q0000001"]
        XCTAssertTrue(task.waitForExistence(timeout: 15))
        task.tap()

        let pause = app.buttons["morde.task.action.pause"]
        let cancel = app.buttons["morde.task.action.cancel"]
        XCTAssertTrue(pause.waitForExistence(timeout: 15))
        XCTAssertTrue(cancel.exists)
        attachScreen(app, named: "MDA-04-task-running-actions")

        pause.tap()
        let bothActionsDisabled = XCTNSPredicateExpectation(
            predicate: NSPredicate(format: "isEnabled == false"), object: cancel)
        XCTAssertEqual(XCTWaiter.wait(for: [bothActionsDisabled], timeout: 2), .completed,
                       "The shared in-flight action must disable the competing cancel action.")
        XCTAssertFalse(pause.isEnabled)

        let resume = app.buttons["morde.task.action.resume"]
        XCTAssertTrue(resume.waitForExistence(timeout: 15),
                      "The accepted pause must refresh the detail and expose Resume.")
        XCTAssertFalse(pause.exists)
        XCTAssertTrue(app.staticTexts["morde.task.status"].label.contains("已暂停"))
        attachScreen(app, named: "MDA-04-task-paused-actions")
    }

    func testMordeTaskDetailRetriesUnknownPauseThroughTheExistingAction() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesFixture",
            "-morde.localTaskPagesTaskActionUnknownRetryFixture",
        ])
        app.buttons["morde.nav.tab.tasks"].tap()

        let task = app.buttons["morde.tasks.row.q0000001"]
        XCTAssertTrue(task.waitForExistence(timeout: 15))
        task.tap()

        let pause = app.buttons["morde.task.action.pause"]
        XCTAssertTrue(pause.waitForExistence(timeout: 15))
        pause.tap()

        let unknownResult = app.staticTexts["任务操作结果未知；请重试相同操作或刷新 Task 状态确认。"]
        XCTAssertTrue(unknownResult.waitForExistence(timeout: 15))
        XCTAssertTrue(pause.isEnabled,
                      "The existing pause action should allow the same operation to be retried after an unknown result.")
        XCTAssertTrue(app.staticTexts["morde.task.status"].label.contains("运行中"),
                      "The UI must keep the last confirmed Task state until the retry is accepted.")
        XCTAssertFalse(app.buttons["Retry loading"].exists,
                       "Failure feedback must not add a separate retry control.")
        attachScreen(app, named: "MDA-04 task action result unknown")

        pause.tap()
        let resume = app.buttons["morde.task.action.resume"]
        XCTAssertTrue(resume.waitForExistence(timeout: 15),
                      "Retrying through the same action should refresh the accepted paused state.")
        XCTAssertFalse(unknownResult.exists)
        XCTAssertTrue(app.staticTexts["morde.task.status"].label.contains("已暂停"))
        attachScreen(app, named: "MDA-04 task action recovered after retry")
    }

    func testMordeTaskListShowsConfirmedEmptyState() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localTaskPagesEmptyFixture"])
        app.buttons["morde.nav.tab.tasks"].tap()

        let menu = app.buttons["morde.nav.menu"]
        let title = app.staticTexts["任务"]
        let newTask = app.buttons["morde.tasks.newTask"]
        XCTAssertTrue(menu.waitForExistence(timeout: 15))
        XCTAssertTrue(title.exists)
        XCTAssertTrue(newTask.exists)
        XCTAssertEqual(menu.frame.minX, 22, accuracy: 1)
        XCTAssertEqual(menu.frame.width, 38, accuracy: 1)
        XCTAssertEqual(title.frame.minX, menu.frame.maxX + 10, accuracy: 1)
        XCTAssertEqual(newTask.frame.maxX, app.frame.width - 22, accuracy: 1)
        XCTAssertTrue(app.descendants(matching: .any)["morde.tasks.empty"].waitForExistence(timeout: 15))
        XCTAssertFalse(app.buttons.matching(NSPredicate(format: "identifier CONTAINS %@", "morde.tasks.row.")).firstMatch.exists)
    }

    func testMordeTaskListShowsStepCountsFromSummary() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localTaskPagesStepCountsFixture"])
        app.buttons["morde.nav.tab.tasks"].tap()

        let stepDetail = app.staticTexts["morde.tasks.row.detail.q0000001"]
        XCTAssertTrue(stepDetail.waitForExistence(timeout: 15))
        XCTAssertEqual(stepDetail.label, "5 步 · 2 步完成")
        attachScreen(app, named: "MDA-03-task-summary-step-counts")
    }

    func testMordeTaskListRendersFiveDesignRowsWithoutExtraActions() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localTaskPagesDesignRowsFixture"])
        app.buttons["morde.nav.tab.tasks"].tap()

        let expectedRows = [
            ("q0000001", "发布 v2.4.1"),
            ("q0000002", "修复支付回调超时"),
            ("q0000003", "机型兼容测试"),
            ("q0000004", "依赖安全升级"),
            ("q0000005", "周报数据汇总"),
        ]
        for (taskID, title) in expectedRows {
            XCTAssertTrue(app.buttons["morde.tasks.row.\(taskID)"].waitForExistence(timeout: 15),
                          "Missing design-shaped task row \(taskID)")
            XCTAssertTrue(app.staticTexts[title].exists)
            XCTAssertTrue(app.staticTexts["morde.tasks.row.detail.\(taskID)"].exists,
                          "Each sample row should show its real step/result detail.")
            XCTAssertTrue(app.staticTexts["morde.tasks.row.status.\(taskID)"].exists)
        }
        XCTAssertEqual(app.staticTexts["morde.tasks.row.detail.q0000001"].label, "5 步 · 4 步完成")
        XCTAssertEqual(app.staticTexts["morde.tasks.row.status.q0000001"].label, "等你审批")
        XCTAssertEqual(app.buttons.matching(
            NSPredicate(format: "identifier BEGINSWITH %@", "morde.tasks.row.")).count, 5)
        XCTAssertFalse(app.buttons["morde.task.action.pause"].exists)
        XCTAssertFalse(app.buttons["morde.task.action.resume"].exists)
        XCTAssertFalse(app.buttons["morde.task.action.cancel"].exists)
        XCTAssertFalse(app.staticTexts.matching(
            NSPredicate(format: "label CONTAINS %@", "预算")).firstMatch.exists)
        attachScreen(app, named: "MDA-03-five-design-rows")
    }

    func testMordeTaskListRendersKnownAndUnknownStatusesWithoutActions() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesStatusFixture",
            "-morde.localTaskPagesLongListFixture",
        ])
        app.buttons["morde.nav.tab.tasks"].tap()

        let taskList = app.scrollViews["morde.tasks.list"]
        XCTAssertTrue(taskList.waitForExistence(timeout: 10))
        let expectedStatuses = [
            ("q0000001", "已排队"), ("q0000002", "运行中"),
            ("q0000003", "等你确认"), ("q0000004", "已暂停"),
            ("q0000005", "已完成"), ("q0000006", "任务失败"),
            ("q0000007", "已取消"), ("q0000008", "状态未知"),
        ]
        for (taskID, expectedLabel) in expectedStatuses {
            let status = app.staticTexts["morde.tasks.row.status.\(taskID)"]
            for _ in 0..<10 where !status.exists { taskList.swipeUp() }
            XCTAssertTrue(status.waitForExistence(timeout: 10), "Missing status for \(taskID)")
            XCTAssertEqual(status.label, expectedLabel, "Unexpected status for \(taskID)")
            XCTAssertFalse(app.staticTexts["morde.tasks.row.detail.\(taskID)"].exists,
                           "A zero-step summary must not add a false progress line.")
        }
        XCTAssertFalse(app.buttons["morde.task.action.pause"].exists)
        XCTAssertFalse(app.buttons["morde.task.action.resume"].exists)
        XCTAssertFalse(app.buttons["morde.task.action.cancel"].exists)
        attachScreen(app, named: "MDA-03-task-list-known-and-unknown-statuses")

        let lastTaskRow = app.buttons["morde.tasks.row.q0000020"]
        for _ in 0..<12 { taskList.swipeUp() }
        XCTAssertTrue(lastTaskRow.waitForExistence(timeout: 5))
        let tabBarButton = app.buttons["morde.nav.tab.chat"]
        XCTAssertTrue(lastTaskRow.isHittable, "The last row should be visible after scrolling to the content end.")
        XCTAssertEqual(tabBarButton.frame.minY - 5 - lastTaskRow.frame.maxY, 30, accuracy: 3,
                       "At the content end, the last row should sit 30 pt above the floating tab bar capsule.")
        attachScreen(app, named: "MDA-03 task list at final-row tail gap")
    }

    func testMordeTaskListContinuesAfterAnEmptyIntermediatePage() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localTaskPagesEmptyIntermediateFixture"])
        app.buttons["morde.nav.tab.tasks"].tap()

        let taskList = app.scrollViews["morde.tasks.list"]
        XCTAssertTrue(taskList.waitForExistence(timeout: 10))
        XCTAssertFalse(app.descendants(matching: .any)["morde.tasks.empty"].exists)
        XCTAssertFalse(app.buttons["morde.tasks.loadMore"].exists)

        let nextPageTask = app.buttons["morde.tasks.row.q0000001"]
        for _ in 0..<10 where !nextPageTask.exists { taskList.swipeUp() }
        XCTAssertTrue(nextPageTask.waitForExistence(timeout: 15),
                      "An empty cursor page should continue automatically without adding a load-more button.")
    }

    func testMordeTaskListRecoversFromLoadFailureWithPullToRefresh() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localTaskPagesRetryFixture"])
        app.buttons["morde.nav.tab.tasks"].tap()

        let loadError = app.descendants(matching: .any)["morde.tasks.loadError"]
        XCTAssertTrue(loadError.waitForExistence(timeout: 15))
        XCTAssertFalse(app.buttons["morde.tasks.loadError.retry"].exists)
        let taskList = app.scrollViews["morde.tasks.list"]
        XCTAssertTrue(taskList.exists)
        let refreshStart = taskList.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.02))
        let refreshEnd = taskList.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.75))
        refreshStart.press(forDuration: 0.1, thenDragTo: refreshEnd)
        XCTAssertTrue(app.descendants(matching: .any)["morde.tasks.empty"].waitForExistence(timeout: 20))
    }

    func testMordeTaskListRecoversFromLoadFailureWithLegacyUIKitBounceFallback() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesRetryFixture",
            "-morde.forceLegacyScrollBounceFixture",
        ])
        app.buttons["morde.nav.tab.tasks"].tap()

        let loadError = app.descendants(matching: .any)["morde.tasks.loadError"]
        XCTAssertTrue(loadError.waitForExistence(timeout: 15))
        XCTAssertFalse(app.buttons["morde.tasks.loadError.retry"].exists)
        let taskList = app.scrollViews["morde.tasks.list"]
        XCTAssertTrue(taskList.exists)
        let refreshStart = taskList.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.02))
        let refreshEnd = taskList.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.75))
        refreshStart.press(forDuration: 0.1, thenDragTo: refreshEnd)
        XCTAssertTrue(app.descendants(matching: .any)["morde.tasks.empty"].waitForExistence(timeout: 20))
    }

    func testMordeEventChainShowsRealEmptyStateAndReturnsToChat() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localTaskPagesEmptyFixture"])
        app.buttons["morde.nav.menu"].tap()
        app.buttons["morde.nav.drawer.eventChain"].tap()

        XCTAssertTrue(app.descendants(matching: .any)["morde.eventChain.empty"].waitForExistence(timeout: 15))
        XCTAssertTrue(app.staticTexts["事件链"].exists)
        XCTAssertTrue(app.staticTexts["当前没有可用的任务"].exists)
        XCTAssertFalse(app.staticTexts["morde.eventChain.taskTitle"].exists)
        XCTAssertFalse(app.descendants(matching: .any)
            .matching(NSPredicate(format: "identifier BEGINSWITH %@", "morde.eventChain.event.")).firstMatch.exists)
        attachScreen(app, named: "MDA-05-event-chain-empty-task-state")

        app.buttons["morde.nav.tab.chat"].tap()
        XCTAssertTrue(app.descendants(matching: .any)["morde.chat.actions"].waitForExistence(timeout: 10))
    }

    func testMordeEventChainShowsConfirmedNoHistoryForLinkedTask() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesFixture",
            "-morde.localTaskPagesEventChainEmptyTimelineFixture",
        ])
        app.buttons["morde.nav.tab.approvals"].tap()

        let eventChain = app.buttons["mif.approval.timeline.q0000101"]
        XCTAssertTrue(eventChain.waitForExistence(timeout: 15))
        eventChain.tap()

        let taskTitle = app.staticTexts["morde.eventChain.taskTitle"]
        XCTAssertTrue(taskTitle.waitForExistence(timeout: 15))
        XCTAssertEqual(taskTitle.label, "本地事件链验收")
        XCTAssertTrue(app.staticTexts["morde.eventChain.noHistory"].waitForExistence(timeout: 15))
        XCTAssertFalse(app.descendants(matching: .any)
            .matching(NSPredicate(format: "identifier BEGINSWITH %@", "morde.eventChain.event.")).firstMatch.exists)
        XCTAssertFalse(app.staticTexts["Private budget field"].exists)
        attachScreen(app, named: "MDA-05-event-chain-no-history")
    }

    func testMordeEventChainTimelineDensityAndStatusesMatchPrototype() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesFixture",
            "-morde.localEventChainVisualFixture",
        ])
        app.buttons["morde.nav.tab.approvals"].tap()

        let eventChain = app.buttons["mif.approval.timeline.q0000101"]
        XCTAssertTrue(eventChain.waitForExistence(timeout: 15))
        eventChain.tap()

        let titles = [
            "Goal confirmed", "Plan prepared", "Repository reviewed", "Changes written",
            "Tests passed", "Approval needed", "Rollout queued",
        ]
        let titlePredicate = NSPredicate(format: "label IN %@", titles)
        let eventTitles = app.staticTexts.matching(titlePredicate)
        XCTAssertTrue(app.staticTexts["morde.eventChain.taskTitle"].waitForExistence(timeout: 15))
        XCTAssertTrue(app.staticTexts["Rollout queued"].waitForExistence(timeout: 20))
        XCTAssertEqual(eventTitles.count, titles.count)
        XCTAssertEqual(app.descendants(matching: .any)
            .matching(NSPredicate(format: "label == %@", "已完成")).count, 5)
        XCTAssertEqual(app.descendants(matching: .any)
            .matching(NSPredicate(format: "label == %@", "当前")).count, 1)
        XCTAssertEqual(app.descendants(matching: .any)
            .matching(NSPredicate(format: "label == %@", "已排队")).count, 1)
        XCTAssertFalse(app.staticTexts["Private budget field"].exists)
        attachScreen(app, named: "MDA-05-event-chain-seven-row-timeline")
    }

    func testMordeEventChainMarksFailedEvents() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesFixture",
            "-morde.localEventChainVisualFixture",
            "-morde.localEventChainFailureFixture",
        ])
        app.buttons["morde.nav.tab.approvals"].tap()

        let eventChain = app.buttons["mif.approval.timeline.q0000101"]
        XCTAssertTrue(eventChain.waitForExistence(timeout: 15))
        eventChain.tap()
        XCTAssertTrue(app.staticTexts["Repository reviewed"].waitForExistence(timeout: 20))
        XCTAssertEqual(app.descendants(matching: .any)
            .matching(NSPredicate(format: "label == %@", "失败")).count, 1)
        let failureSummary = app.staticTexts.matching(
            NSPredicate(format: "label CONTAINS %@", "Project files checked.")).firstMatch
        XCTAssertTrue(failureSummary.exists)
        XCTAssertTrue(failureSummary.label.hasPrefix("失败"))
        attachScreen(app, named: "MDA-05-event-chain-failed-event")
    }

    func testMordeEventChainRecoversExpiredCursorAndDeduplicatesEvents() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesFixture",
            "-morde.localTaskPagesEventChainTimelineFixture",
        ])
        app.buttons["morde.nav.tab.approvals"].tap()

        let eventChain = app.buttons["mif.approval.timeline.q0000101"]
        XCTAssertTrue(eventChain.waitForExistence(timeout: 15))
        eventChain.tap()

        let taskTitle = app.staticTexts["morde.eventChain.taskTitle"]
        XCTAssertTrue(taskTitle.waitForExistence(timeout: 15))
        XCTAssertEqual(taskTitle.label, "Local task timeline fixture")
        let recoveredEvents = app.staticTexts.matching(NSPredicate(format: "label == %@", "Recovered event"))
        XCTAssertTrue(recoveredEvents.firstMatch.waitForExistence(timeout: 25))
        XCTAssertEqual(recoveredEvents.count, 1)
        XCTAssertFalse(app.staticTexts["Private budget field"].exists)
    }

    func testMordeEventChainShowsNetworkErrorWithoutRetryButton() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesFixture",
            "-morde.localEventChainNetworkFailureFixture",
        ])
        app.buttons["morde.nav.tab.approvals"].tap()

        let eventChain = app.buttons["mif.approval.timeline.q0000101"]
        XCTAssertTrue(eventChain.waitForExistence(timeout: 15))
        eventChain.tap()

        let taskTitle = app.staticTexts["morde.eventChain.taskTitle"]
        XCTAssertTrue(taskTitle.waitForExistence(timeout: 15))
        XCTAssertEqual(taskTitle.label, "Local task timeline fixture")
        XCTAssertTrue(app.descendants(matching: .any)["morde.eventChain.timelineError"]
            .waitForExistence(timeout: 15), "The event request failure should remain visible on the loaded task.")
        XCTAssertFalse(app.descendants(matching: .any)
            .matching(NSPredicate(format: "identifier BEGINSWITH %@", "morde.eventChain.event.")).firstMatch.exists)
        XCTAssertFalse(app.alerts.firstMatch.exists,
                       "An event-chain stream error should stay inline and must not replace the task with an alert.")
        XCTAssertFalse(app.buttons["Retry loading"].exists,
                       "The event-chain page should use its existing pull-to-refresh instead of adding a button.")
        attachScreen(app, named: "MDA-05-event-chain-network-error")
    }

    func testMordeEventChainSwitchingAccountsDoesNotShowPreviousAccountData() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localAccountIsolationFixture",
            "-morde.localEventChainAccountIsolationFixture",
            "-morde.localEventChainLateDeliveryFixture",
        ], loginName: "account-a", resetExistingAccount: true)
        signOutCurrentMordeAccount(app)
        signInLocalFixtureAccount(app, name: "account-a")
        app.buttons["morde.nav.tab.approvals"].tap()

        let accountAEventChain = app.buttons["mif.approval.timeline.q0000001"]
        XCTAssertTrue(accountAEventChain.waitForExistence(timeout: 15))
        accountAEventChain.tap()
        let taskTitle = app.staticTexts["morde.eventChain.taskTitle"]
        XCTAssertTrue(taskTitle.waitForExistence(timeout: 15))
        XCTAssertEqual(taskTitle.label, "account-a-private-task-detail")
        XCTAssertTrue(app.descendants(matching: .any)["morde.eventChain.timelineLoading"].waitForExistence(timeout: 15),
                      "Account A's event stream should still be pending before sign-out.")
        attachScreen(app, named: "MDA-05-event-chain-loading")

        signOutCurrentMordeAccount(app)
        signInLocalFixtureAccount(app, name: "account-b")
        app.buttons["morde.nav.tab.approvals"].tap()

        let accountBEventChain = app.buttons["mif.approval.timeline.q0000001"]
        XCTAssertTrue(accountBEventChain.waitForExistence(timeout: 15))
        accountBEventChain.tap()
        XCTAssertTrue(taskTitle.waitForExistence(timeout: 15))
        XCTAssertEqual(taskTitle.label, "account-b-private-task-detail")
        let accountBEvent = app.staticTexts["account-b-private-event"]
        XCTAssertTrue(accountBEvent.waitForExistence(timeout: 15))
        let accountBEventMarker = app.descendants(matching: .any)
            .matching(NSPredicate(format: "label == %@", "已完成")).firstMatch
        XCTAssertTrue(accountBEventMarker.waitForExistence(timeout: 5))
        XCTAssertEqual(accountBEventMarker.label, "已完成",
                       "The event marker should reflect this event's succeeded status, not the running task status.")
        let accountBEventSummary = app.staticTexts.matching(
            NSPredicate(format: "label CONTAINS %@", "Synthetic account-b task event.")).firstMatch
        XCTAssertTrue(accountBEventSummary.exists)
        XCTAssertTrue(accountBEventSummary.label.hasPrefix("已完成"),
                      "Step status text should use the event's succeeded state rather than report an unknown task status.")

        // The fixture attempts Account A's delayed delivery after cancellation; it must not enter Account B's timeline.
        XCTAssertFalse(app.staticTexts["account-a-private-event"].waitForExistence(timeout: 11))
        attachScreen(app, named: "MDA-05-account-B-event-chain-after-late-response")
        XCTAssertTrue(accountBEvent.exists)
        XCTAssertFalse(app.staticTexts["account-a-private-task-detail"].exists)
        XCTAssertFalse(app.staticTexts["account-a-private-event"].exists)
    }

    func testMordeApprovalInboxShowsOnlyConfirmedDecisions() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesFixture",
            "-morde.localTaskPagesApprovalRefreshPendingFixture",
        ])
        app.buttons["morde.nav.tab.approvals"].tap()

        let count = app.staticTexts["mif.approval.count"]
        XCTAssertTrue(count.waitForExistence(timeout: 20))
        XCTAssertEqual(count.label, "1 件", "The compact header count should match the design label")
        XCTAssertTrue(app.buttons["mif.approval.reject.q0000101:r0000001:1"].waitForExistence(timeout: 10))
        XCTAssertTrue(app.buttons["mif.approval.approve.q0000101:r0000001:1"].exists)
        XCTAssertFalse(app.descendants(matching: .any)["mif.approval.partial"].exists)
        attachScreen(app, named: "MDA-06-single-confirmed-approval-before-decision")
        let eventChain = app.buttons["mif.approval.timeline.q0000101"]
        XCTAssertEqual(eventChain.label, "看事件链 ›")
        let approve = app.buttons["mif.approval.approve.q0000101:r0000001:1"]
        approve.tap()
        let decisionSubmitted = XCTNSPredicateExpectation(predicate: NSPredicate(format: "exists == false"),
                                                          object: approve)
        XCTAssertEqual(XCTWaiter.wait(for: [decisionSubmitted], timeout: 10), .completed,
                       "The accepted decision should replace the response buttons while task status refreshes.")
        XCTAssertTrue(eventChain.exists, "The event-chain link must remain available after a decision is submitted.")
        eventChain.tap()
        XCTAssertEqual(app.staticTexts["morde.eventChain.taskTitle"].waitForExistence(timeout: 15), true)
        XCTAssertEqual(app.staticTexts["morde.eventChain.taskTitle"].label, "Local approval fixture")
    }

    func testMordeApprovalEventChainLinkHidesAfterTaskStatusIsConfirmed() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localTaskPagesFixture"])
        app.buttons["morde.nav.tab.approvals"].tap()

        let eventChain = app.buttons["mif.approval.timeline.q0000101"]
        XCTAssertTrue(eventChain.waitForExistence(timeout: 20))
        app.buttons["mif.approval.approve.q0000101:r0000001:1"].tap()
        let linkHidden = XCTNSPredicateExpectation(predicate: NSPredicate(format: "exists == false"),
                                                   object: eventChain)
        XCTAssertEqual(XCTWaiter.wait(for: [linkHidden], timeout: 15), .completed,
                       "Hide the event-chain shortcut only after refreshed Task details confirm the approval is no longer pending.")
    }

    func testMordeApprovalCountIsHiddenWhenTaskDetailsArePartial() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesFixture",
            "-morde.localTaskPagesApprovalPartialFixture",
        ])
        app.buttons["morde.nav.tab.approvals"].tap()

        XCTAssertTrue(app.descendants(matching: .any)["mif.approval.partial"].waitForExistence(timeout: 20))
        XCTAssertFalse(app.staticTexts["mif.approval.count"].exists,
                       "A partial task-detail read must not be presented as a confirmed count.")
        XCTAssertTrue(app.buttons["mif.approval.approve.q0000101:r0000001:1"].exists)
        XCTAssertFalse(app.buttons.matching(
            NSPredicate(format: "identifier BEGINSWITH %@", "mif.approval.approve.q0000102:")
        ).firstMatch.exists)
    }

    func testMordeApprovalInboxShowsUnconfirmedStateWhenTaskListFails() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesFixture",
            "-morde.localTaskPagesApprovalLoadFailureFixture",
        ])
        app.buttons["morde.nav.tab.approvals"].tap()

        XCTAssertTrue(app.staticTexts["mif.approval.unconfirmed"].waitForExistence(timeout: 15))
        XCTAssertFalse(app.staticTexts["mif.approval.count"].exists,
                       "A failed approval-list request must not show a confirmed zero count.")
        XCTAssertFalse(app.descendants(matching: .any)["mif.approval.empty.container"].exists,
                       "An unavailable list must not be presented as a confirmed empty inbox.")
        XCTAssertFalse(app.buttons.matching(
            NSPredicate(format: "identifier BEGINSWITH %@", "mif.approval.approve.")
        ).firstMatch.exists)
        XCTAssertFalse(app.buttons.matching(
            NSPredicate(format: "identifier BEGINSWITH %@", "mif.approval.reject.")
        ).firstMatch.exists)
        attachScreen(app, named: "MDA-06-approval-list-unconfirmed")

        let approvalsList = app.scrollViews["mif.approval.list"]
        XCTAssertTrue(approvalsList.exists)
        let refreshStart = approvalsList.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.02))
        let refreshEnd = approvalsList.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.78))
        refreshStart.press(forDuration: 0.1, thenDragTo: refreshEnd)

        let confirmedCount = app.staticTexts["mif.approval.count"]
        XCTAssertTrue(confirmedCount.waitForExistence(timeout: 20),
                      "Pull-to-refresh should recover the approval list after its first failed request.")
        XCTAssertTrue(confirmedCount.label.contains("0"))
        XCTAssertTrue(app.descendants(matching: .any)["mif.approval.empty.container"].exists)
        XCTAssertFalse(app.staticTexts["mif.approval.unconfirmed"].exists)
    }

    func testMordeApprovalInboxDoesNotDecideMediaWithoutPreview() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskSSEFixture",
            "-morde.localTaskMediaFixture",
        ])
        app.buttons["morde.nav.tab.approvals"].tap()

        let mediaRequest = app.staticTexts["Save generated image?"]
        XCTAssertTrue(mediaRequest.waitForExistence(timeout: 20))
        XCTAssertTrue(app.staticTexts[
            "媒体审批必须在带预览的任务视图中处理；此收件箱不支持批准或拒绝。"
        ].exists)
        XCTAssertFalse(app.buttons.matching(
            NSPredicate(format: "identifier BEGINSWITH %@", "mif.approval.approve.q1234567:")
        ).firstMatch.exists)
        XCTAssertFalse(app.buttons.matching(
            NSPredicate(format: "identifier BEGINSWITH %@", "mif.approval.reject.q1234567:")
        ).firstMatch.exists)
        XCTAssertTrue(app.buttons["mif.approval.timeline.q1234567"].exists,
                      "Only the existing event-chain route should remain visible from this inbox card.")
        attachScreen(app, named: "MDA-06-media-approval-preview-required")
    }

    func testMordeApprovalRejectSubmitsAndRemovesPendingActions() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localTaskPagesFixture"])
        app.buttons["morde.nav.tab.approvals"].tap()

        let reject = app.buttons["mif.approval.reject.q0000101:r0000001:1"]
        let approve = app.buttons["mif.approval.approve.q0000101:r0000001:1"]
        XCTAssertTrue(reject.waitForExistence(timeout: 20))
        reject.tap()

        XCTAssertTrue(app.descendants(matching: .any)["mif.approval.action-result"]
            .waitForExistence(timeout: 20), "An accepted rejection should be shown as submitted.")
        let rejectionSettled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "exists == false"),
                                                         object: reject)
        XCTAssertEqual(XCTWaiter.wait(for: [rejectionSettled], timeout: 20), .completed,
                       "A submitted rejection should remove the pending decision action.")
        XCTAssertFalse(approve.exists, "The opposite decision must not remain available after rejection.")
    }

    func testMordeApprovalRetriesUnknownResponseWithSameDecision() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesFixture",
            "-morde.localTaskPagesApprovalRetryWithoutCommitFixture",
        ])
        app.buttons["morde.nav.tab.approvals"].tap()

        let approve = app.buttons["mif.approval.approve.q0000101:r0000001:1"]
        XCTAssertTrue(approve.waitForExistence(timeout: 20))
        approve.tap()

        XCTAssertTrue(app.descendants(matching: .any)["mif.approval.action-result"]
            .waitForExistence(timeout: 20), "A lost first response must be shown as unconfirmed.")
        XCTAssertTrue(approve.waitForExistence(timeout: 15),
                      "The pending approval must remain available for an explicit retry.")
        approve.tap()

        let responseAccepted = XCTNSPredicateExpectation(predicate: NSPredicate(format: "exists == false"),
                                                         object: approve)
        XCTAssertEqual(XCTWaiter.wait(for: [responseAccepted], timeout: 20), .completed,
                       "Retrying the same decision should settle the synthetic approval once.")
    }

    func testMordeApprovalConflictRefreshesAndRemovesStaleDecision() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskSSEFixture",
            "-morde.localTaskApprovalConflictFixture",
        ])
        app.buttons["morde.nav.tab.approvals"].tap()

        let approve = app.buttons["mif.approval.approve.q1234567:r1234567:1"]
        let count = app.staticTexts["mif.approval.count"]
        XCTAssertTrue(approve.waitForExistence(timeout: 20))
        XCTAssertTrue(count.label.contains("1"))
        approve.tap()

        let staleDecisionRemoved = XCTNSPredicateExpectation(predicate: NSPredicate(format: "exists == false"),
                                                              object: approve)
        XCTAssertEqual(XCTWaiter.wait(for: [staleDecisionRemoved], timeout: 20), .completed,
                       "A version conflict followed by a completed task refresh must remove the stale decision.")
        let zeroCount = XCTNSPredicateExpectation(predicate: NSPredicate(format: "label CONTAINS %@", "0"),
                                                   object: count)
        XCTAssertEqual(XCTWaiter.wait(for: [zeroCount], timeout: 10), .completed,
                       "The header count must reflect the refreshed server task state.")
        XCTAssertTrue(app.descendants(matching: .any)["mif.approval.empty"].exists)
    }

    func testMordeApprovalDisablesBothDecisionsWhileSubmitting() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesFixture",
            "-morde.localTaskPagesApprovalDelayFixture",
        ])
        app.buttons["morde.nav.tab.approvals"].tap()

        let approve = app.buttons["mif.approval.approve.q0000101:r0000001:1"]
        let reject = app.buttons["mif.approval.reject.q0000101:r0000001:1"]
        XCTAssertTrue(approve.waitForExistence(timeout: 20))
        XCTAssertTrue(reject.exists)
        approve.tap()

        let approveDisabled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isEnabled == false"),
                                                        object: approve)
        let rejectDisabled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isEnabled == false"),
                                                       object: reject)
        XCTAssertEqual(XCTWaiter.wait(for: [approveDisabled, rejectDisabled], timeout: 2), .completed,
                       "Submitting either decision must disable both competing actions until the server responds.")
    }

    func testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localWorkItemFixture"])
        app.buttons["morde.nav.menu"].tap()

        let workItems = app.buttons["morde.nav.drawer.workItems"]
        XCTAssertTrue(workItems.waitForExistence(timeout: 10))
        workItems.tap()

        let projectPicker = app.buttons["morde.workItems.projectPicker"]
        XCTAssertTrue(projectPicker.waitForExistence(timeout: 10))
        let create = app.buttons["morde.workItems.create.open"]
        XCTAssertTrue(create.waitForExistence(timeout: 10))
        projectPicker.tap()
        let readOnlyProject = app.buttons["morde.workItems.project.j0000002"]
        XCTAssertTrue(readOnlyProject.waitForExistence(timeout: 5))
        readOnlyProject.tap()
        XCTAssertFalse(create.isEnabled, "A viewer must not be able to open the create form.")

        projectPicker.tap()
        let project = app.buttons["morde.workItems.project.j0000001"]
        XCTAssertTrue(project.waitForExistence(timeout: 5))
        project.tap()
        XCTAssertTrue(create.isEnabled, "The editable project owner should be able to create work items.")

        for filter in ["all", "bug", "request", "unclaimed"] {
            XCTAssertTrue(app.buttons["morde.workItems.filter.\(filter)"].waitForExistence(timeout: 10),
                          "Missing \(filter) work item filter")
        }
        let defect = app.descendants(matching: .any)["morde.workItems.row.title.j1000001"]
        let request = app.descendants(matching: .any)["morde.workItems.row.title.j1000002"]
        let unclaimed = app.descendants(matching: .any)["morde.workItems.row.title.j1000004"]
        XCTAssertTrue(defect.waitForExistence(timeout: 15))
        XCTAssertTrue(request.waitForExistence(timeout: 15))
        XCTAssertTrue(unclaimed.waitForExistence(timeout: 15))

        attachScreen(app, named: "MDA-08-work-items-list")

        app.buttons["morde.workItems.filter.bug"].tap()
        XCTAssertTrue(defect.exists, "Bug filter should show the defect row.")
        XCTAssertFalse(request.exists, "Bug filter should hide the request row.")
        XCTAssertFalse(unclaimed.exists, "Bug filter should hide an unclassified row.")
        app.buttons["morde.workItems.filter.request"].tap()
        XCTAssertFalse(defect.exists, "Request filter should hide the defect row.")
        XCTAssertTrue(request.exists, "Request filter should show the request row.")
        XCTAssertFalse(unclaimed.exists, "Request filter should hide an unclassified row.")
        app.buttons["morde.workItems.filter.unclaimed"].tap()
        XCTAssertFalse(defect.exists, "Unclaimed filter should hide an assigned row.")
        XCTAssertFalse(request.exists, "Unclaimed filter should hide an assigned row.")
        XCTAssertTrue(unclaimed.exists, "Unclaimed filter should show the unassigned row.")
        app.buttons["morde.workItems.filter.all"].tap()

        XCTAssertTrue(create.waitForExistence(timeout: 15))
        XCTAssertTrue(waitForHittable(create))
        create.tap()

        XCTAssertTrue(app.textFields["morde.workItems.create.title"].waitForExistence(timeout: 10))
        XCTAssertTrue(app.buttons["morde.workItems.create.category.bug"].exists)
        XCTAssertTrue(app.buttons["morde.workItems.create.category.request"].exists)
        attachScreen(app, named: "MDA-08-work-item-create-form")
    }

    func testMordeWorkItemsLongListEndsAboveFloatingTabBar() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localWorkItemFixture",
            "-morde.localWorkItemLongListFixture",
        ])
        app.buttons["morde.nav.menu"].tap()
        let workItems = app.buttons["morde.nav.drawer.workItems"]
        XCTAssertTrue(workItems.waitForExistence(timeout: 10))
        workItems.tap()

        let projectPicker = app.buttons["morde.workItems.projectPicker"]
        XCTAssertTrue(projectPicker.waitForExistence(timeout: 10))
        projectPicker.tap()
        let project = app.buttons["morde.workItems.project.j0000001"]
        XCTAssertTrue(project.waitForExistence(timeout: 5))
        project.tap()

        let workItemScroll = app.scrollViews["morde.workItems.scroll"]
        let lastRow = app.descendants(matching: .any)["morde.workItems.row.j1000020"]
        XCTAssertTrue(workItemScroll.waitForExistence(timeout: 10))
        XCTAssertTrue(lastRow.waitForExistence(timeout: 15))
        for _ in 0..<15 { workItemScroll.swipeUp() }
        XCTAssertTrue(lastRow.isHittable, "The last fixture row should be visible at the content end.")
        let tabBarButton = app.buttons["morde.nav.tab.chat"]
        XCTAssertEqual(tabBarButton.frame.minY - 5 - lastRow.frame.maxY, 30, accuracy: 3,
                       "The final work item should sit 30 pt above the floating tab bar capsule.")
        attachScreen(app, named: "MDA-08 work items at final-row tail gap")
    }

    func testMordeWorkItemsProjectEmptyStateShowsDesignCopy() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localWorkItemFixture",
            "-morde.localWorkItemEmptyStateFixture",
        ])
        app.buttons["morde.nav.menu"].tap()
        let workItems = app.buttons["morde.nav.drawer.workItems"]
        XCTAssertTrue(workItems.waitForExistence(timeout: 10))
        workItems.tap()

        let projectPicker = app.buttons["morde.workItems.projectPicker"]
        XCTAssertTrue(projectPicker.waitForExistence(timeout: 10))
        projectPicker.tap()
        app.buttons["morde.workItems.project.j0000001"].tap()

        XCTAssertTrue(app.staticTexts["这一类暂时没有"].waitForExistence(timeout: 15))
        XCTAssertFalse(app.descendants(matching: .any)["morde.workItems.row.title.j1000001"].exists)
        attachScreen(app, named: "MDA-08-project-empty-state")
    }

    func testMordeWorkItemsFilterEmptyStateShowsDesignCopy() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localWorkItemFixture"])
        app.buttons["morde.nav.menu"].tap()
        let workItems = app.buttons["morde.nav.drawer.workItems"]
        XCTAssertTrue(workItems.waitForExistence(timeout: 10))
        workItems.tap()

        let projectPicker = app.buttons["morde.workItems.projectPicker"]
        XCTAssertTrue(projectPicker.waitForExistence(timeout: 10))
        projectPicker.tap()
        app.buttons["morde.workItems.project.j0000002"].tap()
        XCTAssertTrue(app.descendants(matching: .any)["morde.workItems.row.title.j2000001"]
            .waitForExistence(timeout: 15))

        let unclaimedFilter = app.buttons["morde.workItems.filter.unclaimed"]
        XCTAssertTrue(unclaimedFilter.waitForExistence(timeout: 5))
        unclaimedFilter.tap()
        XCTAssertTrue(app.staticTexts["这一类暂时没有"].waitForExistence(timeout: 10))
        XCTAssertFalse(app.descendants(matching: .any)["morde.workItems.row.title.j2000001"].exists)
        attachScreen(app, named: "MDA-08-filter-empty-state")
    }

    func testMordeWorkItemCreateFailureKeepsFormAndInput() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localWorkItemFixture",
            "-morde.localWorkItemCreateFailureFixture",
        ])
        app.buttons["morde.nav.menu"].tap()
        app.buttons["morde.nav.drawer.workItems"].tap()

        let projectPicker = app.buttons["morde.workItems.projectPicker"]
        XCTAssertTrue(projectPicker.waitForExistence(timeout: 10))
        projectPicker.tap()
        app.buttons["morde.workItems.project.j0000001"].tap()
        let create = app.buttons["morde.workItems.create.open"]
        XCTAssertTrue(create.waitForExistence(timeout: 10))
        create.tap()

        let title = app.textFields["morde.workItems.create.title"]
        XCTAssertTrue(title.waitForExistence(timeout: 10))
        title.tap()
        title.typeText("Keep this title after failure")
        let keyboardDone = app.buttons["morde.workItems.keyboardDone"]
        if keyboardDone.waitForExistence(timeout: 2) { keyboardDone.tap() }

        let submit = app.buttons["morde.workItems.create.submit"]
        XCTAssertTrue(waitForHittable(submit, timeout: 10))
        submit.tap()
        let error = app.staticTexts["morde.workItems.create.error"]
        XCTAssertTrue(error.waitForExistence(timeout: 15))
        XCTAssertTrue(app.textFields["morde.workItems.create.title"].exists,
                      "A failed create must leave the form open.")
        XCTAssertEqual(app.textFields["morde.workItems.create.title"].value as? String,
                       "Keep this title after failure")
    }

    func testMordeWorkItemListFailuresRecoverOnPullToRefreshWithoutRetryButtons() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localWorkItemFixture",
            "-morde.localWorkItemListFailureFixture",
        ])
        app.buttons["morde.nav.menu"].tap()
        app.buttons["morde.nav.drawer.workItems"].tap()

        let scroll = app.scrollViews["morde.workItems.scroll"]
        XCTAssertTrue(scroll.waitForExistence(timeout: 10))
        XCTAssertTrue(app.descendants(matching: .any)["morde.workItems.projects.loadError"]
            .waitForExistence(timeout: 15))
        let retryButtons = app.buttons.matching(
            NSPredicate(format: "label CONTAINS[c] 'retry' OR label CONTAINS '重试'")
        )
        XCTAssertEqual(retryButtons.count, 0, "Project list errors should not add a separate Retry button.")
        attachScreen(app, named: "MDA-08-project-list-failure-without-retry")

        let refreshStart = scroll.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.02))
        let refreshEnd = scroll.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.75))
        refreshStart.press(forDuration: 0.1, thenDragTo: refreshEnd)
        let projectPicker = app.buttons["morde.workItems.projectPicker"]
        XCTAssertTrue(projectPicker.waitForExistence(timeout: 20),
                      "Pull-to-refresh should recover the project list.")

        projectPicker.tap()
        app.buttons["morde.workItems.project.j0000001"].tap()
        XCTAssertTrue(app.staticTexts["morde.workItems.conversations.loadError"]
            .waitForExistence(timeout: 15))
        XCTAssertTrue(app.descendants(matching: .any)["morde.workItems.list.loadError"]
            .waitForExistence(timeout: 15))
        XCTAssertEqual(retryButtons.count, 0,
                       "Conversation and work-item list errors should rely on pull-to-refresh.")
        attachScreen(app, named: "MDA-08-conversation-and-work-item-failure-without-retry")

        let secondRefreshStart = scroll.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.02))
        let secondRefreshEnd = scroll.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.75))
        secondRefreshStart.press(forDuration: 0.1, thenDragTo: secondRefreshEnd)
        XCTAssertTrue(app.descendants(matching: .any)["morde.workItems.row.title.j1000001"]
            .waitForExistence(timeout: 20),
            "Pull-to-refresh should recover both the shared-conversation and work-item lists.")
        XCTAssertFalse(app.staticTexts["morde.workItems.conversations.loadError"].exists)
        XCTAssertFalse(app.descendants(matching: .any)["morde.workItems.list.loadError"].exists)
        XCTAssertEqual(retryButtons.count, 0)
    }

    func testMordeWorkItemCreateSubmitsToSelectedProject() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localWorkItemFixture"])
        app.buttons["morde.nav.menu"].tap()
        app.buttons["morde.nav.drawer.workItems"].tap()

        let projectPicker = app.buttons["morde.workItems.projectPicker"]
        XCTAssertTrue(projectPicker.waitForExistence(timeout: 10))
        projectPicker.tap()
        app.buttons["morde.workItems.project.j0000001"].tap()
        let create = app.buttons["morde.workItems.create.open"]
        XCTAssertTrue(create.waitForExistence(timeout: 10))
        create.tap()

        let title = app.textFields["morde.workItems.create.title"]
        XCTAssertTrue(title.waitForExistence(timeout: 10))
        title.tap()
        title.typeText("Created in the selected project")
        let description = app.descendants(matching: .any)["morde.workItems.create.description"]
        XCTAssertTrue(description.waitForExistence(timeout: 5))
        description.tap()
        description.typeText("Synthetic create submission proof")
        let keyboardDone = app.buttons["morde.workItems.keyboardDone"]
        if keyboardDone.waitForExistence(timeout: 2) { keyboardDone.tap() }

        let requestCategory = app.buttons["morde.workItems.create.category.request"]
        XCTAssertTrue(requestCategory.waitForExistence(timeout: 5))
        requestCategory.tap()
        let submit = app.buttons["morde.workItems.create.submit"]
        XCTAssertTrue(waitForHittable(submit, timeout: 10))
        submit.tap()

        let createdTitle = app.descendants(matching: .any)["morde.workItems.row.title.j1000003"]
        XCTAssertTrue(createdTitle.waitForExistence(timeout: 20),
                      "A successful create should add the returned work item to the selected project's list.")
        XCTAssertEqual(createdTitle.label, "Created in the selected project")
        XCTAssertTrue(app.staticTexts["Synthetic create submission proof"].exists,
                      "The optional description should be preserved in the created item.")
        XCTAssertFalse(app.textFields["morde.workItems.create.title"].exists,
                       "A successful create should dismiss the creation sheet.")
    }

    func testMordeTaskListDropsLatePageAfterAccountSwitch() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localAccountIsolationFixture",
            "-morde.localDelayedAccountResponseFixture",
        ], loginName: "account-a")
        app.buttons["morde.nav.tab.tasks"].tap()

        XCTAssertTrue(app.buttons["morde.tasks.row.q0000001"].waitForExistence(timeout: 15))
        XCTAssertTrue(app.descendants(matching: .any)["morde.tasks.loadingMore"].waitForExistence(timeout: 10),
                      "Account A's second page should be in flight before switching accounts.")

        app.buttons["morde.nav.menu"].tap()
        app.buttons["morde.nav.drawer.settings"].tap()
        let drawerScrim = app.buttons["morde.nav.drawer.scrim"]
        let drawerClosed = XCTNSPredicateExpectation(predicate: NSPredicate(format: "exists == false"),
                                                     object: drawerScrim)
        XCTAssertEqual(XCTWaiter.wait(for: [drawerClosed], timeout: 5), .completed,
                       "Wait for the navigation drawer to close before tapping a settings row.")
        let accountSettings = app.buttons["morde.settings.row.account"]
        XCTAssertTrue(waitForHittable(accountSettings, timeout: 10),
                      "Wait until the settings route has finished opening before choosing the account page.")
        accountSettings.coordinate(withNormalizedOffset: CGVector(dx: 0.92, dy: 0.5)).tap()
        XCTAssertTrue(app.staticTexts["账号"].waitForExistence(timeout: 10),
                      "The account detail page should be visible before signing out.")
        let signOut = app.buttons["morde.account.signOut"]
        let bottomTab = app.buttons["morde.nav.tab.approvals"]
        let accountList = app.collectionViews.firstMatch.exists ? app.collectionViews.firstMatch :
            (app.tables.firstMatch.exists ? app.tables.firstMatch : app.scrollViews.firstMatch)
        XCTAssertTrue(accountList.waitForExistence(timeout: 5), "The account settings list should be scrollable.")
        for _ in 0..<8 where signOut.frame.maxY >= bottomTab.frame.minY {
            accountList.swipeUp()
        }
        XCTAssertTrue(signOut.isHittable, "The sign-out action should be visible after scrolling the account list.")
        XCTAssertLessThan(signOut.frame.maxY, bottomTab.frame.minY,
                          "The sign-out action must sit above the custom bottom navigation before tapping.")
        signOut.tap()
        let confirmSignOut = app.descendants(matching: .any)
            .matching(identifier: "morde.account.signOut.confirm").firstMatch
        XCTAssertTrue(confirmSignOut.waitForExistence(timeout: 5),
                      "The native sign-out confirmation should be visible.")
        confirmSignOut.tap()

        let username = app.textFields["morde.auth.username"]
        XCTAssertTrue(username.waitForExistence(timeout: 10))
        username.tap()
        username.typeText("account-b")
        let password = app.secureTextFields["morde.auth.password"]
        password.tap()
        password.typeText("local-password")
        app.buttons["morde.auth.submit"].tap()
        XCTAssertTrue(app.buttons["morde.nav.menu"].waitForExistence(timeout: 20))
        app.buttons["morde.nav.tab.tasks"].tap()
        XCTAssertTrue(app.buttons["morde.tasks.row.q0000002"].waitForExistence(timeout: 15))
        XCTAssertFalse(app.buttons["morde.tasks.row.q0000001"].exists)

        // The fixture releases Account A's pending page after 12 seconds.
        let latePageReleased = expectation(description: "The delayed Account A page has been released")
        DispatchQueue.global().asyncAfter(deadline: .now() + 13) { latePageReleased.fulfill() }
        wait(for: [latePageReleased], timeout: 14)
        XCTAssertFalse(app.buttons["morde.tasks.row.q0000003"].exists,
                       "A late page from Account A must not appear in Account B's task list.")
        XCTAssertFalse(app.staticTexts["account-a-late-private-task"].exists)
    }

    func testMordeChatLongPressActionsOpenTaskOptionsAndImporter() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app)

        let actions = app.descendants(matching: .any)["morde.chat.actions"]
        XCTAssertTrue(actions.waitForExistence(timeout: 10))
        actions.press(forDuration: 1)
        let taskOptions = app.buttons["morde.chat.action.taskOptions"]
        XCTAssertTrue(taskOptions.waitForExistence(timeout: 5))
        XCTAssertTrue(app.buttons["morde.chat.action.voiceInput"].exists)
        XCTAssertTrue(app.buttons["morde.chat.action.attachments"].exists)
        taskOptions.tap()
        if !app.descendants(matching: .any)["morde.task.model"].exists {
            app.collectionViews.firstMatch.swipeUp()
        }
        XCTAssertTrue(app.descendants(matching: .any)["morde.task.model"].waitForExistence(timeout: 10))
        XCTAssertTrue(app.descendants(matching: .any)["morde.chat.taskTarget"].exists,
                      "The Task target selector remains available inside Task options.")
        app.buttons["morde.chat.taskOptions.done"].tap()

        actions.press(forDuration: 1)
        let addAttachment = app.buttons["morde.chat.action.attachments"]
        XCTAssertTrue(addAttachment.waitForExistence(timeout: 5))
        addAttachment.tap()
        let cancelEnglish = app.buttons["Cancel"]
        let cancelChinese = app.buttons["取消"]
        XCTAssertTrue(cancelEnglish.waitForExistence(timeout: 10) || cancelChinese.exists,
                      "The image/PDF document picker did not open")
    }

    func testMordeSettingsRoutesAndHonestUnavailableRows() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localHubFixture"])
        app.buttons["morde.nav.menu"].tap()
        app.buttons["设置"].tap()

        let back = app.buttons["返回"]
        let settingsTitle = app.staticTexts["设置"]
        XCTAssertTrue(back.waitForExistence(timeout: 5))
        XCTAssertEqual(back.frame.minX, 22, accuracy: 1)
        XCTAssertEqual(back.frame.width, 38, accuracy: 1)
        XCTAssertEqual(settingsTitle.frame.minX, back.frame.maxX + 10, accuracy: 1)
        XCTAssertEqual(app.buttons.matching(
            NSPredicate(format: "identifier BEGINSWITH %@", "morde.settings.row.")
        ).count, 9)
        XCTAssertFalse(app.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", "已用")).firstMatch.exists)
        let accountRow = app.buttons["morde.settings.row.account"]
        XCTAssertTrue(accountRow.exists)
        XCTAssertEqual(accountRow.frame.minX, 22, accuracy: 1)
        XCTAssertEqual(accountRow.frame.width, app.frame.width - 44, accuracy: 1)
        XCTAssertEqual(accountRow.frame.height, 45, accuracy: 3)

        accountRow.tap()
        XCTAssertTrue(app.staticTexts["账号"].waitForExistence(timeout: 10))
        XCTAssertTrue(app.staticTexts["服务地址"].exists)
        XCTAssertTrue(app.buttons["morde.account.manageInstallations"].exists)
        XCTAssertTrue(app.buttons["morde.account.signOut"].exists)
        attachScreen(app, named: "MDA-13-account-detail")
        app.buttons["返回"].tap()

        let devicesRow = app.buttons["morde.settings.row.devices"]
        XCTAssertTrue(devicesRow.waitForExistence(timeout: 5))
        devicesRow.tap()
        XCTAssertTrue(app.staticTexts["设备"].waitForExistence(timeout: 10))
        let relayURL = app.textFields["morde.hub.open.url"]
        XCTAssertTrue(relayURL.waitForExistence(timeout: 10))
        relayURL.tap()
        relayURL.typeText("https://example.com")
        let openOnDevice = app.buttons["morde.hub.open.request"]
        XCTAssertTrue(openOnDevice.waitForExistence(timeout: 10))
        openOnDevice.tap()
        let cancelRemoteOpen = app.buttons["morde.hub.open.cancel"].firstMatch
        XCTAssertTrue(cancelRemoteOpen.waitForExistence(timeout: 5))
        attachScreen(app, named: "MDA-13-device-detail")
        cancelRemoteOpen.tap()
        app.buttons["返回"].tap()

        for row in ["connectors", "permissions", "approvalRules", "notifications", "appearance"] {
            let settingsRow = app.buttons["morde.settings.row.\(row)"]
            XCTAssertTrue(settingsRow.waitForExistence(timeout: 5))
            XCTAssertTrue(waitForHittable(settingsRow), "Settings row \(row) is covered by a transition")
            settingsRow.tap()
            XCTAssertTrue(app.staticTexts["暂未实现"].waitForExistence(timeout: 5),
                          "Settings row \(row) did not open its unavailable state")
            app.buttons["返回"].tap()
            XCTAssertTrue(settingsRow.waitForExistence(timeout: 5))
        }

        app.buttons["morde.settings.row.about"].tap()
        XCTAssertTrue(app.staticTexts["Morde iOS"].waitForExistence(timeout: 5))
    }

    func testMordeGlobalExecutionControlConfirmsAndCanBeReenabled() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localExecutionFixture"])
        app.buttons["morde.nav.menu"].tap()
        app.buttons["设置"].tap()

        let execution = app.buttons["morde.settings.executionControl"]
        XCTAssertTrue(execution.waitForExistence(timeout: 10))
        let enabledLabel = NSPredicate(format: "label == %@ AND isEnabled == true", "急停全部任务")
        let enabled = XCTNSPredicateExpectation(predicate: enabledLabel, object: execution)
        XCTAssertEqual(XCTWaiter.wait(for: [enabled], timeout: 10), .completed)
        execution.tap()

        XCTAssertTrue(app.staticTexts["急停全部云端任务？"].waitForExistence(timeout: 5))
        let confirmations = app.buttons.matching(NSPredicate(format: "label == %@", "急停全部任务"))
        XCTAssertGreaterThan(confirmations.count, 1)
        confirmations.element(boundBy: confirmations.count - 1).tap()

        let reenableLabel = NSPredicate(format: "label == %@ AND isEnabled == true", "重新启用全部任务")
        let reenable = XCTNSPredicateExpectation(predicate: reenableLabel, object: execution)
        XCTAssertEqual(XCTWaiter.wait(for: [reenable], timeout: 20), .completed)
        execution.tap()
        let enabledAgain = XCTNSPredicateExpectation(predicate: enabledLabel, object: execution)
        XCTAssertEqual(XCTWaiter.wait(for: [enabledAgain], timeout: 10), .completed)
    }

    func testMordeResourceWorkspaceRouteBrowsesSyntheticManifest() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localResourceAcceptanceFixture",
                                                   "-morde.localWorkspaceReadOnlyFixture",
                                                   "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"])
        app.buttons["morde.nav.tab.resources"].tap()

        let workspace = app.buttons["morde.resources.workspace.open.w0000001"]
        XCTAssertTrue(workspace.waitForExistence(timeout: 10))
        XCTAssertTrue(workspace.label.contains("已挂载"), "A ready Workspace with a healthy Host binding shows its mounted state.")
        XCTAssertTrue(workspace.label.contains("Running host"), "The mounted label names its actual bound Host.")
        let syncingWorkspace = app.buttons["morde.resources.workspace.open.w0000002"]
        XCTAssertTrue(syncingWorkspace.waitForExistence(timeout: 5))
        XCTAssertTrue(syncingWorkspace.label.contains("同步中"), "An active but syncing binding keeps its real status.")
        XCTAssertTrue(syncingWorkspace.label.contains("Unready binding host"), "The syncing row keeps its actual Host name.")
        XCTAssertFalse(syncingWorkspace.label.contains("已挂载"))
        let unavailableWorkspace = app.buttons["morde.resources.workspace.open.w0000003"]
        XCTAssertTrue(unavailableWorkspace.exists)
        XCTAssertTrue(unavailableWorkspace.label.contains("不可用"), "A Workspace the API reports as syncing stays unavailable.")
        XCTAssertFalse(unavailableWorkspace.label.contains("已挂载"))
        XCTAssertFalse(unavailableWorkspace.label.contains("Unready binding host"), "No Host name is shown without a binding.")
        let unboundWorkspace = app.buttons["morde.resources.workspace.open.w0000005"]
        XCTAssertTrue(unboundWorkspace.exists)
        XCTAssertTrue(unboundWorkspace.label.contains("未挂载"), "A ready Workspace without a Host binding stays unmounted.")
        XCTAssertFalse(unboundWorkspace.label.contains("已挂载"))
        XCTAssertFalse(unboundWorkspace.label.contains("Running host"), "An unbound Workspace does not invent a Host.")
        let host = app.buttons["morde.resources.host.open.h0000001"]
        XCTAssertTrue(host.exists)
        XCTAssertTrue(app.staticTexts["云端主机"].exists)
        XCTAssertFalse(app.staticTexts["云端电脑"].exists)
        let cloudPhoneStatus = app.descendants(matching: .any)["morde.resources.empty.cloudPhones"]
        XCTAssertTrue(cloudPhoneStatus.exists,
                      "Client device records must not be presented as cloud phone runtime entries.")
        XCTAssertFalse(app.staticTexts["云手机"].exists)
        XCTAssertFalse(app.staticTexts["Cloud phones"].exists)
        XCTAssertLessThan(host.frame.minY, cloudPhoneStatus.frame.minY)
        XCTAssertLessThan(cloudPhoneStatus.frame.minY, workspace.frame.minY,
                          "The unavailable status stays between Host and Workspace content without an extra heading.")
        XCTAssertTrue(app.descendants(matching: .any)["morde.resources.empty.aiTools"].exists,
                      "Connector metadata must not be presented as live AI tool status.")
        let resourceScroll = app.scrollViews["morde.resources.scroll"]
        XCTAssertTrue(resourceScroll.waitForExistence(timeout: 5))
        attachScreen(app, named: "MDA-07 resources initial viewport")
        for _ in 0..<10 {
            guard !workspace.isHittable || !syncingWorkspace.isHittable else { break }
            resourceScroll.swipeUp()
        }
        XCTAssertTrue(workspace.isHittable && syncingWorkspace.isHittable,
                      "The screenshot should show the mounted and syncing Workspace rows.")
        attachScreen(app, named: "MDA-07 mounted and syncing Workspace status rows")
        let aiToolsEmpty = app.descendants(matching: .any)["morde.resources.empty.aiTools"]
        for _ in 0..<12 { resourceScroll.swipeUp() }
        XCTAssertTrue(aiToolsEmpty.isHittable, "The last resource section should be visible at the content end.")
        let tabBarButton = app.buttons["morde.nav.tab.chat"]
        XCTAssertEqual(tabBarButton.frame.minY - 5 - aiToolsEmpty.frame.maxY, 30, accuracy: 3,
                       "The final resource row should sit 30 pt above the floating tab bar capsule.")
        attachScreen(app, named: "MDA-07 resources at final-row tail gap")
        for _ in 0..<12 where !workspace.isHittable { resourceScroll.swipeDown() }
        XCTAssertTrue(workspace.isHittable, "The Workspace row should return to view when scrolling back up.")
        let addResource = app.buttons["morde.resources.add"]
        XCTAssertTrue(addResource.exists)
        XCTAssertTrue(addResource.isEnabled)
        XCTAssertEqual(addResource.label, "添加资源")
        addResource.tap()
        let addUnavailable = app.staticTexts["morde.resources.add.unavailable"]
        XCTAssertTrue(addUnavailable.waitForExistence(timeout: 5))
        XCTAssertEqual(addUnavailable.label, "暂未实现")
        XCTAssertLessThan(addUnavailable.frame.midY, app.frame.midY,
                          "Transient add feedback should appear near the header, as in the HTML prototype.")
        attachScreen(app, named: "MDA-07 resource add transient top toast")
        XCTAssertFalse(app.alerts.firstMatch.exists,
                       "The prototype add action is transient feedback and must not add a dismissal button.")
        workspace.tap()

        let workspaceFiles = app.scrollViews["morde.workspace.files"]
        XCTAssertTrue(workspaceFiles.waitForExistence(timeout: 10))
        let workspaceTitle = app.descendants(matching: .any)["morde.workspace.title"]
        XCTAssertTrue(workspaceTitle.waitForExistence(timeout: 10))
        XCTAssertFalse(app.pickers["Workspace"].exists)
        XCTAssertFalse(app.buttons["选择文件"].exists)
        XCTAssertFalse(app.buttons["比较来源版本与当前版本"].exists)
        attachScreen(app, named: "MDA-14 Workspace root title and file row spacing")

        workspaceTitle.press(forDuration: 1)
        let uploadMenuItem = app.buttons["上传文件"]
        let historyMenuItem = app.buttons["版本比较与恢复"]
        XCTAssertTrue(uploadMenuItem.waitForExistence(timeout: 5))
        XCTAssertTrue(historyMenuItem.exists)
        uploadMenuItem.tap()
        XCTAssertTrue(app.buttons["选择文件"].waitForExistence(timeout: 5))
        app.buttons["完成"].tap()

        workspaceTitle.press(forDuration: 1)
        let openHistory = app.buttons["版本比较与恢复"]
        XCTAssertTrue(openHistory.waitForExistence(timeout: 5))
        openHistory.tap()
        XCTAssertTrue(app.textFields["恢复来源版本号"].waitForExistence(timeout: 5))
        app.buttons["完成"].tap()

        workspaceFiles.swipeUp()
        XCTAssertTrue(app.staticTexts["README.md"].waitForExistence(timeout: 10))
        app.buttons["morde.workspace.directory.docs"].tap()
        workspaceFiles.swipeUp()
        XCTAssertTrue(app.staticTexts["guide.md"].waitForExistence(timeout: 10))
        app.buttons["morde.workspace.directory.docs/empty"].tap()
        XCTAssertTrue(app.staticTexts["此目录当前没有文件。"].waitForExistence(timeout: 10))
        app.buttons["morde.workspace.breadcrumb.parent"].tap()
        XCTAssertTrue(app.staticTexts["guide.md"].waitForExistence(timeout: 10))
        app.buttons["morde.workspace.breadcrumb.parent"].tap()
        XCTAssertTrue(app.staticTexts["README.md"].waitForExistence(timeout: 10))
        app.buttons["返回资源"].tap()
        XCTAssertTrue(app.buttons["morde.nav.tab.resources"].waitForExistence(timeout: 5))
    }

    func testMordeWorkspaceFileActionsStayInRowMenu() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localResourceAcceptanceFixture"])
        app.buttons["morde.nav.tab.resources"].tap()
        app.buttons["morde.resources.workspace.open.w0000001"].tap()

        let fileActions = app.buttons["README.md 的操作"]
        XCTAssertTrue(fileActions.waitForExistence(timeout: 10))
        let workspaceScreenshot = XCTAttachment(screenshot: app.screenshot())
        workspaceScreenshot.name = "Morde Workspace file list"
        workspaceScreenshot.lifetime = .keepAlways
        add(workspaceScreenshot)
        XCTAssertFalse(app.buttons["下载并校验"].exists,
                       "File actions should stay inside the row menu instead of adding persistent row controls.")
        XCTAssertFalse(app.buttons["重命名"].exists)
        XCTAssertFalse(app.buttons["移动"].exists)
        XCTAssertFalse(app.buttons["删除云端文件"].exists)

        fileActions.tap()
        let download = app.buttons["下载并校验"]
        let rename = app.buttons["重命名"]
        let move = app.buttons["移动"]
        let delete = app.buttons["删除云端文件"]
        XCTAssertTrue(download.waitForExistence(timeout: 5))
        XCTAssertTrue(rename.exists)
        XCTAssertTrue(move.exists)
        XCTAssertTrue(delete.exists)
        XCTAssertTrue(download.isEnabled)
        XCTAssertTrue(rename.isEnabled)
        XCTAssertTrue(move.isEnabled)
        XCTAssertTrue(delete.isEnabled)

        rename.tap()
        let renamePrompt = app.alerts.firstMatch
        XCTAssertTrue(renamePrompt.waitForExistence(timeout: 5))
        XCTAssertTrue(renamePrompt.textFields["新文件名"].exists)
        renamePrompt.buttons["取消"].tap()

        fileActions.tap()
        move.tap()
        let movePrompt = app.alerts.firstMatch
        XCTAssertTrue(movePrompt.waitForExistence(timeout: 5))
        XCTAssertTrue(movePrompt.textFields["目标相对路径"].exists)
        movePrompt.buttons["取消"].tap()

        fileActions.tap()
        delete.tap()
        let deleteConfirmation = app.buttons["删除并提交新检查点"]
        XCTAssertTrue(deleteConfirmation.waitForExistence(timeout: 5))
    }

    func testMordeWorkspaceRejectsUnsafeManifestPaths() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localResourceAcceptanceFixture",
                                                   "-morde.localWorkspaceInvalidManifestFixture"])
        app.buttons["morde.nav.tab.resources"].tap()
        app.buttons["morde.resources.workspace.open.w0000001"].tap()

        let manifestError = app.staticTexts.matching(
            NSPredicate(format: "label BEGINSWITH %@", "文件清单暂不可用：")
        ).firstMatch
        XCTAssertTrue(manifestError.waitForExistence(timeout: 10))
        XCTAssertFalse(app.staticTexts["escape.md"].exists,
                       "An unsafe manifest path must not be rendered as a file row.")
        XCTAssertFalse(app.buttons["README.md 的操作"].exists)
        XCTAssertFalse(app.buttons["Retry"].exists)
        XCTAssertTrue(app.buttons["返回资源"].exists)
    }

    func testMordeWorkspaceRejectsFileDirectoryPathConflicts() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localResourceAcceptanceFixture",
                                                   "-morde.localWorkspacePathConflictFixture"])
        app.buttons["morde.nav.tab.resources"].tap()
        app.buttons["morde.resources.workspace.open.w0000001"].tap()

        let manifestError = app.staticTexts.matching(
            NSPredicate(format: "label BEGINSWITH %@", "文件清单暂不可用：")
        ).firstMatch
        XCTAssertTrue(manifestError.waitForExistence(timeout: 10))
        XCTAssertFalse(app.staticTexts["docs"].exists,
                       "A file path cannot also be the parent of a manifest directory.")
        XCTAssertFalse(app.buttons["morde.workspace.directory.docs"].exists)
        XCTAssertTrue(app.buttons["返回资源"].exists)
    }

    func testMordeWorkspaceStopsRepeatedManifestCursor() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localResourceAcceptanceFixture",
                                                   "-morde.localWorkspaceManifestCursorLoopFixture"])
        app.buttons["morde.nav.tab.resources"].tap()
        app.buttons["morde.resources.workspace.open.w0000001"].tap()

        XCTAssertTrue(app.staticTexts["README.md"].waitForExistence(timeout: 10))
        let manifestError = app.staticTexts.matching(
            NSPredicate(format: "label BEGINSWITH %@", "文件清单暂不可用：")
        ).firstMatch
        XCTAssertTrue(manifestError.waitForExistence(timeout: 10))
        XCTAssertFalse(app.staticTexts["second-page.md"].exists,
                       "A repeated cursor page must not be appended or retried forever.")
        XCTAssertTrue(app.buttons["返回资源"].exists)
    }

    func testMordeWorkspaceShareCreatesLinkWithSelectedPermission() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localResourceAcceptanceFixture",
                                                   "-morde.localWorkspaceShareFixture"])
        app.buttons["morde.nav.tab.resources"].tap()
        app.buttons["morde.resources.workspace.open.w0000001"].tap()
        app.buttons["morde.workspace.share"].tap()

        XCTAssertTrue(app.staticTexts["分享「Ready Workspace」"].waitForExistence(timeout: 5))
        let editPermission = app.buttons["morde.workspace.share.permission.edit"]
        XCTAssertTrue(editPermission.waitForExistence(timeout: 5))
        editPermission.tap()
        XCTAssertEqual(editPermission.value as? String, "selected")
        app.buttons["morde.workspace.share.copy"].tap()
        XCTAssertTrue(app.staticTexts["morde.workspace.share.copied"].waitForExistence(timeout: 10))
    }

    func testMordeWorkspaceShareDeepLinkClaimsPermanentIndependentCopy() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        let springboard = XCUIApplication(bundleIdentifier: "com.apple.springboard")
        let stalePrompt = springboard.alerts.firstMatch
        if stalePrompt.waitForExistence(timeout: 1) {
            stalePrompt.buttons["取消"].tap()
        }
        launchLocalChat(app, additionalArguments: ["-morde.localResourceAcceptanceFixture",
                                                   "-morde.localWorkspaceShareFixture",
                                                   "-morde.localWorkspaceShareDeepLinkFixture"])

        let acceptCopy = app.buttons["morde.workspace.share.accept"]
        XCTAssertTrue(acceptCopy.waitForExistence(timeout: 20), "The share deep link should present the recipient claim flow.")
        XCTAssertTrue(app.staticTexts["Ready Workspace"].exists)
        XCTAssertTrue(app.staticTexts["可查看 · 只能看文件"].exists)
        XCTAssertTrue(app.staticTexts["领取后会创建归你所有的独立副本，之后与原 Workspace 无关。"].exists)
        acceptCopy.tap()

        let resources = app.buttons["morde.nav.tab.resources"]
        XCTAssertTrue(resources.waitForExistence(timeout: 10))
        resources.tap()
        let acceptedCopy = app.buttons["morde.resources.workspace.open.w0000004"]
        XCTAssertTrue(acceptedCopy.waitForExistence(timeout: 20),
                      "Accepting a link should add the independent copy to the recipient's Workspace list.")
    }

    func testMordeWorkspaceEditShareDeepLinkClaimsEditableIndependentCopy() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        let springboard = XCUIApplication(bundleIdentifier: "com.apple.springboard")
        let stalePrompt = springboard.alerts.firstMatch
        if stalePrompt.waitForExistence(timeout: 1) {
            stalePrompt.buttons["取消"].tap()
        }
        launchLocalChat(app, additionalArguments: ["-morde.localResourceAcceptanceFixture",
                                                   "-morde.localWorkspaceShareFixture",
                                                   "-morde.localWorkspaceShareEditFixture",
                                                   "-morde.localWorkspaceShareDeepLinkFixture"])

        let acceptCopy = app.buttons["morde.workspace.share.accept"]
        XCTAssertTrue(acceptCopy.waitForExistence(timeout: 20), "The edit-share deep link should present the recipient claim flow.")
        XCTAssertTrue(app.staticTexts["Ready Workspace"].exists)
        XCTAssertTrue(app.staticTexts["可编辑 · 能改文件"].exists)
        XCTAssertTrue(app.staticTexts["领取后会创建归你所有的独立副本，之后与原 Workspace 无关。"].exists)
        acceptCopy.tap()

        let resources = app.buttons["morde.nav.tab.resources"]
        XCTAssertTrue(resources.waitForExistence(timeout: 10))
        resources.tap()
        let original = app.buttons["morde.resources.workspace.open.w0000001"]
        let acceptedCopy = app.buttons["morde.resources.workspace.open.w0000004"]
        XCTAssertTrue(original.waitForExistence(timeout: 20), "The original Workspace remains distinct from the claimed copy.")
        XCTAssertTrue(acceptedCopy.waitForExistence(timeout: 20),
                      "Accepting an edit link should add a separate Workspace copy to the recipient's list.")
        XCTAssertTrue(acceptedCopy.label.contains("Ready Workspace copy"))
        XCTAssertFalse(acceptedCopy.label.contains("只读"), "An edit-permission copy must not be marked read-only.")
        XCTAssertNotEqual(original.identifier, acceptedCopy.identifier,
                          "The accepted copy must have its own Workspace identity.")

        acceptedCopy.tap()
        XCTAssertTrue(app.staticTexts["Ready Workspace copy"].waitForExistence(timeout: 10))
        let fileActions = app.buttons["README.md 的操作"]
        XCTAssertTrue(fileActions.waitForExistence(timeout: 10))
        fileActions.tap()
        XCTAssertTrue(app.buttons["重命名"].waitForExistence(timeout: 5))
        XCTAssertTrue(app.buttons["重命名"].isEnabled)
        XCTAssertTrue(app.buttons["移动"].isEnabled)
        XCTAssertTrue(app.buttons["删除云端文件"].isEnabled,
                      "The accepted edit copy should allow Workspace file mutations.")
    }

    func testMordeWorkspaceSharePreviewNotFoundShowsInvalidLinkError() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localResourceAcceptanceFixture",
            "-morde.localWorkspaceShareFixture",
            "-morde.localWorkspaceShareDeepLinkFixture",
            "-morde.localWorkspaceSharePreviewNotFoundFixture",
        ])

        let invalidLink = app.staticTexts["此链接已失效或已被领取，请向分享者索取新链接。"]
        XCTAssertTrue(invalidLink.waitForExistence(timeout: 20))
        XCTAssertFalse(app.buttons["morde.workspace.share.accept"].exists,
                       "A not-found preview must not leave a claim action available.")
        XCTAssertFalse(app.staticTexts["Ready Workspace copy"].exists,
                       "A missing share link must not be shown as an accepted copy.")
        attachScreen(app, named: "MDA-14-share-preview-not-found-error")
    }

    func testMordeWorkspaceShareAcceptNotFoundDoesNotReportSuccess() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localResourceAcceptanceFixture",
            "-morde.localWorkspaceShareFixture",
            "-morde.localWorkspaceShareDeepLinkFixture",
            "-morde.localWorkspaceShareAcceptNotFoundFixture",
        ])

        let accept = app.buttons["morde.workspace.share.accept"]
        XCTAssertTrue(accept.waitForExistence(timeout: 20))
        accept.tap()

        let invalidLink = app.staticTexts["此链接已失效或已被领取，请向分享者索取新链接。"]
        XCTAssertTrue(invalidLink.waitForExistence(timeout: 10))
        XCTAssertFalse(app.buttons["morde.workspace.share.accept"].exists,
                       "A not-found acceptance must not leave a claim action available.")
        XCTAssertFalse(app.staticTexts["Ready Workspace copy"].exists,
                       "A rejected claim must not be shown as an accepted copy.")
        attachScreen(app, named: "MDA-14-share-accept-not-found-error")
    }

    func testMordeWorkspaceSharePreviewNetworkFailureShowsError() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localResourceAcceptanceFixture",
            "-morde.localWorkspaceShareFixture",
            "-morde.localWorkspaceShareDeepLinkFixture",
            "-morde.localWorkspaceSharePreviewNetworkFailureFixture",
        ])

        let error = app.staticTexts["Synthetic Workspace share preview network failure"]
        XCTAssertTrue(error.waitForExistence(timeout: 20))
        XCTAssertFalse(app.buttons["morde.workspace.share.accept"].exists,
                       "A failed preview request must not offer a claim action.")
        XCTAssertFalse(app.staticTexts["Ready Workspace copy"].exists,
                       "A failed preview request must not be shown as an accepted copy.")
        attachScreen(app, named: "MDA-14-share-preview-network-error")
    }

    func testMordeWorkspaceShareAcceptNetworkFailureShowsErrorWithoutSuccess() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localResourceAcceptanceFixture",
            "-morde.localWorkspaceShareFixture",
            "-morde.localWorkspaceShareDeepLinkFixture",
            "-morde.localWorkspaceShareAcceptNetworkFailureFixture",
        ])

        let accept = app.buttons["morde.workspace.share.accept"]
        XCTAssertTrue(accept.waitForExistence(timeout: 20))
        accept.tap()

        let error = app.staticTexts["Synthetic Workspace share accept network failure"]
        XCTAssertTrue(error.waitForExistence(timeout: 10))
        XCTAssertFalse(app.buttons["morde.workspace.share.accept"].exists,
                       "A failed claim response must not leave an active claim action.")
        XCTAssertFalse(app.staticTexts["Ready Workspace copy"].exists,
                       "A failed claim response must not be shown as an accepted copy.")
        attachScreen(app, named: "MDA-14-share-accept-network-error")
    }

    func testMordeReadOnlyWorkspaceDisablesCloudMutations() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localResourceAcceptanceFixture",
                                                   "-morde.localWorkspaceReadOnlyFixture"])
        app.buttons["morde.nav.tab.resources"].tap()
        app.buttons["morde.resources.workspace.open.w0000005"].tap()

        let fileActions = app.buttons["README.md 的操作"]
        XCTAssertTrue(fileActions.waitForExistence(timeout: 10))
        fileActions.tap()
        let download = app.buttons["下载并校验"]
        let rename = app.buttons["重命名"]
        let move = app.buttons["移动"]
        let delete = app.buttons["删除云端文件"]
        XCTAssertTrue(download.waitForExistence(timeout: 5))
        XCTAssertTrue(download.isEnabled, "Read-only workspaces allow reading visible file content.")
        XCTAssertFalse(rename.isEnabled)
        XCTAssertFalse(move.isEnabled)
        XCTAssertFalse(delete.isEnabled)
    }

    func testMordeResourcePermissionMessageRequiresRunningSharedHost() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localResourceAcceptanceFixture"])
        app.buttons["morde.nav.tab.resources"].tap()

        let runningWithoutPermission = app.descendants(matching: .any)["morde.resources.host.h0000009"]
        XCTAssertTrue(runningWithoutPermission.waitForExistence(timeout: 15))
        XCTAssertTrue(runningWithoutPermission.staticTexts["你没有权限查看此电脑。"].exists)

        let stoppedWithoutPermission = app.descendants(matching: .any)["morde.resources.host.h0000010"]
        XCTAssertTrue(stoppedWithoutPermission.waitForExistence(timeout: 15))
        XCTAssertEqual(app.staticTexts["morde.resources.host.status.h0000010"].label, "已停止")
        XCTAssertFalse(stoppedWithoutPermission.staticTexts["你没有权限查看此电脑。"].exists,
                       "A stopped host should show its real state, not a permission failure.")
    }

    func testMordeResourceHostSharingUsesOwnerLongPressAndMemberAPI() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localResourceAcceptanceFixture"])
        app.buttons["morde.nav.tab.resources"].tap()

        let host = app.buttons["morde.resources.host.open.h0000001"]
        XCTAssertTrue(host.waitForExistence(timeout: 15))
        let share = app.buttons["morde.resources.host.share.h0000001"]
        XCTAssertFalse(share.exists, "Host sharing should not add a permanent row button.")
        host.press(forDuration: 1)
        XCTAssertTrue(share.waitForExistence(timeout: 5))
        share.tap()

        XCTAssertTrue(app.staticTexts["u7654321"].waitForExistence(timeout: 10))
        XCTAssertTrue(app.staticTexts["morde.hostShare.leasePolicy"].exists,
                      "The sheet should explain that permission lasts until owner revocation while each control lease remains limited.")
        let recipient = app.textFields["morde.hostShare.userID"]
        recipient.tap()
        recipient.typeText("u1234567")
        app.buttons["morde.hostShare.grant"].tap()
        XCTAssertTrue(app.staticTexts["u1234567"].waitForExistence(timeout: 10))
        let permissions = app.staticTexts["morde.hostShare.member.permissions.u1234567"]
        XCTAssertTrue(permissions.waitForExistence(timeout: 10))
        XCTAssertTrue(permissions.label.localizedCaseInsensitiveContains("control") || permissions.label.contains("控制"),
                      "The member list should show that this recipient has desktop-control permission.")

        app.buttons["morde.hostShare.revoke.u1234567"].tap()
        let revoke = app.buttons.matching(identifier: "morde.hostShare.confirmRevoke").firstMatch
        XCTAssertTrue(revoke.waitForExistence(timeout: 5))
        revoke.tap()
        let removedRecipient = app.staticTexts["u1234567"]
        let removed = XCTNSPredicateExpectation(predicate: NSPredicate(format: "exists == false"), object: removedRecipient)
        XCTAssertEqual(XCTWaiter.wait(for: [removed], timeout: 10), .completed)
    }

    func testMordeResourceOrdinaryStateMatchesDesignDensity() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localResourceAcceptanceFixture",
            "-morde.localResourceDesignVisualFixture",
            "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN",
        ])
        app.buttons["morde.nav.tab.resources"].tap()

        let host = app.buttons["morde.resources.host.open.h0000001"]
        XCTAssertTrue(host.waitForExistence(timeout: 15))
        XCTAssertTrue(host.label.contains("PC-01"))
        XCTAssertTrue(host.label.contains("morde 仓库"))
        XCTAssertEqual(app.staticTexts["morde.resources.host.status.h0000001"].label, "运行中")
        XCTAssertFalse(app.buttons["morde.resources.host.open.h0000002"].exists,
                       "This visual fixture intentionally shows one ordinary Host state.")
        XCTAssertTrue(app.buttons["morde.resources.workspace.open.w0000001"].exists)
        XCTAssertTrue(app.descendants(matching: .any)["morde.resources.empty.cloudPhones"].exists)
        XCTAssertTrue(app.descendants(matching: .any)["morde.resources.empty.aiTools"].exists)
        XCTAssertEqual(app.staticTexts.matching(NSPredicate(format: "label CONTAINS[c] %@", "剩余")).count, 0,
                       "A Host row must not display a lease or control countdown.")
        attachScreen(app, named: "MDA-07 ordinary resource state")
    }

    func testMordeResourceHostLifecycleStatesMatchServer() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localResourceAcceptanceFixture"])
        app.buttons["morde.nav.tab.resources"].tap()

        for (hostID, expectedStatus) in [
            ("h0000011", "启动中"),
            ("h0000012", "停止中"),
            ("h0000013", "失败"),
            ("h0000014", "删除中"),
        ] {
            let status = app.staticTexts["morde.resources.host.status.\(hostID)"]
            XCTAssertTrue(status.waitForExistence(timeout: 15), "Missing host status for \(hostID)")
            XCTAssertEqual(status.label, expectedStatus)
        }
    }

    func testMordeResourcePullToRefreshRecoversAfterListFailure() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localResourceRefreshRecoveryFixture"])
        app.buttons["morde.nav.tab.resources"].tap()

        let refreshError = app.descendants(matching: .any)["morde.resources.loadError"]
        XCTAssertTrue(refreshError.waitForExistence(timeout: 15))
        let workspace = app.buttons["morde.resources.workspace.open.w0000001"]
        XCTAssertFalse(workspace.exists)
        let resourceList = app.scrollViews["morde.resources.scroll"]
        XCTAssertTrue(resourceList.exists)
        let pullStart = resourceList.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.08))
        let pullEnd = resourceList.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.85))
        pullStart.press(forDuration: 0.1, thenDragTo: pullEnd)
        XCTAssertTrue(workspace.waitForExistence(timeout: 20))
        XCTAssertFalse(refreshError.exists)
    }

    func testMordeResourcePreferenceFailureKeepsConfirmedEmptySections() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localResourceConfirmedEmptyFixture",
            "-morde.localResourceDefaultHostPreferenceFailureFixture",
        ])
        app.buttons["morde.nav.tab.resources"].tap()

        XCTAssertTrue(app.descendants(matching: .any)["morde.resources.empty.cloudComputers"]
            .waitForExistence(timeout: 15))
        XCTAssertTrue(app.descendants(matching: .any)["morde.resources.empty.workspaces"].exists)
        XCTAssertTrue(app.descendants(matching: .any)["morde.resources.empty.cloudPhones"].exists)
        XCTAssertTrue(app.descendants(matching: .any)["morde.resources.empty.aiTools"].exists)
        XCTAssertFalse(app.alerts["请求失败"].exists)
    }

    func testMordeComputerRouteAcceptsSyntheticTicketAndReturnsToResources() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localResourceAcceptanceFixture",
            "-morde.localHostWorkbenchFixture",
            "-morde.localResourceUserControlFixture",
        ])
        app.buttons["morde.nav.tab.resources"].tap()

        let host = app.buttons["morde.resources.host.open.h0000001"]
        XCTAssertTrue(host.waitForExistence(timeout: 10))
        host.tap()

        XCTAssertTrue(app.buttons["morde.computer.close"].waitForExistence(timeout: 10))
        let title = app.staticTexts["morde.computer.title"]
        XCTAssertTrue(title.waitForExistence(timeout: 10))
        XCTAssertEqual(title.label, "全由你掌控")
        let subtitle = app.staticTexts["morde.computer.subtitle"]
        XCTAssertTrue(subtitle.exists)
        XCTAssertTrue(subtitle.label.contains("Running host · Ready Workspace"))
        let workbench = app.descendants(matching: .any)["morde.computer.webview"]
        XCTAssertTrue(workbench.waitForExistence(timeout: 10))
        XCTAssertTrue(workbench.staticTexts["新标签页"].waitForExistence(timeout: 20),
                      "The local Workbench fixture should load before checking the current control badge.")
        XCTAssertTrue(app.staticTexts["由你控制"].waitForExistence(timeout: 5))
        XCTAssertTrue(app.buttons["morde.computer.capture"].exists)
        XCTAssertTrue(app.buttons["morde.computer.stopTask"].exists)
        XCTAssertFalse(app.buttons["morde.computer.stopTask"].isEnabled)
        XCTAssertTrue(app.buttons["morde.computer.controlAction"].exists)
        attachScreen(app, named: "MDA-15 concise cloud computer header")

        app.buttons["morde.computer.close"].tap()
        XCTAssertTrue(app.buttons["morde.nav.tab.resources"].waitForExistence(timeout: 5))
        XCTAssertTrue(app.buttons["morde.resources.host.open.h0000001"].exists)

        let sharedHost = app.buttons["morde.resources.host.open.h0000008"]
        XCTAssertTrue(sharedHost.waitForExistence(timeout: 10))
        sharedHost.tap()
        XCTAssertTrue(app.staticTexts["morde.computer.subtitle"].waitForExistence(timeout: 10))
        XCTAssertTrue(app.staticTexts["morde.computer.subtitle"].label.contains("Shared read-only host"))
        XCTAssertTrue(app.descendants(matching: .any)["morde.computer.webview"].waitForExistence(timeout: 10))
        app.buttons["morde.computer.close"].tap()
        XCTAssertTrue(app.buttons["morde.resources.host.open.h0000008"].waitForExistence(timeout: 5))

    }

    func testMordeComputerIOSWorkbenchKeepsOnlyDesignTabsAndConciseStatus() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localResourceAcceptanceFixture",
            "-morde.localWorkspaceDesignVisualFixture",
            "-morde.localHostWorkbenchFixture",
        ])
        app.buttons["morde.nav.tab.resources"].tap()

        let host = app.buttons["morde.resources.host.open.h0000001"]
        XCTAssertTrue(host.waitForExistence(timeout: 10))
        host.tap()

        let workbench = app.descendants(matching: .any)["morde.computer.webview"]
        XCTAssertTrue(workbench.waitForExistence(timeout: 10))
        let browser = workbench.buttons["浏览器"]
        let terminal = workbench.buttons["终端"]
        let files = workbench.buttons["文件"]
        XCTAssertTrue(browser.waitForExistence(timeout: 20),
                      "The local synthetic WKWebView fixture should finish loading before its tabs are checked.")
        XCTAssertTrue(terminal.exists)
        XCTAssertTrue(files.exists)
        XCTAssertTrue(workbench.staticTexts["新标签页"].exists,
                       "The fixture's rendered page content confirms the WKWebView completed navigation.")
        XCTAssertTrue(app.staticTexts["morde.computer.subtitle"].label.contains("PC-01 · morde 仓库"),
                      "Use the design稿's paired Host and Workspace names when comparing the compact header.")

        XCTAssertFalse(workbench.buttons["Workspace"].exists,
                       "Workspace should not occupy a permanent tab in the iOS workbench shell.")
        XCTAssertFalse(workbench.buttons["VS Code"].exists,
                       "VS Code should not occupy a permanent tab in the iOS workbench shell.")
        XCTAssertFalse(workbench.buttons["续租"].exists,
                       "The compact iOS workbench should not show a renew button.")
        XCTAssertEqual(workbench.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", "剩余")).count, 0,
                       "The compact iOS workbench should not show a remaining-time countdown.")
        XCTAssertFalse(workbench.staticTexts["你持有电脑操作权"].exists)
        XCTAssertFalse(workbench.staticTexts["桌面画面可能显示此电脑上的文件，获准查看桌面的成员也能看到。"].exists,
                       "The desktop-sharing explanation should not appear as persistent page copy.")
        attachScreen(app, named: "MDA-15 loaded compact cloud computer")
    }

    func testMordeComputerCapturePreviewOpensAfterWorkbenchLoads() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localResourceAcceptanceFixture",
            "-morde.localHostWorkbenchFixture",
        ])
        app.buttons["morde.nav.tab.resources"].tap()

        let host = app.buttons["morde.resources.host.open.h0000001"]
        XCTAssertTrue(host.waitForExistence(timeout: 10))
        host.tap()

        let workbench = app.descendants(matching: .any)["morde.computer.webview"]
        XCTAssertTrue(workbench.waitForExistence(timeout: 10))
        XCTAssertTrue(workbench.staticTexts["新标签页"].waitForExistence(timeout: 20),
                      "The screenshot should capture rendered Workbench content.")
        app.buttons["morde.computer.capture"].tap()

        let preview = app.descendants(matching: .any)["morde.computer.screenshot.preview.sheet"]
        XCTAssertTrue(preview.waitForExistence(timeout: 15),
                      "Capturing the loaded WebView should present its image preview.")
        XCTAssertTrue(app.descendants(matching: .any)["morde.computer.screenshot.preview.image"]
            .waitForExistence(timeout: 5),
                      "The preview sheet should contain the captured Workbench image.")
        attachScreen(app, named: "MDA-15 captured Workbench preview")
    }

    func testMordeComputerControlStatusErrorHasNoRetryButtonAndRecoversOnReentry() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localResourceAcceptanceFixture",
            "-morde.localResourceControlErrorFixture",
        ])
        app.buttons["morde.nav.tab.resources"].tap()

        let host = app.buttons["morde.resources.host.open.h0000001"]
        XCTAssertTrue(host.waitForExistence(timeout: 10))
        XCTAssertTrue(app.staticTexts["morde.resources.host.controlError.h0000001"]
            .waitForExistence(timeout: 15), "The first control-status request should fail in the local fixture.")
        host.tap()

        XCTAssertTrue(app.buttons["morde.computer.close"].waitForExistence(timeout: 10))
        XCTAssertEqual(app.staticTexts["morde.computer.title"].waitForExistence(timeout: 10), true)
        XCTAssertEqual(app.staticTexts["morde.computer.title"].label, "控制状态不可用")
        XCTAssertFalse(app.buttons["morde.computer.controlAction"].exists,
                       "The error state must not add a page-level retry button.")
        attachScreen(app, named: "MDA-15 control status error without retry")

        app.buttons["morde.computer.close"].tap()
        XCTAssertTrue(host.waitForExistence(timeout: 10))
        host.tap()
        let computerTitle = app.staticTexts["morde.computer.title"]
        XCTAssertTrue(computerTitle.waitForExistence(timeout: 10))
        let recovered = XCTNSPredicateExpectation(predicate: NSPredicate(format: "label == %@", "Codex 正在控制"),
                                                  object: computerTitle)
        XCTAssertEqual(XCTWaiter.wait(for: [recovered], timeout: 15), .completed,
                       "Leaving and reopening the computer should reload the control status.")
        XCTAssertEqual(app.buttons["morde.computer.controlAction"].label, "接管控制权")
        attachScreen(app, named: "MDA-15 control status recovered after reentry")
    }

    func testMordeComputerRouteReportsWebViewLoadFailure() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localResourceAcceptanceFixture"])
        app.buttons["morde.nav.tab.resources"].tap()

        let host = app.buttons["morde.resources.host.open.h0000001"]
        XCTAssertTrue(host.waitForExistence(timeout: 10))
        host.tap()
        XCTAssertTrue(app.buttons["morde.computer.close"].waitForExistence(timeout: 10))

        let failure = app.staticTexts["morde.computer.pageLoadFailure"]
        XCTAssertTrue(failure.waitForExistence(timeout: 30),
                      "A failed Host WebView navigation should not leave a blank page.")
        app.buttons["morde.computer.close"].tap()
        XCTAssertTrue(app.buttons["morde.resources.host.open.h0000001"].waitForExistence(timeout: 5))
    }

    func testMordeComputerRouteKeepsHostTicketDenialOnResourceRow() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localResourceAcceptanceFixture",
            "-morde.localHostTicketDeniedFixture",
        ])
        app.buttons["morde.nav.tab.resources"].tap()

        let host = app.buttons["morde.resources.host.open.h0000001"]
        XCTAssertTrue(host.waitForExistence(timeout: 10))
        host.tap()

        XCTAssertTrue(app.buttons["morde.computer.close"].waitForExistence(timeout: 10))
        let denied = app.staticTexts["morde.computer.pageLoadFailure"]
        XCTAssertTrue(denied.waitForExistence(timeout: 10),
                      "A denied host-entry ticket should show a concise failure state in the computer route.")
        XCTAssertEqual(denied.label, "连接失败")
        XCTAssertTrue(app.staticTexts["morde.computer.subtitle"].isHittable)
        XCTAssertTrue(app.staticTexts["morde.computer.subtitle"].label.contains("Running host"))
        XCTAssertTrue(denied.isHittable,
                      "The concise failure state must remain visible over the dark computer background.")
        XCTAssertFalse(app.descendants(matching: .any)["morde.computer.webview"].exists,
                       "A denied ticket must not create a Host WebView.")
        attachScreen(app, named: "Morde Host ticket denied")

        app.buttons["morde.computer.close"].tap()
        XCTAssertTrue(host.waitForExistence(timeout: 5), "Closing the failure state should return to the resource row.")
        XCTAssertTrue(app.staticTexts["morde.resources.host.error.h0000001"].waitForExistence(timeout: 5),
                      "The detailed ticket error should remain available on the resource row after leaving the compact computer page.")
    }

    func testMordeChatComposerSubmitsFromKeyboardReturn() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskSSEFixture", "-morde.localTaskSequentialSendFixture",
        ])

        let composer = app.textFields["morde.chat.composer"]
        XCTAssertTrue(composer.waitForExistence(timeout: 15))
        composer.tap()
        composer.typeText("Keyboard submit fixture\n")

        XCTAssertTrue(app.descendants(matching: .any)["morde.chat.taskSummary"].waitForExistence(timeout: 20),
                      "The keyboard Return action should submit through the same guarded chat path.")
        XCTAssertTrue(app.staticTexts["Keyboard submit fixture"].exists)
    }

    func testMordeDrawerHidesProjectManagementWhileWorkItemsKeepProjectScope() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localWorkItemFixture"])

        app.buttons["morde.nav.menu"].tap()
        XCTAssertTrue(app.scrollViews["morde.nav.drawer.scroll"].waitForExistence(timeout: 5))
        XCTAssertFalse(app.buttons["morde.nav.drawer.projects"].exists)
        XCTAssertFalse(app.buttons["项目"].exists)

        let workItems = app.buttons["morde.nav.drawer.workItems"]
        XCTAssertTrue(workItems.waitForExistence(timeout: 5))
        workItems.tap()
        let projectPicker = app.buttons["morde.workItems.projectPicker"]
        XCTAssertTrue(projectPicker.waitForExistence(timeout: 10))
        projectPicker.tap()
        let editableProject = app.buttons["morde.workItems.project.j0000001"]
        XCTAssertTrue(editableProject.waitForExistence(timeout: 5),
                      "Project data remains available to the work-item scope picker.")
        editableProject.tap()
        XCTAssertTrue(app.staticTexts["本地缺陷"].waitForExistence(timeout: 15),
                      "Selecting a project from the visible work-item flow should load its scoped work items.")
        XCTAssertTrue(projectPicker.label.contains("可编辑项目"),
                      "The project picker should reflect the selected scope.")
        attachScreen(app, named: "MDA-01-project-entry-removed-workitem-project-scope-retained")
    }

    func testMordeEmptyComposerSendFocusesInput() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app)

        let composer = app.textFields["morde.chat.composer"]
        let send = app.buttons["morde.task.send"]
        XCTAssertTrue(composer.waitForExistence(timeout: 15))
        XCTAssertTrue(send.waitForExistence(timeout: 10))
        XCTAssertFalse(app.buttons["morde.chat.composer.mic"].exists,
                       "Voice input is available from the long-press chat menu, not as an extra composer button.")
        XCTAssertTrue(send.isEnabled)
        send.tap()

        XCTAssertTrue(app.keyboards.firstMatch.waitForExistence(timeout: 10),
                      "The design's empty send control should focus the composer without submitting an empty message.")
        XCTAssertFalse(app.descendants(matching: .any)["morde.chat.taskSummary"].exists)
        attachScreen(app, named: "MDA-02 empty composer send")
    }

    func testMordeChatAttachmentLimitRejectsExpandedPDFSet() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localChatAttachmentFixture", "-morde.localChatAttachmentOverflowFixture",
        ])

        let failure = app.alerts.firstMatch
        XCTAssertTrue(failure.waitForExistence(timeout: 40),
                      "Expanded PDF originals, extracted text, and page images must count toward the message limit.")
        XCTAssertTrue(failure.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", "16")).firstMatch.exists)
        failure.buttons["确定"].tap()

        XCTAssertFalse(app.descendants(matching: .any)
            .matching(identifier: "morde.chat.attachments.pending").firstMatch.exists,
                       "An over-limit import must not partially enter the conversation draft.")
        XCTAssertFalse(app.buttons["morde.chat.enqueueTask"].exists,
                       "An over-limit import must not expose an attachment-only queue action.")
    }

    func testMordeTemporaryAttachmentCleanupRetriesAfterFailure() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localAttachmentCleanupFailureFixture"])

        let result = app.alerts.firstMatch
        XCTAssertTrue(result.waitForExistence(timeout: 20))
        XCTAssertTrue(result.staticTexts["Synthetic attachment cleanup retry succeeded."].exists,
                      "A failed safe unlink should remain pending and be removed by the next cleanup retry.")
        result.buttons["确定"].tap()
    }

    func testMordeChatMultiTurnVisualFixtureMatchesDesignDensity() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localChatVisualFixture"])

        XCTAssertTrue(app.staticTexts.matching(
            NSPredicate(format: "label CONTAINS %@", "产物已存进文件库"))
            .firstMatch.waitForExistence(timeout: 15))
        let composer = app.textFields["morde.chat.composer"]
        XCTAssertTrue(composer.exists)
        XCTAssertEqual(composer.placeholderValue, "发消息…")
        XCTAssertTrue(app.buttons["morde.nav.tab.chat"].exists)
        attachScreen(app, named: "MDA-02 multi-turn chat visual")
    }

    func testMordeChatComposerQueuesFollowUpOnActiveTask() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskSSEFixture", "-morde.localTaskSequentialSendFixture", "-morde.localTaskQueueFixture",
        ])

        selectChatTaskTarget(app, taskID: "q1234567")
        XCTAssertEqual(app.staticTexts["morde.chat.header.status"].label, "进行中…",
                       "The chat header should show a localized running status only for an active Task.")
        let composer = app.textFields["morde.chat.composer"]
        XCTAssertTrue(composer.waitForExistence(timeout: 15))
        composer.tap()
        composer.typeText("Queue this follow-up fixture")

        let enqueue = app.buttons["morde.chat.enqueueTask"]
        XCTAssertTrue(enqueue.waitForExistence(timeout: 15), "A matching running task should expose queue in the send position.")
        XCTAssertTrue(enqueue.isEnabled)
        enqueue.tap()

        XCTAssertTrue(app.staticTexts["Queue this follow-up fixture"].waitForExistence(timeout: 20),
                      "The accepted follow-up message should appear in the same conversation.")
        XCTAssertFalse(app.buttons["morde.chat.stopTask"].exists,
                       "A task stop control should not add a permanent button to the composer.")
        attachScreen(app, named: "MDA-02 active Task chat header")
        assertChatLongPressMenuOffersStopTask(app, taskID: "q1234567")
    }

    func testMordeChatComposerQueuesImageAndPDFAttachmentsOnActiveTask() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskSSEFixture", "-morde.localTaskSequentialSendFixture",
            "-morde.localTaskQueueFixture", "-morde.localChatAttachmentFixture",
        ])

        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].waitForExistence(timeout: 40))
        XCTAssertTrue(app.staticTexts["fixture.pdf"].waitForExistence(timeout: 40))
        selectChatTaskTarget(app, taskID: "q1234567")
        let composer = app.textFields["morde.chat.composer"]
        XCTAssertTrue(composer.waitForExistence(timeout: 15))
        composer.tap()
        composer.typeText("Queue image and PDF while the Task is running")

        let enqueue = app.buttons["morde.chat.enqueueTask"]
        XCTAssertTrue(enqueue.waitForExistence(timeout: 15), "A matching running task should expose queue in the send position.")
        let enqueueEnabled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isEnabled == true"), object: enqueue)
        XCTAssertEqual(XCTWaiter.wait(for: [enqueueEnabled], timeout: 40), .completed)
        enqueue.tap()

        XCTAssertTrue(app.staticTexts["Queue image and PDF while the Task is running"].waitForExistence(timeout: 20),
                      "The follow-up message should be accepted into the active Task.")
        XCTAssertTrue(app.descendants(matching: .any).matching(
            NSPredicate(format: "label CONTAINS[c] %@", "fixture-image.jpg")).firstMatch.waitForExistence(timeout: 40),
                      "The queued message should retain the uploaded image attachment.")
        XCTAssertTrue(app.descendants(matching: .any).matching(
            NSPredicate(format: "label CONTAINS[c] %@", "fixture.pdf")).firstMatch.waitForExistence(timeout: 40),
                      "The queued message should retain the uploaded PDF attachment.")
        XCTAssertFalse(app.buttons["morde.chat.stopTask"].exists)
        attachScreen(app, named: "MDA-02 queued image and PDF chips")
        assertChatLongPressMenuOffersStopTask(app, taskID: "q1234567")
    }

    func testMordeChatComposerKeepsQueuedAttachmentsAfterLostResponseForRetry() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskSSEFixture", "-morde.localTaskSequentialSendFixture",
            "-morde.localTaskQueueFixture", "-morde.localChatAttachmentFixture",
            "-morde.localChatAttachmentSendRetryFixture",
        ])

        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].waitForExistence(timeout: 40))
        XCTAssertTrue(app.staticTexts["fixture.pdf"].waitForExistence(timeout: 40))
        selectChatTaskTarget(app, taskID: "q1234567")

        let composer = app.textFields["morde.chat.composer"]
        XCTAssertTrue(composer.waitForExistence(timeout: 15))
        composer.tap()
        composer.typeText("Retry queued image and PDF after response loss")
        let enqueue = app.buttons["morde.chat.enqueueTask"]
        XCTAssertTrue(enqueue.waitForExistence(timeout: 15))
        let enqueueEnabled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isEnabled == true"), object: enqueue)
        XCTAssertEqual(XCTWaiter.wait(for: [enqueueEnabled], timeout: 40), .completed)
        enqueue.tap()

        XCTAssertFalse(app.staticTexts["Retry queued image and PDF after response loss"].waitForExistence(timeout: 3),
                       "A lost response must not be presented as an acknowledged message.")
        let failureAlert = app.alerts["请求失败"]
        XCTAssertTrue(failureAlert.waitForExistence(timeout: 10), "The ambiguous first response should be reported as a failure.")
        failureAlert.buttons["确定"].tap()
        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].exists, "The image chip should stay in the composer after an ambiguous response.")
        XCTAssertTrue(app.staticTexts["fixture.pdf"].exists, "The PDF chip should stay in the composer after an ambiguous response.")
        let retryEnqueue = app.buttons["morde.chat.enqueueTask"]
        XCTAssertTrue(retryEnqueue.waitForExistence(timeout: 10))
        XCTAssertTrue(retryEnqueue.isEnabled)
        retryEnqueue.tap()

        XCTAssertTrue(app.staticTexts["Retry queued image and PDF after response loss"].waitForExistence(timeout: 20),
                      "Retry with the same idempotency key should reconcile the accepted queue message.")
        XCTAssertTrue(app.buttons.matching(NSPredicate(format: "label CONTAINS[c] %@", "fixture-image.jpg")).firstMatch.exists)
        XCTAssertTrue(app.buttons.matching(NSPredicate(format: "label CONTAINS[c] %@", "fixture.pdf")).firstMatch.exists)
        XCTAssertFalse(app.buttons["morde.chat.stopTask"].exists)
        assertChatLongPressMenuOffersStopTask(app, taskID: "q1234567")
    }

    func testMordeChatDraftAndAttachmentsStayWithTheirConversation() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesFixture",
            "-morde.localChatAttachmentFixture",
        ])

        let composer = app.textFields["morde.chat.composer"]
        XCTAssertTrue(composer.waitForExistence(timeout: 15))
        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].waitForExistence(timeout: 40))
        XCTAssertTrue(app.staticTexts["fixture.pdf"].waitForExistence(timeout: 40))
        composer.tap()
        composer.typeText("Private draft for the main conversation")

        app.buttons["morde.nav.tab.tasks"].tap()
        let task = app.buttons["morde.tasks.row.q0000001"]
        XCTAssertTrue(task.waitForExistence(timeout: 15))
        task.tap()
        XCTAssertTrue(app.staticTexts["morde.task.objective"].waitForExistence(timeout: 15))
        app.buttons["morde.task.back"].tap()
        app.buttons["morde.nav.tab.chat"].tap()

        XCTAssertEqual(composer.value as? String, "Private draft for the main conversation",
                       "Returning from Task details should restore the current conversation draft.")
        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].exists,
                      "Returning from Task details should preserve the image draft attachment.")
        XCTAssertTrue(app.staticTexts["fixture.pdf"].exists,
                      "Returning from Task details should preserve the PDF draft attachment.")
        attachScreen(app, named: "MDA-02 draft restored after Task detail")

        app.buttons["morde.nav.menu"].tap()
        let sideConversation = app.buttons["morde.nav.drawer.conversation-c2345678"]
        XCTAssertTrue(sideConversation.waitForExistence(timeout: 10))
        sideConversation.tap()

        XCTAssertTrue(app.staticTexts["Side conversation is sidebar-only."].waitForExistence(timeout: 15))
        XCTAssertFalse(app.staticTexts["Private draft for the main conversation"].exists,
                       "Text from the main conversation draft must not appear in the side conversation.")
        XCTAssertFalse(app.staticTexts["fixture-image.jpg"].exists,
                       "An uploaded image draft must remain scoped to its source conversation.")
        XCTAssertFalse(app.staticTexts["fixture.pdf"].exists,
                       "An uploaded PDF draft must remain scoped to its source conversation.")
    }

    func testMordeChatDraftIsClearedWhenAccountChanges() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localAccountIsolationFixture"], loginName: "account-a")
        signOutCurrentMordeAccount(app)
        signInLocalFixtureAccount(app, name: "account-a")
        XCTAssertTrue(app.staticTexts["account-a-private-message"].waitForExistence(timeout: 15))

        let composer = app.textFields["morde.chat.composer"]
        XCTAssertTrue(composer.waitForExistence(timeout: 15))
        composer.tap()
        composer.typeText("Account A unsent private draft")

        signOutCurrentMordeAccount(app)
        signInLocalFixtureAccount(app, name: "account-b")
        XCTAssertTrue(app.staticTexts["account-b-private-message"].waitForExistence(timeout: 15))
        let accountBComposer = app.textFields["morde.chat.composer"]
        XCTAssertTrue(accountBComposer.waitForExistence(timeout: 15))
        XCTAssertFalse((accountBComposer.value as? String ?? "").contains("Account A unsent private draft"),
                       "An unsent draft from Account A must not appear in Account B's composer.")
        XCTAssertFalse(app.staticTexts["Account A unsent private draft"].exists)
        attachScreen(app, named: "MDA-02-account-B-chat-without-account-A-draft")
    }

    func testMordeFullLayoutUploadsAndSendsImageAndPDFFixture() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        let fixtureAccountID = "u_mda11_\(UUID().uuidString.replacingOccurrences(of: "-", with: ""))"
        launchLocalChat(app, additionalArguments: [
            "-morde.localChatAttachmentFixture", "-morde.localFixtureAccountID", fixtureAccountID,
        ], loginName: "mda11-library-\(UUID().uuidString)", resetExistingAccount: true)

        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].waitForExistence(timeout: 40))
        XCTAssertTrue(app.staticTexts["fixture.pdf"].waitForExistence(timeout: 40))
        let send = app.buttons["morde.task.send"]
        XCTAssertTrue(send.waitForExistence(timeout: 10))
        let sendEnabled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isEnabled == true"),
                                                    object: send)
        XCTAssertEqual(XCTWaiter.wait(for: [sendEnabled], timeout: 40), .completed)
        XCTAssertTrue(send.isEnabled)
        send.tap()
        XCTAssertTrue(app.staticTexts["Synthetic attachment request accepted."].waitForExistence(timeout: 30),
                      "This personal-chat fixture has no associated Task summary.")
        attachScreen(app, named: "MDA-02 sent image and PDF chat")

        let imageAttachment = app.buttons.matching(NSPredicate(format: "label CONTAINS[c] %@", "fixture-image.jpg"))
            .firstMatch
        XCTAssertTrue(imageAttachment.waitForExistence(timeout: 10))
        imageAttachment.tap()
        XCTAssertTrue(app.descendants(matching: .any)["morde.attachment.preview.image"]
            .waitForExistence(timeout: 15))
        XCTAssertTrue(app.buttons["morde.nav.tab.chat"].exists,
                      "The file detail should retain the design's primary navigation.")
        XCTAssertFalse(app.buttons["Share"].exists)
        XCTAssertFalse(app.buttons["Save to Files"].exists)
        attachScreen(app, named: "MDA-12 Chat image preview")
        app.buttons["morde.attachment.preview.back"].tap()

        let pdfAttachment = app.buttons.matching(NSPredicate(format: "label CONTAINS[c] %@", "fixture.pdf"))
            .firstMatch
        XCTAssertTrue(pdfAttachment.waitForExistence(timeout: 10))
        pdfAttachment.tap()
        XCTAssertTrue(app.descendants(matching: .any)["morde.attachment.preview.pdf"]
            .waitForExistence(timeout: 15))
        XCTAssertFalse(app.buttons["Share"].exists)
        XCTAssertFalse(app.buttons["Save to Files"].exists)
        attachScreen(app, named: "MDA-12 Chat PDF preview")
        app.buttons["morde.attachment.preview.back"].tap()

        app.buttons["morde.nav.tab.resources"].tap()
        app.buttons["morde.nav.menu"].tap()
        app.buttons["morde.nav.drawer.library"].tap()
        XCTAssertTrue(app.staticTexts["文件库"].waitForExistence(timeout: 10),
                      "The library title should match the design wording.")
        let allFiles = app.buttons["morde.library.source.all"]
        let aiOutputs = app.buttons["morde.library.source.aiOutputs"]
        let myUploads = app.buttons["morde.library.source.myUploads"]
        XCTAssertTrue(allFiles.waitForExistence(timeout: 15))
        aiOutputs.tap()
        XCTAssertTrue(aiOutputs.isSelected)
        XCTAssertFalse(app.buttons.matching(
            NSPredicate(format: "label CONTAINS[c] %@", "fixture-image.jpg")
        ).firstMatch.exists, "The AI outputs filter must not classify personal uploads as Task artifacts.")
        allFiles.tap()
        XCTAssertTrue(allFiles.isSelected)
        XCTAssertTrue(app.buttons.matching(
            NSPredicate(format: "label CONTAINS[c] %@", "fixture-image.jpg")
        ).firstMatch.waitForExistence(timeout: 15), "All files should include the uploaded image.")
        XCTAssertTrue(app.buttons.matching(
            NSPredicate(format: "label CONTAINS[c] %@", "fixture.pdf")
        ).firstMatch.exists, "All files should include the uploaded PDF.")
        XCTAssertTrue(myUploads.waitForExistence(timeout: 15))
        myUploads.tap()
        XCTAssertTrue(myUploads.isSelected)
        attachScreen(app, named: "MDA-11 Library my uploads")
        let libraryImage = app.buttons.matching(
            NSPredicate(format: "label CONTAINS[c] %@", "fixture-image.jpg")
        ).firstMatch
        XCTAssertTrue(libraryImage.waitForExistence(timeout: 15))
        libraryImage.tap()
        XCTAssertTrue(app.descendants(matching: .any)["morde.attachment.preview.image"]
            .waitForExistence(timeout: 15))
        XCTAssertTrue(app.buttons["morde.nav.tab.resources"].exists,
                      "The file detail should retain the design's primary navigation.")
        attachScreen(app, named: "MDA-12 Library image preview")
        app.buttons["morde.attachment.preview.back"].tap()
        XCTAssertTrue(myUploads.exists, "Returning from a preview should preserve the library source filter.")
        libraryImage.tap()
        XCTAssertTrue(app.descendants(matching: .any)["morde.attachment.preview.image"]
            .waitForExistence(timeout: 15))
        let chatTab = app.buttons.matching(identifier: "morde.nav.tab.chat").firstMatch
        chatTab.tap()
        XCTAssertTrue(app.textFields["morde.chat.composer"].waitForExistence(timeout: 15),
                      "Selecting a primary tab from file detail should close the preview and navigate.")
        XCTAssertTrue(chatTab.isSelected,
                      "The selected primary tab should match the destination chat screen.")
    }

    func testMordeTaskArtifactQuickLookShowsSyntheticPreview() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localTaskPagesFixture",
            "-morde.localTaskArtifactPreviewFixture",
        ])
        app.buttons["morde.nav.tab.tasks"].tap()
        let task = app.buttons["morde.tasks.row.q0000001"]
        XCTAssertTrue(task.waitForExistence(timeout: 15))
        task.tap()
        XCTAssertTrue(app.staticTexts["morde.task.objective"].waitForExistence(timeout: 15),
                      "Load the verified Task detail before opening its artifact in the library.")
        app.buttons["chevron.left"].tap()
        XCTAssertTrue(task.waitForExistence(timeout: 10), "Return to the task list before changing sections.")
        app.buttons["morde.nav.tab.resources"].tap()
        app.buttons["morde.nav.menu"].tap()
        app.buttons["morde.nav.drawer.library"].tap()

        let aiOutputs = app.buttons["morde.library.source.aiOutputs"]
        XCTAssertTrue(aiOutputs.waitForExistence(timeout: 15))
        aiOutputs.tap()
        let legacyArtifact = app.staticTexts["Legacy metadata only"]
        XCTAssertTrue(legacyArtifact.waitForExistence(timeout: 15),
                      "Task artifact metadata remains visible even when its Workspace proof is incomplete.")
        XCTAssertTrue(app.descendants(matching: .any)["morde.library.task-artifact.a7654322"].exists)
        XCTAssertFalse(app.buttons["morde.library.task-artifact.preview.a7654322"].exists,
                       "An artifact without a complete Workspace proof must not be previewable.")
        let artifact = app.buttons.matching(
            NSPredicate(format: "label CONTAINS[c] %@", "Verified local output")
        ).firstMatch
        XCTAssertTrue(artifact.waitForExistence(timeout: 15),
                      "The verified synthetic Task artifact should appear in AI outputs.")
        artifact.tap()
        let quickLook = app.descendants(matching: .any)["QLPreviewControllerView"]
        XCTAssertTrue(quickLook.waitForExistence(timeout: 15),
                      "The native Quick Look controller should present for the verified local Task artifact.")
        XCTAssertGreaterThan(quickLook.frame.height, app.frame.height * 0.7,
                             "Quick Look should occupy the preview sheet instead of remaining a transient row tap.")
        let previewContentLoaded = XCTNSPredicateExpectation(
            predicate: NSPredicate { _, _ in
                app.debugDescription.contains("Local verified Task artifact preview")
            },
            object: app
        )
        XCTAssertEqual(XCTWaiter.wait(for: [previewContentLoaded], timeout: 15), .completed,
                       "Wait for Quick Look's separate preview process to render the local artifact body.")
        let visibleButtons = app.buttons.allElementsBoundByIndex
            .map { "\($0.label) [\($0.identifier)]" }.joined(separator: "\n")
        let controls = XCTAttachment(string: "Visible buttons after opening the verified Task artifact:\n\(visibleButtons)")
        controls.name = "MDA-12 Quick Look accessibility controls"
        controls.lifetime = .keepAlways
        add(controls)
        attachScreen(app, named: "MDA-12 Task artifact Quick Look controls")
    }

    func testMordeAttachmentDownloadFailureDoesNotOpenBlankPreview() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localChatAttachmentFixture",
            "-morde.localChatAttachmentDownloadFailureFixture",
        ])

        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].waitForExistence(timeout: 40))
        let send = app.buttons["morde.task.send"]
        let sendEnabled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isEnabled == true"),
                                                    object: send)
        XCTAssertEqual(XCTWaiter.wait(for: [sendEnabled], timeout: 40), .completed)
        send.tap()
        XCTAssertTrue(app.staticTexts["Synthetic attachment request accepted."].waitForExistence(timeout: 30))

        let imageAttachment = app.buttons.matching(NSPredicate(format: "label CONTAINS[c] %@", "fixture-image.jpg"))
            .firstMatch
        XCTAssertTrue(imageAttachment.waitForExistence(timeout: 10))
        imageAttachment.tap()
        let failure = app.alerts["请求失败"]
        XCTAssertTrue(failure.waitForExistence(timeout: 15))
        XCTAssertTrue(failure.buttons["确定"].exists, "A failed download should be reported to the user.")
        XCTAssertFalse(app.descendants(matching: .any)["morde.attachment.preview.image"].exists)
        failure.buttons["确定"].tap()
    }

    func testMordeAttachmentPreviewFailsClosedWhenPermissionIsDeniedAfterLibraryProof() {
        assertAttachmentPreviewFailsClosedAfterLibraryProof(
            fixtureArgument: "-morde.localChatAttachmentDownloadForbiddenFixture")
    }

    func testMordeAttachmentPreviewFailsClosedWhenServerReturnsNotFoundAfterLibraryProof() {
        assertAttachmentPreviewFailsClosedAfterLibraryProof(
            fixtureArgument: "-morde.localChatAttachmentDownloadNotFoundFixture")
    }

    private func assertAttachmentPreviewFailsClosedAfterLibraryProof(fixtureArgument: String) {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localChatAttachmentFixture", fixtureArgument])

        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].waitForExistence(timeout: 40))
        let send = app.buttons["morde.task.send"]
        let sendEnabled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isEnabled == true"),
                                                    object: send)
        XCTAssertEqual(XCTWaiter.wait(for: [sendEnabled], timeout: 40), .completed)
        send.tap()
        XCTAssertTrue(app.staticTexts["Synthetic attachment request accepted."].waitForExistence(timeout: 30))

        let imageAttachment = app.buttons.matching(
            NSPredicate(format: "label CONTAINS[c] %@", "fixture-image.jpg")
        ).firstMatch
        XCTAssertTrue(imageAttachment.waitForExistence(timeout: 10),
                      "A clickable message attachment proves the current library record authorizes the preview attempt.")
        imageAttachment.tap()

        let failure = app.alerts["请求失败"]
        XCTAssertTrue(failure.waitForExistence(timeout: 15),
                      "The server rejection must be surfaced without opening an attachment preview.")
        XCTAssertFalse(app.descendants(matching: .any)["morde.attachment.preview.image"].exists)
        XCTAssertFalse(app.descendants(matching: .any)["morde.attachment.preview.pdf"].exists)
        failure.buttons["确定"].tap()
    }

    func testMordeChatAttachmentWithoutLibraryProofCannotOpenPreview() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localChatAttachmentFixture",
            "-morde.localChatAttachmentMissingProofFixture",
        ], loginName: "morde-missing-proof-test")

        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].waitForExistence(timeout: 40))
        let send = app.buttons["morde.task.send"]
        let sendEnabled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isEnabled == true"),
                                                    object: send)
        XCTAssertEqual(XCTWaiter.wait(for: [sendEnabled], timeout: 40), .completed)
        send.tap()
        XCTAssertTrue(app.staticTexts["Synthetic attachment request accepted."].waitForExistence(timeout: 30))

        let attachmentIDs = app.staticTexts.matching(
            NSPredicate(format: "label MATCHES %@", "[0-9a-f]{8}")
        )
        XCTAssertGreaterThan(attachmentIDs.count, 0,
                             "The accepted image and PDF should retain opaque attachment references.")
        XCTAssertEqual(app.buttons.matching(
            NSPredicate(format: "label MATCHES %@", "[0-9a-f]{8}")
        ).count, 0, "Attachments without current account library proof must not be tappable.")
        XCTAssertFalse(app.descendants(matching: .any)["morde.attachment.preview.image"].exists)
        XCTAssertFalse(app.alerts["请求失败"].exists,
                       "Unavailable attachment metadata should not produce a preview or a false success alert.")
    }

    func testMordeLibraryMetadataOnlyRowsAreVisibleButNotPreviewable() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localLibraryMetadataOnlyFixture"])

        app.buttons["morde.nav.tab.resources"].tap()
        app.buttons["morde.nav.menu"].tap()
        app.buttons["morde.nav.drawer.library"].tap()

        let allFiles = app.buttons["morde.library.source.all"]
        let myUploads = app.buttons["morde.library.source.myUploads"]
        let metadataOnlyFile = app.staticTexts["metadata-only.pdf"]
        XCTAssertTrue(allFiles.waitForExistence(timeout: 15))
        XCTAssertTrue(metadataOnlyFile.waitForExistence(timeout: 15),
                      "Valid cloud metadata remains visible in All even when no attachment bytes are available.")
        XCTAssertTrue(app.staticTexts.containing(NSPredicate(format: "label CONTAINS %@", "仅元数据")).firstMatch.exists)
        XCTAssertFalse(app.buttons["morde.library.cloud.li_metadata1"].exists,
                       "A metadata-only row must not expose a preview action.")
        attachScreen(app, named: "MDA-11 metadata-only row visible in All")

        myUploads.tap()
        XCTAssertFalse(metadataOnlyFile.waitForExistence(timeout: 2),
                       "My uploads filters out records without a valid attachment ID.")
        attachScreen(app, named: "MDA-11 metadata-only row filtered from My uploads")
        allFiles.tap()
        XCTAssertTrue(metadataOnlyFile.waitForExistence(timeout: 5))
        XCTAssertFalse(app.descendants(matching: .any)["morde.attachment.preview.image"].exists)
        XCTAssertFalse(app.descendants(matching: .any)["morde.attachment.preview.pdf"].exists)
    }

    func testMordeAttachmentPreviewRejectsMismatchedContentLength() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localChatAttachmentFixture",
            "-morde.localChatAttachmentDownloadBadLengthFixture",
        ])

        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].waitForExistence(timeout: 40))
        let send = app.buttons["morde.task.send"]
        let sendEnabled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isEnabled == true"),
                                                    object: send)
        XCTAssertEqual(XCTWaiter.wait(for: [sendEnabled], timeout: 40), .completed)
        send.tap()
        XCTAssertTrue(app.staticTexts["Synthetic attachment request accepted."].waitForExistence(timeout: 30))

        let imageAttachment = app.buttons.matching(NSPredicate(format: "label CONTAINS[c] %@", "fixture-image.jpg"))
            .firstMatch
        XCTAssertTrue(imageAttachment.waitForExistence(timeout: 10))
        imageAttachment.tap()
        let failure = app.alerts["请求失败"]
        XCTAssertTrue(failure.waitForExistence(timeout: 15),
                      "A content length that does not match the downloaded bytes must fail closed.")
        XCTAssertFalse(app.descendants(matching: .any)["morde.attachment.preview.image"].exists)
        failure.buttons["确定"].tap()
    }

    func testMordeAttachmentPreviewRejectsMismatchedETag() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localChatAttachmentFixture",
            "-morde.localChatAttachmentDownloadBadETagFixture",
        ])

        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].waitForExistence(timeout: 40))
        let send = app.buttons["morde.task.send"]
        let sendEnabled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isEnabled == true"),
                                                    object: send)
        XCTAssertEqual(XCTWaiter.wait(for: [sendEnabled], timeout: 40), .completed)
        send.tap()
        XCTAssertTrue(app.staticTexts["Synthetic attachment request accepted."].waitForExistence(timeout: 30))

        let imageAttachment = app.buttons.matching(NSPredicate(format: "label CONTAINS[c] %@", "fixture-image.jpg"))
            .firstMatch
        XCTAssertTrue(imageAttachment.waitForExistence(timeout: 10))
        imageAttachment.tap()
        let failure = app.alerts["请求失败"]
        XCTAssertTrue(failure.waitForExistence(timeout: 15),
                      "An attachment response whose ETag does not match its verified bytes must fail closed.")
        XCTAssertFalse(app.descendants(matching: .any)["morde.attachment.preview.image"].exists)
        failure.buttons["确定"].tap()
    }

    func testMordePersonalAttachmentPreviewCancelsWhenClosedWhileLoading() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localChatAttachmentFixture",
            "-morde.localChatAttachmentDownloadDelayFixture",
        ])

        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].waitForExistence(timeout: 40))
        let send = app.buttons["morde.task.send"]
        let sendEnabled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isEnabled == true"),
                                                    object: send)
        XCTAssertEqual(XCTWaiter.wait(for: [sendEnabled], timeout: 40), .completed)
        send.tap()
        XCTAssertTrue(app.staticTexts["Synthetic attachment request accepted."].waitForExistence(timeout: 30))

        app.buttons["morde.nav.tab.resources"].tap()
        app.buttons["morde.nav.menu"].tap()
        app.buttons["morde.nav.drawer.library"].tap()
        let myUploads = app.buttons["morde.library.source.myUploads"]
        XCTAssertTrue(myUploads.waitForExistence(timeout: 15))
        myUploads.tap()
        let image = app.buttons.matching(NSPredicate(format: "label CONTAINS[c] %@", "fixture-image.jpg"))
            .firstMatch
        XCTAssertTrue(image.waitForExistence(timeout: 15))
        image.tap()
        XCTAssertTrue(app.descendants(matching: .any)["morde.attachment.preview.scroll"]
            .waitForExistence(timeout: 10), "The detail page should show a loading state during download.")
        app.buttons["morde.attachment.preview.back"].tap()
        XCTAssertTrue(app.staticTexts["文件库"].waitForExistence(timeout: 10))
        XCTAssertTrue(myUploads.exists, "Closing the loading page should preserve the library filter.")

        Thread.sleep(forTimeInterval: 3.5)
        XCTAssertFalse(app.alerts["请求失败"].exists,
                       "A cancelled request must not report a late download error.")
        XCTAssertFalse(app.descendants(matching: .any)["morde.attachment.preview.image"].exists,
                       "A cancelled download must not reopen the preview.")
    }

    func testMordePersonalAttachmentPreviewCancelsWhenAccountChangesWhileLoading() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localAccountIsolationFixture",
            "-morde.localChatAttachmentFixture",
            "-morde.localChatAttachmentDownloadDelayFixture",
            "-morde.localAttachmentPreviewAccountChangeFixture",
        ], loginName: "account-a", resetExistingAccount: true)

        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].waitForExistence(timeout: 40))
        let send = app.buttons["morde.task.send"]
        let sendEnabled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isEnabled == true"), object: send)
        XCTAssertEqual(XCTWaiter.wait(for: [sendEnabled], timeout: 40), .completed)
        send.tap()
        XCTAssertTrue(app.staticTexts["Synthetic attachment request accepted."].waitForExistence(timeout: 30))

        app.buttons["morde.nav.tab.resources"].tap()
        app.buttons["morde.nav.menu"].tap()
        app.buttons["morde.nav.drawer.library"].tap()
        let myUploads = app.buttons["morde.library.source.myUploads"]
        XCTAssertTrue(myUploads.waitForExistence(timeout: 15))
        myUploads.tap()
        let image = app.buttons.matching(NSPredicate(format: "label CONTAINS[c] %@", "fixture-image.jpg"))
            .firstMatch
        XCTAssertTrue(image.waitForExistence(timeout: 15))
        image.tap()
        XCTAssertTrue(app.descendants(matching: .any)["morde.attachment.preview.scroll"]
            .waitForExistence(timeout: 10), "The attachment should be loading before the account changes.")
        let username = app.textFields["morde.auth.username"]
        XCTAssertTrue(username.waitForExistence(timeout: 10),
                      "The local account-change fixture should sign out while the download is pending.")
        username.tap()
        username.typeText("account-b")
        let password = app.secureTextFields["morde.auth.password"]
        password.tap()
        password.typeText("local-password")
        app.buttons["morde.auth.submit"].tap()
        XCTAssertTrue(app.buttons["morde.nav.menu"].waitForExistence(timeout: 20),
                      "Account B should load after the pending Account A preview is cancelled.")

        app.buttons["morde.nav.tab.resources"].tap()
        app.buttons["morde.nav.menu"].tap()
        app.buttons["morde.nav.drawer.library"].tap()
        let accountBUploads = app.buttons["morde.library.source.myUploads"]
        XCTAssertTrue(accountBUploads.waitForExistence(timeout: 15))
        accountBUploads.tap()
        XCTAssertTrue(app.staticTexts["云端还没有文件"].waitForExistence(timeout: 10),
                      "Account B must not inherit Account A's uploaded attachment proof.")

        Thread.sleep(forTimeInterval: 3.5)
        XCTAssertFalse(app.alerts["请求失败"].exists,
                       "A late download response from Account A must not show an error in Account B's scope.")
        XCTAssertFalse(app.descendants(matching: .any)["morde.attachment.preview.image"].exists,
                       "An Account A attachment must not reopen after Account B signs in.")
        XCTAssertFalse(app.staticTexts["fixture-image.jpg"].exists,
                       "Account A's attachment must not appear in Account B's library.")
    }

    func testMordePersonalAttachmentPreviewCancelsWhenSyncIsDisabledWhileLoading() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: [
            "-morde.localChatAttachmentFixture",
            "-morde.localChatAttachmentDownloadDelayFixture",
            "-morde.localAttachmentPreviewSyncDisableFixture",
        ])

        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].waitForExistence(timeout: 40))
        let send = app.buttons["morde.task.send"]
        let sendEnabled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isEnabled == true"), object: send)
        XCTAssertEqual(XCTWaiter.wait(for: [sendEnabled], timeout: 40), .completed)
        send.tap()
        XCTAssertTrue(app.staticTexts["Synthetic attachment request accepted."].waitForExistence(timeout: 30))

        app.buttons["morde.nav.tab.resources"].tap()
        app.buttons["morde.nav.menu"].tap()
        app.buttons["morde.nav.drawer.library"].tap()
        let myUploads = app.buttons["morde.library.source.myUploads"]
        XCTAssertTrue(myUploads.waitForExistence(timeout: 15))
        myUploads.tap()
        let image = app.buttons.matching(NSPredicate(format: "label CONTAINS[c] %@", "fixture-image.jpg"))
            .firstMatch
        XCTAssertTrue(image.waitForExistence(timeout: 15))
        image.tap()
        let loadingPreview = app.descendants(matching: .any)["morde.attachment.preview.scroll"]
        XCTAssertTrue(loadingPreview.waitForExistence(timeout: 10),
                      "The attachment should be loading before personal-data sync is disabled.")

        let previewClosed = XCTNSPredicateExpectation(predicate: NSPredicate(format: "exists == false"),
                                                      object: loadingPreview)
        XCTAssertEqual(XCTWaiter.wait(for: [previewClosed], timeout: 10), .completed,
                       "Disabling personal-data sync should dismiss the protected preview immediately.")
        XCTAssertTrue(app.staticTexts["不可用"].waitForExistence(timeout: 10),
                      "The library should report unavailable while personal-data sync is disabled.")

        Thread.sleep(forTimeInterval: 3.5)
        XCTAssertFalse(app.alerts["请求失败"].exists,
                       "A late download response after sync is disabled must not show a stale error.")
        XCTAssertFalse(app.descendants(matching: .any)["morde.attachment.preview.image"].exists,
                       "A disabled personal-data scope must not reopen its pending preview.")
    }

    func testMordeLibraryShowsUnavailableInsteadOfEmptyWhenPersonalSyncFails() {
        let app = XCUIApplication(bundleIdentifier: "io.morde.app")
        launchLocalChat(app, additionalArguments: ["-morde.localPersonalSyncUnavailableFixture"])

        app.buttons["morde.nav.tab.resources"].tap()
        app.buttons["morde.nav.menu"].tap()
        app.buttons["morde.nav.drawer.library"].tap()
        let myUploads = app.buttons["morde.library.source.myUploads"]
        XCTAssertTrue(myUploads.waitForExistence(timeout: 15))
        myUploads.tap()

        XCTAssertTrue(app.staticTexts["不可用"].waitForExistence(timeout: 15),
                      "A failed sync request must remain distinct from a confirmed empty library.")
        XCTAssertFalse(app.staticTexts["云端还没有文件"].exists)
    }

    private func launchLocalChat(_ app: XCUIApplication, additionalArguments: [String] = [],
                                 loginName: String = "morde-design-test",
                                 resetExistingAccount: Bool = false) {
        // Keep fixture requests on loopback so a URLProtocol regression cannot reach a real Portal.
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                               "-morde.localRegistrationApproved", "-morde.localMainChatFixture"] + additionalArguments
        app.launch()
        if resetExistingAccount {
            let taskBack = app.buttons["morde.task.back"]
            if taskBack.waitForExistence(timeout: 2) {
                taskBack.tap()
            }
            let navigationMenu = app.buttons["morde.nav.menu"]
            if !navigationMenu.exists {
                let chatTab = app.buttons["morde.nav.tab.chat"]
                if chatTab.waitForExistence(timeout: 2) { chatTab.tap() }
            }
            if navigationMenu.waitForExistence(timeout: 5) {
                signOutCurrentMordeAccount(app)
            }
        }
        let usernameField = app.textFields["morde.auth.username"]
        if usernameField.waitForExistence(timeout: 5) {
            usernameField.tap()
            let keyboard = app.keyboards.firstMatch
            if !keyboard.waitForExistence(timeout: 2) {
                // The first tap can dismiss an alert surfaced by the initial local capability read.
                // Tap again after XCTest's interruption handler has closed it.
                usernameField.tap()
            }
            XCTAssertTrue(keyboard.waitForExistence(timeout: 5),
                          "The local sign-in form should focus the username field before typing.")
            usernameField.typeText(loginName)
            let password = app.secureTextFields["morde.auth.password"]
            password.tap()
            password.typeText("local-password")
            app.buttons["morde.auth.submit"].tap()
        }
        let tasksTab = app.buttons["morde.nav.tab.tasks"]
        XCTAssertTrue(tasksTab.waitForExistence(timeout: 30), "Local Morde app did not reach an authenticated route")
        if !app.buttons["morde.nav.menu"].exists {
            app.buttons["morde.nav.tab.chat"].tap()
        }
        XCTAssertTrue(app.buttons["morde.nav.menu"].waitForExistence(timeout: 10),
                      "Local Morde app did not return to the chat route")
    }

    private func selectChatTaskTarget(_ app: XCUIApplication, taskID: String) {
        let actions = app.descendants(matching: .any)["morde.chat.actions"]
        XCTAssertTrue(actions.waitForExistence(timeout: 10))
        actions.press(forDuration: 1)
        let taskOptions = app.buttons["morde.chat.action.taskOptions"]
        XCTAssertTrue(taskOptions.waitForExistence(timeout: 5))
        let taskOptionsHittable = XCTNSPredicateExpectation(
            predicate: NSPredicate(format: "isHittable == true"), object: taskOptions)
        XCTAssertEqual(XCTWaiter.wait(for: [taskOptionsHittable], timeout: 5), .completed,
                       "The chat action menu should finish presenting before Task options is tapped.")
        let done = app.buttons["morde.chat.taskOptions.done"]
        for _ in 0..<2 where !done.exists {
            taskOptions.tap()
            _ = done.waitForExistence(timeout: 3)
        }
        XCTAssertTrue(done.exists, "Task options should open from the chat long-press menu.")

        let target = app.descendants(matching: .any)["morde.chat.taskTarget"]
        XCTAssertTrue(target.waitForExistence(timeout: 10))
        target.tap()
        let activeTask = app.descendants(matching: .any)["morde.chat.taskTarget.option.\(taskID)"]
        XCTAssertTrue(activeTask.waitForExistence(timeout: 10),
                      "The active conversation task should be selectable from Task options.")
        activeTask.tap()
        app.buttons["morde.chat.taskOptions.done"].tap()
        XCTAssertFalse(app.descendants(matching: .any)["morde.chat.taskTarget"].exists,
                       "Closing Task options should leave the composer free of a target button.")
    }

    private func assertChatLongPressMenuOffersStopTask(_ app: XCUIApplication, taskID: String) {
        let actions = app.descendants(matching: .any)["morde.chat.actions"]
        XCTAssertTrue(actions.waitForExistence(timeout: 10))
        actions.press(forDuration: 1)
        let stopMenu = app.buttons["morde.chat.action.stopTask"]
        XCTAssertTrue(stopMenu.waitForExistence(timeout: 5),
                      "Stopping an active Task should remain available from the existing chat menu.")
        stopMenu.tap()
        let stopAction = app.buttons.matching(NSPredicate(format: "label CONTAINS %@", taskID)).firstMatch
        XCTAssertTrue(stopAction.waitForExistence(timeout: 5),
                      "The menu should expose the matching active Task's stop action.")
    }

    private func signOutCurrentMordeAccount(_ app: XCUIApplication) {
        app.buttons["morde.nav.menu"].tap()
        app.buttons["morde.nav.drawer.settings"].tap()
        let drawerScrim = app.buttons["morde.nav.drawer.scrim"]
        let drawerClosed = XCTNSPredicateExpectation(predicate: NSPredicate(format: "exists == false"),
                                                     object: drawerScrim)
        XCTAssertEqual(XCTWaiter.wait(for: [drawerClosed], timeout: 5), .completed)
        let accountSettings = app.buttons["morde.settings.row.account"]
        XCTAssertTrue(waitForHittable(accountSettings, timeout: 10))
        accountSettings.coordinate(withNormalizedOffset: CGVector(dx: 0.92, dy: 0.5)).tap()
        XCTAssertTrue(app.staticTexts["账号"].waitForExistence(timeout: 10))
        let signOut = app.buttons["morde.account.signOut"]
        let bottomTab = app.buttons["morde.nav.tab.approvals"]
        let accountList = app.collectionViews.firstMatch.exists ? app.collectionViews.firstMatch :
            (app.tables.firstMatch.exists ? app.tables.firstMatch : app.scrollViews.firstMatch)
        XCTAssertTrue(accountList.waitForExistence(timeout: 5))
        for _ in 0..<8 where signOut.frame.maxY >= bottomTab.frame.minY { accountList.swipeUp() }
        XCTAssertTrue(signOut.isHittable)
        XCTAssertLessThan(signOut.frame.maxY, bottomTab.frame.minY)
        signOut.tap()
        let confirmSignOut = app.descendants(matching: .any)
            .matching(identifier: "morde.account.signOut.confirm").firstMatch
        XCTAssertTrue(confirmSignOut.waitForExistence(timeout: 5))
        confirmSignOut.tap()
    }

    private func signInLocalFixtureAccount(_ app: XCUIApplication, name: String) {
        let username = app.textFields["morde.auth.username"]
        XCTAssertTrue(username.waitForExistence(timeout: 10))
        username.tap()
        if let currentName = username.value as? String, currentName != username.placeholderValue {
            username.typeText(String(repeating: XCUIKeyboardKey.delete.rawValue, count: currentName.count))
        }
        username.typeText(name)
        let password = app.secureTextFields["morde.auth.password"]
        password.tap()
        password.typeText("local-password")
        app.buttons["morde.auth.submit"].tap()
        XCTAssertTrue(app.buttons["morde.nav.menu"].waitForExistence(timeout: 20))
    }

    private func waitForHittable(_ element: XCUIElement, timeout: TimeInterval = 5) -> Bool {
        let expectation = XCTNSPredicateExpectation(predicate: NSPredicate(format: "isHittable == true"),
                                                    object: element)
        return XCTWaiter.wait(for: [expectation], timeout: timeout) == .completed
    }

    private func attachScreen(_ app: XCUIApplication, named name: String) {
        let attachment = XCTAttachment(screenshot: app.screenshot())
        attachment.name = name
        attachment.lifetime = .keepAlways
        add(attachment)
    }

}
