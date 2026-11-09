// NanoMuseSharedChatViews.swift
// Views and small view models shared by Morde surfaces and the legacy transcript.

import SwiftUI
import Combine
import UIKit

/// Posted by "Avatar studio…" on the agent page (and the face's menu); the
/// shell (or the root in the OpenMinis layout) presents the avatar studio.
extension Notification.Name {
    static let nanoMuseOpenAvatarStudio = Notification.Name("nanoMuse.openAvatarStudio")
}

// MARK: - Status line

enum NanoMuseStatus {
    /// What the header says, in order of importance (Android §2.2):
    /// a risky step needs approval > waiting for the person > the avatar
    /// studio's own line > the running step's own title > writing the
    /// reply > "On it: <request>" > "On it" > the avatar's motion clips
    /// being made > nothing (idle, the model line may show).
    static func line(
        waiting: Bool,
        needsApproval: Bool = false,
        processing: Bool,
        toolName: String,
        toolTitle: String,
        request: String?,
        studio: String?,
        motion: String? = nil
    ) -> String? {
        if needsApproval { return AppLocalized("Needs approval") }
        if waiting { return AppLocalized("Waiting for you") }
        if let studio, !studio.isEmpty { return studio }
        if processing {
            if toolName == "text" { return AppLocalized("Writing the reply") }
            if !toolName.isEmpty, !toolTitle.isEmpty { return toolTitle }
            if let brief = requestBrief(request) { return String(format: AppLocalized("On it: %@"), brief) }
            return AppLocalized("On it")
        }
        if let motion, !motion.isEmpty { return motion }
        return nil
    }

    /// The first ~36 characters of the request, cut at a word boundary
    /// when one is found past 18.
    static func requestBrief(_ request: String?) -> String? {
        guard let raw = request?.replacingOccurrences(of: "\n", with: " ")
            .trimmingCharacters(in: .whitespacesAndNewlines), !raw.isEmpty else { return nil }
        if raw.count <= 36 { return raw }
        let head = String(raw.prefix(36))
        if let space = head.lastIndex(of: " "), head.distance(from: head.startIndex, to: space) >= 18 {
            return String(head[..<space]) + "…"
        }
        return head + "…"
    }
}

// nanoMuse: Keep the Agent page's read-only avatar status surface; generation state belongs to NanoMuseAvatarStudioModel.
@MainActor
final class NanoMuseAvatarFlow: ObservableObject {
    static let shared = NanoMuseAvatarFlow()

    var statusLine: String? { nil }

    /// The old in-chat share card is not part of the Morde shell.
    func dismissShare() {}
}

// MARK: - Mood

/// Derives the face's mood from what the chat is doing. Happy and error
/// linger a few seconds after a turn ends, as on Android.
@MainActor
final class NanoMuseMoodModel: ObservableObject {
    @Published private(set) var afterglow: NanoMuseMood?
    private var afterglowTask: Task<Void, Never>?

    func turnEnded(withError: Bool) {
        afterglowTask?.cancel()
        afterglow = withError ? .error : .happy
        let seconds: Double = withError ? 4 : 3
        afterglowTask = Task { @MainActor [weak self] in
            try? await Task.sleep(nanoseconds: UInt64(seconds * 1_000_000_000))
            guard !Task.isCancelled else { return }
            self?.afterglow = nil
        }
    }

    func mood(waiting: Bool, working: Bool) -> NanoMuseMood {
        if waiting { return .waiting }
        if working { return .working }
        if let afterglow { return afterglow }
        return .idle
    }
}

// MARK: - Header scrim

/// What stands behind the Muse header over the main chat. Not a bar: a scrim, as Android's
/// header wrapper since #331. A gradient of the canvas colour from 80 percent at the top edge
/// through 35 percent a little past the middle to clear at the bottom, so the conversation
/// scrolls under the header and dissolves into the canvas instead of stopping at a line; and,
/// while the list is scrolled away from the latest message, a second fade in the full canvas
/// colour from the top edge (Android's `nmTopFade`), so the older messages melt away under the
/// face rather than show through it. The scrim takes the taps the material took before it, so
/// a tap on the header never reaches a message under it.
struct NanoMuseHeaderScrim: View {
    /// The list is scrolled up, away from the latest message.
    var scrolledUp: Bool

    var body: some View {
        ZStack {
            LinearGradient(
                stops: [
                    .init(color: ChatColors.background.opacity(0.8), location: 0),
                    .init(color: ChatColors.background.opacity(0.35), location: 0.55),
                    .init(color: ChatColors.background.opacity(0), location: 1),
                ],
                startPoint: .top,
                endPoint: .bottom
            )
            LinearGradient(
                colors: [ChatColors.background, ChatColors.background.opacity(0)],
                startPoint: .top,
                endPoint: .bottom
            )
            .opacity(scrolledUp ? 1 : 0)
            .animation(.easeInOut(duration: 0.25), value: scrolledUp)
        }
        .contentShape(Rectangle())
        // A tap on the bare scrim ends here: the rows the list has under the header (nmTopOverlay)
        // are not to be tapped through it. The header's own discs and pills lie over the scrim and
        // take their taps first.
        .onTapGesture {}
    }
}

// MARK: - The agent's bubble

/// Muse's transcript: the agent's prose sits in a pill a shade off the canvas
/// (Android: NmAssistantBubble, ChatColors.assistantBubble, radius 20, at most
/// 340 wide; 600 on a regular width, inside the iPad's column, NanoMuseColumn),
/// in the theme's text colour; a block that is a code fence, a table or raw HTML
/// stays bare, as does everything in the classic layout.
struct NanoMuseAssistantBubble: ViewModifier {
    var content: String
    /// The cell's hosting view hands SwiftUI the window's size class through its traits.
    @Environment(\.horizontalSizeClass) private var sizeClass

    /// Code, tables and markup read better without the bubble.
    static func isBare(_ content: String) -> Bool {
        let trimmed = content.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.hasPrefix("```") || trimmed.hasPrefix("~~~") || trimmed.hasPrefix("<") { return true }
        return trimmed.hasPrefix("|") && trimmed.contains("|\n|")
    }

    @ViewBuilder
    func body(content view: Content) -> some View {
        if NanoMuseShellPrefs.shell && !Self.isBare(content) {
            view
                .padding(.horizontal, 14)
                .padding(.vertical, 8)
                .background(NanoMuseTones.agentPill, in: RoundedRectangle(cornerRadius: 20, style: .continuous))
                .frame(maxWidth: NanoMuseColumn.agentPillWidth(for: sizeClass), alignment: .leading)
                .frame(maxWidth: .infinity, alignment: .leading)
        } else {
            view
        }
    }
}

// MARK: - Fence cards inside an assistant turn

/// An assistant text block that carries one or more `nanomuse-*` fences: prose stays
/// Markdown, each fence becomes its card. Unknown kinds show their JSON so nothing is lost.
struct NanoMuseFenceBlockView: View {
    var content: String

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            ForEach(Array(NanoMuseFences.pieces(content).enumerated()), id: \.offset) { _, piece in
                switch piece {
                case .prose(let text):
                    NanoMuseInlineMarkdown(text: text)
                        .font(.body)
                        .textSelection(.enabled)
                        .fixedSize(horizontal: false, vertical: true)
                case .block(let kind, let json):
                    NanoMuseFenceCard(kind: kind, json: json)
                }
            }
        }
    }
}

struct NanoMuseFenceCard: View {
    var kind: String
    var json: String

    private var object: [String: Any]? {
        guard let data = json.data(using: .utf8) else { return nil }
        return (try? JSONSerialization.jsonObject(with: data)) as? [String: Any]
    }

    var body: some View {
        if let o = object {
            switch kind {
            case NanoMuseGoalFlow.blockGoal: NanoMuseGoalCreatedCard(object: o)
            case NanoMuseGoalFlow.blockUpdate: NanoMuseGoalUpdateCard(object: o)
            case NanoMuseFeedFlow.block: NanoMuseFeedFenceCard(object: o)
            // nanoMuse: Persisted fence kinds are rendered without consulting local chat-flow state.
            case "avatar": NanoMuseAvatarFenceCard(object: o)
            case "naming": EmptyView()
            default: raw
            }
        } else if kind == "naming" {
            EmptyView()
        } else {
            raw
        }
    }

    private var raw: some View {
        Text(json)
            .font(.system(.footnote, design: .monospaced))
            .foregroundStyle(.secondary)
            .textSelection(.enabled)
    }
}

// nanoMuse: Render the persisted avatar payload independently of the removed local chat flow.
/// A persisted `nanomuse-avatar` fence: the chosen take and its saved images.
struct NanoMuseAvatarFenceCard: View {
    var object: [String: Any]

    private var chosen: Int { (object["chosen"] as? NSNumber)?.intValue ?? -1 }
    private var files: [String] { (object["files"] as? [String]) ?? [] }
    private var desc: String { object["desc"] as? String ?? "" }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            if !desc.isEmpty {
                Text(String(format: AppLocalized("New look: %@"), desc)).font(.footnote.weight(.semibold))
            }
            LazyVGrid(columns: [GridItem(.flexible(), spacing: 6), GridItem(.flexible(), spacing: 6), GridItem(.flexible(), spacing: 6), GridItem(.flexible(), spacing: 6)], spacing: 6) {
                ForEach(Array(files.enumerated()), id: \.offset) { index, path in
                    ZStack {
                        RoundedRectangle(cornerRadius: 10, style: .continuous).fill(NanoMuseTones.fill)
                        if !path.isEmpty, let image = UIImage(contentsOfFile: path) {
                            Image(uiImage: image).resizable().scaledToFill()
                                .clipShape(RoundedRectangle(cornerRadius: 10, style: .continuous))
                        } else {
                            Image(systemName: "photo").foregroundStyle(.tertiary)
                        }
                    }
                    .aspectRatio(1, contentMode: .fit)
                    .overlay {
                        RoundedRectangle(cornerRadius: 10, style: .continuous)
                            .strokeBorder(index == chosen ? NanoMuseTones.action : Color.clear, lineWidth: 2)
                    }
                }
            }
        }
        .padding(10)
        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 14, style: .continuous))
    }
}

/// The rounded frame every fence card sits in.
struct NanoMuseCardFrame<Content: View>: View {
    @ViewBuilder var content: () -> Content
    var body: some View {
        VStack(alignment: .leading, spacing: 0) { content() }
            .padding(.horizontal, 14)
            .padding(.vertical, 12)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 16, style: .continuous))
            .overlay(RoundedRectangle(cornerRadius: 16, style: .continuous).stroke(NanoMuseTones.hairline))
            .padding(.vertical, 4)
    }
}

/// "Goal created": title, why, up to five steps and the cadence.
struct NanoMuseGoalCreatedCard: View {
    var object: [String: Any]

    var body: some View {
        let title = object["title"] as? String ?? ""
        let why = object["why"] as? String ?? ""
        let steps = ((object["steps"] as? [Any]) ?? []).compactMap { $0 as? String }.filter { !$0.trimmingCharacters(in: .whitespaces).isEmpty }
        let everyHours = (object["check_every_hours"] as? NSNumber)?.intValue ?? Int(object["check_every_hours"] as? String ?? "") ?? 0
        let checkTime = (object["check_time"] as? String).flatMap { $0.isEmpty ? nil : $0 } ?? "09:00"
        NanoMuseCardFrame {
            HStack(spacing: 8) {
                Image(systemName: "flag").font(.footnote.weight(.semibold)).foregroundStyle(NanoMuseTones.action)
                Text(AppLocalized("Goal created")).font(.caption.weight(.medium)).foregroundStyle(.secondary)
            }
            Text(title).font(.body.weight(.semibold)).padding(.top, 6)
            if !why.isEmpty {
                Text(why).font(.footnote).foregroundStyle(.secondary).padding(.top, 2)
            }
            if !steps.isEmpty {
                VStack(alignment: .leading, spacing: 4) {
                    ForEach(Array(steps.prefix(5).enumerated()), id: \.offset) { _, step in
                        HStack(alignment: .top, spacing: 8) {
                            Image(systemName: "square").font(.footnote).foregroundStyle(.secondary).padding(.top, 2)
                            Text(step).font(.footnote)
                        }
                    }
                }
                .padding(.top, 8)
            }
            HStack {
                Text(everyHours > 0
                     ? (everyHours == 1 ? AppLocalized("Checks every hour") : String(format: AppLocalized("Checks every %d hours"), everyHours))
                     : String(format: AppLocalized("Checks daily at %@"), checkTime))
                    .font(.caption).foregroundStyle(.secondary)
                Spacer()
            }
            .padding(.top, 6)
        }
    }
}

/// A check-in's outcome: status, progress bar, the note.
struct NanoMuseGoalUpdateCard: View {
    var object: [String: Any]

    var body: some View {
        let progressRaw = (object["progress"] as? NSNumber)?.intValue ?? Int(object["progress"] as? String ?? "") ?? -1
        let progress = min(max(progressRaw, -1), 100)
        let status = (object["status"] as? String ?? "on_track").lowercased()
        let note = object["note"] as? String ?? ""
        let done = status == "done"
        let attention = status == "attention"
        let tint: Color = attention ? .red : NanoMuseTones.action
        NanoMuseCardFrame {
            HStack(spacing: 8) {
                Image(systemName: done ? "checkmark.square" : "chart.line.uptrend.xyaxis").font(.footnote.weight(.semibold)).foregroundStyle(tint)
                Text(AppLocalized("Goal update")).font(.caption.weight(.medium)).foregroundStyle(.secondary)
                Spacer()
                Text(done ? AppLocalized("Done") : attention ? AppLocalized("Needs attention") : AppLocalized("On track"))
                    .font(.caption.weight(.medium)).foregroundStyle(tint)
            }
            if progress >= 0 {
                ProgressView(value: Double(progress), total: 100)
                    .tint(tint)
                    .padding(.top, 10)
            }
            if !note.isEmpty {
                Text(note).font(.footnote).padding(.top, 8)
            }
        }
    }
}
