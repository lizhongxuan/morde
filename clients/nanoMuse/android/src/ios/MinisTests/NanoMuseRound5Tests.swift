import XCTest
import Combine
@testable import Minis

/// 0.1.38: the pure parts of the composer's native field and its fail-safe, Contract C9 on the
/// phone (scope, tail, the side-chat switch, presence and its expiry, the working line), the
/// own-relay rule and the phone-region check. Android: SyncEngineTest, PresenceTest,
/// CloudSignInTest; relay: tests/test_sync.py.
@MainActor
final class NanoMuseRound5Tests: XCTestCase {

    // MARK: - Composer field

    func testReturnIsOneAppendedNewline() {
        XCTAssertTrue(NanoMuseComposerField.isReturn(from: "hello", to: "hello\n"))
        XCTAssertTrue(NanoMuseComposerField.isReturn(from: "", to: "\n"))
        XCTAssertFalse(NanoMuseComposerField.isReturn(from: "hello", to: "hello"), "nothing typed")
        XCTAssertFalse(NanoMuseComposerField.isReturn(from: "hello", to: "hello\n\n"), "two at once is a paste")
        XCTAssertFalse(NanoMuseComposerField.isReturn(from: "hello", to: "hell\n"), "a character went too")
        XCTAssertFalse(NanoMuseComposerField.isReturn(from: "a\nb", to: "a\nb c"), "a newline in the middle is not Return")
    }

    func testReturnSendsWithAHardwareKeyboardOrThePreference() {
        XCTAssertTrue(NanoMuseComposerField.returnSends(hardwareKeyboard: true, preference: 0))
        XCTAssertTrue(NanoMuseComposerField.returnSends(hardwareKeyboard: false, preference: 1))
        XCTAssertFalse(NanoMuseComposerField.returnSends(hardwareKeyboard: false, preference: 0), "on-screen Return is a newline by default")
    }

    func testKeyboardWatcherTracksSystemNotifications() {
        let watcher = NanoMuseKeyboardWatcher()
        var cancellables: Set<AnyCancellable> = []
        let shown = expectation(description: "keyboard shown")
        let hidden = expectation(description: "keyboard hidden")
        watcher.$visible.dropFirst().sink { visible in
            if visible { shown.fulfill() } else { hidden.fulfill() }
        }.store(in: &cancellables)

        XCTAssertFalse(watcher.visible)
        NotificationCenter.default.post(name: UIResponder.keyboardWillShowNotification, object: nil)
        wait(for: [shown], timeout: 2)
        XCTAssertTrue(watcher.visible)

        NotificationCenter.default.post(name: UIResponder.keyboardWillHideNotification, object: nil)
        wait(for: [hidden], timeout: 2)
        XCTAssertFalse(watcher.visible)
    }

    func testOverflowCountsLinesAndRoughWraps() {
        XCTAssertFalse(NanoMuseComposerField.overflows("one line", lines: 6))
        XCTAssertFalse(NanoMuseComposerField.overflows("1\n2\n3\n4\n5\n6", lines: 6), "six lines fit six")
        XCTAssertTrue(NanoMuseComposerField.overflows("1\n2\n3\n4\n5\n6\n7", lines: 6))
        XCTAssertTrue(NanoMuseComposerField.overflows(String(repeating: "word ", count: 80), lines: 6), "a long paragraph wraps past the cap")
    }

    // MARK: - Composer watch

    func testComposerIsMissingWhenNothingIsAttachedOrNothingHasHeight() {
        XCTAssertTrue(NanoMuseComposerWatch.isMissing(attached: 0, hostHeight: 80, frameHeight: 80), "no probe in a window")
        XCTAssertTrue(NanoMuseComposerWatch.isMissing(attached: 1, hostHeight: 0, frameHeight: 80), "the column collapsed")
        XCTAssertTrue(NanoMuseComposerWatch.isMissing(attached: 1, hostHeight: 80, frameHeight: 0), "the bar reports no height")
        XCTAssertFalse(NanoMuseComposerWatch.isMissing(attached: 1, hostHeight: 80, frameHeight: 64))
    }

    // MARK: - C9: scope, tail, the switch

    func testPullPathCarriesScopeAndTail() {
        XCTAssertEqual(NanoMuseSync.pullPath(since: 0, sideChats: false, tail: 300), "/v1/sync/changes?since=0&limit=500&scope=main&tail=300")
        XCTAssertEqual(NanoMuseSync.pullPath(since: 0, sideChats: true, tail: 300), "/v1/sync/changes?since=0&limit=500&scope=all&tail=300")
        XCTAssertEqual(NanoMuseSync.pullPath(since: 4711, sideChats: false), "/v1/sync/changes?since=4711&limit=500&scope=main", "a normal pull: no tail")
        XCTAssertEqual(NanoMuseSync.tail, 300)
    }

    func testSideChatsAreOnlyTakenWithTheSwitchOn() {
        XCTAssertTrue(NanoMuseSync.takes(kind: "main", sideChats: false))
        XCTAssertFalse(NanoMuseSync.takes(kind: "side", sideChats: false), "a side row is ignored while the switch is off")
        XCTAssertTrue(NanoMuseSync.takes(kind: "side", sideChats: true))
        XCTAssertTrue(NanoMuseSync.takes(kind: "main", sideChats: true))
    }

    func testSideChatSwitchDefaultsOff() {
        UserDefaults.standard.removeObject(forKey: "nanomuse.sync.sidechats")
        XCTAssertFalse(NanoMuseSync.shared.sideChats)
    }

    // MARK: - C9: presence

    func testPresenceFrameParses() {
        let frame: [String: Any] = ["cid": "c1", "from": "dev-a", "device_name": "kwai", "working": true, "at": 1_700_000_000]
        let entry = NanoMusePresence.parse(frame)
        XCTAssertEqual(entry?.cid, "c1")
        XCTAssertEqual(entry?.from, "dev-a")
        XCTAssertEqual(entry?.deviceName, "kwai")
        XCTAssertEqual(entry?.at.timeIntervalSince1970, 1_700_000_000)
        XCTAssertNil(NanoMusePresence.parse(["from": "dev-a"]), "no cid, no entry")
        XCTAssertNil(NanoMusePresence.parse(["cid": "c1"]), "no device, no entry")
        // relay 0.31: the frame says what the run is doing; without the fields it is plain working
        XCTAssertEqual(entry?.state, "working")
        XCTAssertEqual(entry?.detail, "")
        let step = NanoMusePresence.parse(["cid": "c1", "from": "dev-a", "working": true, "at": 1, "state": "waiting", "detail": " a card "])
        XCTAssertEqual(step?.state, "waiting")
        XCTAssertEqual(step?.detail, "a card")
        let odd = NanoMusePresence.parse(["cid": "c1", "from": "dev-a", "working": true, "at": 1, "state": "dancing", "detail": String(repeating: "x", count: 300)])
        XCTAssertEqual(odd?.state, "working")
        XCTAssertEqual(odd?.detail.count, 160)
    }

    func testWorkingSentenceFollowsTheStep() {
        let plain = NanoMusePresence.Working(cid: "c1", from: "dev-a", deviceName: "kwai", at: Date())
        XCTAssertEqual(NanoMusePresence.sentence(device: "kwai", entry: plain), String(format: AppLocalized("%@ is working…"), "kwai"))
        let step = NanoMusePresence.Working(cid: "c1", from: "dev-a", deviceName: "kwai", at: Date(), state: "working", detail: "打开携程网站")
        XCTAssertEqual(NanoMusePresence.sentence(device: "kwai", entry: step), String(format: AppLocalized("%1$@ is working: %2$@"), "kwai", "打开携程网站"))
        let card = NanoMusePresence.Working(cid: "c1", from: "dev-a", deviceName: "kwai", at: Date(), state: "waiting", detail: "")
        XCTAssertEqual(NanoMusePresence.sentence(device: "kwai", entry: card), String(format: AppLocalized("%@ is waiting for you"), "kwai"))
    }

    func testPresenceExpiresAfterTenMinutes() {
        let at = Date(timeIntervalSince1970: 1_700_000_000)
        XCTAssertTrue(NanoMusePresence.isLive(at: at, now: at.addingTimeInterval(599)))
        XCTAssertFalse(NanoMusePresence.isLive(at: at, now: at.addingTimeInterval(600)))
        XCTAssertFalse(NanoMusePresence.isLive(at: at, now: at.addingTimeInterval(3600)))
        XCTAssertEqual(NanoMusePresence.ttl, 600)
    }

    func testWorkingLineOnlyUnderARemoteUserLine() {
        let entry = NanoMusePresence.Working(cid: "c1", from: "dev-a", deviceName: "kwai", at: Date())
        XCTAssertEqual(NanoMusePresence.line(lastIsRemoteUser: true, lastFromDevice: "kwai", entry: entry, me: "dev-b"), "kwai")
        XCTAssertNil(NanoMusePresence.line(lastIsRemoteUser: false, lastFromDevice: nil, entry: entry, me: "dev-b"), "our own line, or an assistant row: nothing")
        XCTAssertNil(NanoMusePresence.line(lastIsRemoteUser: true, lastFromDevice: "kwai", entry: nil, me: "dev-b"), "nobody working: nothing")
        XCTAssertNil(NanoMusePresence.line(lastIsRemoteUser: true, lastFromDevice: "kwai", entry: entry, me: "dev-a"), "our own presence echo is not shown")
        let unnamed = NanoMusePresence.Working(cid: "c1", from: "dev-a", deviceName: "", at: Date())
        XCTAssertEqual(NanoMusePresence.line(lastIsRemoteUser: true, lastFromDevice: "Pixel 8", entry: unnamed, me: "dev-b"), "Pixel 8", "the caption's device name stands in")
    }

    // MARK: - Own relay

    func testRelayAddressesAreNormalised() {
        XCTAssertEqual(NanoMuseCloud.normalizedRelay("relay.example.org"), "https://relay.example.org")
        XCTAssertEqual(NanoMuseCloud.normalizedRelay(" https://relay.example.org/ "), "https://relay.example.org")
        XCTAssertEqual(NanoMuseCloud.normalizedRelay("http://192.168.1.20:8787/"), "http://192.168.1.20:8787")
        XCTAssertNil(NanoMuseCloud.normalizedRelay(""))
        XCTAssertNil(NanoMuseCloud.normalizedRelay("https://"))
    }

    func testPrivateHosts() {
        for host in ["10.0.0.5", "192.168.1.20", "172.16.0.1", "172.31.255.1", "localhost", "mac.local", "studio.tail1234.ts.net"] {
            XCTAssertTrue(NanoMuseCloud.isPrivateHost(host), host)
        }
        for host in ["172.15.0.1", "172.32.0.1", "relay.example.org", "11.0.0.1", "example.local.com"] {
            XCTAssertFalse(NanoMuseCloud.isPrivateHost(host), host)
        }
        // one rule with the proxy's bypass and Android's LanOnly: parsed addresses, not prefixes
        for host in ["10.foo.example.com", "192.168.example.org", "127.example.org", "ts.net.example.org", "100.128.0.1", "2001:db8::1", "[2001:db8::1]"] {
            XCTAssertFalse(NanoMuseCloud.isPrivateHost(host), host)
        }
        for host in ["100.64.0.1", "100.101.102.103", "169.254.3.4", "127.0.0.1", "[::1]", "fd00::1", "[fe80::1%en0]", "desktop", "nas.lan", "box.home.arpa", "LOCALHOST"] {
            XCTAssertTrue(NanoMuseCloud.isPrivateHost(host), host)
        }
    }

    func testHttpsIsRequiredUnlessPrivate() {
        XCTAssertNil(NanoMuseCloud.relayProblem("https://relay.example.org"))
        XCTAssertNil(NanoMuseCloud.relayProblem("relay.example.org"), "no scheme means https")
        XCTAssertNil(NanoMuseCloud.relayProblem("http://192.168.1.20:8787"))
        XCTAssertNil(NanoMuseCloud.relayProblem("http://mac.local:8787"))
        XCTAssertNotNil(NanoMuseCloud.relayProblem("http://relay.example.org"), "plain http on the internet")
        XCTAssertNotNil(NanoMuseCloud.relayProblem("ftp://relay.example.org"))
        XCTAssertNotNil(NanoMuseCloud.relayProblem(""))
    }

    // MARK: - Phone region

    func testNumbersOutsideMainlandChinaAreToldBeforeTheRequest() {
        XCTAssertTrue(NanoMuseCloud.needsEmailInstead(identifier: "+1 415 555 0100"))
        XCTAssertTrue(NanoMuseCloud.needsEmailInstead(identifier: "+44 7700 900123"))
        XCTAssertTrue(NanoMuseCloud.needsEmailInstead(identifier: "0081-90-1234-5678"))
        XCTAssertFalse(NanoMuseCloud.needsEmailInstead(identifier: "+86 138 0013 8000"))
        XCTAssertFalse(NanoMuseCloud.needsEmailInstead(identifier: "0086 13800138000"))
        XCTAssertFalse(NanoMuseCloud.needsEmailInstead(identifier: "13800138000"), "bare digits are the relay's call")
        XCTAssertFalse(NanoMuseCloud.needsEmailInstead(identifier: "someone@example.org"))
        XCTAssertFalse(NanoMuseCloud.needsEmailInstead(identifier: ""))
    }

    func testPhoneRegionErrorSaysTheSameSentence() {
        let error = NanoMuseCloudError(code: "phone_region", message: "sms only for +86", status: 400)
        XCTAssertEqual(NanoMuseCloud.describe(error), NanoMuseCloud.phoneRegionSentence)
        XCTAssertFalse(NanoMuseCloud.phoneRegionSentence.isEmpty)
    }
}
