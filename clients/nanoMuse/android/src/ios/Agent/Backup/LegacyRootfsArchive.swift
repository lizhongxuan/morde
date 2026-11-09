import Foundation
import Darwin

/// Preserves user-owned files before an iSH rootfs is replaced.
/// This helper only copies data; it never installs, resets, or removes a rootfs.
enum LegacyRootfsArchive {
    static let archiveDirectoryName = "MinisLegacyRootfsArchive"

    static func isContainedAfterResolvingSymlinks(_ candidate: URL, under root: URL) -> Bool {
        let resolvedRoot = root.standardizedFileURL.resolvingSymlinksInPath().path
        let resolvedCandidate = candidate.standardizedFileURL.resolvingSymlinksInPath().path
        let rootPrefix = resolvedRoot.hasSuffix("/") ? resolvedRoot : resolvedRoot + "/"
        return resolvedCandidate == resolvedRoot || resolvedCandidate.hasPrefix(rootPrefix)
    }

    struct CopyReport: Equatable, Sendable {
        let sourceRootsFound: Int
        let sourceRootsMissing: [String]
        let filesCopied: Int
        let identicalFilesAlreadyPresent: Int
    }

    enum ArchiveError: Error, Equatable, CustomStringConvertible, Sendable {
        case documentsDirectoryIsNotAFileURL
        case documentsDirectoryMissing
        case symbolicLink(String)
        case pathEscapesRoot(String)
        case expectedDirectory(String)
        case unsupportedFileType(String)
        case destinationConflict(String)
        case filesystem(String, Int32)
        case copiedFileDidNotMatchSource(String)

        var description: String {
            switch self {
            case .documentsDirectoryIsNotAFileURL:
                return "Documents URL is not a file URL"
            case .documentsDirectoryMissing:
                return "Documents directory is missing or is not a directory"
            case .symbolicLink(let path):
                return "Refusing symbolic link: \(path)"
            case .pathEscapesRoot(let path):
                return "Path escapes its archive root: \(path)"
            case .expectedDirectory(let path):
                return "Expected a directory: \(path)"
            case .unsupportedFileType(let path):
                return "Unsupported filesystem entry: \(path)"
            case .destinationConflict(let path):
                return "Destination already contains different data: \(path)"
            case .filesystem(let path, let code):
                return "Filesystem operation failed at \(path) (errno \(code))"
            case .copiedFileDidNotMatchSource(let path):
                return "Copied file could not be verified: \(path)"
            }
        }
    }

    private struct Mapping {
        let sourceComponents: [String]
        let archiveComponents: [String]
        let reportName: String
    }

    private enum EntryKind: Equatable {
        case directory
        case regularFile
    }

    private struct Counters {
        var sourceRootsFound = 0
        var sourceRootsMissing: [String] = []
        var filesCopied = 0
        var identicalFilesAlreadyPresent = 0
    }

    private static let mappings = [
        Mapping(
            sourceComponents: ["alpine-rootfs", "data", "root"],
            archiveComponents: ["root"],
            reportName: "data/root"
        ),
        Mapping(
            sourceComponents: ["alpine-rootfs", "data", "var", "minis"],
            archiveComponents: ["var", "minis"],
            reportName: "data/var/minis"
        ),
    ]

    /// Copies `/root` and `/var/minis` into `Documents/MinisLegacyRootfsArchive`.
    /// Existing identical files are accepted on repeat runs. Different destination
    /// files and symbolic links fail closed; a thrown error means the archive may
    /// contain completed additions, but the caller must not treat the operation as
    /// complete. A later run can safely retry nonconflicting additions because
    /// source and destination files are never overwritten or removed.
    static func preserveUserData(
        in documentsDirectory: URL,
        fileManager: FileManager = .default
    ) throws -> CopyReport {
        guard documentsDirectory.isFileURL else {
            throw ArchiveError.documentsDirectoryIsNotAFileURL
        }
        let documents = documentsDirectory.standardizedFileURL
        guard try entryKind(at: documents) == .directory else {
            throw ArchiveError.documentsDirectoryMissing
        }

        var counters = Counters()
        let archiveRoot = documents.appendingPathComponent(archiveDirectoryName, isDirectory: true)
        var availableSources: [(Mapping, URL)] = []

        for mapping in mappings {
            guard let sourceRoot = try existingDirectory(
                under: documents,
                components: mapping.sourceComponents
            ) else {
                counters.sourceRootsMissing.append(mapping.reportName)
                continue
            }
            availableSources.append((mapping, sourceRoot))
        }

        for (mapping, sourceRoot) in availableSources {

            let destinationRoot = append(mapping.archiveComponents, to: archiveRoot)
            guard isContained(destinationRoot, in: archiveRoot) else {
                throw ArchiveError.pathEscapesRoot(destinationRoot.path)
            }
            try ensureDirectory(archiveRoot, under: documents, fileManager: fileManager)
            try copyDirectory(
                from: sourceRoot,
                to: destinationRoot,
                sourceBoundary: sourceRoot,
                destinationBoundary: archiveRoot,
                fileManager: fileManager,
                counters: &counters
            )
            counters.sourceRootsFound += 1
        }

        return CopyReport(
            sourceRootsFound: counters.sourceRootsFound,
            sourceRootsMissing: counters.sourceRootsMissing,
            filesCopied: counters.filesCopied,
            identicalFilesAlreadyPresent: counters.identicalFilesAlreadyPresent
        )
    }

    /// Returns nil when any source path component is absent. Existing components
    /// are checked one at a time so a symlink cannot redirect traversal elsewhere.
    private static func existingDirectory(
        under root: URL,
        components: [String]
    ) throws -> URL? {
        var current = root
        for component in components {
            guard isSafeComponent(component) else {
                throw ArchiveError.pathEscapesRoot(current.appendingPathComponent(component).path)
            }
            current.appendPathComponent(component, isDirectory: true)
            guard isContained(current, in: root) else {
                throw ArchiveError.pathEscapesRoot(current.path)
            }
            guard let kind = try entryKind(at: current) else { return nil }
            guard kind == .directory else {
                throw ArchiveError.expectedDirectory(current.path)
            }
        }
        return current
    }

    private static func copyDirectory(
        from source: URL,
        to destination: URL,
        sourceBoundary: URL,
        destinationBoundary: URL,
        fileManager: FileManager,
        counters: inout Counters
    ) throws {
        guard isContained(source, in: sourceBoundary),
              isContained(destination, in: destinationBoundary) else {
            throw ArchiveError.pathEscapesRoot(source.path)
        }
        guard try entryKind(at: source) == .directory else {
            throw ArchiveError.expectedDirectory(source.path)
        }
        try ensureDirectory(destination, under: destinationBoundary, fileManager: fileManager)

        let children = try fileManager.contentsOfDirectory(
            at: source,
            includingPropertiesForKeys: nil,
            options: []
        ).sorted { $0.lastPathComponent < $1.lastPathComponent }

        for child in children {
            let name = child.lastPathComponent
            guard isSafeComponent(name) else {
                throw ArchiveError.pathEscapesRoot(child.path)
            }
            let sourceChild = source.appendingPathComponent(name)
            let destinationChild = destination.appendingPathComponent(name)
            guard isContained(sourceChild, in: sourceBoundary),
                  isContained(destinationChild, in: destinationBoundary) else {
                throw ArchiveError.pathEscapesRoot(child.path)
            }

            switch try entryKind(at: sourceChild) {
            case .directory:
                try copyDirectory(
                    from: sourceChild,
                    to: destinationChild,
                    sourceBoundary: sourceBoundary,
                    destinationBoundary: destinationBoundary,
                    fileManager: fileManager,
                    counters: &counters
                )
            case .regularFile:
                try copyFile(
                    from: sourceChild,
                    to: destinationChild,
                    destinationBoundary: destinationBoundary,
                    fileManager: fileManager,
                    counters: &counters
                )
            case nil:
                throw ArchiveError.filesystem(sourceChild.path, ENOENT)
            }
        }
    }

    private static func copyFile(
        from source: URL,
        to destination: URL,
        destinationBoundary: URL,
        fileManager: FileManager,
        counters: inout Counters
    ) throws {
        guard try entryKind(at: source) == .regularFile else {
            throw ArchiveError.unsupportedFileType(source.path)
        }
        guard isContained(destination, in: destinationBoundary) else {
            throw ArchiveError.pathEscapesRoot(destination.path)
        }

        let parent = destination.deletingLastPathComponent()
        try ensureDirectory(parent, under: destinationBoundary, fileManager: fileManager)
        if let existingKind = try entryKind(at: destination) {
            guard existingKind == .regularFile else {
                throw ArchiveError.destinationConflict(destination.path)
            }
            guard fileManager.contentsEqual(atPath: source.path, andPath: destination.path) else {
                throw ArchiveError.destinationConflict(destination.path)
            }
            counters.identicalFilesAlreadyPresent += 1
            return
        }

        let staging = parent.appendingPathComponent(".legacy-rootfs-stage-\(UUID().uuidString)")
        guard try entryKind(at: staging) == nil else {
            throw ArchiveError.destinationConflict(staging.path)
        }
        defer {
            // This path is a uniquely named staging file created by this helper,
            // never an existing source or final destination.
            try? fileManager.removeItem(at: staging)
        }

        try fileManager.copyItem(at: source, to: staging)
        guard try entryKind(at: source) == .regularFile,
              try entryKind(at: staging) == .regularFile,
              fileManager.contentsEqual(atPath: source.path, andPath: staging.path) else {
            throw ArchiveError.copiedFileDidNotMatchSource(source.path)
        }

        // Same-directory move publishes only a complete verified file. It fails
        // if another writer created the final path; it never replaces that path.
        try fileManager.moveItem(at: staging, to: destination)
        counters.filesCopied += 1
    }

    private static func ensureDirectory(
        _ directory: URL,
        under boundary: URL,
        fileManager: FileManager
    ) throws {
        guard isContained(directory, in: boundary) else {
            throw ArchiveError.pathEscapesRoot(directory.path)
        }
        if directory.standardizedFileURL == boundary.standardizedFileURL {
            guard try entryKind(at: directory) == .directory else {
                throw ArchiveError.expectedDirectory(directory.path)
            }
            return
        }

        let parent = directory.deletingLastPathComponent()
        try ensureDirectory(parent, under: boundary, fileManager: fileManager)
        if let kind = try entryKind(at: directory) {
            guard kind == .directory else {
                throw ArchiveError.destinationConflict(directory.path)
            }
            return
        }

        try fileManager.createDirectory(at: directory, withIntermediateDirectories: false)
        guard try entryKind(at: directory) == .directory else {
            throw ArchiveError.expectedDirectory(directory.path)
        }
    }

    /// Uses lstat so even a dangling symbolic link is rejected without following it.
    private static func entryKind(at url: URL) throws -> EntryKind? {
        var info = stat()
        let result = url.path.withCString { lstat($0, &info) }
        guard result == 0 else {
            let code = errno
            if code == ENOENT || code == ENOTDIR { return nil }
            throw ArchiveError.filesystem(url.path, code)
        }

        switch info.st_mode & S_IFMT {
        case S_IFLNK:
            throw ArchiveError.symbolicLink(url.path)
        case S_IFDIR:
            return .directory
        case S_IFREG:
            return .regularFile
        default:
            throw ArchiveError.unsupportedFileType(url.path)
        }
    }

    private static func append(_ components: [String], to root: URL) -> URL {
        components.reduce(root) { partial, component in
            partial.appendingPathComponent(component, isDirectory: true)
        }
    }

    private static func isContained(_ candidate: URL, in root: URL) -> Bool {
        let rootPath = root.standardizedFileURL.path
        let candidatePath = candidate.standardizedFileURL.path
        return candidatePath == rootPath || candidatePath.hasPrefix(rootPath.hasSuffix("/") ? rootPath : rootPath + "/")
    }

    private static func isSafeComponent(_ component: String) -> Bool {
        !component.isEmpty
            && component != "."
            && component != ".."
            && !component.contains("/")
            && !component.contains("\0")
    }
}
