//
//  NanoMuseContextBudget.swift
//  nanoMuse
//
//  The phone's own ceiling on how much of a conversation goes to the model per call. The
//  policy's tiers compact a conversation 20k tokens short of the model's window, 108k on the
//  128k window the hosted model is read as, and move old big tool results to disk from 88k.
//  Over the hosted model's last week the time to the first token grew with the prompt: 4.7 s
//  at the median for 8 to 12k tokens, 9 s for 16 to 24k, 25 s for 24 to 40k, 54 s above 40k
//  (80 to 138 s at the 95th percentile), with 38 % of the calls over 16k; each day about 770
//  calls waited the relay's full 180 s for nothing. So from 1.0.8 the phone compacts at 32k
//  whatever the window and offloads from 28k, keeping the recent exchanges whole as before;
//  the tiers under 64k, which never reach these figures, are untouched. Android: ContextBudget.
//

import Foundation

enum NanoMuseContextBudget {
    /// Above this the conversation is summarised before the next call.
    static let compactTokens = 32_000
    /// Above this the next big tool result of an older message is written to disk.
    static let offloadTokens = 28_000
    /// What offloading works down to.
    static let offloadTargetTokens = 22_000

    /// The tier's policy for `contextWindow`, its lines pulled down to the budget where they were above it.
    static func policy(contextWindow: Int) -> ContextPolicy {
        let tier = ContextPolicy(contextWindow: contextWindow)
        guard tier.compactThreshold > compactTokens else { return tier }
        return ContextPolicy(
            offloadThreshold: min(tier.offloadThreshold, offloadTokens),
            offloadTarget: min(tier.offloadTarget, offloadTargetTokens),
            compactThreshold: compactTokens,
            exhaustedOnly: tier.exhaustedOnly,
            manualCompactAllowed: tier.manualCompactAllowed
        )
    }
}

extension ContextPolicy {
    /// nanoMuse: the figures as given, for `NanoMuseContextBudget`.
    init(offloadThreshold: Int, offloadTarget: Int, compactThreshold: Int, exhaustedOnly: Bool, manualCompactAllowed: Bool) {
        self.offloadThreshold = offloadThreshold
        self.offloadTarget = offloadTarget
        self.compactThreshold = compactThreshold
        self.exhaustedOnly = exhaustedOnly
        self.manualCompactAllowed = manualCompactAllowed
    }
}
