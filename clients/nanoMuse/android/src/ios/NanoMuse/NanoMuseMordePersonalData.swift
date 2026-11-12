import CryptoKit
import Foundation
import Combine
import Network
import UIKit

enum NanoMuseMordeProfileSyncPreference {
    private static func key(for scopeKey: String) -> String? {
        guard scopeKey.range(of: "^[a-f0-9]{64}$", options: .regularExpression) != nil else { return nil }
        return "nanomuse.morde.profile_sync_enabled.\(scopeKey)"
    }

    static func isEnabled(scopeKey: String, defaults: UserDefaults = .standard) -> Bool {
        guard let key = key(for: scopeKey) else { return false }
        return defaults.object(forKey: key) == nil ? true : defaults.bool(forKey: key)
    }

    static func setEnabled(_ enabled: Bool, scopeKey: String, defaults: UserDefaults = .standard) {
        guard let key = key(for: scopeKey) else { return }
        if enabled {
            defaults.removeObject(forKey: key)
        } else {
            defaults.set(false, forKey: key)
        }
    }

    static func canFlush(syncEnabled: Bool, scopeKey: String?) -> Bool {
        guard syncEnabled, let scopeKey else { return false }
        return key(for: scopeKey) != nil
    }

    static func canRead(scopeKey: String?) -> Bool {
        guard let scopeKey else { return false }
        return key(for: scopeKey) != nil
    }
}

struct MordePersonalDataMutation: Codable, Equatable, Sendable {
    let scopeKey: String
    let mutationID: String
    let objectType: String
    let objectID: String
    let baseRevision: Int64
    let body: MordePersonalJSONValue?
    let isDelete: Bool
}

enum NanoMuseMordePersonalDataOutboxError: LocalizedError, Equatable {
    case invalidMutation
    case pendingMutation
    case tooLarge
    case invalidAttachment

    var errorDescription: String? {
        switch self {
        case .invalidMutation: return "Morde personal-data change is invalid."
        case .pendingMutation: return "Resolve the pending Morde personal-data change before queuing another."
        case .tooLarge: return "Morde personal-data change exceeds the local outbox limit."
        case .invalidAttachment: return "Attachment staging data does not match its declared size and SHA-256."
        }
    }
}

enum NanoMuseMordePersonalDataOutbox {
    static let maxMutationBytes = 1_048_576

    private static func fileURL(scopeKey: String, root: URL) -> URL? {
        guard scopeKey.range(of: "^[a-f0-9]{64}$", options: .regularExpression) != nil else { return nil }
        let digest = SHA256.hash(data: Data(scopeKey.utf8)).map { String(format: "%02x", $0) }.joined()
        return root.appendingPathComponent("morde-personal-outbox", isDirectory: true)
            .appendingPathComponent("generic-\(digest).json", isDirectory: false)
    }

    private static func encoded(_ mutation: MordePersonalDataMutation) throws -> Data {
        guard mutation.scopeKey.range(of: "^[a-f0-9]{64}$", options: .regularExpression) != nil,
              MordePersonalDataContract.isValidIdempotencyKey(mutation.mutationID),
              MordePersonalDataContract.mutationObjectTypes.contains(mutation.objectType),
              MordePersonalDataContract.isValidObjectIdentifier(type: mutation.objectType, id: mutation.objectID),
              mutation.baseRevision >= 0,
              (mutation.isDelete ? mutation.body == nil : mutation.body.map(isObject) == true) else {
            throw NanoMuseMordePersonalDataOutboxError.invalidMutation
        }
        let data = try JSONEncoder().encode(mutation)
        guard data.count <= maxMutationBytes else { throw NanoMuseMordePersonalDataOutboxError.tooLarge }
        return data
    }

    private static func isObject(_ value: MordePersonalJSONValue) -> Bool {
        if case .object = value { return true }
        return false
    }

    static func load(scopeKey: String, root: URL) throws -> MordePersonalDataMutation? {
        guard let url = fileURL(scopeKey: scopeKey, root: root) else {
            throw NanoMuseMordePersonalDataOutboxError.invalidMutation
        }
        guard FileManager.default.fileExists(atPath: url.path) else { return nil }
        let data = try Data(contentsOf: url)
        guard data.count <= maxMutationBytes,
              let mutation = try? JSONDecoder().decode(MordePersonalDataMutation.self, from: data),
              mutation.scopeKey == scopeKey else {
            throw NanoMuseMordePersonalDataOutboxError.invalidMutation
        }
        _ = try encoded(mutation)
        return mutation
    }

    static func enqueue(_ mutation: MordePersonalDataMutation, scopeKey: String, root: URL) throws {
        guard mutation.scopeKey == scopeKey, let url = fileURL(scopeKey: scopeKey, root: root) else {
            throw NanoMuseMordePersonalDataOutboxError.invalidMutation
        }
        if let pending = try load(scopeKey: scopeKey, root: root) {
            guard pending == mutation else { throw NanoMuseMordePersonalDataOutboxError.pendingMutation }
            return
        }
        let data = try encoded(mutation)
        try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        try data.write(to: url, options: .atomic)
    }

    static func acknowledge(scopeKey: String, mutationID: String, root: URL) throws {
        guard let pending = try load(scopeKey: scopeKey, root: root), pending.mutationID == mutationID,
              let url = fileURL(scopeKey: scopeKey, root: root) else { return }
        try FileManager.default.removeItem(at: url)
    }

    static func rebase(scopeKey: String, mutationID: String, conflict: MordePersonalRevisionConflict,
                       root: URL) throws -> MordePersonalDataMutation? {
        guard !conflict.deleted, conflict.currentRevision >= 0,
              let pending = try load(scopeKey: scopeKey, root: root),
              pending.mutationID == mutationID, let url = fileURL(scopeKey: scopeKey, root: root) else { return nil }
        let updated = MordePersonalDataMutation(scopeKey: pending.scopeKey, mutationID: UUID().uuidString,
            objectType: pending.objectType, objectID: pending.objectID, baseRevision: conflict.currentRevision,
            body: pending.body, isDelete: pending.isDelete)
        try encoded(updated).write(to: url, options: .atomic)
        return updated
    }

    @discardableResult
    static func discard(scopeKey: String, mutationID: String, root: URL) throws -> Bool {
        guard let pending = try load(scopeKey: scopeKey, root: root), pending.mutationID == mutationID,
              let url = fileURL(scopeKey: scopeKey, root: root) else { return false }
        try FileManager.default.removeItem(at: url)
        return true
    }
}

struct NanoMuseMordeProfileMutation: Codable, Equatable, Sendable {
    let scopeKey: String
    let mutationID: String
    let baseRevision: Int64
    let body: MordePersonalProfileBody
}

enum NanoMuseMordeProfileOutboxError: LocalizedError {
    case invalidMutation
    case pendingMutation
    case tooLarge

    var errorDescription: String? {
        switch self {
        case .invalidMutation: return "Personal profile change is invalid."
        case .pendingMutation: return "Resolve the pending Morde profile change before queuing another."
        case .tooLarge: return "Personal profile change is too large to queue."
        }
    }
}

enum NanoMuseMordeProfileOutbox {
    private static let limit = 16 * 1024

    private static func url(scopeKey: String, root: URL) -> URL? {
        guard scopeKey.range(of: "^[a-f0-9]{64}$", options: .regularExpression) != nil else { return nil }
        let digest = SHA256.hash(data: Data(scopeKey.utf8)).map { String(format: "%02x", $0) }.joined()
        return root.appendingPathComponent("morde-personal-outbox", isDirectory: true)
            .appendingPathComponent("\(digest).json")
    }

    private static func valid(_ mutation: NanoMuseMordeProfileMutation) -> Bool {
        mutation.scopeKey.range(of: "^[a-f0-9]{64}$", options: .regularExpression) != nil &&
            mutation.mutationID.range(of: "^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$", options: .regularExpression) != nil &&
            mutation.baseRevision >= 0 && mutation.body.isValidForWrite
    }

    static func load(scopeKey: String, root: URL) throws -> NanoMuseMordeProfileMutation? {
        guard let url = url(scopeKey: scopeKey, root: root) else { throw NanoMuseMordeProfileOutboxError.invalidMutation }
        guard FileManager.default.fileExists(atPath: url.path) else { return nil }
        let data = try Data(contentsOf: url)
        guard data.count <= limit,
              let mutation = try? JSONDecoder().decode(NanoMuseMordeProfileMutation.self, from: data),
              mutation.scopeKey == scopeKey, valid(mutation) else {
            throw NanoMuseMordeProfileOutboxError.invalidMutation
        }
        return mutation
    }

    static func save(_ mutation: NanoMuseMordeProfileMutation, root: URL) throws {
        guard valid(mutation), let url = url(scopeKey: mutation.scopeKey, root: root) else {
            throw NanoMuseMordeProfileOutboxError.invalidMutation
        }
        if let pending = try load(scopeKey: mutation.scopeKey, root: root) {
            guard pending == mutation else { throw NanoMuseMordeProfileOutboxError.pendingMutation }
            return
        }
        let data = try JSONEncoder().encode(mutation)
        guard data.count <= limit else { throw NanoMuseMordeProfileOutboxError.tooLarge }
        try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        try data.write(to: url, options: .atomic)
    }

    static func clear(scopeKey: String, mutationID: String, root: URL) throws {
        guard let pending = try load(scopeKey: scopeKey, root: root), pending.mutationID == mutationID,
              let url = url(scopeKey: scopeKey, root: root) else { return }
        try FileManager.default.removeItem(at: url)
    }

    static func rebase(scopeKey: String, mutationID: String, conflict: MordePersonalRevisionConflict,
                       root: URL) throws -> NanoMuseMordeProfileMutation? {
        guard !conflict.deleted, conflict.currentRevision >= 0,
              let pending = try load(scopeKey: scopeKey, root: root), pending.mutationID == mutationID,
              let url = url(scopeKey: scopeKey, root: root) else { return nil }
        let updated = NanoMuseMordeProfileMutation(scopeKey: pending.scopeKey, mutationID: UUID().uuidString,
            baseRevision: conflict.currentRevision, body: pending.body)
        guard valid(updated) else { throw NanoMuseMordeProfileOutboxError.invalidMutation }
        let data = try JSONEncoder().encode(updated)
        guard data.count <= limit else { throw NanoMuseMordeProfileOutboxError.tooLarge }
        try data.write(to: url, options: .atomic)
        return updated
    }

    @discardableResult
    static func discard(scopeKey: String, mutationID: String, root: URL) throws -> Bool {
        guard let pending = try load(scopeKey: scopeKey, root: root), pending.mutationID == mutationID,
              let url = url(scopeKey: scopeKey, root: root) else { return false }
        try FileManager.default.removeItem(at: url)
        return true
    }
}

enum MordePersonalAttachmentChunkSupport {
    static let chunkBytes = MordePersonalAttachmentHash.chunkBytes

    static func sha256(data: Data) -> String { MordePersonalAttachmentHash.sha256(data) }

    static func chunkCount(sizeBytes: Int64) -> Int32? {
        MordePersonalAttachmentHash.chunkCount(sizeBytes: sizeBytes)
    }

    static func chunkSize(sizeBytes: Int64, sequence: Int32) -> Int? {
        MordePersonalAttachmentHash.chunkSize(sizeBytes: sizeBytes, sequence: sequence)
    }
}

enum NanoMuseMordePersonalAttachmentStaging {
    static func stage(sourceURL: URL, expectedSizeBytes: Int64, expectedSHA256: String,
                      maxBytes: Int64, root: URL) throws -> MordePersonalStagedAttachment {
        guard sourceURL.isFileURL, expectedSizeBytes >= 0, maxBytes >= 0, expectedSizeBytes <= maxBytes,
              expectedSHA256.range(of: "^[0-9a-f]{64}$", options: .regularExpression) != nil,
              MordePersonalAttachmentChunkSupport.chunkCount(sizeBytes: expectedSizeBytes) != nil,
              let attributes = try? FileManager.default.attributesOfItem(atPath: sourceURL.path),
              let sourceSize = attributes[.size] as? NSNumber,
              sourceSize.int64Value == expectedSizeBytes else {
            throw NanoMuseMordePersonalDataOutboxError.invalidAttachment
        }
        let sourceDigest = try MordePersonalAttachmentHash.measure(fileURL: sourceURL)
        guard sourceDigest.sizeBytes == expectedSizeBytes, sourceDigest.sha256 == expectedSHA256 else {
            throw NanoMuseMordePersonalDataOutboxError.invalidAttachment
        }

        let directory = root.appendingPathComponent("morde-personal-staging", isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let destination = directory.appendingPathComponent("\(UUID().uuidString.lowercased()).attachment", isDirectory: false)
        do {
            try FileManager.default.copyItem(at: sourceURL, to: destination)
            let stagedDigest = try MordePersonalAttachmentHash.measure(fileURL: destination)
            guard stagedDigest.sizeBytes == expectedSizeBytes, stagedDigest.sha256 == expectedSHA256 else {
                try? FileManager.default.removeItem(at: destination)
                throw NanoMuseMordePersonalDataOutboxError.invalidAttachment
            }
            return MordePersonalStagedAttachment(fileURL: destination, sizeBytes: expectedSizeBytes,
                                                 sha256: expectedSHA256)
        } catch {
            try? FileManager.default.removeItem(at: destination)
            throw error
        }
    }

    static func discard(_ attachment: MordePersonalStagedAttachment) {
        try? FileManager.default.removeItem(at: attachment.fileURL)
    }
}

@MainActor
final class NanoMuseMordePersonalContentStore: ObservableObject {
    static let shared = NanoMuseMordePersonalContentStore()

    @Published private(set) var objects: [String: [String: MordePersonalSyncObject]] = [:]
    @Published private(set) var pending: MordePersonalDataMutation?
    @Published private(set) var isLoading = false
    @Published private(set) var isSaving = false
    @Published private(set) var syncEnabled = true
    @Published private(set) var error: String?

    private var loadedScope: String?
    private var changeSequence: Int64?
    private var isForeground = UIApplication.shared.applicationState == .active
    private var networkIsOnline = false
    private var refreshQueued = false
    private var refreshLoop: Task<Void, Never>?
    private var observers: [NSObjectProtocol] = []
    private var cancellables = Set<AnyCancellable>()
    private let pathMonitor = NWPathMonitor()

    private var personalDataRoot: URL {
#if MORDE_STANDALONE
        MordePersonalStorage.root
#else
        NanoMuseDirs.root
#endif
    }

    private init() {
        let center = NotificationCenter.default
        observers.append(center.addObserver(forName: UIApplication.didBecomeActiveNotification,
                                             object: nil, queue: .main) { [weak self] _ in
            Task { @MainActor [weak self] in await self?.foregrounded() }
        })
        observers.append(center.addObserver(forName: UIApplication.didEnterBackgroundNotification,
                                             object: nil, queue: .main) { [weak self] _ in
            Task { @MainActor [weak self] in self?.backgrounded() }
        })
        MordeShellAuthorization.shared.$personalDataScopeKey
            .removeDuplicates()
            .sink { [weak self] _ in
                Task { @MainActor [weak self] in
                    guard let self else { return }
                    await self.refresh(root: self.personalDataRoot)
                }
            }
            .store(in: &cancellables)
        pathMonitor.pathUpdateHandler = { [weak self] path in
            let online = path.status == .satisfied
            Task { @MainActor [weak self] in await self?.networkChanged(isOnline: online) }
        }
        pathMonitor.start(queue: DispatchQueue(label: "morde.personal-data-network"))
    }

    private func foregrounded() async {
        isForeground = true
        updateRefreshLoop()
        if networkIsOnline { await refresh(root: personalDataRoot) }
    }

    private func backgrounded() {
        isForeground = false
        refreshLoop?.cancel()
        refreshLoop = nil
    }

    private func networkChanged(isOnline: Bool) async {
        let wasOnline = networkIsOnline
        guard wasOnline != isOnline else { return }
        networkIsOnline = isOnline
        updateRefreshLoop()
        if isForeground, isOnline, !wasOnline { await refresh(root: personalDataRoot) }
    }

    private func updateRefreshLoop() {
        refreshLoop?.cancel()
        refreshLoop = nil
        guard isForeground, networkIsOnline else { return }
        refreshLoop = Task { @MainActor [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(nanoseconds: 60_000_000_000)
                guard !Task.isCancelled, let self else { return }
                await self.refresh(root: self.personalDataRoot)
            }
        }
    }

    func object(_ type: String, _ id: String) -> MordePersonalSyncObject? { objects[type]?[id] }
    func object(_ type: String, _ id: String, scopeKey: String) -> MordePersonalSyncObject? {
        guard loadedScope == scopeKey else { return nil }
        return object(type, id)
    }
    func values(_ type: String) -> [MordePersonalSyncObject] { Array(objects[type, default: [:]].values) }

    func refresh() async { await refresh(root: personalDataRoot) }

    func refresh(root: URL) async {
        guard !isLoading else { refreshQueued = true; return }
        isLoading = true
        defer {
            isLoading = false
            if refreshQueued {
                refreshQueued = false
                Task { @MainActor [weak self] in await self?.refresh(root: root) }
            }
        }
        let auth = MordeShellAuthorization.shared
        let scope = auth.personalDataScopeKey
        if loadedScope != scope {
            loadedScope = scope
            changeSequence = nil
            objects = [:]
            pending = nil
            error = nil
        }
        guard let scope else { syncEnabled = true; return }
        syncEnabled = NanoMuseMordeProfileSyncPreference.isEnabled(scopeKey: scope)
        guard NanoMuseMordeProfileSyncPreference.canRead(scopeKey: scope) else { return }
        do {
            pending = try NanoMuseMordePersonalDataOutbox.load(scopeKey: scope, root: root)
            if syncEnabled, pending != nil {
                do { _ = try await flush(scope: scope, root: root) }
                catch is MordePersonalRevisionConflict { /* Keep reading the cloud snapshot while the user resolves it. */ }
            }
            if let changeSequence {
                try await refreshChanges(afterSequence: changeSequence, scope: scope, authorization: auth)
            } else {
                try await refreshSnapshot(scope: scope, authorization: auth)
            }
            guard auth.personalDataScopeKey == scope else { return }
            error = nil
        } catch {
            if auth.personalDataScopeKey == scope {
                self.error = "\(AppLocalized("Cloud personal data could not be refreshed. Local changes are preserved.")) \(error.localizedDescription)"
            }
        }
    }

    private func refreshSnapshot(scope: String, authorization: MordeShellAuthorization) async throws {
        var result: [String: [String: MordePersonalSyncObject]] = [:]
        var cursor: String?
        var watermark: Int64?
        var seen = Set<String>()
        repeat {
            let page = try await authorization.personalSnapshot(cursor: cursor)
            if let watermark, watermark != page.watermark { throw URLError(.cannotParseResponse) }
            watermark = page.watermark
            for item in page.items where MordePersonalDataContract.syncObjectTypes.contains(item.object_type) {
                result[item.object_type, default: [:]][item.object_id] = item
            }
            cursor = page.next_cursor
            if let cursor, !cursor.isEmpty, !seen.insert(cursor).inserted { throw URLError(.cannotParseResponse) }
        } while cursor?.isEmpty == false
        let baseSequence = watermark ?? 0
        let (reconciled, sequence) = try await loadChanges(afterSequence: baseSequence, scope: scope,
                                                            authorization: authorization, startingWith: result)
        guard authorization.personalDataScopeKey == scope else { return }
        objects = reconciled
        changeSequence = sequence
    }

    private func refreshChanges(afterSequence: Int64, scope: String,
                                 authorization: MordeShellAuthorization) async throws {
        let (updated, sequence) = try await loadChanges(afterSequence: afterSequence, scope: scope,
                                                        authorization: authorization, startingWith: objects)
        guard authorization.personalDataScopeKey == scope else { return }
        objects = updated
        changeSequence = sequence
    }

    private func loadChanges(afterSequence: Int64, scope: String, authorization: MordeShellAuthorization,
                             startingWith initialObjects: [String: [String: MordePersonalSyncObject]]) async throws
        -> ([String: [String: MordePersonalSyncObject]], Int64) {
        var sequence = afterSequence
        var throughSequence: Int64?
        var updated = initialObjects
        repeat {
            let page = try await authorization.personalChanges(afterSequence: sequence,
                                                               throughSequence: throughSequence)
            guard authorization.personalDataScopeKey == scope else { return (updated, sequence) }
            if let throughSequence, throughSequence != page.through_sequence {
                throw URLError(.cannotParseResponse)
            }
            throughSequence = page.through_sequence
            var latestByObject: [String: MordePersonalChange] = [:]
            for change in page.items {
                latestByObject["\(change.object_type)\u{0}\(change.object_id)"] = change
            }
            for change in latestByObject.values.sorted(by: { $0.change_sequence < $1.change_sequence }) {
                let item: MordePersonalSyncObject?
                if change.operation == "upsert" {
                    item = try await authorization.personalObject(objectType: change.object_type,
                                                                  objectID: change.object_id)
                } else {
                    item = nil
                }
                guard authorization.personalDataScopeKey == scope else { return (updated, sequence) }
                try Self.apply(change, item: item, to: &updated)
            }
            guard page.next_after_sequence >= sequence,
                  !page.has_more || page.next_after_sequence > sequence else {
                throw URLError(.cannotParseResponse)
            }
            sequence = page.next_after_sequence
            if !page.has_more { break }
        } while true
        return (updated, sequence)
    }

    static func apply(_ change: MordePersonalChange, item: MordePersonalSyncObject?,
                      to objects: inout [String: [String: MordePersonalSyncObject]]) throws {
        guard MordePersonalDataContract.syncObjectTypes.contains(change.object_type),
              MordePersonalDataContract.isValidObjectIdentifier(type: change.object_type, id: change.object_id),
              change.revision > 0, ["upsert", "delete"].contains(change.operation) else {
            throw URLError(.cannotParseResponse)
        }
        let current = objects[change.object_type]?[change.object_id]
        if change.operation == "delete" {
            if current.map({ $0.revision < change.revision }) ?? true {
                objects[change.object_type]?.removeValue(forKey: change.object_id)
            }
            return
        }
        // A 404 from the current-object read means a newer delete may have landed after this feed watermark.
        if item == nil {
            objects[change.object_type]?.removeValue(forKey: change.object_id)
            return
        }
        if let current, current.revision >= change.revision { return }
        guard let item, item.object_type == change.object_type, item.object_id == change.object_id,
              item.revision >= change.revision else { throw URLError(.cannotParseResponse) }
        objects[change.object_type, default: [:]][change.object_id] = item
    }

    @discardableResult
    func replace<T: Encodable>(_ type: String, id: String, revision: Int64, body: T, root: URL) async throws -> Int64? {
        guard let scope = MordeShellAuthorization.shared.personalDataScopeKey,
              MordePersonalDataContract.mutationObjectTypes.contains(type),
              MordePersonalDataContract.isValidObjectIdentifier(type: type, id: id), revision >= 0 else {
            throw URLError(.badURL)
        }
        let value = try JSONDecoder().decode(MordePersonalJSONValue.self, from: JSONEncoder().encode(body))
        guard case .object = value else { throw URLError(.cannotParseResponse) }
        let mutation = MordePersonalDataMutation(scopeKey: scope, mutationID: UUID().uuidString,
            objectType: type, objectID: id, baseRevision: revision, body: value, isDelete: false)
        do { try NanoMuseMordePersonalDataOutbox.enqueue(mutation, scopeKey: scope, root: root) }
        catch { self.error = error.localizedDescription; throw error }
        pending = mutation
        error = nil
        syncEnabled = NanoMuseMordeProfileSyncPreference.isEnabled(scopeKey: scope)
        guard syncEnabled, let receipt = try await flush(scope: scope, root: root) else { return nil }
        await refresh(root: root)
        return receipt.result_revision
    }

    @discardableResult
    func delete(_ type: String, id: String, revision: Int64, root: URL) async throws -> Bool {
        guard let scope = MordeShellAuthorization.shared.personalDataScopeKey,
              MordePersonalDataContract.mutationObjectTypes.contains(type),
              MordePersonalDataContract.isValidObjectIdentifier(type: type, id: id), revision > 0 else {
            throw URLError(.badURL)
        }
        let mutation = MordePersonalDataMutation(scopeKey: scope, mutationID: UUID().uuidString,
            objectType: type, objectID: id, baseRevision: revision, body: nil, isDelete: true)
        do { try NanoMuseMordePersonalDataOutbox.enqueue(mutation, scopeKey: scope, root: root) }
        catch { self.error = error.localizedDescription; throw error }
        pending = mutation
        error = nil
        syncEnabled = NanoMuseMordeProfileSyncPreference.isEnabled(scopeKey: scope)
        if !syncEnabled { return false }
        _ = try await flush(scope: scope, root: root)
        await refresh(root: root)
        return pending == nil
    }

    func retry(root: URL) async {
        if NanoMuseMordePersonalConflictResolution.shared.hasConflict {
            NanoMuseMordePersonalConflictResolution.shared.present()
            return
        }
        await refresh(root: root)
    }

    private func flush(scope: String, root: URL) async throws -> MordePersonalMutationReceipt? {
        guard !isSaving, MordeShellAuthorization.shared.personalDataScopeKey == scope,
              NanoMuseMordeProfileSyncPreference.isEnabled(scopeKey: scope),
              let mutation = try NanoMuseMordePersonalDataOutbox.load(scopeKey: scope, root: root) else { return nil }
        pending = mutation
        isSaving = true
        defer { isSaving = false }
        if NanoMuseMordePersonalConflictResolution.shared.isBlocked(mutation.mutationID) {
            NanoMuseMordePersonalConflictResolution.shared.present()
            return nil
        }
        do {
            let receipt: MordePersonalMutationReceipt
            if mutation.isDelete {
                receipt = try await MordeShellAuthorization.shared.deletePersonalObject(objectType: mutation.objectType,
                    objectID: mutation.objectID, baseRevision: mutation.baseRevision, idempotencyKey: mutation.mutationID)
            } else if let body = mutation.body {
                receipt = try await MordeShellAuthorization.shared.replacePersonalObject(objectType: mutation.objectType,
                    objectID: mutation.objectID, baseRevision: mutation.baseRevision, body: body, idempotencyKey: mutation.mutationID)
            } else { throw NanoMuseMordePersonalDataOutboxError.invalidMutation }
            guard MordeShellAuthorization.shared.personalDataScopeKey == scope else { return nil }
            try NanoMuseMordePersonalDataOutbox.acknowledge(scopeKey: scope, mutationID: mutation.mutationID, root: root)
            pending = nil
            error = nil
            return receipt
        } catch let conflict as MordePersonalRevisionConflict {
            NanoMuseMordePersonalConflictResolution.shared.record(mutation, conflict: conflict, root: root)
            if MordeShellAuthorization.shared.personalDataScopeKey == scope { error = conflict.localizedDescription }
            throw conflict
        } catch {
            if MordeShellAuthorization.shared.personalDataScopeKey == scope { self.error = error.localizedDescription }
            throw error
        }
    }
}

enum NanoMuseMordeConflictOutboxSource: Equatable, Sendable {
    case personalData
    case legacyProfile
}

struct NanoMuseMordePendingRevisionConflict: Equatable, Sendable {
    let mutation: MordePersonalDataMutation
    let conflict: MordePersonalRevisionConflict
    let source: NanoMuseMordeConflictOutboxSource
}

struct NanoMuseMordePersonalConflictResolved: Equatable, Sendable {
    let scopeKey: String
    let source: NanoMuseMordeConflictOutboxSource
}

extension Notification.Name {
    static let mordePersonalConflictResolved = Notification.Name("morde.personalConflictResolved")
}

@MainActor
final class NanoMuseMordePersonalConflictResolution: ObservableObject {
    static let shared = NanoMuseMordePersonalConflictResolution()

    @Published private(set) var pending: NanoMuseMordePendingRevisionConflict?
    @Published private(set) var isPromptPresented = false
    @Published private(set) var isResolving = false
    @Published private(set) var actionError: String?

    private var pendingRoot: URL?

    var currentPending: NanoMuseMordePendingRevisionConflict? {
        guard let pending, pending.mutation.scopeKey == MordeShellAuthorization.shared.personalDataScopeKey else { return nil }
        return pending
    }

    var hasConflict: Bool { currentPending != nil }

    private init() {}

    func isBlocked(_ mutationID: String) -> Bool { currentPending?.mutation.mutationID == mutationID }

    func scopeChanged(to scope: String?) {
        guard pending?.mutation.scopeKey != scope else { return }
        pending = nil
        pendingRoot = nil
        isPromptPresented = false
        actionError = nil
    }

    func record(_ mutation: MordePersonalDataMutation, conflict: MordePersonalRevisionConflict,
                source: NanoMuseMordeConflictOutboxSource = .personalData, root: URL) {
        guard mutation.scopeKey == MordeShellAuthorization.shared.personalDataScopeKey,
              conflict.baseRevision == mutation.baseRevision else { return }
        let currentMutationID: String?
        switch source {
        case .personalData:
            currentMutationID = (try? NanoMuseMordePersonalDataOutbox.load(scopeKey: mutation.scopeKey, root: root))?.mutationID
        case .legacyProfile:
            currentMutationID = (try? NanoMuseMordeProfileOutbox.load(scopeKey: mutation.scopeKey, root: root))?.mutationID
        }
        guard currentMutationID == mutation.mutationID else { return }
        pending = NanoMuseMordePendingRevisionConflict(mutation: mutation, conflict: conflict, source: source)
        pendingRoot = root
        actionError = nil
        isPromptPresented = true
    }

    func present() {
        guard pending != nil else { return }
        isPromptPresented = true
    }

    func dismissPrompt() { isPromptPresented = false }

    func applyLocalChange() async {
        guard let pending, !pending.conflict.deleted, !isResolving,
              pending.mutation.scopeKey == MordeShellAuthorization.shared.personalDataScopeKey,
              let root = pendingRoot else { return }
        isResolving = true
        isPromptPresented = false
        defer { isResolving = false }
        do {
            let rebased: Bool
            switch pending.source {
            case .personalData:
                rebased = try NanoMuseMordePersonalDataOutbox.rebase(scopeKey: pending.mutation.scopeKey,
                    mutationID: pending.mutation.mutationID, conflict: pending.conflict, root: root) != nil
            case .legacyProfile:
                rebased = try NanoMuseMordeProfileOutbox.rebase(scopeKey: pending.mutation.scopeKey,
                    mutationID: pending.mutation.mutationID, conflict: pending.conflict, root: root) != nil
            }
            guard rebased else {
                actionError = AppLocalized("This conflict is no longer current. Refresh before resolving it.")
                isPromptPresented = true
                return
            }
            self.pending = nil
            pendingRoot = nil
            actionError = nil
            postResolution(pending)
        } catch {
            actionError = error.localizedDescription
            isPromptPresented = true
        }
    }

    func discardLocalChange() async {
        guard let pending, !isResolving,
              pending.mutation.scopeKey == MordeShellAuthorization.shared.personalDataScopeKey,
              let root = pendingRoot else { return }
        isResolving = true
        isPromptPresented = false
        defer { isResolving = false }
        do {
            let discarded: Bool
            switch pending.source {
            case .personalData:
                discarded = try NanoMuseMordePersonalDataOutbox.discard(scopeKey: pending.mutation.scopeKey,
                                                                         mutationID: pending.mutation.mutationID, root: root)
            case .legacyProfile:
                discarded = try NanoMuseMordeProfileOutbox.discard(scopeKey: pending.mutation.scopeKey,
                                                                    mutationID: pending.mutation.mutationID, root: root)
            }
            guard discarded else {
                actionError = AppLocalized("This conflict is no longer current. Refresh before resolving it.")
                isPromptPresented = true
                return
            }
            self.pending = nil
            pendingRoot = nil
            actionError = nil
            postResolution(pending)
        } catch {
            actionError = error.localizedDescription
            isPromptPresented = true
        }
    }

    private func postResolution(_ pending: NanoMuseMordePendingRevisionConflict) {
        NotificationCenter.default.post(name: .mordePersonalConflictResolved,
            object: NanoMuseMordePersonalConflictResolved(scopeKey: pending.mutation.scopeKey,
                                                         source: pending.source))
    }
}

struct NanoMuseMordeLibraryItemBody: Codable, Equatable {
    let name: String
    let kind: String
    var mime: String?
    var size_bytes: Int64?
    var attachment_id: String?

    var hasAttachment: Bool { attachment_id != nil }

    var isValid: Bool {
        !name.isEmpty && name.unicodeScalars.count <= 255 && !name.contains("/") && !name.contains("\\") &&
            !name.unicodeScalars.contains(where: CharacterSet.controlCharacters.contains) &&
            (kind == "artifact" || kind == "media") && (mime?.unicodeScalars.count ?? 0) <= 128 &&
            (size_bytes.map { $0 >= 0 } ?? true) &&
            (attachment_id.map(MordePersonalDataContract.isValidAttachmentID) ?? true)
    }
}

extension NanoMuseMordeLibraryItemBody {
    private enum CodingKeys: String, CodingKey { case name, kind, mime, size_bytes, attachment_id }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        name = try container.decode(String.self, forKey: .name)
        kind = try container.decode(String.self, forKey: .kind)
        mime = try container.decodeIfPresent(String.self, forKey: .mime)
        size_bytes = try container.decodeIfPresent(Int64.self, forKey: .size_bytes)
        let value = try container.decodeIfPresent(String.self, forKey: .attachment_id)
        attachment_id = value?.isEmpty == false ? value : nil
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(name, forKey: .name)
        try container.encode(kind, forKey: .kind)
        try container.encodeIfPresent(mime, forKey: .mime)
        try container.encodeIfPresent(size_bytes, forKey: .size_bytes)
        try container.encode(attachment_id ?? "", forKey: .attachment_id)
    }
}

struct NanoMuseMordeLibraryRecord: Identifiable, Equatable {
    let id: String
    let revision: Int64
    let body: NanoMuseMordeLibraryItemBody
}

struct MordePersonalHistoryImportMessage: Codable, Equatable, Sendable {
    let source_message_id: String
    let role: String
    let content: String
}

struct MordePersonalHistoryImportRequest: Encodable, Equatable, Sendable {
    let source_id: String
    let device_account_id: String
    let source_conversation_id: String
    let target_conversation_id: String?
    let title: String
    let messages: [MordePersonalHistoryImportMessage]
}

struct MordePersonalHistoryImportReceiptMessage: Decodable, Equatable, Sendable {
    let source_message_id: String
    let message_id: String
    let conversation_id: String
    let role: String
    let content: String
    let sequence: Int64
    let attachment_ids: [String]?
    let replay: Bool
}

struct MordePersonalHistoryImportReceipt: Decodable, Equatable, Sendable {
    let conversation_id: String
    let title: String
    let replay: Bool
    let messages: [MordePersonalHistoryImportReceiptMessage]
}

enum MordePersonalHistoryImportContract {
    static let sourceID = "nanomuse.local-chatstore.v1"

    static func isValidSourceIdentifier(_ value: String) -> Bool {
        !value.isEmpty && value.utf8.count <= 128 &&
        !value.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty &&
        value.unicodeScalars.allSatisfy { $0.value >= 0x20 && $0.value != 0x7f }
    }

    static func isValid(_ request: MordePersonalHistoryImportRequest) -> Bool {
        guard isValidSourceIdentifier(request.source_id), isValidSourceIdentifier(request.device_account_id),
              isValidSourceIdentifier(request.source_conversation_id),
              request.target_conversation_id.map(isValidSourceIdentifier) ?? true,
              request.title.unicodeScalars.count <= 120,
              !request.messages.isEmpty, request.messages.count <= 200 else { return false }
        var seen = Set<String>()
        var totalBytes = 0
        for message in request.messages {
            guard isValidSourceIdentifier(message.source_message_id), seen.insert(message.source_message_id).inserted,
                  ["user", "assistant"].contains(message.role), !message.content.isEmpty,
                  !message.content.unicodeScalars.contains(where: { $0.value == 0 }) else { return false }
            let size = message.content.utf8.count
            guard size <= 32 * 1024, totalBytes <= 1_048_576 - size else { return false }
            totalBytes += size
        }
        return true
    }

    static func isValid(_ receipt: MordePersonalHistoryImportReceipt,
                        for request: MordePersonalHistoryImportRequest) -> Bool {
        guard isValidSourceIdentifier(receipt.conversation_id),
              receipt.title.unicodeScalars.count <= 120,
              receipt.messages.count == request.messages.count else { return false }
        return zip(receipt.messages, request.messages).allSatisfy { imported, source in
            imported.source_message_id == source.source_message_id &&
            imported.conversation_id == receipt.conversation_id &&
            imported.message_id.range(of: "^[A-Za-z0-9._~-]{1,128}$", options: .regularExpression) != nil &&
            imported.role == source.role && imported.content == source.content && imported.sequence > 0 &&
            imported.attachment_ids?.isEmpty != false
        }
    }
}
