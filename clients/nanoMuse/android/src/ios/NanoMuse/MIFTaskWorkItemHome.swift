import SwiftUI

enum MIFTaskWorkItemSection: String, CaseIterable, Identifiable {
    case tasks
    case workItems

    var id: String { rawValue }
    var title: String {
        switch self {
        case .tasks: AppLocalized("Task")
        case .workItems: AppLocalized("Work items")
        }
    }
}

enum MIFTaskHomeDestination: Equatable {
    case tasks, workItems, eventChain
}

struct MIFTaskWorkItemSectionPicker: View {
    @Binding var selection: MIFTaskWorkItemSection

    var body: some View {
        Picker(AppLocalized("Task"), selection: $selection) {
            ForEach(MIFTaskWorkItemSection.allCases) { section in
                Text(section.title).tag(section)
            }
        }
        .pickerStyle(.segmented)
        .padding(.horizontal, 16)
        .padding(.vertical, 8)
        .accessibilityIdentifier("morde.tasks.sectionPicker")
    }
}

@MainActor
struct MIFTaskWorkItemHome: View {
    var chrome: NanoMuseRoomChrome? = nil
    var onMenu: (() -> Void)? = nil
    var onNewConversation: (() -> Void)? = nil
    @Binding var destination: MIFTaskHomeDestination

    init(chrome: NanoMuseRoomChrome? = nil,
         destination: Binding<MIFTaskHomeDestination> = .constant(.tasks),
         onMenu: (() -> Void)? = nil,
         onNewConversation: (() -> Void)? = nil) {
        self.chrome = chrome
        self._destination = destination
        self.onMenu = onMenu
        self.onNewConversation = onNewConversation
    }

    var body: some View {
        switch destination {
        case .tasks:
            MIFTaskRoom(chrome: chrome, onMenu: onMenu, onNewConversation: onNewConversation)
        case .workItems:
            MIFWorkItemRoom(chrome: chrome, onMenu: onMenu)
        case .eventChain:
            MIFTaskEventChainRoom(chrome: chrome, onMenu: onMenu)
        }
    }
}
