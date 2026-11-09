import SwiftUI
import XCTest
@testable import Minis

/// 0.1.39: the shape of the chat's body. Build 9 (0.1.38) overflowed the main thread's stack
/// on an iPad the moment the chat appeared after onboarding — in `AIChatView.body.getter`,
/// while the runtime instantiated the metadata of the body's type (NanoMuseChatModifiers.swift
/// has the story). The two numbers below are what grew: how much of the stack one copy of the
/// body's value takes, and how deep its generic type nests. Neither is read by anyone at run
/// time; both are read here so the next link added to `AIChatView.body` is noticed before an
/// iPad notices it.
@MainActor
final class NanoMuseRound6Tests: XCTestCase {

    /// One copy of the body's value, in bytes. The getter builds the value link by link and
    /// keeps copies of the way there on the stack, so the stack it needs grows faster than
    /// this; 0.1.38's value overflowed the 1 MB the main thread has on a device.
    static let bodyBytesCeiling = 12_288
    /// How deep the body's type nests (`ModifiedContent<ModifiedContent<…>>`): the Swift
    /// runtime's type decoder recurses once per level when the metadata is first asked for.
    static let bodyNestingCeiling = 72

    /// How deep the generic nesting of a type name goes (`A<B<C>>` is 2).
    nonisolated static func nesting(of typeName: String) -> Int {
        var depth = 0
        var deepest = 0
        for character in typeName {
            if character == "<" {
                depth += 1
                deepest = max(deepest, depth)
            } else if character == ">" {
                depth -= 1
            }
        }
        return deepest
    }

    func testNestingCountsAngleBrackets() {
        XCTAssertEqual(Self.nesting(of: "Int"), 0)
        XCTAssertEqual(Self.nesting(of: "ModifiedContent<Text, _PaddingLayout>"), 1)
        XCTAssertEqual(Self.nesting(of: "A<B<C>, D<E<F>>>"), 3)
        XCTAssertEqual(Self.nesting(of: "Optional<ModifiedContent<VStack<TupleView<(Text, Text)>>, _PaddingLayout>>"), 4)
    }

    /// `AIChatView.Body` is the opaque type behind `var body: some View`; naming it instantiates
    /// its metadata the way the first body evaluation does — here with the test process's 8 MB
    /// stack, so a chain that would overflow a phone still measures instead of crashing.
    func testChatBodyStaysSmallAndShallow() {
        let bytes = MemoryLayout<AIChatView.Body>.size
        let depth = Self.nesting(of: _typeName(AIChatView.Body.self, qualified: false))
        XCTAssertLessThanOrEqual(bytes, Self.bodyBytesCeiling, "AIChatView.body's value is \(bytes) bytes; the getter's stack frame grows with every link of the chain — add behaviour to NanoMuseChatHooks (or a modifier of its own), not as another link on the body")
        XCTAssertLessThanOrEqual(depth, Self.bodyNestingCeiling, "AIChatView.body's type nests \(depth) levels deep; the runtime recurses once per level — group modifiers rather than chaining them")
        // the composer stack is boxed (NanoMuseComposerHost takes an AnyView), so it must not show up
        // as a subtree of the body's type at all
        XCTAssertFalse(_typeName(AIChatView.Body.self, qualified: false).contains("NanoMuseChatCardsHost"), "the composer stack is inside the body's type again — it must stay an AnyView")
    }

    /// The two modifiers are one link each on the chat's chain; what they hold stays in their
    /// own bodies, which are short.
    func testTheGroupedLinksAreShortThemselves() {
        XCTAssertLessThanOrEqual(Self.nesting(of: _typeName(NanoMuseComposerHost.Body.self, qualified: false)), 6)
        XCTAssertLessThanOrEqual(Self.nesting(of: _typeName(NanoMuseChatHooks.Body.self, qualified: false)), 8)
        XCTAssertTrue(_typeName(NanoMuseComposerHost.Body.self, qualified: false).contains("AnyView"), "the stack arrives boxed")
    }
}
