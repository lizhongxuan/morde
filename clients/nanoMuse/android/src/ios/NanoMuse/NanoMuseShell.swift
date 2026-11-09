//
//  NanoMuseShell.swift
//  nanoMuse
//
//  The Muse shell on iPhone and iPad: one main chat pinned to the Chat tab,
//  a bottom bar for Chat / Tasks / Approvals / Resources, and a side
//  drawer with read-only access to legacy chats. The first
//  run (NanoMuseFirstRun) shows in front of it all until the account and a
//  model are in. Android: ui/home/*.
//

import SwiftUI
import Combine

// MARK: - Preferences

enum NanoMuseShellPrefs {
    private static let shellKey = "nanomuse.shell.enabled"
    private static let headerKey = "nanomuse.header.enabled"

    /// The Muse shell on phones (off → the upstream OpenMinis layout).
    static var shell: Bool {
        get { UserDefaults.standard.object(forKey: shellKey) as? Bool ?? true }
        set { UserDefaults.standard.set(newValue, forKey: shellKey) }
    }

    /// The face · name · status header in the chat's navigation bar.
    static var museHeader: Bool {
        get { UserDefaults.standard.object(forKey: headerKey) as? Bool ?? true }
        set { UserDefaults.standard.set(newValue, forKey: headerKey) }
    }
}

// MARK: - Tabs

enum NanoMuseTab: String, CaseIterable, Identifiable, Equatable {
    // nanoMuse: Morde's four root destinations use the existing Muse shell and floating bar.
    case chat, tasks, approvals, resources
    var id: String { rawValue }

    var title: String {
        switch self {
        case .chat: return AppLocalized("tab.chat") // its own key: 聊天 as on Android, not upstream's 闲聊
        case .tasks: return AppLocalized("Tasks")
        case .approvals: return AppLocalized("Approvals")
        case .resources: return AppLocalized("Resources")
        }
    }

    var symbol: String {
        switch self {
        case .chat: return "bubble.left"
        case .tasks: return "checkmark.circle"
        case .approvals: return "checkmark.shield"
        case .resources: return "square.grid.2x2"
        }
    }

    var selectedSymbol: String {
        switch self {
        case .chat: return "bubble.left.fill"
        case .tasks: return "checkmark.circle.fill"
        case .approvals: return "checkmark.shield.fill"
        case .resources: return "square.grid.2x2.fill"
        }
    }
}

/// nanoMuse: Keep legacy Library routes landing in the Resources root after the four-tab merge.
enum NanoMuseRoomDestination: Equatable {
    case tab(NanoMuseTab)

    var tab: NanoMuseTab {
        if case .tab(let tab) = self { return tab }
        return .chat
    }

    static func resolve(_ room: String?) -> NanoMuseRoomDestination? {
        switch room {
        case "chat": return .tab(.chat)
        case "tasks": return .tab(.tasks)
        case "approvals": return .tab(.approvals)
        case "resources", "library": return .tab(.resources)
        default: return nil
        }
    }
}

// MARK: - Root

/// What MinisApp shows after first-run setup.
struct NanoMuseRoot: View {
    @ObservedObject private var store = ProviderConfigStore.shared
    // nanoMuse: Observe Morde authorization so a Morde-only account can leave the legacy first-run gate.
    @ObservedObject private var mordeAuthorization = MordeShellAuthorization.shared
    @ObservedObject private var personalConflict = NanoMuseMordePersonalConflictResolution.shared
    @Environment(\.scenePhase) private var scenePhase
    @State private var hasSessions: Bool?
    @State private var setupDone = NanoMuseFirstRun.isDone
    @State private var showSetupSettings = false

    /// The setup in front of everything until the account or a model of one's own is in (see NanoMuseFirstRun.needed).
    /// `nil` while the answer still hangs on the session list, so a fresh install never shows the
    /// shell for a frame before the welcome page.
    private var needsSetup: Bool? {
#if MINIS_CLOUD_ONLY
        false
#else
        let cloudId = NanoMuseCloud.instance?.id
        let providers = store.instances.contains { $0.isEnabled && $0.hasAnyCredential && $0.id != cloudId }
        if let hasSessions {
            return NanoMuseFirstRun.needed(signedIn: NanoMuseCloud.isSignedIn, hasProviders: providers,
                                           hasSessions: hasSessions, done: setupDone,
                                           mordeSignedIn: mordeAuthorization.signedIn || mordeAuthorization.offlineConversationAvailable)
        }
        // Without the sessions: an app with neither a sign-in nor a key of its own needs the setup
        // whatever the list says; a finished setup never does; the one case between waits.
        // nanoMuse: A restored Morde credential is verified before deciding whether legacy setup is needed.
        if mordeAuthorization.signedIn || mordeAuthorization.offlineConversationAvailable { return false }
        if !NanoMuseCloud.isSignedIn && !providers { return true }
        return setupDone ? false : nil
#endif
    }

    var body: some View {
        Group {
            if needsSetup == nil {
                Color.clear
            } else if needsSetup == true {
                NanoMuseFirstRunView(
                    onSettings: { showSetupSettings = true }
                )
                .sheet(isPresented: $showSetupSettings) {
                    NavigationStack {
                        NanoMuseSettingsHomeView()
                    }
                }
            } else {
                NanoMuseHomeView()
            }
        }
        .confirmationDialog(conflictTitle, isPresented: Binding(
            get: { personalConflict.isPromptPresented },
            set: { if !$0 { personalConflict.dismissPrompt() } }
        ), titleVisibility: .visible) {
            if let pending = personalConflict.currentPending {
                if !pending.conflict.deleted {
                    Button(AppLocalized("Apply this iPhone change")) {
                        Task { await personalConflict.applyLocalChange() }
                    }
                    .accessibilityIdentifier("morde.personal.conflict.apply")
                }
                Button(AppLocalized("Discard unsynced change"), role: .destructive) {
                    Task { await personalConflict.discardLocalChange() }
                }
                .accessibilityIdentifier("morde.personal.conflict.discard")
                Button(AppLocalized("Cancel"), role: .cancel) {}
            }
        } message: {
            Text(conflictMessage)
        }
        // The card for a step another device's Muse asks about, over the shell and the upstream layout alike.
        .nanoMuseRemoteApprovalPresenter()
        .onAppear {
            personalConflict.scopeChanged(to: mordeAuthorization.personalDataScopeKey)
#if !MINIS_CLOUD_ONLY
            NanoMuseProfileSync.shared.start()
            NanoMuseAccountData.shared.start() // C12: assign session ownership and scope account data
            NanoMuseSync.shared.start() // C7: conversations between the account's devices
#endif
            NanoMuseStar.shared.dayOpened()
        }
        .nmOnChange(of: scenePhase) { phase in
            // C1: one more day with the app, counted when it comes to the front; C2: the release check, when stale.
            guard phase == .active else { return }
            NanoMuseStar.shared.dayOpened()
            NanoMuseUpdateCheck.shared.checkIfStale()
        }
        .nmOnChange(of: mordeAuthorization.personalDataScopeKey) { scope in
            personalConflict.scopeChanged(to: scope)
        }
        .task {
            async let mordeRestore: Void = mordeAuthorization.restoreForFirstRun()
#if MINIS_CLOUD_ONLY
            hasSessions = !(await ChatStore.shared.listSessions()).isEmpty
#else
            hasSessions = !NanoMuseSync.shared.visible(await ChatStore.shared.listSessions()).isEmpty // C12: another account's chats do not count
#endif
            await mordeRestore
        }
        .onReceive(NotificationCenter.default.publisher(for: .sessionDidCreate)) { _ in
            hasSessions = true
        }
        // nanoMuse: show the one-time Keychain migration notice from the shared provider store at the app root.
        .alert(AppLocalized("About older provider API keys"), isPresented: Binding(
            get: { store.ownProviderAPIKeyCleanupNoticePending },
            set: { if !$0 { store.acknowledgeOwnProviderAPIKeyCleanupNotice() } }
        )) {
            Button(AppLocalized("Got it"), role: .cancel) {
                store.acknowledgeOwnProviderAPIKeyCleanupNotice()
            }
        } message: {
            Text(AppLocalized("This update removes older API keys you entered for model providers. If a key was stored in iCloud Keychain, deleting it may also remove it from other devices signed in to the same Apple Account. OAuth sign-ins, Morde credentials and other app data are kept."))
        }
    }

    private var conflictTitle: String {
        AppLocalized(personalConflict.currentPending?.conflict.deleted == true ? "Cloud item deleted" : "Cloud data changed")
    }

    private var conflictMessage: String {
        let message: String
        if personalConflict.currentPending?.conflict.deleted == true {
            message = AppLocalized("The cloud item was deleted. This ID cannot be restored. Discarding removes the unsynced change from this iPhone.")
        } else if let revision = personalConflict.currentPending?.conflict.currentRevision {
            message = String(format: AppLocalized("Applying this iPhone change will replace cloud revision %lld. Discard removes the unsynced change from this iPhone."), revision)
        } else {
            message = ""
        }
        if let error = personalConflict.actionError, !error.isEmpty { return "\(message)\n\(error)" }
        return message
    }
}

/// Presents the agent's page when the header's face is tapped.
struct NanoMuseAgentPagePresenter: ViewModifier {
    var enabled: Bool
    var onOpenSession: (String) -> Void
    var onPrefillChat: (String) -> Void
    /// nil → the page's "Manage routines" opens the routines list in place.
    var onOpenRoutines: (() -> Void)?
    @State private var shown = false
    @State private var showRoutines = false

    func body(content: Content) -> some View {
        content
            .onReceive(NotificationCenter.default.publisher(for: .nanoMuseOpenAgentPage)) { _ in
                guard enabled else { return }
                shown = true
            }
            .sheet(isPresented: $shown) {
                NanoMuseAgentPage(
                    onOpenSession: { id in shown = false; onOpenSession(id) },
                    onPrefillChat: { text in shown = false; onPrefillChat(text) },
                    onOpenRoutines: {
                        shown = false
                        if let onOpenRoutines { onOpenRoutines() } else { showRoutines = true }
                    }
                )
            }
            .sheet(isPresented: $showRoutines) {
                NavigationStack {
                    NanoMuseRoutinesView()
                        .toolbar { ToolbarItem(placement: .cancellationAction) { Button(AppLocalized("Done")) { showRoutines = false } } }
                }
            }
    }
}

extension View {
    func nanoMuseAgentPagePresenter(enabled: Bool, onOpenSession: @escaping (String) -> Void, onPrefillChat: @escaping (String) -> Void, onOpenRoutines: (() -> Void)?) -> some View {
        modifier(NanoMuseAgentPagePresenter(enabled: enabled, onOpenSession: onOpenSession, onPrefillChat: onPrefillChat, onOpenRoutines: onOpenRoutines))
    }
}

/// Presents the avatar studio when the header's face is tapped.
struct NanoMuseStudioPresenter: ViewModifier {
    var enabled: Bool
    @State private var shown = false

    func body(content: Content) -> some View {
        content
            .onReceive(NotificationCenter.default.publisher(for: .nanoMuseOpenAvatarStudio)) { _ in
                guard enabled else { return }
                shown = true
            }
            .sheet(isPresented: $shown) {
                NanoMuseAvatarStudioView()
            }
    }
}

extension View {
    func nanoMuseStudioPresenter(enabled: Bool) -> some View {
        modifier(NanoMuseStudioPresenter(enabled: enabled))
    }
}

// MARK: - Main chat

/// The one conversation the Chat tab always shows. Remembered across
/// launches (UserDefaults); resolved as: the remembered session when it
/// still exists → the most recent session → a fresh draft.
@MainActor
final class NanoMuseMainChat: ObservableObject {
    static let key = "nanomuse.main_chat.session"
    static let draftPrefix = "__new__"

    @Published private(set) var chatId: String?
    @Published private(set) var isDraft = false
    /// The id the view model is cached under: the session, or nil while a draft has not been sent yet.
    @Published private(set) var liveId: String?

    private var cancellables: Set<AnyCancellable> = []

    init() {
        NotificationCenter.default.publisher(for: .sessionDidCreate)
            .receive(on: RunLoop.main)
            .sink { [weak self] note in self?.sessionCreated(note) }
            .store(in: &cancellables)
        // C10: another account signed in — its own main chat, or a fresh draft; never the
        // conversation the previous account left pinned here.
        NotificationCenter.default.publisher(for: .nanoMuseAccountSwitched)
            .receive(on: RunLoop.main)
            .sink { [weak self] _ in self?.accountSwitched() }
            .store(in: &cancellables)
    }

    func resolve() {
        Task { @MainActor [self] in
            let remembered = UserDefaults.standard.string(forKey: Self.key)
            // C10: a remembered chat of another account is not shown (NanoMuseSync.shows)
            if let remembered, NanoMuseSync.shared.shows(remembered), await ChatStore.shared.sessionExists(id: remembered) {
                set(remembered, draft: false)
                return
            }
            let sessions = NanoMuseSync.shared.visible(await ChatStore.shared.listSessions())
            if let latest = sessions.filter({ !$0.isRemote }).max(by: { $0.updatedAt < $1.updatedAt }) {
                UserDefaults.standard.set(latest.id, forKey: Self.key)
                set(latest.id, draft: false)
                return
            }
            set(Self.draftPrefix + UUID().uuidString, draft: true)
        }
    }

    /// C10: the account changed under the tab. The chat it showed for the account that came
    /// back is pinned again when it is still there; otherwise the tab resolves as on a first
    /// launch — the account's own conversations, or a draft the account's synced main adopts.
    private func accountSwitched() {
        Task { @MainActor [self] in
            if let remembered = NanoMuseSync.shared.rememberedMainSession, NanoMuseSync.shared.shows(remembered), await ChatStore.shared.sessionExists(id: remembered) {
                pin(remembered)
                return
            }
            UserDefaults.standard.removeObject(forKey: Self.key)
            resolve()
        }
    }

    /// Pin another conversation to the Chat tab.
    func pin(_ id: String) {
        UserDefaults.standard.set(id, forKey: Self.key)
        set(id, draft: false)
    }

    /// C7: a synced main conversation takes the place of a draft that has not been sent yet.
    func adoptIfDraft(_ id: String) {
        guard isDraft else { return }
        pin(id)
    }

    /// Start the main chat over with an empty draft.
    func startFresh() {
        UserDefaults.standard.removeObject(forKey: Self.key)
        set(Self.draftPrefix + UUID().uuidString, draft: true)
    }

    private func set(_ id: String, draft: Bool) {
        isDraft = draft
        chatId = id
        liveId = draft ? nil : id
    }

    private func sessionCreated(_ note: Notification) {
        guard isDraft, let realId = note.object as? String else { return }
        let draftId = (note.userInfo as? [String: String])?["draftId"]
        guard draftId == chatId else { return }
        // The draft became a real session: remember it, keep the view as is
        // (the view model is already cached under the real id).
        UserDefaults.standard.set(realId, forKey: Self.key)
        liveId = realId
    }
}

// MARK: - Keyboard

/// Hides the bottom bar while the keyboard is up, as Android's IME does.
@MainActor
final class NanoMuseKeyboardWatcher: ObservableObject {
    @Published private(set) var visible = false
    private var cancellables: Set<AnyCancellable> = []

    init() {
        NotificationCenter.default.publisher(for: UIResponder.keyboardWillShowNotification)
            .receive(on: RunLoop.main)
            .sink { [weak self] _ in self?.visible = true }
            .store(in: &cancellables)
        NotificationCenter.default.publisher(for: UIResponder.keyboardWillHideNotification)
            .receive(on: RunLoop.main)
            .sink { [weak self] _ in self?.visible = false }
            .store(in: &cancellables)
    }

    func reset() { visible = false }
}

// MARK: - Home

struct NanoMuseHomeView: View {
    @StateObject private var main = NanoMuseMainChat()
    @StateObject private var keyboard = NanoMuseKeyboardWatcher()
    @ObservedObject private var star = NanoMuseStar.shared
    @ObservedObject private var allowance = NanoMuseAllowance.shared
    // nanoMuse: Clear a known approval badge when the Morde account or service scope changes.
    @ObservedObject private var mordeAuthorization = MordeShellAuthorization.shared
    @ObservedObject private var mordeTasks = MIFTaskPageStore.shared
    /// The composer's voice panel stands in for the keyboard: the bottom bar leaves the same way.
    @ObservedObject private var voiceMode = VoiceModePreference.shared
    @State private var tab: NanoMuseTab = .chat
    @State private var drawerDestination: MIFDrawerDestination = .chat
    @State private var taskHomeDestination: MIFTaskHomeDestination = .tasks
    @State private var resourceShowsLibrary = false
    @State private var placeholderDestination: MIFDrawerDestination?
    @State private var approvalCount: Int?
    @State private var drawerOpen = false
    @State private var chatPath: [String] = []
    /// The Muse header's measured height over the main chat (`nmTopOverlay`), without the status bar.
    @State private var chatHeaderHeight: CGFloat = 0
    @State private var showNanoMuseSettings = false
    @State private var showSharedFolders = false
    @State private var showDevices = false
    @State private var showProjects = false
    @State private var showWorkspaceNotConnected = false
    @State private var agentName = NanoMuseHomeView.currentAgentName()
    /// Reduce Motion on: the bottom bar fades out for the keyboard instead of sliding down.
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    /// The agent's name from SOUL.md, "nanoMuse" until it has one.
    static func currentAgentName() -> String {
        let n = SoulStore.cachedMetadata.name.trimmingCharacters(in: .whitespacesAndNewlines)
        return n.isEmpty || n.lowercased() == "nanomuse" ? "morde" : n
    }

    /// Hidden while the keyboard is up (Android's IME) and while the chat's voice panel is
    /// open: the panel's own row — keyboard button, read-aloud, send — would otherwise sit
    /// under the bar with no way back to typing.
    private var bottomBarHidden: Bool {
        keyboard.visible || (tab == .chat && voiceMode.isVoiceActive)
    }

    private var taskHeaderMood: NanoMuseMood { MIFTaskPresentation.headerMood(for: mordeTasks.currentTask) }
    private var taskHeaderStatus: String? { mordeTasks.currentTask?.statusLabel }
    private var taskHeaderStatusColor: Color {
        switch mordeTasks.currentTask?.status {
        case "waiting_user", "paused": .orange
        case "completed": .green
        case "failed": .red
        case "queued", "running", "pausing", "cancelling": NanoMuseTones.actionText
        default: .secondary
        }
    }

    var body: some View {
        // The chat never gets the bottom bar as a `safeAreaInset`. 0.1.36 to 0.1.39 had it as
        // one, and an inset on the NavigationStack's ancestor reaches the chat inside it through
        // UIKit's safe area. What the device showed (iPad, 0.1.39; the maintainer's iPhone,
        // 0.1.36 to 0.1.38): the chat respected the bar's 56 pt at launch, and once the bar had
        // left for the keyboard and come back it no longer did: the chat ran under the bar and
        // its composer, bottom-aligned, sat behind it ("the input field shows until I dismiss
        // the keyboard, then never again"; the sliver of a composer's top edge above the bar in
        // the 0.1.36 screenshot). 0.1.40 made the bar a VStack row under the rooms, plain layout:
        // the chat is as tall as what is left over the bar, and over the keyboard, whether the
        // bar is there or not. The floating pill kept that for the chat as bottom padding of
        // the bar's height up to 1.0.3, which left a plain canvas behind the glass. Now the chat
        // gets the bar's height as an environment value instead (`nmBottomOverlay`): the chat
        // keeps its full height, its composer column stands above the bar and its rows scroll
        // under the column and the bar (NanoMuseComposerHost), so the pill blurs the transcript
        // the way it blurs the rooms. The rooms are plain SwiftUI scroll views with no
        // NavigationStack, so they take the bar's height as a safe-area inset: their content
        // scrolls under the pill too. The pill itself lies over both, aligned to the bottom,
        // inside the safe area.
        ZStack(alignment: .bottom) {
            ZStack {
                chatLayer
                    .environment(\.nmBottomOverlay, bottomBarHidden ? 0 : NanoMuseBottomBar.height)
                    .background(ChatColors.background.ignoresSafeArea())
                    .opacity(tab == .chat ? 1 : 0)
                    .allowsHitTesting(tab == .chat)
                    .accessibilityHidden(tab != .chat)
                if tab != .chat {
                    roomLayer
                        .safeAreaInset(edge: .bottom, spacing: 0) {
                            Color.clear
                                .frame(height: bottomBarHidden ? 0 : NanoMuseBottomBar.height)
                                .allowsHitTesting(false)
                        }
                        .transition(.opacity)
                }
            }
            // nanoMuse: honor Reduce Motion when switching root rooms.
            .animation(reduceMotion ? nil : .easeInOut(duration: 0.15), value: tab)
            if !bottomBarHidden {
                NanoMuseBottomBar(selected: $tab, approvalCount: approvalCount) { picked in
                    if picked == tab, picked != .chat { return }
                    tab = picked
                    resourceShowsLibrary = false
                    taskHomeDestination = .tasks
                    drawerDestination = Self.drawerDestination(for: picked)
                }
                .transition(reduceMotion ? .opacity : .move(edge: .bottom).combined(with: .opacity))
            }
        }
        .animation(.easeOut(duration: 0.2), value: bottomBarHidden)
        .overlay {
            NanoMuseDrawer(
                isOpen: $drawerOpen,
                agentName: agentName,
                selectedDestination: drawerDestination,
                onNavigate: openDrawerDestination,
                onOpenSession: { id in openCloudConversation(id) },
                onNewChat: { openSideChat(NanoMuseMainChat.draftPrefix + UUID().uuidString) },
                onSettings: { showNanoMuseSettings = true }
            )
        }
        .nanoMuseStudioPresenter(enabled: true)
        .nanoMuseAgentPagePresenter(
            enabled: true,
            onOpenSession: { id in openSideChat(id) },
            onPrefillChat: { text in prefillMainChat(text) },
            onOpenRoutines: nil
        )
        .sheet(isPresented: $showSharedFolders) {
            NanoMusePageSheet(title: AppLocalized("Shared Folders")) { SharedFoldersSettingsView() }
        }
        .sheet(isPresented: $showDevices) {
            NanoMusePageSheet(title: AppLocalized("Computers")) { NanoMuseComputersView() }
        }
        .sheet(isPresented: $showProjects) {
            MordeProjectsEntryView()
        }
        .alert(AppLocalized("Default"), isPresented: $showWorkspaceNotConnected) {
            Button(AppLocalized("OK"), role: .cancel) {}
        } message: {
            Text(AppLocalized("Not set") + " · " + AppLocalized("Not Supported"))
        }
        .sheet(isPresented: $showNanoMuseSettings) {
            NavigationStack {
                NanoMuseSettingsHomeView(onOpenCloudTasks: {
                    tab = .tasks
                    showNanoMuseSettings = false
                })
            }
        }
        .sheet(item: $placeholderDestination) { destination in
            NanoMusePageSheet(title: destination.title) {
                MIFPlaceholderPage(destination: destination)
            }
        }
        .onAppear {
            if main.chatId == nil { main.resolve() }
            // A notification tap that launched the app cold: the conversation it named.
            if let id = NotificationNavigationStore.shared.takePending() { openSideChat(id) }
        }
        .onReceive(NotificationCenter.default.publisher(for: .soulMdChanged)) { _ in
            agentName = Self.currentAgentName()
        }
        .onReceive(NotificationCenter.default.publisher(for: .nanoMuseHomeAction)) { note in
            // The Muse header's ••• menu on the rooms, and the rooms' own shortcuts.
            switch note.object as? String {
            case "settings": showNanoMuseSettings = true
            case "newTask":
                openSideChat(NanoMuseMainChat.draftPrefix + UUID().uuidString, focusComposer: true)
            case "computers": showDevices = true
            case "defaultWorkspace": showWorkspaceNotConnected = true
            case "sharedFolders": showSharedFolders = true
            case "askDevice":
                // C7: "Ask this device" in the Devices list — the main chat with "@<name> " in the composer.
                guard let text = note.userInfo?["text"] as? String else { return }
                showDevices = false
                showNanoMuseSettings = false
                prefillMainChat(text)
            default: break
            }
        }
        .onReceive(NotificationCenter.default.publisher(for: .nanoMuseMainChatAdopt)) { note in
            // C7: the account's main chat arrived while ours was still an empty draft.
            guard let id = note.object as? String else { return }
            main.adoptIfDraft(id)
        }
        .onReceive(NotificationCenter.default.publisher(for: .nanoMuseOpenChat)) { note in
            guard let id = note.object as? String else { return }
            openSideChat(id)
        }
        .onReceive(NotificationCenter.default.publisher(for: .nanoMuseOpenRoom)) { note in
            guard let destination = NanoMuseRoomDestination.resolve(note.object as? String) else {
                tab = .chat
                return
            }
            tab = destination.tab
        }
        .onReceive(NotificationCenter.default.publisher(for: .openSessionFromIntent)) { note in
            // A notification tap ("Check-in: … — open to run it") or a Shortcut: show that conversation.
            guard let id = note.userInfo?["sessionId"] as? String, !id.isEmpty else { return }
            openSideChat(id)
        }
        .onReceive(NotificationCenter.default.publisher(for: .newChatRequested)) { _ in
            // New Chat, ⌘N and context-limit actions all go through Morde.
            openSideChat(NanoMuseMainChat.draftPrefix + UUID().uuidString)
        }
        .nmOnChange(of: mordeAuthorization.personalDataScopeKey) { _ in
            approvalCount = nil
        }
        .nmOnChange(of: mordeAuthorization.signedIn) { _ in
            keyboard.reset()
        }
    }

    private static func drawerDestination(for tab: NanoMuseTab) -> MIFDrawerDestination {
        switch tab {
        case .chat: .chat
        case .tasks: .tasks
        case .approvals: .approvals
        case .resources: .resources
        }
    }

    private func openDrawerDestination(_ destination: MIFDrawerDestination) {
        drawerDestination = destination
        switch destination {
        case .chat:
            chatPath.removeAll()
            tab = .chat
            Task { await MIFConversationDrawerStore.shared.followMain() }
        case .tasks:
            taskHomeDestination = .tasks
            tab = .tasks
        case .approvals:
            tab = .approvals
        case .eventChain:
            taskHomeDestination = .eventChain
            tab = .tasks
        case .resources:
            resourceShowsLibrary = false
            tab = .resources
        case .library:
            resourceShowsLibrary = true
            tab = .resources
        case .workItems:
            taskHomeDestination = .workItems
            tab = .tasks
        case .projects:
            showProjects = true
        case .friends, .toolChats:
            placeholderDestination = destination
        }
    }

    /// Words in the main chat's composer ("Change your avatar to "), with the chat on screen.
    private func prefillMainChat(_ text: String) {
        chatPath.removeAll()
        tab = .chat
        drawerDestination = .chat
        // nanoMuse: Prefill the service-backed Morde composer instead of the retired local chat pipeline.
        NotificationCenter.default.post(name: .mordeShellPrompt, object: nil, userInfo: ["text": text])
    }

    private func openSideChat(_ id: String, focusComposer: Bool = false) {
        drawerOpen = false
        tab = .chat
        drawerDestination = .chat
        if id.hasPrefix(NanoMuseMainChat.draftPrefix) {
            chatPath.removeAll()
            NotificationCenter.default.post(name: .mordeShellNewConversation, object: nil,
                                            userInfo: focusComposer ? ["focusComposer": true] : nil)
            return
        }
        if id.range(of: #"^c[a-z0-9]{7}$"#, options: .regularExpression) != nil {
            openCloudConversation(id)
            return
        }
        chatPath = [id]
    }

    private func openCloudConversation(_ id: String) {
        drawerOpen = false
        tab = .chat
        drawerDestination = .chat
        chatPath.removeAll()
        Task { await MIFConversationDrawerStore.shared.choose(id) }
    }

    private func openLocalSession(_ id: String) {
        drawerOpen = false
        tab = .chat
        drawerDestination = .chat
        chatPath = [id]
    }

    // MARK: Chat layer

    private var chatLayer: some View {
        NavigationStack(path: $chatPath) {
            MordeConversationRoom(
                bottomOverlay: bottomBarHidden ? 0 : NanoMuseBottomBar.height
            ) // nanoMuse: pass the shell inset without coupling Morde to nanoMuse environment keys.
                .id("morde-main-chat")
            // Android: the header floats over the transcript, which scrolls under it. The header
            // is a safe-area inset with its measured height handed to the chat as `nmTopOverlay`:
            // the chat lets its message list extend under the header and gives the list that
            // much top content inset (AIChatView.messagesArea), so the rows pass under the face
            // the way they pass under the composer and the bottom bar. Behind the header a scrim
            // of the canvas colour fading to clear, not a blur and not a bar, and there is no
            // divider line (NanoMuseHeaderScrim). Up to 1.0.4 the inset alone shortened the
            // list, a UIKit view, and the rows stopped at a hard edge under the face.
            .environment(\.nmTopOverlay, chatHeaderHeight)
            .safeAreaInset(edge: .top, spacing: 0) {
                VStack(spacing: 0) {
                    // MuseHeader over the main chat — the face, the name pill, the drawer and ••• discs.
                    NanoMuseChatHeaderHost(
                        name: agentName,
                        onFace: { NotificationCenter.default.post(name: .nanoMuseOpenAgentPage, object: main.liveId ?? main.chatId) },
                        mood: taskHeaderMood,
                        status: taskHeaderStatus,
                        statusColor: taskHeaderStatusColor,
                        leading: { drawerDisc },
                        trailing: {
                            Color.clear
                                .frame(width: 38, height: 38)
                                .contentShape(Rectangle())
                                .contextMenu {
                                    Button {
                                        NotificationCenter.default.post(name: .mordeShellTaskOptions, object: nil)
                                    } label: {
                                        Label(AppLocalized("Task options"), systemImage: "slider.horizontal.3")
                                    }
                                    .accessibilityIdentifier("morde.shell.taskOptions")
                                    Button {
                                        NotificationCenter.default.post(name: .mordeShellVoiceInput, object: nil)
                                    } label: {
                                        Label(AppLocalized("Voice input"), systemImage: "mic")
                                    }
                                    Button {
                                        NotificationCenter.default.post(name: .mordeShellAttachmentOptions, object: nil)
                                    } label: {
                                        Label(AppLocalized("Add attachment"), systemImage: "paperclip")
                                    }
                                    .accessibilityIdentifier("morde.shell.attachments")
                                }
                                .accessibilityElement()
                                .accessibilityLabel(AppLocalized("Chat actions"))
                                .accessibilityHint(AppLocalized("Long press to open chat actions"))
                                .accessibilityIdentifier("morde.shell.chatActions")
                        }
                    )
                    // C1: the ask for a star, when NanoMuseStar's gate raises one; under the header, over the chat.
                    if let ask = star.pending {
                        NanoMuseStarCard(text: ask.text) {
                            star.dismiss()
                        }
                        .padding(.horizontal, 14)
                        .padding(.vertical, 8)
                        .background(NanoMuseTones.fill, in: RoundedRectangle(cornerRadius: 14, style: .continuous))
                        .padding(.horizontal, 12)
                        .padding(.bottom, 6)
                        .transition(reduceMotion ? .opacity : .move(edge: .top).combined(with: .opacity))
                    }
                    // C11 / parity #33: a turn refused for the free allowance — the ways on, pinned like the star card.
                    if let refused = allowance.pending {
                        ScrollView(showsIndicators: false) {
                            // nanoMuse: Keep relay guidance and invite actions without retrying a legacy local VM.
                            NanoMuseAllowanceCard(
                                refused: refused,
                                onDismiss: { allowance.dismiss() }
                            )
                            .padding(.horizontal, 14)
                            .padding(.vertical, 10)
                        }
                        .frame(maxHeight: 360)
                        .background(NanoMuseTones.fill, in: RoundedRectangle(cornerRadius: 14, style: .continuous))
                        .padding(.horizontal, 12)
                        .padding(.bottom, 6)
                        .transition(reduceMotion ? .opacity : .move(edge: .top).combined(with: .opacity))
                    }
                }
                .onGeometryChange(for: CGFloat.self) { proxy in
                    proxy.size.height
                } action: { height in
                    if abs(height - chatHeaderHeight) > 0.5 { chatHeaderHeight = height }
                }
                .background {
                    if dynamicTypeSize.isAccessibilitySize {
                        ChatColors.background.ignoresSafeArea(edges: .top)
                            .contentShape(Rectangle()).onTapGesture {}
                    } else {
                        NanoMuseHeaderScrim(scrolledUp: false).ignoresSafeArea(edges: .top)
                    }
                }
            }
            .background(ChatColors.background.ignoresSafeArea())
            // Android: a swipe in from the leading edge opens the drawer on the Chat tab; here too,
            // on the main chat (a pushed side chat keeps the system's back swipe).
            .background(NanoMuseEdgeSwipe(enabled: tab == .chat && chatPath.isEmpty && !drawerOpen) { drawerOpen = true })
            // The system bar stays out of the main chat; side chats pushed from here keep theirs.
            .toolbar(.hidden, for: .navigationBar)
            .animation(reduceMotion ? nil : .easeInOut(duration: 0.25), value: star.pending)
            .animation(reduceMotion ? nil : .easeInOut(duration: 0.25), value: allowance.pending)
            .navigationDestination(for: String.self) { id in
                NanoMuseLegacySessionArchive(sessionId: id)
                    .id(id)
                    // Historical sessions are read-only; new work goes through Morde.
                    .environment(\.nmSystemBarScrim, true)
                    .environment(\.nmTopOverlay, 0)
            }
        }
    }

    /// The round hamburger: the drawer with the side chats.
    private var drawerDisc: some View {
        NanoMuseRoundButton(symbol: "line.3.horizontal", label: AppLocalized("Chats and settings")) {
            drawerOpen = true
        }
        .accessibilityIdentifier("morde.shell.drawer")
    }

    // MARK: Rooms

    @ViewBuilder
    private var roomLayer: some View {
        switch tab {
        case .chat:
            EmptyView()
        case .tasks:
            // nanoMuse: Keep task and work-item pages on this room's stack while the shell owns root navigation.
            NavigationStack {
                MIFTaskWorkItemHome(chrome: chrome, destination: $taskHomeDestination)
            }
            .toolbar(.hidden, for: .navigationBar)
        case .approvals:
            NavigationStack {
                MIFApprovalRoom(onConfirmedCount: { approvalCount = $0 }, chrome: chrome)
            }
            .toolbar(.hidden, for: .navigationBar)
        case .resources:
            if resourceShowsLibrary {
                MIFLibraryRoot(chrome: chrome, personalDataRoot: NanoMuseDirs.root)
            } else {
                MIFResourcesRoot(chrome: chrome)
            }
        }
    }

    /// What the rooms need for their Muse header.
    private var chrome: NanoMuseRoomChrome {
        NanoMuseRoomChrome(
            liveId: main.liveId,
            name: agentName,
            hasMainSession: !main.isDraft && main.chatId != nil,
            onMenu: { drawerOpen = true },
            showsLibraryShortcuts: false
        )
    }
}

extension Notification.Name {
    /// `object` is a session id (or a `__new__…` draft id): the shell shows it.
    static let nanoMuseOpenChat = Notification.Name("nanoMuse.openChat")
    static let mordeShellPrompt = Notification.Name("morde.shellPrompt")
    static let mordeShellNewConversation = Notification.Name("morde.shellNewConversation")
    static let mordeShellTaskOptions = Notification.Name("morde.shellTaskOptions")
    static let mordeShellVoiceInput = Notification.Name("morde.shellVoiceInput")
    static let mordeShellAttachmentOptions = Notification.Name("morde.shellAttachmentOptions")
}

/// Legacy sessions remain readable without opening the local execution UI.
struct NanoMuseLegacySessionsList: View {
    var onOpenSession: (String) -> Void
    @State private var sessions: [ChatSession] = []
    @State private var isLoading = true

    var body: some View {
        List {
            if isLoading {
                ProgressView().frame(maxWidth: .infinity)
            } else if sessions.isEmpty {
                Text(AppLocalized("No saved conversations"))
                    .foregroundStyle(.secondary)
                    .frame(maxWidth: .infinity, alignment: .center)
            } else {
                ForEach(sessions) { session in
                    Button { onOpenSession(session.id) } label: {
                        VStack(alignment: .leading, spacing: 4) {
                            HStack {
                                Text((session.title?.isEmpty == false ? session.title : nil) ?? AppLocalized("Conversation"))
                                    .foregroundStyle(.primary)
                                    .lineLimit(1)
                                Spacer()
                                Text(session.updatedAt, style: .relative)
                                    .font(.caption2)
                                    .foregroundStyle(.tertiary)
                            }
                            if let preview = session.lastMessage, !preview.isEmpty {
                                Text(preview)
                                    .font(.footnote)
                                    .foregroundStyle(.secondary)
                                    .lineLimit(2)
                            }
                            if session.isRemote, let device = session.remoteDeviceName, !device.isEmpty {
                                Text("From \(device)")
                                    .font(.caption)
                                    .foregroundStyle(.tertiary)
                            }
                        }
                        .contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                }
            }
        }
        .listStyle(.plain)
        .task { await loadSessions() }
    }

    @MainActor
    private func loadSessions() async {
        let stored = await ChatStore.shared.listSessions()
        sessions = NanoMuseSync.shared.visible(stored).sorted { $0.updatedAt > $1.updatedAt }
        isLoading = false
    }
}

// MARK: - The column on a wide window

/// The chat on a regular width (an iPad, a wide Split View pane): the message list, the
/// composer pill and the bottom bar share one centred column, so the conversation reads as one
/// column and not as three widths. Up to 1.0.3 the list was capped at 900 pt, the composer with
/// it and the bar at 420 pt: on an iPad in portrait the composer ran across the window, the bar
/// sat 420 pt wide under it and the agent's replies stayed in a 340 pt pill at the left edge.
/// The column is 760 pt, the width the web console gives its chat (`max-w-[760px]` in
/// `web/src/App.tsx`); the rows keep their 16 pt padding inside it and the two pills are as wide
/// as the rows' content, so their edges line up with the bubbles'. A compact width (every
/// iPhone, a narrow pane) is unchanged: the rows and the composer take the window and the bar
/// keeps its 420 pt cap, which no phone reaches.
enum NanoMuseColumn {
    /// The column's outer width, the rows' 16 pt padding included.
    static let width: CGFloat = 760
    /// The rows' horizontal padding, the composer's margins and the bar's inset: one number.
    static let margin: CGFloat = 16
    /// The composer pill and the bottom bar on a regular width: the column less its margins.
    static var pillWidth: CGFloat { width - margin * 2 }
    /// The agent's reply pill on a regular width, the web console's `wide:max-w-[600px]`; on a
    /// compact one it stays 340 pt, as Android's NmAssistantBubble.
    static let agentPillWidth: CGFloat = 600

    /// The width of a pill for a size class: the column's on a regular width, `fallback` on a
    /// compact one (the bar's 420 pt cap; nothing for the composer, which takes the window).
    static func pillWidth(for sizeClass: UserInterfaceSizeClass?, otherwise fallback: CGFloat?) -> CGFloat? {
        sizeClass == .regular ? pillWidth : fallback
    }

    /// The agent's reply pill for a size class.
    static func agentPillWidth(for sizeClass: UserInterfaceSizeClass?) -> CGFloat {
        sizeClass == .regular ? agentPillWidth : 340
    }
}

// MARK: - Bottom bar

// nanoMuse: the restored iOS shell has four root destinations.
/// Android's MuseBottomBar: a floating frosted-glass pill a little above the bottom edge, four
/// glyphs and no visible labels. The pill is `.ultraThinMaterial` under the shell's surface tone at 55
/// percent, with a 1 pt hairline; the current tab shows its filled glyph on a soft indicator
/// capsule that springs across to the tab tapped next, the others stay outlined and muted. The
/// pill is as wide as the composer above it (the same 16 pt margins): capped at 420 pt on a
/// compact width, the column's width on a regular one (NanoMuseColumn).
struct NanoMuseBottomBar: View {
    @Binding var selected: NanoMuseTab
    var approvalCount: Int? = nil
    var onPick: (NanoMuseTab) -> Void
    @Namespace private var indicator
    /// Reduce Motion on: the indicator moves to the tapped tab without the spring.
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    /// Regular on an iPad: the pill takes the column's width, the composer's.
    @Environment(\.horizontalSizeClass) private var sizeClass

    /// The indicator capsule behind the selected glyph, centred in its slot; the two slots share
    /// the pill's width equally.
    static let tabSize = CGSize(width: 60, height: 46)
    /// The pill's padding above and below the slots.
    static let pillPadding: CGFloat = 6
    /// Between the pill and the bottom safe area.
    static let bottomGap: CGFloat = 12
    /// The pill's distance from the screen's sides: the composer's margins in the shell
    /// (AIChatView, `nmPill`), so the two capsules stack with one edge.
    static let inset: CGFloat = 16
    /// The pill's cap on a compact width (an iPhone's composer never reaches it); a regular width
    /// takes `NanoMuseColumn.pillWidth` instead, so the pill is as wide as the composer there too.
    static let maxWidth: CGFloat = 420
    /// How much of the screen the bar takes above the safe area: the rooms' bottom safe-area
    /// inset and the chat's `nmBottomOverlay`, so their content scrolls under the pill and stops
    /// above it at rest; the glass blurs whatever passes behind.
    static var height: CGFloat { tabSize.height + pillPadding * 2 + bottomGap }

    var body: some View {
        HStack(spacing: 2) {
            ForEach(NanoMuseTab.allCases) { tab in
                let isSelected = selected == tab
                Button {
                    onPick(tab)
                } label: {
                    Group {
                        if tab == .resources {
                            NanoMuseCategoryShape(filled: isSelected)
                                .fill(style: FillStyle(eoFill: true))
                                .frame(width: 24, height: 24)
                        } else {
                            Image(systemName: isSelected ? tab.selectedSymbol : tab.symbol)
                                .font(.system(size: 22, weight: isSelected ? .semibold : .regular))
                        }
                    }
                    .overlay(alignment: .topTrailing) {
                        if tab == .approvals, let approvalCount, approvalCount > 0 {
                            Text(approvalCount > 99 ? "99+" : "\(approvalCount)")
                                .font(.footnote.weight(.bold))
                                .foregroundStyle(.white)
                                .fixedSize()
                                .padding(.horizontal, approvalCount > 9 ? 6 : 5)
                                .padding(.vertical, 3)
                                .background(Color(red: 0.70, green: 0.08, blue: 0.14), in: Capsule())
                                .offset(x: 6, y: -5)
                        }
                    }
                    .foregroundStyle(isSelected ? NanoMuseTones.tabIndicatorGlyph : Color.secondary)
                    // The capsule keeps its 60 by 46 (narrower only when the slot is).
                    .frame(maxWidth: Self.tabSize.width)
                    .frame(height: Self.tabSize.height)
                    .background {
                        if isSelected {
                            Capsule()
                                .fill(NanoMuseTones.tabIndicator)
                                .matchedGeometryEffect(id: "indicator", in: indicator)
                        }
                    }
                    // The slot: an equal share of the pill, all of it tappable.
                    .frame(maxWidth: .infinity)
                    .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .accessibilityLabel(Text(tab.title))
                .accessibilityValue(Text(verbatim: tab == .approvals ? (approvalCount?.formatted() ?? "") : ""))
                .accessibilityIdentifier("morde.shell.tab.\(tab.rawValue)")
                .accessibilityAddTraits(isSelected ? .isSelected : [])
                .accessibilityRemoveTraits(isSelected ? [] : .isSelected)
            }
        }
        .padding(.horizontal, 14)
        .padding(.vertical, Self.pillPadding)
        .background {
            Capsule().fill(.ultraThinMaterial)
            Capsule().fill(NanoMuseTones.surface.opacity(0.55))
        }
        .overlay(Capsule().strokeBorder(NanoMuseTones.hairline, lineWidth: 1))
        .animation(reduceMotion ? nil : Animation.spring(response: 0.35, dampingFraction: 0.8), value: selected)
        .frame(maxWidth: NanoMuseColumn.pillWidth(for: sizeClass, otherwise: Self.maxWidth))
        .frame(maxWidth: .infinity)
        .padding(.horizontal, Self.inset)
        .padding(.bottom, Self.bottomGap)
    }
}

/// Material's `Category` icon (top triangle, bottom-left square, bottom-right circle on a 24x24
/// grid), matching Android's Library tab icon (`Icons.Outlined.Category` / `Icons.Filled.Category`).
private struct NanoMuseCategoryShape: Shape {
    let filled: Bool

    func path(in rect: CGRect) -> Path {
        let s = min(rect.width, rect.height) / 24.0
        let ox = rect.minX + (rect.width - 24.0 * s) / 2.0
        let oy = rect.minY + (rect.height - 24.0 * s) / 2.0
        func pt(_ x: CGFloat, _ y: CGFloat) -> CGPoint {
            CGPoint(x: ox + x * s, y: oy + y * s)
        }
        func box(_ x: CGFloat, _ y: CGFloat, _ w: CGFloat, _ h: CGFloat) -> CGRect {
            CGRect(x: ox + x * s, y: oy + y * s, width: w * s, height: h * s)
        }

        var p = Path()
        p.move(to: pt(12, 2))
        p.addLine(to: pt(6.5, 11))
        p.addLine(to: pt(17.5, 11))
        p.closeSubpath()
        if !filled {
            p.move(to: pt(12, 5.84))
            p.addLine(to: pt(13.93, 9))
            p.addLine(to: pt(10.07, 9))
            p.closeSubpath()
        }

        p.addEllipse(in: box(13, 13, 9, 9))
        if !filled {
            p.addEllipse(in: box(15, 15, 5, 5))
        }

        p.addRect(box(3, 13.5, 8, 8))
        if !filled {
            p.addRect(box(5, 15.5, 4, 4))
        }
        return p
    }
}

// MARK: - Room header

extension Notification.Name {
    /// `object` is one of "settings", "routines", "computers", or "defaultWorkspace".
    /// "systemFiles", "sharedFolders", or "chatFiles": the home opens that page over the current tab.
    static let nanoMuseHomeAction = Notification.Name("nanoMuse.homeAction")
}

/// What the Library room needs to draw the Muse header: the
/// main chat's live id (its mood and status stay on the face while the
/// person is elsewhere), the agent's name, and the drawer.
struct NanoMuseRoomChrome {
    var liveId: String?
    var name: String
    /// The main chat is a real session (its files can be browsed).
    var hasMainSession: Bool
    var onMenu: () -> Void
    var onBack: (() -> Void)? = nil
    var showsLibraryShortcuts = false

    /// Ask the home to open one of its pages ("settings", "systemFiles", …).
    func open(_ page: String) {
        NotificationCenter.default.post(name: .nanoMuseHomeAction, object: page)
    }
}

/// The big-face header on the two rooms: the drawer disc on the left, the
/// room's own trailing content on the right (Android: NanoMuseHome.TabHeader).
struct NanoMuseRoomHeader<Trailing: View>: View {
    var chrome: NanoMuseRoomChrome
    @ViewBuilder var trailing: () -> Trailing

    init(chrome: NanoMuseRoomChrome, @ViewBuilder trailing: @escaping () -> Trailing) {
        self.chrome = chrome
        self.trailing = trailing
    }

    var body: some View {
        NanoMuseChatHeaderHost(
            name: chrome.name,
            onFace: { NotificationCenter.default.post(name: .nanoMuseOpenAgentPage, object: chrome.liveId) },
            leading: {
                HStack(spacing: 8) {
                    if let onBack = chrome.onBack {
                        NanoMuseRoundButton(symbol: "chevron.left", label: AppLocalized("Back"), action: onBack)
                    }
                    NanoMuseRoundButton(symbol: "line.3.horizontal", label: AppLocalized("Chats and settings"), action: chrome.onMenu)
                    if chrome.showsLibraryShortcuts {
                        Menu {
                            Button { chrome.open("computers") } label: {
                                Label(AppLocalized("Computers"), systemImage: "desktopcomputer")
                            }
                            Button { chrome.open("defaultWorkspace") } label: {
                                Label(AppLocalized("Default"), systemImage: "folder")
                            }
                        } label: {
                            NanoMuseRoundDisc(symbol: "square.stack.3d.up")
                        }
                        .accessibilityLabel(Text(AppLocalized("Library")))
                    }
                }
            },
            trailing: trailing
        )
    }
}

/// The ••• menu of a room: the room's own entries first, then System files and Settings, as Android.
struct NanoMuseRoomMenu<Items: View>: View {
    var chrome: NanoMuseRoomChrome
    @ViewBuilder var items: () -> Items

    init(chrome: NanoMuseRoomChrome, @ViewBuilder items: @escaping () -> Items) {
        self.chrome = chrome
        self.items = items
    }

    var body: some View {
        Menu {
            items()
#if !MINIS_CLOUD_ONLY
            Button { chrome.open("systemFiles") } label: { Label(AppLocalized("System files"), systemImage: "doc.text") }
#endif
            Button { chrome.open("settings") } label: { Label(AppLocalized("Settings"), systemImage: "gearshape") }
        } label: {
            NanoMuseRoundDisc(symbol: "ellipsis")
        }
        .accessibilityLabel(Text(AppLocalized("More")))
    }
}

/// The big-face header for the Morde conversation; legacy local sessions stay read-only.
struct NanoMuseChatHeaderHost<Leading: View, Trailing: View>: View {
    var name: String
    var onFace: () -> Void
    var mood: NanoMuseMood = .idle
    var status: String? = nil
    var statusColor: Color = .secondary
    @ViewBuilder var leading: () -> Leading
    @ViewBuilder var trailing: () -> Trailing

    var body: some View {
        NanoMuseMuseHeader(mood: mood, name: name, status: status, statusColor: statusColor,
                           onFace: onFace, leading: leading, trailing: trailing)
    }
}

/// A page in a sheet: a navigation stack with a Done button.
struct NanoMusePageSheet<Content: View>: View {
    var title: String
    @ViewBuilder var content: () -> Content
    @Environment(\.dismiss) private var dismiss

    init(title: String, @ViewBuilder content: @escaping () -> Content) {
        self.title = title
        self.content = content
    }

    var body: some View {
        NavigationStack {
            content()
                .navigationTitle(title)
                .navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) {
                        Button(AppLocalized("Done")) { dismiss() }
                            .tint(NanoMuseTones.actionText)
                        .accessibilityIdentifier("morde.sheet.done")
                    }
                }
        }
    }
}

private struct NanoMuseLegacyChatFilesArchive: View {
    @State private var report: LegacyRootfsArchive.CopyReport?
    @State private var failure: String?
    @State private var retryID = UUID()

    private var documents: URL {
        FileManager.default.urls(for: .documentDirectory, in: .userDomainMask)[0]
    }

    private var archiveRoot: URL {
        documents.appendingPathComponent(LegacyRootfsArchive.archiveDirectoryName, isDirectory: true)
    }

    var body: some View {
        Group {
            if let failure {
                VStack(spacing: 12) {
                    Text(AppLocalized("Could not preserve the old chat files."))
                        .font(.headline)
                    Text(failure)
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                        .textSelection(.enabled)
                    Button(AppLocalized("Retry")) {
                        self.failure = nil
                        self.report = nil
                        retryID = UUID()
                    }
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .padding()
            } else if let report, report.sourceRootsFound > 0 {
                VStack(spacing: 0) {
                    if !report.sourceRootsMissing.isEmpty {
                        Text(AppLocalized("One old folder was absent; only existing folders were copied. Original files remain in place."))
                            .font(.footnote)
                            .foregroundStyle(.secondary)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .padding(.horizontal)
                            .padding(.vertical, 8)
                    }
                    NanoMuseReadOnlyFileBrowser(root: archiveRoot, directory: archiveRoot)
                }
            } else if report != nil {
                VStack(spacing: 10) {
                    Image(systemName: "folder")
                        .font(.largeTitle)
                        .foregroundStyle(.secondary)
                    Text(AppLocalized("No old chat files found"))
                        .foregroundStyle(.secondary)
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                ProgressView(AppLocalized("Preserving old chat files…"))
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }
        .task(id: retryID) { await preserve() }
    }

    private func preserve() async {
        let directory = documents
        do {
            let result = try await Task.detached(priority: .utility) {
                try LegacyRootfsArchive.preserveUserData(in: directory)
            }.value
            report = result
        } catch {
            failure = error.localizedDescription
        }
    }
}

private struct NanoMuseReadOnlyFileBrowser: View {
    private struct Entry: Identifiable {
        let url: URL
        let isDirectory: Bool
        let size: Int
        var id: String { url.path }
    }

    private struct Preview: Identifiable {
        let url: URL
        var id: String { url.path }
    }

    private static let textExtensions: Set<String> = ["md", "txt", "json", "csv", "log", "yaml", "yml", "toml", "xml", "html", "css", "js", "py", "sh", "conf", "ini"]
    private static let imageExtensions: Set<String> = ["png", "jpg", "jpeg", "gif", "heic", "webp"]
    private static let maxTextBytes = 512 * 1024
    private static let maxImageBytes = 12 * 1024 * 1024

    let root: URL
    let directory: URL
    @State private var entries: [Entry] = []
    @State private var preview: Preview?
    @State private var previewText: String?
    @State private var previewImage: UIImage?

    var body: some View {
        List {
            if entries.isEmpty {
                Text(AppLocalized("This folder is empty."))
                    .foregroundStyle(.secondary)
            }
            ForEach(entries) { entry in
                if entry.isDirectory {
                    NavigationLink {
                        NanoMuseReadOnlyFileBrowser(root: root, directory: entry.url)
                    } label: {
                        entryLabel(entry, symbol: "folder")
                    }
                } else {
                    Button { showPreview(entry) } label: {
                        entryLabel(entry, symbol: Self.imageExtensions.contains(entry.url.pathExtension.lowercased()) ? "photo" : "doc.text")
                    }
                    .buttonStyle(.plain)
                    .disabled(!Self.canPreview(entry))
                }
            }
        }
        .navigationTitle(directory == root ? AppLocalized("Chat files") : directory.lastPathComponent)
        .navigationBarTitleDisplayMode(.inline)
        .task { reload() }
        .sheet(item: $preview) { item in
            NavigationStack {
                Group {
                    if let previewImage {
                        ScrollView([.horizontal, .vertical]) {
                            Image(uiImage: previewImage)
                                .resizable()
                                .scaledToFit()
                                .padding()
                        }
                    } else if let previewText {
                        ScrollView {
                            Text(previewText)
                                .font(.system(.body, design: .monospaced))
                                .textSelection(.enabled)
                                .frame(maxWidth: .infinity, alignment: .leading)
                                .padding()
                        }
                    } else {
                        VStack(spacing: 8) {
                            Image(systemName: "doc.questionmark")
                                .font(.largeTitle)
                                .foregroundStyle(.secondary)
                            Text(AppLocalized("Preview unavailable"))
                                .foregroundStyle(.secondary)
                        }
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                    }
                }
                .navigationTitle(item.url.lastPathComponent)
                .navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .confirmationAction) {
                        Button(AppLocalized("Done")) { preview = nil }
                    }
                }
            }
        }
    }

    private func entryLabel(_ entry: Entry, symbol: String) -> some View {
        HStack(spacing: 10) {
            Image(systemName: symbol).foregroundStyle(.secondary)
            Text(entry.url.lastPathComponent)
                .lineLimit(1)
                .truncationMode(.middle)
            Spacer(minLength: 8)
            if !entry.isDirectory {
                Text(ByteCountFormatter.string(fromByteCount: Int64(entry.size), countStyle: .file))
                    .font(.caption)
                    .foregroundStyle(.tertiary)
            }
        }
        .contentShape(Rectangle())
    }

    private func reload() {
        guard LegacyRootfsArchive.isContainedAfterResolvingSymlinks(directory, under: root),
              let children = try? FileManager.default.contentsOfDirectory(
                at: directory,
                includingPropertiesForKeys: [.isDirectoryKey, .isSymbolicLinkKey, .fileSizeKey],
                options: [.skipsHiddenFiles]
              ) else {
            entries = []
            return
        }

        entries = children.compactMap { url in
            guard let values = try? url.resourceValues(forKeys: [.isDirectoryKey, .isSymbolicLinkKey, .fileSizeKey]),
                  values.isSymbolicLink != true else { return nil }
            guard LegacyRootfsArchive.isContainedAfterResolvingSymlinks(url, under: root) else { return nil }
            return Entry(url: url, isDirectory: values.isDirectory == true, size: values.fileSize ?? 0)
        }
        .sorted {
            if $0.isDirectory != $1.isDirectory { return $0.isDirectory }
            return $0.url.lastPathComponent.localizedStandardCompare($1.url.lastPathComponent) == .orderedAscending
        }
    }

    private func showPreview(_ entry: Entry) {
        guard Self.canPreview(entry), LegacyRootfsArchive.isContainedAfterResolvingSymlinks(entry.url, under: root) else { return }
        previewImage = nil
        previewText = nil
        let ext = entry.url.pathExtension.lowercased()
        if Self.imageExtensions.contains(ext), entry.size <= Self.maxImageBytes {
            previewImage = UIImage(contentsOfFile: entry.url.path)
        } else if Self.textExtensions.contains(ext), entry.size <= Self.maxTextBytes {
            previewText = try? String(contentsOf: entry.url, encoding: .utf8)
        }
        preview = Preview(url: entry.url)
    }

    private static func canPreview(_ entry: Entry) -> Bool {
        let ext = entry.url.pathExtension.lowercased()
        return (textExtensions.contains(ext) && entry.size <= maxTextBytes)
            || (imageExtensions.contains(ext) && entry.size <= maxImageBytes)
    }

}

// MARK: - Drawer

enum MIFDrawerDestination: String, CaseIterable, Identifiable {
    case chat, tasks, approvals, eventChain, resources, library, workItems, friends, projects, toolChats

    var id: String { rawValue }

    var title: String {
        switch self {
        case .chat: AppLocalized("Main chat")
        case .tasks: AppLocalized("Tasks")
        case .approvals: AppLocalized("Approvals")
        case .eventChain: AppLocalized("Event chain")
        case .resources: AppLocalized("Resources")
        case .library: AppLocalized("Library")
        case .workItems: AppLocalized("Work items")
        case .friends: AppLocalized("Friends")
        case .projects: AppLocalized("Project")
        case .toolChats: AppLocalized("AI tool conversations")
        }
    }

    var symbol: String {
        switch self {
        case .chat: "bubble.left"
        case .tasks: "checklist"
        case .approvals: "checkmark.shield"
        case .eventChain: "point.3.connected.trianglepath.dotted"
        case .resources: "square.grid.2x2"
        case .library: "folder"
        case .workItems: "list.bullet.rectangle"
        case .friends: "person.2"
        case .projects: "person.3"
        case .toolChats: "terminal"
        }
    }

    static let tabItems: [Self] = [.chat, .tasks, .approvals, .eventChain, .resources, .library, .workItems, .friends]
    static let collaborationItems: [Self] = [.projects]
}

/// The Morde drawer follows the approved design destinations and keeps cloud chats separate
/// from legacy local history.
struct NanoMuseDrawer: View {
    @Binding var isOpen: Bool
    var agentName: String
    var selectedDestination: MIFDrawerDestination
    var onNavigate: (MIFDrawerDestination) -> Void
    var onOpenSession: (String) -> Void
    var onNewChat: () -> Void
    var onSettings: () -> Void

    @ObservedObject private var cloudChats = MIFConversationDrawerStore.shared
    @State private var query = ""
    @State private var dragOffset: CGFloat = 0
    /// Reduce Motion on: the panel fades in instead of sliding in from the edge.
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    /// The agent's name follows Dynamic Type, capped as the header's lines are (NanoMuseHeaderMetrics).
    @ScaledMetric(relativeTo: .title2) private var nameSize: CGFloat = 24

    /// The panel's width: most of the window, at most 320 pt. Measured from the window the
    /// drawer is in rather than the screen, so an iPad window narrower than the screen (Stage
    /// Manager, Split View) gets a drawer that fits it.
    private func width(in container: CGFloat) -> CGFloat { min(320, container * 0.82) }

    var body: some View {
        GeometryReader { geo in
            ZStack(alignment: .leading) {
                if isOpen {
                    Color.black.opacity(0.35)
                        .ignoresSafeArea()
                        .onTapGesture { close() }
                        .transition(.opacity)
                    panel(width: width(in: geo.size.width))
                        .frame(width: width(in: geo.size.width))
                        .offset(x: min(0, dragOffset))
                        .transition(reduceMotion ? .opacity : .move(edge: .leading))
                        .gesture(
                            DragGesture()
                                .onChanged { v in dragOffset = v.translation.width }
                                .onEnded { v in
                                    if v.translation.width < -60 { close() }
                                    dragOffset = 0
                                }
                        )
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .leading)
        }
        .allowsHitTesting(isOpen)
        .animation(.easeInOut(duration: 0.22), value: isOpen)
    }

    /// Search only covers conversation titles currently loaded from Morde.
    private var filtered: [MIFConversationDrawerItem] {
        let q = query.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
        let sideChats = cloudChats.conversations.filter { $0.id != cloudChats.mainID }
        guard !q.isEmpty else { return sideChats }
        return sideChats.filter { $0.title.lowercased().contains(q) }
    }

    private func panel(width: CGFloat) -> some View {
        VStack(alignment: .leading, spacing: 0) {
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 4) {
                    Text(agentName)
                        .font(.system(size: NanoMuseHeaderMetrics.capped(nameSize, base: 24), weight: .bold))
                        .lineLimit(1)
                        .padding(.horizontal, 20)
                        .padding(.top, 18)
                        .padding(.bottom, 12)
                    Text(AppLocalized("Tabs"))
                        .font(.footnote.weight(.semibold))
                        .foregroundStyle(.secondary)
                        .padding(.horizontal, 20)
                        .padding(.top, 2)
                    Text(AppLocalized("Long press to reorder"))
                        .font(.caption)
                        .foregroundStyle(.tertiary)
                        .padding(.horizontal, 20)
                        .padding(.bottom, 4)

                    ForEach(MIFDrawerDestination.tabItems) { destination in
                        destinationRow(destination)
                    }

                    sectionLabel(AppLocalized("Collaboration"))
                        .padding(.top, 12)
                    ForEach(MIFDrawerDestination.collaborationItems) { destination in
                        destinationRow(destination)
                    }

                    sectionLabel(AppLocalized("Side chats"))
                        .padding(.top, 12)
                    if cloudChats.isLoading && cloudChats.conversations.isEmpty {
                        ProgressView().frame(maxWidth: .infinity).padding(.vertical, 18)
                    } else if let error = cloudChats.error, cloudChats.conversations.isEmpty {
                        Text(error)
                            .font(.footnote)
                            .foregroundStyle(.orange)
                            .padding(.horizontal, 20)
                            .padding(.vertical, 12)
                    } else if filtered.isEmpty {
                        Text(query.isEmpty ? AppLocalized("Start a side chat") : AppLocalized("No results"))
                            .font(.subheadline)
                            .foregroundStyle(.secondary)
                            .padding(.horizontal, 20)
                            .padding(.vertical, 12)
                            .accessibilityIdentifier(query.isEmpty
                                ? "morde.shell.sideChats.empty" : "morde.shell.conversationSearch.empty")
                    } else {
                        ForEach(filtered) { session in
                            Button {
                                close()
                                onOpenSession(session.id)
                            } label: {
                                row(session)
                            }
                            .buttonStyle(.plain)
                            .accessibilityIdentifier("morde.shell.cloudConversation.\(session.id)")
                            .onAppear {
                                guard session.id == filtered.last?.id,
                                      cloudChats.nextCursor != nil,
                                      cloudChats.error == nil,
                                      !cloudChats.loadingMore else { return }
                                Task { await cloudChats.loadMore() }
                            }
                        }
                    }

                    if cloudChats.loadingMore {
                        ProgressView().frame(maxWidth: .infinity).padding(.vertical, 10)
                    }
                    if let error = cloudChats.error, !cloudChats.conversations.isEmpty {
                        Text(error)
                            .font(.caption)
                            .foregroundStyle(.orange)
                            .padding(.horizontal, 20)
                    }

                    sectionLabel(AppLocalized("AI tool conversations"))
                        .padding(.top, 12)
                    Button {
                        close()
                        onNavigate(.toolChats)
                    } label: {
                        HStack(spacing: 12) {
                            Image(systemName: MIFDrawerDestination.toolChats.symbol).frame(width: 24)
                            Text(AppLocalized("Not implemented yet"))
                                .font(.subheadline)
                                .foregroundStyle(.secondary)
                            Spacer(minLength: 0)
                        }
                        .padding(.horizontal, 20)
                        .padding(.vertical, 10)
                        .contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                    .accessibilityIdentifier("morde.shell.toolChats")
                }
                .padding(.bottom, 12)
            }
            .accessibilityIdentifier("morde.shell.drawer.scroll")

            Divider()
            HStack(spacing: 6) {
                stripButton(symbol: "gearshape", label: AppLocalized("Settings")) {
                    close()
                    onSettings()
                }
                .accessibilityIdentifier("morde.shell.settings")
                HStack(spacing: 6) {
                    Image(systemName: "magnifyingglass")
                        .font(.system(size: 14))
                        .foregroundStyle(.secondary)
                    TextField(AppLocalized("Search"), text: $query)
                        .font(.subheadline)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .accessibilityIdentifier("morde.shell.search")
                }
                .padding(.horizontal, 10)
                .padding(.vertical, 7)
                .background(NanoMuseTones.fill, in: Capsule())
                stripButton(symbol: "square.and.pencil", label: AppLocalized("New side chat")) {
                    close()
                    onNewChat()
                }
                .accessibilityIdentifier("morde.shell.newSideChat")
            }
            .padding(.horizontal, 8)
            .padding(.vertical, 8)
        }
        .background(NanoMuseTones.surface.ignoresSafeArea())
        .task(id: isOpen) {
            guard isOpen, !cloudChats.isLoading,
                  !cloudChats.hasLoaded || cloudChats.error != nil else { return }
            await cloudChats.retry()
        }
    }

    private func sectionLabel(_ title: String) -> some View {
        Text(title)
            .font(.footnote.weight(.semibold))
            .foregroundStyle(.secondary)
            .padding(.horizontal, 20)
    }

    private func destinationRow(_ destination: MIFDrawerDestination) -> some View {
        Button {
            close()
            onNavigate(destination)
        } label: {
            HStack(spacing: 12) {
                Image(systemName: destination.symbol)
                    .font(.system(size: 17))
                    .frame(width: 24)
                Text(destination.title).font(.body)
                Spacer(minLength: 0)
            }
            .foregroundStyle(.primary)
            .padding(.horizontal, 20)
            .padding(.vertical, 10)
            .background(selectedDestination == destination ? NanoMuseTones.fill : Color.clear,
                        in: RoundedRectangle(cornerRadius: 12, style: .continuous))
            .padding(.horizontal, 8)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityIdentifier(destination == .chat ? "morde.shell.mainChat" : "morde.shell.drawer.\(destination.rawValue)")
    }

    private func stripButton(symbol: String, label: String, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Image(systemName: symbol)
                .font(.system(size: 18))
                .foregroundStyle(.primary)
                .frame(width: 40, height: 36)
                .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel(Text(label))
    }

    private func row(_ session: MIFConversationDrawerItem) -> some View {
        HStack(spacing: 10) {
            VStack(alignment: .leading, spacing: 2) {
                Text(session.title.isEmpty ? AppLocalized("New chat") : session.title)
                    .font(.body)
                    .foregroundStyle(.primary)
                    .lineLimit(1)
                if session.isMain {
                    Text(AppLocalized("Main chat"))
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                }
            }
            Spacer(minLength: 0)
        }
        .contentShape(Rectangle())
    }

    private func close() {
        isOpen = false
    }

    private static let relative: RelativeDateTimeFormatter = {
        let formatter = RelativeDateTimeFormatter()
        formatter.unitsStyle = .short
        return formatter
    }()

    static func when(_ date: Date) -> String {
        relative.localizedString(for: date, relativeTo: Date())
    }

}
