import AppIntents

/// Registers app shortcuts so they appear in Shortcuts with zero user setup.
// nanoMuse: Keep only the read-only session shortcuts available in the cloud-only Minis target.
///
/// Phrase localization: the English phrases below are the KEYS. Localized
/// Siri trigger phrases (zh-Hans/zh-Hant, e.g. "问问Minis") live in
/// `src/ios/<locale>.lproj/AppShortcuts.strings` — the table name
/// AppShortcuts is what the AppIntents runtime looks up. Legacy .strings
/// (not .xcstrings) because the deployment target is iOS 16 and Xcode
/// rejects AppShortcuts.xcstrings below iOS 17. Add new phrases here AND
/// to every AppShortcuts.strings; keys must match exactly with
/// `\(.applicationName)` spelled `${applicationName}` in the tables.
@available(iOS 17.0, *)
struct MinisShortcutsProvider: AppShortcutsProvider {
    static var appShortcuts: [AppShortcut] {
        AppShortcut(
            intent: GetSessionStatusIntent(),
            phrases: ["Get \(.applicationName) session status", "Check \(.applicationName) task"],
            shortTitle: "Session Status",
            systemImageName: "info.circle.fill"
        )
        AppShortcut(
            intent: ListSessionsIntent(),
            phrases: ["List \(.applicationName) sessions", "Show \(.applicationName) chats"],
            shortTitle: "List Sessions",
            systemImageName: "list.bullet"
        )
        AppShortcut(
            intent: OpenSessionIntent(),
            phrases: ["Open a \(.applicationName) session"],
            shortTitle: "Open Session",
            systemImageName: "arrow.up.right.square"
        )
    }
}
