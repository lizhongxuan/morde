import CryptoKit
import Foundation

@main
enum MordeWorkspaceTransferTests {
    static func main() throws {
        let manager = FileManager.default
        let root = manager.temporaryDirectory.appendingPathComponent("morde-transfer-\(UUID().uuidString)", isDirectory: true)
        try manager.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? manager.removeItem(at: root) }

        try MordeWorkspaceTransferSupport.validatePath("docs/readme.md")
        for path in ["", ".", "..", "/etc/passwd", "a/../b", "a//b", "a/", "a\\b", "nul\0name"] {
            do {
                try MordeWorkspaceTransferSupport.validatePath(path)
                fatalError("unsafe path was accepted: \(path)")
            } catch is MordeWorkspaceTransferError {
            }
        }
        let emptyPrefix = try MordeWorkspaceTransferSupport.normalizedManifestPrefix("")
        let sourcePrefix = try MordeWorkspaceTransferSupport.normalizedManifestPrefix("src")
        let normalizedSourcePrefix = try MordeWorkspaceTransferSupport.normalizedManifestPrefix("src/")
        let nestedPrefix = try MordeWorkspaceTransferSupport.normalizedManifestPrefix("src/nested/")
        assert(emptyPrefix == nil)
        assert(sourcePrefix == "src")
        assert(normalizedSourcePrefix == "src")
        assert(nestedPrefix == "src/nested")
        let renamedPath = try MordeWorkspaceTransferSupport.fileOperationTargetPath(
            action: "rename", sourcePath: "notes/draft.txt", input: "final.txt")
        let movedPath = try MordeWorkspaceTransferSupport.fileOperationTargetPath(
            action: "move", sourcePath: "notes/draft.txt", input: "archive/draft.txt")
        assert(renamedPath == "notes/final.txt")
        assert(movedPath == "archive/draft.txt")
        for (action, source, input) in [("rename", "a.txt", "a.txt"),
                                        ("rename", "notes/a.txt", "../secret"),
                                        ("rename", "notes/a.txt", "archive/a.txt"),
                                        ("move", "notes/a.txt", "/tmp/a.txt"),
                                        ("copy", "notes/a.txt", "archive/a.txt")] {
            do {
                _ = try MordeWorkspaceTransferSupport.fileOperationTargetPath(
                    action: action, sourcePath: source, input: input)
                fatalError("invalid Workspace file operation target was accepted: \(action) \(input)")
            } catch is MordeWorkspaceTransferError {
            }
        }
        let restoreKey = MordeWorkspaceTransferSupport.idempotencyKey(["workspace-1", "8", "9", "src/a.txt"])
        assert(restoreKey.count == 32)
        assert(restoreKey == MordeWorkspaceTransferSupport.idempotencyKey(["workspace-1", "8", "9", "src/a.txt"]))
        assert(restoreKey != MordeWorkspaceTransferSupport.idempotencyKey(["workspace-1", "8", "9", "src/b.txt"]))
        for prefix in ["/src", "/", "../src", "src//nested", "src\\nested", "nul\0src",
                       String(repeating: "a", count: 1025)] {
            do {
                _ = try MordeWorkspaceTransferSupport.normalizedManifestPrefix(prefix)
                fatalError("unsafe manifest prefix was accepted: \(prefix)")
            } catch is MordeWorkspaceTransferError {
            }
        }
        assert(MordeWorkspaceTransferSupport.hasCaseInsensitiveUniquePaths(["A.txt", "folder/b.txt"]))
        assert(!MordeWorkspaceTransferSupport.hasCaseInsensitiveUniquePaths(["A.txt", "a.TXT"]))
        assert(!MordeWorkspaceTransferSupport.hasCaseInsensitiveUniquePaths(["../bad"]))
        assert(MordeWorkspaceTransferSupport.hasValidManifestPaths(
            filePaths: ["docs/guide.md"], directoryPaths: [".", "docs"]))
        assert(!MordeWorkspaceTransferSupport.hasValidManifestPaths(
            filePaths: ["docs"], directoryPaths: [".", "docs/project"]))
        assert(!MordeWorkspaceTransferSupport.hasValidManifestPaths(
            filePaths: ["Docs"], directoryPaths: ["docs/project"]))
        assert(!MordeWorkspaceTransferSupport.hasValidManifestPaths(
            filePaths: ["README.md", "readme.MD"], directoryPaths: ["."]))

        let removable = root.appendingPathComponent("local-copy.txt")
        try Data("local copy".utf8).write(to: removable)
        assert(MordeWorkspaceTransferSupport.isRegularFileWithoutFollowingSymlinks(removable))
        try MordeWorkspaceTransferSupport.removeRegularFileWithoutFollowingSymlinks(removable)
        assert(!manager.fileExists(atPath: removable.path))

        let directory = root.appendingPathComponent("directory-copy", isDirectory: true)
        try manager.createDirectory(at: directory, withIntermediateDirectories: false)
        let nested = directory.appendingPathComponent("keep.txt")
        try Data("preserve directory contents".utf8).write(to: nested)
        do {
            try MordeWorkspaceTransferSupport.removeRegularFileWithoutFollowingSymlinks(directory)
            fatalError("a directory must not be accepted as a local file copy")
        } catch MordeWorkspaceTransferError.notRegularFile {
        }
        assert(manager.fileExists(atPath: nested.path), "rejecting a directory must preserve its contents")

        let removalSymlinkTarget = root.appendingPathComponent("symlink-target.txt")
        try Data("preserve symlink target".utf8).write(to: removalSymlinkTarget)
        let symlink = root.appendingPathComponent("symlink-copy.txt")
        try manager.createSymbolicLink(at: symlink, withDestinationURL: removalSymlinkTarget)
        do {
            try MordeWorkspaceTransferSupport.removeRegularFileWithoutFollowingSymlinks(symlink)
            fatalError("a symlink must not be accepted as a local file copy")
        } catch MordeWorkspaceTransferError.notRegularFile {
        }
        let symlinkTargetContents = try Data(contentsOf: removalSymlinkTarget)
        assert(symlinkTargetContents == Data("preserve symlink target".utf8))

        let emptyURL = root.appendingPathComponent("empty.bin")
        try Data().write(to: emptyURL)
        let emptyFile = try MordeWorkspaceTransferSupport.scan(url: emptyURL, path: "empty.bin")
        assert(emptyFile.sizeBytes == 0 && emptyFile.chunks.isEmpty)
        assert(emptyFile.sha256 == "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")

        let overflowManifest = MordeWorkspaceFile(path: "overflow.bin", sizeBytes: 0,
            sha256: digest(Data()), mode: 0o600,
            chunks: [MordeWorkspaceChunk(sha256: digest(Data()), sizeBytes: Int64.min)])
        do {
            _ = try MordeWorkspaceDownloadWriter(file: overflowManifest,
                to: root.appendingPathComponent("overflow.bin"))
            fatalError("overflowing manifest chunk size was accepted")
        } catch MordeWorkspaceTransferError.invalidManifest {
        }

        var bytes = Data(repeating: 0x6d, count: MordeWorkspaceTransferSupport.chunkByteLimit + 17)
        bytes[bytes.count - 1] = 0x2a
        let source = root.appendingPathComponent("source.bin")
        try bytes.write(to: source)
        let file = try MordeWorkspaceTransferSupport.scan(url: source, path: "folder/source.bin")
        assert(file.sizeBytes == Int64(bytes.count))
        assert(file.chunks.map(\.sizeBytes) == [Int64(MordeWorkspaceTransferSupport.chunkByteLimit), 17])
        assert(file.chunks.map(\.sha256) == [
            digest(Data(bytes.prefix(MordeWorkspaceTransferSupport.chunkByteLimit))),
            digest(Data(bytes.suffix(17))),
        ])
        assert(file.sha256 == digest(bytes))
        let firstRead = try MordeWorkspaceTransferSupport.readChunk(url: source, index: 0, expected: file.chunks[0])
        let secondRead = try MordeWorkspaceTransferSupport.readChunk(url: source, index: 1, expected: file.chunks[1])
        assert(firstRead.count == MordeWorkspaceTransferSupport.chunkByteLimit)
        assert(secondRead == Data(bytes.suffix(17)))

        let corruptedChunk = MordeWorkspaceChunk(sha256: String(repeating: "0", count: 64), sizeBytes: file.chunks[0].sizeBytes)
        do {
            _ = try MordeWorkspaceTransferSupport.readChunk(url: source, index: 0, expected: corruptedChunk)
            fatalError("changed source chunk was accepted")
        } catch is MordeWorkspaceTransferError {
        }

        let changedSource = root.appendingPathComponent("changed-source.bin")
        var changedBytes = bytes
        changedBytes[0] ^= 0xff
        try changedBytes.write(to: changedSource)
        do {
            _ = try MordeWorkspaceTransferSupport.readChunk(url: changedSource, index: 0, expected: file.chunks[0])
            fatalError("a file changed after selection was accepted")
        } catch MordeWorkspaceTransferError.changedFile {
        }

        let link = root.appendingPathComponent("source-link.bin")
        try manager.createSymbolicLink(at: link, withDestinationURL: source)
        do {
            _ = try MordeWorkspaceTransferSupport.scan(url: link, path: "source-link.bin")
            fatalError("symbolic link was accepted")
        } catch is MordeWorkspaceTransferError {
        }

        let downloadFolder = root.appendingPathComponent("downloads", isDirectory: true)
        try manager.createDirectory(at: downloadFolder, withIntermediateDirectories: true)
        let traversingDestination = downloadFolder.appendingPathComponent("../escaped.bin")
        do {
            _ = try MordeWorkspaceDownloadWriter(file: file, to: traversingDestination)
            fatalError("a non-normalized download destination was accepted")
        } catch MordeWorkspaceTransferError.invalidPath {
        }
        assert(!manager.fileExists(atPath: root.appendingPathComponent("escaped.bin").path))

        let linkedDownloadFolder = root.appendingPathComponent("linked-downloads", isDirectory: true)
        try manager.createSymbolicLink(at: linkedDownloadFolder, withDestinationURL: downloadFolder)
        do {
            _ = try MordeWorkspaceDownloadWriter(file: file,
                to: linkedDownloadFolder.appendingPathComponent("linked.bin"))
            fatalError("a symlinked download parent was accepted")
        } catch MordeWorkspaceTransferError.invalidPath {
        }

        let downloaded = downloadFolder.appendingPathComponent("source.bin")
        let writer = try MordeWorkspaceDownloadWriter(file: file, to: downloaded)
        for (index, chunk) in file.chunks.enumerated() {
            let data = Data(bytes.dropFirst(index * MordeWorkspaceTransferSupport.chunkByteLimit).prefix(Int(chunk.sizeBytes)))
            try writer.append(data, expected: chunk)
        }
        try writer.finish()
        let downloadedBytes = try Data(contentsOf: downloaded)
        assert(downloadedBytes == bytes)

        let shouldNotOverwrite = Data("keep me".utf8)
        try shouldNotOverwrite.write(to: downloaded)
        do {
            _ = try MordeWorkspaceDownloadWriter(file: file, to: downloaded)
            fatalError("existing destination was overwritten")
        } catch is MordeWorkspaceTransferError {
        }
        let unchangedBytes = try Data(contentsOf: downloaded)
        assert(unchangedBytes == shouldNotOverwrite)

        let symlinkTarget = root.appendingPathComponent("protected-target.bin")
        let protectedBytes = Data("keep symlink target".utf8)
        try protectedBytes.write(to: symlinkTarget)
        let symlinkDestination = downloadFolder.appendingPathComponent("existing-link.bin")
        try manager.createSymbolicLink(at: symlinkDestination, withDestinationURL: symlinkTarget)
        do {
            _ = try MordeWorkspaceDownloadWriter(file: file, to: symlinkDestination)
            fatalError("a symlink download destination was accepted")
        } catch MordeWorkspaceTransferError.targetExists {
        }
        let unchangedSymlinkTarget = try Data(contentsOf: symlinkTarget)
        assert(unchangedSymlinkTarget == protectedBytes)

        let badDestination = downloadFolder.appendingPathComponent("bad.bin")
        do {
            let badWriter = try MordeWorkspaceDownloadWriter(file: file, to: badDestination)
            try badWriter.append(Data(repeating: 0, count: Int(file.chunks[0].sizeBytes)), expected: file.chunks[0])
            fatalError("bad download block was accepted")
        } catch is MordeWorkspaceTransferError {
        }
        assert(!manager.fileExists(atPath: badDestination.path))

        let outOfOrderDestination = downloadFolder.appendingPathComponent("out-of-order.bin")
        do {
            let orderWriter = try MordeWorkspaceDownloadWriter(file: file, to: outOfOrderDestination)
            try orderWriter.append(secondRead, expected: file.chunks[1])
            fatalError("out-of-order download block was accepted")
        } catch is MordeWorkspaceTransferError {
        }
        assert(!manager.fileExists(atPath: outOfOrderDestination.path))

        let wrongFileDigest = MordeWorkspaceFile(path: "final-bad.bin", sizeBytes: file.sizeBytes,
                                                 sha256: String(repeating: "0", count: 64),
                                                 mode: file.mode, chunks: file.chunks)
        let wrongDigestDestination = downloadFolder.appendingPathComponent("final-bad.bin")
        do {
            let digestWriter = try MordeWorkspaceDownloadWriter(file: wrongFileDigest, to: wrongDigestDestination)
            for (index, chunk) in file.chunks.enumerated() {
                let data = Data(bytes.dropFirst(index * MordeWorkspaceTransferSupport.chunkByteLimit).prefix(Int(chunk.sizeBytes)))
                try digestWriter.append(data, expected: chunk)
            }
            try digestWriter.finish()
            fatalError("bad complete-file digest was accepted")
        } catch is MordeWorkspaceTransferError {
        }
        assert(!manager.fileExists(atPath: wrongDigestDestination.path))
        let remainingDownloads = try manager.contentsOfDirectory(atPath: downloadFolder.path)
        assert(remainingDownloads.allSatisfy { !$0.hasPrefix(".morde-") })

        let restrictedDownloads = root.appendingPathComponent("restricted-downloads", isDirectory: true)
        try manager.createDirectory(at: restrictedDownloads, withIntermediateDirectories: true)
        defer { try? manager.setAttributes([.posixPermissions: 0o700], ofItemAtPath: restrictedDownloads.path) }
        let abandonedWriter = try MordeWorkspaceDownloadWriter(file: file,
            to: restrictedDownloads.appendingPathComponent("abandoned.bin"))
        try manager.setAttributes([.posixPermissions: 0o500], ofItemAtPath: restrictedDownloads.path)
        abandonedWriter.cancel()
        let failedCleanupFiles = try manager.contentsOfDirectory(atPath: restrictedDownloads.path)
            .filter { $0.hasPrefix(".morde-") }
        assert(failedCleanupFiles.count == 1)

        try manager.setAttributes([.posixPermissions: 0o700], ofItemAtPath: restrictedDownloads.path)
        let retryWriter = try MordeWorkspaceDownloadWriter(file: file,
            to: restrictedDownloads.appendingPathComponent("retry.bin"))
        let retriedCleanupFiles = try manager.contentsOfDirectory(atPath: restrictedDownloads.path)
            .filter { $0.hasPrefix(".morde-") }
        assert(retriedCleanupFiles.count == 1)
        retryWriter.cancel()
        let finalCleanupFiles = try manager.contentsOfDirectory(atPath: restrictedDownloads.path)
            .filter { $0.hasPrefix(".morde-") }
        assert(finalCleanupFiles.isEmpty)

        let encoded = try JSONEncoder().encode(file)
        let object = try JSONSerialization.jsonObject(with: encoded) as! [String: Any]
        assert(object["size_bytes"] as? Int == bytes.count)
        let chunkObjects = object["chunks"] as! [[String: Any]]
        assert(chunkObjects[0]["size_bytes"] as? Int == MordeWorkspaceTransferSupport.chunkByteLimit)
        print("Morde workspace transfer file checks passed")
    }

    private static func digest(_ data: Data) -> String {
        SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
    }
}
