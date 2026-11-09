import UIKit
import XCTest
@testable import Minis

/// The emoji look another device set (#435): the profile's `#rrggbb` as a colour with the
/// desktop's fallbacks, and the order the face is settled in (drawn, then emoji, then dragon).
final class NanoMuseEmojiLookTests: XCTestCase {

    private func rgb(_ color: UIColor) -> (Int, Int, Int) {
        var r: CGFloat = 0, g: CGFloat = 0, b: CGFloat = 0, a: CGFloat = 0
        color.getRed(&r, green: &g, blue: &b, alpha: &a)
        return (Int((r * 255).rounded()), Int((g * 255).rounded()), Int((b * 255).rounded()))
    }

    func testHexColourParsesEitherCaseWithOrWithoutTheHash() {
        XCTAssertTrue(rgb(NanoMuseEmojiLook.color(hex: "#7c3aed")) == (0x7c, 0x3a, 0xed))
        XCTAssertTrue(rgb(NanoMuseEmojiLook.color(hex: "#DC2626")) == (0xdc, 0x26, 0x26))
        XCTAssertTrue(rgb(NanoMuseEmojiLook.color(hex: "059669")) == (0x05, 0x96, 0x69))
        XCTAssertTrue(rgb(NanoMuseEmojiLook.color(hex: " #4b5563 ")) == (0x4b, 0x55, 0x63))
    }

    func testBadOrMissingColourIsTheActionBlue() {
        let blue = rgb(NanoMuseEmojiLook.color(hex: NanoMuseEmojiLook.defaultColor))
        XCTAssertTrue(blue == (0x00, 0x64, 0xd4))
        for bad in ["", "#", "#fff", "#12345g", "blue", "#0064d4ff"] {
            XCTAssertTrue(rgb(NanoMuseEmojiLook.color(hex: bad)) == blue, bad)
        }
        XCTAssertTrue(rgb(NanoMuseEmojiLook(emoji: "", color: "").uiColor) == blue)
    }

    func testEmptyEmojiShowsTheSparkle() {
        XCTAssertEqual(NanoMuseEmojiLook(emoji: "", color: "").shownEmoji, "✨")
        XCTAssertEqual(NanoMuseEmojiLook(emoji: "  ", color: "").shownEmoji, "✨")
        XCTAssertEqual(NanoMuseEmojiLook(emoji: "🦊", color: "#0891b2").shownEmoji, "🦊")
    }

    func testTheDrawnFaceComesBeforeTheEmojiAndTheEmojiBeforeTheDragon() {
        let look = NanoMuseEmojiLook(emoji: "🦉", color: "#059669")
        XCTAssertEqual(NanoMuseFaceSource.of(hasDrawn: true, emoji: look), .drawn)
        XCTAssertEqual(NanoMuseFaceSource.of(hasDrawn: true, emoji: nil), .drawn)
        XCTAssertEqual(NanoMuseFaceSource.of(hasDrawn: false, emoji: look), .emoji)
        XCTAssertEqual(NanoMuseFaceSource.of(hasDrawn: false, emoji: nil), .dragon)
    }

    func testTheLookRoundTripsThroughJSON() throws {
        let look = NanoMuseEmojiLook(emoji: "🪐", color: "#d97706")
        let data = try JSONEncoder().encode(look)
        XCTAssertEqual(try JSONDecoder().decode(NanoMuseEmojiLook.self, from: data), look)
    }
}
