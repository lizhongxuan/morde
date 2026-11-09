import XCTest

@MainActor
final class MordeChatModelGateUITests: XCTestCase {
    func testConfirmedMissingCloudModelRetainsDraftAndExplainsSetup() {
        let app = XCUIApplication()
        launchLocalChat(app, additionalArguments: ["-morde.localNoCloudModelFixture"])
        XCTAssertTrue(app.staticTexts["Main chat is ready."].waitForExistence(timeout: 20),
                      "The main conversation loads after model status, so the send gate has an authoritative result.")

        let composer = app.textFields["morde.chat.composer"]
        XCTAssertTrue(composer.waitForExistence(timeout: 15))
        let send = app.buttons["morde.task.send"]
        let microphone = app.buttons["morde.chat.composer.mic"]
        XCTAssertFalse(send.exists, "An empty composer must not present a send action.")
        XCTAssertTrue(microphone.exists, "The empty composer presents voice input instead.")
        composer.tap()
        composer.typeText("请先配置模型后再发送")

        XCTAssertTrue(send.waitForExistence(timeout: 10))
        XCTAssertFalse(microphone.exists, "The send action replaces voice input when text is present.")
        XCTAssertTrue(send.isEnabled, "Text keeps the send button available when model configuration is missing.")
        send.tap()

        let alert = app.alerts["请求失败"]
        XCTAssertTrue(alert.waitForExistence(timeout: 10))
        XCTAssertTrue(alert.staticTexts["请先配置云端模型，输入内容已保留。"].exists)
        alert.buttons["确定"].tap()
        XCTAssertEqual(composer.value as? String, "请先配置模型后再发送")
    }

    func testUnknownCloudModelStatusDoesNotBlockMessageAcceptance() {
        let app = XCUIApplication()
        launchLocalChat(app, additionalArguments: [
            "-morde.localMessageSendWithoutAutoRoutingFixture",
            "-morde.localDelayConversationHistoryFixture",
            "-morde.localUnknownCloudModelFixture",
            "-morde.localFixtureDiagnostics",
        ])
        XCTAssertTrue(app.staticTexts["Main chat is ready."].waitForExistence(timeout: 20))

        let composer = app.textFields["morde.chat.composer"]
        XCTAssertTrue(composer.waitForExistence(timeout: 15))
        composer.tap()
        composer.typeText("模型状态未知时仍可发送")

        let send = app.buttons["morde.task.send"]
        XCTAssertTrue(send.waitForExistence(timeout: 10))
        XCTAssertTrue(send.isEnabled)
        send.tap()

        XCTAssertTrue(app.staticTexts["模型状态未知时仍可发送"].waitForExistence(timeout: 15))
        XCTAssertTrue(app.staticTexts["已发送"].waitForExistence(timeout: 20),
                      "Unknown model status and missing auto-routing capability must not block acceptance.")
        XCTAssertFalse(app.staticTexts["请先配置云端模型，输入内容已保留。"].exists)
    }

    func testUnavailableHostAndWorkspaceListsDoNotBlockUnassignedTextMessage() {
        let app = XCUIApplication()
        launchLocalChat(app, additionalArguments: [
            "-morde.localResourceListFailureFixture",
            "-morde.localMessageSendWithoutAutoRoutingFixture",
            "-morde.localFixtureDiagnostics",
        ])
        XCTAssertTrue(app.staticTexts["Main chat is ready."].waitForExistence(timeout: 20))

        let content = "电脑与 Workspace 列表不可用时仍能发送普通对话"
        let composer = app.textFields["morde.chat.composer"]
        XCTAssertTrue(composer.waitForExistence(timeout: 15))
        composer.tap()
        composer.typeText(content)

        let send = app.buttons["morde.task.send"]
        XCTAssertTrue(send.waitForExistence(timeout: 10))
        XCTAssertTrue(send.isEnabled)
        send.tap()

        XCTAssertTrue(app.staticTexts[content].waitForExistence(timeout: 15))
        XCTAssertTrue(app.staticTexts["已发送"].waitForExistence(timeout: 20),
                      "Unavailable Host and Workspace lists must not block a message without a selected target.")
        XCTAssertFalse(app.staticTexts["Synthetic resource list unavailable"].exists,
                       "Resource list errors belong to optional tools, not the conversation send path.")
    }

    func testSelectedUnavailableHostDoesNotBlockMessageAcceptance() {
        let app = XCUIApplication()
        launchLocalChat(app, additionalArguments: [
            "-morde.localResourceAcceptanceFixture",
            "-morde.localMessageSendWithoutAutoRoutingFixture",
            "-morde.localSelectedUnavailableHostSendFixture",
        ])
        XCTAssertTrue(app.staticTexts["Main chat is ready."].waitForExistence(timeout: 20))

        let actions = app.descendants(matching: .any)["morde.shell.chatActions"]
        XCTAssertTrue(actions.waitForExistence(timeout: 10))
        actions.press(forDuration: 1)
        let taskOptions = app.buttons["morde.shell.taskOptions"]
        XCTAssertTrue(taskOptions.waitForExistence(timeout: 5))
        taskOptions.tap()

        let hostTarget = app.descendants(matching: .any)["morde.chat.hostTarget"]
        if !hostTarget.isHittable { app.swipeUp() }
        XCTAssertTrue(hostTarget.waitForExistence(timeout: 10))
        hostTarget.tap()
        let stoppedHost = app.buttons.matching(NSPredicate(format: "label CONTAINS %@", "Stopped host")).firstMatch
        XCTAssertTrue(stoppedHost.waitForExistence(timeout: 10), "The fixture must expose its stopped Host as a selectable target.")
        stoppedHost.tap()
        XCTAssertTrue(app.staticTexts["电脑状态：已停止 · AI 控制"].waitForExistence(timeout: 5))
        app.buttons["morde.chat.taskOptions.done"].tap()

        let content = "请使用已停止的云电脑处理这个任务"
        let composer = app.textFields["morde.chat.composer"]
        XCTAssertTrue(composer.waitForExistence(timeout: 10))
        composer.tap()
        composer.typeText(content)
        let send = app.buttons["morde.task.send"]
        XCTAssertTrue(send.waitForExistence(timeout: 10))
        XCTAssertTrue(send.isEnabled)
        send.tap()

        XCTAssertTrue(app.staticTexts[content].waitForExistence(timeout: 15))
        XCTAssertTrue(app.staticTexts["已发送"].waitForExistence(timeout: 20),
                      "A selected but unavailable Host must not block message acceptance; the request fixture only accepts h0000002.")
        XCTAssertEqual(app.staticTexts.matching(NSPredicate(format: "label == %@", content)).count, 1,
                       "The selected Host must still result in one accepted user message.")
        app.terminate()
    }

    func testSelectedUnavailableWorkspaceDoesNotBlockMessageAcceptance() {
        let app = XCUIApplication()
        launchLocalChat(app, additionalArguments: [
            "-morde.localResourceAcceptanceFixture",
            "-morde.localMessageSendWithoutAutoRoutingFixture",
            "-morde.localSelectedUnavailableWorkspaceSendFixture",
        ])
        XCTAssertTrue(app.staticTexts["Main chat is ready."].waitForExistence(timeout: 20))

        let actions = app.descendants(matching: .any)["morde.shell.chatActions"]
        XCTAssertTrue(actions.waitForExistence(timeout: 10))
        actions.press(forDuration: 1)
        let taskOptions = app.buttons["morde.shell.taskOptions"]
        XCTAssertTrue(taskOptions.waitForExistence(timeout: 5))
        taskOptions.tap()

        let hostTarget = app.descendants(matching: .any)["morde.chat.hostTarget"]
        if !hostTarget.isHittable { app.swipeUp() }
        XCTAssertTrue(hostTarget.waitForExistence(timeout: 10))
        hostTarget.tap()
        let unreadyBindingHost = app.buttons.matching(NSPredicate(format: "label CONTAINS %@", "Unready binding host")).firstMatch
        XCTAssertTrue(unreadyBindingHost.waitForExistence(timeout: 10))
        unreadyBindingHost.tap()

        let workspaceTarget = app.descendants(matching: .any)["morde.chat.workspaceTarget"]
        XCTAssertTrue(workspaceTarget.waitForExistence(timeout: 10))
        workspaceTarget.tap()
        let syncingWorkspace = app.buttons.matching(NSPredicate(format: "label CONTAINS %@", "Syncing Workspace")).firstMatch
        XCTAssertTrue(syncingWorkspace.waitForExistence(timeout: 10), "The fixture must expose its syncing Workspace as a selectable target.")
        syncingWorkspace.tap()
        XCTAssertTrue(app.staticTexts["任务需要使用此 Workspace 时，助手会检查其状态。"].waitForExistence(timeout: 5))
        app.buttons["morde.chat.taskOptions.done"].tap()

        let content = "请使用同步中的 Workspace 处理这个任务"
        let composer = app.textFields["morde.chat.composer"]
        XCTAssertTrue(composer.waitForExistence(timeout: 10))
        composer.tap()
        composer.typeText(content)
        let send = app.buttons["morde.task.send"]
        XCTAssertTrue(send.waitForExistence(timeout: 10))
        XCTAssertTrue(send.isEnabled)
        send.tap()

        XCTAssertTrue(app.staticTexts[content].waitForExistence(timeout: 15))
        XCTAssertTrue(app.staticTexts["已发送"].waitForExistence(timeout: 20),
                      "A selected but unavailable Workspace must not block acceptance; the request fixture only accepts h0000003/w0000002.")
        XCTAssertEqual(app.staticTexts.matching(NSPredicate(format: "label == %@", content)).count, 1,
                       "The selected Workspace must still result in one accepted user message.")
        app.terminate()
    }

    func testFailedMessageCanBeRetriedAfterLegacyServiceUpgrade() {
        let app = XCUIApplication()
        launchLocalChat(app, additionalArguments: [
            "-morde.localServiceUpgradeRetryFixture",
            "-AppleLanguages", "(en)", "-AppleLocale", "en_US",
        ])
        XCTAssertTrue(app.staticTexts["Main chat is ready."].waitForExistence(timeout: 20))

        let content = "Keep this message until the service upgrades"
        let composer = app.textFields["morde.chat.composer"]
        XCTAssertTrue(composer.waitForExistence(timeout: 15))
        composer.tap()
        composer.typeText(content)
        app.buttons["morde.task.send"].tap()

        let retry = app.buttons["Retry"]
        XCTAssertTrue(retry.waitForExistence(timeout: 15),
                      "A confirmed rejection from an older service must keep the message and offer recovery.")
        XCTAssertTrue(app.staticTexts["服务尚不支持自动分流，请升级后重试"].exists)
        XCTAssertTrue(app.staticTexts[content].exists)
        retry.tap()

        XCTAssertTrue(app.staticTexts["Sent"].waitForExistence(timeout: 20),
                      "After capabilities upgrade, retry should submit the preserved message.")
        XCTAssertEqual(app.staticTexts.matching(NSPredicate(format: "label == %@", content)).count, 1,
                       "Recovery must leave one visible copy of the original message.")
        XCTAssertFalse(app.buttons["Retry"].exists)
        app.terminate()
    }

    func testPersonalAttachmentUploadResumesMissingChunksAfterAppRestart() {
        let app = XCUIApplication()
        let loginName = "attachment-resume-\(UUID().uuidString.prefix(8).lowercased())"
        let content = "Resume my attachment after restarting the app"
        launchLocalChat(app, loginName: loginName, resetExistingAccount: true, additionalArguments: [
            "-morde.localChatAttachmentFixture", "-morde.localChatAttachmentResumeFixture",
            "-morde.localChatAttachmentScopedAccountFixture",
            "-morde.localChatAttachmentFixtureReset", "-morde.localChatAttachmentInterruptFixture",
        ])

        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].waitForExistence(timeout: 40),
                      "The fixture should stage an attachment larger than one upload chunk.")
        let composer = app.textFields["morde.chat.composer"]
        XCTAssertTrue(composer.waitForExistence(timeout: 15))
        composer.tap()
        composer.typeText(content)
        let send = app.buttons["morde.task.send"]
        XCTAssertTrue(send.waitForExistence(timeout: 10))
        XCTAssertTrue(send.isEnabled)
        send.tap()

        XCTAssertTrue(app.descendants(matching: .any)["morde.chat.message.m9999999"]
            .waitForExistence(timeout: 20), "The text should be accepted before attachment upload is interrupted.")
        let pendingAttachmentStatus = app.staticTexts.matching(NSPredicate(
            format: "label == %@ OR label == %@", "已发送，正在上传附件", "已发送，附件需要处理"
        )).firstMatch
        XCTAssertTrue(pendingAttachmentStatus.waitForExistence(timeout: 40),
                      "The accepted message should remain visible while attachment recovery is pending.")
        app.terminate()

        launchLocalChat(app, loginName: loginName, resetExistingAccount: false, additionalArguments: [
            "-morde.localChatAttachmentFixture", "-morde.localChatAttachmentResumeFixture",
            "-morde.localChatAttachmentScopedAccountFixture",
            "-morde.localChatAttachmentNoAutoImportFixture", "-morde.localChatAttachmentFixtureCleanup",
        ])
        XCTAssertTrue(app.staticTexts[content].waitForExistence(timeout: 20),
                      "The accepted message should survive the app restart.")
        XCTAssertTrue(app.staticTexts["已发送"].waitForExistence(timeout: 90),
                      "The restarted app should skip the persisted chunk, upload only missing chunks, and link the file.")
        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].exists)
        XCTAssertFalse(app.staticTexts.matching(NSPredicate(
            format: "label CONTAINS %@", "morde-解析-"
        )).firstMatch.exists, "Internal image derivatives must not appear as user attachments.")
        XCTAssertFalse(app.staticTexts.matching(NSPredicate(
            format: "label CONTAINS %@", "Synthetic upload interrupted after first chunk"
        )).firstMatch.exists)
        app.terminate()
    }

    func testPlatformDefaultModelWithoutPersonalConnectionsDoesNotBlockMessageAcceptance() {
        let app = XCUIApplication()
        launchLocalChat(app, additionalArguments: [
            "-morde.localMessageSendWithoutAutoRoutingFixture",
            "-morde.localPlatformOnlyModelFixture",
            "-morde.localFixtureDiagnostics",
        ])
        XCTAssertTrue(app.staticTexts["Main chat is ready."].waitForExistence(timeout: 20))

        let composer = app.textFields["morde.chat.composer"]
        XCTAssertTrue(composer.waitForExistence(timeout: 15))
        composer.tap()
        composer.typeText("平台默认模型已配置时允许发送")

        let send = app.buttons["morde.task.send"]
        XCTAssertTrue(send.waitForExistence(timeout: 10))
        XCTAssertTrue(send.isEnabled)
        send.tap()

        XCTAssertTrue(app.staticTexts["平台默认模型已配置时允许发送"].waitForExistence(timeout: 15))
        XCTAssertTrue(app.staticTexts["已发送"].waitForExistence(timeout: 20))
        XCTAssertFalse(app.staticTexts["请先配置云端模型，消息已保留在本机。"].exists)
    }

    func testCombinedReplyShowsEachMessageAndTaskSource() {
        let app = XCUIApplication()
        launchLocalChat(app, additionalArguments: ["-morde.localChatCombinedSourcesFixture"])
        app.buttons["morde.shell.tab.chat"].tap()

        let first = app.descendants(matching: .any)
            .matching(identifier: "morde.chat.message.source.q1111111.r2.e5").firstMatch
        let second = app.descendants(matching: .any)
            .matching(identifier: "morde.chat.message.source.q2222222.r1.e8").firstMatch
        XCTAssertTrue(first.waitForExistence(timeout: 20))
        XCTAssertTrue(second.waitForExistence(timeout: 20))
        XCTAssertTrue(first.label.contains("关联消息：请整理第一项：生成摘要"))
        XCTAssertTrue(first.label.contains("任务 q1111111，修订 2，结果事件 5"))
        XCTAssertTrue(first.label.contains("第一项：摘要已完成。"))
        XCTAssertTrue(second.label.contains("关联消息：再核对第二项：列出风险"))
        XCTAssertTrue(second.label.contains("任务 q2222222，修订 1，结果事件 8"))
        XCTAssertTrue(second.label.contains("第二项：风险已列出。"))

        let assistantMessage = app.descendants(matching: .any)
            .matching(identifier: "morde.chat.message.m3333333").firstMatch
        XCTAssertTrue(assistantMessage.exists)
        XCTAssertFalse(assistantMessage.staticTexts["Morde"].exists,
                       "Assistant messages must not repeat the Morde label above each bubble.")
    }

    func testAssistantReplyRendersInlineMarkdownWithoutRepeatedName() {
        let app = XCUIApplication()
        launchLocalChat(app, additionalArguments: ["-morde.localChatVisualFixture"])
        app.buttons["morde.shell.tab.chat"].tap()

        let assistantMessage = app.descendants(matching: .any)
            .matching(identifier: "morde.chat.message.mvisual002").firstMatch
        XCTAssertTrue(assistantMessage.waitForExistence(timeout: 20))
        XCTAssertTrue(assistantMessage.staticTexts.matching(NSPredicate(
            format: "label CONTAINS %@", "资料结构"
        )).firstMatch.exists, "The assistant fixture should render the Markdown content.")
        XCTAssertFalse(assistantMessage.staticTexts.matching(NSPredicate(
            format: "label CONTAINS %@", "**"
        )).firstMatch.exists, "Markdown control characters must not leak into rendered text.")
        XCTAssertFalse(assistantMessage.staticTexts["Morde"].exists,
                       "The chat header identifies Morde; individual assistant bubbles do not repeat it.")

        let latestMessage = app.descendants(matching: .any)
            .matching(identifier: "morde.chat.message.mvisual007").firstMatch
        let composer = app.textFields["morde.chat.composer"]
        XCTAssertTrue(latestMessage.exists)
        XCTAssertTrue(composer.exists)
        XCTAssertLessThan(latestMessage.frame.maxY, composer.frame.minY,
                          "Opening a conversation should show the latest reply above the composer.")
        XCTAssertFalse(app.buttons["morde.chat.latestButton"].exists,
                       "The latest-message control should be hidden while already at the latest reply.")
    }

    private func launchLocalChat(_ app: XCUIApplication, accountID: String? = nil,
                                 loginName: String = "morde-model-gate-test",
                                 resetExistingAccount: Bool = true,
                                 additionalArguments: [String] = []) {
        let resolvedAccountID = accountID ?? "u" + String(UUID().uuidString.replacingOccurrences(of: "-", with: "").lowercased().prefix(12))
        let fixtureArguments = [
            "-morde.server", "https://127.0.0.1:49713",
            "-morde.localRegistrationStub", "-morde.localRegistrationApproved", "-morde.localMainChatFixture",
        ] + additionalArguments
        let usesScopedAttachmentAccount = additionalArguments.contains("-morde.localChatAttachmentScopedAccountFixture")
        let launchArguments = fixtureArguments + (usesScopedAttachmentAccount ? [] : ["-morde.localFixtureAccountID", resolvedAccountID])
        let deferAttachmentImport = resetExistingAccount &&
            additionalArguments.contains("-morde.localChatAttachmentFixture") &&
            !additionalArguments.contains("-morde.localChatAttachmentNoAutoImportFixture")
        app.launchArguments = deferAttachmentImport
            ? fixtureArguments + ["-morde.localChatAttachmentNoAutoImportFixture"] +
                (usesScopedAttachmentAccount ? [] : ["-morde.localFixtureAccountID", resolvedAccountID])
            : launchArguments
        app.launch()
        dismissStartupError(in: app)
        if resetExistingAccount {
            if !app.textFields["morde.auth.username"].exists,
               app.buttons["morde.shell.drawer"].waitForExistence(timeout: 3) {
                signOutCurrentMordeAccount(app)
            }
            app.terminate()
            app.launchArguments = launchArguments
            app.launch()
            dismissStartupError(in: app)
        }
        let usernameField = app.textFields["morde.auth.username"]
        if !usernameField.waitForExistence(timeout: 2) {
            let loginEntry = app.buttons.matching(
                NSPredicate(format: "label == %@ OR label == %@", "登录", "Sign in")
            ).firstMatch
            if loginEntry.waitForExistence(timeout: 2) { loginEntry.tap() }
        }
        if usernameField.waitForExistence(timeout: 5) {
            usernameField.tap()
            if let currentName = usernameField.value as? String,
               currentName != usernameField.placeholderValue, !currentName.isEmpty {
                usernameField.typeText(String(repeating: XCUIKeyboardKey.delete.rawValue, count: currentName.count))
            }
            usernameField.typeText(loginName)
            let passwordField = app.secureTextFields["morde.auth.password"]
            passwordField.tap()
            passwordField.typeText("local-password")
            app.buttons["morde.auth.submit"].tap()
        }
        XCTAssertTrue(app.buttons["morde.shell.tab.tasks"].waitForExistence(timeout: 30),
                      "The app should reach the local authenticated shell.")
        if !app.buttons["morde.shell.drawer"].exists {
            app.buttons["morde.shell.tab.chat"].tap()
        }
        XCTAssertTrue(app.buttons["morde.shell.drawer"].waitForExistence(timeout: 10),
                      "The local chat tests require the authenticated conversation route.")
        XCTAssertTrue(app.buttons["morde.shell.tab.chat"].exists)
    }

    private func dismissStartupError(in app: XCUIApplication) {
        let alert = app.alerts.matching(
            NSPredicate(format: "label == %@ OR label == %@", "请求失败", "Request failed")
        ).firstMatch
        guard alert.waitForExistence(timeout: 2) else { return }
        let confirm = alert.buttons.matching(
            NSPredicate(format: "label == %@ OR label == %@", "确定", "OK")
        ).firstMatch
        if confirm.exists { confirm.tap() }
    }

    private func signOutCurrentMordeAccount(_ app: XCUIApplication) {
        app.buttons["morde.shell.drawer"].tap()
        let settings = app.buttons["morde.shell.settings"]
        XCTAssertTrue(settings.waitForExistence(timeout: 5))
        settings.tap()
        let accountSettings = app.buttons["morde.settings.account"]
        XCTAssertTrue(accountSettings.waitForExistence(timeout: 10))
        accountSettings.tap()
        let signOut = app.buttons["morde.account.signOut"]
        if !signOut.waitForExistence(timeout: 5) {
            let loginEntry = app.buttons.matching(
                NSPredicate(format: "label == %@ OR label == %@", "登录", "Sign in")
            ).firstMatch
            XCTAssertTrue(loginEntry.exists,
                          "The current account screen should offer sign-out or a sign-in entry for a signed-out session.")
            return
        }
        for _ in 0..<8 where signOut.exists && !signOut.isHittable { app.scrollViews.firstMatch.swipeUp() }
        XCTAssertTrue(signOut.isHittable)
        signOut.tap()
        let confirmSignOut = app.descendants(matching: .any)
            .matching(identifier: "morde.account.signOut.confirm").firstMatch
        XCTAssertTrue(confirmSignOut.waitForExistence(timeout: 5))
        confirmSignOut.tap()
        XCTAssertTrue(app.textFields["morde.auth.username"].waitForExistence(timeout: 20))
    }
}
