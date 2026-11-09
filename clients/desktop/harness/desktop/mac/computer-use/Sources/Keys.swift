// The runtime's key names → virtual key codes of the US layout (Carbon's kVK_* values), and
// the modifiers as CGEvent flags. The same names the Electron operator accepts
// (src/operator.ts `hotkeys`): `ctrl` is ⌘ on a Mac, where the model's ctrl+c means copy;
// `control` is the Control key itself.

import CoreGraphics
import Foundation

struct Key {
    let code: CGKeyCode
    /// Set for a modifier key; nil for an ordinary key.
    let modifier: CGEventFlags?
}

enum Keys {
    enum Failure: Error {
        case unknown(String)
    }

    static let command: CGKeyCode = 55
    static let shift: CGKeyCode = 56
    static let option: CGKeyCode = 58
    static let control: CGKeyCode = 59
    static let returnKey: CGKeyCode = 36
    static let tab: CGKeyCode = 48

    private static let modifiers: [String: (CGKeyCode, CGEventFlags)] = [
        "cmd": (command, .maskCommand),
        "command": (command, .maskCommand),
        "meta": (command, .maskCommand),
        "win": (command, .maskCommand),
        "super": (command, .maskCommand),
        "ctrl": (command, .maskCommand),
        "control": (control, .maskControl),
        "shift": (shift, .maskShift),
        "alt": (option, .maskAlternate),
        "option": (option, .maskAlternate),
    ]

    private static let codes: [String: CGKeyCode] = [
        "a": 0, "s": 1, "d": 2, "f": 3, "h": 4, "g": 5, "z": 6, "x": 7, "c": 8, "v": 9,
        "b": 11, "q": 12, "w": 13, "e": 14, "r": 15, "y": 16, "t": 17,
        "1": 18, "2": 19, "3": 20, "4": 21, "6": 22, "5": 23, "=": 24, "9": 25, "7": 26,
        "-": 27, "8": 28, "0": 29, "]": 30, "o": 31, "u": 32, "[": 33, "i": 34, "p": 35,
        "return": 36, "enter": 36, "l": 37, "j": 38, "'": 39, "k": 40, ";": 41, "\\": 42,
        ",": 43, "/": 44, "n": 45, "m": 46, ".": 47, "tab": 48, "space": 49, "`": 50,
        "backspace": 51, "esc": 53, "escape": 53,
        "f5": 96, "f6": 97, "f7": 98, "f3": 99, "f8": 100, "f9": 101, "f11": 103, "f13": 105,
        "f14": 107, "f10": 109, "f12": 111, "f15": 113,
        "insert": 114, "help": 114, "home": 115, "pageup": 116, "page_up": 116, "page up": 116,
        "del": 117, "delete": 117, "f4": 118, "end": 119, "f2": 120,
        "pagedown": 121, "page_down": 121, "page down": 121, "f1": 122,
        "left": 123, "arrowleft": 123, "right": 124, "arrowright": 124,
        "down": 125, "arrowdown": 125, "up": 126, "arrowup": 126,
        "capslock": 57,
    ]

    /// The key for one of the runtime's names (case-insensitive; `num3` means `3`).
    static func lookup(_ raw: String) throws -> Key {
        var name = raw.trimmingCharacters(in: .whitespaces).lowercased()
        if name.hasPrefix("num"), name.count == 4 {
            name = String(name.dropFirst(3))
        }
        if let modifier = modifiers[name] {
            return Key(code: modifier.0, modifier: modifier.1)
        }
        if let code = codes[name] {
            return Key(code: code, modifier: nil)
        }
        throw Failure.unknown(raw)
    }

    /// The modifier keys to hold for a set of flags, in the order to press them.
    static func modifierCodes(_ flags: CGEventFlags) -> [CGKeyCode] {
        var out: [CGKeyCode] = []
        if flags.contains(.maskCommand) { out.append(command) }
        if flags.contains(.maskControl) { out.append(control) }
        if flags.contains(.maskAlternate) { out.append(option) }
        if flags.contains(.maskShift) { out.append(shift) }
        return out
    }
}
