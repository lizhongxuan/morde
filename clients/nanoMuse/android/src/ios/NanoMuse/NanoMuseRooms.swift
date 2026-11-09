//
//  NanoMuseRooms.swift
//  nanoMuse
//
//  Two of the rooms next to the chat: Ideas (the bundled catalogue — one
//  tap sends a prompt to a new chat, schedules a routine or starts a goal)
//  and Library (the files the agent made, from every session's workspace
//  and the shared folder). Feed and Goals live in NanoMuseFeed.swift and
//  NanoMuseGoals.swift. Android: ui/ideas, ui/library.
//

import SwiftUI
import QuickLook
import CryptoKit
import UniformTypeIdentifiers

// MARK: - Shared bits

/// A centred empty state with a symbol, a title and a line of body text.
struct NanoMuseEmptyState: View {
    var symbol: String
    var title: String
    var message: String
    var hint: (title: String, body: String)? = nil

    var body: some View {
        VStack(spacing: 14) {
            Image(systemName: symbol)
                .font(.system(size: 40, weight: .light))
                .foregroundStyle(.secondary)
                .padding(.bottom, 4)
            Text(title)
                .font(.title3.weight(.semibold))
                .multilineTextAlignment(.center)
            Text(message)
                .font(.body)
                .foregroundStyle(NanoMuseTones.secondaryText)
                .multilineTextAlignment(.center)
            if let hint {
                VStack(alignment: .leading, spacing: 6) {
                    Text(hint.title).font(.subheadline.weight(.semibold))
                    Text(hint.body).font(.subheadline).foregroundStyle(.secondary)
                }
                .padding(14)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(NanoMuseTones.fill, in: RoundedRectangle(cornerRadius: 14, style: .continuous))
                .padding(.top, 10)
            }
        }
        .padding(.horizontal, 28)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}

// MARK: - Ideas

struct NanoMuseIdea: Identifiable, Decodable {
    enum Kind: String, Decodable { case chat = "CHAT", routine = "ROUTINE", goal = "GOAL" }

    var id: String
    var emoji: String
    var title: String
    var body: String
    var kind: Kind
    var category: String?
    var prompt: String?
    var time: String?

    private enum CodingKeys: String, CodingKey { case id, emoji, title, body, kind, category, prompt, time }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        emoji = try c.decodeIfPresent(String.self, forKey: .emoji) ?? "💡"
        title = try c.decode(String.self, forKey: .title)
        body = try c.decodeIfPresent(String.self, forKey: .body) ?? ""
        kind = (try? c.decode(Kind.self, forKey: .kind)) ?? .chat
        category = try c.decodeIfPresent(String.self, forKey: .category)
        prompt = try c.decodeIfPresent(String.self, forKey: .prompt)
        time = try c.decodeIfPresent(String.self, forKey: .time)
    }

    var promptText: String { (prompt?.isEmpty == false ? prompt : nil) ?? title }
}

struct NanoMuseIdeaSection: Identifiable, Decodable {
    var id: String
    var title: String
    var ideas: [NanoMuseIdea]
}

enum NanoMuseIdeas {
    private struct File: Decodable { var sections: [NanoMuseIdeaSection] }

    static func load() -> [NanoMuseIdeaSection] {
        let name = NanoMuseLocale.isChinese ? "ideas.zh" : "ideas.en"
        guard let url = Bundle.main.url(forResource: name, withExtension: "json")
                ?? Bundle.main.url(forResource: "ideas.en", withExtension: "json"),
              let data = try? Data(contentsOf: url),
              let file = try? JSONDecoder().decode(File.self, from: data) else { return [] }
        return file.sections
    }
}

struct NanoMuseIdeasRoom: View {
    var chrome: NanoMuseRoomChrome
    var onSend: (String) -> Void
    /// A ROUTINE idea: schedule it (NanoMuseScheduler) and open the editor.
    var onCreateRoutine: (NanoMuseIdea) -> Void
    /// A GOAL idea: "Create goal › category" in the main chat, with the idea's prompt as the seed.
    var onStartGoal: (NanoMuseGoalCategory, String?) -> Void

    @State private var sections: [NanoMuseIdeaSection] = []
    @State private var selected: NanoMuseIdea?
    @ObservedObject private var personal = NanoMuseMordePersonalContentStore.shared
    @ObservedObject private var authorization = MordeShellAuthorization.shared

    var body: some View {
        VStack(spacing: 0) {
            // Android: the Ideas room's ••• menu has only the shared rows.
            NanoMuseRoomHeader(chrome: chrome) {
                NanoMuseRoomMenu(chrome: chrome) { EmptyView() }
            }
            if sections.isEmpty {
                NanoMuseEmptyState(symbol: "lightbulb", title: AppLocalized("Ideas"),
                                   message: AppLocalized("The ideas could not be loaded."))
            } else {
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 0, pinnedViews: []) {
                        ForEach(sections) { section in
                            Text(section.title)
                                .font(.headline)
                                .padding(.horizontal, 20)
                                .padding(.top, 18)
                                .padding(.bottom, 4)
                            ForEach(section.ideas) { idea in
                                Button { selected = idea } label: { row(idea) }
                                    .buttonStyle(.plain)
                                    .accessibilityIdentifier("morde.idea.row.\(idea.id)")
                                    .contextMenu {
                                        if authorization.signedIn {
                                            ideaMarkActions(idea)
                                        }
                                    }
                                // Android's IdeasTab: a hairline under each row, from the text's left edge.
                                NanoMuseTones.hairline.frame(height: 1).padding(.leading, 76).padding(.trailing, 20)
                            }
                        }
                        Spacer(minLength: 24)
                    }
                }
            }
            if let pending = personal.pending, pending.objectType == "idea_mark" {
                Label(AppLocalized(personal.syncEnabled ? "An idea mark is waiting to sync." : "Sync is paused; the idea mark stays on this iPhone."), systemImage: "clock")
                    .font(.footnote).foregroundStyle(.secondary).padding(.horizontal, 16).padding(.vertical, 6)
                Button(AppLocalized("Retry idea sync")) { Task { await personal.retry(root: NanoMuseDirs.root) } }
                    .font(.footnote).disabled(!personal.syncEnabled || personal.isSaving)
            }
        }
        // The chat's canvas, as Android's IdeasTab and docs/design.md; the grey page is the Feed's alone.
        .background(ChatColors.background.ignoresSafeArea())
        .onAppear {
            if sections.isEmpty { sections = NanoMuseIdeas.load() }
            Task { await personal.refresh(root: NanoMuseDirs.root) }
        }
        .nmOnChange(of: authorization.personalDataScopeKey) { _ in Task { await personal.refresh(root: NanoMuseDirs.root) } }
        .sheet(item: $selected) { idea in
            NanoMuseIdeaSheet(idea: idea) { action in
                selected = nil
                switch action {
                case .chat: onSend(idea.promptText)
                case .routine: onCreateRoutine(idea)
                case .goal: onStartGoal(NanoMuseGoalCategory.from(idea.category), idea.prompt)
                }
            }
            .presentationDetents([.medium, .large])
        }
    }

    private func row(_ idea: NanoMuseIdea) -> some View {
        HStack(alignment: .top, spacing: 16) {
            Text(idea.emoji)
                .font(.system(size: 28))
                .frame(width: 40)
            VStack(alignment: .leading, spacing: 4) {
                Text(idea.title)
                    .font(.body.weight(.semibold))
                    .foregroundStyle(.primary)
                    .multilineTextAlignment(.leading)
                Text(idea.body)
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
                    .lineLimit(2)
                    .multilineTextAlignment(.leading)
            }
            if let mark = ideaMark(idea.id) {
                Image(systemName: mark.state == "saved" ? "bookmark.fill" : "eye.slash")
                    .font(.caption).foregroundStyle(.secondary)
            }
            Spacer(minLength: 0)
        }
        .padding(.horizontal, 20)
        .padding(.vertical, 14)
        .contentShape(Rectangle())
    }

    private func ideaMark(_ id: String) -> NanoMuseMordeIdeaMarkBody? {
        guard let item = personal.object("idea_mark", id),
              let body = try? item.decodeBody(NanoMuseMordeIdeaMarkBody.self), body.isValid else { return nil }
        return body
    }

    @ViewBuilder
    private func ideaMarkActions(_ idea: NanoMuseIdea) -> some View {
        let mark = ideaMark(idea.id)
        let isSaved = mark?.state == "saved"
        let isDismissed = mark?.state == "dismissed"
        Button(action: { Task { await setIdeaMark(idea.id, state: "saved", remove: isSaved) } }) {
            Label(AppLocalized(isSaved ? "Remove saved mark" : "Save idea"), systemImage: "bookmark")
        }
        Button(action: { Task { await setIdeaMark(idea.id, state: "dismissed", remove: isDismissed) } }) {
            Label(AppLocalized(isDismissed ? "Restore idea" : "Dismiss idea"), systemImage: "eye.slash")
        }
    }

    private func setIdeaMark(_ id: String, state: String, remove: Bool) async {
        guard let current = personal.object("idea_mark", id) else {
            if !remove { try? await personal.replace("idea_mark", id: id, revision: 0,
                                                       body: NanoMuseMordeIdeaMarkBody(state: state), root: NanoMuseDirs.root) }
            return
        }
        if remove { try? await personal.delete("idea_mark", id: id, revision: current.revision, root: NanoMuseDirs.root) }
        else { try? await personal.replace("idea_mark", id: id, revision: current.revision,
                                            body: NanoMuseMordeIdeaMarkBody(state: state), root: NanoMuseDirs.root) }
    }
}

private struct NanoMuseIdeaSheet: View {
    var idea: NanoMuseIdea
    var onAct: (NanoMuseIdea.Kind) -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack(alignment: .top, spacing: 14) {
                Text(idea.emoji).font(.system(size: 40))
                Text(idea.title).font(.title3.weight(.bold))
            }
            Text(idea.body).font(.body)
            HStack(spacing: 6) {
                Image(systemName: kindSymbol).font(.footnote)
                Text(kindText).font(.footnote)
            }
            .foregroundStyle(.secondary)
            Spacer(minLength: 8)
            Button {
                onAct(idea.kind)
            } label: {
                Label(actionText, systemImage: kindSymbol)
                    .font(.body.weight(.semibold))
                    .frame(maxWidth: .infinity)
                    .padding(.vertical, 12)
            }
            .buttonStyle(.borderedProminent)
            .tint(NanoMuseTones.action)
            if idea.kind != .chat {
                Button {
                    onAct(.chat)
                } label: {
                    Text(AppLocalized("Send to chat"))
                        .font(.subheadline.weight(.medium))
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.bordered)
            }
            if idea.kind == .routine {
                Text(AppLocalized("Runs when the app is open; at the set time the phone reminds you to open it."))
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .padding(22)
    }

    private var actionText: String {
        switch idea.kind {
        case .chat: return AppLocalized("Send to chat")
        case .routine: return AppLocalized("Create routine")
        case .goal: return AppLocalized("Start goal")
        }
    }

    private var kindSymbol: String {
        switch idea.kind {
        case .chat: return "bubble.left"
        case .routine: return "clock"
        case .goal: return "flag"
        }
    }

    private var kindText: String {
        let base: String
        switch idea.kind {
        case .chat: base = AppLocalized("Starts a conversation")
        case .routine: base = AppLocalized("Creates a routine")
        case .goal: base = AppLocalized("Creates a goal")
        }
        if idea.kind == .routine, let time = idea.time, !time.isEmpty { return "\(base) · \(time)" }
        return base
    }
}

// MARK: - Library

struct NanoMuseLibraryEntry: Identifiable, Hashable {
    enum Kind { case artifact, media }

    var id: String { url.path }
    var url: URL
    var name: String
    var relativePath: String
    var size: Int64
    var modified: Date
    var kind: Kind
    var sessionId: String?
    var sessionTitle: String?

    var symbol: String {
        switch url.pathExtension.lowercased() {
        case "png", "jpg", "jpeg", "gif", "bmp", "webp", "heic", "svg": return "photo"
        case "mp4", "mov", "avi", "mkv", "webm": return "film"
        case "mp3", "wav", "aac", "flac", "ogg", "m4a": return "waveform"
        case "pdf": return "doc.richtext"
        case "md", "txt": return "doc.text"
        case "csv", "xlsx", "xls": return "tablecells"
        case "html", "htm": return "globe"
        case "py", "js", "ts", "swift", "sh", "json": return "chevron.left.forwardslash.chevron.right"
        default: return "doc"
        }
    }
}

enum NanoMuseLibraryIndex {
    static let maxDepth = 4
    static let maxEntries = 600
    static let skipDirs: Set<String> = ["node_modules", ".git", "__pycache__", ".cache", ".venv", "venv", ".npm"]
    static let mediaExt: Set<String> = [
        "png", "jpg", "jpeg", "gif", "bmp", "webp", "heic", "svg",
        "mp4", "mov", "avi", "mkv", "webm", "mp3", "wav", "aac", "flac", "ogg", "m4a",
    ]

    /// Every file under the workspace of each session in `titles` and under the shared
    /// folder, newest first. `titles` is the set of chats the person can see (C12: this
    /// account's); a workspace left by another account's chat, or by a chat since deleted,
    /// is not listed, previewed or shared from here.
    static func scan(titles: [String: String]) -> [NanoMuseLibraryEntry] {
        var out: [NanoMuseLibraryEntry] = []
        let fm = FileManager.default
        let base = MinisStoragePaths.minisPersistentBase
        if let sessions = try? fm.contentsOfDirectory(at: base, includingPropertiesForKeys: [.isDirectoryKey], options: [.skipsHiddenFiles]) {
            for dir in sessions {
                let sid = dir.lastPathComponent
                guard titles[sid] != nil else { continue }
                let ws = dir.appendingPathComponent("workspace", isDirectory: true)
                var isDir: ObjCBool = false
                guard fm.fileExists(atPath: ws.path, isDirectory: &isDir), isDir.boolValue else { continue }
                walk(ws, root: ws, depth: 0, sessionId: sid, title: titles[sid], into: &out)
                if out.count >= maxEntries { break }
            }
        }
        let shared = MinisStoragePaths.minisSharedPersistentDir
        if out.count < maxEntries, fm.fileExists(atPath: shared.path) {
            walk(shared, root: shared, depth: 0, sessionId: nil, title: nil, into: &out)
        }
        return out.sorted { $0.modified > $1.modified }
    }

    private static func walk(_ dir: URL, root: URL, depth: Int, sessionId: String?, title: String?, into out: inout [NanoMuseLibraryEntry]) {
        guard depth <= maxDepth, out.count < maxEntries else { return }
        let fm = FileManager.default
        guard let children = try? fm.contentsOfDirectory(at: dir, includingPropertiesForKeys: [.isDirectoryKey, .fileSizeKey, .contentModificationDateKey], options: [.skipsHiddenFiles]) else { return }
        for child in children {
            guard out.count < maxEntries else { return }
            let values = try? child.resourceValues(forKeys: [.isDirectoryKey, .fileSizeKey, .contentModificationDateKey])
            if values?.isDirectory == true {
                if skipDirs.contains(child.lastPathComponent) { continue }
                walk(child, root: root, depth: depth + 1, sessionId: sessionId, title: title, into: &out)
                continue
            }
            let rel = child.path.hasPrefix(root.path) ? String(child.path.dropFirst(root.path.count)).trimmingCharacters(in: CharacterSet(charactersIn: "/")) : child.lastPathComponent
            let kind: NanoMuseLibraryEntry.Kind = mediaExt.contains(child.pathExtension.lowercased()) ? .media : .artifact
            out.append(NanoMuseLibraryEntry(
                url: child,
                name: child.lastPathComponent,
                relativePath: rel,
                size: Int64(values?.fileSize ?? 0),
                modified: values?.contentModificationDate ?? .distantPast,
                kind: kind,
                sessionId: sessionId,
                sessionTitle: title
            ))
        }
    }
}

struct NanoMuseLibraryRoom: View {
    var chrome: NanoMuseRoomChrome?
    var sessionId: String?

    @State private var entries: [NanoMuseLibraryEntry] = []
    @State private var segment = 0
    @State private var loading = false
    @State private var preview: NanoMuseLibraryEntry?
    @State private var shareItem: NanoMuseShareTarget?
    @State private var remotePreview: NanoMuseShareTarget?
    @State private var libraryError: String?
    @ObservedObject private var personal = NanoMuseMordePersonalContentStore.shared
    @ObservedObject private var authorization = MordeShellAuthorization.shared

    private var shown: [NanoMuseLibraryEntry] {
        entries.filter { segment == 0 ? $0.kind == .artifact : $0.kind == .media }
    }

    var body: some View {
        VStack(spacing: 0) {
            // Android: the Library's ••• menu — shared folders, the chat's files, then the shared rows.
            if let chrome {
                NanoMuseRoomHeader(chrome: chrome) {
                    HStack(spacing: 8) {
                        NanoMuseRoundButton(symbol: "arrow.clockwise", label: AppLocalized("Refresh")) {
                            refresh()
                            Task { await personal.refresh(root: NanoMuseDirs.root) }
                        }
                        NanoMuseRoomMenu(chrome: chrome) {
                            Button { chrome.open("sharedFolders") } label: { Label(AppLocalized("Shared folders"), systemImage: "folder.badge.person.crop") }
                            if chrome.hasMainSession {
                                Button { chrome.open("chatFiles") } label: { Label(AppLocalized("Browse Chat Files"), systemImage: "folder") }
                            }
                        }
                    }
                }
            }
            if let libraryError {
                Label(libraryError, systemImage: "exclamationmark.triangle")
                    .font(.footnote).foregroundStyle(.red).padding(.horizontal, 16)
            }
            Picker(AppLocalized("Library"), selection: $segment) {
                Text(AppLocalized("Artifacts")).tag(0)
                Text(AppLocalized("Media")).tag(1)
                if authorization.signedIn { Text(AppLocalized("Cloud")).tag(2) }
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            .padding(.horizontal, 16)
            .padding(.bottom, 8)

            if segment != 2, authorization.signedIn {
                Text(AppLocalized("On this iPhone")).font(.caption).foregroundStyle(.secondary).padding(.horizontal, 16)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }

            if segment == 2 {
                cloudLibraryContent
            } else if shown.isEmpty {
                if loading {
                    ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
                } else if segment == 0 {
                    NanoMuseEmptyState(symbol: "doc.text", title: AppLocalized("Nothing created yet"),
                                       message: AppLocalized("Documents, tables and pages I write for you will show up here."))
                } else {
                    NanoMuseEmptyState(symbol: "photo.on.rectangle", title: AppLocalized("No media yet"),
                                       message: AppLocalized("Photos, screenshots and recordings I produce will show up here."))
                }
            } else {
                List {
                    ForEach(shown) { entry in
                        Button { preview = entry } label: { row(entry) }
                            .listRowBackground(ChatColors.background)
                            .contextMenu {
                                Button {
                                    shareItem = NanoMuseShareTarget(url: entry.url)
                                } label: {
                                    Label(AppLocalized("Share"), systemImage: "square.and.arrow.up")
                                }
                                if let sid = entry.sessionId {
                                    Button {
                                        NotificationCenter.default.post(name: .nanoMuseOpenChat, object: sid)
                                    } label: {
                                        Label(AppLocalized("Open conversation"), systemImage: "bubble.left")
                                    }
                                }
                                if authorization.signedIn {
                                    Button { upload(entry) } label: {
                                        Label(AppLocalized("Save attachment to Morde"), systemImage: "icloud.and.arrow.up")
                                    }
                                }
                            }
                    }
                }
                .listStyle(.plain)
                .scrollContentBackground(.hidden)
            }
        }
        // Plain rows on the chat's canvas, as Android's LibraryTab and docs/design.md.
        .background(ChatColors.background.ignoresSafeArea())
        .onAppear { if entries.isEmpty { refresh() } }
        .task { await personal.refresh(root: NanoMuseDirs.root) }
        .nmOnChange(of: authorization.personalDataScopeKey) { _ in
            if !authorization.signedIn { segment = 0 }
            Task { await personal.refresh(root: NanoMuseDirs.root) }
        }
        .sheet(item: $preview) { entry in
            NanoMuseQuickLook(url: entry.url)
                .ignoresSafeArea()
        }
        .sheet(item: $shareItem) { target in
            NanoMuseShareSheet(items: [target.url])
        }
        .sheet(item: $remotePreview) { target in
            NanoMuseQuickLook(url: target.url).ignoresSafeArea()
        }
    }

    @ViewBuilder
    private var cloudLibraryContent: some View {
        let items = personal.values("library_item").compactMap { item -> NanoMuseMordeLibraryRecord? in
            guard let body = try? item.decodeBody(NanoMuseMordeLibraryItemBody.self), body.isValid else { return nil }
            return NanoMuseMordeLibraryRecord(id: item.object_id, revision: item.revision, body: body)
        }.sorted { $0.body.name.localizedCaseInsensitiveCompare($1.body.name) == .orderedAscending }
        if libraryError != nil || personal.error != nil {
            VStack {
                NanoMuseEmptyState(symbol: "exclamationmark.triangle", title: AppLocalized("Cloud library unavailable"),
                                   message: libraryError ?? personal.error ?? "")
                Button(AppLocalized("Retry")) { Task { await personal.retry(root: NanoMuseDirs.root); libraryError = nil } }
            }
        } else if personal.isLoading && items.isEmpty {
            ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
        } else if items.isEmpty {
            NanoMuseEmptyState(symbol: "doc.text", title: AppLocalized("No cloud files yet"),
                               message: AppLocalized("Files saved to your Morde account will appear here."))
        } else {
            List {
                if let pending = personal.pending, pending.objectType == "library_item" {
                    Text(AppLocalized(personal.syncEnabled ? "A library change is waiting to sync." : "Sync is paused; the change stays on this iPhone."))
                        .font(.footnote).foregroundStyle(.secondary)
                }
                ForEach(items) { item in
                    Button { Task { await download(item.body) } } label: {
                        HStack(spacing: 12) {
                            Image(systemName: item.body.kind == "media" ? "photo" : "doc.text")
                                .font(.title3).foregroundStyle(.secondary).frame(width: 28)
                            VStack(alignment: .leading, spacing: 2) {
                                Text(item.body.name).font(.body).foregroundStyle(.primary).lineLimit(1)
                                Text(!item.body.hasAttachment ? AppLocalized("Metadata only") : ByteCountFormatter.string(fromByteCount: item.body.size_bytes ?? 0, countStyle: .file))
                                    .font(.caption).foregroundStyle(.secondary)
                            }
                            Spacer()
                            if item.body.hasAttachment { Image(systemName: "arrow.down.circle").foregroundStyle(.secondary) }
                        }
                    }
                    .disabled(!item.body.hasAttachment)
                    .accessibilityIdentifier("morde.library.cloud.item.\(item.id)")
                    .listRowBackground(ChatColors.background)
                }
            }
            .listStyle(.plain).scrollContentBackground(.hidden)
        }
    }

    private func row(_ entry: NanoMuseLibraryEntry) -> some View {
        HStack(spacing: 12) {
            Image(systemName: entry.symbol)
                .font(.title3)
                .foregroundStyle(.secondary)
                .frame(width: 28)
            VStack(alignment: .leading, spacing: 2) {
                Text(entry.name).font(.body).foregroundStyle(.primary).lineLimit(1)
                Text(subtitle(entry)).font(.caption).foregroundStyle(.secondary).lineLimit(1)
            }
            Spacer(minLength: 0)
        }
        .contentShape(Rectangle())
    }

    private func subtitle(_ entry: NanoMuseLibraryEntry) -> String {
        let size = ByteCountFormatter.string(fromByteCount: entry.size, countStyle: .file)
        let origin = entry.sessionId == nil
            ? AppLocalized("Shared folder")
            : ((entry.sessionTitle?.isEmpty == false ? entry.sessionTitle : nil) ?? AppLocalized("Untitled chat"))
        return "\(origin) · \(size) · \(NanoMuseDrawer.when(entry.modified))"
    }

    private func refresh() {
        loading = true
        Task {
            let sessions = NanoMuseSync.shared.visible(await ChatStore.shared.listSessions()) // C12: this account's chats only
            var titles: [String: String] = [:]
            for s in sessions { titles[s.id] = s.title ?? "" }
            let found = await Task.detached(priority: .utility) { NanoMuseLibraryIndex.scan(titles: titles) }.value
            await MainActor.run {
                entries = found
                loading = false
            }
        }
    }

    private func download(_ body: NanoMuseMordeLibraryItemBody) async {
        guard let scope = authorization.personalDataScopeKey, let attachmentID = body.attachment_id else { return }
        libraryError = nil
        do {
            let result = try await MordeShellAuthorization.shared.downloadPersonalAttachment(attachmentID: attachmentID)
            guard authorization.personalDataScopeKey == scope else {
                try? FileManager.default.removeItem(at: result.fileURL)
                return
            }
            let directory = NanoMuseDirs.root.appendingPathComponent("morde-library-downloads", isDirectory: true)
            try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
            let destination = directory.appendingPathComponent("\(UUID().uuidString.prefix(8))-\(body.name)")
            try FileManager.default.moveItem(at: result.fileURL, to: destination)
            remotePreview = NanoMuseShareTarget(url: destination)
        } catch { libraryError = error.localizedDescription }
    }

    private func upload(_ entry: NanoMuseLibraryEntry) {
        Task {
            libraryError = nil
            guard authorization.signedIn, personal.syncEnabled, let scope = authorization.personalDataScopeKey else {
                libraryError = AppLocalized("Sign in and enable personal sync before uploading an attachment.")
                return
            }
            do {
                let sourceURL = entry.url
                let digest = try await Task.detached(priority: .utility) {
                    try MordePersonalAttachmentHash.measure(fileURL: sourceURL)
                }.value
                let rawID = SHA256.hash(data: Data((scope + "\u{0}" + entry.url.path).utf8)).map { String(format: "%02x", $0) }.joined()
                let objectID = "li_" + String(rawID.prefix(40))
                var baseRevision: Int64
                let record = personal.object("library_item", objectID)
                if let record {
                    guard let body = try? record.decodeBody(NanoMuseMordeLibraryItemBody.self), body.isValid,
                          !body.hasAttachment else { throw URLError(.cannotWriteToFile) }
                    baseRevision = record.revision
                } else {
                    let kind = entry.kind == .media ? "media" : "artifact"
                    let body = NanoMuseMordeLibraryItemBody(name: entry.name, kind: kind,
                        mime: mimeType(entry), size_bytes: digest.sizeBytes, attachment_id: nil)
                    guard let resultRevision = try await personal.replace("library_item", id: objectID, revision: 0,
                        body: body, root: NanoMuseDirs.root) else {
                        libraryError = AppLocalized("File details are queued on this iPhone. Retry the attachment upload after sync completes.")
                        return
                    }
                    baseRevision = resultRevision
                }
                guard authorization.personalDataScopeKey == scope else { return }
                let quota = try await MordeShellAuthorization.shared.personalDataQuota()
                guard authorization.personalDataScopeKey == scope else { return }
                let staged = try await Task.detached(priority: .utility) {
                    try NanoMuseMordePersonalAttachmentStaging.stage(sourceURL: sourceURL,
                        expectedSizeBytes: digest.sizeBytes, expectedSHA256: digest.sha256,
                        maxBytes: quota.max_attachment_bytes, root: NanoMuseDirs.root)
                }.value
                defer { NanoMuseMordePersonalAttachmentStaging.discard(staged) }
                guard authorization.personalDataScopeKey == scope else { return }
                _ = try await MordeShellAuthorization.shared.uploadPersonalAttachment(objectID: objectID,
                    baseRevision: baseRevision, fileURL: staged.fileURL, sizeBytes: staged.sizeBytes,
                    sha256: staged.sha256, mediaType: mimeType(entry),
                    idempotencyKey: "library-upload-\(objectID)")
                guard authorization.personalDataScopeKey == scope else { return }
                await personal.refresh(root: NanoMuseDirs.root)
            } catch { libraryError = error.localizedDescription }
        }
    }

    private func mimeType(_ entry: NanoMuseLibraryEntry) -> String {
        UTType(filenameExtension: entry.url.pathExtension)?.preferredMIMEType ?? "application/octet-stream"
    }
}

struct NanoMuseShareTarget: Identifiable {
    var url: URL
    var id: String { url.absoluteString }
}

struct NanoMuseQuickLook: UIViewControllerRepresentable {
    let url: URL

    func makeUIViewController(context: Context) -> QLPreviewController {
        let controller = QLPreviewController()
        controller.dataSource = context.coordinator
        return controller
    }

    func updateUIViewController(_ controller: QLPreviewController, context: Context) {}

    func makeCoordinator() -> Coordinator { Coordinator(url: url) }

    final class Coordinator: NSObject, QLPreviewControllerDataSource {
        let url: URL
        init(url: URL) { self.url = url }
        func numberOfPreviewItems(in controller: QLPreviewController) -> Int { 1 }
        func previewController(_ controller: QLPreviewController, previewItemAt index: Int) -> QLPreviewItem { url as NSURL }
    }
}

struct NanoMuseShareSheet: UIViewControllerRepresentable {
    var items: [Any]

    func makeUIViewController(context: Context) -> UIActivityViewController {
        UIActivityViewController(activityItems: items, applicationActivities: nil)
    }

    func updateUIViewController(_ controller: UIActivityViewController, context: Context) {}
}
