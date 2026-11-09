//
//  NanoMuseHeader.swift
//  nanoMuse
//
//  VM-backed header hosts for the legacy AIChatView. Shared header views,
//  status, and mood types live in NanoMuseSharedChatViews.swift.
//

import SwiftUI
import Combine

/// Finds a conversation's view model in the cache. AIChatView puts it there as it appears, so
/// the header and the scrim look a few times, then give up quietly.
enum NanoMuseChatLookup {
    @MainActor
    static func viewModel(for liveId: String?) async -> AIChatViewModel? {
        guard let liveId else { return nil }
        for _ in 0..<20 {
            if let found = ViewModelCache.shared.get(for: liveId) { return found }
            try? await Task.sleep(for: .milliseconds(150))
            if Task.isCancelled { return nil }
        }
        return nil
    }
}

/// The scrim bound to the main chat's view model once it is in the cache (the same wait as
/// `NanoMuseChatHeaderHost`): the second fade follows the list's `isNearBottom`. Until the
/// model exists there is nothing to scroll, so the scrim stands still.
struct NanoMuseChatScrimHost: View {
    /// The id the view model is cached under (nil for an unsent draft).
    var liveId: String?

    @State private var vm: AIChatViewModel?

    var body: some View {
        Group {
            if let vm {
                NanoMuseLiveScrim(vm: vm)
            } else {
                NanoMuseHeaderScrim(scrolledUp: false)
            }
        }
        .task(id: liveId) {
            vm = nil
            vm = await NanoMuseChatLookup.viewModel(for: liveId)
        }
    }
}

/// The scrim watching one conversation's list position.
struct NanoMuseLiveScrim: View {
    @ObservedObject var vm: AIChatViewModel

    var body: some View {
        NanoMuseHeaderScrim(scrolledUp: !vm.messages.isEmpty && !vm.isNearBottom)
    }
}

// MARK: - Header title (navigation bar principal item)

/// Face · name · status line, sized for the 44 pt navigation band. Observes
/// the activity tracker and the gates itself so the chat's equatable
/// toolbar host does not have to rebuild for every tick.
struct NanoMuseHeaderTitle: View {
    @ObservedObject var vm: AIChatViewModel
    var soulName: String
    var modelName: String
    var onTapText: () -> Void

    @ObservedObject private var tracker = SessionActivityTracker.shared
    @ObservedObject private var permissions = OffloadPermissionManager.shared
    @ObservedObject private var gate = ConfigConfirmationGate.shared
    @ObservedObject private var studio = NanoMuseAvatarStudioModel.shared
    @ObservedObject private var avatarFlow = NanoMuseAvatarFlow.shared
    @ObservedObject private var motion = NanoMuseAvatarMotion.shared
    @StateObject private var moods = NanoMuseMoodModel()
    // Dynamic Type, capped as in the big-face header (NanoMuseHeaderMetrics)
    @ScaledMetric(relativeTo: .subheadline) private var nameSize: CGFloat = 14
    @ScaledMetric(relativeTo: .caption) private var statusSize: CGFloat = 11

    private var waiting: Bool {
        permissions.pendingRequest != nil || gate.pending != nil
    }

    private var info: SessionActivityTracker.SessionToolInfo? {
        guard let sid = vm.sessionId else { return nil }
        return tracker.sessionToolInfo[sid]
    }

    private var lastRequest: String? {
        vm.messages.last(where: { $0.role == .user })?.content
    }

    private var statusLine: String? {
        NanoMuseStatus.line(
            waiting: gate.pending != nil,
            needsApproval: permissions.pendingRequest != nil,
            processing: vm.isProcessing,
            toolName: info?.toolName ?? "",
            toolTitle: info?.toolStatus ?? "",
            request: lastRequest,
            studio: avatarFlow.statusLine ?? studio.headerStatus,
            motion: motion.statusLine
        )
    }

    private var mood: NanoMuseMood {
        moods.mood(waiting: waiting, working: vm.isProcessing || studio.isBusy)
    }

    var body: some View {
        HStack(spacing: 8) {
            // The face opens the agent's page (Android: AgentProfileScreen); the studio is one of its doors.
            Button {
                NotificationCenter.default.post(name: .nanoMuseOpenAgentPage, object: vm.nmSessionKey)
            } label: {
                NanoMuseFaceView(mood: mood, size: 32)
            }
            .buttonStyle(.plain)
            .accessibilityLabel(Text(AppLocalized("About the agent")))

            Button(action: onTapText) {
                VStack(alignment: .leading, spacing: 0) {
                    Text(soulName)
                        .font(.system(size: NanoMuseHeaderMetrics.capped(nameSize, base: 14), weight: .semibold))
                        .foregroundStyle(ChatColors.primaryText)
                        .lineLimit(1)
                    Text(statusLine ?? modelName)
                        .font(.system(size: NanoMuseHeaderMetrics.capped(statusSize, base: 11)))
                        .foregroundStyle(statusLine == nil ? ChatColors.tertiaryText : NanoMuseTones.action)
                        .lineLimit(1)
                        .truncationMode(.tail)
                        .animation(.easeInOut(duration: 0.2), value: statusLine)
                }
                .fixedSize(horizontal: false, vertical: true)
            }
            .buttonStyle(.plain)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .nmOnChange(of: vm.isProcessing) { processing in
            guard !processing else { return }
            let failed = vm.errorMessage != nil || vm.messages.last?.error != nil
            moods.turnEnded(withError: failed)
        }
    }
}
