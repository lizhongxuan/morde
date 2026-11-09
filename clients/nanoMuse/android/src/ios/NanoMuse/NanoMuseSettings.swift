//
//  NanoMuseSettings.swift
//  nanoMuse
//
//  Settings as Muse cards, in Android's order: the model, the agent, the
//  phone, the app, about. Reached from the drawer, the ••• menus and the
//  agent page.
//  Also the "All routines" page. Android: ui/settings/SettingsScreen.kt.
//

import SwiftUI
import UIKit

// MARK: - Settings home

struct NanoMuseSettingsHomeView: View {
    var onOpenCloudTasks: (() -> Void)? = nil
    @ObservedObject private var store = ProviderConfigStore.shared
#if !MINIS_CLOUD_ONLY
    @ObservedObject private var hub = NanoMuseHub.shared
#endif
    @ObservedObject private var updates = NanoMuseUpdateCheck.shared // C2: the installed build and the latest release
    @Environment(\.openURL) private var openURL

    var body: some View {
        NanoMusePage(title: AppLocalized("Settings")) {
            accountCard
            agentCard
            phoneCard
            appCard
            aboutCard
        }
        .onAppear { updates.checkIfStale() }
    }

    // MARK: Card 1 — accounts

    private var accountCard: some View {
        NanoMuseCard {
#if !MINIS_CLOUD_ONLY
            NanoMuseLinkRow(title: NanoMuseCloud.displayLabel, value: cloudLine) { NanoMuseCloudView() }
            NanoMuseRowDivider()
            if let onOpenCloudTasks {
                NanoMuseActionRow(title: AppLocalized("Morde cloud tasks"), action: onOpenCloudTasks)
                    .accessibilityIdentifier("morde.settings.cloudTasks")
            } else {
                NanoMuseLinkRow(title: AppLocalized("Morde cloud tasks")) { MordeEmbeddedView(ownsLifecycle: false) }
                    .accessibilityIdentifier("morde.settings.cloudTasks")
            }
            NanoMuseRowDivider()
            NanoMuseLinkRow(title: AppLocalized("Cloud task execution")) { NanoMuseCloudExecutionView() }
                .accessibilityIdentifier("morde.settings.executionControl")
            NanoMuseRowDivider()
#endif
            NanoMuseLinkRow(title: AppLocalized("Morde account")) { MordeAccountSettingsView() }
                .accessibilityIdentifier("morde.settings.account")
        }
    }

    /// Android: "Sign in" until signed in; then what is left of the allowance (in yuan on a relay that
    /// prices in money, the account page's headline figure; tokens on an older relay), or who is
    /// signed in when there is no ceiling.
    private var cloudLine: String {
        _ = store.instances
        guard NanoMuseCloud.isSignedIn, let account = NanoMuseCloud.account else { return AppLocalized("Sign in") }
        return account.leftLine ?? account.hint
    }

    // MARK: Card 2 — the agent

#if !MINIS_CLOUD_ONLY
    private var computers: Int { hub.others.filter(\.isComputer).count }
#endif

    private var agentCard: some View {
        NanoMuseCard {
            Group {
                NanoMuseLinkRow(title: AppLocalized("Soul")) { SoulSettingsView() }
                NanoMuseRowDivider()
                NanoMuseActionRow(title: AppLocalized("Avatar")) {
                    NotificationCenter.default.post(name: .nanoMuseOpenAvatarStudio, object: nil)
                }
                NanoMuseRowDivider()
                NanoMuseLinkRow(title: AppLocalized("Memory")) { MemoryManagementView() }
#if !MINIS_CLOUD_ONLY
                NanoMuseRowDivider()
                NanoMuseLinkRow(title: AppLocalized("System files")) { NanoMuseSystemFilesView() }
                NanoMuseRowDivider()
                NanoMuseLinkRow(title: AppLocalized("Skills")) { SkillsManagementView() }
                NanoMuseRowDivider()
#endif
            }
            Group {
#if !MINIS_CLOUD_ONLY
                NanoMuseLinkRow(title: AppLocalized("Connectors")) { NanoMuseConnectorsView() }
                NanoMuseRowDivider()
                // Android's Hands row; on iPhone the page explains why the switch is not here.
                NanoMuseLinkRow(title: AppLocalized("Hands"), value: AppLocalized("Not on iPhone")) { NanoMuseHandsView() }
                    .accessibilityIdentifier("morde.settings.hands")
                NanoMuseRowDivider()
                NanoMuseLinkRow(title: AppLocalized("Computers"), value: computers == 0 ? AppLocalized("None") : "\(computers)") { NanoMuseComputersView() }
                    .accessibilityIdentifier("morde.settings.legacyComputers")
                NanoMuseRowDivider()
                NanoMuseLinkRow(title: AppLocalized("Environment Variables")) { EnvironmentVariablesView() }
#endif
            }
        }
    }

    // MARK: Card 3 — the phone

    private var phoneCard: some View {
        NanoMuseCard {
            Group {
                // Android: "Background & notifications" — what the phone lets the agent do while the app is away.
                NanoMuseLinkRow(title: AppLocalized("Background & notifications")) { NanoMuseBackgroundView() }
                NanoMuseRowDivider()
                NanoMuseLinkRow(title: AppLocalized("Storage")) { StorageManagementView() }
                NanoMuseRowDivider()
                NanoMuseLinkRow(title: AppLocalized("Shared Folders")) { SharedFoldersSettingsView() }
            }
            Group {
                NanoMuseRowDivider()
                NanoMuseLinkRow(title: AppLocalized("Mount External Folders")) { MountedFoldersSettingsView() }
#if !MINIS_CLOUD_ONLY
                if #available(iOS 17.0, *) {
                    NanoMuseRowDivider()
                    NanoMuseLinkRow(title: AppLocalized("iCloud Sync")) { CloudSyncSettingsV2View() }
                }
#endif
                NanoMuseRowDivider()
                NanoMuseLinkRow(title: AppLocalized("Backup & Restore")) { BackupAndRestoreView() }
            }
        }
    }

    // MARK: Card 4 — the app

    private var appCard: some View {
        NanoMuseCard {
            NanoMuseLinkRow(title: AppLocalized("Appearance")) { NanoMuseAppearanceView() }
            NanoMuseRowDivider()
            NanoMuseLinkRow(title: AppLocalized("Data controls")) { NanoMuseDataControlsView() }
#if !MINIS_CLOUD_ONLY
            NanoMuseRowDivider()
            NanoMuseLinkRow(title: AppLocalized("Network")) { NanoMuseNetworkView() }
#endif
            NanoMuseRowDivider()
            NanoMuseLinkRow(title: AppLocalized("Logs")) { LogManagementView() }
        }
    }

    // MARK: Card 5 — about

    private var aboutCard: some View {
        NanoMuseCard {
            NanoMuseLinkRow(title: AppLocalized("About morde")) { AboutView() }
            NanoMuseRowDivider()
            NanoMuseActionRow(title: AppLocalized("Privacy Policy")) {
                openURL(NanoMuseLinks.privacy)
            }
            NanoMuseRowDivider()
            NanoMuseActionRow(title: AppLocalized("Feedback")) {
                openURL(NanoMuseLinks.newIssue())
            }
            NanoMuseRowDivider()
            // C2: the installed build and the latest release (NanoMuseUpdateCheck); "Update" when a newer one is out.
            NanoMuseActionRow(
                title: AppLocalized("Version"),
                value: updates.installedLine,
                titleColor: .primary,
                chevron: false
            ) {
                if updates.latestIsNewer { openURL(updates.releasePage) } else { Task { @MainActor in await updates.checkNow() } }
            }
            HStack(spacing: 8) {
                Text(updates.latestLine)
                    .font(.footnote)
                    .foregroundStyle(updates.latestIsNewer ? NanoMuseTones.action : Color.secondary)
                    .lineLimit(2)
                Spacer(minLength: 8)
                if updates.latestIsNewer {
                    Button(AppLocalized("Update")) { openURL(updates.releasePage) }
                        .font(.subheadline.weight(.semibold))
                        .foregroundStyle(NanoMuseTones.action)
                        .buttonStyle(.plain)
                }
            }
            .padding(.horizontal, 16)
            .padding(.bottom, 12)
        }
    }
}

private struct NanoMuseCloudExecutionView: View {
    @StateObject private var store = MIFExecutionControlStore.shared
    @State private var confirmsStop = false

    private var statusTitle: String {
        switch store.state {
        case "enabled": AppLocalized("Cloud execution is enabled")
        case "stopping": AppLocalized("Cloud execution is stopping")
        case "disabled": AppLocalized("Cloud execution is disabled")
        default: AppLocalized("Execution status unavailable")
        }
    }

    var body: some View {
        NanoMusePage(title: AppLocalized("Cloud task execution")) {
            NanoMuseCard {
                VStack(alignment: .leading, spacing: 12) {
                    Label(statusTitle, systemImage: store.state == "disabled" ? "pause.circle" : "play.circle")
                        .font(.headline)
                        .accessibilityIdentifier("morde.execution.state")
                    if let count = store.pendingTaskCount {
                        Text(String(format: AppLocalized("Pending tasks: %d"), count))
                            .font(.footnote).foregroundStyle(.secondary)
                    }
                    if let count = store.activeTaskCount {
                        Text(String(format: AppLocalized("Active cloud tasks: %d"), count))
                            .font(.footnote).foregroundStyle(.secondary)
                    }
                    if let error = store.error {
                        Text(error).font(.footnote).foregroundStyle(.orange)
                            .accessibilityIdentifier("morde.execution.error")
                    }
                    HStack {
                        if store.isBusy || (store.state == "stopping" && store.isLoading) {
                            ProgressView().controlSize(.small)
                        }
                        Spacer()
                        if store.state == "enabled" {
                            Button(AppLocalized("Stop all cloud tasks"), role: .destructive) {
                                confirmsStop = true
                            }
                            .accessibilityIdentifier("morde.execution.stop")
                            .disabled(store.isBusy)
                        } else if store.state == "disabled" {
                            Button(AppLocalized("Enable cloud execution")) {
                                Task { await store.enable() }
                            }
                            .accessibilityIdentifier("morde.execution.enable")
                            .disabled(store.isBusy)
                        }
                        Button(AppLocalized("Refresh")) { Task { await store.refresh() } }
                            .accessibilityIdentifier("morde.execution.refresh")
                            .disabled(store.isBusy || store.isLoading)
                    }
                }
                .padding(16)
            }
        }
        .task { await store.refresh() }
        .confirmationDialog(AppLocalized("Stop all cloud tasks?"), isPresented: $confirmsStop) {
            Button(AppLocalized("Stop all cloud tasks"), role: .destructive) {
                Task { await store.stopAll() }
            }
            .accessibilityIdentifier("morde.execution.stop.confirm")
            Button(AppLocalized("Cancel"), role: .cancel) {}
        } message: {
            Text(AppLocalized("This stops current cloud tasks and keeps future cloud execution disabled until you enable it."))
        }
    }
}

// MARK: - Links

/// The project's public pages.
enum NanoMuseLinks {
    /// The policy on the site (docs/privacy.md is its source), as Android's PRIVACY_URL.
    static let privacy = URL(string: "https://nanomuse.cn/privacy/")!
    /// The own-key guide on the site (docs/own-key.md), when the relay sent no address of its own (Android's OWN_KEY_DOCS).
    static let ownKeyDocs = "https://nanomuse.cn/own-key"

    /// A new GitHub issue with the build and the device filled in (no personal data): the
    /// issue form's fields by id (.github/ISSUE_TEMPLATE/bug_report.yml); a bare `body`
    /// would be dropped on the way to the form.
    static func newIssue() -> URL {
        let info = Bundle.main.infoDictionary
        let version = info?["CFBundleShortVersionString"] as? String ?? "?"
        let build = info?["CFBundleVersion"] as? String ?? "?"
        var components = URLComponents(string: "https://github.com/nano-muse/nanoMuse/issues/new")!
        components.queryItems = [
            URLQueryItem(name: "template", value: "bug_report.yml"),
            URLQueryItem(name: "labels", value: "bug,ios"),
            URLQueryItem(name: "surface", value: "iPhone app"),
            URLQueryItem(name: "version", value: "\(version) (\(build))"),
            URLQueryItem(name: "os", value: "iOS \(UIDevice.current.systemVersion), \(UIDevice.current.model)"),
        ]
        return components.url ?? URL(string: "https://github.com/nano-muse/nanoMuse/issues")!
    }
}

// MARK: - Computers

/// Settings → Computers: this phone on the hub, the other devices, and what can be asked of them.
struct NanoMuseComputersView: View {
    var body: some View {
        List {
            NanoMuseDevicesSection()
            NanoMuseReachSection()
        }
        .navigationTitle(AppLocalized("Computers"))
        .navigationBarTitleDisplayMode(.inline)
    }
}

extension NanoMuseFirstRun {
    /// Forget the setup's state so it shows again (the Cloud sign-in and the providers stay).
    static func reset() {
        for key in ["nanomuse.setup.done", "nanomuse.setup.source_chosen", NanoMuseFirstRun.notificationsKey] { UserDefaults.standard.removeObject(forKey: key) }
    }
}

// MARK: - All routines

/// Every routine on this phone, the goal check-ins and the feed's included: on/off, edit, run now.
struct NanoMuseRoutinesView: View {
    @ObservedObject private var scheduler = NanoMuseScheduler.shared
    @ObservedObject private var goals = NanoMuseGoalStore.shared
    @State private var editing: NanoMuseRoutine?
    @Environment(\.openURL) private var openURL

    var body: some View {
        List {
            Section {
                Text(AppLocalized("A routine is a message the agent sends itself at a set time, in its own conversation. On the iPhone it runs only while the app is open: when it comes due while the app is asleep, the phone shows a reminder, and the run happens as soon as you open the app. Nothing runs in the background."))
                    .font(.footnote)
                    .foregroundStyle(.secondary)
            }
            if scheduler.routines.isEmpty {
                Section {
                    Text(AppLocalized("Nothing scheduled yet."))
                        .foregroundStyle(.secondary)
                }
            } else {
                Section(AppLocalized("Routines")) {
                    ForEach(scheduler.routines) { routine in
                        routineRow(routine)
                    }
                }
            }
            Section {
                Button {
                    editing = NanoMuseRoutine(label: "", prompt: "", hour: 9, minute: 0)
                } label: {
                    Label(AppLocalized("New routine"), systemImage: "plus")
                }
                Button {
                    scheduler.requestNotificationPermission()
                } label: {
                    Label(AppLocalized("Allow reminders"), systemImage: "bell")
                }
                Button {
                    if let url = URL(string: UIApplication.openSettingsURLString) { openURL(url) }
                } label: {
                    Label(AppLocalized("Background App Refresh in iOS Settings"), systemImage: "arrow.clockwise")
                }
            } footer: {
                Text(AppLocalized("Reminders need notification permission. With Background App Refresh on, iOS can wake the app for a moment around the due time to post the reminder; the routine itself still waits for you to open the app."))
            }
        }
        .navigationTitle(AppLocalized("Scheduled tasks"))
        .navigationBarTitleDisplayMode(.inline)
        .sheet(item: $editing) { routine in
            NanoMuseRoutineEditor(routine: routine)
        }
    }

    private func label(for routine: NanoMuseRoutine) -> String {
        if let goalId = routine.goalId, let goal = goals.goal(id: goalId) {
            return String(format: AppLocalized("Check-in: %@"), goal.title)
        }
        return routine.label.isEmpty ? AppLocalized("Routine") : routine.label
    }

    private func routineRow(_ routine: NanoMuseRoutine) -> some View {
        HStack(spacing: 12) {
            VStack(alignment: .leading, spacing: 3) {
                Text(label(for: routine)).font(.body.weight(.medium))
                Text(routine.isCloudArchive ? "Imported to cloud" : (routine.enabled ? routine.cadence : AppLocalized("Off")))
                    .font(.caption).foregroundStyle(.secondary)
                if let next = routine.nextDue(after: Date()), routine.enabled {
                    Text(String(format: AppLocalized("next %@"), NanoMuseDay.relative(next))).font(.caption2).foregroundStyle(.tertiary)
                }
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
                    .accessibilityLabel(Text(label(for: routine)))
            }
        }
        .contentShape(Rectangle())
        .onTapGesture {
            guard !routine.isCloudArchive else { return }
            editing = routine
        }
        .swipeActions(edge: .trailing) {
            if !routine.isCloudArchive {
                Button(role: .destructive) { scheduler.delete(routine.id) } label: { Label(AppLocalized("Delete"), systemImage: "trash") }
            }
        }
    }
}
