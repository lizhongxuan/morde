package app

import (
	"context"
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
		return record.QueueItem{}, err
	}
	return s.store.EnqueueRecordCapture(ctx, principal, input)
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
