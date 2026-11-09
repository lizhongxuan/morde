import SwiftUI
import UIKit

enum MordeDesignTab: String, CaseIterable, Hashable, Identifiable {
    case chat
    case tasks
    case approvals
    case resources

    var id: String { rawValue }

    var route: MordeDesignRoute {
        switch self {
        case .chat: .chat
        case .tasks: .tasks
        case .approvals: .approvals
        case .resources: .resources
        }
    }

    var title: String {
        switch self {
        case .chat: AppLocalized("Chat")
        case .tasks: AppLocalized("Tasks")
        case .approvals: AppLocalized("Approvals")
        case .resources: AppLocalized("Resources")
        }
    }

    var symbol: String? {
        switch self {
        case .chat: "bubble.left"
        case .tasks: "checkmark.square"
        case .approvals: "checkmark.shield"
        case .resources: nil
        }
    }

    var customGlyph: MordeDesignGlyphKind? {
        self == .resources ? .resources : nil
    }
}

enum MordeDesignGlyphKind: Hashable {
    case resources
    case library
    case workItems
}

private struct MordeDesignGlyphShape: Shape {
    let kind: MordeDesignGlyphKind

    func path(in rect: CGRect) -> Path {
        var path = Path()
        switch kind {
        case .resources:
            path.move(to: CGPoint(x: 9.2, y: 3.8))
            path.addLine(to: CGPoint(x: 4, y: 9))
            path.addLine(to: CGPoint(x: 9.2, y: 14.2))
            path.addLine(to: CGPoint(x: 14.4, y: 9))
            path.closeSubpath()
            path.addEllipse(in: CGRect(x: 12.2, y: 12.8, width: 7.2, height: 7.2))
            path.addRoundedRect(in: CGRect(x: 4.2, y: 14.6, width: 6.4, height: 6.4),
                                cornerSize: CGSize(width: 1.6, height: 1.6), style: .circular)
        case .library:
            path.addRoundedRect(in: CGRect(x: 3.5, y: 5.5, width: 17, height: 13),
                                cornerSize: CGSize(width: 1.5, height: 1.5), style: .circular)
            path.move(to: CGPoint(x: 3.5, y: 10))
            path.addLine(to: CGPoint(x: 20.5, y: 10))
            path.move(to: CGPoint(x: 7, y: 14.2))
            path.addLine(to: CGPoint(x: 11, y: 14.2))
        case .workItems:
            path.addRoundedRect(in: CGRect(x: 5, y: 4, width: 14, height: 17),
                                cornerSize: CGSize(width: 2.5, height: 2.5), style: .circular)
            path.move(to: CGPoint(x: 9, y: 4.5))
            path.addLine(to: CGPoint(x: 9, y: 3))
            path.addLine(to: CGPoint(x: 15, y: 3))
            path.addLine(to: CGPoint(x: 15, y: 4.5))
            path.move(to: CGPoint(x: 9, y: 11))
            path.addLine(to: CGPoint(x: 15, y: 11))
            path.move(to: CGPoint(x: 9, y: 15))
            path.addLine(to: CGPoint(x: 13, y: 15))
        }
        return path.applying(CGAffineTransform(scaleX: rect.width / 24, y: rect.height / 24))
    }
}

private struct MordeDesignGlyph: View {
    let kind: MordeDesignGlyphKind

    var body: some View {
        MordeDesignGlyphShape(kind: kind)
            .stroke(Color.primary, style: StrokeStyle(lineWidth: 1.8, lineCap: .round, lineJoin: .round))
    }
}

enum MordeDesignRoute: Hashable {
    case chat
    case tasks
    case approvals
    case eventChain
    case resources
    case library
    case projects
    case workItems
    case friends
    case toolChats
    case settings
    case settingsDetail(String)
    case search
    case chatConversation(String)
    case task(String)
    case workspace(String)
    case computer(String, shared: Bool)
    case file(String)

    var primaryTab: MordeDesignTab? {
        switch self {
        case .chat, .chatConversation: .chat
        case .tasks: .tasks
        case .approvals: .approvals
        case .resources: .resources
        case .eventChain, .library, .projects, .workItems, .friends, .toolChats,
             .settings, .settingsDetail, .search, .task, .workspace, .computer, .file: nil
        }
    }

    var hidesBottomBar: Bool {
        if case .computer = self { return true }
        return false
    }
}

// nanoMuse: the integrated app uses the shared drawer destination; the standalone target owns this copy.
#if !MORDE_CANONICAL
enum MIFDrawerDestination: String, CaseIterable, Identifiable {
    case chat, tasks, approvals, eventChain, resources, library, workItems, friends, toolChats

    var id: String { rawValue }
}
#endif

extension MIFDrawerDestination {
    var route: MordeDesignRoute {
        switch self {
        case .chat: .chat
        case .tasks: .tasks
        case .approvals: .approvals
        case .eventChain: .eventChain
        case .resources: .resources
        case .library: .library
        case .workItems: .workItems
        case .friends: .friends
        #if MORDE_CANONICAL
        case .projects: .projects
        #endif
        case .toolChats: .toolChats
        }
    }
}

struct MordeDesignDrawerItem: Identifiable {
    let id: String
    let title: String
    let systemImage: String?
    let customGlyph: MordeDesignGlyphKind?
    let route: MordeDesignRoute
    var badge: Int? = nil
    var isSelected = false

    init(id: String, title: String, systemImage: String?, route: MordeDesignRoute,
         customGlyph: MordeDesignGlyphKind? = nil, badge: Int? = nil, isSelected: Bool = false) {
        self.id = id
        self.title = title
        self.systemImage = systemImage
        self.customGlyph = customGlyph
        self.route = route
        self.badge = badge
        self.isSelected = isSelected
    }
}

struct MordeDesignDrawerSection: Identifiable {
    let id: String
    let title: String?
    let hint: String?
    let items: [MordeDesignDrawerItem]
    let emptyMessage: String?

    init(id: String, title: String? = nil, hint: String? = nil,
         items: [MordeDesignDrawerItem], emptyMessage: String? = nil) {
        self.id = id
        self.title = title
        self.hint = hint
        self.items = items
        self.emptyMessage = emptyMessage
    }
}

struct MordeDesignDrawerFooter {
    let settingsTitle: String
    let searchPlaceholder: String
    let newChatTitle: String

    init(settingsTitle: String, searchPlaceholder: String, newChatTitle: String) {
        self.settingsTitle = settingsTitle
        self.searchPlaceholder = searchPlaceholder
        self.newChatTitle = newChatTitle
    }
}

struct MordeDesignNavigationShell<Content: View>: View {
    private let selectedRoute: MordeDesignRoute
    private let selectedTab: MordeDesignTab?
    private let approvalCount: Int?
    private let brandTitle: String
    private let drawerSections: [MordeDesignDrawerSection]
    private let footer: MordeDesignDrawerFooter
    @Binding private var searchText: String
    private let onSelectTab: (MordeDesignTab) -> Void
    private let onSelectRoute: (MordeDesignRoute) -> Void
    private let onSettings: () -> Void
    private let onNewChat: () -> Void
    private let onOpenDrawer: () -> Void
    private let onDrawerItemAppear: (MordeDesignRoute) -> Void
    private let content: (MordeDesignRoute, @escaping () -> Void) -> Content

    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var isDrawerOpen = false

    init(selectedRoute: MordeDesignRoute, selectedTab: MordeDesignTab?, approvalCount: Int?,
         brandTitle: String = "Morde", drawerSections: [MordeDesignDrawerSection],
         footer: MordeDesignDrawerFooter, searchText: Binding<String>,
         onSelectTab: @escaping (MordeDesignTab) -> Void,
         onSelectRoute: @escaping (MordeDesignRoute) -> Void,
         onSettings: @escaping () -> Void,
         onNewChat: @escaping () -> Void, onOpenDrawer: @escaping () -> Void = {},
         onDrawerItemAppear: @escaping (MordeDesignRoute) -> Void = { _ in },
         @ViewBuilder content: @escaping (MordeDesignRoute, @escaping () -> Void) -> Content) {
        self.selectedRoute = selectedRoute
        self.selectedTab = selectedTab
        self.approvalCount = approvalCount
        self.brandTitle = brandTitle
        self.drawerSections = drawerSections
        self.footer = footer
        self._searchText = searchText
        self.onSelectTab = onSelectTab
        self.onSelectRoute = onSelectRoute
        self.onSettings = onSettings
        self.onNewChat = onNewChat
        self.onOpenDrawer = onOpenDrawer
        self.onDrawerItemAppear = onDrawerItemAppear
        self.content = content
    }

    var body: some View {
        ZStack {
            routedContent
            if isDrawerOpen { drawerOverlay.zIndex(2) }
        }
        .animation(reduceMotion ? nil : .easeInOut(duration: 0.3), value: isDrawerOpen)
    }

    @ViewBuilder
    private var routedContent: some View {
        if selectedRoute.hidesBottomBar {
            content(selectedRoute, openDrawer)
        } else {
            content(selectedRoute, openDrawer)
                .safeAreaInset(edge: .bottom, spacing: 0) {
                    bottomBar
                        .padding(.horizontal, 16)
                        .padding(.top, 8)
                        .padding(.bottom, 16)
                        .background(Color.clear)
                }
        }
    }

    private var bottomBar: some View {
        MordeDesignTabBar(selectedTab: selectedTab, approvalCount: approvalCount,
                          onSelectTab: onSelectTab)
    }

    private var drawerOverlay: some View {
        GeometryReader { geometry in
            ZStack(alignment: .leading) {
                Button(action: closeDrawer) {
                    Color.black.opacity(0.28)
                        .contentShape(Rectangle())
                        .ignoresSafeArea()
                }
                .buttonStyle(.plain)
                .accessibilityLabel(Text("Close navigation menu"))
                .accessibilityIdentifier("morde.nav.drawer.scrim")

                drawerPanel(width: min(geometry.size.width * 0.86, 322),
                            topInset: geometry.safeAreaInsets.top,
                            bottomInset: geometry.safeAreaInsets.bottom)
                    .frame(width: min(geometry.size.width * 0.86, 322))
                    .frame(maxHeight: .infinity)
                    .transition(.move(edge: .leading))
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .leading)
            .ignoresSafeArea()
        }
    }

    private func drawerPanel(width: CGFloat, topInset: CGFloat, bottomInset: CGFloat) -> some View {
        VStack(spacing: 0) {
            ScrollView {
                VStack(alignment: .leading, spacing: 0) {
                    Text(brandTitle)
                        .font(.system(size: 26, weight: .heavy))
                        .tracking(-0.5)
                        .padding(.horizontal, 12)
                        .padding(.bottom, 8)

                    ForEach(displayedDrawerSections) { section in
                        drawerSection(section)
                    }
                }
                .padding(.top, max(64, topInset + 16))
                .padding(.horizontal, 14)
                .padding(.bottom, 10)
            }
            .scrollIndicators(.hidden)
            .accessibilityIdentifier("morde.nav.drawer.scroll")

            drawerFooter(bottomInset: bottomInset)
        }
        .frame(width: width)
        .frame(maxHeight: .infinity)
        .background(Color(uiColor: .systemGroupedBackground))
        .clipShape(MordeDrawerShape())
        .shadow(color: .black.opacity(0.16), radius: 24, x: 12, y: 0)
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier("morde.nav.drawer")
    }

    @ViewBuilder
    private func drawerSection(_ section: MordeDesignDrawerSection) -> some View {
        if let title = section.title {
            Text(title)
                .font(.subheadline.weight(.semibold))
                .foregroundStyle(.secondary)
                .padding(.horizontal, 12)
                .padding(.top, 18)
        }
        if let hint = section.hint {
            Text(hint)
                .font(.subheadline)
                .foregroundStyle(.secondary)
                .padding(.horizontal, 12)
                .padding(.top, 5)
        }
        if !section.items.isEmpty {
            LazyVStack(spacing: section.id == "conversations" ? 0 : 2) {
                ForEach(section.items) { item in
                    drawerRow(item)
                }
            }
            .padding(.top, 10)
        } else if let emptyMessage = section.emptyMessage {
            Text(emptyMessage)
                .font(.footnote)
                .foregroundStyle(.secondary)
                .padding(.horizontal, 12)
                .padding(.vertical, 10)
        }
        if section.id != drawerSections.last?.id {
            Rectangle()
                .fill(Color.primary.opacity(0.08))
                .frame(height: 1)
                .padding(.horizontal, 12)
                .padding(.top, 16)
        }
    }

    private func drawerRow(_ item: MordeDesignDrawerItem) -> some View {
        Button {
            closeDrawer()
            onSelectRoute(item.route)
        } label: {
            HStack(spacing: item.systemImage == nil && item.customGlyph == nil ? 0 : 13) {
                if let customGlyph = item.customGlyph {
                    MordeDesignGlyph(kind: customGlyph)
                        .frame(width: 22, height: 22)
                        .frame(width: 24)
                } else if let systemImage = item.systemImage {
                    Image(systemName: systemImage)
                        .font(.system(size: 22, weight: .regular))
                        .frame(width: 24)
                }
                Text(item.title)
                    .font(item.systemImage == nil && item.customGlyph == nil
                          ? .system(size: 16, weight: .medium) : .body.weight(.semibold))
                    .lineLimit(1)
                Spacer(minLength: 8)
                if let badge = item.badge, badge > 0 {
                    Text(badge > 99 ? "99+" : String(badge))
                        .font(.caption.weight(.bold))
                        .foregroundStyle(.white)
                        .padding(.horizontal, 6)
                        .frame(minWidth: 19, minHeight: 19)
                        .background(Color.red, in: Capsule())
                }
            }
            .foregroundStyle(Color.primary)
            .padding(.horizontal, item.systemImage == nil ? 12 : 13)
            .padding(.vertical, 11)
            .contentShape(Capsule())
            .background {
                if item.systemImage == nil {
                    RoundedRectangle(cornerRadius: 12)
                        .fill(item.isSelected ? Color.primary.opacity(0.08) : Color.clear)
                } else {
                    Capsule().fill(item.isSelected ? Color.primary.opacity(0.08) : Color.clear)
                }
            }
        }
        .buttonStyle(.plain)
        .accessibilityLabel(Text(item.title))
        .accessibilityValue(item.badge.map { Text(String($0)) } ?? Text(""))
        .accessibilityAddTraits(item.isSelected ? .isSelected : [])
        .accessibilityIdentifier("morde.nav.drawer.\(item.id)")
        .onAppear {
            if case .chatConversation = item.route { onDrawerItemAppear(item.route) }
        }
    }

    private func drawerFooter(bottomInset: CGFloat) -> some View {
        HStack(spacing: 9) {
            NanoMuseRoundButton(symbol: "gearshape", label: footer.settingsTitle, size: 38,
                                accessibilityIdentifier: "morde.nav.drawer.settings") {
                closeDrawer()
                onSettings()
            }

            HStack(spacing: 7) {
                Image(systemName: "magnifyingglass")
                    .font(.system(size: 15, weight: .regular))
                    .foregroundStyle(.secondary)
                TextField(footer.searchPlaceholder, text: $searchText)
                    .font(.body)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                    .submitLabel(.search)
                    .accessibilityLabel(Text(footer.searchPlaceholder))
                    .accessibilityIdentifier("morde.nav.drawer.search")
            }
            .padding(.horizontal, 13)
            .frame(height: 38)
            .background(Color(uiColor: .systemBackground), in: Capsule())
            .overlay(Capsule().strokeBorder(Color.primary.opacity(0.1), lineWidth: 1))
            .accessibilityElement(children: .contain)
            .accessibilityIdentifier("morde.nav.drawer.search.container")

            NanoMuseRoundButton(symbol: "square.and.pencil", label: footer.newChatTitle, size: 38,
                                accessibilityIdentifier: "morde.nav.drawer.newChat") {
                closeDrawer()
                onNewChat()
            }
        }
        .padding(.horizontal, 14)
        .padding(.top, 12)
        .padding(.bottom, max(20, bottomInset + 8))
        .background(Color(uiColor: .systemGroupedBackground))
    }

    private func openDrawer() {
        onOpenDrawer()
        if reduceMotion { isDrawerOpen = true }
        else { withAnimation(.timingCurve(0.32, 0.72, 0.24, 1, duration: 0.34)) { isDrawerOpen = true } }
    }

    private func closeDrawer() {
        if reduceMotion { isDrawerOpen = false }
        else { withAnimation(.timingCurve(0.32, 0.72, 0.24, 1, duration: 0.34)) { isDrawerOpen = false } }
    }

    private var displayedDrawerSections: [MordeDesignDrawerSection] {
        let query = searchText.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !query.isEmpty else { return drawerSections }
        return drawerSections.map { section in
            guard section.id == "conversations" else { return section }
            let items = section.items.filter { $0.title.localizedCaseInsensitiveContains(query) }
            return MordeDesignDrawerSection(
                id: section.id, title: section.title, hint: section.hint, items: items,
                emptyMessage: items.isEmpty ? AppLocalized("No chats found") : section.emptyMessage)
        }
    }
}

struct MordeDesignTabBar: View {
    let selectedTab: MordeDesignTab?
    let approvalCount: Int?
    let onSelectTab: (MordeDesignTab) -> Void

    var body: some View {
        HStack(spacing: 2) {
            ForEach(MordeDesignTab.allCases) { tab in
                tabButton(tab)
            }
        }
        .padding(5)
        .frame(height: 62)
        .background(.ultraThinMaterial, in: Capsule())
        .overlay(Capsule().strokeBorder(Color.primary.opacity(0.08), lineWidth: 1))
        .shadow(color: .black.opacity(0.08), radius: 12, y: 5)
    }

    private func tabButton(_ tab: MordeDesignTab) -> some View {
        let isSelected = selectedTab == tab
        return Button { onSelectTab(tab) } label: {
            ZStack(alignment: .topTrailing) {
                Group {
                    if let customGlyph = tab.customGlyph {
                        MordeDesignGlyph(kind: customGlyph)
                            .frame(width: 23, height: 23)
                    } else if let symbol = tab.symbol {
                        Image(systemName: symbol)
                            .font(.system(size: 22, weight: .regular))
                    }
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .contentShape(Capsule())
                if tab == .approvals, let approvalCount, approvalCount > 0 {
                    Text(approvalCount > 99 ? "99+" : String(approvalCount))
                        .font(.caption2.weight(.bold))
                        .foregroundStyle(.white)
                        .padding(.horizontal, 4)
                        .frame(minWidth: 17, minHeight: 17)
                        .background(Color.red, in: Capsule())
                        .overlay(Capsule().strokeBorder(Color(uiColor: .systemBackground), lineWidth: 2))
                        .offset(x: -8, y: 2)
                }
            }
            .foregroundStyle(Color.primary)
            .background(isSelected ? Color.primary.opacity(0.08) : Color.clear, in: Capsule())
            .frame(maxWidth: .infinity)
            .frame(height: 52)
        }
        .buttonStyle(.plain)
        .accessibilityLabel(Text(tab.title))
        .accessibilityValue(isSelected ? Text("Selected") : Text(""))
        .accessibilityAddTraits(isSelected ? .isSelected : [])
        .accessibilityRemoveTraits(isSelected ? [] : .isSelected)
        .accessibilityIdentifier("morde.nav.tab.\(tab.id)")
    }
}

struct MordeFilePreviewNavigation {
    let selectedTab: MordeDesignTab?
    let approvalCount: Int?
    let onSelectTab: (MordeDesignTab) -> Void
}

private struct MordeDrawerShape: Shape {
    func path(in rect: CGRect) -> Path {
        let radius: CGFloat = 26
        var path = Path()
        path.move(to: CGPoint(x: rect.minX, y: rect.minY))
        path.addLine(to: CGPoint(x: rect.maxX - radius, y: rect.minY))
        path.addQuadCurve(to: CGPoint(x: rect.maxX, y: rect.minY + radius),
                          control: CGPoint(x: rect.maxX, y: rect.minY))
        path.addLine(to: CGPoint(x: rect.maxX, y: rect.maxY - radius))
        path.addQuadCurve(to: CGPoint(x: rect.maxX - radius, y: rect.maxY),
                          control: CGPoint(x: rect.maxX, y: rect.maxY))
        path.addLine(to: CGPoint(x: rect.minX, y: rect.maxY))
        path.closeSubpath()
        return path
    }
}

// nanoMuse: the integrated target reuses these shared room types from NanoMuseShell.swift.
#if !MORDE_CANONICAL
struct NanoMuseRoomChrome {
    var liveId: String?
    var name: String
    var hasMainSession: Bool
    var onMenu: () -> Void
    var onBack: (() -> Void)?
    var showsLibraryShortcuts: Bool
    var onOpenPage: ((String) -> Void)?

    init(liveId: String? = nil, name: String = "Morde", hasMainSession: Bool = false,
         onMenu: @escaping () -> Void, onBack: (() -> Void)? = nil,
         showsLibraryShortcuts: Bool = false, onOpenPage: ((String) -> Void)? = nil) {
        self.liveId = liveId
        self.name = name
        self.hasMainSession = hasMainSession
        self.onMenu = onMenu
        self.onBack = onBack
        self.showsLibraryShortcuts = showsLibraryShortcuts
        self.onOpenPage = onOpenPage
    }

    func open(_ page: String) { onOpenPage?(page) }
}

struct NanoMuseRoomHeader<Trailing: View>: View {
    var chrome: NanoMuseRoomChrome
    @ViewBuilder var trailing: () -> Trailing

    init(chrome: NanoMuseRoomChrome, @ViewBuilder trailing: @escaping () -> Trailing) {
        self.chrome = chrome
        self.trailing = trailing
    }

    var body: some View {
        MordeDesignPageHeader(title: chrome.name, onMenu: chrome.onMenu, trailing: trailing)
    }
}
#endif
