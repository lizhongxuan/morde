import Foundation

indirect enum MordeCodexJSONValue: Codable, Equatable, Sendable {
    case object([String: MordeCodexJSONValue])
    case array([MordeCodexJSONValue])
    case string(String)
    case integer(Int64)
    case number(Double)
    case bool(Bool)
    case null

    init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        if container.decodeNil() { self = .null }
        else if let value = try? container.decode(Bool.self) { self = .bool(value) }
        else if let value = try? container.decode(Int64.self) { self = .integer(value) }
        else if let value = try? container.decode(Double.self) { self = .number(value) }
        else if let value = try? container.decode(String.self) { self = .string(value) }
        else if let value = try? container.decode([String: Self].self) { self = .object(value) }
        else if let value = try? container.decode([Self].self) { self = .array(value) }
        else { throw DecodingError.dataCorruptedError(in: container, debugDescription: "Unsupported JSON value") }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.singleValueContainer()
        switch self {
        case .object(let value): try container.encode(value)
        case .array(let value): try container.encode(value)
        case .string(let value): try container.encode(value)
        case .integer(let value): try container.encode(value)
        case .number(let value): try container.encode(value)
        case .bool(let value): try container.encode(value)
        case .null: try container.encodeNil()
        }
    }

    var stringValue: String? {
        switch self {
        case .string(let value): value
        case .integer(let value): String(value)
        case .number(let value): String(value)
        default: nil
        }
    }

    var objectValue: [String: MordeCodexJSONValue]? {
        guard case .object(let value) = self else { return nil }
        return value
    }

    var arrayValue: [MordeCodexJSONValue]? {
        guard case .array(let value) = self else { return nil }
        return value
    }

    var boolValue: Bool? {
        guard case .bool(let value) = self else { return nil }
        return value
    }

    var foundationValue: Any {
        switch self {
        case .object(let value): value.mapValues(\.foundationValue)
        case .array(let value): value.map(\.foundationValue)
        case .string(let value): value
        case .integer(let value): value
        case .number(let value): value
        case .bool(let value): value
        case .null: NSNull()
        }
    }
}

struct MordeCodexSession: Decodable, Equatable, Identifiable, Sendable {
    let id: String
    let installation_id: String
    let profile_key: String?
    let thread_id: String
    let title: String
    let cwd_label: String
    let updated_at: String?
    let status: String
    let scopes: [String]
    let current_sequence: Int64
    let turn_id: String?

    private enum CodingKeys: String, CodingKey {
        case id, session_id, installation_id, profile_key, thread_id, title, cwd_label, updated_at, status, scopes
        case last_sequence, turn_id, current_turn_id
    }

    init(from decoder: Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        id = try values.decodeIfPresent(String.self, forKey: .id)
            ?? values.decode(String.self, forKey: .session_id)
        installation_id = try values.decode(String.self, forKey: .installation_id)
        profile_key = try values.decodeIfPresent(String.self, forKey: .profile_key)
        thread_id = try values.decode(String.self, forKey: .thread_id)
        title = try values.decode(String.self, forKey: .title)
        cwd_label = try values.decode(String.self, forKey: .cwd_label)
        updated_at = try values.decodeIfPresent(String.self, forKey: .updated_at)
        status = try values.decode(String.self, forKey: .status)
        scopes = try values.decode([String].self, forKey: .scopes)
        current_sequence = try values.decode(Int64.self, forKey: .last_sequence)
        turn_id = try values.decodeIfPresent(String.self, forKey: .current_turn_id)
            ?? values.decodeIfPresent(String.self, forKey: .turn_id)
    }

    var shortThreadID: String { String(thread_id.suffix(8)) }

    var updatedAtDate: Date? {
        guard let updated_at else { return nil }
        let fractional = ISO8601DateFormatter()
        fractional.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return fractional.date(from: updated_at) ?? ISO8601DateFormatter().date(from: updated_at)
    }

    func hasScope(_ scope: String) -> Bool { scopes.contains(scope) }
}

struct MordeCodexSessionDiscoveryStatus: Decodable, Equatable, Sendable {
    let state: String
    let error_code: String?
    let updated_at: String

    var updatedAtDate: Date? {
        let fractional = ISO8601DateFormatter()
        fractional.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return fractional.date(from: updated_at) ?? ISO8601DateFormatter().date(from: updated_at)
    }
}

struct MordeCodexHistorySyncProgress: Decodable, Equatable, Sendable {
    let discovered_sessions: Int64
    let history_complete_sessions: Int64
    let pending_upload_event_count: Int64
}

struct MordeCodexProfileConnection: Decodable, Equatable, Identifiable, Sendable {
    let installation_id: String
    let profile_key: String
    let scopes: [String]
    let grant_version: Int64
    let connected: Bool
    let updated_at: String
    let session_discovery: MordeCodexSessionDiscoveryStatus?
    let history_sync: MordeCodexHistorySyncProgress?

    var id: String { profile_key }

    var updatedAtDate: Date? {
        let fractional = ISO8601DateFormatter()
        fractional.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return fractional.date(from: updated_at) ?? ISO8601DateFormatter().date(from: updated_at)
    }
}

struct MordeCodexProfileConnectionList: Decodable, Sendable {
    let items: [MordeCodexProfileConnection]
}

enum MordeCodexProfileConnectionPolicy {
    static func isValid(_ connection: MordeCodexProfileConnection,
                        installationID: String) -> Bool {
        let allowedScopes: Set<String> = ["read", "live", "send", "interrupt", "approval"]
        let profileKeyIsSafe = !connection.profile_key.isEmpty && connection.profile_key.utf8.count <= 128 &&
            connection.profile_key.unicodeScalars.allSatisfy { scalar in
                (scalar.value >= 48 && scalar.value <= 57) ||
                    (scalar.value >= 65 && scalar.value <= 90) ||
                    (scalar.value >= 97 && scalar.value <= 122) || scalar.value == 95 || scalar.value == 45
            } && !connection.profile_key.uppercased().contains("CODEX_HOME")
        guard connection.installation_id == installationID, profileKeyIsSafe,
              connection.connected, connection.grant_version > 0,
              connection.scopes.count >= 2,
              Set(connection.scopes).count == connection.scopes.count,
              Set(connection.scopes).isSubset(of: allowedScopes),
              connection.scopes.contains("read"), connection.scopes.contains("live"),
              connection.updatedAtDate != nil,
              (connection.session_discovery == nil) == (connection.history_sync == nil) else { return false }
        if let status = connection.session_discovery {
            guard ["syncing", "synced", "partial"].contains(status.state),
                  status.updatedAtDate != nil,
                  status.error_code.map({
                      ["profile_unavailable", "session_list_failed", "connection_lost", "unknown"].contains($0)
                  }) ?? true,
                  (status.state == "partial") == (status.error_code != nil) else { return false }
        }
        if let progress = connection.history_sync {
            guard progress.discovered_sessions >= 0,
                  progress.history_complete_sessions >= 0,
                  progress.history_complete_sessions <= progress.discovered_sessions,
                  progress.pending_upload_event_count >= 0 else { return false }
        }
        return true
    }
}

enum MordeCodexSessionScopePolicy {
    static func profileKeys(in sessions: [MordeCodexSession]) -> [String] {
        Array(Set(sessions.compactMap(\.profile_key))).sorted()
    }

    static func sessions(_ sessions: [MordeCodexSession], profileKey: String?) -> [MordeCodexSession] {
        guard let profileKey else { return sessions }
        return sessions.filter { $0.profile_key == profileKey }
    }

    static func acceptsNextCursor(_ next: String?, after current: String?, seen: Set<String>) -> Bool {
        guard let next else { return true }
        return !next.isEmpty && next != current && !seen.contains(next)
    }

    static func sessionsMissingFromCompletedRefresh(previous: Set<String>, observed: Set<String>,
                                                     finished: Bool) -> Set<String> {
        finished ? previous.subtracting(observed) : []
    }

    static func shouldClearReadHistory(afterRefreshing scopes: [String]?) -> Bool {
        guard let scopes else { return false }
        return !scopes.contains("read")
    }

    static func permitsReadResponse(_ session: MordeCodexSession?, selectedSessionID: String?,
                                    installationAuthorized: Bool, revoked: Bool,
                                    requireLive: Bool = false) -> Bool {
        guard let session, installationAuthorized, !revoked, session.id == selectedSessionID,
              session.hasScope("read") else { return false }
        return !requireLive || session.hasScope("live")
    }

    static func permitsCommandResponse(_ session: MordeCodexSession?,
                                       installationAuthorized: Bool, revoked: Bool,
                                       requiredScope: String) -> Bool {
        guard let session, installationAuthorized, !revoked,
              session.hasScope("read"), !requiredScope.isEmpty else { return false }
        return session.hasScope(requiredScope)
    }
}

struct MordeCodexEvent: Decodable, Equatable, Identifiable, Sendable {
    let session_id: String
    let sequence: Int64
    let provider_event_id: String
    let kind: String
    let occurred_at: String
    let turn_id: String?
    let item_id: String?
    let parent_item_id: String?
    let status: String?
    let payload: [String: MordeCodexJSONValue]
    let stored_at: String

    var id: Int64 { sequence }
    var values: [String: MordeCodexJSONValue] { payload }
    var visibleStatus: String? { status ?? values["status"]?.stringValue }
    var diagnostic: String? {
        guard normalizedKind == "unsupported",
              let diagnostic = values["diagnostic"]?.stringValue,
              !diagnostic.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty,
              diagnostic.utf8.count <= 24 * 1024,
              !MordeCodexEventPolicy.isRawJSON(diagnostic) else { return nil }
        return diagnostic
    }
    var text: String? {
        func first(_ keys: [String]) -> String? {
            keys.lazy.compactMap { values[$0]?.stringValue }
                .first { !$0.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty }
        }
        func joinedItems() -> String? {
            guard case .array(let items)? = values["items"] else { return nil }
            let text = items.compactMap(\.stringValue).joined(separator: "\n")
            return text.isEmpty ? nil : text
        }
        func fileText() -> String? {
            let text = [values["path_label"]?.stringValue, values["change"]?.stringValue]
                .compactMap { $0 }.joined(separator: " · ")
            return text.isEmpty ? nil : text
        }
        return switch normalizedKind {
        case "user_message", "assistant_message", "assistant_delta": first(["text", "summary"])
        case "reasoning_summary", "plan": joinedItems() ?? first(["summary", "text"])
        case "turn_started", "turn_completed", "turn_failed", "turn_cancelled": first(["summary", "text"])
        case "tool_call": first(["summary", "text", "tool"])
        case "tool_result": first(["summary", "text"]) ?? safePlainOutput
        case "file_change": first(["summary", "text"]) ?? fileText()
        case "child_agent": first(["summary", "text", "agent_label"])
        case "command_status": first(["type"])
        case "approval_request", "approval_status": first(["summary", "text"])
        default: nil
        }
    }

    private var safePlainOutput: String? {
        guard let output = values["output"]?.stringValue,
              !output.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty,
              let data = output.data(using: .utf8),
              (try? JSONSerialization.jsonObject(with: data, options: [.fragmentsAllowed])) == nil else { return nil }
        return output
    }

    var normalizedKind: String { kind.lowercased().replacingOccurrences(of: ".", with: "_") }
    var isSessionStatus: Bool {
        ["command_status", "turn_status", "turn_started", "turn_completed", "turn_failed",
         "turn_interrupted", "turn_cancelled", "status"].contains(normalizedKind)
    }
    var isAssistantDelta: Bool { ["assistant_delta"].contains(normalizedKind) }
    var isAssistantMessage: Bool {
        ["assistant_message", "assistant_final"].contains(normalizedKind)
    }
    var isUserMessage: Bool { ["user_message"].contains(normalizedKind) }
    var isApproval: Bool {
        ["approval", "approval_request", "approval_requested", "approval_required",
         "approval_status", "approval_update", "approval_pending", "approval_resolved"].contains(normalizedKind)
    }

    var approval: MordeCodexApproval? {
        guard isApproval else { return nil }
        func value(_ keys: [String]) -> String? {
            keys.lazy.compactMap { values[$0]?.stringValue }.first
        }
        guard let id = value(["approval_id", "id"]), !id.isEmpty,
              let version = value(["approval_version", "version"]), !version.isEmpty else { return nil }
        return MordeCodexApproval(
            id: id,
            version: version,
            title: value(["title", "summary"]) ?? "",
            object: value(["object", "path_label"]),
            detail: value(["detail", "content", "description", "text"]),
            impact: value(["impact"]),
            status: value(["approval_status", "status"]) ?? visibleStatus ??
                (normalizedKind == "approval_request" ? "pending" : "unknown"),
            expiresAt: value(["expires_at"]))
    }

    var questionRequest: MordeCodexUserQuestionRequest? {
        guard normalizedKind == "question",
              let requestID = values["question_request_id"]?.stringValue, !requestID.isEmpty,
              let version = values["question_version"]?.stringValue,
              version.utf8.count == 64,
              version.utf8.allSatisfy({ ($0 >= 48 && $0 <= 57) || ($0 >= 97 && $0 <= 102) }),
              let status = values["status"]?.stringValue,
              ["pending", "answered", "stale"].contains(status) else { return nil }
        let questionValues = values["questions"]?.arrayValue ?? []
        let questions = questionValues.compactMap { MordeCodexUserInputQuestion(value: $0) }
        guard questions.count == questionValues.count,
              Set(questions.map(\.id)).count == questions.count else { return nil }
        guard status != "pending" || !questions.isEmpty else { return nil }
        return MordeCodexUserQuestionRequest(id: requestID, version: version, status: status, questions: questions)
    }

    var replyIdentity: MordeCodexReplyIdentity? {
        if let approval {
            return MordeCodexReplyIdentity(sessionID: session_id, kind: .approval,
                                           requestID: approval.id, version: approval.version)
        }
        if let question = questionRequest {
            return MordeCodexReplyIdentity(sessionID: session_id, kind: .question,
                                           requestID: question.id, version: question.version)
        }
        return nil
    }

    var codexToolCallID: String? {
        guard ["tool_call", "tool_result", "tool_output_delta", "tool_call_summary", "tool_result_summary"]
                .contains(normalizedKind),
              let callID = values["call_id"]?.stringValue,
              !callID.isEmpty, callID.utf8.count <= 512,
              callID == callID.trimmingCharacters(in: .whitespacesAndNewlines),
              !callID.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) else { return nil }
        return callID
    }
}

struct MordeCodexApproval: Equatable, Sendable {
    let id: String
    let version: String
    let title: String
    let object: String?
    let detail: String?
    let impact: String?
    let status: String
    let expiresAt: String?

    func expirationDate() -> Date? {
        guard let expiresAt else { return nil }
        let fractional = ISO8601DateFormatter()
        fractional.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return fractional.date(from: expiresAt) ?? ISO8601DateFormatter().date(from: expiresAt)
    }

    func canRespond(at now: Date = Date()) -> Bool {
        guard status.lowercased() == "pending" else { return false }
        guard expiresAt != nil else { return true }
        guard let expiration = expirationDate() else { return false }
        return expiration > now
    }

    var isNoLongerPending: Bool {
        ["expired", "stale", "approved", "rejected", "resolved", "cancelled", "canceled", "completed"]
            .contains(status.lowercased())
    }
}

struct MordeCodexUserInputOption: Equatable, Sendable {
    let label: String
    let description: String
}

struct MordeCodexUserInputQuestion: Equatable, Sendable, Identifiable {
    let id: String
    let header: String
    let question: String
    let options: [MordeCodexUserInputOption]
    let isSecret: Bool
    let isOther: Bool

    fileprivate init?(value: MordeCodexJSONValue) {
        guard let row = value.objectValue,
              let id = row["id"]?.stringValue, !id.isEmpty,
              let header = row["header"]?.stringValue,
              let question = row["question"]?.stringValue else { return nil }
        let optionValues = row["options"]?.arrayValue ?? []
        var options: [MordeCodexUserInputOption] = []
        for value in optionValues {
            guard let option = value.objectValue, let label = option["label"]?.stringValue else { return nil }
            options.append(MordeCodexUserInputOption(label: label, description: option["description"]?.stringValue ?? ""))
        }
        self.id = id
        self.header = header
        self.question = question
        self.options = options
        isSecret = row["is_secret"]?.boolValue ?? false
        isOther = row["is_other"]?.boolValue ?? false
    }
}

struct MordeCodexUserQuestionRequest: Equatable, Sendable {
    let id: String
    let version: String
    let status: String
    let questions: [MordeCodexUserInputQuestion]

    func withStatus(_ status: String) -> Self {
        Self(id: id, version: version, status: status, questions: questions)
    }

    func accepts(_ answers: [String: [String]]) -> Bool {
        guard status == "pending", !questions.isEmpty,
              Set(answers.keys) == Set(questions.map(\.id)) else { return false }
        return questions.allSatisfy { question in
            guard let values = answers[question.id], !values.isEmpty, values.count <= 64 else { return false }
            let controls = CharacterSet.controlCharacters
            let allowedControls = CharacterSet(charactersIn: "\n\r\t")
            return values.allSatisfy { value in
                !value.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && value.utf8.count <= 4096 &&
                    value.unicodeScalars.allSatisfy { !controls.contains($0) || allowedControls.contains($0) }
            }
        }
    }
}

enum MordeCodexReplyKind: Hashable, Sendable {
    case approval
    case question
}

struct MordeCodexReplyIdentity: Hashable, Sendable {
    let sessionID: String
    let kind: MordeCodexReplyKind
    let requestID: String
    let version: String
}

enum MordeCodexReplyPolicy {
    static func isTerminalCommandStatus(_ status: String) -> Bool {
        ["completed", "failed", "cancelled", "canceled", "interrupted", "rejected"]
            .contains(status.lowercased())
    }

    static func identity(sessionID: String, approval: MordeCodexApproval) -> MordeCodexReplyIdentity {
        MordeCodexReplyIdentity(sessionID: sessionID, kind: .approval,
                                requestID: approval.id, version: approval.version)
    }

    static func identity(sessionID: String, question: MordeCodexUserQuestionRequest) -> MordeCodexReplyIdentity {
        MordeCodexReplyIdentity(sessionID: sessionID, kind: .question,
                                requestID: question.id, version: question.version)
    }

    static func terminalReplyIdentity(for event: MordeCodexEvent) -> MordeCodexReplyIdentity? {
        guard let identity = event.replyIdentity else { return nil }
        if let approval = event.approval, approval.isNoLongerPending { return identity }
        if let question = event.questionRequest, ["answered", "stale"].contains(question.status) { return identity }
        return nil
    }

    static func terminalCommandID(for event: MordeCodexEvent) -> String? {
        guard event.normalizedKind == "command_status",
              let status = event.visibleStatus,
              isTerminalCommandStatus(status),
              let commandID = event.values["command_id"]?.stringValue, !commandID.isEmpty else { return nil }
        return commandID
    }

    static func isTerminal(_ event: MordeCodexEvent, for identity: MordeCodexReplyIdentity) -> Bool {
        terminalReplyIdentity(for: event) == identity
    }
}

struct MordeCodexEventPage: Decodable, Sendable {
    let items: [MordeCodexEvent]
    let current_sequence: Int64
    let next_cursor: String?
}

struct MordeCodexCommandReceipt: Decodable, Sendable {
    let command_id: String
    let session_id: String
    let type: String
    let status: String
    let current_sequence: Int64?

    private enum CodingKeys: String, CodingKey {
        case command_id, id, session_id, type, status, accepted_status, current_sequence, event_sequence
    }

    init(from decoder: Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        command_id = try values.decodeIfPresent(String.self, forKey: .command_id)
            ?? values.decodeIfPresent(String.self, forKey: .id) ?? ""
        session_id = try values.decode(String.self, forKey: .session_id)
        type = try values.decode(String.self, forKey: .type)
        status = try values.decodeIfPresent(String.self, forKey: .status)
            ?? values.decodeIfPresent(String.self, forKey: .accepted_status) ?? "unknown"
        current_sequence = try values.decodeIfPresent(Int64.self, forKey: .current_sequence)
            ?? values.decodeIfPresent(Int64.self, forKey: .event_sequence)
    }
}

enum MordeCodexEventCursorResult: Equatable {
    case accepted
    case duplicate
    case gap(expected: Int64, received: Int64)
    case invalid
}

struct MordeCodexEventCursor: Equatable, Sendable {
    private(set) var sequence: Int64

    init(afterSequence: Int64 = 0) { sequence = max(0, afterSequence) }

    mutating func accept(_ event: MordeCodexEvent, sessionID: String) -> MordeCodexEventCursorResult {
        guard event.session_id == sessionID, event.sequence > 0,
              !event.provider_event_id.isEmpty, event.provider_event_id.utf8.count <= 512,
              !event.kind.isEmpty else { return .invalid }
        guard event.sequence > sequence else { return .duplicate }
        let expected = sequence == Int64.max ? Int64.max : sequence + 1
        guard event.sequence == expected else { return .gap(expected: expected, received: event.sequence) }
        sequence = event.sequence
        return .accepted
    }
}

private struct MordeCodexTextItemKey: Hashable {
    let kind: MordeCodexTimelineKind
    let turnID: String?
    let itemID: String
}

private struct MordeCodexToolCallKey: Hashable {
    let callID: String
}

enum MordeCodexEventPolicy {
    enum MergeError: Error, Equatable {
        case invalidEvent
        case conflictingDuplicate
        case sequenceGap(expected: Int64, received: Int64)
    }

    static func isValidSessionID(_ id: String) -> Bool {
        id.utf8.count == 8 && id.utf8.first == 108 &&
            id.utf8.dropFirst().allSatisfy { ($0 >= 48 && $0 <= 57) || ($0 >= 97 && $0 <= 122) }
    }

    static func isValidCommandID(_ id: String) -> Bool {
        id.utf8.count == 8 && id.utf8.first == 103 &&
            id.utf8.dropFirst().allSatisfy { ($0 >= 48 && $0 <= 57) || ($0 >= 97 && $0 <= 122) }
    }

    static func isRawJSON(_ text: String?) -> Bool {
        guard let text, let data = text.data(using: .utf8) else { return false }
        return (try? JSONSerialization.jsonObject(with: data, options: [.fragmentsAllowed])) != nil
    }

    static func merge(_ current: [MordeCodexEvent], incoming: [MordeCodexEvent],
                      sessionID: String, afterSequence: Int64? = nil) throws -> [MordeCodexEvent] {
        var bySequence: [Int64: MordeCodexEvent] = [:]
        var byProviderID: [String: Int64] = [:]
        for event in current + incoming {
            guard event.session_id == sessionID, event.sequence > 0,
                  !event.provider_event_id.isEmpty, event.provider_event_id.utf8.count <= 512,
                  !event.kind.isEmpty else { throw MergeError.invalidEvent }
            if let prior = bySequence[event.sequence] {
                guard prior == event else { throw MergeError.conflictingDuplicate }
                continue
            }
            if let priorSequence = byProviderID[event.provider_event_id], priorSequence != event.sequence {
                throw MergeError.conflictingDuplicate
            }
            bySequence[event.sequence] = event
            byProviderID[event.provider_event_id] = event.sequence
        }
        let events = bySequence.values.sorted { $0.sequence < $1.sequence }
        if let afterSequence, afterSequence == Int64.max { throw MergeError.invalidEvent }
        if let afterSequence, let first = events.first, first.sequence != afterSequence + 1 {
            throw MergeError.sequenceGap(expected: afterSequence + 1, received: first.sequence)
        }
        for (left, right) in zip(events, events.dropFirst()) where right.sequence != left.sequence + 1 {
            throw MergeError.sequenceGap(expected: left.sequence + 1, received: right.sequence)
        }
        return events
    }

    static func timelineItems(_ events: [MordeCodexEvent]) -> [MordeCodexTimelineItem] {
        var output: [MordeCodexTimelineItem] = []
        let ordered = events.sorted(by: { $0.sequence < $1.sequence })
        var messageEvents: [MordeCodexTextItemKey: [MordeCodexEvent]] = [:]
        var toolCallEvents: [MordeCodexToolCallKey: [MordeCodexEvent]] = [:]
        for event in ordered {
            let kind: MordeCodexTimelineKind?
            if event.isUserMessage { kind = .user }
            else if event.isAssistantDelta || event.isAssistantMessage { kind = .assistant }
            else { kind = nil }
            if let kind {
                let key = MordeCodexTextItemKey(kind: kind, turnID: event.turn_id,
                                                itemID: event.item_id ?? "sequence-\(event.sequence)")
                messageEvents[key, default: []].append(event)
            }
            if let callID = event.codexToolCallID {
                toolCallEvents[MordeCodexToolCallKey(callID: callID), default: []].append(event)
            }
        }
        var questions: [String: MordeCodexUserQuestionRequest] = [:]
        var questionStatuses: [String: String] = [:]
        for event in ordered {
            guard let request = event.questionRequest else { continue }
            let key = "\(request.id)\0\(request.version)"
            if request.status == "pending", questions[key] == nil { questions[key] = request }
            questionStatuses[key] = request.status
        }
        var emittedQuestions = Set<String>()
        var emittedMessages = Set<MordeCodexTextItemKey>()
        var emittedToolCalls = Set<MordeCodexToolCallKey>()
        for event in ordered {
            let textKind: MordeCodexTimelineKind?
            if event.isUserMessage { textKind = .user }
            else if event.isAssistantDelta || event.isAssistantMessage { textKind = .assistant }
            else { textKind = nil }
            if let textKind {
                let key = MordeCodexTextItemKey(kind: textKind, turnID: event.turn_id,
                                                itemID: event.item_id ?? "sequence-\(event.sequence)")
                guard emittedMessages.insert(key).inserted, let group = messageEvents[key] else { continue }
                let snapshots = group.filter { $0.isUserMessage || $0.isAssistantMessage }
                let source = snapshots.isEmpty ? group : snapshots
                let text = joinedMessageText(source, kind: textKind)
                let latest = source.max(by: { $0.sequence < $1.sequence }) ?? event
                output.append(MordeCodexTimelineItem(
                    id: "\(textKind == .user ? "user" : "assistant")-\(key.turnID ?? "")-\(key.itemID)",
                    sequence: latest.sequence, kind: textKind, text: text, status: latest.visibleStatus,
                    approval: nil, question: nil, details: []))
                continue
            }
            if let request = event.questionRequest {
                let key = "\(request.id)\0\(request.version)"
                guard emittedQuestions.insert(key).inserted else { continue }
                let original = questions[key] ?? request
                let latestStatus = questionStatuses[key] ?? request.status
                output.append(MordeCodexTimelineItem(id: "question-\(request.id)-\(request.version)",
                    sequence: event.sequence, kind: .question, text: nil, status: latestStatus,
                    approval: nil, question: original.withStatus(latestStatus), details: []))
                continue
            }
            if let callID = event.codexToolCallID {
                let key = MordeCodexToolCallKey(callID: callID)
                guard emittedToolCalls.insert(key).inserted,
                      let group = toolCallEvents[key], let first = group.first else { continue }
                output.append(toolTimelineItem(callID: callID, events: group, sequence: first.sequence))
                continue
            }
            let kind: MordeCodexTimelineKind
            if event.isUserMessage { kind = .user }
            else if event.isApproval { kind = .approval }
            else if ["reasoning_summary", "reasoning_summary_part"].contains(event.normalizedKind) { kind = .reasoning }
            else if ["plan", "plan_update", "plan_summary"].contains(event.normalizedKind) { kind = .plan }
            else if ["tool_activity", "tool_call", "tool_call_summary", "tool_result", "tool_output_delta", "tool_result_summary",
                     "command", "command_summary", "command_output", "command_result"].contains(event.normalizedKind) { kind = .tool }
            else if ["file_change", "file_change_summary", "file_summary"].contains(event.normalizedKind) { kind = .file }
            else if ["child_agent", "subagent_activity", "subagent_summary", "agent_activity", "agent_summary"].contains(event.normalizedKind) { kind = .agent }
            else if event.normalizedKind == "command_status" { kind = .commandStatus }
            else if ["turn_status", "turn_started", "turn_completed", "turn_failed", "turn_interrupted",
                     "turn_cancelled", "status", "question"].contains(event.normalizedKind) { kind = .status }
            else { kind = .unsupported }
            let visibleText: String?
            if kind == .unsupported {
                visibleText = event.diagnostic
            } else {
                visibleText = kind != .user && kind != .assistant && isRawJSON(event.text) ? nil : event.text
            }
            let reasoningDetails = kind == .reasoning && visibleText != nil
                ? [MordeCodexTimelineDetail(field: .reasoning, text: visibleText, state: .complete)]
                : []
            output.append(MordeCodexTimelineItem(id: "event-\(event.sequence)", sequence: event.sequence,
                kind: kind, text: kind == .reasoning ? nil : visibleText, status: event.visibleStatus,
                approval: kind == .approval ? event.approval : nil, question: nil, details: reasoningDetails))
        }
        return output
    }

    private static func toolTimelineItem(callID: String, events: [MordeCodexEvent],
                                         sequence: Int64) -> MordeCodexTimelineItem {
        let ordered = events.sorted { $0.sequence < $1.sequence }
        let callEvents = ordered.filter { $0.normalizedKind == "tool_call" }
        let resultEvents = ordered.filter { ["tool_result", "tool_output_delta"].contains($0.normalizedKind) }
        let summaryEvents = ordered.filter {
            ["tool_call", "tool_call_summary", "tool_result_summary", "tool_result", "tool_output_delta"]
                .contains($0.normalizedKind)
        }
        let summary = ["summary", "text", "tool"].lazy
            .flatMap { key in summaryEvents.compactMap { $0.values[key]?.stringValue } }
            .first { !$0.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && !isRawJSON($0) }
        let input = assembleToolDetail(callEvents, field: "input", isOutput: false)
        let result = assembleToolDetail(resultEvents, field: "output", isOutput: true)
        let details = [input, result].compactMap { $0 }
        return MordeCodexTimelineItem(
            id: "tool-\(callID)", sequence: sequence, kind: .tool, text: summary,
            status: ordered.reversed().compactMap(\.visibleStatus).first,
            approval: nil, question: nil, details: details)
    }

    private static func assembleToolDetail(_ events: [MordeCodexEvent], field: String,
                                           isOutput: Bool) -> MordeCodexTimelineDetail? {
        guard !events.isEmpty else { return nil }
        let partKey = "\(field)_part"
        let countKey = "\(field)_parts"
        let hasField = events.contains { $0.values[field] != nil || $0.values[partKey] != nil || $0.values[countKey] != nil }
        guard hasField else { return nil }

        struct Chunk {
            let sequence: Int64
            let index: Int?
            let count: Int?
            let hasIndex: Bool
            let hasCount: Bool
            let hasText: Bool
            let text: String?
        }
        let receivedChunks = events.map { event in
            Chunk(sequence: event.sequence,
                  index: integerValue(event.values[partKey]),
                  count: integerValue(event.values[countKey]),
                  hasIndex: event.values[partKey] != nil,
                  hasCount: event.values[countKey] != nil,
                  hasText: event.values[field] != nil,
                  text: event.values[field]?.stringValue)
        }
        let hasDeclaredOutputSnapshot = isOutput && receivedChunks.contains(where: \.hasCount)
        let chunks = hasDeclaredOutputSnapshot
            ? receivedChunks.filter(\.hasCount)
            : receivedChunks
        var invalid = chunks.contains { chunk in
            if let text = chunk.text, text.utf8.count > 24 * 1024 { return true }
            if chunk.hasText && chunk.text == nil { return true }
            if chunk.hasIndex && chunk.index == nil { return true }
            if chunk.hasCount && chunk.count == nil { return true }
            if let index = chunk.index, index < 0 { return true }
            if let count = chunk.count, count <= 0 || count > 4096 { return true }
            if let index = chunk.index, let count = chunk.count, index >= count { return true }
            return false
        }
        let declaredCounts = Set(chunks.compactMap(\.count))
        if declaredCounts.count > 1 { invalid = true }
        let expectedCount = chunks.sorted { $0.sequence < $1.sequence }.compactMap(\.count).first
        let indexed = chunks.filter { $0.text != nil }
        let allIndexed = indexed.allSatisfy { $0.index != nil }
        var assembled: String
        var missing = false
        if allIndexed, !indexed.isEmpty, expectedCount == nil, isOutput {
            let sorted = indexed.sorted {
                guard let left = $0.index, let right = $1.index else { return $0.sequence < $1.sequence }
                return left == right ? $0.sequence < $1.sequence : left < right
            }
            var seenByIndex: [Int: String] = [:]
            var pieces: [String] = []
            var seenIndexes = Set<Int>()
            for chunk in sorted {
                guard let index = chunk.index, let text = chunk.text else { continue }
                if let previous = seenByIndex[index], previous == text { continue }
                if seenByIndex[index] != nil { invalid = true }
                seenByIndex[index] = text
                pieces.append(text)
                seenIndexes.insert(index)
            }
            let indexes = seenIndexes.sorted()
            missing = indexes != Array(0..<indexes.count)
            assembled = pieces.joined()
        } else if allIndexed, !indexed.isEmpty {
            var latestByIndex: [Int: String] = [:]
            for chunk in indexed {
                guard let index = chunk.index, let text = chunk.text else { continue }
                if let previous = latestByIndex[index], previous != text { invalid = true }
                latestByIndex[index] = text
            }
            if let expectedCount {
                let expected = Set(0..<expectedCount)
                missing = Set(latestByIndex.keys) != expected
            } else {
                let indexes = latestByIndex.keys.sorted()
                missing = indexes != Array(0..<indexes.count)
            }
            assembled = latestByIndex.keys.sorted().compactMap { latestByIndex[$0] }.joined()
        } else {
            let ordered = chunks.sorted { $0.sequence < $1.sequence }
            assembled = ordered.compactMap(\.text).joined()
            if expectedCount != nil || chunks.count > 1 || chunks.contains(where: \.hasIndex) {
                missing = true
            }
        }
        var state: MordeCodexTimelineDetailState
        if invalid || missing {
            state = .incomplete
        } else if expectedCount == nil && (chunks.contains(where: \.hasIndex) || chunks.count > 1) {
            state = isOutput ? .streaming : .incomplete
        } else {
            state = .complete
        }
        if !invalid, expectedCount == nil, chunks.count == 1,
           chunks.first?.hasIndex == false, chunks.first?.hasCount == false {
            state = .complete
        }
        return MordeCodexTimelineDetail(field: isOutput ? .output : .input,
                                        text: assembled.isEmpty ? nil : assembled, state: state)
    }

    private static func integerValue(_ value: MordeCodexJSONValue?) -> Int? {
        guard let value else { return nil }
        switch value {
        case .integer(let number): return Int(exactly: number)
        case .number(let number): return number.isFinite ? Int(exactly: number) : nil
        default: return nil
        }
    }

    private static func joinedMessageText(_ events: [MordeCodexEvent], kind: MordeCodexTimelineKind) -> String {
        guard !events.isEmpty else { return "" }
        let ordered: [MordeCodexEvent]
        if kind == .assistant, events.allSatisfy(\.isAssistantDelta) {
            let indexed = events.compactMap { event -> (Int, MordeCodexEvent)? in
                guard let value = event.values["delta_index"] else { return nil }
                let index: Int?
                switch value {
                case .integer(let number): index = Int(exactly: number)
                case .number(let number): index = number.isFinite ? Int(exactly: number) : nil
                default: index = nil
                }
                return index.map { ($0, event) }
            }
            if indexed.count == events.count {
                ordered = latestEventsByIndex(indexed).map { $0.1 }
            } else {
                ordered = events.sorted { $0.sequence < $1.sequence }
            }
        } else {
            let indexed = events.compactMap { event -> (Int, MordeCodexEvent)? in
                guard let itemID = event.item_id,
                      let segmentID = event.values["segment_id"]?.stringValue,
                      segmentID.hasPrefix(itemID + ":"),
                      let index = Int(segmentID.split(separator: ":").last ?? "") else { return nil }
                return (index, event)
            }
            if indexed.count == events.count, indexed.count > 1 {
                ordered = latestEventsByIndex(indexed).map { $0.1 }
            } else {
                ordered = [events.max(by: { $0.sequence < $1.sequence })!]
            }
        }
        return ordered.compactMap(\.text).joined()
    }

    private static func latestEventsByIndex(_ indexed: [(Int, MordeCodexEvent)]) -> [(Int, MordeCodexEvent)] {
        var latest: [Int: MordeCodexEvent] = [:]
        for (index, event) in indexed where latest[index].map({ $0.sequence < event.sequence }) ?? true {
            latest[index] = event
        }
        return latest.keys.sorted().compactMap { index in latest[index].map { (index, $0) } }
    }
}

enum MordeCodexTimelineKind: Hashable, Sendable {
    case user, assistant, reasoning, plan, tool, file, agent, approval, question, status, commandStatus, unsupported
}

enum MordeCodexTimelineDetailField: Hashable, Sendable {
    case input, output, reasoning

    var localizationKey: String {
        switch self {
        case .input: "Input"
        case .output: "Output"
        case .reasoning: "Reasoning summary"
        }
    }
}

enum MordeCodexTimelineDetailState: Hashable, Sendable {
    case complete, incomplete, streaming

    var localizationKey: String? {
        switch self {
        case .complete: nil
        case .incomplete: "Incomplete"
        case .streaming: "Streaming"
        }
    }
}

struct MordeCodexTimelineDetail: Equatable, Sendable {
    let field: MordeCodexTimelineDetailField
    let text: String?
    let state: MordeCodexTimelineDetailState
}

struct MordeCodexTimelineItem: Identifiable, Equatable, Sendable {
    let id: String
    let sequence: Int64
    let kind: MordeCodexTimelineKind
    let text: String?
    let status: String?
    let approval: MordeCodexApproval?
    let question: MordeCodexUserQuestionRequest?
    let details: [MordeCodexTimelineDetail]
}

enum MordeCodexSSEParserError: Error {
    case lineTooLong
    case dataTooLong
    case invalidUTF8
}

struct MordeCodexSSEFrame: Equatable, Sendable {
    let id: String?
    let event: String?
    let data: String
}

struct MordeCodexSSEParser {
    private var line = Data()
    private var frameID: String?
    private var frameEvent: String?
    private var dataLines: [String] = []
    private var dataBytes = 0

    mutating func append(_ byte: UInt8) throws -> MordeCodexSSEFrame? {
        guard byte == 0x0A else {
            guard line.count < 65_536 else { throw MordeCodexSSEParserError.lineTooLong }
            line.append(byte)
            return nil
        }
        guard var text = String(data: line, encoding: .utf8) else { throw MordeCodexSSEParserError.invalidUTF8 }
        line.removeAll(keepingCapacity: true)
        if text.last == "\r" { text.removeLast() }
        if text.isEmpty {
            defer { resetFrame() }
            guard !dataLines.isEmpty else { return nil }
            return MordeCodexSSEFrame(id: frameID, event: frameEvent, data: dataLines.joined(separator: "\n"))
        }
        guard !text.hasPrefix(":") else { return nil }
        let separator = text.firstIndex(of: ":")
        let field = separator.map { String(text[..<$0]) } ?? text
        var value = separator.map { String(text[text.index(after: $0)...]) } ?? ""
        if value.first == " " { value.removeFirst() }
        switch field {
        case "id": frameID = value
        case "event": frameEvent = value
        case "data":
            dataBytes += value.utf8.count + (dataLines.isEmpty ? 0 : 1)
            guard dataBytes <= 65_536 else { throw MordeCodexSSEParserError.dataTooLong }
            dataLines.append(value)
        default: break
        }
        return nil
    }

    private mutating func resetFrame() {
        frameID = nil
        frameEvent = nil
        dataLines.removeAll(keepingCapacity: true)
        dataBytes = 0
    }
}
