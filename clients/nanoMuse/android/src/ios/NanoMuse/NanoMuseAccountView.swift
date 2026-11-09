import SwiftUI

/// The sections of the account page under the identity line: password, devices, a star row,
/// allowance, invite code, usage and the account timeline.
/// Lives inside `NanoMuseCloudView`'s `Form`, which adds the links and the ways out below.
/// Everything comes from the relay each time the page opens; nothing of it is kept on the phone.
struct NanoMuseAccountSections: View {
    /// The parent's view of the balance, refreshed together with the sheet.
    @Binding var account: NanoMuseCloudAccount?

    @State private var sheet: NanoMuseSheet?
    @State private var sessions: [NanoMuseSession] = []
    @State private var events: [NanoMuseEvent] = []
    @State private var error: String?
    @State private var usageScope = 0
    @State private var starAsk = false
    @State private var exhaustedAsk = false

    var body: some View {
        Group {
            passwordSection
            sessionsSection
            if starAsk {
                Section {
                    NanoMuseStarCard(text: NanoMuseStar.words(for: .signedIn)) {
                        starAsk = false
                    }
                }
            }
            allowanceSection
            if let invite = sheet?.invite { inviteSection(invite) }
            usageSection
            if !events.isEmpty { timelineSection }
        }
        .task { await load() }
    }

    // MARK: - Allowance

    @ViewBuilder
    private var allowanceSection: some View {
        Section {
            if let sheet {
                if sheet.member || sheet.spend?.unlimited == true {
                    Text(AppLocalized("Member: no cap on this account."))
                        .font(.subheadline)
                } else if let spend = sheet.spend, let grant = spend.grant, grant > 0 {
                    let left = spend.remaining ?? 0
                    VStack(alignment: .leading, spacing: 6) {
                        ProgressView(value: min(spend.total / grant, 1))
                        Text(String(format: AppLocalized("%@ of %@ left"), yuan(left), yuan(grant)))
                            .font(.subheadline)
                            .foregroundStyle(.secondary)
                        if let usd = spend.usdCny, usd > 0 {
                            let spent = yuan(spend.total)
                            let dollars = (spend.total / usd).formatted(.number.precision(.fractionLength(2)))
                            Text(String(format: AppLocalized("%@ spent (about $%@)"), spent, dollars))
                                .font(.caption)
                                .foregroundStyle(.tertiary)
                        }
                    }
                    .padding(.vertical, 4)
                    if spend.low {
                        // nanoMuse: Morde keeps its provider-managed allowance and does not add a personal provider here.
                        Text(AppLocalized("Your sign-in and your devices keep working either way."))
                            .font(.footnote)
                            .foregroundStyle(.secondary)
                        // nanoMuse: Keep the exhaustion star prompt without the removed provider sign-in choices.
                        if spend.exhausted && exhaustedAsk {
                            Label {
                                VStack(alignment: .leading, spacing: 4) {
                                    Text(NanoMuseStar.words(for: .exhausted))
                                        .font(.caption).foregroundStyle(.secondary)
                                    HStack(spacing: 12) {
                                        Button(AppLocalized("Star on GitHub")) { NanoMuseStar.open() }
                                            .font(.caption.weight(.medium))
                                        Button(AppLocalized("I already starred")) { NanoMuseStar.markStarred(); exhaustedAsk = false }
                                            .font(.caption.weight(.medium))
                                    }
                                }
                            } icon: { Image(systemName: "star") }
                        }
                    }
                } else if let spend = sheet.spend {
                    Text(String(format: AppLocalized("Spent so far: %@. No allowance figure from this relay."), yuan(spend.total)))
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                } else if let account {
                    // an older relay: tokens, as before
                    let remaining = account.remaining.formatted()
                    let granted = account.granted.formatted()
                    Text(account.unlimited ? AppLocalized("No limit on this account") : String(format: AppLocalized("%@ of %@ tokens left"), remaining, granted))
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                }
            } else if let error {
                Text(error).foregroundStyle(.red).font(.subheadline)
            } else {
                HStack { ProgressView(); Text(AppLocalized("Loading…")).foregroundStyle(.secondary) }
            }
        } header: {
            Text(AppLocalized("Allowance"))
        }
    }

    // MARK: - Invite

    private func inviteSection(_ invite: NanoMuseSheet.Invite) -> some View {
        Section {
            HStack {
                Text(invite.code).font(.body.monospaced())
                Spacer()
                Button {
                    UIPasteboard.general.string = invite.code
                } label: { Label(AppLocalized("Copy"), systemImage: "doc.on.doc") }
                .labelStyle(.iconOnly)
            }
            if !invite.url.isEmpty {
                ShareLink(item: URL(string: invite.url) ?? NanoMuseStar.repoURL) {
                    Label(AppLocalized("Share the link"), systemImage: "square.and.arrow.up")
                }
            }
        } header: {
            Text(AppLocalized("Invite a friend"))
        } footer: {
            let bonus = yuan(invite.bonusCny)
            let earned = yuan(invite.earnedCny)
            let invited = invite.invites.formatted()
            Text(String(format: AppLocalized("When a friend signs up with your code and makes their first request, you both get %@. %@ invited · %@ earned."), bonus, invited, earned))
        }
    }

    // MARK: - Usage

    @ViewBuilder
    private var usageSection: some View {
        if let sheet {
            Section {
                Picker(AppLocalized("Usage"), selection: $usageScope) {
                    Text(AppLocalized("Today")).tag(0)
                    Text(AppLocalized("All time")).tag(1)
                }
                .pickerStyle(.segmented)
                .labelsHidden()
                let rows = usageScope == 0 ? sheet.today : sheet.total
                if rows.isEmpty {
                    Text(AppLocalized("Nothing used yet.")).foregroundStyle(.secondary).font(.subheadline)
                } else {
                    ForEach(rows) { row in usageLine(row, label: kindWord(row.kind)) }
                }
                if usageScope == 1 {
                    ForEach(sheet.byModel) { row in usageLine(row, label: row.model ?? row.kind) }
                }
            } header: {
                Text(AppLocalized("Usage"))
            }
        }
    }

    private func usageLine(_ row: NanoMuseSheet.UsageRow, label: String) -> some View {
        HStack {
            VStack(alignment: .leading, spacing: 2) {
                Text(label)
                let tokens = (row.promptTokens + row.completionTokens).formatted()
                let requests = row.requests.formatted()
                Text(String(format: AppLocalized("%@ requests · %@ tokens"), requests, tokens))
                    .font(.caption).foregroundStyle(.secondary)
            }
            Spacer()
            Text(yuan(row.costCny)).foregroundStyle(.secondary)
        }
    }

    private func kindWord(_ kind: String) -> String {
        switch kind {
        case "chat": return AppLocalized("models.slot.chat") // 对话, as the Models page and Android's usage rows
        case "image": return AppLocalized("Images")
        case "video": return AppLocalized("Video")
        case "realtime": return AppLocalized("Calls")
        default: return kind
        }
    }

    // MARK: - Password

    @State private var passwordOpen = false
    @State private var currentPassword = ""
    @State private var newPassword = ""
    @State private var passwordBusy = false
    @State private var passwordNote: String?

    private var passwordSection: some View {
        Section {
            if passwordOpen {
                if sheet?.hasPassword == true {
                    SecureField(AppLocalized("Current password"), text: $currentPassword)
                        .textContentType(.password)
                }
                SecureField(sheet?.hasPassword == true ? AppLocalized("New password (8 or more)") : AppLocalized("Password (8 or more)"), text: $newPassword)
                    .textContentType(.newPassword)
                HStack {
                    Button(AppLocalized("Save")) { Task { await savePassword() } }
                        .disabled(passwordBusy || !passwordValid)
                    Spacer()
                    Button(AppLocalized("Cancel"), role: .cancel) { passwordOpen = false; newPassword = ""; currentPassword = "" }
                        .disabled(passwordBusy)
                }
            } else {
                Button {
                    passwordNote = nil
                    passwordOpen = true
                } label: {
                    Label(sheet?.hasPassword == true ? AppLocalized("Change password") : AppLocalized("Add a password"), systemImage: "lock")
                }
            }
        } header: {
            Text(AppLocalized("Password"))
        } footer: {
            if let passwordNote {
                Text(passwordNote)
            } else if passwordOpen && sheet?.hasPassword == true {
                Text(AppLocalized("Leave the new password empty to remove it."))
            } else {
                Text(AppLocalized("With one, you can sign in without waiting for a code. Codes keep working either way."))
            }
        }
    }

    private var passwordValid: Bool {
        let has = sheet?.hasPassword == true
        if has && currentPassword.isEmpty { return false }
        if newPassword.isEmpty { return has } // removal
        return newPassword.count >= 8
    }

    private func savePassword() async {
        passwordBusy = true
        defer { passwordBusy = false }
        do {
            try await NanoMuseCloud.setPassword(newPassword, current: currentPassword.isEmpty ? nil : currentPassword)
            passwordNote = newPassword.isEmpty ? AppLocalized("Password removed.") : AppLocalized("Saved.")
            passwordOpen = false
            newPassword = ""
            currentPassword = ""
            await load()
        } catch {
            passwordNote = NanoMuseCloud.describe(error)
        }
    }

    // MARK: - Sessions

    @State private var sessionBusy: String?

    private var sessionsSection: some View {
        Section {
            if sessions.isEmpty {
                Text(AppLocalized("No device holds a key.")).foregroundStyle(.secondary).font(.subheadline)
            }
            ForEach(sessions) { s in
                HStack {
                    VStack(alignment: .leading, spacing: 2) {
                        Text(s.device.isEmpty ? AppLocalized("A device") : s.device) + Text(s.current ? " · \(AppLocalized("this phone"))" : "")
                        let via = s.via == "password" ? AppLocalized("a password") : AppLocalized("a code")
                        let when = (s.lastUsedAt ?? s.createdAt).formatted(.relative(presentation: .named))
                        Text(String(format: AppLocalized("Signed in with %@ · last used %@"), via, when))
                            .font(.caption).foregroundStyle(.secondary)
                    }
                    Spacer()
                    if !s.current {
                        Button(AppLocalized("Sign out")) { Task { await revoke(s.prefix) } }
                            .font(.caption)
                            .disabled(sessionBusy != nil)
                    }
                }
            }
        } header: {
            Text(AppLocalized("Signed in on"))
        }
    }

    private func revoke(_ prefix: String) async {
        sessionBusy = prefix
        defer { sessionBusy = nil }
        do {
            try await NanoMuseCloud.revokeSession(prefix: prefix)
            sessions = (try? await NanoMuseCloud.sessions()) ?? sessions.filter { $0.prefix != prefix }
        } catch {
            self.error = NanoMuseCloud.describe(error)
        }
    }

    // MARK: - Timeline

    private var timelineSection: some View {
        Section {
            ForEach(events.prefix(40)) { e in
                HStack(alignment: .firstTextBaseline) {
                    VStack(alignment: .leading, spacing: 2) {
                        Text(e.kind.replacingOccurrences(of: "_", with: " ").replacingOccurrences(of: ".", with: " "))
                        if !e.detail.isEmpty {
                            Text(e.detail).font(.caption).foregroundStyle(.secondary)
                        }
                    }
                    Spacer()
                    Text(e.at.formatted(date: .abbreviated, time: .shortened))
                        .font(.caption).foregroundStyle(.tertiary)
                }
            }
        } header: {
            Text(AppLocalized("Account activity"))
        }
    }

    // MARK: - Loading

    private func load() async {
        error = nil
        do {
            let next = try await NanoMuseCloud.sheet()
            sheet = next
            account = NanoMuseCloud.account
            // nanoMuse: contract C1 — the policy's gate decides, once per moment
            if NanoMuseStar.shared.signedIn() { starAsk = true }
            if next.spend?.exhausted == true, NanoMuseStar.shared.due(.exhausted) {
                NanoMuseStar.shared.markShown(.exhausted)
                exhaustedAsk = true
            }
        } catch {
            self.error = NanoMuseCloud.describe(error)
        }
        sessions = (try? await NanoMuseCloud.sessions()) ?? []
        events = (try? await NanoMuseCloud.events()) ?? []
    }

    private func yuan(_ n: Double) -> String {
        let whole = n.rounded() == n
        return "¥" + n.formatted(.number.precision(.fractionLength(whole ? 0 : 2)))
    }
}

/// One card asking for a star: the reason, "Star on GitHub", "Not now", "I already starred".
struct NanoMuseStarCard: View {
    var text: String
    var onDone: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Label {
                VStack(alignment: .leading, spacing: 2) {
                    Text(AppLocalized("A star on GitHub helps")).font(.subheadline.weight(.semibold))
                    Text(text).font(.caption).foregroundStyle(.secondary)
                }
            } icon: {
                Image(systemName: "star.fill").foregroundStyle(Color.accentColor)
            }
            HStack {
                Button(AppLocalized("Star on GitHub")) { NanoMuseStar.open(); onDone() }
                    .buttonStyle(.borderedProminent)
                    .controlSize(.small)
                Button(AppLocalized("Not now")) { onDone() }
                    .buttonStyle(.bordered)
                    .controlSize(.small)
                Button(AppLocalized("I already starred")) { NanoMuseStar.markStarred(); onDone() }
                    .buttonStyle(.bordered)
                    .controlSize(.small)
            }
        }
        .padding(.vertical, 4)
    }
}

// Kept as a source-compatible name for the existing Morde settings bridge.
typealias NanoMuseMordePersonalDataQuota = MordePersonalDataQuota

enum NanoMuseMordeQuotaLoadError: LocalizedError {
    case invalidResponse

    var errorDescription: String? {
        switch self {
        case .invalidResponse: return "The Morde personal-data quota response is invalid."
        }
    }
}

/// Add this section to Morde account settings. The default loader uses the typed bridge for
/// GET /api/personal-data/quota; injection remains available to keep the view unit-testable.
struct NanoMuseMordePersonalSyncSection: View {
    typealias QuotaLoader = @MainActor () async throws -> MordePersonalDataQuota

    @ObservedObject private var authorization = MordeShellAuthorization.shared
    @ObservedObject private var sync = NanoMuseMordeProfileSync.shared
    @State private var quota: MordePersonalDataQuota?
    @State private var quotaError: String?
    @State private var quotaLoading = false
    @State private var quotaGeneration = UUID()
    private let quotaLoader: QuotaLoader

    init(quotaLoader: @escaping QuotaLoader = {
        try await MordeShellAuthorization.shared.personalDataQuota()
    }) {
        self.quotaLoader = quotaLoader
    }

    private var hasPersonalScope: Bool {
        authorization.signedIn && authorization.personalDataScopeKey != nil
    }

    var body: some View {
        Group {
            Section {
                if hasPersonalScope {
                    Toggle("Sync Morde personal data", isOn: Binding(
                        get: { sync.syncEnabled },
                        set: { sync.setSyncEnabled($0) }
                    ))
                    if sync.isFlushing {
                        Label("Syncing personal data…", systemImage: "arrow.triangle.2.circlepath")
                            .font(.footnote).foregroundStyle(.secondary)
                    }
                    if let pending = sync.pending {
                        Label("Pending \(pending.objectType) change · \(pending.objectID)", systemImage: "clock")
                            .font(.footnote)
                        if !sync.syncEnabled {
                            Text("Sync is paused. The pending change remains on this device.")
                                .font(.footnote).foregroundStyle(.secondary)
                        }
                        Button("Retry personal-data sync") { Task { await sync.retry() } }
                            .disabled(!sync.syncEnabled || sync.isFlushing || pending.scopeKey != authorization.personalDataScopeKey)
                    } else if let pending = sync.legacyPending {
                        Label("A legacy profile change is waiting to sync.", systemImage: "clock")
                            .font(.footnote)
                        if !sync.syncEnabled {
                            Text("Sync is paused. The pending change remains on this device.")
                                .font(.footnote).foregroundStyle(.secondary)
                        }
                        Button("Retry personal-data sync") { Task { await sync.retry() } }
                            .disabled(!sync.syncEnabled || sync.isFlushing || pending.scopeKey != authorization.personalDataScopeKey)
                    } else if let error = sync.outboxError {
                        Label("The pending personal-data change could not be read: \(error)", systemImage: "exclamationmark.triangle")
                            .font(.footnote).foregroundStyle(.red)
                    } else if let error = sync.queueError {
                        Label("The local file was saved, but could not be queued for sync: \(error)", systemImage: "exclamationmark.triangle")
                            .font(.footnote).foregroundStyle(.red)
                    } else if let error = sync.lastError {
                        Label("Personal-data sync failed: \(error)", systemImage: "exclamationmark.triangle")
                            .font(.footnote).foregroundStyle(.red)
                    } else {
                        Text(sync.syncEnabled ? "No pending personal-data changes." : "Sync is paused. New changes will stay on this device.")
                            .font(.footnote).foregroundStyle(.secondary)
                    }
                    if let error = sync.queueError, (sync.pending != nil || sync.legacyPending != nil) {
                        Label("The latest local file change could not be queued: \(error)", systemImage: "exclamationmark.triangle")
                            .font(.footnote).foregroundStyle(.red)
                        if sync.pending != nil {
                            Text("The newest edit remains on this device. Save the file again after the pending change finishes syncing.")
                                .font(.footnote).foregroundStyle(.secondary)
                        }
                    }
                    if let error = sync.lastError, (sync.pending != nil || sync.legacyPending != nil) {
                        Label("Personal-data sync failed: \(error)", systemImage: "exclamationmark.triangle")
                            .font(.footnote).foregroundStyle(.red)
                    }
                } else {
                    Text("Sign in to Morde to manage personal-data sync. Pending changes remain on this device.")
                        .font(.footnote).foregroundStyle(.secondary)
                }
            } header: {
                Text("Personal data sync")
            } footer: {
                Text("Pausing stops personal-data changes from syncing. It does not revoke schedules or tasks already accepted by Morde.")
            }

            Section {
                if !hasPersonalScope {
                    Text("Sign in to Morde to view this account’s quota.")
                        .font(.footnote).foregroundStyle(.secondary)
                } else if let quota {
                    let reserved = quota.reserved_bytes
                    let used = quota.used_bytes
                    let consumed = used + reserved
                    VStack(alignment: .leading, spacing: 6) {
                        ProgressView(value: quota.quota_bytes > 0
                            ? min(Double(consumed) / Double(quota.quota_bytes), 1)
                            : 0)
                        Text("\(byteCount(used)) used, \(byteCount(reserved)) reserved of \(byteCount(quota.quota_bytes))")
                            .font(.subheadline)
                        Text("\(byteCount(quota.remaining_bytes)) remaining · max attachment \(byteCount(quota.max_attachment_bytes))")
                            .font(.footnote).foregroundStyle(.secondary)
                    }
                    .padding(.vertical, 2)
                    Button { Task { await refreshQuota() } } label: {
                        Label("Refresh quota", systemImage: "arrow.clockwise")
                    }
                        .disabled(quotaLoading)
                } else if quotaLoading {
                    HStack { ProgressView(); Text("Loading personal-data quota…").foregroundStyle(.secondary) }
                } else if let quotaError {
                    Label("Could not load personal-data quota: \(quotaError)", systemImage: "exclamationmark.triangle")
                        .font(.footnote).foregroundStyle(.red)
                    Button { Task { await refreshQuota() } } label: {
                        Label("Retry quota query", systemImage: "arrow.clockwise")
                    }
                        .disabled(quotaLoading)
                } else {
                    Text("Personal-data quota has not been checked.")
                        .font(.footnote).foregroundStyle(.secondary)
                    Button { Task { await refreshQuota() } } label: {
                        Label("Check quota", systemImage: "arrow.clockwise")
                    }
                        .disabled(quotaLoading)
                }
            } header: {
                Text("Personal data quota")
            } footer: {
                if let quota { Text("Quota version \(quota.version).") }
            }
        }
        .onAppear {
            sync.refresh()
            Task { await sync.retry() }
        }
        .nmOnChange(of: authorization.personalDataScopeKey) { _ in
            sync.refresh()
            Task { await sync.retry() }
        }
        .nmOnChange(of: authorization.signedIn) { _ in sync.refresh() }
        .onReceive(NotificationCenter.default.publisher(for: UIApplication.didBecomeActiveNotification)) { _ in
            Task { await sync.retry() }
        }
        .task(id: authorization.personalDataScopeKey) {
            quota = nil
            quotaError = nil
            guard hasPersonalScope else { return }
            await refreshQuota()
        }
    }

    @MainActor
    private func refreshQuota() async {
        guard hasPersonalScope, let scope = authorization.personalDataScopeKey else { return }
        let generation = UUID()
        quotaGeneration = generation
        quotaLoading = true
        quotaError = nil
        quota = nil
        do {
            let value = try await quotaLoader()
            guard quotaGeneration == generation, authorization.personalDataScopeKey == scope else { return }
            guard value.isValid else { throw NanoMuseMordeQuotaLoadError.invalidResponse }
            quota = value
        } catch {
            guard quotaGeneration == generation, authorization.personalDataScopeKey == scope else { return }
            quotaError = error.localizedDescription
        }
        if quotaGeneration == generation { quotaLoading = false }
    }

    private func byteCount(_ value: Int64) -> String {
        ByteCountFormatter.string(fromByteCount: value, countStyle: .file)
    }
}

struct NanoMuseMordeProfileView: View {
    @ObservedObject private var authorization = MordeShellAuthorization.shared
    @ObservedObject private var sync = NanoMuseMordeProfileSync.shared
    @Environment(\.dismiss) private var dismiss
    @State private var name = "Muse"
    @State private var description = ""
    @State private var style = "muse"
    @State private var revision: Int64 = 0
    @State private var isLoading = false
    @State private var isSaving = false
    @State private var error: String?
    @State private var saved = false

    var body: some View {
        Form {
            Section("Personal profile") {
                TextField(AppLocalized("Display name"), text: $name)
                    .textInputAutocapitalization(.words)
                TextField(AppLocalized("Description"), text: $description, axis: .vertical)
                    .lineLimit(2...4)
                Picker(AppLocalized("Style"), selection: $style) {
                    Text("Muse").tag("muse")
                    Text("Flat").tag("flat")
                    Text("Clay").tag("clay")
                    Text("Watercolor").tag("watercolor")
                    Text("Pixel").tag("pixel")
                    Text("Line").tag("line")
                    Text("Sticker").tag("sticker")
                }
            }
            Section {
                if isLoading { ProgressView(AppLocalized("Loading Morde profile…")) }
                else { Button(AppLocalized("Reload cloud profile")) { Task { await load() } } }
                if sync.pending != nil {
                    Label(sync.syncEnabled ? AppLocalized("Profile change is waiting to sync.") : AppLocalized("Sync is paused; this profile change stays on this device."), systemImage: "clock")
                        .font(.footnote)
                    Button(AppLocalized("Retry personal data sync")) { Task { await sync.retry() } }
                        .disabled(!sync.syncEnabled || sync.isFlushing)
                }
                if let message = sync.lastError { Label(message, systemImage: "exclamationmark.triangle").font(.footnote).foregroundStyle(.red) }
                if let error { Label(error, systemImage: "exclamationmark.triangle").font(.footnote).foregroundStyle(.red) }
                if saved { Label(AppLocalized("Profile change queued for Morde sync."), systemImage: "checkmark.circle").font(.footnote).foregroundStyle(.secondary) }
            }
        }
        .navigationTitle(AppLocalized("Personal profile"))
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .confirmationAction) {
                Button(AppLocalized("Save")) { save() }
                    .disabled(isLoading || isSaving || !authorization.signedIn || name.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            }
        }
        .task(id: authorization.personalDataScopeKey) { await load() }
    }

    @MainActor
    private func load() async {
        guard authorization.signedIn, let scope = authorization.personalDataScopeKey else {
            name = "Muse"
            description = ""
            revision = 0
            return
        }
        isLoading = true
        error = nil
        sync.refresh()
        do {
            let profile = try await authorization.personalProfile()
            guard authorization.personalDataScopeKey == scope else { return }
            if let profile {
                name = profile.body.name
                description = profile.body.description ?? ""
                style = profile.body.style ?? "muse"
                revision = profile.revision
            } else {
                name = "Muse"
                description = ""
                style = "muse"
                revision = 0
            }
        } catch {
            if authorization.personalDataScopeKey == scope { self.error = error.localizedDescription }
        }
        isLoading = false
    }

    private func save() {
        let trimmedName = name.trimmingCharacters(in: .whitespacesAndNewlines)
        let trimmedDescription = description.trimmingCharacters(in: .whitespacesAndNewlines)
        let body = MordePersonalProfileBody(name: trimmedName, avatar: "dragon", emoji: nil, color: nil,
            description: trimmedDescription.isEmpty ? nil : trimmedDescription, style: style)
        guard body.isValidForWrite else { error = AppLocalized("The profile fields are outside the allowed limits."); return }
        isSaving = true
        error = nil
        saved = false
        do {
            _ = try sync.enqueue(body, baseRevision: revision)
            saved = true
        } catch { self.error = error.localizedDescription }
        isSaving = false
    }
}
