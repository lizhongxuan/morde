import SwiftUI

struct MIFPlaceholderPage: View {
    enum Kind {
        case friends
        case toolChats

        var title: String {
            switch self {
            case .friends: return AppLocalized("Friends")
            case .toolChats: return AppLocalized("AI tool conversations")
            }
        }

        var symbol: String {
            switch self {
            case .friends: return "person.2"
            case .toolChats: return "wrench.and.screwdriver"
            }
        }

        var accessibilityIdentifier: String {
            switch self {
            case .friends: return "morde.placeholder.friends"
            case .toolChats: return "morde.placeholder.toolChats"
            }
        }
    }

    let kind: Kind

    var body: some View {
        NanoMuseEmptyState(
            symbol: kind.symbol,
            title: kind.title,
            message: AppLocalized("Not implemented yet")
        )
        .accessibilityElement(children: .combine)
        .accessibilityIdentifier(kind.accessibilityIdentifier)
    }
}
