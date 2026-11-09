//
//  NanoMuseSystemFiles.swift
//  nanoMuse
//
//  The agent's own files, readable and (mostly) editable by the person:
//  SOUL.md, USER.md, GLOBAL.md and the diary in the memory folder (what
//  the shell sees under /var/minis/memory), feed-preferences.md, and
//  HEARTBEAT.md — a read-only rendering of the routines. Plus "Import
//  memory": paste what another assistant remembers. Android: sysfiles/.
//

import Foundation
import SwiftUI
import UIKit

enum NanoMuseSystemFile: String, CaseIterable, Identifiable {
    case soul, user, memory, feed, heartbeat
    var id: String { rawValue }

    var fileName: String {
        switch self {
        case .soul: return "SOUL.md"
        case .user: return "USER.md"
        case .memory: return "GLOBAL.md"
        case .feed: return "feed-preferences.md"
        case .heartbeat: return "HEARTBEAT.md"
        }
    }

    var editable: Bool { self != .heartbeat }

    /// Where the file lives; HEARTBEAT has no file of its own.
    var url: URL {
        switch self {
        case .soul: return SoulStore.fileURL
        case .user, .memory: return NanoMuseDirs.memory.appendingPathComponent(fileName)
        case .feed: return NanoMuseDirs.root.appendingPathComponent(fileName)
        case .heartbeat: return NanoMuseDirs.root.appendingPathComponent(fileName)
        }
    }

    var about: String {
        switch self {
        case .soul: return AppLocalized("This is the agent's persona: the values and habits it tries to hold to in every conversation, its name, and the style it answers in. It starts from a template that ships with morde; the agent may refine it over time and tells you when it does. You can edit it at any time. This note is not part of the file.")
        case .user: return AppLocalized("The agent keeps this file as its working notes about you: your name, how you like to be addressed, your timezone, and the context it needs to be useful, such as what you are working on. It fills it in over time from your conversations and reads it at the start of each one. It reflects the agent's current understanding, so it can be incomplete or out of date. You can edit anything here, and the agent will follow what you write. This note is not part of the file.")
        case .memory: return AppLocalized("Durable notes the agent keeps across conversations: the things worth remembering, in its own words, plus anything you imported from another assistant. The day-by-day diary lives beside it in the memory folder. Read at the start of every conversation when memory is on. This note is not part of the file.")
        case .feed: return AppLocalized("One paragraph that steers the feed: what you want more of, what to skip, how long posts should be. The daily feed routine reads it before writing. This note is not part of the file.")
        case .heartbeat: return AppLocalized("Routines run while the app is open; at their time the phone reminds you to open it.")
        }
    }

    @MainActor
    func read() -> String {
        switch self {
        case .heartbeat: return NanoMuseSystemFiles.renderHeartbeat()
        case .feed: return NanoMuseFeedStore.shared.preferences
        default: return (try? String(contentsOf: url, encoding: .utf8)) ?? ""
        }
    }

    @MainActor
    @discardableResult
    func write(_ text: String) -> Bool {
        switch self {
        case .heartbeat:
            return false
        case .feed:
            NanoMuseFeedStore.shared.savePreferences(text)
            return true
        case .soul:
            // Keep the front matter the app manages; only the body is edited here.
            let parsed = SoulMDParser.parse(text)
            let current = SoulStore.load()
            let file = SoulFile(metadata: text.hasPrefix("---") ? parsed.metadata : (current?.metadata ?? .default), body: text.hasPrefix("---") ? parsed.body : text)
            do { try SoulStore.save(file); return true } catch { return false }
        default:
            do {
                try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
                try text.write(to: url, atomically: true, encoding: .utf8)
                return true
            } catch {
                return false
            }
        }
    }

    func personalDataChange(for text: String) throws -> NanoMuseSystemFilePersonalDataChange? {
        func contentBody(_ content: String) throws -> MordePersonalJSONValue? {
            guard !content.isEmpty else { return nil }
            guard content.unicodeScalars.count <= 65_536 else {
                throw NanoMuseMordePersonalDataOutboxError.tooLarge
            }
            return .object(["content": .string(content)])
        }

        switch self {
        case .soul:
            let parsed = SoulMDParser.parse(text)
            let content = parsed.body.isEmpty ? "\n" : parsed.body
            let name = parsed.metadata.name
            let style = parsed.metadata.style
            let lang = parsed.metadata.lang
            let emoji = parsed.metadata.emoji
            guard (1...80).contains(name.unicodeScalars.count), style.unicodeScalars.count <= 64,
                  lang.range(of: "^[A-Za-z0-9_-]{1,16}$", options: .regularExpression) != nil,
                  emoji.unicodeScalars.count <= 16, (1...65_536).contains(content.unicodeScalars.count) else {
                throw NanoMuseMordePersonalDataOutboxError.invalidMutation
            }
            var fields: [String: MordePersonalJSONValue] = [
                "name": .string(name), "lang": .string(lang), "content": .string(content),
            ]
            if !style.isEmpty { fields["style"] = .string(style) }
            if !emoji.isEmpty { fields["emoji"] = .string(emoji) }
            return NanoMuseSystemFilePersonalDataChange(objectType: "soul", objectID: "default",
                                                        body: .object(fields), isDelete: false)
        case .user, .memory:
            let objectID = self == .user ? "user" : "global"
            let body = try contentBody(text)
            return NanoMuseSystemFilePersonalDataChange(objectType: "memory", objectID: objectID,
                                                        body: body, isDelete: body == nil)
        case .feed, .heartbeat:
            return nil
        }
    }

    @MainActor
    var modifiedAt: Date? {
        switch self {
        case .heartbeat:
            return NanoMuseScheduler.shared.routines.compactMap { $0.lastFiredAt ?? $0.createdAt }.max()
        default:
            return (try? FileManager.default.attributesOfItem(atPath: url.path)[.modificationDate]) as? Date
        }
    }
}

struct NanoMuseSystemFilePersonalDataChange: Equatable {
    let objectType: String
    let objectID: String
    let body: MordePersonalJSONValue?
    let isDelete: Bool
}

enum NanoMuseDiarySyncPolicy {
    enum Reconciliation: Equatable {
        case noCopies, localOnly, cloudOnly, matching, different
    }

    struct CloudCopy: Equatable, Identifiable {
        let date: String
        let content: String
        let revision: Int64
        var id: String { date }
    }

    static func isValidDate(_ value: String) -> Bool {
        guard value.range(of: #"^[0-9]{4}-[0-9]{2}-[0-9]{2}$"#, options: .regularExpression) != nil,
              let year = Int(String(value.prefix(4))), year > 0,
              let month = Int(String(value.dropFirst(5).prefix(2))),
              let day = Int(String(value.suffix(2))), (1...12).contains(month) else { return false }
        let leapYear = year.isMultiple(of: 400) || (year.isMultiple(of: 4) && !year.isMultiple(of: 100))
        let daysInMonth = [31, leapYear ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31]
        return (1...daysInMonth[month - 1]).contains(day)
    }

    static func body(date: String, content: String) -> MordePersonalJSONValue? {
        guard isValidDate(date), content.unicodeScalars.count <= 65_536 else { return nil }
        return .object(["date": .string(date), "content": .string(content)])
    }

    static func cloudCopy(objectID: String, revision: Int64,
                          body: MordePersonalJSONValue) -> CloudCopy? {
        guard revision > 0, case .object(let fields) = body,
              Set(fields.keys) == Set(["date", "content"]),
              case .string(let date)? = fields["date"], date == objectID,
              case .string(let content)? = fields["content"],
              self.body(date: date, content: content) != nil else { return nil }
        return CloudCopy(date: date, content: content, revision: revision)
    }

    static func reconciliation(local: String?, cloud: String?) -> Reconciliation {
        switch (local, cloud) {
        case (.none, .none): return .noCopies
        case (.some(_), .none): return .localOnly
        case (.none, .some(_)): return .cloudOnly
        case let (.some(local), .some(cloud)): return local == cloud ? .matching : .different
        }
    }

    static func fileURL(date: String, directory: URL = NanoMuseDirs.memory) -> URL? {
        guard isValidDate(date) else { return nil }
        return directory.appendingPathComponent("\(date).md", isDirectory: false)
    }

    static func localEntries(in directory: URL = NanoMuseDirs.memory) -> [String: String] {
        guard let files = try? FileManager.default.contentsOfDirectory(
            at: directory, includingPropertiesForKeys: [.isRegularFileKey, .isSymbolicLinkKey]) else { return [:] }
        var entries: [String: String] = [:]
        for file in files where file.pathExtension == "md" {
            let date = file.deletingPathExtension().lastPathComponent
            guard isValidDate(date),
                  let values = try? file.resourceValues(forKeys: [.isRegularFileKey, .isSymbolicLinkKey]),
                  values.isRegularFile == true, values.isSymbolicLink != true,
                  let content = try? String(contentsOf: file, encoding: .utf8) else { continue }
            entries[date] = content
        }
        return entries
    }
}

@MainActor
enum NanoMuseSystemFiles {
    /// USER.md in the system prompt — one paragraph, only when the file has content.
    static func userPromptFragment() -> String? {
        let text = NanoMuseSystemFile.user.read().trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty else { return nil }
        return "## About the user (USER.md, maintained by you and the user)\n"
            + "Read at the start of every conversation. Fill it in over time from what the user tells you (name, how to address them, timezone, what they are working on, preferences); edit it with your shell at /var/minis/memory/USER.md when you learn something durable, and tell the user in one line when you do. Never write secrets into it.\n"
            + String(text.prefix(4000))
    }

    /// The routines, as the agent's heartbeat: what runs, when, and how the last run went.
    static func renderHeartbeat() -> String {
        let all = NanoMuseScheduler.shared.routines.sorted { ($0.hour * 60 + $0.minute) < ($1.hour * 60 + $1.minute) }
        var s = "# HEARTBEAT.md\n\n"
        s += AppLocalized("Routines run while the app is open; at their time the phone reminds you to open it.") + "\n\n"
        let routines = all.filter { !$0.hidden }
        s += "## " + AppLocalized("Routines") + "\n"
        if routines.isEmpty { s += "_" + AppLocalized("none") + "_\n" }
        for r in routines {
            // the same words as the Routines list: "Daily · 08:00", "Checks every 6 hours"
            s += "- \(r.enabled ? "[x]" : "[ ]") **\(r.label.isEmpty ? AppLocalized("Routine") : r.label)** · \(r.cadence)" + lastRun(r) + "\n"
        }
        return s
    }

    private static func lastRun(_ r: NanoMuseRoutine) -> String {
        guard let at = r.lastFiredAt else { return "" }
        let ok = r.runs.first?.ok ?? true
        return " · " + String(format: ok ? AppLocalized("last run %@") : AppLocalized("last run %@, failed"), NanoMuseDay.relative(at))
    }

    // MARK: Import memory

    /// The section heading in the person's language; any of these is recognised when appending.
    static let importHeadings = ["## 导入", "## 匯入", "## Imported"]

    /// Pure: `existing` GLOBAL.md plus a pasted block under "## Imported" (tested).
    static func appendImported(to existing: String, pasted: String, from: String?, heading: String, date: String) -> String? {
        let body = pasted.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !body.isEmpty else { return nil }
        let fromPart = (from ?? "").trimmingCharacters(in: .whitespaces)
        let header = "### \(date)" + (fromPart.isEmpty ? "" : " · \(fromPart)")
        let addition = "\n\(header)\n\n\(body)\n"
        let lines = existing.components(separatedBy: "\n")
        if let headingAt = lines.firstIndex(where: { importHeadings.contains($0.trimmingCharacters(in: .whitespaces)) }) {
            // At the end of the Imported section (before the next "## " heading, if any).
            let nextAt = ((headingAt + 1)..<lines.count).first { lines[$0].hasPrefix("## ") } ?? lines.count
            let before = lines[0..<nextAt].joined(separator: "\n").trimmingCharacters(in: .whitespacesAndNewlines)
            let after = lines[nextAt...].joined(separator: "\n")
            return before + "\n" + addition + (after.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty ? "" : "\n" + after)
        }
        let trimmed = existing.trimmingCharacters(in: .whitespacesAndNewlines)
        return (trimmed.isEmpty ? "" : trimmed + "\n\n") + heading + "\n" + addition
    }

    @discardableResult
    static func importMemory(pasted: String, from: String?) -> Bool {
        let file = NanoMuseSystemFile.memory.url
        let existing = (try? String(contentsOf: file, encoding: .utf8)) ?? ""
        let date = Date().formatted(date: .abbreviated, time: .omitted)
        guard let updated = appendImported(to: existing, pasted: pasted, from: from, heading: "## " + AppLocalized("Imported"), date: date) else { return false }
        do {
            try FileManager.default.createDirectory(at: file.deletingLastPathComponent(), withIntermediateDirectories: true)
            try updated.write(to: file, atomically: true, encoding: .utf8)
            return true
        } catch {
            return false
        }
    }

    static var importPrompt: String { AppLocalized("Please gather everything you remember about me into a concise list of bullet points: who I am, how I like to be addressed, where I live and my timezone, what I do, what I am working on right now, my preferences and habits, and anything you were told to always or never do. Plain text, no headings, no secrets or passwords.") }
}

// MARK: - Views

struct NanoMuseSystemFilesView: View {
    @State private var byRecent = false
    @State private var tick = 0

    private var files: [NanoMuseSystemFile] {
        let all = NanoMuseSystemFile.allCases.filter { $0 != .feed }
        guard byRecent else { return all }
        return all.sorted { ($0.modifiedAt ?? .distantPast) > ($1.modifiedAt ?? .distantPast) }
    }

    var body: some View {
        List {
            Section {
                ForEach(files) { file in
                    NavigationLink {
                        NanoMuseSystemFileView(file: file)
                    } label: {
                        HStack(spacing: 12) {
                            Image(systemName: file == .heartbeat ? "waveform.path.ecg" : "doc.text")
                                .foregroundStyle(NanoMuseTones.action)
                                .frame(width: 24)
                            VStack(alignment: .leading, spacing: 2) {
                                Text(file.fileName).font(.body.weight(.medium))
                                if let at = file.modifiedAt {
                                    Text(NanoMuseDay.relative(at)).font(.caption).foregroundStyle(.secondary)
                                }
                            }
                        }
                    }
                }
            } footer: {
                Text(AppLocalized("These files are the agent's: who it is, what it knows about you, what it remembers, what it writes for you and when it wakes up. They live in your phone's storage; the agent reads and edits them from its shell under /var/minis/memory, and you can edit anything here."))
            }
            Section {
                NavigationLink(AppLocalized("Import memory")) { NanoMuseMemoryImportView() }
            }
#if MINIS_CLOUD_ONLY
            Section {
                NavigationLink("Diary sync") { NanoMuseDiarySyncView() }
            }
#endif
        }
        .id(tick)
        .navigationTitle(AppLocalized("System files"))
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                Button {
                    byRecent.toggle()
                } label: {
                    Image(systemName: byRecent ? "clock" : "textformat.abc")
                }
                .accessibilityLabel(Text(AppLocalized("Change order")))
            }
        }
        .onAppear { tick += 1 }
    }
}

struct NanoMuseSystemFileView: View {
    var file: NanoMuseSystemFile
#if MINIS_CLOUD_ONLY
    @ObservedObject private var authorization = MordeShellAuthorization.shared
#endif
    @State private var text = ""
    @State private var editing = false
    @State private var draft = ""
    @State private var copied = false
    @State private var share = false
    @State private var saveMessage: String?

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                Text(AppLocalized("About this file.") + " " + file.about)
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                    .padding(12)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(NanoMuseTones.fill, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
                if editing {
                    TextEditor(text: $draft)
                        .font(.system(.body, design: .monospaced))
                        .frame(minHeight: 320)
                        .padding(6)
                        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
                    Text(String(format: AppLocalized("Write %@ in Markdown. Headings and bullet points read best; keep secrets out."), file.fileName))
                        .font(.caption).foregroundStyle(.secondary)
                } else if text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                    Text(file.editable ? AppLocalized("Nothing here yet. Tap the pencil to write it, or let the agent fill it in as you talk.") : AppLocalized("Nothing here yet."))
                        .font(.body).foregroundStyle(.secondary)
                        .padding(.top, 20)
                } else {
                    Text(text)
                        .font(.system(.body, design: .monospaced))
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
                if let saveMessage {
                    Text(saveMessage).font(.footnote).foregroundStyle(.red)
                }
#if MINIS_CLOUD_ONLY
                if file == .soul || file == .user || file == .memory {
                    VStack(alignment: .leading, spacing: 10) {
                        HStack {
                            Text(AppLocalized("Morde copy"))
                                .font(.headline)
                            Spacer()
                            if remoteLoading { ProgressView() }
                            else {
                                Button(AppLocalized("Refresh")) { Task { await loadMordeCopy() } }
                                    .font(.footnote)
                            }
                        }
                        if let remoteError = currentRemoteError {
                            Text(remoteError).font(.footnote).foregroundStyle(.red)
                        } else if remoteLoading {
                            Text(AppLocalized("Loading Morde copy…")).font(.footnote).foregroundStyle(.secondary)
                        } else if currentRemoteText.isEmpty {
                            Text(AppLocalized("No Morde copy has been saved yet.")).font(.footnote).foregroundStyle(.secondary)
                        } else {
                            Text(currentRemoteText)
                                .font(.system(.footnote, design: .monospaced))
                                .textSelection(.enabled)
                                .frame(maxWidth: .infinity, alignment: .leading)
                            Button(AppLocalized("Use Morde copy as draft")) {
                                draft = currentRemoteText
                                editing = true
                            }
                            .disabled(!file.editable)
                        }
                    }
                    .padding(12)
                    .background(NanoMuseTones.fill, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
                }
#endif
            }
            .padding(16)
        }
        .background(NanoMuseTones.canvas.ignoresSafeArea())
        .navigationTitle(file.fileName)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItemGroup(placement: .topBarTrailing) {
                if editing {
                    Button(AppLocalized("Save")) {
                        guard file.write(draft) else {
                            saveMessage = AppLocalized("The file could not be saved on this device.")
                            return
                        }
                        text = file.read()
                        editing = false
                        do {
                            try NanoMuseMordeProfileSync.shared.enqueueSystemFile(file, text: text)
                            saveMessage = nil
                        } catch {
                            saveMessage = String(format: AppLocalized("Saved on this device, but could not queue Morde sync: %@"), error.localizedDescription)
                        }
                    }
                } else {
                    Menu {
                        if file.editable {
                            Button { draft = text; editing = true } label: { Label(AppLocalized("Edit"), systemImage: "pencil") }
                        }
                        Button {
                            UIPasteboard.general.string = text
                            copied = true
                        } label: { Label(copied ? AppLocalized("Copied") : AppLocalized("Copy contents"), systemImage: "doc.on.doc") }
                        Button { share = true } label: { Label(AppLocalized("Share"), systemImage: "square.and.arrow.up") }
                    } label: {
                        Image(systemName: "ellipsis.circle")
                    }
                }
            }
        }
#if MINIS_CLOUD_ONLY
        .task(id: authorization.personalDataScopeKey) {
            text = file.read()
            await NanoMuseMordeProfileSync.shared.prepareRevision(for: file, text: text)
            await loadMordeCopy()
        }
#else
        .task {
            text = file.read()
            await NanoMuseMordeProfileSync.shared.prepareRevision(for: file, text: text)
        }
#endif
        .sheet(isPresented: $share) {
            NanoMuseShareSheet(items: [text])
        }
    }

#if MINIS_CLOUD_ONLY
    @State private var remoteText = ""
    @State private var remoteLoading = false
    @State private var remoteError: String?
    @State private var remoteScope: String?

    private var currentRemoteText: String {
        guard authorization.signedIn, let scope = authorization.personalDataScopeKey,
              remoteScope == scope else { return "" }
        return remoteText
    }

    private var currentRemoteError: String? {
        guard authorization.signedIn else { return remoteScope == nil ? remoteError : nil }
        guard let scope = authorization.personalDataScopeKey, remoteScope == scope else { return nil }
        return remoteError
    }

    @MainActor
    private func loadMordeCopy() async {
        remoteText = ""
        remoteScope = nil
        remoteError = nil
        guard authorization.signedIn, let scope = authorization.personalDataScopeKey else {
            remoteText = ""
            remoteError = AppLocalized("Sign in to load the Morde copy.")
            return
        }
        let type = file == .soul ? "soul" : "memory"
        let objectID = file == .soul ? "default" : (file == .user ? "user" : "global")
        remoteLoading = true
        remoteError = nil
        defer { remoteLoading = false }
        do {
            let object = try await MordeShellAuthorization.shared.personalObject(objectType: type, objectID: objectID)
            guard scope == MordeShellAuthorization.shared.personalDataScopeKey else { return }
            guard let object, case .object(let fields) = object.body,
                  case .string(let content) = fields["content"] else {
                remoteText = ""
                remoteScope = scope
                return
            }
            remoteText = content
            remoteScope = scope
        } catch {
            guard scope == MordeShellAuthorization.shared.personalDataScopeKey else { return }
            remoteError = error.localizedDescription
            remoteScope = scope
        }
    }
#endif
}


struct NanoMuseMemoryImportView: View {
    @State private var from = ""
    @State private var pasted = ""
    @State private var result: String?
    @State private var copiedPrompt = false

    var body: some View {
        Form {
            Section {
                Text(AppLocalized("Bring over what another assistant already knows about you. Ask it with the prompt below, paste its answer here, and it is appended to GLOBAL.md under “Imported”, where your agent reads it at the start of every conversation."))
                    .font(.footnote).foregroundStyle(.secondary)
            }
            Section(AppLocalized("Ask your other assistant")) {
                Text(NanoMuseSystemFiles.importPrompt).font(.footnote)
                Button(copiedPrompt ? AppLocalized("Copied") : AppLocalized("Copy prompt")) {
                    UIPasteboard.general.string = NanoMuseSystemFiles.importPrompt
                    copiedPrompt = true
                }
            }
            Section(AppLocalized("Paste its answer")) {
                TextField(AppLocalized("From (optional), e.g. ChatGPT, Claude"), text: $from)
                TextEditor(text: $pasted).frame(minHeight: 140)
                    .overlay(alignment: .topLeading) {
                        if pasted.isEmpty {
                            Text(AppLocalized("Paste the list here")).foregroundStyle(.tertiary).padding(.top, 8).padding(.leading, 4).allowsHitTesting(false)
                        }
                    }
            }
            Section {
                Button(AppLocalized("Add to memory")) {
                    if NanoMuseSystemFiles.importMemory(pasted: pasted, from: from) {
                        do {
                            try NanoMuseMordeProfileSync.shared.enqueueSystemFile(.memory,
                                                                                  text: NanoMuseSystemFile.memory.read())
                            result = AppLocalized("Added to GLOBAL.md")
                        } catch {
                            result = String(format: AppLocalized("Added locally, but could not queue Morde sync: %@"), error.localizedDescription)
                        }
                        pasted = ""
                    } else {
                        result = AppLocalized("Nothing to add")
                    }
                }
                .disabled(pasted.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                if let result { Text(result).font(.footnote).foregroundStyle(.secondary) }
            } footer: {
                Text(AppLocalized("Look it over first: leave out passwords, card numbers and anything you would not want in a plain text file on your phone."))
            }
        }
        .navigationTitle(AppLocalized("Import memory"))
        .navigationBarTitleDisplayMode(.inline)
    }
}

#if MINIS_CLOUD_ONLY
private struct NanoMuseDiarySyncView: View {
    @ObservedObject private var authorization = MordeShellAuthorization.shared
    @State private var localEntries: [String: String] = [:]
    @State private var cloudCopies: [String: NanoMuseDiarySyncPolicy.CloudCopy] = [:]
    @State private var cloudLoading = false
    @State private var cloudSnapshotLoaded = false
    @State private var cloudError: String?
    @State private var loadedScope: String?
    @State private var loadGeneration = 0

    private var dates: [String] {
        Set(localEntries.keys).union(cloudCopies.keys).sorted(by: >)
    }

    var body: some View {
        List {
            Section {
                Text("Diary files stay on this iPhone unless you choose a copy to sync.")
                    .font(.footnote).foregroundStyle(.secondary)
                if !authorization.signedIn {
                    Text("Sign in to inspect Morde diary copies.")
                        .font(.footnote).foregroundStyle(.secondary)
                }
                if cloudLoading {
                    ProgressView("Loading Morde diary…")
                } else {
                    Button("Refresh Morde diary") { Task { await refresh() } }
                        .disabled(!authorization.signedIn)
                }
                if let cloudError {
                    Text(cloudError).font(.footnote).foregroundStyle(.red)
                }
            }

            Section("Diary entries") {
                if dates.isEmpty && !cloudLoading {
                    Text("No local or Morde diary entries.")
                        .font(.footnote).foregroundStyle(.secondary)
                }
                ForEach(dates, id: \.self) { date in
                    let local = localEntries[date]
                    let cloud = cloudCopies[date]
                    NavigationLink {
                        NanoMuseDiarySyncDayView(
                            date: date, localContent: local, cloudCopy: cloud,
                            scope: loadedScope, canQueue: cloudSnapshotLoaded && authorization.signedIn,
                            onSaved: {
                                localEntries = NanoMuseDiarySyncPolicy.localEntries()
                                Task { await refresh() }
                            })
                    } label: {
                        VStack(alignment: .leading, spacing: 3) {
                            Text(date).font(.body.weight(.medium))
                            Text(reconciliationLabel(NanoMuseDiarySyncPolicy.reconciliation(
                                local: local, cloud: cloud?.content)))
                                .font(.caption).foregroundStyle(.secondary)
                        }
                    }
                }
            }
        }
        .navigationTitle("Diary sync")
        .task(id: authorization.personalDataScopeKey) { await refresh() }
        .refreshable { await refresh() }
    }

    @MainActor
    private func refresh() async {
        loadGeneration += 1
        let generation = loadGeneration
        localEntries = NanoMuseDiarySyncPolicy.localEntries()
        cloudCopies = [:]
        cloudSnapshotLoaded = false
        cloudError = nil
        let scope = authorization.personalDataScopeKey
        loadedScope = scope
        guard authorization.signedIn, let scope else { return }

        cloudLoading = true
        defer {
            if generation == loadGeneration { cloudLoading = false }
        }
        do {
            var copies: [String: NanoMuseDiarySyncPolicy.CloudCopy] = [:]
            var cursor: String?
            var seen = Set<String>()
            repeat {
                guard generation == loadGeneration, authorization.personalDataScopeKey == scope,
                      !Task.isCancelled else { return }
                let page = try await authorization.personalSnapshot(cursor: cursor)
                guard generation == loadGeneration, authorization.personalDataScopeKey == scope,
                      !Task.isCancelled else { return }
                for item in page.items where item.object_type == "diary" {
                    guard let copy = NanoMuseDiarySyncPolicy.cloudCopy(
                        objectID: item.object_id, revision: item.revision, body: item.body),
                          copies[copy.date] == nil else {
                        throw URLError(.cannotParseResponse)
                    }
                    copies[copy.date] = copy
                }
                cursor = page.next_cursor
                if MordePersonalSnapshotCursorPolicy.repeats(cursor, seen: &seen) {
                    throw URLError(.cannotParseResponse)
                }
            } while cursor?.isEmpty == false
            guard generation == loadGeneration, authorization.personalDataScopeKey == scope,
                  !Task.isCancelled else { return }
            cloudCopies = copies
            cloudSnapshotLoaded = true
        } catch {
            guard generation == loadGeneration, authorization.personalDataScopeKey == scope,
                  !Task.isCancelled else { return }
            cloudError = error.localizedDescription
        }
    }

    private func reconciliationLabel(_ value: NanoMuseDiarySyncPolicy.Reconciliation) -> String {
        switch value {
        case .noCopies: return "No copy"
        case .localOnly: return "On this iPhone"
        case .cloudOnly: return "Morde copy only"
        case .matching: return "Copies match"
        case .different: return "Copies differ · choose one to draft"
        }
    }
}

private struct NanoMuseDiarySyncDayView: View {
    let date: String
    let localContent: String?
    let cloudCopy: NanoMuseDiarySyncPolicy.CloudCopy?
    let scope: String?
    let canQueue: Bool
    let onSaved: () -> Void
    @ObservedObject private var authorization = MordeShellAuthorization.shared
    @ObservedObject private var sync = NanoMuseMordeProfileSync.shared
    @State private var draft = ""
    @State private var draftBaseRevision: Int64 = 0
    @State private var isEditing = false
    @State private var message: String?

    private var visibleCloudCopy: NanoMuseDiarySyncPolicy.CloudCopy? {
        guard authorization.signedIn, let scope,
              authorization.personalDataScopeKey == scope else { return nil }
        return cloudCopy
    }

    private var canQueueForCurrentScope: Bool {
        canQueue && authorization.signedIn && scope != nil &&
            authorization.personalDataScopeKey == scope
    }

    var body: some View {
        Form {
            Section("On this iPhone") {
                if let localContent {
                    Text(localContent.isEmpty ? "(Empty diary entry)" : localContent)
                        .textSelection(.enabled)
                } else {
                    Text("No local diary file for this date.").foregroundStyle(.secondary)
                }
            }
            Section("Morde copy") {
                if let cloudCopy = visibleCloudCopy {
                    Text("Revision \(cloudCopy.revision)").font(.caption).foregroundStyle(.secondary)
                    Text(cloudCopy.content.isEmpty ? "(Empty diary entry)" : cloudCopy.content)
                        .textSelection(.enabled)
                } else {
                    Text(scope == authorization.personalDataScopeKey
                         ? "No Morde copy for this date."
                         : "The Morde account changed. The previous account copy is hidden; return and refresh.")
                        .foregroundStyle(.secondary)
                }
            }
            Section("Copies") {
                Text(reconciliationLabel).font(.footnote).foregroundStyle(.secondary)
            }
            Section("Choose a draft") {
                if let cloudCopy = visibleCloudCopy {
                    Button("Use Morde copy as draft") {
                        beginDraft(cloudCopy.content, baseRevision: cloudCopy.revision)
                    }
                    .disabled(!canQueueForCurrentScope)
                }
                if let localContent {
                    Button("Use local diary as cloud draft") {
                        beginDraft(localContent, baseRevision: visibleCloudCopy?.revision ?? 0)
                    }
                    .disabled(!canQueueForCurrentScope)
                }
                if !canQueueForCurrentScope {
                    Text("Refresh the signed-in Morde account before choosing a copy to sync.")
                        .font(.footnote).foregroundStyle(.secondary)
                }
            }
            if isEditing {
                Section("Draft") {
                    TextEditor(text: $draft).frame(minHeight: 220)
                    Button("Save draft to this iPhone and queue Morde sync") {
                        Task { await saveDraft() }
                    }
                    .disabled(!canQueueForCurrentScope)
                    Button("Cancel", role: .cancel) { isEditing = false }
                }
            }
            if let message {
                Section { Text(message).font(.footnote).foregroundStyle(.secondary) }
            }
        }
        .navigationTitle(date)
    }

    private var reconciliationLabel: String {
        switch NanoMuseDiarySyncPolicy.reconciliation(local: localContent, cloud: visibleCloudCopy?.content) {
        case .noCopies: return "No copy"
        case .localOnly: return "Only on this iPhone"
        case .cloudOnly: return "Only in Morde"
        case .matching: return "Copies match"
        case .different: return "Copies differ. Choose which copy to use as a draft."
        }
    }

    private func beginDraft(_ content: String, baseRevision: Int64) {
        draft = content
        draftBaseRevision = baseRevision
        message = nil
        isEditing = true
    }

    @MainActor
    private func saveDraft() async {
        guard canQueueForCurrentScope, let scope, authorization.signedIn,
              authorization.personalDataScopeKey == scope else {
            message = "The Morde account changed. Refresh before saving this draft."
            return
        }
        guard let body = NanoMuseDiarySyncPolicy.body(date: date, content: draft),
              let file = NanoMuseDiarySyncPolicy.fileURL(date: date) else {
            message = "The diary date or content is invalid or too large."
            return
        }
        if let values = try? file.resourceValues(forKeys: [.isSymbolicLinkKey]), values.isSymbolicLink == true {
            message = "The local diary path is not a regular diary file."
            return
        }
        if FileManager.default.fileExists(atPath: file.path) {
            guard let currentLocal = try? String(contentsOf: file, encoding: .utf8),
                  currentLocal == localContent else {
                message = "The local diary changed after you opened it. Refresh before saving this draft."
                return
            }
        } else if localContent != nil {
            message = "The local diary changed after you opened it. Refresh before saving this draft."
            return
        }
        do {
            try FileManager.default.createDirectory(at: file.deletingLastPathComponent(),
                                                    withIntermediateDirectories: true)
            try draft.write(to: file, atomically: true, encoding: .utf8)
        } catch {
            message = "The diary draft could not be saved on this iPhone: \(error.localizedDescription)"
            return
        }

        guard authorization.personalDataScopeKey == scope else {
            message = "Saved locally; the Morde account changed before the sync could be queued."
            isEditing = false
            onSaved()
            return
        }
        if visibleCloudCopy?.content == draft {
            message = "Saved the Morde copy on this iPhone. No cloud change was queued."
            isEditing = false
            onSaved()
            return
        }
        let mutation = MordePersonalDataMutation(
            scopeKey: scope, mutationID: UUID().uuidString.replacingOccurrences(of: "-", with: ""),
            objectType: "diary", objectID: date, baseRevision: draftBaseRevision,
            body: body, isDelete: false)
        do {
            try NanoMuseMordePersonalDataOutbox.enqueue(mutation, scopeKey: scope, root: NanoMuseDirs.root)
            sync.refresh()
            await sync.retry()
            message = sync.pending?.objectType == "diary"
                ? (sync.syncEnabled ? "Saved locally; diary is queued for Morde sync." : "Saved locally; Morde sync is paused.")
                : "Saved locally; Morde sync was attempted."
        } catch {
            message = "Saved locally, but the diary could not be queued for Morde sync: \(error.localizedDescription)"
        }
        isEditing = false
        onSaved()
    }
}
#endif
