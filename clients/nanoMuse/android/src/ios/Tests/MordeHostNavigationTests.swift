import Foundation

@main
enum MordeHostNavigationTests {
    static func main() {
        precondition(MordeResourceRoomSection.allCases == [.computers, .workspaces, .files])

        let origin = URL(string: "https://host.example")!
        precondition(isAllowedMordeHostNavigation(URL(string: "https://HOST.example:443/tasks/1"), relativeTo: origin))
        precondition(isAllowedMordeHostNavigation(URL(string: "https://host.example/tasks/1"), relativeTo: URL(string: "https://HOST.example:443")!))
        precondition(!isAllowedMordeHostNavigation(URL(string: "https://other.example/tasks"), relativeTo: origin))
        precondition(!isAllowedMordeHostNavigation(URL(string: "https://host.example:444/tasks"), relativeTo: origin))
        precondition(!isAllowedMordeHostNavigation(URL(string: "http://host.example/tasks"), relativeTo: origin))
        precondition(!isAllowedMordeHostNavigation(URL(string: "https://user@host.example/tasks"), relativeTo: origin))
        precondition(isAllowedMordeHostDownload(requestURL: URL(string: "blob:https://host.example/id"), sourceURL: origin, sourceIsMainFrame: true, relativeTo: origin))
        precondition(isAllowedMordeHostDownload(requestURL: URL(string: "https://host.example/file"), sourceURL: origin, sourceIsMainFrame: true, relativeTo: origin))
        precondition(!isAllowedMordeHostDownload(requestURL: URL(string: "https://other.example/file"), sourceURL: origin, sourceIsMainFrame: true, relativeTo: origin))
        precondition(!isAllowedMordeHostDownload(requestURL: URL(string: "blob:https://other.example/id"), sourceURL: origin, sourceIsMainFrame: true, relativeTo: origin))
        precondition(!isAllowedMordeHostDownload(requestURL: URL(string: "https://host.example/file"), sourceURL: URL(string: "https://other.example"), sourceIsMainFrame: true, relativeTo: origin))
        precondition(!isAllowedMordeHostDownload(requestURL: URL(string: "https://host.example/file"), sourceURL: origin, sourceIsMainFrame: false, relativeTo: origin))

        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try! FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let existing = directory.appendingPathComponent("notes.txt")
        try! Data().write(to: existing)
        let download = mordeHostDownloadDestination(
            suggestedFilename: "../../notes.txt", directory: directory, reservedFilenames: ["notes-1.txt"])
        precondition(download?.lastPathComponent == "notes-2.txt")
        precondition(download?.deletingLastPathComponent() == directory)
        let windowsPath = mordeHostDownloadDestination(suggestedFilename: "..\\..\\secret.txt", directory: directory)
        precondition(windowsPath?.lastPathComponent == "secret.txt")
        precondition(mordeHostDownloadDestination(suggestedFilename: "..", directory: directory)?.lastPathComponent == "download")

        let partial = directory.appendingPathComponent(".morde-test.part")
        let final = directory.appendingPathComponent("complete.txt")
        try! Data("complete".utf8).write(to: partial)
        precondition(promoteMordeHostDownload(from: partial, to: final))
        precondition(!FileManager.default.fileExists(atPath: partial.path))
        precondition((try? String(contentsOf: final, encoding: .utf8)) == "complete")
        let rejectedPartial = directory.appendingPathComponent(".morde-rejected.part")
        let existingFinal = directory.appendingPathComponent("existing.txt")
        try! Data("partial".utf8).write(to: rejectedPartial)
        try! Data("keep".utf8).write(to: existingFinal)
        precondition(!promoteMordeHostDownload(from: rejectedPartial, to: existingFinal))
        precondition((try? String(contentsOf: existingFinal, encoding: .utf8)) == "keep")
        precondition(isValidMordeHostTicketURL(URL(string: "https://host.example/__ch/auth?t=single-use")!, for: origin))
        precondition(!isValidMordeHostTicketURL(URL(string: "https://other.example/__ch/auth?t=single-use")!, for: origin))
        precondition(!isValidMordeHostTicketURL(URL(string: "https://host.example:444/__ch/auth?t=single-use")!, for: origin))
        precondition(!isValidMordeHostTicketURL(URL(string: "https://host.example/__ch/wb/?t=single-use")!, for: origin))
        precondition(!isValidMordeHostTicketURL(URL(string: "https://host.example/__ch/auth")!, for: origin))
        precondition(!isValidMordeHostTicketURL(URL(string: "https://host.example/__ch/auth?t=one&t=two")!, for: origin))
        precondition(!isValidMordeHostTicketURL(URL(string: "https://host.example/__ch/auth?t=%20%20")!, for: origin))
        let localHostOrigin = URL(string: "http://hngkra2j.hosts.localhost")!
#if DEBUG
        precondition(isAllowedMordeHostOrigin(localHostOrigin))
        precondition(isAllowedMordeHostNavigation(URL(string: "http://hngkra2j.hosts.localhost:80/__ch/auth"), relativeTo: localHostOrigin))
        precondition(isValidMordeHostTicketURL(URL(string: "http://hngkra2j.hosts.localhost/__ch/auth?t=single-use")!, for: localHostOrigin))
        precondition(!isValidMordeHostTicketURL(URL(string: "http://other.hosts.localhost/__ch/auth?t=single-use")!, for: localHostOrigin))
        precondition(!isAllowedMordeHostNavigation(URL(string: "http://public.example/__ch/auth"), relativeTo: localHostOrigin))
#else
        precondition(!isAllowedMordeHostOrigin(localHostOrigin))
        precondition(!isAllowedMordeHostNavigation(URL(string: "http://hngkra2j.hosts.localhost/__ch/auth"), relativeTo: localHostOrigin))
        precondition(!isValidMordeHostTicketURL(URL(string: "http://hngkra2j.hosts.localhost/__ch/auth?t=single-use")!, for: localHostOrigin))
#endif
        precondition(!isAllowedMordeHostOrigin(URL(string: "http://public.example")!))
        precondition(canStartMordeAction(busy: false))
        precondition(!canStartMordeAction(busy: true))
        precondition(canRetryMordeBatchItem(taskStatus: "failed", itemStatus: "failed"))
        precondition(!canRetryMordeBatchItem(taskStatus: "running", itemStatus: "failed"))
        precondition(!canRetryMordeBatchItem(taskStatus: "failed", itemStatus: "succeeded"))
        print("Morde host navigation, safe download destination, and action busy checks passed")
    }
}
