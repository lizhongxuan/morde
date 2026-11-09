//
//  NanoMuseRoutineWakes.swift
//  nanoMuse
//
//  A routine's turn is not the person's words. When a routine fires (the
//  daily feed, a goal check-in, a routine made from an idea), its prompt
//  goes into the conversation as a user turn, since that is how a turn
//  starts; the chat shows that row as one quiet line, "Routine · Morning
//  brief · 08:00", instead of a bubble in the person's voice, and the
//  model is told, for that one turn, that nobody typed it, so it opens the
//  way an assistant does who comes to you with the thing done.
//  Android: chat/RoutineWakes.kt, ui/chat/NmRoutineWakeRow.kt.
//

import Foundation
import SwiftUI

/// Which persisted user rows a routine sent, by row id (`ChatMessage.nmRoutine` is read from
/// here on reload). A small JSON map in UserDefaults, bounded.
@MainActor
final class NanoMuseRoutineWakes {
    static let shared = NanoMuseRoutineWakes()

    struct Wake: Codable, Equatable {
        var label: String
        var at: Date
    }

    private static let key = "nanomuse.routine_wakes"
    private static let keep = 400
    static let addendumTag = "nm-routine-wake"

    private var map: [String: Wake]

    init() {
        map = Self.load()
    }

    private static func load() -> [String: Wake] {
        if let data = UserDefaults.standard.data(forKey: key),
           let decoded = try? JSONDecoder().decode([String: Wake].self, from: data) {
            return decoded
        }
        return [:]
    }

    /// The defaults changed hands (C12, `NanoMuseAccountData`): the map is the account's.
    func reload() {
        map = Self.load()
    }

    /// The user row `mid` was a routine's; remembered for good.
    func record(mid: String, label: String, at: Date = Date()) {
        map[mid] = Wake(label: label, at: at)
        if map.count > Self.keep {
            for (k, _) in map.sorted(by: { $0.value.at < $1.value.at }).prefix(map.count - Self.keep) { map.removeValue(forKey: k) }
        }
        if let data = try? JSONEncoder().encode(map) { UserDefaults.standard.set(data, forKey: Self.key) }
    }

    func wake(mid: String) -> Wake? { map[mid] }

    /// "Routine · Morning brief · 08:00".
    nonisolated static func caption(label: String, at: Date) -> String {
        let c = Calendar.current.dateComponents([.hour, .minute], from: at)
        return String(format: AppLocalized("Routine · %1$@ · %2$@"), label, NanoMuseDay.clock(hour: c.hour ?? 0, minute: c.minute ?? 0))
    }

    /// What the model is told for this one turn (not shown to the person; English like the rest of the system prompt).
    nonisolated static func framing(label: String, at: Date) -> String {
        let c = Calendar.current.dateComponents([.hour, .minute], from: at)
        let time = NanoMuseDay.clock(hour: c.hour ?? 0, minute: c.minute ?? 0)
        return "## This turn is a routine's, not a message the person typed (morde)\n"
            + "The routine \"\(label)\" they set up fired at \(time) and sent the next message on their behalf. "
            + "Do what it asks, then write to them as the one who brings it up: open with the result or the point itself, "
            + "do not thank them for asking or say \"as requested\", and keep it as short as the task allows. "
            + "If something needs their say before you can act, ask plainly."
    }
}

/// The quiet line that stands where a routine's prompt would be a bubble (inside the user row's
/// trailing column, so it sits on the person's side like the bubble would).
struct NanoMuseRoutineWakeRow: View {
    let label: String
    let at: Date

    var body: some View {
        Text(NanoMuseRoutineWakes.caption(label: label, at: at))
            .font(.system(size: FontSettings.shared.scaledMessage(12)))
            .foregroundStyle(ChatColors.secondaryText)
            .multilineTextAlignment(.trailing)
            .padding(.horizontal, 4)
            .padding(.vertical, 6)
    }
}
