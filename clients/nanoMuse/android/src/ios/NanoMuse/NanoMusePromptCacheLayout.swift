//
//  NanoMusePromptCacheLayout.swift
//  Minis
//
//  Keeps provider-cacheable instructions ahead of session-dependent prompt data.
//

import Foundation

struct NanoMusePromptCacheLayout {
    struct VolatileSuffix {
        fileprivate let text: String
    }

    struct Parts {
        let stablePrefix: String
        let volatileSuffix: String

        var prompt: String { stablePrefix + volatileSuffix }
    }

    static func build(
        stableSections: [String],
        volatileSections: [String?],
        runtimeContext: String?
    ) -> Parts {
        buildWithSuffix(
            stableSections: stableSections,
            leadingVolatileSections: [],
            suffix: makeVolatileSuffix(
                volatileSections: volatileSections,
                runtimeContext: runtimeContext
            )
        )
    }

    /// Runtime context is a separate argument so it is always the suffix's final section.
    static func makeVolatileSuffix(
        volatileSections: [String?],
        runtimeContext: String?
    ) -> VolatileSuffix {
        VolatileSuffix(text: join(volatileSections.compactMap { $0 } + [runtimeContext].compactMap { $0 }))
    }

    /// Adds model-specific mutable sections without unpacking or duplicating the shared suffix.
    static func buildWithSuffix(
        stableSections: [String],
        leadingVolatileSections: [String?],
        suffix: VolatileSuffix
    ) -> Parts {
        let stablePrefix = join(stableSections)
        let volatileBody = join(leadingVolatileSections.compactMap { $0 } + [suffix.text])
        return Parts(
            stablePrefix: stablePrefix,
            volatileSuffix: volatileBody.isEmpty ? "" : "\n\n" + volatileBody
        )
    }

    private static func join(_ sections: [String]) -> String {
        sections
            .map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }
            .filter { !$0.isEmpty }
            .joined(separator: "\n\n")
    }
}
