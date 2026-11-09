import QuickLook
import SwiftUI
import UIKit

enum NanoMuseMood: String, CaseIterable, Codable {
    case idle, working, waiting, happy, error
}

enum ChatColors {
    static let background = Color(UIColor.systemBackground)
}

enum NanoMuseTones {
    static let surface = Color(uiColor: UIColor { $0.userInterfaceStyle == .dark ? UIColor(red: 0.11, green: 0.11, blue: 0.118, alpha: 1) : .white })
    static let fill = Color(uiColor: UIColor { $0.userInterfaceStyle == .dark ? UIColor(red: 0.165, green: 0.165, blue: 0.18, alpha: 1) : UIColor(red: 0.945, green: 0.945, blue: 0.957, alpha: 1) })
    static let bubble = Color(uiColor: UIColor { $0.userInterfaceStyle == .dark ? UIColor(red: 0.149, green: 0.149, blue: 0.165, alpha: 1) : UIColor(red: 0.914, green: 0.918, blue: 0.925, alpha: 1) })
    static let userMessage = Color(uiColor: UIColor { $0.userInterfaceStyle == .dark ? UIColor(red: 0.17, green: 0.31, blue: 0.47, alpha: 1) : UIColor(red: 0.75, green: 0.87, blue: 0.99, alpha: 1) })
    static let hairline = Color(uiColor: UIColor { $0.userInterfaceStyle == .dark ? UIColor(red: 0.227, green: 0.227, blue: 0.235, alpha: 1) : UIColor(red: 0.898, green: 0.898, blue: 0.918, alpha: 1) })
    static let canvas = Color(uiColor: UIColor { $0.userInterfaceStyle == .dark ? .black : UIColor(red: 0.953, green: 0.953, blue: 0.961, alpha: 1) })
    static let action = Color(red: 0.039, green: 0.4, blue: 0.894)
    static let actionText = Color(uiColor: UIColor { $0.userInterfaceStyle == .dark ? UIColor(red: 0.49, green: 0.68, blue: 1, alpha: 1) : UIColor(red: 0.039, green: 0.4, blue: 0.894, alpha: 1) })
    static let secondaryText = Color(uiColor: UIColor { $0.userInterfaceStyle == .dark ? UIColor(red: 0.73, green: 0.73, blue: 0.77, alpha: 1) : UIColor(red: 0.37, green: 0.37, blue: 0.4, alpha: 1) })
    static let ink = Color(uiColor: UIColor { $0.userInterfaceStyle == .dark ? .white : .black })
    static let onInk = Color(uiColor: UIColor { $0.userInterfaceStyle == .dark ? .black : .white })
    static let agentPill = Color(uiColor: UIColor { $0.userInterfaceStyle == .dark ? UIColor(red: 0.11, green: 0.11, blue: 0.118, alpha: 1) : UIColor(red: 0.949, green: 0.949, blue: 0.969, alpha: 1) })
}

enum NanoMuseBottomBar {
    static let height: CGFloat = 86
}

extension Notification.Name {
    static let mordeShellTaskOptions = Notification.Name("morde.shellTaskOptions")
    static let mordeShellVoiceInput = Notification.Name("morde.shellVoiceInput")
    static let mordeShellAttachmentOptions = Notification.Name("morde.shellAttachmentOptions")
}

extension View {
    @ViewBuilder
    func nmOnChange<V: Equatable>(of value: V, perform action: @escaping (V) -> Void) -> some View {
        if #available(iOS 17.0, *) {
            onChange(of: value) { _, newValue in action(newValue) }
        } else {
            onChange(of: value, perform: action)
        }
    }
}

struct NanoMuseRoundButton: View {
    var symbol: String
    var label: String
    var badge = false
    var size: CGFloat = 44
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
            ZStack(alignment: .topTrailing) {
                Circle()
                    .fill(NanoMuseTones.surface)
                    .overlay(Circle().strokeBorder(NanoMuseTones.hairline, lineWidth: 0.5))
                    .overlay(Image(systemName: symbol).font(.system(size: size * 0.41, weight: .medium)))
                    .shadow(color: .black.opacity(0.12), radius: 3, y: 1)
                if badge {
                    Circle().fill(NanoMuseTones.action).frame(width: 9, height: 9).padding(2)
                }
            }
            .frame(width: size, height: size)
        }
        .buttonStyle(.plain)
        .accessibilityLabel(Text(label))
    }
}

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
        HStack(spacing: 10) {
            if let onBack {
                NanoMuseRoundButton(symbol: "chevron.left", label: AppLocalized("Back"), size: 38, action: onBack)
            }
            Text(title).font(.headline.weight(.semibold)).lineLimit(1)
            Spacer(minLength: 0)
            trailing()
        }
        .padding(.horizontal, 22)
        .frame(minHeight: 52)
    }
}

extension NanoMuseTopBar where Trailing == EmptyView {
    init(title: String, onBack: (() -> Void)?) {
        self.init(title: title, onBack: onBack) { EmptyView() }
    }
}

struct NanoMuseMenuHeader<Trailing: View>: View {
    var title: String
    var onMenu: () -> Void
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
        HStack(spacing: 10) {
            Button(action: onMenu) {
                ZStack {
                    Circle()
                        .fill(NanoMuseTones.surface)
                        .overlay(Circle().strokeBorder(NanoMuseTones.hairline, lineWidth: 0.5))
                        .shadow(color: .black.opacity(0.12), radius: 3, y: 1)
                    VStack(spacing: 3.1) {
                        Capsule().frame(width: 13, height: 1.7)
                        Capsule().frame(width: 13, height: 1.7)
                    }
                    .foregroundStyle(NanoMuseTones.ink)
                }
                .frame(width: 38, height: 38)
                .contentShape(Circle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel(Text(AppLocalized("Chats and settings")))
            .accessibilityIdentifier("morde.nav.menu")
            Text(title)
                .font(.system(size: 17, weight: .semibold))
                .tracking(-0.17)
                .lineLimit(1)
            if let titleAccessory {
                Text(titleAccessory)
                    .font(.system(size: 13.5, weight: .medium))
                    .foregroundStyle(Color(red: 163.0 / 255, green: 163.0 / 255, blue: 168.0 / 255))
                    .lineLimit(1)
                    .accessibilityIdentifier("mif.approval.count")
            }
            Spacer(minLength: 8)
            trailing()
        }
        .padding(.horizontal, 22)
        .frame(minHeight: 52)
    }
}

extension NanoMuseMenuHeader where Trailing == EmptyView {
    init(title: String, onMenu: @escaping () -> Void) {
        self.init(title: title, onMenu: onMenu) { EmptyView() }
    }
}

typealias MordeDesignPageHeader<Trailing: View> = NanoMuseMenuHeader<Trailing>

struct NanoMuseCard<Content: View>: View {
    @ViewBuilder var content: () -> Content

    var body: some View {
        VStack(spacing: 0, content: content)
            .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 18, style: .continuous))
            .padding(.horizontal, 16)
    }
}

struct NanoMuseRowDivider: View {
    var inset: CGFloat = 16

    var body: some View {
        NanoMuseTones.hairline.frame(height: 0.5).padding(.leading, inset)
    }
}

struct NanoMuseQuickLook: UIViewControllerRepresentable {
    let url: URL

    func makeUIViewController(context: Context) -> QLPreviewController {
        let controller = QLPreviewController()
        controller.dataSource = context.coordinator
        return controller
    }

    func updateUIViewController(_ controller: QLPreviewController, context: Context) {}
    func makeCoordinator() -> Coordinator { Coordinator(url: url) }

    final class Coordinator: NSObject, QLPreviewControllerDataSource {
        let url: URL
        init(url: URL) { self.url = url }
        func numberOfPreviewItems(in controller: QLPreviewController) -> Int { 1 }
        func previewController(_ controller: QLPreviewController, previewItemAt index: Int) -> QLPreviewItem {
            url as NSURL
        }
    }
}
