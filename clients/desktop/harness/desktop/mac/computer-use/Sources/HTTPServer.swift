// A loopback HTTP/1.1 server on Network.framework, just big enough for the five routes:
// one request per connection (`Connection: close`), JSON in and out, bodies up to 8 MB.
// Everything — parsing, the handler, the answer — runs on one serial queue, so actions
// never interleave (the operator in the shell serialises them too).

import Foundation
import Network

struct HTTPRequest {
    let method: String
    let path: String
    let headers: [String: String]
    let body: Data
}

struct HTTPResponse {
    let status: Int
    let body: [String: Any]
}

final class HTTPServer {
    private let listener: NWListener
    private let queue: DispatchQueue
    private let handler: (HTTPRequest) -> HTTPResponse
    /// The port, once bound.
    var onReady: ((UInt16) -> Void)?
    /// A listener that failed or was cancelled; the process should end.
    var onFailure: ((String) -> Void)?

    init(queue: DispatchQueue, handler: @escaping (HTTPRequest) -> HTTPResponse) throws {
        let parameters = NWParameters.tcp
        // loopback only, any free port
        parameters.requiredLocalEndpoint = NWEndpoint.hostPort(host: .ipv4(.loopback), port: .any)
        listener = try NWListener(using: parameters)
        self.queue = queue
        self.handler = handler
    }

    func start() {
        listener.stateUpdateHandler = { [weak self] state in
            guard let self = self else { return }
            switch state {
            case .ready:
                if let port = self.listener.port {
                    self.onReady?(port.rawValue)
                } else {
                    self.onFailure?("the listener is ready but has no port")
                }
            case .failed(let error):
                self.onFailure?("the listener failed: \(error)")
            case .cancelled:
                self.onFailure?("the listener was cancelled")
            default:
                break
            }
        }
        listener.newConnectionHandler = { [weak self] connection in
            guard let self = self else { return }
            HTTPConnection(connection, queue: self.queue, handler: self.handler).start()
        }
        listener.start(queue: queue)
    }
}

/// One client connection: read a request, answer it, close.
private final class HTTPConnection {
    private static let headerLimit = 64 * 1024
    private static let bodyLimit = 8 * 1024 * 1024

    private let connection: NWConnection
    private let queue: DispatchQueue
    private let handler: (HTTPRequest) -> HTTPResponse
    private var buffer = Data()
    /// Keeps this object alive until the answer is on the wire (nothing else holds it).
    private var retained: HTTPConnection?

    init(_ connection: NWConnection, queue: DispatchQueue, handler: @escaping (HTTPRequest) -> HTTPResponse) {
        self.connection = connection
        self.queue = queue
        self.handler = handler
    }

    func start() {
        retained = self
        connection.stateUpdateHandler = { [weak self] state in
            switch state {
            case .failed, .cancelled:
                self?.finish()
            default:
                break
            }
        }
        connection.start(queue: queue)
        receive()
    }

    private func receive() {
        connection.receive(minimumIncompleteLength: 1, maximumLength: 64 * 1024) { [weak self] data, _, isComplete, error in
            guard let self = self else { return }
            if let data = data {
                self.buffer.append(data)
            }
            if error != nil {
                self.finish()
                return
            }
            if self.respondIfComplete() {
                return
            }
            if isComplete {
                self.finish()
                return
            }
            self.receive()
        }
    }

    /// True once a whole request was read and the answer sent (or refused).
    private func respondIfComplete() -> Bool {
        guard let headerEnd = buffer.range(of: Data("\r\n\r\n".utf8)) else {
            if buffer.count > HTTPConnection.headerLimit {
                respond(HTTPResponse(status: 431, body: ["error": "bad_request", "message": "the request headers are too large"]))
                return true
            }
            return false
        }
        guard let head = String(data: buffer.subdata(in: buffer.startIndex..<headerEnd.lowerBound), encoding: .utf8) else {
            respond(HTTPResponse(status: 400, body: ["error": "bad_request", "message": "the request head is not UTF-8"]))
            return true
        }
        let lines = head.components(separatedBy: "\r\n")
        let requestLine = (lines.first ?? "").split(separator: " ")
        guard requestLine.count >= 2 else {
            respond(HTTPResponse(status: 400, body: ["error": "bad_request", "message": "no request line"]))
            return true
        }
        var headers: [String: String] = [:]
        for line in lines.dropFirst() {
            let pair = line.split(separator: ":", maxSplits: 1)
            guard pair.count == 2 else { continue }
            headers[pair[0].trimmingCharacters(in: .whitespaces).lowercased()] = pair[1].trimmingCharacters(in: .whitespaces)
        }
        let contentLength = Int(headers["content-length"] ?? "0") ?? 0
        if contentLength > HTTPConnection.bodyLimit {
            respond(HTTPResponse(status: 413, body: ["error": "too_large", "message": "the request body is over 8 MB"]))
            return true
        }
        let bodyStart = headerEnd.upperBound
        guard buffer.endIndex - bodyStart >= contentLength else { return false }
        let body = buffer.subdata(in: bodyStart..<(bodyStart + contentLength))
        let path = String(requestLine[1]).components(separatedBy: "?")[0]
        let request = HTTPRequest(method: String(requestLine[0]), path: path, headers: headers, body: body)
        respond(handler(request))
        return true
    }

    private func respond(_ response: HTTPResponse) {
        let payload = (try? JSONSerialization.data(withJSONObject: response.body, options: [])) ?? Data("{}".utf8)
        var head = "HTTP/1.1 \(response.status) \(HTTPConnection.reason(response.status))\r\n"
        head += "Content-Type: application/json; charset=utf-8\r\n"
        head += "Content-Length: \(payload.count)\r\n"
        head += "Cache-Control: no-store\r\n"
        head += "Connection: close\r\n\r\n"
        var data = Data(head.utf8)
        data.append(payload)
        connection.send(content: data, completion: .contentProcessed { [weak self] _ in
            self?.finish()
        })
    }

    private func finish() {
        connection.cancel()
        retained = nil
    }

    private static func reason(_ status: Int) -> String {
        switch status {
        case 200: return "OK"
        case 400: return "Bad Request"
        case 401: return "Unauthorized"
        case 403: return "Forbidden"
        case 404: return "Not Found"
        case 413: return "Payload Too Large"
        case 431: return "Request Header Fields Too Large"
        case 500: return "Internal Server Error"
        default: return "Error"
        }
    }
}
