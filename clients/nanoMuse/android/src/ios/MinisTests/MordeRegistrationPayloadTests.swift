import XCTest
@testable import Minis

final class MordeRegistrationPayloadTests: XCTestCase {
    func testPayloadTrimsUsernameAndContainsOnlyNameAndPassword() throws {
        let payload = try XCTUnwrap(MordeRegistrationPayload.make(name: "  alice  ", password: "password123"))
        XCTAssertEqual(payload.jsonObject, ["name": "alice", "password": "password123"])
    }

    func testPayloadRejectsBlankUsernameOrPassword() {
        XCTAssertNil(MordeRegistrationPayload.make(name: " \n ", password: "password123"))
        XCTAssertNil(MordeRegistrationPayload.make(name: "alice", password: ""))
    }
}
