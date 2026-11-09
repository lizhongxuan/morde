import XCTest
@testable import Minis

final class MordeClientDataScopeTests: XCTestCase {
    func testServiceInstancePartitionsScopeAndStorageKey() {
        let original = makeScope(instanceID: "11111111-1111-4111-8111-111111111111")
        let rebuilt = makeScope(instanceID: "22222222-2222-4222-8222-222222222222")

        XCTAssertEqual(original.serviceOrigin, rebuilt.serviceOrigin)
        XCTAssertEqual(original.accountID, rebuilt.accountID)
        XCTAssertEqual(original.installationID, rebuilt.installationID)
        XCTAssertNotEqual(original, rebuilt)
        XCTAssertNotEqual(original.storageKey, rebuilt.storageKey)
    }

    func testRecentConversationDoesNotMatchRebuiltServiceInstance() {
        let original = makeScope(instanceID: "11111111-1111-4111-8111-111111111111")
        let rebuilt = makeScope(instanceID: "22222222-2222-4222-8222-222222222222")
        let recent = MordeScopedRecentConversation(scope: original, conversationID: "conversation-1")

        XCTAssertTrue(recent.matches(scope: original))
        XCTAssertFalse(recent.matches(scope: rebuilt))
    }

    func testLateUnauthorizedResponseCannotClearRebuiltServiceAuthorization() {
        let requestScope = makeScope(instanceID: "11111111-1111-4111-8111-111111111111")
        let currentScope = makeScope(instanceID: "22222222-2222-4222-8222-222222222222")

        XCTAssertFalse(MordeUnauthorizedResponsePolicy.shouldClearAuthorization(
            requestCredential: "same-token", currentCredential: "same-token",
            requestScope: requestScope, currentScope: currentScope,
            hasPendingLoginIntent: false, isCancelled: false))
    }

    private func makeScope(instanceID: String) -> MordeClientDataScope {
        MordeClientDataScope(serviceOrigin: "https://morde.example", serviceInstanceID: instanceID,
                             accountID: "user-1", installationID: "install-1")!
    }
}
