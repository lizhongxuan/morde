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
	"morde/server/internal/domain/workspace"
)

var taskWorkspaceViewIDPattern = regexp.MustCompile(`^v[a-f0-9]{64}$`)

type TaskWorkspaceBlockReader func(context.Context, string) ([]byte, error)

func ValidateTaskWorkspacePublishCommand(value TaskWorkspacePublishCommand) error {
	if !domain.ValidUserID(value.OwnerID) || !domain.ValidTaskID(value.TaskID) || !domain.ValidWorkspaceID(value.WorkspaceID) ||
		!domain.ValidHostID(value.HostID) || value.LeaseOwner == "" || len(value.LeaseOwner) > 255 || value.LeaseVersion < 1 ||
		value.BindingVersion < 1 || value.InputRevision < 0 || value.BaseRevision < value.InputRevision ||
		value.OperationKey == "" || len(value.OperationKey) > 255 || len(value.RequestHash) != sha256.Size ||
		value.HostFence.RuntimeGeneration < 1 || value.HostFence.RuntimeInstanceID == "" || value.HostFence.ControlVersion < 0 || !value.HostFence.AIAllowed ||
		!taskWorkspaceViewIDPattern.MatchString(value.ViewID) || !validSyncDigest(value.InputManifestSHA256) {
		return domain.InvalidArgument("task_workspace_publish", "Task Workspace publish fields are invalid")
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

func (s *WorkspaceSyncService) CommitTaskWorkspacePublish(ctx context.Context, value TaskWorkspacePublishCommand, files []workspace.File, manifestSHA string, readBlock TaskWorkspaceBlockReader) (receipt TaskWorkspacePublishReceipt, retErr error) {
	if err := ValidateTaskWorkspacePublishCommand(value); err != nil {
		return TaskWorkspacePublishReceipt{}, err
	}
	if readBlock == nil || !validSyncDigest(manifestSHA) || len(files) > workspace.MaxSyncFiles {
		return TaskWorkspacePublishReceipt{}, domain.InvalidArgument("task_workspace_publish", "Task Workspace output manifest is invalid")
	}
	actualManifestSHA, err := workspace.ManifestSHA256(files)
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
			return taskWorkspacePublishReceiptResult(receipt)
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
	receipt, err = s.store.CommitTaskWorkspacePublish(ctx, value, files, manifestSHA)
	if err != nil {
		return TaskWorkspacePublishReceipt{}, err
	}
	return taskWorkspacePublishReceiptResult(receipt)
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
