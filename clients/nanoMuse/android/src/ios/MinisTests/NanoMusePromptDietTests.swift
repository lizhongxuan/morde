import XCTest
@testable import Minis

/// The prompt diet (1.0.8): `browser_use` keeps every action and parameter in fewer characters,
/// and the compaction line is 32k on every window from 64k up. The relay's "where you stand"
/// field is read for the step whose dots were already showing when it came.
final class NanoMusePromptDietTests: XCTestCase {
    private func browserUse(parameters: [String: AgentToolParam]) -> AgentToolDefinition {
        AgentToolDefinition(
            name: "browser_use",
            description: String(repeating: "Use get_cookies to retrieve cookies. ", count: 200),
            parameters: parameters,
            required: ["tool_title", "action"],
            propertyOrdering: ["tool_title", "action", "cookies"]
        )
    }

    func testBrowserUseKeepsEveryActionAndParameterInFewerCharacters() {
        let actions = BrowserAction.allCases.map(\.rawValue)
        let before = browserUse(parameters: [
            "tool_title": AgentToolParam(type: .string, description: "A concise 5-10 word summary."),
            "action": AgentToolParam(type: .string, description: "The browser action to perform", enumValues: actions),
            "cookies": AgentToolParam(type: .string, description: String(repeating: "cookie objects ", count: 80)),
            "new_param": AgentToolParam(type: .integer, description: "A parameter this diet does not know."),
        ])
        let other = AgentToolDefinition(name: "shell_execute", description: "Run a command.", parameters: [:], required: [])
        let out = NanoMusePromptDiet.tools([other, before])
        XCTAssertEqual(out.count, 2)
        XCTAssertEqual(out[0].name, "shell_execute")
        XCTAssertEqual(out[0].description, "Run a command.")
        let after = out[1]
        XCTAssertEqual(after.name, "browser_use")
        for action in actions { XCTAssertTrue(after.description.contains(action), action) }
        XCTAssertEqual(Set(after.parameters.keys), Set(before.parameters.keys))
        XCTAssertEqual(after.parameters["action"]?.enumValues, actions)
        XCTAssertEqual(after.parameters["action"]?.type, .string)
        XCTAssertEqual(after.parameters["new_param"]?.description, "A parameter this diet does not know.")
        XCTAssertEqual(after.parameters["tool_title"]?.description, "A concise 5-10 word summary.")
        XCTAssertLessThan(after.parameters["cookies"]!.description.count, before.parameters["cookies"]!.description.count / 2)
        XCTAssertLessThan(after.description.count, before.description.count)
        XCTAssertEqual(after.required, before.required)
        XCTAssertEqual(after.propertyOrdering, before.propertyOrdering)
        XCTAssertTrue(after.description.contains("Do NOT use this tool for minis:// action URLs"))
    }

    func testCompactionIsAt32kOnEveryWindowFrom64kUp() {
        for window in [64_000, 100_000, 128_000, 200_000, 1_000_000] {
            let p = NanoMuseContextBudget.policy(contextWindow: window)
            XCTAssertEqual(p.compactThreshold, 32_000, "\(window)")
            XCTAssertEqual(p.offloadThreshold, 28_000, "\(window)")
            XCTAssertEqual(p.offloadTarget, 22_000, "\(window)")
            XCTAssertFalse(p.exhaustedOnly)
            XCTAssertTrue(p.manualCompactAllowed)
            XCTAssertEqual(p.check(estimatedTokens: 31_999, contextWindow: window), .ok)
            XCTAssertEqual(p.check(estimatedTokens: 32_000, contextWindow: window), .needsCompact)
            XCTAssertTrue(p.shouldOffload(estimatedTokens: 28_000))
            XCTAssertFalse(p.shouldOffload(estimatedTokens: 27_999))
        }
        // the small tiers never reached the figures and are as upstream left them
        let small = NanoMuseContextBudget.policy(contextWindow: 40_000)
        XCTAssertEqual(small.compactThreshold, 0)
        XCTAssertEqual(small.offloadThreshold, 30_000)
        XCTAssertTrue(small.exhaustedOnly)
        XCTAssertEqual(NanoMuseContextBudget.policy(contextWindow: 16_000).offloadThreshold, 0)
    }

    func testAQueuedStatusIsReadForTheStepItArrivedIn() {
        let at = Date()
        let s = NanoMuseStreamStatus.Status(phase: "queued", ahead: 3, waitedMs: 5004, at: at)
        XCTAssertTrue(s.queued(now: at.addingTimeInterval(1), since: at.addingTimeInterval(-6)))
        // the dots of a later step appeared after it: an earlier step's news, not this one's
        XCTAssertFalse(s.queued(now: at.addingTimeInterval(1), since: at.addingTimeInterval(0.5)))
        // the relay repeats it every five seconds; one it stopped repeating is stale
        XCTAssertFalse(s.queued(now: at.addingTimeInterval(NanoMuseStreamStatus.staleSeconds + 1), since: at.addingTimeInterval(-6)))
        let working = NanoMuseStreamStatus.Status(phase: "working", ahead: -1, waitedMs: 12_000, at: at)
        XCTAssertFalse(working.queued(now: at, since: .distantPast))
    }
}
