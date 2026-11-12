import Foundation

enum MIFHostRuntimeStatus: Equatable {
    case online
    case offline
    case unknown

    init(online: Bool?) {
        switch online {
        case true: self = .online
        case false: self = .offline
        case nil: self = .unknown
        }
    }
}

enum MIFDesktopConnectionStatus: Equatable {
    case unknown
    case connecting
    case connected
    case disconnected
    case failed

    init(scriptValue: String) {
        switch scriptValue {
        case "connecting": self = .connecting
        case "connected": self = .connected
        case "disconnected": self = .disconnected
        case "failed": self = .failed
        default: self = .unknown
        }
    }
}

struct MordeWorkspaceSyncHostApplication: Decodable, Equatable {
    let status: String
    let host_id: String
    let workspace_binding_version: Int64
    let target_revision: Int64
    let manifest_sha256: String
    let runtime_generation: Int64?
    let applied_revision: Int64?
    let sync_state: String?
    let impacts: [MordeWorkspaceSyncHostApplicationImpact]

    func confirmsApplication(targetRevision: Int64, hostID: String, bindingVersion: Int64) -> Bool {
        status == "applied" &&
            self.host_id == hostID &&
            workspace_binding_version == bindingVersion &&
            self.target_revision == targetRevision &&
            runtime_generation.map { $0 > 0 } == true &&
            applied_revision.map { $0 >= targetRevision } == true &&
            sync_state == "healthy" &&
            impacts.allSatisfy { $0.state == "applied" }
    }
}

struct MordeWorkspaceSyncHostApplicationImpact: Decodable, Equatable {
    let path: String
    let state: String
    let reason: String?
}
