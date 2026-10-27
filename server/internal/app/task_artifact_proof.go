package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

// TaskArtifactProof identifies a file in one immutable, committed Workspace revision.
type TaskArtifactProof struct {
	OwnerID     string
	WorkspaceID string
	Revision    int64
	Path        string
	SizeBytes   int64
	SHA256      string
}

// VerifyCommittedArtifact checks expected metadata against an immutable manifest
// and actually reads all referenced blocks. The caller must independently bind
// these facts to the authorized Task and step in its completion transaction.
func (s *WorkspaceSyncService) VerifyCommittedArtifact(ctx context.Context, expected TaskArtifactProof) (proof TaskArtifactProof, retErr error) {
	if ctx == nil || !domain.ValidUserID(expected.OwnerID) || !domain.ValidWorkspaceID(expected.WorkspaceID) ||
		expected.Revision < 0 || expected.SizeBytes < 0 || !validSyncDigest(expected.SHA256) ||
		workspace.ValidateRelativePath(expected.Path) != nil {
		return TaskArtifactProof{}, domain.InvalidArgument("artifact", "workspace artifact proof is invalid")
	}
	if err := ctx.Err(); err != nil {
		return TaskArtifactProof{}, err
	}
	value, err := s.store.GetWorkspace(ctx, expected.WorkspaceID)
	if err != nil {
		return TaskArtifactProof{}, err
	}
	if value.ID != expected.WorkspaceID || value.OwnerID != expected.OwnerID || value.State != workspace.StateReady || value.DeletedAt != nil {
		return TaskArtifactProof{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	lock, err := s.lockWorkspaceBlockStore(ctx, expected.WorkspaceID, true)
	if err != nil {
		return TaskArtifactProof{}, err
	}
	defer func() {
		if err := lock.Release(ctx); err != nil {
			proof = TaskArtifactProof{}
			retErr = errors.Join(retErr, fmt.Errorf("release workspace block store read lock: %w", err))
		}
	}()
	value, err = s.store.GetWorkspace(ctx, expected.WorkspaceID)
	if err != nil {
		return TaskArtifactProof{}, err
	}
	if value.ID != expected.WorkspaceID || value.OwnerID != expected.OwnerID || value.State != workspace.StateReady || value.DeletedAt != nil {
		return TaskArtifactProof{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	if expected.Revision > value.Revision {
		return TaskArtifactProof{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace revision is not committed")
	}
	page, err := s.store.GetWorkspaceManifestPage(ctx, expected.WorkspaceID, expected.Revision, expected.Path, "", 2)
	if err != nil {
		return TaskArtifactProof{}, err
	}
	if page.WorkspaceID != expected.WorkspaceID || page.Revision != expected.Revision {
		return TaskArtifactProof{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace manifest revision does not match the requested proof")
	}
	if len(page.Files) == 0 || page.Files[0].Path != expected.Path {
		return TaskArtifactProof{}, domain.NewError(domain.CodeNotFound, "workspace artifact not found")
	}
	if len(page.Files) != 1 || page.NextCursor != nil || workspace.ValidateFile(page.Files[0]) != nil {
		return TaskArtifactProof{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace artifact manifest is invalid")
	}
	file := page.Files[0]
	if file.SizeBytes != expected.SizeBytes || file.SHA256 != expected.SHA256 {
		return TaskArtifactProof{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace artifact does not match the expected file")
	}
	hasher := sha256.New()
	var total int64
	for _, chunk := range file.Chunks {
		if err := ctx.Err(); err != nil {
			return TaskArtifactProof{}, err
		}
		data, err := readVerifiedBlock(s.blocks, expected.WorkspaceID, chunk)
		if err != nil {
			return TaskArtifactProof{}, fmt.Errorf("read workspace artifact block: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return TaskArtifactProof{}, err
		}
		_, _ = hasher.Write(data)
		total += int64(len(data))
	}
	if total != expected.SizeBytes || hex.EncodeToString(hasher.Sum(nil)) != expected.SHA256 {
		return TaskArtifactProof{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace artifact content does not match the expected file")
	}
	if err := ctx.Err(); err != nil {
		return TaskArtifactProof{}, err
	}
	return expected, nil
}
