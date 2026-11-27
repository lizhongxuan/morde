import Foundation
import OSLog

struct MordeTraceContext: Equatable {
    private static let logger = Logger(subsystem: Bundle.main.bundleIdentifier ?? "Morde", category: "network")

    let traceID: String
    let spanID: String

    init() {
        traceID = UUID().uuidString.replacingOccurrences(of: "-", with: "").lowercased()
        spanID = String(UUID().uuidString.replacingOccurrences(of: "-", with: "").lowercased().suffix(16))
    }

    var traceparent: String { "00-\(traceID)-\(spanID)-01" }

    func inject(into request: inout URLRequest) {
        if request.value(forHTTPHeaderField: "X-Request-Id") == nil {
            request.setValue(UUID().uuidString, forHTTPHeaderField: "X-Request-Id")
        }
        request.setValue(traceparent, forHTTPHeaderField: "traceparent")
    }

    static func responseTraceID(from response: HTTPURLResponse) -> String? {
        guard let value = response.value(forHTTPHeaderField: "X-Trace-Id")?.lowercased(),
              value.count == 32, value.unicodeScalars.allSatisfy({ CharacterSet(charactersIn: "0123456789abcdef").contains($0) }),
              value != String(repeating: "0", count: 32) else { return nil }
        return value
    }

    func recordResponse(for request: URLRequest, response: HTTPURLResponse) {
        let requestID = request.value(forHTTPHeaderField: "X-Request-Id") ?? "unavailable"
        let traceID = Self.responseTraceID(from: response) ?? self.traceID
        Self.logger.info("HTTP response request_id=\(requestID, privacy: .public) trace_id=\(traceID, privacy: .public) status=\(response.statusCode, privacy: .public)")
    }
}
