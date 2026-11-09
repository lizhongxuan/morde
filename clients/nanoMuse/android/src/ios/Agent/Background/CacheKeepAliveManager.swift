import Foundation

/// Retired local provider warmup. Kept as a no-op compatibility surface while
/// the old chat executor is removed from the Minis target.
@MainActor
final class CacheKeepAliveManager {
    static let shared = CacheKeepAliveManager()
    private init() {}

    func recordRequest(sessionId: String, vm: AnyObject) {
        _ = (sessionId, vm)
    }

    func cancelKeepAlive(sessionId: String) {
        _ = sessionId
    }
}
