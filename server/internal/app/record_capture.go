package app

import (
	"context"
	"encoding/json"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/record"
)

type RecordCaptureStore interface {
	EnqueueRecordCapture(context.Context, identity.Principal, record.CaptureInput) (record.QueueItem, error)
	ClaimRecordCaptures(context.Context, string, time.Time, time.Duration, int32) ([]record.QueueItem, error)
	CommitRecordCapture(context.Context, string, string, int64) (record.RecordMetadata, error)
	FailRecordCapture(context.Context, string, string, int64, string) error
}

type RecordCaptureStatusStore interface {
	ListRecordCaptureStatuses(context.Context, identity.Principal, record.CaptureStatusQuery) (record.CaptureStatusPage, error)
}

type RecordCaptureFailureStore interface {
	RejectRecordCapture(context.Context, identity.Principal, string, record.Purpose, record.ItemKind) (record.QueueItem, error)
}

type CloudDSHCaptureCandidate struct {
	Principal      identity.Principal
	TaskID         string
	Objective      string
	Result         string
	ConsentVersion int64
}

type RecordCaptureRecoveryStore interface {
	ListRecoverableCloudDSHCaptures(context.Context, int32) ([]CloudDSHCaptureCandidate, error)
}

type RecordCaptureConsentFenceStore interface {
	EnqueueRecordCaptureAtConsentVersion(context.Context, identity.Principal, record.CaptureInput, int64) (record.QueueItem, error)
}

type RecordCaptureService struct {
	store RecordCaptureStore
}

func NewRecordCaptureService(store RecordCaptureStore) (*RecordCaptureService, error) {
	if store == nil {
		return nil, domain.InvalidArgument("record_capture", "record capture store is required")
	}
	return &RecordCaptureService{store: store}, nil
}

// Capture returns an empty queue item without error when that purpose is disabled.
func (s *RecordCaptureService) Capture(ctx context.Context, principal identity.Principal, input record.CaptureInput) (record.QueueItem, error) {
	if err := authorizeRecordOwner(principal); err != nil {
		return record.QueueItem{}, err
	}
	if err := record.ValidateCapture(input); err != nil {
		if domain.ValidTaskID(input.TaskID) && input.Purpose.Valid() && input.Kind.Valid() &&
			((input.Purpose == record.PurposeFullRecord && input.Kind == record.ItemTurn) ||
				(input.Purpose == record.PurposeTrainingContribution && input.Kind == record.ItemSample)) {
			if store, ok := s.store.(RecordCaptureFailureStore); ok {
				_, _ = store.RejectRecordCapture(ctx, principal, input.TaskID, input.Purpose, input.Kind)
			}
		}
		return record.QueueItem{}, err
	}
	return s.store.EnqueueRecordCapture(ctx, principal, input)
}

func (s *RecordCaptureService) RecoverCloudDSHCaptures(ctx context.Context, limit int32) error {
	// shortcut: recovery needs the completed task event retained; use an outbox if retention can expire before the worker runs.
	if limit < 1 || limit > 100 {
		return domain.InvalidArgument("record_capture_recovery", "record capture recovery limit is invalid")
	}
	recoveryStore, ok := s.store.(RecordCaptureRecoveryStore)
	if !ok {
		return domain.NewError(domain.CodeInternal, "record capture recovery is unavailable")
	}
	fencedStore, ok := s.store.(RecordCaptureConsentFenceStore)
	if !ok {
		return domain.NewError(domain.CodeInternal, "record capture consent fencing is unavailable")
	}
	candidates, err := recoveryStore.ListRecoverableCloudDSHCaptures(ctx, limit)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		payload, err := json.Marshal(struct {
			Objective string `json:"objective"`
			Result    string `json:"result"`
		}{Objective: candidate.Objective, Result: candidate.Result})
		if err != nil {
			return err
		}
		_, err = fencedStore.EnqueueRecordCaptureAtConsentVersion(ctx, candidate.Principal, record.CaptureInput{
			Purpose: record.PurposeFullRecord, Kind: record.ItemTurn, TaskID: candidate.TaskID, Payload: payload,
		}, candidate.ConsentVersion)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *RecordCaptureService) Statuses(ctx context.Context, principal identity.Principal, query record.CaptureStatusQuery) (record.CaptureStatusPage, error) {
	if err := authorizeRecordOwner(principal); err != nil {
		return record.CaptureStatusPage{}, err
	}
	if query.Limit == 0 {
		query.Limit = 50
	}
	if query.Limit < 1 || query.Limit > 100 ||
		(query.BeforeID != "" && !record.ValidQueueItemID(query.BeforeID)) {
		return record.CaptureStatusPage{}, domain.InvalidArgument("record_capture_status", "capture status query is invalid")
	}
	store, ok := s.store.(RecordCaptureStatusStore)
	if !ok {
		return record.CaptureStatusPage{}, domain.NewError(domain.CodeInternal, "record capture status store is unavailable")
	}
	return store.ListRecordCaptureStatuses(ctx, principal, query)
}

func (s *RecordCaptureService) Claim(ctx context.Context, worker string, now time.Time, lease time.Duration, limit int32) ([]record.QueueItem, error) {
	if !validRecordWorker(worker) || now.IsZero() || lease <= 0 || lease > 5*time.Minute || limit < 1 || limit > 100 {
		return nil, domain.InvalidArgument("record_capture_lease", "record capture lease is invalid")
	}
	return s.store.ClaimRecordCaptures(ctx, worker, now, lease, limit)
}

func (s *RecordCaptureService) Commit(ctx context.Context, queueID, worker string, leaseVersion int64) (record.RecordMetadata, error) {
	if !record.ValidQueueItemID(queueID) || !validRecordWorker(worker) || leaseVersion < 1 {
		return record.RecordMetadata{}, domain.InvalidArgument("record_capture_lease", "record capture lease is invalid")
	}
	return s.store.CommitRecordCapture(ctx, queueID, worker, leaseVersion)
}

func (s *RecordCaptureService) Fail(ctx context.Context, queueID, worker string, leaseVersion int64, failureCode string) error {
	if !record.ValidQueueItemID(queueID) || !validRecordWorker(worker) || leaseVersion < 1 ||
		!validRecordFailureCode(failureCode) {
		return domain.InvalidArgument("record_capture_failure", "record capture failure is invalid")
	}
	return s.store.FailRecordCapture(ctx, queueID, worker, leaseVersion, failureCode)
}

func validRecordWorker(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char > 0x7e {
			return false
		}
	}
	return true
}

func validRecordFailureCode(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}
