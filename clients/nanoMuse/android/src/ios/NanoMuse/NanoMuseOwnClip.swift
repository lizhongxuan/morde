//
//  NanoMuseOwnClip.swift
//  nanoMuse
//
//  A clip of the person's own as the face (#324). One short video from the
//  photo library plays muted in a loop wherever the face moves: for the mood
//  at rest, and for every other mood too, since there is only the one clip.
//  The stills keep their place where a still is shown: a drawn face's mood
//  pictures sit under the clip until its first frame is on screen; with no
//  drawn face the clip's own first frame is the still.
//
//  The clip lives next to the stills as `avatar/own-clip.mp4` with its first
//  frame in `avatar/own-clip.png`, and nowhere else: it is never part of the
//  profile the relay holds (NanoMuseProfileSync reads NanoMuseFaceStore only)
//  and does not follow the account to another device. It survives a new drawn
//  face and a reset to the dragon; only "Default face" removes it.
//  Android: avatar/OwnClip.kt, ui/avatar/OwnClipChooser.kt.
//

import AVFoundation
import PhotosUI
import SwiftUI
import UIKit
import UniformTypeIdentifiers

@MainActor
final class NanoMuseOwnClip: ObservableObject {
    static let shared = NanoMuseOwnClip()

    /// The longest clip taken, in whole seconds; a clip that rounds down to this is fine.
    nonisolated static let maxSeconds = 15
    /// The largest file taken.
    nonisolated static let maxBytes: Int64 = 30 * 1024 * 1024
    /// The video codecs the system player decodes here, as the four-character codes of the format
    /// description: H.264 in both of its sample entries (Android takes the one MIME type
    /// `video/avc`, the desktop `avc1` and `avc3`) and HEVC.
    nonisolated static let codecs: Set<String> = ["avc1", "avc3", "hvc1", "hev1"]
    /// The containers taken: MP4 and QuickTime (MOV), as uniform type identifiers.
    nonisolated static let containers: Set<String> = [UTType.mpeg4Movie.identifier, UTType.quickTimeMovie.identifier]

    enum Refusal: Equatable {
        case format, tooLong, tooLarge
    }

    /// What a look at a picked file found; a nil field is one that could not be read.
    struct Probe: Equatable {
        var container: String?
        var codec: String?
        var durationMs: Int64?
        var bytes: Int64?
    }

    /// A clip that passed the checks and was copied in, waiting for "Use" or "Cancel".
    struct Pending: Identifiable {
        let id = UUID()
        let url: URL
        let poster: UIImage?
        let durationMs: Int64
        let bytes: Int64
    }

    enum Outcome {
        case ready(Pending)
        case refused(Refusal, Probe)
        case failed(String)
    }

    /// The rule, apart from the phone: the format first (an unreadable file is a format problem,
    /// whatever its size), then the length, then the size. Nil means the clip is taken.
    nonisolated static func check(_ p: Probe) -> Refusal? {
        guard let codec = p.codec, let duration = p.durationMs else { return .format }
        if !codecs.contains(codec) { return .format }
        if let container = p.container, !containers.contains(container) { return .format }
        if duration >= Int64(maxSeconds + 1) * 1000 { return .tooLong }
        if let bytes = p.bytes, bytes > maxBytes { return .tooLarge }
        return nil
    }

    // MARK: State

    /// The clip on disk, or nil for none.
    @Published private(set) var clip: URL?
    /// The clip's first frame: the still under it, and in its place when there is no drawn face.
    @Published private(set) var poster: UIImage?
    /// Per-mood clips override the older single clip without changing its file.
    @Published private(set) var moods: [NanoMuseMood: URL] = [:]
    @Published private(set) var moodPosters: [NanoMuseMood: UIImage] = [:]
    /// Bumped whenever the clip is written or removed, so a player on the same path is rebuilt.
    @Published private(set) var version = 0

    private init() {
        // a copy that was never used or cancelled (the app died in between) is of no use now
        try? FileManager.default.removeItem(at: Self.pendingFile)
        rescan()
    }

    // MARK: Paths

    nonisolated static var clipFile: URL { NanoMuseFaceStore.directory.appendingPathComponent("own-clip.mp4") }
    nonisolated static var posterFile: URL { NanoMuseFaceStore.directory.appendingPathComponent("own-clip.png") }
    nonisolated static var pendingFile: URL { NanoMuseFaceStore.directory.appendingPathComponent("own-clip.pending.mp4") }
    nonisolated static func moodClipFile(_ mood: NanoMuseMood) -> URL {
        NanoMuseFaceStore.directory.appendingPathComponent("own-clip-\(mood.rawValue).mp4")
    }
    nonisolated static func moodPosterFile(_ mood: NanoMuseMood) -> URL {
        NanoMuseFaceStore.directory.appendingPathComponent("own-clip-\(mood.rawValue).png")
    }

    nonisolated private static func size(of url: URL) -> Int64? {
        ((try? FileManager.default.attributesOfItem(atPath: url.path))?[.size] as? NSNumber)?.int64Value
    }

    /// The clip is up to 30 MB the person can pick again: it stays out of the iCloud and
    /// computer backups, with its still and the pending copy. Set on every write, as the
    /// flag belongs to the file, not the path.
    nonisolated private static func keepOutOfBackup(_ url: URL) {
        var target = url
        var values = URLResourceValues()
        values.isExcludedFromBackup = true
        try? target.setResourceValues(values)
    }

    /// Reads the folder again: after an import, "Default face", or an account change (C12).
    func rescan() {
        let url = Self.clipFile
        if let size = Self.size(of: url), size > 0 {
            clip = url
            poster = (try? Data(contentsOf: Self.posterFile)).flatMap { UIImage(data: $0) }
        } else {
            clip = nil
            poster = nil
        }
        var next: [NanoMuseMood: URL] = [:]
        var stills: [NanoMuseMood: UIImage] = [:]
        for mood in NanoMuseMood.allCases {
            let url = Self.moodClipFile(mood)
            if let size = Self.size(of: url), size > 0 {
                next[mood] = url
                if let data = try? Data(contentsOf: Self.moodPosterFile(mood)),
                   let image = UIImage(data: data) {
                    stills[mood] = image
                }
            }
        }
        moods = next
        moodPosters = stills
    }

    /// "Use": the pending copy becomes the clip, its first frame the still.
    func adopt(_ pending: Pending, mood: NanoMuseMood? = nil) {
        let fm = FileManager.default
        try? fm.createDirectory(at: NanoMuseFaceStore.directory, withIntermediateDirectories: true)
        let destination = mood.map(Self.moodClipFile) ?? Self.clipFile
        let still = mood.map(Self.moodPosterFile) ?? Self.posterFile
        try? fm.removeItem(at: destination)
        do {
            try fm.moveItem(at: pending.url, to: destination)
        } catch {
            try? fm.copyItem(at: pending.url, to: destination)
            try? fm.removeItem(at: pending.url)
        }
        Self.keepOutOfBackup(destination)
        if let poster = pending.poster, let png = poster.pngData() {
            try? png.write(to: still, options: .atomic)
            Self.keepOutOfBackup(still)
        } else {
            try? fm.removeItem(at: still)
        }
        version += 1
        rescan()
    }

    /// "Cancel" in the preview: the copy goes.
    nonisolated func discard(_ pending: Pending) {
        try? FileManager.default.removeItem(at: pending.url)
    }

    /// "Default face": back to the drawn face, or the dragon.
    func clear(mood: NanoMuseMood? = nil) {
        let fm = FileManager.default
        if let mood {
            try? fm.removeItem(at: Self.moodClipFile(mood))
            try? fm.removeItem(at: Self.moodPosterFile(mood))
        } else {
            try? fm.removeItem(at: Self.clipFile)
            try? fm.removeItem(at: Self.posterFile)
            for state in NanoMuseMood.allCases {
                try? fm.removeItem(at: Self.moodClipFile(state))
                try? fm.removeItem(at: Self.moodPosterFile(state))
            }
        }
        try? fm.removeItem(at: Self.pendingFile)
        version += 1
        rescan()
    }

    // MARK: Looking at a file

    nonisolated private static func fourCC(_ code: FourCharCode) -> String {
        let bytes: [UInt8] = [
            UInt8((code >> 24) & 0xff), UInt8((code >> 16) & 0xff), UInt8((code >> 8) & 0xff), UInt8(code & 0xff),
        ]
        return String(bytes: bytes, encoding: .ascii) ?? String(code)
    }

    /// Looks at the file without copying it: the size from the file system, the container from
    /// its type, the length and the codec of the first video track from AVFoundation.
    nonisolated static func probe(_ url: URL) async -> Probe {
        let bytes = size(of: url)
        let container = UTType(filenameExtension: url.pathExtension)?.identifier
        let asset = AVURLAsset(url: url)
        var durationMs: Int64?
        if let duration = try? await asset.load(.duration), duration.isNumeric {
            durationMs = Int64((duration.seconds * 1000).rounded())
        }
        var codec: String?
        if let tracks = try? await asset.loadTracks(withMediaType: .video), let track = tracks.first,
           let descriptions = try? await track.load(.formatDescriptions), let description = descriptions.first {
            codec = fourCC(CMFormatDescriptionGetMediaSubType(description))
        }
        return Probe(container: container, codec: codec, durationMs: durationMs, bytes: bytes)
    }

    /// The clip's first frame, no larger than 512 px on a side.
    nonisolated static func firstFrame(_ url: URL) async -> UIImage? {
        let generator = AVAssetImageGenerator(asset: AVURLAsset(url: url))
        generator.appliesPreferredTrackTransform = true
        generator.maximumSize = CGSize(width: 512, height: 512)
        guard let result = try? await generator.image(at: .zero) else { return nil }
        return UIImage(cgImage: result.image)
    }

    /// The whole import: a look first, and a clip the rule refuses never reaches the avatar
    /// folder; one it takes is moved there as a pending file, its first frame decoded, and
    /// handed back for the preview. `url` is the picker's temporary copy; it is gone afterwards.
    nonisolated static func importClip(from url: URL) async -> Outcome {
        let probe = await probe(url)
        if let why = check(probe) {
            try? FileManager.default.removeItem(at: url)
            return .refused(why, probe)
        }
        let fm = FileManager.default
        let dst = pendingFile
        do {
            try fm.createDirectory(at: NanoMuseFaceStore.directory, withIntermediateDirectories: true)
            try? fm.removeItem(at: dst)
            do {
                try fm.moveItem(at: url, to: dst)
            } catch {
                try fm.copyItem(at: url, to: dst)
                try? fm.removeItem(at: url)
            }
        } catch {
            try? fm.removeItem(at: url)
            return .failed(error.localizedDescription)
        }
        keepOutOfBackup(dst)
        let poster = await firstFrame(dst)
        return .ready(Pending(url: dst, poster: poster, durationMs: probe.durationMs ?? 0, bytes: probe.bytes ?? size(of: dst) ?? 0))
    }
}

// MARK: - Picking

/// The picked video as a file: the photo library hands over a copy, kept in the temporary
/// folder with its own extension until the import has looked at it.
struct NanoMuseOwnClipFile: Transferable {
    let url: URL

    static var transferRepresentation: some TransferRepresentation {
        FileRepresentation(contentType: .movie) { file in
            SentTransferredFile(file.url)
        } importing: { received in
            let name = "own-clip-" + String(UUID().uuidString.prefix(8))
            let ext = received.file.pathExtension
            let tmp = FileManager.default.temporaryDirectory
                .appendingPathComponent(ext.isEmpty ? name : name + "." + ext)
            try? FileManager.default.removeItem(at: tmp)
            try FileManager.default.copyItem(at: received.file, to: tmp)
            return Self(url: tmp)
        }
    }
}

/// Picking a clip of one's own: the photo library's video picker, the checks, and the preview
/// sheet with the clip playing in the face's own frame, "Use" and "Cancel". A screen calls
/// `choose()` from a button or a menu and wears `.nanoMuseOwnClipHost(picker)` once.
@MainActor
final class NanoMuseOwnClipPicker: ObservableObject {
    @Published var presented = false
    @Published var item: PhotosPickerItem?
    @Published var pending: NanoMuseOwnClip.Pending?
    var mood: NanoMuseMood?
    @Published var message: String?
    @Published private(set) var busy = false
    private var readyForPreview: NanoMuseOwnClip.Pending?
    private var previewTask: Task<Void, Never>?

    /// Open the picker.
    func choose(mood: NanoMuseMood? = nil) {
        previewTask?.cancel()
        if let previous = readyForPreview { NanoMuseOwnClip.shared.discard(previous) }
        readyForPreview = nil
        self.mood = mood
        presented = true
    }

    /// Wait for PhotosPicker's dismissal animation before opening another sheet.
    func presentPreviewWhenPickerCloses() {
        guard !presented, readyForPreview != nil else { return }
        previewTask?.cancel()
        previewTask = Task { @MainActor [weak self] in
            do {
                try await Task.sleep(for: .milliseconds(350))
            } catch {
                return
            }
            guard let self, !self.presented, let next = self.readyForPreview else { return }
            self.readyForPreview = nil
            self.pending = next
        }
    }

    /// The picker returned: look at the file, refuse or preview.
    func handle(_ picked: PhotosPickerItem?) {
        guard let picked else { return }
        item = nil
        busy = true
        Task { @MainActor [weak self] in
            guard let self else { return }
            defer { self.busy = false }
            guard let file = try? await picked.loadTransferable(type: NanoMuseOwnClipFile.self) else {
                self.message = AppLocalized("The clip could not be read. Try another one.")
                return
            }
            switch await NanoMuseOwnClip.importClip(from: file.url) {
            case .ready(let next):
                self.readyForPreview = next
                self.presentPreviewWhenPickerCloses()
            case .refused(let why, let probe):
                self.message = Self.refusalText(why, probe)
            case .failed:
                self.message = AppLocalized("The clip could not be read. Try another one.")
            }
        }
    }

    /// The one sentence a refused clip gets, with the limits in it.
    nonisolated static func refusalText(_ why: NanoMuseOwnClip.Refusal, _ probe: NanoMuseOwnClip.Probe) -> String {
        switch why {
        case .format:
            return AppLocalized("That file cannot be used here. Pick an MP4 or MOV with H.264 or HEVC video, up to 15 seconds and 30 MB.")
        case .tooLong:
            return String(format: AppLocalized("That clip runs %d seconds. The limit is 15 seconds, and 30 MB."), Int((probe.durationMs ?? 0) / 1000))
        case .tooLarge:
            return String(format: AppLocalized("That clip is %@. The limit is 30 MB, and 15 seconds."),
                          ByteCountFormatter.string(fromByteCount: probe.bytes ?? 0, countStyle: .file))
        }
    }
}

/// The picker, the preview sheet and the refusal alert, placed once on the screen that offers the clip.
struct NanoMuseOwnClipHost: ViewModifier {
    @ObservedObject var picker: NanoMuseOwnClipPicker

    func body(content: Content) -> some View {
        content
            .photosPicker(isPresented: $picker.presented, selection: $picker.item, matching: .videos)
            .nmOnChange(of: picker.item) { picked in picker.handle(picked) }
            .nmOnChange(of: picker.presented) { shown in
                if !shown { picker.presentPreviewWhenPickerCloses() }
            }
            .sheet(item: $picker.pending) { pending in
                NanoMuseOwnClipPreview(
                    pending: pending,
                    onUse: {
                        NanoMuseOwnClip.shared.adopt(pending, mood: picker.mood)
                        picker.pending = nil
                    },
                    onCancel: {
                        NanoMuseOwnClip.shared.discard(pending)
                        picker.pending = nil
                    }
                )
                .presentationDetents([.large])
                .interactiveDismissDisabled()
            }
            .alert(
                AppLocalized("Your own clip"),
                isPresented: Binding(get: { picker.message != nil }, set: { if !$0 { picker.message = nil } })
            ) {
                Button(AppLocalized("OK"), role: .cancel) {}
            } message: {
                Text(picker.message ?? "")
            }
            .overlay {
                if picker.busy {
                    ProgressView()
                        .padding(20)
                        .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 16, style: .continuous))
                }
            }
    }
}

extension View {
    func nanoMuseOwnClipHost(_ picker: NanoMuseOwnClipPicker) -> some View {
        modifier(NanoMuseOwnClipHost(picker: picker))
    }
}

/// The preview: the clip playing in the same round frame the face uses, its length and size,
/// "Use" and "Cancel", and the sentence that it stays on this phone.
struct NanoMuseOwnClipPreview: View {
    let pending: NanoMuseOwnClip.Pending
    var onUse: () -> Void
    var onCancel: () -> Void

    var body: some View {
        VStack(spacing: 14) {
            Text(AppLocalized("Preview")).font(.title3.weight(.semibold))
            Text(AppLocalized("This is how it plays as the face: muted, in a loop, cropped to the circle."))
                .font(.footnote)
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
            ZStack {
                Circle().fill(NanoMuseTones.disc)
                // The first frame underneath covers the moment before the player has a picture.
                if let poster = pending.poster {
                    Image(uiImage: poster).resizable().scaledToFill()
                }
                NanoMuseLoopingClipView(url: pending.url, version: 0)
            }
            .frame(width: 148, height: 148)
            .clipShape(Circle())
            .padding(.top, 8)
            .accessibilityLabel(Text(AppLocalized("Preview of your clip")))
            Text(String(format: AppLocalized("%d s · %@"),
                        Int((pending.durationMs + 500) / 1000),
                        ByteCountFormatter.string(fromByteCount: pending.bytes, countStyle: .file)))
                .font(.footnote)
                .foregroundStyle(.secondary)
            Spacer(minLength: 0)
            Button {
                onUse()
            } label: {
                Text(AppLocalized("Use"))
                    .font(.body.weight(.semibold))
                    .frame(maxWidth: .infinity)
                    .padding(.vertical, 12)
            }
            .buttonStyle(.borderedProminent)
            .tint(NanoMuseTones.action)
            Button(AppLocalized("Cancel")) {
                onCancel()
            }
            .frame(maxWidth: .infinity)
            Text(AppLocalized("The clip stays on this phone. It is not uploaded to morde Cloud or synced to your other devices."))
                .font(.caption)
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
        }
        .padding(22)
    }
}
