import Foundation

@main
enum MordePortalNavigationTests {
    static func main() {
        let origin = URL(string: "https://portal.example")!
        for path in ["/", "/assistant", "/hosts", "/connections", "/apikeys"] {
            precondition(isAllowedMordePortalNavigation(URL(string: "https://portal.example\(path)?tab=1#top"), relativeTo: origin))
        }

        for address in [
            "http://portal.example/hosts",
            "https://other.example/hosts",
            "https://portal.example:444/hosts",
            "https://user@portal.example/hosts",
            "https://portal.example/admin",
            "https://portal.example/admin/hosts",
            "https://portal.example/login",
            "https://portal.example/api/apikeys",
            "https://portal.example/hosts/new",
            "https://portal.example/assistant/../admin",
            "https://portal.example/%2e%2e/admin",
            "https://portal.example/%68osts",
            "https://portal.example//hosts",
        ] {
            precondition(!isAllowedMordePortalNavigation(URL(string: address), relativeTo: origin), address)
        }

        precondition(!isAllowedMordePortalNavigation(URL(string: "https://portal.example/hosts"), relativeTo: URL(string: "http://portal.example")!))
        precondition(!isAllowedMordePortalNavigation(URL(string: "https://portal.example/hosts"), relativeTo: URL(string: "https://user@portal.example")!))
        precondition(!isAllowedMordePortalNavigation(URL(string: "https://portal.example/hosts"), relativeTo: URL(string: "https://portal.example/?next=/admin")!))
#if DEBUG
        let localOrigin = URL(string: "http://localhost:7080")!
        precondition(isAllowedMordePortalNavigation(URL(string: "http://localhost:7080/hosts"), relativeTo: localOrigin))
        precondition(!isAllowedMordePortalNavigation(URL(string: "http://example.com/hosts"), relativeTo: localOrigin))
#else
        precondition(!isAllowedMordePortalNavigation(URL(string: "http://localhost:7080/hosts"), relativeTo: URL(string: "http://localhost:7080")!))
#endif
        print("Morde Portal navigation checks passed")
    }
}
