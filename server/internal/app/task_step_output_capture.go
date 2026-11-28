package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
	"morde/server/internal/runtimeclient"
)

const (
	TaskStepOutputCaptureMaxBytes        = runtimeclient.SupplementalOutputCaptureMaxBytes
	TaskStepOutputCapturePageBytes       = runtimeclient.SupplementalOutputCapturePageBytes
	TaskStepOutputCaptureMaxPages        = TaskStepOutputCaptureMaxBytes / TaskStepOutputCapturePageBytes
	TaskStepOutputCaptureOwnerQuotaBytes = 64 << 20
	TaskStepOutputCaptureCleanupBatch    = 1000
	TaskStepOutputCaptureRetention       = 24 * time.Hour
)

const TaskStepOutputCaptureCleanupInterval = time.Minute

type taskStepOutputCaptureExecutionKey struct{}

// taskStepOutputCaptureExecution is request-local memory only. It is never
// serialized into Runtime operation, DSH checkpoint, or Task result payloads.
type taskStepOutputCaptureExecution struct {
	identity    TaskStepOutputCaptureIdentity
	state       TaskStepOutputCaptureState
	legacyRaw   []byte
	stdoutSpill []byte
	stderrSpill []byte
}

func newTaskStepOutputCaptureExecution(identity TaskStepOutputCaptureIdentity) *taskStepOutputCaptureExecution {
	return &taskStepOutputCaptureExecution{identity: identity, state: TaskStepOutputCaptureStateNotRecorded}
}

func withTaskStepOutputCaptureExecution(ctx context.Context, capture *taskStepOutputCaptureExecution) context.Context {
	if ctx == nil || capture == nil {
		return ctx
	}
	return context.WithValue(ctx, taskStepOutputCaptureExecutionKey{}, capture)
}

func taskStepOutputCaptureExecutionFromContext(ctx context.Context) *taskStepOutputCaptureExecution {
	if ctx == nil {
		return nil
	}
	capture, _ := ctx.Value(taskStepOutputCaptureExecutionKey{}).(*taskStepOutputCaptureExecution)
	return capture
}

func (capture *taskStepOutputCaptureExecution) setStatus(state TaskStepOutputCaptureState) {
	if capture == nil || state == TaskStepOutputCaptureStateComplete || state == TaskStepOutputCaptureStateNotRecorded {
		return
	}
	capture.clearBytes()
	capture.state = state
}

func (capture *taskStepOutputCaptureExecution) setIdentity(identity TaskStepOutputCaptureIdentity) bool {
	if capture == nil || !taskStepOutputCaptureIdentityValid(identity) {
		return false
	}
	capture.identity = identity
	return true
}

// setLegacyRaw stores only the bounded Runtime legacy Shell result in request
// memory so the server can join it with the spill and redact the complete
// stream once. It is never serialized and is cleared with the spill buffers.
func (capture *taskStepOutputCaptureExecution) setLegacyRaw(raw []byte) bool {
	if capture == nil || len(raw) == 0 || len(raw) > maxAgentToolOutputBytes {
		return false
	}
	var shell taskStepShellOutput
	if decodeStrictRuntimeResult(raw, &shell) != nil || len(shell.Stdout)+len(shell.Stderr) > maxAgentToolOutputBytes ||
		!utf8.ValidString(shell.Stdout) || !utf8.ValidString(shell.Stderr) {
		return false
	}
	encoded, err := json.Marshal(shell)
	if err != nil || len(encoded) > maxAgentToolOutputBytes {
		return false
	}
	capture.clearLegacyRaw()
	capture.legacyRaw = encoded
	return true
}

func (capture *taskStepOutputCaptureExecution) setComplete(identity TaskStepOutputCaptureIdentity, stdout, stderr []byte) bool {
	if capture == nil || !taskStepOutputCaptureIdentityValid(identity) || len(stdout) > TaskStepOutputCaptureMaxBytes ||
		len(stderr) > TaskStepOutputCaptureMaxBytes-len(stdout) || !utf8.Valid(stdout) || !utf8.Valid(stderr) ||
		strings.ContainsRune(string(stdout), '\x00') || strings.ContainsRune(string(stderr), '\x00') || len(capture.legacyRaw) == 0 {
		return false
	}
	clear(capture.stdoutSpill)
	clear(capture.stderrSpill)
	capture.identity = identity
	capture.stdoutSpill = stdout
	capture.stderrSpill = stderr
	capture.state = TaskStepOutputCaptureStateComplete
	return true
}

func (capture *taskStepOutputCaptureExecution) take() (TaskStepOutputCaptureIdentity, TaskStepOutputCaptureState, []byte, []byte) {
	if capture == nil {
		return TaskStepOutputCaptureIdentity{}, TaskStepOutputCaptureStateNotRecorded, nil, nil
	}
	identity, state, stdout, stderr := capture.identity, capture.state, capture.stdoutSpill, capture.stderrSpill
	capture.stdoutSpill, capture.stderrSpill = nil, nil
	return identity, state, stdout, stderr
}

func (capture *taskStepOutputCaptureExecution) takeLegacyRaw() []byte {
	if capture == nil {
		return nil
	}
	raw := capture.legacyRaw
	capture.legacyRaw = nil
	return raw
}

func (capture *taskStepOutputCaptureExecution) clearLegacyRaw() {
	if capture == nil {
		return
	}
	clear(capture.legacyRaw)
	capture.legacyRaw = nil
}

func (capture *taskStepOutputCaptureExecution) clearBytes() {
	if capture == nil {
		return
	}
	clear(capture.stdoutSpill)
	clear(capture.stderrSpill)
	capture.clearLegacyRaw()
	capture.stdoutSpill, capture.stderrSpill = nil, nil
}

type TaskStepOutputCaptureState string

const (
	TaskStepOutputCaptureStateComplete          TaskStepOutputCaptureState = "complete"
	TaskStepOutputCaptureStateSourceTruncated   TaskStepOutputCaptureState = "source_truncated"
	TaskStepOutputCaptureStateCapacityTruncated TaskStepOutputCaptureState = "capacity_truncated"
	TaskStepOutputCaptureStateNotRecorded       TaskStepOutputCaptureState = "not_recorded"
	TaskStepOutputCaptureStateFailed            TaskStepOutputCaptureState = "failed"
)

// TaskStepOutputCaptureIdentity binds supplemental output to the exact Task,
// Workspace authorization snapshot, step, operation and original model CallID.
// It intentionally contains no output bytes or Runtime credentials.
type TaskStepOutputCaptureIdentity struct {
	OwnerID                       string `json:"owner_id"`
	TaskID                        string `json:"task_id"`
	AuthorizationVersion          int64  `json:"authorization_version"`
	TaskRevision                  int64  `json:"task_revision"`
	WorkspaceID                   string `json:"workspace_id"`
	WorkspaceAuthorizationVersion int64  `json:"workspace_authorization_version"`
	WorkspaceBindingVersion       int64  `json:"workspace_binding_version"`
	WorkspaceViewID               string `json:"workspace_view_id"`
	WorkspaceCheckpointRevision   int64  `json:"workspace_checkpoint_revision"`
	StepID                        string `json:"step_id"`
	CallID                        string `json:"call_id"`
	OperationID                   string `json:"operation_id"`
}

type TaskStepOutputCapturePageRecord struct {
	Identity       TaskStepOutputCaptureIdentity
	Stream         string
	SourceBytes    int
	TotalBytes     int
	StreamSHA256   []byte
	PageIndex      int
	Offset         int
	PlaintextBytes int
	PageSHA256     []byte
	Ciphertext     []byte
	ExpiresAt      time.Time
}

// TaskStepOutputCaptureStreamRecord stores only encrypted pages and digests of
// the redacted text. SourceBytes is a count, never the original source bytes.
type TaskStepOutputCaptureStreamRecord struct {
	Stream      string
	SourceBytes int
	TotalBytes  int
	SHA256      []byte
	Pages       []TaskStepOutputCapturePageRecord
}

// TaskStepOutputCaptureRecord is the single receipt unit. A PostgreSQL writer
// must save its manifest and all pages atomically. Failed or incomplete
// captures are not represented by this complete-only type.
type TaskStepOutputCaptureRecord struct {
	Identity TaskStepOutputCaptureIdentity
	Stdout   TaskStepOutputCaptureStreamRecord
	Stderr   TaskStepOutputCaptureStreamRecord
}

type TaskStepOutputCapturePage struct {
	Identity    TaskStepOutputCaptureIdentity `json:"-"`
	Stream      string                        `json:"stream"`
	State       TaskStepOutputCaptureState    `json:"state"`
	Offset      int                           `json:"offset"`
	NextOffset  int                           `json:"next_offset"`
	TotalBytes  int                           `json:"total_bytes"`
	SourceBytes int                           `json:"source_bytes"`
	EOF         bool                          `json:"eof"`
	Complete    bool                          `json:"complete"`
	Data        []byte                        `json:"data"`
}

// TaskStepOutputCaptureStatusRecord persists only the identity fence and a
// non-sensitive result state when no complete encrypted receipt can be saved.
// It never contains output bytes, a reason, or page metadata.
type TaskStepOutputCaptureStatusRecord struct {
	Identity  TaskStepOutputCaptureIdentity
	State     TaskStepOutputCaptureState
	ExpiresAt time.Time
}

func validTaskStepOutputCaptureStatusRecord(value TaskStepOutputCaptureStatusRecord) bool {
	return taskStepOutputCaptureIdentityValid(value.Identity) &&
		(value.State == TaskStepOutputCaptureStateSourceTruncated ||
			value.State == TaskStepOutputCaptureStateCapacityTruncated || value.State == TaskStepOutputCaptureStateFailed)
}

func ValidTaskStepOutputCaptureStatusRecordForStore(value TaskStepOutputCaptureStatusRecord) bool {
	return validTaskStepOutputCaptureStatusRecord(value)
}

func taskStepOutputCaptureIdentityValid(value TaskStepOutputCaptureIdentity) bool {
	runtimeIdentity := runtimeclient.SupplementalOutputCaptureIdentity{
		OwnerID: value.OwnerID, TaskID: value.TaskID, AuthorizationVersion: value.AuthorizationVersion,
		TaskRevision: value.TaskRevision, WorkspaceID: value.WorkspaceID,
		WorkspaceAuthorizationVersion: value.WorkspaceAuthorizationVersion,
		WorkspaceBindingVersion:       value.WorkspaceBindingVersion, WorkspaceViewID: value.WorkspaceViewID,
		WorkspaceCheckpointRevision: value.WorkspaceCheckpointRevision, StepID: value.StepID, CallID: value.CallID,
	}
	return runtimeclient.ValidSupplementalOutputCaptureIdentity(runtimeIdentity) && domain.ValidOperationID(value.OperationID)
}

type taskStepOutputCaptureAADValue struct {
	Version                       int    `json:"version"`
	OwnerID                       string `json:"owner_id"`
	TaskID                        string `json:"task_id"`
	AuthorizationVersion          int64  `json:"authorization_version"`
	TaskRevision                  int64  `json:"task_revision"`
	WorkspaceID                   string `json:"workspace_id"`
	WorkspaceAuthorizationVersion int64  `json:"workspace_authorization_version"`
	WorkspaceBindingVersion       int64  `json:"workspace_binding_version"`
	WorkspaceViewID               string `json:"workspace_view_id"`
	WorkspaceCheckpointRevision   int64  `json:"workspace_checkpoint_revision"`
	StepID                        string `json:"step_id"`
	CallID                        string `json:"call_id"`
	OperationID                   string `json:"operation_id"`
	Stream                        string `json:"stream"`
	SourceBytes                   int    `json:"source_bytes"`
	TotalBytes                    int    `json:"total_bytes"`
	StreamSHA256                  string `json:"stream_sha256"`
	PageIndex                     int    `json:"page_index"`
	Offset                        int    `json:"offset"`
	PageBytes                     int    `json:"page_bytes"`
	PageSHA256                    string `json:"page_sha256"`
}

func taskStepOutputCaptureAAD(page TaskStepOutputCapturePageRecord) []byte {
	value := taskStepOutputCaptureAADValue{
		Version: 1, OwnerID: page.Identity.OwnerID, TaskID: page.Identity.TaskID,
		AuthorizationVersion: page.Identity.AuthorizationVersion, TaskRevision: page.Identity.TaskRevision,
		WorkspaceID: page.Identity.WorkspaceID, WorkspaceAuthorizationVersion: page.Identity.WorkspaceAuthorizationVersion,
		WorkspaceBindingVersion: page.Identity.WorkspaceBindingVersion, WorkspaceViewID: page.Identity.WorkspaceViewID,
		WorkspaceCheckpointRevision: page.Identity.WorkspaceCheckpointRevision, StepID: page.Identity.StepID,
		CallID: page.Identity.CallID, OperationID: page.Identity.OperationID, Stream: page.Stream,
		SourceBytes: page.SourceBytes, TotalBytes: page.TotalBytes,
		StreamSHA256: hex.EncodeToString(page.StreamSHA256), PageIndex: page.PageIndex,
		Offset: page.Offset, PageBytes: page.PlaintextBytes, PageSHA256: hex.EncodeToString(page.PageSHA256),
	}
	encoded, _ := json.Marshal(value)
	return encoded
}

// SealTaskStepOutputCapture joins each stream before applying the existing
// full-string credential/private-key redactor, then encrypts bounded UTF-8
// pages. Callers must never persist the supplied raw slices.
func SealTaskStepOutputCapture(sealer *Sealer, identity TaskStepOutputCaptureIdentity, stdout, stderr []byte) (TaskStepOutputCaptureRecord, error) {
	if sealer == nil || !taskStepOutputCaptureIdentityValid(identity) ||
		len(stdout) > TaskStepOutputCaptureMaxBytes || len(stderr) > TaskStepOutputCaptureMaxBytes-len(stdout) ||
		!utf8.Valid(stdout) || !utf8.Valid(stderr) || strings.ContainsRune(string(stdout), '\x00') || strings.ContainsRune(string(stderr), '\x00') {
		return TaskStepOutputCaptureRecord{}, domain.NewError(domain.CodeInvalidArgument, "supplemental output capture is invalid")
	}
	stdoutRecord, err := sealTaskStepOutputCaptureStream(sealer, identity, "stdout", stdout)
	if err != nil {
		return TaskStepOutputCaptureRecord{}, err
	}
	stderrRecord, err := sealTaskStepOutputCaptureStream(sealer, identity, "stderr", stderr)
	if err != nil {
		return TaskStepOutputCaptureRecord{}, err
	}
	return TaskStepOutputCaptureRecord{Identity: identity, Stdout: stdoutRecord, Stderr: stderrRecord}, nil
}

func sealTaskStepOutputCaptureStream(sealer *Sealer, identity TaskStepOutputCaptureIdentity, stream string, source []byte) (TaskStepOutputCaptureStreamRecord, error) {
	clean, _ := taskStepSanitizedText(string(source))
	if !utf8.ValidString(clean) || strings.ContainsRune(clean, '\x00') {
		return TaskStepOutputCaptureStreamRecord{}, domain.NewError(domain.CodeInvalidState, "supplemental output redaction failed")
	}
	// A second pass must be a no-op; otherwise the capture fails closed instead
	// of sealing content the configured redactor could not conclusively handle.
	secondPass, _ := taskStepSanitizedText(clean)
	if secondPass != clean || len(clean) > TaskStepOutputCaptureMaxBytes {
		return TaskStepOutputCaptureStreamRecord{}, domain.NewError(domain.CodeInvalidState, "supplemental output redaction failed")
	}
	streamDigest := sha256.Sum256([]byte(clean))
	record := TaskStepOutputCaptureStreamRecord{Stream: stream, SourceBytes: len(source), TotalBytes: len(clean), SHA256: append([]byte(nil), streamDigest[:]...)}
	for offset, pageIndex := 0, 0; offset < len(clean); pageIndex++ {
		if pageIndex >= TaskStepOutputCaptureMaxPages {
			return TaskStepOutputCaptureStreamRecord{}, domain.NewError(domain.CodeResultTooLarge, "supplemental output capture exceeds its storage limit")
		}
		end := offset + TaskStepOutputCapturePageBytes
		if end > len(clean) {
			end = len(clean)
		} else {
			for end > offset && !utf8.RuneStart(clean[end]) {
				end--
			}
		}
		plain := []byte(clean[offset:end])
		pageDigest := sha256.Sum256(plain)
		page := TaskStepOutputCapturePageRecord{
			Identity: identity, Stream: stream, SourceBytes: len(source), TotalBytes: len(clean),
			StreamSHA256: append([]byte(nil), streamDigest[:]...), PageIndex: pageIndex,
			Offset: offset, PlaintextBytes: len(plain), PageSHA256: append([]byte(nil), pageDigest[:]...),
		}
		ciphertext, err := sealer.SealWithAAD(plain, taskStepOutputCaptureAAD(page))
		if err != nil {
			return TaskStepOutputCaptureStreamRecord{}, fmt.Errorf("encrypt supplemental output page: %w", err)
		}
		page.Ciphertext = ciphertext
		record.Pages = append(record.Pages, page)
		offset = end
	}
	return record, nil
}

func validTaskStepOutputCaptureRecord(value TaskStepOutputCaptureRecord) bool {
	if !taskStepOutputCaptureIdentityValid(value.Identity) {
		return false
	}
	streams := []TaskStepOutputCaptureStreamRecord{value.Stdout, value.Stderr}
	totalStored, totalSource, totalPages := 0, 0, 0
	for index, stream := range streams {
		wantName := "stdout"
		if index == 1 {
			wantName = "stderr"
		}
		if stream.Stream != wantName || stream.SourceBytes < 0 || stream.TotalBytes < 0 ||
			stream.SourceBytes > TaskStepOutputCaptureMaxBytes || stream.TotalBytes > TaskStepOutputCaptureMaxBytes || len(stream.SHA256) != sha256.Size {
			return false
		}
		if stream.TotalBytes == 0 && len(stream.Pages) != 0 || stream.TotalBytes > 0 && len(stream.Pages) == 0 {
			return false
		}
		offset := 0
		for pageIndex, page := range stream.Pages {
			if page.Identity != value.Identity || page.Stream != stream.Stream || page.SourceBytes != stream.SourceBytes ||
				page.TotalBytes != stream.TotalBytes || !equalByteSlices(page.StreamSHA256, stream.SHA256) || page.PageIndex != pageIndex ||
				page.Offset != offset || page.PlaintextBytes < 1 || page.PlaintextBytes > TaskStepOutputCapturePageBytes ||
				len(page.PageSHA256) != sha256.Size || len(page.Ciphertext) != page.PlaintextBytes+28 ||
				page.Offset > stream.TotalBytes-page.PlaintextBytes {
				return false
			}
			offset += page.PlaintextBytes
		}
		if offset != stream.TotalBytes {
			return false
		}
		totalStored += stream.TotalBytes
		totalSource += stream.SourceBytes
		totalPages += len(stream.Pages)
	}
	return totalStored <= TaskStepOutputCaptureMaxBytes && totalSource <= TaskStepOutputCaptureMaxBytes && totalPages <= TaskStepOutputCaptureMaxPages
}

// ValidTaskStepOutputCaptureRecordForStore exposes the structural check to
// persistence adapters without granting them any sealing or disclosure API.
func ValidTaskStepOutputCaptureRecordForStore(value TaskStepOutputCaptureRecord) bool {
	return validTaskStepOutputCaptureRecord(value)
}

// GetStepOutputCapturePage is an internal, read-only page primitive. It runs
// the normal Task authorization and Workspace visibility checks for every
// call, then binds the encrypted page to the current Task revision and step.
func (s *TaskService) GetStepOutputCapturePage(ctx context.Context, principal identity.Principal, taskID, stepID, stream string, offset int) (TaskStepOutputCapturePage, error) {
	if s == nil || ctx == nil || !domain.ValidTaskID(taskID) || !domain.ValidStepID(stepID) ||
		(stream != "stdout" && stream != "stderr") || offset < 0 || offset > TaskStepOutputCaptureMaxBytes {
		return TaskStepOutputCapturePage{}, domain.NewError(domain.CodeNotFound, "supplemental output page not found")
	}
	details, err := s.Get(ctx, principal, taskID)
	if err != nil {
		return TaskStepOutputCapturePage{}, err
	}
	current := details.Task
	if current.ID != taskID || current.OwnerID == "" || current.Authorization.Version < 1 || current.CurrentRevision == nil ||
		current.DesiredRevision == nil || *current.CurrentRevision != *current.DesiredRevision || current.WorkspaceID == "" ||
		current.WorkspaceCheckpointRevision == nil {
		return TaskStepOutputCapturePage{}, domain.NewError(domain.CodeNotFound, "supplemental output page not found")
	}
	var step *task.Step
	for index := range details.Steps {
		if details.Steps[index].ID == stepID {
			step = &details.Steps[index]
			break
		}
	}
	if step == nil || step.TaskID != taskID || step.TaskRevision == nil || *step.TaskRevision != *current.CurrentRevision ||
		step.ToolName != runtimeToolShellExec || step.Phase != "execute" || step.ToolCallKey == "" ||
		(step.Status != task.StepSucceeded && step.Status != task.StepFailed) {
		return TaskStepOutputCapturePage{}, domain.NewError(domain.CodeNotFound, "supplemental output page not found")
	}
	visible, err := s.taskWorkspaceDetailVisible(ctx, principal, current)
	if err != nil {
		return TaskStepOutputCapturePage{}, err
	}
	if !visible {
		return TaskStepOutputCapturePage{}, domain.NewError(domain.CodeNotFound, "supplemental output page not found")
	}
	workspaceAuthVersion := int64(0)
	if current.ProjectID != "" {
		if s.projectAccess == nil {
			return TaskStepOutputCapturePage{}, domain.NewError(domain.CodeNotFound, "supplemental output page not found")
		}
		member, err := s.projectAccess.RequireAccess(ctx, principal, current.ProjectID, project.RoleMember)
		if err != nil || member.ProjectID != current.ProjectID || member.UserID != principal.UserID || member.Version < 1 {
			if err != nil && domain.CodeOf(err) != domain.CodeForbidden && domain.CodeOf(err) != domain.CodeNotFound {
				return TaskStepOutputCapturePage{}, err
			}
			return TaskStepOutputCapturePage{}, domain.NewError(domain.CodeNotFound, "supplemental output page not found")
		}
		projectReader, ok := s.store.(interface {
			GetProject(context.Context, string, string) (project.Project, error)
		})
		if !ok || current.WorkspaceProjectResourceVersion == nil || *current.WorkspaceProjectResourceVersion < 1 {
			return TaskStepOutputCapturePage{}, domain.NewError(domain.CodeNotFound, "supplemental output page not found")
		}
		projectValue, err := projectReader.GetProject(ctx, principal.UserID, current.ProjectID)
		if err != nil {
			if domain.CodeOf(err) != domain.CodeForbidden && domain.CodeOf(err) != domain.CodeNotFound {
				return TaskStepOutputCapturePage{}, err
			}
			return TaskStepOutputCapturePage{}, domain.NewError(domain.CodeNotFound, "supplemental output page not found")
		}
		if projectValue.ID != current.ProjectID || projectValue.Version != *current.WorkspaceProjectResourceVersion || projectValue.Version < 1 {
			return TaskStepOutputCapturePage{}, domain.NewError(domain.CodeNotFound, "supplemental output page not found")
		}
		workspaceAuthVersion = member.Version
	}
	statusReader, hasStatusReader := s.store.(TaskStepOutputCaptureStatusReader)
	if !hasStatusReader {
		return TaskStepOutputCapturePage{Stream: stream, State: TaskStepOutputCaptureStateNotRecorded}, nil
	}
	status, err := statusReader.GetTaskStepOutputCaptureStatus(ctx, principal.UserID, taskID, stepID)
	if err != nil {
		return TaskStepOutputCapturePage{}, err
	}
	if status.State == TaskStepOutputCaptureStateNotRecorded {
		return TaskStepOutputCapturePage{Stream: stream, State: TaskStepOutputCaptureStateNotRecorded}, nil
	}
	if !validTaskStepOutputCaptureStatusRecord(status) && status.State != TaskStepOutputCaptureStateComplete ||
		!taskStepOutputCaptureIdentityValid(status.Identity) || !status.ExpiresAt.After(time.Now().UTC()) ||
		status.Identity.OwnerID != current.OwnerID || status.Identity.TaskID != taskID ||
		status.Identity.TaskRevision != *current.CurrentRevision || status.Identity.AuthorizationVersion != current.Authorization.Version ||
		status.Identity.WorkspaceID != current.WorkspaceID || status.Identity.WorkspaceAuthorizationVersion != workspaceAuthVersion ||
		status.Identity.WorkspaceCheckpointRevision != *current.WorkspaceCheckpointRevision ||
		status.Identity.StepID != stepID || status.Identity.CallID != step.ToolCallKey {
		return TaskStepOutputCapturePage{}, domain.NewError(domain.CodeInvalidState, "supplemental output capture identity is invalid")
	}
	if status.State != TaskStepOutputCaptureStateComplete {
		return TaskStepOutputCapturePage{Stream: stream, State: status.State, Complete: false}, nil
	}
	reader, ok := s.store.(TaskStepOutputCaptureReader)
	if !ok || s.stepDetailSealer == nil {
		return TaskStepOutputCapturePage{}, domain.NewError(domain.CodeUnsupported, "supplemental output capture reader is unavailable")
	}
	record, err := reader.GetTaskStepOutputCapturePage(ctx, principal.UserID, taskID, stepID, stream, offset)
	if err != nil {
		return TaskStepOutputCapturePage{}, err
	}
	identity := record.Identity
	if !taskStepOutputCaptureIdentityValid(identity) || status.Identity != identity || !record.ExpiresAt.After(time.Now().UTC()) || identity.OwnerID != current.OwnerID ||
		identity.TaskID != taskID || identity.TaskRevision != *current.CurrentRevision || identity.AuthorizationVersion != current.Authorization.Version ||
		identity.WorkspaceID != current.WorkspaceID || identity.WorkspaceAuthorizationVersion != workspaceAuthVersion ||
		identity.WorkspaceCheckpointRevision != *current.WorkspaceCheckpointRevision || identity.StepID != stepID || identity.CallID != step.ToolCallKey ||
		record.Stream != stream || record.SourceBytes < 0 || record.SourceBytes > TaskStepOutputCaptureMaxBytes ||
		record.TotalBytes < 0 || record.TotalBytes > TaskStepOutputCaptureMaxBytes ||
		len(record.StreamSHA256) != sha256.Size || offset > record.TotalBytes {
		return TaskStepOutputCapturePage{}, domain.NewError(domain.CodeInvalidState, "supplemental output capture identity is invalid")
	}
	if record.TotalBytes == 0 {
		emptyDigest := sha256.Sum256(nil)
		if offset != 0 || record.PlaintextBytes != 0 || len(record.Ciphertext) != 0 || !equalByteSlices(record.StreamSHA256, emptyDigest[:]) {
			return TaskStepOutputCapturePage{}, domain.NewError(domain.CodeInvalidState, "supplemental output capture integrity check failed")
		}
		return TaskStepOutputCapturePage{Identity: identity, Stream: stream, State: TaskStepOutputCaptureStateComplete, Offset: 0, NextOffset: 0, TotalBytes: 0,
			SourceBytes: record.SourceBytes, EOF: true, Complete: true, Data: []byte{}}, nil
	}
	if record.Offset != offset || record.PlaintextBytes < 1 || record.PlaintextBytes > TaskStepOutputCapturePageBytes ||
		offset > record.TotalBytes-record.PlaintextBytes || len(record.PageSHA256) != sha256.Size ||
		len(record.Ciphertext) != record.PlaintextBytes+28 {
		return TaskStepOutputCapturePage{}, domain.NewError(domain.CodeInvalidState, "supplemental output capture page is invalid")
	}
	plain, err := s.stepDetailSealer.OpenWithAAD(record.Ciphertext, taskStepOutputCaptureAAD(record))
	pageDigest := sha256.Sum256(plain)
	if err != nil || len(plain) != record.PlaintextBytes || !equalByteSlices(pageDigest[:], record.PageSHA256) ||
		!utf8.Valid(plain) || strings.ContainsRune(string(plain), '\x00') {
		return TaskStepOutputCapturePage{}, domain.NewError(domain.CodeInvalidState, "supplemental output capture integrity check failed")
	}
	redacted, _ := taskStepSanitizedText(string(plain))
	if redacted != string(plain) {
		return TaskStepOutputCapturePage{}, domain.NewError(domain.CodeInvalidState, "supplemental output capture redaction check failed")
	}
	return TaskStepOutputCapturePage{Identity: identity, Stream: stream, State: TaskStepOutputCaptureStateComplete, Offset: offset,
		NextOffset: offset + len(plain), TotalBytes: record.TotalBytes, SourceBytes: record.SourceBytes,
		EOF: offset+len(plain) == record.TotalBytes, Complete: true, Data: append([]byte(nil), plain...)}, nil
}

// CleanupExpiredTaskStepOutputCaptures deletes encrypted capture receipts and
// pages past their fixed retention window. The store owns the bounded batch.
func (s *TaskService) CleanupExpiredTaskStepOutputCaptures(ctx context.Context, limit int32) (int32, error) {
	if s == nil || ctx == nil || limit < 1 || limit > TaskStepOutputCaptureCleanupBatch {
		return 0, domain.InvalidArgument("limit", "supplemental output cleanup limit is invalid")
	}
	cleaner, ok := s.store.(interface {
		CleanupExpiredTaskStepOutputCaptures(context.Context, int32) (int32, error)
	})
	if !ok {
		return 0, domain.NewError(domain.CodeUnsupported, "supplemental output cleanup storage is unavailable")
	}
	return cleaner.CleanupExpiredTaskStepOutputCaptures(ctx, limit)
}

func equalByteSlices(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
