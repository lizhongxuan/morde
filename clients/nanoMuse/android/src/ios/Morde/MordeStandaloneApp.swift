import SwiftUI

extension Bundle {
    var languageBundle: Bundle? { nil }
}

@main struct MordeApp: App {
    var body: some Scene {
        WindowGroup { MordeEmbeddedView() }
    }
}
