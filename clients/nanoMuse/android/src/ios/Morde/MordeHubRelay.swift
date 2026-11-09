import CoreFoundation
import Foundation

enum MordeHubRelayError: Error, Equatable {
    case invalidPortalServer
    case invalidCredential
    case invalidTarget
    case invalidURL
    case targetIsCurrentDevice
    case targetOffline
    case timeout
    case malformedFrame
    case unsupportedFrame
    case callRejected(String)
}

enum MordeHubRelayPolicy {
    private static let maxFrameBytes = 65_536
    private static let callIDPattern = #"^[A-Za-z0-9._:-]{4,80}$"#

    static func endpoint(portalServer: URL) throws -> (url: URL, origin: String) {
        guard var components = URLComponents(url: portalServer, resolvingAgainstBaseURL: false),
              components.scheme?.lowercased() == "https",
              let host = components.host, !host.isEmpty,
              components.user == nil, components.password == nil,
              validPort(components.port),
              ["", "/"].contains(components.percentEncodedPath),
              components.query == nil, components.fragment == nil else {
            throw MordeHubRelayError.invalidPortalServer
        }

        var originComponents = components
        originComponents.path = ""
        if originComponents.port == 443 { originComponents.port = nil }
        guard let origin = originComponents.string else { throw MordeHubRelayError.invalidPortalServer }

        components.scheme = "wss"
        components.path = "/api/hub"
        if components.port == 443 { components.port = nil }
        guard let url = components.url else { throw MordeHubRelayError.invalidPortalServer }
        return (url, origin)
    }

    static func validateCredential(_ credential: String) throws {
        guard credential.count == 56,
              credential.range(of: #"^chc_t[a-z0-9]{7}_[A-Za-z0-9_-]{43}$"#,
                              options: .regularExpression) != nil else {
            throw MordeHubRelayError.invalidCredential
        }
    }

    static func validateTarget(_ target: String) throws {
        guard target.range(of: #"^i[a-z0-9]{7}$"#, options: .regularExpression) != nil else {
            throw MordeHubRelayError.invalidTarget
        }
    }

    @discardableResult
    static func validateOpenURL(_ url: URL) throws -> String {
        guard let components = URLComponents(url: url, resolvingAgainstBaseURL: false),
              let scheme = components.scheme?.lowercased(), ["http", "https"].contains(scheme),
              let host = components.host, !host.isEmpty,
              components.user == nil, components.password == nil,
              validPort(components.port),
              url.absoluteString.utf8.count <= 2_048 else {
            throw MordeHubRelayError.invalidURL
        }
        return url.absoluteString
    }

    static func makeOpenCall(url: String, target: String, id: String) throws -> Data {
        try validateTarget(target)
        guard id.range(of: callIDPattern, options: .regularExpression) != nil,
              id.utf8.count <= 80,
              let openURL = URL(string: url) else { throw MordeHubRelayError.invalidURL }
        let validatedURL = try validateOpenURL(openURL)
        let frame: [String: Any] = [
            "type": "call", "id": id, "to": target, "action": "open", "args": ["url": validatedURL],
        ]
        return try JSONSerialization.data(withJSONObject: frame, options: [.sortedKeys])
    }

    static func validateWelcome(_ data: Data, target: String) throws -> String {
        try validateTarget(target)
        let frame = try object(data)
        guard keys(frame) == Set(["type", "device_id", "devices", "server"]),
              frame["type"] as? String == "welcome",
              let currentID = frame["device_id"] as? String,
              isValidTarget(currentID),
              let devices = frame["devices"] as? [Any], devices.count <= 256,
              let server = frame["server"] as? [String: Any],
              keys(server) == Set(["frame_limit", "time"]),
              integer(server["frame_limit"]) == Int64(maxFrameBytes),
              let serverTime = integer(server["time"]), serverTime >= 0 else {
            throw MordeHubRelayError.malformedFrame
        }
        if target == currentID { throw MordeHubRelayError.targetIsCurrentDevice }
        try validateDevices(devices, target: target)
        return currentID
    }

    static func validatePendingFrame(_ data: Data, target: String, callID: String) throws -> Bool {
        try validateTarget(target)
        guard callID.range(of: callIDPattern, options: .regularExpression) != nil else {
            throw MordeHubRelayError.malformedFrame
        }
        let frame = try object(data)
        guard let type = frame["type"] as? String else { throw MordeHubRelayError.malformedFrame }
        switch type {
        case "devices":
            guard keys(frame) == Set(["type", "devices"]), let devices = frame["devices"] as? [Any] else {
                throw MordeHubRelayError.malformedFrame
            }
            try validateDevices(devices, target: target)
            return false
        case "error":
            guard keys(frame).isSubset(of: Set(["type", "id", "code", "message"])),
                  let code = frame["code"] as? String, !code.isEmpty, code.utf8.count <= 64,
                  let message = frame["message"] as? String, message.utf8.count <= 200 else {
                throw MordeHubRelayError.malformedFrame
            }
            if let rawID = frame["id"] {
                guard let id = rawID as? String, id == callID else { throw MordeHubRelayError.unsupportedFrame }
            }
            throw MordeHubRelayError.callRejected(code)
        case "result":
            guard keys(frame).isSubset(of: Set(["type", "id", "ok", "body", "error", "message"])),
                  let id = frame["id"] as? String,
                  let ok = boolean(frame["ok"]) else {
                throw MordeHubRelayError.malformedFrame
            }
            guard id == callID else { throw MordeHubRelayError.unsupportedFrame }
            if let rawMessage = frame["message"] {
                guard let message = rawMessage as? String, message.utf8.count <= 200 else {
                    throw MordeHubRelayError.malformedFrame
                }
            }
            guard ok else {
                guard let error = frame["error"] as? String, !error.isEmpty, error.utf8.count <= 64,
                      frame["body"] == nil else { throw MordeHubRelayError.malformedFrame }
                throw MordeHubRelayError.callRejected(error)
            }
            guard frame["error"] == nil, frame["message"] == nil,
                  let body = frame["body"] as? [String: Any],
                  keys(body) == Set(["opened"]), boolean(body["opened"]) == true else {
                throw MordeHubRelayError.malformedFrame
            }
            return true
        default:
            throw MordeHubRelayError.unsupportedFrame
        }
    }

    private static func validateDevices(_ devices: [Any], target: String) throws {
        guard devices.count <= 256 else { throw MordeHubRelayError.malformedFrame }
        var identifiers = Set<String>()
        var targetOnline = false
        for value in devices {
            guard let device = value as? [String: Any],
                  keys(device) == Set(["id", "name", "platform", "online"]),
                  let id = device["id"] as? String, isValidTarget(id), identifiers.insert(id).inserted,
                  let name = device["name"] as? String, !name.isEmpty, name.utf8.count <= 100,
                  let platform = device["platform"] as? String, ["desktop", "android", "ios"].contains(platform),
                  let online = boolean(device["online"]) else {
                throw MordeHubRelayError.malformedFrame
            }
            if id == target { targetOnline = online }
        }
        guard targetOnline else { throw MordeHubRelayError.targetOffline }
    }

    private static func object(_ data: Data) throws -> [String: Any] {
        guard !data.isEmpty, data.count <= maxFrameBytes,
              let value = try? JSONSerialization.jsonObject(with: data),
              let object = value as? [String: Any] else { throw MordeHubRelayError.malformedFrame }
        return object
    }

    private static func keys(_ object: [String: Any]) -> Set<String> { Set(object.keys) }

    private static func isValidTarget(_ value: String) -> Bool {
        value.range(of: #"^i[a-z0-9]{7}$"#, options: .regularExpression) != nil
    }

    private static func validPort(_ value: Int?) -> Bool {
        value.map { (1...65_535).contains($0) } ?? true
    }

    private static func boolean(_ value: Any?) -> Bool? {
        guard let value = value as? NSNumber, CFGetTypeID(value) == CFBooleanGetTypeID() else { return nil }
        return value.boolValue
    }

    private static func integer(_ value: Any?) -> Int64? {
        guard let value = value as? NSNumber, CFGetTypeID(value) != CFBooleanGetTypeID() else { return nil }
        let doubleValue = value.doubleValue
        guard doubleValue.isFinite, doubleValue.rounded(.towardZero) == doubleValue,
              doubleValue >= Double(Int64.min), doubleValue <= Double(Int64.max) else { return nil }
        return value.int64Value
    }
}

enum MordeHubRelay {
    static func open(url: URL, on target: String, portalServer: URL, credential: String) async throws {
        let openURL = try MordeHubRelayPolicy.validateOpenURL(url)
        try MordeHubRelayPolicy.validateTarget(target)
        try MordeHubRelayPolicy.validateCredential(credential)
        let endpoint = try MordeHubRelayPolicy.endpoint(portalServer: portalServer)

        var request = URLRequest(url: endpoint.url)
        request.httpShouldHandleCookies = false
        request.cachePolicy = .reloadIgnoringLocalCacheData
        request.setValue("Bearer \(credential)", forHTTPHeaderField: "Authorization")
        request.setValue(endpoint.origin, forHTTPHeaderField: "Origin")

        let session = URLSession(configuration: .ephemeral)
        let socket = session.webSocketTask(with: request)
        defer {
            socket.cancel(with: .goingAway, reason: nil)
            session.invalidateAndCancel()
        }
        socket.resume()
        try await socket.send(.string(#"{"type":"hello"}"#))
        let welcome = try await receiveText(from: socket, timeout: 15)
        _ = try MordeHubRelayPolicy.validateWelcome(welcome, target: target)

        let callID = UUID().uuidString.lowercased()
        let call = try MordeHubRelayPolicy.makeOpenCall(url: openURL, target: target, id: callID)
        guard let callText = String(data: call, encoding: .utf8) else { throw MordeHubRelayError.malformedFrame }
        try await socket.send(.string(callText))

        try await awaitOpenConfirmation(target: target, callID: callID) {
            try await receiveText(from: socket, timeout: 30)
        }
    }

    static func awaitOpenConfirmation(target: String, callID: String,
                                      receive: () async throws -> Data) async throws {
        while true {
            let result = try await receive()
            if try MordeHubRelayPolicy.validatePendingFrame(result, target: target, callID: callID) { return }
        }
    }

    private static func receiveText(from socket: URLSessionWebSocketTask, timeout: UInt64) async throws -> Data {
        let message = try await withThrowingTaskGroup(of: URLSessionWebSocketTask.Message.self) { group in
            group.addTask { try await socket.receive() }
            group.addTask {
                try await Task.sleep(nanoseconds: timeout * 1_000_000_000)
                throw MordeHubRelayError.timeout
            }
            do {
                guard let message = try await group.next() else { throw MordeHubRelayError.malformedFrame }
                group.cancelAll()
                return message
            } catch {
                socket.cancel(with: .goingAway, reason: nil)
                group.cancelAll()
                throw error
            }
        }
        guard case .string(let text) = message else { throw MordeHubRelayError.malformedFrame }
        let data = Data(text.utf8)
        guard data.count <= 65_536 else { throw MordeHubRelayError.malformedFrame }
        return data
    }
}
