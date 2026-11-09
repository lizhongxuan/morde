//
//  NanoMuseShellChrome.swift
//  nanoMuse
//
//  Muse's page furniture, shared by the home shell and the settings pages:
//  the big-face header (face on a disc, the name in a white pill under the
//  chin with the status as its second line, a round button in each top
//  corner), the round button itself, the Muse top bar with a centred title
//  and a back disc, and the white cards with rows that Settings is made of.
//  Android: ui/home/MuseHeader.kt, ui/muse/MuseChrome.kt.
//

import SwiftUI
import UIKit

// nanoMuse: Shared shell and settings chrome stays independent of the local chat view model.

// MARK: - Shared app colors

enum ChatColors {
    static let background = Color(UIColor.systemBackground)
    static let secondaryBg = Color(UIColor.secondarySystemBackground)
    static let inputIconBg = Color(UIColor.secondarySystemBackground)
    static let inputIconBorder = Color(UIColor { $0.userInterfaceStyle == .dark ? UIColor(white: 0.35, alpha: 1) : UIColor(white: 0, alpha: 0) })
    static let inputBg = Color(UIColor { $0.userInterfaceStyle == .dark ? UIColor(white: 0.12, alpha: 1) : .white })
    static let inputBorder = Color(UIColor.separator)
    static let primaryText = Color(UIColor.label)
    static let secondaryText = Color(UIColor.secondaryLabel)
    static let tertiaryText = Color(UIColor.tertiaryLabel)
    static let userBubble = Color(UIColor.tertiarySystemFill)
    static let toolBg = Color(UIColor.tertiarySystemGroupedBackground)
    static let toolBorder = Color(UIColor.separator).opacity(0.5)
    static let accent = Color(UIColor.label)
    static let sendButton = Color(UIColor.label)
    static let sendButtonDisabled = Color(UIColor.quaternaryLabel)
}

// MARK: - MinisToast

/// [T-toast-feedback] Lightweight, self-dismissing toast for one-shot action
/// feedback ("Saved", "Table copied", …). Window-level so it works from both
/// SwiftUI screens and UIKit views (e.g. the markdown table copy actions,
/// which run in a UILabel subclass with no SwiftUI context). Mirrors the
/// capsule style FileBrowserView already uses for its "Copied" toast.
enum MinisToast {
    /// Show a toast with an already-localized message. Safe from any thread.
    /// `systemImage` sets the leading icon (default: success checkmark); pass an
    /// error glyph like "exclamationmark.triangle.fill" for failures.
    static func show(_ message: String, duration: TimeInterval = 1.6,
                     systemImage: String = "checkmark.circle.fill") {
        if Thread.isMainThread {
            present(message, duration: duration, systemImage: systemImage)
        } else {
            DispatchQueue.main.async { present(message, duration: duration, systemImage: systemImage) }
        }
    }

    private static func present(_ message: String, duration: TimeInterval,
                                systemImage: String = "checkmark.circle.fill") {
        guard let window = keyWindow() else { return }

        let label = UILabel()
        label.text = message
        label.font = .preferredFont(forTextStyle: .subheadline)
        label.adjustsFontForContentSizeCategory = true
        label.textColor = .white
        label.numberOfLines = 1
        label.translatesAutoresizingMaskIntoConstraints = false

        let check = UIImageView(image: UIImage(systemName: systemImage))
        check.tintColor = .white
        check.contentMode = .scaleAspectFit
        check.translatesAutoresizingMaskIntoConstraints = false

        let capsule = UIView()
        capsule.backgroundColor = UIColor.black.withAlphaComponent(0.8)
        capsule.layer.cornerRadius = 18
        capsule.layer.cornerCurve = .continuous
        capsule.translatesAutoresizingMaskIntoConstraints = false
        capsule.alpha = 0

        capsule.addSubview(check)
        capsule.addSubview(label)
        window.addSubview(capsule)

        NSLayoutConstraint.activate([
            check.leadingAnchor.constraint(equalTo: capsule.leadingAnchor, constant: 16),
            check.centerYAnchor.constraint(equalTo: capsule.centerYAnchor),
            check.widthAnchor.constraint(equalToConstant: 16),
            check.heightAnchor.constraint(equalToConstant: 16),
            label.leadingAnchor.constraint(equalTo: check.trailingAnchor, constant: 8),
            label.trailingAnchor.constraint(equalTo: capsule.trailingAnchor, constant: -16),
            label.topAnchor.constraint(equalTo: capsule.topAnchor, constant: 10),
            label.bottomAnchor.constraint(equalTo: capsule.bottomAnchor, constant: -10),
            capsule.centerXAnchor.constraint(equalTo: window.centerXAnchor),
            capsule.bottomAnchor.constraint(equalTo: window.safeAreaLayoutGuide.bottomAnchor, constant: -48),
            capsule.leadingAnchor.constraint(greaterThanOrEqualTo: window.leadingAnchor, constant: 24),
            capsule.trailingAnchor.constraint(lessThanOrEqualTo: window.trailingAnchor, constant: -24),
        ])

        UIView.animate(withDuration: 0.25) { capsule.alpha = 1 }
        UIView.animate(withDuration: 0.3, delay: duration, options: [.curveEaseIn]) {
            capsule.alpha = 0
        } completion: { _ in
            capsule.removeFromSuperview()
        }
    }

    private static func keyWindow() -> UIWindow? {
        let windows = UIApplication.shared.connectedScenes
            .compactMap { $0 as? UIWindowScene }
            .flatMap { $0.windows }
        return windows.first { $0.isKeyWindow } ?? windows.first
    }
}

/// On iPad the default `.sheet` presents as a ~540pt-wide form sheet which
/// wastes most of the screen for long-form markdown. iOS 18 introduced
/// `presentationSizing(.page)` to widen the form sheet to a more readable
/// page-style width. iPhone keeps the default behavior either way.
// Non-private so cross-file preview sheets (e.g. MinisLinkPreviewView in
// WebPreviewSheet.swift) can reuse the same wide-sizing logic.
// [T-ios-html-preview-wide-sheet]
struct WideSheetSizingModifier: ViewModifier {
    func body(content: Content) -> some View {
        if #available(iOS 18.0, *) {
            content.presentationSizing(.page)
        } else {
            content
        }
    }
}

// MARK: - onChange across iOS versions

extension View {
    /// `onChange` handed the new value only. The iOS 17 form where it exists,
    /// the earlier one below it: the app's deployment target is iOS 16.0
    /// (Minis.xcodeproj), where the two-parameter form alone does not compile
    /// and the one-parameter form carries no deprecation warning.
    @ViewBuilder
    func nmOnChange<V: Equatable>(of value: V, perform action: @escaping (V) -> Void) -> some View {
        if #available(iOS 17.0, *) {
            onChange(of: value) { _, newValue in action(newValue) }
        } else {
            onChange(of: value, perform: action)
        }
    }
}

// MARK: - Wrapping row

/// A row of small things (chips, the buttons under a card) that wraps to the next line when
/// the width runs out, so four names or three actions fit an iPhone SE and a large text size
/// without being cut off. Leading-aligned; `spacing` between items and between lines.
struct NanoMuseFlowLayout: Layout {
    var spacing: CGFloat = 8

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        let width = proposal.width ?? .infinity
        var x: CGFloat = 0
        var y: CGFloat = 0
        var lineHeight: CGFloat = 0
        var widest: CGFloat = 0
        for view in subviews {
            let size = view.sizeThatFits(.unspecified)
            if x > 0, x + size.width > width {
                x = 0
                y += lineHeight + spacing
                lineHeight = 0
            }
            x += size.width + spacing
            lineHeight = max(lineHeight, size.height)
            widest = max(widest, x - spacing)
        }
        return CGSize(width: width == .infinity ? widest : min(widest, width), height: y + lineHeight)
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        // Lines first, then each item centred on its line (a filled button next to a plain one).
        var lines: [[(index: Int, size: CGSize)]] = [[]]
        var x: CGFloat = 0
        for (i, view) in subviews.enumerated() {
            let size = view.sizeThatFits(.unspecified)
            if x > 0, x + size.width > bounds.width {
                lines.append([])
                x = 0
            }
            lines[lines.count - 1].append((i, size))
            x += size.width + spacing
        }
        var y = bounds.minY
        for line in lines {
            let lineHeight = line.map(\.size.height).max() ?? 0
            var lx = bounds.minX
            for item in line {
                let dy = (lineHeight - item.size.height) / 2
                subviews[item.index].place(at: CGPoint(x: lx, y: y + dy), proposal: .unspecified)
                lx += item.size.width + spacing
            }
            y += lineHeight + spacing
        }
    }
}

// MARK: - Round button

/// Muse's corner disc: white, one glyph, a soft shadow (a hairline in the dark).
struct NanoMuseRoundDisc: View {
    var symbol: String
    var badge: Bool = false
    var size: CGFloat = 44

    @Environment(\.colorScheme) private var scheme

    var body: some View {
        ZStack(alignment: .topTrailing) {
            Circle()
                .fill(NanoMuseTones.surface)
                .overlay(Circle().strokeBorder(NanoMuseTones.hairline, lineWidth: scheme == .dark ? 1 : 0))
                .shadow(color: .black.opacity(scheme == .dark ? 0 : 0.14), radius: 3, y: 1)
                .overlay(
                    Image(systemName: symbol)
                        .font(.system(size: size * 0.41, weight: .medium))
                        .foregroundStyle(Color.primary)
                )
            if badge {
                Circle()
                    .fill(NanoMuseTones.action)
                    .frame(width: 9, height: 9)
                    .overlay(Circle().strokeBorder(NanoMuseTones.surface, lineWidth: 1.5))
                    .padding(2)
            }
        }
        .frame(width: size, height: size)
        .contentShape(Circle())
    }
}

/// The disc as a button.
struct NanoMuseRoundButton: View {
    var symbol: String
    var label: String
    var badge: Bool = false
    var size: CGFloat = 44
    // nanoMuse: feature screens assign stable identifiers to shared controls.
    var accessibilityIdentifier: String? = nil
    var action: () -> Void

    @ViewBuilder var body: some View {
        if let accessibilityIdentifier {
            button.accessibilityIdentifier(accessibilityIdentifier)
        } else {
            button
        }
    }

    private var button: some View {
        Button(action: action) {
            NanoMuseRoundDisc(symbol: symbol, badge: badge, size: size)
        }
        .buttonStyle(.plain)
        .accessibilityLabel(Text(label))
    }
}

// MARK: - Name pill

/// Muse's name tag: a small white tag, the name in regular weight, a faint
/// shadow; what the agent is doing right now as a smaller second line.
struct NanoMuseNamePill: View {
    var name: String
    var status: String?
    var statusColor: Color = .secondary
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    // The name pill follows full Dynamic Type; the compact navigation bar has a separate cap.
    @ScaledMetric(relativeTo: .subheadline) private var nameSize: CGFloat = 14
    @ScaledMetric(relativeTo: .caption) private var statusSize: CGFloat = 11.5

    var body: some View {
        VStack(spacing: 2) {
            Text(name)
                .font(.system(size: nameSize))
                .foregroundStyle(Color.primary)
                .lineLimit(1...2)
            if let status, !status.isEmpty {
                Text(status)
                    .font(.system(size: statusSize))
                    .foregroundStyle(statusColor)
                    .lineLimit(1...2)
                    .transition(.opacity)
            }
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 4)
        .fixedSize(horizontal: false, vertical: true)
        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 14, style: .continuous))
        .shadow(color: .black.opacity(0.14), radius: 2, y: 1)
        .frame(maxWidth: 260)
        .animation(reduceMotion ? nil : .easeInOut(duration: 0.2), value: status)
    }
}

// MARK: - Big-face header

/// How far the name pill rides up over the chin of the face.
private let nmPillOverlap: CGFloat = 10

/// Compact text in single-line headers stays within the space reserved for navigation controls.
enum NanoMuseHeaderMetrics {
    /// Largest multiple used by compact, single-line headers (xxxLarge is about 1.35).
    static let scaleCap: CGFloat = 1.35
    /// Height reserved under the face for the pill (name + status line) at the default text size.
    static let pillAreaBase: CGFloat = 44

    static func capped(_ scaled: CGFloat, base: CGFloat) -> CGFloat { min(scaled, base * scaleCap) }
}

/// The header of the main chat and Library: the face
/// centred on its disc (its size is the Appearance setting; Hidden keeps the
/// pill only), the name pill, a round button in each top corner.
struct NanoMuseMuseHeader<Leading: View, Trailing: View>: View {
    var mood: NanoMuseMood
    var name: String
    var status: String?
    var statusColor: Color = .secondary
    var onFace: () -> Void
    var onName: (() -> Void)? = nil
    @ViewBuilder var leading: () -> Leading
    @ViewBuilder var trailing: () -> Trailing

    @ObservedObject private var appearance = NanoMuseAppearance.shared

    var body: some View {
        let disc = appearance.avatarSize.points
        ZStack(alignment: .top) {
            VStack(spacing: 0) {
                if let disc {
                    Button(action: onFace) {
                        NanoMuseFaceView(mood: mood, size: disc)
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel(Text(AppLocalized("About the agent")))
                } else {
                    Color.clear.frame(height: 10)
                }
                Button(action: onName ?? onFace) {
                    NanoMuseNamePill(name: name, status: status, statusColor: statusColor)
                }
                .buttonStyle(.plain)
                .tint(Color.primary)
                .frame(minHeight: NanoMuseHeaderMetrics.pillAreaBase, alignment: .top)
                .padding(.top, disc == nil ? 0 : -nmPillOverlap)
                .zIndex(1)
            }
            .frame(maxWidth: .infinity)
            HStack(alignment: .top) {
                leading()
                Spacer(minLength: 0)
                trailing()
            }
            .padding(.horizontal, 16)
            .padding(.top, 6)
        }
        .padding(.top, 6)
        .padding(.bottom, 4)
        .frame(maxWidth: .infinity)
    }
}

/// The header when no conversation is loaded yet: the face idle, no status.
struct NanoMuseStillHeader<Leading: View, Trailing: View>: View {
    var name: String
    var onFace: () -> Void
    @ViewBuilder var leading: () -> Leading
    @ViewBuilder var trailing: () -> Trailing

    var body: some View {
        NanoMuseMuseHeader(mood: .idle, name: name, status: nil, onFace: onFace, leading: leading, trailing: trailing)
    }
}

// MARK: - Top bar for pages

/// Muse's page bar: a centred title, the back arrow on a white disc. Used
/// by the Muse settings pages (the system bar stays hidden under it).
struct NanoMuseTopBar<Trailing: View>: View {
    var title: String
    var onBack: (() -> Void)?
    @ViewBuilder var trailing: () -> Trailing
    init(title: String, onBack: (() -> Void)?, @ViewBuilder trailing: @escaping () -> Trailing) {
        self.title = title
        self.onBack = onBack
        self.trailing = trailing
    }

    var body: some View {
        ZStack {
            Text(title)
                .font(.headline.weight(.semibold))
                .lineLimit(2)
                .fixedSize(horizontal: false, vertical: true)
                .padding(.horizontal, 60)
            HStack {
                if let onBack {
                    NanoMuseRoundButton(symbol: "chevron.left", label: AppLocalized("Back"), size: 40, action: onBack)
                }
                Spacer(minLength: 0)
                trailing()
            }
            .padding(.horizontal, 12)
        }
        .frame(minHeight: 52)
    }
}

extension NanoMuseTopBar where Trailing == EmptyView {
    init(title: String, onBack: (() -> Void)?) {
        self.init(title: title, onBack: onBack) { EmptyView() }
    }
}

/// Compact room header from the Morde design, using the existing nanoMuse button and colors.
struct NanoMuseMenuHeader<Trailing: View>: View {
    var title: String
    var onMenu: () -> Void
    // nanoMuse: approval pages show their confirmed count beside the title.
    var titleAccessory: String? = nil
    @ViewBuilder var trailing: () -> Trailing

    init(title: String, onMenu: @escaping () -> Void, titleAccessory: String? = nil,
         @ViewBuilder trailing: @escaping () -> Trailing) {
        self.title = title
        self.onMenu = onMenu
        self.titleAccessory = titleAccessory
        self.trailing = trailing
    }

    var body: some View {
        HStack(spacing: 12) {
            NanoMuseRoundButton(symbol: "line.3.horizontal", label: AppLocalized("Chats and settings"),
                                size: 40, action: onMenu)
            Text(title)
                .font(.headline.weight(.semibold))
                .lineLimit(1)
            if let titleAccessory {
                Text(titleAccessory)
                    .font(.system(size: 13.5, weight: .medium))
                    .foregroundStyle(NanoMuseTones.secondaryText)
                    .lineLimit(1)
                    .accessibilityIdentifier("mif.approval.count")
            }
            Spacer(minLength: 8)
            trailing()
        }
        .padding(.horizontal, 12)
        .frame(minHeight: 52)
    }
}

extension NanoMuseMenuHeader where Trailing == EmptyView {
    init(title: String, onMenu: @escaping () -> Void) {
        self.init(title: title, onMenu: onMenu) { EmptyView() }
    }
}

typealias MordeDesignPageHeader<Trailing: View> = NanoMuseMenuHeader<Trailing>

// MARK: - Cards and rows

/// A white card with rounded corners; rows inside are separated by hairlines.
struct NanoMuseCard<Content: View>: View {
    @ViewBuilder var content: () -> Content

    var body: some View {
        VStack(spacing: 0) {
            content()
        }
        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 18, style: .continuous))
        .padding(.horizontal, 16)
    }
}

/// The hairline between two rows of a card.
struct NanoMuseRowDivider: View {
    var inset: CGFloat = 16
    var body: some View {
        NanoMuseTones.hairline
            .frame(height: 0.5)
            .padding(.leading, inset)
    }
}

/// One row of a Muse card: a title, an optional value on the right, a chevron.
struct NanoMuseRowLabel: View {
    var title: String
    var value: String? = nil
    var titleColor: Color = .primary
    var chevron: Bool = true

    var body: some View {
        HStack(spacing: 12) {
            Text(title)
                .font(.body)
                .foregroundStyle(titleColor)
                .lineLimit(1)
            Spacer(minLength: 8)
            if let value, !value.isEmpty {
                Text(value)
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
                    .truncationMode(.middle)
                    .layoutPriority(-1)
            }
            if chevron {
                Image(systemName: "chevron.right")
                    .font(.system(size: 13, weight: .semibold))
                    .foregroundStyle(.tertiary)
            }
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 13)
        .contentShape(Rectangle())
    }
}

/// A row that pushes a destination.
struct NanoMuseLinkRow<Destination: View>: View {
    var title: String
    var value: String? = nil
    @ViewBuilder var destination: () -> Destination

    var body: some View {
        NavigationLink {
            destination()
        } label: {
            NanoMuseRowLabel(title: title, value: value)
        }
        .buttonStyle(.plain)
    }
}

/// A row that runs an action.
struct NanoMuseActionRow: View {
    var title: String
    var value: String? = nil
    var titleColor: Color = .primary
    var chevron: Bool = true
    var action: () -> Void

    var body: some View {
        Button(action: action) {
            NanoMuseRowLabel(title: title, value: value, titleColor: titleColor, chevron: chevron)
        }
        .buttonStyle(.plain)
    }
}

/// A row with a switch.
struct NanoMuseToggleRow: View {
    var title: String
    var subtitle: String? = nil
    @Binding var isOn: Bool

    var body: some View {
        Toggle(isOn: $isOn) {
            VStack(alignment: .leading, spacing: 2) {
                Text(title).font(.body)
                if let subtitle, !subtitle.isEmpty {
                    Text(subtitle).font(.footnote).foregroundStyle(.secondary)
                }
            }
        }
        .tint(NanoMuseTones.action)
        .padding(.horizontal, 16)
        .padding(.vertical, 11)
    }
}

/// The small grey text under a card.
struct NanoMuseCaption: View {
    var text: String
    var body: some View {
        Text(text)
            .font(.footnote)
            .foregroundStyle(.secondary)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, 32)
            .padding(.top, 6)
    }
}

/// A page on the Muse canvas: the top bar, then scrolling content. The
/// system navigation bar is hidden under it; pushes from inside still work.
struct NanoMusePage<Content: View, Trailing: View>: View {
    var title: String
    @ViewBuilder var trailing: () -> Trailing
    @ViewBuilder var content: () -> Content
    @Environment(\.dismiss) private var dismiss

    init(title: String, @ViewBuilder trailing: @escaping () -> Trailing, @ViewBuilder content: @escaping () -> Content) {
        self.title = title
        self.trailing = trailing
        self.content = content
    }

    var body: some View {
        VStack(spacing: 0) {
            NanoMuseTopBar(title: title, onBack: { dismiss() }, trailing: trailing)
            ScrollView {
                VStack(spacing: 12) {
                    content()
                }
                .padding(.top, 4)
                .padding(.bottom, 32)
            }
        }
        .background(NanoMuseTones.canvas.ignoresSafeArea())
        .toolbar(.hidden, for: .navigationBar)
    }
}

extension NanoMusePage where Trailing == EmptyView {
    init(title: String, @ViewBuilder content: @escaping () -> Content) {
        self.init(title: title, trailing: { EmptyView() }, content: content)
    }
}
