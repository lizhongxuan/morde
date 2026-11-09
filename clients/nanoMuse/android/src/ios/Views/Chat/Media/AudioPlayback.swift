//
//  AudioPlayback.swift
//  MinisApp
//
//  Global audio player singleton + floating PiP capsule + inline audio
//  bubble. Extracted from AIChatView.swift.
//

import AVFoundation
import Combine
import SwiftUI
import UIKit

// MARK: - Floating Audio PiP Capsule

struct AudioPiPCapsule: View {
    @ObservedObject private var player = GlobalAudioPlayer.shared
    /// Persisted position of the capsule center.
    @State private var position: CGPoint? = nil
    @State private var dragOffset: CGSize = .zero

    private let capsuleWidth: CGFloat = 160
    private let capsuleHeight: CGFloat = 40

    /// Default position: bottom-trailing, 10pt above the Tool Status Bar area.
    private func defaultPosition(in size: CGSize) -> CGPoint {
        CGPoint(
            x: size.width - capsuleWidth / 2 - 10,
            y: size.height - capsuleHeight / 2 - 165
        )
    }

    /// Clamp the position within the container bounds with padding.
    private func clamp(_ point: CGPoint, in size: CGSize) -> CGPoint {
        let margin: CGFloat = 6
        let minX = capsuleWidth / 2 + margin
        let maxX = size.width - capsuleWidth / 2 - margin
        let minY = capsuleHeight / 2 + margin
        let maxY = size.height - capsuleHeight / 2 - margin
        return CGPoint(
            x: max(minX, min(maxX, point.x)),
            y: max(minY, min(maxY, point.y))
        )
    }

    var body: some View {
        GeometryReader { geo in
            if player.isPiPActive && !player.showFullPreview {
                let current = position ?? defaultPosition(in: geo.size)
                capsuleContent
                    .position(
                        x: current.x + dragOffset.width,
                        y: current.y + dragOffset.height
                    )
                    .highPriorityGesture(
                        DragGesture(minimumDistance: 5)
                            .onChanged { dragOffset = $0.translation }
                            .onEnded { value in
                                let raw = CGPoint(
                                    x: current.x + value.translation.width,
                                    y: current.y + value.translation.height
                                )
                                withAnimation(.spring(response: 0.3)) {
                                    position = clamp(raw, in: geo.size)
                                    dragOffset = .zero
                                }
                            }
                    )
                    .transition(.move(edge: .bottom).combined(with: .opacity))
                    .sheet(isPresented: $player.showFullPreview) {
                        if let url = player.activeFileURL {
                            MinisAudioPreviewView(fileURL: url)
                                .presentationDetents([.large])
                                .presentationDragIndicator(.hidden)
                        }
                    }
                    .onAppear {
                        if position == nil {
                            position = defaultPosition(in: geo.size)
                        }
                    }
            }
        }
    }

    private var capsuleContent: some View {
        HStack {
            Button {
                player.showFullPreview = true
            } label: {
                Image(systemName: "waveform")
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
                    .frame(width: 36, height: 40)
            }
            .buttonStyle(.plain)

            Spacer()

            Button {
                player.togglePlayPause()
            } label: {
                Image(systemName: player.isPlaying ? "pause.fill" : "play.fill")
                    .font(.title3.weight(.semibold))
                    .foregroundStyle(.primary)
                    .frame(width: 36, height: 40)
            }
            .buttonStyle(.plain)

            Spacer()

            Button {
                player.stop()
            } label: {
                Image(systemName: "xmark")
                    .font(.caption.weight(.bold))
                    .foregroundStyle(.secondary)
                    .frame(width: 36, height: 40)
            }
            .buttonStyle(.plain)
        }
        .padding(.horizontal, 16)
        .frame(width: 160, height: 40)
        .background {
            Capsule()
                .fill(.ultraThinMaterial)
                .overlay(
                    Capsule()
                        .fill(
                            LinearGradient(
                                colors: [
                                    Color.white.opacity(0.15),
                                    Color.white.opacity(0.05),
                                    Color.clear
                                ],
                                startPoint: .top,
                                endPoint: .bottom
                            )
                        )
                )
                .opacity(0.85)
        }
        .clipShape(Capsule())
        .shadow(color: .black.opacity(0.2), radius: 10, y: 4)
    }
}

// MARK: - Inline Audio Bubble

struct MinisAudioPlayerView: View {
    let url: URL
    let fileURL: URL
    @ObservedObject private var player = GlobalAudioPlayer.shared
    @State private var fileReady = false
    @State private var showPreview = false

    /// Fixed height for both placeholder and controls to prevent layout jumps.
    private let controlsHeight: CGFloat = 70

    /// Whether this file is the one currently active in the global player.
    private var isActive: Bool { player.isActive(url: fileURL) }

    var body: some View {
        if fileReady {
            audioControls
        } else {
            placeholder
                .task { await waitForFile() }
        }
    }

    private var audioControls: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 4) {
                Image(systemName: "waveform")
                    .font(.caption2)
                Text(url.lastPathComponent)
                    .lineLimit(1)
                    .font(.caption)
                Spacer()
                Button {
                    showPreview = true
                } label: {
                    Image(systemName: "arrow.up.left.and.arrow.down.right")
                        .font(.caption2)
                        .foregroundColor(ChatColors.secondaryText)
                }
                .buttonStyle(.plain)
            }
            .foregroundColor(ChatColors.secondaryText)

            HStack(spacing: 10) {
                Button {
                    if isActive {
                        player.togglePlayPause()
                    } else {
                        player.play(url: fileURL)
                    }
                } label: {
                    Image(systemName: isActive && player.isPlaying ? "pause.circle.fill" : "play.circle.fill")
                        .font(.title2)
                        .foregroundColor(.accentColor)
                }
                .buttonStyle(.plain)

                if isActive && player.isLoaded {
                    Slider(
                        value: Binding(
                            get: { player.currentTime },
                            set: { player.debouncedSeek(to: $0) }
                        ),
                        in: 0...max(player.duration, 0.01)
                    )
                    .tint(.accentColor)

                    Text(formatTime(player.duration))
                        .font(.caption2)
                        .foregroundColor(ChatColors.secondaryText)
                        .monospacedDigit()
                } else {
                    // Idle state — show empty slider
                    Slider(value: .constant(0), in: 0...1)
                        .tint(.accentColor)
                        .disabled(true)

                    Text(formatTime(0))
                        .font(.caption2)
                        .foregroundColor(ChatColors.secondaryText)
                        .monospacedDigit()
                }
            }
        }
        .padding(10)
        .frame(maxWidth: 320, minHeight: controlsHeight)
        .background(ChatColors.secondaryBg)
        .clipShape(RoundedRectangle(cornerRadius: 10))
        .sheet(isPresented: $showPreview) {
            MinisAudioPreviewView(fileURL: fileURL)
                .presentationDetents([.large])
                .presentationDragIndicator(.hidden)
        }
    }

    private var placeholder: some View {
        HStack(spacing: 6) {
            ProgressView().controlSize(.small)
            Text(url.lastPathComponent).lineLimit(1)
        }
        .font(.caption)
        .foregroundColor(ChatColors.secondaryText)
        .padding(10)
        .frame(maxWidth: 320, minHeight: controlsHeight)
        .background(ChatColors.secondaryBg)
        .clipShape(RoundedRectangle(cornerRadius: 8))
    }

    private func waitForFile() async {
        for _ in 0..<12 {
            if FileManager.default.fileExists(atPath: fileURL.path) {
                fileReady = true
                return
            }
            try? await Task.sleep(nanoseconds: 500_000_000)
        }
        fileReady = true
    }

    private func formatTime(_ t: TimeInterval) -> String {
        let m = Int(t) / 60
        let s = Int(t) % 60
        return String(format: "%d:%02d", m, s)
    }
}
