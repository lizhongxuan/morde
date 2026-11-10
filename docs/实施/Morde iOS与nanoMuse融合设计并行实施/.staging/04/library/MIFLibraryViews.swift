import SwiftUI

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
    case local
    case taskArtifacts
    case cloud

    var id: Self { self }
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
    let sizeBytes: Int64?
}

/// Pure gate for page actions. The caller owns permission checks and handle
/// creation; this view only exposes a callback for an already verified handle.
enum MIFLibraryPreviewPolicy {
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

struct MIFLibraryPage: View {
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

    var body: some View {
        VStack(spacing: 0) {
            Picker(AppLocalized("Library"), selection: $source) {
                Text(AppLocalized("All")).tag(MIFLibrarySource.all)
                Text(AppLocalized("On this iPhone")).tag(MIFLibrarySource.local)
                Text(AppLocalized("Artifacts")).tag(MIFLibrarySource.taskArtifacts)
                Text(AppLocalized("Cloud")).tag(MIFLibrarySource.cloud)
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            .padding(.horizontal, 16)
            .padding(.vertical, 8)

            List {
                if source == .all || source == .local { localSection }
                if source == .all || source == .taskArtifacts { taskSection }
                if source == .all || source == .cloud { accountSection }
            }
            .listStyle(.plain)
            .scrollContentBackground(.hidden)
        }
        .background(ChatColors.background.ignoresSafeArea())
        .tint(NanoMuseTones.action)
    }

    @ViewBuilder
    private var localSection: some View {
        Section {
            switch localAccess {
            case .loading:
                ProgressView().frame(maxWidth: .infinity)
            case .denied, .unavailable:
                Text(AppLocalized("Unavailable"))
                    .foregroundStyle(.secondary)
            case .available where localItems.isEmpty:
                Text(AppLocalized("Nothing created yet"))
                    .foregroundStyle(.secondary)
            case .available:
                ForEach(localItems) { item in localRow(item) }
            }
        } header: {
            Text(AppLocalized("On this iPhone"))
        }
    }

    @ViewBuilder
    private var taskSection: some View {
        Section {
            switch taskAccess {
            case .loading:
                ProgressView().frame(maxWidth: .infinity)
            case .denied, .unavailable:
                Text(AppLocalized("Artifact details unavailable"))
                    .foregroundStyle(.secondary)
            case .available where taskArtifacts.isEmpty:
                Text(AppLocalized("No delivered artifacts"))
                    .foregroundStyle(.secondary)
            case .available:
                ForEach(taskArtifacts) { artifact in taskArtifactRow(artifact) }
            }
        } header: {
            Text("\(AppLocalized("Task")) · \(AppLocalized("Artifacts"))")
        }
    }

    @ViewBuilder
    private var accountSection: some View {
        Section {
            switch accountAccess {
            case .loading:
                ProgressView().frame(maxWidth: .infinity)
            case .denied, .unavailable:
                Text(AppLocalized("Unavailable"))
                    .foregroundStyle(.secondary)
            case .available where accountItems.isEmpty:
                Text(AppLocalized("No cloud files yet"))
                    .foregroundStyle(.secondary)
            case .available:
                ForEach(accountItems) { item in accountRow(item) }
            }
        } header: {
            Text(AppLocalized("Cloud"))
        }
    }

    private func localRow(_ item: MIFLibraryLocalItem) -> some View {
        HStack(spacing: 12) {
            Image(systemName: item.kind == .media ? "photo" : "doc.text")
                .font(.title3)
                .foregroundStyle(.secondary)
                .frame(width: 28)
            VStack(alignment: .leading, spacing: 3) {
                Text(item.name).font(.body).lineLimit(1)
                if let detail = localDetail(item) {
                    Text(detail).font(.caption).foregroundStyle(.secondary).lineLimit(2)
                }
            }
            Spacer(minLength: 8)
            if let handle = MIFLibraryPreviewPolicy.localHandle(for: item, access: localAccess) {
                Button {
                    onPreview(handle)
                } label: {
                    Label(AppLocalized("Preview"), systemImage: "eye")
                        .labelStyle(.iconOnly)
                }
                .buttonStyle(.plain)
                .accessibilityLabel(AppLocalized("Preview"))
            } else {
                Text(AppLocalized("Metadata only"))
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .padding(.vertical, 4)
        .accessibilityIdentifier("morde.library.local.\(item.id.uuidString)")
    }

    private func taskArtifactRow(_ artifact: MIFLibraryTaskArtifact) -> some View {
        HStack(spacing: 12) {
            Image(systemName: artifact.mediaType.lowercased().hasPrefix("image/") ? "photo" : "doc.text")
                .font(.title3)
                .foregroundStyle(.secondary)
                .frame(width: 28)
            VStack(alignment: .leading, spacing: 3) {
                Text(artifact.label).font(.body).lineLimit(1)
                Text(taskDetail(artifact)).font(.caption).foregroundStyle(.secondary).lineLimit(2)
            }
            Spacer(minLength: 8)
            if let handle = MIFLibraryPreviewPolicy.taskHandle(
                for: artifact, access: taskAccess,
                workspaceSyncAvailable: workspaceSyncAvailable,
                taskMediaGenerationAvailable: taskMediaGenerationAvailable) {
                Button {
                    onPreview(handle)
                } label: {
                    Label(AppLocalized("Preview"), systemImage: "eye")
                        .labelStyle(.iconOnly)
                }
                .buttonStyle(.plain)
                .accessibilityLabel(AppLocalized("Preview"))
            } else {
                Text(AppLocalized("Metadata only"))
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .padding(.vertical, 4)
        .accessibilityIdentifier("morde.library.task-artifact.\(artifact.id)")
    }

    private func accountRow(_ item: MIFLibraryAccountItem) -> some View {
        HStack(spacing: 12) {
            Image(systemName: item.kind == .media ? "photo" : "doc.text")
                .font(.title3)
                .foregroundStyle(.secondary)
                .frame(width: 28)
            VStack(alignment: .leading, spacing: 3) {
                Text(item.name).font(.body).lineLimit(1)
                if let size = item.sizeBytes, size >= 0 {
                    Text(ByteCountFormatter.string(fromByteCount: size, countStyle: .file))
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            Spacer(minLength: 8)
            Text(AppLocalized("Metadata only"))
                .font(.caption).foregroundStyle(.secondary)
        }
        .padding(.vertical, 4)
        .accessibilityIdentifier("morde.library.cloud.\(item.id)")
    }

    private func localDetail(_ item: MIFLibraryLocalItem) -> String? {
        var parts: [String] = []
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
        return parts.isEmpty ? nil : parts.joined(separator: " · ")
    }

    private func taskDetail(_ artifact: MIFLibraryTaskArtifact) -> String {
        var parts: [String] = []
        if !artifact.mediaType.isEmpty { parts.append(artifact.mediaType) }
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
}
