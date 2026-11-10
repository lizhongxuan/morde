import XCTest
@testable import Minis

final class MIFResourceViewsTests: XCTestCase {
    func testSharedHostNeverOffersControlActions() {
        let host = MIFResourcePageState.Host(
            id: "h1234567", name: "Shared", scope: .sharedReadOnly, status: .running,
            workspaceSummary: nil, canOpen: true,
            control: control(canTakeOver: true, canReturnToAI: true, canRenewLease: true),
            error: nil)

        XCTAssertTrue(MIFResourcePageRules.controlActions(for: host).isEmpty)
    }

    func testUnknownOrTransitioningControlOffersNoActions() {
        let unknown = ownedHost(control: control(state: .unknown, canTakeOver: true))
        let unknownLease = ownedHost(control: control(lease: .unknown, canTakeOver: true))
        let transitioning = ownedHost(control: control(canTakeOver: true, transitionSummary: "handoff"))
        let returning = ownedHost(control: control(state: .returning, canReturnToAI: true))
        let offline = ownedHost(status: .offline, control: control(canTakeOver: true))

        XCTAssertTrue(MIFResourcePageRules.controlActions(for: unknown).isEmpty)
        XCTAssertTrue(MIFResourcePageRules.controlActions(for: unknownLease).isEmpty)
        XCTAssertTrue(MIFResourcePageRules.controlActions(for: transitioning).isEmpty)
        XCTAssertTrue(MIFResourcePageRules.controlActions(for: returning).isEmpty)
        XCTAssertTrue(MIFResourcePageRules.controlActions(for: offline).isEmpty)
    }

    func testOwnedStableControlUsesOnlyClientApprovedActions() {
        let host = ownedHost(control: control(canReturnToAI: true, canRenewLease: true))

        XCTAssertEqual(MIFResourcePageRules.controlActions(for: host), [.returnToAI, .renewLease])
    }

    func testExpiredLeaseOnlyOffersTheApprovedTakeoverAction() {
        let host = ownedHost(control: control(
            lease: .expired, canTakeOver: true, canReturnToAI: true, canRenewLease: true))

        XCTAssertEqual(MIFResourcePageRules.controlActions(for: host), [.takeOver])
    }

    private func ownedHost(
        status: MIFResourcePageState.Host.Status = .running,
        control: MIFResourcePageState.Control
    ) -> MIFResourcePageState.Host {
        MIFResourcePageState.Host(
            id: "h1234567", name: "Owned", scope: .owned, status: status,
            workspaceSummary: nil, canOpen: true, control: control, error: nil)
    }

    private func control(
        state: MIFResourcePageState.Control.State = .user,
        lease: MIFResourcePageState.Control.Lease = .active,
        canTakeOver: Bool = false,
        canReturnToAI: Bool = false,
        canRenewLease: Bool = false,
        transitionSummary: String? = nil
    ) -> MIFResourcePageState.Control {
        MIFResourcePageState.Control(
            state: state, lease: lease, leaseExpiresAt: nil,
            transitionSummary: transitionSummary, isLoading: false, isActionInFlight: false,
            canTakeOver: canTakeOver, canReturnToAI: canReturnToAI,
            canRenewLease: canRenewLease, error: nil)
    }
}
