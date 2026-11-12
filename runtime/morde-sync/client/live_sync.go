package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	mordesync "morde-sync"
)

const (
	defaultRescanInterval = 30 * time.Second
	changesWaitSeconds    = 20
	changesPageLimit      = 200
)

// LiveSyncOptions configures a long-running, bidirectional live workspace
// replica. It uses filesystem notifications for prompt local updates and a
// periodic content rescan as a recovery path for missed events.
type LiveSyncOptions struct {
	WorkspaceID string
	Directory   string
	Scope       []string
	// IdempotencyKey is the stable handoff identity. Each changed sync request
	// derives a distinct server create key from it; retries of that request
	// reuse the same server-recognized key.
	IdempotencyKey string
	RescanInterval time.Duration
	Progress       func(Progress)
	OnResult       func(Result)
}

// LiveConflictError means an automatic merge was unsafe. The session and
// conflicting paths are retained for the caller; the live loop stops without
// checkpointing its baseline or cursor.
type LiveConflictError struct {
	Session   SyncSession
	Conflicts []Conflict
	Reason    string
}

func (e *LiveConflictError) Error() string {
	if e.Reason != "" {
		return "live sync paused: " + e.Reason
	}
	return "live sync paused because the workspace changed on both sides"
}

// ValidateLiveBaseline verifies that this directory has a durable common state
// before a Host resumes an already-active binding after restart.
func (c *Client) ValidateLiveBaseline(ctx context.Context, workspaceID, directory string, scope []string) error {
	if ctx == nil || !workspaceIDPattern.MatchString(workspaceID) || directory == "" {
		return errors.New("context, valid workspace ID, and local directory are required")
	}
	if err := validateScope(scope); err != nil {
		return err
	}
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		return fmt.Errorf("resolve live workspace directory: %w", err)
	}
	releaseOperation, err := acquireOperationLock(ctx, canonical)
	if err != nil {
		return err
	}
	defer releaseOperation()

	state, err := c.readLiveState(canonical, workspaceID)
	if err != nil {
		return err
	}
	if !state.Initialized {
		return errors.New("live sync baseline is not initialized")
	}
	if !equalStrings(state.Scope, scope) {
		return errors.New("live sync scope differs from the persisted baseline")
	}
	return nil
}

// LiveSync keeps one local directory and one Morde workspace converged. State
// is checkpointed only after the server commit and a fresh local/remote
// comparison agree. A conflict or ambiguous cursor is fail-closed.
func (c *Client) LiveSync(ctx context.Context, options LiveSyncOptions) error {
	if ctx == nil || !workspaceIDPattern.MatchString(options.WorkspaceID) || options.Directory == "" {
		return errors.New("context, valid workspace ID, and local directory are required")
	}
	if len(options.Scope) > 1000 {
		return errors.New("live sync scope has too many entries")
	}
	if options.RescanInterval == 0 {
		options.RescanInterval = defaultRescanInterval
	}
	if options.RescanInterval < 5*time.Second || options.RescanInterval > time.Hour {
		return errors.New("live sync rescan interval must be between 5 seconds and 1 hour")
	}
	directory, err := canonicalDirectory(options.Directory)
	if err != nil {
		return fmt.Errorf("resolve live workspace directory: %w", err)
	}
	releaseOperation, err := acquireOperationLock(ctx, directory)
	if err != nil {
		return err
	}
	defer releaseOperation()
	if err := validateScope(options.Scope); err != nil {
		return err
	}
	state, err := c.loadOrCreateLiveState(ctx, options, directory)
	if err != nil {
		return err
	}
	events, watcherErrors, closeWatcher, err := watchWorkspace(ctx, directory)
	if err != nil {
		// A platform watch-limit failure must not block sync; periodic scans remain active.
		events, watcherErrors, closeWatcher = nil, nil, func() {}
		if options.Progress != nil {
			options.Progress(Progress{State: "reconnecting", Stage: "rescan_fallback", ConfirmedRevision: int64Pointer(state.Revision)})
		}
	}
	defer closeWatcher()

	rescanTicker := time.NewTicker(options.RescanInterval)
	defer rescanTicker.Stop()
	remoteResults := make(chan changesPollResult, 1)
	var pollCancel context.CancelFunc
	var pollGeneration uint64
	var pollCursor string
	startPoll := func(cursor string) {
		if pollCancel != nil {
			if pollCursor == cursor {
				return
			}
			pollCancel()
		}
		pollGeneration++
		generation := pollGeneration
		pollCtx, cancel := context.WithCancel(ctx)
		pollCancel = cancel
		pollCursor = cursor
		go func() {
			page, pollErr := c.getChanges(pollCtx, options.WorkspaceID, cursor, changesWaitSeconds, changesPageLimit)
			select {
			case remoteResults <- changesPollResult{page: page, err: pollErr, generation: generation}:
			case <-ctx.Done():
			}
		}()
	}
	defer func() {
		if pollCancel != nil {
			pollCancel()
		}
	}()

	reportLiveStage(options, state, "sync_planning")
	if err := c.runLiveCycleWithRetry(ctx, options, directory, state, nil); err != nil {
		return err
	}
	state, err = c.readLiveState(directory, options.WorkspaceID)
	if err != nil {
		return err
	}
	startPoll(state.Cursor)
	if options.Progress != nil {
		options.Progress(Progress{State: "running", Stage: "watch", ConfirmedRevision: int64Pointer(state.Revision)})
	}
	var debounce *time.Timer
	var debounceC <-chan time.Time
	resetDebounce := func() {
		if debounce == nil {
			debounce = time.NewTimer(250 * time.Millisecond)
		} else {
			if !debounce.Stop() {
				select {
				case <-debounce.C:
				default:
				}
			}
			debounce.Reset(250 * time.Millisecond)
		}
		debounceC = debounce.C
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			resetDebounce()
		case watchErr, ok := <-watcherErrors:
			if !ok {
				watcherErrors = nil
				continue
			}
			if watchErr != nil && options.Progress != nil {
				options.Progress(Progress{State: "running", Stage: "rescan_fallback", ConfirmedRevision: int64Pointer(state.Revision)})
			}
		case <-debounceC:
			debounceC = nil
			reportLiveStage(options, state, "sync_planning")
			if err := c.runLiveCycleWithRetry(ctx, options, directory, state, nil); err != nil {
				return err
			}
			state, err = c.readLiveState(directory, options.WorkspaceID)
			if err != nil {
				return err
			}
			reportLiveStage(options, state, "watch")
			startPoll(state.Cursor)
		case <-rescanTicker.C:
			reportLiveStage(options, state, "sync_planning")
			if err := c.runLiveCycleWithRetry(ctx, options, directory, state, nil); err != nil {
				return err
			}
			state, err = c.readLiveState(directory, options.WorkspaceID)
			if err != nil {
				return err
			}
			reportLiveStage(options, state, "watch")
			startPoll(state.Cursor)
		case polled := <-remoteResults:
			if polled.generation != pollGeneration {
				continue
			}
			pollCancel = nil
			if polled.err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if isStaleCursorError(polled.err) {
					return fmt.Errorf("live sync cursor is no longer valid; a safe baseline reset is not available: %w", polled.err)
				}
				if !retryableError(polled.err) {
					return polled.err
				}
				if options.Progress != nil {
					options.Progress(Progress{State: "reconnecting", Stage: "changes", ConfirmedRevision: int64Pointer(state.Revision)})
				}
				if err := waitBackoff(ctx, 500*time.Millisecond); err != nil {
					return err
				}
				state, err = c.readLiveState(directory, options.WorkspaceID)
				if err != nil {
					return err
				}
				startPoll(state.Cursor)
				continue
			}
			if len(polled.page.Items) > 0 {
				cursor := polled.page.NextCursor
				reportLiveStage(options, state, "sync_planning")
				if err := c.runLiveCycleWithRetry(ctx, options, directory, state, &cursor); err != nil {
					return err
				}
				state, err = c.readLiveState(directory, options.WorkspaceID)
				if err != nil {
					return err
				}
				reportLiveStage(options, state, "watch")
			}
			startPoll(state.Cursor)
		}
	}
}

func reportLiveStage(options LiveSyncOptions, state liveState, stage string) {
	if options.Progress != nil {
		options.Progress(Progress{State: "running", Stage: stage, ConfirmedRevision: int64Pointer(state.Revision)})
	}
}

func int64Pointer(value int64) *int64 { return &value }

type liveState struct {
	Version     int                `json:"version"`
	Workspace   string             `json:"workspace_id"`
	Directory   string             `json:"directory"`
	Scope       []string           `json:"scope"`
	Initialized bool               `json:"initialized"`
	Revision    int64              `json:"revision"`
	Cursor      string             `json:"cursor"`
	Baseline    mordesync.Manifest `json:"baseline"`
}

type changesPollResult struct {
	page       workspaceChangePage
	err        error
	generation uint64
}

type workspaceChange struct {
	Revision int64          `json:"revision"`
	Path     string         `json:"path"`
	Action   string         `json:"action"`
	File     *workspaceFile `json:"file"`
}

type workspaceChangePage struct {
	Items      []workspaceChange `json:"items"`
	NextCursor string            `json:"next_cursor"`
}

func (c *Client) loadOrCreateLiveState(ctx context.Context, options LiveSyncOptions, directory string) (liveState, error) {
	state, err := c.readLiveState(directory, options.WorkspaceID)
	if err == nil {
		if !equalStrings(state.Scope, options.Scope) {
			return liveState{}, errors.New("live sync scope differs from the persisted baseline; use a new workspace directory or explicit reset")
		}
		return state, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return liveState{}, err
	}
	latest, err := c.latestRevision(ctx, options.WorkspaceID)
	if err != nil {
		return liveState{}, err
	}
	cursor := encodeLiveCursor(liveCursor{Revision: latest})
	state = liveState{Version: 1, Workspace: options.WorkspaceID, Directory: directory, Scope: append([]string(nil), options.Scope...),
		Initialized: false, Revision: latest, Cursor: cursor, Baseline: mordesync.Manifest{FileCount: 0, TotalBytes: 0, Files: []mordesync.File{}, EmptyDirectories: []string{}}}
	if err := c.writeLiveState(directory, state); err != nil {
		return liveState{}, err
	}
	return state, nil
}

func (c *Client) runLiveCycleWithRetry(ctx context.Context, options LiveSyncOptions, directory string, state liveState, ackCursor *string) error {
	delay := 500 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		next, result, ran, err := c.liveCycle(ctx, options, directory, state, ackCursor)
		if reportErr := c.reportLiveResult(ctx, options, directory, result, ran, err); reportErr != nil {
			return reportErr
		}
		if err == nil {
			if next.Version != 0 {
				state = next
			}
			return nil
		}
		var conflict *LiveConflictError
		if errors.As(err, &conflict) || !retryableError(err) {
			return err
		}
		if isStaleCursorError(err) {
			return fmt.Errorf("live sync cursor or baseline is no longer valid; refusing an unsafe reset: %w", err)
		}
		if options.Progress != nil {
			options.Progress(Progress{State: "reconnecting", Stage: "sync", ConfirmedRevision: int64Pointer(state.Revision)})
		}
		if err := waitBackoff(ctx, delay); err != nil {
			return err
		}
		if delay < 15*time.Second {
			delay *= 2
		}
		// Read back the durable state on each retry. A commit may have succeeded
		// even when its response was lost, and the state checkpoint may already
		// have been atomically installed.
		if latest, readErr := c.readLiveState(directory, options.WorkspaceID); readErr == nil {
			state = latest
		}
	}
}

func (c *Client) liveCycle(ctx context.Context, options LiveSyncOptions, directory string, state liveState, candidateCursor *string) (liveState, Result, bool, error) {
	if err := ctx.Err(); err != nil {
		return liveState{}, Result{}, false, err
	}
	if pending, err := readResumeMetadata(directory, options.WorkspaceID); err == nil && pending.SessionID != "" && (!isTerminal(pending.State) || pending.State == "completed") {
		if err := c.validatePendingHandoffKey(options, directory, pending); err != nil {
			return liveState{}, Result{}, false, err
		}
		wantPolicy := "live"
		if !state.Initialized {
			wantPolicy = "fill_missing"
		}
		resumePolicy := effectiveSavedPolicy(pending)
		if resumePolicy != wantPolicy {
			// An initial fill-missing session may have checkpointed its live baseline
			// before its external receipt was acknowledged.
			session, err := c.getSession(ctx, options.WorkspaceID, pending.SessionID)
			if err != nil || !state.Initialized || resumePolicy != "fill_missing" || session.State != "completed" {
				return liveState{}, Result{}, false, fmt.Errorf("workspace has an unfinished %s session %s; cancel it before starting live sync", resumePolicy, pending.SessionID)
			}
		}
		result, syncErr := c.sync(ctx, SyncOptions{WorkspaceID: options.WorkspaceID, Directory: directory, Direction: "bidirectional", Policy: resumePolicy,
			Scope: options.Scope, SessionID: pending.SessionID, Baseline: state.Baseline, Progress: options.Progress}, false, true)
		if syncErr != nil {
			return liveState{}, result, true, syncErr
		}
		if result.Session.State == "conflict" || len(result.Conflicts) > 0 {
			return liveState{}, result, true, &LiveConflictError{Session: result.Session, Conflicts: result.Conflicts, Reason: "the server detected concurrent edits"}
		}
		if result.Session.State == "completed" {
			recovered, err := c.checkpointRecoveredLiveSession(ctx, options, directory, state, pending, result)
			if err != nil {
				return liveState{}, result, true, err
			}
			if err := c.reportLiveResult(ctx, options, directory, result, true, nil); err != nil {
				return liveState{}, result, true, err
			}
			return c.liveCycle(ctx, options, directory, recovered, candidateCursor)
		}
		return c.checkpointLiveSession(ctx, options, directory, state, result, cloneStringPointer(pending.AckCursor))
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return liveState{}, Result{}, false, err
	}
	local, err := mordesync.Scan(ctx, directory, nil)
	if err != nil {
		return liveState{}, Result{}, false, fmt.Errorf("scan local live workspace: %w", err)
	}
	local = filterManifest(local, options.Scope)
	latestRevision, err := c.latestRevision(ctx, options.WorkspaceID)
	if err != nil {
		return liveState{}, Result{}, false, err
	}
	remote, err := c.manifest(ctx, options.WorkspaceID, latestRevision)
	if err != nil {
		return liveState{}, Result{}, false, err
	}
	remote = filterManifest(remote, options.Scope)
	changedLocal := !sameManifestContent(local, state.Baseline)
	changedRemote := !sameManifestContentExact(remote, state.Baseline)
	ack := state.Cursor
	if candidateCursor != nil {
		if _, err := decodeLiveCursor(*candidateCursor); err != nil {
			return liveState{}, Result{}, false, errors.New("changes API returned an invalid cursor")
		}
		ack = *candidateCursor
	}
	if state.Initialized && !changedLocal && !changedRemote && ack == state.Cursor {
		return liveState{}, Result{}, false, nil
	}
	policy := "live"
	deleted := missingBaselinePaths(state.Baseline, local, options.Scope)
	deletedDirectories := missingBaselineDirectories(state.Baseline, local, options.Scope)
	if !state.Initialized {
		policy = "fill_missing"
		deleted = nil
		deletedDirectories = nil
	}
	baseRevision := state.Revision
	result, syncErr := c.sync(ctx, SyncOptions{WorkspaceID: options.WorkspaceID, Directory: directory, Direction: "bidirectional", Policy: policy,
		BaseRevision: &baseRevision, Scope: options.Scope, DeletedPaths: deleted, DeletedDirectories: deletedDirectories, AckCursor: &ack, Baseline: state.Baseline,
		IdempotencyNamespace: options.IdempotencyKey, Progress: options.Progress}, false, true)
	if syncErr != nil {
		if errors.Is(syncErr, mordesync.ErrRescanRequired) || errors.Is(syncErr, mordesync.ErrDestinationChange) {
			conflicts := compareManifestConflicts(local, mustScan(ctx, directory, options.Scope))
			if len(conflicts) == 0 {
				conflicts = []Conflict{{Reason: "content_changed"}}
			}
			return liveState{}, result, true, &LiveConflictError{Session: result.Session, Conflicts: conflicts, Reason: "local files changed after the server plan; both versions were preserved"}
		}
		return liveState{}, result, true, syncErr
	}
	if result.Session.State == "conflict" || len(result.Conflicts) > 0 {
		return liveState{}, result, true, &LiveConflictError{Session: result.Session, Conflicts: result.Conflicts, Reason: "the server detected concurrent edits"}
	}
	return c.checkpointLiveSession(ctx, options, directory, state, result, &ack)
}

func (c *Client) validatePendingHandoffKey(options LiveSyncOptions, directory string, pending resumeMetadata) error {
	if options.IdempotencyKey == "" {
		return nil
	}
	separator := strings.LastIndexByte(pending.IdempotencyKey, '.')
	if separator < 1 || !digestPattern.MatchString(pending.IdempotencyKey[separator+1:]) {
		return nil // Existing non-handoff live session; resume by its server session ID.
	}
	if pending.IdempotencyKey[:separator] != options.IdempotencyKey {
		return errors.New("workspace has an unfinished session for another handoff; resume that handoff before starting this one")
	}
	expected, err := derivedCreateKey(options.IdempotencyKey, c.baseURL.String(), directory, options.WorkspaceID,
		pending.FolderID, pending.Direction, effectiveSavedPolicy(pending), pending.BaseRevision, pending.Scope, pending.Manifest,
		pending.DeletedPaths, pending.DeletedDirectories, pending.AckCursor, pending.TaskID, pending.TaskHubOperationKey)
	if err != nil {
		return err
	}
	if expected != pending.IdempotencyKey {
		return errors.New("unfinished handoff session belongs to another Portal or directory; refusing to resume it")
	}
	return nil
}

func (c *Client) checkpointLiveSession(ctx context.Context, options LiveSyncOptions, directory string, previous liveState, result Result, ackCursor *string) (liveState, Result, bool, error) {
	if result.Session.State != "completed" {
		return liveState{}, result, true, fmt.Errorf("live sync session ended in state %q", result.Session.State)
	}
	if result.Session.TargetRevision < previous.Revision {
		return liveState{}, result, true, errors.New("live sync target revision moved behind its common baseline")
	}
	remote, err := c.manifest(ctx, options.WorkspaceID, result.Session.TargetRevision)
	if err != nil {
		return liveState{}, result, true, err
	}
	remote = filterManifest(remote, options.Scope)
	local, err := mordesync.Scan(ctx, directory, nil)
	if err != nil {
		return liveState{}, result, true, err
	}
	local = filterManifest(local, options.Scope)
	conflicts := compareManifestConflicts(local, remote)
	if len(conflicts) > 0 {
		return liveState{}, result, true, &LiveConflictError{Session: result.Session, Conflicts: conflicts, Reason: "workspace changed before a shared baseline could be confirmed"}
	}
	if ackCursor == nil {
		ackCursor = cloneStringPointer(&previous.Cursor)
	}
	if ackCursor == nil {
		return liveState{}, result, true, errors.New("live sync has no durable changes cursor")
	}
	state := liveState{Version: 1, Workspace: options.WorkspaceID, Directory: directory, Scope: append([]string(nil), options.Scope...),
		Initialized: true, Revision: result.Session.TargetRevision, Cursor: *ackCursor, Baseline: remote}
	if err := c.writeLiveState(directory, state); err != nil {
		return liveState{}, result, true, fmt.Errorf("sync committed but baseline checkpoint failed: %w", err)
	}
	return state, result, true, nil
}

func (c *Client) checkpointRecoveredLiveSession(ctx context.Context, options LiveSyncOptions, directory string, previous liveState, pending resumeMetadata, result Result) (liveState, error) {
	if result.Session.ID == "" || result.Session.ID != pending.SessionID {
		return liveState{}, errors.New("resumed live sync returned a different session")
	}
	if result.Session.TargetRevision < previous.Revision {
		return liveState{}, errors.New("recovered live sync target moved behind its common baseline")
	}
	if previous.Initialized && previous.Revision >= result.Session.TargetRevision {
		return previous, nil
	}
	cursor := cloneStringPointer(pending.AckCursor)
	if cursor == nil {
		cursor = cloneStringPointer(&previous.Cursor)
	}
	decoded, err := decodeLiveCursor(*cursor)
	if err != nil || decoded.Revision > result.Session.TargetRevision {
		return liveState{}, errors.New("completed live sync has no valid durable changes cursor")
	}
	remote, err := c.manifest(ctx, options.WorkspaceID, result.Session.TargetRevision)
	if err != nil {
		return liveState{}, err
	}
	remote = filterManifest(remote, options.Scope)
	local, err := mordesync.Scan(ctx, directory, nil)
	if err != nil {
		return liveState{}, err
	}
	local = filterManifest(local, options.Scope)
	if conflicts := recoveredSessionConflicts(filterManifest(pending.Baseline, options.Scope), filterManifest(pending.Manifest, options.Scope), local, remote); len(conflicts) > 0 {
		return liveState{}, &LiveConflictError{Session: result.Session, Conflicts: conflicts, Reason: "completed session contains unresolved concurrent edits; both versions were preserved"}
	}
	state := liveState{Version: 1, Workspace: options.WorkspaceID, Directory: directory, Scope: append([]string(nil), options.Scope...),
		Initialized: true, Revision: result.Session.TargetRevision, Cursor: *cursor, Baseline: remote}
	if err := c.writeLiveState(directory, state); err != nil {
		return liveState{}, fmt.Errorf("sync committed but recovered baseline checkpoint failed: %w", err)
	}
	return state, nil
}

func (c *Client) reportLiveResult(ctx context.Context, options LiveSyncOptions, directory string, result Result, ran bool, cycleErr error) error {
	if !ran {
		return nil
	}
	if cycleErr == nil && result.Session.State == "completed" && result.Session.ID == "" {
		return errors.New("completed live sync session has no ID")
	}
	// Keep the completed session recoverable until the caller finishes its receipt.
	if options.OnResult != nil && (cycleErr == nil || result.Session.State != "completed") {
		options.OnResult(result)
	}
	if cycleErr != nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if result.Session.State == "completed" {
		if err := removeResumeMetadata(directory, options.WorkspaceID, result.Session.ID); err != nil {
			return fmt.Errorf("live receipt completed but session metadata could not be removed: %w", err)
		}
	}
	return nil
}

func recoveredSessionConflicts(baseline, sessionLocal, currentLocal, remote mordesync.Manifest) []Conflict {
	baseFiles, sessionFiles := manifestFileMap(baseline), manifestFileMap(sessionLocal)
	currentFiles, remoteFiles := manifestFileMap(currentLocal), manifestFileMap(remote)
	paths := make(map[string]struct{}, len(baseFiles)+len(sessionFiles)+len(remoteFiles))
	for _, files := range []map[string]mordesync.File{baseFiles, sessionFiles, remoteFiles} {
		for key := range files {
			paths[key] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(paths))
	for key := range paths {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	var conflicts []Conflict
	for _, key := range ordered {
		base, baseOK := baseFiles[key]
		started, startedOK := sessionFiles[key]
		latest, latestOK := remoteFiles[key]
		if sameManifestFile(started, startedOK, base, baseOK) || sameManifestFile(latest, latestOK, base, baseOK) || sameManifestFile(started, startedOK, latest, latestOK) {
			continue
		}
		current, currentOK := currentFiles[key]
		if sameManifestFile(current, currentOK, latest, latestOK) {
			continue
		}
		name := key
		if currentOK {
			name = current.Path
		} else if latestOK {
			name = latest.Path
		}
		reason := "content_changed"
		if currentOK && latestOK && current.Path != latest.Path && strings.EqualFold(current.Path, latest.Path) {
			reason = "case_collision"
			name = latest.Path
		}
		conflicts = append(conflicts, Conflict{Path: name, Reason: reason, LocalSHA256: fileSHA256(current, currentOK), RemoteSHA256: fileSHA256(latest, latestOK)})
	}
	return conflicts
}

func sameManifestFile(left mordesync.File, leftOK bool, right mordesync.File, rightOK bool) bool {
	return leftOK == rightOK && (!leftOK || (left.Path == right.Path && manifestFileKind(left) == manifestFileKind(right) && left.SymlinkTarget == right.SymlinkTarget && left.HardlinkTo == right.HardlinkTo &&
		left.Size == right.Size && left.SHA256 == right.SHA256 && left.Mode == right.Mode &&
		(!left.ModTimeKnown || !right.ModTimeKnown || left.ModTimeNS == right.ModTimeNS)))
}

func manifestFileKind(file mordesync.File) string {
	if file.Kind == "" {
		return mordesync.FileKindRegular
	}
	return file.Kind
}

func fileSHA256(file mordesync.File, exists bool) *string {
	if !exists {
		return nil
	}
	return &file.SHA256
}

func (c *Client) getChanges(ctx context.Context, workspaceID, after string, waitSeconds, limit int) (workspaceChangePage, error) {
	if _, err := decodeLiveCursor(after); err != nil {
		return workspaceChangePage{}, errors.New("persisted workspace changes cursor is invalid")
	}
	query := url.Values{"after": []string{after}, "wait_seconds": []string{fmt.Sprint(waitSeconds)}, "limit": []string{fmt.Sprint(limit)}}
	var page workspaceChangePage
	err := c.requestJSON(ctx, http.MethodGet, c.workspacePath(workspaceID)+"/changes?"+query.Encode(), "", nil, http.StatusOK, &page)
	if err != nil {
		return workspaceChangePage{}, err
	}
	if page.Items == nil || len(page.Items) > limit || len(page.NextCursor) == 0 || len(page.NextCursor) > 512 {
		return workspaceChangePage{}, errors.New("Morde API returned an invalid workspace changes page")
	}
	if _, err := decodeLiveCursor(page.NextCursor); err != nil {
		return workspaceChangePage{}, errors.New("Morde API returned an invalid workspace changes cursor")
	}
	for _, item := range page.Items {
		if item.Revision < 1 || item.Path == "" || !fs.ValidPath(item.Path) || path.Clean(item.Path) != item.Path || strings.ContainsRune(item.Path, '\\') ||
			(item.Action != "upsert" && item.Action != "delete" && item.Action != "directory_upsert" && item.Action != "directory_delete") {
			return workspaceChangePage{}, errors.New("Morde API returned an invalid workspace change")
		}
		if item.Action == "delete" || item.Action == "directory_delete" || item.Action == "directory_upsert" {
			if item.File != nil {
				return workspaceChangePage{}, errors.New("Morde API returned file data for a directory or delete change")
			}
			if item.Action == "directory_upsert" && item.Path != "." {
				if err := mordesync.ValidateDirectoryPathList([]string{item.Path}); err != nil {
					return workspaceChangePage{}, errors.New("Morde API returned an invalid empty-directory change path")
				}
			} else if item.Action == "directory_delete" {
				if err := mordesync.ValidateDirectoryPathList([]string{item.Path}); err != nil {
					return workspaceChangePage{}, errors.New("Morde API returned an invalid empty-directory change path")
				}
			}
		} else if item.File == nil || item.File.Path != item.Path {
			return workspaceChangePage{}, errors.New("Morde API omitted the file for an upsert change")
		} else if _, err := toCoreFile(*item.File); err != nil {
			return workspaceChangePage{}, err
		}
	}
	return page, nil
}

type liveCursor struct {
	Revision int64  `json:"revision"`
	Path     string `json:"path"`
}

func encodeLiveCursor(cursor liveCursor) string {
	data, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeLiveCursor(value string) (liveCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(value) > 512 {
		return liveCursor{}, errors.New("invalid workspace changes cursor")
	}
	var cursor liveCursor
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.Revision < 0 || (cursor.Path != "" && (!fs.ValidPath(cursor.Path) || path.Clean(cursor.Path) != cursor.Path || strings.ContainsRune(cursor.Path, '\\'))) {
		return liveCursor{}, errors.New("invalid workspace changes cursor")
	}
	return cursor, nil
}

func (c *Client) liveStateFilename(directory, workspaceID string) (string, error) {
	filename, err := resumeFilename(directory, workspaceID)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(filename, ".json") + ".live.json", nil
}

func (c *Client) readLiveState(directory, workspaceID string) (liveState, error) {
	filename, err := c.liveStateFilename(directory, workspaceID)
	if err != nil {
		return liveState{}, err
	}
	root, err := os.OpenRoot(filepath.Dir(filename))
	if err != nil {
		return liveState{}, err
	}
	defer root.Close()
	name := filepath.Base(filename)
	info, err := root.Lstat(name)
	if err != nil {
		return liveState{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return liveState{}, errors.New("live sync state is not a private regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return liveState{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxJSONBytes+1))
	if err != nil || len(data) > maxJSONBytes {
		return liveState{}, errors.New("live sync state cannot be read safely")
	}
	var state liveState
	if err := json.Unmarshal(data, &state); err != nil {
		return liveState{}, errors.New("live sync state is invalid")
	}
	if state.Version != 1 || state.Workspace != workspaceID || state.Directory != directory || state.Revision < 0 || state.Cursor == "" ||
		mordesync.ValidateManifest(state.Baseline) != nil {
		return liveState{}, errors.New("live sync state does not match this workspace and directory")
	}
	if _, err := decodeLiveCursor(state.Cursor); err != nil {
		return liveState{}, err
	}
	return state, nil
}

func (c *Client) writeLiveState(directory string, state liveState) error {
	filename, err := c.liveStateFilename(directory, state.Workspace)
	if err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(data) > maxJSONBytes {
		return errors.New("live sync state exceeds the size limit")
	}
	root, err := os.OpenRoot(filepath.Dir(filename))
	if err != nil {
		return err
	}
	defer root.Close()
	name := filepath.Base(filename)
	if info, statErr := root.Lstat(name); statErr == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("refusing to replace a non-regular live sync state")
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	key, err := randomIdempotencyKey()
	if err != nil {
		return err
	}
	temp := "." + name + "." + key + ".tmp"
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	keep := true
	defer func() {
		_ = file.Close()
		if keep {
			_ = root.Remove(temp)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := root.Rename(temp, name); err != nil {
		return err
	}
	keep = false
	directoryFile, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directoryFile.Close()
	return directoryFile.Sync()
}

func filterManifest(manifest mordesync.Manifest, scope []string) mordesync.Manifest {
	if len(scope) == 0 {
		return manifest
	}
	filtered := mordesync.Manifest{Files: make([]mordesync.File, 0, len(manifest.Files)), EmptyDirectories: make([]string, 0, len(manifest.EmptyDirectories)), Directories: make([]mordesync.Directory, 0, len(manifest.Directories))}
	for _, file := range manifest.Files {
		if inScope(file.Path, scope) {
			filtered.Files = append(filtered.Files, file)
			filtered.TotalBytes += file.Size
		}
	}
	for _, directory := range manifest.EmptyDirectories {
		if inScope(directory, scope) {
			filtered.EmptyDirectories = append(filtered.EmptyDirectories, directory)
		}
	}
	for _, directory := range manifest.Directories {
		if mordesync.InDirectoryScope(directory.Path, scope) {
			filtered.Directories = append(filtered.Directories, directory)
		}
	}
	filtered.FileCount = int64(len(filtered.Files))
	return filtered
}

func sameManifestContent(left, right mordesync.Manifest) bool {
	leftFiles, rightFiles := manifestFileMap(left), manifestFileMap(right)
	if len(leftFiles) != len(rightFiles) {
		return false
	}
	for key, file := range leftFiles {
		other, exists := rightFiles[key]
		if !exists || file.Size != other.Size || file.SHA256 != other.SHA256 || file.Mode != other.Mode || file.Path != other.Path ||
			manifestFileKind(file) != manifestFileKind(other) || file.SymlinkTarget != other.SymlinkTarget || file.HardlinkTo != other.HardlinkTo ||
			file.ModTimeKnown && other.ModTimeKnown && file.ModTimeNS != other.ModTimeNS {
			return false
		}
		if other.MetadataKnown && (!file.MetadataKnown || !sameMetadata(file.Xattrs, file.ACL, other.Xattrs, other.ACL)) {
			return false
		}
	}
	leftDirectories, rightDirectories := emptyDirectoryMap(left), emptyDirectoryMap(right)
	if len(leftDirectories) != len(rightDirectories) {
		return false
	}
	for key, directory := range leftDirectories {
		if other, exists := rightDirectories[key]; !exists || other != directory {
			return false
		}
	}
	leftMetadata, rightMetadata := mordesync.DirectoryMetadataMap(left.Directories), mordesync.DirectoryMetadataMap(right.Directories)
	for key, expected := range rightMetadata {
		actual, exists := leftMetadata[key]
		if !exists || actual.Path != expected.Path || expected.ModeKnown && (!actual.ModeKnown || actual.Mode != expected.Mode) ||
			expected.ModTimeKnown && (!actual.ModTimeKnown || actual.ModTimeNS != expected.ModTimeNS) ||
			expected.MetadataKnown && (!actual.MetadataKnown || !sameMetadata(actual.Xattrs, actual.ACL, expected.Xattrs, expected.ACL)) {
			return false
		}
	}
	return true
}

// sameManifestContentExact compares two snapshots, including whether metadata
// is known. sameManifestContent is directional: it is used to verify a local
// scan against a remote manifest whose legacy metadata may be unknown.
func sameManifestContentExact(left, right mordesync.Manifest) bool {
	if !sameManifestContent(left, right) || !sameManifestContent(right, left) {
		return false
	}
	leftFiles, rightFiles := manifestFileMap(left), manifestFileMap(right)
	for key, file := range leftFiles {
		other := rightFiles[key]
		if file.ModTimeKnown != other.ModTimeKnown || file.ModTimeKnown && file.ModTimeNS != other.ModTimeNS ||
			file.MetadataKnown != other.MetadataKnown || !sameMetadata(file.Xattrs, file.ACL, other.Xattrs, other.ACL) {
			return false
		}
	}
	leftDirectories, rightDirectories := mordesync.DirectoryMetadataMap(left.Directories), mordesync.DirectoryMetadataMap(right.Directories)
	if len(leftDirectories) != len(rightDirectories) {
		return false
	}
	for key, directory := range leftDirectories {
		other, exists := rightDirectories[key]
		if !exists || directory.Path != other.Path || directory.ModeKnown != other.ModeKnown || directory.ModeKnown && directory.Mode != other.Mode ||
			directory.ModTimeKnown != other.ModTimeKnown || directory.ModTimeKnown && directory.ModTimeNS != other.ModTimeNS ||
			directory.MetadataKnown != other.MetadataKnown || !sameMetadata(directory.Xattrs, directory.ACL, other.Xattrs, other.ACL) {
			return false
		}
	}
	return true
}

func sameMetadata(left []mordesync.ExtendedAttribute, leftACL *mordesync.NativeACL, right []mordesync.ExtendedAttribute, rightACL *mordesync.NativeACL) bool {
	if len(left) != len(right) || (leftACL == nil) != (rightACL == nil) {
		return false
	}
	for index := range left {
		if left[index].Name != right[index].Name || !bytes.Equal(left[index].Value, right[index].Value) {
			return false
		}
	}
	return leftACL == nil || leftACL.Platform == rightACL.Platform && leftACL.Format == rightACL.Format && bytes.Equal(leftACL.Data, rightACL.Data)
}

func emptyDirectoryMap(manifest mordesync.Manifest) map[string]string {
	directories := make(map[string]string, len(manifest.EmptyDirectories))
	for _, directory := range manifest.EmptyDirectories {
		directories[strings.ToLower(directory)] = directory
	}
	return directories
}

// manifestDirectoryPathSet includes explicit empty directories and the
// implicit parents required by files or nested empty directories.
func manifestDirectoryPathSet(manifest mordesync.Manifest) map[string]struct{} {
	directories := make(map[string]struct{}, len(manifest.Files)+len(manifest.EmptyDirectories)+len(manifest.Directories))
	addParents := func(value string, includeSelf bool) {
		for index := 0; index < len(value); index++ {
			if value[index] == '/' {
				directories[strings.ToLower(value[:index])] = struct{}{}
			}
		}
		if includeSelf {
			directories[strings.ToLower(value)] = struct{}{}
		}
	}
	for _, file := range manifest.Files {
		addParents(file.Path, false)
	}
	for _, directory := range manifest.EmptyDirectories {
		addParents(directory, true)
	}
	for _, directory := range manifest.Directories {
		addParents(directory.Path, true)
	}
	return directories
}

func compareManifestConflicts(local, remote mordesync.Manifest) []Conflict {
	left, right := manifestFileMap(local), manifestFileMap(remote)
	paths := make(map[string]struct{}, len(left)+len(right))
	for key := range left {
		paths[key] = struct{}{}
	}
	for key := range right {
		paths[key] = struct{}{}
	}
	ordered := make([]string, 0, len(paths))
	for key := range paths {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	var conflicts []Conflict
	for _, key := range ordered {
		localFile, localOK := left[key]
		remoteFile, remoteOK := right[key]
		if sameManifestFile(localFile, localOK, remoteFile, remoteOK) {
			continue
		}
		name := key
		if localOK {
			name = localFile.Path
		} else if remoteOK {
			name = remoteFile.Path
		}
		reason := "content_changed"
		if localOK && remoteOK && localFile.Path != remoteFile.Path && strings.EqualFold(localFile.Path, remoteFile.Path) {
			reason = "case_collision"
			name = remoteFile.Path
		}
		conflicts = append(conflicts, Conflict{Path: name, Reason: reason, LocalSHA256: func() *string {
			if localOK {
				return &localFile.SHA256
			}
			return nil
		}(), RemoteSHA256: func() *string {
			if remoteOK {
				return &remoteFile.SHA256
			}
			return nil
		}()})
	}
	leftDirectories, rightDirectories := emptyDirectoryMap(local), emptyDirectoryMap(remote)
	directoryKeys := make(map[string]struct{}, len(leftDirectories)+len(rightDirectories))
	for key := range leftDirectories {
		directoryKeys[key] = struct{}{}
	}
	for key := range rightDirectories {
		directoryKeys[key] = struct{}{}
	}
	orderedDirectories := make([]string, 0, len(directoryKeys))
	for key := range directoryKeys {
		orderedDirectories = append(orderedDirectories, key)
	}
	sort.Strings(orderedDirectories)
	seenConflictPaths := make(map[string]struct{}, len(conflicts))
	for _, conflict := range conflicts {
		seenConflictPaths[strings.ToLower(conflict.Path)] = struct{}{}
	}
	for _, key := range orderedDirectories {
		localPath, localOK := leftDirectories[key]
		remotePath, remoteOK := rightDirectories[key]
		if localOK == remoteOK && (!localOK || localPath == remotePath) {
			continue
		}
		name := key
		if localOK {
			name = localPath
		} else if remoteOK {
			name = remotePath
		}
		if _, exists := seenConflictPaths[key]; exists {
			continue
		}
		reason := "content_changed"
		if localOK && remoteOK && localPath != remotePath {
			reason = "case_collision"
			name = remotePath
		}
		conflicts = append(conflicts, Conflict{Path: name, Reason: reason})
		seenConflictPaths[key] = struct{}{}
	}
	return conflicts
}

func missingBaselinePaths(baseline, local mordesync.Manifest, scope []string) []string {
	localFiles := manifestFileMap(local)
	var deleted []string
	for _, file := range baseline.Files {
		if len(scope) > 0 && !inScope(file.Path, scope) {
			continue
		}
		if _, exists := localFiles[strings.ToLower(file.Path)]; !exists {
			deleted = append(deleted, file.Path)
		}
	}
	sort.Strings(deleted)
	return deleted
}

func missingBaselineDirectories(baseline, local mordesync.Manifest, scope []string) []string {
	localDirectories := emptyDirectoryMap(local)
	localDirectoryPaths := manifestDirectoryPathSet(local)
	var deleted []string
	for _, directory := range baseline.EmptyDirectories {
		if len(scope) > 0 && !inScope(directory, scope) {
			continue
		}
		if _, exists := localDirectories[strings.ToLower(directory)]; exists {
			continue
		}
		// A formerly empty directory that now contains descendants is still a
		// directory in the final tree; the new file/child manifest describes it.
		if _, stillStructural := localDirectoryPaths[strings.ToLower(directory)]; stillStructural {
			continue
		}
		deleted = append(deleted, directory)
	}
	sort.Strings(deleted)
	return deleted
}

func validateScope(scope []string) error {
	seen := make(map[string]struct{}, len(scope))
	for _, value := range scope {
		if value == "" || !fs.ValidPath(value) || path.Clean(value) != value || strings.ContainsRune(value, '\\') {
			return errors.New("live sync scope contains an invalid relative path")
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			return errors.New("live sync scope contains a duplicate path")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func watchWorkspace(ctx context.Context, directory string) (<-chan struct{}, <-chan error, func(), error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, nil, err
	}
	events := make(chan struct{}, 1)
	errorsOut := make(chan error, 1)
	addTree := func(start string) error {
		return filepath.WalkDir(start, func(current string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
				return nil
			}
			if err := watcher.Add(current); err != nil {
				return err
			}
			return nil
		})
	}
	if err := addTree(directory); err != nil {
		_ = watcher.Close()
		return nil, nil, nil, fmt.Errorf("watch local workspace directories: %w", err)
	}
	watchCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer close(events)
		defer close(errorsOut)
		for {
			select {
			case <-watchCtx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Create != 0 {
					info, statErr := os.Lstat(event.Name)
					if statErr == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
						if watchErr := addTree(event.Name); watchErr != nil {
							select {
							case errorsOut <- watchErr:
							default:
							}
						}
					}
				}
				select {
				case events <- struct{}{}:
				default:
				}
			case watchErr, ok := <-watcher.Errors:
				if !ok {
					return
				}
				select {
				case errorsOut <- watchErr:
				default:
				}
			}
		}
	}()
	closeFn := func() {
		cancel()
		_ = watcher.Close()
	}
	return events, errorsOut, closeFn, nil
}

func retryableError(err error) bool {
	if errors.Is(err, mordesync.ErrRescanRequired) {
		return true
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Status == http.StatusRequestTimeout || httpErr.Status == http.StatusTooManyRequests || httpErr.Status >= 500 ||
			httpErr.Status == http.StatusConflict && httpErr.Code == "workspace_busy"
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return strings.Contains(err.Error(), "Morde API request:")
}

func isStaleCursorError(err error) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && httpErr.Status == http.StatusConflict && httpErr.Code == "workspace_conflict"
}

func waitBackoff(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func mustScan(ctx context.Context, directory string, scope []string) mordesync.Manifest {
	manifest, err := mordesync.Scan(ctx, directory, nil)
	if err != nil {
		return mordesync.Manifest{}
	}
	return filterManifest(manifest, scope)
}
