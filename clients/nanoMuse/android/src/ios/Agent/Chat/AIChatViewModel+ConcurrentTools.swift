//
//  AIChatViewModel+ConcurrentTools.swift
//  MinisApp
//
//  Concurrent tool execution: dispatches up to `maxConcurrentTools` tool
//  calls in parallel via TaskGroup, waits for all to complete, then
//  returns results ordered to match the original tool_use sequence
//  (Anthropic API requires tool_result order to mirror tool_use order
//  within the same user message).
//
//  Created for T-concurrent-tools 2026-05-25.
//

import Foundation
import UIKit

private let ctLogger = AppLogger(category: "AIChatVM")

extension AIChatViewModel {

    /// Hard cap on simultaneous in-flight tool executions per agent turn.
    /// 10 is the requested ceiling; iSH itself can fan out further but
    /// concurrent shell forks on the emulator past this start to compete
    /// for emulator scheduling.
    static let maxConcurrentTools = 10

    /// Compatibility view for lifecycle code that still reports legacy shell state.
    ///
    /// [T-ios-concurrent-toolcall-dup-id]
    var runningCommandPids: Set<Int32> {
        get { runningCommandPid > 0 ? [runningCommandPid] : [] }
        set { runningCommandPid = newValue.first ?? 0 }
    }

    /// Convert a Linux resource path into the equivalent `minis://` link.
    func linuxPathToMinisURL(_ path: String) -> String? {
        let prefix = MinisStoragePaths.minisLinuxBaseDir + "/"
        guard path.hasPrefix(prefix) else { return nil }
        let rest = String(path.dropFirst(prefix.count))
        guard let slashIndex = rest.firstIndex(of: "/") else { return nil }
        let namespace = String(rest[..<slashIndex])
        let filename = String(rest[rest.index(after: slashIndex)...])
        guard !namespace.isEmpty, !filename.isEmpty else { return nil }
        let encoded = filename.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) ?? filename
        return "minis://\(namespace)/\(encoded)"
    }

    /// Resolve an image path without consulting the local shell executor.
    /// Mounted-folder URLs come from the manager's active security-scoped bookmarks.
    func resolveMinisPath(_ pathOrURL: String) -> URL? {
        if let url = URL(string: pathOrURL), url.scheme == "minis", url.host == "mounts" {
            let subPath = MinisURLPathDecoding.subPathCandidates(for: url).first ?? ""
            return resolveMountedFolderSubpath(subPath)
        }
        if let url = URL(string: pathOrURL), url.scheme == "minis" {
            guard isSafeMinisURL(url) else { return nil }
            return Self.resolveMinisURL(url)
        }

        guard pathOrURL.hasPrefix("/"), isSafeReadPath(pathOrURL) else { return nil }
        let mountsPrefix = MinisStoragePaths.minisMountsLinuxDir + "/"
        if pathOrURL.hasPrefix(mountsPrefix) {
            return resolveMountedFolderSubpath(String(pathOrURL.dropFirst(mountsPrefix.count)))
        }
        if let minisURLString = linuxPathToMinisURL(pathOrURL),
           let minisURL = URL(string: minisURLString) {
            if minisURL.host == "mounts" {
                let subPath = MinisURLPathDecoding.subPathCandidates(for: minisURL).first ?? ""
                return resolveMountedFolderSubpath(subPath)
            }
            guard isSafeMinisURL(minisURL) else { return nil }
            return Self.resolveMinisURL(minisURL)
        }

        let dataPath = RootfsManager.shared.dataPath
        let candidate = dataPath.appendingPathComponent(String(pathOrURL.dropFirst()))
        guard FileManager.default.fileExists(atPath: candidate.path) else { return candidate }
        let rootPath = dataPath.resolvingSymlinksInPath().standardizedFileURL.path
        let resolvedPath = candidate.resolvingSymlinksInPath().standardizedFileURL.path
        guard resolvedPath == rootPath || resolvedPath.hasPrefix(rootPath + "/") else { return nil }
        return candidate
    }

    private func resolveMountedFolderSubpath(_ subPath: String) -> URL? {
        let components = subPath.split(separator: "/", omittingEmptySubsequences: false).map(String.init)
        guard components.count >= 2,
              components.allSatisfy({ !$0.isEmpty && $0 != "." && $0 != ".." }),
              let mountName = components.first,
              let mountURL = MountedFoldersManager.shared.resolvedURL(forName: mountName) else {
            return nil
        }
        let candidate = components.dropFirst().reduce(mountURL) { url, component in
            url.appendingPathComponent(component)
        }
        guard FileManager.default.fileExists(atPath: candidate.path) else { return nil }

        // Do not let an in-folder symlink redirect read_image outside the active bookmark.
        let canonicalRoot = mountURL.resolvingSymlinksInPath().standardizedFileURL.path
        let canonicalCandidate = candidate.resolvingSymlinksInPath().standardizedFileURL.path
        guard canonicalCandidate == canonicalRoot || canonicalCandidate.hasPrefix(canonicalRoot + "/") else {
            return nil
        }
        return candidate
    }

    private func isSafeReadPath(_ path: String) -> Bool {
        !(path as NSString).pathComponents.contains { $0 == ".." || $0 == "." }
    }

    private func isSafeMinisURL(_ url: URL) -> Bool {
        guard url.scheme == "minis", let host = url.host,
              host != ".", host != ".." else { return false }
        return MinisURLPathDecoding.subPathCandidates(for: url).allSatisfy { path in
            guard !path.hasPrefix("/") else { return false }
            if path.isEmpty { return true }
            return path.split(separator: "/", omittingEmptySubsequences: false)
                .allSatisfy { !$0.isEmpty && $0 != "." && $0 != ".." }
        }
    }

    /// Tiny actor wrapping the per-turn image budget so concurrent tool
    /// tasks can race-free claim a slot for their image bytes.
    /// `reserveSlot()` returns true iff a slot was claimed; the caller
    /// then attaches `imageData` to its toolResult. False → caller must
    /// substitute a text placeholder.
    actor BatchImageBudget {
        private var remaining: Int
        private(set) var strippedCount: Int = 0

        init(initial: Int) { self.remaining = max(0, initial) }

        /// Claim one image slot. Returns true if granted.
        func reserveSlot() -> Bool {
            if remaining > 0 {
                remaining -= 1
                return true
            }
            strippedCount += 1
            return false
        }

        var strippedSoFar: Int { strippedCount }
    }

    /// Outcome of executing a single tool call. Collected by each child
    /// task and merged in original index order by the outer dispatcher.
    struct ToolExecOutcome {
        let toolId: String
        let toolName: String
        let resultPart: AgentContentPart            // .toolResult(...)
        let snapshotEntry: (toolName: String, snapshot: ToolSnapshot)?
        let snapshotItem: ToolSnapshotItem?
        let cancelled: Bool
    }

    private func disabledLocalToolOutcome(
        _ tu: StreamResult.ToolEntry,
        msgIdx: Int,
        message: String
    ) async -> ToolExecOutcome {
        let blockIdx = tu.blockIdx
        if msgIdx < messages.count, blockIdx < messages[msgIdx].blocks.count {
            messages[msgIdx].blocks[blockIdx].content = message
            messages[msgIdx].blocks[blockIdx].toolStatus = .failed(message: message)
        }
        toolLoopDetector.record(
            toolName: tu.name, params: tu.args,
            result: nil, errorMessage: message, toolCallId: tu.id
        )
        let snapshot = ToolSnapshot(type: .text, text: message, mediaRef: nil, duration: nil)
        let item = ToolSnapshotItem(
            id: tu.id, toolName: tu.name, snapshot: snapshot,
            mediaResolver: await ChatStore.shared.mediaFileURLResolver()
        )
        return ToolExecOutcome(
            toolId: tu.id, toolName: tu.name,
            resultPart: .toolResult(id: tu.id, name: tu.name, content: message, isError: true),
            snapshotEntry: (toolName: tu.name, snapshot: snapshot),
            snapshotItem: item,
            cancelled: false
        )
    }

    /// Execute a single tool use, returning a self-contained outcome.
    /// All mutations to `messages[msgIdx].blocks[blockIdx]` happen inside
    /// (the VM is @MainActor so this is safe even when invoked from
    /// concurrent child tasks). The outcome carries the toolResult part,
    /// snapshot, and cancellation flag so the dispatcher can collect them
    /// in original tool_use order after every child task completes.
    func executeSingleToolUse(
        tu: StreamResult.ToolEntry,
        msgIdx: Int,
        tools: [AgentToolDefinition],
        batchBudget: BatchImageBudget
    ) async -> ToolExecOutcome {
        let blockIdx = tu.blockIdx

        // Graceful cancel pre-check: any task that begins after the user
        // tapped Stop short-circuits with a synthetic cancellation result
        // so history stays paired.
        if Task.isCancelled || self.userDidCancel {
            let cancelContent = "<system-reminder>The user cancelled this operation. The returned result may be incomplete.</system-reminder>"
            if msgIdx < messages.count, blockIdx < messages[msgIdx].blocks.count {
                messages[msgIdx].blocks[blockIdx].toolStatus = .cancelled
                messages[msgIdx].blocks[blockIdx].content = cancelContent
            }
            let cancelSnap = ToolSnapshot(type: .text, text: cancelContent, mediaRef: nil, duration: nil)
            let item = ToolSnapshotItem(
                id: tu.id, toolName: tu.name, snapshot: cancelSnap,
                mediaResolver: await ChatStore.shared.mediaFileURLResolver()
            )
            return ToolExecOutcome(
                toolId: tu.id, toolName: tu.name,
                resultPart: .toolResult(id: tu.id, name: tu.name, content: cancelContent, isError: true),
                snapshotEntry: (toolName: tu.name, snapshot: cancelSnap),
                snapshotItem: item,
                cancelled: true
            )
        }

        if ["shell_execute", "file_read", "file_write", "file_edit"].contains(tu.name) {
            // nanoMuse: Historical local Agent tool calls fail without touching iSH or user files.
            return await disabledLocalToolOutcome(
                tu, msgIdx: msgIdx, message: MordeLocalExecutionPolicy.disabledMessage
            )
        }

        var toolOutput: String = ""
        var toolSuccess: Bool = false
        var toolImageData: Data?
        var toolImageMimeType: String?
        var toolImageLinuxPath: String?
        var toolPageURL: String?
        var cancelledHere = false

        // Loop-detector pre-check: short-circuit when the model is stuck
        // in a runaway pattern (unknown tool spam, no-progress polling, etc).
        let loopPreCheck = toolLoopDetector.check(toolName: tu.name, params: tu.args)
        if loopPreCheck.level == .critical, let blockedMsg = loopPreCheck.message {
            toolOutput = blockedMsg
            toolSuccess = false
            if msgIdx < messages.count, blockIdx < messages[msgIdx].blocks.count {
                messages[msgIdx].blocks[blockIdx].content = blockedMsg
                messages[msgIdx].blocks[blockIdx].toolStatus = .failed(message: "loop blocked")
            }
            toolLoopDetector.record(
                toolName: tu.name, params: tu.args,
                result: nil, errorMessage: blockedMsg, toolCallId: tu.id
            )
            let blockedSnap = ToolSnapshot(type: .text, text: blockedMsg, mediaRef: nil, duration: nil)
            let item = ToolSnapshotItem(
                id: tu.id, toolName: tu.name, snapshot: blockedSnap,
                mediaResolver: await ChatStore.shared.mediaFileURLResolver()
            )
            return ToolExecOutcome(
                toolId: tu.id, toolName: tu.name,
                resultPart: .toolResult(id: tu.id, name: tu.name, content: blockedMsg, isError: true),
                snapshotEntry: (toolName: tu.name, snapshot: blockedSnap),
                snapshotItem: item,
                cancelled: false
            )
        }

        // JSON Repair (T-tool-json-repair b2c4f8a6).
        var toolArgs: [String: Any] = tu.args
        let needsRepair: Bool = {
            guard let toolDef = tools.first(where: { $0.name == tu.name }) else { return false }
            if tu.args.isEmpty && !toolDef.required.isEmpty { return true }
            for field in toolDef.required {
                guard let raw = tu.args[field] else { return true }
                if raw is NSNull { return true }
                if let s = raw as? String,
                   s.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty { return true }
                if !(raw is String) && !(raw is [Any]) && !(raw is [String: Any]) { return true }
            }
            return false
        }()
        // [T-truncated-args-visibility #119] Tracks whether THIS call's
        // arguments arrived truncated and were glued shut by the repair pass.
        // Non-nil ⇒ the args are not what the model actually emitted.
        var truncationRepairTag: String? = nil
        if needsRepair {
            let rawJoined = tu.inputChunkRing.joined()
            let repairOutcome = Self.repairToolArgs(
                name: tu.name, args: tu.args, rawTail: rawJoined, tools: tools
            )
            if !repairOutcome.repairs.isEmpty {
                AppLogger(category: "ToolPreflight").warning(
                    "[ToolRepair] REPAIRED tool=\(tu.name) id=\(tu.id) strategies=[\(repairOutcome.repairs.joined(separator: ", "))] beforeKeys=[\(tu.args.keys.sorted().joined(separator: ","))] afterKeys=[\(repairOutcome.args.keys.sorted().joined(separator: ","))] rawJoined=<<<\(rawJoined.prefix(500))>>>"
                )
                toolArgs = repairOutcome.args
                // Only the truncation strategy means "the value itself was cut
                // short". Type-coercion and fuzzy-name repairs fix the SHAPE of
                // a fully-received argument and are not a data-loss signal.
                truncationRepairTag = repairOutcome.repairs.first { $0.hasPrefix("truncation+") }
            }
        }

        // Preflight: reject empty / missing-required-field tool calls.
        if let preflightError = Self.preflightValidateToolCall(name: tu.name, args: toolArgs, tools: tools) {
            let chunkRing = tu.inputChunkRing
            AppLogger(category: "ToolPreflight").warning(
                "[ToolPreflight] BLOCKED tool=\(tu.name) id=\(tu.id) reason=\"\(preflightError)\" argsKeys=[\(tu.args.keys.sorted().joined(separator: ","))] chunkCount=\(chunkRing.count) lastChunk=<<<\(chunkRing.last?.prefix(500) ?? "")>>>"
            )
            let uiMessage = AppLocalized("Blocked invalid tool call")
            let modelMessage = "Error: Tool call rejected before execution. \(preflightError) The arguments your client sent were empty or missing required fields — re-issue the call with all required parameters filled in. Do not retry with the same empty arguments."
            if msgIdx < messages.count, blockIdx < messages[msgIdx].blocks.count {
                messages[msgIdx].blocks[blockIdx].content = uiMessage
                messages[msgIdx].blocks[blockIdx].toolStatus = .failed(message: uiMessage)
            }
            toolLoopDetector.record(
                toolName: tu.name, params: tu.args,
                result: nil, errorMessage: modelMessage, toolCallId: tu.id
            )
            let blockedSnap = ToolSnapshot(type: .text, text: modelMessage, mediaRef: nil, duration: nil)
            let item = ToolSnapshotItem(
                id: tu.id, toolName: tu.name, snapshot: blockedSnap,
                mediaResolver: await ChatStore.shared.mediaFileURLResolver()
            )
            return ToolExecOutcome(
                toolId: tu.id, toolName: tu.name,
                resultPart: .toolResult(id: tu.id, name: tu.name, content: modelMessage, isError: true),
                snapshotEntry: (toolName: tu.name, snapshot: blockedSnap),
                snapshotItem: item,
                cancelled: false
            )
        }

        let argsJson: String = {
            if let data = try? JSONSerialization.data(withJSONObject: toolArgs),
               let str = String(data: data, encoding: .utf8) {
                return str
            }
            return "{}"
        }()

        do {
        switch tu.name {
        case "browser_use":
            var browserResult: BrowserActionResult
            if let input = BrowserActionInput.parse(from: argsJson) {
                do {
                    browserResult = try await browserTabPool.execute(action: input)
                } catch {
                    browserResult = .error(error.localizedDescription)
                }
            } else {
                browserResult = .error("Invalid browser_use input. Required: 'action' parameter.")
            }

            if browserTakeoverActive {
                if let freshResult = await waitForMidActionTakeover() {
                    let originalText = browserResult.text
                    let takeoverNote = "\n[Browser takeover] User manually operated the browser. Screenshot updated."
                    browserResult = BrowserActionResult(
                        text: originalText + takeoverNote,
                        success: browserResult.success,
                        base64Image: freshResult.base64Image,
                        imageFilePath: freshResult.imageFilePath,
                        pageURL: freshResult.pageURL ?? browserResult.pageURL
                    )
                }
            }
            if msgIdx < messages.count, blockIdx < messages[msgIdx].blocks.count {
                messages[msgIdx].blocks[blockIdx].content = browserResult.text
                messages[msgIdx].blocks[blockIdx].imageFilePath = browserResult.imageFilePath
                if let pageURL = browserResult.pageURL {
                    messages[msgIdx].blocks[blockIdx].browserURL = pageURL
                }
            }
            toolOutput = browserResult.text
            toolSuccess = browserResult.success
            toolPageURL = browserResult.pageURL
            if let b64 = browserResult.base64Image, let data = Data(base64Encoded: b64) {
                toolImageData = Self.resizedImageData(data, maxLongEdge: 2000) ?? data
                toolImageMimeType = "image/jpeg"

                let timestamp = Int(Date().timeIntervalSince1970)
                let screenshotFilename = "screenshot_\(timestamp).jpg"
                let sid = sessionId ?? "unknown"
                let persistDir = Self.minisBrowserPersistentDir(for: sid)
                let fm = FileManager.default
                try? fm.createDirectory(at: persistDir, withIntermediateDirectories: true)
                let persistPath = persistDir.appendingPathComponent(screenshotFilename)
                try? data.write(to: persistPath)

                let linuxPath = "\(Self.minisBrowserLinuxDir)/\(screenshotFilename)"
                toolImageLinuxPath = linuxPath

                if let minisURL = linuxPathToMinisURL(linuxPath) {
                    toolOutput += "\nminis_url: \(minisURL)"
                }
            }

            if let fetchData = browserResult.fetchedFileData,
               let fetchName = browserResult.fetchedFileName {
                let sid = sessionId ?? "unknown"
                let persistDir = Self.minisBrowserPersistentDir(for: sid)
                try? FileManager.default.createDirectory(at: persistDir, withIntermediateDirectories: true)
                let persistPath = persistDir.appendingPathComponent(fetchName)
                try? fetchData.write(to: persistPath)

                let linuxPath = "\(Self.minisBrowserLinuxDir)/\(fetchName)"
                if let minisURL = linuxPathToMinisURL(linuxPath) {
                    toolOutput += "\nminis_url: \(minisURL)"
                }
            }

            // [T-browser-download-ux] Surface native WKDownload activity in the
            // tool result so the agent KNOWS a page interaction triggered a
            // download. WebKit's decideDestination callback can land a beat
            // after the action resolves (navigation-turned-download resumes
            // the continuation first), so if a download is in flight but not
            // yet registered, wait briefly for the filename to be known.
            if let sid = sessionId {
                var downloadReport = BrowserDownloadCenter.shared.agentReport(for: sid)
                if downloadReport == nil,
                   browserTabPool.activeManager?.hasInflightDownloads == true {
                    try? await Task.sleep(nanoseconds: 500_000_000)
                    downloadReport = BrowserDownloadCenter.shared.agentReport(for: sid)
                        ?? "[browser_downloads] This action triggered a native browser file "
                         + "download that is still starting (filename not yet resolved). It will "
                         + "be saved into /var/minis/workspace/; check the workspace or the next "
                         + "browser_use result for it."
                }
                if let downloadReport {
                    toolOutput += "\n\n" + downloadReport
                }
            }

        case "read_image":
            let pathArg = toolArgs["path"] as? String ?? ""
            let resolvedURL = resolveMinisPath(pathArg)
            ctLogger.info("[read_image] pathArg=\(pathArg) resolvedURL=\(resolvedURL?.path ?? "nil") exists=\(resolvedURL.map { FileManager.default.fileExists(atPath: $0.path) } ?? false)")
            if let resolvedURL {
                let dataOK = (try? Data(contentsOf: resolvedURL)) != nil
                let uiOK = (try? Data(contentsOf: resolvedURL)).flatMap { UIImage(data: $0) } != nil
                ctLogger.info("[read_image] dataReadable=\(dataOK) uiImageDecodable=\(uiOK) fileSize=\((try? FileManager.default.attributesOfItem(atPath: resolvedURL.path)[.size]) ?? "?")")
            }

            if let fileURL = resolvedURL,
               let fileData = try? Data(contentsOf: fileURL),
               let uiImage = UIImage(data: fileData) {
                let originalSize = fileData.count
                let originalW = uiImage.cgImage.map { $0.width } ?? Int(uiImage.size.width)
                let originalH = uiImage.cgImage.map { $0.height } ?? Int(uiImage.size.height)

                let inferenceData: Data
                let resizedW: Int
                let resizedH: Int
                if let resized = Self.resizedImageData(fileData, maxLongEdge: 2000),
                   let resizedImage = UIImage(data: resized) {
                    inferenceData = resized
                    resizedW = resizedImage.cgImage.map { $0.width } ?? Int(resizedImage.size.width)
                    resizedH = resizedImage.cgImage.map { $0.height } ?? Int(resizedImage.size.height)
                } else {
                    inferenceData = uiImage.jpegData(compressionQuality: 0.85) ?? fileData
                    resizedW = originalW
                    resizedH = originalH
                }

                if pathArg.hasPrefix("/var/minis/") {
                    toolImageLinuxPath = pathArg
                } else if pathArg.hasPrefix("minis://") {
                    let tail = String(pathArg.dropFirst("minis://".count))
                    if !tail.isEmpty {
                        toolImageLinuxPath = "/var/minis/\(tail)"
                    }
                }

                var meta = "Image loaded successfully."
                meta += "\nPath: \(pathArg)"
                meta += "\nOriginal: \(originalW)x\(originalH), \(formatBytes(originalSize))"
                if resizedW != originalW || resizedH != originalH {
                    meta += "\nResized for analysis: \(resizedW)x\(resizedH)"
                }
                meta += "\nMIME: image/jpeg"

                // [T-ios-vision-group #182] Two ways to answer this call.
                //
                // Native-vision models keep the original behaviour exactly:
                // attach the pixels and let the model look at them.
                //
                // A model WITHOUT native vision only reaches this line because a
                // Vision Group is configured (that's the tool-exposure gate), so
                // hand the bytes to that group and return its DESCRIPTION as
                // text. Crucially we then leave `toolImageData` nil: attaching
                // pixels a text-only model can't decode is what the providers
                // silently drop today, and on the OpenAI Chat Completions path
                // they'd be dropped without even a placeholder.
                // [T-ios-vision-group-t264 #182] Optional caller-supplied question
                // about the image. Only load-bearing on the vision-group branch —
                // there it steers the describing model, which is the host model's
                // only way to follow up on a detail it cannot look at itself.
                let visionPrompt = (toolArgs["prompt"] as? String)?
                    .trimmingCharacters(in: .whitespacesAndNewlines)

                // [T-ios-vision-branch-mismatch #182] MUST be the same source the
                // registration used — see `activeModelHasNativeVision`. Reading
                // `selectedModel` here (while the request is built from
                // resolveCurrentEntry) is what made a text-only model fall into
                // the native pixel branch and get metadata instead of a
                // description.
                let nativeVision = self.activeModelHasNativeVision
                if nativeVision {
                    toolImageData = inferenceData
                    toolImageMimeType = "image/jpeg"
                    // Native models see the pixels, so the prompt isn't needed to
                    // direct anything — but echo it so the transcript shows what
                    // the model was looking for. Image data is untouched.
                    if let p = visionPrompt, !p.isEmpty {
                        toolOutput = meta + "\nRequested focus: \(p)"
                    } else {
                        toolOutput = meta
                    }
                    toolSuccess = true
                } else {
                    do {
                        // [T-ios-vision-group-attribution #182] Name the model
                        // that is currently reading, live. Previously the card
                        // just said "Reading image <path>…" for however many
                        // seconds the call took, so the user could not tell
                        // which member of the group was working — nor see a
                        // fallback happen. `onAttempt` fires before each
                        // candidate, so a switch is visible as it occurs.
                        let groupLabel = VisionGroupResolver.groupName()
                        let pathForUI = pathArg
                        let outcome = try await VisionGroupResolver.describe(
                            imageData: inferenceData,
                            mimeType: "image/jpeg",
                            customPrompt: visionPrompt,
                            seed: abs(tu.id.hashValue),
                            onAttempt: { [weak self] attempt in
                                guard let self,
                                      msgIdx < self.messages.count,
                                      blockIdx < self.messages[msgIdx].blocks.count else { return }
                                let via = groupLabel.map { " (\($0))" } ?? ""
                                let retry = attempt.index > 1
                                    ? " — attempt \(attempt.index)/\(attempt.total)" : ""
                                self.messages[msgIdx].blocks[blockIdx].content =
                                    "Reading image \(pathForUI) via \(attempt.modelName)\(via)\(retry)…"
                            }
                        )
                        let framed = VisionGroupResolver.framedDescription(
                            outcome,
                            groupName: groupLabel,
                            question: visionPrompt
                        )
                        toolOutput = meta + "\n\n" + framed
                        toolSuccess = true
                        ctLogger.info("[read_image] vision-group described image (\(outcome.description.count) chars) via \(outcome.modelName), priorFailures=\(outcome.priorFailures.count)")
                    } catch {
                        // Deliberately a SUCCESSFUL result carrying failure text:
                        // an errored tool result tends to make models retry in a
                        // loop, whereas this lets the model tell the user plainly.
                        let reason = (error as? VisionGroupResolver.VisionError)?.errorDescription
                            ?? error.localizedDescription
                        toolOutput = meta + "\n\n" + VisionGroupResolver.failureText(reason)
                        toolSuccess = true
                        ctLogger.error("[read_image] vision-group describe FAILED: \(reason)")
                    }
                }

                // The on-screen tool block shows the image itself in BOTH
                // branches — the user can always see what was read, regardless
                // of what the model received.
                if msgIdx < messages.count, blockIdx < messages[msgIdx].blocks.count {
                    messages[msgIdx].blocks[blockIdx].imageFilePath = fileURL.path
                    messages[msgIdx].blocks[blockIdx].content = toolOutput
                }
            } else {
                ctLogger.error("[read_image] FAILED pathArg=\(pathArg) resolvedURL=\(resolvedURL?.path ?? "nil")")
                toolOutput = "Error: Could not read image at '\(pathArg)'. Verify the path exists and is a valid image file."
                toolSuccess = false
            }

        case "memory_write":
            let memResult = executeMemoryWrite(from: argsJson)
            if msgIdx < messages.count, blockIdx < messages[msgIdx].blocks.count {
                messages[msgIdx].blocks[blockIdx].content = memResult.output
            }
            toolOutput = memResult.output
            toolSuccess = memResult.success

        case "memory_get":
            let memResult = executeMemoryGet(from: argsJson)
            if msgIdx < messages.count, blockIdx < messages[msgIdx].blocks.count {
                messages[msgIdx].blocks[blockIdx].content = memResult.output
            }
            toolOutput = memResult.output
            toolSuccess = memResult.success

        default:
            toolOutput = "Error: Unknown tool '\(tu.name)'"
            toolSuccess = false
        }
        } catch is CancellationError {
            let cancelContent = "<system-reminder>The user cancelled this operation. The returned result may be incomplete.</system-reminder>"
            let existing = (msgIdx < messages.count && blockIdx < messages[msgIdx].blocks.count)
                ? messages[msgIdx].blocks[blockIdx].content : ""
            toolOutput = existing.isEmpty ? cancelContent : existing + "\n" + cancelContent
            toolSuccess = false
            cancelledHere = true
            if msgIdx < messages.count, blockIdx < messages[msgIdx].blocks.count {
                messages[msgIdx].blocks[blockIdx].toolStatus = .cancelled
            }
        } catch {
            ctLogger.error("Tool execution threw non-cancellation error: \(error)")
            toolOutput = "Error: \(error.localizedDescription)"
            toolSuccess = false
        }

        // Tail cancel-detection: if Task got cancelled mid-execution.
        if !cancelledHere && Task.isCancelled && self.userDidCancel {
            cancelledHere = true
            toolOutput += "\n<system-reminder>The user cancelled this operation. The returned result may be incomplete.</system-reminder>"
            if msgIdx < messages.count, blockIdx < messages[msgIdx].blocks.count {
                messages[msgIdx].blocks[blockIdx].content = toolOutput
                messages[msgIdx].blocks[blockIdx].toolStatus = .cancelled
            }
        }

        let toolDuration: TimeInterval? = {
            if msgIdx < messages.count, blockIdx < messages[msgIdx].blocks.count,
               let start = messages[msgIdx].blocks[blockIdx].toolStartTime {
                return Date().timeIntervalSince(start)
            }
            return nil
        }()

        // Create snapshot from tool output.
        let snapshot: ToolSnapshot
        switch tu.name {
        case "browser_use":
            if let imagePath = (msgIdx < messages.count && blockIdx < messages[msgIdx].blocks.count)
                ? messages[msgIdx].blocks[blockIdx].imageFilePath : nil,
               let imageData = try? Data(contentsOf: URL(fileURLWithPath: imagePath)),
               let sid = sessionId {
                let ref = await ChatStore.shared.saveMedia(
                    data: imageData, mimeType: "image/jpeg", sessionId: sid,
                    originalFileName: "browser_snapshot.jpg", subdir: "browser",
                    linuxPath: toolImageLinuxPath
                )
                snapshot = ToolSnapshot(type: .image, text: nil, mediaRef: ref, duration: toolDuration)
            } else {
                let lines = toolOutput.components(separatedBy: "\n")
                let lastLines = lines.suffix(20).joined(separator: "\n")
                snapshot = ToolSnapshot(type: .text, text: lastLines, mediaRef: nil, duration: toolDuration)
            }
        case "read_image":
            if let imagePath = (msgIdx < messages.count && blockIdx < messages[msgIdx].blocks.count)
                ? messages[msgIdx].blocks[blockIdx].imageFilePath : nil,
               let imageData = try? Data(contentsOf: URL(fileURLWithPath: imagePath)),
               let sid = sessionId {
                let fileURL = URL(fileURLWithPath: imagePath)
                let mime = Self.detectImageMime(imageData)
                let ref = await ChatStore.shared.saveMedia(
                    data: imageData, mimeType: mime, sessionId: sid,
                    originalFileName: fileURL.lastPathComponent, subdir: "images",
                    linuxPath: toolImageLinuxPath
                )
                snapshot = ToolSnapshot(type: .image, text: nil, mediaRef: ref, duration: toolDuration)
            } else {
                snapshot = ToolSnapshot(type: .text, text: toolOutput, mediaRef: nil, duration: toolDuration)
            }
        default:
            snapshot = ToolSnapshot(type: .text, text: toolOutput, mediaRef: nil, duration: toolDuration)
        }

        let snapshotResolver = await ChatStore.shared.mediaFileURLResolver()
        let snapshotItem = ToolSnapshotItem(
            id: tu.id, toolName: tu.name, snapshot: snapshot, mediaResolver: snapshotResolver
        )

        // Update tool block status and store execution duration.
        if msgIdx < messages.count, blockIdx < messages[msgIdx].blocks.count {
            let blk = messages[msgIdx].blocks[blockIdx]
            blk.toolDuration = toolDuration
            if cancelledHere {
                blk.toolStatus = .cancelled
            } else if toolSuccess, truncationRepairTag != nil {
                // [T-truncated-args-visibility #119] A repaired call must not
                // render as a clean success — that is exactly the silence the
                // user reported. Surface it with the same weight the blocked
                // path already gets, so "arguments were altered" is visible in
                // the transcript rather than buried in a log line.
                blk.toolStatus = .failed(
                    message: AppLocalized("Arguments truncated in transit — result may be incomplete")
                )
            } else {
                blk.toolStatus = toolSuccess
                    ? .success
                    : .failed(message: toolOutput.components(separatedBy: "\n").first ?? "Failed")
            }
            ctLogger.info("[ToolLifecycle] COMPLETED toolId=\(tu.id.prefix(20)) tool=\(tu.name) sid=\(sessionId?.prefix(8) ?? "nil") appState=\(UIApplication.shared.applicationState == .active ? "fg" : "bg") suspended=\(streamingUIUpdatesSuspended) isProcessing=\(isProcessing) success=\(toolSuccess) duration=\(String(format: "%.1f", toolDuration ?? 0))s")
            scrollToBottomSignal.send()
        }

        // Compose finalOutput with truncation/offload.
        let maxToolResultLength = Self.kMaxToolResultChars
        var finalOutput: String
        if toolOutput.isEmpty {
            finalOutput = "(no output)"
        } else if toolOutput.count > maxToolResultLength {
            let offloadResult = offloadToolOutput(toolOutput, toolName: tu.name, toolId: tu.id)
            let offloadMinisURL = linuxPathToMinisURL(offloadResult.linuxPath)
            let truncatedBody: String
            if tu.name == "browser_use" {
                let halfLen = maxToolResultLength / 2
                truncatedBody = String(toolOutput.prefix(halfLen))
                    + "\n\n...\n\n"
                    + String(toolOutput.suffix(halfLen))
            } else {
                truncatedBody = String(toolOutput.prefix(maxToolResultLength))
            }
            finalOutput = truncatedBody
                + "\n\n[OUTPUT TRUNCATED] Full output (\(toolOutput.count) chars) saved to: \(offloadResult.linuxPath)"
                + (offloadMinisURL.map { "\nminis_url: \($0)" } ?? "")
                + "\nOpen the saved resource link to inspect the complete output."
        } else {
            finalOutput = toolOutput
        }

        // Image budget: reserve a slot atomically via the actor. If
        // declined, swap image bytes for a text placeholder.
        if let imgData = toolImageData {
            let granted = await batchBudget.reserveSlot()
            if !granted {
                let placeholder = Self.imagePlaceholderText(data: imgData, originalPath: toolArgs["path"] as? String, snapshotPath: nil)
                if finalOutput.isEmpty || !toolSuccess {
                    finalOutput = placeholder
                } else {
                    finalOutput += "\n\n" + placeholder
                }
                toolImageData = nil
                toolImageMimeType = nil
                ctLogger.info("🖼️ Tool image budget exhausted, stripped image from \(tu.name) id:\(tu.id.prefix(8))")
            }
        }

        // Loop-detector post-record.
        let postCheck = toolLoopDetector.record(
            toolName: tu.name, params: toolArgs,
            result: toolSuccess ? finalOutput : nil,
            errorMessage: toolSuccess ? nil : finalOutput,
            toolCallId: tu.id
        )
        if postCheck.level == .warning, let warningMsg = postCheck.message {
            if finalOutput.isEmpty {
                finalOutput = warningMsg
            } else {
                finalOutput += "\n\n" + warningMsg
            }
        }

        // [T-truncated-args-visibility #119] Tell the MODEL when the call it
        // just got a success for was built from truncated arguments.
        //
        // This covers supported tools that still run after argument repair.
        // the repaired call may well have done the right thing, but the model
        // has no way to know its own arguments were altered, and silently
        // assuming they were intact is how a half-truth propagates downstream.
        // Stating it lets the model verify rather than guess.
        if let tag = truncationRepairTag {
            finalOutput += "\n\n<system-reminder>The argument stream for this call was truncated in "
                + "transit and auto-closed by the client (repair strategy: \(tag)) before execution. "
                + "The arguments actually used may be incomplete — verify the result and re-issue "
                + "the call with complete arguments if anything is missing.</system-reminder>"
        }

        let resultPart = AgentContentPart.toolResult(
            id: tu.id, name: tu.name, content: finalOutput, isError: !toolSuccess,
            imageData: toolImageData, imageMimeType: toolImageMimeType,
            pageURL: toolPageURL, imageLinuxPath: toolImageLinuxPath
        )

        #if DEBUG
        let head = String(finalOutput.prefix(200))
        let tail = finalOutput.count > 400 ? "...\(String(finalOutput.suffix(200)))" : ""
        ctLogger.debug("🔧 Tool result [\(tu.name)] id:\(tu.id.prefix(8)) success:\(toolSuccess) len:\(finalOutput.count) head=\"\(head)\" \(tail)")
        #endif

        return ToolExecOutcome(
            toolId: tu.id, toolName: tu.name,
            resultPart: resultPart,
            snapshotEntry: (toolName: tu.name, snapshot: snapshot),
            snapshotItem: snapshotItem,
            cancelled: cancelledHere
        )
    }
}
