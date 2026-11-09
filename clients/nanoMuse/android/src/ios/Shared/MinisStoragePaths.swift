import Foundation

/// Storage locations shared by the app, FileProvider and local file services.
enum MinisStoragePaths {
    // Keep these paths aligned with the legacy AIChatViewModel constants until their execution-only callers are removed.
    static let minisLinuxBaseDir = "/var/minis"
    static let minisAttachmentsLinuxDir = "/var/minis/attachments"
    static let minisOffloadsLinuxDir = "/var/minis/offloads"
    static let minisWorkspaceLinuxDir = "/var/minis/workspace"
    static let minisBrowserLinuxDir = "/var/minis/browser"
    static let minisMemoryLinuxDir = "/var/minis/memory"
    static let minisSkillsLinuxDir = "/var/minis/skills"
    static let minisSharedLinuxDir = "/var/minis/shared"
    static let minisMcpServersLinuxDir = "/var/minis/mcp-servers"
    static let minisMountsLinuxDir = "/var/minis/mounts"

    static var minisPersistentBase: URL {
        let lib = FileManager.default.urls(for: .libraryDirectory, in: .userDomainMask).first!
        return lib.appendingPathComponent("MinisChat/minis", isDirectory: true)
    }

    static func minisOffloadsPersistentDir(for sid: String) -> URL {
        minisPersistentBase.appendingPathComponent(sid, isDirectory: true)
            .appendingPathComponent("offloads", isDirectory: true)
    }

    static func minisAttachmentsPersistentDir(for sid: String) -> URL {
        minisPersistentBase.appendingPathComponent(sid, isDirectory: true)
            .appendingPathComponent("attachments", isDirectory: true)
    }

    static func minisUploadsDir(for sid: String) -> URL {
        minisAttachmentsPersistentDir(for: sid)
            .appendingPathComponent("uploads", isDirectory: true)
    }

    static func minisWorkspacePersistentDir(for sid: String) -> URL {
        minisPersistentBase.appendingPathComponent(sid, isDirectory: true)
            .appendingPathComponent("workspace", isDirectory: true)
    }

    static func minisBrowserPersistentDir(for sid: String) -> URL {
        minisPersistentBase.appendingPathComponent(sid, isDirectory: true)
            .appendingPathComponent("browser", isDirectory: true)
    }

    // nanoMuse: simulator builds without a signing team may not receive the App Group entitlement.
    static var minisAppGroupContainer: URL {
        if let container = FileManager.default.containerURL(
            forSecurityApplicationGroupIdentifier: SharedContainerStore.appGroupID
        ) {
            return container
        }

#if targetEnvironment(simulator)
        let base = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first
            ?? FileManager.default.temporaryDirectory
        let fallback = base.appendingPathComponent("MinisAppGroupFallback", isDirectory: true)
        try? FileManager.default.createDirectory(at: fallback, withIntermediateDirectories: true)
        return fallback
#else
        fatalError("Missing App Group container entitlement")
#endif
    }

    static var minisAppGroupRoot: URL {
        minisAppGroupContainer.appendingPathComponent("MinisFileProvider", isDirectory: true)
    }

    static var minisConfigRoot: URL {
        let url = minisAppGroupContainer.appendingPathComponent("MinisConfig", isDirectory: true)
        try? FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        return url
    }

    static var minisMemoryPersistentDir: URL {
        minisAppGroupRoot.appendingPathComponent("memory", isDirectory: true)
    }

    static var minisSkillsPersistentDir: URL {
        minisAppGroupRoot.appendingPathComponent("skills", isDirectory: true)
    }

    static var minisSharedPersistentDir: URL {
        minisAppGroupRoot.appendingPathComponent("shared", isDirectory: true)
    }

    static var minisMcpServersPersistentDir: URL {
        minisConfigRoot.appendingPathComponent("mcp-servers", isDirectory: true)
    }
}
