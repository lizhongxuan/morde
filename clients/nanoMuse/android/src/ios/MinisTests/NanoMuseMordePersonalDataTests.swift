import Foundation
import XCTest
@testable import Minis

final class NanoMuseMordePersonalDataTests: XCTestCase {
    func testPersonalObjectAllowlistAndTypeSpecificIdentifiers() {
        XCTAssertTrue(MordePersonalDataContract.syncObjectTypes.contains("connector_setting"))
        XCTAssertTrue(MordePersonalDataContract.syncObjectTypes.contains("schedule"))
        XCTAssertFalse(MordePersonalDataContract.mutationObjectTypes.contains("schedule"))
        XCTAssertFalse(MordePersonalDataContract.syncObjectTypes.contains("private_key"))

        XCTAssertTrue(MordePersonalDataContract.isValidObjectIdentifier(type: "profile", id: "default"))
        XCTAssertFalse(MordePersonalDataContract.isValidObjectIdentifier(type: "profile", id: "other"))
        XCTAssertTrue(MordePersonalDataContract.isValidObjectIdentifier(type: "memory", id: "m_0123abcd"))
        XCTAssertFalse(MordePersonalDataContract.isValidObjectIdentifier(type: "memory", id: "m_0123ABCD"))
        XCTAssertTrue(MordePersonalDataContract.isValidObjectIdentifier(type: "schedule", id: "j1234567"))
        XCTAssertFalse(MordePersonalDataContract.isValidObjectIdentifier(type: "schedule", id: "j12345678"))
        XCTAssertFalse(MordePersonalDataContract.isValidObjectIdentifier(type: "library_item", id: "../item"))
    }

    func testLegacySyncPauseDoesNotDisableAutomaticPersonalContentFlushOrCloudReads() {
        let scope = String(repeating: "e", count: 64)
        let suiteName = "NanoMuseMordePersonalDataTests.\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suiteName)!
        defer { defaults.removePersistentDomain(forName: suiteName) }

        NanoMuseMordeProfileSyncPreference.setEnabled(false, scopeKey: scope, defaults: defaults)

        XCTAssertFalse(NanoMuseMordeProfileSyncPreference.isEnabled(scopeKey: scope, defaults: defaults))
        XCTAssertTrue(NanoMuseMordeProfileSyncPreference.canRead(scopeKey: scope))
        XCTAssertFalse(NanoMuseMordeProfileSyncPreference.canFlush(syncEnabled: false, scopeKey: scope))
        XCTAssertTrue(NanoMuseMordePersonalContentSyncPolicy.canFlush(scopeKey: scope, isSaving: false))
        XCTAssertFalse(NanoMuseMordePersonalContentSyncPolicy.canFlush(scopeKey: scope, isSaving: true))
        XCTAssertFalse(NanoMuseMordePersonalContentSyncPolicy.canFlush(scopeKey: nil, isSaving: false))
    }

    func testProfileRefreshUsesNewerCloudRevisionWithoutReplacingAnUnsavedDraft() {
        let baseline = NanoMuseMordeProfileDraft(name: "Muse", description: "", style: "muse")
        let remote = NanoMuseMordeProfileDraft(name: "Morde", description: "Remote", style: "flat")
        let localDraft = NanoMuseMordeProfileDraft(name: "Local", description: "My draft", style: "muse")

        XCTAssertTrue(NanoMuseMordeProfileRefreshPolicy.shouldApply(
            remoteRevision: 2, currentRevision: 1, remoteDraft: remote,
            currentDraft: baseline, baselineDraft: baseline))
        XCTAssertFalse(NanoMuseMordeProfileRefreshPolicy.shouldApply(
            remoteRevision: 1, currentRevision: 1, remoteDraft: remote,
            currentDraft: baseline, baselineDraft: baseline))
        XCTAssertFalse(NanoMuseMordeProfileRefreshPolicy.shouldApply(
            remoteRevision: 2, currentRevision: 1, remoteDraft: remote,
            currentDraft: localDraft, baselineDraft: baseline))
        XCTAssertTrue(NanoMuseMordeProfileRefreshPolicy.shouldApply(
            remoteRevision: 2, currentRevision: 1, remoteDraft: localDraft,
            currentDraft: localDraft, baselineDraft: baseline))
    }

    func testMissingProfileResetsOnlyAfterConfirmedEmptySnapshotAndWithoutDraftChanges() {
        let baseline = NanoMuseMordeProfileDraft.empty
        let edited = NanoMuseMordeProfileDraft(name: "Local", description: "Draft", style: "muse")
        func shouldReset(confirmed: Bool = true, loading: Bool = false, error: String? = nil,
                         discarding: Bool = false, current: NanoMuseMordeProfileDraft = .empty) -> Bool {
            NanoMuseMordeProfileRefreshPolicy.shouldResetForMissingCloudProfile(
                hasConfirmedSnapshot: confirmed, isLoading: loading, error: error,
                discardingDraft: discarding, currentDraft: current, baselineDraft: baseline)
        }

        XCTAssertFalse(shouldReset(confirmed: false))
        XCTAssertFalse(shouldReset(loading: true))
        XCTAssertFalse(shouldReset(error: "offline"))
        XCTAssertFalse(shouldReset(current: edited))
        XCTAssertTrue(shouldReset())
        XCTAssertTrue(shouldReset(discarding: true, current: edited))
    }

    func testProfileRefreshFailurePreservesAnUnsubmittedDraft() {
        let baseline = NanoMuseMordeProfileDraft.empty
        let draft = NanoMuseMordeProfileDraft(name: "Local", description: "Unsaved", style: "flat")

        XCTAssertFalse(NanoMuseMordeProfileRefreshPolicy.shouldResetForMissingCloudProfile(
            hasConfirmedSnapshot: true, isLoading: false, error: "offline", discardingDraft: false,
            currentDraft: draft, baselineDraft: baseline))
        XCTAssertFalse(NanoMuseMordeProfileRefreshPolicy.shouldResetForMissingCloudProfile(
            hasConfirmedSnapshot: false, isLoading: false, error: nil, discardingDraft: false,
            currentDraft: draft, baselineDraft: baseline))
    }

    func testAutomaticRefreshPolicyCoversForegroundBackgroundAndReconnect() {
        XCTAssertTrue(NanoMuseMordePersonalRefreshPolicy.shouldRefreshOnForeground(isOnline: true))
        XCTAssertFalse(NanoMuseMordePersonalRefreshPolicy.shouldRefreshOnForeground(isOnline: false))

        XCTAssertTrue(NanoMuseMordePersonalRefreshPolicy.shouldRefreshOnReconnect(
            isForeground: true, wasOnline: false, isOnline: true))
        XCTAssertFalse(NanoMuseMordePersonalRefreshPolicy.shouldRefreshOnReconnect(
            isForeground: false, wasOnline: false, isOnline: true))
        XCTAssertFalse(NanoMuseMordePersonalRefreshPolicy.shouldRefreshOnReconnect(
            isForeground: true, wasOnline: true, isOnline: true))
        XCTAssertFalse(NanoMuseMordePersonalRefreshPolicy.shouldRefreshOnReconnect(
            isForeground: true, wasOnline: true, isOnline: false))

        XCTAssertTrue(NanoMuseMordePersonalRefreshPolicy.shouldRunPeriodicRefresh(
            isForeground: true, isOnline: true))
        XCTAssertFalse(NanoMuseMordePersonalRefreshPolicy.shouldRunPeriodicRefresh(
            isForeground: false, isOnline: true))
        XCTAssertFalse(NanoMuseMordePersonalRefreshPolicy.shouldRunPeriodicRefresh(
            isForeground: true, isOnline: false))
    }

    func testMutationReceiptMustMatchScopeObjectAndCloudSequence() {
        let scope = String(repeating: "a", count: 64)
        let expected = mutation(scopeKey: scope, mutationID: "mutation-receipt-001", content: "local edit")

        func receipt(objectType: String = "memory", objectID: String = "global",
                     revision: Int64 = 2, sequence: Int64 = 9) -> MordePersonalMutationReceipt {
            MordePersonalMutationReceipt(object_type: objectType, object_id: objectID,
                result_revision: revision, account_change_sequence: sequence, replay: false)
        }

        XCTAssertTrue(NanoMuseMordePersonalMutationReceiptPolicy.accepts(
            receipt: receipt(), mutation: expected, currentScope: scope))
        XCTAssertFalse(NanoMuseMordePersonalMutationReceiptPolicy.accepts(
            receipt: receipt(), mutation: expected, currentScope: String(repeating: "b", count: 64)))
        XCTAssertFalse(NanoMuseMordePersonalMutationReceiptPolicy.accepts(
            receipt: receipt(objectType: "goal"), mutation: expected, currentScope: scope))
        XCTAssertFalse(NanoMuseMordePersonalMutationReceiptPolicy.accepts(
            receipt: receipt(objectID: "user"), mutation: expected, currentScope: scope))
        XCTAssertFalse(NanoMuseMordePersonalMutationReceiptPolicy.accepts(
            receipt: receipt(revision: 0), mutation: expected, currentScope: scope))
        XCTAssertFalse(NanoMuseMordePersonalMutationReceiptPolicy.accepts(
            receipt: receipt(sequence: 0), mutation: expected, currentScope: scope))
    }

    func testRefreshFailurePreservesPreviouslyConfirmedProfile() {
        let confirmed = NanoMuseMordeProfileDraft(name: "Cloud", description: "Confirmed profile", style: "flat")
        var visibleDraft = confirmed

        if NanoMuseMordeProfileRefreshPolicy.shouldResetForMissingCloudProfile(
            hasConfirmedSnapshot: true, isLoading: false, error: "offline", discardingDraft: false,
            currentDraft: visibleDraft, baselineDraft: confirmed) {
            visibleDraft = .empty
        }

        XCTAssertEqual(visibleDraft, confirmed)
    }

    @MainActor func testIncrementalChangesKeepRevisionsMonotonicAndApplyDeletion() throws {
        let old = MordePersonalSyncObject(object_type: "memory", object_id: "global", revision: 3,
                                           body: .object(["content": .string("current")]))
        var objects = ["memory": ["global": old]]

        try NanoMuseMordePersonalContentStore.apply(
            MordePersonalChange(change_sequence: 8, object_type: "memory", object_id: "global",
                               revision: 2, operation: "upsert", mutation_id: nil),
            item: MordePersonalSyncObject(object_type: "memory", object_id: "global", revision: 2,
                                          body: .object(["content": .string("stale")])), to: &objects)
        XCTAssertEqual(objects["memory"]?["global"], old)

        try NanoMuseMordePersonalContentStore.apply(
            MordePersonalChange(change_sequence: 9, object_type: "memory", object_id: "global",
                               revision: 4, operation: "delete", mutation_id: nil), item: nil, to: &objects)
        XCTAssertNil(objects["memory"]?["global"])

        try NanoMuseMordePersonalContentStore.apply(
            MordePersonalChange(change_sequence: 10, object_type: "memory", object_id: "global",
                               revision: 5, operation: "upsert", mutation_id: nil),
            item: MordePersonalSyncObject(object_type: "memory", object_id: "global", revision: 5,
                                          body: .object(["content": .string("restored")])), to: &objects)
        XCTAssertEqual(objects["memory"]?["global"]?.revision, 5)
    }

    @MainActor func testDuplicateAndOutOfOrderIncrementalChangesAreIdempotent() throws {
        let previous = MordePersonalSyncObject(object_type: "memory", object_id: "global", revision: 4,
                                                 body: .object(["content": .string("previous")]))
        var objects = ["memory": ["global": previous]]

        let latestChange = MordePersonalChange(change_sequence: 12, object_type: "memory", object_id: "global",
            revision: 5, operation: "upsert", mutation_id: "mutation-latest-001")
        let latestItem = MordePersonalSyncObject(object_type: "memory", object_id: "global", revision: 5,
            body: .object(["content": .string("latest")]))
        try NanoMuseMordePersonalContentStore.apply(latestChange, item: latestItem, to: &objects)
        XCTAssertEqual(objects["memory"]?["global"], latestItem)

        let outOfOrderChange = MordePersonalChange(change_sequence: 11, object_type: "memory", object_id: "global",
            revision: 4, operation: "upsert", mutation_id: "mutation-previous-001")
        try NanoMuseMordePersonalContentStore.apply(outOfOrderChange, item: previous, to: &objects)
        XCTAssertEqual(objects["memory"]?["global"], latestItem)

        try NanoMuseMordePersonalContentStore.apply(latestChange, item: latestItem, to: &objects)
        XCTAssertEqual(objects["memory"]?["global"], latestItem)

        let staleDelete = MordePersonalChange(change_sequence: 10, object_type: "memory", object_id: "global",
            revision: 4, operation: "delete", mutation_id: "mutation-stale-delete-001")
        try NanoMuseMordePersonalContentStore.apply(staleDelete, item: nil, to: &objects)
        XCTAssertEqual(objects["memory"]?["global"], latestItem)

        let duplicateDelete = MordePersonalChange(change_sequence: 13, object_type: "memory", object_id: "global",
            revision: 6, operation: "delete", mutation_id: "mutation-delete-001")
        try NanoMuseMordePersonalContentStore.apply(duplicateDelete, item: nil, to: &objects)
        try NanoMuseMordePersonalContentStore.apply(duplicateDelete, item: nil, to: &objects)
        XCTAssertNil(objects["memory"]?["global"])
    }

    @MainActor func testIncrementalUpsertRejectsMismatchedObjectIdentity() {
        var objects: [String: [String: MordePersonalSyncObject]] = [:]
        let change = MordePersonalChange(change_sequence: 1, object_type: "memory", object_id: "global",
                                         revision: 2, operation: "upsert", mutation_id: nil)
        XCTAssertThrowsError(try NanoMuseMordePersonalContentStore.apply(
            change,
            item: MordePersonalSyncObject(object_type: "memory", object_id: "user", revision: 2,
                                          body: .object(["content": .string("wrong object")])), to: &objects))
        XCTAssertTrue(objects.isEmpty)
    }

    @MainActor func testIncrementalUpsertRead404RemovesStaleCurrentObject() throws {
        var objects = ["memory": ["global": MordePersonalSyncObject(
            object_type: "memory", object_id: "global", revision: 4,
            body: .object(["content": .string("was current")]))]]
        try NanoMuseMordePersonalContentStore.apply(
            MordePersonalChange(change_sequence: 5, object_type: "memory", object_id: "global",
                               revision: 3, operation: "upsert", mutation_id: nil),
            item: nil, to: &objects)
        XCTAssertNil(objects["memory"]?["global"])
    }

    func testGenericOutboxIsAccountScopedAndNeverOverwritesPending() throws {
        let root = temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: root) }
        let scopeA = String(repeating: "a", count: 64)
        let scopeB = String(repeating: "b", count: 64)
        let first = mutation(scopeKey: scopeA, mutationID: "mutation-001", content: "first")
        let otherAccount = mutation(scopeKey: scopeB, mutationID: "mutation-002", content: "account B")
        try NanoMuseMordePersonalDataOutbox.enqueue(first, scopeKey: scopeA, root: root)
        try NanoMuseMordePersonalDataOutbox.enqueue(otherAccount, scopeKey: scopeB, root: root)

        XCTAssertEqual(try NanoMuseMordePersonalDataOutbox.load(scopeKey: scopeA, root: root), first)
        XCTAssertEqual(try NanoMuseMordePersonalDataOutbox.load(scopeKey: scopeB, root: root), otherAccount)
        XCTAssertThrowsError(try NanoMuseMordePersonalDataOutbox.enqueue(
            mutation(scopeKey: scopeA, mutationID: "mutation-003", content: "overwrite"),
            scopeKey: scopeA, root: root)) { error in
                XCTAssertEqual(error as? NanoMuseMordePersonalDataOutboxError, .pendingMutation)
            }
        XCTAssertEqual(try NanoMuseMordePersonalDataOutbox.load(scopeKey: scopeA, root: root), first)

        try NanoMuseMordePersonalDataOutbox.acknowledge(scopeKey: scopeA, mutationID: "wrong-id", root: root)
        XCTAssertEqual(try NanoMuseMordePersonalDataOutbox.load(scopeKey: scopeA, root: root), first)
        try NanoMuseMordePersonalDataOutbox.acknowledge(scopeKey: scopeA, mutationID: first.mutationID, root: root)
        XCTAssertNil(try NanoMuseMordePersonalDataOutbox.load(scopeKey: scopeA, root: root))
        XCTAssertEqual(try NanoMuseMordePersonalDataOutbox.load(scopeKey: scopeB, root: root), otherAccount)
    }

    func testGenericOutboxIsolatedByServiceInstanceForSameAccount() throws {
        let root = temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: root) }
        let accountA = try XCTUnwrap(MordeClientDataScope(
            serviceOrigin: "https://morde.example",
            serviceInstanceID: "00000000-0000-0000-0000-000000000001",
            accountID: "user-1", installationID: "personal-data-v1"))
        let accountB = try XCTUnwrap(MordeClientDataScope(
            serviceOrigin: "https://morde.example",
            serviceInstanceID: "00000000-0000-0000-0000-000000000002",
            accountID: "user-1", installationID: "personal-data-v1"))
        XCTAssertNotEqual(accountA.storageKey, accountB.storageKey)

        let mutationA = mutation(scopeKey: accountA.storageKey, mutationID: "mutation-instance-001",
                                 content: "service A")
        try NanoMuseMordePersonalDataOutbox.enqueue(mutationA, scopeKey: accountA.storageKey, root: root)

        XCTAssertEqual(try NanoMuseMordePersonalDataOutbox.load(scopeKey: accountA.storageKey, root: root), mutationA)
        XCTAssertNil(try NanoMuseMordePersonalDataOutbox.load(scopeKey: accountB.storageKey, root: root))
        try NanoMuseMordePersonalDataOutbox.acknowledge(
            scopeKey: accountB.storageKey, mutationID: mutationA.mutationID, root: root)
        XCTAssertEqual(try NanoMuseMordePersonalDataOutbox.load(scopeKey: accountA.storageKey, root: root), mutationA)

        try NanoMuseMordePersonalDataOutbox.acknowledge(
            scopeKey: accountA.storageKey, mutationID: mutationA.mutationID, root: root)
        XCTAssertNil(try NanoMuseMordePersonalDataOutbox.load(scopeKey: accountA.storageKey, root: root))
    }

    func testGenericOutboxEnforcesSerializedSizeLimit() throws {
        let root = temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: root) }
        let scope = String(repeating: "c", count: 64)
        let tooLarge = mutation(scopeKey: scope, mutationID: "mutation-004",
                                content: String(repeating: "x", count: NanoMuseMordePersonalDataOutbox.maxMutationBytes))
        XCTAssertThrowsError(try NanoMuseMordePersonalDataOutbox.enqueue(tooLarge, scopeKey: scope, root: root)) { error in
            XCTAssertEqual(error as? NanoMuseMordePersonalDataOutboxError, .tooLarge)
        }
    }

    func testPersonalRevisionConflictDecodesOnlyMatchingConflictMetadata() throws {
        let live = Data(#"{"error":{"code":"conflict","message":"conflict","details":{"reason":"personal_object_revision_changed","base_revision":5,"current_revision":7,"deleted":false}}}"#.utf8)
        let deleted = Data(#"{"error":{"code":"conflict","message":"conflict","details":{"reason":"personal_object_revision_changed","base_revision":5,"current_revision":7,"deleted":true}}}"#.utf8)
        let unrelated = Data(#"{"error":{"code":"conflict","message":"conflict","details":{"reason":"schedule_version_changed","base_revision":5,"current_revision":7,"deleted":false}}}"#.utf8)
        let legacy = Data(#"{"error":{"code":"conflict","message":"conflict","details":{"base_revision":5,"current_revision":7,"deleted":true}}}"#.utf8)

        XCTAssertEqual(MordePersonalRevisionConflict.decode(live),
            MordePersonalRevisionConflict(baseRevision: 5, currentRevision: 7, deleted: false, message: "conflict"))
        XCTAssertEqual(MordePersonalRevisionConflict.decode(deleted)?.deleted, true)
        XCTAssertEqual(MordePersonalRevisionConflict.decode(legacy)?.deleted, true)
        XCTAssertNil(MordePersonalRevisionConflict.decode(unrelated))
    }

    func testOutboxRebaseUsesNewMutationIDAndPreservesBodyButCannotReviveTombstones() throws {
        let root = temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: root) }
        let scope = String(repeating: "d", count: 64)
        let original = mutation(scopeKey: scope, mutationID: "mutation-rebase-001", content: "local edit")
        try NanoMuseMordePersonalDataOutbox.enqueue(original, scopeKey: scope, root: root)

        let liveConflict = MordePersonalRevisionConflict(baseRevision: 1, currentRevision: 3,
                                                          deleted: false, message: "conflict")
        let rebased = try XCTUnwrap(NanoMuseMordePersonalDataOutbox.rebase(scopeKey: scope,
            mutationID: original.mutationID, conflict: liveConflict, root: root))
        XCTAssertNotEqual(rebased.mutationID, original.mutationID)
        XCTAssertEqual(rebased.baseRevision, 3)
        XCTAssertEqual(rebased.objectType, original.objectType)
        XCTAssertEqual(rebased.objectID, original.objectID)
        XCTAssertEqual(rebased.body, original.body)
        XCTAssertEqual(try NanoMuseMordePersonalDataOutbox.load(scopeKey: scope, root: root), rebased)
        XCTAssertNil(try NanoMuseMordePersonalDataOutbox.rebase(scopeKey: scope,
            mutationID: original.mutationID, conflict: liveConflict, root: root))

        let tombstone = MordePersonalRevisionConflict(baseRevision: 3, currentRevision: 4,
                                                       deleted: true, message: "conflict")
        XCTAssertNil(try NanoMuseMordePersonalDataOutbox.rebase(scopeKey: scope,
            mutationID: rebased.mutationID, conflict: tombstone, root: root))
        XCTAssertEqual(try NanoMuseMordePersonalDataOutbox.load(scopeKey: scope, root: root), rebased)
        XCTAssertFalse(try NanoMuseMordePersonalDataOutbox.discard(scopeKey: scope,
            mutationID: "wrong-id", root: root))
        XCTAssertTrue(try NanoMuseMordePersonalDataOutbox.discard(scopeKey: scope,
            mutationID: rebased.mutationID, root: root))
        XCTAssertNil(try NanoMuseMordePersonalDataOutbox.load(scopeKey: scope, root: root))
    }

    func testAttachmentDigestChunkBoundariesAndStagingValidation() throws {
        let bytes = Data("personal attachment".utf8)
        let digest = MordePersonalAttachmentChunkSupport.sha256(data: bytes)
        XCTAssertEqual(digest, "49c36d1d2bcbabdad4802b302b733bfadf42b18e5745f7b88a9e1ca5d27c8800")
        XCTAssertEqual(MordePersonalAttachmentChunkSupport.chunkCount(sizeBytes: 0), Optional(Int32(0)))
        XCTAssertEqual(MordePersonalAttachmentChunkSupport.chunkCount(sizeBytes: Int64(MordePersonalAttachmentChunkSupport.chunkBytes) + 7), Optional(Int32(2)))
        XCTAssertEqual(MordePersonalAttachmentChunkSupport.chunkSize(sizeBytes: Int64(MordePersonalAttachmentChunkSupport.chunkBytes) + 7, sequence: 0),
                       Optional(MordePersonalAttachmentChunkSupport.chunkBytes))
        XCTAssertEqual(MordePersonalAttachmentChunkSupport.chunkSize(sizeBytes: Int64(MordePersonalAttachmentChunkSupport.chunkBytes) + 7, sequence: 1), Optional(7))

        let root = temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: root) }
        let source = root.appendingPathComponent("source.bin")
        try bytes.write(to: source)
        let staged = try NanoMuseMordePersonalAttachmentStaging.stage(sourceURL: source,
            expectedSizeBytes: Int64(bytes.count), expectedSHA256: digest, maxBytes: 1024, root: root)
        XCTAssertEqual(staged.sizeBytes, Int64(bytes.count))
        XCTAssertEqual(try Data(contentsOf: staged.fileURL), bytes)
        XCTAssertThrowsError(try NanoMuseMordePersonalAttachmentStaging.stage(sourceURL: source,
            expectedSizeBytes: Int64(bytes.count), expectedSHA256: String(repeating: "0", count: 64),
            maxBytes: 1024, root: root))
        NanoMuseMordePersonalAttachmentStaging.discard(staged)
        XCTAssertFalse(FileManager.default.fileExists(atPath: staged.fileURL.path))
    }

    private func mutation(scopeKey: String, mutationID: String, content: String) -> MordePersonalDataMutation {
        MordePersonalDataMutation(scopeKey: scopeKey, mutationID: mutationID,
            objectType: "memory", objectID: "global", baseRevision: 0,
            body: .object(["content": .string(content)]), isDelete: false)
    }

    private func temporaryDirectory() -> URL {
        let url = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try! FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        return url
    }
}
