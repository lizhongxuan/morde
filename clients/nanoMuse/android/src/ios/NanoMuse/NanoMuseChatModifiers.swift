//
//  NanoMuseChatModifiers.swift
//  nanoMuse
//
//  VM-bound hooks for the legacy local conversation view.
//

import SwiftUI

// nanoMuse: Shared shell values and composer layout live in NanoMuseSharedChatModifiers.swift.

/// The chat's listeners of ours, one link of the chain: the Muse header's ••• menu, the
/// composer's expectation when a message goes out or a turn ends, presence (C9 — the
/// "{device} is working…" line under the last remote row, cleared when the reply arrives),
/// in the chat.
struct NanoMuseChatHooks: ViewModifier {
    let vm: AIChatViewModel
    /// `vm.isProcessing`, passed as a value so a change re-runs this body and `onChange` sees it.
    let processing: Bool
    let composer: NanoMuseComposerWatch
    let readOnly: Bool
    let perform: (NanoMuseChatAction) -> Void
    func body(content: Content) -> some View {
        content
            .onReceive(NotificationCenter.default.publisher(for: .nanoMuseChatAction)) { note in
                guard let action = NanoMuseChatAction.from(note, for: vm.nmSessionKey) else { return }
                perform(action)
            }
            .nmOnChange(of: processing) { running in
                // a message went out (any path: the pill, a card's pick, a flow) or a turn ended —
                // the composer must be there a second later
                if !readOnly { composer.expect(running ? "a message went out" : "the turn ended") }
            }
            .onReceive(NanoMusePresence.shared.$revision) { _ in vm.nmApplyPresence() }
            .onReceive(vm.$messages) { _ in vm.nmApplyPresence() }
    }
}
