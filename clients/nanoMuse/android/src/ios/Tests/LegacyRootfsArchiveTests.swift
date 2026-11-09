import Foundation

@main
enum LegacyRootfsArchiveTests {
    static func main() throws {
        let fileManager = FileManager.default
        let sandbox = fileManager.temporaryDirectory
            .appendingPathComponent("legacy-rootfs-archive-tests-\(UUID().uuidString)", isDirectory: true)
        let documents = sandbox.appendingPathComponent("Documents", isDirectory: true)
        try fileManager.createDirectory(at: documents, withIntermediateDirectories: true)
        defer { try? fileManager.removeItem(at: sandbox) }

        let emptyDocuments = sandbox.appendingPathComponent("EmptyDocuments", isDirectory: true)
        try fileManager.createDirectory(at: emptyDocuments, withIntermediateDirectories: true)
        let emptyReport = try LegacyRootfsArchive.preserveUserData(in: emptyDocuments)
        precondition(emptyReport.sourceRootsFound == 0)
        precondition(emptyReport.sourceRootsMissing.count == 2)
        precondition(emptyReport.filesCopied == 0)
        precondition(!fileManager.fileExists(atPath: emptyDocuments
            .appendingPathComponent(LegacyRootfsArchive.archiveDirectoryName).path))

        let rootOnlyDocuments = sandbox.appendingPathComponent("RootOnlyDocuments", isDirectory: true)
        let rootOnly = rootOnlyDocuments.appendingPathComponent("alpine-rootfs/data/root", isDirectory: true)
        try fileManager.createDirectory(at: rootOnly, withIntermediateDirectories: true)
        try Data("preserve me".utf8).write(to: rootOnly.appendingPathComponent("file.txt"))
        let rootOnlyReport = try LegacyRootfsArchive.preserveUserData(in: rootOnlyDocuments)
        precondition(rootOnlyReport.sourceRootsFound == 1)
        precondition(rootOnlyReport.sourceRootsMissing == ["data/var/minis"])
        precondition(rootOnlyReport.filesCopied == 1)
        try assertContents(
            at: rootOnlyDocuments.appendingPathComponent("MinisLegacyRootfsArchive/root/file.txt"),
            equals: "preserve me"
        )
        try assertContents(at: rootOnly.appendingPathComponent("file.txt"), equals: "preserve me")

        let rootSource = documents.appendingPathComponent("alpine-rootfs/data/root", isDirectory: true)
        let minisSource = documents.appendingPathComponent("alpine-rootfs/data/var/minis", isDirectory: true)
        try fileManager.createDirectory(at: rootSource.appendingPathComponent("workspace", isDirectory: true), withIntermediateDirectories: true)
        try fileManager.createDirectory(at: minisSource.appendingPathComponent("mcp/servers", isDirectory: true), withIntermediateDirectories: true)
        try Data("root-owned content".utf8).write(to: rootSource.appendingPathComponent(".profile"))
        try Data("nested file".utf8).write(to: rootSource.appendingPathComponent("workspace/notes.txt"))
        try Data("minis state".utf8).write(to: minisSource.appendingPathComponent("state.json"))
        try Data("server config".utf8).write(to: minisSource.appendingPathComponent("mcp/servers/config.json"))

        let report = try LegacyRootfsArchive.preserveUserData(in: documents)
        precondition(report.sourceRootsFound == 2)
        precondition(report.sourceRootsMissing.isEmpty)
        precondition(report.filesCopied == 4)
        precondition(report.identicalFilesAlreadyPresent == 0)

        let archive = documents.appendingPathComponent(LegacyRootfsArchive.archiveDirectoryName, isDirectory: true)
        try assertContents(at: archive.appendingPathComponent("root/.profile"), equals: "root-owned content")
        try assertContents(at: archive.appendingPathComponent("root/workspace/notes.txt"), equals: "nested file")
        try assertContents(at: archive.appendingPathComponent("var/minis/state.json"), equals: "minis state")
        try assertContents(at: archive.appendingPathComponent("var/minis/mcp/servers/config.json"), equals: "server config")

        let repeatReport = try LegacyRootfsArchive.preserveUserData(in: documents)
        precondition(repeatReport.filesCopied == 0)
        precondition(repeatReport.identicalFilesAlreadyPresent == 4)
        try assertContents(at: rootSource.appendingPathComponent(".profile"), equals: "root-owned content")
        try assertContents(at: minisSource.appendingPathComponent("state.json"), equals: "minis state")

        // A conflicting destination is never overwritten. The call throws even if
        // earlier sorted files were copied, so callers cannot mistake a partial run
        // for a complete archive.
        let partialDocs = sandbox.appendingPathComponent("PartialDocuments", isDirectory: true)
        let partialRoot = partialDocs.appendingPathComponent("alpine-rootfs/data/root", isDirectory: true)
        try fileManager.createDirectory(at: partialRoot, withIntermediateDirectories: true)
        try fileManager.createDirectory(
            at: partialDocs.appendingPathComponent("alpine-rootfs/data/var/minis", isDirectory: true),
            withIntermediateDirectories: true
        )
        try Data("a new file".utf8).write(to: partialRoot.appendingPathComponent("a-new.txt"))
        try Data("source wins nowhere".utf8).write(to: partialRoot.appendingPathComponent("z-conflict.txt"))
        let partialArchive = partialDocs
            .appendingPathComponent(LegacyRootfsArchive.archiveDirectoryName, isDirectory: true)
            .appendingPathComponent("root", isDirectory: true)
        try fileManager.createDirectory(at: partialArchive, withIntermediateDirectories: true)
        try Data("existing destination".utf8).write(to: partialArchive.appendingPathComponent("z-conflict.txt"))
        do {
            _ = try LegacyRootfsArchive.preserveUserData(in: partialDocs)
            preconditionFailure("A destination conflict must not report success")
        } catch LegacyRootfsArchive.ArchiveError.destinationConflict {
        }
        try assertContents(at: partialArchive.appendingPathComponent("a-new.txt"), equals: "a new file")
        try assertContents(at: partialArchive.appendingPathComponent("z-conflict.txt"), equals: "existing destination")
        try assertContents(at: partialRoot.appendingPathComponent("z-conflict.txt"), equals: "source wins nowhere")

        // Source and destination symlinks that escape their roots are rejected.
        let outside = sandbox.appendingPathComponent("outside", isDirectory: true)
        try fileManager.createDirectory(at: outside, withIntermediateDirectories: true)
        let outsideFile = outside.appendingPathComponent("private.txt")
        try Data("outside sentinel".utf8).write(to: outsideFile)
        precondition(LegacyRootfsArchive.isContainedAfterResolvingSymlinks(
            archive.appendingPathComponent("root/.profile"), under: archive
        ))
        precondition(!LegacyRootfsArchive.isContainedAfterResolvingSymlinks(outsideFile, under: archive))
        let archiveSymlink = archive.appendingPathComponent("outside.txt")
        try fileManager.createSymbolicLink(at: archiveSymlink, withDestinationURL: outsideFile)
        precondition(!LegacyRootfsArchive.isContainedAfterResolvingSymlinks(archiveSymlink, under: archive))

        let symlinkDocs = sandbox.appendingPathComponent("SymlinkDocuments", isDirectory: true)
        let symlinkRoot = symlinkDocs.appendingPathComponent("alpine-rootfs/data/root", isDirectory: true)
        try fileManager.createDirectory(at: symlinkRoot.deletingLastPathComponent(), withIntermediateDirectories: true)
        try fileManager.createSymbolicLink(at: symlinkRoot, withDestinationURL: outside)
        do {
            _ = try LegacyRootfsArchive.preserveUserData(in: symlinkDocs)
            preconditionFailure("An escaping source symlink must be rejected")
        } catch LegacyRootfsArchive.ArchiveError.symbolicLink {
        }
        let sourceLinkTarget = try fileManager.destinationOfSymbolicLink(atPath: symlinkRoot.path)
        precondition(sourceLinkTarget == outside.path)
        try assertContents(at: outsideFile, equals: "outside sentinel")

        let destinationLinkDocs = sandbox.appendingPathComponent("DestinationLinkDocuments", isDirectory: true)
        let destinationRoot = destinationLinkDocs
            .appendingPathComponent(LegacyRootfsArchive.archiveDirectoryName, isDirectory: true)
        try fileManager.createDirectory(at: destinationLinkDocs.appendingPathComponent("alpine-rootfs/data/root", isDirectory: true), withIntermediateDirectories: true)
        try fileManager.createDirectory(at: destinationLinkDocs.appendingPathComponent("alpine-rootfs/data/var/minis", isDirectory: true), withIntermediateDirectories: true)
        try Data("safe source".utf8).write(to: destinationLinkDocs.appendingPathComponent("alpine-rootfs/data/root/file.txt"))
        try fileManager.createSymbolicLink(at: destinationRoot, withDestinationURL: outside)
        do {
            _ = try LegacyRootfsArchive.preserveUserData(in: destinationLinkDocs)
            preconditionFailure("An escaping destination symlink must be rejected")
        } catch LegacyRootfsArchive.ArchiveError.symbolicLink {
        }
        let destinationLinkTarget = try fileManager.destinationOfSymbolicLink(atPath: destinationRoot.path)
        precondition(destinationLinkTarget == outside.path)
        try assertContents(at: outsideFile, equals: "outside sentinel")

        print("LegacyRootfsArchive host tests passed")
    }

    private static func assertContents(at url: URL, equals expected: String) throws {
        let actual = try String(contentsOf: url, encoding: .utf8)
        precondition(actual == expected)
    }
}
