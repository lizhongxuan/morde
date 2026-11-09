import SwiftUI
import Darwin
import PDFKit
import UIKit

#if MORDE_STANDALONE
struct NanoMuseLibraryEntry: Identifiable, Hashable {
    enum Kind: Hashable { case artifact, media }
    var id: String { url.path }
    var url: URL
    var name: String
    var relativePath: String
    var size: Int64
    var modified: Date
    var kind: Kind
    var sessionId: String?
    var sessionTitle: String?
}

enum NanoMuseLibraryIndex {
    static func scan(titles: [String: String]) -> [NanoMuseLibraryEntry] {
        scanResult(titles: titles) ?? []
    }

    static func scanResult(titles _: [String: String]) -> [NanoMuseLibraryEntry]? {
        let root = FileManager.default.urls(for: .documentDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("Morde Workspace Files", isDirectory: true)
        var rootInfo = stat()
        guard lstat(root.path, &rootInfo) == 0 else {
            return errno == ENOENT || errno == ENOTDIR ? [] : nil
        }
        guard (rootInfo.st_mode & mode_t(S_IFMT)) == mode_t(S_IFDIR) else { return nil }

        var entries: [NanoMuseLibraryEntry] = []
        guard walk(root, root: root, depth: 0, into: &entries) else { return nil }
        return entries.sorted { $0.modified > $1.modified }
    }

    private static func walk(_ directory: URL, root: URL, depth: Int,
                             into entries: inout [NanoMuseLibraryEntry]) -> Bool {
        guard depth <= 4, entries.count < 600 else { return false }
        let children: [URL]
        do {
            children = try FileManager.default.contentsOfDirectory(at: directory,
                includingPropertiesForKeys: [.isDirectoryKey, .fileSizeKey, .contentModificationDateKey],
                options: [.skipsHiddenFiles])
        } catch {
            return false
        }

        for child in children {
            guard entries.count < 600 else { return false }
            var info = stat()
            let statResult = lstat(child.path, &info)
            let statError = errno
            guard statResult == 0 else {
                if statError == ENOENT || statError == ENOTDIR { continue }
                return false
            }
            let type = info.st_mode & mode_t(S_IFMT)
            if type == mode_t(S_IFLNK) { continue }
            if type == mode_t(S_IFDIR) {
                guard walk(child, root: root, depth: depth + 1, into: &entries) else { return false }
                continue
            }
            guard type == mode_t(S_IFREG) else { continue }
            let values: URLResourceValues
            do {
                values = try child.resourceValues(forKeys: [.fileSizeKey, .contentModificationDateKey])
            } catch {
                return false
            }
            let relativePath = String(child.path.dropFirst(root.path.count + 1))
            let media = ["png", "jpg", "jpeg", "gif", "bmp", "webp", "heic", "svg",
                         "mp4", "mov", "avi", "mkv", "webm", "mp3", "wav", "aac", "flac", "ogg", "m4a"]
                .contains(child.pathExtension.lowercased())
            entries.append(NanoMuseLibraryEntry(url: child, name: child.lastPathComponent,
                relativePath: relativePath, size: Int64(values.fileSize ?? 0),
                modified: values.contentModificationDate ?? .distantPast,
                kind: media ? .media : .artifact, sessionId: nil, sessionTitle: nil))
        }
        return true
    }
}
#endif

enum MIFLibraryAccessState: Equatable, Sendable {
    case loading
    case available
    case denied
    case unavailable
}

enum MIFLibraryItemKind: String, Equatable, Sendable {
    case artifact
    case media
}

enum MIFLibrarySource: String, CaseIterable, Identifiable, Hashable {
    case all
    case aiOutputs
    case myUploads

    var id: Self { self }

    var title: String {
        switch self {
        case .all: return AppLocalized("All")
        case .aiOutputs: return AppLocalized("AI outputs")
        case .myUploads: return AppLocalized("My uploads")
        }
    }

    func includes(_ item: MIFLibraryAccountItem) -> Bool {
        switch self {
        case .all: return true
        case .aiOutputs: return false
        case .myUploads: return item.hasAttachment
        }
    }
}

enum MIFLibraryLocalOrigin: Equatable, Sendable {
    case sharedFolder
    case conversation(title: String)
}

/// Root-owned opaque reference. It never contains a URL, path, or ticket.
/// The root may provide one only after verifying a local file and confirming
/// that the current account scope can preview it. Recheck both when resolving
/// it for preview; the page cannot resolve the handle itself.
struct MIFLibraryPreviewHandle: Hashable, Identifiable, Sendable {
    let id: UUID
}

struct MIFLibraryLocalItem: Identifiable, Equatable, Sendable {
    let id: UUID
    /// Display name only; the adapter must not pass a file path here.
    let name: String
    let kind: MIFLibraryItemKind
    let sizeBytes: Int64?
    let modifiedAt: Date?
    let origin: MIFLibraryLocalOrigin?
    let previewHandle: MIFLibraryPreviewHandle?
}

struct MIFLibraryTaskArtifact: Identifiable, Equatable, Sendable {
    let id: String
    let label: String
    let mediaType: String
    let workspaceID: String?
    let workspaceRevision: Int64?
    let sizeBytes: Int64?
    let sha256: String?
    let verified: Bool?
    let previewHandle: MIFLibraryPreviewHandle?
}

struct MIFLibraryAccountItem: Identifiable, Equatable, Sendable {
    let id: String
    let name: String
    let kind: MIFLibraryItemKind
    let mediaType: String?
    let sizeBytes: Int64?
    let attachmentID: String?
    let previewHandle: MIFLibraryPreviewHandle?

    var hasAttachment: Bool {
        attachmentID.map(MordePersonalDataContract.isValidAttachmentID) == true
    }
}

/// Pure gate for page actions. The caller owns permission checks and handle
/// creation; this view only exposes a callback for an already verified handle.
enum MIFLibraryPreviewPolicy {
    static func normalizedPersonalAttachmentMediaType(_ value: String?) -> String? {
        guard let value else { return nil }
        let mediaType = value.split(separator: ";", maxSplits: 1).first?
            .trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
        guard let mediaType, MordePersonalDataContract.isValidAttachmentMediaType(mediaType),
              mediaType == "application/pdf" || mediaType.hasPrefix("image/") else {
            return nil
        }
        return mediaType
    }

    static func personalAttachmentHandle(for item: MIFLibraryAccountItem,
                                         access: MIFLibraryAccessState) -> MIFLibraryPreviewHandle? {
        guard access == .available,
              item.attachmentID.map(MordePersonalDataContract.isValidAttachmentID) == true,
              normalizedPersonalAttachmentMediaType(item.mediaType) != nil,
              let handle = item.previewHandle else { return nil }
        return handle
    }

    static func localHandle(for item: MIFLibraryLocalItem,
                            access: MIFLibraryAccessState) -> MIFLibraryPreviewHandle? {
        guard access == .available, let handle = item.previewHandle else { return nil }
        return handle
    }

    static func taskHandle(for artifact: MIFLibraryTaskArtifact,
                           access: MIFLibraryAccessState,
                           workspaceSyncAvailable: Bool?,
                           taskMediaGenerationAvailable: Bool?) -> MIFLibraryPreviewHandle? {
        let mediaType = artifact.mediaType.lowercased()
        let requiresMediaCapability = mediaType.hasPrefix("image/") ||
            mediaType.hasPrefix("video/") || mediaType.hasPrefix("audio/")
        guard access == .available, workspaceSyncAvailable == true,
              !requiresMediaCapability || taskMediaGenerationAvailable == true,
              !artifact.id.isEmpty,
              artifact.verified == true,
              let workspaceID = artifact.workspaceID,
              workspaceID.range(of: "^w[a-z0-9]{7}$", options: .regularExpression) != nil,
              let revision = artifact.workspaceRevision, revision >= 0,
              let sizeBytes = artifact.sizeBytes, sizeBytes >= 0,
              let sha256 = artifact.sha256,
              sha256.range(of: "^[0-9a-f]{64}$", options: .regularExpression) != nil,
              let handle = artifact.previewHandle else { return nil }
        return handle
    }
}

private enum MIFLibraryDesignPalette {
    #if MORDE_STANDALONE
    static let secondaryText = Color(uiColor: UIColor {
        $0.userInterfaceStyle == .dark
            ? UIColor(red: 0.73, green: 0.73, blue: 0.77, alpha: 1)
            : UIColor(red: 0.45098, green: 0.45098, blue: 0.470588, alpha: 1)
    })
    static let selectedSourceFill = Color(uiColor: UIColor {
        $0.userInterfaceStyle == .dark
            ? UIColor(red: 0.165, green: 0.165, blue: 0.18, alpha: 1)
            : UIColor(red: 0.937255, green: 0.937255, blue: 0.937255, alpha: 1)
    })
    static let defaultTypeFill = Color(uiColor: UIColor {
        $0.userInterfaceStyle == .dark
            ? UIColor(red: 0.165, green: 0.165, blue: 0.18, alpha: 1)
            : UIColor(red: 0.92549, green: 0.92549, blue: 0.941176, alpha: 1)
    })
    static let defaultTypeText = Color(uiColor: UIColor {
        $0.userInterfaceStyle == .dark
            ? UIColor(red: 0.73, green: 0.73, blue: 0.77, alpha: 1)
            : UIColor(red: 0.333333, green: 0.333333, blue: 0.360784, alpha: 1)
    })
    #else
    static let secondaryText = NanoMuseTones.secondaryText
    static let selectedSourceFill = NanoMuseTones.fill
    static let defaultTypeFill = NanoMuseTones.fill
    static let defaultTypeText = NanoMuseTones.secondaryText
    #endif
}

private struct MIFLibraryDesignHeader: View {
    let onMenu: () -> Void

    var body: some View {
        HStack(spacing: 10) {
            Button(action: onMenu) {
                Circle()
                    .fill(NanoMuseTones.surface)
                    .overlay(Circle().strokeBorder(NanoMuseTones.hairline, lineWidth: 0.5))
                    .overlay {
                        VStack(spacing: 3.1) {
                            Capsule().frame(width: 13, height: 1.7)
                            Capsule().frame(width: 13, height: 1.7)
                        }
                        .foregroundStyle(NanoMuseTones.ink)
                    }
                    .shadow(color: .black.opacity(0.12), radius: 3, y: 1)
                    .frame(width: 38, height: 38)
            }
            .buttonStyle(.plain)
            .accessibilityLabel(AppLocalized("Chats and settings"))
            .accessibilityIdentifier("morde.nav.menu")

            Text(AppLocalized("Library"))
                .font(.system(size: 17, weight: .semibold))
                .tracking(-0.17)
                .lineLimit(1)

            Spacer(minLength: 0)
        }
        .padding(.horizontal, 22)
        .padding(.top, 10)
        .padding(.bottom, 4)
        .frame(minHeight: 52)
    }
}

struct MIFLibraryPage: View {
    var chrome: NanoMuseRoomChrome? = nil
    var onMenu: (() -> Void)? = nil
    let localAccess: MIFLibraryAccessState
    let taskAccess: MIFLibraryAccessState
    let accountAccess: MIFLibraryAccessState
    let workspaceSyncAvailable: Bool?
    let taskMediaGenerationAvailable: Bool?
    let localItems: [MIFLibraryLocalItem]
    let taskArtifacts: [MIFLibraryTaskArtifact]
    let accountItems: [MIFLibraryAccountItem]
    let onPreview: (MIFLibraryPreviewHandle) -> Void

    @State private var source: MIFLibrarySource = .all

    static func localPreviewAccessibilityLabel(for name: String) -> String {
        String(format: AppLocalized("Preview file: %@"), name)
    }

    static func taskArtifactPreviewAccessibilityLabel(for label: String) -> String {
        String(format: AppLocalized("Preview artifact: %@"), label)
    }

    static func accountAttachmentPreviewAccessibilityLabel(for name: String) -> String {
        String(format: AppLocalized("Preview file: %@"), name)
    }

    var body: some View {
        VStack(spacing: 0) {
            if let onMenu {
                MIFLibraryDesignHeader(onMenu: onMenu)
            } else if let chrome {
                NanoMuseMenuHeader(title: AppLocalized("Library"), onMenu: chrome.onMenu)
            }
            ScrollView {
                VStack(alignment: .leading, spacing: 0) {
                    HStack(spacing: 4) {
                        ForEach(MIFLibrarySource.allCases) { option in
                            Button {
                                source = option
                            } label: {
                                Text(option.title)
                                    .font(.system(size: 14.5, weight: .semibold))
                                    .foregroundStyle(option == source ? Color.primary : MIFLibraryDesignPalette.secondaryText)
                                    .padding(.horizontal, 13)
                                    .padding(.vertical, 7)
                                    .background {
                                        if option == source {
                                            Capsule().fill(MIFLibraryDesignPalette.selectedSourceFill)
                                        }
                                    }
                            }
                            .buttonStyle(.plain)
                            .accessibilityIdentifier("morde.library.source.\(option.rawValue)")
                            .accessibilityAddTraits(option == source ? .isSelected : [])
                        }
                    }
                    .accessibilityElement(children: .contain)
                    .accessibilityIdentifier("morde.library.sourcePicker")
                    .padding(.top, 8)
                    .padding(.bottom, 2)

                    if source == .all { localRows }
                    if source == .all || source == .aiOutputs { taskRows }
                    if source == .all || source == .myUploads { accountRows }
                    if source == .all && allSourcesConfirmedEmpty {
                        emptyRow(AppLocalized("No files in this category"))
                    }
                }
                .padding(.horizontal, 22)
                .padding(.top, 2)
                .padding(.bottom, 108)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .accessibilityIdentifier("morde.library.scroll")
        }
        .background(ChatColors.background.ignoresSafeArea())
        .tint(NanoMuseTones.action)
    }

    @ViewBuilder
    private var localRows: some View {
        switch localAccess {
        case .loading:
            ProgressView().frame(maxWidth: .infinity).padding(.vertical, 13)
        case .denied, .unavailable:
            emptyRow(AppLocalized("Unavailable"))
        case .available where localItems.isEmpty:
            if source != .all { emptyRow(AppLocalized("Nothing created yet")) }
        case .available:
            ForEach(localItems) { item in localRow(item) }
        }
    }

    @ViewBuilder
    private var taskRows: some View {
        switch taskAccess {
        case .loading:
            ProgressView().frame(maxWidth: .infinity).padding(.vertical, 13)
        case .denied:
            emptyRow(AppLocalized("Sign in to view Morde tasks"))
        case .unavailable:
            emptyRow(AppLocalized("Artifact details unavailable"))
        case .available where taskArtifacts.isEmpty:
            if source != .all { emptyRow(AppLocalized("No delivered artifacts")) }
        case .available:
            ForEach(taskArtifacts) { artifact in taskArtifactRow(artifact) }
        }
    }

    private var visibleAccountItems: [MIFLibraryAccountItem] {
        accountItems.filter { source.includes($0) }
    }

    private var allSourcesConfirmedEmpty: Bool {
        localAccess == .available && localItems.isEmpty &&
            taskAccess == .available && taskArtifacts.isEmpty &&
            accountAccess == .available && visibleAccountItems.isEmpty
    }

    @ViewBuilder
    private var accountRows: some View {
        switch accountAccess {
        case .loading:
            ProgressView().frame(maxWidth: .infinity).padding(.vertical, 13)
        case .denied:
            emptyRow(AppLocalized("Sign in to sync"))
        case .unavailable:
            emptyRow(AppLocalized("Unavailable"))
        case .available where visibleAccountItems.isEmpty:
            if source != .all { emptyRow(AppLocalized("No cloud files yet")) }
        case .available:
            ForEach(visibleAccountItems) { item in accountRow(item) }
        }
    }

    private func emptyRow(_ title: String) -> some View {
        Text(title)
            .font(.system(size: 14.5))
            .foregroundStyle(MIFLibraryDesignPalette.secondaryText)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.top, 7)
    }

    @ViewBuilder
    private func localRow(_ item: MIFLibraryLocalItem) -> some View {
        let handle = MIFLibraryPreviewPolicy.localHandle(for: item, access: localAccess)
        let row = HStack(alignment: .top, spacing: 13) {
            fileTypeTile(fileTypeLabel(name: item.name, mediaType: nil,
                                      defaultLabel: item.kind == .media ? "图片" : "文件"))
            VStack(alignment: .leading, spacing: 2) {
                Text(item.name)
                    .font(.system(size: 16, weight: .semibold))
                    .tracking(-0.16)
                    .lineSpacing(1.5)
                    .fixedSize(horizontal: false, vertical: true)
                Text(localDetail(item, metadataOnly: handle == nil))
                    .font(.system(size: 14))
                    .lineSpacing(3.5)
                    .foregroundStyle(MIFLibraryDesignPalette.secondaryText)
                    .fixedSize(horizontal: false, vertical: true)
            }
            .padding(.top, 1)
            Spacer(minLength: 8)
        }
        .padding(.vertical, 13)

        if let handle {
            Button { onPreview(handle) } label: { row }
                .buttonStyle(.plain)
                .contentShape(Rectangle())
                .accessibilityLabel(Self.localPreviewAccessibilityLabel(for: item.name))
                .accessibilityIdentifier("morde.library.local.preview.\(item.id.uuidString)")
        } else {
            row.accessibilityIdentifier("morde.library.local.\(item.id.uuidString)")
        }
    }

    @ViewBuilder
    private func taskArtifactRow(_ artifact: MIFLibraryTaskArtifact) -> some View {
        let handle = MIFLibraryPreviewPolicy.taskHandle(
            for: artifact, access: taskAccess,
            workspaceSyncAvailable: workspaceSyncAvailable,
            taskMediaGenerationAvailable: taskMediaGenerationAvailable)
        let row = HStack(alignment: .top, spacing: 13) {
            fileTypeTile(fileTypeLabel(name: artifact.label, mediaType: artifact.mediaType,
                                      defaultLabel: "构件"))
            VStack(alignment: .leading, spacing: 2) {
                Text(artifact.label)
                    .font(.system(size: 16, weight: .semibold))
                    .tracking(-0.16)
                    .lineSpacing(1.5)
                    .fixedSize(horizontal: false, vertical: true)
                Text(taskDetail(artifact))
                    .font(.system(size: 14))
                    .lineSpacing(3.5)
                    .foregroundStyle(MIFLibraryDesignPalette.secondaryText)
                    .fixedSize(horizontal: false, vertical: true)
            }
            .padding(.top, 1)
            Spacer(minLength: 8)
        }
        .padding(.vertical, 13)

        if let handle {
            Button { onPreview(handle) } label: { row }
                .buttonStyle(.plain)
                .contentShape(Rectangle())
                .accessibilityLabel(Self.taskArtifactPreviewAccessibilityLabel(for: artifact.label))
                .accessibilityIdentifier("morde.library.task-artifact.preview.\(artifact.id)")
        } else {
            row.accessibilityIdentifier("morde.library.task-artifact.\(artifact.id)")
        }
    }

    @ViewBuilder
    private func accountRow(_ item: MIFLibraryAccountItem) -> some View {
        let handle = MIFLibraryPreviewPolicy.personalAttachmentHandle(for: item, access: accountAccess)
        let row = HStack(alignment: .top, spacing: 13) {
            fileTypeTile(fileTypeLabel(name: item.name, mediaType: item.mediaType,
                                      defaultLabel: item.kind == .media ? "图片" : "文件"))
            VStack(alignment: .leading, spacing: 2) {
                Text(item.name)
                    .font(.system(size: 16, weight: .semibold))
                    .tracking(-0.16)
                    .lineSpacing(1.5)
                    .fixedSize(horizontal: false, vertical: true)
                Text(accountDetail(item, metadataOnly: handle == nil))
                    .font(.system(size: 14))
                    .lineSpacing(3.5)
                    .foregroundStyle(MIFLibraryDesignPalette.secondaryText)
                    .fixedSize(horizontal: false, vertical: true)
            }
            .padding(.top, 1)
            Spacer(minLength: 8)
        }
        .padding(.vertical, 13)

        if let handle {
            Button { onPreview(handle) } label: { row }
                .buttonStyle(.plain)
                .contentShape(Rectangle())
                .accessibilityLabel(MIFLibraryPage.accountAttachmentPreviewAccessibilityLabel(for: item.name))
                .accessibilityIdentifier("morde.library.cloud.preview.\(item.id)")
        } else {
            row.accessibilityIdentifier("morde.library.cloud.\(item.id)")
        }
    }

    private func accountDetail(_ item: MIFLibraryAccountItem, metadataOnly: Bool) -> String {
        var parts = [fileTypeLabel(name: item.name, mediaType: item.mediaType,
                                   defaultLabel: item.kind == .media ? "图片" : "文件"),
                     item.hasAttachment ? AppLocalized("My uploads") : AppLocalized("Cloud")]
        if let size = item.sizeBytes, size >= 0 {
            parts.append(ByteCountFormatter.string(fromByteCount: size, countStyle: .file))
        }
        if metadataOnly { parts.append(AppLocalized("Metadata only")) }
        return parts.joined(separator: " · ")
    }

    private func localDetail(_ item: MIFLibraryLocalItem, metadataOnly: Bool) -> String {
        var parts = [item.kind == .media ? AppLocalized("Media") : AppLocalized("File"),
                     AppLocalized("On this iPhone")]
        if let origin = item.origin {
            switch origin {
            case .sharedFolder:
                parts.append(AppLocalized("Shared folder"))
            case .conversation(let title) where !title.isEmpty:
                parts.append(title)
            case .conversation:
                break
            }
        }
        if let size = item.sizeBytes, size >= 0 {
            parts.append(ByteCountFormatter.string(fromByteCount: size, countStyle: .file))
        }
        if let modifiedAt = item.modifiedAt {
            parts.append(modifiedAt.formatted(date: .abbreviated, time: .omitted))
        }
        if metadataOnly { parts.append(AppLocalized("Metadata only")) }
        return parts.joined(separator: " · ")
    }

    private func taskDetail(_ artifact: MIFLibraryTaskArtifact) -> String {
        var parts = [AppLocalized("Task"), AppLocalized("Artifacts")]
        if !artifact.mediaType.isEmpty {
            parts.append(fileTypeLabel(name: artifact.label, mediaType: artifact.mediaType, defaultLabel: "构件"))
        }
        if let size = artifact.sizeBytes, size >= 0 {
            parts.append(ByteCountFormatter.string(fromByteCount: size, countStyle: .file))
        }
        if let revision = artifact.workspaceRevision, revision >= 0 { parts.append("v\(revision)") }
        if MIFLibraryPreviewPolicy.taskHandle(
            for: artifact, access: taskAccess,
            workspaceSyncAvailable: workspaceSyncAvailable,
            taskMediaGenerationAvailable: taskMediaGenerationAvailable) == nil {
            parts.append(AppLocalized("Metadata only"))
        }
        return parts.isEmpty ? AppLocalized("Metadata only") : parts.joined(separator: " · ")
    }

    private func fileTypeLabel(name: String, mediaType: String?, defaultLabel: String) -> String {
        let type = mediaType?.split(separator: ";", maxSplits: 1).first.map { String($0).lowercased() }
        let ext = URL(fileURLWithPath: name).pathExtension.lowercased()
        if type == "application/pdf" || ext == "pdf" { return "PDF" }
        if type == "application/vnd.openxmlformats-officedocument.wordprocessingml.document" || ext == "docx" {
            return "DOCX"
        }
        if type?.hasPrefix("image/") == true || ["png", "jpg", "jpeg", "gif", "heic", "webp"].contains(ext) {
            return "图片"
        }
        if defaultLabel == "构件" { return defaultLabel }
        return ext.isEmpty ? defaultLabel : String(ext.prefix(4)).uppercased()
    }

    private func fileTypeTile(_ label: String) -> some View {
        let normalized = label.uppercased()
        let fill: Color
        let foreground: Color
        switch normalized {
        case "PDF":
            #if MORDE_STANDALONE
            fill = Color(red: 253.0 / 255.0, green: 236.0 / 255.0, blue: 236.0 / 255.0)
            foreground = Color(red: 200.0 / 255.0, green: 30.0 / 255.0, blue: 30.0 / 255.0)
            #else
            fill = Color(red: 0.992, green: 0.925, blue: 0.925)
            foreground = Color(red: 0.78, green: 0.12, blue: 0.12)
            #endif
        case "DOC", "DOCX":
            #if MORDE_STANDALONE
            fill = Color(red: 232.0 / 255.0, green: 240.0 / 255.0, blue: 254.0 / 255.0)
            foreground = Color(red: 26.0 / 255.0, green: 86.0 / 255.0, blue: 219.0 / 255.0)
            #else
            fill = Color(red: 0.91, green: 0.94, blue: 0.99)
            foreground = Color(red: 0.10, green: 0.34, blue: 0.86)
            #endif
        case "图片", "IMAGE":
            #if MORDE_STANDALONE
            fill = Color(red: 230.0 / 255.0, green: 247.0 / 255.0, blue: 236.0 / 255.0)
            foreground = Color(red: 30.0 / 255.0, green: 142.0 / 255.0, blue: 62.0 / 255.0)
            #else
            fill = Color(red: 0.90, green: 0.97, blue: 0.92)
            foreground = Color(red: 0.12, green: 0.55, blue: 0.24)
            #endif
        case "构件", "ART":
            fill = .black
            foreground = .white
        default:
            fill = MIFLibraryDesignPalette.defaultTypeFill
            foreground = MIFLibraryDesignPalette.defaultTypeText
        }
        return Text(label)
            .font(.system(size: 8.5, weight: .heavy))
            .tracking(0.17)
            .lineLimit(1)
            .minimumScaleFactor(0.7)
            .foregroundStyle(foreground)
            .frame(width: 32, height: 38)
            .background(fill, in: RoundedRectangle(cornerRadius: 7, style: .continuous))
    }
}

@MainActor
struct MIFLibraryRoot: View {
    var chrome: NanoMuseRoomChrome? = nil
    var onMenu: (() -> Void)? = nil
    var personalDataRoot: URL = MordePersonalStorage.root
    var previewNavigation: MordeFilePreviewNavigation? = nil
    @ObservedObject private var personal = NanoMuseMordePersonalContentStore.shared
    @ObservedObject private var authorization = MordeShellAuthorization.shared
    @ObservedObject private var taskStore = MIFTaskPageStore.shared
    @State private var localAccess: MIFLibraryAccessState = .loading
    @State private var localItems: [MIFLibraryLocalItem] = []
    @State private var personalSnapshotScopeKey: String?
    @State private var taskPreviewHandles: [String: MIFLibraryPreviewHandle] = [:]
    @State private var localPreviewTargets: [MIFLibraryPreviewHandle: NanoMuseLibraryEntry] = [:]
    @State private var taskPreviewTargets: [MIFLibraryPreviewHandle: MIFLibraryTaskPreviewTarget] = [:]
    @State private var personalAttachmentHandles: [String: MIFLibraryPreviewHandle] = [:]
    @State private var personalAttachmentTargets: [MIFLibraryPreviewHandle: MIFLibraryAttachmentPreviewTarget] = [:]
    @State private var previewItem: MIFLibraryPreviewItem?
    @State private var personalAttachmentDownload: MordePersonalAttachmentDownload?
    @State private var personalAttachmentDownloadTask: Task<Void, Never>?
    @State private var personalAttachmentRequestID: UUID?
#if DEBUG
    @State private var localAttachmentPreviewIsolationFixtureStarted = false
#endif
    @State private var previewUnavailable = false
    @State private var previewUnavailableAfterDismissal = false

    private struct MIFLibraryTaskPreviewTarget {
        let workspaceID: String
        let file: MordeWorkspaceFile
        let scopeKey: String
        let artifact: MIFLibraryTaskArtifact
    }

    private struct MIFLibraryPreviewItem: Identifiable {
        let id: UUID
        let url: URL?
        let fileName: String?
        var attachmentName: String? = nil
        var attachmentMediaType: String? = nil

        init(id: UUID = UUID(), url: URL? = nil,
             fileName: String? = nil,
             attachmentName: String? = nil, attachmentMediaType: String? = nil) {
            self.id = id
            self.url = url
            self.fileName = fileName ?? attachmentName
            self.attachmentName = attachmentName
            self.attachmentMediaType = attachmentMediaType
        }

        var isPersonalAttachment: Bool { attachmentName != nil && attachmentMediaType != nil }
    }

    private struct MIFLibraryAttachmentPreviewTarget: Equatable, Sendable {
        let objectID: String
        let attachmentID: String
        let name: String
        let mediaType: String
        let scopeKey: String
    }

    private var taskAccess: MIFLibraryAccessState {
        guard taskStore.isSignedIn else { return .denied }
        guard let available = taskStore.taskAPIAvailable else { return .loading }
        return available ? .available : .unavailable
    }

    private var accountAccess: MIFLibraryAccessState {
        guard authorization.signedIn else { return .denied }
        guard let scope = authorization.personalDataScopeKey else { return .unavailable }
        guard personalSnapshotScopeKey == scope else { return .loading }
        guard personal.syncEnabled else { return .unavailable }
        if personal.isLoading { return .loading }
        return personal.error == nil ? .available : .unavailable
    }

    private var taskArtifacts: [MIFLibraryTaskArtifact] {
        taskArtifactTasks.flatMap { task in
            (task.artifacts ?? []).map { artifact in
                MIFLibraryTaskArtifact(id: artifact.id, label: artifact.label, mediaType: artifact.media_type,
                                       workspaceID: artifact.workspace_id, workspaceRevision: artifact.workspace_revision,
                                       sizeBytes: artifact.size_bytes, sha256: artifact.sha256,
                                       verified: artifact.verified,
                                       previewHandle: taskPreviewHandles[Self.taskArtifactKey(taskID: task.id,
                                                                                           artifactID: artifact.id)])
            }
        }
    }

    private var taskArtifactTasks: [MordeTask] {
        let summaries = taskStore.tasks
        let details = taskStore.details
        let summaryIDs = Set(summaries.map(\.id))
        return summaries.map { details[$0.id] ?? $0 } +
            details.values.filter { !summaryIDs.contains($0.id) }.sorted { $0.id < $1.id }
    }

    private var taskArtifactPreviewKey: String {
        var values = [taskStore.scopeKey ?? "", String(describing: taskStore.isSignedIn),
                      String(describing: taskStore.taskAPIAvailable),
                      String(describing: taskStore.workspaceSyncAvailable),
                      String(describing: taskStore.taskMediaGenerationAvailable)]
        for task in taskArtifactTasks {
            for artifact in task.artifacts ?? [] {
                values.append(contentsOf: [task.id, artifact.id, artifact.workspace_id ?? "",
                                           artifact.workspace_revision.map(String.init) ?? "",
                                           artifact.path ?? "", artifact.size_bytes.map(String.init) ?? "",
                                           artifact.sha256 ?? "", String(artifact.verified)])
            }
        }
        return values.joined(separator: "\u{1f}")
    }

    private var accountItems: [MIFLibraryAccountItem] {
        personal.values("library_item").compactMap { item -> MIFLibraryAccountItem? in
            guard let body = try? item.decodeBody(NanoMuseMordeLibraryItemBody.self), body.isValid else { return nil }
            return MIFLibraryAccountItem(id: item.object_id, name: body.name,
                                         kind: body.kind == "media" ? .media : .artifact,
                                         mediaType: body.mime, sizeBytes: body.size_bytes,
                                         attachmentID: body.attachment_id,
                                         previewHandle: personalAttachmentHandles[item.object_id])
        }.sorted { $0.name.localizedCaseInsensitiveCompare($1.name) == .orderedAscending }
    }

    var body: some View {
        MIFLibraryPage(chrome: chrome, onMenu: onMenu, localAccess: localAccess,
                       taskAccess: taskAccess, accountAccess: accountAccess,
                       workspaceSyncAvailable: taskStore.workspaceSyncAvailable,
                       taskMediaGenerationAvailable: taskStore.taskMediaGenerationAvailable,
                       localItems: localItems, taskArtifacts: taskArtifacts, accountItems: accountItems,
                       onPreview: preview)
            .fullScreenCover(item: personalPreviewBinding, onDismiss: previewDismissed) { item in
                if let name = item.attachmentName, let mediaType = item.attachmentMediaType {
                    MIFPersonalAttachmentPreview(
                        item: MIFPersonalAttachmentPreviewItem(fileURL: item.url, name: name, mediaType: mediaType),
                        navigation: previewNavigation,
                        onClose: { dismissPersonalAttachmentPreview() })
                } else {
                    Text(AppLocalized("Artifact details unavailable"))
                        .foregroundStyle(NanoMuseTones.secondaryText)
                }
            }
            .fullScreenCover(item: nonPersonalPreviewBinding, onDismiss: previewDismissed) { item in
                if let url = item.url {
                    MIFQuickLookFilePreview(
                        url: url,
                        title: item.fileName ?? url.lastPathComponent,
                        navigation: previewNavigation,
                        onClose: { previewItem = nil })
                } else {
                    ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
                }
            }
            .alert(AppLocalized("Unavailable"), isPresented: $previewUnavailable) {
                Button(AppLocalized("Done"), role: .cancel) {}
            } message: {
                Text(AppLocalized("Artifact details unavailable"))
            }
            .task(id: authorization.personalDataScopeKey) { await refresh() }
            .task(id: taskArtifactPreviewKey) { await refreshTaskPreviews(scopeKey: taskStore.scopeKey) }
            .nmOnChange(of: authorization.personalDataScopeKey) { _ in clearPersonalAttachmentPreview() }
            .nmOnChange(of: personal.syncEnabled) { enabled in
                if !enabled { clearPersonalAttachmentPreview() }
            }
            .nmOnChange(of: personal.error) { error in
                if error != nil { clearPersonalAttachmentPreview() }
            }
            .onDisappear { clearPersonalAttachmentPreview() }
    }

    private func refresh() async {
        let scope = authorization.personalDataScopeKey
        personalSnapshotScopeKey = nil
        localItems = []
        localPreviewTargets = [:]
        personalAttachmentHandles = [:]
        personalAttachmentTargets = [:]
        previewItem = nil
        clearPersonalAttachmentPreview()
        localAccess = .loading
        await personal.refresh(root: personalDataRoot)
        guard !Task.isCancelled, authorization.personalDataScopeKey == scope else { return }

        var attachmentHandles: [String: MIFLibraryPreviewHandle] = [:]
        var attachmentTargets: [MIFLibraryPreviewHandle: MIFLibraryAttachmentPreviewTarget] = [:]
        if authorization.signedIn, personal.syncEnabled, !personal.isLoading, personal.error == nil,
           let scope {
            for object in personal.values("library_item") {
                guard let body = try? object.decodeBody(NanoMuseMordeLibraryItemBody.self), body.isValid,
                      let attachmentID = body.attachment_id,
                      MordePersonalDataContract.isValidAttachmentID(attachmentID),
                      let mediaType = MIFLibraryPreviewPolicy.normalizedPersonalAttachmentMediaType(body.mime)
                else { continue }
                let handle = MIFLibraryPreviewHandle(id: UUID())
                attachmentHandles[object.object_id] = handle
                attachmentTargets[handle] = MIFLibraryAttachmentPreviewTarget(
                    objectID: object.object_id, attachmentID: attachmentID, name: body.name,
                    mediaType: mediaType, scopeKey: scope)
            }
        }

        let titles = await visibleSessionTitles()
        let scan = await scanLocalLibrary(titles: titles)
        guard !Task.isCancelled, authorization.personalDataScopeKey == scope else { return }
        personalAttachmentHandles = attachmentHandles
        personalAttachmentTargets = attachmentTargets
        let visibleEntries = scan.entries.filter { Self.safeLocalURL(for: $0, titles: titles) != nil }
        var targets: [MIFLibraryPreviewHandle: NanoMuseLibraryEntry] = [:]
        localItems = visibleEntries.map { entry in
            let handle = MIFLibraryPreviewHandle(id: UUID())
            targets[handle] = entry
            return MIFLibraryLocalItem(id: UUID(), name: entry.name,
                                       kind: entry.kind == .media ? .media : .artifact,
                                       sizeBytes: entry.size, modifiedAt: entry.modified,
                                       origin: entry.sessionId == nil ? .sharedFolder : .conversation(title: entry.sessionTitle ?? ""),
                                       previewHandle: handle)
        }
        localPreviewTargets = targets
        localAccess = scan.access
        personalSnapshotScopeKey = scope
    }

    private func refreshTaskPreviews(scopeKey: String?) async {
        taskPreviewHandles = [:]
        taskPreviewTargets = [:]
        if previewItem?.isPersonalAttachment != true { previewItem = nil }
        guard let scopeKey, taskAccess == .available, taskStore.workspaceSyncAvailable == true else { return }

        var handles: [String: MIFLibraryPreviewHandle] = [:]
        var targets: [MIFLibraryPreviewHandle: MIFLibraryTaskPreviewTarget] = [:]
        for task in taskArtifactTasks {
            for artifact in task.artifacts ?? [] {
                guard !Task.isCancelled,
                      let workspaceID = artifact.workspace_id,
                      let revision = artifact.workspace_revision,
                      let path = artifact.path,
                      let sizeBytes = artifact.size_bytes,
                      let sha256 = artifact.sha256,
                      artifact.verified,
                      Self.taskMediaAllowed(mediaType: artifact.media_type,
                                            taskMediaGenerationAvailable: taskStore.taskMediaGenerationAvailable)
                else { continue }

                do {
                    let file = try await taskStore.verifiedTaskArtifactFile(
                        workspaceID: workspaceID, revision: revision, path: path,
                        sizeBytes: sizeBytes, sha256: sha256, scopeKey: scopeKey)
                    guard !Task.isCancelled, taskStore.scopeKey == scopeKey,
                          taskAccess == .available, taskStore.workspaceSyncAvailable == true,
                          Self.taskMediaAllowed(mediaType: artifact.media_type,
                                                taskMediaGenerationAvailable: taskStore.taskMediaGenerationAvailable)
                    else { return }

                    let handle = MIFLibraryPreviewHandle(id: UUID())
                    let libraryArtifact = MIFLibraryTaskArtifact(
                        id: artifact.id, label: artifact.label, mediaType: artifact.media_type,
                        workspaceID: workspaceID, workspaceRevision: revision,
                        sizeBytes: sizeBytes, sha256: sha256, verified: true, previewHandle: handle)
                    guard MIFLibraryPreviewPolicy.taskHandle(
                        for: libraryArtifact, access: taskAccess,
                        workspaceSyncAvailable: taskStore.workspaceSyncAvailable,
                        taskMediaGenerationAvailable: taskStore.taskMediaGenerationAvailable) == handle else { continue }
                    let key = Self.taskArtifactKey(taskID: task.id, artifactID: artifact.id)
                    handles[key] = handle
                    targets[handle] = MIFLibraryTaskPreviewTarget(
                        workspaceID: workspaceID, file: file, scopeKey: scopeKey, artifact: libraryArtifact)
                } catch {
                    // Missing or mismatched local proofs remain metadata only.
                    continue
                }
            }
        }
        guard !Task.isCancelled, taskStore.scopeKey == scopeKey else { return }
        taskPreviewHandles = handles
        taskPreviewTargets = targets
    }

    private func preview(_ handle: MIFLibraryPreviewHandle) {
        Task { @MainActor in
            if let target = personalAttachmentTargets[handle] {
                await previewPersonalAttachment(target, handle: handle)
                return
            }
            if let entry = localPreviewTargets[handle] {
                do {
                    let scope = authorization.personalDataScopeKey
                    let titles = await visibleSessionTitles()
                    let scan = await scanLocalLibrary(titles: titles)
                    guard authorization.personalDataScopeKey == scope else { throw CancellationError() }
                    guard scan.access == .available,
                          let current = scan.entries.first(where: { $0.url.standardizedFileURL == entry.url.standardizedFileURL &&
                                                                      $0.sessionId == entry.sessionId &&
                                                                      $0.relativePath == entry.relativePath &&
                                                                      $0.size == entry.size && $0.modified == entry.modified }),
                          let url = Self.safeLocalURL(for: current, titles: titles) else {
                        previewUnavailable = true
                        return
                    }
                    guard authorization.personalDataScopeKey == scope else {
                        throw CancellationError()
                    }
                    previewItem = MIFLibraryPreviewItem(url: url, fileName: current.name)
                } catch is CancellationError {
                    return
                } catch {
                    previewUnavailable = true
                }
                return
            }

            guard let target = taskPreviewTargets[handle],
                  target.scopeKey == taskStore.scopeKey,
                  MIFLibraryPreviewPolicy.taskHandle(
                    for: target.artifact, access: taskAccess,
                    workspaceSyncAvailable: taskStore.workspaceSyncAvailable,
                    taskMediaGenerationAvailable: taskStore.taskMediaGenerationAvailable) == handle else {
                previewUnavailable = true
                return
            }
            do {
                let url = try await taskStore.verifyLocalTaskArtifactFile(
                    workspaceID: target.workspaceID, file: target.file, scopeKey: target.scopeKey)
                guard target.scopeKey == taskStore.scopeKey,
                      MIFLibraryPreviewPolicy.taskHandle(
                        for: target.artifact, access: taskAccess,
                        workspaceSyncAvailable: taskStore.workspaceSyncAvailable,
                        taskMediaGenerationAvailable: taskStore.taskMediaGenerationAvailable) == handle else {
                    throw CancellationError()
                }
                previewItem = MIFLibraryPreviewItem(url: url, fileName: target.artifact.label)
            } catch is CancellationError {
                return
            } catch {
                previewUnavailable = true
            }
        }
    }

    private func previewPersonalAttachment(_ target: MIFLibraryAttachmentPreviewTarget,
                                           handle: MIFLibraryPreviewHandle) async {
        clearPersonalAttachmentPreview()
        guard personalAttachmentTargets[handle] == target,
              personalAttachmentTargetIsCurrent(target) else {
            previewUnavailable = true
            return
        }
        previewUnavailable = false
        let sheetItem = MIFLibraryPreviewItem(
            url: nil, attachmentName: target.name, attachmentMediaType: target.mediaType)
        previewItem = sheetItem
        let requestID = UUID()
        personalAttachmentRequestID = requestID
        personalAttachmentDownloadTask = Task { @MainActor in
            do {
                let downloaded = try await authorization.downloadPersonalAttachment(attachmentID: target.attachmentID)
                guard !Task.isCancelled, personalAttachmentRequestID == requestID else {
                    MIFLibraryTemporaryFileCleanup.discard(downloaded.fileURL)
                    return
                }
                guard downloaded.attachmentID == target.attachmentID,
                      MIFLibraryPreviewPolicy.normalizedPersonalAttachmentMediaType(downloaded.mediaType) == target.mediaType,
                      personalAttachmentTargetIsCurrent(target) else {
                    MIFLibraryTemporaryFileCleanup.discard(downloaded.fileURL)
                    personalAttachmentRequestID = nil
                    personalAttachmentDownloadTask = nil
                    previewItem = nil
                    previewUnavailableAfterDismissal = true
                    return
                }
                personalAttachmentDownload = downloaded
                personalAttachmentRequestID = nil
                personalAttachmentDownloadTask = nil
                previewItem = MIFLibraryPreviewItem(id: sheetItem.id, url: downloaded.fileURL,
                                                    attachmentName: target.name, attachmentMediaType: target.mediaType)
            } catch {
                guard personalAttachmentRequestID == requestID else { return }
                personalAttachmentRequestID = nil
                personalAttachmentDownloadTask = nil
                previewItem = nil
                previewUnavailableAfterDismissal = true
            }
        }
#if DEBUG
        scheduleLocalAttachmentPreviewIsolationFixture(scopeKey: target.scopeKey, requestID: requestID)
#endif
        await personalAttachmentDownloadTask?.value
    }

#if DEBUG
    private func scheduleLocalAttachmentPreviewIsolationFixture(scopeKey: String, requestID: UUID) {
        let arguments = ProcessInfo.processInfo.arguments
        let changesAccount = arguments.contains("-morde.localAttachmentPreviewAccountChangeFixture")
        let disablesSync = arguments.contains("-morde.localAttachmentPreviewSyncDisableFixture")
        guard (changesAccount || disablesSync), !localAttachmentPreviewIsolationFixtureStarted else { return }
        localAttachmentPreviewIsolationFixtureStarted = true
        Task { @MainActor in
            try? await Task.sleep(nanoseconds: 1_500_000_000)
            guard personalAttachmentRequestID == requestID else { return }
            if changesAccount {
                NotificationCenter.default.post(
                    name: Notification.Name("morde.localAttachmentPreviewChangeAccount"), object: nil)
            } else {
                NanoMuseMordeProfileSyncPreference.setEnabled(false, scopeKey: scopeKey)
                await personal.refresh(root: personalDataRoot)
                // Keep the test fixture from persisting a preference into later Simulator tests.
                NanoMuseMordeProfileSyncPreference.setEnabled(true, scopeKey: scopeKey)
            }
        }
    }
#endif

    private func personalAttachmentTargetIsCurrent(_ target: MIFLibraryAttachmentPreviewTarget) -> Bool {
        guard authorization.signedIn, authorization.personalDataScopeKey == target.scopeKey,
              personalSnapshotScopeKey == target.scopeKey,
              personal.syncEnabled, !personal.isLoading, personal.error == nil else { return false }
        return accountItems.contains { item in
            item.id == target.objectID && item.name == target.name &&
                item.attachmentID == target.attachmentID &&
                MIFLibraryPreviewPolicy.normalizedPersonalAttachmentMediaType(item.mediaType) == target.mediaType
        }
    }

    private func clearPersonalAttachmentPreview() {
        dismissPersonalAttachmentPreview()
        if let downloaded = personalAttachmentDownload {
            MIFLibraryTemporaryFileCleanup.discard(downloaded.fileURL)
            personalAttachmentDownload = nil
        }
    }

    private func dismissPersonalAttachmentPreview() {
        personalAttachmentDownloadTask?.cancel()
        personalAttachmentDownloadTask = nil
        personalAttachmentRequestID = nil
        if previewItem?.isPersonalAttachment == true { previewItem = nil }
    }

    private func previewDismissed() {
        clearPersonalAttachmentPreview()
        if previewUnavailableAfterDismissal {
            previewUnavailableAfterDismissal = false
            previewUnavailable = true
        }
    }

    private var personalPreviewBinding: Binding<MIFLibraryPreviewItem?> {
        Binding(get: { previewItem?.isPersonalAttachment == true ? previewItem : nil },
                set: { if $0 == nil, previewItem?.isPersonalAttachment == true { previewItem = nil } })
    }

    private var nonPersonalPreviewBinding: Binding<MIFLibraryPreviewItem?> {
        Binding(get: { previewItem?.isPersonalAttachment == false ? previewItem : nil },
                set: { if $0 == nil, previewItem?.isPersonalAttachment == false { previewItem = nil } })
    }

    private func visibleSessionTitles() async -> [String: String] {
#if MORDE_STANDALONE
        [:]
#else
        let sessions = NanoMuseSync.shared.visible(await ChatStore.shared.listSessions())
        var titles: [String: String] = [:]
        for session in sessions { titles[session.id] = session.title ?? "" }
        return titles
#endif
    }

    private func scanLocalLibrary(titles: [String: String]) async -> (entries: [NanoMuseLibraryEntry], access: MIFLibraryAccessState) {
#if MORDE_STANDALONE
        let result = await Task.detached(priority: .utility) {
            NanoMuseLibraryIndex.scanResult(titles: titles)
        }.value
        return (result ?? [], result == nil ? .unavailable : .available)
#else
        let entries = await Task.detached(priority: .utility) {
            NanoMuseLibraryIndex.scan(titles: titles)
        }.value
        return (entries, .available)
#endif
    }

    private static func taskArtifactKey(taskID: String, artifactID: String) -> String {
        "\(taskID):\(artifactID)"
    }

    private static func taskMediaAllowed(mediaType: String, taskMediaGenerationAvailable: Bool?) -> Bool {
        let type = mediaType.lowercased()
        return !(type.hasPrefix("image/") || type.hasPrefix("video/") || type.hasPrefix("audio/")) ||
            taskMediaGenerationAvailable == true
    }

    private static func safeLocalURL(for entry: NanoMuseLibraryEntry, titles: [String: String]) -> URL? {
        let root: URL
#if MORDE_STANDALONE
        guard entry.sessionId == nil else { return nil }
        root = FileManager.default.urls(for: .documentDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("Morde Workspace Files", isDirectory: true)
#else
        if let sessionID = entry.sessionId {
            guard titles[sessionID] != nil else { return nil }
            root = MinisStoragePaths.minisWorkspacePersistentDir(for: sessionID)
        } else {
            root = MinisStoragePaths.minisSharedPersistentDir
        }
#endif
        guard (try? MordeWorkspaceTransferSupport.validatePath(entry.relativePath)) != nil else { return nil }
        let rootURL = root.standardizedFileURL
        let fileURL = entry.url.standardizedFileURL
        guard fileURL.path.hasPrefix(rootURL.path + "/"),
              rootURL.appendingPathComponent(entry.relativePath).standardizedFileURL == fileURL else { return nil }

        var rootInfo = stat()
        guard lstat(rootURL.path, &rootInfo) == 0,
              (rootInfo.st_mode & mode_t(S_IFMT)) == mode_t(S_IFDIR) else { return nil }
        let components = entry.relativePath.split(separator: "/")
        var current = rootURL
        for (index, component) in components.enumerated() {
            current.appendPathComponent(String(component))
            var info = stat()
            guard lstat(current.path, &info) == 0 else { return nil }
            let type = info.st_mode & mode_t(S_IFMT)
            let isFile = index == components.count - 1
            guard type == (isFile ? mode_t(S_IFREG) : mode_t(S_IFDIR)) else { return nil }
        }
        return fileURL
    }
}

struct MIFPersonalAttachmentPreviewItem: Identifiable {
    let id = UUID()
    let fileURL: URL?
    let name: String
    let mediaType: String

    var isLoading: Bool { fileURL == nil }
}

@MainActor
enum MIFLibraryTemporaryFileCleanup {
    private(set) static var pendingFiles = Set<URL>()

    static func discard(_ url: URL) {
        guard let url = temporaryFileURL(url) else { return }
        retryPending()
        remove(url)
    }

    static func retryPending() {
        for url in Array(pendingFiles) { remove(url) }
    }

    private static func remove(_ url: URL) {
        guard let url = temporaryFileURL(url) else {
            pendingFiles.remove(url)
            return
        }

        var metadata = stat()
        let statResult = lstat(url.path, &metadata)
        let statError = errno
        guard statResult == 0 else {
            if statError == ENOENT || statError == ENOTDIR {
                pendingFiles.remove(url)
            } else {
                pendingFiles.insert(url)
            }
            return
        }
        guard (metadata.st_mode & mode_t(S_IFMT)) == mode_t(S_IFREG) else {
            pendingFiles.remove(url)
            return
        }

        if unlink(url.path) == 0 {
            pendingFiles.remove(url)
        } else {
            retainIfRemovalFailed(url)
        }
    }

    private static func retainIfRemovalFailed(_ url: URL) {
        var metadata = stat()
        let result = lstat(url.path, &metadata)
        let error = errno
        if result == 0 {
            if (metadata.st_mode & mode_t(S_IFMT)) == mode_t(S_IFREG) {
                pendingFiles.insert(url)
            } else {
                pendingFiles.remove(url)
            }
        } else if error != ENOENT && error != ENOTDIR {
            pendingFiles.insert(url)
        } else {
            pendingFiles.remove(url)
        }
    }

    private static func temporaryFileURL(_ url: URL) -> URL? {
        guard url.isFileURL else { return nil }
        let candidate = url.standardizedFileURL
        let temporaryDirectory = FileManager.default.temporaryDirectory.standardizedFileURL
        guard candidate.path.hasPrefix(temporaryDirectory.path + "/") else { return nil }
        return candidate
    }
}

struct MIFPersonalAttachmentPreview: View {
    let item: MIFPersonalAttachmentPreviewItem
    var navigation: MordeFilePreviewNavigation? = nil
    let onClose: () -> Void

    @State private var image: UIImage?
    @State private var imageLoaded = false
    @State private var pdfAvailable: Bool?

    private var mediaType: String {
        MIFLibraryPreviewPolicy.normalizedPersonalAttachmentMediaType(item.mediaType) ?? ""
    }

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 10) {
                NanoMuseRoundButton(symbol: "chevron.left", label: AppLocalized("Back"), size: 38,
                                    action: onClose)
                .accessibilityIdentifier("morde.attachment.preview.back")

                Text(item.name)
                    .font(.headline)
                    .tracking(-0.17)
                    .lineLimit(1)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .accessibilityIdentifier("morde.attachment.preview.title")
            }
            .padding(.horizontal, 22)
            .padding(.top, 10)
            .padding(.bottom, 4)
            .background(ChatColors.background)

            ScrollView {
                Group {
                    if let fileURL = item.fileURL, mediaType == "application/pdf" {
                        if let pdfAvailable {
                            if pdfAvailable {
                                MIFPersonalPDFPreview(fileURL: fileURL)
                                    .accessibilityIdentifier("morde.attachment.preview.pdf")
                            } else {
                                unavailableContent
                            }
                        } else {
                            ProgressView().frame(maxWidth: .infinity, minHeight: 440)
                        }
                    } else if item.fileURL != nil, imageLoaded {
                        if let image {
                            Image(uiImage: image)
                                .resizable()
                                .scaledToFit()
                                .frame(maxWidth: .infinity, maxHeight: 440)
                                .padding(12)
                                .accessibilityIdentifier("morde.attachment.preview.image")
                        } else {
                            unavailableContent
                        }
                    } else {
                        ProgressView().frame(maxWidth: .infinity, minHeight: 440)
                    }
                }
                .frame(maxWidth: .infinity, minHeight: 440)
                .padding(.horizontal, 19)
                .padding(.vertical, 22)
                .background(NanoMuseTones.surface,
                            in: RoundedRectangle(cornerRadius: 18, style: .continuous))
                .overlay {
                    RoundedRectangle(cornerRadius: 18, style: .continuous)
                        .stroke(NanoMuseTones.hairline, lineWidth: 0.5)
                }
                .padding(.horizontal, 22)
                .padding(.top, 2)
            }
            .accessibilityIdentifier("morde.attachment.preview.scroll")

            if let navigation {
                MordeDesignTabBar(selectedTab: navigation.selectedTab,
                                  approvalCount: navigation.approvalCount) { tab in
                    onClose()
                    navigation.onSelectTab(tab)
                }
                .padding(.horizontal, 16)
                .padding(.top, 8)
                .padding(.bottom, 16)
            }
        }
        .background(ChatColors.background.ignoresSafeArea())
        .task(id: item.fileURL) {
            guard let fileURL = item.fileURL else {
                image = nil
                imageLoaded = false
                pdfAvailable = nil
                return
            }
            if mediaType == "application/pdf" {
                pdfAvailable = (PDFDocument(url: fileURL)?.pageCount ?? 0) > 0
            } else {
                image = UIImage(contentsOfFile: fileURL.path)
                imageLoaded = true
            }
        }
    }

    private var unavailableContent: some View {
        Text(AppLocalized("Artifact details unavailable"))
            .font(.body)
            .foregroundStyle(NanoMuseTones.secondaryText)
            .frame(maxWidth: .infinity, minHeight: 440)
    }
}

private struct MIFQuickLookFilePreview: View {
    let url: URL
    let title: String
    var navigation: MordeFilePreviewNavigation? = nil
    let onClose: () -> Void

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 10) {
                NanoMuseRoundButton(symbol: "chevron.left", label: AppLocalized("Back"), size: 38,
                                    action: onClose)
                .accessibilityIdentifier("morde.attachment.preview.back")

                Text(title)
                    .font(.headline)
                    .tracking(-0.17)
                    .lineLimit(1)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .accessibilityIdentifier("morde.attachment.preview.title")
            }
            .padding(.horizontal, 22)
            .padding(.top, 10)
            .padding(.bottom, 4)
            .background(ChatColors.background)

            GeometryReader { geometry in
                let shape = RoundedRectangle(cornerRadius: 18, style: .continuous)
                NanoMuseQuickLook(url: url)
                    .frame(width: geometry.size.width - 44, height: geometry.size.height - 2)
                    .background(NanoMuseTones.surface, in: shape)
                    .clipShape(shape)
                    .overlay { shape.stroke(NanoMuseTones.hairline, lineWidth: 0.5) }
                    .padding(.horizontal, 22)
                    .padding(.top, 2)
                    .accessibilityIdentifier("morde.attachment.preview.quickLook")
            }

            if let navigation {
                MordeDesignTabBar(selectedTab: navigation.selectedTab,
                                  approvalCount: navigation.approvalCount) { tab in
                    onClose()
                    navigation.onSelectTab(tab)
                }
                .padding(.horizontal, 16)
                .padding(.top, 8)
                .padding(.bottom, 16)
            }
        }
        .background(ChatColors.background.ignoresSafeArea())
    }
}

private struct MIFPersonalPDFPreview: UIViewRepresentable {
    let fileURL: URL

    func makeUIView(context: Context) -> PDFView {
        let view = PDFView()
        view.autoScales = true
        view.displayMode = .singlePageContinuous
        view.displayDirection = .vertical
        view.document = PDFDocument(url: fileURL)
        return view
    }

    func updateUIView(_ view: PDFView, context: Context) {
        if view.document?.documentURL != fileURL {
            view.document = PDFDocument(url: fileURL)
        }
    }
}
