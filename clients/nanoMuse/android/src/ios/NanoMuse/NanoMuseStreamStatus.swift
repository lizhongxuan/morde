//
//  NanoMuseStreamStatus.swift
//  nanoMuse
//
//  Where a turn stands before the model has said anything, as nanoMuse Cloud tells it
//  (relay 0.31, docs/cloud.md). While the request waits for one of the relay's provider
//  slots, the stream carries a chunk with no `choices` and a `nanomuse` field every five
//  seconds: `phase: "queued"` and `ahead`, how many requests are in front of it; then one
//  with `phase: "working"` once the slot is its own. The providers hand every such field
//  here and the line under the typing dots reads the latest. Android: NmStreamStatus and
//  NmWaitLine.
//

import Combine
import SwiftUI

@MainActor
final class NanoMuseStreamStatus: ObservableObject {
    static let shared = NanoMuseStreamStatus()

    /// A queued status the relay has not refreshed for this long says nothing any more.
    nonisolated static let staleSeconds: TimeInterval = 15

    /// One `nanomuse` field: the phase, how many are ahead (-1 when the relay did not say), when it came.
    struct Status: Equatable, Sendable {
        let phase: String
        let ahead: Int
        let waitedMs: Int
        let at: Date

        /// True for a "queued" that came after `since` and is not yet stale at `now`.
        func queued(now: Date, since: Date) -> Bool {
            phase == "queued" && at >= since && now.timeIntervalSince(at) <= NanoMuseStreamStatus.staleSeconds
        }
    }

    /// The last status any stream received, for the line under the dots.
    @Published private(set) var latest: Status?

    private init() {}

    /// The `nanomuse` object of a chat chunk, from a provider's stream task; anything without a phase is ignored.
    nonisolated static func note(_ status: [String: Any]) {
        guard let phase = status["phase"] as? String, !phase.isEmpty else { return }
        let parsed = Status(
            phase: phase,
            ahead: status["ahead"] as? Int ?? -1,
            waitedMs: status["waited_ms"] as? Int ?? 0,
            at: Date()
        )
        Task { @MainActor in shared.latest = parsed }
    }
}

/// A small line under the typing dots while a step of the turn has no first token yet. Nothing
/// for the first ten seconds; then "The model is busy, 15 s so far" with a Cancel; and, when
/// nanoMuse Cloud has said the request is queued, "3 ahead of you" or "You are next" instead, as
/// soon as that is known. The clock starts when the dots appear, so each step of a tool-using
/// turn counts its own wait; a status that arrived before this step's dots is an earlier step's
/// and is ignored. `onCancel` is the chat's stop.
struct NanoMuseWaitLine: View {
    let onCancel: (() -> Void)?

    /// The typing dots have bounced this long before the line under them says anything.
    private static let quietSeconds = 10

    @ObservedObject private var status = NanoMuseStreamStatus.shared
    @State private var shownAt = Date()
    @State private var now = Date()
    private let tick = Timer.publish(every: 1, on: .main, in: .common).autoconnect()

    var body: some View {
        Group {
            if let text = line {
                HStack(spacing: 10) {
                    Text(text)
                    if let onCancel {
                        Button(AppLocalized("Cancel"), action: onCancel)
                            .buttonStyle(.plain)
                            .foregroundStyle(NanoMuseTones.action)
                    }
                }
                .font(.system(size: 12))
                .foregroundStyle(ChatColors.tertiaryText)
                .padding(.bottom, 4)
            }
        }
        .onReceive(tick) { now = $0 }
    }

    private var line: String? {
        let waited = max(0, Int(now.timeIntervalSince(shownAt)))
        if let queued = status.latest, queued.queued(now: now, since: shownAt) {
            if queued.ahead > 0 { return AppLocalized("\(queued.ahead) ahead of you, \(waited) s so far") }
            return AppLocalized("You are next, \(waited) s so far")
        }
        if waited >= Self.quietSeconds { return AppLocalized("The model is busy, \(waited) s so far") }
        return nil
    }
}
