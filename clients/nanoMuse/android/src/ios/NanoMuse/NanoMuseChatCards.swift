//
//  NanoMuseChatCards.swift
//  nanoMuse
//
//  VM-backed cards hosted by the legacy AIChatView. Shared transcript views
//  live in NanoMuseSharedChatViews.swift.
//

import SwiftUI

// MARK: - Virtual cards above the composer

/// Hosted by AIChatView just above the input bar. Also where the two
/// notifications from the agent page land: a prefilled composer and the
/// first conversation's seeding.
struct NanoMuseChatCardsHost: View {
    @ObservedObject var vm: AIChatViewModel
    @ObservedObject private var flow = NanoMuseAvatarFlow.shared
    @ObservedObject private var naming = NanoMuseFirstConversation.shared
    @ObservedObject private var allowance = NanoMuseAllowance.shared

    private var key: String { vm.nmSessionKey }

    var body: some View {
        VStack(spacing: 8) {
            // C11 / parity #33: 80 % of the free allowance spent — one line, once per pool size.
            if let line = allowance.headsUp, allowance.pending == nil {
                NanoMuseAllowanceHeadsUp(text: line) { allowance.hideHeadsUp() }
            }
            switch flow.stage {
            case .confirming(let s, _) where s == key:
                NanoMuseAvatarCostCard()
            case .choosing(let s, _) where s == key, .finalizing(let s, _, _) where s == key:
                NanoMuseAvatarOptionsCard()
            case .done(let s, let desc) where s == key:
                NanoMuseAvatarDoneCard(description: desc)
            default:
                EmptyView()
            }
            if naming.card != nil, naming.isBound(to: key) {
                NanoMuseNamingCardView(vm: vm)
            }
        }
        .padding(.horizontal, 12)
        .padding(.bottom, 6)
        .onReceive(NotificationCenter.default.publisher(for: .nanoMuseComposerPrefill)) { note in
            // Meant for this chat: the object is the session key it was posted for, or nil for "whichever is open".
            if let target = note.object as? String, target != key, target != vm.sessionId, target != vm.draftId { return }
            guard let text = note.userInfo?["text"] as? String else { return }
            if (note.userInfo?["send"] as? Bool) == true { vm.nmSendNow(text) } else { vm.nmPrefill(text) }
        }
        .task(id: key) {
            await naming.seedIfNeeded(vm: vm)
        }
    }
}
