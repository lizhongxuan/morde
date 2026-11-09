import XCTest
@testable import Minis

@MainActor
final class NanoMuseMordeProviderBoundaryTests: XCTestCase {
    // nanoMuse: A stale provider slot must not restore a direct route in Morde.
    func testDirectImageRouteDoesNotResolveLegacyProviderWhenDisabled() {
        XCTAssertFalse(MordeLocalExecutionPolicy.isEnabled)
        XCTAssertEqual(NanoMuseImageGen.route(), .unavailable)
    }
}
