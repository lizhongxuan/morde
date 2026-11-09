//
//  NanoMuseCloudHeaders.swift
//  nanoMuse
//
//  The two headers a chat request to nanoMuse Cloud carries from 1.0.7 (docs/cloud.md
//  "The record"): which device asked and which conversation the turn belongs to, so the
//  record can file it, and the devices list can say what each device is on. Only the
//  account's own instance gets them; a request to a provider of one's own carries nothing
//  of the account. The conversation id is the prompt cache key the agent provider already
//  derives (a hash of the conversation's first user message, stable across turns), the
//  same value the own-key turns name, so the halves of one conversation meet in the record.
//  Android: io.github.nanomuse.cloud.CloudHeaders.
//

import Foundation

enum NanoMuseCloudHeaders {
    static let device = "X-NanoMuse-Device"
    static let conversation = "X-NanoMuse-Conversation"

    /// The device header for requests on `instance`: nanoMuse Cloud's own instance only.
    @MainActor
    static func forInstance(_ instance: ProviderInstance) -> [String: String] {
        guard let cloud = NanoMuseCloud.instance, cloud.id == instance.id else { return [:] }
        return [device: NanoMuseHub.shared.deviceId]
    }

    /// Whether a provider's extra headers mark it as the account's (set by `forInstance`).
    static func isCloud(_ extraHeaders: [String: String]) -> Bool {
        extraHeaders[device] != nil
    }
}
