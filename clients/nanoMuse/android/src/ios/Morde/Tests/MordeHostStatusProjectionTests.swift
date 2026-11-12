import XCTest
@testable import MordeTaskPolicies

final class MordeHostStatusProjectionTests: XCTestCase {
    func testRuntimeHeartbeatMissingIsUnknownAndRecoversIndependently() {
        let heartbeatSequence = ([false, true, nil] as [Bool?]).map { MIFHostRuntimeStatus(online: $0) }

        XCTAssertEqual(heartbeatSequence, [.offline, .online, .unknown])
    }

    func testUnknownDesktopSignalNeverProjectsAsConnected() {
        XCTAssertEqual(MIFDesktopConnectionStatus(scriptValue: "connecting"), .connecting)
        XCTAssertEqual(MIFDesktopConnectionStatus(scriptValue: "connected"), .connected)
        XCTAssertEqual(MIFDesktopConnectionStatus(scriptValue: "disconnected"), .disconnected)
        XCTAssertEqual(MIFDesktopConnectionStatus(scriptValue: "failed"), .failed)
        XCTAssertEqual(MIFDesktopConnectionStatus(scriptValue: "unrecognized"), .unknown)
        XCTAssertNotEqual(MIFDesktopConnectionStatus(scriptValue: "unrecognized"), .connected)
    }

    func testSyncApplicationRequiresHealthyReceiptAndPreservesUnknownStatus() throws {
        let confirmed = hostApplication(status: "applied", runtimeGeneration: 4,
                                        appliedRevision: 12, syncState: "healthy")
        XCTAssertTrue(confirmed.confirmsApplication(targetRevision: 12,
                                                    hostID: "h1234567", bindingVersion: 3))
        let verifiedLaterRevision = hostApplication(status: "applied", runtimeGeneration: 4,
                                                   appliedRevision: 13, syncState: "healthy")
        XCTAssertTrue(verifiedLaterRevision.confirmsApplication(targetRevision: 12,
                                                                hostID: "h1234567", bindingVersion: 3))
        XCTAssertFalse(confirmed.confirmsApplication(targetRevision: 11,
                                                     hostID: "h1234567", bindingVersion: 3))
        XCTAssertFalse(confirmed.confirmsApplication(targetRevision: 12,
                                                     hostID: "h7654321", bindingVersion: 3))
        XCTAssertFalse(confirmed.confirmsApplication(targetRevision: 12,
                                                     hostID: "h1234567", bindingVersion: 4))

        let unresolved = hostApplication(status: "applied", runtimeGeneration: 4,
                                         appliedRevision: 12, syncState: "healthy",
                                         impacts: [.init(path: "src/a.swift", state: "unknown", reason: nil)])
        XCTAssertFalse(unresolved.confirmsApplication(targetRevision: 12,
                                                      hostID: "h1234567", bindingVersion: 3))
        let olderRevision = hostApplication(status: "applied", runtimeGeneration: 4,
                                            appliedRevision: 11, syncState: "healthy")
        XCTAssertFalse(olderRevision.confirmsApplication(targetRevision: 12,
                                                        hostID: "h1234567", bindingVersion: 3))
        let missingGeneration = hostApplication(status: "applied", runtimeGeneration: nil,
                                                appliedRevision: 12, syncState: "healthy")
        XCTAssertFalse(missingGeneration.confirmsApplication(targetRevision: 12,
                                                             hostID: "h1234567", bindingVersion: 3))
        let unhealthy = hostApplication(status: "applied", runtimeGeneration: 4,
                                        appliedRevision: 12, syncState: "conflict")
        XCTAssertFalse(unhealthy.confirmsApplication(targetRevision: 12,
                                                     hostID: "h1234567", bindingVersion: 3))

        let future = try JSONDecoder().decode(MordeWorkspaceSyncHostApplication.self, from: Data("""
        {"status":"future_state","host_id":"h1234567","workspace_binding_version":3,
         "target_revision":12,"manifest_sha256":"\(String(repeating: "a", count: 64))",
         "runtime_generation":null,"applied_revision":null,"sync_state":null,"impacts":[]}
        """.utf8))
        XCTAssertEqual(future.status, "future_state")
        XCTAssertFalse(future.confirmsApplication(targetRevision: 12,
                                                  hostID: "h1234567", bindingVersion: 3))
    }

    private func hostApplication(
        status: String,
        runtimeGeneration: Int64?,
        appliedRevision: Int64?,
        syncState: String?,
        impacts: [MordeWorkspaceSyncHostApplicationImpact] = []
    ) -> MordeWorkspaceSyncHostApplication {
        MordeWorkspaceSyncHostApplication(
            status: status, host_id: "h1234567", workspace_binding_version: 3,
            target_revision: 12, manifest_sha256: String(repeating: "a", count: 64),
            runtime_generation: runtimeGeneration, applied_revision: appliedRevision,
            sync_state: syncState, impacts: impacts)
    }
}
