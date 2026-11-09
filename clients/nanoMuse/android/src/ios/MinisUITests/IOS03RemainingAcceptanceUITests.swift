import XCTest

@MainActor
final class IOS03RemainingAcceptanceUITests: XCTestCase {
    private struct Config {
        let account: String
        let password: String
        let server: String
        let hostID: String
        let memberID: String
    }

    func testPortalMemberAPIKeyAndModelSave() throws {
        let config = try localConfig()
        let app = try signedInApp(config)
        openAccountSettings(app)

        tap(button(app, "Host members", "Host members"))
        let hostWeb = app.webViews.firstMatch
        XCTAssertTrue(hostWeb.waitForExistence(timeout: 20))
        XCTAssertTrue(hostWeb.staticTexts["云端电脑"].waitForExistence(timeout: 20))
        let manageMembers = hostWeb.buttons["管理共享成员"]
        if !manageMembers.isHittable { hostWeb.swipeUp() }
        XCTAssertTrue(manageMembers.waitForExistence(timeout: 10))
        manageMembers.tap()

        let memberField = hostWeb.textFields.matching(NSPredicate(
            format: "identifier == %@ OR label == %@", "host-member-user-\(config.hostID)", "对方账号 ID"
        )).firstMatch
        XCTAssertTrue(memberField.waitForExistence(timeout: 10))
        memberField.tap()
        memberField.typeText(config.memberID)
        let desktopView = hostWeb.checkBoxes.matching(NSPredicate(
            format: "label CONTAINS %@", "desktop:view"
        )).firstMatch
        if !desktopView.isHittable { hostWeb.swipeUp() }
        XCTAssertTrue(desktopView.waitForExistence(timeout: 10))
        desktopView.tap()
        let invite = hostWeb.buttons["邀请并授权"]
        if !invite.isHittable { hostWeb.swipeUp() }
        tap(invite)
        tap(hostWeb.buttons["确认授权"])
        XCTAssertTrue(hostWeb.staticTexts[config.memberID].waitForExistence(timeout: 15))
        let revokeMember = hostWeb.buttons["撤销权限"].firstMatch
        if !revokeMember.isHittable { hostWeb.swipeUp() }
        tap(revokeMember)
        let revokeConfirmButtons = hostWeb.buttons.matching(identifier: "撤销权限")
        XCTAssertEqual(revokeConfirmButtons.count, 2)
        tap(revokeConfirmButtons.element(boundBy: 1))
        XCTAssertTrue(hostWeb.staticTexts[config.memberID].waitForNonExistence(timeout: 15))

        back(app)
        tap(button(app, "API Keys", "API Keys"))
        let keyWeb = app.webViews.firstMatch
        XCTAssertTrue(keyWeb.waitForExistence(timeout: 20))
        XCTAssertTrue(keyWeb.staticTexts["API Key"].waitForExistence(timeout: 20))
        let keyName = "IOS03 Simulator Key \(UUID().uuidString.prefix(8))"
        let keyNameField = keyWeb.textFields["apikey-name"]
        XCTAssertTrue(keyNameField.waitForExistence(timeout: 10))
        keyNameField.tap()
        keyNameField.typeText(keyName)
        tap(keyWeb.buttons["创建 API Key"])
        XCTAssertTrue(keyWeb.staticTexts["API Key 已创建"].waitForExistence(timeout: 15))
        tap(keyWeb.buttons["关闭并清除"])
        let revokeKey = keyWeb.buttons["吊销 API Key \(keyName)"]
        XCTAssertTrue(revokeKey.waitForExistence(timeout: 15))
        revokeKey.tap()
        tap(keyWeb.buttons["吊销 Key"])
        XCTAssertTrue(keyWeb.staticTexts["已吊销"].waitForExistence(timeout: 15))

        back(app)
        tap(button(app, "Model connections", "Model connections"))
        let modelWeb = app.webViews.firstMatch
        XCTAssertTrue(modelWeb.waitForExistence(timeout: 20))
        XCTAssertTrue(modelWeb.staticTexts["模型连接"].waitForExistence(timeout: 20))
        let addModel = modelWeb.buttons.matching(NSPredicate(format: "label CONTAINS %@", "添加模型")).firstMatch
        tap(addModel)
        let modelName = "IOS03-Simulator-Model-Delete-20261102"
        let nameField = modelWeb.textFields["model-connection-name"]
        let modelField = modelWeb.textFields["model-connection-model"]
        let urlField = modelWeb.textFields["model-connection-url"]
        let keyField = modelWeb.secureTextFields["model-connection-key"]
        XCTAssertTrue(nameField.waitForExistence(timeout: 10))
        nameField.tap(); nameField.typeText(modelName)
        modelField.tap(); modelField.typeText("gpt-4o-mini")
        urlField.tap(); urlField.typeText("https://api.openai.com/v1")
        keyField.tap(); keyField.typeText("ios03-synthetic-invalid-key")
        tap(modelWeb.buttons["保存模型"])
        XCTAssertTrue(modelWeb.staticTexts[modelName].waitForExistence(timeout: 20))
    }

    func testSecondarySimulatorLoginCreatesInstallation() throws {
        let app = try signedInApp(localConfig())
        XCTAssertTrue(app.buttons["morde.shell.tab.chat"].waitForExistence(timeout: 20))
        XCTAssertTrue(app.buttons["morde.shell.tab.tasks"].exists)
        XCTAssertTrue(app.buttons["morde.shell.tab.approvals"].exists)
        XCTAssertTrue(app.buttons["morde.shell.tab.resources"].exists)
        app.terminate()
    }

    func testRevokeOnlyTheOtherClientInstallation() throws {
        let config = try localConfig()
        guard let targetID = ProcessInfo.processInfo.environment["MORDE_IOS03_TEST_TARGET_INSTALLATION_ID"],
              !targetID.isEmpty else {
            throw XCTSkip("Set the newly created local secondary installation ID.")
        }
        let app = try signedInApp(config)
        openAccountSettings(app)
        tap(button(app, "Manage client installations", "Manage client installations"))

        let currentRow = app.cells.containing(.staticText, identifier: "Current device").firstMatch
        XCTAssertTrue(currentRow.waitForExistence(timeout: 15))
        XCTAssertFalse(currentRow.buttons["Revoke"].exists, "The current installation must not have a revoke action.")
        let targetRow = app.cells.containing(.staticText, identifier: targetID).firstMatch
        XCTAssertTrue(targetRow.waitForExistence(timeout: 15))
        let revoke = targetRow.buttons["Revoke"]
        XCTAssertTrue(revoke.exists)
        revoke.tap()
        let confirmButtons = app.buttons.matching(identifier: "Revoke")
        XCTAssertGreaterThan(confirmButtons.count, 0)
        let confirm = confirmButtons.element(boundBy: confirmButtons.count - 1)
        tap(confirm)
        XCTAssertTrue(app.staticTexts["Installation revoked."].waitForExistence(timeout: 15))
        XCTAssertTrue(currentRow.exists, "Revoking another installation must leave this simulator signed in.")
        XCTAssertTrue(targetRow.waitForNonExistence(timeout: 15))
    }

    private func localConfig() throws -> Config {
        guard let account = ProcessInfo.processInfo.environment["MORDE_IOS03_TEST_USER"],
              let password = ProcessInfo.processInfo.environment["MORDE_IOS03_TEST_PASSWORD"],
              let server = ProcessInfo.processInfo.environment["MORDE_IOS03_TEST_SERVER"],
              let hostID = ProcessInfo.processInfo.environment["MORDE_IOS03_TEST_HOST_ID"],
              let memberID = ProcessInfo.processInfo.environment["MORDE_IOS03_TEST_MEMBER_ID"],
              let url = URL(string: server), url.scheme == "https",
              (url.host == "localhost" || url.host == "127.0.0.1"), url.port == 17443 else {
            throw XCTSkip("IOS-03 simulator acceptance only accepts the local synthetic Portal at port 17443.")
        }
        return Config(account: account, password: password, server: server, hostID: hostID, memberID: memberID)
    }

    private func signedInApp(_ config: Config) throws -> XCUIApplication {
        let app = XCUIApplication()
        app.launchArguments = ["-morde.server", config.server]
        app.launch()
        let accountField = app.textFields["morde.auth.username"]
        if accountField.waitForExistence(timeout: 5) {
            accountField.tap(); accountField.typeText(config.account)
            let passwordField = app.secureTextFields["morde.auth.password"]
            passwordField.tap(); passwordField.typeText(config.password)
            app.buttons["morde.auth.submit"].tap()
        }
        XCTAssertTrue(app.buttons["morde.shell.tab.chat"].waitForExistence(timeout: 20))
        return app
    }

    private func openAccountSettings(_ app: XCUIApplication) {
        tap(button(app, "Chat", "聊天"))
        tap(button(app, "More", "更多"))
        tap(button(app, "Settings", "设置"))
        tap(button(app, "Morde account", "Morde 账户"))
    }

    private func back(_ app: XCUIApplication) {
        let back = app.navigationBars.buttons.firstMatch
        XCTAssertTrue(back.waitForExistence(timeout: 10))
        back.tap()
    }

    private func button(_ app: XCUIApplication, _ primary: String, _ alternate: String) -> XCUIElement {
        app.buttons.matching(NSPredicate(format: "label == %@ OR label == %@", primary, alternate)).firstMatch
    }

    private func tap(_ element: XCUIElement, timeout: TimeInterval = 10) {
        XCTAssertTrue(element.waitForExistence(timeout: timeout))
        element.tap()
    }
}
