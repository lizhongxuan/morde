import SwiftUI

struct MIFResourcePageState: Equatable {
    enum DefaultTarget: Equatable {
        case unknown
        case notConfigured
        case configured(hostName: String?, workspaceName: String?)
    }

    struct Host: Equatable, Identifiable {
        enum Scope: Equatable {
            case owned
            case sharedReadOnly
        }

        enum Status: Equatable {
            case running
            case starting
            case offline
            case stopped
            case unknown
        }

        let id: String
        let name: String
        let scope: Scope
        let status: Status
        let workspaceSummary: String?
        let canOpen: Bool
        let control: Control?
        let error: String?
    }

    struct Control: Equatable {
        enum State: Equatable {
            case ai
            case user
            case takingOver
            case returning
            case unknown
        }

        enum Lease: Equatable {
            case none
            case active
            case expired
            case unknown
        }

        let state: State
        let lease: Lease
        let leaseExpiresAt: String?
        let transitionSummary: String?
        let isLoading: Bool
        let isActionInFlight: Bool
        let canTakeOver: Bool
        let canReturnToAI: Bool
        let canRenewLease: Bool
        let error: String?
    }

    struct Workspace: Equatable, Identifiable {
        enum Status: Equatable {
            case ready
            case syncing
            case unbound
            case unavailable
            case unknown
        }

        let id: String
        let name: String
        let status: Status
        let hostName: String?
        let isReadOnly: Bool
        let canOpenFiles: Bool
    }

    let isLoading: Bool
    let loadError: String?
    let defaultTarget: DefaultTarget
    let hosts: [Host]
    let workspaces: [Workspace]
}

enum MIFResourceControlAction: String, Hashable {
    case takeOver
    case returnToAI
    case renewLease
}

enum MIFResourcePageRules {
    static func controlActions(for host: MIFResourcePageState.Host) -> [MIFResourceControlAction] {
        guard host.scope == .owned, host.status == .running, let control = host.control,
              !control.isLoading, !control.isActionInFlight,
              control.transitionSummary == nil, control.lease != .unknown,
              control.state == .ai || control.state == .user else { return [] }

        var actions: [MIFResourceControlAction] = []
        if control.canTakeOver { actions.append(.takeOver) }
        if control.state == .user && control.lease == .active {
            if control.canReturnToAI { actions.append(.returnToAI) }
            if control.canRenewLease { actions.append(.renewLease) }
        }
        return actions
    }
}

struct MIFResourcePageActions {
    let refresh: () -> Void
    let openHost: (String) -> Void
    let refreshControl: (String) -> Void
    let control: (String, MIFResourceControlAction) -> Void
    let openWorkspaceFiles: (String) -> Void
}

@MainActor
struct MIFResourceRoom: View {
    var chrome: NanoMuseRoomChrome? = nil
    let state: MIFResourcePageState
    let actions: MIFResourcePageActions
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(spacing: 0) {
            if let chrome {
                NanoMuseRoomHeader(chrome: chrome) {
                    refreshButton
                }
            } else {
                NanoMuseTopBar(title: AppLocalized("Resources"), onBack: { dismiss() }) {
                    refreshButton
                }
            }

            ScrollView {
                VStack(alignment: .leading, spacing: 14) {
                    if state.isLoading && state.hosts.isEmpty && state.workspaces.isEmpty {
                        ProgressView(AppLocalized("Loading resources…"))
                            .frame(maxWidth: .infinity, minHeight: 180)
                    } else {
                        if let loadError = state.loadError {
                            statusCard(loadError, symbol: "exclamationmark.triangle")
                        }
                        defaultTargetCard
                        hostsSection
                        workspacesSection
                    }
                }
                .padding(16)
                .padding(.bottom, 24)
            }
            .refreshable { actions.refresh() }
        }
        .background(NanoMuseTones.canvas.ignoresSafeArea())
        .toolbar(.hidden, for: .navigationBar)
        .accessibilityIdentifier("morde.resources.page")
    }

    private var refreshButton: some View {
        Button(action: actions.refresh) {
            Image(systemName: "arrow.clockwise")
        }
        .accessibilityLabel(Text(AppLocalized("Refresh resources")))
        .accessibilityIdentifier("morde.resources.refresh")
    }

    private var defaultTargetCard: some View {
        NanoMuseCard {
            VStack(alignment: .leading, spacing: 6) {
                Label(AppLocalized("Default execution target"), systemImage: "scope")
                    .font(.subheadline.weight(.semibold))
                switch state.defaultTarget {
                case .unknown:
                    Text(AppLocalized("Default target status is unknown."))
                        .foregroundStyle(.secondary)
                case .notConfigured:
                    Text(AppLocalized("No default computer or Workspace is set."))
                        .foregroundStyle(.secondary)
                case let .configured(hostName, workspaceName):
                    let summary = [hostName, workspaceName].compactMap { $0 }.joined(separator: " · ")
                    Text(summary.isEmpty ? AppLocalized("Default target details are unavailable.") : summary)
                        .foregroundStyle(.secondary)
                }
            }
            .font(.footnote)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(16)
        }
        .accessibilityIdentifier("morde.resources.defaultTarget")
    }

    private var hostsSection: some View {
        VStack(alignment: .leading, spacing: 10) {
            sectionTitle(AppLocalized("Cloud computers"), symbol: "desktopcomputer")
            if state.hosts.isEmpty {
                statusCard(AppLocalized("No cloud computers are available in this account scope."), symbol: "desktopcomputer")
            } else {
                ForEach(state.hosts) { host in
                    hostCard(host)
                }
            }
        }
    }

    private func hostCard(_ host: MIFResourcePageState.Host) -> some View {
        NanoMuseCard {
            VStack(alignment: .leading, spacing: 12) {
                HStack(alignment: .top, spacing: 12) {
                    Image(systemName: "desktopcomputer")
                        .font(.system(size: 20))
                        .foregroundStyle(NanoMuseTones.action)
                        .frame(width: 30)
                    VStack(alignment: .leading, spacing: 4) {
                        Text(host.name)
                            .font(.body.weight(.semibold))
                            .lineLimit(2)
                        HStack(spacing: 6) {
                            Text(host.status.localizedTitle)
                            if host.scope == .sharedReadOnly {
                                Text("·")
                                Text(AppLocalized("Shared read-only"))
                            }
                        }
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                        if let workspaceSummary = host.workspaceSummary, !workspaceSummary.isEmpty {
                            Text(workspaceSummary)
                                .font(.footnote)
                                .foregroundStyle(.secondary)
                                .lineLimit(2)
                        }
                    }
                    Spacer(minLength: 0)
                }

                if host.scope == .owned {
                    controlStatus(host)
                }

                if host.status == .running && !host.canOpen && host.scope == .owned && host.workspaceSummary == nil {
                    Text(AppLocalized("A ready Workspace binding is required to open this computer."))
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                } else if host.scope == .sharedReadOnly && !host.canOpen {
                    Text(AppLocalized("You do not have permission to view this computer."))
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                }

                if let error = host.error {
                    Text(error)
                        .font(.footnote)
                        .foregroundStyle(.orange)
                        .accessibilityIdentifier("morde.resources.host.error.\(host.id)")
                }

                HStack(spacing: 12) {
                    if host.canOpen {
                        Button {
                            actions.openHost(host.id)
                        } label: {
                            Label(AppLocalized("Open computer"), systemImage: "arrow.up.right.square")
                        }
                        .accessibilityIdentifier("morde.resources.host.open.\(host.id)")
                    }
                    Spacer(minLength: 0)
                    if host.scope == .owned {
                        ForEach(MIFResourcePageRules.controlActions(for: host), id: \.self) { action in
                            Button {
                                actions.control(host.id, action)
                            } label: {
                                Label(action.localizedTitle, systemImage: action.systemImage)
                                    .labelStyle(.iconOnly)
                            }
                            .accessibilityLabel(Text(action.localizedTitle))
                            .accessibilityIdentifier("morde.resources.host.\(action.rawValue).\(host.id)")
                        }
                        if host.control?.isLoading == true || host.control?.isActionInFlight == true {
                            ProgressView().controlSize(.small)
                                .accessibilityLabel(Text(AppLocalized("Updating control status")))
                        } else {
                            Button {
                                actions.refreshControl(host.id)
                            } label: {
                                Image(systemName: "arrow.clockwise")
                            }
                            .accessibilityLabel(Text(AppLocalized("Refresh control status")))
                            .accessibilityIdentifier("morde.resources.host.refreshControl.\(host.id)")
                        }
                    }
                }
                .buttonStyle(.borderless)
                .disabled(host.control?.isActionInFlight == true)
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(16)
        }
        .accessibilityIdentifier("morde.resources.host.\(host.id)")
    }

    @ViewBuilder
    private func controlStatus(_ host: MIFResourcePageState.Host) -> some View {
        if let control = host.control {
            VStack(alignment: .leading, spacing: 4) {
                Label(control.state.localizedTitle, systemImage: "lock.shield")
                    .font(.footnote.weight(.medium))
                if control.lease != .none {
                    Text(control.lease.localizedTitle)
                        .font(.footnote)
                        .foregroundStyle(control.lease == .expired || control.lease == .unknown ? Color.orange : Color.secondary)
                }
                if let expiresAt = control.leaseExpiresAt, control.lease == .active {
                    Text(String(format: AppLocalized("Lease expires %@"), expiresAt))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                if let transitionSummary = control.transitionSummary {
                    Text(transitionSummary)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .accessibilityIdentifier("morde.resources.host.transition.\(host.id)")
                }
                if let error = control.error {
                    Text(error)
                        .font(.caption)
                        .foregroundStyle(.orange)
                }
            }
        } else {
            Text(AppLocalized("Control status unknown; control actions are disabled."))
                .font(.footnote)
                .foregroundStyle(.orange)
        }
    }

    private var workspacesSection: some View {
        VStack(alignment: .leading, spacing: 10) {
            sectionTitle(AppLocalized("Workspaces"), symbol: "externaldrive")
            if state.workspaces.isEmpty {
                statusCard(AppLocalized("No Workspaces are available in this account scope."), symbol: "externaldrive")
            } else {
                ForEach(state.workspaces) { workspace in
                    workspaceCard(workspace)
                }
            }
        }
    }

    private func workspaceCard(_ workspace: MIFResourcePageState.Workspace) -> some View {
        NanoMuseCard {
            VStack(alignment: .leading, spacing: 10) {
                HStack(spacing: 12) {
                    Image(systemName: "externaldrive")
                        .font(.system(size: 19))
                        .foregroundStyle(NanoMuseTones.action)
                        .frame(width: 30)
                    VStack(alignment: .leading, spacing: 4) {
                        Text(workspace.name)
                            .font(.body.weight(.semibold))
                            .lineLimit(2)
                        HStack(spacing: 6) {
                            Text(workspace.status.localizedTitle)
                            if let hostName = workspace.hostName {
                                Text("·")
                                Text(hostName)
                            }
                            if workspace.isReadOnly {
                                Text("·")
                                Text(AppLocalized("Read-only"))
                            }
                        }
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                    }
                    Spacer(minLength: 0)
                }

                if workspace.canOpenFiles {
                    Button {
                        actions.openWorkspaceFiles(workspace.id)
                    } label: {
                        Label(AppLocalized("Browse Workspace files"), systemImage: "folder")
                    }
                    .accessibilityIdentifier("morde.resources.workspace.open.\(workspace.id)")
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(16)
        }
        .accessibilityIdentifier("morde.resources.workspace.\(workspace.id)")
    }

    private func sectionTitle(_ title: String, symbol: String) -> some View {
        Label(title, systemImage: symbol)
            .font(.headline)
            .padding(.horizontal, 4)
    }

    private func statusCard(_ message: String, symbol: String) -> some View {
        NanoMuseCard {
            Label {
                Text(message)
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                    .frame(maxWidth: .infinity, alignment: .leading)
            } icon: {
                Image(systemName: symbol)
                    .foregroundStyle(.secondary)
            }
            .padding(16)
        }
    }
}

private extension MIFResourcePageState.Host.Status {
    var localizedTitle: String {
        switch self {
        case .running: return AppLocalized("Running")
        case .starting: return AppLocalized("Starting")
        case .offline: return AppLocalized("Offline")
        case .stopped: return AppLocalized("Stopped")
        case .unknown: return AppLocalized("Status unknown")
        }
    }
}

private extension MIFResourcePageState.Control.State {
    var localizedTitle: String {
        switch self {
        case .ai: return AppLocalized("AI controls this computer")
        case .user: return AppLocalized("You control this computer")
        case .takingOver: return AppLocalized("Control handoff in progress")
        case .returning: return AppLocalized("Returning control to AI")
        case .unknown: return AppLocalized("Control status unknown")
        }
    }
}

private extension MIFResourcePageState.Control.Lease {
    var localizedTitle: String {
        switch self {
        case .none: return AppLocalized("No active control lease")
        case .active: return AppLocalized("Control lease active")
        case .expired: return AppLocalized("Control lease expired")
        case .unknown: return AppLocalized("Lease status unknown")
        }
    }
}

private extension MIFResourcePageState.Workspace.Status {
    var localizedTitle: String {
        switch self {
        case .ready: return AppLocalized("Ready")
        case .syncing: return AppLocalized("Syncing")
        case .unbound: return AppLocalized("Not mounted")
        case .unavailable: return AppLocalized("Unavailable")
        case .unknown: return AppLocalized("Status unknown")
        }
    }
}

private extension MIFResourceControlAction {
    var localizedTitle: String {
        switch self {
        case .takeOver: return AppLocalized("Take control")
        case .returnToAI: return AppLocalized("Return control to AI")
        case .renewLease: return AppLocalized("Renew lease")
        }
    }

    var systemImage: String {
        switch self {
        case .takeOver: return "hand.raised"
        case .returnToAI: return "arrow.uturn.backward"
        case .renewLease: return "clock.arrow.circlepath"
        }
    }
}
