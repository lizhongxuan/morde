import AppIntents
import Foundation

enum AudioTogglePlaybackBridge {
    static let darwinNotificationName = "io.github.nanomuse.app.liveActivity.audioToggle"
}

@available(iOS 17.0, *)
struct AudioTogglePlaybackIntent: LiveActivityIntent {
    static var title: LocalizedStringResource = "Play or Pause Narration"
    static var description = IntentDescription("Toggles play/pause on morde audio narration from the Live Activity.")
    static var openAppWhenRun = false

    func perform() async throws -> some IntentResult {
        let center = CFNotificationCenterGetDarwinNotifyCenter()
        CFNotificationCenterPostNotification(
            center,
            CFNotificationName(AudioTogglePlaybackBridge.darwinNotificationName as CFString),
            nil,
            nil,
            true
        )
        return .result()
    }
}
