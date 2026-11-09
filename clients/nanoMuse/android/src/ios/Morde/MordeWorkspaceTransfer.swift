import CryptoKit
import Darwin
import Foundation

struct MordeWorkspaceChunk: Codable, Hashable, Sendable {
    let sha256: String
    let sizeBytes: Int64

    enum CodingKeys: String, CodingKey {
        case sha256
        case sizeBytes = "size_bytes"
    }
}

struct MordeWorkspaceFile: Codable, Hashable, Identifiable, Sendable {
    let path: String
    let sizeBytes: Int64
    let sha256: String
    let mode: Int
    let chunks: [MordeWorkspaceChunk]

    var id: String { path }

    enum CodingKeys: String, CodingKey {
        case path
        case sizeBytes = "size_bytes"
        case sha256
        case mode
        case chunks
    }
}

enum MordeWorkspaceTransferError: LocalizedError {
    case invalidPath
    case pathCollision
    case notRegularFile
    case fileTooLarge
    case invalidManifest
    case changedFile
    case targetExists

    var errorDescription: String? {
        switch self {
        case .invalidPath: return "文件路径必须是规范的相对 POSIX 路径。"
        case .pathCollision: return "文件路径与另一个文件存在大小写冲突。"
        case .notRegularFile: return "只支持普通文件，不支持符号链接或目录。"
        case .fileTooLarge: return "单个文件不能超过 300 MiB。"
        case .invalidManifest: return "文件清单或分块摘要无效。"
        case .changedFile: return "文件在确认后发生了变化，请重新选择。"
        case .targetExists: return "目标文件已存在，没有覆盖它。"
        }
    }
}

enum MordeWorkspaceTransferSupport {
    static let chunkByteLimit = 4 * 1024 * 1024
    static let fileByteLimit: Int64 = 300 * 1024 * 1024

    static func idempotencyKey(_ components: [String]) -> String {
        let data = Data(components.joined(separator: "\u{0}").utf8)
        return SHA256.hash(data: data).prefix(16).map { String(format: "%02x", $0) }.joined()
    }

    static func validatePath(_ path: String) throws {
        let bytes = Array(path.utf8)
        guard !bytes.isEmpty, bytes.count <= 4096,
              !path.hasPrefix("/"), !path.contains("\\"), !bytes.contains(0),
              path.split(separator: "/", omittingEmptySubsequences: false).allSatisfy({
                  !$0.isEmpty && $0 != "." && $0 != ".."
              }) else {
            throw MordeWorkspaceTransferError.invalidPath
        }
    }

    static func normalizedManifestPrefix(_ prefix: String) throws -> String? {
        guard !prefix.isEmpty else { return nil }
        guard prefix.utf8.count <= 1024 else { throw MordeWorkspaceTransferError.invalidPath }
        let normalized = prefix.hasSuffix("/") ? String(prefix.dropLast()) : prefix
        try validatePath(normalized)
        return normalized
    }

    static func fileOperationTargetPath(action: String, sourcePath: String, input: String) throws -> String {
        try validatePath(sourcePath)
        let target: String
        switch action {
        case "rename":
            guard !input.contains("/") else { throw MordeWorkspaceTransferError.invalidPath }
            let parent = sourcePath.lastIndex(of: "/").map { String(sourcePath[..<$0]) } ?? ""
            target = parent.isEmpty ? input : "\(parent)/\(input)"
        case "move":
            target = input
        default:
            throw MordeWorkspaceTransferError.invalidPath
        }
        try validatePath(target)
        guard target != sourcePath else { throw MordeWorkspaceTransferError.invalidPath }
        return target
    }

    static func hasCaseInsensitiveUniquePaths<S: Sequence>(_ paths: S) -> Bool where S.Element == String {
        var seen = Set<String>()
        for path in paths {
            guard (try? validatePath(path)) != nil else { return false }
            let key = normalizedManifestPathKey(path)
            guard seen.insert(key).inserted else { return false }
        }
        return true
    }

    static func hasValidManifestPaths(filePaths: [String], directoryPaths: [String]) -> Bool {
        let directories = directoryPaths.filter { $0 != "." }
        let paths = filePaths + directories
        guard hasCaseInsensitiveUniquePaths(paths) else { return false }
        let fileKeys = Set(filePaths.map(normalizedManifestPathKey))
        for path in paths.map(normalizedManifestPathKey) {
            let components = path.split(separator: "/")
            for ancestorCount in 1..<components.count {
                let ancestor = components.prefix(ancestorCount).joined(separator: "/")
                if fileKeys.contains(ancestor) { return false }
            }
        }
        return true
    }

    private static func normalizedManifestPathKey(_ path: String) -> String {
        path.folding(options: [.caseInsensitive], locale: Locale(identifier: "en_US_POSIX"))
    }

    static func scan(url: URL, path: String) throws -> MordeWorkspaceFile {
        try validatePath(path)
        let (handle, size, mode) = try openRegularFile(url)
        defer { try? handle.close() }
        guard size <= fileByteLimit else { throw MordeWorkspaceTransferError.fileTooLarge }

        var chunks: [MordeWorkspaceChunk] = []
        var fileHasher = SHA256()
        var total: Int64 = 0
        while true {
            let data = try readUpTo(chunkByteLimit, from: handle)
            if data.isEmpty { break }
            total += Int64(data.count)
            guard total <= fileByteLimit else { throw MordeWorkspaceTransferError.fileTooLarge }
            fileHasher.update(data: data)
            chunks.append(MordeWorkspaceChunk(sha256: digest(data), sizeBytes: Int64(data.count)))
        }
        guard total == size else { throw MordeWorkspaceTransferError.changedFile }
        return MordeWorkspaceFile(path: path, sizeBytes: total, sha256: hex(fileHasher.finalize()),
                                  mode: mode & 0o777, chunks: chunks)
    }

    static func readChunk(url: URL, index: Int, expected: MordeWorkspaceChunk) throws -> Data {
        guard index >= 0, isValidDigest(expected.sha256),
              Int64(index) <= Int64.max / Int64(chunkByteLimit),
              expected.sizeBytes > 0, expected.sizeBytes <= Int64(chunkByteLimit) else {
            throw MordeWorkspaceTransferError.invalidManifest
        }
        let (handle, size, _) = try openRegularFile(url)
        defer { try? handle.close() }
        guard size <= fileByteLimit else { throw MordeWorkspaceTransferError.fileTooLarge }
        let offset = Int64(index) * Int64(chunkByteLimit)
        guard offset <= size, expected.sizeBytes <= size - offset else {
            throw MordeWorkspaceTransferError.changedFile
        }
        try handle.seek(toOffset: UInt64(offset))
        let data = try readExactly(Int(expected.sizeBytes), from: handle)
        guard Int64(data.count) == expected.sizeBytes, digest(data) == expected.sha256 else {
            throw MordeWorkspaceTransferError.changedFile
        }
        return data
    }

    static func isValidDigest(_ value: String) -> Bool {
        value.utf8.count == 64 && value.utf8.allSatisfy { ($0 >= 48 && $0 <= 57) || ($0 >= 97 && $0 <= 102) }
    }

    fileprivate static func digest(_ data: Data) -> String {
        hex(SHA256.hash(data: data))
    }

    fileprivate static func hex<D: Sequence>(_ digest: D) -> String where D.Element == UInt8 {
        digest.map { String(format: "%02x", $0) }.joined()
    }

    fileprivate static func readUpTo(_ count: Int, from handle: FileHandle) throws -> Data {
        var data = Data()
        while data.count < count {
            guard let next = try handle.read(upToCount: min(64 * 1024, count - data.count)), !next.isEmpty else { break }
            data.append(next)
        }
        return data
    }

    fileprivate static func readExactly(_ count: Int, from handle: FileHandle) throws -> Data {
        let data = try readUpTo(count, from: handle)
        guard data.count == count else { throw MordeWorkspaceTransferError.changedFile }
        return data
    }

    fileprivate static func openRegularFile(_ url: URL) throws -> (FileHandle, Int64, Int) {
        let descriptor = Darwin.open(url.path, O_RDONLY | O_NOFOLLOW | O_CLOEXEC | O_NONBLOCK)
        guard descriptor >= 0 else { throw MordeWorkspaceTransferError.notRegularFile }
        var info = stat()
        guard Darwin.fstat(descriptor, &info) == 0,
              (info.st_mode & mode_t(S_IFMT)) == mode_t(S_IFREG), info.st_size >= 0 else {
            Darwin.close(descriptor)
            throw MordeWorkspaceTransferError.notRegularFile
        }
        return (FileHandle(fileDescriptor: descriptor, closeOnDealloc: true),
                Int64(info.st_size), Int(info.st_mode & mode_t(0o777)))
    }

    static func pathEntryExists(_ url: URL) -> Bool {
        var info = stat()
        return Darwin.lstat(url.path, &info) == 0
    }

    static func isDirectoryWithoutFollowingSymlinks(_ url: URL) -> Bool {
        var info = stat()
        return Darwin.lstat(url.path, &info) == 0 &&
            (info.st_mode & mode_t(S_IFMT)) == mode_t(S_IFDIR)
    }

    static func isRegularFileWithoutFollowingSymlinks(_ url: URL) -> Bool {
        var info = stat()
        return Darwin.lstat(url.path, &info) == 0 &&
            (info.st_mode & mode_t(S_IFMT)) == mode_t(S_IFREG)
    }

    static func removeRegularFileWithoutFollowingSymlinks(_ url: URL) throws {
        guard url.isFileURL, url.host == nil, url.query == nil, url.fragment == nil,
              url.standardizedFileURL.path == url.path, !url.path.hasSuffix("/") else {
            throw MordeWorkspaceTransferError.invalidPath
        }
        let parentDescriptor = try MordeWorkspaceDirectoryAccess.open(url.deletingLastPathComponent())
        defer { Darwin.close(parentDescriptor) }
        let name = url.lastPathComponent
        guard !name.isEmpty, name != ".", name != ".." else { throw MordeWorkspaceTransferError.invalidPath }
        var info = stat()
        let status = name.withCString { Darwin.fstatat(parentDescriptor, $0, &info, AT_SYMLINK_NOFOLLOW) }
        if status != 0 {
            if errno == ENOENT { return }
            throw NSError(domain: NSPOSIXErrorDomain, code: Int(errno))
        }
        guard (info.st_mode & mode_t(S_IFMT)) == mode_t(S_IFREG) else {
            throw MordeWorkspaceTransferError.notRegularFile
        }
        let removed = name.withCString { Darwin.unlinkat(parentDescriptor, $0, 0) }
        if removed != 0 && errno != ENOENT {
            throw NSError(domain: NSPOSIXErrorDomain, code: Int(errno))
        }
    }
}

final class MordeWorkspaceDownloadWriter {
    private let file: MordeWorkspaceFile
    private let temporary: URL
    private let temporaryName: String
    private let destinationName: String
    private let parentDescriptor: Int32
    private let temporaryIdentity: MordeWorkspaceTemporaryFileIdentity
    private let handle: FileHandle
    private var hasher = SHA256()
    private var nextChunkIndex = 0
    private var totalBytes: Int64 = 0
    private var finished = false

    init(file: MordeWorkspaceFile, to destination: URL) throws {
        MordeWorkspacePartialFileCleanup.shared.retryPending()
        try MordeWorkspaceTransferSupport.validatePath(file.path)
        guard file.sizeBytes >= 0, file.sizeBytes <= MordeWorkspaceTransferSupport.fileByteLimit,
              (0...0o777).contains(file.mode), MordeWorkspaceTransferSupport.isValidDigest(file.sha256),
              (file.sizeBytes != 0 || file.chunks.isEmpty) else {
            throw MordeWorkspaceTransferError.invalidManifest
        }
        let expectedChunkCount = file.sizeBytes == 0 ? 0 :
            Int((file.sizeBytes - 1) / Int64(MordeWorkspaceTransferSupport.chunkByteLimit)) + 1
        guard file.chunks.count == expectedChunkCount,
              file.chunks.allSatisfy({ $0.sizeBytes > 0 && $0.sizeBytes <= Int64(MordeWorkspaceTransferSupport.chunkByteLimit) &&
                  MordeWorkspaceTransferSupport.isValidDigest($0.sha256) }) else {
            throw MordeWorkspaceTransferError.invalidManifest
        }
        var remainingBytes = file.sizeBytes
        for chunk in file.chunks {
            let expectedSize = min(Int64(MordeWorkspaceTransferSupport.chunkByteLimit), remainingBytes)
            guard chunk.sizeBytes == expectedSize else {
                throw MordeWorkspaceTransferError.invalidManifest
            }
            remainingBytes -= chunk.sizeBytes
        }
        guard remainingBytes == 0 else { throw MordeWorkspaceTransferError.invalidManifest }

        guard destination.isFileURL, destination.host == nil,
              destination.query == nil, destination.fragment == nil,
              destination.path.hasPrefix("/"),
              !destination.path.hasSuffix("/"),
              destination.standardizedFileURL.path == destination.path,
              destination.lastPathComponent != ".", destination.lastPathComponent != ".." else {
            throw MordeWorkspaceTransferError.invalidPath
        }
        let parent = destination.deletingLastPathComponent()
        let parentDescriptor = try MordeWorkspaceDirectoryAccess.open(parent)
        let destinationName = destination.lastPathComponent
        var existingInfo = stat()
        let existingResult = destinationName.withCString {
            Darwin.fstatat(parentDescriptor, $0, &existingInfo, AT_SYMLINK_NOFOLLOW)
        }
        if existingResult == 0 {
            Darwin.close(parentDescriptor)
            throw MordeWorkspaceTransferError.targetExists
        }
        let existingError = errno
        guard existingError == ENOENT else {
            Darwin.close(parentDescriptor)
            throw NSError(domain: NSPOSIXErrorDomain, code: Int(existingError))
        }

        self.file = file
        self.parentDescriptor = parentDescriptor
        self.destinationName = destinationName
        let temporaryName = ".morde-\(UUID().uuidString).tmp"
        self.temporaryName = temporaryName
        self.temporary = parent.appendingPathComponent(temporaryName)
        let descriptor = temporaryName.withCString {
            Darwin.openat(parentDescriptor, $0,
                          O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC,
                          mode_t(0o600))
        }
        guard descriptor >= 0 else {
            let errorCode = errno
            Darwin.close(parentDescriptor)
            throw NSError(domain: NSPOSIXErrorDomain, code: Int(errorCode))
        }
        var temporaryInfo = stat()
        guard Darwin.fstat(descriptor, &temporaryInfo) == 0 else {
            let errorCode = errno
            Darwin.close(descriptor)
            _ = temporaryName.withCString { Darwin.unlinkat(parentDescriptor, $0, 0) }
            Darwin.close(parentDescriptor)
            throw NSError(domain: NSPOSIXErrorDomain, code: Int(errorCode))
        }
        guard (temporaryInfo.st_mode & mode_t(S_IFMT)) == mode_t(S_IFREG) else {
            Darwin.close(descriptor)
            _ = temporaryName.withCString { Darwin.unlinkat(parentDescriptor, $0, 0) }
            Darwin.close(parentDescriptor)
            throw MordeWorkspaceTransferError.notRegularFile
        }
        self.temporaryIdentity = MordeWorkspaceTemporaryFileIdentity(
            device: temporaryInfo.st_dev, inode: temporaryInfo.st_ino)
        self.handle = FileHandle(fileDescriptor: descriptor, closeOnDealloc: true)
    }

    func append(_ data: Data, expected chunk: MordeWorkspaceChunk) throws {
        guard !finished, nextChunkIndex < file.chunks.count,
              file.chunks[nextChunkIndex] == chunk,
              Int64(data.count) == chunk.sizeBytes,
              MordeWorkspaceTransferSupport.digest(data) == chunk.sha256 else {
            throw MordeWorkspaceTransferError.changedFile
        }
        try handle.write(contentsOf: data)
        hasher.update(data: data)
        totalBytes += Int64(data.count)
        nextChunkIndex += 1
    }

    func finish() throws {
        guard !finished, nextChunkIndex == file.chunks.count, totalBytes == file.sizeBytes,
              MordeWorkspaceTransferSupport.hex(hasher.finalize()) == file.sha256 else {
            throw MordeWorkspaceTransferError.changedFile
        }
        guard Darwin.fchmod(handle.fileDescriptor, mode_t(file.mode)) == 0 else {
            throw NSError(domain: NSPOSIXErrorDomain, code: Int(errno))
        }
        try handle.synchronize()
        try handle.close()
        var temporaryInfo = stat()
        let temporaryResult = temporaryName.withCString {
            Darwin.fstatat(parentDescriptor, $0, &temporaryInfo, AT_SYMLINK_NOFOLLOW)
        }
        guard temporaryResult == 0,
              (temporaryInfo.st_mode & mode_t(S_IFMT)) == mode_t(S_IFREG),
              MordeWorkspaceTemporaryFileIdentity(device: temporaryInfo.st_dev,
                                                  inode: temporaryInfo.st_ino) == temporaryIdentity else {
            throw MordeWorkspaceTransferError.notRegularFile
        }
        guard temporaryName.withCString({ sourceName in
            destinationName.withCString { destinationName in
                Darwin.renameatx_np(parentDescriptor, sourceName,
                                    parentDescriptor, destinationName, UInt32(RENAME_EXCL))
            }
        }) == 0 else {
            let errorCode = errno
            var existingInfo = stat()
            let destinationExists = destinationName.withCString {
                Darwin.fstatat(parentDescriptor, $0, &existingInfo, AT_SYMLINK_NOFOLLOW)
            } == 0
            if destinationExists {
                throw MordeWorkspaceTransferError.targetExists
            }
            throw NSError(domain: NSPOSIXErrorDomain, code: Int(errorCode))
        }
        finished = true
        Darwin.close(parentDescriptor)
    }

    func cancel() {
        guard !finished else { return }
        try? handle.close()
        MordeWorkspacePartialFileCleanup.shared.discard(
            temporary, identity: temporaryIdentity, parentDescriptor: parentDescriptor)
        finished = true
        Darwin.close(parentDescriptor)
    }

    deinit { cancel() }
}

private struct MordeWorkspaceTemporaryFileIdentity: Hashable, Sendable {
    let device: dev_t
    let inode: ino_t
}

private enum MordeWorkspaceDirectoryAccess {
    static func open(_ url: URL) throws -> Int32 {
        guard url.isFileURL, url.host == nil,
              url.query == nil, url.fragment == nil,
              url.path.hasPrefix("/"),
              url.standardizedFileURL.path == url.path else {
            throw MordeWorkspaceTransferError.invalidPath
        }
        // shortcut: normalize Darwin's /var alias only; add another root alias if file providers need it.
        let path = url.path == "/var" || url.path.hasPrefix("/var/") ? "/private" + url.path : url.path
        var descriptor = Darwin.open("/", O_RDONLY | O_DIRECTORY | O_CLOEXEC)
        guard descriptor >= 0 else {
            throw NSError(domain: NSPOSIXErrorDomain, code: Int(errno))
        }
        for component in path.split(separator: "/", omittingEmptySubsequences: true) {
            guard component != ".", component != ".." else {
                Darwin.close(descriptor)
                throw MordeWorkspaceTransferError.invalidPath
            }
            let componentName = String(component)
            let nextDescriptor = componentName.withCString {
                Darwin.openat(descriptor, $0, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
            }
            guard nextDescriptor >= 0 else {
                let errorCode = errno
                Darwin.close(descriptor)
                if errorCode == ELOOP || errorCode == ENOTDIR {
                    throw MordeWorkspaceTransferError.invalidPath
                }
                throw NSError(domain: NSPOSIXErrorDomain, code: Int(errorCode))
            }
            Darwin.close(descriptor)
            descriptor = nextDescriptor
        }
        return descriptor
    }
}

private final class MordeWorkspacePartialFileCleanup: @unchecked Sendable {
    static let shared = MordeWorkspacePartialFileCleanup()

    private let lock = NSLock()
    private var pending: [URL: MordeWorkspaceTemporaryFileIdentity] = [:]

    func discard(_ url: URL, identity: MordeWorkspaceTemporaryFileIdentity,
                 parentDescriptor: Int32) {
        lock.lock()
        defer { lock.unlock() }
        retryPendingLocked()
        removeLocked(url, expectedIdentity: identity, parentDescriptor: parentDescriptor)
    }

    func retryPending() {
        lock.lock()
        defer { lock.unlock() }
        retryPendingLocked()
    }

    private func retryPendingLocked() {
        for (url, identity) in Array(pending) {
            removeLocked(url, expectedIdentity: identity)
        }
    }

    private func removeLocked(_ url: URL, expectedIdentity: MordeWorkspaceTemporaryFileIdentity,
                              parentDescriptor suppliedDescriptor: Int32? = nil) {
        guard Self.isPartialFile(url) else {
            pending.removeValue(forKey: url)
            return
        }
        let parentDescriptor: Int32
        if let suppliedDescriptor {
            parentDescriptor = suppliedDescriptor
        } else if let openedDescriptor = try? MordeWorkspaceDirectoryAccess.open(url.deletingLastPathComponent()) {
            parentDescriptor = openedDescriptor
        } else {
            pending[url] = expectedIdentity
            return
        }
        let shouldCloseDescriptor = suppliedDescriptor == nil
        defer {
            if shouldCloseDescriptor { Darwin.close(parentDescriptor) }
        }
        let name = url.lastPathComponent

        var info = stat()
        let result = name.withCString {
            Darwin.fstatat(parentDescriptor, $0, &info, AT_SYMLINK_NOFOLLOW)
        }
        let error = errno
        guard result == 0 else {
            if error == ENOENT || error == ENOTDIR { pending.removeValue(forKey: url) }
            else { pending[url] = expectedIdentity }
            return
        }
        guard (info.st_mode & mode_t(S_IFMT)) == mode_t(S_IFREG) else {
            pending.removeValue(forKey: url)
            return
        }
        let currentIdentity = MordeWorkspaceTemporaryFileIdentity(device: info.st_dev, inode: info.st_ino)
        guard currentIdentity == expectedIdentity else {
            pending.removeValue(forKey: url)
            return
        }

        if name.withCString({ Darwin.unlinkat(parentDescriptor, $0, 0) }) == 0 {
            pending.removeValue(forKey: url)
        } else {
            let unlinkError = errno
            if unlinkError == ENOENT || unlinkError == ENOTDIR { pending.removeValue(forKey: url) }
            else { pending[url] = expectedIdentity }
        }
    }

    private static func isPartialFile(_ url: URL) -> Bool {
        guard url.isFileURL else { return false }
        let name = url.lastPathComponent
        let prefix = ".morde-"
        let suffix = ".tmp"
        guard name.hasPrefix(prefix), name.hasSuffix(suffix) else { return false }
        let identifier = name.dropFirst(prefix.count).dropLast(suffix.count)
        return UUID(uuidString: String(identifier)) != nil
    }
}
