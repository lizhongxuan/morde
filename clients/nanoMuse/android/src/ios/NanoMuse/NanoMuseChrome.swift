//
//  NanoMuseChrome.swift
//  nanoMuse
//
//  The legacy conversation's live header, which observes AIChatViewModel.
//  Shared Morde shell chrome lives in NanoMuseShellChrome.swift.
//

import SwiftUI
import UIKit

// nanoMuse: Morde uses StillHeader; this VM-bound live header has no caller in the current target.

// MARK: - Live header for a conversation

/// The big-face header fed by a conversation: the mood and the status line
/// follow what the chat is doing (Android: rememberNanoMuseStatusLine +
/// rememberAgentMood). Rooms pass the main chat's view model so the face
/// stays alive while the person is on another tab.
struct NanoMuseLiveHeader<Leading: View, Trailing: View>: View {
    @ObservedObject var vm: AIChatViewModel
    var name: String
    var onFace: () -> Void
    var onName: (() -> Void)? = nil
    @ViewBuilder var leading: () -> Leading
    @ViewBuilder var trailing: () -> Trailing

    @ObservedObject private var tracker = SessionActivityTracker.shared
    @ObservedObject private var permissions = OffloadPermissionManager.shared
    @ObservedObject private var gate = ConfigConfirmationGate.shared
    @ObservedObject private var studio = NanoMuseAvatarStudioModel.shared
    @ObservedObject private var avatarFlow = NanoMuseAvatarFlow.shared
    @ObservedObject private var motion = NanoMuseAvatarMotion.shared
    @ObservedObject private var appearance = NanoMuseAppearance.shared
    @ObservedObject private var configStore = ProviderConfigStore.shared
    @StateObject private var moods = NanoMuseMoodModel()

    private var info: SessionActivityTracker.SessionToolInfo? {
        guard let sid = vm.sessionId else { return nil }
        return tracker.sessionToolInfo[sid]
    }

    private var statusLine: String? {
        NanoMuseStatus.line(
            waiting: gate.pending != nil,
            needsApproval: permissions.pendingRequest != nil,
            processing: vm.isProcessing,
            toolName: info?.toolName ?? "",
            toolTitle: info?.toolStatus ?? "",
            request: vm.messages.last(where: { $0.role == .user })?.content,
            studio: avatarFlow.statusLine ?? studio.headerStatus,
            motion: motion.statusLine
        )
    }

    /// Idle, with the Appearance switch on: the model answering, as Android's model rows.
    private var idleLine: String? {
        guard appearance.headerModel else { return nil }
        let line = SessionModelDisplay(store: configStore, draftGroupId: vm.initialGroupId).displayName(for: vm.sessionId)
        return line.isEmpty ? nil : line
    }

    private var mood: NanoMuseMood {
        moods.mood(waiting: permissions.pendingRequest != nil || gate.pending != nil, working: vm.isProcessing || studio.isBusy)
    }

    var body: some View {
        let status = statusLine
        NanoMuseMuseHeader(
            mood: mood,
            name: name,
            status: status ?? idleLine,
            statusColor: status == nil ? .secondary : NanoMuseTones.action,
            onFace: onFace,
            onName: onName,
            leading: leading,
            trailing: trailing
        )
        .nmOnChange(of: vm.isProcessing) { processing in
            guard !processing else { return }
            moods.turnEnded(withError: vm.errorMessage != nil || vm.messages.last?.error != nil)
        }
    }
}
