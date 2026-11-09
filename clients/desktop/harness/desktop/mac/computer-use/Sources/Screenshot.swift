// The picture of the main display, or of one window, scaled down in a CGContext and
// encoded with ImageIO through NSBitmapImageRep.
//
// Where the pixels come from (ScreenCapture.swift): ScreenCaptureKit on macOS 14 and later —
// `CGDisplayCreateImage` returns nil on macOS 26/27 even with Screen Recording granted, and
// `CGWindowListCreateImage` is deprecated from 14 — and the CoreGraphics calls below that.
// Everything else here (the exact size or the pixel budget, JPEG or PNG, the black check)
// is the same whichever source the picture came from.

import AppKit
import CoreGraphics
import Foundation

struct Shot {
    let data: Data
    let mime: String
    let width: Int
    let height: Int
    /// What the picture stands for, in points: the display, or the window's frame.
    let screenWidth: Int
    let screenHeight: Int
    /// Physical pixels per point of the source.
    let scale: Double
    /// The top-left of a window's frame in screen points (0,0 for the display).
    let originX: Int
    let originY: Int
}

enum Screenshot {
    enum Failure: Error, CustomStringConvertible {
        case noImage
        case black
        case encoding
        /// The capture layer failed in a way of its own, in words (ScreenCapture).
        case capture(String)
        case noWindow(UInt32)

        var description: String {
            switch self {
            case .noImage: return "no image came back from the capture"
            case .black: return "the picture is black"
            case .encoding: return "the picture could not be encoded"
            case .capture(let text): return text
            case .noWindow(let id): return "no window with id \(id) is on screen"
            }
        }
    }

    /// A channel below this in every sampled pixel means a black picture.
    private static let blackLevel: UInt8 = 8

    /// The main display: exactly `width`×`height` when both are given; otherwise the display's
    /// pixels, scaled down to at most `maxPixels` (the model never needs more than about 2 Mpx).
    static func take(width: Int?, height: Int?, maxPixels: Int, format: String, quality: Double) throws -> Shot {
        let display = CGMainDisplayID()
        let image = try ScreenCapture.display(display)
        if isBlack(image) { throw Failure.black }
        let bounds = CGDisplayBounds(display)
        let scale = bounds.width > 0 ? Double(image.width) / Double(bounds.width) : 1
        return try encode(image, width: width, height: height, maxPixels: maxPixels, format: format, quality: quality, screenWidth: Int(bounds.width), screenHeight: Int(bounds.height), scale: scale, originX: 0, originY: 0)
    }

    /// One window by its id (what `/windows` listed), its own pixels only: the window's
    /// pixels, scaled down to at most `maxPixels`. The frame comes back with the picture so
    /// the runtime can map a pixel of it to a point on the screen.
    static func window(id: UInt32, maxPixels: Int, format: String, quality: Double) throws -> Shot {
        let (image, frame) = try ScreenCapture.window(id)
        let scale = frame.width > 0 ? Double(image.width) / Double(frame.width) : 1
        return try encode(image, width: nil, height: nil, maxPixels: maxPixels, format: format, quality: quality, screenWidth: Int(frame.width), screenHeight: Int(frame.height), scale: scale, originX: Int(frame.origin.x), originY: Int(frame.origin.y))
    }

    private static func encode(_ image: CGImage, width: Int?, height: Int?, maxPixels: Int, format: String, quality: Double, screenWidth: Int, screenHeight: Int, scale: Double, originX: Int, originY: Int) throws -> Shot {
        var targetWidth = image.width
        var targetHeight = image.height
        if let width = width, let height = height, width > 0, height > 0 {
            targetWidth = width
            targetHeight = height
        } else if maxPixels > 0 && image.width * image.height > maxPixels {
            let factor = (Double(maxPixels) / Double(image.width * image.height)).squareRoot()
            targetWidth = max(1, Int(Double(image.width) * factor))
            targetHeight = max(1, Int(Double(image.height) * factor))
        }
        let scaled = try targetWidth == image.width && targetHeight == image.height ? image : resize(image, width: targetWidth, height: targetHeight)
        let representation = NSBitmapImageRep(cgImage: scaled)
        let png = format == "png"
        let properties: [NSBitmapImageRep.PropertyKey: Any] = png ? [:] : [.compressionFactor: quality]
        guard let data = representation.representation(using: png ? .png : .jpeg, properties: properties) else { throw Failure.encoding }
        return Shot(data: data, mime: png ? "image/png" : "image/jpeg", width: scaled.width, height: scaled.height, screenWidth: screenWidth, screenHeight: screenHeight, scale: scale, originX: originX, originY: originY)
    }

    private static func resize(_ image: CGImage, width: Int, height: Int) throws -> CGImage {
        guard let context = CGContext(data: nil, width: width, height: height, bitsPerComponent: 8, bytesPerRow: 0, space: CGColorSpaceCreateDeviceRGB(), bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue) else {
            throw Failure.encoding
        }
        context.interpolationQuality = .high
        context.draw(image, in: CGRect(x: 0, y: 0, width: width, height: height))
        guard let scaled = context.makeImage() else { throw Failure.encoding }
        return scaled
    }

    /// Whether the picture is black all over, judged on a 64×36 sample of it (what a process
    /// without Screen Recording can get from some capture paths).
    static func isBlack(_ image: CGImage) -> Bool {
        let width = 64
        let height = 36
        guard let context = CGContext(data: nil, width: width, height: height, bitsPerComponent: 8, bytesPerRow: width * 4, space: CGColorSpaceCreateDeviceRGB(), bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue) else {
            return false
        }
        context.interpolationQuality = .low
        context.draw(image, in: CGRect(x: 0, y: 0, width: width, height: height))
        guard let pixels = context.data else { return false }
        let bytes = pixels.assumingMemoryBound(to: UInt8.self)
        for offset in stride(from: 0, to: width * height * 4, by: 4) {
            if bytes[offset] >= blackLevel || bytes[offset + 1] >= blackLevel || bytes[offset + 2] >= blackLevel {
                return false
            }
        }
        return true
    }
}
