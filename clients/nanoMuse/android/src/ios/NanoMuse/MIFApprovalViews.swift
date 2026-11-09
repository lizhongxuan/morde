import SwiftUI

@MainActor
struct MIFApprovalInboxRoom: View {
    // nanoMuse shell uses its room header; standalone Morde injects the design header.
    private let chrome: NanoMuseRoomChrome?
    private let onMenu: (() -> Void)?
    @StateObject private var store = MIFTaskPageStore.shared
    @Environment(\.dismiss) private var dismiss
    private let onConfirmedCount: ((Int?) -> Void)?
    private let onOpenEventChain: ((String) -> Void)?
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
    @State private var submissionLedger = MIFApprovalSubmissionLedger()
    @Environment(\.colorScheme) private var colorScheme

    init(onConfirmedCount: ((Int?) -> Void)? = nil, chrome: NanoMuseRoomChrome? = nil,
         onMenu: (() -> Void)? = nil, onOpenEventChain: ((String) -> Void)? = nil) {
        self.onConfirmedCount = onConfirmedCount
        self.chrome = chrome
        self.onMenu = onMenu
        self.onOpenEventChain = onOpenEventChain
    }

    private var summary: MIFApprovalInboxSummary {
        let currentDetails = details.merging(store.details) { _, refreshed in refreshed }
        return MIFApprovalInboxSummary.make(taskIDs: taskIDs, details: currentDetails,
                                     failedTaskIDs: failedTaskIDs, pageReadFailed: pageReadFailed)
    }

    private var approvalSeparatorColor: Color {
        colorScheme == .dark ? NanoMuseTones.hairline : Color(red: 239.0 / 255, green: 239.0 / 255, blue: 239.0 / 255)
    }

    private var approvalNeutralActionFill: Color {
        colorScheme == .dark ? NanoMuseTones.fill : Color(red: 240.0 / 255, green: 240.0 / 255, blue: 242.0 / 255)
    }

    private var approvalCanvasColor: Color {
        guard onMenu != nil, colorScheme != .dark else { return NanoMuseTones.canvas }
        return Color(red: 250.0 / 255, green: 250.0 / 255, blue: 250.0 / 255)
    }

    var body: some View {
        VStack(spacing: 0) {
            if let onMenu {
                approvalDesignHeader(onMenu: onMenu)
            } else if let chrome {
                NanoMuseRoomHeader(chrome: chrome) {
                    // nanoMuse: shared room headers need the confirmed count for the approval inbox.
                    if let confirmedHeaderCountText {
                        Text(confirmedHeaderCountText)
                            .font(.system(size: 13.5, weight: .medium))
                            .foregroundStyle(NanoMuseTones.secondaryText)
                            .lineLimit(1)
                            .accessibilityIdentifier("mif.approval.count")
                    }
                }
            } else {
                NanoMuseTopBar(title: AppLocalized("Approvals"), onBack: { dismiss() })
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
                        if let actionMessage {
                            statusCard(actionMessage)
                                .accessibilityIdentifier("mif.approval.action-result")
                        }

                        if summary.isPartial, !summary.approvals.isEmpty {
                            statusCard(AppLocalized("The task list or some task details could not be fully read; the approval count is unknown. Only confirmed approvals are shown below."))
                                .accessibilityIdentifier("mif.approval.partial")
                        }

                        if summary.approvals.isEmpty {
                            if summary.isConfirmedEmpty {
                                emptyState
                            } else {
                                statusCard(AppLocalized("Unable to confirm whether any approvals are pending. Refresh and try again."))
                                    .accessibilityIdentifier("mif.approval.unconfirmed")
                            }
                        } else {
                            VStack(spacing: 0) {
                                ForEach(summary.approvals) { approval in
                                    if summary.eventChainApprovalID != approval.id {
                                        Rectangle()
                                            .fill(approvalSeparatorColor)
                                            .frame(height: 1)
                                    }
                                    approvalCard(approval)
                                }
                            }
                        }
                    }
                }
                .padding(.horizontal, 22)
                .padding(.top, 2)
                .padding(.bottom, onMenu == nil && chrome != nil ? NanoMuseBottomBar.height : 0)
            }
            .accessibilityIdentifier("mif.approval.list")
            .refreshable { await reload(scopeKey: store.scopeKey) }
        }
        .background(approvalCanvasColor.ignoresSafeArea())
        .toolbar(.hidden, for: .navigationBar)
        .task(id: store.scopeKey) { await reload(scopeKey: store.scopeKey) }
    }

    private var confirmedHeaderCountText: String? {
        guard store.isSignedIn, let scopeKey = store.scopeKey,
              hasLoaded, loadedScopeKey == scopeKey, !summary.isPartial else { return nil }
        return String(format: AppLocalized("%d approvals pending"), summary.approvals.count)
    }

    private func approvalDesignHeader(onMenu: @escaping () -> Void) -> some View {
        HStack(spacing: 10) {
            Button(action: onMenu) {
                Canvas { context, size in
                    let scale = size.width / 24
                    var path = Path()
                    path.move(to: CGPoint(x: 4 * scale, y: 9 * scale))
                    path.addLine(to: CGPoint(x: 20 * scale, y: 9 * scale))
                    path.move(to: CGPoint(x: 4 * scale, y: 15 * scale))
                    path.addLine(to: CGPoint(x: 20 * scale, y: 15 * scale))
                    context.stroke(path, with: .color(.primary),
                                   style: StrokeStyle(lineWidth: 2.1 * scale, lineCap: .round))
                }
                .frame(width: 19, height: 19)
                .frame(width: 38, height: 38)
                .background(NanoMuseTones.surface, in: Circle())
                .overlay(Circle().stroke(colorScheme == .dark ? NanoMuseTones.hairline : Color.black.opacity(0.09), lineWidth: 1))
                .contentShape(Circle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel(Text(AppLocalized("Chats and settings")))
            Text(AppLocalized("Approvals"))
                .font(.system(size: 17, weight: .semibold))
                .tracking(-0.17)
                .foregroundStyle(Color.primary)
                .lineLimit(1)
            if let confirmedHeaderCountText {
                Text(confirmedHeaderCountText)
                    .font(.system(size: 13.5, weight: .medium))
                    .foregroundStyle(Color(red: 163.0 / 255, green: 163.0 / 255, blue: 168.0 / 255))
                    .lineLimit(1)
                    .padding(.leading, 10)
                    .accessibilityIdentifier("mif.approval.count")
            }
            Spacer(minLength: 8)
        }
        .padding(.horizontal, 22)
        .padding(.top, 10)
        .padding(.bottom, 4)
        .frame(minHeight: 48)
    }

    private var emptyState: some View {
        VStack(spacing: 10) {
            Image(systemName: "checkmark.circle")
                .font(.system(size: 32))
                .foregroundStyle(NanoMuseTones.action)
                .accessibilityHidden(true)
            Text(AppLocalized("No approvals are currently pending"))
                .font(.headline)
                .accessibilityIdentifier("mif.approval.empty")
            Text(AppLocalized("Only approval requests in task details appear here."))
                .font(.footnote)
                .foregroundStyle(NanoMuseTones.secondaryText)
                .multilineTextAlignment(.center)
                .fixedSize(horizontal: false, vertical: true)
        }
        .frame(maxWidth: .infinity, minHeight: 180)
        .padding(20)
        .accessibilityElement(children: .combine)
        .accessibilityIdentifier("mif.approval.empty.container")
    }

    private func approvalCard(_ approval: MIFApprovalEntry) -> some View {
        let task = approval.task
        let pending = approval.pending
        let canRespondInInbox = MIFApprovalInboxPolicy.canRespond(pending: pending)
        let decisionWasAccepted = submissionLedger.contains(approval.id, scopeKey: store.scopeKey)
        let taskStateConfirmsDecision = details[task.id].map {
            !taskStillHasPendingRequest($0, matching: approval)
        } ?? false
        var factLines: [String] = []
        if let object = pending.object, !object.isEmpty { factLines.append(object) }
        if !pending.content.isEmpty { factLines.append(pending.content) }
        if !pending.impact.isEmpty { factLines.append(pending.impact) }

        return VStack(alignment: .leading, spacing: 0) {
            Text(pending.title)
                .font(.system(size: 17.5, weight: .bold))
                .tracking(-0.175)
                .lineSpacing(2.5)
                .fixedSize(horizontal: false, vertical: true)

            Text(factLines.joined(separator: "\n"))
            .font(.system(size: 14.5))
            .lineSpacing(6)
            .foregroundStyle(Color(red: 115.0 / 255, green: 115.0 / 255, blue: 120.0 / 255))
            .padding(.top, 7)
            .fixedSize(horizontal: false, vertical: true)

            if let message = store.actionErrors[task.id] {
                Text(message)
                    .font(.footnote)
                    .foregroundStyle(.orange)
                    .fixedSize(horizontal: false, vertical: true)
            }

            if decisionWasAccepted {
                Text(AppLocalized("Decision submitted; final status depends on refreshed task details."))
                    .font(.system(size: 14.5, weight: .semibold))
                    .foregroundStyle(NanoMuseTones.secondaryText)
                    .fixedSize(horizontal: false, vertical: true)
                    .padding(.top, 14)
            } else if !canRespondInInbox {
                Text(AppLocalized("Media approvals must be handled in the Task view with preview; this inbox does not offer approve or reject."))
                    .font(.footnote)
                    .foregroundStyle(NanoMuseTones.secondaryText)
                    .fixedSize(horizontal: false, vertical: true)
            } else {
                GeometryReader { geometry in
                    let availableWidth = max(0, geometry.size.width - 8)
                    HStack(spacing: 8) {
                        approvalActionButton(AppLocalized("Reject"), isPrimary: false, isReject: true,
                                             id: "mif.approval.reject.\(approval.id)", title: pending.title) {
                            Task { await respond(to: approval, decision: "reject") }
                        }
                        .frame(width: availableWidth / 2.25)
                        approvalActionButton(AppLocalized("Approve"), isPrimary: true, isReject: false,
                                             id: "mif.approval.approve.\(approval.id)", title: pending.title) {
                            Task { await respond(to: approval, decision: "approve") }
                        }
                        .frame(width: availableWidth * 1.25 / 2.25)
                    }
                }
                .frame(height: 44)
                .padding(.top, 14)
                .disabled(isSubmitting || store.actionInFlight)
            }

            if summary.eventChainApprovalID == approval.id, !taskStateConfirmsDecision {
                eventChainLink(task: task)
                    .padding(.top, 12)
            }
        }
        .padding(.top, 16)
        .padding(.bottom, 18)
    }

    @ViewBuilder
    private func eventChainLink(task: MordeTask) -> some View {
        let label = Text(AppLocalized("View task event chain") + " ›")
            .font(.system(size: 14, weight: .semibold))
            .frame(minHeight: 32, alignment: .leading)
            .contentShape(Rectangle())
        if let onOpenEventChain {
            Button { onOpenEventChain(task.id) } label: { label }
                .buttonStyle(.plain)
                .foregroundStyle(NanoMuseTones.ink)
                .accessibilityIdentifier("mif.approval.timeline.\(task.id)")
        } else {
            NavigationLink(destination: MIFTaskEventChainRoom(taskID: task.id)) { label }
                .buttonStyle(.plain)
                .foregroundStyle(NanoMuseTones.ink)
                .accessibilityIdentifier("mif.approval.timeline.\(task.id)")
        }
    }

    private func approvalActionButton(_ label: String, isPrimary: Bool, isReject: Bool,
                                      id: String, title: String, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Text(label)
                .font(.system(size: 14.5, weight: .semibold))
                .foregroundStyle(isPrimary ? NanoMuseTones.onInk : (isReject ? Color(red: 0.784, green: 0.118, blue: 0.118) : Color.primary))
                .frame(maxWidth: .infinity, minHeight: 44)
                .background(isPrimary ? Color.primary : approvalNeutralActionFill, in: Capsule().inset(by: 2))
        }
        .buttonStyle(.plain)
        .accessibilityLabel(isReject
                            ? String(format: AppLocalized("Reject approval: %@"), title)
                            : String(format: AppLocalized("Approve approval: %@"), title))
        .accessibilityIdentifier(id)
    }

    private func statusCard(_ message: String) -> some View {
        Text(message)
            .font(.subheadline)
            .foregroundStyle(NanoMuseTones.secondaryText)
        .fixedSize(horizontal: false, vertical: true)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(14)
            .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 14, style: .continuous))
        .accessibilityElement(children: .combine)
    }

    @MainActor
    private func reload(scopeKey: String?) async {
        loadGeneration += 1
        let generation = loadGeneration
        submissionLedger.use(scopeKey: scopeKey)
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
                    if MIFApprovalInboxSummary.isPendingPageStatus(task.status) {
                        if seenTaskIDs.insert(task.id).inserted { collectedTaskIDs.append(task.id) }
                    } else {
                        // A filtered list returning another state cannot confirm the pending count.
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
        if accepted {
            submissionLedger.markAccepted(approval.id, scopeKey: scopeKey)
        }

        let callbackMessage = store.actionErrors[approval.task.id]
        if accepted, callbackMessage == nil, let refreshed = store.details[approval.task.id] {
            details[approval.task.id] = refreshed
            actionMessage = taskStillHasPendingRequest(refreshed, matching: approval)
                ? nil
                : AppLocalized("Decision submitted; final status depends on refreshed task details.")
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
            if accepted, taskStillHasPendingRequest(detail, matching: approval) {
                actionMessage = nil
            } else if accepted {
                actionMessage = String(format: AppLocalized("Decision submitted; final status depends on refreshed task details. Current status: %@"), detail.statusLabel)
            } else if let callbackMessage {
                actionMessage = String(format: AppLocalized("%@ Current task status: %@"), callbackMessage, detail.statusLabel)
            } else {
                actionMessage = String(format: AppLocalized("Approval result is not confirmed; current task status: %@. Resubmitting the same decision will reuse the client's saved request."), detail.statusLabel)
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
        // SwiftUI can cancel the refresh task after its response arrives; these two checks
        // still reject account switches and superseded reloads without dropping valid data.
        store.scopeKey == scopeKey && loadGeneration == generation
    }

    private func taskStillHasPendingRequest(_ task: MordeTask, matching approval: MIFApprovalEntry) -> Bool {
        guard task.status == "waiting_user",
              let pending = task.pending_request,
              pending.id == approval.pending.id,
              pending.version == approval.pending.version else { return false }
        return pending.kind == nil || pending.kind == "approval"
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

    var eventChainApprovalID: String? { approvals.first?.id }
    var isPartial: Bool { unresolvedCount > 0 }
    var isConfirmedEmpty: Bool { approvals.isEmpty && !isPartial }

    private static let knownTaskStatuses: Set<String> = [
        "queued", "running", "waiting_user", "pausing", "paused", "cancelling", "completed", "failed", "cancelled"
    ]

    static func isPendingPageStatus(_ status: String) -> Bool {
        status == "waiting_user"
    }

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

            if let pending = task.pending_request {
                guard pending.kind == nil || pending.kind == "approval" else {
                    unresolvedCount += 1
                    continue
                }
                guard task.status == "waiting_user" else {
                    unresolvedCount += 1
                    continue
                }
                approvals.append(MIFApprovalEntry(task: task, pending: pending))
                continue
            }

            guard task.status == "waiting_user" else {
                if !isKnownTaskStatus(task.status) { unresolvedCount += 1 }
                continue
            }

            if task.clarification_request == nil {
                unresolvedCount += 1
            }
        }

        return MIFApprovalInboxSummary(approvals: approvals, unresolvedCount: unresolvedCount)
    }
}

// Keep the existing root call site source-compatible until the integration pass renames it.
typealias MIFApprovalRoom = MIFApprovalInboxRoom

struct MIFApprovalSubmissionLedger {
    private(set) var scopeKey: String? = nil
    private(set) var acceptedApprovalIDs = Set<String>()

    mutating func use(scopeKey: String?) {
        guard self.scopeKey != scopeKey else { return }
        self.scopeKey = scopeKey
        acceptedApprovalIDs.removeAll()
    }

    mutating func markAccepted(_ approvalID: String, scopeKey: String) {
        guard self.scopeKey == scopeKey else { return }
        acceptedApprovalIDs.insert(approvalID)
    }

    func contains(_ approvalID: String, scopeKey: String?) -> Bool {
        self.scopeKey == scopeKey && acceptedApprovalIDs.contains(approvalID)
    }
}
