import XCTest
@testable import Minis

final class NanoMusePromptCacheLayoutTests: XCTestCase {
    func testStaticInstructionsPrecedeMemoryAndRuntimeContext() throws {
        let parts = NanoMusePromptCacheLayout.build(
            stableSections: ["BASE INSTRUCTIONS", "RISK POLICY", "HANDS POLICY"],
            volatileSections: ["SESSION ADDENDUM", "GLOBAL.md: remembered"],
            runtimeContext: "Runtime context: today"
        )

        XCTAssertLessThan(
            try XCTUnwrap(parts.prompt.range(of: "RISK POLICY")?.lowerBound),
            try XCTUnwrap(parts.prompt.range(of: "GLOBAL.md")?.lowerBound)
        )
        XCTAssertLessThan(
            try XCTUnwrap(parts.prompt.range(of: "HANDS POLICY")?.lowerBound),
            try XCTUnwrap(parts.prompt.range(of: "Runtime context")?.lowerBound)
        )
        XCTAssertTrue(parts.prompt.hasSuffix("Runtime context: today"))
    }

    func testChangingOnlyVolatileContentPreservesByteStablePrefix() {
        let first = NanoMusePromptCacheLayout.build(
            stableSections: ["BASE INSTRUCTIONS", "RISK POLICY", "HANDS POLICY"],
            volatileSections: ["Recent memories: first"],
            runtimeContext: "Runtime context: day one"
        )
        let second = NanoMusePromptCacheLayout.build(
            stableSections: ["BASE INSTRUCTIONS", "RISK POLICY", "HANDS POLICY"],
            volatileSections: ["Recent memories: second"],
            runtimeContext: "Runtime context: day two"
        )

        XCTAssertEqual(first.stablePrefix, second.stablePrefix)
        XCTAssertNotEqual(first.volatileSuffix, second.volatileSuffix)
        XCTAssertEqual(first.prompt, first.stablePrefix + first.volatileSuffix)
        XCTAssertEqual(second.prompt, second.stablePrefix + second.volatileSuffix)
    }

    func testFallbackPrependsModelSectionsWithoutLosingOrDuplicatingSharedSuffix() {
        let suffix = NanoMusePromptCacheLayout.makeVolatileSuffix(
            volatileSections: ["SKILLS", "GLOBAL.md", "USER.md"],
            runtimeContext: "Runtime context"
        )
        let parts = NanoMusePromptCacheLayout.buildWithSuffix(
            stableSections: ["BASE"],
            leadingVolatileSections: ["MODEL CAPABILITIES", "MODEL BEHAVIOR"],
            suffix: suffix
        )

        XCTAssertEqual(
            parts.prompt,
            "BASE\n\nMODEL CAPABILITIES\n\nMODEL BEHAVIOR\n\nSKILLS\n\nGLOBAL.md\n\nUSER.md\n\nRuntime context"
        )
        XCTAssertEqual(parts.prompt.components(separatedBy: "Runtime context").count - 1, 1)
        XCTAssertTrue(parts.prompt.hasSuffix("Runtime context"))
    }
}
