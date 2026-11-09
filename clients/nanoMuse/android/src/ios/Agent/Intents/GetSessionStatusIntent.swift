import AppIntents
import Foundation

/// Structured status result for a session, usable in Shortcuts conditionals and text blocks.
struct SessionStatus: AppEntity {
    static var typeDisplayRepresentation = TypeDisplayRepresentation(name: "Session Status")
    static var defaultQuery = SessionStatusQuery()

    var id: String  // session ID

    @Property(title: "Session ID")
    var sessionId: String

    @Property(title: "Title")
    var title: String

    @Property(title: "Model")
    var modelName: String

    @Property(title: "Is Running")
    var isRunning: Bool

    @Property(title: "Last Message")
    var lastMessage: String

    @Property(title: "Last Tool")
    var lastTool: String

    @Property(title: "Updated At")
    var updatedAt: Date

    var displayRepresentation: DisplayRepresentation {
        DisplayRepresentation(
            title: "\(title)",
            subtitle: "\(isRunning ? "Running" : "Idle") · \(modelName)"
        )
    }

    init(sessionId: String, title: String, modelName: String, isRunning: Bool,
         lastMessage: String, lastTool: String, updatedAt: Date) {
        self.id = sessionId
        self.sessionId = sessionId
        self.title = title
        self.modelName = modelName
        self.isRunning = isRunning
        self.lastMessage = lastMessage
        self.lastTool = lastTool
        self.updatedAt = updatedAt
    }
}

struct SessionStatusQuery: EntityQuery {
    func entities(for identifiers: [String]) async throws -> [SessionStatus] {
        []
    }
}

/// Queries the current status of a session — whether the agent is still running,
/// the latest message text, last tool summary, etc. Use after SendPrompt to poll.
struct GetSessionStatusIntent: AppIntent {
    static var title: LocalizedStringResource = "Get Session Status"
    static var description = IntentDescription("Gets the current status of a morde session, including whether the agent is still running, the latest message, and last tool call.")
    static var openAppWhenRun = false

    @Parameter(title: "Session ID")
    var sessionID: String

    @MainActor
    func perform() async throws -> some IntentResult & ReturnsValue<SessionStatus> {
        let unavailable = SessionStatus(
            sessionId: "", title: "Untitled", modelName: "unknown", isRunning: false,
            lastMessage: "", lastTool: "", updatedAt: .distantPast)
        let visibleSessions = await NanoMuseSync.shared.visible(await ChatStore.shared.listSessions())
        // nanoMuse: C12 prevents a guessed session ID from exposing another account's status.
        guard let preview = visibleSessions.first(where: { $0.id == sessionID }) else {
            return .result(value: unavailable)
        }
        let session = await ChatStore.shared.getSession(sessionID)
        guard NanoMuseSync.shared.shows(sessionID) else { return .result(value: unavailable) }
        let isRunning = SessionActivityTracker.shared.isActive(sessionID)
        let title = session?.title ?? preview.title ?? "Untitled"
        let updatedAt = session?.updatedAt ?? preview.updatedAt

        // Resolve model name from binding
        var modelName = session?.modelId ?? "unknown"
        let store = ProviderConfigStore.shared
        if let binding = store.binding(for: sessionID) {
            switch binding.primarySource {
            case .group(_, let resolvedEntryId):
                if let entry = store.entry(for: resolvedEntryId) {
                    modelName = entry.model.displayName
                }
            case .directEntry(let modelEntryId, _):
                if let entry = store.entry(for: modelEntryId) {
                    modelName = entry.model.displayName
                }
            }
        }

        // nanoMuse: Use the persisted preview; live VM/tool state is not required for this read-only intent.
        let lastMessage = preview.lastMessage ?? ""
        let lastTool = ""

        let status = SessionStatus(
            sessionId: sessionID,
            title: title,
            modelName: modelName,
            isRunning: isRunning,
            lastMessage: lastMessage,
            lastTool: lastTool,
            updatedAt: updatedAt
        )

        return .result(value: status)
    }
}
