import Foundation

@main
enum MordeHubRelayTests {
    static func main() async throws {
        try endpointPolicy()
        try requestPolicy()
        try welcomePolicy()
        try responsePolicy()
        try await disconnectedRelayDoesNotConfirm()
        print("Morde Hub relay policy checks passed")
    }

    private static func endpointPolicy() throws {
        let endpoint = try MordeHubRelayPolicy.endpoint(portalServer: URL(string: "https://portal.example:443/")!)
        precondition(endpoint.url.absoluteString == "wss://portal.example/api/hub")
        precondition(endpoint.origin == "https://portal.example")

        for value in [
            "http://portal.example",
            "wss://portal.example",
            "https://user@portal.example",
            "https://portal.example/path",
            "https://portal.example?next=/",
            "https://portal.example/#fragment",
        ] {
            try expectError(.invalidPortalServer) {
                _ = try MordeHubRelayPolicy.endpoint(portalServer: URL(string: value)!)
            }
        }
    }

    private static func requestPolicy() throws {
        try MordeHubRelayPolicy.validateCredential("chc_t1234567_" + String(repeating: "A", count: 43))
        try expectError(.invalidCredential) {
            try MordeHubRelayPolicy.validateCredential("chs_t1234567_" + String(repeating: "A", count: 43))
        }
        try expectError(.invalidCredential) { try MordeHubRelayPolicy.validateCredential("chc_token") }

        try MordeHubRelayPolicy.validateTarget("i1234567")
        try expectError(.invalidTarget) { try MordeHubRelayPolicy.validateTarget("i123456") }
        try expectError(.invalidTarget) { try MordeHubRelayPolicy.validateTarget("../admin") }

        for value in ["https://example.com", "http://example.com/path?q=1"] {
            let url = URL(string: value)!
            let validatedURL = try MordeHubRelayPolicy.validateOpenURL(url)
            precondition(validatedURL == value)
        }
        for value in [
            "file:///etc/passwd",
            "javascript:alert(1)",
            "https://user:secret@example.com",
            "https://example.com/" + String(repeating: "a", count: 2_100),
        ] {
            try expectError(.invalidURL) { try MordeHubRelayPolicy.validateOpenURL(URL(string: value)!) }
        }

        let call = try MordeHubRelayPolicy.makeOpenCall(
            url: "https://example.com/path", target: "i1234567", id: "call-1234")
        let frame = try JSONSerialization.jsonObject(with: call) as! [String: Any]
        precondition(frame["type"] as? String == "call")
        precondition(frame["id"] as? String == "call-1234")
        precondition(frame["to"] as? String == "i1234567")
        precondition(frame["action"] as? String == "open")
        precondition((frame["args"] as? [String: String])?["url"] == "https://example.com/path")
        try expectError(.invalidURL) {
            _ = try MordeHubRelayPolicy.makeOpenCall(url: "file:///tmp/a", target: "i1234567", id: "call-1234")
        }
    }

    private static func welcomePolicy() throws {
        let welcome = json([
            "type": "welcome",
            "device_id": "i7654321",
            "devices": [device(id: "i1234567", online: true), device(id: "i7654321", online: true)],
            "server": ["frame_limit": 65_536, "time": 1_793_520_000],
        ])
        let currentID = try MordeHubRelayPolicy.validateWelcome(welcome, target: "i1234567")
        precondition(currentID == "i7654321")

        try expectError(.targetOffline) {
            _ = try MordeHubRelayPolicy.validateWelcome(
                json(["type": "welcome", "device_id": "i7654321",
                      "devices": [device(id: "i1234567", online: false)],
                      "server": ["frame_limit": 65_536, "time": 1_793_520_000]]),
                target: "i1234567")
        }
        try expectError(.targetOffline) {
            _ = try MordeHubRelayPolicy.validateWelcome(
                json(["type": "welcome", "device_id": "i7654321",
                      "devices": [device(id: "i7654321", online: true)],
                      "server": ["frame_limit": 65_536, "time": 1_793_520_000]]),
                target: "i1234567")
        }
        try expectError(.targetIsCurrentDevice) {
            _ = try MordeHubRelayPolicy.validateWelcome(
                json(["type": "welcome", "device_id": "i1234567",
                      "devices": [device(id: "i1234567", online: true)],
                      "server": ["frame_limit": 65_536, "time": 1_793_520_000]]),
                target: "i1234567")
        }
        try expectError(.malformedFrame) {
            _ = try MordeHubRelayPolicy.validateWelcome(
                json(["type": "welcome", "device_id": "i7654321",
                      "devices": [device(id: "i1234567", online: true)],
                      "server": ["frame_limit": 1_024, "time": 1_793_520_000]]),
                target: "i1234567")
        }
    }

    private static func responsePolicy() throws {
        let opened = try MordeHubRelayPolicy.validatePendingFrame(
            json(["type": "result", "id": "call-1234", "ok": true, "body": ["opened": true]]),
            target: "i1234567", callID: "call-1234")
        precondition(opened)

        let pending = try MordeHubRelayPolicy.validatePendingFrame(
            json(["type": "devices", "devices": [device(id: "i1234567", online: true)]]),
            target: "i1234567", callID: "call-1234")
        precondition(!pending)

        try expectError(.callRejected("denied")) {
            _ = try MordeHubRelayPolicy.validatePendingFrame(
                json(["type": "result", "id": "call-1234", "ok": false, "error": "denied"]),
                target: "i1234567", callID: "call-1234")
        }
        try expectError(.callRejected("forbidden")) {
            _ = try MordeHubRelayPolicy.validatePendingFrame(
                json(["type": "result", "id": "call-1234", "ok": false, "error": "forbidden"]),
                target: "i1234567", callID: "call-1234")
        }
        try expectError(.callRejected("device_offline")) {
            _ = try MordeHubRelayPolicy.validatePendingFrame(
                json(["type": "error", "id": "call-1234", "code": "device_offline", "message": "offline"]),
                target: "i1234567", callID: "call-1234")
        }
        try expectError(.malformedFrame) {
            _ = try MordeHubRelayPolicy.validatePendingFrame(
                json(["type": "result", "id": "call-1234", "ok": true, "body": ["opened": false]]),
                target: "i1234567", callID: "call-1234")
        }
        try expectError(.malformedFrame) {
            _ = try MordeHubRelayPolicy.validatePendingFrame(
                json(["type": "result", "id": "call-1234", "ok": true,
                      "body": ["opened": true, "path": "/tmp"]]),
                target: "i1234567", callID: "call-1234")
        }
        try expectError(.unsupportedFrame) {
            _ = try MordeHubRelayPolicy.validatePendingFrame(
                json(["type": "screenshot", "id": "call-1234"]),
                target: "i1234567", callID: "call-1234")
        }
        try expectError(.unsupportedFrame) {
            _ = try MordeHubRelayPolicy.validatePendingFrame(
                json(["type": "result", "id": "another-call", "ok": true, "body": ["opened": true]]),
                target: "i1234567", callID: "call-1234")
        }
    }

    private static func disconnectedRelayDoesNotConfirm() async throws {
        var sentPendingDevices = false
        do {
            try await MordeHubRelay.awaitOpenConfirmation(target: "i1234567", callID: "call-1234") {
                if !sentPendingDevices {
                    sentPendingDevices = true
                    return json(["type": "devices", "devices": [device(id: "i1234567", online: true)]])
                }
                throw URLError(.networkConnectionLost)
            }
            fatalError("a disconnected Hub relay was reported as confirmed")
        } catch let error as URLError {
            precondition(error.code == .networkConnectionLost)
        }
    }

    private static func device(id: String, online: Bool) -> [String: Any] {
        ["id": id, "name": "Test Device", "platform": "ios", "online": online]
    }

    private static func json(_ value: Any) -> Data {
        try! JSONSerialization.data(withJSONObject: value, options: [.sortedKeys])
    }

    private static func expectError(
        _ expected: MordeHubRelayError,
        operation: () throws -> Void,
        file: StaticString = #file,
        line: UInt = #line
    ) throws {
        do {
            try operation()
            preconditionFailure("expected \(expected)", file: file, line: line)
        } catch let error as MordeHubRelayError {
            precondition(error == expected, "expected \(expected), got \(error)", file: file, line: line)
        }
    }
}
