//
//  NanoMuseToolBar.swift
//  nanoMuse
//
//  The step bar above the composer, as the Muse shell draws it. Upstream's
//  bar (FloatingToolBar in ToolLiveSheet.swift) is 38 pt tall with a 100 × 65
//  thumbnail standing 27 pt out above it; next to the shell's flat pills the
//  two read as misaligned. In the shell the thumbnail gives way to a glyph
//  for the kind of step, inside the bar, and the bar takes the composer's
//  width, corner and fill. Tapping it still opens the live sheet, so nothing
//  the thumbnail showed is lost. iOS only; Android keeps upstream's bar.
//

import SwiftUI

enum NanoMuseToolBar {
    /// The bar's corner: the composer pill's 26 pt, held to half the bar's height so it reads
    /// as a capsule (ComposerSurface).
    static let corner: CGFloat = 19

    /// The glyph for the kind of step: terminal, file, page, picture, memory.
    static func symbol(for kind: AssistantBlockKind) -> String {
        switch kind {
        case .shellTool: return "terminal"
        case .fileReadTool: return "doc.text"
        case .fileWriteTool, .fileEditTool: return "square.and.pencil"
        case .browserTool: return "globe"
        case .readImageTool: return "photo"
        case .memoryTool: return "brain.head.profile"
        case .text, .thinking, .info: return "sparkles"
        }
    }
}

/// The glyph at the bar's leading edge.
struct NanoMuseToolGlyph: View {
    @ObservedObject var block: AssistantBlock

    var body: some View {
        Image(systemName: NanoMuseToolBar.symbol(for: block.kind))
            .font(.system(size: 14, weight: .regular))
            .foregroundStyle(ChatColors.primaryText)
            .frame(width: 22, height: 22)
            .accessibilityHidden(true)
    }
}

/// The bar's surface in the shell: the composer pill's fill, no stroke, no shadow.
struct NanoMuseToolBarSurface: ViewModifier {
    func body(content: Content) -> some View {
        content.background(
            RoundedRectangle(cornerRadius: NanoMuseToolBar.corner, style: .continuous)
                .fill(NanoMuseTones.bubble)
        )
    }
}
