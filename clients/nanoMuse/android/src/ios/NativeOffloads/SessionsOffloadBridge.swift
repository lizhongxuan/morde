//
//  SessionsOffloadBridge.swift
//  MinisApp
//
//  Swift bridge for minis-sessions-cli offload.
//  Provides synchronous @objc entry points that query ChatStore.
//

import Foundation

@objc public class SessionsOffloadBridge: NSObject {

    // MARK: - List / Search Sessions

    /// Query sessions with optional filters. Returns NSDictionary with "sessions" array.
    /// Thread-safe: uses Task.detached to call ChatStore actor, blocks caller until done.
    /// Call from any thread EXCEPT main (semaphore would deadlock main).
    @objc public static func querySessions(
        sessionIds: [String]?,
        keywords: [String]?,
        startDate: Date?,
        endDate: Date?,
        limit: Int
    ) -> NSDictionary {
        // ChatStore is an actor — bridge into async via semaphore + Task.detached.
        // Called from the iSH guest thread (never main), so blocking is safe.
        let sem = DispatchSemaphore(value: 0)
        var result: [[String: Any]] = []

        Task.detached {
            let metas = await ChatStore.shared.querySessionsMeta(
                sessionIds: sessionIds,
                keywords: keywords,
                limit: limit,
                startDate: startDate,
                endDate: endDate
            )

            let dateFmt = DateFormatter()
            dateFmt.dateFormat = "yyyy-MM-dd HH:mm"

            result = metas.map { m in
                var dict: [String: Any] = [
                    "session_id": m.id,
                    "started_at": dateFmt.string(from: m.startedAt),
                    "last_active": dateFmt.string(from: m.lastActive),
                    "message_count": m.messageCount,
                ]
                if let t = m.title { dict["title"] = t }
                if let p = m.preview { dict["preview"] = p }
                if let s = m.source { dict["source"] = s }
                return dict
            }
            sem.signal()
        }
        sem.wait()

        return [
            "count": result.count,
            "sessions": result,
        ] as NSDictionary
    }

    // MARK: - Search Messages

    /// Search messages across sessions by keywords. Returns NSDictionary with "messages" array.
    @objc public static func searchMessages(
        sessionIds: [String]?,
        keywords: [String],
        startDate: Date?,
        endDate: Date?,
        limit: Int
    ) -> NSDictionary {
        let sem = DispatchSemaphore(value: 0)
        var result: [[String: Any]] = []

        Task.detached {
            let matches = await ChatStore.shared.searchMessages(
                sessionIds: sessionIds,
                keywords: keywords,
                limit: limit,
                startDate: startDate,
                endDate: endDate
            )

            let dateFmt = DateFormatter()
            dateFmt.dateFormat = "yyyy-MM-dd HH:mm"

            result = matches.map { m in
                // [T-search-cli-title] session_title mirrors SessionList row's
                // `session.title ?? "Untitled"` fallback so CLI users can
                // identify a hit's conversation by name instead of having to
                // cross-reference the raw UUID against `list`.
                [
                    "session_id": m.sessionId,
                    "session_title": m.sessionTitle ?? "Untitled",
                    "message_id": m.messageId,
                    "role": m.role,
                    "created_at": dateFmt.string(from: m.createdAt),
                    "snippet": m.snippet,
                ] as [String: Any]
            }
            sem.signal()
        }
        sem.wait()

        return [
            "count": result.count,
            "messages": result,
        ] as NSDictionary
    }

    // MARK: - Load Messages by Session ID

    /// Load a page of messages for a specific session. Returns NSDictionary with "messages" array and pagination info.
    /// `full` switches the per-message text cap from 1000 to 50000 chars; messages whose
    /// stored text exceeds the cap are still truncated and marked with "truncated": true.
    /// [T-sessions-cli-messages-date-filter] (GH#200) `startDate`/`endDate`
    /// forward the CLI's `--start`/`--end` down to ChatStore. They were absent
    /// from this signature entirely, which is why the ObjC layer had nothing to
    /// pass and the flags were silently dropped.
    @objc public static func loadMessages(
        sessionId: String,
        offset: Int,
        limit: Int,
        full: Bool,
        startDate: Date?,
        endDate: Date?
    ) -> NSDictionary {
        let sem = DispatchSemaphore(value: 0)
        var result: [[String: Any]] = []
        var totalCount = 0
        let maxChars = full ? 50_000 : 1_000

        Task.detached {
            let messages = await ChatStore.shared.loadMessagePage(
                sessionId: sessionId,
                offset: offset,
                limit: limit,
                maxChars: maxChars,
                startDate: startDate,
                endDate: endDate
            )
            // Counted over the SAME range so `total` describes the filtered set
            // the caller is paging through, not the whole session.
            totalCount = await ChatStore.shared.messageCount(sessionId: sessionId,
                                                             startDate: startDate,
                                                             endDate: endDate)

            let dateFmt = DateFormatter()
            dateFmt.dateFormat = "yyyy-MM-dd HH:mm"

            result = messages.map { m in
                var dict: [String: Any] = [
                    "message_id": m.messageId,
                    "role": m.role,
                    "created_at": dateFmt.string(from: m.createdAt),
                    "text": m.text,
                ]
                if m.truncated { dict["truncated"] = true }
                return dict
            }
            sem.signal()
        }
        sem.wait()

        return [
            "session_id": sessionId,
            "offset": offset,
            "limit": limit,
            "full": full,
            "max_chars": maxChars,
            "total": totalCount,
            "count": result.count,
            "messages": result,
        ] as NSDictionary
    }

    // MARK: - Shared helpers

    /// Resolve a binding source to a readable model display name.
    /// Returns nil when no matching entry is found.
    @MainActor
    private static func resolveBindingModelName(_ binding: SessionModelBinding) -> String? {
        let store = ProviderConfigStore.shared
        switch binding.primarySource {
        case .group(_, let resolvedEntryId):
            return store.entry(for: resolvedEntryId)?.model.displayName
        case .directEntry(let modelEntryId, _):
            return store.entry(for: modelEntryId)?.model.displayName
        }
    }

    // MARK: - Local Send / Retry

    /// Compatibility endpoint for the legacy iSH CLI. Local execution is
    /// retired; remote Morde tasks use their own remote task API.
    @objc public static func sendPrompt(
        sessionId: String?,
        prompt: String,
        attachmentPaths: [String],
        modelEntryId: String?,
        source: String
    ) -> NSDictionary {
        _ = (sessionId, prompt, attachmentPaths, modelEntryId, source)
        return [
            "ok": false,
            "error": "local_execution_disabled",
            "message": "Local iSH execution is disabled.",
        ] as NSDictionary
    }

    /// Compatibility endpoint for the legacy iSH CLI. Local execution is
    /// retired; message history remains available through the read-only APIs.
    @objc public static func retryMessage(
        sessionId: String,
        messageId: String?,
        attachmentPaths: [String]
    ) -> NSDictionary {
        _ = (sessionId, messageId, attachmentPaths)
        return [
            "ok": false,
            "error": "local_execution_disabled",
            "message": "Local iSH execution is disabled.",
        ] as NSDictionary
    }

    // MARK: - Open Session (navigate UI to a given session)

    /// Routes the app UI to the given session, mirroring what
    /// `minis://sessions/<id>` does. Validates that the session exists
    /// (via ChatStore) before posting the open-session notification so
    /// a typo from the CLI doesn't leave the user staring at a stale
    /// navigation. Returns immediately — UI navigation happens on the
    /// main actor on the next runloop.
    @objc public static func openSession(sessionId: String) -> NSDictionary {
        let sem = DispatchSemaphore(value: 0)
        var output: [String: Any] = [:]

        Task.detached {
            let session = await ChatStore.shared.getSession(sessionId)
            guard let session else {
                output = [
                    "ok": false,
                    "action": "open",
                    "error": "session_not_found",
                    "session_id": sessionId,
                ]
                sem.signal()
                return
            }
            await MainActor.run {
                NotificationCenter.default.post(
                    name: .openSessionFromIntent,
                    object: nil,
                    userInfo: ["sessionId": session.id]
                )
            }
            output = [
                "ok": true,
                "action": "open",
                "session_id": session.id,
                "title": session.title ?? "",
            ]
            sem.signal()
        }
        sem.wait()
        return output as NSDictionary
    }

    // MARK: - Get Session Status

    /// Query session status from ChatStore and SessionActivityTracker only.
    @objc public static func getSessionStatus(sessionId: String) -> NSDictionary {
        let sem = DispatchSemaphore(value: 0)
        var output: [String: Any] = [:]

        Task { @MainActor in
            // SessionActivityTracker is the authoritative is-running signal.
            let isRunning = SessionActivityTracker.shared.isActive(sessionId)

            // Pull canonical session metadata from the DB.
            let session = await ChatStore.shared.getSession(sessionId)
            guard let session else {
                output = [
                    "ok": false,
                    "error": "session_not_found",
                    "session_id": sessionId,
                ]
                sem.signal()
                return
            }

            // Resolve model display name via the session binding.
            var modelName = session.modelId
            if let binding = ProviderConfigStore.shared.binding(for: sessionId),
               let resolved = resolveBindingModelName(binding) {
                modelName = resolved
            }

            var lastMessage = session.lastMessage ?? ""
            var lastTool = ""
            let messages = await ChatStore.shared.loadMessages(sessionId: sessionId)
            if let lastAssistant = messages.last(where: { $0.role == .assistant }) {
                let textBlocks = lastAssistant.parts.compactMap { part -> String? in
                    if case .text(let text) = part { return text }
                    return nil
                }
                let joined = textBlocks.joined(separator: "\n").trimmingCharacters(in: .whitespacesAndNewlines)
                if !joined.isEmpty { lastMessage = joined }
                for part in lastAssistant.parts {
                    if case .toolUse(let toolUse) = part {
                        lastTool = toolUse.description ?? toolUse.name
                    }
                }
            }

            let dateFmt = DateFormatter()
            dateFmt.dateFormat = "yyyy-MM-dd HH:mm"

            output = [
                "ok": true,
                "action": "status",
                "data": [
                    "session_id": session.id,
                    "title": session.title ?? "",
                    "model_name": modelName,
                    "is_running": isRunning,
                    "last_message": lastMessage,
                    "last_tool": lastTool,
                    "updated_at": dateFmt.string(from: session.updatedAt),
                ] as [String: Any],
            ]
            sem.signal()
        }
        sem.wait()
        return output as NSDictionary
    }
}
