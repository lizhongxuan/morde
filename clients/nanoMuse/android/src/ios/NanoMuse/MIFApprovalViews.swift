import SwiftUI

@MainActor
struct MIFApprovalInboxRoom: View {
    // nanoMuse shell uses its room header; standalone Morde injects the design header.
    private let chrome: NanoMuseRoomChrome?
    private let onMenu: (() -> Void)?
    @StateObject private var store = MIFTaskPageStore.shared
    @Environment(\.dismiss) private var dismiss
    private let onConfirmedCount: ((MIFApprovalCountSnapshot) -> Void)?
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
    @State private var expiryRevision = 0
    @State private var pageIndex = 1
    @State private var selectedApproval: MIFApprovalEntry?

    private let approvalsPerPage = 5

    init(onConfirmedCount: ((MIFApprovalCountSnapshot) -> Void)? = nil, chrome: NanoMuseRoomChrome? = nil,
         onMenu: (() -> Void)? = nil, onOpenEventChain: ((String) -> Void)? = nil) {
        self.onConfirmedCount = onConfirmedCount
        self.chrome = chrome
        self.onMenu = onMenu
        self.onOpenEventChain = onOpenEventChain
    }

    private var summary: MIFApprovalInboxSummary {
        _ = expiryRevision
        let currentDetails = details.merging(store.details) { _, refreshed in refreshed }
        return MIFApprovalInboxSummary.make(taskIDs: taskIDs, details: currentDetails,
                                     failedTaskIDs: failedTaskIDs, pageReadFailed: pageReadFailed)
    }

    private var pageCount: Int { max(1, (summary.approvals.count + approvalsPerPage - 1) / approvalsPerPage) }
    private var currentPage: Int { min(pageIndex, pageCount) }
    private var visibleApprovals: [MIFApprovalEntry] {
        Array(summary.approvals.dropFirst((currentPage - 1) * approvalsPerPage).prefix(approvalsPerPage))
    }
    private var pageNumbers: [Int] {
        Array(Set([1, currentPage - 1, currentPage, currentPage + 1, pageCount]
            .filter { $0 >= 1 && $0 <= pageCount })).sorted()
    }

    private var approvalNeutralActionFill: Color {
        NanoMuseTones.surface
    }

    private var approvalCanvasColor: Color {
        NanoMuseTones.canvas
    }

    var body: some View {
        VStack(spacing: 0) {
            if let onMenu {
                approvalDesignHeader(onMenu: onMenu)
            } else if let chrome {
                NanoMuseRoomHeader(chrome: chrome) { EmptyView() }
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
                            LazyVStack(spacing: 10) {
                                ForEach(visibleApprovals) { approval in approvalCard(approval) }
                            }
                            .accessibilityIdentifier("mif.approval.cards")
                        }
                    }
                }
                .padding(.horizontal, 22)
                .padding(.top, 2)
            }
            .accessibilityIdentifier("mif.approval.list")
            .refreshable { await reload(scopeKey: store.scopeKey) }
            if hasLoaded, loadedScopeKey == store.scopeKey, !summary.approvals.isEmpty {
                paginationBar
                    .padding(.horizontal, 22)
                    .padding(.bottom, onMenu == nil && chrome != nil ? NanoMuseBottomBar.height : 0)
            }
        }
        .background(approvalCanvasColor.ignoresSafeArea())
        .toolbar(.hidden, for: .navigationBar)
        .sheet(item: $selectedApproval) { approval in approvalDetailsSheet(approval) }
        .onChange(of: summary.approvals.count) { count in
            pageIndex = min(pageIndex, max(1, (count + approvalsPerPage - 1) / approvalsPerPage))
        }
        .onChange(of: confirmedCount) { snapshot in
            guard store.isSignedIn, let scopeKey = store.scopeKey,
                  hasLoaded, loadedScopeKey == scopeKey else { return }
            onConfirmedCount?(snapshot)
        }
        .task(id: store.scopeKey) {
            let scopeKey = store.scopeKey
            await reload(scopeKey: scopeKey)
            guard let scopeKey else { return }
            while !Task.isCancelled {
                try? await Task.sleep(nanoseconds: 5_000_000_000)
                guard !Task.isCancelled else { return }
                await reload(scopeKey: scopeKey, preservingSnapshot: true)
            }
        }
    }

    private var confirmedCount: MIFApprovalCountSnapshot { summary.confirmedCount }

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
                    context.stroke(path, with: .color(NanoMuseTones.ink),
                                   style: StrokeStyle(lineWidth: 2.1 * scale, lineCap: .round))
                }
                .frame(width: 19, height: 19)
                .frame(width: 38, height: 38)
                .background(NanoMuseTones.surface, in: Circle())
                .overlay(Circle().stroke(NanoMuseTones.hairline, lineWidth: 1))
                .contentShape(Circle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel(Text(AppLocalized("Chats and settings")))
            Text(AppLocalized("Approvals"))
                .font(.system(size: 17, weight: .semibold))
                .tracking(-0.17)
                .foregroundStyle(NanoMuseTones.ink)
                .lineLimit(1)
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
        let decisionWasAccepted = submissionLedger.contains(approval.id, scopeKey: store.scopeKey)
        let summaryLine = [pending.object, pending.content.isEmpty ? nil : pending.content]
            .compactMap { $0?.isEmpty == false ? $0 : nil }.joined(separator: " · ")

        return VStack(alignment: .leading, spacing: 0) {
            Button { selectedApproval = approval } label: {
                VStack(alignment: .leading, spacing: 5) {
                    HStack(alignment: .firstTextBaseline, spacing: 9) {
                        Circle().fill(NanoMuseTones.action).frame(width: 8, height: 8)
                            .accessibilityHidden(true)
                        Text(pending.title)
                            .font(.system(size: 16, weight: .bold))
                            .tracking(-0.16)
                            .lineLimit(2)
                            .multilineTextAlignment(.leading)
                            .foregroundStyle(NanoMuseTones.ink)
                    }
                    Text(summaryLine.isEmpty ? pending.impact : summaryLine)
                        .font(.system(size: 13.5))
                        .foregroundStyle(NanoMuseTones.secondaryText)
                        .lineLimit(2)
                        .multilineTextAlignment(.leading)
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel(AppLocalized("Details") + ": " + pending.title)
            .accessibilityIdentifier("mif.approval.details.\(approval.id)")

            if let message = store.actionErrors[task.id] {
                Text(message)
                    .font(.footnote)
                    .foregroundStyle(NanoMuseTones.dangerInk)
                    .fixedSize(horizontal: false, vertical: true)
                    .padding(.top, 6)
            }

            TimelineView(.periodic(from: .now, by: 1)) { timeline in
                Group {
                    if pending.isExpired {
                        Text(AppLocalized("Approval expired; the requested action was not run."))
                            .font(.system(size: 14.5, weight: .semibold))
                            .foregroundStyle(NanoMuseTones.secondaryText)
                            .fixedSize(horizontal: false, vertical: true)
                            .padding(.top, 14)
                            .accessibilityIdentifier("mif.approval.expired")
                    } else if pending.deadlineHasPassed(at: timeline.date) {
                        Text(AppLocalized("Approval deadline passed; refresh to confirm its status."))
                            .font(.footnote)
                            .foregroundStyle(NanoMuseTones.secondaryText)
                            .fixedSize(horizontal: false, vertical: true)
                            .padding(.top, 14)
                    } else if !pending.canRespond(taskStatus: task.status, at: timeline.date) {
                        Text(AppLocalized("Approval status is unknown; refresh task details before acting."))
                            .font(.footnote)
                            .foregroundStyle(NanoMuseTones.secondaryText)
                            .fixedSize(horizontal: false, vertical: true)
                            .padding(.top, 14)
                    } else if decisionWasAccepted {
                        Text(AppLocalized("Decision submitted; final status depends on refreshed task details."))
                            .font(.system(size: 14.5, weight: .semibold))
                            .foregroundStyle(NanoMuseTones.secondaryText)
                            .fixedSize(horizontal: false, vertical: true)
                            .padding(.top, 14)
                    } else if pending.media_preview_operation_id != nil {
                        Text(AppLocalized("Media approvals must be handled in the Task view with preview; this inbox does not offer approve or reject."))
                            .font(.footnote)
                            .foregroundStyle(NanoMuseTones.secondaryText)
                            .fixedSize(horizontal: false, vertical: true)
                    } else {
                        HStack(spacing: 8) {
                            approvalActionButton(AppLocalized("Reject"), isPrimary: false, isReject: true,
                                                 id: "mif.approval.reject.\(approval.id)", title: pending.title) {
                                Task { await respond(to: approval, decision: "reject") }
                            }
                            approvalActionButton(AppLocalized("Approve"), isPrimary: true, isReject: false,
                                                 id: "mif.approval.approve.\(approval.id)", title: pending.title) {
                                Task { await respond(to: approval, decision: "approve") }
                            }
                        }
                        .padding(.top, 10)
                        .disabled(isSubmitting || store.actionInFlight ||
                                  !MIFApprovalInboxPolicy.canRespond(pending: pending, taskStatus: task.status, at: timeline.date))
                    }
                }
                .onChange(of: pending.deadlineHasPassed(at: timeline.date)) { passed in
                    if passed && pending.isPendingStatus { expiryRevision &+= 1 }
                }
            }

        }
        .padding(14)
        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 20, style: .continuous))
        .overlay(RoundedRectangle(cornerRadius: 20, style: .continuous).strokeBorder(NanoMuseTones.hairline))
        .shadow(color: NanoMuseTones.action.opacity(0.12), radius: 12, y: 5)
        .accessibilityIdentifier("mif.approval.card.\(approval.id)")
    }

    private var paginationBar: some View {
        let total = summary.approvals.count
        let start = (currentPage - 1) * approvalsPerPage + 1
        let end = min(currentPage * approvalsPerPage, total)
        return HStack(spacing: 10) {
            Text("\(start)–\(end) / \(total)")
                .font(.system(size: 12.5, weight: .medium))
                .foregroundStyle(NanoMuseTones.secondaryText)
                .lineLimit(1)
                .accessibilityIdentifier("mif.approval.range")
            Spacer(minLength: 4)
            Button { pageIndex = max(1, currentPage - 1) } label: {
                Image(systemName: "chevron.left")
                    .font(.system(size: 12, weight: .semibold))
                    .frame(width: 34, height: 34)
                    .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 11, style: .continuous))
                    .overlay(RoundedRectangle(cornerRadius: 11, style: .continuous).strokeBorder(NanoMuseTones.hairline))
            }
            .buttonStyle(.plain)
            .accessibilityLabel(AppLocalized("Back"))
            .disabled(currentPage == 1)

            HStack(spacing: 4) {
                ForEach(Array(pageNumbers.enumerated()), id: \.element) { index, number in
                    if index > 0, number - pageNumbers[index - 1] > 1 {
                        Text("…").foregroundStyle(NanoMuseTones.secondaryText)
                    }
                    Button { pageIndex = number } label: {
                        Text("\(number)")
                            .font(.system(size: 12.5, weight: .semibold))
                            .foregroundStyle(number == currentPage ? NanoMuseTones.onInk : NanoMuseTones.secondaryText)
                            .frame(minWidth: 34, minHeight: 34)
                            .background(number == currentPage ? NanoMuseTones.action : NanoMuseTones.surface,
                                        in: RoundedRectangle(cornerRadius: 11, style: .continuous))
                            .overlay(RoundedRectangle(cornerRadius: 11, style: .continuous)
                                .strokeBorder(number == currentPage ? Color.clear : NanoMuseTones.hairline))
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel(String(format: AppLocalized("Page: %@"), "\(number)"))
                    .accessibilityIdentifier("mif.approval.page.\(number)")
                }
            }

            Button { pageIndex = min(pageCount, currentPage + 1) } label: {
                Image(systemName: "chevron.right")
                    .font(.system(size: 12, weight: .semibold))
                    .frame(width: 34, height: 34)
                    .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 11, style: .continuous))
                    .overlay(RoundedRectangle(cornerRadius: 11, style: .continuous).strokeBorder(NanoMuseTones.hairline))
            }
            .buttonStyle(.plain)
            .accessibilityLabel(AppLocalized("Next"))
            .disabled(currentPage == pageCount)
        }
        .padding(.top, 10)
        .padding(.bottom, 8)
        .overlay(alignment: .top) { Rectangle().fill(NanoMuseTones.hairline).frame(height: 1) }
        .background(approvalCanvasColor)
        .accessibilityIdentifier("mif.approval.pagination")
    }

    private func approvalDetailsSheet(_ approval: MIFApprovalEntry) -> some View {
        let task = approval.task
        let pending = approval.pending
        let taskStateConfirmsDecision = details[task.id].map {
            !taskStillHasPendingRequest($0, matching: approval)
        } ?? false
        return NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: 18) {
                    Text(pending.title)
                        .font(.system(size: 22, weight: .bold))
                        .tracking(-0.22)
                        .fixedSize(horizontal: false, vertical: true)
                    if let object = pending.object, !object.isEmpty {
                        approvalDetailField(AppLocalized("Object"), value: object)
                    }
                    if !pending.content.isEmpty {
                        approvalDetailField(AppLocalized("Content"), value: pending.content)
                    }
                    if !pending.impact.isEmpty {
                        approvalDetailField(AppLocalized("Impact"), value: pending.impact)
                    }
                    ForEach(Array(MordeTaskApprovalDisclosurePresentation.lines(pending.disclosures).enumerated()), id: \.offset) { line in
                        Text(line.element)
                            .font(.system(size: 14))
                            .lineSpacing(5)
                            .foregroundStyle(NanoMuseTones.secondaryText)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    if let expiresAt = pending.expirationDate {
                        Text(String(format: AppLocalized("Expires: %@"),
                                    expiresAt.formatted(date: .abbreviated, time: .shortened)))
                            .font(.system(size: 14))
                            .foregroundStyle(NanoMuseTones.secondaryText)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    if let message = store.actionErrors[task.id] {
                        Text(message)
                            .font(.footnote)
                            .foregroundStyle(NanoMuseTones.dangerInk)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    if summary.eventChainApprovalID == approval.id, !taskStateConfirmsDecision {
                        eventChainLink(task: task)
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(22)
            }
            .background(approvalCanvasColor)
            .navigationTitle(AppLocalized("Details"))
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    Button(AppLocalized("Close")) { selectedApproval = nil }
                }
            }
        }
        .presentationDetents([.large])
        .presentationDragIndicator(.visible)
    }

    private func approvalDetailField(_ title: String, value: String) -> some View {
        VStack(alignment: .leading, spacing: 5) {
            Text(title)
                .font(.system(size: 12.5, weight: .semibold))
                .foregroundStyle(NanoMuseTones.secondaryText)
            Text(value)
                .font(.system(size: 14.5))
                .foregroundStyle(NanoMuseTones.ink)
                .lineSpacing(5)
                .fixedSize(horizontal: false, vertical: true)
        }
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
                .foregroundStyle(isPrimary ? NanoMuseTones.onInk : (isReject ? NanoMuseTones.danger : NanoMuseTones.ink))
                .frame(maxWidth: .infinity, minHeight: 44)
                .background(isPrimary ? NanoMuseTones.action : approvalNeutralActionFill, in: Capsule())
                .overlay(Capsule().strokeBorder(isPrimary ? Color.clear : (isReject ? NanoMuseTones.danger : NanoMuseTones.brandTint), lineWidth: 1))
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
            .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 24, style: .continuous))
            .overlay(RoundedRectangle(cornerRadius: 24, style: .continuous).strokeBorder(NanoMuseTones.hairline))
            .shadow(color: NanoMuseTones.action.opacity(0.18), radius: 15, y: 7)
        .accessibilityElement(children: .combine)
    }

    @MainActor
    private func reload(scopeKey: String?, preservingSnapshot: Bool = false) async {
        loadGeneration += 1
        let generation = loadGeneration
        submissionLedger.use(scopeKey: scopeKey)
        if !preservingSnapshot {
            pageIndex = 1
            selectedApproval = nil
            taskIDs = []
            details = [:]
            failedTaskIDs = []
            pageReadFailed = false
            hasLoaded = false
            loadedScopeKey = nil
            actionMessage = nil
            onConfirmedCount?(.unknown)
        }

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

        var refreshedDetails: [String: MordeTask] = [:]
        var refreshedFailedTaskIDs = Set<String>()
        for taskID in collectedTaskIDs {
            guard isCurrent(scopeKey, generation: generation) else { return }
            do {
                let detail = try await store.task(id: taskID)
                guard isCurrent(scopeKey, generation: generation) else { return }
                refreshedDetails[taskID] = detail
            } catch {
                guard isCurrent(scopeKey, generation: generation) else { return }
                refreshedFailedTaskIDs.insert(taskID)
            }
        }

        guard isCurrent(scopeKey, generation: generation) else { return }
        taskIDs = collectedTaskIDs
        details = refreshedDetails
        failedTaskIDs = refreshedFailedTaskIDs
        pageReadFailed = listFailed
        loadedScopeKey = scopeKey
        hasLoaded = true
        publishConfirmedCount(for: scopeKey)
    }

    @MainActor
    private func respond(to approval: MIFApprovalEntry, decision: String) async {
        guard MIFApprovalInboxPolicy.canRespond(pending: approval.pending, taskStatus: approval.task.status) else { return }
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
            onConfirmedCount?(.unknown)
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
              pending.version == approval.pending.version,
              pending.isPendingStatus else { return false }
        return pending.kind == nil || pending.kind == "approval"
    }

    private func publishConfirmedCount(for scopeKey: String) {
        guard store.scopeKey == scopeKey else { return }
        onConfirmedCount?(summary.confirmedCount)
    }
}

struct MIFApprovalEntry: Identifiable {
    let task: MordeTask
    let pending: MordeTask.Pending
    var id: String { "\(task.id):\(pending.id):\(pending.version)" }
}

enum MIFApprovalInboxPolicy {
    static func canRespond(pending: MordeTask.Pending, taskStatus: String, at now: Date = Date()) -> Bool {
        pending.canRespond(taskStatus: taskStatus, at: now) && pending.media_preview_operation_id == nil
    }
}

struct MIFApprovalInboxSummary {
    let approvals: [MIFApprovalEntry]
    let unresolvedCount: Int
    let evaluatedAt: Date

    var eventChainApprovalID: String? { approvals.first?.id }
    var isPartial: Bool { unresolvedCount > 0 }
    var isConfirmedEmpty: Bool { approvals.isEmpty && !isPartial }
    var pendingCount: Int { approvals.filter { $0.pending.canRespond(taskStatus: $0.task.status, at: evaluatedAt) }.count }
    var confirmedCount: MIFApprovalCountSnapshot {
        guard !isPartial else { return .unknown }
        let deadline = approvals.compactMap { entry -> Date? in
            guard entry.pending.canRespond(taskStatus: entry.task.status, at: evaluatedAt) else { return nil }
            return entry.pending.expirationDate
        }.min()
        return MIFApprovalCountSnapshot(count: pendingCount, validUntil: deadline)
    }

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
                     failedTaskIDs: Set<String>, pageReadFailed: Bool, at now: Date = Date()) -> MIFApprovalInboxSummary {
        var approvals: [MIFApprovalEntry] = []
        var unresolvedCount = pageReadFailed ? 1 : 0

        for taskID in taskIDs {
            guard !failedTaskIDs.contains(taskID), let task = details[taskID], task.id == taskID else {
                unresolvedCount += 1
                continue
            }

            if let pending = task.pending_request {
                guard (pending.kind == nil || pending.kind == "approval"), pending.version > 0,
                      MordeTaskResponsePolicy.hasValidRequestID(pending.id) else {
                    unresolvedCount += 1
                    continue
                }
                if pending.isExpired {
                    approvals.append(MIFApprovalEntry(task: task, pending: pending))
                    continue
                }
                guard pending.isPendingStatus else {
                    unresolvedCount += 1
                    continue
                }
                guard task.status == "waiting_user" else {
                    unresolvedCount += 1
                    continue
                }
                if pending.hasInvalidExpiration || pending.deadlineHasPassed(at: now) {
                    unresolvedCount += 1
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

        return MIFApprovalInboxSummary(approvals: approvals, unresolvedCount: unresolvedCount, evaluatedAt: now)
    }
}

struct MIFApprovalCountSnapshot: Equatable {
    let count: Int?
    let validUntil: Date?
    static let unknown = MIFApprovalCountSnapshot(count: nil, validUntil: nil)
}

@MainActor
final class MIFApprovalCountTracker: ObservableObject {
    @Published private(set) var count: Int?
    private var expiryTask: Task<Void, Never>?
    private var generation: UInt64 = 0

    func update(_ snapshot: MIFApprovalCountSnapshot) {
        generation &+= 1
        expiryTask?.cancel()
        expiryTask = nil
        guard let value = snapshot.count else { count = nil; return }
        guard let deadline = snapshot.validUntil else { count = value; return }
        guard deadline > Date() else { count = nil; return }
        count = value
        let currentGeneration = generation
        expiryTask = Task { @MainActor [weak self] in
            while !Task.isCancelled {
                let remaining = deadline.timeIntervalSinceNow
                if remaining <= 0 {
                    guard let self, self.generation == currentGeneration else { return }
                    self.count = nil
                    self.expiryTask = nil
                    return
                }
                do { try await Task.sleep(nanoseconds: UInt64(min(remaining, 86_400) * 1_000_000_000)) }
                catch { return }
            }
        }
    }

    func clear() { update(.unknown) }
    deinit { expiryTask?.cancel() }
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
