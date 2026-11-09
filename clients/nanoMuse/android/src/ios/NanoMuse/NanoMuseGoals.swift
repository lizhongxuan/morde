//
//  NanoMuseGoals.swift
//  nanoMuse
//
//  Goals, Muse style: shaped in the chat (three short questions), handed
//  over by the model as a ```nanomuse-goal``` block, turned into a goal
//  with its own conversation and a hidden check-in routine; every check
//  ends with a ```nanomuse-goal-update``` block that flows back here.
//  Android: io.github.nanomuse.goals.*.
//

import Foundation
import SwiftUI

// MARK: - Model

enum NanoMuseGoalCategory: String, Codable, CaseIterable, Identifiable {
    case health, relationships, finance, career, interests, productivity, other
    var id: String { rawValue }

    static func from(_ key: String?) -> NanoMuseGoalCategory {
        NanoMuseGoalCategory(rawValue: (key ?? "").lowercased()) ?? .other
    }

    var label: String {
        switch self {
        case .health: return AppLocalized("Health")
        case .relationships: return AppLocalized("Relationships")
        case .finance: return AppLocalized("Finance")
        case .career: return AppLocalized("Career")
        case .interests: return AppLocalized("Interests")
        case .productivity: return AppLocalized("Productivity")
        case .other: return AppLocalized("Other")
        }
    }

    var symbol: String {
        switch self {
        case .health: return "heart"
        case .relationships: return "person.2"
        case .finance: return "banknote"
        case .career: return "briefcase"
        case .interests: return "paintpalette"
        case .productivity: return "checkmark.circle"
        case .other: return "flag"
        }
    }
}

enum NanoMuseGoalStatus: String, Codable { case active, paused, done }

struct NanoMuseGoalStep: Codable, Equatable, Identifiable {
    var id: String = UUID().uuidString
    var text: String
    var done: Bool = false
}

struct NanoMuseGoal: Codable, Identifiable, Equatable {
    var id: String = UUID().uuidString
    var title: String
    var why: String = ""
    var category: NanoMuseGoalCategory = .other
    /// 0 = a daily check at `checkHour:checkMinute`; otherwise every N hours.
    var checkEveryHours: Int = 0
    var checkHour: Int = 9
    var checkMinute: Int = 0
    var steps: [NanoMuseGoalStep] = []
    var progress: Int = 0
    var status: NanoMuseGoalStatus = .active
    var sessionId: String? = nil
    var routineId: String? = nil
    var lastNote: String? = nil
    var lastCheckedAt: Date? = nil
    var createdAt: Date = Date()
    var updatedAt: Date = Date()

    /// "Checks every 2 hours" / "Checks daily at 08:00".
    var cadence: String {
        if checkEveryHours > 0 {
            return checkEveryHours == 1 ? AppLocalized("Checks every hour") : String(format: AppLocalized("Checks every %d hours"), checkEveryHours)
        }
        return String(format: AppLocalized("Checks daily at %@"), NanoMuseDay.clock(hour: checkHour, minute: checkMinute))
    }

    // MARK: From the model's block (pure, tested)

    /// A goal from a `nanomuse-goal` block; nil when there is no usable title.
    static func from(block o: [String: Any]) -> (goal: NanoMuseGoal, firstCheck: String)? {
        let title = (o["title"] as? String ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        guard !title.isEmpty else { return nil }
        let everyRaw = (o["check_every_hours"] as? NSNumber)?.intValue ?? Int(o["check_every_hours"] as? String ?? "") ?? 0
        let every = min(max(everyRaw, 0), 24 * 7)
        let time = NanoMuseDay.parseClock(o["check_time"] as? String) ?? parseLooseTime(o["check_time"] as? String) ?? (9, 0)
        let steps = ((o["steps"] as? [Any]) ?? []).compactMap { ($0 as? String)?.trimmingCharacters(in: .whitespacesAndNewlines) }
            .filter { !$0.isEmpty }
            .prefix(6)
            .map { NanoMuseGoalStep(text: $0) }
        var goal = NanoMuseGoal(title: String(title.prefix(60)))
        goal.why = (o["why"] as? String ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        goal.category = .from(o["category"] as? String)
        goal.checkEveryHours = every
        goal.checkHour = time.hour
        goal.checkMinute = time.minute
        goal.steps = Array(steps)
        let first = (o["first_check"] as? String ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        return (goal, first)
    }

    /// "9:00", "at 18:30" → the time inside.
    private static func parseLooseTime(_ s: String?) -> (hour: Int, minute: Int)? {
        guard let s, let regex = try? NSRegularExpression(pattern: "(\\d{1,2}):(\\d{2})") else { return nil }
        let ns = s as NSString
        guard let m = regex.firstMatch(in: s, range: NSRange(location: 0, length: ns.length)),
              let h = Int(ns.substring(with: m.range(at: 1))), let min = Int(ns.substring(with: m.range(at: 2))) else { return nil }
        return (Swift.min(Swift.max(h, 0), 23), Swift.min(Swift.max(min, 0), 59))
    }

    /// What a `nanomuse-goal-update` block changes.
    struct Update: Equatable {
        var goalId: String?
        var progress: Int?
        var status: NanoMuseGoalStatus?
        var note: String?

        init?(block o: [String: Any]) {
            goalId = (o["goal_id"] as? String).flatMap { $0.isEmpty ? nil : $0 }
            if let n = o["progress"] as? NSNumber { progress = min(max(n.intValue, 0), 100) }
            else if let s = o["progress"] as? String, let n = Int(s) { progress = min(max(n, 0), 100) }
            switch (o["status"] as? String ?? "").lowercased() {
            case "on_track", "active": status = .active
            case "attention": status = .active
            case "done": status = .done
            case "paused": status = .paused
            default: status = nil
            }
            note = (o["note"] as? String).flatMap { $0.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty ? nil : $0 }
            if goalId == nil && progress == nil && status == nil && note == nil { return nil }
        }
    }
}

// MARK: - Store

@MainActor
final class NanoMuseGoalStore: ObservableObject {
    static let shared = NanoMuseGoalStore()

    @Published private(set) var goals: [NanoMuseGoal] = []

    private var file: URL { NanoMuseDirs.root.appendingPathComponent("goals.json") }

    private init() { load() }

    private func load() {
        guard let data = try? Data(contentsOf: file) else { return }
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        goals = (try? decoder.decode([NanoMuseGoal].self, from: data)) ?? []
    }

    /// C12: the file changed under the store — another account's goals are in place (or none).
    func reload() {
        goals = []
        load()
    }

    private func save() {
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        if let data = try? encoder.encode(goals) { try? data.write(to: file, options: .atomic) }
    }

    func goal(id: String) -> NanoMuseGoal? { goals.first { $0.id == id } }
    func forSession(_ sessionId: String) -> NanoMuseGoal? { goals.first { $0.sessionId == sessionId } }

    var tracking: [NanoMuseGoal] { goals.filter { $0.status != .done }.sorted { $0.createdAt > $1.createdAt } }
    var finished: [NanoMuseGoal] { goals.filter { $0.status == .done }.sorted { $0.updatedAt > $1.updatedAt } }

    func upsert(_ goal: NanoMuseGoal) {
        var g = goal
        g.updatedAt = Date()
        if let i = goals.firstIndex(where: { $0.id == goal.id }) { goals[i] = g } else { goals.append(g) }
        save()
    }

    func delete(_ id: String) {
        goals.removeAll { $0.id == id }
        save()
    }

    func applyUpdate(_ update: NanoMuseGoal.Update, fallbackGoalId: String?) {
        guard let id = update.goalId ?? fallbackGoalId, var g = goal(id: id) else { return }
        if let p = update.progress { g.progress = p }
        let wasDone = g.status == .done
        if let s = update.status {
            g.status = s
            if s == .done { g.progress = 100 }
        }
        if let n = update.note { g.lastNote = n }
        g.lastCheckedAt = Date()
        upsert(g)
        if g.status == .done, let rid = g.routineId { NanoMuseScheduler.shared.setEnabled(rid, false) }
        if g.status == .done, !wasDone { NanoMuseStar.shared.goalDone() } // C1
    }
}

// MARK: - Flow

enum NanoMuseGoalFlow {
    static let addendumTag = "nanomuse-goal-create"
    static let blockGoal = "goal"
    static let blockUpdate = "goal-update"

    /// "I'd like to create a Health goal." — the message sent on the person's behalf; the steering goes into the addendum.
    @MainActor
    static func startCreation(session: String, category: NanoMuseGoalCategory) -> String {
        NanoMuseSessionAddenda.add(session: session, tag: addendumTag, text: creationAddendum(category), turns: 8)
        return String(format: AppLocalized("I'd like to create a %@ goal."), category.label)
    }

    static func creationAddendum(_ category: NanoMuseGoalCategory) -> String {
        """
        ## Creating a goal (morde)
        The user just chose to create a goal in the "\(category.rawValue)" category from the Goals tab.
        Shape it together: ask at most three short questions, ONE message at a time,
        in the user's language: (1) what exactly they want to achieve, (2) why it matters and by
        when, (3) how often you should check in (every N hours, or daily at a time). Two or three
        sentences per message; if the user already answered something, skip that question.
        When you have enough, reply with one warm sentence of confirmation and then EXACTLY ONE fenced
        code block tagged `nanomuse-goal` containing JSON with these keys:
        {"title": "<= 40 chars", "why": "one sentence", "category": "\(category.rawValue)",
         "check_every_hours": <integer, 0 means a daily check>, "check_time": "HH:MM" (used when check_every_hours is 0),
         "steps": ["3 to 5 short steps you will take or track"], "first_check": "what you will do at the first check"}
        Do not describe the block or mention JSON; the app renders it as a card. Say nothing after the block.
        Later, checks for this goal will happen in their own conversation when the app is open; on the iPhone the app cannot run while it is asleep, so the person is reminded to open it.
        """
    }

    /// After every finished turn: goal blocks become goals, update blocks update them.
    @MainActor
    static func afterTurn(session: String, assistantText: String?) {
        guard MordeLocalExecutionPolicy.isEnabled else { return }
        guard let text = assistantText, !text.isEmpty else { return }
        let store = NanoMuseGoalStore.shared
        if NanoMuseFences.contains(text, kind: blockGoal), let o = NanoMuseFences.lastObject(blockGoal, in: text) {
            if NanoMuseSessionAddenda.has(session: session, tag: addendumTag) || !alreadyCreated(from: o) {
                if let made = NanoMuseGoal.from(block: o) {
                    store.upsert(made.goal)
                    let first = made.firstCheck
                    Task { @MainActor in await setUp(made.goal, firstCheck: first) }
                }
            }
            NanoMuseSessionAddenda.remove(session: session, tag: addendumTag)
        }
        if NanoMuseFences.contains(text, kind: blockUpdate) {
            for o in NanoMuseFences.objects(blockUpdate, in: text) {
                guard let update = NanoMuseGoal.Update(block: o) else { continue }
                store.applyUpdate(update, fallbackGoalId: store.forSession(session)?.id)
            }
        }
    }

    @MainActor
    private static func alreadyCreated(from o: [String: Any]) -> Bool {
        let title = String((o["title"] as? String ?? "").trimmingCharacters(in: .whitespacesAndNewlines).prefix(60))
        guard !title.isEmpty else { return true }
        return NanoMuseGoalStore.shared.goals.contains { $0.title == title }
    }

    // nanoMuse: Goal data remains readable; Morde does not create local check-in sessions.
    @MainActor
    static func setUp(_ goal: NanoMuseGoal, firstCheck: String) async {
        return
    }

    /// The one line each check sends into the goal's conversation.
    static func checkPrompt(_ goal: NanoMuseGoal, firstCheck: String? = nil) -> String {
        var s = String(format: AppLocalized("Time to check on this goal: %@"), goal.title)
        if let first = firstCheck?.trimmingCharacters(in: .whitespacesAndNewlines), !first.isEmpty, goal.lastCheckedAt == nil {
            s += "\n" + String(format: AppLocalized("First time round: %@"), first)
        }
        return s
    }

    /// The system-prompt addendum of a goal's conversation; nil for every other one.
    @MainActor
    static func systemAddendum(session: String) -> String? {
        guard let goal = NanoMuseGoalStore.shared.forSession(session) else { return nil }
        let cadence = goal.checkEveryHours > 0 ? "every \(goal.checkEveryHours) hour(s)" : "daily at \(NanoMuseDay.clock(hour: goal.checkHour, minute: goal.checkMinute))"
        let steps = goal.steps.isEmpty ? "(none yet)" : goal.steps.enumerated().map { "\($0.offset + 1). \($0.element.text)" }.joined(separator: "\n")
        var s = "## This conversation tracks a goal (morde)\n"
        s += "Goal: \(goal.title)\n"
        if !goal.why.isEmpty { s += "Why: \(goal.why)\n" }
        s += "Steps:\n\(steps)\n"
        s += "Checks run \(cadence); progress so far \(goal.progress)%" + (goal.lastNote.map { ", last note: \($0)" } ?? "") + ".\n\n"
        s += "When a message asks you to check on the goal: find out where it stands right now with your tools "
        s += "(shell, browser, MCP servers, memory) and take the next small step if it is safe and reversible. "
        s += "Never pay, send messages or delete anything without asking. Then write the user a short update in "
        s += "their language (at most six lines) and end with EXACTLY ONE fenced code block tagged `nanomuse-goal-update` "
        s += "containing {\"goal_id\": \"\(goal.id)\", \"progress\": <0-100>, \"status\": \"on_track|attention|done\", \"note\": \"one line\"}. "
        s += "In ordinary conversation here, answer normally and add that block only when the goal's progress actually changed."
        return s
    }

    @MainActor
    static func setPaused(_ goal: NanoMuseGoal, _ paused: Bool) {
        var g = goal
        g.status = paused ? .paused : .active
        NanoMuseGoalStore.shared.upsert(g)
        if let rid = g.routineId { NanoMuseScheduler.shared.setEnabled(rid, !paused) }
    }

    @MainActor
    static func markDone(_ goal: NanoMuseGoal, _ done: Bool) {
        var g = goal
        g.status = done ? .done : .active
        if done { g.progress = 100 }
        NanoMuseGoalStore.shared.upsert(g)
        if let rid = g.routineId { NanoMuseScheduler.shared.setEnabled(rid, !done) }
        if done, goal.status != .done { NanoMuseStar.shared.goalDone() } // C1: a goal reached is a moment to ask for a star
    }

    /// The goal and its check go; its conversation stays (it is the person's history).
    @MainActor
    static func delete(_ goal: NanoMuseGoal) {
        if let rid = goal.routineId { NanoMuseScheduler.shared.delete(rid) }
        NanoMuseGoalStore.shared.delete(goal.id)
    }

    @MainActor
    static func checkNow(_ goal: NanoMuseGoal) {
        guard let rid = goal.routineId else { return }
        Task { @MainActor in await NanoMuseScheduler.shared.run(rid, trigger: "manual") }
    }

    /// "Checks every 2 hours · next 09:51"
    @MainActor
    static func cadenceLabel(_ goal: NanoMuseGoal) -> String {
        let base = goal.cadence
        guard let rid = goal.routineId, let routine = NanoMuseScheduler.shared.routine(id: rid), let next = routine.nextDue(after: Date()) else { return base }
        if next <= Date() { return String(format: AppLocalized("%@ · due now, runs when the app is open"), base) }
        let when = Calendar.current.isDateInToday(next)
            ? next.formatted(date: .omitted, time: .shortened)
            : next.formatted(date: .abbreviated, time: .shortened)
        return String(format: AppLocalized("%@ · next %@"), base, when)
    }
}

// MARK: - Goals room

struct NanoMuseMordeGoalRecord: Identifiable, Equatable {
    let id: String
    let revision: Int64
    let body: MordePersonalGoalBody

    var progress: Int {
        if body.status == "done" { return 100 }
        guard !body.steps.isEmpty else { return 0 }
        let done = body.steps.filter { $0.status == "done" }.count
        return (done * 100 + body.steps.count / 2) / body.steps.count
    }
}

enum NanoMuseMordeGoalMapper {
    static func body(title: String, description: String, status: String = "active",
                     steps: [MordePersonalGoalStep] = [], relatedMaterialIDs: [String] = []) -> MordePersonalGoalBody {
        MordePersonalGoalBody(title: title.trimmingCharacters(in: .whitespacesAndNewlines),
                              description: description.trimmingCharacters(in: .whitespacesAndNewlines),
                              status: status, steps: steps, related_material_ids: relatedMaterialIDs)
    }
}

enum NanoMuseMordeScheduleRunHistoryPolicy {
    static func canAppend(existingIDs: [String], page: [MordeScheduleRunDTO], scheduleID: String) -> Bool {
        let ids = existingIDs + page.map(\.id)
        return page.allSatisfy { $0.schedule_id == scheduleID } && Set(ids).count == ids.count
    }
}

@MainActor
final class NanoMuseMordeGoalsCloud: ObservableObject {
    static let shared = NanoMuseMordeGoalsCloud()

    @Published private(set) var goals: [NanoMuseMordeGoalRecord] = []
    @Published private(set) var schedules: [NanoMuseScheduleDTO] = []
    @Published private(set) var scheduleRuns: [String: [MordeScheduleRunDTO]] = [:]
    @Published private(set) var scheduleRunCursors: [String: String] = [:]
    @Published private(set) var scheduleRunErrors: [String: String] = [:]
    @Published private(set) var scheduleRunLoadingIDs: Set<String> = []
    @Published private(set) var scheduleTargets: MordeScheduleTargets?
    @Published private(set) var pending: MordePersonalDataMutation?
    @Published private(set) var syncEnabled = true
    @Published private(set) var isLoading = false
    @Published private(set) var isSaving = false
    @Published private(set) var isAuthorizingSchedules = false
    @Published private(set) var error: String?

    private var loadedScope: String?
    private var scheduleError: String?
    private var requestGeneration = UUID()

    private func requestIsCurrent(scope: String?, generation: UUID) -> Bool {
        MordeScopedRequestPolicy.isCurrent(scope: scope, generation: generation,
                                            activeScope: MordeShellAuthorization.shared.personalDataScopeKey,
                                            activeGeneration: requestGeneration)
    }

    private init() {}

    func refresh() async {
        let auth = MordeShellAuthorization.shared
        let scope = auth.personalDataScopeKey
        if loadedScope != scope {
            loadedScope = scope
            requestGeneration = UUID()
            goals = []
            schedules = []
            scheduleRuns = [:]
            scheduleRunCursors = [:]
            scheduleRunErrors = [:]
            scheduleRunLoadingIDs = []
            scheduleTargets = nil
            scheduleError = nil
            pending = nil
            error = nil
            isLoading = false
            isAuthorizingSchedules = false
        }
        let generation = requestGeneration
        guard let scope else { return }
        syncEnabled = NanoMuseMordeProfileSyncPreference.isEnabled(scopeKey: scope)
        do {
            pending = try NanoMuseMordePersonalDataOutbox.load(scopeKey: scope, root: NanoMuseDirs.root)
            if syncEnabled, pending != nil {
                do { try await flush(scope: scope) }
                catch is MordePersonalRevisionConflict { /* Keep reading the cloud snapshot while the user resolves it. */ }
            }
            guard requestIsCurrent(scope: scope, generation: generation) else { return }
            guard syncEnabled else { return }
            isLoading = true
            defer { if requestGeneration == generation { isLoading = false } }
            let values = try await readCloudGoals(scope: scope)
            guard requestIsCurrent(scope: scope, generation: generation) else { return }
            goals = values
            if error == nil || pending == nil { error = nil }
        } catch {
            if requestIsCurrent(scope: scope, generation: generation) { self.error = error.localizedDescription }
        }
    }

    func save(_ body: MordePersonalGoalBody, id: String? = nil, revision: Int64 = 0) async throws {
        guard let scope = MordeShellAuthorization.shared.personalDataScopeKey,
              body.isValidForWrite else { throw URLError(.userAuthenticationRequired) }
        do {
            let objectID = id ?? "g_\(UUID().uuidString.lowercased().replacingOccurrences(of: "-", with: ""))"
            let encoded = try JSONEncoder().encode(body)
            let value = try JSONDecoder().decode(MordePersonalJSONValue.self, from: encoded)
            let mutation = MordePersonalDataMutation(scopeKey: scope, mutationID: UUID().uuidString,
                objectType: "goal", objectID: objectID, baseRevision: revision, body: value, isDelete: false)
            try NanoMuseMordePersonalDataOutbox.enqueue(mutation, scopeKey: scope, root: NanoMuseDirs.root)
            pending = mutation
            error = nil
            syncEnabled = NanoMuseMordeProfileSyncPreference.isEnabled(scopeKey: scope)
            if syncEnabled { try await flush(scope: scope); await refreshGoals(scope: scope) }
        } catch {
            if MordeShellAuthorization.shared.personalDataScopeKey == scope { self.error = error.localizedDescription }
            throw error
        }
    }

    func delete(_ goal: NanoMuseMordeGoalRecord) async throws {
        guard let scope = MordeShellAuthorization.shared.personalDataScopeKey else { throw URLError(.userAuthenticationRequired) }
        do {
            let mutation = MordePersonalDataMutation(scopeKey: scope, mutationID: UUID().uuidString,
                objectType: "goal", objectID: goal.id, baseRevision: goal.revision, body: nil, isDelete: true)
            try NanoMuseMordePersonalDataOutbox.enqueue(mutation, scopeKey: scope, root: NanoMuseDirs.root)
            pending = mutation
            error = nil
            syncEnabled = NanoMuseMordeProfileSyncPreference.isEnabled(scopeKey: scope)
            if syncEnabled { try await flush(scope: scope); await refreshGoals(scope: scope) }
        } catch {
            if MordeShellAuthorization.shared.personalDataScopeKey == scope { self.error = error.localizedDescription }
            throw error
        }
    }

    func retry() async {
        if NanoMuseMordePersonalConflictResolution.shared.hasConflict {
            NanoMuseMordePersonalConflictResolution.shared.present()
            return
        }
        await refresh()
    }

    private func flush(scope: String) async throws {
        guard !isSaving, MordeShellAuthorization.shared.personalDataScopeKey == scope,
              NanoMuseMordeProfileSyncPreference.isEnabled(scopeKey: scope),
              let mutation = try NanoMuseMordePersonalDataOutbox.load(scopeKey: scope, root: NanoMuseDirs.root) else { return }
        pending = mutation
        if NanoMuseMordePersonalConflictResolution.shared.isBlocked(mutation.mutationID) {
            NanoMuseMordePersonalConflictResolution.shared.present()
            return
        }
        isSaving = true
        defer { isSaving = false }
        do {
            if mutation.isDelete {
                _ = try await MordeShellAuthorization.shared.deletePersonalObject(
                    objectType: mutation.objectType, objectID: mutation.objectID,
                    baseRevision: mutation.baseRevision, idempotencyKey: mutation.mutationID)
            } else if let body = mutation.body {
                _ = try await MordeShellAuthorization.shared.replacePersonalObject(
                    objectType: mutation.objectType, objectID: mutation.objectID,
                    baseRevision: mutation.baseRevision, body: body, idempotencyKey: mutation.mutationID)
            } else {
                throw NanoMuseMordePersonalDataOutboxError.invalidMutation
            }
            guard MordeShellAuthorization.shared.personalDataScopeKey == scope else { return }
            try NanoMuseMordePersonalDataOutbox.acknowledge(scopeKey: scope, mutationID: mutation.mutationID,
                                                            root: NanoMuseDirs.root)
            pending = nil
            error = nil
        } catch let conflict as MordePersonalRevisionConflict {
            NanoMuseMordePersonalConflictResolution.shared.record(mutation, conflict: conflict, root: NanoMuseDirs.root)
            if MordeShellAuthorization.shared.personalDataScopeKey == scope { self.error = conflict.localizedDescription }
            throw conflict
        } catch {
            if MordeShellAuthorization.shared.personalDataScopeKey == scope { self.error = error.localizedDescription }
            throw error
        }
    }

    private func readCloudGoals(scope: String) async throws -> [NanoMuseMordeGoalRecord] {
        let auth = MordeShellAuthorization.shared
        var values: [NanoMuseMordeGoalRecord] = []
        var cursor: String?
        var seen = Set<String>()
        repeat {
            guard auth.personalDataScopeKey == scope else { throw CancellationError() }
            let page = try await auth.personalSnapshot(cursor: cursor)
            guard auth.personalDataScopeKey == scope else { throw CancellationError() }
            for item in page.items where item.object_type == "goal" {
                let body = try item.decodeBody(MordePersonalGoalBody.self)
                guard body.isValidForWrite else { throw URLError(.cannotParseResponse) }
                values.append(NanoMuseMordeGoalRecord(id: item.object_id, revision: item.revision, body: body))
            }
            cursor = page.next_cursor
            if MordePersonalSnapshotCursorPolicy.repeats(cursor, seen: &seen) {
                throw URLError(.cannotParseResponse)
            }
        } while cursor?.isEmpty == false
        return values.sorted { $0.body.title.localizedCaseInsensitiveCompare($1.body.title) == .orderedAscending }
    }

    private func refreshGoals(scope: String) async {
        let generation = requestGeneration
        guard requestIsCurrent(scope: scope, generation: generation),
              NanoMuseMordeProfileSyncPreference.isEnabled(scopeKey: scope) else { return }
        do {
            let values = try await readCloudGoals(scope: scope)
            guard requestIsCurrent(scope: scope, generation: generation) else { return }
            goals = values
        } catch {
            if requestIsCurrent(scope: scope, generation: generation) { self.error = error.localizedDescription }
        }
    }

    func refreshSchedules() async {
        let auth = MordeShellAuthorization.shared
        guard let scope = auth.personalDataScopeKey else {
            schedules = []
            scheduleRuns = [:]
            scheduleRunCursors = [:]
            scheduleRunErrors = [:]
            scheduleTargets = nil
            scheduleError = nil
            return
        }
        if loadedScope != scope { await refresh() }
        guard auth.personalDataScopeKey == scope else { return }
        let generation = requestGeneration
        guard auth.scheduleAuthorizationState.enabled else {
            guard requestIsCurrent(scope: scope, generation: generation) else { return }
            schedules = []
            scheduleRuns = [:]
            scheduleRunCursors = [:]
            scheduleRunErrors = [:]
            scheduleTargets = nil
            scheduleError = nil
            return
        }
        do {
            let page = try await auth.schedules(limit: 100)
            guard requestIsCurrent(scope: scope, generation: generation) else { return }
            schedules = page.items
            scheduleError = nil
        } catch {
            guard requestIsCurrent(scope: scope, generation: generation) else { return }
            scheduleError = error.localizedDescription
        }
    }

    func setScheduleAuthorization(_ enabled: Bool, for expectedScope: String?) async {
        let auth = MordeShellAuthorization.shared
        guard let expectedScope, auth.personalDataScopeKey == expectedScope else { return }
        if loadedScope != expectedScope { await refresh() }
        guard auth.personalDataScopeKey == expectedScope else { return }
        let generation = requestGeneration
        isAuthorizingSchedules = true
        defer { if requestGeneration == generation { isAuthorizingSchedules = false } }
        do {
            if enabled { _ = try await auth.enableCloudSchedules() }
            else { _ = try await auth.disableCloudSchedules() }
            guard requestIsCurrent(scope: expectedScope, generation: generation) else { return }
            scheduleError = nil
            await refreshSchedules()
        } catch {
            guard requestIsCurrent(scope: expectedScope, generation: generation) else { return }
            scheduleError = error.localizedDescription
        }
    }

    func refreshScheduleTargets() async {
        let auth = MordeShellAuthorization.shared
        guard let scope = auth.personalDataScopeKey else {
            scheduleTargets = nil
            return
        }
        if loadedScope != scope { await refresh() }
        guard auth.personalDataScopeKey == scope, auth.scheduleAuthorizationState.enabled else {
            scheduleTargets = nil
            return
        }
        let generation = requestGeneration
        do {
            let targets = try await auth.scheduleTargets()
            guard requestIsCurrent(scope: scope, generation: generation) else { return }
            scheduleTargets = targets
            scheduleError = nil
        } catch {
            guard requestIsCurrent(scope: scope, generation: generation) else { return }
            scheduleError = error.localizedDescription
        }
    }

    func toggle(_ schedule: NanoMuseScheduleDTO, for expectedScope: String?) async {
        guard let expectedScope,
              loadedScope == expectedScope,
              requestIsCurrent(scope: expectedScope, generation: requestGeneration),
              MordeShellAuthorization.shared.scheduleAuthorizationState.enabled else { return }
        let generation = requestGeneration
        do {
            let action = schedule.status == "enabled" ? "disable" : "enable"
            _ = try await MordeShellAuthorization.shared.scheduleAction(id: schedule.id,
                request: MordeScheduleActionRequest(action: action, expected_version: schedule.version,
                    current_run_action: schedule.current_run == nil ? nil : "continue"),
                idempotencyKey: UUID().uuidString)
            guard requestIsCurrent(scope: expectedScope, generation: generation) else { return }
            await refreshSchedules()
        } catch {
            guard requestIsCurrent(scope: expectedScope, generation: generation) else { return }
            scheduleError = error.localizedDescription
        }
    }

    func loadScheduleRuns(id: String, cursor: String? = nil) async {
        let auth = MordeShellAuthorization.shared
        guard let scope = auth.personalDataScopeKey, auth.scheduleAuthorizationState.enabled else { return }
        if loadedScope != scope { await refresh() }
        guard auth.personalDataScopeKey == scope, schedules.contains(where: { $0.id == id }),
              !scheduleRunLoadingIDs.contains(id),
              cursor.map({ scheduleRunCursors[id] == $0 }) ?? true else { return }
        let generation = requestGeneration
        scheduleRunLoadingIDs.insert(id)
        defer { if requestGeneration == generation { scheduleRunLoadingIDs.remove(id) } }
        do {
            let page = try await auth.scheduleRuns(id: id, limit: 50, cursor: cursor)
            guard requestIsCurrent(scope: scope, generation: generation) else { return }
            let existing = cursor == nil ? [] : (scheduleRuns[id] ?? [])
            guard NanoMuseMordeScheduleRunHistoryPolicy.canAppend(
                existingIDs: existing.map(\.id), page: page.items, scheduleID: id),
                  cursor.map({ page.next_cursor != $0 }) ?? true else {
                throw URLError(.cannotParseResponse)
            }
            scheduleRuns[id] = existing + page.items
            scheduleRunCursors[id] = page.next_cursor
            scheduleRunErrors[id] = nil
        } catch {
            guard requestIsCurrent(scope: scope, generation: generation) else { return }
            scheduleRunErrors[id] = error.localizedDescription
        }
    }

    var schedulesError: String? { scheduleError }
}

struct NanoMuseGoalsRoom: View {
    var chrome: NanoMuseRoomChrome
    var onStartGoal: (NanoMuseGoalCategory) -> Void
    var onOpenSession: (String) -> Void

    @ObservedObject private var store = NanoMuseGoalStore.shared
    @ObservedObject private var scheduler = NanoMuseScheduler.shared
    @ObservedObject private var cloud = NanoMuseMordeGoalsCloud.shared
    @ObservedObject private var authorization = MordeShellAuthorization.shared
    @State private var pickedCategory: NanoMuseGoalCategory?
    @State private var deleting: NanoMuseGoal?
    @State private var editingRoutine: NanoMuseRoutine?
    @State private var importingRoutine: NanoMuseRoutine?
    @State private var editingCloudGoal: NanoMuseMordeGoalRecord?
    @State private var showingCloudGoalEditor = false
    @State private var deletingCloudGoal: NanoMuseMordeGoalRecord?
    @State private var expandedScheduleRunIDs = Set<String>()

    var body: some View {
        // Cloud Goals and Schedules remain server-backed; this room exposes no iOS management entry.
        VStack(spacing: 0) {
            // Android: the Goals room's ••• menu — all routines, then the shared rows.
            NanoMuseRoomHeader(chrome: chrome) {
                NanoMuseRoomMenu(chrome: chrome) {
                    Button { chrome.open("routines") } label: { Label(AppLocalized("All routines"), systemImage: "clock") }
                }
            }
            ScrollView {
                VStack(alignment: .leading, spacing: 18) {
                    trackingSection
                    routinesSection
                    if !authorization.signedIn { createSection }
                }
                .padding(16)
            }
        }
        // The chat's canvas (white, black in dark), as Android's GoalsTab and docs/design.md; the
        // grey page is the Feed's alone. The cards keep a hairline so they read on it.
        .background(ChatColors.background.ignoresSafeArea())
        .sheet(item: $pickedCategory) { category in
            NanoMuseGoalCategorySheet(category: category) {
                pickedCategory = nil
                onStartGoal(category)
            }
            .presentationDetents([.medium])
        }
        .sheet(item: $editingRoutine) { routine in
            NanoMuseRoutineEditor(routine: routine)
        }
        .sheet(item: $importingRoutine) { routine in
            NanoMuseScheduleImportSheet(routine: routine) {
                Task { await cloud.refreshSchedules() }
            }
        }
        .sheet(isPresented: $showingCloudGoalEditor, onDismiss: { editingCloudGoal = nil }) {
            NanoMuseMordeGoalEditor(goal: editingCloudGoal) { body in
                try await cloud.save(body, id: editingCloudGoal?.id, revision: editingCloudGoal?.revision ?? 0)
            }
        }
        .confirmationDialog(AppLocalized("Delete this goal? Its conversation stays in your chats."), isPresented: Binding(get: { deleting != nil }, set: { if !$0 { deleting = nil } }), titleVisibility: .visible) {
            Button(AppLocalized("Delete goal"), role: .destructive) {
                if let g = deleting { NanoMuseGoalFlow.delete(g) }
                deleting = nil
            }
            Button(AppLocalized("Cancel"), role: .cancel) { deleting = nil }
        }
        .confirmationDialog("Delete this cloud goal?", isPresented: Binding(get: { deletingCloudGoal != nil }, set: { if !$0 { deletingCloudGoal = nil } }), titleVisibility: .visible) {
            Button("Delete cloud goal", role: .destructive) {
                guard let goal = deletingCloudGoal else { return }
                Task { try? await cloud.delete(goal) }
                deletingCloudGoal = nil
            }
            Button(AppLocalized("Cancel"), role: .cancel) { deletingCloudGoal = nil }
        }
    }

    private var cloudGoalsSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 8) {
                Circle().fill(Color.blue).frame(width: 8, height: 8)
                Text("Cloud goals").font(.headline)
                Spacer()
                Button { Task { await cloud.refresh() } } label: { Image(systemName: "arrow.clockwise") }
                    .buttonStyle(.plain).accessibilityLabel(Text(AppLocalized("Refresh")))
                Button { editingCloudGoal = nil; showingCloudGoalEditor = true } label: { Image(systemName: "plus") }
                    .buttonStyle(.plain).accessibilityLabel(Text("Create cloud goal"))
                    .disabled(!authorization.signedIn)
            }
            if !authorization.signedIn {
                card { Text("Sign in to Morde to view cloud goals.").font(.subheadline).foregroundStyle(.secondary) }
            } else if let error = cloud.error {
                card {
                    VStack(alignment: .leading, spacing: 8) {
                        Label("Cloud goals could not be loaded: \(error)", systemImage: "exclamationmark.triangle")
                            .font(.footnote).foregroundStyle(.red)
                        Button("Retry") { Task { await cloud.refresh() } }
                    }
                }
            } else if cloud.isLoading && cloud.goals.isEmpty {
                card { ProgressView("Loading cloud goals…") }
            } else if cloud.goals.isEmpty {
                card { Text("No cloud goals yet.").font(.subheadline).foregroundStyle(.secondary) }
            } else {
                card {
                    VStack(spacing: 0) {
                        ForEach(cloud.goals) { goal in
                            cloudGoalRow(goal)
                            if goal.id != cloud.goals.last?.id { Divider().padding(.leading, 12) }
                        }
                    }
                }
            }
            if let pending = cloud.pending {
                Label(cloud.syncEnabled ? "A personal-data change is waiting to sync." : "Sync is paused; the pending change stays on this device.", systemImage: "clock")
                    .font(.footnote).foregroundStyle(.secondary)
                Button("Retry cloud goal sync") { Task { await cloud.retry() } }
                    .font(.footnote).disabled(!cloud.syncEnabled || cloud.isSaving || pending.scopeKey != authorization.personalDataScopeKey)
            }
            cloudSchedulesSection
        }
    }

    private func cloudGoalRow(_ goal: NanoMuseMordeGoalRecord) -> some View {
        HStack(spacing: 10) {
            Image(systemName: goal.body.status == "done" ? "checkmark.circle.fill" : "flag.circle")
                .foregroundStyle(goal.body.status == "done" ? Color.green : NanoMuseTones.action)
            VStack(alignment: .leading, spacing: 3) {
                Text(goal.body.title).font(.body.weight(.semibold))
                if !goal.body.description.isEmpty {
                    Text(goal.body.description).font(.footnote).foregroundStyle(.secondary).lineLimit(2)
                }
                Text("\(goal.progress)% · \(goal.body.status)").font(.caption).foregroundStyle(.secondary)
            }
            Spacer(minLength: 0)
            Menu {
                Button { editingCloudGoal = goal; showingCloudGoalEditor = true } label: { Label(AppLocalized("Edit"), systemImage: "pencil") }
                Button {
                    let body = NanoMuseMordeGoalMapper.body(title: goal.body.title, description: goal.body.description,
                        status: goal.body.status == "done" ? "active" : "done", steps: goal.body.steps,
                        relatedMaterialIDs: goal.body.related_material_ids)
                    Task { try? await cloud.save(body, id: goal.id, revision: goal.revision) }
                } label: { Label(goal.body.status == "done" ? "Reopen" : "Mark done", systemImage: "checkmark") }
                Button(role: .destructive) { deletingCloudGoal = goal } label: { Label("Delete", systemImage: "trash") }
            } label: { Image(systemName: "ellipsis").foregroundStyle(.secondary).frame(width: 28, height: 32) }
                .accessibilityLabel(Text(AppLocalized("More")))
        }
        .padding(.vertical, 10)
    }

    private var cloudSchedulesSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Text("Cloud schedules").font(.headline)
                Spacer()
                if cloud.isAuthorizingSchedules { ProgressView().controlSize(.small) }
                Toggle("Cloud schedule permission", isOn: Binding(
                    get: { authorization.scheduleAuthorizationState.enabled },
                    set: { enabled in
                        let scope = authorization.personalDataScopeKey
                        Task { await cloud.setScheduleAuthorization(enabled, for: scope) }
                    }
                ))
                .labelsHidden()
                .disabled(!authorization.signedIn || cloud.isAuthorizingSchedules)
                .accessibilityLabel(Text("Enable cloud schedules for up to 12 hours"))
                .accessibilityIdentifier("morde.schedule.permission")
            }
            card {
                VStack(alignment: .leading, spacing: 8) {
                    Text(authorization.scheduleAuthorizationState.enabled
                         ? "Cloud schedules have separate authorization. Revoking it does not cancel schedules or accepted tasks."
                         : "Enable the separate 12-hour permission to manage cloud schedules.")
                        .font(.footnote).foregroundStyle(.secondary)
                    if authorization.scheduleAuthorizationState.enabled {
                        if cloud.schedules.isEmpty {
                            Text("No cloud schedules.").font(.subheadline).foregroundStyle(.secondary)
                        }
                        ForEach(cloud.schedules) { schedule in
                            VStack(alignment: .leading, spacing: 6) {
                                HStack(alignment: .top, spacing: 8) {
                                    VStack(alignment: .leading, spacing: 2) {
                                        Text(schedule.objective).font(.subheadline.weight(.medium)).lineLimit(2)
                                        Text("\(schedule.status) · \(schedule.timing.kind)").font(.caption).foregroundStyle(.secondary)
                                    }
                                    Spacer()
                                    Toggle("Schedule enabled", isOn: Binding(
                                        get: { schedule.status == "enabled" },
                                        set: { _ in
                                            let scope = authorization.personalDataScopeKey
                                            Task { await cloud.toggle(schedule, for: scope) }
                                        }
                                    )).labelsHidden()
                                        .accessibilityIdentifier("morde.schedule.\(schedule.id).enabled")
                                }
                                Button(expandedScheduleRunIDs.contains(schedule.id) ? "隐藏运行记录" : "查看运行记录") {
                                    if expandedScheduleRunIDs.remove(schedule.id) == nil {
                                        expandedScheduleRunIDs.insert(schedule.id)
                                        if cloud.scheduleRuns[schedule.id] == nil {
                                            Task { await cloud.loadScheduleRuns(id: schedule.id) }
                                        }
                                    }
                                }
                                .font(.caption)
                                if expandedScheduleRunIDs.contains(schedule.id) {
                                    if let error = cloud.scheduleRunErrors[schedule.id] {
                                        Text("运行记录读取失败：\(error)").font(.caption).foregroundStyle(.red)
                                        Button("重试") { Task { await cloud.loadScheduleRuns(id: schedule.id) } }
                                            .font(.caption)
                                    } else if cloud.scheduleRunLoadingIDs.contains(schedule.id) {
                                        ProgressView("正在读取运行记录…").font(.caption)
                                    } else if let runs = cloud.scheduleRuns[schedule.id], runs.isEmpty {
                                        Text("此 Schedule 暂无运行记录。")
                                            .font(.caption).foregroundStyle(.secondary)
                                    } else if let runs = cloud.scheduleRuns[schedule.id] {
                                        ForEach(runs) { run in
                                            VStack(alignment: .leading, spacing: 2) {
                                                Text("\(run.status) · \(run.scheduled_at ?? run.created_at)")
                                                    .font(.caption)
                                                if let reason = run.skip_reason, !reason.isEmpty {
                                                    Text(reason).font(.caption2).foregroundStyle(.secondary)
                                                }
                                                if let taskID = run.task_id {
                                                    Text("Task \(taskID)").font(.caption2).foregroundStyle(.secondary)
                                                }
                                            }
                                            .frame(maxWidth: .infinity, alignment: .leading)
                                        }
                                        if let cursor = cloud.scheduleRunCursors[schedule.id], !cursor.isEmpty {
                                            Button("读取更多运行记录") {
                                                Task { await cloud.loadScheduleRuns(id: schedule.id, cursor: cursor) }
                                            }
                                            .font(.caption)
                                        }
                                    }
                                }
                            }
                        }
                    }
                    if let error = cloud.schedulesError {
                        Label(error, systemImage: "exclamationmark.triangle").font(.footnote).foregroundStyle(.red)
                        Button("Retry cloud schedules") { Task { await cloud.refreshSchedules() } }
                    }
                }
            }
        }
    }

    // MARK: Tracking

    private var trackingSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 8) {
                Circle().fill(Color.green).frame(width: 8, height: 8)
                Text(authorization.signedIn ? "On this iPhone" : AppLocalized("Tracking")).font(.headline)
                Spacer()
                Button { pickedCategory = .other } label: { Image(systemName: "plus").font(.body.weight(.semibold)) }
                    .buttonStyle(.plain)
                    .accessibilityLabel(Text(AppLocalized("Create a goal")))
            }
            if store.tracking.isEmpty && store.finished.isEmpty {
                card { Text(AppLocalized("Nothing tracked yet")).font(.subheadline).foregroundStyle(.secondary) }
            } else {
                card {
                    VStack(spacing: 0) {
                        ForEach(store.tracking) { goal in
                            goalRow(goal)
                            if goal.id != store.tracking.last?.id || !store.finished.isEmpty { Divider().padding(.leading, 44) }
                        }
                        ForEach(store.finished) { goal in
                            goalRow(goal)
                            if goal.id != store.finished.last?.id { Divider().padding(.leading, 44) }
                        }
                    }
                }
            }
        }
    }

    private func goalRow(_ goal: NanoMuseGoal) -> some View {
        HStack(alignment: .top, spacing: 12) {
            Button {
                NanoMuseGoalFlow.markDone(goal, goal.status != .done)
            } label: {
                Image(systemName: goal.status == .done ? "checkmark.circle.fill" : "circle")
                    .font(.system(size: 22))
                    .foregroundStyle(goal.status == .done ? Color.green : Color.secondary)
            }
            .buttonStyle(.plain)
            .accessibilityLabel(Text(goal.status == .done ? AppLocalized("Done") : AppLocalized("Mark as done")))
            VStack(alignment: .leading, spacing: 3) {
                Text(goal.title)
                    .font(.body.weight(.semibold))
                    .strikethrough(goal.status == .done)
                    .foregroundStyle(goal.status == .done ? .secondary : .primary)
                if !goal.why.isEmpty {
                    Text(goal.why).font(.footnote).foregroundStyle(.secondary).lineLimit(2)
                }
                HStack(spacing: 6) {
                    Text(statusLine(goal)).font(.caption).foregroundStyle(.secondary)
                    Spacer()
                    Text("\(goal.progress)%").font(.caption.weight(.semibold)).foregroundStyle(NanoMuseTones.action)
                }
                ProgressView(value: Double(goal.progress) / 100).tint(NanoMuseTones.action)
                if let note = goal.lastNote, !note.isEmpty {
                    Text(note).font(.caption).foregroundStyle(.secondary).lineLimit(2)
                }
            }
            Menu {
                if let sid = goal.sessionId {
                    Button { onOpenSession(sid) } label: { Label(AppLocalized("Open conversation"), systemImage: "bubble.left") }
                }
                if goal.status != .done {
                    if MordeLocalExecutionPolicy.isEnabled {
                        Button { NanoMuseGoalFlow.checkNow(goal) } label: { Label(AppLocalized("Check now"), systemImage: "arrow.clockwise") }
                    }
                    Button { NanoMuseGoalFlow.setPaused(goal, goal.status != .paused) } label: {
                        Label(goal.status == .paused ? AppLocalized("Resume checks") : AppLocalized("Pause checks"), systemImage: goal.status == .paused ? "play" : "pause")
                    }
                }
                Button(role: .destructive) { deleting = goal } label: { Label(AppLocalized("Delete goal"), systemImage: "trash") }
            } label: {
                Image(systemName: "ellipsis")
                    .font(.body)
                    .foregroundStyle(.secondary)
                    .frame(width: 28, height: 28)
                    .contentShape(Rectangle())
            }
            .accessibilityLabel(Text(AppLocalized("More")))
        }
        .padding(.vertical, 10)
        .contentShape(Rectangle())
        .onTapGesture { if let sid = goal.sessionId { onOpenSession(sid) } }
    }

    private func statusLine(_ goal: NanoMuseGoal) -> String {
        switch goal.status {
        case .paused: return AppLocalized("Paused")
        case .done: return AppLocalized("Done")
        case .active:
            if goal.lastCheckedAt == nil, goal.routineId != nil { return AppLocalized("First check in about a minute") }
            return NanoMuseGoalFlow.cadenceLabel(goal)
        }
    }

    // MARK: Routines

    private var routinesSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Text(authorization.signedIn ? "On this iPhone" : AppLocalized("Routines")).font(.headline)
                Spacer()
                Button { editingRoutine = NanoMuseRoutine(label: "", prompt: "", hour: 9, minute: 0) } label: { Image(systemName: "plus").font(.body.weight(.semibold)) }
                    .buttonStyle(.plain)
                    .accessibilityLabel(Text(AppLocalized("New routine")))
            }
            if authorization.signedIn {
                Text("Local plans stay on this iPhone. Import a plan explicitly to create a cloud schedule; local agent runs are unavailable in Morde.")
                    .font(.footnote).foregroundStyle(.secondary)
            }
            let visible = scheduler.visible
            if visible.isEmpty {
                card {
                    Text(AppLocalized("Nothing scheduled yet. A routine is something I do for you at a set time, every day or on weekdays. On the iPhone it runs when the app is open; at the set time the phone reminds you to open it."))
                        .font(.subheadline).foregroundStyle(.secondary)
                }
            } else {
                card {
                    VStack(spacing: 0) {
                        ForEach(visible) { routine in
                            routineRow(routine)
                            if routine.id != visible.last?.id { Divider().padding(.leading, 44) }
                        }
                    }
                }
            }
        }
    }

    private func routineRow(_ routine: NanoMuseRoutine) -> some View {
        HStack(spacing: 12) {
            Image(systemName: "clock")
                .font(.system(size: 20))
                .foregroundStyle(routine.enabled ? NanoMuseTones.action : Color.secondary)
                .frame(width: 24)
            VStack(alignment: .leading, spacing: 3) {
                Text(routine.label.isEmpty ? AppLocalized("Routine") : routine.label)
                    .font(.body.weight(.semibold))
                    .foregroundStyle(routine.enabled ? .primary : .secondary)
            Text(routine.isCloudArchive ? "Imported to cloud" : (routine.enabled ? routine.cadence : AppLocalized("Off")))
                    .font(.caption).foregroundStyle(.secondary)
                if let last = routine.runs.first {
                    Text(String(format: last.ok ? AppLocalized("last run %@") : AppLocalized("last run %@, failed"), NanoMuseDay.relative(last.at)))
                        .font(.caption2).foregroundStyle(.tertiary)
                }
            }
            Spacer()
            if routine.isCloudArchive {
                Image(systemName: "icloud")
                    .foregroundStyle(.secondary)
                    .accessibilityLabel(Text("Managed in Cloud schedules"))
            } else if scheduler.running.contains(routine.id) {
                ProgressView()
            } else {
                Toggle("", isOn: Binding(get: { routine.enabled }, set: { scheduler.setEnabled(routine.id, $0) }))
                    .labelsHidden()
                    .tint(NanoMuseTones.action)
                    .accessibilityLabel(Text(routine.label.isEmpty ? AppLocalized("Routine") : routine.label))
            }
            Menu {
                if routine.isCloudArchive {
                    Label("Imported to cloud", systemImage: "checkmark.icloud")
                } else {
                    Button { editingRoutine = routine } label: { Label(AppLocalized("Edit"), systemImage: "pencil") }
                }
                if MordeLocalExecutionPolicy.isEnabled {
                    Button { Task { @MainActor in await scheduler.run(routine.id) } } label: { Label(AppLocalized("Run now"), systemImage: "play") }
                }
                if let sid = routine.sessionId {
                    Button { onOpenSession(sid) } label: { Label(AppLocalized("Open conversation"), systemImage: "bubble.left") }
                }
                if !routine.isCloudArchive {
                    Button(role: .destructive) { scheduler.delete(routine.id) } label: { Label(AppLocalized("Delete"), systemImage: "trash") }
                }
            } label: {
                Image(systemName: "ellipsis")
                    .font(.body)
                    .foregroundStyle(.secondary)
                    .frame(width: 28, height: 28)
                    .contentShape(Rectangle())
            }
            .accessibilityLabel(Text(AppLocalized("More")))
            .accessibilityIdentifier("morde.routine.\(routine.label).more")
        }
        .padding(.vertical, 10)
    }

    // MARK: Create

    private var createSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text(AppLocalized("Create a goal")).font(.headline)
            Text(AppLocalized("Pick a category and tell me the goal you have in mind. I'll shape a plan with you and keep improving it as you go."))
                .font(.subheadline)
                .foregroundStyle(.secondary)
            card {
                VStack(spacing: 0) {
                    ForEach(NanoMuseGoalCategory.allCases) { category in
                        Button { pickedCategory = category } label: {
                            HStack(spacing: 12) {
                                Image(systemName: category.symbol)
                                    .font(.system(size: 18))
                                    .foregroundStyle(NanoMuseTones.action)
                                    .frame(width: 24)
                                Text(category.label).font(.body).foregroundStyle(.primary)
                                Spacer()
                                Image(systemName: "chevron.right").font(.footnote).foregroundStyle(.tertiary)
                            }
                            .padding(.vertical, 12)
                            .contentShape(Rectangle())
                        }
                        .buttonStyle(.plain)
                        if category != NanoMuseGoalCategory.allCases.last { Divider().padding(.leading, 36) }
                    }
                }
            }
        }
    }

    private func card<Content: View>(@ViewBuilder _ content: () -> Content) -> some View {
        content()
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, 14)
            .padding(.vertical, 4)
            .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 16, style: .continuous))
            .overlay(RoundedRectangle(cornerRadius: 16, style: .continuous).strokeBorder(NanoMuseTones.hairline))
    }
}

struct NanoMuseMordeGoalEditor: View {
    let goal: NanoMuseMordeGoalRecord?
    let onSave: (MordePersonalGoalBody) async throws -> Void

    @Environment(\.dismiss) private var dismiss
    @State private var title: String
    @State private var description: String
    @State private var status: String
    @State private var stepText: String
    @State private var isSaving = false
    @State private var error: String?

    init(goal: NanoMuseMordeGoalRecord?, onSave: @escaping (MordePersonalGoalBody) async throws -> Void) {
        self.goal = goal
        self.onSave = onSave
        _title = State(initialValue: goal?.body.title ?? "")
        _description = State(initialValue: goal?.body.description ?? "")
        _status = State(initialValue: goal?.body.status ?? "active")
        _stepText = State(initialValue: goal?.body.steps.map(\.title).joined(separator: "\n") ?? "")
    }

    var body: some View {
        NavigationStack {
            Form {
                Section("Goal") {
                    TextField("Title", text: $title)
                        .accessibilityIdentifier("morde.goal.title")
                    TextField("Description", text: $description, axis: .vertical)
                        .lineLimit(2...5)
                    Picker("Status", selection: $status) {
                        Text("Active").tag("active")
                        Text("Paused").tag("paused")
                        Text("Done").tag("done")
                        Text("Cancelled").tag("cancelled")
                    }
                }
                Section("Steps · one per line") {
                    TextEditor(text: $stepText).frame(minHeight: 120)
                }
                if let error {
                    Section { Text(error).foregroundStyle(.red) }
                }
            }
            .navigationTitle(goal == nil ? "New cloud goal" : "Edit cloud goal")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button(AppLocalized("Cancel")) { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button(AppLocalized("Save")) { save() }
                        .disabled(isSaving || title.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                }
            }
            .overlay { if isSaving { ProgressView().padding().background(.regularMaterial, in: Capsule()) } }
        }
    }

    private func save() {
        let lines = stepText.components(separatedBy: .newlines).map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }
            .filter { !$0.isEmpty }
        var oldSteps = goal?.body.steps ?? []
        let steps = lines.map { line -> MordePersonalGoalStep in
            if let index = oldSteps.firstIndex(where: { $0.title == line }) {
                let old = oldSteps.remove(at: index)
                return MordePersonalGoalStep(id: old.id, title: line, status: old.status, note: old.note)
            }
            return MordePersonalGoalStep(id: "s_\(UUID().uuidString.lowercased().replacingOccurrences(of: "-", with: ""))",
                                         title: line, status: "pending")
        }
        let body = NanoMuseMordeGoalMapper.body(title: title, description: description, status: status,
            steps: steps, relatedMaterialIDs: goal?.body.related_material_ids ?? [])
        guard body.isValidForWrite else { error = "The goal is too long or contains too many steps."; return }
        isSaving = true
        error = nil
        Task {
            do { try await onSave(body); dismiss() }
            catch { self.error = error.localizedDescription }
            isSaving = false
        }
    }
}

struct NanoMuseScheduleImportSheet: View {
    let routine: NanoMuseRoutine
    let onImported: () -> Void

    @Environment(\.dismiss) private var dismiss
    @ObservedObject private var authorization = MordeShellAuthorization.shared
    @State private var targets: MordeScheduleTargets?
    @State private var hostID = ""
    @State private var modelID = ""
    @State private var isLoading = true
    @State private var isSaving = false
    @State private var error: String?

    private var preview: NanoMuseScheduleImportPreview {
        NanoMuseScheduleImportMapper.preview(routine, hostID: hostID, modelID: modelID)
    }

    var body: some View {
        NavigationStack {
            Form {
                Section("Local plan") {
                    LabeledContent("Name", value: routine.label.isEmpty ? "Routine" : routine.label)
                    LabeledContent("Timing", value: routine.cadence)
                    Text(routine.prompt.isEmpty ? "This plan has no instructions to send." : routine.prompt)
                        .font(.footnote).foregroundStyle(.secondary)
                    Text("The local plan is turned off only after the cloud confirms creation. Its source remains on this iPhone.")
                        .font(.footnote).foregroundStyle(.secondary)
                }
                Section("Cloud authorization") {
                    if authorization.scheduleAuthorizationState.enabled {
                        Text("Separate cloud schedule permission is active. It expires within 12 hours.")
                            .font(.footnote).foregroundStyle(.secondary)
                    } else {
                        Text("Turn on Cloud schedules in Goals before creating this schedule.")
                            .font(.footnote).foregroundStyle(.orange)
                    }
                }
                if isLoading {
                    Section { ProgressView("Loading available Hosts and models…") }
                } else if let targets {
                    Section("Cloud target") {
                        Picker("Host", selection: $hostID) {
                            ForEach(targets.hosts) { Text($0.name).tag($0.id) }
                        }
                        Picker("Model", selection: $modelID) {
                            ForEach(targets.models) { Text($0.name).tag($0.id) }
                        }
                    }
                    if let reason = preview.unavailableReason {
                        Section { Text(reason).font(.footnote).foregroundStyle(.orange) }
                    } else if let request = preview.request {
                        Section("Preview") {
                            Text(request.objective).font(.footnote)
                            LabeledContent("Schedule", value: "\(request.timing.kind) · \(request.timing.local_time ?? request.timing.run_at ?? "")")
                        }
                    }
                } else {
                    Section {
                        Text(error ?? "No available cloud targets.").font(.footnote).foregroundStyle(.red)
                        Button("Retry") { Task { await loadTargets() } }
                    }
                }
                if let error { Section { Text(error).font(.footnote).foregroundStyle(.red) } }
            }
            .navigationTitle("Import to cloud")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button(AppLocalized("Cancel")) { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Create cloud schedule") { create() }
                        .disabled(!authorization.scheduleAuthorizationState.enabled || !preview.isAvailable || isSaving || isLoading)
                        .accessibilityIdentifier("morde.schedule.import.create")
                }
            }
            .overlay { if isSaving { ProgressView().padding().background(.regularMaterial, in: Capsule()) } }
        }
        .task { await loadTargets() }
    }

    @MainActor
    private func loadTargets() async {
        isLoading = true
        do {
            let value = try await MordeShellAuthorization.shared.scheduleTargets()
            targets = value
            if hostID.isEmpty { hostID = value.hosts.first?.id ?? "" }
            if modelID.isEmpty { modelID = value.models.first?.id ?? "" }
            error = nil
        } catch { self.error = error.localizedDescription }
        isLoading = false
    }

    private func create() {
        guard preview.isAvailable else { return }
        isSaving = true
        error = nil
        Task { @MainActor in
            do {
                _ = try await NanoMuseScheduler.shared.importRoutineToCloud(routine.id, hostID: hostID, modelID: modelID)
                onImported()
                dismiss()
            } catch { self.error = error.localizedDescription }
            isSaving = false
        }
    }
}

/// "Create a Health goal" — what happens next, and the button that starts it.
struct NanoMuseGoalCategorySheet: View {
    var category: NanoMuseGoalCategory
    var onGo: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack(spacing: 12) {
                Image(systemName: category.symbol).font(.system(size: 28)).foregroundStyle(NanoMuseTones.action)
                Text(String(format: AppLocalized("Create a %@ goal"), category.label)).font(.title3.weight(.bold))
            }
            Text(AppLocalized("First, we'll shape the goal together in the chat. I'll ask a few questions so I understand exactly what you're after."))
                .font(.body)
            Text(AppLocalized("Once it's set, I'll track your progress here. The check-ins run when the app is open; at check time the phone reminds you to open it."))
                .font(.body).foregroundStyle(.secondary)
            Spacer(minLength: 8)
            Button(action: onGo) {
                Text(AppLocalized("Start"))
                    .font(.body.weight(.semibold))
                    .frame(maxWidth: .infinity)
                    .padding(.vertical, 12)
            }
            .buttonStyle(.borderedProminent)
            .tint(NanoMuseTones.action)
        }
        .padding(22)
    }
}

// MARK: - Routine editor

struct NanoMuseRoutineEditor: View {
    @State var routine: NanoMuseRoutine
    @Environment(\.dismiss) private var dismiss
    @State private var time: Date = Date()

    private var isNew: Bool { NanoMuseScheduler.shared.routine(id: routine.id) == nil }
    private var isCloudArchive: Bool { routine.isCloudArchive }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    if isCloudArchive {
                        Label("Imported routines are read-only here. Manage the cloud schedule in Cloud schedules.",
                              systemImage: "icloud")
                            .font(.footnote).foregroundStyle(.secondary)
                    }
                    TextField(AppLocalized("Name"), text: $routine.label)
                        .accessibilityIdentifier("morde.routine.name")
                        .disabled(isCloudArchive)
                    DatePicker(AppLocalized("Time"), selection: $time, displayedComponents: .hourAndMinute)
                        .disabled(isCloudArchive)
                    Picker(AppLocalized("Repeat"), selection: $routine.repeatMode) {
                        Text(AppLocalized("Every day")).tag(NanoMuseRoutine.Repeat.daily)
                        Text(AppLocalized("Weekdays")).tag(NanoMuseRoutine.Repeat.weekdays)
                        Text(AppLocalized("Once")).tag(NanoMuseRoutine.Repeat.once)
                    }
                    .disabled(isCloudArchive)
                    Toggle(AppLocalized("On"), isOn: $routine.enabled)
                        .tint(NanoMuseTones.action)
                        .disabled(isCloudArchive)
                }
                Section {
                    TextEditor(text: $routine.prompt)
                        .frame(minHeight: 120)
                        .accessibilityIdentifier("morde.routine.prompt")
                        .disabled(isCloudArchive)
                } header: {
                    Text(AppLocalized("What to do"))
                } footer: {
                    Text(AppLocalized("Sent to the agent as a message at the set time, in its own conversation. On the iPhone the run happens when the app is open; at the set time you get a reminder to open it. Nothing runs while the app is asleep."))
                }
                if !routine.runs.isEmpty {
                    Section(AppLocalized("Run records")) {
                        ForEach(Array(routine.runs.enumerated()), id: \.offset) { _, run in
                            HStack {
                                Image(systemName: run.ok ? "checkmark.circle" : "exclamationmark.circle")
                                    .foregroundStyle(run.ok ? Color.green : Color.orange)
                                VStack(alignment: .leading, spacing: 2) {
                                    Text(run.at.formatted(date: .abbreviated, time: .shortened)).font(.subheadline)
                                    if let note = run.note, !note.isEmpty { Text(note).font(.caption).foregroundStyle(.secondary) }
                                }
                            }
                        }
                    }
                }
            }
            .navigationTitle(isNew ? AppLocalized("New routine") : AppLocalized("Edit routine"))
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button(AppLocalized("Cancel")) { dismiss() } }
                if !isCloudArchive {
                    ToolbarItem(placement: .confirmationAction) {
                        Button(AppLocalized("Save")) { save() }
                            .disabled(routine.prompt.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                    }
                }
            }
            .onAppear {
                var c = DateComponents()
                c.hour = routine.hour
                c.minute = routine.minute
                time = Calendar.current.date(from: c) ?? Date()
            }
        }
    }

    private func save() {
        let c = Calendar.current.dateComponents([.hour, .minute], from: time)
        var r = routine
        r.hour = c.hour ?? 9
        r.minute = c.minute ?? 0
        if r.label.trimmingCharacters(in: .whitespaces).isEmpty { r.label = String(r.prompt.prefix(40)) }
        if isNew {
            guard NanoMuseScheduler.shared.create(r) != nil else { return }
        } else {
            guard NanoMuseScheduler.shared.update(r) else { return }
        }
        NanoMuseScheduler.shared.requestNotificationPermission()
        dismiss()
    }
}
