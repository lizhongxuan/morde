import AppIntents
import Foundation

/// Wraps a ChatSession as an AppEntity so Shortcuts can reference sessions by name.
struct SessionEntity: AppEntity {
    static var typeDisplayRepresentation = TypeDisplayRepresentation(name: "New Session")
    static var defaultQuery = SessionEntityQuery()

    var id: String
    var displayName: String
    var modelId: String

    var displayRepresentation: DisplayRepresentation {
        DisplayRepresentation(title: "\(displayName)")
    }

    init(id: String, displayName: String, modelId: String) {
        self.id = id
        self.displayName = displayName
        self.modelId = modelId
    }

    init(from session: ChatSession) {
        self.id = session.id
        self.displayName = session.title ?? "Untitled"
        self.modelId = session.modelId
    }
}

struct SessionEntityQuery: EntityQuery {
    func entities(for identifiers: [String]) async throws -> [SessionEntity] {
        let sessions = await NanoMuseSync.shared.visible(await ChatStore.shared.listSessions())
        // nanoMuse: C12 filters explicit ID resolution as well as suggested sessions.
        return identifiers.compactMap { id in
            sessions.first(where: { $0.id == id }).map(SessionEntity.init(from:))
        }
    }

    func suggestedEntities() async throws -> [SessionEntity] {
        let sessions = await NanoMuseSync.shared.visible(await ChatStore.shared.listSessions()) // nanoMuse: C12 — another account's chats stay hidden
        return sessions.prefix(100).map { SessionEntity(from: $0) }
    }
}
