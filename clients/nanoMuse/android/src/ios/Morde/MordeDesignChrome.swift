import QuickLook
import SwiftUI
import UIKit

enum NanoMuseMood: String, CaseIterable, Codable {
    case idle, working, waiting, happy, error
}

enum ChatColors {
    static let background = NanoMuseTones.canvas
}

enum NanoMuseTones {
    static let brand = Color(red: 96.0 / 255, green: 165.0 / 255, blue: 250.0 / 255)
    static let brandDeep = Color(red: 29.0 / 255, green: 78.0 / 255, blue: 216.0 / 255)
    static let brandSoft = Color(red: 219.0 / 255, green: 234.0 / 255, blue: 254.0 / 255)
    static let brandTint = Color(red: 191.0 / 255, green: 219.0 / 255, blue: 254.0 / 255)
    static let canvas = Color(red: 248.0 / 255, green: 250.0 / 255, blue: 255.0 / 255)
    static let side = Color(red: 239.0 / 255, green: 246.0 / 255, blue: 255.0 / 255)
    static let surface = Color.white
    static let fill = side
    static let bubble = surface
    static let userMessage = brand
    static let hairline = brandSoft
    static let action = brand
    static let actionText = brandDeep
    static let secondaryText = Color(red: 100.0 / 255, green: 116.0 / 255, blue: 139.0 / 255)
    static let muted = Color(red: 148.0 / 255, green: 163.0 / 255, blue: 184.0 / 255)
    static let ink = Color(red: 51.0 / 255, green: 65.0 / 255, blue: 85.0 / 255)
    static let onInk = Color.white
    static let agentPill = surface
    static let success = Color(red: 52.0 / 255, green: 178.0 / 255, blue: 122.0 / 255)
    static let successFill = Color(red: 209.0 / 255, green: 250.0 / 255, blue: 229.0 / 255)
    static let successInk = Color(red: 4.0 / 255, green: 120.0 / 255, blue: 87.0 / 255)
    static let warning = Color(red: 180.0 / 255, green: 83.0 / 255, blue: 9.0 / 255)
    static let warningFill = Color(red: 254.0 / 255, green: 243.0 / 255, blue: 199.0 / 255)
    static let danger = Color(red: 240.0 / 255, green: 101.0 / 255, blue: 90.0 / 255)
    static let dangerFill = Color(red: 254.0 / 255, green: 226.0 / 255, blue: 226.0 / 255)
    static let dangerInk = Color(red: 185.0 / 255, green: 28.0 / 255, blue: 28.0 / 255)
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
    var size: CGFloat = 40
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
                    .overlay(Circle().strokeBorder(NanoMuseTones.hairline, lineWidth: 1))
                    .overlay(Image(systemName: symbol)
                        .font(.system(size: size * 0.41, weight: .medium))
                        .foregroundStyle(NanoMuseTones.brandDeep))
                    .shadow(color: NanoMuseTones.brand.opacity(0.12), radius: 8, y: 4)
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
                        .shadow(color: NanoMuseTones.brand.opacity(0.12), radius: 8, y: 4)
                    VStack(spacing: 3.1) {
                        Capsule().frame(width: 13, height: 1.7)
                        Capsule().frame(width: 13, height: 1.7)
                    }
                    .foregroundStyle(NanoMuseTones.brandDeep)
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
                    .foregroundStyle(NanoMuseTones.secondaryText)
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
            .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 24, style: .continuous))
            .overlay(RoundedRectangle(cornerRadius: 24, style: .continuous).strokeBorder(NanoMuseTones.hairline))
            .shadow(color: NanoMuseTones.brand.opacity(0.18), radius: 15, y: 7)
            .padding(.horizontal, 16)
    }
}

struct NanoMuseRowDivider: View {
    var inset: CGFloat = 16

    var body: some View {
        NanoMuseTones.hairline.frame(height: 1).padding(.leading, inset)
    }
}

struct NanoMuseIceCreamTextFieldStyle: TextFieldStyle {
    func _body(configuration: TextField<Self._Label>) -> some View {
        configuration
            .font(.body)
            .foregroundStyle(NanoMuseTones.ink)
            .padding(.horizontal, 14)
            .padding(.vertical, 10)
            .frame(minHeight: 44)
            .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 16, style: .continuous))
            .overlay(RoundedRectangle(cornerRadius: 16, style: .continuous)
                .strokeBorder(NanoMuseTones.brandTint, lineWidth: 1.5))
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
