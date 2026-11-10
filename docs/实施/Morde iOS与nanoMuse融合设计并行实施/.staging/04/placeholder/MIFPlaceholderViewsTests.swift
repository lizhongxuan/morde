import XCTest
@testable import Minis

final class MIFPlaceholderViewsTests: XCTestCase {
    func testFriendsPlaceholderIsStaticAndAccessible() {
        let kind = MIFPlaceholderPage.Kind.friends

        XCTAssertEqual(kind.title, AppLocalized("Friends"))
        XCTAssertEqual(kind.accessibilityIdentifier, "morde.placeholder.friends")
    }

    func testToolChatsPlaceholderIsStaticAndAccessible() {
        let kind = MIFPlaceholderPage.Kind.toolChats

        XCTAssertEqual(kind.title, AppLocalized("AI tool conversations"))
        XCTAssertEqual(kind.accessibilityIdentifier, "morde.placeholder.toolChats")
    }
}
