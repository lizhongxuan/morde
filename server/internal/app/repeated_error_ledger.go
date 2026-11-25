package app

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"morde/server/internal/domain"
)

// Repeated-error sedimentation (P3-6, design proposal §16.7): the second
// time the same error appears, there must be a record of it, queryable,
// and convertible into a check item. Evidence is the record plus the task
// ledger at the second occurrence.
//
// What exists already, and what this file adds: E09 derives repetition
// inside one task from stored steps (DeriveTaskValidationCorrectionFacts)
// and its own comment says there is no corrections table in this base, so
// a correction settled into a reusable lint, check or skill cannot be
// verified there. This ledger is the missing record: it keys errors by a
// normalized signature, counts occurrences across tasks, and converts a
// repeated error into a check item value. It is in-memory and process
// local — durable storage (a corrections table), the feed wiring (which
// call site records into it), and the consumer of the check item are all
// pending decisions, recorded in the gapfix notes, not guessed here.
//
// Two feeds share the ledger. The tool feed records stored step failures
// (the E09 definition of repeated: the same (tool, failure reason)
// failing twice). The model feed records model failures for the P3-1
// ladder: cross-pause attempt counting reads TaskAttempts, so a pause
// after a resume sees the attempts that happened before it. Neither feed
// is wired to a live call site in this batch; both are exposed here so
// the wiring only needs the ownership decision, not a new mechanism.

type RepeatedErrorKind string

const (
	// RepeatedErrorKindTool is a stored tool-step failure: Tool is the
	// tool name, Reason is the step failure reason.
	RepeatedErrorKindTool RepeatedErrorKind = "tool"
	// RepeatedErrorKindModel is a model-call failure: Tool is the
	// model-call source (executor, compaction, review), Reason is the
	// ladder reason (rate_limited, timeout, invalid_response, ...).
	RepeatedErrorKindModel RepeatedErrorKind = "model"
)

// RepeatedErrorSignature keys one error. Tool and Reason are stored
// trimmed; an empty Tool or Reason cannot be keyed and is rejected.
type RepeatedErrorSignature struct {
	Kind   RepeatedErrorKind
	Tool   string
	Reason string
}

// RepeatedErrorOccurrence is one appearance of a signature in one task.
// Sequence orders occurrences inside the task: the step sequence for the
// tool feed, the pause index for the model feed.
type RepeatedErrorOccurrence struct {
	TaskID   string
	Sequence int64
	At       time.Time
}

type RepeatedErrorStatus string

const (
	RepeatedErrorStatusOpen      RepeatedErrorStatus = "open"
	RepeatedErrorStatusConverted RepeatedErrorStatus = "converted"
)

// RepeatedErrorRecord is the sedimented record of one signature: every
// occurrence, in first-seen order, plus its conversion state.
type RepeatedErrorRecord struct {
	Signature   RepeatedErrorSignature
	Occurrences []RepeatedErrorOccurrence
	Status      RepeatedErrorStatus
	CheckItemID string
	checkItem   RepeatedErrorCheckItem
}

// Repeated reports whether the error has appeared at least twice.
func (r RepeatedErrorRecord) Repeated() bool {
	return len(r.Occurrences) >= 2
}

// FirstSeenAt returns the earliest occurrence time, or the zero time when
// the record has no occurrences.
func (r RepeatedErrorRecord) FirstSeenAt() time.Time {
	earliest := time.Time{}
	for _, occurrence := range r.Occurrences {
		if earliest.IsZero() || occurrence.At.Before(earliest) {
			earliest = occurrence.At
		}
	}
	return earliest
}

// LastSeenAt returns the latest occurrence time, or the zero time when the
// record has no occurrences.
func (r RepeatedErrorRecord) LastSeenAt() time.Time {
	latest := time.Time{}
	for _, occurrence := range r.Occurrences {
		if latest.IsZero() || occurrence.At.After(latest) {
			latest = occurrence.At
		}
	}
	return latest
}

// TaskAttempts counts occurrences of this record inside one task.
func (r RepeatedErrorRecord) TaskAttempts(taskID string) int {
	count := 0
	for _, occurrence := range r.Occurrences {
		if occurrence.TaskID == taskID {
			count++
		}
	}
	return count
}

// RepeatedErrorCheckItem is what a repeated error converts into: a stable,
// checkable item with a deterministic ID (the same signature always
// converts to the same item, so converting twice cannot duplicate it).
// There is no checks system consuming these items yet; the item is the
// conversion output, and its destination is pending.
type RepeatedErrorCheckItem struct {
	ID          string
	Signature   RepeatedErrorSignature
	Title       string
	FirstTaskID string
	Occurrences int
	CreatedAt   time.Time
}

func repeatedErrorCheckItemID(signature RepeatedErrorSignature) string {
	sum := sha256.Sum256([]byte(string(signature.Kind) + "\x00" + signature.Tool + "\x00" + signature.Reason))
	return "rec-" + hex.EncodeToString(sum[:])[:16]
}

func repeatedErrorCheckItemTitle(signature RepeatedErrorSignature) string {
	if signature.Kind == RepeatedErrorKindModel {
		return "Repeated model error (" + signature.Tool + "): " + signature.Reason
	}
	return "Repeated error in " + signature.Tool + ": " + signature.Reason
}

// RepeatedErrorLedger is the in-memory record of repeated errors. All
// methods are safe for concurrent use.
type RepeatedErrorLedger struct {
	mu      sync.Mutex
	records map[RepeatedErrorSignature]*RepeatedErrorRecord
	order   []RepeatedErrorSignature
}

func NewRepeatedErrorLedger() *RepeatedErrorLedger {
	return &RepeatedErrorLedger{records: make(map[RepeatedErrorSignature]*RepeatedErrorRecord)}
}

func normalizeRepeatedErrorSignature(signature RepeatedErrorSignature) (RepeatedErrorSignature, error) {
	switch signature.Kind {
	case RepeatedErrorKindTool, RepeatedErrorKindModel:
	default:
		return signature, domain.InvalidArgument("kind", "repeated error kind is invalid")
	}
	tool := strings.TrimSpace(signature.Tool)
	reason := strings.TrimSpace(signature.Reason)
	if tool == "" {
		return signature, domain.InvalidArgument("tool", "repeated error tool is required")
	}
	if reason == "" {
		return signature, domain.InvalidArgument("reason", "repeated error reason is required")
	}
	return RepeatedErrorSignature{Kind: signature.Kind, Tool: tool, Reason: reason}, nil
}

func validateRepeatedErrorOccurrence(occurrence RepeatedErrorOccurrence) (RepeatedErrorOccurrence, error) {
	taskID := strings.TrimSpace(occurrence.TaskID)
	if taskID == "" {
		return occurrence, domain.InvalidArgument("task_id", "repeated error task is required")
	}
	if occurrence.Sequence < 1 {
		return occurrence, domain.InvalidArgument("sequence", "repeated error sequence is invalid")
	}
	if occurrence.At.IsZero() {
		return occurrence, domain.InvalidArgument("at", "repeated error time is required")
	}
	occurrence.TaskID = taskID
	return occurrence, nil
}

func copyRepeatedErrorRecord(record *RepeatedErrorRecord) RepeatedErrorRecord {
	copied := *record
	copied.Occurrences = append([]RepeatedErrorOccurrence(nil), record.Occurrences...)
	return copied
}

// Record stores one occurrence of a signature. Recording the same
// (task, sequence) twice is an idempotent replay: the occurrence is kept
// once. The returned record is the state after this call.
func (l *RepeatedErrorLedger) Record(signature RepeatedErrorSignature, occurrence RepeatedErrorOccurrence) (RepeatedErrorRecord, error) {
	signature, err := normalizeRepeatedErrorSignature(signature)
	if err != nil {
		return RepeatedErrorRecord{}, err
	}
	occurrence, err = validateRepeatedErrorOccurrence(occurrence)
	if err != nil {
		return RepeatedErrorRecord{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	record, ok := l.records[signature]
	if !ok {
		record = &RepeatedErrorRecord{Signature: signature, Status: RepeatedErrorStatusOpen}
		l.records[signature] = record
		l.order = append(l.order, signature)
	}
	for _, existing := range record.Occurrences {
		if existing.TaskID == occurrence.TaskID && existing.Sequence == occurrence.Sequence {
			return copyRepeatedErrorRecord(record), nil
		}
	}
	record.Occurrences = append(record.Occurrences, occurrence)
	return copyRepeatedErrorRecord(record), nil
}

// RecordTaskSteps is the tool feed: it records every failed step of one
// task into the ledger and returns the records that are repeated after
// this call, in first-seen order. Failed steps with no tool name, no
// failure reason, or no usable sequence cannot be keyed; they are counted
// in skipped, not silently dropped and not recorded under a blank key.
func (l *RepeatedErrorLedger) RecordTaskSteps(taskID string, steps []TaskValidationStepRecord, at time.Time) ([]RepeatedErrorRecord, int, error) {
	if strings.TrimSpace(taskID) == "" {
		return nil, 0, domain.InvalidArgument("task_id", "repeated error task is required")
	}
	if at.IsZero() {
		return nil, 0, domain.InvalidArgument("at", "repeated error time is required")
	}
	var repeated []RepeatedErrorRecord
	seen := make(map[RepeatedErrorSignature]struct{})
	skipped := 0
	for _, step := range steps {
		if step.Status != "failed" {
			continue
		}
		if step.ToolName == "" || step.FailureReason == "" || step.Sequence < 1 {
			skipped++
			continue
		}
		record, err := l.Record(RepeatedErrorSignature{Kind: RepeatedErrorKindTool, Tool: step.ToolName, Reason: step.FailureReason},
			RepeatedErrorOccurrence{TaskID: taskID, Sequence: step.Sequence, At: at})
		if err != nil {
			return nil, 0, err
		}
		if record.Repeated() {
			if _, ok := seen[record.Signature]; !ok {
				seen[record.Signature] = struct{}{}
				repeated = append(repeated, record)
			}
		}
	}
	return repeated, skipped, nil
}

// RecordModelFailure is the model feed for the P3-1 ladder: it records one
// model failure (source is the model-call source, reason is the ladder
// reason) and returns the record after this call. TaskAttempts on the
// returned record is the cross-pause attempt count for this task.
func (l *RepeatedErrorLedger) RecordModelFailure(taskID, source, reason string, sequence int64, at time.Time) (RepeatedErrorRecord, error) {
	return l.Record(RepeatedErrorSignature{Kind: RepeatedErrorKindModel, Tool: source, Reason: reason},
		RepeatedErrorOccurrence{TaskID: taskID, Sequence: sequence, At: at})
}

// Find returns a copy of the record for a signature, if one was ever
// recorded.
func (l *RepeatedErrorLedger) Find(signature RepeatedErrorSignature) (RepeatedErrorRecord, bool) {
	signature, err := normalizeRepeatedErrorSignature(signature)
	if err != nil {
		return RepeatedErrorRecord{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	record, ok := l.records[signature]
	if !ok {
		return RepeatedErrorRecord{}, false
	}
	return copyRepeatedErrorRecord(record), true
}

// IsRepeated reports whether a signature has appeared at least twice.
func (l *RepeatedErrorLedger) IsRepeated(signature RepeatedErrorSignature) bool {
	record, ok := l.Find(signature)
	return ok && record.Repeated()
}

// TaskAttempts counts occurrences of a signature inside one task: the
// cross-pause attempt count the P3-1 ladder needs. Zero when the signature
// was never recorded.
func (l *RepeatedErrorLedger) TaskAttempts(taskID string, signature RepeatedErrorSignature) int {
	record, ok := l.Find(signature)
	if !ok {
		return 0
	}
	return record.TaskAttempts(taskID)
}

// List returns copies of all records in first-seen order.
func (l *RepeatedErrorLedger) List() []RepeatedErrorRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	records := make([]RepeatedErrorRecord, 0, len(l.order))
	for _, signature := range l.order {
		records = append(records, copyRepeatedErrorRecord(l.records[signature]))
	}
	return records
}

// ConvertToCheckItem converts a repeated error into its check item. It
// fails closed: an unrecorded signature is not found, and a single
// occurrence is not a repeated error, so neither converts. Converting an
// already converted error returns the same item instead of duplicating it.
func (l *RepeatedErrorLedger) ConvertToCheckItem(signature RepeatedErrorSignature, at time.Time) (RepeatedErrorCheckItem, error) {
	signature, err := normalizeRepeatedErrorSignature(signature)
	if err != nil {
		return RepeatedErrorCheckItem{}, err
	}
	if at.IsZero() {
		return RepeatedErrorCheckItem{}, domain.InvalidArgument("at", "repeated error check time is required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	record, ok := l.records[signature]
	if !ok {
		return RepeatedErrorCheckItem{}, domain.NewError(domain.CodeNotFound, "repeated error is not recorded")
	}
	if !record.Repeated() {
		return RepeatedErrorCheckItem{}, domain.NewError(domain.CodeInvalidState, "repeated error has only one occurrence")
	}
	if record.Status == RepeatedErrorStatusConverted {
		return record.checkItem, nil
	}
	item := RepeatedErrorCheckItem{
		ID:          repeatedErrorCheckItemID(signature),
		Signature:   signature,
		Title:       repeatedErrorCheckItemTitle(signature),
		FirstTaskID: record.Occurrences[0].TaskID,
		Occurrences: len(record.Occurrences),
		CreatedAt:   at,
	}
	record.Status = RepeatedErrorStatusConverted
	record.CheckItemID = item.ID
	record.checkItem = item
	return item, nil
}
