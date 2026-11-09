// SessionFileChangeTracker.swift
//
// Buffers file changes that already have an explicit session owner and
// exposes drain APIs for SyncCore to translate into SessionFile dirty rows.
// nanoMuse: Legacy iSH fakefs events are no longer installed or assigned an
// owner from ambient mount state; only explicitly attributed batches qualify.
//
// Design:
//   - Producer: an explicitly attributed caller → `recordBatch(_:)`.
//   - Storage: in-memory dict, last-write-wins per relPath.
//   - Consumer: SyncCore.sendNow() calls `drainAll()` before scanning
//     sync_dirty_records, so any pending file changes turn into
//     SessionFile dirty rows for this push cycle.
//
// Concurrency: actor isolates producer/consumer. The consumer drains and
// clears atomically — events recorded between drain and markDirty stay in
// the next snapshot, never lost.

import Foundation
import OSLog

actor SessionFileChangeTracker {
    static let shared = SessionFileChangeTracker()

    private let logger = AppLogger(category: "SessionFileTracker")

    enum Op {
        case upsert
        case delete
    }

    struct Change {
        let op: Op
        let lastTouchedAt: Date
    }

    /// sessionId → relPath → Change
    private var pending: [String: [String: Change]] = [:]

    /// Total events received (diagnostic).
    private(set) var totalRecorded: UInt64 = 0
    /// Total events ignored (missing owner sessionId or invalid relative path).
    private(set) var totalIgnored: UInt64 = 0

    private init() {}

    // MARK: - Producer

    /// Each event must carry its owner sessionId explicitly. This method
    /// never consults mounted or active-session state, so an unowned legacy
    /// iSH event must be discarded by its producer instead of guessed here.
    func recordBatch(
        _ events: [(sid: String, rawPath: String, op: Int, tsNs: Int64)],
        minisBaseURL: URL
    ) {
        var accepted = 0, ignored = 0, upserts = 0, deletes = 0
        var firstSid: String? = nil
        var firstRel: String? = nil
        var firstIgnoredPath: String? = nil
        for (sid, rawPath, opCode, _) in events {
            totalRecorded &+= 1
            guard !sid.isEmpty else {
                totalIgnored &+= 1
                ignored += 1
                if firstIgnoredPath == nil { firstIgnoredPath = rawPath }
                continue
            }
            guard let rel = Self.resolveSessionRelative(
                rawPath: rawPath, sessionId: sid, minisBaseURL: minisBaseURL
            ) else {
                totalIgnored &+= 1
                ignored += 1
                if firstIgnoredPath == nil { firstIgnoredPath = rawPath }
                continue
            }
            let isDelete = (opCode == 1)
            let change = Change(
                op: isDelete ? .delete : .upsert,
                lastTouchedAt: Date()
            )
            pending[sid, default: [:]][rel] = change
            accepted += 1
            if isDelete { deletes += 1 } else { upserts += 1 }
            if firstSid == nil { firstSid = sid; firstRel = rel }
        }
        if accepted > 0 {
            let sample = "\(firstSid?.prefix(8) ?? "?"):\(firstRel ?? "?")"
            logger.info("[SessionFileTracker] batch recv: events=\(events.count) accepted=\(accepted) ignored=\(ignored) upsert=\(upserts) delete=\(deletes) sample=\(sample) pending_sessions=\(self.pending.count)")
        } else if ignored > 0 {
            // Bumped to info + sample so invalid attributed paths remain diagnosable.
            logger.info("[SessionFileTracker] batch recv: events=\(events.count) all ignored (missing owner or path outside session subdirs) sample=\(firstIgnoredPath ?? "?")")
        }
    }

    // MARK: - Consumer

    /// Atomically read-and-clear all pending changes. Returns a snapshot
    /// of [sessionId: [relPath: Change]]. Caller (SyncCore) translates
    /// each entry into a markDirty call.
    func drainAll() -> [String: [String: Change]] {
        let snapshot = pending
        pending.removeAll(keepingCapacity: true)
        if !snapshot.isEmpty {
            let totalFiles = snapshot.values.reduce(0) { $0 + $1.count }
            logger.info("[SessionFileTracker] drainAll: sessions=\(snapshot.count) files=\(totalFiles)")
        }
        return snapshot
    }

    /// Drain entries whose host file shows real modification, AND keep
    /// entries whose mtime hasn't caught up yet (the open() was recorded
    /// but the actual write may still be in flight). Used by SyncCore
    /// instead of `drainAll()` to suppress false positives where a
    /// process opened a file write-mode but never wrote anything.
    ///
    /// Rules:
    ///   • op = .delete → always drain (file is gone, stat will fail)
    ///   • op = .upsert + file missing → drain anyway (race vs delete);
    ///     downstream markDirty + buildPortable will skip if file doesn't
    ///     exist by the time it builds the record
    ///   • op = .upsert + mtime >= lastTouchedAt - 1s → real write,
    ///     drain
    ///   • op = .upsert + mtime < lastTouchedAt - 1s → write may still
    ///     be in flight, KEEP in pending for next drain
    ///
    /// The 1s slack covers clock skew between open-time wall clock and
    /// host-FS mtime resolution. Also covers cases where the kernel
    /// flushes mtime slightly *before* we record the open (very tight
    /// schedule races).
    func drainAllWithMtimeFilter(minisBaseURL: URL) -> [String: [String: Change]] {
        guard !pending.isEmpty else { return [:] }
        var drained: [String: [String: Change]] = [:]
        var keptCount = 0
        var droppedFalsePositive = 0
        var totalUpsert = 0, totalDelete = 0
        let now = Date()

        for (sid, files) in pending {
            var sidDrained: [String: Change] = [:]
            var sidKept: [String: Change] = [:]
            for (rel, change) in files {
                if change.op == .delete {
                    sidDrained[rel] = change
                    totalDelete += 1
                    continue
                }
                let hostURL = minisBaseURL
                    .appendingPathComponent(sid, isDirectory: true)
                    .appendingPathComponent(rel)
                let attrs = try? FileManager.default.attributesOfItem(atPath: hostURL.path)
                guard let mtime = attrs?[.modificationDate] as? Date else {
                    // File missing — let downstream resolve the race.
                    sidDrained[rel] = change
                    totalUpsert += 1
                    continue
                }
                if mtime >= change.lastTouchedAt.addingTimeInterval(-1.0) {
                    sidDrained[rel] = change
                    totalUpsert += 1
                } else {
                    // Open recorded but mtime hasn't caught up. Either
                    // the process opened-then-skipped-the-write (false
                    // positive) or the write is still in flight. Keep
                    // for next drain; if it stays unchanged for several
                    // cycles the entry effectively "decays" out.
                    let age = now.timeIntervalSince(change.lastTouchedAt)
                    if age > 30.0 {
                        // Stale enough to be confident it was a no-op
                        // open. Drop without queuing markDirty.
                        droppedFalsePositive += 1
                    } else {
                        sidKept[rel] = change
                        keptCount += 1
                    }
                }
            }
            if !sidDrained.isEmpty { drained[sid] = sidDrained }
            if sidKept.isEmpty {
                pending.removeValue(forKey: sid)
            } else {
                pending[sid] = sidKept
            }
        }

        if !drained.isEmpty || keptCount > 0 || droppedFalsePositive > 0 {
            logger.info("[SessionFileTracker] drainAllWithMtimeFilter: drained=\(totalUpsert + totalDelete) (upsert=\(totalUpsert) delete=\(totalDelete)) kept_pending=\(keptCount) dropped_no_op=\(droppedFalsePositive)")
        }
        return drained
    }

    /// Drain only one session's pending changes. Used by deletion paths
    /// (deleteSession, FileBrowser delete) so the explicit op=delete
    /// markDirty wins over a stale upsert sitting in the tracker.
    func drainSession(_ sessionId: String) -> [String: Change] {
        return pending.removeValue(forKey: sessionId) ?? [:]
    }

    /// Diagnostic snapshot of how many sessions / how many files are
    /// currently buffered. Cheap; for debug RPC.
    func snapshot() -> (sessionCount: Int, fileCount: Int) {
        let files = pending.values.reduce(0) { $0 + $1.count }
        return (pending.count, files)
    }

    // MARK: - Path parsing

    /// Resolve a path into a SessionFile relative path
    /// ("<subdir>/<rel>"), or return nil to ignore.
    ///
    /// realfs may surface either of two path shapes for the same logical
    /// file:
    ///
    ///   • Guest path:  `/var/minis/<subdir>/<rel>` (fakefs path-normalize
    ///     left the bind-mount symlink alone, e.g. unlink/rename)
    ///   • Host path:   `<minisBaseURL>/<sid>/<subdir>/<rel>` (path-normalize
    ///     dereferenced the symlink — common for open() of a regular file
    ///     under a bind mount)
    ///
    /// The caller must provide the explicit owner sessionId. No active or
    /// mounted session fallback is performed here.
    nonisolated static func resolveSessionRelative(
        rawPath: String, sessionId: String, minisBaseURL: URL
    ) -> String? {
        // Guest-shape: trivial prefix strip.
        let guestPrefix = "/var/minis/"
        if rawPath.hasPrefix(guestPrefix) {
            let after = String(rawPath.dropFirst(guestPrefix.count))
            return validateAndReturnSubpath(after)
        }

        // Host-shape: must live under this session's per-session minis dir.
        // We use the *current* sessionId (capture happens at producer
        // time), so a path like ".../<sid>/workspace/foo.txt" matches.
        // /var/folders symlink resolution and similar host quirks: we
        // intentionally do a string-prefix check here without
        // resolvingSymlinksInPath because that would block the consumer
        // queue on disk syscalls. iOS sandbox paths are stable enough
        // that string matching is sufficient.
        let hostBase = minisBaseURL.path
            .trimmingCharacters(in: CharacterSet(charactersIn: "/"))
        // Match "<hostBase>/<sid>/" — accept both leading-slash and
        // /private-prefixed variants (iOS resolves /var → /private/var).
        let candidates = [
            "/\(hostBase)/\(sessionId)/",
            "/private/\(hostBase)/\(sessionId)/",
        ]
        for cand in candidates {
            if rawPath.hasPrefix(cand) {
                let after = String(rawPath.dropFirst(cand.count))
                return validateAndReturnSubpath(after)
            }
        }
        return nil
    }

    /// Validate that "<subdir>/<rest>" is one of the session-scoped
    /// SessionFile subdirs. memory/skills/shared/mounts have their own
    /// sync mechanisms and do NOT produce SessionFile rows.
    private nonisolated static func validateAndReturnSubpath(_ subpath: String) -> String? {
        guard let slash = subpath.firstIndex(of: "/") else { return nil }
        let subdir = String(subpath[..<slash])
        let scoped: Set<String> = ["workspace", "attachments", "browser", "offloads"]
        guard scoped.contains(subdir) else { return nil }
        return subpath
    }
}

/// Compatibility entry points for lifecycle callers. Skill changes are
/// detected by SyncDirtyScanner, so the retired fakefs notifier has no
/// pending work to drain.
///
/// Its lifecycle entry points preserve MainActor-compatible call sites.
@MainActor
final class SkillFilesystemNotifier {
    static let shared = SkillFilesystemNotifier()
    private init() {}

    /// Retained for lifecycle call sites while the fakefs event source is
    /// retired. Skill file changes are found by SyncDirtyScanner.
    func drainIfDirty(reason: String) {
        _ = reason
    }

    /// Same no-op lifecycle compatibility entry point for callers that
    /// cannot await MainActor inline.
    nonisolated func drainIfDirtyAsync(reason: String) {
        Task { @MainActor in self.drainIfDirty(reason: reason) }
    }
}
