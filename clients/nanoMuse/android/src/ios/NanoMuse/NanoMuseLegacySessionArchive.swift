import SwiftUI
import UIKit

/// Read-only browser for a conversation persisted by an earlier local chat runtime.
/// It reads the existing ChatStore tables and media files without creating a VM
/// or exposing send, retry, or tool-execution actions.
struct NanoMuseLegacySessionArchive: View {
    private let sessionId: String

    @ObservedObject private var authorization = MordeShellAuthorization.shared
    @State private var title = "Conversation"
    @State private var messages: [RawMessage] = []
    @State private var mediaURLs: [String: URL] = [:]
    @State private var isLoading = true
    @State private var sessionNotFound = false
    @State private var isImporting = false
    @State private var confirmsImport = false
    @State private var importNotice: String?

    init(sessionId: String) {
        self.sessionId = sessionId
    }

    var body: some View {
        Group {
            if isLoading {
                ProgressView()
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else if sessionNotFound {
                emptyState("Conversation unavailable", symbol: "bubble.left.and.bubble.right")
            } else if messages.isEmpty {
                emptyState("No saved messages", symbol: "text.bubble")
            } else {
                ScrollView {
                    LazyVStack(spacing: 14) {
                        ForEach(messages) { message in
                            LegacyArchiveMessageRow(
                                message: message,
                                mediaURLs: mediaURLs
                            )
                        }
                    }
                    .padding()
                }
            }
        }
        .background(Color(uiColor: .systemBackground))
        .navigationTitle(title)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            if authorization.signedIn, !isLoading, !importableMessages.isEmpty {
                ToolbarItem(placement: .topBarTrailing) {
                    Button {
                        confirmsImport = true
                    } label: {
                        if isImporting {
                            ProgressView()
                        } else {
                            Label("Import to Morde", systemImage: "square.and.arrow.up")
                        }
                    }
                    .disabled(isImporting)
                    .accessibilityIdentifier("morde.localHistory.import")
                }
            }
        }
        .confirmationDialog("Import this local conversation to Morde?", isPresented: $confirmsImport,
                            titleVisibility: .visible) {
            Button("Import \(importableMessages.count) text messages") {
                Task { await importHistory() }
            }
            .accessibilityIdentifier("morde.localHistory.confirmImport")
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("Only user and assistant text will be sent. Tool calls, tool results, private reasoning, and local media files stay on this device. The local conversation will not be changed.")
        }
        .alert("Morde history import", isPresented: Binding(
            get: { importNotice != nil },
            set: { if !$0 { importNotice = nil } }
        )) {
            Button("OK", role: .cancel) { importNotice = nil }
        } message: {
            Text(importNotice ?? "")
        }
        .task(id: sessionId) {
            await loadSession()
        }
    }

    private var importableMessages: [MordePersonalHistoryImportMessage] {
        (try? NanoMuseLegacyHistoryImport.messages(from: messages)) ?? []
    }

    @MainActor
    private func importHistory() async {
        let scope = authorization.personalDataScopeKey
        guard authorization.signedIn, let scope else {
            importNotice = "Sign in to Morde before importing local history."
            return
        }
        isImporting = true
        defer { isImporting = false }

        do {
            let selectedMessages = try NanoMuseLegacyHistoryImport.messages(from: messages)
            let requests = try NanoMuseLegacyHistoryImport.requests(
                sessionID: sessionId, title: title, messages: selectedMessages,
                deviceAccountID: NanoMuseLegacyHistoryImport.localDeviceAccountID()
            )
            var importedConversationID: String?
            var completedBatches = 0
            do {
                for request in requests {
                    guard authorization.signedIn, authorization.personalDataScopeKey == scope else {
                        throw CancellationError()
                    }
                    let receipt = try await authorization.importPersonalHistory(request)
                    guard importedConversationID == nil || importedConversationID == receipt.conversation_id else {
                        throw URLError(.cannotParseResponse)
                    }
                    importedConversationID = receipt.conversation_id
                    completedBatches += 1
                }
                importNotice = "Imported \(selectedMessages.count) text messages to Morde. The local conversation remains unchanged."
            } catch {
                let progress = completedBatches == 0
                    ? "The local conversation remains unchanged."
                    : "Some batches may already be imported; retry is safe and will not duplicate them."
                importNotice = "\(error.localizedDescription)\n\(progress)"
            }
        } catch {
            importNotice = error.localizedDescription
        }
    }

    private func emptyState(_ text: String, symbol: String) -> some View {
        VStack(spacing: 10) {
            Image(systemName: symbol)
                .font(.largeTitle)
                .foregroundStyle(.secondary)
            Text(text)
                .foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    @MainActor
    private func loadSession() async {
        isLoading = true
        sessionNotFound = false

        let store = ChatStore.shared
        let listedSessions = await store.listSessions()
        guard !Task.isCancelled else { return }
        var session = listedSessions.first(where: { $0.id == sessionId })
        if session == nil {
            session = await store.getSession(sessionId)
        }
        guard !Task.isCancelled else { return }

        guard let session else {
            messages = []
            mediaURLs = [:]
            sessionNotFound = true
            isLoading = false
            return
        }

        if let savedTitle = session.title?.trimmingCharacters(in: .whitespacesAndNewlines),
           !savedTitle.isEmpty {
            title = savedTitle
        } else {
            title = "Conversation"
        }

        let loadedMessages: [RawMessage]
        if let remoteDeviceId = session.remoteDeviceId {
            loadedMessages = await store.loadRemoteMessages(sessionId: sessionId, deviceId: remoteDeviceId)
        } else {
            loadedMessages = await store.loadMessages(sessionId: sessionId)
        }
        guard !Task.isCancelled else { return }

        let resolver = await store.mediaFileURLResolver()
        let mediaRoot = store.minisBaseURL
        var resolvedMedia: [String: URL] = [:]
        for reference in Self.mediaReferences(in: loadedMessages) {
            let candidate = resolver(reference)
            if Self.isSafeMediaURL(candidate, under: mediaRoot),
               FileManager.default.fileExists(atPath: candidate.path) {
                resolvedMedia[reference.relativePath] = candidate
            }
        }

        messages = loadedMessages
        mediaURLs = resolvedMedia
        isLoading = false
    }

    private static func mediaReferences(in messages: [RawMessage]) -> [MediaRef] {
        var references: [MediaRef] = []
        for message in messages {
            for part in message.parts {
                switch part {
                case .mediaRef(let reference):
                    references.append(reference)
                case .toolResult(let result):
                    if let reference = result.mediaRef {
                        references.append(reference)
                    }
                    if let reference = result.snapshot?.mediaRef {
                        references.append(reference)
                    }
                case .text, .toolUse:
                    break
                }
            }
        }
        return references
    }

    private static func isSafeMediaURL(_ candidate: URL, under root: URL) -> Bool {
        guard candidate.isFileURL else { return false }
        let resolvedRoot = root.standardizedFileURL.resolvingSymlinksInPath()
        let resolvedCandidate = candidate.standardizedFileURL.resolvingSymlinksInPath()
        let rootPrefix = resolvedRoot.path.hasSuffix("/") ? resolvedRoot.path : resolvedRoot.path + "/"
        return resolvedCandidate.path.hasPrefix(rootPrefix)
    }
}

enum NanoMuseLegacyHistoryImportError: LocalizedError, Equatable {
    case noImportableText
    case invalidSourceIdentifier
    case messageTooLarge

    var errorDescription: String? {
        switch self {
        case .noImportableText: return "This conversation has no user or assistant text to import."
        case .invalidSourceIdentifier: return "This local conversation has an invalid source identifier."
        case .messageTooLarge: return "A message is too large for the Morde history import limit."
        }
    }
}

enum NanoMuseLegacyHistoryImport {
    static var sourceID: String { MordePersonalHistoryImportContract.sourceID }
    private static let deviceAccountDefaultsKey = "morde.localHistoryImport.deviceAccountID.v1"
    private static let maxMessagesPerRequest = 200
    private static let maxMessageBytes = 32 * 1024
    private static let maxTextBytesPerRequest = 1 * 1024 * 1024

    static func localDeviceAccountID() -> String {
        if let existing = UserDefaults.standard.string(forKey: deviceAccountDefaultsKey),
           isValidSourceIdentifier(existing) {
            return existing
        }
        let value = UUID().uuidString.lowercased()
        UserDefaults.standard.set(value, forKey: deviceAccountDefaultsKey)
        return value
    }

    static func messages(from rawMessages: [RawMessage]) throws -> [MordePersonalHistoryImportMessage] {
        var seen = Set<String>()
        var selected: [MordePersonalHistoryImportMessage] = []
        for message in rawMessages where !message.isToolResultOnly {
            let content = message.parts.compactMap { part -> String? in
                guard case .text(let text) = part else { return nil }
                return text
            }.joined()
            guard !content.isEmpty else { continue }
            guard isValidSourceIdentifier(message.id), seen.insert(message.id).inserted else {
                throw NanoMuseLegacyHistoryImportError.invalidSourceIdentifier
            }
            guard content.utf8.count <= maxMessageBytes else {
                throw NanoMuseLegacyHistoryImportError.messageTooLarge
            }
            selected.append(MordePersonalHistoryImportMessage(
                source_message_id: message.id, role: message.role.rawValue, content: content
            ))
        }
        guard !selected.isEmpty else { throw NanoMuseLegacyHistoryImportError.noImportableText }
        return selected
    }

    static func requests(sessionID: String, title: String, messages: [MordePersonalHistoryImportMessage],
                         deviceAccountID: String) throws -> [MordePersonalHistoryImportRequest] {
        guard isValidSourceIdentifier(sessionID), isValidSourceIdentifier(deviceAccountID), !messages.isEmpty else {
            throw NanoMuseLegacyHistoryImportError.invalidSourceIdentifier
        }
        var batches: [[MordePersonalHistoryImportMessage]] = []
        var batch: [MordePersonalHistoryImportMessage] = []
        var batchTextBytes = 0
        for message in messages {
            guard isValidSourceIdentifier(message.source_message_id),
                  ["user", "assistant"].contains(message.role), !message.content.isEmpty else {
                throw NanoMuseLegacyHistoryImportError.invalidSourceIdentifier
            }
            let messageBytes = message.content.utf8.count
            guard messageBytes <= maxMessageBytes else { throw NanoMuseLegacyHistoryImportError.messageTooLarge }
            if batch.count == maxMessagesPerRequest || batchTextBytes + messageBytes > maxTextBytesPerRequest {
                batches.append(batch)
                batch = []
                batchTextBytes = 0
            }
            batch.append(message)
            batchTextBytes += messageBytes
        }
        if !batch.isEmpty { batches.append(batch) }
        let cleanTitle = title.trimmingCharacters(in: .whitespacesAndNewlines)
        let importTitle = String(String.UnicodeScalarView((cleanTitle.isEmpty ? "Conversation" : cleanTitle).unicodeScalars.prefix(120)))
        return batches.map {
            MordePersonalHistoryImportRequest(
                source_id: sourceID, device_account_id: deviceAccountID,
                source_conversation_id: sessionID, target_conversation_id: nil,
                title: importTitle, messages: $0
            )
        }
    }

    private static func isValidSourceIdentifier(_ value: String) -> Bool {
        MordePersonalHistoryImportContract.isValidSourceIdentifier(value)
    }
}

private struct LegacyArchiveMessageRow: View {
    let message: RawMessage
    let mediaURLs: [String: URL]

    private var isToolOutput: Bool { message.isToolResultOnly }
    private var isUserMessage: Bool { message.role == .user && !isToolOutput }

    var body: some View {
        HStack {
            if isUserMessage { Spacer(minLength: 36) }

            VStack(alignment: .leading, spacing: 8) {
                HStack {
                    Text(authorLabel)
                        .font(.caption.weight(.semibold))
                    Spacer()
                    Text(message.createdAt.formatted(date: .abbreviated, time: .shortened))
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                }

                if let reasoning = message.reasoningContent,
                   !reasoning.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                    DisclosureGroup("Thinking") {
                        Text(reasoning)
                            .font(.callout)
                            .textSelection(.enabled)
                    }
                    .padding(9)
                    .background(Color.secondary.opacity(0.08), in: RoundedRectangle(cornerRadius: 10))
                }

                ForEach(Array(message.parts.enumerated()), id: \.offset) { entry in
                    partView(entry.element)
                }

                if let error = message.errorInfo, !error.isEmpty {
                    Label(error, systemImage: "exclamationmark.triangle")
                        .font(.caption)
                        .foregroundStyle(.orange)
                        .textSelection(.enabled)
                }
            }
            .padding(12)
            .frame(maxWidth: 560, alignment: .leading)
            .background(bubbleColor, in: RoundedRectangle(cornerRadius: 16))

            if !isUserMessage { Spacer(minLength: 36) }
        }
        .frame(maxWidth: .infinity)
    }

    private var authorLabel: String {
        if isToolOutput { return "Tool output" }
        return isUserMessage ? "You" : "Assistant"
    }

    private var bubbleColor: Color {
        if isUserMessage { return Color.accentColor.opacity(0.12) }
        if isToolOutput { return Color.orange.opacity(0.08) }
        return Color.secondary.opacity(0.08)
    }

    @ViewBuilder
    private func partView(_ part: ContentPart) -> some View {
        switch part {
        case .text(let text):
            if !text.isEmpty {
                Text(text)
                    .font(.body)
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }

        case .mediaRef(let reference):
            LegacyArchiveMedia(reference: reference, fileURL: mediaURLs[reference.relativePath])

        case .toolUse(let use):
            VStack(alignment: .leading, spacing: 5) {
                Label("Tool call · \(use.name)", systemImage: "wrench.and.screwdriver")
                    .font(.caption.weight(.semibold))
                if let description = use.description, !description.isEmpty {
                    Text(description)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                Text(use.input)
                    .font(.caption.monospaced())
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
            .padding(9)
            .background(Color.secondary.opacity(0.08), in: RoundedRectangle(cornerRadius: 10))

        case .toolResult(let result):
            VStack(alignment: .leading, spacing: 6) {
                Label(
                    "Tool result · \(result.status ?? (result.success ? "success" : "failed"))",
                    systemImage: result.success ? "checkmark.circle" : "xmark.circle"
                )
                .font(.caption.weight(.semibold))
                Text(result.output.isEmpty ? "(empty output)" : result.output)
                    .font(.callout.monospaced())
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)

                if let pageURL = result.pageURL, !pageURL.isEmpty {
                    Text("Page: \(pageURL)")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .textSelection(.enabled)
                }
                if let snapshotText = result.snapshot?.text,
                   !snapshotText.isEmpty,
                   snapshotText != result.output {
                    Text(snapshotText)
                        .font(.caption.monospaced())
                        .textSelection(.enabled)
                }
                if let reference = result.mediaRef {
                    LegacyArchiveMedia(reference: reference, fileURL: mediaURLs[reference.relativePath])
                }
                if let snapshotReference = result.snapshot?.mediaRef,
                   snapshotReference.relativePath != result.mediaRef?.relativePath {
                    LegacyArchiveMedia(
                        reference: snapshotReference,
                        fileURL: mediaURLs[snapshotReference.relativePath]
                    )
                }
            }
            .padding(9)
            .background(Color.secondary.opacity(0.08), in: RoundedRectangle(cornerRadius: 10))
        }
    }
}

private struct LegacyArchiveMedia: View {
    let reference: MediaRef
    let fileURL: URL?

    @State private var image: UIImage?
    @State private var didLoadImage = false

    private var displayName: String {
        if let originalFileName = reference.originalFileName, !originalFileName.isEmpty {
            return originalFileName
        }
        return URL(fileURLWithPath: reference.relativePath).lastPathComponent
    }

    var body: some View {
        Group {
            if reference.mimeType.lowercased().hasPrefix("image/") {
                if let fileURL {
                    imageContent
                        .task(id: fileURL) {
                            didLoadImage = false
                            image = nil
                            let imageData = await Task.detached(priority: .userInitiated) {
                                try? Data(contentsOf: fileURL)
                            }.value
                            guard !Task.isCancelled else { return }
                            image = imageData.flatMap { UIImage(data: $0) }
                            didLoadImage = true
                        }
                } else {
                    Label("Image unavailable · \(displayName)", systemImage: "photo")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            } else {
                Label(displayName, systemImage: mediaIcon)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .accessibilityLabel("\(displayName), \(reference.mimeType)")
            }
        }
    }

    @ViewBuilder
    private var imageContent: some View {
        if let image {
            Image(uiImage: image)
                .resizable()
                .aspectRatio(contentMode: .fit)
                .frame(maxWidth: 320, maxHeight: 280, alignment: .leading)
                .clipShape(RoundedRectangle(cornerRadius: 10))
        } else if didLoadImage {
            Label("Image unavailable · \(displayName)", systemImage: "photo")
                .font(.caption)
                .foregroundStyle(.secondary)
        } else {
            ProgressView()
                .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private var mediaIcon: String {
        let mimeType = reference.mimeType.lowercased()
        if mimeType.hasPrefix("audio/") { return "waveform" }
        if mimeType.hasPrefix("video/") { return "video" }
        return "doc"
    }
}
