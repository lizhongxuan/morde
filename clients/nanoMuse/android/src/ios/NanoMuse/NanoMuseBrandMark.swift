//
//  NanoMuseBrandMark.swift
//  nanoMuse
//
//  The app icon as a view, for the pages that stand for the app rather than
//  for the agent (docs/brand.md): the sign-in page of the first run. The
//  tile is the white squircle with the hairline edge and the N stroke in the
//  brand gradient — drawn from the same path as `assets/brand/nanomuse-mark.svg`
//  (tile coordinates, viewBox 0 0 100 100), so it needs no asset and matches
//  the icon on the home screen. The face stays the agent's; it is never used
//  where the app is meant.
//

import SwiftUI

/// The single pen stroke that reads as an N. Path from `nanomuse-mark.svg`, in a 100 × 100 tile.
struct NanoMuseMarkShape: Shape {
    func path(in rect: CGRect) -> Path {
        let s = min(rect.width, rect.height) / 100
        let ox = rect.minX + (rect.width - 100 * s) / 2
        let oy = rect.minY + (rect.height - 100 * s) / 2
        func p(_ x: CGFloat, _ y: CGFloat) -> CGPoint { CGPoint(x: ox + x * s, y: oy + y * s) }
        var path = Path()
        path.move(to: p(50.02, 79.14))
        path.addCurve(to: p(41.94, 68.06), control1: p(45.88, 78.24), control2: p(43.14, 74.48))
        path.addCurve(to: p(41.57, 48.4), control1: p(41.21, 64.11), control2: p(41.07, 57.03))
        path.addCurve(to: p(41.25, 38.95), control1: p(42.01, 40.73), control2: p(41.95, 38.95))
        path.addCurve(to: p(37.31, 42.85), control1: p(40.84, 38.95), control2: p(38.78, 40.99))
        path.addCurve(to: p(29.36, 53.81), control1: p(35.79, 44.78), control2: p(32.24, 49.66))
        path.addCurve(to: p(21.73, 63.62), control1: p(24.1, 61.36), control2: p(23.23, 62.5))
        path.addCurve(to: p(18.1, 64.89), control1: p(20.32, 64.69), control2: p(19.77, 64.88))
        path.addCurve(to: p(15.8, 64.51), control1: p(16.81, 64.9), control2: p(16.55, 64.85))
        path.addCurve(to: p(13.09, 61.7), control1: p(14.63, 63.95), control2: p(13.5, 62.79))
        path.addCurve(to: p(13.03, 56.4), control1: p(12.38, 59.79), control2: p(12.36, 58.4))
        path.addCurve(to: p(37.95, 24.43), control1: p(15.13, 50.07), control2: p(30.84, 29.92))
        path.addCurve(to: p(44.44, 21.8), control1: p(40.06, 22.8), control2: p(42.52, 21.8))
        path.addCurve(to: p(51.98, 26.02), control1: p(47.3, 21.8), control2: p(50.22, 23.43))
        path.addCurve(to: p(55.18, 47.82), control1: p(54.35, 29.48), control2: p(55.41, 36.71))
        path.addCurve(to: p(55.36, 63.2), control1: p(54.87, 62.95), control2: p(54.87, 63.03))
        path.addCurve(to: p(56.6, 62.19), control1: p(55.47, 63.23), control2: p(56.02, 62.78))
        path.addCurve(to: p(67.56, 47.75), control1: p(58.17, 60.58), control2: p(59.77, 58.46))
        path.addCurve(to: p(79.17, 34.51), control1: p(74.06, 38.79), control2: p(76.77, 35.7))
        path.addCurve(to: p(81.98, 33.94), control1: p(80.29, 33.94), control2: p(80.32, 33.94))
        path.addCurve(to: p(84.59, 34.39), control1: p(83.54, 33.95), control2: p(83.71, 33.97))
        path.addCurve(to: p(88.0, 38.44), control1: p(86.34, 35.22), control2: p(87.54, 36.65))
        path.addCurve(to: p(87.94, 42.08), control1: p(88.3, 39.61), control2: p(88.28, 40.72))
        path.addCurve(to: p(82.58, 50.7), control1: p(87.45, 43.98), control2: p(86.62, 45.31))
        path.addCurve(to: p(77.3, 57.82), control1: p(81.37, 52.32), control2: p(78.99, 55.53))
        path.addCurve(to: p(65.78, 72.03), control1: p(71.35, 65.91), control2: p(68.99, 68.81))
        path.addCurve(to: p(58.18, 77.81), control1: p(63.02, 74.79), control2: p(60.71, 76.55))
        path.addCurve(to: p(50.02, 79.14), control1: p(55.42, 79.19), control2: p(52.47, 79.67))
        path.closeSubpath()
        return path
    }
}

/// The icon tile: white squircle (27 % radius), hairline edge, the mark in the brand gradient.
struct NanoMuseBrandMark: View {
    var size: CGFloat = 72

    private var tile: RoundedRectangle {
        RoundedRectangle(cornerRadius: size * 0.27, style: .continuous)
    }

    var body: some View {
        ZStack {
            tile.fill(Color.white)
            NanoMuseMarkShape()
                .fill(LinearGradient(
                    colors: [Color(red: 0x01 / 255, green: 0x5C / 255, blue: 0xFB / 255), Color(red: 0x01 / 255, green: 0x86 / 255, blue: 0xFB / 255)],
                    startPoint: .leading,
                    endPoint: .trailing
                ))
        }
        .frame(width: size, height: size)
        .overlay(tile.stroke(Color(red: 0xD9 / 255, green: 0xD9 / 255, blue: 0xDE / 255), lineWidth: 1))
        .accessibilityHidden(true)
    }
}
