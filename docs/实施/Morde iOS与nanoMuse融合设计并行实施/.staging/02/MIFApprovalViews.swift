import SwiftUI

@MainActor
struct MIFApprovalInboxRoom: View {
    @StateObject private var store = MIFTaskPageStore.shared
    @Environment(\.dismiss) private var dismiss
    private let onConfirmedCount: ((Int?) -> Void)?
    @State private var taskIDs: [String] = []
    @State private var details: [String: MordeTask] = [:]
    @State private var failedTaskIDs: Set<String> = []
    @State private var pageReadFailed = false
    @State private var isLoading = false
    @State private var hasLoaded = false
    @State private var loadedScopeKey: String?
    @State private var loadGeneration = 0
    @State private var isSubmitting = false
    @State private var actionMessage: String?

    init(onConfirmedCount: ((Int?) -> Void)? = nil) {
        self.onConfirmedCount = onConfirmedCount
    }

    private var summary: MIFApprovalInboxSummary {
        let currentDetails = details.merging(store.details) { _, refreshed in refreshed }
        return MIFApprovalInboxSummary.make(taskIDs: taskIDs, details: currentDetails,
                                     failedTaskIDs: failedTaskIDs, pageReadFailed: pageReadFailed)
    }

    var body: some View {
        VStack(spacing: 0) {
            NanoMuseTopBar(title: AppLocalized("Approvals"), onBack: { dismiss() }) {
                Button {
                    Task { await reload(scopeKey: store.scopeKey) }
                } label: {
                    Image(systemName: "arrow.clockwise")
                        .font(.system(size: 16, weight: .medium))
                        .foregroundStyle(NanoMuseTones.action)
                        .frame(width: 40, height: 40)
                        .contentShape(Circle())
                }
                .disabled(isLoading || !store.isSignedIn || store.scopeKey == nil)
                .accessibilityLabel(AppLocalized("Refresh approvals"))
            }
            ScrollView {
                VStack(alignment: .leading, spacing: 14) {
                    if !store.isSignedIn {
                        statusCard(AppLocalized("Sign in to your Morde account to view approvals."))
                    } else if store.scopeKey == nil {
                        statusCard(AppLocalized("Confirming the current account scope and service status…"))
                    } else if loadedScopeKey != store.scopeKey || !hasLoaded {
                        ProgressView(AppLocalized("Loading approvals…"))
                            .frame(maxWidth: .infinity, minHeight: 180)
                    } else {
                        Text(summary.isPartial
                             ? String(format: AppLocalized("Confirmed pending approvals: %d · Unknown: %d"), summary.approvals.count, summary.unresolvedCount)
                             : String(format: AppLocalized("%d approvals pending"), summary.approvals.count))
                            .font(.footnote.weight(.medium))
                            .foregroundStyle(summary.isPartial ? .orange : .secondary)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .accessibilityIdentifier("mif.approval.count")

                        if let actionMessage {
                            statusCard(actionMessage)
                                .accessibilityIdentifier("mif.approval.action-result")
                        }

                        if summary.isPartial {
                            statusCard(AppLocalized("The task list or some task details could not be fully read; the approval count is unknown. Only confirmed approvals are shown below."))
                                .accessibilityIdentifier("mif.approval.partial")
                        }

                        if summary.approvals.isEmpty {
                            if summary.isConfirmedEmpty {
                                emptyState
                            } else {
                                statusCard(AppLocalized("Unable to confirm whether any approvals are pending. Refresh and try again."))
                            }
                        } else {
                            ForEach(summary.approvals) { approval in
                                approvalCard(approval)
                            }
                        }
                    }
                }
                .padding(16)
            }
            .refreshable { await reload(scopeKey: store.scopeKey) }
        }
        .background(NanoMuseTones.canvas.ignoresSafeArea())
        .toolbar(.hidden, for: .navigationBar)
        .task(id: store.scopeKey) { await reload(scopeKey: store.scopeKey) }
    }

    private var emptyState: some View {
        VStack(spacing: 10) {
            Image(systemName: "checkmark.circle")
                .font(.system(size: 32))
                .foregroundStyle(NanoMuseTones.action)
            Text(AppLocalized("No approvals are currently pending"))
                .font(.headline)
                .accessibilityIdentifier("mif.approval.empty")
            Text(AppLocalized("Only approval requests in task details appear here."))
                .font(.footnote)
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
        }
        .frame(maxWidth: .infinity, minHeight: 180)
        .padding(20)
        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 18, style: .continuous))
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier("mif.approval.empty.container")
    }

    private func approvalCard(_ approval: MIFApprovalEntry) -> some View {
        let task = approval.task
        let pending = approval.pending
        let canRespondInInbox = MIFApprovalInboxPolicy.canRespond(pending: pending)

        return VStack(alignment: .leading, spacing: 12) {
            HStack(alignment: .top, spacing: 10) {
                Image(systemName: "checkmark.shield")
                    .font(.system(size: 20, weight: .semibold))
                    .foregroundStyle(NanoMuseTones.action)
                    .frame(width: 38, height: 38)
                    .background(NanoMuseTones.fill, in: RoundedRectangle(cornerRadius: 12))
                VStack(alignment: .leading, spacing: 5) {
                    Text(pending.title)
                        .font(.headline)
                        .fixedSize(horizontal: false, vertical: true)
                    Text(String(format: AppLocalized("Source · Conversation %@"), task.conversation_id))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    Text(String(format: AppLocalized("Task %@"), task.id))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    Text(String(format: AppLocalized("Status · %@"), task.statusLabel))
                        .font(.subheadline.weight(.medium))
                }
                Spacer(minLength: 0)
            }

            Text(task.objective)
                .font(.subheadline)
                .fixedSize(horizontal: false, vertical: true)

            if let object = pending.object, !object.isEmpty {
                fact(label: AppLocalized("Object"), value: object)
            }
            fact(label: AppLocalized("Content"), value: pending.content)
            fact(label: AppLocalized("Impact"), value: pending.impact)

            if let message = store.actionErrors[task.id] {
                Text(message)
                    .font(.footnote)
                    .foregroundStyle(.orange)
                    .fixedSize(horizontal: false, vertical: true)
            }

            if !canRespondInInbox {
                Text(AppLocalized("Media approvals must be handled in the Task view with preview; this inbox does not offer approve or reject."))
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            } else {
                HStack(spacing: 10) {
                    Button(AppLocalized("Reject")) { Task { await respond(to: approval, decision: "reject") } }
                        .buttonStyle(.bordered)
                        .tint(.red)
                        .accessibilityLabel(String(format: AppLocalized("Reject approval: %@"), pending.title))
                        .accessibilityIdentifier("mif.approval.reject.\(approval.id)")
                    Button(AppLocalized("Approve")) { Task { await respond(to: approval, decision: "approve") } }
                        .buttonStyle(.borderedProminent)
                        .tint(NanoMuseTones.action)
                        .accessibilityLabel(String(format: AppLocalized("Approve approval: %@"), pending.title))
                        .accessibilityIdentifier("mif.approval.approve.\(approval.id)")
                }
                .disabled(isSubmitting)
            }

            NavigationLink {
                MIFTaskDetailView(taskID: task.id)
            } label: {
                Label(AppLocalized("View task event chain"), systemImage: "point.3.connected.trianglepath.dotted")
                    .font(.subheadline.weight(.medium))
            }
            .buttonStyle(.plain)
            .foregroundStyle(NanoMuseTones.action)
            .accessibilityIdentifier("mif.approval.timeline.\(task.id)")

            Text(AppLocalized("HTTP 202 means the server accepted the decision; final status depends on refreshed task details."))
                .font(.caption)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
        }
        .padding(16)
        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 18, style: .continuous))
        .overlay(RoundedRectangle(cornerRadius: 18, style: .continuous).strokeBorder(NanoMuseTones.hairline))
    }

    private func fact(label: String, value: String) -> some View {
        VStack(alignment: .leading, spacing: 3) {
            Text(label)
                .font(.caption.weight(.semibold))
                .foregroundStyle(.secondary)
            Text(value)
                .font(.subheadline)
                .fixedSize(horizontal: false, vertical: true)
        }
    }

    private func statusCard(_ message: String) -> some View {
        Text(message)
            .font(.subheadline)
            .foregroundStyle(.secondary)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(14)
            .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 14, style: .continuous))
    }

    @MainActor
    private func reload(scopeKey: String?) async {
        loadGeneration += 1
        let generation = loadGeneration
        taskIDs = []
        details = [:]
        failedTaskIDs = []
        pageReadFailed = false
        hasLoaded = false
        loadedScopeKey = nil
        actionMessage = nil
        onConfirmedCount?(nil)

        guard let scopeKey else {
            isLoading = false
            hasLoaded = true
            return
        }

        isLoading = true
        defer {
            if generation == loadGeneration { isLoading = false }
        }

        var cursor: String?
        var seenCursors = Set<String>()
        var seenTaskIDs = Set<String>()
        var collectedTaskIDs: [String] = []
        var listFailed = false

        while true {
            do {
                let page = try await store.taskPage(status: "waiting_user", cursor: cursor)
                guard isCurrent(scopeKey, generation: generation) else { return }
                for task in page.items {
                    if task.status == "waiting_user" {
                        if seenTaskIDs.insert(task.id).inserted { collectedTaskIDs.append(task.id) }
                    } else if !MIFApprovalInboxSummary.isKnownTaskStatus(task.status) {
                        listFailed = true
                    }
                }
                guard let next = page.next_cursor, !next.isEmpty else { break }
                guard seenCursors.insert(next).inserted else {
                    listFailed = true
                    break
                }
                cursor = next
            } catch {
                guard isCurrent(scopeKey, generation: generation) else { return }
                listFailed = true
                break
            }
        }

        taskIDs = collectedTaskIDs
        pageReadFailed = listFailed

        for taskID in collectedTaskIDs {
            guard isCurrent(scopeKey, generation: generation) else { return }
            do {
                let detail = try await store.task(id: taskID)
                guard isCurrent(scopeKey, generation: generation) else { return }
                details[taskID] = detail
            } catch {
                guard isCurrent(scopeKey, generation: generation) else { return }
                failedTaskIDs.insert(taskID)
            }
        }

        guard isCurrent(scopeKey, generation: generation) else { return }
        loadedScopeKey = scopeKey
        hasLoaded = true
        publishConfirmedCount(for: scopeKey)
    }

    @MainActor
    private func respond(to approval: MIFApprovalEntry, decision: String) async {
        guard MIFApprovalInboxPolicy.canRespond(pending: approval.pending) else { return }
        guard !isSubmitting, let scopeKey = store.scopeKey else { return }
        isSubmitting = true
        defer { isSubmitting = false }

        let accepted = await store.respondToApproval(taskID: approval.task.id,
                                                    approvalID: approval.pending.id,
                                                    version: approval.pending.version,
                                                    decision: decision)
        guard store.scopeKey == scopeKey else { return }

        let callbackMessage = store.actionErrors[approval.task.id]
        if accepted, callbackMessage == nil, let refreshed = store.details[approval.task.id] {
            details[approval.task.id] = refreshed
            actionMessage = AppLocalized("Decision submitted; final status depends on refreshed task details.")
            publishConfirmedCount(for: scopeKey)
            return
        }
        actionMessage = accepted
            ? AppLocalized("Decision submitted; status refresh is not confirmed.")
            : (callbackMessage ?? AppLocalized("Result not confirmed; retrying the same decision will reuse the client's saved request."))

        // The callback owns approval retries and its idempotency key; this read only confirms the current snapshot.
        do {
            let detail = try await store.task(id: approval.task.id)
            guard store.scopeKey == scopeKey else { return }
            details[approval.task.id] = detail
            failedTaskIDs.remove(approval.task.id)
            publishConfirmedCount(for: scopeKey)
            if !accepted, let callbackMessage {
                actionMessage = String(format: AppLocalized("%@ Current task status: %@"), callbackMessage, detail.statusLabel)
            } else if !accepted {
                actionMessage = String(format: AppLocalized("Approval result is not confirmed; current task status: %@. Resubmitting the same decision will reuse the client's saved request."), detail.statusLabel)
            } else {
                actionMessage = String(format: AppLocalized("Decision submitted; final status depends on refreshed task details. Current status: %@"), detail.statusLabel)
            }
        } catch {
            guard store.scopeKey == scopeKey else { return }
            details.removeValue(forKey: approval.task.id)
            failedTaskIDs.insert(approval.task.id)
            onConfirmedCount?(nil)
            actionMessage = accepted
                ? AppLocalized("Decision accepted, but current status cannot be confirmed; approval count is unknown. Refresh task details to check.")
                : AppLocalized("Approval result is unknown and status refresh failed; approval count is unknown. Retrying the same decision will reuse the client's saved request.")
        }
    }

    private func isCurrent(_ scopeKey: String, generation: Int) -> Bool {
        !Task.isCancelled && store.scopeKey == scopeKey && loadGeneration == generation
    }

    private func publishConfirmedCount(for scopeKey: String) {
        guard store.scopeKey == scopeKey else { return }
        let currentSummary = summary
        onConfirmedCount?(currentSummary.isPartial ? nil : currentSummary.approvals.count)
    }
}

struct MIFApprovalEntry: Identifiable {
    let task: MordeTask
    let pending: MordeTask.Pending
    var id: String { "\(task.id):\(pending.id):\(pending.version)" }
}

enum MIFApprovalInboxPolicy {
    static func canRespond(pending: MordeTask.Pending) -> Bool {
        pending.media_preview_operation_id == nil
    }
}

struct MIFApprovalInboxSummary {
    let approvals: [MIFApprovalEntry]
    let unresolvedCount: Int

    var isPartial: Bool { unresolvedCount > 0 }
    var isConfirmedEmpty: Bool { approvals.isEmpty && !isPartial }

    private static let knownTaskStatuses: Set<String> = [
        "queued", "running", "waiting_user", "pausing", "paused", "cancelling", "completed", "failed", "cancelled"
    ]

    static func isKnownTaskStatus(_ status: String) -> Bool {
        knownTaskStatuses.contains(status)
    }

    static func make(taskIDs: [String], details: [String: MordeTask],
                     failedTaskIDs: Set<String>, pageReadFailed: Bool) -> MIFApprovalInboxSummary {
        var approvals: [MIFApprovalEntry] = []
        var unresolvedCount = pageReadFailed ? 1 : 0

        for taskID in taskIDs {
            guard !failedTaskIDs.contains(taskID), let task = details[taskID], task.id == taskID else {
                unresolvedCount += 1
                continue
            }

            guard task.status == "waiting_user" else {
                if !isKnownTaskStatus(task.status) { unresolvedCount += 1 }
                continue
            }

            if let pending = task.pending_request {
                guard pending.kind == "approval" else {
                    unresolvedCount += 1
                    continue
                }
                approvals.append(MIFApprovalEntry(task: task, pending: pending))
            } else if task.clarification_request == nil {
                unresolvedCount += 1
            }
        }

        return MIFApprovalInboxSummary(approvals: approvals, unresolvedCount: unresolvedCount)
    }
}

// Keep the existing root call site source-compatible until the integration pass renames it.
typealias MIFApprovalRoom = MIFApprovalInboxRoom
