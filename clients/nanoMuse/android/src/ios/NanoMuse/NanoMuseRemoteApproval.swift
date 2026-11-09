//
//  NanoMuseRemoteApproval.swift
//  nanoMuse
//
//  A task sent to another device of the account ("@Mac tidy the downloads") runs on that
//  device's Muse. When a step there needs a yes, the question comes back over the hub as an
//  `event {stage:"approval"}` and is answered where the task was typed (docs/hub.md,
//  docs/every-device.md); the answer goes back as `approve {approval_id, allow}`. Up to 1.0.3
//  the phone declined every such question. This is the card: the four answers of the Android
//  phone's own card (RiskGate), the "for this chat" grants in memory, the "always" grants on
//  this phone under the device's id, listed under Approvals on the agent's page with Revoke.
//  A payment gets neither. A question nobody answers in time is a no, as on Android; one the
//  person at the other device answered first (`approval_result`) leaves without an answer
//  from here.
//

import Foundation
import SwiftUI

/// The fields of one `approval` event, read where the frame lands so the hub's dictionary
/// never crosses into the card's work.
struct NanoMuseRemoteQuestion: Sendable {
    /// The `approval_id`.
    let id: String
    let preview: String
    let risk: String
    let reason: String
    /// What a remembered answer covers, as the sender names it (`grant_key`, since 1.0.6:
    /// the runtime's tool and target, the desktop's tool and whole argument); empty from an
    /// older sender, whose preview is all there is to remember by.
    let grantKey: String
    /// Seconds the other side waits before it takes silence as a no; 0 when it did not say.
    let timeout: Int

    init?(body: [String: Any]) {
        guard let id = body["approval_id"] as? String, !id.isEmpty else { return nil }
        self.id = id
        preview = ((body["preview"] as? String) ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        risk = (body["risk"] as? String) ?? ""
        reason = ((body["reason"] as? String) ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        grantKey = ((body["grant_key"] as? String) ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        timeout = (body["timeout"] as? Int) ?? (body["timeout"] as? Double).map { Int($0) } ?? 0
    }
}

@MainActor
final class NanoMuseRemoteApprovals: ObservableObject {
    static let shared = NanoMuseRemoteApprovals()

    /// What the step does, from the `risk` word on the wire: the Android class names
    /// (destructive, outbound, money) and anything else, the runtime's levels included.
    enum Risk: String {
        case delete, send, pay, other

        init(wire: String) {
            switch wire.lowercased() {
            case "destructive", "delete": self = .delete
            case "outbound", "send", "network": self = .send
            case "money", "payment": self = .pay
            default: self = .other
            }
        }
    }

    /// The question as the other device's Muse put it.
    struct Request: Identifiable, Equatable {
        /// The `approval_id`.
        let id: String
        let deviceId: String
        let deviceName: String
        /// The chat the task was typed in; "for this chat" grants live under it.
        let sessionId: String
        let risk: Risk
        let preview: String
        let reason: String
        /// The sender's name for what an answer covers; empty from an older sender.
        let grantKey: String
        /// Seconds the other side waits before it takes silence as a no.
        let timeout: Int

        /// A payment is answered one at a time, as on Android.
        var canSession: Bool { risk != .pay }
        var canAlways: Bool { risk != .pay && !preview.isEmpty }
        /// The same step on the same device; what an "always" covers: the sender's key when
        /// it gave one (two steps that begin with the same words never share an answer),
        /// the preview's first 60 characters from an older sender, as before.
        var target: String { "task:" + (grantKey.isEmpty ? String(preview.prefix(60)) : grantKey) }
    }

    /// One standing answer: the same step on one device runs without a card.
    struct Grant: Codable, Identifiable, Equatable {
        var id: String { Self.key(deviceId: deviceId, risk: risk, target: target) }
        let deviceId: String
        let deviceName: String
        let risk: String
        let target: String
        let preview: String
        let grantedAt: Date

        static func key(deviceId: String, risk: String, target: String) -> String {
            "\(deviceId)|\(risk)|\(target)"
        }
    }

    enum Decision {
        case once, session, always, deny, timeout, elsewhere
    }

    /// What `decide` hands back to the task: the answer to send, or nothing to send.
    enum Outcome {
        /// `standing` when a remembered answer decided, with no card.
        case allowed(standing: Bool)
        case denied
        case settledElsewhere

        var allow: Bool? {
            switch self {
            case .allowed: return true
            case .denied: return false
            case .settledElsewhere: return nil
            }
        }
    }

    /// The questions waiting, first in front.
    @Published private(set) var queue: [Request] = []
    /// The "always" answers, oldest first.
    @Published private(set) var grants: [Grant]

    var current: Request? { queue.first }

    private var waiters: [String: CheckedContinuation<Decision, Never>] = [:]
    private var timers: [String: Task<Void, Never>] = [:]
    /// "sessionId|grant key" for the answers that hold until the chat is left.
    private var sessionGrants: Set<String> = []

    private static let grantsKey = "nanomuse.remote_approvals.always"
    /// Android's RiskGate waits this long when the other side names no timeout.
    static let defaultTimeout = 180

    private init() {
        grants = Self.loadGrants()
    }

    // MARK: - The question

    /// Answers one `approval` event from `device` about a task typed in `sessionId`: a
    /// remembered answer at once, otherwise the card, otherwise the timeout.
    func decide(_ question: NanoMuseRemoteQuestion, from device: HubDevice, sessionId: String) async -> Outcome {
        let id = question.id
        let preview = question.preview
        let request = Request(
            id: id,
            deviceId: device.id,
            deviceName: device.name,
            sessionId: sessionId,
            risk: Risk(wire: question.risk),
            preview: preview,
            reason: question.reason,
            grantKey: question.grantKey,
            timeout: question.timeout > 0 ? min(max(question.timeout, 30), 600) : Self.defaultTimeout
        )
        let key = Grant.key(deviceId: device.id, risk: request.risk.rawValue, target: request.target)
        if request.canAlways, grants.contains(where: { $0.id == key }) { return .allowed(standing: true) }
        if request.canSession, sessionGrants.contains("\(sessionId)|\(key)") { return .allowed(standing: true) }

        queue.append(request)
        timers[id] = Task { @MainActor [weak self] in
            try? await Task.sleep(nanoseconds: UInt64(request.timeout) * 1_000_000_000)
            guard !Task.isCancelled else { return }
            self?.resolve(id, .timeout)
        }
        let decision = await withCheckedContinuation { (cont: CheckedContinuation<Decision, Never>) in
            waiters[id] = cont
        }
        queue.removeAll { $0.id == id }
        switch decision {
        case .once:
            return .allowed(standing: false)
        case .session:
            if request.canSession { sessionGrants.insert("\(sessionId)|\(key)") }
            return .allowed(standing: false)
        case .always:
            if request.canAlways {
                grants.removeAll { $0.id == key }
                grants.append(Grant(deviceId: device.id, deviceName: device.name, risk: request.risk.rawValue, target: request.target, preview: preview, grantedAt: Date()))
                saveGrants()
            }
            return .allowed(standing: false)
        case .deny, .timeout:
            return .denied
        case .elsewhere:
            return .settledElsewhere
        }
    }

    /// The card's answer.
    func answer(_ id: String, _ decision: Decision) {
        resolve(id, decision)
    }

    /// The other side's `approval_result`: decided there, or expired; the card goes without an answer from here.
    func settled(_ id: String) {
        resolve(id, .elsewhere)
    }

    /// Takes one "always" back.
    func revoke(_ grantId: String) {
        grants.removeAll { $0.id == grantId }
        saveGrants()
    }

    private func resolve(_ id: String, _ decision: Decision) {
        timers.removeValue(forKey: id)?.cancel()
        guard let cont = waiters.removeValue(forKey: id) else { return }
        cont.resume(returning: decision)
    }

    // MARK: - The grants on disk

    private static func loadGrants() -> [Grant] {
        guard let data = UserDefaults.standard.data(forKey: grantsKey) else { return [] }
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        return (try? decoder.decode([Grant].self, from: data)) ?? []
    }

    private func saveGrants() {
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        if let data = try? encoder.encode(grants) {
            UserDefaults.standard.set(data, forKey: Self.grantsKey)
        } else {
            UserDefaults.standard.removeObject(forKey: Self.grantsKey)
        }
    }
}

// MARK: - The card

/// Shows the question in front, as a sheet over whatever is on screen; the next one follows.
struct NanoMuseRemoteApprovalPresenter: ViewModifier {
    @ObservedObject private var approvals = NanoMuseRemoteApprovals.shared

    func body(content: Content) -> some View {
        content
            .sheet(item: Binding(get: { approvals.current }, set: { _ in })) { request in
                NanoMuseRemoteApprovalSheet(request: request)
            }
    }
}

extension View {
    /// The remote approval card, once, at the root.
    func nanoMuseRemoteApprovalPresenter() -> some View {
        modifier(NanoMuseRemoteApprovalPresenter())
    }
}

struct NanoMuseRemoteApprovalSheet: View {
    let request: NanoMuseRemoteApprovals.Request
    @ObservedObject private var approvals = NanoMuseRemoteApprovals.shared

    private var agentName: String { NanoMuseHomeView.currentAgentName() }

    /// Android's titles for the three classes; a plain one for the rest.
    private var title: String {
        switch request.risk {
        case .delete: return String(format: AppLocalized("Allow %@ to delete this?"), agentName)
        case .send: return String(format: AppLocalized("Allow %@ to send this?"), agentName)
        case .pay: return String(format: AppLocalized("Allow %@ to make a payment?"), agentName)
        case .other: return String(format: AppLocalized("Allow %@ to take this step?"), agentName)
        }
    }

    private var waiting: Int { max(0, approvals.queue.count - 1) }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    Text(String(format: AppLocalized("The step runs on %@, not on this phone."), request.deviceName))
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                    if !request.reason.isEmpty {
                        Text(request.reason)
                            .font(.subheadline)
                            .foregroundStyle(.secondary)
                    }
                    if waiting > 0 {
                        Text(String(format: AppLocalized("%d more waiting"), waiting))
                            .font(.footnote)
                            .foregroundStyle(.tertiary)
                    }
                }
                Section {
                    Text(request.preview.isEmpty ? AppLocalized("No details were given.") : request.preview)
                        .font(.callout)
                        .textSelection(.enabled)
                } header: {
                    Text(AppLocalized("The step"))
                }
                Section {
                    Button(AppLocalized("Allow once")) { approvals.answer(request.id, .once) }
                    if request.canSession {
                        Button(AppLocalized("Allow for this chat")) { approvals.answer(request.id, .session) }
                    }
                    if request.canAlways {
                        Button(String(format: AppLocalized("Always allow this on %@"), request.deviceName)) { approvals.answer(request.id, .always) }
                    }
                    Button(AppLocalized("Deny"), role: .destructive) { approvals.answer(request.id, .deny) }
                } footer: {
                    Text(footer)
                }
            }
            .navigationTitle(title)
            .navigationBarTitleDisplayMode(.inline)
        }
        .presentationDetents([.medium, .large])
        .interactiveDismissDisabled()
    }

    private var footer: String {
        var lines = [String(format: AppLocalized("No answer in %d seconds counts as a no."), request.timeout)]
        if request.canAlways {
            lines.append(String(format: AppLocalized("Always allow lets this same step run on %@ without asking; Approvals on the agent's page lists it and can take it back."), request.deviceName))
        } else if request.risk == .pay {
            lines.append(AppLocalized("A payment is answered one at a time."))
        }
        return lines.joined(separator: " ")
    }
}
