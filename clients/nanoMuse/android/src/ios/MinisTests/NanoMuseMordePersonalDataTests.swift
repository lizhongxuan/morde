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
