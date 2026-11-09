//
//  NanoMuseFirstRun.swift
//  nanoMuse
//
//  The Morde sign-in screen shown on a new install, plus setup helpers that
//  remain for existing nanoMuse account data.
//

import Combine
import Foundation
import SwiftUI
import UserNotifications

// MARK: - When the setup shows

enum NanoMuseFirstRun {
    private static let doneKey = "nanomuse.setup.done"
    private static let sourceKey = "nanomuse.setup.source_chosen"
    static let notificationsKey = "nanomuse.setup.notifications_seen"

    static var isDone: Bool { UserDefaults.standard.bool(forKey: doneKey) }
    static func markDone() { UserDefaults.standard.set(true, forKey: doneKey) }
    static var sourceChosen: Bool { UserDefaults.standard.bool(forKey: sourceKey) }
    static func markSourceChosen() { UserDefaults.standard.set(true, forKey: sourceKey) }
    static var notificationsSeen: Bool { UserDefaults.standard.bool(forKey: notificationsKey) }
    static func markNotificationsSeen() { UserDefaults.standard.set(true, forKey: notificationsKey) }

    /// Keep a new install at the Morde sign-in until its service session is verified.
    static func needed(signedIn: Bool, hasProviders: Bool, hasSessions: Bool, done: Bool,
                       mordeSignedIn: Bool = false) -> Bool {
        MordeFirstRunPolicy.isNeeded(
            signedIn: signedIn,
            hasProviders: hasProviders,
            hasSessions: hasSessions,
            done: done,
            mordeSignedIn: mordeSignedIn
        )
    }

    enum Stage: Int { case welcome, password, source, models, notifications, meet }

    /// The page to show, from what the app has. Android has a Hands page where the iPhone
    /// has Notifications: routines and goal check-ins need the permission.
    /// Signed out with a provider of one's own (`hasProviders`), the account pages are skipped:
    /// the password is the account's and the source question is answered by the key.
    static func stage(signedIn: Bool, hasProviders: Bool = false, hasGroups: Bool, sourceChosen: Bool, modelsSkipped: Bool, fresh: Bool, passwordAnswered: Bool, notificationsSeen: Bool = true) -> Stage {
        if !signedIn && !hasProviders { return .welcome }
        if signedIn && fresh && !passwordAnswered { return .password }
        if signedIn && !sourceChosen { return .source }
        if !hasGroups && !modelsSkipped { return .models }
        if !notificationsSeen { return .notifications }
        return .meet
    }

    /// The dot that lights: account · phone · meet (the model pages fold into the first), as Android.
    static func dot(_ stage: Stage) -> Int {
        switch stage {
        case .welcome, .password, .source, .models: return 0
        case .notifications: return 1
        case .meet: return 2
        }
    }
}

// MARK: - The setup screen

struct NanoMuseFirstRunView: View {
    var onSettings: () -> Void

    @ObservedObject private var mordeAuthorization = MordeShellAuthorization.shared
    @State private var showMordeLogin = false

    var body: some View {
        VStack(spacing: 0) {
            HStack {
                Spacer()
                Button(action: onSettings) {
                    Image(systemName: "gearshape")
                        .font(.system(size: 18, weight: .medium))
                        .foregroundStyle(.primary)
                        .frame(width: 44, height: 44)
                }
                .accessibilityLabel(AppLocalized("Settings"))
            }
            .padding(.trailing, 8)
            .frame(height: 56)
            NanoMuseSetupPage(
                hero: {
                    Image("MordeLaunchMark").resizable().scaledToFit()
                        .frame(width: 52, height: 52)
                        .frame(width: 104, height: 104)
                        .background(NanoMuseTones.fill, in: Circle())
                }, // nanoMuse: Reuse the new launch mark without replacing the legacy asset.
                title: AppLocalized("Welcome to morde"), // nanoMuse: The original Minis target is branded Morde.
                subtitle: "Sign in to your Morde service to use cloud conversations and tasks.",
                primaryLabel: AppLocalized("Morde cloud tasks"),
                onPrimary: { showMordeLogin = true },
                finePrint: "This app does not make direct model calls from the phone."
            ) {
                NanoMuseFeatureRow(symbol: "bubble.left.and.bubble.right", title: "Cloud conversations", subtitle: "Continue conversations through your Morde account.")
                NanoMuseFeatureRow(symbol: "desktopcomputer", title: "Computers and workspaces", subtitle: "Use resources the signed-in Morde account is allowed to access.")
            }
        }
        .background(NanoMuseTones.surface.ignoresSafeArea())
        .sheet(isPresented: $showMordeLogin) {
            NavigationStack {
                MordeEmbeddedView()
                    .navigationTitle(AppLocalized("Morde cloud tasks"))
                    .toolbar {
                        ToolbarItem(placement: .cancellationAction) {
                            Button(AppLocalized("Close")) { showMordeLogin = false }
                        }
                    }
            }
        .nmOnChange(of: mordeAuthorization.signedIn) { signedIn in
            if signedIn { showMordeLogin = false }
        }
        }
    }
}

/// Right after a sign-in that created the account: a password, so the next device signs in
/// without waiting for a code. Skippable; Account has the same form later.
struct NanoMusePasswordPage: View {
    var onDone: () -> Void
    @State private var next = ""
    @State private var again = ""
    @State private var busy = false
    @State private var error: String?

    var body: some View {
        NanoMuseSetupPage(
            hero: { NanoMuseHeroGlyph(symbol: "key") },
            title: AppLocalized("Set a password"),
            subtitle: AppLocalized("Your account is in. With a password, your computers and your other phones sign in at once, without waiting for a code."),
            primaryLabel: AppLocalized("Set the password"),
            onPrimary: { Task { await save() } },
            secondaryLabel: AppLocalized("Skip for now"),
            onSecondary: onDone,
            finePrint: AppLocalized("Optional. You can set or change it later under Account."),
            busy: busy
        ) {
            VStack(spacing: 10) {
                SecureField(AppLocalized("New password (8 characters or more)"), text: $next)
                    .textContentType(.newPassword)
                    .textFieldStyle(.roundedBorder)
                SecureField(AppLocalized("Repeat the new password"), text: $again)
                    .textContentType(.newPassword)
                    .textFieldStyle(.roundedBorder)
                if let error {
                    Text(error).font(.footnote).foregroundStyle(.red).multilineTextAlignment(.center)
                }
            }
        }
    }

    private func save() async {
        guard !busy else { return }
        if next.count < 8 { error = AppLocalized("The password needs at least 8 characters."); return }
        if next != again { error = AppLocalized("The two passwords differ."); return }
        error = nil
        busy = true
        defer { busy = false }
        do {
            try await NanoMuseCloud.setPassword(next, current: nil)
            onDone()
        } catch {
            self.error = NanoMuseCloud.describe(error)
        }
    }
}

/// The phone page (Android has Hands here): notifications, so a routine coming due or a goal
/// check-in can call the person back to the app. Continue asks iOS; Skip leaves it for later.
struct NanoMuseNotificationsPage: View {
    var onDone: () -> Void
    @State private var status: UNAuthorizationStatus = .notDetermined
    @State private var busy = false

    private var granted: Bool { status == .authorized || status == .provisional || status == .ephemeral }

    var body: some View {
        NanoMuseSetupPage(
            hero: { NanoMuseHeroGlyph(symbol: "bubble.left.and.bubble.right.fill") }, // nanoMuse: Keep the notification step's semantic glyph separate from the brand mark.
            title: AppLocalized("Notifications"),
            subtitle: AppLocalized("Routines and goal check-ins run at a set time. On the iPhone the agent cannot wake itself, so a reminder brings you back when one is due."),
            primaryLabel: granted ? AppLocalized("Continue") : AppLocalized("Allow notifications"),
            onPrimary: { if granted { onDone() } else { ask() } },
            secondaryLabel: granted ? nil : AppLocalized("Skip for now"),
            onSecondary: onDone,
            finePrint: AppLocalized("Everything else works without it. It lives under iOS Settings → morde whenever you want it."),
            busy: busy
        ) {
            NanoMuseFeatureRow(symbol: "clock", title: AppLocalized("Routines"), subtitle: AppLocalized("A message the agent sends itself at a set time: the feed every morning, anything you schedule."))
            NanoMuseFeatureRow(symbol: "target", title: AppLocalized("Goal check-ins"), subtitle: AppLocalized("It asks how a goal is going, when you said it should."))
        }
        .task { await refresh() }
    }

    private func refresh() async {
        status = await UNUserNotificationCenter.current().notificationSettings().authorizationStatus
    }

    private func ask() {
        guard !busy else { return }
        busy = true
        Task { @MainActor in
            defer { busy = false }
            if status == .notDetermined {
                _ = try? await UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound, .badge])
            } else if let url = URL(string: UIApplication.openSettingsURLString) {
                _ = await UIApplication.shared.open(url)
            }
            await refresh()
            if granted { onDone() }
        }
    }
}

/// Hero, title, one line under it, the page's own rows, then the pill(s) and the fine print.
struct NanoMuseSetupPage<Hero: View, Content: View>: View {
    @ViewBuilder var hero: () -> Hero
    var title: String
    var subtitle: String
    var primaryLabel: String
    var onPrimary: () -> Void
    var secondaryLabel: String? = nil
    var onSecondary: (() -> Void)? = nil
    var finePrint: String
    var learnMore: URL? = nil
    var busy: Bool = false
    @ViewBuilder var content: () -> Content
    /// The title follows Dynamic Type, capped as the header's lines are (NanoMuseHeaderMetrics); the page scrolls, so it may wrap.
    @ScaledMetric(relativeTo: .title) private var titleSize: CGFloat = 26

    /// The fine print as one paragraph, *Learn more* a link at its end.
    private var finePrintText: Text {
        guard let learnMore else { return Text(verbatim: finePrint) }
        let label = AppLocalized("Learn more")
        let link = (try? AttributedString(markdown: "[\(label)](\(learnMore.absoluteString))")) ?? AttributedString(label)
        return Text(verbatim: finePrint + " ") + Text(link)
    }

    var body: some View {
        VStack(spacing: 0) {
            ScrollView {
                VStack(spacing: 14) {
                    hero()
                        .padding(.top, 12)
                    Text(title)
                        .font(.system(size: NanoMuseHeaderMetrics.capped(titleSize, base: 26), weight: .semibold, design: .rounded))
                        .multilineTextAlignment(.center)
                    Text(subtitle)
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                        .multilineTextAlignment(.center)
                        .padding(.horizontal, 12)
                    VStack(alignment: .leading, spacing: 10) {
                        content()
                    }
                    .padding(.top, 10)
                }
                .padding(.horizontal, 24)
                .frame(maxWidth: 520)
                .frame(maxWidth: .infinity)
            }
            VStack(spacing: 10) {
                Button(action: onPrimary) {
                    HStack {
                        if busy { ProgressView().tint(.white) }
                        Text(primaryLabel).font(.headline)
                    }
                    .frame(maxWidth: .infinity)
                    .padding(.vertical, 6)
                }
                .buttonStyle(.borderedProminent)
                .tint(NanoMuseTones.action)
                .controlSize(.large)
                .disabled(busy)
                if let secondaryLabel, let onSecondary {
                    Button(secondaryLabel, action: onSecondary)
                        .font(.subheadline.weight(.medium))
                        .disabled(busy)
                }
                // One paragraph with the link at its end, so a long fine print wraps as text
                // instead of sitting beside a "Learn more" column.
                finePrintText
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .tint(NanoMuseTones.action)
                    .multilineTextAlignment(.center)
            }
            .padding(.horizontal, 24)
            .padding(.top, 8)
            .padding(.bottom, 12)
            .frame(maxWidth: 520)
        }
    }
}

struct NanoMuseFeatureRow: View {
    var symbol: String
    var title: String
    var subtitle: String

    var body: some View {
        HStack(alignment: .top, spacing: 12) {
            Image(systemName: symbol)
                .font(.system(size: 18, weight: .medium))
                .foregroundStyle(NanoMuseTones.action)
                .frame(width: 32, height: 32)
                .background(NanoMuseTones.fill, in: RoundedRectangle(cornerRadius: 9, style: .continuous))
            VStack(alignment: .leading, spacing: 2) {
                Text(title).font(.subheadline.weight(.semibold))
                Text(subtitle).font(.footnote).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
            Spacer(minLength: 0)
        }
    }
}

struct NanoMuseNoticeCard: View {
    var title: String
    var body_: String
    init(title: String, body: String) { self.title = title; self.body_ = body }

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(title).font(.footnote.weight(.semibold))
            Text(body_).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(NanoMuseTones.fill, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
    }
}

struct NanoMuseHeroGlyph: View {
    var symbol: String
    var body: some View {
        Image(systemName: symbol)
            .font(.system(size: 40, weight: .medium))
            .foregroundStyle(NanoMuseTones.action)
            .frame(width: 104, height: 104)
            .background(NanoMuseTones.fill, in: Circle())
    }
}

struct NanoMuseDots: View {
    var current: Int
    var total: Int
    var body: some View {
        HStack(spacing: 6) {
            ForEach(0..<total, id: \.self) { i in
                Capsule()
                    .fill(i == current ? NanoMuseTones.action : NanoMuseTones.hairline)
                    .frame(width: i == current ? 18 : 6, height: 6)
            }
        }
    }
}

// MARK: - The first conversation

/// What the model told the app in a `nanomuse-naming` block.
struct NanoMuseNamingBlock: Equatable {
    /// The block had a `user_address` key (nil value = the user wants no form of address).
    var addressGiven: Bool
    var userAddress: String?
    /// Names the model suggests for itself, already cleaned.
    var suggestions: [String]
    /// The name the user gave the agent, if this block says so.
    var agentName: String?
}

// nanoMuse: Keep historical naming metadata for account counters and session sync; Morde chats do not run local chat onboarding.
@MainActor
final class NanoMuseFirstConversation: ObservableObject {
    static let shared = NanoMuseFirstConversation()

    nonisolated static let block = "naming"
    nonisolated static let maxName = 16
    nonisolated static let namePoolEN = ["Pip", "Wren", "Juno", "Remy", "Tilly", "Milo", "Sol", "Fig"]
    nonisolated static let namePoolZH = ["豆丁", "小满", "团团", "叮叮", "小北", "一一", "小竹", "阿岳"]

    enum Phase: String {
        /// Never started; eligible on the next fresh draft if the name is still the default.
        case none
        /// The opening is on screen; the user is answering "what should I call you?".
        case askUserName
        /// The model asked for its name; the chooser card is showing.
        case askAgentName
        /// The name was just picked from the chooser; the model's next reply is its first as itself.
        case named
        /// Over — either completed or the user talked past it.
        case done
    }

    private enum Keys {
        static let phase = "nanomuse.first_conversation.phase"
        static let session = "nanomuse.first_conversation.session"
        static let address = "nanomuse.first_conversation.address"
        static let suggestions = "nanomuse.first_conversation.suggestions"
    }

    var phase: Phase {
        get { UserDefaults.standard.string(forKey: Keys.phase).flatMap(Phase.init(rawValue:)) ?? .none }
        set { UserDefaults.standard.set(newValue.rawValue, forKey: Keys.phase); objectWillChange.send() }
    }

    /// The session (draft key or real id) the conversation is bound to.
    private(set) var sessionKey: String? {
        get { UserDefaults.standard.string(forKey: Keys.session) }
        set { UserDefaults.standard.set(newValue, forKey: Keys.session) }
    }

    /// A draft's key starts with `__new__` (ContentView.makeNewSessionId) or is the bare "draft"; a real session's is its id.
    nonisolated static func isRealSession(_ key: String) -> Bool {
        !(key.hasPrefix("__new__") || key == "draft")
    }

    func isBound(to key: String) -> Bool { sessionKey == key && phase != .none }

    /// The three opening paragraphs, in the app's language.
    func intro() -> [String] {
        [
            AppLocalized("Hi, I'm morde, the assistant that lives on your phone. Let me take a few things off your plate."),
            AppLocalized("A bit about how I work:\n\n- I have my own computer, a Linux sandbox and a browser, so I can run commands, open websites and fill in forms.\n- I can read and organise the files and photos you share with me, and take care of reminders and scheduled tasks.\n- Before any step that matters, I ask you first.\n- Everything runs on this phone. Your messages go to the model you configured; signed in to morde Cloud, this conversation also follows you to your other devices. Data controls switches that off."),
            AppLocalized("Before we start, what should I call you?"),
        ]
    }

    /// Forget the whole thing (the SOUL name stays): used when the person resets the setup.
    func reset() {
        for key in [Keys.phase, Keys.session, Keys.address, Keys.suggestions] {
            UserDefaults.standard.removeObject(forKey: key)
        }
        objectWillChange.send()
    }

    /// "- Call them: X" under "## About the user" in GLOBAL.md; an existing line is replaced.
    nonisolated static func withAddress(_ address: String, in current: String) -> String {
        let line = "- Call them: \(address)"
        let lines = current.components(separatedBy: "\n")
        if lines.contains(where: { $0.trimmingCharacters(in: .whitespaces).hasPrefix("- Call them:") }) {
            return lines.map { $0.trimmingCharacters(in: .whitespaces).hasPrefix("- Call them:") ? line : $0 }.joined(separator: "\n")
        }
        if current.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
            return "## About the user\n\(line)\n"
        }
        var trimmed = current
        while trimmed.hasSuffix("\n") || trimmed.hasSuffix(" ") { trimmed.removeLast() }
        return trimmed + "\n\n## About the user\n\(line)\n"
    }

    /// Two names from the pool, stable for a given seed.
    nonisolated static func poolPick(seed: String, chinese: Bool) -> [String] {
        let pool = chinese ? namePoolZH : namePoolEN
        var h: UInt64 = 1469598103934665603
        for b in seed.utf8 { h = (h ^ UInt64(b)) &* 1099511628211 }
        let first = Int(h % UInt64(pool.count))
        var second = Int((h >> 17) % UInt64(pool.count))
        if second == first { second = (second + 1) % pool.count }
        return [pool[first], pool[second]]
    }

    /// The last `nanomuse-naming` block in `text`, or nil when there is none or it is not JSON.
    nonisolated static func parseBlock(_ text: String?) -> NanoMuseNamingBlock? {
        guard let text, let o = NanoMuseFences.lastObject(block, in: text) else { return nil }
        let suggestions = ((o["suggest"] as? [Any]) ?? []).compactMap { cleanName($0 as? String) }
        var seen: [String] = []
        for s in suggestions where !seen.contains(s) { seen.append(s) }
        let addressGiven = o.keys.contains("user_address")
        let address = o["user_address"] is NSNull ? nil : cleanName(o["user_address"] as? String)
        let agent = o["agent_name"] is NSNull ? nil : cleanName(o["agent_name"] as? String)
        return NanoMuseNamingBlock(addressGiven: addressGiven, userAddress: address, suggestions: Array(seen.prefix(3)), agentName: agent)
    }

    /// A usable name: one line, quotes and trailing punctuation gone, not absurdly long.
    nonisolated static func cleanName(_ raw: String?) -> String? {
        guard let raw else { return nil }
        let ns = raw as NSString
        let t = nmNameQuoteEdges.stringByReplacingMatches(in: raw, range: NSRange(location: 0, length: ns.length), withTemplate: "").trimmingCharacters(in: .whitespaces)
        if t.isEmpty || t.contains("\n") || t.count > maxName { return nil }
        return t
    }
}

/// Quotes and trailing punctuation around a name.
private let nmNameQuoteEdges = try! NSRegularExpression(pattern: "^[\\s\"'“”‘’「」『』]+|[\\s\"'“”‘’「」『』。，、！!？?.]+$")

// nanoMuse: The local chat naming card was removed; Morde sign-in remains the first-run entry.
