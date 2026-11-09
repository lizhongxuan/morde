import XCTest

@MainActor
final class MIFWorkItemAcceptanceUITests: XCTestCase {
    func testTaskNavigationPreservesChatDraftAndSelectedTabAccessibility() {
        let app = launchFixture()
        let chatTab = app.buttons["morde.shell.tab.chat"]
        let tasksTab = app.buttons["morde.shell.tab.tasks"]
        XCTAssertEqual(chatTab.label, "聊天")
        XCTAssertEqual(tasksTab.label, "任务")
        XCTAssertTrue(chatTab.isSelected)
        chatTab.tap()
        XCTAssertTrue(chatTab.isSelected)

        let draft = element("morde.chat.composer", in: app)
        XCTAssertTrue(element("morde.chat.transcript", in: app).waitForExistence(timeout: 10))
        XCTAssertTrue(draft.waitForExistence(timeout: 10))
        XCTAssertTrue(draft.isHittable)
        draft.tap()
        draft.typeText("Draft survives the task tab")
        XCTAssertTrue(app.keyboards.firstMatch.waitForExistence(timeout: 5))
        element("morde.chat.transcript", in: app).tap()
        let keyboard = app.keyboards.firstMatch
        let keyboardDismissed = expectation(for: NSPredicate(format: "exists == false"), evaluatedWith: keyboard)
        wait(for: [keyboardDismissed], timeout: 5)
        XCTAssertTrue(tasksTab.waitForExistence(timeout: 5))

        tasksTab.tap()
        XCTAssertTrue(tasksTab.isSelected)
        XCTAssertFalse(chatTab.isSelected)
        XCTAssertTrue(app.buttons["morde.tasks.row.q0000001"].waitForExistence(timeout: 15))

        chatTab.tap()
        XCTAssertTrue(chatTab.isSelected)
        XCTAssertFalse(app.buttons["morde.shell.tab.tasks"].isSelected)
        XCTAssertEqual(draft.value as? String, "Draft survives the task tab")
        app.terminate()
    }

    func testTaskPlusCreatesConversationAndFocusesComposer() {
        let app = launchFixture()
        let tasks = app.buttons["morde.shell.tab.tasks"]
        XCTAssertTrue(tasks.waitForExistence(timeout: 10))
        tasks.tap()

        let newTask = app.buttons["morde.tasks.newTask"]
        XCTAssertTrue(newTask.waitForExistence(timeout: 10))
        newTask.tap()

        let composer = element("morde.chat.composer", in: app)
        XCTAssertTrue(composer.waitForExistence(timeout: 10))
        XCTAssertTrue(app.keyboards.firstMatch.waitForExistence(timeout: 10),
                      "Creating a task should leave the new conversation ready for its objective.")
        element("morde.chat.transcript", in: app).tap()
        let keyboardHidden = expectation(for: NSPredicate(format: "exists == false"),
                                         evaluatedWith: app.keyboards.firstMatch)
        wait(for: [keyboardHidden], timeout: 5)
        XCTAssertTrue(app.buttons["morde.shell.tab.chat"].isSelected)
        app.terminate()
    }

    func testCloudConversationDraftsStayWithTheirConversation() {
        let app = launchFixture()
        let draft = element("morde.chat.composer", in: app)
        XCTAssertTrue(draft.waitForExistence(timeout: 10))
        draft.tap()
        draft.typeText("Draft for the main conversation")
        app.buttons["morde.shell.drawer"].tap()
        let sideChat = app.buttons["morde.shell.cloudConversation.c2345678"]
        XCTAssertTrue(sideChat.waitForExistence(timeout: 10))
        sideChat.tap()
        let sideMessage = app.descendants(matching: .any)
            .matching(identifier: "morde.chat.message.m2345678").firstMatch
        XCTAssertTrue(sideMessage.waitForExistence(timeout: 10))
        XCTAssertEqual(draft.value as? String, "")
        draft.tap()
        draft.typeText("Draft for the side conversation")

        app.buttons["morde.shell.drawer"].tap()
        app.buttons["morde.shell.mainChat"].tap()
        XCTAssertEqual(draft.value as? String, "Draft for the main conversation")
        XCTAssertFalse(app.staticTexts["First page chat"].exists)
        app.buttons["morde.shell.drawer"].tap()
        app.buttons["morde.shell.cloudConversation.c2345678"].tap()
        XCTAssertTrue(app.staticTexts["First page chat"].waitForExistence(timeout: 10))
        XCTAssertEqual(draft.value as? String, "Draft for the side conversation")
        app.terminate()
    }

    func testFourRootTabsStayInsideTheNanoMuseShell() {
        let app = launchFixture()
        let chatTab = app.buttons["morde.shell.tab.chat"]
        let tasksTab = app.buttons["morde.shell.tab.tasks"]
        let approvalsTab = app.buttons["morde.shell.tab.approvals"]
        let resourcesTab = app.buttons["morde.shell.tab.resources"]

        XCTAssertTrue(chatTab.waitForExistence(timeout: 10))
        XCTAssertTrue(chatTab.isSelected)
        tasksTab.tap()
        XCTAssertTrue(tasksTab.isSelected)
        XCTAssertTrue(app.staticTexts["任务"].waitForExistence(timeout: 10))
        approvalsTab.tap()
        XCTAssertTrue(approvalsTab.isSelected)
        XCTAssertEqual(resourcesTab.label, "资源")
        resourcesTab.tap()
        XCTAssertTrue(resourcesTab.isSelected)
        XCTAssertTrue(app.buttons["morde.resources.add"].waitForExistence(timeout: 10))
        XCTAssertFalse(app.buttons["morde.resources.refresh"].exists)
        XCTAssertFalse(app.buttons["morde.resources.library"].exists)

        chatTab.tap()
        XCTAssertTrue(chatTab.isSelected)
        let drawer = app.buttons["morde.shell.drawer"]
        drawer.tap()
        app.buttons["morde.shell.drawer.library"].tap()
        XCTAssertTrue(element("morde.library.sourcePicker", in: app).waitForExistence(timeout: 10))
        app.terminate()
    }

    func testProjectsDrawerRouteOpensProjectList() {
        let app = launchFixture()
        app.buttons["morde.shell.drawer"].tap()

        let drawer = app.scrollViews["morde.shell.drawer.scroll"]
        XCTAssertTrue(drawer.waitForExistence(timeout: 5))
        let projects = app.buttons["morde.shell.drawer.projects"]
        for _ in 0..<8 where !projects.isHittable {
            drawer.swipeUp()
        }
        XCTAssertTrue(projects.waitForExistence(timeout: 5))
        XCTAssertTrue(projects.isHittable)
        projects.tap()

        let list = app.descendants(matching: .any).matching(identifier: "morde.projects.list").firstMatch
        XCTAssertTrue(list.waitForExistence(timeout: 15))
        let project = app.buttons.matching(
            NSPredicate(format: "identifier BEGINSWITH %@", "morde.project.row.")
        ).firstMatch
        XCTAssertTrue(project.waitForExistence(timeout: 15))
        app.terminate()
    }

    func testDrawerSearchCanContinueToTheNextPageWhenCurrentPageHasNoMatches() {
        let app = launchFixture()
        app.buttons["morde.shell.drawer"].tap()
        let search = app.textFields["morde.shell.search"]
        XCTAssertTrue(search.waitForExistence(timeout: 10))
        search.tap()
        search.typeText("Needle\n")

        let secondPageMatch = app.buttons["morde.shell.cloudConversation.c3456789"]
        XCTAssertTrue(secondPageMatch.waitForExistence(timeout: 10))
        XCTAssertFalse(app.buttons["morde.shell.cloudConversation.c2345678"].exists)
        secondPageMatch.tap()
        app.buttons["morde.shell.drawer"].tap()
        XCTAssertTrue(app.buttons["morde.shell.cloudConversation.c3456789"].waitForExistence(timeout: 5))
        app.terminate()
    }

    func testShellNewChatCreatesCloudConversationVisibleInDrawer() {
        let app = launchFixture()
        app.buttons["morde.shell.drawer"].tap()
        let newConversation = app.buttons["morde.shell.newSideChat"]
        XCTAssertTrue(newConversation.waitForExistence(timeout: 10))
        XCTAssertTrue(newConversation.isEnabled)
        newConversation.tap()

        XCTAssertTrue(app.staticTexts["此对话暂无消息。"].waitForExistence(timeout: 15))
        app.buttons["morde.shell.drawer"].tap()
        let created = app.buttons["morde.shell.cloudConversation.c4567890"]
        XCTAssertTrue(created.waitForExistence(timeout: 10))
        XCTAssertTrue(created.label.contains("新对话"))
        app.terminate()
    }

    func testFailedConversationLoadShowsRetryAction() {
        let app = launchFixture(extraArguments: ["-morde.localConversationFailureFixture"])
        XCTAssertTrue(app.staticTexts["Local conversation fixture failure"].waitForExistence(timeout: 10))
        XCTAssertTrue(app.buttons["重试读取"].waitForExistence(timeout: 15))
        app.terminate()
    }

    func testFailedConversationDrawerLoadRecoversWhenDrawerOpensLocally() {
        let app = launchFixture(extraArguments: [
            "-morde.localConversationFailureFixture",
            "-morde.localConversationDrawerRetryFixture",
        ])
        app.buttons["morde.shell.drawer"].tap()
        XCTAssertTrue(app.buttons["morde.shell.cloudConversation.c2345678"].waitForExistence(timeout: 10))
        app.terminate()
    }

    func testDrawerShowsApprovedDestinationsAndHidesRetiredEntries() {
        let app = launchFixture()
        app.buttons["morde.shell.drawer"].tap()
        for destination in ["tasks", "approvals", "eventChain", "resources", "library", "workItems", "friends"] {
            XCTAssertTrue(app.buttons["morde.shell.drawer.\(destination)"].waitForExistence(timeout: 10), destination)
        }
        XCTAssertTrue(app.buttons["morde.shell.mainChat"].exists)
        XCTAssertTrue(app.buttons["morde.shell.toolChats"].exists)
        XCTAssertFalse(app.buttons["morde.shell.localHistory"].exists)
        XCTAssertFalse(app.buttons["System files"].exists)
        app.terminate()
    }

    func testDrawerRoutesIndependentPagesAndPlaceholders() {
        let app = launchFixture()
        let drawer = app.buttons["morde.shell.drawer"]
        drawer.tap()
        app.buttons["morde.shell.drawer.eventChain"].tap()
        XCTAssertFalse(app.buttons["morde.tasks.newTask"].exists)

        app.buttons["morde.shell.tab.chat"].tap()
        drawer.tap()
        app.buttons["morde.shell.drawer.library"].tap()
        XCTAssertTrue(element("morde.library.sourcePicker", in: app).waitForExistence(timeout: 10))

        app.buttons["morde.shell.tab.chat"].tap()
        drawer.tap()
        app.buttons["morde.shell.drawer.workItems"].tap()
        XCTAssertTrue(app.buttons["morde.workItems.create.open"].waitForExistence(timeout: 10))
        XCTAssertFalse(app.buttons["morde.workItems.refresh"].exists)

        app.buttons["morde.shell.tab.chat"].tap()
        drawer.tap()
        app.buttons["morde.shell.drawer.friends"].tap()
        XCTAssertTrue(element("morde.placeholder.friends", in: app).waitForExistence(timeout: 10))
        app.buttons["morde.sheet.done"].tap()

        drawer.tap()
        app.buttons["morde.shell.toolChats"].tap()
        XCTAssertTrue(element("morde.placeholder.toolChats", in: app).waitForExistence(timeout: 10))
        app.buttons["morde.sheet.done"].tap()
        app.terminate()
    }

    func testDrawerOmitsRetiredDeviceAndSystemFileEntries() {
        let app = launchFixture()
        app.buttons["morde.shell.drawer"].tap()
        XCTAssertFalse(app.buttons["Devices"].exists)
        XCTAssertFalse(app.buttons["System files"].exists)
        app.terminate()
    }

    func testLegacyCodingAgentsAreNotReachableFromMordeChatOrSettings() {
        let app = launchFixture()
        app.buttons["morde.shell.drawer"].tap()
        XCTAssertFalse(app.buttons["编程助手"].exists)
        app.buttons["morde.shell.settings"].tap()
        XCTAssertFalse(app.buttons["编程助手"].exists)
        app.terminate()
    }

    func testWorkItemsFilterCreateClaimConflictLinkAndDeliveryLocally() {
        let app = launchFixture()
        app.buttons["morde.shell.drawer"].tap()
        app.buttons["morde.shell.drawer.workItems"].tap()

        let projectPicker = app.buttons["morde.workItems.projectPicker"]
        XCTAssertTrue(projectPicker.waitForExistence(timeout: 10))
        let createButton = app.buttons["morde.workItems.create.open"]
        XCTAssertTrue(createButton.waitForExistence(timeout: 5))
        projectPicker.tap()
        app.buttons["morde.workItems.project.j0000002"].tap()
        XCTAssertFalse(createButton.isEnabled)
        projectPicker.tap()
        app.buttons["morde.workItems.project.j0000001"].tap()
        XCTAssertTrue(element("morde.workItems.row.title.j1000001", in: app).waitForExistence(timeout: 15))
        XCTAssertTrue(element("morde.workItems.row.title.j1000002", in: app).waitForExistence(timeout: 15))
        XCTAssertTrue(createButton.isEnabled)

        let conversationPicker = app.buttons["morde.workItems.conversationPicker"]
        scrollUntilHittable(conversationPicker, in: app)
        conversationPicker.tap()
        app.buttons["morde.workItems.conversation.c0000001"].tap()
        let taskPicker = app.buttons["morde.workItems.taskPicker"]
        XCTAssertTrue(taskPicker.waitForExistence(timeout: 15))
        taskPicker.tap()
        app.buttons["morde.workItems.task.q0000001"].tap()
        XCTAssertTrue(app.staticTexts["本机工作项关联 Task"].waitForExistence(timeout: 15))

        selectFilter("bug", in: app)
        XCTAssertTrue(element("morde.workItems.row.title.j1000001", in: app).exists)
        XCTAssertFalse(element("morde.workItems.row.title.j1000002", in: app).exists)
        selectFilter("request", in: app)
        XCTAssertTrue(element("morde.workItems.row.title.j1000002", in: app).exists)
        selectFilter("unclaimed", in: app)
        XCTAssertTrue(element("morde.workItems.row.title.j1000004", in: app).exists)

        let claim = app.buttons["morde.workItems.claim.j1000004"]
        scrollUntilHittable(claim, in: app)
        claim.tap()
        let claimNotice = element("morde.workItems.notice", in: app)
        if !claimNotice.waitForExistence(timeout: 10) {
            let actionError = element("morde.workItems.error", in: app)
            XCTFail("Claim did not complete: \(actionError.exists ? actionError.label : "no action feedback")")
            app.terminate()
            return
        }
        selectFilter("all", in: app)
        XCTAssertTrue(element("morde.workItems.row.state.j1000004", in: app).label.contains("已阻塞"))
        XCTAssertTrue(element("morde.workItems.row.metadata.j1000004", in: app).label.contains("u0000001"))
        let resume = app.buttons["morde.workItems.resume.j1000004"]
        scrollUntilHittable(resume, in: app)
        XCTAssertTrue(resume.isHittable)
        XCTAssertTrue(resume.isEnabled, "Resume should be enabled after the claim has completed.")
        resume.tap()
        let resumeNotice = element("morde.workItems.notice", in: app)
        let resumedNotice = expectation(for: NSPredicate(format: "label == %@", "工作项已恢复。"), evaluatedWith: resumeNotice)
        let resumeResult = XCTWaiter.wait(for: [resumedNotice], timeout: 10)
        let resumeError = element("morde.workItems.error", in: app)
        XCTAssertEqual(resumeResult, .completed,
                       "Resume failed: \(resumeError.exists ? resumeError.label : resumeNotice.label)")
        let resumedState = element("morde.workItems.row.state.j1000004", in: app)
        let resumed = expectation(for: NSPredicate(format: "label CONTAINS %@", "待处理"), evaluatedWith: resumedState)
        wait(for: [resumed], timeout: 10)

        let taskLink = app.buttons["morde.workItems.linkTask.j1000001"]
        scrollUntilHittable(taskLink, in: app)
        XCTAssertTrue(taskLink.waitForExistence(timeout: 10))
        taskLink.tap()
        XCTAssertTrue(element("morde.workItems.linkTask.done.j1000001", in: app).waitForExistence(timeout: 10))

        let deliveryPicker = app.buttons["morde.workItems.delivery.picker.j1000001"]
        scrollUntilHittable(deliveryPicker, in: app)
        XCTAssertTrue(deliveryPicker.waitForExistence(timeout: 10))
        deliveryPicker.tap()
        app.buttons["morde.workItems.delivery.target.h0000001:w0000001"].tap()
        let deliveryConfirm = app.buttons.matching(identifier: "morde.workItems.delivery.confirm").firstMatch
        XCTAssertTrue(deliveryConfirm.waitForExistence(timeout: 10))
        deliveryConfirm.tap()
        let deliveredState = element("morde.workItems.row.state.j1000001", in: app)
        let delivered = expectation(for: NSPredicate(format: "label CONTAINS %@", "已交付"), evaluatedWith: deliveredState)
        wait(for: [delivered], timeout: 10)

        createButton.tap()
        XCTAssertTrue(element("morde.workItems.create.scroll", in: app).waitForExistence(timeout: 5),
                      "Creating a work item should present its own scroll view.")
        let createTitle = app.textFields["morde.workItems.create.title"]
        scrollUntilHittable(createTitle, in: app)
        createTitle.tap()
        createTitle.typeText("Local unassigned bug")
        XCTAssertTrue(app.buttons["morde.workItems.keyboardDone"].waitForExistence(timeout: 5))
        let create = app.buttons["morde.workItems.create.submit"]
        scrollUntilHittable(create, in: app)
        create.tap()
        XCTAssertTrue(element("morde.workItems.row.title.j1000003", in: app).waitForExistence(timeout: 15))
        XCTAssertTrue(element("morde.workItems.row.metadata.j1000003", in: app).label.contains("待认领"))

        selectFilter("all", in: app)
        let block = app.buttons["morde.workItems.block.j1000002"]
        scrollUntilHittable(block, in: app)
        block.tap()
        let reason = app.textFields["morde.workItems.blockReason.j1000002"]
        XCTAssertTrue(reason.waitForExistence(timeout: 10))
        scrollUntilHittable(reason, in: app)
        reason.tap()
        reason.typeText("Local conflict retry")
        let confirmBlock = app.buttons["morde.workItems.block.confirm.j1000002"]
        scrollUntilHittable(confirmBlock, in: app)
        confirmBlock.tap()
        XCTAssertTrue(element("morde.workItems.error", in: app).waitForExistence(timeout: 10))
        confirmBlock.tap()
        let blockedState = element("morde.workItems.row.state.j1000002", in: app)
        let blocked = expectation(for: NSPredicate(format: "label CONTAINS %@", "已阻塞"), evaluatedWith: blockedState)
        wait(for: [blocked], timeout: 10)
        dismissKeyboard(in: app)
        app.terminate()
    }

    func testCloudExecutionStopAndExplicitEnableLocally() throws {
        let app = launchExecutionFixture()
        let state = try openExecutionSettings(in: app)
        XCTAssertTrue(state.label.contains("已启用"), "Expected the local fixture to start enabled.")
        app.buttons["morde.execution.stop"].tap()
        let confirm = app.buttons.matching(identifier: "morde.execution.stop.confirm").firstMatch
        XCTAssertTrue(confirm.waitForExistence(timeout: 5))
        confirm.tap()

        let disabled = expectation(for: NSPredicate(format: "label CONTAINS %@", "已停用"), evaluatedWith: state)
        wait(for: [disabled], timeout: 15)
        let enable = app.buttons["morde.execution.enable"]
        XCTAssertTrue(enable.waitForExistence(timeout: 5))
        enable.tap()
        let enabled = expectation(for: NSPredicate(format: "label CONTAINS %@", "已启用"), evaluatedWith: state)
        wait(for: [enabled], timeout: 10)
        app.terminate()
    }

    func testCloudExecutionStopConfirmationCanBeCancelledLocally() throws {
        let app = launchExecutionFixture()
        let state = try openExecutionSettings(in: app)
        XCTAssertTrue(state.label.contains("已启用"))

        app.buttons["morde.execution.stop"].tap()
        let confirm = app.buttons.matching(identifier: "morde.execution.stop.confirm").firstMatch
        XCTAssertTrue(confirm.waitForExistence(timeout: 5))
        app.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.2)).tap()
        XCTAssertFalse(app.buttons["morde.execution.stop.confirm"].exists,
                       "Tapping outside the confirmation popover must dismiss it without stopping tasks.")

        let stopped = expectation(for: NSPredicate(format: "label CONTAINS %@", "已停用"), evaluatedWith: state)
        XCTAssertEqual(XCTWaiter.wait(for: [stopped], timeout: 5), .timedOut)
        XCTAssertTrue(state.label.contains("已启用"), "Cancelling confirmation must leave cloud execution enabled.")
        app.terminate()
    }

    func testCloudExecutionStopButtonStaysDisabledWhileRequestIsInFlight() throws {
        let app = launchExecutionFixture(extraArguments: ["-morde.localExecutionDelayStopFixture"])
        let state = try openExecutionSettings(in: app)
        XCTAssertTrue(state.label.contains("已启用"), "Wait for the local execution state before opening confirmation.")
        let stop = app.buttons["morde.execution.stop"]
        stop.tap()
        let confirm = app.buttons.matching(identifier: "morde.execution.stop.confirm").firstMatch
        XCTAssertTrue(confirm.waitForExistence(timeout: 5))
        confirm.tap()

        let stopDisabled = expectation(for: NSPredicate(format: "isEnabled == false"), evaluatedWith: stop)
        wait(for: [stopDisabled], timeout: 5)
        XCTAssertTrue(stop.exists)
        XCTAssertTrue(stop.isHittable)
        XCTAssertFalse(app.buttons["morde.execution.stop.confirm"].exists)

        let repeatTap = stop.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.5))
        repeatTap.tap()
        repeatTap.tap()
        XCTAssertFalse(app.buttons["morde.execution.stop.confirm"].exists,
                       "A disabled stop control must not reopen confirmation during the in-flight request.")

        let disabled = expectation(for: NSPredicate(format: "label CONTAINS %@", "已停用"), evaluatedWith: state)
        wait(for: [disabled], timeout: 20)
        app.terminate()
    }

    func testCloudExecutionConflictRefreshesTheFixtureState() throws {
        let app = launchExecutionFixture(extraArguments: ["-morde.localExecutionConflictFixture"])
        let state = try openExecutionSettings(in: app)
        XCTAssertTrue(state.label.contains("已启用"))

        app.buttons["morde.execution.stop"].tap()
        let confirm = app.buttons.matching(identifier: "morde.execution.stop.confirm").firstMatch
        XCTAssertTrue(confirm.waitForExistence(timeout: 5))
        confirm.tap()

        let disabled = expectation(for: NSPredicate(format: "label CONTAINS %@", "已停用"), evaluatedWith: state)
        wait(for: [disabled], timeout: 10)
        XCTAssertTrue(app.buttons["morde.execution.enable"].waitForExistence(timeout: 5),
                      "A 409 must show the state read from the fixture, not a fabricated stop success.")
        XCTAssertFalse(app.buttons["morde.execution.stop"].exists)
        app.terminate()
    }

    func testCloudExecutionUnknownResultCanBeRetriedLocally() throws {
        let app = launchExecutionFixture(extraArguments: ["-morde.localExecutionUnknownFixture"])
        let state = try openExecutionSettings(in: app)
        XCTAssertTrue(state.label.contains("已启用"))

        app.buttons["morde.execution.stop"].tap()
        let firstConfirm = app.buttons.matching(identifier: "morde.execution.stop.confirm").firstMatch
        XCTAssertTrue(firstConfirm.waitForExistence(timeout: 5))
        firstConfirm.tap()

        let error = element("morde.execution.error", in: app)
        XCTAssertTrue(error.waitForExistence(timeout: 10), "The lost response must be visible as an unknown result.")
        XCTAssertTrue(state.label.contains("已启用"), "The failed response must not be shown as a completed stop.")

        app.buttons["morde.execution.stop"].tap()
        let retryConfirm = app.buttons.matching(identifier: "morde.execution.stop.confirm").firstMatch
        XCTAssertTrue(retryConfirm.waitForExistence(timeout: 5))
        retryConfirm.tap()

        let disabled = expectation(for: NSPredicate(format: "label CONTAINS %@", "已停用"), evaluatedWith: state)
        wait(for: [disabled], timeout: 20)
        app.terminate()
    }

    private func launchFixture(extraArguments: [String] = []) -> XCUIApplication {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localWorkItemFixture",
                              "-morde.localTaskPagesFixture"] + extraArguments + ["-AppleLanguages", "(zh-Hans)",
                              "-AppleLocale", "zh_CN"]
        app.launch()
        dismissLocalFixtureStartupError(in: app)
        let drawer = app.buttons["morde.shell.drawer"]
        let usernameField = app.textFields["morde.auth.username"]
        openMordeFromCurrentRoot(app)
        dismissLocalFixtureStartupError(in: app)
        if usernameField.waitForExistence(timeout: 10) {
            signInToFixtureAccount("mif-work-items-user", in: app)
        }
        XCTAssertFalse(usernameField.exists, "The fixture must finish signing in before page assertions run.")
        XCTAssertTrue(drawer.waitForExistence(timeout: 10))
        let chatTab = app.buttons["morde.shell.tab.chat"]
        if chatTab.waitForExistence(timeout: 5), !chatTab.isSelected { chatTab.tap() }
        let mainChat = app.buttons["morde.shell.mainChat"]
        if !mainChat.exists { drawer.tap() }
        if mainChat.waitForExistence(timeout: 5) { mainChat.tap() }
        if !extraArguments.contains("-morde.localConversationFailureFixture") {
            XCTAssertTrue(app.buttons["morde.shell.tab.tasks"].waitForExistence(timeout: 15))
        }
        return app
    }

    private func launchExecutionFixture(extraArguments: [String] = []) -> XCUIApplication {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", "https://127.0.0.1:49713", "-morde.localRegistrationStub",
                              "-morde.localRegistrationApproved", "-morde.localTaskPagesFixture",
                              "-morde.localExecutionFixture"] + extraArguments + [
                              "-AppleLanguages", "(zh-Hans)", "-AppleLocale", "zh_CN"]
        app.launch()
        dismissLocalFixtureStartupError(in: app)
        let drawer = app.buttons["morde.shell.drawer"]
        let usernameField = app.textFields["morde.auth.username"]
        openMordeFromCurrentRoot(app)
        dismissLocalFixtureStartupError(in: app)
        if usernameField.waitForExistence(timeout: 10) {
            signInToFixtureAccount("mif-execution-user", in: app)
        }
        XCTAssertFalse(usernameField.exists, "The fixture must finish signing in before page assertions run.")
        XCTAssertTrue(drawer.waitForExistence(timeout: 10))
        let chatTab = app.buttons["morde.shell.tab.chat"]
        if chatTab.waitForExistence(timeout: 5), !chatTab.isSelected { chatTab.tap() }
        return app
    }

    private func openExecutionSettings(in app: XCUIApplication) throws -> XCUIElement {
        openSettingsFromDrawer(app)
        let settings = app.buttons["morde.settings.executionControl"]
        guard settings.waitForExistence(timeout: 3) else {
            throw XCTSkip("Cloud execution controls are excluded from the MINIS_CLOUD_ONLY target.")
        }
        for _ in 0..<8 where !settings.isHittable { app.swipeUp() }
        settings.tap()
        let state = element("morde.execution.state", in: app)
        XCTAssertTrue(state.waitForExistence(timeout: 10))
        return state
    }

    private func selectFilter(_ filter: String, in app: XCUIApplication) {
        app.buttons["morde.workItems.filter.\(filter)"].tap()
    }

    private func element(_ identifier: String, in app: XCUIApplication) -> XCUIElement {
        app.descendants(matching: .any).matching(identifier: identifier).firstMatch
    }

    private func dismissLocalFixtureStartupError(in app: XCUIApplication) {
        let error = app.alerts["请求失败"]
        if error.waitForExistence(timeout: 2) {
            error.buttons["确定"].tap()
        }
    }

    private func scrollUntilHittable(_ element: XCUIElement, in app: XCUIApplication) {
        dismissKeyboard(in: app)
        let createScroll = self.element("morde.workItems.create.scroll", in: app)
        if createScroll.exists {
            for _ in 0..<12 where !element.isHittable { createScroll.swipeUp() }
            XCTAssertTrue(element.isHittable, "Expected \(element.identifier) to be tappable in its form.")
            return
        }
        let workItemsScroll = app.descendants(matching: .any)
            .matching(identifier: "morde.workItems.scroll").firstMatch
        if workItemsScroll.exists {
            for _ in 0..<12 {
                let frame = element.frame
                let viewport = workItemsScroll.frame
                let tabTop = ["chat", "tasks", "approvals", "resources"]
                    .map { app.buttons["morde.shell.tab.\($0)"] }
                    .filter(\.exists)
                    .map(\.frame.minY)
                    .min() ?? viewport.maxY
                if element.isHittable && frame.midY < min(viewport.maxY, tabTop) - 8 { return }
                if frame.minY < viewport.minY {
                    workItemsScroll.swipeDown()
                } else {
                    workItemsScroll.swipeUp()
                }
            }
            let viewport = workItemsScroll.frame
            let tabTop = ["chat", "tasks", "approvals", "resources"]
                .map { app.buttons["morde.shell.tab.\($0)"] }
                .filter(\.exists)
                .map(\.frame.minY)
                .min() ?? viewport.maxY
            XCTAssertTrue(element.isHittable && element.frame.midY < min(viewport.maxY, tabTop) - 8,
                          "Expected \(element.identifier)'s tap point to clear the shell tab bar.")
        } else {
            for _ in 0..<12 where !element.isHittable { app.swipeUp() }
        }
    }

    private func signInToFixtureAccount(_ username: String, in app: XCUIApplication) {
        let usernameField = app.textFields["morde.auth.username"]
        XCTAssertTrue(usernameField.waitForExistence(timeout: 10))
        let usernameReady = XCTNSPredicateExpectation(
            predicate: NSPredicate(format: "exists == true AND hittable == true"), object: usernameField)
        XCTAssertEqual(XCTWaiter.wait(for: [usernameReady], timeout: 10), .completed)
        usernameField.tap()
        let existingUsername = usernameField.value as? String ?? ""
        if !existingUsername.isEmpty && existingUsername != usernameField.placeholderValue {
            usernameField.typeText(String(repeating: XCUIKeyboardKey.delete.rawValue,
                                          count: existingUsername.count))
        }
        usernameField.tap()
        usernameField.typeText(username)
        app.secureTextFields["morde.auth.password"].tap()
        app.secureTextFields["morde.auth.password"].typeText("LocalAccountFixture!2468")
        app.buttons["morde.auth.submit"].tap()
        let signedIn = XCTNSPredicateExpectation(predicate: NSPredicate(format: "exists == false"),
                                                 object: usernameField)
        XCTAssertEqual(XCTWaiter.wait(for: [signedIn], timeout: 15), .completed,
                       "The sign-in form must close after a successful local fixture login.")
        dismissKeyboard(in: app)
    }

    private func dismissKeyboard(in app: XCUIApplication) {
        guard app.keyboards.firstMatch.exists else { return }
        let workItemDone = app.buttons["morde.workItems.keyboardDone"]
        if workItemDone.exists && workItemDone.isHittable {
            workItemDone.tap()
        } else {
            let transcript = element("morde.chat.transcript", in: app)
            if transcript.exists && transcript.isHittable {
                transcript.tap()
            } else {
                let returnKey = app.keyboards.buttons["Return"]
                if returnKey.exists && returnKey.isHittable { returnKey.tap() }
            }
        }
    }

    private func openMordeFromCurrentRoot(_ app: XCUIApplication) {
        let changedAccountAlert = app.alerts.firstMatch
        let changedAccountMessage = changedAccountAlert.staticTexts.matching(
            NSPredicate(format: "label CONTAINS %@", "账号已变化")
        ).firstMatch
        if changedAccountMessage.waitForExistence(timeout: 1) {
            changedAccountAlert.buttons["确定"].tap()
        }

        let usernameField = app.textFields["morde.auth.username"]
        if usernameField.waitForExistence(timeout: 2), usernameField.isHittable { return }
        let drawer = app.buttons["morde.shell.drawer"]
        if drawer.waitForExistence(timeout: 2), drawer.isHittable {
            for _ in 0..<4 {
                if usernameField.isHittable { return }
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
                if chatTab.exists && chatTab.isHittable && !chatTab.isSelected {
                    chatTab.tap()
                    continue
                }
                return
            }
            return
        }
        let cloudTasks = app.buttons.matching(NSPredicate(
            format: "label CONTAINS[c] %@ OR label CONTAINS %@", "cloud tasks", "云端任务"
        )).firstMatch
        if cloudTasks.waitForExistence(timeout: 2), cloudTasks.isHittable {
            cloudTasks.tap()
            XCTAssertTrue(usernameField.waitForExistence(timeout: 10))
            return
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
        if drawer.waitForExistence(timeout: 2) && drawer.isHittable {
            drawer.tap()
            let mainChat = app.buttons["morde.shell.mainChat"]
            XCTAssertTrue(mainChat.waitForExistence(timeout: 5))
            mainChat.tap()
        } else {
            let usernameReady = XCTNSPredicateExpectation(
                predicate: NSPredicate(format: "exists == true AND hittable == true"), object: usernameField)
            XCTAssertEqual(XCTWaiter.wait(for: [usernameReady], timeout: 5), .completed,
                           "Morde chat should be the default root.")
        }
    }

    private func openSettingsFromDrawer(_ app: XCUIApplication) {
        let drawer = app.buttons["morde.shell.drawer"]
        XCTAssertTrue(drawer.waitForExistence(timeout: 5))
        let settings = app.buttons["morde.shell.settings"]
        if !settings.waitForExistence(timeout: 2) { drawer.tap() }
        XCTAssertTrue(settings.waitForExistence(timeout: 5))
        let settingsVisible = expectation(for: NSPredicate(format: "isHittable == true"), evaluatedWith: settings)
        wait(for: [settingsVisible], timeout: 5)
        settings.tap()
    }
}
