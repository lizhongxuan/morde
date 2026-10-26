package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

const (
	workspaceSyncIdleTTL          = 24 * time.Hour
	workspaceBlockGCDeletesPerRun = 256
)

type workspaceBlockGCFiles interface {
	ListBlockNames(workspaceID string) ([]string, error)
	DeleteBlock(workspaceID, digest string) (int64, error)
	RemoveStaleTempBlocks(workspaceID string) error
}

type WorkspaceBlockGCStats struct {
	WorkspacesScanned int64
	SessionsExpired   int64
	BlocksDeleted     int64
	BytesReclaimed    int64
}

// WorkspaceBlockGarbageCollector removes only blocks outside retained
// revisions and active upload sessions. Database and filesystem work is
// serialized with workspace creation, uploads, legacy import, and other GC.
type WorkspaceBlockGarbageCollector struct {
	store  WorkspaceBlockGCStore
	blocks workspaceBlockGCFiles
}

func NewWorkspaceBlockGarbageCollector(store WorkspaceBlockGCStore, blocks workspaceBlockGCFiles) (*WorkspaceBlockGarbageCollector, error) {
	if store == nil || blocks == nil {
		return nil, errors.New("workspace block garbage collection dependencies are required")
	}
	return &WorkspaceBlockGarbageCollector{store: store, blocks: blocks}, nil
}

func (g *WorkspaceBlockGarbageCollector) Collect(ctx context.Context) (WorkspaceBlockGCStats, error) {
	var stats WorkspaceBlockGCStats
	if ctx == nil {
		return stats, errors.New("workspace block garbage collection context is required")
	}
	workspaceIDs, err := g.store.ListWorkspaceIDsForBlockGC(ctx)
	if err != nil {
		return stats, fmt.Errorf("list workspaces for block garbage collection: %w", err)
	}
	cutoff := time.Now().UTC().Add(-workspaceSyncIdleTTL)
	var failures []error
	for _, workspaceID := range workspaceIDs {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			return stats, errors.Join(failures...)
		}
		if !domain.ValidWorkspaceID(workspaceID) {
			failures = append(failures, errors.New("workspace block garbage collection received an invalid workspace id"))
			continue
		}
		if err := g.collectWorkspace(ctx, workspaceID, cutoff, &stats); err != nil {
			failures = append(failures, fmt.Errorf("collect workspace %s blocks: %w", workspaceID, err))
		}
	}
	return stats, errors.Join(failures...)
}

func (g *WorkspaceBlockGarbageCollector) collectWorkspace(ctx context.Context, workspaceID string, cutoff time.Time, stats *WorkspaceBlockGCStats) (retErr error) {
	lock, acquired, err := g.store.TryWorkspaceBlockStoreLock(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("lock workspace block store: %w", err)
	}
	if !acquired {
		return nil
	}
	if lock == nil {
		return errors.New("workspace block store lock was not returned")
	}
	defer func() {
		if err := lock.Release(ctx); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release workspace block store lock: %w", err))
		}
	}()

	expired, err := g.store.ExpireIdleWorkspaceSyncSessions(ctx, workspaceID, cutoff)
	if err != nil {
		return fmt.Errorf("expire idle workspace sync sessions: %w", err)
	}
	stats.SessionsExpired += expired
	err = g.store.WithWorkspaceBlockGC(ctx, workspaceID, func(references []workspace.BlockReference) ([]WorkspaceBlockRemoval, error) {
		if err := g.blocks.RemoveStaleTempBlocks(workspaceID); err != nil {
			return nil, fmt.Errorf("remove abandoned upload temp blocks: %w", err)
		}
		protected := make(map[string]int64, len(references))
		for _, reference := range references {
			if reference.WorkspaceID != workspaceID || !validSyncDigest(reference.SHA256) || reference.SizeBytes <= 0 || reference.SizeBytes > workspace.MaxSyncBlockBytes {
				return nil, errors.New("workspace block protection reference is invalid")
			}
			if size, ok := protected[reference.SHA256]; ok && size != reference.SizeBytes {
				return nil, errors.New("workspace block protection references disagree on size")
			}
			protected[reference.SHA256] = reference.SizeBytes
		}
		names, err := g.blocks.ListBlockNames(workspaceID)
		if err != nil {
			return nil, fmt.Errorf("list workspace block names: %w", err)
		}
		stats.WorkspacesScanned++
		deleted := int64(0)
		removals := make([]WorkspaceBlockRemoval, 0)
		for _, digest := range names {
			if _, ok := protected[digest]; ok {
				continue
			}
			if deleted >= workspaceBlockGCDeletesPerRun {
				break
			}
			removed, err := g.blocks.DeleteBlock(workspaceID, digest)
			if err != nil {
				return removals, fmt.Errorf("delete unreferenced workspace block: %w", err)
			}
			if removed > 0 {
				deleted++
				stats.BlocksDeleted++
				if removed > int64(^uint64(0)>>1)-stats.BytesReclaimed {
					return removals, errors.New("workspace block garbage collection byte count overflow")
				}
				stats.BytesReclaimed += removed
				removals = append(removals, WorkspaceBlockRemoval{Digest: digest, AllocatedBytes: removed})
			}
		}
		return removals, nil
	})
	if err != nil {
		return fmt.Errorf("lock references and collect workspace blocks: %w", err)
	}
	return nil
}
