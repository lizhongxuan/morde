package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	mordesync "morde-sync"
)

// ConfirmDownloadedLiveBaseline records a common live-sync baseline only
// after a download left the local tree identical to the requested revision.
func (c *Client) ConfirmDownloadedLiveBaseline(ctx context.Context, workspaceID, directory string, revision int64) error {
	if ctx == nil || !workspaceIDPattern.MatchString(workspaceID) || directory == "" || revision < 0 {
		return errors.New("context, valid workspace ID, directory, and revision are required")
	}
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		return fmt.Errorf("resolve downloaded workspace directory: %w", err)
	}
	releaseOperation, err := acquireOperationLock(ctx, canonical)
	if err != nil {
		return err
	}
	defer releaseOperation()
	if _, err := c.readLiveState(canonical, workspaceID); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := requireHandoffRevision(ctx, c, workspaceID, revision); err != nil {
		return err
	}
	remote, err := c.manifest(ctx, workspaceID, revision)
	if err != nil {
		return err
	}
	local, err := scanHandoffWorkspace(ctx, canonical, nil)
	if err != nil {
		return err
	}
	if !sameManifestContent(local, remote) {
		return &LiveConflictError{Reason: "downloaded Host workspace differs from the server revision", Conflicts: compareManifestConflicts(local, remote)}
	}
	if err := requireHandoffRevision(ctx, c, workspaceID, revision); err != nil {
		return err
	}
	local, err = scanHandoffWorkspace(ctx, canonical, nil)
	if err != nil {
		return err
	}
	if !sameManifestContent(local, remote) {
		return &LiveConflictError{Reason: "downloaded Host workspace changed during verification", Conflicts: compareManifestConflicts(local, remote)}
	}
	state := liveState{Version: 1, Workspace: workspaceID, Directory: canonical, Initialized: true,
		Revision: revision, Cursor: encodeLiveCursor(liveCursor{Revision: revision}), Baseline: remote}
	if err := c.writeLiveState(canonical, state); err != nil {
		return err
	}
	return requireHandoffRevision(ctx, c, workspaceID, revision)
}

// HandoffCheckpoint converges a workspace using its persisted live-sync
// baseline, then verifies one stable local/remote checkpoint and returns its
// server revision. It does not pause local applications or prevent the server
// from changing after return.
func (c *Client) HandoffCheckpoint(ctx context.Context, options LiveSyncOptions) (int64, error) {
	if ctx == nil || !workspaceIDPattern.MatchString(options.WorkspaceID) || options.Directory == "" {
		return 0, errors.New("context, valid workspace ID, and local directory are required")
	}
	if len(options.Scope) > 1000 {
		return 0, errors.New("live sync scope has too many entries")
	}
	if err := validateScope(options.Scope); err != nil {
		return 0, err
	}
	if options.IdempotencyKey != "" && (len(options.IdempotencyKey) > 63 || !idempotencyPattern.MatchString(options.IdempotencyKey)) {
		return 0, errors.New("handoff idempotency key is invalid")
	}
	directory, err := canonicalDirectory(options.Directory)
	if err != nil {
		return 0, fmt.Errorf("resolve handoff workspace directory: %w", err)
	}
	releaseOperation, err := acquireOperationLock(ctx, directory)
	if err != nil {
		return 0, err
	}
	defer releaseOperation()
	state, err := c.loadOrCreateLiveState(ctx, options, directory)
	if err != nil {
		return 0, err
	}
	if !equalStrings(state.Scope, options.Scope) {
		return 0, errors.New("handoff scope differs from the persisted live-sync baseline")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	previousRevision, err := c.latestRevision(ctx, options.WorkspaceID)
	if err != nil {
		return 0, err
	}
	next, result, ran, err := c.liveCycle(ctx, options, directory, state, nil)
	if reportErr := c.reportLiveResult(ctx, options, directory, result, ran, err); reportErr != nil {
		return 0, reportErr
	}
	if err != nil {
		return 0, err
	}
	targetRevision := previousRevision
	if ran {
		targetRevision = result.Session.TargetRevision
	}
	state, err = c.readLiveState(directory, options.WorkspaceID)
	if err != nil {
		return 0, err
	}
	if next.Version != 0 && (next.Revision != state.Revision || next.Cursor != state.Cursor) {
		return 0, errors.New("live-sync checkpoint changed while handoff was running")
	}
	if ran && state.Revision != targetRevision {
		return 0, errors.New("handoff live-sync baseline does not match its target revision")
	}
	if err := requireHandoffRevision(ctx, c, options.WorkspaceID, targetRevision); err != nil {
		return 0, err
	}

	// A sync that uploads local changes may leave its own change events after
	// the persisted cursor. Acknowledge those through the same live-sync cycle.
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		page, err := c.getChanges(ctx, options.WorkspaceID, state.Cursor, 0, changesPageLimit)
		if err != nil {
			return 0, err
		}
		if err := validateHandoffChangePage(state.Cursor, page, targetRevision); err != nil {
			return 0, err
		}
		if len(page.Items) == 0 {
			break
		}
		next, result, ran, err = c.liveCycle(ctx, options, directory, state, &page.NextCursor)
		if reportErr := c.reportLiveResult(ctx, options, directory, result, ran, err); reportErr != nil {
			return 0, reportErr
		}
		if err != nil {
			return 0, err
		}
		if !ran || result.Session.TargetRevision != targetRevision {
			return 0, fmt.Errorf("workspace revision advanced while acknowledging handoff changes: base=%d target=%d expected=%d", result.Session.BaseRevision, result.Session.TargetRevision, targetRevision)
		}
		state, err = c.readLiveState(directory, options.WorkspaceID)
		if err != nil {
			return 0, err
		}
		if next.Version == 0 || next.Revision != targetRevision || next.Cursor != page.NextCursor || state.Revision != targetRevision || state.Cursor != page.NextCursor {
			return 0, errors.New("handoff changes cursor was not durably checkpointed")
		}
		if err := requireHandoffRevision(ctx, c, options.WorkspaceID, targetRevision); err != nil {
			return 0, err
		}
	}

	state, err = c.readLiveState(directory, options.WorkspaceID)
	if err != nil {
		return 0, err
	}
	cursor, err := decodeLiveCursor(state.Cursor)
	if err != nil {
		return 0, err
	}
	if state.Revision != targetRevision || cursor.Revision != targetRevision {
		return 0, errors.New("handoff checkpoint revision or changes cursor is stale")
	}
	remote, err := c.manifest(ctx, options.WorkspaceID, targetRevision)
	if err != nil {
		return 0, err
	}
	remote = filterManifest(remote, options.Scope)
	if !sameManifestContentExact(remote, state.Baseline) {
		return 0, errors.New("handoff server manifest differs from the persisted common baseline")
	}
	local, err := scanHandoffWorkspace(ctx, directory, options.Scope)
	if err != nil {
		return 0, err
	}
	if !sameManifestContent(local, remote) {
		if sameManifestContentIgnoringDirectoryTimes(local, remote) {
			if err := applyHandoffDirectoryTimes(ctx, directory, local, remote); err != nil {
				return 0, err
			}
			local, err = scanHandoffWorkspace(ctx, directory, options.Scope)
			if err != nil {
				return 0, err
			}
		}
		if !sameManifestContent(local, remote) {
			return 0, &LiveConflictError{Reason: "local and server files differ at the handoff checkpoint", Conflicts: compareManifestConflicts(local, remote)}
		}
	}
	localAgain, err := scanHandoffWorkspace(ctx, directory, options.Scope)
	if err != nil {
		return 0, err
	}
	if !sameManifestContentExact(local, localAgain) {
		return 0, errors.New("local workspace changed during handoff verification")
	}
	if err := requireHandoffRevision(ctx, c, options.WorkspaceID, targetRevision); err != nil {
		return 0, err
	}
	page, err := c.getChanges(ctx, options.WorkspaceID, state.Cursor, 0, changesPageLimit)
	if err != nil {
		return 0, err
	}
	if err := validateHandoffChangePage(state.Cursor, page, targetRevision); err != nil {
		return 0, err
	}
	if len(page.Items) != 0 {
		return 0, errors.New("workspace revision advanced during handoff verification")
	}
	if err := requireHandoffRevision(ctx, c, options.WorkspaceID, targetRevision); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return targetRevision, nil
}

func sameManifestContentIgnoringDirectoryTimes(left, right mordesync.Manifest) bool {
	left.Directories = append([]mordesync.Directory(nil), left.Directories...)
	right.Directories = append([]mordesync.Directory(nil), right.Directories...)
	for index := range left.Directories {
		left.Directories[index].ModTimeKnown = false
	}
	for index := range right.Directories {
		right.Directories[index].ModTimeKnown = false
	}
	return sameManifestContentExact(left, right)
}

func applyHandoffDirectoryTimes(ctx context.Context, directory string, local, remote mordesync.Manifest) error {
	localDirectories := mordesync.DirectoryMetadataMap(local.Directories)
	directories := append([]mordesync.Directory(nil), remote.Directories...)
	sort.Slice(directories, func(i, j int) bool {
		if directories[i].Path == "." {
			return false
		}
		if directories[j].Path == "." {
			return true
		}
		depthI, depthJ := strings.Count(directories[i].Path, "/"), strings.Count(directories[j].Path, "/")
		if depthI != depthJ {
			return depthI > depthJ
		}
		return directories[i].Path < directories[j].Path
	})
	for _, desired := range directories {
		actual, exists := localDirectories[strings.ToLower(desired.Path)]
		if !exists || !actual.ModTimeKnown || !desired.ModTimeKnown || actual.ModTimeNS == desired.ModTimeNS {
			continue
		}
		result, err := mordesync.ApplyDirectoryMetadata(ctx, directory, &actual, desired, false)
		if err != nil {
			return err
		}
		if result.Status == mordesync.ApplyConflict {
			return mordesync.ErrDestinationChange
		}
	}
	return nil
}

func requireHandoffRevision(ctx context.Context, c *Client, workspaceID string, expected int64) error {
	actual, err := c.latestRevision(ctx, workspaceID)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("workspace revision advanced during handoff: expected %d, found %d", expected, actual)
	}
	return nil
}

func validateHandoffChangePage(after string, page workspaceChangePage, targetRevision int64) error {
	previous, err := decodeLiveCursor(after)
	if err != nil {
		return errors.New("handoff persisted changes cursor is invalid")
	}
	if _, err := decodeLiveCursor(page.NextCursor); err != nil {
		return errors.New("handoff changes API returned an invalid cursor")
	}
	for _, item := range page.Items {
		if item.Revision > targetRevision || item.Revision < previous.Revision || (item.Revision == previous.Revision && item.Path <= previous.Path) {
			return errors.New("handoff changes API returned an out-of-order change")
		}
		previous = liveCursor{Revision: item.Revision, Path: item.Path}
	}
	if len(page.Items) == 0 {
		if page.NextCursor != after {
			return errors.New("handoff changes API advanced its cursor without returning changes")
		}
		return nil
	}
	next, _ := decodeLiveCursor(page.NextCursor)
	if next != previous {
		return errors.New("handoff changes cursor does not match the returned page")
	}
	return nil
}

func scanHandoffWorkspace(ctx context.Context, directory string, scope []string) (mordesync.Manifest, error) {
	manifest, err := mordesync.Scan(ctx, directory, nil)
	if err != nil {
		return mordesync.Manifest{}, fmt.Errorf("scan local handoff workspace: %w", err)
	}
	return filterManifest(manifest, scope), nil
}
