// Where the pixels come from: ScreenCaptureKit on macOS 14 and later, CoreGraphics before.
//
// `CGDisplayCreateImage` — what 0.1.38 and 0.1.39 used — returns nil on macOS 26/27 even with
// Screen Recording granted (`swift -e 'import CoreGraphics; print(CGDisplayCreateImage(CGMainDisplayID()) == nil)'`
// prints `true` there), and `CGWindowListCreateImage` is deprecated from 14. So on 14+ the
// display and a single window are taken the way UI-TARS-desktop takes them: the shareable
// content → the display (or the window by id) → an SCContentFilter → an SCStreamConfiguration
// at the source's pixel size → `SCScreenshotManager.captureImage`. SCK's calls are
// asynchronous; the HTTP handler is synchronous on its own serial queue, so each one is
// waited for with a semaphore (SCK answers on a queue of its own) and given ten seconds.
//
// The permission probe lives here too: on 14+ `SCShareableContent` throwing
// `SCStreamError.userDeclined` (or `.noDisplayList`) is the word that Screen Recording is
// not granted, where `CGPreflightScreenCaptureAccess` alone can say yes and the capture
// still fail. Errors are reported with SCK's own code name, so the log says `userDeclined`
// or `noDisplay…` rather than `noImage`.
//
// The deployment target is macOS 12.3 (build.sh), the first release with the framework:
// every SCK use is behind `#available(macOS 14.0, *)`, and the pre-14 branch keeps the
// CoreGraphics calls.

import AppKit
import CoreGraphics
import CoreVideo
import Foundation
import ScreenCaptureKit

/// One on-screen window as the capture layer lists it — what `/windows` answers, in the
/// shape the runtime's window mode reads (nanomuse/computer/mac_window.py, WindowInfo).
struct WindowEntry {
    let id: UInt32
    let pid: Int32
    let app: String
    let bundleID: String
    let title: String
    /// Points, top-left origin; the main display's corner is (0, 0).
    let frame: CGRect
    let layer: Int
    let onScreen: Bool

    var json: [String: Any] {
        [
            "id": Int(id),
            "pid": Int(pid),
            "app": app,
            "bundle_id": bundleID,
            "title": title,
            "bounds": [Double(frame.origin.x), Double(frame.origin.y), Double(frame.width), Double(frame.height)],
            "layer": layer,
            "on_screen": onScreen,
        ]
    }
}

enum ScreenCapture {
    /// What the capture layer says about Screen Recording, on top of the TCC preflight.
    enum Probe {
        case granted
        case denied(String)
        case unknown(String)
    }

    /// How long one ScreenCaptureKit call may take before it is reported as hung.
    fileprivate static let timeout: DispatchTimeInterval = .seconds(10)

    /// `"ScreenCaptureKit"` on macOS 14 and later, `"CoreGraphics"` before — for `/status`
    /// and the log, so a reader knows which path took the picture.
    static var source: String {
        if #available(macOS 14.0, *) {
            return "ScreenCaptureKit"
        }
        return "CoreGraphics"
    }

    // MARK: the display

    static func display(_ id: CGDirectDisplayID) throws -> CGImage {
        if #available(macOS 14.0, *) {
            let content = try SCK.content(excludingDesktopWindows: false, onScreenWindowsOnly: true)
            guard let display = content.displays.first(where: { $0.displayID == id }) else {
                throw Screenshot.Failure.capture("ScreenCaptureKit lists no display \(id) (\(content.displays.count) on the list)")
            }
            let filter = SCContentFilter(display: display, excludingWindows: [])
            let scale = SCK.scale(of: filter, displayID: id)
            let configuration = SCStreamConfiguration()
            configuration.width = max(1, Int((Double(display.width) * scale).rounded()))
            configuration.height = max(1, Int((Double(display.height) * scale).rounded()))
            configuration.showsCursor = true
            configuration.pixelFormat = kCVPixelFormatType_32BGRA
            configuration.captureResolution = .best
            return try SCK.capture(filter: filter, configuration: configuration)
        }
        guard let image = CGDisplayCreateImage(id) else { throw Screenshot.Failure.noImage }
        return image
    }

    // MARK: windows

    /// The windows on screen, front to back as the window server orders them.
    static func windows() throws -> [WindowEntry] {
        if #available(macOS 14.0, *) {
            let content = try SCK.content(excludingDesktopWindows: true, onScreenWindowsOnly: true)
            return content.windows.map { window in
                WindowEntry(
                    id: window.windowID,
                    pid: window.owningApplication?.processID ?? 0,
                    app: window.owningApplication?.applicationName ?? "",
                    bundleID: window.owningApplication?.bundleIdentifier ?? "",
                    title: window.title ?? "",
                    frame: window.frame,
                    layer: window.windowLayer,
                    onScreen: window.isOnScreen
                )
            }
        }
        return try legacyWindows()
    }

    /// One window's own pixels (no shadow) and its frame in screen points.
    static func window(_ id: UInt32) throws -> (CGImage, CGRect) {
        if #available(macOS 14.0, *) {
            let content = try SCK.content(excludingDesktopWindows: false, onScreenWindowsOnly: false)
            guard let window = content.windows.first(where: { $0.windowID == id }) else {
                throw Screenshot.Failure.noWindow(id)
            }
            let filter = SCContentFilter(desktopIndependentWindow: window)
            let frame = window.frame
            let scale = SCK.scale(of: filter, displayID: CGMainDisplayID())
            let configuration = SCStreamConfiguration()
            configuration.width = max(1, Int((Double(frame.width) * scale).rounded()))
            configuration.height = max(1, Int((Double(frame.height) * scale).rounded()))
            // the pointer is the person's in window mode; the picture is the window alone
            configuration.showsCursor = false
            configuration.ignoreShadowsSingleWindow = true
            configuration.pixelFormat = kCVPixelFormatType_32BGRA
            configuration.captureResolution = .best
            return (try SCK.capture(filter: filter, configuration: configuration), frame)
        }
        guard let entry = try legacyWindows().first(where: { $0.id == id }) else { throw Screenshot.Failure.noWindow(id) }
        guard let image = CGWindowListCreateImage(.null, .optionIncludingWindow, id, [.boundsIgnoreFraming, .bestResolution]), image.width > 0 else {
            throw Screenshot.Failure.noImage
        }
        return (image, entry.frame)
    }

    // MARK: the permission

    /// Whether the capture layer agrees that Screen Recording is granted. Called only after
    /// `CGPreflightScreenCaptureAccess` said yes — a `SCShareableContent` request from a
    /// process TCC has not decided on would show the system's dialog, and that is `/request`'s
    /// job, not `/status`'s. Before macOS 14 the preflight is the whole answer.
    static func probe() -> Probe {
        if #available(macOS 14.0, *) {
            do {
                _ = try SCK.content(excludingDesktopWindows: true, onScreenWindowsOnly: true)
                return .granted
            } catch {
                let text = describe(error)
                return isDenied(error) ? .denied(text) : .unknown(text)
            }
        }
        return .granted
    }

    /// Whether an error from the capture layer means "Screen Recording is not granted".
    static func isDenied(_ error: Error) -> Bool {
        if #available(macOS 14.0, *), let sck = error as? SCStreamError {
            switch sck.code {
            case .userDeclined, .noDisplayList:
                return true
            default:
                return false
            }
        }
        return false
    }

    /// An error in words a log reader can act on: SCK's code by name, then the system's text.
    static func describe(_ error: Error) -> String {
        if #available(macOS 14.0, *), let sck = error as? SCStreamError {
            return "ScreenCaptureKit \(SCK.name(sck.code)) (\(sck.code.rawValue)): \(sck.localizedDescription)"
        }
        if let failure = error as? Screenshot.Failure {
            return failure.description
        }
        return "\(error)"
    }

    // MARK: CoreGraphics (macOS 12 and 13)

    private static func legacyWindows() throws -> [WindowEntry] {
        let options: CGWindowListOption = [.optionOnScreenOnly, .excludeDesktopElements]
        guard let list = CGWindowListCopyWindowInfo(options, kCGNullWindowID) as? [[String: Any]] else {
            throw Screenshot.Failure.capture("the window server returned no window list")
        }
        var bundles: [Int32: String] = [:]
        return list.compactMap { info in
            guard let id = info[kCGWindowNumber as String] as? UInt32 else { return nil }
            let pid = Int32(truncatingIfNeeded: (info[kCGWindowOwnerPID as String] as? Int) ?? 0)
            let bounds = (info[kCGWindowBounds as String] as? [String: Any]).flatMap { CGRect(dictionaryRepresentation: $0 as CFDictionary) } ?? .zero
            if bundles[pid] == nil {
                bundles[pid] = NSRunningApplication(processIdentifier: pid)?.bundleIdentifier ?? ""
            }
            return WindowEntry(
                id: id,
                pid: pid,
                app: info[kCGWindowOwnerName as String] as? String ?? "",
                bundleID: bundles[pid] ?? "",
                title: info[kCGWindowName as String] as? String ?? "",
                frame: bounds,
                layer: info[kCGWindowLayer as String] as? Int ?? 0,
                onScreen: (info[kCGWindowIsOnscreen as String] as? Bool) ?? true
            )
        }
    }
}

/// A result handed from ScreenCaptureKit's queue back to the waiting handler.
private final class Handoff<T>: @unchecked Sendable {
    var result: Result<T, Error>?
}

@available(macOS 14.0, *)
private enum SCK {
    static func content(excludingDesktopWindows: Bool, onScreenWindowsOnly: Bool) throws -> SCShareableContent {
        let handoff = Handoff<SCShareableContent>()
        let done = DispatchSemaphore(value: 0)
        SCShareableContent.getExcludingDesktopWindows(excludingDesktopWindows, onScreenWindowsOnly: onScreenWindowsOnly) { content, error in
            if let content = content {
                handoff.result = .success(content)
            } else {
                handoff.result = .failure(error ?? Screenshot.Failure.capture("ScreenCaptureKit returned neither content nor an error"))
            }
            done.signal()
        }
        guard done.wait(timeout: .now() + ScreenCapture.timeout) == .success, let result = handoff.result else {
            throw Screenshot.Failure.capture("ScreenCaptureKit did not list the shareable content within 10 s")
        }
        return try result.get()
    }

    static func capture(filter: SCContentFilter, configuration: SCStreamConfiguration) throws -> CGImage {
        let handoff = Handoff<CGImage>()
        let done = DispatchSemaphore(value: 0)
        SCScreenshotManager.captureImage(contentFilter: filter, configuration: configuration) { image, error in
            if let image = image {
                handoff.result = .success(image)
            } else {
                handoff.result = .failure(error ?? Screenshot.Failure.noImage)
            }
            done.signal()
        }
        guard done.wait(timeout: .now() + ScreenCapture.timeout) == .success, let result = handoff.result else {
            throw Screenshot.Failure.capture("ScreenCaptureKit did not deliver the picture within 10 s")
        }
        return try result.get()
    }

    /// Pixels per point of what the filter captures (`pointPixelScale`), or the display mode's
    /// ratio when SCK does not say, or 1.
    static func scale(of filter: SCContentFilter, displayID: CGDirectDisplayID) -> Double {
        let reported = Double(filter.pointPixelScale)
        if reported > 0 {
            return reported
        }
        let bounds = CGDisplayBounds(displayID)
        if let mode = CGDisplayCopyDisplayMode(displayID), bounds.width > 0 {
            return Double(mode.pixelWidth) / Double(bounds.width)
        }
        return 1
    }

    static func name(_ code: SCStreamError.Code) -> String {
        switch code {
        case .userDeclined: return "userDeclined"
        case .failedToStart: return "failedToStart"
        case .missingEntitlements: return "missingEntitlements"
        case .failedApplicationConnectionInvalid: return "failedApplicationConnectionInvalid"
        case .failedApplicationConnectionInterrupted: return "failedApplicationConnectionInterrupted"
        case .failedNoMatchingApplicationContext: return "failedNoMatchingApplicationContext"
        case .attemptToStartStreamState: return "attemptToStartStreamState"
        case .attemptToStopStreamState: return "attemptToStopStreamState"
        case .attemptToUpdateFilterState: return "attemptToUpdateFilterState"
        case .attemptToConfigState: return "attemptToConfigState"
        case .internalError: return "internalError"
        case .invalidParameter: return "invalidParameter"
        case .noWindowList: return "noWindowList"
        case .noDisplayList: return "noDisplayList"
        case .noCaptureSource: return "noCaptureSource"
        case .removingStream: return "removingStream"
        default: return "code \(code.rawValue)"
        }
    }
}