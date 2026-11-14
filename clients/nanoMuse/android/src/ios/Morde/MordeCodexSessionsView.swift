import SwiftUI

// nanoMuse: Keep the Morde Codex page's user-visible strings in the shared catalogue.
@MainActor
struct MordeCodexSessionsRoom: View {
    let onMenu: () -> Void
    @StateObject private var store = MIFCodexSessionStore.shared
    @State private var draft = ""
    @State private var deleteTarget: MordeCodexSession?
    @FocusState private var composerFocused: Bool

    var body: some View {
        VStack(spacing: 0) {
            MordeDesignPageHeader(title: AppLocalized("Codex sessions"), onMenu: onMenu) {
                Button {
                    Task { await store.refresh() }
                } label: {
                    Image(systemName: "arrow.clockwise")
                        .font(.system(size: 16, weight: .medium))
                        .frame(width: 38, height: 38)
                        .background(NanoMuseTones.surface, in: Circle())
                        .overlay(Circle().strokeBorder(NanoMuseTones.hairline))
                }
                .buttonStyle(.plain)
                .accessibilityLabel(AppLocalized("Refresh status"))
                .accessibilityIdentifier("morde.codex.refresh")
            }
            if !store.isSignedIn {
                emptyState(AppLocalized("Sign in to Morde to view Codex sessions."))
            } else if let session = store.selectedSession {
                sessionDetail(session)
            } else {
                sessionList
            }
        }
        .background(NanoMuseTones.canvas.ignoresSafeArea())
        .task { await store.loadInstallations() }
        .onChange(of: store.cacheScopeIdentity) { _, _ in draft = "" }
        .task(id: store.selectedStreamIdentity) {
            guard store.selectedSessionID != nil else { return }
            await store.followSelectedSession()
        }
        .confirmationDialog(AppLocalized("Delete this Codex mirror session?"), isPresented: Binding(
            get: { deleteTarget != nil }, set: { if !$0 { deleteTarget = nil } }
        ), titleVisibility: .visible) {
            Button(AppLocalized("Delete"), role: .destructive) {
                guard let session = deleteTarget else { return }
                deleteTarget = nil
                Task { await store.deleteSession(session) }
            }
            Button(AppLocalized("Cancel"), role: .cancel) { deleteTarget = nil }
        } message: {
            Text(AppLocalized("Deleting this mirror removes its cloud history and pending commands. It does not stop a turn already running on the PC."))
        }
    }

    private var sessionList: some View {
        VStack(spacing: 0) {
            installationPicker
            if let installation = store.selectedInstallation {
                if installation.revoked || store.revokedInstallations.contains(installation.id) {
                    emptyState(AppLocalized("This PC installation has been revoked."))
                } else if let error = store.installationErrors[installation.id] {
                    failureState(error) { Task { await store.loadSessions(installationID: installation.id) } }
                } else if store.loadingSessions.contains(installation.id) && store.sessions.isEmpty {
                    ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
                } else if store.sessions.isEmpty {
                    emptyState(AppLocalized("No Codex sessions were found."))
                } else {
                    ScrollView {
                        LazyVStack(spacing: 10) {
                            ForEach(store.sessions) { session in sessionRow(session) }
                            if store.canLoadMoreSessions {
                                Button {
                                    Task { await store.loadMoreSessions() }
                                } label: {
                                    HStack {
                                        if store.loadingSessions.contains(installation.id) { ProgressView().controlSize(.small) }
                                        Text(AppLocalized("Load more sessions"))
                                    }
                                    .frame(maxWidth: .infinity).padding(.vertical, 12)
                                }
                                .disabled(store.loadingSessions.contains(installation.id))
                            }
                        }
                        .padding(.horizontal, 18).padding(.top, 12).padding(.bottom, 28)
                    }
                    .refreshable { await store.refresh() }
                }
            } else if let error = store.installationLoadError {
                failureState(error) { Task { await store.loadInstallations() } }
            } else if store.loadingInstallations {
                ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                emptyState(AppLocalized("No authorized PC installations are available."))
            }
        }
    }

    private var installationPicker: some View {
        Group {
            if !store.installations.isEmpty {
                ScrollView(.horizontal, showsIndicators: false) {
                    HStack(spacing: 8) {
                        ForEach(store.installations) { installation in
                            Button {
                                Task { await store.selectInstallation(installation.id) }
                            } label: {
                                VStack(alignment: .leading, spacing: 3) {
                                    Text(installation.displayName).lineLimit(1).font(.subheadline.weight(.medium))
                                    Text(installation.revoked ? AppLocalized("Installation revoked.") :
                                         AppLocalized("Authorized PC installation"))
                                        .font(.caption).foregroundStyle(.secondary)
                                }
                                .padding(.horizontal, 14).padding(.vertical, 9)
                                .background(store.selectedInstallationID == installation.id ? NanoMuseTones.surface : .clear,
                                            in: Capsule())
                                .overlay(Capsule().strokeBorder(NanoMuseTones.hairline))
                            }
                            .buttonStyle(.plain)
                            .accessibilityIdentifier("morde.codex.installation.\(installation.id)")
                        }
                    }
                    .padding(.horizontal, 18).padding(.vertical, 10)
                }
            }
        }
    }

    private func sessionRow(_ session: MordeCodexSession) -> some View {
        HStack(spacing: 10) {
            Button { store.selectSession(session.id) } label: {
                VStack(alignment: .leading, spacing: 5) {
                    Text(session.title.isEmpty ? AppLocalized("Untitled chat") : session.title)
                        .font(.headline).foregroundStyle(.primary).lineLimit(1)
                    if !session.cwd_label.isEmpty {
                        let cwd = session.cwd_label
                        Text(cwd).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                    }
                    statusBadge(statusLabel(session.status))
                    if let error = store.deleteErrors[session.id] {
                        Text(error).font(.caption).foregroundStyle(.orange).lineLimit(2)
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityIdentifier("morde.codex.session.\(session.id)")
            Button {
                deleteTarget = session
            } label: {
                if store.deletingSessions.contains(session.id) { ProgressView().frame(width: 38, height: 38) }
                else { Image(systemName: "trash").frame(width: 38, height: 38).foregroundStyle(.secondary) }
            }
            .buttonStyle(.plain)
            .disabled(store.deletingSessions.contains(session.id))
            .accessibilityLabel(AppLocalized("Delete Codex session"))
            .accessibilityIdentifier("morde.codex.delete.\(session.id)")
        }
        .padding(14)
        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 18))
        .overlay(RoundedRectangle(cornerRadius: 18).strokeBorder(NanoMuseTones.hairline))
    }

    private func sessionDetail(_ session: MordeCodexSession) -> some View {
        VStack(spacing: 0) {
            HStack(spacing: 10) {
                Button { store.closeSession() } label: {
                    Label(AppLocalized("Back"), systemImage: "chevron.left")
                }
                .buttonStyle(.plain)
                .accessibilityIdentifier("morde.codex.back")
                VStack(alignment: .leading, spacing: 2) {
                    Text(session.title.isEmpty ? AppLocalized("Untitled chat") : session.title)
                        .font(.headline).lineLimit(1)
                    Text(statusLabel(store.selectedSessionStatus)).font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
                Button { deleteTarget = session } label: { Image(systemName: "trash") }
                    .buttonStyle(.plain)
                    .accessibilityLabel(AppLocalized("Delete Codex session"))
                    .accessibilityIdentifier("morde.codex.delete.selected")
            }
            .padding(.horizontal, 18).padding(.vertical, 10)
            if let error = store.deleteErrors[session.id] {
                Text(error).font(.footnote).foregroundStyle(.orange)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.horizontal, 18).padding(.top, 4)
            }
            if store.revokedSessions.contains(session.id) {
                emptyState(AppLocalized("Session access was revoked."))
            } else {
                if !session.hasScope("read") {
                    Text(AppLocalized("Read history permission is unavailable."))
                        .font(.footnote).foregroundStyle(.secondary).frame(maxWidth: .infinity, alignment: .leading)
                        .padding(.horizontal, 18).padding(.vertical, 8)
                }
                if let error = store.eventErrors[session.id] {
                    Text(eventErrorText(error)).font(.footnote).foregroundStyle(.orange)
                        .frame(maxWidth: .infinity, alignment: .leading).padding(.horizontal, 18).padding(.vertical, 5)
                }
                ScrollView {
                    LazyVStack(spacing: 9) {
                        if store.hasEarlierEvents {
                            Button {
                                Task { await store.loadEarlierEvents() }
                            } label: {
                                HStack {
                                    if store.loadingEarlierEvents.contains(session.id) { ProgressView().controlSize(.small) }
                                    Text(AppLocalized("Load earlier events"))
                                }
                                .frame(maxWidth: .infinity).padding(.vertical, 9)
                            }
                            .disabled(store.loadingEarlierEvents.contains(session.id))
                        }
                        if store.loadingEvents.contains(session.id) && store.events.isEmpty {
                            ProgressView().padding(.vertical, 18)
                        } else if store.events.isEmpty && store.eventErrors[session.id] == nil {
                            Text(AppLocalized("No visible events yet.")).font(.footnote).foregroundStyle(.secondary)
                                .padding(.vertical, 18)
                        }
                        ForEach(store.timeline) { item in timelineItem(item, session: session) }
                    }
                    .padding(.horizontal, 16).padding(.top, 10).padding(.bottom, 12)
                }
                .refreshable { await store.refresh() }
                commandFooter(session)
            }
        }
    }

    @ViewBuilder
    private func timelineItem(_ item: MordeCodexTimelineItem, session: MordeCodexSession) -> some View {
        if let approval = item.approval {
            approvalCard(approval, session: session)
        } else {
            VStack(alignment: .leading, spacing: 6) {
                HStack {
                    Text(eventLabel(item.kind)).font(.caption.weight(.semibold)).foregroundStyle(.secondary)
                    Spacer()
                    if let status = item.status { Text(statusLabel(status)).font(.caption).foregroundStyle(.secondary) }
                }
                if let text = item.text, !text.isEmpty {
                    Text(text).font(.body).textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading)
                }
            }
            .padding(12)
            .background(item.kind == .user ? Color.accentColor.opacity(0.08) : NanoMuseTones.surface,
                        in: RoundedRectangle(cornerRadius: 16))
            .overlay(RoundedRectangle(cornerRadius: 16).strokeBorder(NanoMuseTones.hairline))
        }
    }

    private func approvalCard(_ approval: MordeCodexApproval, session: MordeCodexSession) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Label(approval.title.isEmpty ? AppLocalized("Waiting for approval") : approval.title,
                  systemImage: "checkmark.shield")
                .font(.headline)
            if let object = approval.object, !object.isEmpty {
                LabeledContent(AppLocalized("Object"), value: object)
            }
            if let detail = approval.detail, !detail.isEmpty { Text(detail).textSelection(.enabled) }
            if let impact = approval.impact, !impact.isEmpty {
                LabeledContent(AppLocalized("Impact"), value: impact)
            }
            if let expiry = approval.expirationDate() {
                Text(String(format: AppLocalized("Expires: %@"), expiry.formatted(date: .abbreviated, time: .shortened)))
                    .font(.caption).foregroundStyle(.secondary)
            }
            TimelineView(.periodic(from: .now, by: 15)) { timeline in
                let expired = approval.expirationDate().map { $0 <= timeline.date } == true || approval.isNoLongerPending
                if expired {
                    Text(AppLocalized("Approval is no longer valid."))
                        .font(.footnote).foregroundStyle(.secondary)
                        .accessibilityIdentifier("morde.codex.approval.expired.\(approval.id)")
                } else if !approval.canRespond(at: timeline.date) {
                    Text(AppLocalized("Approval validity is unknown."))
                        .font(.footnote).foregroundStyle(.secondary)
                        .accessibilityIdentifier("morde.codex.approval.unknown.\(approval.id)")
                } else if !session.hasScope("read") || !session.hasScope("approval") {
                    Text(AppLocalized("Approval permission is unavailable."))
                        .font(.footnote).foregroundStyle(.secondary)
                } else {
                    HStack {
                        Button(AppLocalized("Approve")) {
                            Task { _ = await store.respondToApproval(approval, decision: "approve") }
                        }
                        .buttonStyle(.borderedProminent)
                        .accessibilityIdentifier("morde.codex.approval.approve.\(approval.id)")
                        Button(AppLocalized("Reject"), role: .destructive) {
                            Task { _ = await store.respondToApproval(approval, decision: "reject") }
                        }
                        .buttonStyle(.bordered)
                        .accessibilityIdentifier("morde.codex.approval.reject.\(approval.id)")
                    }
                    .disabled(store.selectedPendingCommand || store.submittingSessions.contains(session.id))
                }
            }
        }
        .padding(14)
        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 18))
        .overlay(RoundedRectangle(cornerRadius: 18).strokeBorder(Color.orange.opacity(0.4)))
    }

    private func commandFooter(_ session: MordeCodexSession) -> some View {
        VStack(spacing: 8) {
            if let error = store.commandErrors[session.id] {
                Text(commandErrorText(error)).font(.footnote).foregroundStyle(.orange)
                    .frame(maxWidth: .infinity, alignment: .leading)
                if store.pendingCommands[session.id] != nil {
                    Button(AppLocalized("Retry the same request")) {
                        Task { _ = await store.retryUnconfirmedCommand() }
                    }
                    .buttonStyle(.bordered)
                    .accessibilityIdentifier("morde.codex.retrySameRequest")
                }
            }
            if store.selectedSessionStatus == "unknown" {
                Text(AppLocalized("Status is unknown. Refresh the session before sending again."))
                    .font(.footnote).foregroundStyle(.secondary)
            }
            HStack(spacing: 8) {
                if store.canInterrupt {
                    Button {
                        Task { _ = await store.interrupt() }
                    } label: { Image(systemName: "stop.fill").frame(width: 42, height: 42) }
                        .buttonStyle(.bordered)
                        .accessibilityLabel(AppLocalized("Interrupt"))
                        .accessibilityIdentifier("morde.codex.interrupt")
                }
                TextField(AppLocalized("Message Codex"), text: $draft, axis: .vertical)
                    .lineLimit(1...4).focused($composerFocused)
                    .padding(.horizontal, 12).padding(.vertical, 9)
                    .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 18))
                    .overlay(RoundedRectangle(cornerRadius: 18).strokeBorder(NanoMuseTones.hairline))
                    .accessibilityIdentifier("morde.codex.composer")
                Button {
                    let message = draft
                    Task {
                        if await store.sendMessage(message) { draft = "" }
                    }
                } label: { Image(systemName: "arrow.up").frame(width: 42, height: 42) }
                    .buttonStyle(.borderedProminent)
                    .disabled(!store.canSend || draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                    .accessibilityLabel(AppLocalized("Send"))
                    .accessibilityIdentifier("morde.codex.send")
            }
        }
        .padding(.horizontal, 14).padding(.top, 9).padding(.bottom, 12)
        .background(NanoMuseTones.canvas)
    }

    private func statusLabel(_ raw: String?) -> String {
        switch raw?.lowercased() {
        case "waiting_pc", "queued": AppLocalized("Waiting for PC to reconnect")
        case "forwarded": AppLocalized("Forwarded")
        case "claimed": AppLocalized("Claimed by PC")
        case "running", "active", "in_progress": AppLocalized("Running")
        case "waiting_approval", "waiting_for_approval", "approval", "approval_required": AppLocalized("Waiting for approval")
        case "cancelled", "canceled", "interrupted": AppLocalized("Cancelled")
        case "completed", "done": AppLocalized("Completed")
        case "failed", "error": AppLocalized("Failed")
        case "idle": AppLocalized("Idle")
        default: AppLocalized("Unknown")
        }
    }

    private func eventLabel(_ kind: MordeCodexTimelineKind) -> String {
        switch kind {
        case .user: AppLocalized("You")
        case .assistant: AppLocalized("Codex assistant")
        case .reasoning: AppLocalized("Reasoning summary")
        case .plan: AppLocalized("Plan")
        case .tool: AppLocalized("Tool activity")
        case .file: AppLocalized("File change summary")
        case .agent: AppLocalized("Subagent activity")
        case .approval: AppLocalized("Waiting for approval")
        case .status: AppLocalized("Turn status")
        case .commandStatus: AppLocalized("Command status")
        case .unsupported: AppLocalized("Unsupported event")
        }
    }

    private func eventErrorText(_ error: String) -> String {
        error == "event_gap" ? AppLocalized("Event history has a gap. Refresh to repair it.") : error
    }

    private func commandErrorText(_ error: String) -> String {
        error == "unknown" ? AppLocalized("The request result is unknown; retrying uses the same request ID.") : error
    }

    private func statusBadge(_ text: String) -> some View {
        Text(text).font(.caption.weight(.medium)).foregroundStyle(.secondary)
            .padding(.horizontal, 8).padding(.vertical, 4)
            .background(NanoMuseTones.canvas, in: Capsule())
    }

    private func emptyState(_ text: String) -> some View {
        ContentUnavailableView(text, systemImage: "desktopcomputer")
    }

    private func failureState(_ message: String, retry: @escaping () -> Void) -> some View {
        VStack(spacing: 10) {
            Text(message).font(.footnote).foregroundStyle(.orange).multilineTextAlignment(.center)
            Button(AppLocalized("Retry"), action: retry).buttonStyle(.bordered)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity).padding(24)
    }
}
