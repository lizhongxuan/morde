//
//  NanoMuseScheduler.swift
//  nanoMuse
//
//  The iPhone's scheduler primitive: a due-list of routines (the daily
//  feed, goal check-ins, the routines made from Ideas) kept in a JSON file,
//  run by the app itself when it is in the foreground — on open, and while
//  it stays active — with a BGAppRefreshTask and local notifications for
//  the hours it is not. iOS gives an app no way to run a model turn on a
//  schedule while it sleeps: when a routine comes due and the app is not
//  up, the phone shows "Check-in: <goal> — open to run it", and the run
//  happens when the person opens the app. Android has WorkManager for this
//  (io.github.nanomuse.app.scheduled.*); the words here say what the phone can
//  and cannot do.
//

import Foundation
import BackgroundTasks
import UserNotifications
import UIKit
import CryptoKit

// MARK: - Cloud schedule wire models

/// Codable values shared with MordeShellAuthorization's typed API bridge. The
/// bridge owns credentials and only exposes these fixed request/response DTOs.
typealias NanoMuseScheduleTimingDTO = MordeScheduleTimingDTO
typealias NanoMuseScheduleBudgetDTO = MordeScheduleBudgetDTO
typealias NanoMuseScheduleObservationDTO = MordeScheduleObservationDTO
typealias NanoMuseScheduleRunDTO = MordeScheduleRunDTO
typealias NanoMuseScheduleDTO = MordeScheduleDTO
typealias NanoMuseScheduleListPage = MordeScheduleListPage
typealias NanoMuseScheduleRunListPage = MordeScheduleRunListPage
typealias NanoMuseScheduleCreateRequest = MordeScheduleCreateRequest
typealias NanoMuseScheduleUpdateRequest = MordeScheduleUpdateRequest
typealias NanoMuseScheduleActionRequest = MordeScheduleActionRequest
typealias NanoMuseScheduleActionResponse = MordeScheduleActionResponse

struct NanoMuseScheduleImportPreview: Equatable {
    let request: NanoMuseScheduleCreateRequest?
    let idempotencyKey: String?
    let unavailableReason: String?

    var isAvailable: Bool { request != nil && idempotencyKey != nil }
}

/// Converts a selected, preserved local routine into the cloud Schedule
/// contract. Unsupported cadence is reported to the preview instead of being
/// approximated, so importing can never silently change its timing.
@MainActor
enum NanoMuseScheduleImportMapper {
    static func preview(_ routine: NanoMuseRoutine, hostID: String, modelID: String,
                        now: Date = Date(), timeZone: TimeZone = .current) -> NanoMuseScheduleImportPreview {
        guard !hostID.isEmpty, !modelID.isEmpty else {
            return unavailable("Choose an available Host and model.")
        }
        if let hours = routine.intervalHours, hours > 0 {
            return unavailable("Every-\(hours)-hour plans are not supported by cloud schedules yet. The local plan is unchanged.")
        }
        let objective = objective(for: routine)
        guard !objective.isEmpty, objective.utf8.count <= 32_768 else {
            return unavailable(objective.isEmpty ? "This plan has no task instructions." : "Task instructions exceed the cloud schedule limit.")
        }

        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = timeZone
        let timeParts = NanoMuseDay.clock(hour: routine.hour, minute: routine.minute)
        let timeZoneName = timeZone.identifier
        let timing: NanoMuseScheduleTimingDTO
        switch routine.repeatMode {
        case .once:
            guard let next = routine.nextDue(after: now, calendar: calendar), next > now else {
                return unavailable("This one-time plan has already run or has no future run.")
            }
            let formatter = ISO8601DateFormatter()
            formatter.timeZone = TimeZone(secondsFromGMT: 0)
            timing = NanoMuseScheduleTimingDTO(kind: "once", run_at: formatter.string(from: next),
                                                time_zone: timeZoneName)
        case .daily:
            timing = NanoMuseScheduleTimingDTO(kind: "daily", starts_on: dateOnly(now, calendar: calendar),
                                                local_time: timeParts, time_zone: timeZoneName)
        case .weekdays:
            timing = NanoMuseScheduleTimingDTO(kind: "weekly", starts_on: dateOnly(now, calendar: calendar),
                                                local_time: timeParts, time_zone: timeZoneName,
                                                weekdays: [1, 2, 3, 4, 5])
        }
        let request = NanoMuseScheduleCreateRequest(
            host_id: hostID,
            model_id: modelID,
            objective: objective,
            timing: timing,
            budget: NanoMuseScheduleBudgetDTO(active_seconds: 300, tool_calls: 10, model_tokens: 8_000),
            misfire_policy: "skip",
            notification_events: ["completed", "failed", "waiting_user"],
            observation: nil
        )
        return NanoMuseScheduleImportPreview(request: request, idempotencyKey: idempotencyKey(routine),
                                             unavailableReason: nil)
    }

    private static func unavailable(_ reason: String) -> NanoMuseScheduleImportPreview {
        NanoMuseScheduleImportPreview(request: nil, idempotencyKey: nil, unavailableReason: reason)
    }

    private static func dateOnly(_ date: Date, calendar: Calendar) -> String {
        let formatter = DateFormatter()
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.calendar = calendar
        formatter.timeZone = calendar.timeZone
        formatter.dateFormat = "yyyy-MM-dd"
        return formatter.string(from: date)
    }

    private static func objective(for routine: NanoMuseRoutine) -> String {
        if !routine.prompt.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
            return routine.prompt.trimmingCharacters(in: .whitespacesAndNewlines)
        }
        guard let goalID = routine.goalId, let goal = NanoMuseGoalStore.shared.goal(id: goalID) else { return "" }
        var lines = ["Check in on the goal: \(goal.title)"]
        if !goal.why.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty { lines.append("Why: \(goal.why)") }
        if !goal.steps.isEmpty { lines.append("Steps:\n" + goal.steps.map { "- \($0.text)" }.joined(separator: "\n")) }
        return lines.joined(separator: "\n")
    }

    private static func idempotencyKey(_ routine: NanoMuseRoutine) -> String {
        // One key per preserved local source prevents duplicate cloud schedules
        // when a create response is lost and the user changes the destination.
        let digest = SHA256.hash(data: Data(routine.id.utf8)).map { String(format: "%02x", $0) }.joined()
        return "ios-schedule-import-" + String(digest.prefix(32))
    }
}

// MARK: - Model

struct NanoMuseRoutine: Codable, Identifiable, Equatable {
    enum Repeat: String, Codable, CaseIterable { case once, daily, weekdays }

    struct Run: Codable, Equatable {
        var at: Date
        var ok: Bool
        var note: String?
    }

    var id: String = UUID().uuidString
    var label: String
    var prompt: String
    var hour: Int
    var minute: Int
    var repeatMode: Repeat = .daily
    /// Goal checks run every N hours instead of at a time of day.
    var intervalHours: Int? = nil
    var enabled: Bool = true
    /// Hidden routines (goal check-ins, the feed) do not show under Routines.
    var hidden: Bool = false
    var goalId: String? = nil
    /// The cloud schedule created from this preserved local source, if imported.
    var cloudScheduleID: String? = nil
    var cloudScheduleVersion: Int64? = nil
    var isCloudArchive: Bool { cloudScheduleID != nil }
    /// The conversation the runs land in; nil = a fresh side chat per run.
    var sessionId: String? = nil
    var createdAt: Date = Date()
    var lastFiredAt: Date? = nil
    var runs: [Run] = []

    var lastOk: Bool? { runs.first?.ok }

    /// "Checks every 6 hours" / "Daily 08:00" / "Weekdays 07:30" / "Once 18:00".
    var cadence: String {
        if let h = intervalHours, h > 0 {
            return h == 1 ? AppLocalized("Checks every hour") : String(format: AppLocalized("Checks every %d hours"), h)
        }
        let time = NanoMuseDay.clock(hour: hour, minute: minute)
        switch repeatMode {
        case .once: return String(format: AppLocalized("Once · %@"), time)
        case .daily: return String(format: AppLocalized("Daily · %@"), time)
        case .weekdays: return String(format: AppLocalized("Weekdays · %@"), time)
        }
    }

    // MARK: Due logic (pure, tested)

    /// The next moment this routine should run after `after`, or nil (a spent
    /// one-off, a disabled routine).
    func nextDue(after: Date, calendar: Calendar = .current) -> Date? {
        guard enabled else { return nil }
        if let hours = intervalHours, hours > 0 {
            // First check about a minute after creation, then every N hours.
            // Overdue reads as due now (the anchor is in the past); otherwise it is the next slot.
            return lastFiredAt.map { $0.addingTimeInterval(TimeInterval(hours) * 3600) }
                ?? createdAt.addingTimeInterval(60)
        }
        var components = calendar.dateComponents([.year, .month, .day], from: after)
        components.hour = hour
        components.minute = minute
        components.second = 0
        guard var candidate = calendar.date(from: components) else { return nil }
        // Already fired for this slot, or the slot is in the past → the next day that qualifies.
        var guardCount = 0
        while guardCount < 10 {
            guardCount += 1
            let firedThisSlot = lastFiredAt.map { $0 >= candidate } ?? false
            // A slot before the routine existed is not a missed one.
            let afterCreation = candidate > createdAt
            let qualifies: Bool = {
                switch repeatMode {
                case .once:
                    return lastFiredAt == nil
                case .daily:
                    return true
                case .weekdays:
                    let weekday = calendar.component(.weekday, from: candidate)
                    return weekday >= 2 && weekday <= 6
                }
            }()
            // In the future → the next run; already passed and never run → missed, due now.
            if qualifies && !firedThisSlot && afterCreation { return candidate }
            // A one-off that has run is spent; one made after today's time runs tomorrow.
            if repeatMode == .once && lastFiredAt != nil { return nil }
            guard let next = calendar.date(byAdding: .day, value: 1, to: candidate) else { return nil }
            candidate = next
        }
        return nil
    }

    /// True when the routine should run at `now` — its slot has passed and has not been run.
    func isDue(at now: Date, calendar: Calendar = .current) -> Bool {
        guard let due = nextDue(after: now, calendar: calendar) else { return false }
        return due <= now
    }
}

/// Pure local mutation rules for routines. Imported schedules keep a local
/// source copy for recovery, but local routine controls must not change it.
enum NanoMuseRoutineLocalMutationPolicy {
    static func disablingCloudArchives(in routines: [NanoMuseRoutine]) -> [NanoMuseRoutine] {
        routines.map { routine in
            guard routine.isCloudArchive, routine.enabled else { return routine }
            var disabled = routine
            disabled.enabled = false
            return disabled
        }
    }

    static func adding(_ routine: NanoMuseRoutine, to routines: [NanoMuseRoutine]) -> [NanoMuseRoutine]? {
        guard !routine.isCloudArchive, !routines.contains(where: { $0.id == routine.id }) else { return nil }
        return routines + [routine]
    }

    static func replacing(_ routine: NanoMuseRoutine, in routines: [NanoMuseRoutine]) -> [NanoMuseRoutine]? {
        guard let index = routines.firstIndex(where: { $0.id == routine.id }),
              !routines[index].isCloudArchive, !routine.isCloudArchive else { return nil }
        var updated = routines
        updated[index] = routine
        return updated
    }

    static func settingEnabled(_ id: String, to enabled: Bool,
                               in routines: [NanoMuseRoutine]) -> [NanoMuseRoutine]? {
        guard let index = routines.firstIndex(where: { $0.id == id }),
              !routines[index].isCloudArchive else { return nil }
        var updated = routines
        updated[index].enabled = enabled
        return updated
    }

    static func deleting(_ id: String, from routines: [NanoMuseRoutine]) -> [NanoMuseRoutine]? {
        guard let index = routines.firstIndex(where: { $0.id == id }),
              !routines[index].isCloudArchive else { return nil }
        var updated = routines
        updated.remove(at: index)
        return updated
    }
}

// MARK: - Store + runner

@MainActor
final class NanoMuseScheduler: ObservableObject {
    static let shared = NanoMuseScheduler()

    /// Also listed under BGTaskSchedulerPermittedIdentifiers in Info.plist.
    nonisolated static let backgroundTaskId = "io.github.nanomuse.app.scheduler"
    static let notificationCategory = "NANOMUSE_ROUTINE"
    private static let maxRunsKept = 10

    @Published private(set) var routines: [NanoMuseRoutine] = []
    /// Ids being run right now.
    @Published private(set) var running: Set<String> = []

    private var file: URL { NanoMuseDirs.root.appendingPathComponent("routines.json") }
    private var catchUpTask: Task<Void, Never>?
    private var notificationsTask: Task<Void, Never>?
    private var tickTimer: Timer?

    private init() {
        load()
    }

    // MARK: Persistence

    private func load() {
        guard let data = try? Data(contentsOf: file) else { return }
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        let loaded = (try? decoder.decode([NanoMuseRoutine].self, from: data)) ?? []
        let normalized = NanoMuseRoutineLocalMutationPolicy.disablingCloudArchives(in: loaded)
        routines = normalized
        if loaded != normalized { _ = persist(normalized) }
    }

    /// C12: the file changed under the store — another account's routines are in place (or
    /// none). The list and the notifications follow.
    func reload() {
        // a catch-up under way was the account's that left: the routines still in its list
        // are not started (a run already talking to the model finishes in its own session)
        catchUpTask?.cancel()
        catchUpTask = nil
        routines = []
        load()
        refreshNotifications()
    }

    @discardableResult
    private func persist(_ updatedRoutines: [NanoMuseRoutine]) -> Bool {
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        guard let data = try? encoder.encode(updatedRoutines),
              (try? data.write(to: file, options: .atomic)) != nil else { return false }
        routines = updatedRoutines
        refreshNotifications()
        return true
    }

    // MARK: CRUD

    var visible: [NanoMuseRoutine] {
        routines.filter { !$0.hidden }.sorted { ($0.hour * 60 + $0.minute) < ($1.hour * 60 + $1.minute) }
    }

    func routine(id: String) -> NanoMuseRoutine? { routines.first { $0.id == id } }

    @discardableResult
    func create(_ routine: NanoMuseRoutine) -> NanoMuseRoutine? {
        guard let updated = NanoMuseRoutineLocalMutationPolicy.adding(routine, to: routines),
              persist(updated) else { return nil }
        return routine
    }

    @discardableResult
    func update(_ routine: NanoMuseRoutine) -> Bool {
        guard let updated = NanoMuseRoutineLocalMutationPolicy.replacing(routine, in: routines) else { return false }
        return persist(updated)
    }

    /// Only the cloud-import flow may attach or refresh the remote receipt.
    /// All local CRUD entry points reject linked sources.
    private func recordCloudScheduleReceipt(id: String, scheduleID: String, version: Int64) -> Bool {
        guard !scheduleID.isEmpty, version > 0,
              let index = routines.firstIndex(where: { $0.id == id }),
              routines[index].cloudScheduleID == nil || routines[index].cloudScheduleID == scheduleID else { return false }
        var imported = routines[index]
        imported.cloudScheduleID = scheduleID
        imported.cloudScheduleVersion = version
        imported.enabled = false
        var updated = routines
        updated[index] = imported
        return persist(updated)
    }

    /// Creates the Schedule first. The local plan is only disabled after the
    /// server returns its confirmed ID and version; its original instructions
    /// and cadence remain in routines.json for recovery.
    func importRoutineToCloud(_ id: String, hostID: String, modelID: String) async throws -> NanoMuseScheduleDTO {
        guard let original = routine(id: id) else { throw NanoMuseScheduleImportError.sourceMissing }
        if let cloudID = original.cloudScheduleID {
            let confirmed = try await MordeShellAuthorization.shared.schedule(id: cloudID)
            guard confirmed.id == cloudID else { throw NanoMuseScheduleImportError.invalidServerReceipt }
            // A previous create may have reached the server while the local
            // receipt write failed. Recheck the cloud copy, then retry the
            // local receipt and disable operation before returning success.
            guard recordCloudScheduleReceipt(id: id, scheduleID: confirmed.id, version: confirmed.version) else {
                throw NanoMuseScheduleImportError.localSourceSaveFailed
            }
            return confirmed
        }
        let preview = NanoMuseScheduleImportMapper.preview(original, hostID: hostID, modelID: modelID)
        guard let request = preview.request, let idempotencyKey = preview.idempotencyKey else {
            throw NanoMuseScheduleImportError.unavailable(preview.unavailableReason ?? "This plan cannot be imported.")
        }
        let created = try await MordeShellAuthorization.shared.createSchedule(request, idempotencyKey: idempotencyKey)
        guard !created.id.isEmpty, created.version > 0 else { throw NanoMuseScheduleImportError.invalidServerReceipt }
        guard recordCloudScheduleReceipt(id: id, scheduleID: created.id, version: created.version) else {
            throw NanoMuseScheduleImportError.localSourceSaveFailed
        }
        return created
    }

    @discardableResult
    func setEnabled(_ id: String, _ enabled: Bool) -> Bool {
        guard let updated = NanoMuseRoutineLocalMutationPolicy.settingEnabled(id, to: enabled, in: routines) else { return false }
        return persist(updated)
    }

    @discardableResult
    func delete(_ id: String) -> Bool {
        guard let updated = NanoMuseRoutineLocalMutationPolicy.deleting(id, from: routines) else { return false }
        return persist(updated)
    }

    /// Routines that belong to a goal.
    func forGoal(_ goalId: String) -> [NanoMuseRoutine] { routines.filter { $0.goalId == goalId } }

    // MARK: Lifecycle hooks (MinisApp)

    /// Call from the app's init: BGTaskScheduler wants the handler before launch finishes.
    nonisolated static func registerBackgroundTask() {
        BGTaskScheduler.shared.register(forTaskWithIdentifier: Self.backgroundTaskId, using: nil) { task in
            guard let refresh = task as? BGAppRefreshTask else { task.setTaskCompleted(success: false); return }
            Task { @MainActor in
                NanoMuseScheduler.shared.handleBackgroundRefresh(refresh)
            }
        }
    }

    /// The app is on screen: run what is due, and keep checking every minute while it stays up.
    func appBecameActive() {
        guard MordeLocalExecutionPolicy.isEnabled else {
            tickTimer?.invalidate()
            tickTimer = nil
            catchUpTask?.cancel()
            catchUpTask = nil
            return
        }
        catchUp(reason: "active")
        tickTimer?.invalidate()
        tickTimer = Timer.scheduledTimer(withTimeInterval: 60, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.catchUp(reason: "tick") }
        }
        BGTaskScheduler.shared.cancel(taskRequestWithIdentifier: Self.backgroundTaskId)
    }

    /// The app is leaving the screen: ask for a refresh around the next due time, and make sure the reminders are set.
    func appWentBackground() {
        tickTimer?.invalidate()
        tickTimer = nil
        guard MordeLocalExecutionPolicy.isEnabled else {
            catchUpTask?.cancel()
            catchUpTask = nil
            return
        }
        scheduleBackgroundRefresh()
        refreshNotifications()
    }

    /// The next due moment across all routines.
    func nextDue(after: Date = Date()) -> (routine: NanoMuseRoutine, at: Date)? {
        routines.compactMap { r in
            guard !r.isCloudArchive else { return nil }
            return r.nextDue(after: after).map { (r, $0) }
        }.min { $0.1 < $1.1 }
    }

    func scheduleBackgroundRefresh() {
        guard let next = nextDue() else { return }
        let request = BGAppRefreshTaskRequest(identifier: Self.backgroundTaskId)
        request.earliestBeginDate = max(next.at, Date().addingTimeInterval(15 * 60))
        try? BGTaskScheduler.shared.submit(request)
    }

    /// iOS woke us for a moment: there is no time for a model turn, so the
    /// routines that came due get a notification each, and the next refresh is asked for.
    private func handleBackgroundRefresh(_ task: BGAppRefreshTask) {
        guard MordeLocalExecutionPolicy.isEnabled else {
            task.setTaskCompleted(success: true)
            return
        }
        task.expirationHandler = { }
        let now = Date()
        for r in routines where !r.isCloudArchive && r.isDue(at: now) {
            postDueNotification(r, at: now)
        }
        scheduleBackgroundRefresh()
        task.setTaskCompleted(success: true)
    }

    // MARK: Running

    /// Run every routine that is due, one after the other.
    func catchUp(reason: String) {
        guard MordeLocalExecutionPolicy.isEnabled, catchUpTask == nil else { return }
        let now = Date()
        let due = routines.filter { !$0.isCloudArchive && $0.isDue(at: now) && !running.contains($0.id) }
        guard !due.isEmpty else { return }
        let task = Task { @MainActor [self] in
            for r in due {
                if Task.isCancelled { break }
                _ = await run(r.id, trigger: reason)
            }
        }
        catchUpTask = task
        Task { @MainActor [self] in
            await task.value
            if catchUpTask == task { catchUpTask = nil }
        }
    }

    /// Run one routine now (from the UI or the catch-up). Returns whether the turn finished without an error.
    @discardableResult
    func run(_ id: String, trigger: String = "manual") async -> Bool {
        guard MordeLocalExecutionPolicy.isEnabled, var r = routine(id: id), !r.isCloudArchive,
              !running.contains(id) else { return false }
        running.insert(id)
        defer { running.remove(id) }
        // Mark the slot as taken first, so a crash mid-run does not re-run it on the next open.
        let startedAt = Date()
        r.lastFiredAt = startedAt
        update(r)

        let outcome = await NanoMuseHeadless.run(prompt: prompt(for: r), sessionId: r.sessionId, title: r.label, source: "nanomuse-routine", routine: r.label)
        guard var fresh = routine(id: id) else { return outcome.ok }
        if fresh.sessionId == nil, let sid = outcome.sessionId { fresh.sessionId = sid }
        fresh.runs.insert(NanoMuseRoutine.Run(at: startedAt, ok: outcome.ok, note: outcome.note), at: 0)
        if fresh.runs.count > Self.maxRunsKept { fresh.runs = Array(fresh.runs.prefix(Self.maxRunsKept)) }
        if fresh.repeatMode == .once, fresh.intervalHours == nil { fresh.enabled = false }
        update(fresh)
        NanoMuseRoutineEvents.finished.send((fresh, outcome))
        return outcome.ok
    }

    private func prompt(for r: NanoMuseRoutine) -> String {
        if let goalId = r.goalId, let goal = NanoMuseGoalStore.shared.goal(id: goalId) {
            return NanoMuseGoalFlow.checkPrompt(goal)
        }
        return r.prompt
    }

    // MARK: Notifications

    /// One pending calendar notification per enabled routine, at its next time: "open to run it".
    func refreshNotifications() {
        notificationsTask?.cancel()
        notificationsTask = Task { [weak self] in
            let center = UNUserNotificationCenter.current()
            let pending = await center.pendingNotificationRequests()
            guard !Task.isCancelled, let self else { return }
            let ours = pending.filter { $0.identifier.hasPrefix("nanomuse-routine-") }.map(\.identifier)
            center.removePendingNotificationRequests(withIdentifiers: ours)
            let now = Date()
            // iOS keeps 64 pending requests per app; the soonest routines get the slots.
            let soon = self.routines
                .compactMap { r in !r.isCloudArchive && r.enabled ? r.nextDue(after: now).map { (r, $0) } : nil }
                .filter { $0.1 > now.addingTimeInterval(30) }
                .sorted { $0.1 < $1.1 }
                .prefix(20)
            for (r, at) in soon {
                let content = self.notificationContent(for: r)
                let components = Calendar.current.dateComponents([.year, .month, .day, .hour, .minute], from: at)
                let trigger = UNCalendarNotificationTrigger(dateMatching: components, repeats: false)
                try? await center.add(UNNotificationRequest(identifier: "nanomuse-routine-\(r.id)", content: content, trigger: trigger))
            }
        }
    }

    private func postDueNotification(_ r: NanoMuseRoutine, at now: Date) {
        let content = notificationContent(for: r)
        let request = UNNotificationRequest(identifier: "nanomuse-due-\(r.id)-\(Int(now.timeIntervalSince1970))", content: content, trigger: nil)
        UNUserNotificationCenter.current().add(request) { _ in }
    }

    private func notificationContent(for r: NanoMuseRoutine) -> UNMutableNotificationContent {
        let content = UNMutableNotificationContent()
        if let goalId = r.goalId, let goal = NanoMuseGoalStore.shared.goal(id: goalId) {
            content.title = String(format: AppLocalized("Check-in: %@"), goal.title)
        } else {
            content.title = r.label.isEmpty ? AppLocalized("Routine") : r.label
        }
        content.body = AppLocalized("Open morde to run it. The phone cannot run the agent while the app is asleep.")
        content.sound = .default
        content.categoryIdentifier = Self.notificationCategory
        var info: [String: Any] = ["nanomuse.routine": r.id]
        if let sid = r.sessionId { info["sessionId"] = sid }
        content.userInfo = info
        return content
    }

    /// Ask once; iOS keeps the answer.
    func requestNotificationPermission() {
        Task {
            let center = UNUserNotificationCenter.current()
            let settings = await center.notificationSettings()
            guard settings.authorizationStatus == .notDetermined else { return }
            _ = try? await center.requestAuthorization(options: [.alert, .sound, .badge])
        }
    }
}

enum NanoMuseScheduleImportError: LocalizedError {
    case sourceMissing
    case unavailable(String)
    case invalidServerReceipt
    case localSourceSaveFailed

    var errorDescription: String? {
        switch self {
        case .sourceMissing: return "The local plan no longer exists."
        case .unavailable(let reason): return reason
        case .invalidServerReceipt: return "The server did not confirm the created schedule."
        case .localSourceSaveFailed: return "The cloud schedule was created, but the local import receipt could not be saved. Refresh schedules before retrying."
        }
    }
}

// MARK: - Events

enum NanoMuseRoutineEvents {
    /// A routine finished; the feed and the goals listen.
    static let finished = NanoMuseSignal<(NanoMuseRoutine, NanoMuseHeadless.Outcome)>()
}

/// A tiny broadcaster: observers by id, no Combine.
final class NanoMuseSignal<Value> {
    private var handlers: [UUID: (Value) -> Void] = [:]

    @discardableResult
    func observe(_ handler: @escaping (Value) -> Void) -> UUID {
        let id = UUID()
        handlers[id] = handler
        return id
    }

    func cancel(_ id: UUID) { handlers[id] = nil }

    func send(_ value: Value) { handlers.values.forEach { $0(value) } }
}

// MARK: - Headless runs

// nanoMuse: The routine store remains available, but Morde does not run local headless sessions.
enum NanoMuseHeadless {
    struct Outcome {
        var ok: Bool
        var sessionId: String?
        var text: String
        var note: String?
    }

    @MainActor
    static func run(prompt: String, sessionId: String?, title: String?, source: String, routine: String? = nil, timeout: TimeInterval = 10 * 60) async -> Outcome {
        Outcome(ok: false, sessionId: sessionId, text: "", note: MordeLocalExecutionPolicy.disabledMessage)
    }
}
