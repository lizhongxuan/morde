package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"

	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/workspace"
)

var taskWorkspaceViewIDPattern = regexp.MustCompile(`^v[a-f0-9]{64}$`)

type TaskWorkspaceBlockReader func(context.Context, string) ([]byte, error)

func ValidateTaskWorkspacePublishCommand(value TaskWorkspacePublishCommand) error {
	hostless := value.HostID == ""
	if !domain.ValidUserID(value.OwnerID) || !domain.ValidTaskID(value.TaskID) || !domain.ValidWorkspaceID(value.WorkspaceID) ||
		(!hostless && !domain.ValidHostID(value.HostID)) || value.LeaseOwner == "" || len(value.LeaseOwner) > 255 || value.LeaseVersion < 1 ||
		value.InputRevision < 0 || value.BaseRevision < value.InputRevision ||
		value.OperationKey == "" || len(value.OperationKey) > 255 || len(value.RequestHash) != sha256.Size ||
		!validSyncDigest(value.InputManifestSHA256) {
		return domain.InvalidArgument("task_workspace_publish", "Task Workspace publish fields are invalid")
	}
	if value.ProjectID != "" && (!project.ValidID(value.ProjectID) || !domain.ValidUserID(value.InitiatorID) || !domain.ValidUserID(value.WorkspaceOwnerID)) {
		return domain.InvalidArgument("task_workspace_publish", "project Workspace publish identity is invalid")
	}
	if hostless {
		if value.BindingVersion != 0 || value.HostFence != (AgentHostFence{}) || value.ViewID != "" {
			return domain.InvalidArgument("task_workspace_publish", "Hostless Task Workspace publish must not include Host binding fields")
		}
		return nil
	}
	if value.BindingVersion < 1 || value.HostFence.RuntimeGeneration < 1 || value.HostFence.RuntimeInstanceID == "" ||
		value.HostFence.ControlVersion < 0 || !value.HostFence.AIAllowed || !taskWorkspaceViewIDPattern.MatchString(value.ViewID) {
		return domain.InvalidArgument("task_workspace_publish", "Task Workspace publish Host fence is invalid")
	}
	return nil
}

func (s *WorkspaceSyncService) ReserveTaskWorkspacePublish(ctx context.Context, value TaskWorkspacePublishCommand) (TaskWorkspacePublishReceipt, error) {
	if err := ValidateTaskWorkspacePublishCommand(value); err != nil {
		return TaskWorkspacePublishReceipt{}, err
	}
	return s.store.ReserveTaskWorkspacePublish(ctx, value)
}

func (s *WorkspaceSyncService) GetTaskWorkspacePublishReceipt(ctx context.Context, value TaskWorkspacePublishCommand) (TaskWorkspacePublishReceipt, error) {
	if err := ValidateTaskWorkspacePublishCommand(value); err != nil {
		return TaskWorkspacePublishReceipt{}, err
	}
	return s.store.GetTaskWorkspacePublishReceipt(ctx, value)
}

func (s *WorkspaceSyncService) FindTaskWorkspacePublishReceipt(ctx context.Context, ownerID, taskID, workspaceID, operationKey string) (TaskWorkspacePublishReceipt, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !domain.ValidWorkspaceID(workspaceID) || operationKey == "" || len(operationKey) > 255 {
		return TaskWorkspacePublishReceipt{}, domain.InvalidArgument("task_workspace_publish", "Task Workspace publish receipt identity is invalid")
	}
	return s.store.FindTaskWorkspacePublishReceipt(ctx, ownerID, taskID, workspaceID, operationKey)
}

type projectTaskWorkspacePublishGuard interface {
	WithProjectTaskWorkspacePublish(context.Context, TaskWorkspacePublishCommand, func(context.Context) error) error
}

func (s *WorkspaceSyncService) CommitTaskWorkspacePublish(ctx context.Context, value TaskWorkspacePublishCommand, files []workspace.File, emptyDirectories []string, directories []workspace.Directory, manifestSHA string, readBlock TaskWorkspaceBlockReader) (TaskWorkspacePublishReceipt, error) {
	guard, ok := s.store.(projectTaskWorkspacePublishGuard)
	if !ok {
		if value.ProjectID != "" {
			return TaskWorkspacePublishReceipt{}, domain.NewError(domain.CodeForbidden, "project Workspace publish authorization is unavailable")
		}
		return s.commitTaskWorkspacePublish(ctx, value, files, emptyDirectories, directories, manifestSHA, readBlock)
	}
	var receipt TaskWorkspacePublishReceipt
	err := guard.WithProjectTaskWorkspacePublish(ctx, value, func(guardedCtx context.Context) error {
		var err error
		receipt, err = s.commitTaskWorkspacePublish(guardedCtx, value, files, emptyDirectories, directories, manifestSHA, readBlock)
		return err
	})
	return receipt, err
}

func (s *WorkspaceSyncService) commitTaskWorkspacePublish(ctx context.Context, value TaskWorkspacePublishCommand, files []workspace.File, emptyDirectories []string, directories []workspace.Directory, manifestSHA string, readBlock TaskWorkspaceBlockReader) (receipt TaskWorkspacePublishReceipt, retErr error) {
	if err := ValidateTaskWorkspacePublishCommand(value); err != nil {
		return TaskWorkspacePublishReceipt{}, err
	}
	if readBlock == nil || !validSyncDigest(manifestSHA) || len(files)+max(len(emptyDirectories), len(directories)) > workspace.MaxSyncFiles {
		return TaskWorkspacePublishReceipt{}, domain.InvalidArgument("task_workspace_publish", "Task Workspace output manifest is invalid")
	}
	actualManifestSHA, err := workspace.ManifestTreeWithMetadataSHA256(files, emptyDirectories, directories)
	if err != nil || actualManifestSHA != manifestSHA {
		return TaskWorkspacePublishReceipt{}, domain.NewError(domain.CodeWorkspaceConflict, "Task Workspace output manifest digest changed")
	}
	chunks := make(map[string]int64)
	for _, file := range files {
		if err := workspace.ValidateFile(file); err != nil || file.SizeBytes > workspace.MaxSyncFileBytes {
			return TaskWorkspacePublishReceipt{}, domain.InvalidArgument("task_workspace_publish", "Task Workspace output file is invalid")
		}
		for _, chunk := range file.Chunks {
			if previous, exists := chunks[chunk.SHA256]; exists && previous != chunk.SizeBytes {
				return TaskWorkspacePublishReceipt{}, domain.InvalidArgument("task_workspace_publish", "Task Workspace output blocks disagree")
			}
			chunks[chunk.SHA256] = chunk.SizeBytes
		}
	}
	if receipt, err := s.GetTaskWorkspacePublishReceipt(ctx, value); err == nil {
		if receipt.State != TaskWorkspacePublishPending {
			return s.finishTaskWorkspacePublish(ctx, value.WorkspaceID, receipt)
		}
	} else if domain.CodeOf(err) != domain.CodeNotFound {
		return TaskWorkspacePublishReceipt{}, err
	}
	blockLock, err := s.lockWorkspaceBlockStore(ctx, value.WorkspaceID, false)
	if err != nil {
		return TaskWorkspacePublishReceipt{}, err
	}
	defer func() {
		if err := blockLock.Release(ctx); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release workspace block store publish lock: %w", err))
		}
	}()
	reservations := make([]WorkspaceBlockReservation, 0, len(chunks))
	digests := make([]string, 0, len(chunks))
	for digest, size := range chunks {
		reserved, err := s.blocks.ReservationBytes(size)
		if err != nil || reserved <= 0 {
			return TaskWorkspacePublishReceipt{}, domain.NewError(domain.CodeRequestTooLarge, "Task Workspace output block is invalid")
		}
		reservations = append(reservations, WorkspaceBlockReservation{WorkspaceID: value.WorkspaceID, Digest: digest, ReservedBytes: reserved})
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	if err := s.store.ReserveTaskWorkspaceBlockUsage(ctx, value.WorkspaceID, reservations, s.maxOwnerBytes); err != nil {
		if domain.CodeOf(err) == domain.CodeQuotaExceeded {
			if rejectErr := s.store.RejectTaskWorkspacePublishQuota(ctx, value); rejectErr != nil {
				return TaskWorkspacePublishReceipt{}, errors.Join(err, rejectErr)
			}
		}
		return TaskWorkspacePublishReceipt{}, err
	}
	for _, digest := range digests {
		if err := ctx.Err(); err != nil {
			return TaskWorkspacePublishReceipt{}, err
		}
		if _, err := readVerifiedBlock(s.blocks, value.WorkspaceID, workspace.Chunk{SHA256: digest, SizeBytes: chunks[digest]}); err == nil {
			allocatedBytes, err := s.blocks.BlockAllocatedBytes(value.WorkspaceID, digest)
			if err != nil {
				return TaskWorkspacePublishReceipt{}, fmt.Errorf("inspect existing Task Workspace output block: %w", err)
			}
			if err := s.store.FinalizeWorkspaceBlockUsage(ctx, value.WorkspaceID, digest, allocatedBytes, s.maxOwnerBytes); err != nil {
				if domain.CodeOf(err) == domain.CodeQuotaExceeded {
					if rejectErr := s.store.RejectTaskWorkspacePublishQuota(ctx, value); rejectErr != nil {
						return TaskWorkspacePublishReceipt{}, errors.Join(err, rejectErr)
					}
				}
				return TaskWorkspacePublishReceipt{}, err
			}
			continue
		} else if domain.CodeOf(err) != domain.CodeNotFound {
			return TaskWorkspacePublishReceipt{}, fmt.Errorf("verify existing Task Workspace output block: %w", err)
		}
		data, err := readBlock(ctx, digest)
		if err != nil {
			return TaskWorkspacePublishReceipt{}, fmt.Errorf("read Task Workspace output block: %w", err)
		}
		if int64(len(data)) != chunks[digest] || sha256Hex(data) != digest {
			return TaskWorkspacePublishReceipt{}, domain.NewError(domain.CodeWorkspaceConflict, "Task Workspace output block failed integrity verification")
		}
		written, err := s.blocks.PutBlock(value.WorkspaceID, digest, bytes.NewReader(data))
		if err != nil || written != chunks[digest] {
			if err == nil {
				err = io.ErrShortWrite
			}
			return TaskWorkspacePublishReceipt{}, fmt.Errorf("store Task Workspace output block: %w", err)
		}
		allocatedBytes, err := s.blocks.BlockAllocatedBytes(value.WorkspaceID, digest)
		if err != nil {
			return TaskWorkspacePublishReceipt{}, fmt.Errorf("inspect Task Workspace output block: %w", err)
		}
		if err := s.store.FinalizeWorkspaceBlockUsage(ctx, value.WorkspaceID, digest, allocatedBytes, s.maxOwnerBytes); err != nil {
			if domain.CodeOf(err) == domain.CodeQuotaExceeded {
				if rejectErr := s.store.RejectTaskWorkspacePublishQuota(ctx, value); rejectErr != nil {
					return TaskWorkspacePublishReceipt{}, errors.Join(err, rejectErr)
				}
			}
			return TaskWorkspacePublishReceipt{}, err
		}
	}
	for _, file := range files {
		if file.Kind == "symlink" {
			continue // ValidateFile authenticates the link target; reading content blocks would follow the wrong model.
		}
		hasher := sha256.New()
		var total int64
		for _, chunk := range file.Chunks {
			data, err := readVerifiedBlock(s.blocks, value.WorkspaceID, workspace.Chunk{SHA256: chunk.SHA256, SizeBytes: chunk.SizeBytes})
			if err != nil {
				return TaskWorkspacePublishReceipt{}, fmt.Errorf("verify Task Workspace output file block: %w", err)
			}
			_, _ = hasher.Write(data)
			total += int64(len(data))
		}
		if total != file.SizeBytes || hex.EncodeToString(hasher.Sum(nil)) != file.SHA256 {
			return TaskWorkspacePublishReceipt{}, domain.NewError(domain.CodeWorkspaceConflict, "Task Workspace output file failed integrity verification")
		}
	}
	receipt, err = s.store.CommitTaskWorkspacePublish(ctx, value, files, emptyDirectories, directories, manifestSHA)
	if err != nil {
		return TaskWorkspacePublishReceipt{}, err
	}
	return s.finishTaskWorkspacePublish(ctx, value.WorkspaceID, receipt)
}

func (s *WorkspaceSyncService) finishTaskWorkspacePublish(ctx context.Context, workspaceID string, receipt TaskWorkspacePublishReceipt) (TaskWorkspacePublishReceipt, error) {
	result, err := taskWorkspacePublishReceiptResult(receipt)
	if err != nil {
		return result, err
	}
	if err := s.materializeWorkspaceCurrent(ctx, workspaceID); err != nil {
		if ctx.Err() != nil {
			return TaskWorkspacePublishReceipt{}, ctx.Err()
		}
		return TaskWorkspacePublishReceipt{}, domain.NewError(domain.CodeWorkspaceBusy, "Task Workspace publish is durable but current-directory materialization is pending; retry with the same operation key")
	}
	return result, nil
}

func taskWorkspacePublishReceiptResult(receipt TaskWorkspacePublishReceipt) (TaskWorkspacePublishReceipt, error) {
	switch receipt.State {
	case TaskWorkspacePublishPending:
		return receipt, domain.NewError(domain.CodeWorkspaceBusy, "Task Workspace publish is pending; retry with the same operation key")
	case TaskWorkspacePublishConflict:
		return receipt, domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed after this task checkpoint was confirmed")
	case TaskWorkspacePublishQuotaRejected:
		return receipt, domain.NewError(domain.CodeQuotaExceeded, "Workspace storage quota would be exceeded")
	case TaskWorkspacePublishSucceeded:
		return receipt, nil
	default:
		return TaskWorkspacePublishReceipt{}, domain.NewError(domain.CodeInternal, "Task Workspace publish receipt state is invalid")
	}
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
