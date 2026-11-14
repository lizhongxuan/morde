// swift-tools-version: 5.10
import PackageDescription

let package = Package(
    name: "MordeTaskIsolation",
    platforms: [.macOS(.v13)],
    products: [],
    targets: [
        .target(
            name: "MordeTaskPolicies",
            path: ".",
            exclude: [
                "MordeApp.swift",
                "MordeDesignChrome.swift",
                "MordeDesignNavigation.swift",
                "MordeHubRelay.swift",
                "MordePortalNavigation.swift",
                "MordeStandaloneApp.swift",
                "MordeWorkspaceTransfer.swift",
                "Tests",
            ],
            sources: ["MordeCredentialExpiry.swift", "MordeHostStatusProjection.swift", "MordeCodexSessionPolicy.swift"]
        ),
        .testTarget(
            name: "MordeTaskIsolationTests",
            dependencies: ["MordeTaskPolicies"],
            path: "Tests"
        ),
    ]
)
