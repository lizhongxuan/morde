import XCTest
@testable import Minis

final class ResponsesCallIdDeduplicationTests: XCTestCase {
    func testNaturalSuffixCollisionStaysUniqueAndKeepsOutputsPaired() {
        let longId = String(repeating: "x", count: 64)
        let input: [[String: Any]] = [
            ["type": "reasoning", "id": "rs_keep", "summary": []],
            ["type": "function_call", "call_id": "call_same"],
            ["type": "function_call", "call_id": "call_same"],
            ["type": "function_call", "call_id": "call_same-2"],
            ["type": "function_call_output", "call_id": "call_same"],
            ["type": "function_call_output", "call_id": "call_same"],
            ["type": "function_call_output", "call_id": "call_same-2"],
            ["type": "function_call", "call_id": "call_same"],
            ["type": "function_call", "call_id": longId],
            ["type": "function_call", "call_id": longId],
            ["type": "function_call_output", "call_id": "call_same"],
            ["type": "function_call_output", "call_id": longId],
            ["type": "function_call_output", "call_id": longId],
        ]

        let result = OpenAIAgentProvider.dedupeResponsesCallIds(input)
        let calls = result.compactMap { item -> String? in
            (item["type"] as? String) == "function_call" ? item["call_id"] as? String : nil
        }
        let outputs = result.compactMap { item -> String? in
            (item["type"] as? String) == "function_call_output" ? item["call_id"] as? String : nil
        }

        XCTAssertEqual(
            calls,
            [
                "call_same",
                "call_same-2",
                "call_same-2-2",
                "call_same-3",
                longId,
                "\(String(repeating: "x", count: 62))-2",
            ]
        )
        XCTAssertEqual(outputs, calls)
        XCTAssertEqual(Set(calls).count, calls.count)
        XCTAssertTrue(calls.allSatisfy { $0.count <= 64 })
        XCTAssertEqual(result[0]["id"] as? String, "rs_keep")
    }
}
