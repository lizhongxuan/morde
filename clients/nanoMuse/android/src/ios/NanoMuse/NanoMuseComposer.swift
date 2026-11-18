//
//  NanoMuseComposer.swift
//  nanoMuse
//
//  Muse's composer pill, used by AIChatView when the Muse shell is on: one
//  row on a flat grey capsule — plus on the left, the field in the middle,
//  the mic on the right while there is nothing to send, the send arrow once
//  there is, the stop square while the agent answers. Bare glyphs for plus
//  and mic; send and stop use the shared Morde brand disc.
//  Android: the `nmPill` branch of ChatScreen.kt.
//

import SwiftUI
import UIKit

/// The pill's row. AIChatView hands it the text field it already builds and
/// the actions its own buttons run, so the behaviour is upstream's and only
/// the look is Muse's.
struct NanoMuseComposerPill<Field: View>: View {
    /// Text or an attachment is in the composer.
    var hasContent: Bool
    var isProcessing: Bool
    var canSend: Bool
    var canEnqueue: Bool
    var showsAttachments = true
    var showsMicrophone = true
    /// Uses the compact single-field geometry from the Morde design while keeping
    /// the existing 44 pt interactive target.
    var designCompactLayout = false
    var sendAccessibilityIdentifier = "nanoMuse.composer.send"
    var microphoneAccessibilityIdentifier = "nanoMuse.composer.mic"
    var onCamera: () -> Void
    var onPhotos: () -> Void
    var onFile: () -> Void
    /// An image on the pasteboard, as an attachment. The pill's field is SwiftUI's (0.1.38) and
    /// does not take an image paste the way upstream's text view did; the plus menu offers it
    /// whenever the pasteboard holds one.
    var onPasteImage: (UIImage) -> Void
    /// The slash commands: the "/" button has no seat in the pill, so they open from the plus menu.
    var onCommands: () -> Void
    var onMic: () -> Void
    var onSend: () -> Void
    var onIdleSend: (() -> Void)? = nil
    var onEnqueue: () -> Void
    var onStop: () -> Void
    @ViewBuilder var field: () -> Field

    @State private var showAttachMenu = false
    private let glyph: CGFloat = 44
    /// The send disc's diameter, the size the `arrow.up.circle.fill` glyph had.
    private var disc: CGFloat { designCompactLayout ? 34 : 30 }

    var body: some View {
        HStack(alignment: .bottom, spacing: designCompactLayout ? 8 : 2) {
            if showsAttachments { plusButton }
            // One line as tall as a glyph, so its text sits on the plus and the mic's centre line;
            // the row stays bottom-aligned, so a longer text keeps them level with its last line.
            field()
                .font(designCompactLayout ? .system(size: FontSettings.shared.scaledChatInput(15)) : nil)
                .padding(.vertical, max(4, (glyph - UIFont.systemFont(
                    ofSize: FontSettings.shared.scaledChatInput(16.5)).lineHeight.rounded(.up)) / 2))
            trailingButton
        }
        .padding(.leading, designCompactLayout ? 12 : 6)
        .padding(.trailing, designCompactLayout ? 1 : 6)
        .padding(.vertical, designCompactLayout ? 0 : 4)
    }

    // MARK: Plus

    /// iOS 16's Menu misaligns inside an HStack with the keyboard up (upstream's note), so the
    /// sheet form is used there and the Menu from iOS 17 on.
    @ViewBuilder
    private var plusButton: some View {
        let icon = Image(systemName: "plus")
            .font(.system(size: 22, weight: .regular))
            .foregroundStyle(NanoMuseTones.ink)
            .frame(width: glyph, height: glyph)
            .contentShape(Circle())
            .accessibilityLabel(Text("Add attachment", comment: "VoiceOver label for the attachment button"))
        if #available(iOS 17, *) {
            Menu {
                attachItems
            } label: {
                icon
            }
        } else {
            Button { showAttachMenu = true } label: { icon }
                .buttonStyle(.plain)
                .confirmationDialog("Add Attachment", isPresented: $showAttachMenu) {
                    attachItems
                }
        }
    }

    @ViewBuilder
    private var attachItems: some View {
        Button(action: onCamera) { Label("Take Photo", systemImage: "camera") }
        Button(action: onPhotos) { Label("Choose Photos & Videos", systemImage: "photo.on.rectangle") }
        Button(action: onFile) { Label("Add File", systemImage: "doc") }
        // `hasImages` reads only the pasteboard's types, so no paste banner; the image itself is
        // read when the person asks for it.
        if UIPasteboard.general.hasImages {
            Button {
                if let image = UIPasteboard.general.image { onPasteImage(image) }
            } label: {
                Label(AppLocalized("Paste image"), systemImage: "doc.on.clipboard")
            }
        }
        Button(action: onCommands) { Label(AppLocalized("Commands"), systemImage: "terminal") }
    }

    // MARK: Mic · send · stop

    /// The send arrow and stop square share the blue action disc; disabled uses the tint token.
    private func inkDisc(_ symbol: String, enabled: Bool) -> some View {
        ZStack {
            Circle()
                .fill(enabled ? NanoMuseTones.action : NanoMuseTones.brandTint)
            Image(systemName: symbol)
                .font(.system(size: 15, weight: .semibold))
                .foregroundStyle(enabled ? NanoMuseTones.onInk : NanoMuseTones.brandDeep)
        }
        .shadow(color: enabled ? NanoMuseTones.action.opacity(0.28) : .clear, radius: 8, y: 4)
        .frame(width: disc, height: disc)
        .frame(width: glyph, height: glyph)
        .contentShape(Rectangle())
    }

    @ViewBuilder
    private var trailingButton: some View {
        if isProcessing && canEnqueue {
            Button(action: onEnqueue) {
                inkDisc("arrow.up", enabled: true)
            }
            .buttonStyle(.plain)
            .keyboardShortcut(.return, modifiers: .command)
            .accessibilityLabel(Text("Add to queue", comment: "VoiceOver label for the send button while a reply is generating"))
            .accessibilityIdentifier("morde.chat.enqueueTask")
        } else if isProcessing {
            Button(action: onStop) {
                inkDisc("stop.fill", enabled: true)
            }
            .buttonStyle(.plain)
            .accessibilityLabel(Text("Stop generating", comment: "VoiceOver label for the stop button"))
            .accessibilityIdentifier("morde.chat.stopTask")
        } else if hasContent {
            Button(action: onSend) {
                inkDisc("arrow.up", enabled: canSend)
            }
            .buttonStyle(.plain)
            .disabled(!canSend)
            .keyboardShortcut(.return, modifiers: .command)
            .accessibilityLabel(Text("Send", comment: "VoiceOver label for the send button"))
            .accessibilityIdentifier(sendAccessibilityIdentifier)
        } else if showsMicrophone {
            Button(action: onMic) {
                Image(systemName: "mic")
                    .font(.system(size: 20, weight: .regular))
                    .foregroundStyle(NanoMuseTones.ink)
                    .frame(width: glyph, height: glyph)
                    .contentShape(Circle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel(Text("Voice input", comment: "Mic button opens voice panel"))
            .accessibilityIdentifier(microphoneAccessibilityIdentifier)
        } else if let onIdleSend {
            Button(action: onIdleSend) {
                inkDisc("arrow.up", enabled: true)
            }
            .buttonStyle(.plain)
            .accessibilityLabel(Text("Send", comment: "VoiceOver label for the send button"))
            .accessibilityIdentifier(sendAccessibilityIdentifier)
        } else {
            inkDisc("arrow.up", enabled: false)
                .accessibilityHidden(true)
        }
    }
}
