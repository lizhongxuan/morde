package postgres

import (
	"bytes"
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

// ConfirmCloudDSHPersonalMutationReceipt reads the operation event and the
// mutation receipt under the recovered Agent and current authorization locks.
// It never performs or retries a personal-data mutation.
func (s *Store) ConfirmCloudDSHPersonalMutationReceipt(ctx context.Context, request app.CloudDSHPersonalReceiptRequest) error {
	fence, commit := request.Fence, request.Commit
	if ctx == nil || fence.Validate() != nil || request.ThroughSequence < 0 || len(request.CheckpointDigest) != 32 ||
		!app.ValidCloudDSHPersonalMutationCommit(commit) || commit.TaskID != fence.TaskID || commit.OwnerID != fence.OwnerID ||
		fence.InitiatorID != fence.OwnerID || commit.AgentSessionID != fence.AgentSessionID || commit.AuthorizationVersion != fence.AuthorizationVersion {
		return domain.InvalidArgument("personal_receipt", "personal write receipt identity is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := confirmCloudDSHPersonalReceiptFence(ctx, tx, fence, request.ThroughSequence, request.CheckpointDigest); err != nil {
			return err
		}
		event, found, err := findCloudDSHPersonalMutationEvent(ctx, tx, fence.TaskID, commit.CallID)
		if err != nil {
			return err
		}
		if !found || app.ValidateCloudDSHPersonalMutationEvent(event.Payload, commit) != nil {
			return domain.NewError(domain.CodeInvalidState, "personal write Task receipt is unavailable")
		}
		instanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		receipt, found, err := loadPersonalMutationReceipt(ctx, tx, instanceID, commit.Mutation)
		if err != nil {
			return err
		}
		if !found || receipt.ResultRevision != commit.Mutation.BaseRevision+1 {
			return domain.NewError(domain.CodeInvalidState, "personal write mutation receipt is unavailable")
		}
		return nil
	})
}

func confirmCloudDSHPersonalReceiptFence(ctx context.Context, tx pgx.Tx, fence app.AgentInstanceFence, through int64, digest []byte) error {
	current, err := lockAuthorizedAgentTask(ctx, tx, fence)
	if err != nil {
		return err
	}
	if current.ProjectID != "" || current.InitiatorID != fence.OwnerID || current.InstanceID != fence.AgentInstanceID ||
		current.Generation != fence.AgentGeneration || current.SessionID != fence.AgentSessionID ||
		current.EngineVersion != fence.EngineVersion || current.ProfileVersion != fence.ProfileVersion ||
		current.CheckpointThrough != through || !bytes.Equal(current.CheckpointDigest, digest) {
		return domain.NewError(domain.CodeUnauthenticated, "personal write checkpoint changed")
	}
	if err := lockActiveAgentLease(ctx, tx, fence); err != nil {
		return err
	}
	var currentRevision bool
	if err := tx.QueryRow(ctx, `SELECT current_revision=desired_revision FROM tasks WHERE id=$1`, fence.TaskID).Scan(&currentRevision); err != nil {
		return fmt.Errorf("check personal write Task revision: %w", err)
	}
	if !currentRevision {
		return domain.NewError(domain.CodeInvalidState, "personal write Task revision changed")
	}
	return nil
}

// ConfirmCloudDSHGoalMutationReceipt verifies the pinned Goal call against
// both its atomic Task event and the account mutation ledger. It never retries
// a mutation or loads the Goal body, which may have changed since the call.
func (s *Store) ConfirmCloudDSHGoalMutationReceipt(ctx context.Context, request app.CloudDSHGoalReceiptRequest) (app.CloudDSHGoalReceipt, error) {
	fence := request.Fence
	if ctx == nil || fence.Validate() != nil || fence.InitiatorID != fence.OwnerID ||
		request.ThroughSequence < 0 || len(request.CheckpointDigest) != 32 {
		return app.CloudDSHGoalReceipt{}, domain.InvalidArgument("goal_receipt", "goal write receipt identity is invalid")
	}
	var result app.CloudDSHGoalReceipt
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := confirmCloudDSHPersonalReceiptFence(ctx, tx, fence, request.ThroughSequence, request.CheckpointDigest); err != nil {
			return err
		}
		event, found, err := findCloudDSHPersonalMutationEvent(ctx, tx, fence.TaskID, request.CallID)
		if err != nil {
			return err
		}
		if !found {
			return domain.NewError(domain.CodeInvalidState, "goal write Task receipt is unavailable")
		}
		result, err = app.ValidateCloudDSHGoalReceiptEvent(event.Payload, request)
		if err != nil {
			return err
		}
		instanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		receipt, found, err := loadPersonalMutationReceipt(ctx, tx, instanceID, result.Mutation)
		if err != nil {
			return err
		}
		if !found || receipt.ResultRevision != request.BaseRevision+1 {
			return domain.NewError(domain.CodeInvalidState, "goal write mutation receipt is unavailable")
		}
		return nil
	})
	if err != nil {
		return app.CloudDSHGoalReceipt{}, err
	}
	return result, nil
}
