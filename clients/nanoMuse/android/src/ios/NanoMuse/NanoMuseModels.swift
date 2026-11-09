//
//  NanoMuseModels.swift
//  nanoMuse
//
//  Which model answers, and how a pick sticks (contract C4): the relay's
//  menu says `for: ["chat"] | ["gui"] | ["chat","gui"]`; the chat opens on
//  the model recommended for that lane; a model picked in the chat's picker moves to the
//  front of the nanoMuse Cloud group so new chats follow it, and whether a
//  model sees pictures judged from its name when nobody said.
//  Android: cloud/NanoMuseCloud.kt (followPick, recommendedChat),
//  chat/VisionFamilies.kt.
//

import Foundation
import SwiftUI

// MARK: - The relay's menu (pure, tested)

enum NanoMuseModelMenu {
    static let defaultChatModel = "Qwen/Qwen3.6-27B-FP8"
    static let defaultGuiModel = "Qwen/Qwen3.6-27B-FP8"

    /// The relay's `for` on a menu entry — under `nanomuse` with its other flags (relay 0.17),
    /// a top-level `for` read too; nil when it did not say (a relay from before 0.17).
    static func lanes(_ model: [String: Any]) -> [String]? {
        ((model["nanomuse"] as? [String: Any])?["for"] as? [String]) ?? (model["for"] as? [String])
    }

    static func isRecommended(_ model: [String: Any]) -> Bool {
        ((model["nanomuse"] as? [String: Any])?["recommended"] as? Bool) == true
    }

    /// The lanes the relay recommends the model in (`nanomuse.recommended_for`); nil when it did not say.
    static func recommendedLanes(_ model: [String: Any]) -> [String]? {
        (model["nanomuse"] as? [String: Any])?["recommended_for"] as? [String]
    }

    static func forChat(_ model: [String: Any]) -> Bool {
        lanes(model)?.contains("chat") != false
    }

    static func drawsOnly(_ model: [String: Any]) -> Bool {
        let arch = model["architecture"] as? [String: Any]
        let outputs = arch?["output_modalities"] as? [String] ?? []
        return outputs.contains("image") && !outputs.contains("text")
    }

    /// The chat model the menu opens on: the one recommended *for chat* (`recommended_for`, else
    /// a recommended one that is not hands-only), then the recommended one, then the known
    /// default by name, then the first chat model.
    static func recommendedChat(_ models: [[String: Any]]) -> [String: Any]? {
        let chats = models.filter { !drawsOnly($0) }
        return chats.first { recommendedLanes($0)?.contains("chat") == true }
            ?? chats.first { isRecommended($0) && forChat($0) && lanes($0)?.contains("gui") != true }
            ?? chats.first { isRecommended($0) && forChat($0) }
            ?? chats.first { ($0["id"] as? String) == defaultChatModel }
            ?? chats.first { forChat($0) }
    }

    /// The model for looking at a screen: the one recommended for `gui`, else any `for: ["gui"]`,
    /// else the known default by name.
    static func guiModel(_ models: [[String: Any]]) -> String? {
        (models.first { recommendedLanes($0)?.contains("gui") == true }?["id"] as? String)
            ?? (models.first { lanes($0)?.contains("gui") == true }?["id"] as? String)
            ?? (models.first { ($0["id"] as? String) == defaultGuiModel }?["id"] as? String)
    }

    // MARK: Following the pick

    private static var observer: NSObjectProtocol?

    /// Start moving a pick made in the chat's picker to the front of the Cloud group.
    @MainActor
    static func startFollowingPicks() {
        guard observer == nil else { return }
        observer = NotificationCenter.default.addObserver(forName: .sessionModelBindingChanged, object: nil, queue: .main) { note in
            guard let sid = note.userInfo?["sessionId"] as? String else { return }
            Task { @MainActor in
                guard let binding = ProviderConfigStore.shared.binding(for: sid) else { return }
                switch binding.primarySource {
                case .group(let groupId, let resolved): NanoMuseCloud.followPick(entryId: resolved, groupId: groupId)
                case .directEntry(let id, _): NanoMuseCloud.followPick(entryId: id)
                }
            }
        }
    }
}

extension NanoMuseCloud {
    /// The picker's binding is per chat and a new chat starts from the default group, so a
    /// choice made in the picker was undone by the next "New chat". Since 0.1.41 any pick
    /// sticks, not only one in the Cloud group: a model of a provider of the person's own
    /// becomes the chat slot (`NanoMuseModelSlots.useForChat`: a group of ours for that
    /// provider, the pick first, that group the default); a group the person picked becomes
    /// the default as it is, and is never edited when it mixes providers. A picture or video
    /// model is left alone. True when new chats will follow the pick.
    @discardableResult
    static func followPick(entryId: String, groupId: String? = nil) -> Bool {
        let store = ProviderConfigStore.shared
        guard let entry = store.entry(for: entryId), NanoMuseModelSlots.chats(entry) else { return false }
        if let groupId, let group = store.group(for: groupId) {
            let ours = group.memberEntryIds.allSatisfy { store.entry(for: $0)?.providerInstanceId == entry.providerInstanceId }
            if !ours {
                if store.defaultPrimaryGroupId != groupId { store.defaultPrimaryGroupId = groupId }
                NanoMuseModelSlots.mainChatFollows(groupId: groupId, entry: entry)
                return true
            }
        }
        NanoMuseModelSlots.useForChat(entry)
        return true
    }
}

// MARK: - Vision (pure, tested)

enum NanoMuseVision {
    /// Names that mean text only however the rest of the name reads.
    private static let textOnly = [
        "embed", "whisper", "tts", "rerank", "moderation", "transcri", "speech",
        "-coder", "codestral", "deepseek-chat", "deepseek-reasoner", "deepseek-v3", "deepseek-r1",
        "qwen2.5-", "qwen2-", "qwen1.5", "qwq", "qwen-long", "qwen-math", "qwen-coder", "qwen3-coder", "qwen3-235b", "qwen3-32b", "qwen3-30b", "qwen3-14b", "qwen3-8b", "qwen3-4b",
        "llama-3", "llama3", "mistral-7b", "mixtral", "phi-", "gemma-2", "gemma2", "glm-4-", "glm-4.5-air", "glm-4.6", "glm-4.7",
        "text-", "davinci", "babbage", "kimi-k2-", "kimi-k2.1", "moonshot-v1-8k", "moonshot-v1-32k", "moonshot-v1-128k",
        "minimax-m", "minimax-text", "abab", "hunyuan-lite", "hunyuan-turbo", "ernie-speed", "ernie-lite",
    ]

    /// Families that see. Checked after `textOnly`, so `qwen2.5-vl` still passes via "-vl".
    private static let sees = [
        "kimi-vl", "glm-4v", "glm-4.5v", "glm-4.6v", "glm-5",
        "qwen3", "qwen-max", "qwen-plus", "qwen-turbo", "qwen-flash", "qvq",
        "gpt-4o", "gpt-4.1", "gpt-4.5", "gpt-5", "gpt-6", "gpt-4-turbo", "chatgpt-4o",
        "claude-3", "claude-4", "claude-opus", "claude-sonnet", "claude-haiku",
        "gemini", "gemma-3", "gemma3", "grok-2-vision", "grok-3", "grok-4",
        "llama-4", "llama4", "mistral-medium", "mistral-large-2", "mistral-small-3", "magistral",
        "minimax-h", "minimax-vl", "kimi-k2.5", "kimi-k3", "doubao-seed", "doubao-1.5-vision", "doubao-1.6", "seed-1",
        "step-1v", "step-1o", "step-3", "hunyuan-vision", "hunyuan-t1", "hunyuan-large-vision", "ernie-4.5",
    ]

    private static let oSeries = try! NSRegularExpression(pattern: "(^|[^a-z0-9])o[134](?=$|[^a-z0-9])")
    private static let deepSeekVersion = try! NSRegularExpression(pattern: "v(\\d+)(?:\\.(\\d+))?")

    /// DeepSeek's rule (contract C4): text only unless the id names a version from `v4.1` on,
    /// or says `vision` / `ocr`. Nil when the id is not DeepSeek's.
    static func deepSeekSees(_ hay: String) -> Bool? {
        guard hay.contains("deepseek") else { return nil }
        if hay.contains("vision") || hay.contains("ocr") { return true }
        let ns = hay as NSString
        guard let m = deepSeekVersion.firstMatch(in: hay, range: NSRange(location: 0, length: ns.length)) else { return false }
        let major = Int(ns.substring(with: m.range(at: 1))) ?? 0
        let minor = m.range(at: 2).location == NSNotFound ? 0 : (Int(ns.substring(with: m.range(at: 2))) ?? 0)
        return major > 4 || (major == 4 && minor >= 1)
    }

    static func looksLikeItSees(id: String, displayName: String = "") -> Bool {
        let hay = (id + " " + displayName).lowercased()
        if ["-vl", "vl-", "vision", "omni", "llava", "pixtral", "internvl"].contains(where: { hay.contains($0) }) { return true }
        if let ds = deepSeekSees(hay) { return ds }
        if textOnly.contains(where: { hay.contains($0) }) { return false }
        if sees.contains(where: { hay.contains($0) }) { return true }
        return oSeries.firstMatch(in: hay, range: NSRange(location: 0, length: (hay as NSString).length)) != nil
    }

    /// What to assume when neither the provider nor models.dev said: `.vision` for a family that
    /// sees, `.textOnly` for a DeepSeek id the rule says is text-only, nil to leave the provider's default.
    static func guess(id: String, displayName: String = "") -> ModelModality? {
        let hay = (id + " " + displayName).lowercased()
        if let ds = deepSeekSees(hay) { return ds ? .vision : .textOnly }
        return looksLikeItSees(id: id, displayName: displayName) ? .vision : nil
    }
}

// nanoMuse: Morde keeps catalogue data for compatibility but does not expose personal provider setup.
