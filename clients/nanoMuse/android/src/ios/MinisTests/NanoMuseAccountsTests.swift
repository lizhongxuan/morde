import XCTest
@testable import Minis

/// 0.1.39, contract C10 on the phone: which account a local conversation belongs to, what the
/// lists and the push take under the signed-in account, and how an account is told apart.
/// Android: the same rules in SyncEngineTest; desktop: sync.test.mjs; relay: tests/test_sync.py.
@MainActor
final class NanoMuseAccountsTests: XCTestCase {

    // MARK: - Rule 2 and 3: shown and pushed

    func testUnownedAndOwnConversationsShowUnderAnAccount() {
        XCTAssertTrue(NanoMuseSync.visible(owner: nil, current: "acc-b"), "a conversation no account has synced is shown — and becomes the account's with its first push")
        XCTAssertTrue(NanoMuseSync.visible(owner: "acc-b", current: "acc-b"))
    }

    func testAnotherAccountsConversationsStayHidden() {
        XCTAssertFalse(NanoMuseSync.visible(owner: "acc-a", current: "acc-b"), "A's conversation neither shows under B nor goes up as B's")
    }

    func testSignedOutShowsEverythingOnThePhone() {
        XCTAssertTrue(NanoMuseSync.visible(owner: "acc-a", current: ""))
        XCTAssertTrue(NanoMuseSync.visible(owner: nil, current: ""))
    }

    // MARK: - The account's key

    func testTheRelaysOpaqueIdKeysTheTable() {
        XCTAssertEqual(NanoMuseSync.accountKey(id: "acc-7f3a", hint: "138****1234", apiKey: "nm_abcdefgh12345678"), "acc-7f3a", "the id, never the identifier")
    }

    func testARelayWithoutAnIdFallsBackTo0138sKey() {
        XCTAssertEqual(NanoMuseSync.accountKey(id: nil, hint: "a***@b.com", apiKey: "nm_abcdefgh12345678"), "a***@b.com")
        XCTAssertEqual(NanoMuseSync.accountKey(id: "", hint: "", apiKey: "nm_abcdefgh12345678"), "12345678", "the key's tail when there is no hint either")
        XCTAssertEqual(NanoMuseSync.accountKey(id: nil, hint: nil, apiKey: nil), "", "signed out")
    }

    func testTheAccountCarriesItsIdAndDecodesWithoutOne() throws {
        let reply: [String: Any] = ["account": ["id": "acc-7f3a", "channel": "sms", "hint": "138****1234"], "tokens": ["granted": 1, "used": 0]]
        XCTAssertEqual(NanoMuseCloud.parseAccount(reply).id, "acc-7f3a")
        XCTAssertEqual(NanoMuseCloud.parseAccount(["account": ["channel": "sms", "hint": "x"]]).id, "", "a relay that sends no id")
        // an account cached by 0.1.38 has no `id` field
        let cached = Data(#"{"channel":"sms","hint":"138****1234","granted":1,"used":0,"usedToday":0,"dailyCap":0,"checkedAt":0}"#.utf8)
        let decoded = try JSONDecoder().decode(NanoMuseCloudAccount.self, from: cached)
        XCTAssertEqual(decoded.id, "")
        XCTAssertEqual(decoded.hint, "138****1234")
        XCTAssertNil(decoded.poolLeftCny, "an account cached before 1.0.1 carries no pool")
    }

    func testTheSettingsRowReadsThePoolInYuanOnAMoneyRelay() {
        // the Settings row and the account page name the same figure: what is left of the pool
        let reply: [String: Any] = ["account": ["channel": "sms", "hint": "138****1234"], "tokens": ["granted": 1000, "used": 10],
                                    "spend": ["total": 1.8, "grant": 5, "left": 3.2, "usd_cny": 7.2]]
        XCTAssertEqual(NanoMuseCloud.parseAccount(reply).poolLeftCny, 3.2)
        // a member or an open relay has no pool; an older relay sends tokens only
        XCTAssertNil(NanoMuseCloud.parseAccount(["account": ["channel": "sms"], "spend": ["total": 1.8, "unlimited": true, "left": NSNull()]]).poolLeftCny)
        XCTAssertNil(NanoMuseCloud.parseAccount(["account": ["channel": "sms"], "tokens": ["granted": 1000, "used": 10]]).poolLeftCny)
        // spent past the pool reads as nothing left, not a negative figure
        XCTAssertEqual(NanoMuseCloud.parseAccount(["account": ["channel": "sms"], "spend": ["grant": 5, "left": -0.4]]).poolLeftCny, 0)
    }

    // MARK: - C12 (0.1.40): a key the relay refuses

    func testARefusedKeyKeepsTheAccountsDataAside() {
        XCTAssertTrue(NanoMuseAccountData.keepOnRefusedKey(code: "bad_key"), "revoked elsewhere, or by the person's own Sign out everywhere")
        XCTAssertTrue(NanoMuseAccountData.keepOnRefusedKey(code: nil), "a relay reset or a relay bug answers like a revoked key")
        XCTAssertTrue(NanoMuseAccountData.keepOnRefusedKey(code: "http_401"), "an older relay, or a code the phone does not know")
    }

    func testOnlyADeletedAccountLeavesNothingToComeBackTo() {
        XCTAssertFalse(NanoMuseAccountData.keepOnRefusedKey(code: NanoMuseAccountData.accountDeleted))
        XCTAssertEqual(NanoMuseAccountData.accountDeleted, "account_deleted", "the relay's code, as documented")
    }

    func testMordePersonalProfileOutboxIsScopedAndNeverReplacesPendingData() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: root) }
        let scope = String(repeating: "a", count: 64)
        let otherScope = String(repeating: "b", count: 64)
        let body = MordePersonalProfileBody(name: "Muse", avatar: "dragon", emoji: nil, color: nil,
                                            description: "", style: "")
        let pending = NanoMuseMordeProfileMutation(scopeKey: scope, mutationID: UUID().uuidString,
                                                   baseRevision: 0, body: body)
        try NanoMuseMordeProfileOutbox.save(pending, root: root)
        XCTAssertEqual(try NanoMuseMordeProfileOutbox.load(scopeKey: scope, root: root), pending)
        XCTAssertNil(try NanoMuseMordeProfileOutbox.load(scopeKey: otherScope, root: root))

        let replacement = NanoMuseMordeProfileMutation(scopeKey: scope, mutationID: UUID().uuidString,
                                                       baseRevision: 1, body: body)
        XCTAssertThrowsError(try NanoMuseMordeProfileOutbox.save(replacement, root: root))
        try NanoMuseMordeProfileOutbox.clear(scopeKey: scope, mutationID: replacement.mutationID, root: root)
        XCTAssertEqual(try NanoMuseMordeProfileOutbox.load(scopeKey: scope, root: root), pending)
        try NanoMuseMordeProfileOutbox.clear(scopeKey: scope, mutationID: pending.mutationID, root: root)
        XCTAssertNil(try NanoMuseMordeProfileOutbox.load(scopeKey: scope, root: root))
    }

    func testMordePersonalProfileOutboxRebasesLiveConflictAndKeepsTombstonePending() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: root) }
        let scope = String(repeating: "c", count: 64)
        let body = MordePersonalProfileBody(name: "Muse", avatar: "dragon", emoji: nil, color: nil,
                                            description: "local change", style: "")
        let original = NanoMuseMordeProfileMutation(scopeKey: scope, mutationID: "profile-mutation-001",
                                                    baseRevision: 1, body: body)
        try NanoMuseMordeProfileOutbox.save(original, root: root)

        let liveConflict = MordePersonalRevisionConflict(baseRevision: 1, currentRevision: 3,
                                                          deleted: false, message: "conflict")
        let rebased = try XCTUnwrap(NanoMuseMordeProfileOutbox.rebase(scopeKey: scope,
            mutationID: original.mutationID, conflict: liveConflict, root: root))
        XCTAssertNotEqual(rebased.mutationID, original.mutationID)
        XCTAssertEqual(rebased.baseRevision, 3)
        XCTAssertEqual(rebased.body, original.body)
        XCTAssertEqual(try NanoMuseMordeProfileOutbox.load(scopeKey: scope, root: root), rebased)

        let tombstone = MordePersonalRevisionConflict(baseRevision: 3, currentRevision: 4,
                                                       deleted: true, message: "conflict")
        XCTAssertNil(try NanoMuseMordeProfileOutbox.rebase(scopeKey: scope,
            mutationID: rebased.mutationID, conflict: tombstone, root: root))
        XCTAssertEqual(try NanoMuseMordeProfileOutbox.load(scopeKey: scope, root: root), rebased)
        XCTAssertFalse(try NanoMuseMordeProfileOutbox.discard(scopeKey: scope,
            mutationID: "wrong-profile-mutation", root: root))
        XCTAssertTrue(try NanoMuseMordeProfileOutbox.discard(scopeKey: scope,
            mutationID: rebased.mutationID, root: root))
        XCTAssertNil(try NanoMuseMordeProfileOutbox.load(scopeKey: scope, root: root))
    }

    func testPausingProfileSyncKeepsItsPendingMutationAndIsScopedPerAccount() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: root) }
        let suite = "NanoMuseAccountsTests.\(UUID().uuidString)"
        let defaults = try XCTUnwrap(UserDefaults(suiteName: suite))
        defer { defaults.removePersistentDomain(forName: suite) }
        let accountA = String(repeating: "a", count: 64)
        let accountB = String(repeating: "b", count: 64)
        let body = MordePersonalProfileBody(name: "Muse", avatar: "dragon", emoji: nil, color: nil,
                                            description: "", style: "")
        let pending = NanoMuseMordeProfileMutation(scopeKey: accountA, mutationID: UUID().uuidString,
                                                   baseRevision: 0, body: body)
        try NanoMuseMordeProfileOutbox.save(pending, root: root)

        XCTAssertTrue(NanoMuseMordeProfileSyncPreference.isEnabled(scopeKey: accountA, defaults: defaults))
        NanoMuseMordeProfileSyncPreference.setEnabled(false, scopeKey: accountA, defaults: defaults)
        XCTAssertFalse(NanoMuseMordeProfileSyncPreference.isEnabled(scopeKey: accountA, defaults: defaults))
        XCTAssertTrue(NanoMuseMordeProfileSyncPreference.isEnabled(scopeKey: accountB, defaults: defaults))
        XCTAssertFalse(NanoMuseMordeProfileSyncPreference.canFlush(syncEnabled: false, scopeKey: accountA))
        XCTAssertFalse(NanoMuseMordeProfileSyncPreference.canFlush(syncEnabled: true, scopeKey: nil))
        XCTAssertEqual(try NanoMuseMordeProfileOutbox.load(scopeKey: accountA, root: root), pending)
        XCTAssertNil(try NanoMuseMordeProfileOutbox.load(scopeKey: accountB, root: root))

        NanoMuseMordeProfileSyncPreference.setEnabled(true, scopeKey: accountA, defaults: defaults)
        XCTAssertTrue(NanoMuseMordeProfileSyncPreference.isEnabled(scopeKey: accountA, defaults: defaults))
        XCTAssertEqual(try NanoMuseMordeProfileOutbox.load(scopeKey: accountA, root: root), pending)
    }

    func testPausingGenericPersonalDataSyncKeepsItsPendingMutationAndIsScopedPerAccount() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: root) }
        let suite = "NanoMuseAccountsTests.\(UUID().uuidString)"
        let defaults = try XCTUnwrap(UserDefaults(suiteName: suite))
        defer { defaults.removePersistentDomain(forName: suite) }
        let accountA = String(repeating: "a", count: 64)
        let accountB = String(repeating: "b", count: 64)
        let pending = MordePersonalDataMutation(
            scopeKey: accountA, mutationID: UUID().uuidString.replacingOccurrences(of: "-", with: ""),
            objectType: "memory", objectID: "global", baseRevision: 3,
            body: .object(["content": .string("keep this local while paused")]), isDelete: false)
        try NanoMuseMordePersonalDataOutbox.enqueue(pending, scopeKey: accountA, root: root)
        let replacement = MordePersonalDataMutation(
            scopeKey: accountA, mutationID: UUID().uuidString.replacingOccurrences(of: "-", with: ""),
            objectType: "memory", objectID: "global", baseRevision: 4,
            body: .object(["content": .string("must not replace pending data")]), isDelete: false)
        XCTAssertThrowsError(try NanoMuseMordePersonalDataOutbox.enqueue(replacement, scopeKey: accountA, root: root))

        NanoMuseMordeProfileSyncPreference.setEnabled(false, scopeKey: accountA, defaults: defaults)
        XCTAssertFalse(NanoMuseMordeProfileSyncPreference.canFlush(
            syncEnabled: NanoMuseMordeProfileSyncPreference.isEnabled(scopeKey: accountA, defaults: defaults),
            scopeKey: accountA))
        XCTAssertTrue(NanoMuseMordeProfileSyncPreference.isEnabled(scopeKey: accountB, defaults: defaults))
        XCTAssertEqual(try NanoMuseMordePersonalDataOutbox.load(scopeKey: accountA, root: root), pending)
        XCTAssertNil(try NanoMuseMordePersonalDataOutbox.load(scopeKey: accountB, root: root))

        NanoMuseMordeProfileSyncPreference.setEnabled(true, scopeKey: accountA, defaults: defaults)
        XCTAssertTrue(NanoMuseMordeProfileSyncPreference.canFlush(
            syncEnabled: NanoMuseMordeProfileSyncPreference.isEnabled(scopeKey: accountA, defaults: defaults),
            scopeKey: accountA))
        XCTAssertEqual(try NanoMuseMordePersonalDataOutbox.load(scopeKey: accountA, root: root), pending)
    }

    func testSystemFilesMapOnlySoulAndMemoryToPersonalDataObjects() throws {
        let soul = try XCTUnwrap(NanoMuseSystemFile.soul.personalDataChange(
            for: "---\nname: Muse\nemoji: 🐱\nstyle: calm\nlang: en\nicon: local-only\n---\nHelpful and direct."))
        XCTAssertEqual(soul.objectType, "soul")
        XCTAssertEqual(soul.objectID, "default")
        guard case .object(let soulBody)? = soul.body else { return XCTFail("SOUL must map to an object body") }
        XCTAssertEqual(soulBody["name"], .string("Muse"))
        XCTAssertEqual(soulBody["emoji"], .string("🐱"))
        XCTAssertEqual(soulBody["style"], .string("calm"))
        XCTAssertEqual(soulBody["lang"], .string("en"))
        XCTAssertEqual(soulBody["content"], .string("Helpful and direct."))
        XCTAssertNil(soulBody["icon"], "the local-only icon field is not sent to the personal-data API")

        let user = try XCTUnwrap(NanoMuseSystemFile.user.personalDataChange(for: "About this person"))
        XCTAssertEqual(user.objectType, "memory")
        XCTAssertEqual(user.objectID, "user")
        XCTAssertEqual(user.body, .object(["content": .string("About this person")]))

        let global = try XCTUnwrap(NanoMuseSystemFile.memory.personalDataChange(for: "Durable notes"))
        XCTAssertEqual(global.objectType, "memory")
        XCTAssertEqual(global.objectID, "global")
        XCTAssertFalse(global.isDelete)

        let empty = try XCTUnwrap(NanoMuseSystemFile.memory.personalDataChange(for: ""))
        XCTAssertTrue(empty.isDelete)
        XCTAssertNil(empty.body)
        XCTAssertNil(try NanoMuseSystemFile.feed.personalDataChange(for: "feed preferences"))
        XCTAssertNil(try NanoMuseSystemFile.heartbeat.personalDataChange(for: "read-only"))
    }

    func testPersonalDataQuotaDecodesTheServerFieldsAndRejectsInvalidCounts() throws {
        let response = Data(#"{"owner_id":"owner-1","used_bytes":12,"reserved_bytes":3,"quota_bytes":100,"max_attachment_bytes":25,"remaining_bytes":85,"version":4}"#.utf8)
        let quota = try JSONDecoder().decode(MordePersonalDataQuota.self, from: response)
        XCTAssertTrue(quota.isValid)
        XCTAssertEqual(quota.used_bytes, 12)
        XCTAssertEqual(quota.remaining_bytes, 85)
        XCTAssertEqual(quota.version, 4)

        let invalid = MordePersonalDataQuota(owner_id: "owner-1", used_bytes: 12,
                                             reserved_bytes: -1, quota_bytes: 100,
                                             max_attachment_bytes: 25, remaining_bytes: 85,
                                             version: 4)
        XCTAssertFalse(invalid.isValid)
        let inconsistent = MordePersonalDataQuota(owner_id: "owner-1", used_bytes: 12,
                                                   reserved_bytes: 3, quota_bytes: 100,
                                                   max_attachment_bytes: 25, remaining_bytes: 84,
                                                   version: 4)
        XCTAssertFalse(inconsistent.isValid, "remaining_bytes must match quota minus used and reserved")
    }
}
