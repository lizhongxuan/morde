import XCTest
@testable import Minis

final class MIFResourceViewsTests: XCTestCase {
    func testResourceSectionsFollowDesignOrder() {
        XCTAssertEqual(MIFResourcePageSection.allCases, [
            .cloudComputers, .cloudPhones, .workspaces, .aiTools,
        ])
        XCTAssertEqual(MIFResourcePageSection.cloudPhones.title, AppLocalized("Cloud phones"))
        XCTAssertEqual(MIFResourcePageSection.aiTools.title, AppLocalized("AI tools"))
    }

    func testUnavailableSectionsDoNotClaimSyntheticRuntimeData() {
        let cloudPhones = MIFResourcePageSection.cloudPhones.emptyMessage
        let aiTools = MIFResourcePageSection.aiTools.emptyMessage

        XCTAssertEqual(cloudPhones, AppLocalized("Execution status unavailable"))
        XCTAssertEqual(aiTools, AppLocalized("No AI tool runtime status is available in this account scope."))
        XCTAssertFalse(cloudPhones.localizedCaseInsensitiveContains("running"))
        XCTAssertFalse(aiTools.localizedCaseInsensitiveContains("running"))
        XCTAssertFalse(aiTools.contains("Codex"))
        XCTAssertFalse(aiTools.contains("Claude"))
    }

    func testHostAndWorkspaceEmptyStatesRequireConfirmedLists() {
        let loadedEmpty = resourceState(isLoading: false, loadError: nil)
        XCTAssertTrue(MIFResourcePageRules.showsEmptyState(for: .cloudComputers, state: loadedEmpty))
        XCTAssertTrue(MIFResourcePageRules.showsEmptyState(for: .workspaces, state: loadedEmpty))

        let failed = resourceState(isLoading: false, loadError: "resource list failed")
        XCTAssertFalse(MIFResourcePageRules.showsEmptyState(for: .cloudComputers, state: failed))
        XCTAssertFalse(MIFResourcePageRules.showsEmptyState(for: .workspaces, state: failed))

        let loading = resourceState(isLoading: true, loadError: nil)
        XCTAssertFalse(MIFResourcePageRules.showsEmptyState(for: .cloudComputers, state: loading))
        XCTAssertFalse(MIFResourcePageRules.showsEmptyState(for: .workspaces, state: loading))
        XCTAssertTrue(MIFResourcePageRules.showsEmptyState(for: .cloudPhones, state: failed))
        XCTAssertTrue(MIFResourcePageRules.showsEmptyState(for: .aiTools, state: failed))
    }

    func testResourceListFailureDoesNotHideOtherConfirmedEmptySection() {
        let workspaceFailure = resourceState(
            isLoading: false, loadError: "Workspace list failed",
            hostsState: .init(isLoading: false, loadError: nil),
            workspacesState: .init(isLoading: false, loadError: "Workspace list failed"))
        XCTAssertTrue(MIFResourcePageRules.showsEmptyState(for: .cloudComputers, state: workspaceFailure))
        XCTAssertFalse(MIFResourcePageRules.showsEmptyState(for: .workspaces, state: workspaceFailure))

        let hostFailure = resourceState(
            isLoading: false, loadError: "Host list failed",
            hostsState: .init(isLoading: false, loadError: "Host list failed"),
            workspacesState: .init(isLoading: false, loadError: nil))
        XCTAssertFalse(MIFResourcePageRules.showsEmptyState(for: .cloudComputers, state: hostFailure))
        XCTAssertTrue(MIFResourcePageRules.showsEmptyState(for: .workspaces, state: hostFailure))
    }

    func testSharedHostNeverOffersControlActions() {
        let host = MIFResourcePageState.Host(
            id: "h1234567", name: "Shared", scope: .sharedReadOnly, status: .running,
            workspaceSummary: nil, canOpen: true,
            control: control(canTakeOver: true, canReturnToAI: true),
            error: nil)

        XCTAssertTrue(MIFResourcePageRules.controlActions(for: host).isEmpty)
    }

    func testUnknownOrTransitioningControlOffersNoActions() {
        let unknown = ownedHost(control: control(state: .unknown, canTakeOver: true))
        let unknownLease = ownedHost(control: control(lease: .unknown, canTakeOver: true))
        let transitioning = ownedHost(control: control(canTakeOver: true, isTransitioning: true))
        let returning = ownedHost(control: control(state: .returning, canReturnToAI: true))
        // nanoMuse: offline hosts now report their concrete failed or stopped state.
        let failed = ownedHost(status: .failed, control: control(canTakeOver: true))
        let starting = ownedHost(status: .starting, control: control(canTakeOver: true))
        let stopped = ownedHost(status: .stopped, control: control(canTakeOver: true))
        let unknownHostStatus = ownedHost(status: .unknown, control: control(canTakeOver: true))

        XCTAssertTrue(MIFResourcePageRules.controlActions(for: unknown).isEmpty)
        XCTAssertTrue(MIFResourcePageRules.controlActions(for: unknownLease).isEmpty)
        XCTAssertTrue(MIFResourcePageRules.controlActions(for: transitioning).isEmpty)
        XCTAssertTrue(MIFResourcePageRules.controlActions(for: returning).isEmpty)
        XCTAssertTrue(MIFResourcePageRules.controlActions(for: failed).isEmpty)
        XCTAssertTrue(MIFResourcePageRules.controlActions(for: starting).isEmpty)
        XCTAssertTrue(MIFResourcePageRules.controlActions(for: stopped).isEmpty)
        XCTAssertTrue(MIFResourcePageRules.controlActions(for: unknownHostStatus).isEmpty)
    }

    func testRunningAIControlDoesNotOfferTakeoverInResourceList() {
        let approved = ownedHost(control: control(
            state: .ai, lease: .none, canTakeOver: true))
        let denied = ownedHost(control: control(
            state: .ai, lease: .none, canTakeOver: false))

        XCTAssertTrue(MIFResourcePageRules.controlActions(for: approved).isEmpty)
        XCTAssertTrue(MIFResourcePageRules.controlActions(for: denied).isEmpty)
    }

    func testLoadingOrInFlightControlSuppressesActions() {
        let loading = ownedHost(control: control(canReturnToAI: true, isLoading: true))
        let inFlight = ownedHost(control: control(canReturnToAI: true, isActionInFlight: true))

        XCTAssertTrue(MIFResourcePageRules.controlActions(for: loading).isEmpty)
        XCTAssertTrue(MIFResourcePageRules.controlActions(for: inFlight).isEmpty)
    }

    func testOwnedStableControlUsesOnlyClientApprovedActions() {
        let host = ownedHost(control: control(canReturnToAI: true))

        XCTAssertEqual(MIFResourcePageRules.controlActions(for: host), [.returnToAI])
    }

    func testExpiredLeaseOffersOnlyOwnerApprovedReturnAction() {
        let host = ownedHost(control: control(
            lease: .expired, canTakeOver: true, canReturnToAI: true))

        XCTAssertEqual(MIFResourcePageRules.controlActions(for: host), [.returnToAI])
    }

    func testUserControlWithoutActiveLeaseOnlyReturnsWhenOwnerPolicyAllowsIt() {
        for lease in [MIFResourcePageState.Control.Lease.none, .unknown] {
            let host = ownedHost(control: control(
                state: .user, lease: lease, canReturnToAI: true))

            XCTAssertTrue(MIFResourcePageRules.controlActions(for: host).isEmpty, "lease: \(lease)")
        }
    }

    private func ownedHost(
        status: MIFResourcePageState.Host.Status = .running,
        control: MIFResourcePageState.Control
    ) -> MIFResourcePageState.Host {
        MIFResourcePageState.Host(
            id: "h1234567", name: "Owned", scope: .owned, status: status,
            workspaceSummary: nil, canOpen: true, control: control, error: nil)
    }

    private func resourceState(isLoading: Bool, loadError: String?,
                               hostsState: MIFResourcePageState.ListState? = nil,
                               workspacesState: MIFResourcePageState.ListState? = nil) -> MIFResourcePageState {
        MIFResourcePageState(isLoading: isLoading, loadError: loadError,
                             hostsState: hostsState ?? .init(isLoading: isLoading, loadError: loadError),
                             workspacesState: workspacesState ?? .init(isLoading: isLoading, loadError: loadError),
                             defaultTarget: .unknown,
                             hosts: [], workspaces: [])
    }

    private func control(
        state: MIFResourcePageState.Control.State = .user,
        lease: MIFResourcePageState.Control.Lease = .active,
        canTakeOver: Bool = false,
        canReturnToAI: Bool = false,
        isTransitioning: Bool = false,
        isLoading: Bool = false,
        isActionInFlight: Bool = false
    ) -> MIFResourcePageState.Control {
        MIFResourcePageState.Control(
            state: state, lease: lease, isTransitioning: isTransitioning,
            isLoading: isLoading, isActionInFlight: isActionInFlight,
            canTakeOver: canTakeOver, canReturnToAI: canReturnToAI,
            error: nil)
    }
}
