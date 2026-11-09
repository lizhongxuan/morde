import XCTest
@testable import MordeTaskPolicies

final class MordeRevocationRetryPolicyTests: XCTestCase {
    func testOnlyRetriesUnconfirmedInstallationsForTheSignedInAccountAndService() throws {
        let origin = "https://morde.example"
        let serviceID = "a1234567-1234-4234-8234-123456789abc"
        let currentScope = try XCTUnwrap(MordeClientDataScope(
            serviceOrigin: origin, serviceInstanceID: serviceID,
            accountID: "u_account1", installationID: "i1234567"))
        let oldScope = try XCTUnwrap(MordeClientDataScope(
            serviceOrigin: origin, serviceInstanceID: serviceID,
            accountID: "u_account1", installationID: "i7654321"))
        let otherAccountScope = try XCTUnwrap(MordeClientDataScope(
            serviceOrigin: origin, serviceInstanceID: serviceID,
            accountID: "u_account2", installationID: "i2345678"))
        let otherServiceScope = try XCTUnwrap(MordeClientDataScope(
            serviceOrigin: origin, serviceInstanceID: "b1234567-1234-4234-8234-123456789abc",
            accountID: "u_account1", installationID: "i3456789"))
        let usernameFallbackScope = try XCTUnwrap(MordeClientDataScope(
            serviceOrigin: origin, serviceInstanceID: serviceID,
            accountID: "admin", installationID: "i4567890"))
        let activeScope = try XCTUnwrap(MordeClientDataScope(
            serviceOrigin: origin, serviceInstanceID: serviceID,
            accountID: "u_account1", installationID: "i1234567"))

        let retryable = MordeRevocationRecord(scope: oldScope, clientInstanceID: nil, confirmed: false)
        let records = [
            retryable,
            MordeRevocationRecord(scope: usernameFallbackScope, clientInstanceID: nil, confirmed: false),
            MordeRevocationRecord(scope: otherAccountScope, clientInstanceID: nil, confirmed: false),
            MordeRevocationRecord(scope: otherServiceScope, clientInstanceID: nil, confirmed: false),
            MordeRevocationRecord(scope: activeScope, clientInstanceID: nil, confirmed: false),
            MordeRevocationRecord(scope: oldScope, clientInstanceID: nil, confirmed: true),
        ]

        let candidates = MordeRevocationRetryPolicy.candidates(
            records, currentScope: currentScope, currentInstallationID: "i1234567", accountName: "admin")

        XCTAssertEqual(candidates.map(\.scope.installationID), ["i7654321", "i4567890"])
    }

    func testConfirmedRevocationIsRemovedFromPendingAfterStoreReopen() throws {
        let suite = "MordeRevocationRetryPolicyTests-\(UUID().uuidString)"
        let defaults = try XCTUnwrap(UserDefaults(suiteName: suite))
        defaults.removePersistentDomain(forName: suite)
        defer { defaults.removePersistentDomain(forName: suite) }

        let scope = try XCTUnwrap(MordeClientDataScope(
            serviceOrigin: "https://morde.example",
            serviceInstanceID: "a1234567-1234-4234-8234-123456789abc",
            accountID: "u_account1", installationID: "i7654321"))
        let record = MordeRevocationRecord(scope: scope, clientInstanceID: UUID(), confirmed: false)
        let store = MordeRevocationStore(defaults: defaults)
        try store.record(record)

        try store.confirm(record)

        XCTAssertTrue(MordeRevocationStore(defaults: defaults).pending().isEmpty)
    }
}
