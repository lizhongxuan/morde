import Foundation

enum SharedContainerStore {
    static let appGroupID = "group.io.github.nanomuse.app"
}

@main
enum MinisStoragePathsTests {
    static func main() {
        let library = FileManager.default.urls(for: .libraryDirectory, in: .userDomainMask).first!
            .appendingPathComponent("MinisChat/minis", isDirectory: true)
        let base = MinisStoragePaths.minisPersistentBase.standardizedFileURL
        assert(base == library.standardizedFileURL)

        assert(MinisStoragePaths.minisAttachmentsPersistentDir(for: "session-1").path ==
               base.appendingPathComponent("session-1/attachments", isDirectory: true).path)
        assert(MinisStoragePaths.minisUploadsDir(for: "session-1").path ==
               base.appendingPathComponent("session-1/attachments/uploads", isDirectory: true).path)
        assert(MinisStoragePaths.minisOffloadsPersistentDir(for: "session-1").path ==
               base.appendingPathComponent("session-1/offloads", isDirectory: true).path)
        assert(MinisStoragePaths.minisWorkspacePersistentDir(for: "session-1").path ==
               base.appendingPathComponent("session-1/workspace", isDirectory: true).path)
        assert(MinisStoragePaths.minisBrowserPersistentDir(for: "session-1").path ==
               base.appendingPathComponent("session-1/browser", isDirectory: true).path)

        assert(MinisStoragePaths.minisLinuxBaseDir == "/var/minis")
        assert(MinisStoragePaths.minisAttachmentsLinuxDir == "/var/minis/attachments")
        assert(MinisStoragePaths.minisOffloadsLinuxDir == "/var/minis/offloads")
        assert(MinisStoragePaths.minisWorkspaceLinuxDir == "/var/minis/workspace")
        assert(MinisStoragePaths.minisBrowserLinuxDir == "/var/minis/browser")
        assert(MinisStoragePaths.minisMemoryLinuxDir == "/var/minis/memory")
        assert(MinisStoragePaths.minisSkillsLinuxDir == "/var/minis/skills")
        assert(MinisStoragePaths.minisSharedLinuxDir == "/var/minis/shared")
        assert(MinisStoragePaths.minisMcpServersLinuxDir == "/var/minis/mcp-servers")
        assert(MinisStoragePaths.minisMountsLinuxDir == "/var/minis/mounts")
    }
}
