import Foundation
import XCTest

@MainActor
final class IOS04CloudAcceptanceUITests: XCTestCase {
    func testLocalPortalAuthenticatedShell() throws {
        guard let account = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_USER"],
              let password = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_PASSWORD"],
              let server = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_SERVER"],
              let url = URL(string: server), url.scheme == "http",
              ["localhost", "127.0.0.1"].contains(url.host ?? ""),
              let port = url.port, (1...65535).contains(port) else {
            throw XCTSkip("Set local Portal credentials and a loopback HTTP URL with an explicit port in the test environment.")
        }

        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", server]
        app.launch()
        let alert = app.alerts.firstMatch
        if alert.waitForExistence(timeout: 2) { alert.buttons["确定"].tap() }

        let accountField = app.textFields["morde.auth.username"]
        XCTAssertTrue(accountField.waitForExistence(timeout: 15))
        accountField.tap()
        accountField.typeText(account)
        let passwordField = app.secureTextFields["morde.auth.password"]
        passwordField.tap()
        passwordField.typeText(password)
        app.buttons["morde.auth.submit"].tap()

        let loginFormDismissed = expectation(
            for: NSPredicate(format: "exists == false"),
            evaluatedWith: accountField
        )
        wait(for: [loginFormDismissed], timeout: 30)
        XCTAssertFalse(app.secureTextFields["morde.auth.password"].exists)
        XCTAssertTrue(app.buttons["morde.shell.tab.chat"].waitForExistence(timeout: 30))
        XCTAssertTrue(app.buttons["morde.shell.tab.tasks"].exists)
        XCTAssertTrue(app.buttons["morde.shell.tab.approvals"].exists)
        XCTAssertTrue(app.buttons["morde.shell.tab.resources"].exists)
    }

    // nanoMuse: Keep this opt-in UI path confined to explicitly configured loopback services.
    func testLocalPortalChatReadsAndSendsMessagesWithoutAComputer() throws {
        guard let account = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_USER"],
              let password = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_PASSWORD"],
              let server = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_SERVER"],
              let url = URL(string: server), url.scheme == "http",
              ["localhost", "127.0.0.1"].contains(url.host ?? ""),
              let port = url.port, (1...65535).contains(port) else {
            throw XCTSkip("Set local Portal credentials and a loopback HTTP URL with an explicit port in the test environment.")
        }
        let expectedMessage = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_EXPECTED_MESSAGE"] ?? ""
        let outgoingMessage = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_OUTGOING_MESSAGE"]
            ?? "来自 iOS 的本地服务验收消息"
        let expectedProgress = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_EXPECTED_PROGRESS"] ?? ""
        let expectedReply = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_EXPECTED_REPLY"]
            ?? "本地验收回复：" + outgoingMessage
        let replyTimeout = TimeInterval(Int(
            ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_REPLY_TIMEOUT"] ?? ""
        ) ?? 30)

        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", server]
        app.launch()

        let accountField = app.textFields["morde.auth.username"]
        if accountField.waitForExistence(timeout: 5) {
            enterLocalPortalCredentials(app: app, account: account, password: password)
        }

        let chatTab = app.buttons["morde.shell.tab.chat"]
        XCTAssertTrue(chatTab.waitForExistence(timeout: 30))
        chatTab.tap()
        let transcript = app.descendants(matching: .any).matching(identifier: "morde.chat.transcript").firstMatch
        XCTAssertTrue(transcript.waitForExistence(timeout: 15))
        if !expectedMessage.isEmpty {
            let browserMessage = app.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", expectedMessage)).firstMatch
            XCTAssertTrue(browserMessage.waitForExistence(timeout: 20),
                          "The iOS client should read the message already accepted by the local Portal.")
        }

        let composer = app.descendants(matching: .any).matching(identifier: "morde.chat.composer").firstMatch
        let send = app.buttons["morde.task.send"]
        XCTAssertTrue(composer.waitForExistence(timeout: 10))
        composer.tap()
        composer.typeText(outgoingMessage)
        XCTAssertTrue(send.waitForExistence(timeout: 10))
        XCTAssertTrue(send.isEnabled, "The iOS composer should allow a text message without any computer target.")
        send.tap()

        let sentMessage = app.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", outgoingMessage)).firstMatch
        XCTAssertTrue(sentMessage.waitForExistence(timeout: 20), "The iOS message should be visible after server acceptance.")
        if !expectedProgress.isEmpty {
            let visibleProgress = app.staticTexts.matching(
                NSPredicate(format: "label CONTAINS %@", expectedProgress)
            ).firstMatch
            XCTAssertTrue(visibleProgress.waitForExistence(timeout: 30),
                          "The iOS chat should show progress from the accepted task before its result arrives.")
        }
        let syntheticReply = app.staticTexts.matching(
            NSPredicate(format: "label CONTAINS %@", expectedReply)
        ).firstMatch
        XCTAssertTrue(syntheticReply.waitForExistence(timeout: replyTimeout),
                      "The local synthetic executor reply should return to the same iOS conversation.")
        app.terminate()
    }

    func testLocalPortalLongMessageUploadResumesAfterAppRestart() async throws {
        guard let account = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_USER"],
              let password = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_PASSWORD"],
              let server = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_SERVER"],
              let url = URL(string: server), url.scheme == "http",
              ["localhost", "127.0.0.1"].contains(url.host ?? ""),
              let port = url.port, (1...65535).contains(port) else {
            throw XCTSkip("Set local Portal credentials and a loopback HTTP URL with an explicit port in the test environment.")
        }

        let expectedMessage = String(repeating: "中文abc🙂", count: 3000) + "尾部完整"
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", server, "-morde.testConversationMessageUploadHTTPRecovery"]
        app.launch()
        let accountField = app.textFields["morde.auth.username"]
        if accountField.waitForExistence(timeout: 5) {
            enterLocalPortalCredentials(app: app, account: account, password: password)
            let loginFormDismissed = XCTNSPredicateExpectation(
                predicate: NSPredicate(format: "exists == false"), object: accountField)
            await fulfillment(of: [loginFormDismissed], timeout: 30)
        }

        let chatTab = app.buttons["morde.shell.tab.chat"]
        XCTAssertTrue(chatTab.waitForExistence(timeout: 30))
        chatTab.tap()
        let composer = app.descendants(matching: .any).matching(identifier: "morde.chat.composer").firstMatch
        XCTAssertTrue(composer.waitForExistence(timeout: 20))
        let stagedMessage = composer.value as? String ?? ""
        XCTAssertEqual(stagedMessage.utf8.count, expectedMessage.utf8.count)
        XCTAssertTrue(stagedMessage.hasPrefix("中文abc🙂"))
        XCTAssertTrue(stagedMessage.hasSuffix("尾部完整"))
        let send = app.buttons["morde.task.send"]
        XCTAssertTrue(send.waitForExistence(timeout: 10))
        XCTAssertTrue(send.isEnabled, "A long text message should remain sendable without attachments or a computer.")
        send.tap()

        let interrupted = try await waitForMessageUploadResumeStatus(server: url, timeout: 60) {
            $0.failureInjected
        }
        XCTAssertFalse(interrupted.resumeInitializationSeen,
                       "The first attempt should stop after chunk zero is durable and chunk one fails.")
        XCTAssertFalse(interrupted.resumeAllowed,
                       "The loopback fixture should block retries until the app process stops.")
        app.terminate()
        try await allowMessageUploadResume(server: url)

        app.launchArguments = ["-morde.server", server]
        app.launch()
        let relaunchedAccountField = app.textFields["morde.auth.username"]
        if relaunchedAccountField.waitForExistence(timeout: 5) {
            enterLocalPortalCredentials(app: app, account: account, password: password)
        }
        let restoredChatTab = app.buttons["morde.shell.tab.chat"]
        XCTAssertTrue(restoredChatTab.waitForExistence(timeout: 30))
        restoredChatTab.tap()

        let recovered = try await waitForMessageUploadResumeStatus(server: url, timeout: 90) {
            $0.missingChunksRecovered && $0.committed
        }
        XCTAssertFalse(recovered.clientMessageID.isEmpty)
        XCTAssertTrue(recovered.resumeInitializationSeen)
        XCTAssertFalse(recovered.firstChunkRepeatedAfterRestart,
                       "The app should resume from the server's durable sequence and skip chunk zero.")
        let sent = app.descendants(matching: .any)
            .matching(identifier: "morde.chat.message.status.\(recovered.clientMessageID)").firstMatch
        XCTAssertTrue(sent.waitForExistence(timeout: 60), "The recovered long message should return to the sent state.")
        let restoredMessage = app.descendants(matching: .any)
            .matching(identifier: "morde.chat.message.content.\(recovered.clientMessageID)").firstMatch
        XCTAssertTrue(restoredMessage.waitForExistence(timeout: 30),
                      "The restored user message should retain its accessible content node.")
        XCTAssertTrue(restoredMessage.label.contains("尾部完整"),
                      "The restored user message should retain its final text marker.")
        app.terminate()
    }

    func testLocalPortalAttachmentUploadResumesAfterAppRestart() async throws {
        guard let account = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_USER"],
              let password = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_PASSWORD"],
              let server = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_SERVER"],
              let url = URL(string: server), url.scheme == "http",
              ["localhost", "127.0.0.1"].contains(url.host ?? ""),
              let port = url.port, (1...65535).contains(port) else {
            throw XCTSkip("Set local Portal credentials and a loopback HTTP URL with an explicit port in the test environment.")
        }

        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", server, "-morde.testChatAttachmentHTTPRecovery"]
        app.launch()
        let accountField = app.textFields["morde.auth.username"]
        if accountField.waitForExistence(timeout: 5) {
            enterLocalPortalCredentials(app: app, account: account, password: password)
            let loginFormDismissed = XCTNSPredicateExpectation(
                predicate: NSPredicate(format: "exists == false"), object: accountField)
            await fulfillment(of: [loginFormDismissed], timeout: 30)
        }

        let chatTab = app.buttons["morde.shell.tab.chat"]
        XCTAssertTrue(chatTab.waitForExistence(timeout: 30))
        chatTab.tap()
        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].waitForExistence(timeout: 60),
                      "The test build should stage a synthetic multi-chunk image into the real chat attachment flow.")

        let message = "跨重启恢复本地合成附件"
        let composer = app.descendants(matching: .any).matching(identifier: "morde.chat.composer").firstMatch
        XCTAssertTrue(composer.waitForExistence(timeout: 15))
        composer.tap()
        composer.typeText(message)
        let send = app.buttons["morde.task.send"]
        XCTAssertTrue(send.waitForExistence(timeout: 10))
        XCTAssertTrue(send.isEnabled)
        send.tap()
        XCTAssertTrue(app.staticTexts[message].waitForExistence(timeout: 30),
                      "The message should be accepted by the local server before its attachment finishes uploading.")

        let interrupted = try await waitForAttachmentResumeStatus(server: url, timeout: 60) { $0.failureInjected }
        XCTAssertFalse(interrupted.resumeInitializationSeen,
                       "The first upload attempt should stop only after its first chunk is durable.")
        XCTAssertFalse(interrupted.resumeAllowed,
                       "The fixture must keep retrying requests blocked until the app process has stopped.")
        let pending = app.staticTexts.matching(NSPredicate(
            format: "label == %@ OR label == %@", "已发送，正在上传附件", "已发送，附件需要处理"
        )).firstMatch
        XCTAssertTrue(pending.waitForExistence(timeout: 30),
                      "A failed attachment request should leave the accepted message recoverable.")
        app.terminate()
        try await allowAttachmentResume(server: url)

        app.launch()
        let relaunchedAccountField = app.textFields["morde.auth.username"]
        if relaunchedAccountField.waitForExistence(timeout: 5) {
            enterLocalPortalCredentials(app: app, account: account, password: password)
        }
        let restoredChatTab = app.buttons["morde.shell.tab.chat"]
        XCTAssertTrue(restoredChatTab.waitForExistence(timeout: 30))
        restoredChatTab.tap()
        XCTAssertTrue(app.staticTexts.matching(NSPredicate(format: "label == %@", message))
            .firstMatch.waitForExistence(timeout: 30))

        let sent = app.staticTexts.matching(NSPredicate(format: "label == %@", "已发送")).firstMatch
        XCTAssertTrue(sent.waitForExistence(timeout: 90),
                      "After restart, iOS should skip the durable chunk and finish the missing chunk.")
        let recovered = try await waitForAttachmentResumeStatus(server: url, timeout: 30) {
            $0.missingChunkRecovered && $0.committed
        }
        XCTAssertTrue(recovered.resumeInitializationSeen)
        XCTAssertFalse(recovered.firstChunkRepeatedAfterRestart,
                       "The app should trust the server's received sequence and skip chunk zero after restart.")
        XCTAssertTrue(app.staticTexts["fixture-image.jpg"].exists)
        app.terminate()
    }

    func testLocalPortalProjectAttachmentUploadResumesAfterAppRestart() async throws {
        try await assertProjectAttachmentUploadResumesAfterAppRestart(usesPDF: false)
    }

    func testLocalPortalProjectPDFAttachmentUploadResumesAfterAppRestart() async throws {
        try await assertProjectAttachmentUploadResumesAfterAppRestart(usesPDF: true)
    }

    private func assertProjectAttachmentUploadResumesAfterAppRestart(usesPDF: Bool) async throws {
        guard let account = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_USER"],
              let password = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_PASSWORD"],
              let server = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_SERVER"],
              let url = URL(string: server), url.scheme == "http",
              ["localhost", "127.0.0.1"].contains(url.host ?? ""),
              let port = url.port, (1...65535).contains(port) else {
            throw XCTSkip("Set local Portal credentials and a loopback HTTP URL with an explicit port in the test environment.")
        }

        let app = XCUIApplication()
        let fixtureArgument = usesPDF ? "-morde.testProjectPDFAttachmentHTTPRecovery" :
            "-morde.testProjectAttachmentHTTPRecovery"
        app.launchArguments = ["-morde.server", server, fixtureArgument]
        app.launch()
        let accountField = app.textFields["morde.auth.username"]
        if accountField.waitForExistence(timeout: 5) {
            enterLocalPortalCredentials(app: app, account: account, password: password)
            let loginFormDismissed = XCTNSPredicateExpectation(
                predicate: NSPredicate(format: "exists == false"), object: accountField)
            await fulfillment(of: [loginFormDismissed], timeout: 30)
        }

        try openLocalProjectConversation(app)
        let attachmentName = usesPDF ? "fixture.pdf" : "fixture-image.jpg"
        XCTAssertTrue(app.staticTexts[attachmentName].waitForExistence(timeout: 60),
                      "The iOS project composer should stage a synthetic multi-chunk attachment.")
        let message = usesPDF ? "跨重启恢复项目 PDF 附件" : "跨重启恢复项目附件"
        let composer = app.textFields["morde.projectConversation.composer"]
        XCTAssertTrue(composer.waitForExistence(timeout: 15))
        XCTAssertEqual(composer.value as? String, message)
        let automaticTaskOption = app.buttons["自动"]
        if automaticTaskOption.exists { automaticTaskOption.tap() }
        XCTAssertFalse(automaticTaskOption.exists, "Any task picker opened by scrolling should be dismissed before sending.")
        let send = app.buttons["morde.projectConversation.send"]
        XCTAssertTrue(send.waitForExistence(timeout: 10))
        XCTAssertTrue(send.isEnabled, "A project message with text and an attachment should be sendable.")
        XCTAssertLessThan(send.frame.maxY, app.buttons["morde.shell.tab.chat"].frame.minY - 4,
                          "The Send button should be fully above the floating tab bar.")
        send.tap()
        let clearedValue = composer.placeholderValue ?? "发送项目消息"
        let draftCleared = XCTNSPredicateExpectation(
            predicate: NSPredicate(format: "value == %@", clearedValue), object: composer)
        let draftClearResult = await XCTWaiter.fulfillment(of: [draftCleared], timeout: 10)
        if draftClearResult != .completed {
            let error = app.staticTexts["morde.projectConversation.error"]
            XCTFail("The project message was not accepted: \(error.exists ? error.label : "no client error was surfaced").")
            return
        }

        let interrupted = try await waitForProjectAttachmentResumeStatus(server: url, timeout: 60) {
            $0.failureInjected
        }
        XCTAssertFalse(interrupted.resumeInitializationSeen,
                       "The first attempt should stop after the first chunk is durable.")
        XCTAssertFalse(interrupted.resumeAllowed,
                       "The loopback fixture should block retries until the app process stops.")
        app.terminate()
        try await allowProjectAttachmentResume(server: url)

        app.launchArguments = ["-morde.server", server]
        app.launch()
        let relaunchedAccountField = app.textFields["morde.auth.username"]
        if relaunchedAccountField.waitForExistence(timeout: 5) {
            enterLocalPortalCredentials(app: app, account: account, password: password)
        }
        try openLocalProjectConversation(app)
        XCTAssertTrue(app.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", message))
            .firstMatch.waitForExistence(timeout: 30), "The accepted project message should return after restart.")
        let recovered = try await waitForProjectAttachmentResumeStatus(server: url, timeout: 120) {
            $0.missingChunkRecovered && $0.committed && $0.derivativesCompleted
        }
        XCTAssertTrue(recovered.resumeInitializationSeen)
        XCTAssertFalse(recovered.firstChunkRepeatedAfterRestart,
                       "The resumed upload should skip the chunk already stored by the server.")
        app.terminate()
    }

    func testLocalPortalChatMergesTwoTaskRepliesWithTheirSources() async throws {
        guard let account = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_USER"],
              let password = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_PASSWORD"],
              let server = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_SERVER"],
              let url = URL(string: server), url.scheme == "http",
              ["localhost", "127.0.0.1"].contains(url.host ?? ""),
              let port = url.port, (1...65535).contains(port),
              let token = ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_ACTIVITY_TOKEN"] else {
            throw XCTSkip("Set local Portal credentials, a loopback HTTP URL, and the isolated fixture token.")
        }
        guard !token.isEmpty else { throw XCTSkip("The local activity token is empty.") }

        let firstObjective = "跨端活动验收：合并 A"
        let secondObjective = "跨端活动验收：合并 B"
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", server]
        app.launch()

        let accountField = app.textFields["morde.auth.username"]
        if accountField.waitForExistence(timeout: 5) {
            // nanoMuse: wait for keyboard focus and successful login before navigating away from the form.
            enterLocalPortalCredentials(app: app, account: account, password: password)
            let loginFormDismissed = XCTNSPredicateExpectation(
                predicate: NSPredicate(format: "exists == false"),
                object: accountField
            )
            await fulfillment(of: [loginFormDismissed], timeout: 30)
            XCTAssertFalse(accountField.exists, "The iOS client must finish authenticating before opening chat.")
        }

        let chatTab = app.buttons["morde.shell.tab.chat"]
        XCTAssertTrue(chatTab.waitForExistence(timeout: 30))
        chatTab.tap()
        let composer = app.descendants(matching: .any).matching(identifier: "morde.chat.composer").firstMatch
        let send = app.buttons["morde.task.send"]
        XCTAssertTrue(composer.waitForExistence(timeout: 15))

        // nanoMuse: receive the first merge item from the Portal during mixed-client acceptance.
        if ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_MIXED_MERGE"] == "1" {
            XCTAssertTrue(app.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", firstObjective))
                .firstMatch.waitForExistence(timeout: 40),
                "The iOS conversation should receive the first task sent from the Portal browser.")
        } else {
            composer.tap()
            composer.typeText(firstObjective)
            XCTAssertTrue(send.waitForExistence(timeout: 10))
            XCTAssertTrue(send.isEnabled)
            send.tap()
            XCTAssertTrue(app.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", firstObjective))
                .firstMatch.waitForExistence(timeout: 20))
        }

        composer.tap()
        composer.typeText(secondObjective)
        XCTAssertTrue(send.isEnabled)
        send.tap()
        XCTAssertTrue(app.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", secondObjective))
            .firstMatch.waitForExistence(timeout: 20))

        _ = try await waitForFixtureRun("merge-a", stage: "claimed", server: url, token: token)
        _ = try await waitForFixtureRun("merge-b", stage: "claimed", server: url, token: token)
        try await advanceFixture("merge-a", phase: "start", server: url, token: token)
        try await advanceFixture("merge-b", phase: "start", server: url, token: token)
        _ = try await waitForFixtureRun("merge-a", stage: "started", server: url, token: token)
        _ = try await waitForFixtureRun("merge-b", stage: "started", server: url, token: token)
        try await advanceFixture("merge-a", phase: "progress", server: url, token: token)
        try await advanceFixture("merge-b", phase: "progress", server: url, token: token)
        _ = try await waitForFixtureRun("merge-a", stage: "progress", server: url, token: token)
        _ = try await waitForFixtureRun("merge-b", stage: "progress", server: url, token: token)

        // Complete the pair back-to-back so the production aggregation worker
        // receives both results inside its normal two-second collection window.
        try await advanceFixture("merge-a", phase: "complete", server: url, token: token)
        try await advanceFixture("merge-b", phase: "complete", server: url, token: token)
        let first = try await waitForFixtureRun("merge-a", stage: "completed", server: url, token: token)
        let second = try await waitForFixtureRun("merge-b", stage: "completed", server: url, token: token)
        try await verifyFixture("merge", server: url, token: token)
        guard let firstTaskID = first.taskID, !firstTaskID.isEmpty,
              let secondTaskID = second.taskID, !secondTaskID.isEmpty,
              let firstResultSequence = first.resultEventSequence, firstResultSequence > 0,
              let secondResultSequence = second.resultEventSequence, secondResultSequence > 0 else {
            throw NSError(domain: "MordeLocalFixture", code: 6,
                          userInfo: [NSLocalizedDescriptionKey: "Completed fixture runs must include task and result event identities."])
        }

        let firstSourceID = "morde.chat.message.source.\(firstTaskID).r0.e\(firstResultSequence)"
        let secondSourceID = "morde.chat.message.source.\(secondTaskID).r0.e\(secondResultSequence)"
        let firstSource = app.descendants(matching: .any).matching(identifier: firstSourceID).firstMatch
        let secondSource = app.descendants(matching: .any).matching(identifier: secondSourceID).firstMatch
        XCTAssertTrue(firstSource.waitForExistence(timeout: 30))
        XCTAssertTrue(secondSource.waitForExistence(timeout: 10))
        XCTAssertTrue(firstSource.label.contains("关联消息：\(firstObjective)"))
        XCTAssertTrue(firstSource.label.contains("本地验收结果：\(firstObjective)"))
        XCTAssertTrue(secondSource.label.contains("关联消息：\(secondObjective)"))
        XCTAssertTrue(secondSource.label.contains("本地验收结果：\(secondObjective)"))

        let messageContainers = app.descendants(matching: .any)
            .matching(NSPredicate(format: "identifier BEGINSWITH %@", "morde.chat.message.m"))
        let firstContainer = messageContainers.containing(.any, identifier: firstSourceID).firstMatch
        let secondContainer = messageContainers.containing(.any, identifier: secondSourceID).firstMatch
        XCTAssertTrue(firstContainer.exists)
        XCTAssertTrue(secondContainer.exists)
        XCTAssertEqual(firstContainer.identifier, secondContainer.identifier,
                       "Both result segments should be rendered within one persistent assistant message.")

        // nanoMuse: after the cross-client merge, relaunch the real iOS app and
        // verify the server-persisted message and source segments are not duplicated.
        if ProcessInfo.processInfo.environment["MORDE_IOS_LOCAL_PORTAL_MIXED_MERGE"] == "1" {
            app.terminate()

            app.launch()
            let accountField = app.textFields["morde.auth.username"]
            if accountField.waitForExistence(timeout: 5) {
                enterLocalPortalCredentials(app: app, account: account, password: password)
            }

            let restoredChatTab = app.buttons["morde.shell.tab.chat"]
            XCTAssertTrue(restoredChatTab.waitForExistence(timeout: 30),
                          "The iOS app should restore its authenticated shell after relaunch.")
            restoredChatTab.tap()
            let restoredTranscript = app.descendants(matching: .any)
                .matching(identifier: "morde.chat.transcript").firstMatch
            XCTAssertTrue(restoredTranscript.waitForExistence(timeout: 15))

            let restoredFirstObjective = app.staticTexts.matching(
                NSPredicate(format: "label == %@", firstObjective)
            )
            let restoredSecondObjective = app.staticTexts.matching(
                NSPredicate(format: "label == %@", secondObjective)
            )
            XCTAssertEqual(restoredFirstObjective.count, 1,
                           "The first user message should occur once after app restart.")
            XCTAssertEqual(restoredSecondObjective.count, 1,
                           "The second user message should occur once after app restart.")

            let restoredFirstSource = app.descendants(matching: .any)
                .matching(identifier: firstSourceID).firstMatch
            let restoredSecondSource = app.descendants(matching: .any)
                .matching(identifier: secondSourceID).firstMatch
            XCTAssertTrue(restoredFirstSource.waitForExistence(timeout: 30))
            XCTAssertTrue(restoredSecondSource.waitForExistence(timeout: 10))
            XCTAssertEqual(app.descendants(matching: .any).matching(identifier: firstSourceID).count, 1,
                           "The first result source should occur once after app restart.")
            XCTAssertEqual(app.descendants(matching: .any).matching(identifier: secondSourceID).count, 1,
                           "The second result source should occur once after app restart.")
            XCTAssertTrue(restoredFirstSource.label.contains("关联消息：\(firstObjective)"))
            XCTAssertTrue(restoredFirstSource.label.contains("本地验收结果：\(firstObjective)"))
            XCTAssertTrue(restoredSecondSource.label.contains("关联消息：\(secondObjective)"))
            XCTAssertTrue(restoredSecondSource.label.contains("本地验收结果：\(secondObjective)"))

            let restoredMessageContainers = app.descendants(matching: .any)
                .matching(NSPredicate(format: "identifier BEGINSWITH %@", "morde.chat.message.m"))
            let restoredFirstContainer = restoredMessageContainers
                .containing(.any, identifier: firstSourceID).firstMatch
            let restoredSecondContainer = restoredMessageContainers
                .containing(.any, identifier: secondSourceID).firstMatch
            XCTAssertTrue(restoredFirstContainer.exists)
            XCTAssertTrue(restoredSecondContainer.exists)
            XCTAssertEqual(restoredFirstContainer.identifier, firstContainer.identifier,
                           "The persisted assistant message ID should remain stable after restart.")
            XCTAssertEqual(restoredSecondContainer.identifier, secondContainer.identifier,
                           "The persisted assistant message ID should remain stable after restart.")
            XCTAssertEqual(restoredFirstContainer.identifier, restoredSecondContainer.identifier,
                           "Both result segments should still belong to the same assistant message after restart.")

            app.terminate()
            return
        }

        let separateFirstObjective = "跨端活动验收：分开 A"
        let separateSecondObjective = "跨端活动验收：分开 B"
        composer.tap()
        composer.typeText(separateFirstObjective)
        XCTAssertTrue(send.isEnabled)
        send.tap()
        XCTAssertTrue(app.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", separateFirstObjective))
            .firstMatch.waitForExistence(timeout: 20))
        composer.tap()
        composer.typeText(separateSecondObjective)
        XCTAssertTrue(send.isEnabled)
        send.tap()
        XCTAssertTrue(app.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", separateSecondObjective))
            .firstMatch.waitForExistence(timeout: 20))

        for name in ["separate-a", "separate-b"] {
            _ = try await waitForFixtureRun(name, stage: "claimed", server: url, token: token)
        }
        for name in ["separate-a", "separate-b"] {
            try await advanceFixture(name, phase: "start", server: url, token: token)
        }
        for name in ["separate-a", "separate-b"] {
            _ = try await waitForFixtureRun(name, stage: "started", server: url, token: token)
        }
        for name in ["separate-a", "separate-b"] {
            try await advanceFixture(name, phase: "progress", server: url, token: token)
        }
        for name in ["separate-a", "separate-b"] {
            _ = try await waitForFixtureRun(name, stage: "progress", server: url, token: token)
        }

        // Keep B running while A's collection window expires. A must be visible
        // as its own message before B is released.
        try await advanceFixture("separate-a", phase: "complete", server: url, token: token)
        let separateFirst = try await waitForFixtureRun("separate-a", stage: "completed", server: url, token: token)
        guard let separateFirstTaskID = separateFirst.taskID, !separateFirstTaskID.isEmpty,
              let separateFirstSequence = separateFirst.resultEventSequence, separateFirstSequence > 0 else {
            throw NSError(domain: "MordeLocalFixture", code: 7,
                          userInfo: [NSLocalizedDescriptionKey: "The first timed-out result must include task and event identities."])
        }
        let separateFirstSourceID = "morde.chat.message.source.\(separateFirstTaskID).r0.e\(separateFirstSequence)"
        let separateFirstSource = app.descendants(matching: .any).matching(identifier: separateFirstSourceID).firstMatch
        XCTAssertTrue(separateFirstSource.waitForExistence(timeout: 30),
                      "A should appear separately after the collection window expires while B is still running.")

        try await advanceFixture("separate-b", phase: "complete", server: url, token: token)
        let separateSecond = try await waitForFixtureRun("separate-b", stage: "completed", server: url, token: token)
        guard let separateSecondTaskID = separateSecond.taskID, !separateSecondTaskID.isEmpty,
              let separateSecondSequence = separateSecond.resultEventSequence, separateSecondSequence > 0 else {
            throw NSError(domain: "MordeLocalFixture", code: 8,
                          userInfo: [NSLocalizedDescriptionKey: "The second separate result must include task and event identities."])
        }
        try await verifyFixture("separate", server: url, token: token)
        let separateSecondSourceID = "morde.chat.message.source.\(separateSecondTaskID).r0.e\(separateSecondSequence)"
        let separateSecondSource = app.descendants(matching: .any).matching(identifier: separateSecondSourceID).firstMatch
        XCTAssertTrue(separateSecondSource.waitForExistence(timeout: 30))
        XCTAssertTrue(separateFirstSource.label.contains("关联消息：\(separateFirstObjective)"))
        XCTAssertTrue(separateFirstSource.label.contains("本地验收结果：\(separateFirstObjective)"))
        XCTAssertTrue(separateSecondSource.label.contains("关联消息：\(separateSecondObjective)"))
        XCTAssertTrue(separateSecondSource.label.contains("本地验收结果：\(separateSecondObjective)"))
        let separateFirstContainer = messageContainers.containing(.any, identifier: separateFirstSourceID).firstMatch
        let separateSecondContainer = messageContainers.containing(.any, identifier: separateSecondSourceID).firstMatch
        XCTAssertTrue(separateFirstContainer.exists)
        XCTAssertTrue(separateSecondContainer.exists)
        XCTAssertNotEqual(separateFirstContainer.identifier, separateSecondContainer.identifier,
                          "A result outside the collection window should remain in its own assistant message.")
        app.terminate()
    }

    private struct LocalActivityStatus: Decodable {
        var runs: [LocalActivityRun]
    }

    private struct AttachmentResumeStatus: Decodable {
        var failureInjected: Bool
        var resumeAllowed: Bool
        var uploadID: String
        var resumeInitializationSeen: Bool
        var firstChunkRepeatedAfterRestart: Bool
        var missingChunkRecovered: Bool
        var committed: Bool

        enum CodingKeys: String, CodingKey {
            case failureInjected = "failure_injected"
            case resumeAllowed = "resume_allowed"
            case uploadID = "upload_id"
            case resumeInitializationSeen = "resume_initialization_seen"
            case firstChunkRepeatedAfterRestart = "first_chunk_repeated_after_restart"
            case missingChunkRecovered = "missing_chunk_recovered"
            case committed
        }
    }

    private struct MessageUploadResumeStatus: Decodable {
        var failureInjected: Bool
        var resumeAllowed: Bool
        var clientMessageID: String
        var resumeInitializationSeen: Bool
        var firstChunkRepeatedAfterRestart: Bool
        var missingChunksRecovered: Bool
        var committed: Bool

        enum CodingKeys: String, CodingKey {
            case failureInjected = "failure_injected"
            case resumeAllowed = "resume_allowed"
            case clientMessageID = "client_message_id"
            case resumeInitializationSeen = "resume_initialization_seen"
            case firstChunkRepeatedAfterRestart = "first_chunk_repeated_after_restart"
            case missingChunksRecovered = "missing_chunks_recovered"
            case committed
        }
    }

    private struct ProjectAttachmentResumeStatus: Decodable {
        var failureInjected: Bool
        var resumeAllowed: Bool
        var messageID: String
        var clientAttachmentID: String
        var resumeInitializationSeen: Bool
        var firstChunkRepeatedAfterRestart: Bool
        var missingChunkRecovered: Bool
        var committed: Bool
        var derivativesCompleted: Bool

        enum CodingKeys: String, CodingKey {
            case failureInjected = "failure_injected"
            case resumeAllowed = "resume_allowed"
            case messageID = "message_id"
            case clientAttachmentID = "client_attachment_id"
            case resumeInitializationSeen = "resume_initialization_seen"
            case firstChunkRepeatedAfterRestart = "first_chunk_repeated_after_restart"
            case missingChunkRecovered = "missing_chunk_recovered"
            case committed
            case derivativesCompleted = "derivatives_completed"
        }
    }

    private func waitForProjectAttachmentResumeStatus(
        server: URL,
        timeout: TimeInterval,
        until predicate: (ProjectAttachmentResumeStatus) -> Bool
    ) async throws -> ProjectAttachmentResumeStatus {
        let statusURL = server.appendingPathComponent("__morde_fixture/project-attachment-status")
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            var request = URLRequest(url: statusURL)
            request.timeoutInterval = 3
            let (data, response) = try await URLSession.shared.data(for: request)
            guard let response = response as? HTTPURLResponse, response.statusCode == 200 else {
                throw NSError(domain: "MordeLocalFixture", code: 10,
                              userInfo: [NSLocalizedDescriptionKey: "Local project attachment status returned an unexpected response."])
            }
            let status = try JSONDecoder().decode(ProjectAttachmentResumeStatus.self, from: data)
            if predicate(status) { return status }
            try await Task.sleep(nanoseconds: 200_000_000)
        }
        throw NSError(domain: "MordeLocalFixture", code: 11,
                      userInfo: [NSLocalizedDescriptionKey: "Timed out waiting for local project attachment recovery."])
    }

    private func allowProjectAttachmentResume(server: URL) async throws {
        var request = URLRequest(url: server.appendingPathComponent("__morde_fixture/allow-project-attachment-resume"))
        request.httpMethod = "POST"
        request.timeoutInterval = 3
        let (_, response) = try await URLSession.shared.data(for: request)
        guard let response = response as? HTTPURLResponse, response.statusCode == 204 else {
            throw NSError(domain: "MordeLocalFixture", code: 12,
                          userInfo: [NSLocalizedDescriptionKey: "The local project attachment fixture did not release the restart gate."])
        }
    }

    private func openLocalProjectConversation(_ app: XCUIApplication) throws {
        let menu = app.buttons["morde.shell.drawer"]
        XCTAssertTrue(menu.waitForExistence(timeout: 20))
        menu.tap()
        let drawer = app.scrollViews["morde.shell.drawer.scroll"]
        XCTAssertTrue(drawer.waitForExistence(timeout: 5), "The current NanoMuse drawer should expose its scrollable destinations.")
        let projects = app.buttons["morde.shell.drawer.projects"]
        for _ in 0..<8 where !projects.isHittable {
            drawer.swipeUp()
        }
        XCTAssertTrue(projects.waitForExistence(timeout: 5), "The Projects destination should load in the navigation drawer.")
        XCTAssertTrue(projects.isHittable, "The Projects destination should be available in the navigation drawer.")
        projects.tap()

        let list = app.descendants(matching: .any).matching(identifier: "morde.projects.list").firstMatch
        XCTAssertTrue(list.waitForExistence(timeout: 20))
        let project = app.buttons.matching(NSPredicate(format: "identifier BEGINSWITH %@", "morde.project.row."))
            .firstMatch
        XCTAssertTrue(project.waitForExistence(timeout: 20))
        for _ in 0..<10 where !project.isHittable {
            list.swipeUp()
        }
        XCTAssertTrue(project.isHittable, "The local project should be reachable in the project list.")
        project.tap()

        let conversation = app.buttons.matching(
            NSPredicate(format: "identifier BEGINSWITH %@", "morde.projectConversation.row.")
        ).firstMatch
        XCTAssertTrue(conversation.waitForExistence(timeout: 30))
        let bottomTab = app.buttons["morde.shell.tab.chat"]
        for _ in 0..<12 {
            guard conversation.exists else { break }
            let row = conversation.frame
            if conversation.isHittable && row.minY >= list.frame.minY && row.maxY < bottomTab.frame.minY - 4 {
                break
            }
            let moveContentUp = row.maxY >= bottomTab.frame.minY - 4
            let fromY: CGFloat = moveContentUp ? 0.82 : 0.28
            let toY: CGFloat = moveContentUp ? 0.70 : 0.40
            list.coordinate(withNormalizedOffset: CGVector(dx: 0.02, dy: fromY))
                .press(forDuration: 0.05,
                       thenDragTo: list.coordinate(withNormalizedOffset: CGVector(dx: 0.02, dy: toY)))
        }
        guard conversation.exists, conversation.isHittable,
              conversation.frame.minY >= list.frame.minY,
              conversation.frame.maxY < bottomTab.frame.minY - 4 else {
            XCTFail("The shared conversation row is not tappable: row=\(conversation.frame), list=\(list.frame), tab=\(bottomTab.frame).")
            return
        }
        conversation.tap()
        let composer = app.textFields["morde.projectConversation.composer"]
        for _ in 0..<24 {
            let frame = composer.frame
            if composer.exists && composer.isHittable && frame.minY >= list.frame.minY &&
                frame.maxY < bottomTab.frame.minY - 4 { break }
            let moveContentUp = !composer.exists || frame.maxY >= bottomTab.frame.minY - 4
            let fromY: CGFloat = moveContentUp ? 0.82 : 0.28
            let toY: CGFloat = moveContentUp ? 0.70 : 0.40
            list.coordinate(withNormalizedOffset: CGVector(dx: 0.02, dy: fromY))
                .press(forDuration: 0.05,
                       thenDragTo: list.coordinate(withNormalizedOffset: CGVector(dx: 0.02, dy: toY)))
        }
        XCTAssertTrue(composer.waitForExistence(timeout: 10), "The project composer should load after selecting the shared conversation.")
        XCTAssertTrue(composer.isHittable, "The project composer should be reachable by scrolling the project list.")
    }

    private func waitForAttachmentResumeStatus(
        server: URL,
        timeout: TimeInterval,
        until predicate: (AttachmentResumeStatus) -> Bool
    ) async throws -> AttachmentResumeStatus {
        let statusURL = server.appendingPathComponent("__morde_fixture/attachment-status")
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            var request = URLRequest(url: statusURL)
            request.timeoutInterval = 3
            let (data, response) = try await URLSession.shared.data(for: request)
            guard let response = response as? HTTPURLResponse, response.statusCode == 200 else {
                throw NSError(domain: "MordeLocalFixture", code: 7,
                              userInfo: [NSLocalizedDescriptionKey: "Local attachment status returned an unexpected response."])
            }
            let status = try JSONDecoder().decode(AttachmentResumeStatus.self, from: data)
            if predicate(status) { return status }
            try await Task.sleep(nanoseconds: 200_000_000)
        }
        throw NSError(domain: "MordeLocalFixture", code: 8,
                      userInfo: [NSLocalizedDescriptionKey: "Timed out waiting for local attachment recovery."])
    }

    private func waitForMessageUploadResumeStatus(
        server: URL,
        timeout: TimeInterval,
        until predicate: (MessageUploadResumeStatus) -> Bool
    ) async throws -> MessageUploadResumeStatus {
        let statusURL = server.appendingPathComponent("__morde_fixture/message-upload-status")
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            var request = URLRequest(url: statusURL)
            request.timeoutInterval = 3
            let (data, response) = try await URLSession.shared.data(for: request)
            guard let response = response as? HTTPURLResponse, response.statusCode == 200 else {
                throw NSError(domain: "MordeLocalFixture", code: 13,
                              userInfo: [NSLocalizedDescriptionKey: "Local message upload status returned an unexpected response."])
            }
            let status = try JSONDecoder().decode(MessageUploadResumeStatus.self, from: data)
            if predicate(status) { return status }
            try await Task.sleep(nanoseconds: 200_000_000)
        }
        throw NSError(domain: "MordeLocalFixture", code: 14,
                      userInfo: [NSLocalizedDescriptionKey: "Timed out waiting for local message upload recovery."])
    }

    private func allowMessageUploadResume(server: URL) async throws {
        var request = URLRequest(url: server.appendingPathComponent("__morde_fixture/allow-message-upload-resume"))
        request.httpMethod = "POST"
        request.timeoutInterval = 3
        let (_, response) = try await URLSession.shared.data(for: request)
        guard let response = response as? HTTPURLResponse, response.statusCode == 204 else {
            throw NSError(domain: "MordeLocalFixture", code: 15,
                          userInfo: [NSLocalizedDescriptionKey: "The local message upload fixture did not release the restart gate."])
        }
    }

    private func allowAttachmentResume(server: URL) async throws {
        var request = URLRequest(url: server.appendingPathComponent("__morde_fixture/allow-attachment-resume"))
        request.httpMethod = "POST"
        request.timeoutInterval = 3
        let (_, response) = try await URLSession.shared.data(for: request)
        guard let response = response as? HTTPURLResponse, response.statusCode == 204 else {
            throw NSError(domain: "MordeLocalFixture", code: 9,
                          userInfo: [NSLocalizedDescriptionKey: "The local attachment fixture did not release the restart gate."])
        }
    }

    private struct LocalActivityRun: Decodable {
        var name: String
        var stage: String
        var failure: String?
        var taskID: String?
        var resultEventSequence: Int64?

        enum CodingKeys: String, CodingKey {
            case name, stage, failure
            case taskID = "task_id"
            case resultEventSequence = "result_event_sequence"
        }
    }

    private func waitForFixtureRun(_ name: String, stage: String, server: URL, token: String) async throws -> LocalActivityRun {
        let deadline = Date().addingTimeInterval(30)
        while Date() < deadline {
            let status = try await readFixtureStatus(server: server, token: token)
            guard let run = status.runs.first(where: { $0.name == name }) else {
                throw NSError(domain: "MordeLocalFixture", code: 1,
                              userInfo: [NSLocalizedDescriptionKey: "Missing local fixture run \(name)."])
            }
            if let failure = run.failure, !failure.isEmpty {
                throw NSError(domain: "MordeLocalFixture", code: 2,
                              userInfo: [NSLocalizedDescriptionKey: "Local fixture run \(name) failed: \(failure)"])
            }
            if run.stage == stage { return run }
            try await Task.sleep(nanoseconds: 100_000_000)
        }
        throw NSError(domain: "MordeLocalFixture", code: 3,
                      userInfo: [NSLocalizedDescriptionKey: "Timed out waiting for \(name) to reach \(stage)."])
    }

    private func readFixtureStatus(server: URL, token: String) async throws -> LocalActivityStatus {
        var request = URLRequest(url: server.appendingPathComponent("__morde_fixture/status"))
        request.setValue(token, forHTTPHeaderField: "X-Morde-Fixture-Token")
        let (data, response) = try await URLSession.shared.data(for: request)
        guard let response = response as? HTTPURLResponse, response.statusCode == 200 else {
            throw NSError(domain: "MordeLocalFixture", code: 4,
                          userInfo: [NSLocalizedDescriptionKey: "Local fixture status returned an unexpected response."])
        }
        return try JSONDecoder().decode(LocalActivityStatus.self, from: data)
    }

    private func advanceFixture(_ name: String, phase: String, server: URL, token: String) async throws {
        try await sendFixtureControl("advance/\(name)/\(phase)", server: server, token: token, method: "POST")
    }

    private func verifyFixture(_ scenario: String, server: URL, token: String) async throws {
        try await sendFixtureControl("verify/\(scenario)", server: server, token: token, method: "GET")
    }

    private func sendFixtureControl(_ path: String, server: URL, token: String, method: String) async throws {
        var request = URLRequest(url: server.appendingPathComponent("__morde_fixture/\(path)"))
        request.httpMethod = method
        request.setValue(token, forHTTPHeaderField: "X-Morde-Fixture-Token")
        let (_, response) = try await URLSession.shared.data(for: request)
        guard let response = response as? HTTPURLResponse,
              method == "POST" ? response.statusCode == 204 : response.statusCode == 200 else {
            throw NSError(domain: "MordeLocalFixture", code: 5,
                          userInfo: [NSLocalizedDescriptionKey: "Local fixture control \(path) failed."])
        }
    }

    private func enterLocalPortalCredentials(app: XCUIApplication, account: String, password: String) {
        // nanoMuse: Clear an asynchronous startup error before focusing secure login fields.
        let startupError = app.alerts.firstMatch
        if startupError.waitForExistence(timeout: 2) {
            startupError.buttons["确定"].tap()
        }
        let accountField = app.textFields["morde.auth.username"]
        XCTAssertTrue(accountField.waitForExistence(timeout: 5))
        accountField.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.5)).tap()
        let keyboard = app.keyboards.firstMatch
        XCTAssertTrue(keyboard.waitForExistence(timeout: 5), "The username field must receive keyboard focus.")
        accountField.typeText(account)

        let passwordField = app.secureTextFields["morde.auth.password"]
        XCTAssertTrue(passwordField.waitForExistence(timeout: 5))
        passwordField.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.5)).tap()
        XCTAssertTrue(keyboard.waitForExistence(timeout: 5), "The password field must receive keyboard focus.")
        passwordField.typeText(password)
        let submit = app.buttons["morde.auth.submit"]
        XCTAssertTrue(submit.isEnabled, "The local test password must reach the secure field before login.")
        submit.tap()
    }

    func testPersonalDataSyncAndLibrary() throws {
        guard let account = ProcessInfo.processInfo.environment["MORDE_IOS04_TEST_USER"],
              let password = ProcessInfo.processInfo.environment["MORDE_IOS04_TEST_PASSWORD"],
              let serverAddress = ProcessInfo.processInfo.environment["MORDE_IOS04_TEST_SERVER"] else {
            throw XCTSkip("Set local IOS-04 test credentials in the test action environment.")
        }

        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", serverAddress]
        app.launch()
        let accountField = app.textFields["morde.auth.username"]
        XCTAssertTrue(accountField.waitForExistence(timeout: 10))
        accountField.tap()
        accountField.typeText(account)

        let passwordField = app.secureTextFields["morde.auth.password"]
        XCTAssertTrue(passwordField.waitForExistence(timeout: 5))
        passwordField.tap()
        passwordField.typeText(password)
        app.buttons["morde.auth.submit"].tap()
        let chatTab = app.buttons["morde.shell.tab.chat"]
        let tasksTab = app.buttons["morde.shell.tab.tasks"]
        let approvalsTab = app.buttons["morde.shell.tab.approvals"]
        let libraryTab = app.buttons["morde.shell.tab.resources"]
        XCTAssertTrue(chatTab.waitForExistence(timeout: 20))
        XCTAssertTrue(tasksTab.exists)
        XCTAssertTrue(approvalsTab.exists)
        XCTAssertTrue(libraryTab.exists)
        for room in ["feed", "ideas", "goals"] {
            XCTAssertFalse(app.buttons["morde.shell.tab.\(room)"].exists)
        }
        libraryTab.tap()
        let libraryAll = app.buttons["morde.library.source.all"]
        XCTAssertTrue(libraryAll.waitForExistence(timeout: 10))
        libraryAll.tap()
        let libraryMetadata = app.descendants(matching: .any)["morde.library.cloud.item.ios04acceptlibrary"]
        XCTAssertTrue(libraryMetadata.waitForExistence(timeout: 15))
        XCTAssertFalse(app.buttons["morde.library.cloud.item.ios04acceptlibrary"].exists)
        XCTAssertTrue(text(app, "Metadata only", "仅元数据").exists)
        chatTab.tap()
        button(app, "More", "更多").tap()
        button(app, "Settings", "设置").tap()
        button(app, "Morde account", "Morde 账户").tap()
        XCTAssertTrue(app.staticTexts["Personal data quota"].waitForExistence(timeout: 10))
        XCTAssertTrue(button(app, "Refresh quota", "刷新额度").waitForExistence(timeout: 15))

        let syncToggle = app.switches.matching(
            NSPredicate(format: "label CONTAINS[c] 'Sync Morde personal data' OR label CONTAINS[c] '同步 Morde 个人数据'")
        ).firstMatch
        XCTAssertTrue(syncToggle.waitForExistence(timeout: 10))
        if syncToggle.value as? String == "1" {
            syncToggle.coordinate(withNormalizedOffset: CGVector(dx: 0.92, dy: 0.5)).tap()
        }
        XCTAssertEqual(syncToggle.value as? String, "0")
        let pausedStatus = app.descendants(matching: .any).matching(
            NSPredicate(format: "label CONTAINS[c] 'Sync is paused' OR label CONTAINS[c] '暂停'")
        ).firstMatch
        XCTAssertTrue(pausedStatus.waitForExistence(timeout: 10))

        button(app, "Personal profile", "个人资料").tap()
        let displayName = app.textFields.matching(
            NSPredicate(format: "placeholderValue CONTAINS[c] 'Display name' OR placeholderValue CONTAINS[c] '显示名称'")
        ).firstMatch
        XCTAssertTrue(displayName.waitForExistence(timeout: 15))
        displayName.tap()
        if let currentName = displayName.value as? String, !currentName.isEmpty {
            displayName.typeText(String(repeating: XCUIKeyboardKey.delete.rawValue, count: currentName.count))
        }
        displayName.typeText("IOS04 Simulator")
        button(app, "Save", "保存").tap()
        app.navigationBars.buttons.element(boundBy: 0).tap()
        let pendingStatus = app.staticTexts.matching(
            NSPredicate(format: "label CONTAINS[c] 'Pending' OR label CONTAINS[c] '待处理' OR label CONTAINS[c] '待同步'")
        ).firstMatch
        XCTAssertTrue(pendingStatus.waitForExistence(timeout: 10))
        XCTAssertEqual(syncToggle.value as? String, "0")
        XCTAssertTrue(pausedStatus.waitForExistence(timeout: 10))
        if syncToggle.value as? String != "1" {
            syncToggle.coordinate(withNormalizedOffset: CGVector(dx: 0.92, dy: 0.5)).tap()
        }
        XCTAssertEqual(syncToggle.value as? String, "1")
        XCTAssertTrue(app.staticTexts["No pending personal-data changes."].waitForExistence(timeout: 20))
        button(app, "Personal profile", "个人资料").tap()
        XCTAssertTrue(app.textFields.matching(
            NSPredicate(format: "value == %@", "IOS04 Simulator")
        ).firstMatch.waitForExistence(timeout: 15))

        // The memory file is a separate personal-data object from the profile.
        app.navigationBars.buttons.element(boundBy: 0).tap()
        app.navigationBars.buttons.element(boundBy: 0).tap()
        let systemFiles = button(app, "System files", "系统文件")
        if !systemFiles.isHittable { app.scrollViews.firstMatch.swipeUp() }
        XCTAssertTrue(systemFiles.waitForExistence(timeout: 10))
        systemFiles.tap()
        XCTAssertFalse(app.staticTexts["feed-preferences.md"].exists)
        app.staticTexts["GLOBAL.md"].tap()
        button(app, "More", "更多").tap()
        button(app, "Edit", "编辑").tap()
        let memoryEditor = app.textViews.firstMatch
        XCTAssertTrue(memoryEditor.waitForExistence(timeout: 10))
        let memoryMarker = "IOS04 Simulator Memory \(UUID().uuidString)"
        memoryEditor.tap()
        memoryEditor.typeText("\n\(memoryMarker)")
        app.navigationBars.buttons.matching(
            NSPredicate(format: "label == 'Save' OR label == '保存'")
        ).firstMatch.tap()
        app.navigationBars.buttons.element(boundBy: 0).tap()
        app.navigationBars.buttons.element(boundBy: 0).tap()
        button(app, "Morde account", "Morde 账户").tap()
        XCTAssertTrue(app.staticTexts["No pending personal-data changes."].waitForExistence(timeout: 20))
        app.navigationBars.buttons.element(boundBy: 0).tap()
        let systemFilesAgain = button(app, "System files", "系统文件")
        if !systemFilesAgain.isHittable { app.scrollViews.firstMatch.swipeUp() }
        systemFilesAgain.tap()
        app.staticTexts["GLOBAL.md"].tap()
        let memoryCopy = app.staticTexts.matching(
            NSPredicate(format: "label CONTAINS %@", memoryMarker)
        )
        XCTAssertTrue(memoryCopy.firstMatch.waitForExistence(timeout: 20), "The saved memory marker should round-trip from Morde.")

    }

    private func button(_ app: XCUIApplication, _ primary: String, _ alternate: String) -> XCUIElement {
        app.buttons.matching(NSPredicate(format: "label == %@ OR label == %@", primary, alternate)).firstMatch
    }

    private func text(_ app: XCUIApplication, _ primary: String, _ alternate: String) -> XCUIElement {
        app.staticTexts.matching(NSPredicate(format: "label == %@ OR label == %@", primary, alternate)).firstMatch
    }
}
