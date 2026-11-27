import XCTest
@testable import MordeTaskPolicies

final class MordeTraceContextTests: XCTestCase {
    func testInjectsW3CContextWithoutChangingRequestIDAndReadsResponseTraceID() throws {
        var request = URLRequest(url: URL(string: "https://portal.example/api/test")!)
        request.setValue("existing-request-id", forHTTPHeaderField: "X-Request-Id")

        let context = MordeTraceContext()
        context.inject(into: &request)

        XCTAssertEqual(request.value(forHTTPHeaderField: "X-Request-Id"), "existing-request-id")
        let traceparent = try XCTUnwrap(request.value(forHTTPHeaderField: "traceparent"))
        let parts = traceparent.split(separator: "-")
        XCTAssertEqual(parts.count, 4)
        XCTAssertEqual(parts[0], "00")
        XCTAssertEqual(parts[1].count, 32)
        XCTAssertEqual(parts[2].count, 16)
        XCTAssertEqual(parts[3], "01")
        XCTAssertTrue(parts[1].allSatisfy(\.isHexDigit))
        XCTAssertTrue(parts[2].allSatisfy(\.isHexDigit))
        XCTAssertNotEqual(String(parts[1]), String(repeating: "0", count: 32))
        XCTAssertNotEqual(String(parts[2]), String(repeating: "0", count: 16))

        let response = try XCTUnwrap(HTTPURLResponse(url: request.url!, statusCode: 200,
            httpVersion: "HTTP/1.1", headerFields: ["X-Trace-Id": String(repeating: "a", count: 32)]))
        XCTAssertEqual(MordeTraceContext.responseTraceID(from: response), String(repeating: "a", count: 32))
    }

    func testRejectsMalformedOrAllZeroResponseTraceIDs() throws {
        for value in ["bad", String(repeating: "0", count: 32), String(repeating: "g", count: 32)] {
            let response = try XCTUnwrap(HTTPURLResponse(url: URL(string: "https://portal.example")!, statusCode: 500,
                httpVersion: "HTTP/1.1", headerFields: ["X-Trace-Id": value]))
            XCTAssertNil(MordeTraceContext.responseTraceID(from: response))
        }
    }
}
