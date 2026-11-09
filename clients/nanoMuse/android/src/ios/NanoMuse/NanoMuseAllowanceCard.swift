//
//  NanoMuseAllowanceCard.swift
//  nanoMuse
//
//  `NanoMuseAllowanceCard` is pinned under the chat header when a turn was refused;
//  `NanoMuseAllowanceHeadsUp` is the one line above the composer at 80 %.
//

import SwiftUI

/// Pinned under the chat header when a turn was refused for the allowance.
struct NanoMuseAllowanceCard: View {
    let refused: NanoMuseAllowance.Refused
    var onDismiss: () -> Void

    private var title: String {
        if refused.paused { return AppLocalized("The free allowance is paused on this relay for now. It is not used up.") }
        if refused.dailyCap { return AppLocalized("Today's share of the free allowance is used up.") }
        return AppLocalized("The free allowance is used up.")
    }

    private var subtitle: String {
        AppLocalized("Your sign-in and your devices keep working either way.")
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(alignment: .top, spacing: 10) {
                Image(systemName: refused.paused ? "pause.circle" : "gauge").font(.system(size: 16, weight: .medium)).foregroundStyle(Color(red: 0.49, green: 0.36, blue: 1.0)).padding(.top, 2)
                VStack(alignment: .leading, spacing: 3) {
                    Text(title).font(.subheadline.weight(.semibold))
                    Text(subtitle).font(.footnote).foregroundStyle(.secondary)
                }
                Spacer(minLength: 4)
                Button(action: onDismiss) {
                    Image(systemName: "xmark").font(.system(size: 12, weight: .semibold)).foregroundStyle(.secondary).padding(4)
                }
                .buttonStyle(.plain)
                .accessibilityLabel(Text(AppLocalized("Not now")))
            }
            // nanoMuse: Keep the referral link; Morde does not offer legacy keys or plan sign-ins.
            if (refused.facts.inviteBonusCny ?? 0) > 0,
               !refused.facts.inviteURL.isEmpty,
               let url = URL(string: refused.facts.inviteURL) {
                ShareLink(item: url) { Label(AppLocalized("Share the link"), systemImage: "square.and.arrow.up").font(.footnote) }
                    .buttonStyle(.plain).foregroundStyle(NanoMuseTones.action)
            }
            // nanoMuse: Relay guidance and invite sharing remain available; this card no longer retries an old local VM.
            Button(AppLocalized("Open Settings")) { NotificationCenter.default.post(name: .nanoMuseHomeAction, object: "settings") }
                .buttonStyle(.plain).font(.footnote).foregroundStyle(NanoMuseTones.action)
        }
    }
}

/// The one line above the composer at 80 % of the pool (`spend.warn`), once per pool size.
struct NanoMuseAllowanceHeadsUp: View {
    var text: String
    var onHide: () -> Void

    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: "gauge").font(.system(size: 13, weight: .medium)).foregroundStyle(Color(red: 0.88, green: 0.47, blue: 0.17)).padding(.top, 2)
            Text(text).font(.caption).foregroundStyle(.secondary)
            Spacer(minLength: 4)
            Button(action: onHide) {
                Image(systemName: "xmark").font(.system(size: 11, weight: .semibold)).foregroundStyle(.secondary).padding(3)
            }
            .buttonStyle(.plain)
            .accessibilityLabel(Text(AppLocalized("Not now")))
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .background(NanoMuseTones.fill, in: RoundedRectangle(cornerRadius: 14, style: .continuous))
    }
}
