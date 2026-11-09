//
//  NanoMuseFeed.swift
//  nanoMuse
//
//  The Feed, Muse style: short posts the agent writes for the person, from
//  what it remembers (GLOBAL.md, the last week of diary, USER.md, goals).
//  One built-in routine sends "Write today's feed." into the feed's own
//  conversation; the model answers with ```nanomuse-feed``` blocks, which
//  become `nanomuse/feed/YYYY-MM-DD/NN.md` files and cards. Plain files on
//  purpose — the agent can read them back and a backup carries them.
//  Android: feed/FeedPost.kt, feed/FeedStore.kt, feed/FeedFlow.kt, ui/feed/FeedTab.kt.
//

import Foundation
import SwiftUI

struct NanoMuseMordeFeedPostBody: Codable, Equatable {
    var title: String
    var type: String
    var emoji: String?
    var created_at: String?
    var liked: Bool?
    var body: String

    var isValid: Bool {
        !title.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && title.unicodeScalars.count <= 200 &&
            NanoMusePost.types.contains(type) && (emoji?.unicodeScalars.count ?? 0) <= 16 &&
            (created_at.map { ISO8601DateFormatter().date(from: $0) != nil } ?? true) &&
            body.unicodeScalars.count > 0 && body.unicodeScalars.count <= 65_536
    }
}

struct NanoMuseMordeFeedPost: Identifiable, Equatable {
    let id: String
    let revision: Int64
    let body: NanoMuseMordeFeedPostBody
}

struct NanoMuseMordeFeedPreferences: Codable, Equatable {
    let content: String
    var isValid: Bool { !content.isEmpty && content.unicodeScalars.count <= 8192 }
}

struct NanoMuseMordeIdeaMarkBody: Codable, Equatable {
    let state: String
    var isValid: Bool { state == "saved" || state == "dismissed" }
}

// MARK: - Post (pure parse/serialize, tested)

struct NanoMusePost: Identifiable, Equatable {
    var day: String
    var index: Int
    var title: String
    var type: String
    var emoji: String
    var body: String
    var source: [String]
    var createdAt: Date
    var liked: Bool
    var file: URL

    var id: String { "\(day)/" + String(format: "%02d", index) }

    static let types: Set<String> = ["brief", "reminder", "idea", "goal", "memory", "note"]

    static func typeLabel(_ type: String) -> String {
        switch type {
        case "brief": return AppLocalized("Briefing")
        case "reminder": return AppLocalized("Reminder")
        case "idea": return AppLocalized("Idea")
        case "goal": return AppLocalized("Goal")
        case "memory": return AppLocalized("Memory")
        default: return AppLocalized("Note")
        }
    }

    static func defaultEmoji(_ type: String) -> String {
        switch type {
        case "brief": return "📰"
        case "reminder": return "⏰"
        case "idea": return "💡"
        case "goal": return "🎯"
        case "memory": return "🧠"
        default: return "📝"
        }
    }

    private static let iso: DateFormatter = {
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX")
        f.dateFormat = "yyyy-MM-dd'T'HH:mm:ssZ"
        return f
    }()

    private static func escape(_ s: String) -> String {
        s.replacingOccurrences(of: "\n", with: " ").replacingOccurrences(of: ";", with: "，").trimmingCharacters(in: .whitespaces)
    }

    private static func unescape(_ s: String) -> String {
        var t = s.trimmingCharacters(in: .whitespaces)
        if t.count >= 2, t.hasPrefix("\""), t.hasSuffix("\"") { t = String(t.dropFirst().dropLast()) }
        return t
    }

    func serialize() -> String {
        var s = "---\n"
        s += "title: \(Self.escape(title))\n"
        s += "type: \(type)\n"
        if !emoji.trimmingCharacters(in: .whitespaces).isEmpty { s += "emoji: \(emoji)\n" }
        if !source.isEmpty { s += "source: \(source.map(Self.escape).joined(separator: "; "))\n" }
        s += "created: \(Self.iso.string(from: createdAt))\n"
        if liked { s += "liked: true\n" }
        s += "---\n\n"
        s += body.trimmingCharacters(in: .whitespacesAndNewlines) + "\n"
        return s
    }

    /// Front matter + body → a post; nil when the name or the text are not a post.
    static func parse(text: String, day: String, index: Int, file: URL, modified: Date) -> NanoMusePost? {
        let (meta, body) = splitFrontMatter(text)
        let created = meta["created"].flatMap { iso.date(from: $0) } ?? modified
        var title = meta["title"].map(unescape) ?? ""
        if title.isEmpty {
            title = body.split(separator: "\n", omittingEmptySubsequences: true).first.map { String($0).trimmingCharacters(in: CharacterSet(charactersIn: "# ")) } ?? ""
        }
        let source = (meta["source"] ?? "").split(separator: ";").map { unescape(String($0)) }.filter { !$0.isEmpty }
        let type = (meta["type"] ?? "").isEmpty ? "note" : meta["type"]!
        return NanoMusePost(day: day, index: index, title: title, type: type, emoji: meta["emoji"] ?? "", body: body.trimmingCharacters(in: .whitespacesAndNewlines), source: source, createdAt: created, liked: meta["liked"] == "true", file: file)
    }

    static func splitFrontMatter(_ text: String) -> ([String: String], String) {
        let lines = text.components(separatedBy: "\n")
        guard lines.first?.trimmingCharacters(in: .whitespaces) == "---" else { return ([:], text) }
        guard let end = lines.dropFirst().firstIndex(where: { $0.trimmingCharacters(in: .whitespaces) == "---" }) else { return ([:], text) }
        var meta: [String: String] = [:]
        for line in lines[1..<end] {
            guard let i = line.firstIndex(of: ":"), i > line.startIndex else { continue }
            meta[String(line[..<i]).trimmingCharacters(in: .whitespaces)] = String(line[line.index(after: i)...]).trimmingCharacters(in: .whitespaces)
        }
        let body = lines.dropFirst(end + 1).joined(separator: "\n")
        return (meta, body)
    }

    /// What the model's `nanomuse-feed` block becomes before it is written.
    struct Draft: Equatable {
        var title: String
        var type: String
        var emoji: String
        var body: String
        var source: [String]

        init?(block o: [String: Any]) {
            let title = (o["title"] as? String ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
            let body = (o["body"] as? String ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
            guard !title.isEmpty, !body.isEmpty else { return nil }
            let rawType = (o["type"] as? String ?? "").trimmingCharacters(in: .whitespaces).lowercased()
            var source: [String] = []
            if let arr = o["source"] as? [Any] {
                source = arr.compactMap { ($0 as? String)?.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
            } else if let s = o["source"] as? String {
                source = s.split(whereSeparator: { $0 == ";" || $0 == "," }).map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
            }
            self.title = title
            self.body = body
            self.type = NanoMusePost.types.contains(rawType) ? rawType : "note"
            self.emoji = String((o["emoji"] as? String ?? "").trimmingCharacters(in: .whitespaces).prefix(4))
            self.source = Array(source.prefix(4))
        }
    }
}

// MARK: - Store

@MainActor
final class NanoMuseFeedStore: ObservableObject {
    static let shared = NanoMuseFeedStore()
    static let maxPostsPerDay = 12
    static let keepDays = 30

    /// Newest day first, then by index.
    @Published private(set) var posts: [NanoMusePost] = []
    @Published private(set) var preferences: String = ""

    var root: URL { NanoMuseDirs.root.appendingPathComponent("feed", isDirectory: true) }
    var preferencesFile: URL { NanoMuseDirs.root.appendingPathComponent("feed-preferences.md") }

    private init() {
        try? FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        reload()
    }

    static var defaultPreferences: String {
        AppLocalized("Build me a feed about what I care about. Keep it short and direct, easy to skim, no clickbait.")
    }

    private static let dayName = try! NSRegularExpression(pattern: "^\\d{4}-\\d{2}-\\d{2}$")
    private static let fileName = try! NSRegularExpression(pattern: "^(\\d{2})\\.md$")

    private static func isDay(_ name: String) -> Bool {
        dayName.firstMatch(in: name, range: NSRange(location: 0, length: (name as NSString).length)) != nil
    }

    private static func index(of name: String) -> Int? {
        let ns = name as NSString
        guard let m = fileName.firstMatch(in: name, range: NSRange(location: 0, length: ns.length)) else { return nil }
        return Int(ns.substring(with: m.range(at: 1)))
    }

    func reload() {
        let fm = FileManager.default
        let days = ((try? fm.contentsOfDirectory(atPath: root.path)) ?? []).filter(Self.isDay).sorted(by: >)
        var all: [NanoMusePost] = []
        for day in days {
            let dir = root.appendingPathComponent(day, isDirectory: true)
            let files = ((try? fm.contentsOfDirectory(atPath: dir.path)) ?? [])
            var ofDay: [NanoMusePost] = []
            for name in files {
                guard let index = Self.index(of: name) else { continue }
                let url = dir.appendingPathComponent(name)
                guard let text = try? String(contentsOf: url, encoding: .utf8) else { continue }
                let modified = (try? fm.attributesOfItem(atPath: url.path)[.modificationDate] as? Date) ?? Date()
                if let post = NanoMusePost.parse(text: text, day: day, index: index, file: url, modified: modified) { ofDay.append(post) }
            }
            // A batch is written in one go and shares a `created` time: the newest batch first,
            // and inside a batch the model's order, most important first (#555).
            all.append(contentsOf: ofDay.sorted { a, b in
                a.createdAt != b.createdAt ? a.createdAt > b.createdAt : a.index < b.index
            })
        }
        posts = all
        let saved = ((try? String(contentsOf: preferencesFile, encoding: .utf8)) ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        preferences = saved.isEmpty ? Self.defaultPreferences : saved
    }

    func savePreferences(_ text: String) {
        let t = text.trimmingCharacters(in: .whitespacesAndNewlines)
        try? (t + "\n").write(to: preferencesFile, atomically: true, encoding: .utf8)
        preferences = t.isEmpty ? Self.defaultPreferences : t
    }

    func setLiked(_ post: NanoMusePost, _ liked: Bool) {
        var p = post
        p.liked = liked
        try? p.serialize().write(to: p.file, atomically: true, encoding: .utf8)
        posts = posts.map { $0.file == post.file ? p : $0 }
    }

    func delete(_ post: NanoMusePost) {
        try? FileManager.default.removeItem(at: post.file)
        posts.removeAll { $0.file == post.file }
    }

    /// Writes a batch for today, continuing today's numbering. Returns what was written.
    @discardableResult
    func append(_ drafts: [NanoMusePost.Draft], now: Date = Date()) -> [NanoMusePost] {
        guard !drafts.isEmpty else { return [] }
        let day = NanoMuseDay.key(now)
        let dir = root.appendingPathComponent(day, isDirectory: true)
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        var next = (posts.filter { $0.day == day }.map(\.index).max() ?? 0) + 1
        var written: [NanoMusePost] = []
        for d in drafts {
            if next > Self.maxPostsPerDay { break }
            let file = dir.appendingPathComponent(String(format: "%02d.md", next))
            let post = NanoMusePost(day: day, index: next, title: String(d.title.prefix(80)), type: d.type, emoji: d.emoji, body: d.body, source: d.source, createdAt: now, liked: false, file: file)
            do {
                try post.serialize().write(to: file, atomically: true, encoding: .utf8)
                written.append(post)
                next += 1
            } catch {
                continue
            }
        }
        if !written.isEmpty {
            reload()
            prune()
        }
        return written
    }

    private func prune() {
        let fm = FileManager.default
        let days = ((try? fm.contentsOfDirectory(atPath: root.path)) ?? []).filter(Self.isDay).sorted(by: >)
        for day in days.dropFirst(Self.keepDays) {
            try? fm.removeItem(at: root.appendingPathComponent(day))
        }
    }

    var latestDay: String? { posts.first?.day }
}

// MARK: - Flow

@MainActor
enum NanoMuseFeedFlow {
    static let block = "feed"
    static let defaultHour = 8
    static let defaultMinute = 0

    private enum Keys {
        static let routine = "nanomuse.feed.routine"
        static let session = "nanomuse.feed.session"
        static let introAck = "nanomuse.feed.intro_ack"
    }

    static var routineId: String? { UserDefaults.standard.string(forKey: Keys.routine) }
    static var sessionId: String? { UserDefaults.standard.string(forKey: Keys.session) }
    static var routine: NanoMuseRoutine? { routineId.flatMap { NanoMuseScheduler.shared.routine(id: $0) } }
    static func isFeedSession(_ id: String) -> Bool { !id.isEmpty && id == sessionId }

    static var introAcknowledged: Bool {
        get { UserDefaults.standard.bool(forKey: Keys.introAck) }
        set { UserDefaults.standard.set(newValue, forKey: Keys.introAck) }
    }

    static var hasModel: Bool {
        ProviderConfigStore.shared.defaultPrimaryGroupId != nil || !ProviderConfigStore.shared.modelEntries.isEmpty
    }
    static var canRun: Bool { MordeLocalExecutionPolicy.isEnabled && hasModel }

    // nanoMuse: Existing feed posts and routines stay readable; Morde does not create local Feed sessions.
    @discardableResult
    static func ensureRoutine() async -> NanoMuseRoutine? {
        nil
    }

    static var generating: Bool { NanoMuseScheduler.shared.running.contains(routineId ?? "") }

    /// "Write it now".
    static func generateNow() {
        guard MordeLocalExecutionPolicy.isEnabled else { return }
        Task { @MainActor in
            guard let r = await ensureRoutine() else { return }
            await NanoMuseScheduler.shared.run(r.id, trigger: "feed-now")
        }
    }

    /// C5: the first feed day, written on its own right after the first conversation ends
    /// (the agent has its name) when a model is there. Once per phone; a background run in
    /// the feed's own conversation, so it is never counted as a task.
    static func writeFirstDayIfNeeded() {
        let key = "nanomuse.feed.first_day_written"
        guard canRun, !UserDefaults.standard.bool(forKey: key), NanoMuseFeedStore.shared.posts.isEmpty, !generating else { return }
        UserDefaults.standard.set(true, forKey: key)
        Task { @MainActor in
            guard let r = await ensureRoutine() else { return }
            await NanoMuseScheduler.shared.run(r.id, trigger: "feed-first-day")
        }
    }

    static func setEnabled(_ on: Bool) {
        guard MordeLocalExecutionPolicy.isEnabled else { return }
        guard let id = routineId else { return }
        NanoMuseScheduler.shared.setEnabled(id, on)
    }

    static func setTime(hour: Int, minute: Int) {
        guard MordeLocalExecutionPolicy.isEnabled else { return }
        guard var r = routine else { return }
        r.hour = hour
        r.minute = minute
        NanoMuseScheduler.shared.update(r)
        NanoMuseScheduler.shared.refreshNotifications()
    }

    /// After every finished turn: `nanomuse-feed` blocks become posts.
    static func afterTurn(session: String, assistantText: String?) {
        guard let text = assistantText, NanoMuseFences.contains(text, kind: block) else { return }
        let store = NanoMuseFeedStore.shared
        let today = store.posts.filter { $0.day == NanoMuseDay.key() }
        let drafts = NanoMuseFences.objects(block, in: text)
            .compactMap(NanoMusePost.Draft.init(block:))
            .filter { d in !today.contains { $0.title.lowercased() == String(d.title.prefix(80)).lowercased() } }
        guard !drafts.isEmpty else { return }
        store.append(drafts)
    }

    /// Appended to the system prompt of the feed's conversation (nil elsewhere).
    static func systemAddendum(session: String) -> String? {
        guard isFeedSession(session) else { return nil }
        let store = NanoMuseFeedStore.shared
        let recent = store.posts.prefix(24).map(\.title)
        var s = "## This conversation writes the user's feed (morde)\n"
        s += "The Feed tab shows short posts you write for the user, like Muse's feed. The user's standing instruction for it:\n"
        s += "> " + store.preferences.replacingOccurrences(of: "\n", with: "\n> ") + "\n\n"
        s += "What you know about them right now (from GLOBAL.md, the diary of the last seven days, USER.md and their goals):\n"
        let d = digest()
        s += (d.isEmpty ? "(nothing recorded yet; write a gentle first day: what the feed is for, and three things you could start tracking or preparing if they tell you a little about themselves)" : d) + "\n\n"
        s += "When a message asks you to write the feed: reply with one short line, then 3 to 6 posts, EACH as its own fenced code block tagged `nanomuse-feed` "
        s += "containing JSON {\"emoji\": \"one emoji\", \"title\": \"<= 30 characters\", \"type\": \"brief|reminder|idea|goal|memory|note\", "
        s += "\"body\": \"2 to 6 sentences or a short bullet list, Markdown allowed\", \"source\": [\"where it came from, e.g. GLOBAL.md, diary 2026-09-24, goal: <title>\"]}. "
        s += "Write in the user's language; be concrete and useful (today's follow-ups, things they said they would do, goal progress, something they would enjoy); no clickbait, no filler, nothing you already posted. "
        s += "Use tools only if a post needs a live fact (weather, a price, a date), at most two quick lookups. Say nothing after the last block.\n"
        if !recent.isEmpty { s += "Recent titles (do not repeat): \(recent.joined(separator: " · "))\n" }
        s += "In ordinary conversation here, answer normally; add a `nanomuse-feed` block only when the user asks to put something in the feed."
        return s
    }

    /// Bounded digest of the agent's memory and the person's goals.
    private static func digest() -> String {
        var sb = ""
        func add(_ label: String, _ url: URL, limit: Int) {
            guard let text = try? String(contentsOf: url, encoding: .utf8) else { return }
            let t = text.trimmingCharacters(in: .whitespacesAndNewlines)
            guard !t.isEmpty else { return }
            sb += "### \(label)\n" + (t.count > limit ? String(t.prefix(limit)) + " …" : t) + "\n"
        }
        let memory = NanoMuseDirs.memory
        add("USER.md", memory.appendingPathComponent("USER.md"), limit: 1500)
        add("GLOBAL.md", memory.appendingPathComponent("GLOBAL.md"), limit: 2500)
        for i in 0..<7 {
            guard let date = Calendar.current.date(byAdding: .day, value: -i, to: Date()) else { continue }
            let name = NanoMuseDay.key(date) + ".md"
            add("Diary \(name)", memory.appendingPathComponent(name), limit: i == 0 ? 1500 : 600)
        }
        let goals = NanoMuseGoalStore.shared.goals
        if !goals.isEmpty {
            sb += "### Goals\n"
            for g in goals {
                sb += "- \(g.title): \(g.status.rawValue), \(g.progress)%" + (g.lastNote.map { "; last: \($0)" } ?? "") + "\n"
            }
        }
        return String(sb.prefix(7000))
    }

    /// "Can we talk about this post from my feed: …" — what Discuss sends.
    static func discussOpener(_ post: NanoMusePost) -> String {
        String(format: AppLocalized("Can we talk about this post from my feed:\n\n**%@**\n\n%@"), post.title, post.body)
    }
}

// MARK: - Feed room

struct NanoMuseFeedRoom: View {
    var chrome: NanoMuseRoomChrome
    var onDiscuss: (NanoMusePost) -> Void
    var onOpenSession: (String) -> Void

    @ObservedObject private var store = NanoMuseFeedStore.shared
    @ObservedObject private var scheduler = NanoMuseScheduler.shared
    @ObservedObject private var personal = NanoMuseMordePersonalContentStore.shared
    @ObservedObject private var authorization = MordeShellAuthorization.shared
    @State private var showSettings = false
    @State private var introShown = !NanoMuseFeedFlow.introAcknowledged
    @State private var detail: NanoMusePost?

    private var groupedDays: [String] {
        var seen: [String] = []
        for p in store.posts where !seen.contains(p.day) { seen.append(p.day) }
        return seen
    }

    private var cloudPosts: [NanoMuseMordeFeedPost] {
        personal.values("feed_post").compactMap { item in
            guard let body = try? item.decodeBody(NanoMuseMordeFeedPostBody.self), body.isValid else { return nil }
            return NanoMuseMordeFeedPost(id: item.object_id, revision: item.revision, body: body)
        }.sorted { ($0.body.created_at ?? "") > ($1.body.created_at ?? "") }
    }

    var body: some View {
        VStack(spacing: 0) {
            // Android: the Feed's header has the sliders where the other rooms have •••.
            NanoMuseRoomHeader(chrome: chrome) {
                HStack(spacing: 8) {
                    if NanoMuseFeedFlow.generating {
                        ProgressView().controlSize(.small)
                    }
                    NanoMuseRoundButton(symbol: "slider.horizontal.3", label: AppLocalized("Feed settings")) { showSettings = true }
                }
            }
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 14) {
                    // C5: the day's title, the intro card until it is acknowledged, then the posts or the empty state.
                    Text(dayLabel(NanoMuseDay.key()))
                        .font(.footnote.weight(.semibold))
                        .foregroundStyle(.secondary)
                        .padding(.horizontal, 4)
                    if authorization.signedIn { cloudFeedSection }
                    if introShown {
                        introCard
                    }
                    if store.posts.isEmpty {
                        if authorization.signedIn && !cloudPosts.isEmpty {
                            Text(AppLocalized("No additional feed posts are stored on this iPhone."))
                                .font(.footnote).foregroundStyle(.secondary).padding(.horizontal, 4)
                        } else {
                            emptyState
                        }
                    } else {
                        Text(AppLocalized("On this iPhone")).font(.footnote.weight(.semibold)).foregroundStyle(.secondary).padding(.horizontal, 4)
                        ForEach(groupedDays, id: \.self) { day in
                            if day != NanoMuseDay.key() {
                                Text(dayLabel(day))
                                    .font(.footnote.weight(.semibold))
                                    .foregroundStyle(.secondary)
                                    .padding(.horizontal, 4)
                            }
                            ForEach(store.posts.filter { $0.day == day }) { post in
                                NanoMusePostCard(post: post, onDiscuss: { onDiscuss(post) }, onOpen: { detail = post })
                            }
                        }
                    }
                }
                .padding(16)
            }
            .refreshable { store.reload(); await personal.refresh(root: NanoMuseDirs.root) }
        }
        .background(NanoMuseTones.canvas.ignoresSafeArea())
        .onAppear {
            store.reload()
            Task { @MainActor in await NanoMuseFeedFlow.ensureRoutine() }
        }
        .task { await personal.refresh(root: NanoMuseDirs.root) }
        .nmOnChange(of: authorization.personalDataScopeKey) { _ in Task { await personal.refresh(root: NanoMuseDirs.root) } }
        .sheet(isPresented: $showSettings) { NanoMuseFeedSettingsSheet(onOpenSession: onOpenSession) }
        .sheet(item: $detail) { post in
            NanoMusePostDetail(post: post, onDiscuss: { detail = nil; onDiscuss(post) })
        }
    }

    private var cloudFeedSection: some View {
        let posts = cloudPosts
        return VStack(alignment: .leading, spacing: 10) {
            HStack {
                Text(AppLocalized("Cloud feed")).font(.footnote.weight(.semibold)).foregroundStyle(.secondary)
                Spacer()
                if personal.isLoading { ProgressView().controlSize(.small) }
                Button { Task { await personal.refresh(root: NanoMuseDirs.root) } } label: { Image(systemName: "arrow.clockwise") }
                    .buttonStyle(.plain).accessibilityLabel(Text(AppLocalized("Refresh")))
            }.padding(.horizontal, 4)
            if let error = personal.error {
                Label(String(format: AppLocalized("Cloud feed unavailable: %@"), error), systemImage: "exclamationmark.triangle")
                    .font(.footnote).foregroundStyle(.red)
                Button(AppLocalized("Retry cloud feed")) { Task { await personal.retry(root: NanoMuseDirs.root) } }.font(.footnote)
            } else if posts.isEmpty {
                Text(AppLocalized("No cloud feed posts yet.")).font(.subheadline).foregroundStyle(.secondary)
                    .padding(14).frame(maxWidth: .infinity, alignment: .leading)
                    .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 14))
            } else {
                ForEach(posts) { post in
                    VStack(alignment: .leading, spacing: 8) {
                        HStack {
                            Text(post.body.emoji ?? "📝")
                            Text(post.body.title).font(.subheadline.weight(.semibold))
                            Spacer()
                            Menu {
                                Button(AppLocalized(post.body.liked == true ? "Unlike" : "Like")) {
                                    var body = post.body
                                    body.liked = body.liked != true
                                    Task { try? await personal.replace("feed_post", id: post.id, revision: post.revision, body: body, root: NanoMuseDirs.root) }
                                }
                                Button(AppLocalized("Delete"), role: .destructive) {
                                    Task { try? await personal.delete("feed_post", id: post.id, revision: post.revision, root: NanoMuseDirs.root) }
                                }
                            } label: { Image(systemName: post.body.liked == true ? "heart.fill" : "ellipsis") }
                                .accessibilityLabel(Text(AppLocalized("More")))
                                .accessibilityIdentifier("morde.feed.post.menu.\(post.id)")
                        }
                        Text(post.body.body).font(.subheadline).foregroundStyle(.secondary)
                    }
                    .padding(14)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 14))
                }
            }
            if let pending = personal.pending, pending.objectType == "feed_post" || pending.objectType == "feed_preferences" {
                Label(AppLocalized(personal.syncEnabled ? "A Feed change is waiting to sync." : "Sync is paused; the Feed change stays on this device."), systemImage: "clock")
                    .font(.footnote).foregroundStyle(.secondary)
            }
        }
    }

    /// Android: the "About the feed" card — what drives it, the sentence itself, Edit / Got it.
    private var introCard: some View {
        VStack(alignment: .leading, spacing: 0) {
            VStack(alignment: .leading, spacing: 4) {
                Text(AppLocalized("About the feed")).font(.headline)
                Text(AppLocalized("Short posts your agent writes for you from what it remembers: your memory files, the last week of diary, your goals. The sentence below steers every post from now on; edit it any time."))
                    .font(.footnote).foregroundStyle(.secondary)
            }
            .padding(.horizontal, 16)
            .padding(.vertical, 14)
            NanoMuseTones.hairline.frame(height: 1)
            VStack(alignment: .leading, spacing: 14) {
                Text(store.preferences).font(.subheadline)
                HStack(spacing: 10) {
                    Spacer(minLength: 0)
                    Button(AppLocalized("Edit")) {
                        acknowledgeIntro()
                        showSettings = true
                    }
                    .font(.subheadline.weight(.medium))
                    .foregroundStyle(.primary)
                    .padding(.horizontal, 22).padding(.vertical, 10)
                    .background(NanoMuseTones.fill, in: Capsule())
                    .buttonStyle(.plain)
                    Button(AppLocalized("Got it")) { acknowledgeIntro() }
                        .font(.subheadline.weight(.semibold))
                        .foregroundStyle(.white)
                        .padding(.horizontal, 22).padding(.vertical, 10)
                        .background(NanoMuseTones.action, in: Capsule())
                        .buttonStyle(.plain)
                }
            }
            .padding(.horizontal, 16)
            .padding(.vertical, 14)
        }
        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 16, style: .continuous))
    }

    private func acknowledgeIntro() {
        NanoMuseFeedFlow.introAcknowledged = true
        withAnimation(.easeInOut(duration: 0.2)) { introShown = false }
    }

    /// Android: two plain cards, then "Write it now" (or the reason there is nothing to write with).
    @ViewBuilder
    private var emptyState: some View {
        let time = NanoMuseDay.clock(hour: NanoMuseFeedFlow.routine?.hour ?? NanoMuseFeedFlow.defaultHour, minute: NanoMuseFeedFlow.routine?.minute ?? NanoMuseFeedFlow.defaultMinute)
        staticCard("🖼️", AppLocalized("Nothing in the feed yet"),
                   String(format: AppLocalized("As we get to know each other, new posts will show up here. Every day at %@ I read what I remember about you, your memory files, the last week of diary and your goals, and write a few short posts."), time))
        staticCard("📝", AppLocalized("Steer it with one sentence"),
                   AppLocalized("Tap the sliders at the top right to tell me what you want more of, switch the daily routine off, or have me write the first day now."))
        VStack(spacing: 8) {
            if NanoMuseFeedFlow.canRun {
                if NanoMuseFeedFlow.generating {
                    HStack(spacing: 10) {
                        ProgressView().controlSize(.small)
                        Text(AppLocalized("Writing…")).font(.subheadline)
                    }
                } else {
                    Button(AppLocalized("Write it now")) { NanoMuseFeedFlow.generateNow() }
                        .font(.subheadline.weight(.semibold))
                        .foregroundStyle(.white)
                        .padding(.horizontal, 26).padding(.vertical, 11)
                        .background(NanoMuseTones.action, in: Capsule())
                        .buttonStyle(.plain)
                }
            } else if MordeLocalExecutionPolicy.isEnabled {
                Text(AppLocalized("Add a model first; the feed is written by your agent."))
                    .font(.footnote).foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
            } else {
                Text(MordeLocalExecutionPolicy.disabledMessage)
                    .font(.footnote).foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
            }
        }
        .frame(maxWidth: .infinity)
        .padding(16)
        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 16, style: .continuous))
    }

    private func staticCard(_ emoji: String, _ title: String, _ body: String) -> some View {
        HStack(alignment: .top, spacing: 12) {
            Text(emoji).font(.title2)
            VStack(alignment: .leading, spacing: 6) {
                Text(title).font(.body.weight(.semibold))
                Text(body).font(.subheadline).foregroundStyle(.secondary)
            }
        }
        .padding(16)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 16, style: .continuous))
    }

    private func dayLabel(_ day: String) -> String {
        guard let date = NanoMuseDay.date(day) else { return day }
        if Calendar.current.isDateInToday(date) { return AppLocalized("Today") }
        if Calendar.current.isDateInYesterday(date) { return AppLocalized("Yesterday") }
        return date.formatted(date: .abbreviated, time: .omitted)
    }
}

struct NanoMusePostCard: View {
    var post: NanoMusePost
    var onDiscuss: () -> Void
    var onOpen: () -> Void
    @ObservedObject private var store = NanoMuseFeedStore.shared

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                Text(post.emoji.isEmpty ? NanoMusePost.defaultEmoji(post.type) : post.emoji)
                Text(post.title).font(.body.weight(.semibold)).lineLimit(2)
                Spacer(minLength: 0)
                Text(NanoMusePost.typeLabel(post.type))
                    .font(.caption2.weight(.medium))
                    .padding(.horizontal, 7).padding(.vertical, 3)
                    .background(NanoMuseTones.fill, in: Capsule())
                    .foregroundStyle(.secondary)
            }
            NanoMuseInlineMarkdown(text: post.body)
                .font(.subheadline)
                .lineLimit(6)
            HStack(spacing: 14) {
                Text(post.createdAt.formatted(date: .omitted, time: .shortened)).font(.caption).foregroundStyle(.tertiary)
                Spacer()
                Button { store.setLiked(post, !post.liked) } label: {
                    Image(systemName: post.liked ? "heart.fill" : "heart")
                        .foregroundStyle(post.liked ? Color.red : Color.secondary)
                }
                .buttonStyle(.plain)
                .accessibilityLabel(Text(post.liked ? AppLocalized("Unlike") : AppLocalized("Like")))
                Button(action: onDiscuss) {
                    Label(AppLocalized("Discuss"), systemImage: "bubble.left").labelStyle(.titleAndIcon)
                }
                .font(.footnote.weight(.medium))
                .tint(NanoMuseTones.action)
                Menu {
                    Button(action: onOpen) { Label(AppLocalized("Details"), systemImage: "info.circle") }
                    Button(role: .destructive) { store.delete(post) } label: { Label(AppLocalized("Remove this post"), systemImage: "trash") }
                } label: {
                    Image(systemName: "ellipsis").foregroundStyle(.secondary).frame(width: 24, height: 24).contentShape(Rectangle())
                }
                .accessibilityLabel(Text(AppLocalized("More")))
            }
        }
        .padding(14)
        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 16, style: .continuous))
        .contentShape(Rectangle())
        .onTapGesture(perform: onOpen)
    }
}

/// Bold, italics and links in a short body; the Markdown of a post is small on purpose.
struct NanoMuseInlineMarkdown: View {
    var text: String
    var body: some View {
        if let attributed = try? AttributedString(markdown: text, options: AttributedString.MarkdownParsingOptions(interpretedSyntax: .inlineOnlyPreservingWhitespace)) {
            Text(attributed)
        } else {
            Text(text)
        }
    }
}

struct NanoMusePostDetail: View {
    var post: NanoMusePost
    var onDiscuss: () -> Void
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: 14) {
                    HStack(spacing: 8) {
                        Text(post.emoji.isEmpty ? NanoMusePost.defaultEmoji(post.type) : post.emoji).font(.title2)
                        Text(post.title).font(.title3.weight(.bold))
                    }
                    NanoMuseInlineMarkdown(text: post.body).font(.body)
                    Divider()
                    // A grid sizes the label column to the longest label ("Geschrieben", "Написано") instead of a fixed 72 pt.
                    Grid(alignment: .topLeading, horizontalSpacing: 12, verticalSpacing: 8) {
                        row(AppLocalized("Type"), NanoMusePost.typeLabel(post.type))
                        row(AppLocalized("Written"), post.createdAt.formatted(date: .abbreviated, time: .shortened))
                        if !post.source.isEmpty { row(AppLocalized("From"), post.source.joined(separator: " · ")) }
                        row(AppLocalized("File"), "nanomuse/feed/\(post.id).md")
                    }
                    .font(.footnote)
                    Button(action: onDiscuss) {
                        Label(AppLocalized("Discuss"), systemImage: "bubble.left").frame(maxWidth: .infinity).padding(.vertical, 8)
                    }
                    .buttonStyle(.borderedProminent)
                    .tint(NanoMuseTones.action)
                }
                .padding(20)
            }
            .navigationBarTitleDisplayMode(.inline)
            .toolbar { ToolbarItem(placement: .confirmationAction) { Button(AppLocalized("Done")) { dismiss() } } }
        }
    }

    private func row(_ label: String, _ value: String) -> some View {
        GridRow {
            Text(label).foregroundStyle(.secondary)
            Text(value).textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading)
        }
    }
}

/// Preferences, the daily routine and its time, "Write it now".
struct NanoMuseFeedSettingsSheet: View {
    var onOpenSession: (String) -> Void
    @ObservedObject private var store = NanoMuseFeedStore.shared
    @ObservedObject private var scheduler = NanoMuseScheduler.shared
    @ObservedObject private var personal = NanoMuseMordePersonalContentStore.shared
    @ObservedObject private var authorization = MordeShellAuthorization.shared
    @Environment(\.dismiss) private var dismiss
    @State private var text = ""
    @State private var cloudText = ""
    @State private var cloudSaving = false
    @State private var time = Date()

    private var routine: NanoMuseRoutine? { NanoMuseFeedFlow.routine }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextEditor(text: $text).frame(minHeight: 90)
                } header: {
                    Text(AppLocalized("What the feed is about"))
                } footer: {
                    Text(AppLocalized("One or two sentences. Every post from now on follows it."))
                }
                if authorization.signedIn {
                    Section {
                        TextEditor(text: $cloudText).frame(minHeight: 90)
                        Button(AppLocalized("Save cloud feed preference")) { saveCloudPreference() }
                            .disabled(cloudSaving || cloudText.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                        if let error = personal.error {
                            Label(error, systemImage: "exclamationmark.triangle").font(.footnote).foregroundStyle(.red)
                        }
                        if let pending = personal.pending, pending.objectType == "feed_preferences" {
                            Label(AppLocalized(personal.syncEnabled ? "Cloud feed preference is waiting to sync." : "Sync is paused; the preference remains on this device."), systemImage: "clock")
                                .font(.footnote).foregroundStyle(.secondary)
                        }
                    } header: { Text(AppLocalized("Cloud feed preference")) } footer: {
                        Text(AppLocalized("This preference belongs to your Morde account. Local feed preferences stay on this iPhone."))
                    }
                }
                Section {
                    if let r = routine {
                        Toggle(AppLocalized("Write it every day"), isOn: Binding(get: { r.enabled }, set: { NanoMuseFeedFlow.setEnabled($0) }))
                            .tint(NanoMuseTones.action)
                            .disabled(!MordeLocalExecutionPolicy.isEnabled)
                        DatePicker(AppLocalized("Time"), selection: $time, displayedComponents: .hourAndMinute)
                            .nmOnChange(of: time) { t in
                                let c = Calendar.current.dateComponents([.hour, .minute], from: t)
                                NanoMuseFeedFlow.setTime(hour: c.hour ?? NanoMuseFeedFlow.defaultHour, minute: c.minute ?? 0)
                            }
                            .disabled(!MordeLocalExecutionPolicy.isEnabled)
                        if let last = r.runs.first {
                            Text(String(format: last.ok ? AppLocalized("last run %@") : AppLocalized("last run %@, failed"), NanoMuseDay.relative(last.at)))
                                .font(.footnote).foregroundStyle(.secondary)
                        }
                    } else {
                        Text(MordeLocalExecutionPolicy.isEnabled
                             ? AppLocalized("Add a model first; the feed is written by your agent.")
                             : MordeLocalExecutionPolicy.disabledMessage)
                            .font(.footnote).foregroundStyle(.secondary)
                    }
                    Button {
                        NanoMuseFeedFlow.generateNow()
                    } label: {
                        if NanoMuseFeedFlow.generating {
                            HStack(spacing: 8) { ProgressView().controlSize(.small); Text(AppLocalized("Writing…")) }
                        } else {
                            Text(AppLocalized("Write it now"))
                        }
                    }
                    .disabled(!NanoMuseFeedFlow.canRun || NanoMuseFeedFlow.generating)
                    if let sid = NanoMuseFeedFlow.sessionId {
                        Button(AppLocalized("Open the feed's conversation")) {
                            dismiss()
                            onOpenSession(sid)
                        }
                    }
                } header: {
                    Text(AppLocalized("Routine"))
                } footer: {
                    Text(AppLocalized("On the iPhone the feed is written when the app is open; at the set time the phone reminds you to open it. Nothing runs while the app is asleep."))
                }
            }
            .navigationTitle(AppLocalized("Feed"))
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button(AppLocalized("Cancel")) { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button(AppLocalized("Save")) {
                        store.savePreferences(text)
                        dismiss()
                    }
                }
            }
            .onAppear {
                text = store.preferences
                cloudText = store.preferences
                Task {
                    await personal.refresh(root: NanoMuseDirs.root)
                    if let item = personal.object("feed_preferences", "default"),
                       let body = try? item.decodeBody(NanoMuseMordeFeedPreferences.self), body.isValid {
                        cloudText = body.content
                    }
                }
                var c = DateComponents()
                c.hour = routine?.hour ?? NanoMuseFeedFlow.defaultHour
                c.minute = routine?.minute ?? NanoMuseFeedFlow.defaultMinute
                time = Calendar.current.date(from: c) ?? Date()
            }
            .nmOnChange(of: authorization.personalDataScopeKey) { _ in
                cloudText = store.preferences
                Task {
                    await personal.refresh(root: NanoMuseDirs.root)
                    if let item = personal.object("feed_preferences", "default"),
                       let body = try? item.decodeBody(NanoMuseMordeFeedPreferences.self), body.isValid {
                        cloudText = body.content
                    }
                }
            }
        }
    }

    private func saveCloudPreference() {
        guard let item = personal.object("feed_preferences", "default") else {
            Task { await saveCloudPreference(revision: 0) }
            return
        }
        Task { await saveCloudPreference(revision: item.revision) }
    }

    private func saveCloudPreference(revision: Int64) async {
        cloudSaving = true
        defer { cloudSaving = false }
        do {
            _ = try await personal.replace("feed_preferences", id: "default", revision: revision,
                body: NanoMuseMordeFeedPreferences(content: cloudText.trimmingCharacters(in: .whitespacesAndNewlines)),
                root: NanoMuseDirs.root)
        } catch { }
    }
}

/// A persisted `nanomuse-feed` fence in the chat: "Added to your feed".
struct NanoMuseFeedFenceCard: View {
    var object: [String: Any]
    var body: some View {
        let title = object["title"] as? String ?? ""
        let emoji = (object["emoji"] as? String).flatMap { $0.isEmpty ? nil : $0 } ?? NanoMusePost.defaultEmoji(object["type"] as? String ?? "note")
        HStack(spacing: 10) {
            Text(emoji)
            VStack(alignment: .leading, spacing: 2) {
                Text(AppLocalized("Added to your feed")).font(.caption).foregroundStyle(.secondary)
                Text(title).font(.subheadline.weight(.semibold)).lineLimit(2)
            }
            Spacer(minLength: 0)
        }
        .padding(12)
        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 14, style: .continuous))
    }
}

extension Notification.Name {
    /// Switch the Muse shell to a room: object is "feed" | "goals" | "ideas" | "library".
    static let nanoMuseOpenRoom = Notification.Name("nanoMuse.openRoom")
}
