import SwiftUI

struct MIFPlaceholderPage: View {
    let destination: MIFDrawerDestination
    var onMenu: (() -> Void)? = nil
    var onBack: (() -> Void)? = nil
    var body: some View {
        VStack(spacing: 0) {
            if destination == .friends, let onMenu {
                MordeDesignPageHeader(title: AppLocalized("Friends"), onMenu: onMenu)
            } else if destination == .toolChats, let onBack {
                HStack(spacing: 10) {
                    NanoMuseRoundButton(symbol: "chevron.left", label: AppLocalized("Back"),
                                        size: 38, action: onBack)
                        .accessibilityIdentifier("morde.toolChats.back")
                    Text(AppLocalized("AI tool conversations"))
                        .font(.headline.weight(.semibold))
                        .lineLimit(1)
                    Spacer(minLength: 0)
                }
                .padding(.horizontal, 22)
                .padding(.top, 10)
                .padding(.bottom, 4)
            }

            Text(AppLocalized("Not implemented yet"))
                .font(.body)
                .foregroundStyle(NanoMuseTones.secondaryText)
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .accessibilityIdentifier("morde.placeholder.\(destination.rawValue)")
        }
        .background(NanoMuseTones.canvas.ignoresSafeArea())
    }
}
