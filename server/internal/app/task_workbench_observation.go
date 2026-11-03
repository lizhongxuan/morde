package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

const (
	taskWorkbenchObservationTicketTTL  = 60 * time.Second
	taskWorkbenchObservationSessionTTL = 5 * time.Minute
	taskWorkbenchObservationScope      = "task:read"
)

// TaskWorkbenchObservationRecord is the durable, hash-only binding behind a
// one-time Workbench ticket and its short-lived read session.
type TaskWorkbenchObservationRecord struct {
	TicketHash        []byte
	SessionHash       []byte
	Nonce             []byte
	TaskID            string
	HostID            string
	ActorID           string
	SessionTokenID    string
	RuntimeGeneration int64
	Scope             string
	ExpiresAt         time.Time
	SessionExpiresAt  time.Time
	UsedAt            *time.Time
}

type taskWorkbenchObservationStore interface {
	CreateTaskWorkbenchObservationTicket(context.Context, TaskWorkbenchObservationRecord) error
	GetTaskWorkbenchObservationTicket(context.Context, []byte) (TaskWorkbenchObservationRecord, error)
	ConsumeTaskWorkbenchObservationTicket(context.Context, []byte, []byte, int64, time.Time) (TaskWorkbenchObservationRecord, error)
	GetTaskWorkbenchObservationSession(context.Context, []byte, time.Time) (TaskWorkbenchObservationRecord, error)
}

type taskWorkbenchObservationHostReader interface {
	GetHost(context.Context, string) (host.Host, error)
}

type TaskWorkbenchObservationTicket struct {
	Ticket    string    `json:"ticket"`
	TaskID    string    `json:"task_id"`
	HostID    string    `json:"host_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// TaskWorkbenchObservationSession deliberately contains only the identifiers
// Gateway needs. The Portal session token and read scope remain server-side.
type TaskWorkbenchObservationSession struct {
	TaskID            string
	HostID            string
	ActorID           string
	RuntimeGeneration int64
	ExpiresAt         time.Time
	sessionTokenID    string
}

type TaskWorkbenchObservationArtifact struct {
	ID          string    `json:"id"`
	Label       string    `json:"label"`
	MediaType   string    `json:"media_type"`
	WorkspaceID string    `json:"workspace_id"`
	Revision    int64     `json:"revision"`
	SizeBytes   int64     `json:"size_bytes"`
	SHA256      string    `json:"sha256"`
	CreatedAt   time.Time `json:"created_at"`
}

type TaskWorkbenchObservationSnapshot struct {
	TaskID        string                             `json:"task_id"`
	HostID        string                             `json:"host_id"`
	Status        task.Status                        `json:"status"`
	EventSequence int64                              `json:"event_sequence"`
	UpdatedAt     time.Time                          `json:"updated_at"`
	Artifacts     []TaskWorkbenchObservationArtifact `json:"artifacts"`
}

type TaskWorkbenchObservationEvent struct {
	Sequence  int64     `json:"sequence"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
}

type TaskWorkbenchObservationEventPage struct {
	TaskID          string                          `json:"task_id"`
	Status          task.Status                     `json:"status"`
	CurrentSequence int64                           `json:"current_sequence"`
	Items           []TaskWorkbenchObservationEvent `json:"items"`
}

// IssueWorkbenchObservationTicket issues a short-lived, single-use ticket for
// an owner who can currently read the Host-bound Task.
func (s *TaskService) IssueWorkbenchObservationTicket(ctx context.Context, principal identity.Principal, taskID string) (TaskWorkbenchObservationTicket, error) {
	if s == nil || s.store == nil || principal.Kind != identity.KindSession ||
		!domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) || !domain.ValidTaskID(taskID) {
		return TaskWorkbenchObservationTicket{}, domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	store, ok := s.store.(taskWorkbenchObservationStore)
	hosts, hostOK := s.store.(taskWorkbenchObservationHostReader)
	if !ok || !hostOK {
		return TaskWorkbenchObservationTicket{}, domain.NewError(domain.CodeUnsupported, "Workbench Task observation is unavailable")
	}
	details, err := s.Get(ctx, principal, taskID)
	if err != nil {
		return TaskWorkbenchObservationTicket{}, err
	}
	if !domain.ValidHostID(details.Task.HostID) {
		return TaskWorkbenchObservationTicket{}, domain.NewError(domain.CodeNotFound, "Task not found")
	}
	hostValue, err := hosts.GetHost(ctx, details.Task.HostID)
	if err != nil {
		return TaskWorkbenchObservationTicket{}, err
	}
	if hostValue.ID != details.Task.HostID || hostValue.OwnerID != principal.UserID || hostValue.State != host.StateRunning ||
		hostValue.RuntimeGeneration < 1 || hostValue.DeletedAt != nil {
		return TaskWorkbenchObservationTicket{}, domain.NewError(domain.CodeNotFound, "Task not found")
	}
	rawTicket, nonce, err := newObservationSecret()
	if err != nil {
		return TaskWorkbenchObservationTicket{}, domain.NewError(domain.CodeInternal, "could not issue Workbench observation ticket")
	}
	expiresAt := time.Now().UTC().Add(taskWorkbenchObservationTicketTTL)
	record := TaskWorkbenchObservationRecord{
		TicketHash: observationHash(rawTicket), Nonce: observationBytesHash(nonce), TaskID: details.Task.ID,
		HostID: details.Task.HostID, ActorID: principal.UserID, SessionTokenID: principal.TokenID,
		RuntimeGeneration: hostValue.RuntimeGeneration, Scope: taskWorkbenchObservationScope, ExpiresAt: expiresAt,
	}
	if err := store.CreateTaskWorkbenchObservationTicket(ctx, record); err != nil {
		return TaskWorkbenchObservationTicket{}, err
	}
	return TaskWorkbenchObservationTicket{Ticket: rawTicket, TaskID: details.Task.ID, HostID: details.Task.HostID, ExpiresAt: expiresAt}, nil
}

// ConsumeWorkbenchObservationTicket atomically exchanges a Portal ticket for a
// separate opaque Gateway cookie. The cookie is never an identity/Host cookie.
func (s *TaskService) ConsumeWorkbenchObservationTicket(ctx context.Context, rawTicket, hostID string, runtimeGeneration int64) (string, TaskWorkbenchObservationSession, error) {
	store, ok := s.observationStore()
	if !ok || !validObservationSecret(rawTicket) || !domain.ValidHostID(hostID) || runtimeGeneration < 1 {
		return "", TaskWorkbenchObservationSession{}, observationNotFound()
	}
	record, err := store.GetTaskWorkbenchObservationTicket(ctx, observationHash(rawTicket))
	if err != nil {
		return "", TaskWorkbenchObservationSession{}, hideObservationAuthorization(err)
	}
	if record.HostID != hostID || record.RuntimeGeneration != runtimeGeneration || !validObservationTicketRecord(record) {
		return "", TaskWorkbenchObservationSession{}, observationNotFound()
	}
	if !record.ExpiresAt.After(time.Now()) {
		return "", TaskWorkbenchObservationSession{}, observationNotFound()
	}
	if err := s.revalidateObservationTask(ctx, record); err != nil {
		return "", TaskWorkbenchObservationSession{}, err
	}
	rawSession, _, err := newObservationSecret()
	if err != nil {
		return "", TaskWorkbenchObservationSession{}, domain.NewError(domain.CodeInternal, "could not create Workbench observation session")
	}
	now := time.Now().UTC()
	sessionExpiresAt := now.Add(taskWorkbenchObservationSessionTTL)
	sessionHash := observationHash(rawSession)
	consumed, err := store.ConsumeTaskWorkbenchObservationTicket(ctx, record.TicketHash, sessionHash, runtimeGeneration, sessionExpiresAt)
	record.SessionHash = sessionHash
	if err != nil {
		return "", TaskWorkbenchObservationSession{}, hideObservationAuthorization(err)
	}
	if !sameObservationBinding(record, consumed) {
		return "", TaskWorkbenchObservationSession{}, observationNotFound()
	}
	return rawSession, observationSession(consumed), nil
}

// ValidateWorkbenchObservationSession rechecks the Host generation and current
// Task read authorization. It is called for each static/API request and each
// SSE poll so revoked Portal sessions stop observing promptly.
func (s *TaskService) ValidateWorkbenchObservationSession(ctx context.Context, rawSession, hostID string, runtimeGeneration int64) (TaskWorkbenchObservationSession, error) {
	store, ok := s.observationStore()
	if !ok || !validObservationSecret(rawSession) || !domain.ValidHostID(hostID) || runtimeGeneration < 1 {
		return TaskWorkbenchObservationSession{}, observationNotFound()
	}
	record, err := store.GetTaskWorkbenchObservationSession(ctx, observationHash(rawSession), time.Now().UTC())
	now := time.Now().UTC()
	if err != nil {
		return TaskWorkbenchObservationSession{}, hideObservationAuthorization(err)
	}
	if record.HostID != hostID || record.RuntimeGeneration != runtimeGeneration || !validObservationRecord(record) ||
		!record.SessionExpiresAt.After(now) {
		return TaskWorkbenchObservationSession{}, observationNotFound()
	}
	if err := s.revalidateObservationTask(ctx, record); err != nil {
		return TaskWorkbenchObservationSession{}, err
	}
	return observationSession(record), nil
}

// GetWorkbenchObservationSnapshot returns the intentionally narrow read model.
func (s *TaskService) GetWorkbenchObservationSnapshot(ctx context.Context, rawSession, hostID string, runtimeGeneration int64) (TaskWorkbenchObservationSnapshot, error) {
	session, err := s.ValidateWorkbenchObservationSession(ctx, rawSession, hostID, runtimeGeneration)
	if err != nil {
		return TaskWorkbenchObservationSnapshot{}, err
	}
	details, err := s.getObservationTask(ctx, session)
	if err != nil {
		return TaskWorkbenchObservationSnapshot{}, hideObservationAuthorization(err)
	}
	if !domain.ValidTaskID(details.Task.ID) || !domain.ValidHostID(details.Task.HostID) || !validTaskStatus(details.Task.Status) ||
		details.Task.ID != session.TaskID || details.Task.HostID != session.HostID || details.Task.EventSequence < 0 || details.Task.UpdatedAt.IsZero() {
		return TaskWorkbenchObservationSnapshot{}, domain.NewError(domain.CodeInternal, "Task observation snapshot is invalid")
	}
	artifacts := make([]TaskWorkbenchObservationArtifact, 0, len(details.Artifacts))
	for _, artifact := range details.Artifacts {
		if artifact.TaskID != details.Task.ID || !validVerifiedObservationArtifact(details.Task.WorkspaceID, artifact) {
			continue
		}
		artifacts = append(artifacts, TaskWorkbenchObservationArtifact{
			ID: artifact.ID, Label: artifact.Label, MediaType: artifact.MediaType,
			WorkspaceID: artifact.WorkspaceID, Revision: artifact.Revision,
			SizeBytes: artifact.SizeBytes, SHA256: artifact.SHA256, CreatedAt: artifact.CreatedAt,
		})
	}
	return TaskWorkbenchObservationSnapshot{
		TaskID: details.Task.ID, HostID: details.Task.HostID, Status: details.Task.Status,
		EventSequence: details.Task.EventSequence, UpdatedAt: details.Task.UpdatedAt, Artifacts: artifacts,
	}, nil
}

// ListWorkbenchObservationEvents strips every event payload before it leaves the
// app boundary; the caller may poll this method repeatedly for revocation.
func (s *TaskService) ListWorkbenchObservationEvents(ctx context.Context, rawSession, hostID string, runtimeGeneration, after int64, limit int32) (TaskWorkbenchObservationEventPage, error) {
	if after < 0 || limit < 1 || limit > maxEventPage {
		return TaskWorkbenchObservationEventPage{}, domain.InvalidArgument("after", "event cursor or limit is out of range")
	}
	session, err := s.ValidateWorkbenchObservationSession(ctx, rawSession, hostID, runtimeGeneration)
	if err != nil {
		return TaskWorkbenchObservationEventPage{}, err
	}
	page, err := s.ListEvents(ctx, observationPrincipal(session), session.TaskID, after, limit)
	if err != nil && domain.CodeOf(err) != domain.CodeEventCursorExpired {
		return TaskWorkbenchObservationEventPage{}, hideObservationAuthorization(err)
	}
	result := TaskWorkbenchObservationEventPage{
		TaskID: session.TaskID, Status: page.Status, CurrentSequence: page.CurrentSequence,
		Items: make([]TaskWorkbenchObservationEvent, 0, len(page.Items)),
	}
	for _, event := range page.Items {
		result.Items = append(result.Items, TaskWorkbenchObservationEvent{Sequence: event.Sequence, Type: event.Type, CreatedAt: event.CreatedAt})
	}
	return result, err
}

func (s *TaskService) observationStore() (taskWorkbenchObservationStore, bool) {
	if s == nil || s.store == nil {
		return nil, false
	}
	store, ok := s.store.(taskWorkbenchObservationStore)
	return store, ok
}

func (s *TaskService) revalidateObservationTask(ctx context.Context, record TaskWorkbenchObservationRecord) error {
	if !validObservationTicketRecord(record) {
		return observationNotFound()
	}
	hosts, ok := s.store.(taskWorkbenchObservationHostReader)
	if !ok {
		return observationNotFound()
	}
	hostValue, err := hosts.GetHost(ctx, record.HostID)
	if err != nil {
		return hideObservationAuthorization(err)
	}
	if hostValue.ID != record.HostID || hostValue.OwnerID != record.ActorID ||
		hostValue.State != host.StateRunning || hostValue.RuntimeGeneration != record.RuntimeGeneration || hostValue.DeletedAt != nil {
		return observationNotFound()
	}
	principal := identity.Principal{UserID: record.ActorID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: record.SessionTokenID}
	details, err := s.Get(ctx, principal, record.TaskID)
	if err != nil {
		return hideObservationAuthorization(err)
	}
	if details.Task.ID != record.TaskID || details.Task.HostID != record.HostID {
		return observationNotFound()
	}
	return nil
}

func (s *TaskService) getObservationTask(ctx context.Context, session TaskWorkbenchObservationSession) (TaskDetails, error) {
	return s.Get(ctx, observationPrincipal(session), session.TaskID)
}

func observationPrincipal(session TaskWorkbenchObservationSession) identity.Principal {
	return identity.Principal{UserID: session.ActorID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: session.sessionTokenID}
}

func observationSession(record TaskWorkbenchObservationRecord) TaskWorkbenchObservationSession {
	return TaskWorkbenchObservationSession{TaskID: record.TaskID, HostID: record.HostID, ActorID: record.ActorID,
		RuntimeGeneration: record.RuntimeGeneration, ExpiresAt: record.SessionExpiresAt, sessionTokenID: record.SessionTokenID}
}

func validVerifiedObservationArtifact(workspaceID string, artifact task.Artifact) bool {
	if !domain.ValidArtifactID(artifact.ID) || !domain.ValidTaskID(artifact.TaskID) || !domain.ValidWorkspaceID(artifact.WorkspaceID) || artifact.WorkspaceID != workspaceID ||
		artifact.Revision < 0 || artifact.SizeBytes < 0 || artifact.CreatedAt.IsZero() || !validSyncDigest(artifact.SHA256) {
		return false
	}
	return true
}

func validObservationRecord(record TaskWorkbenchObservationRecord) bool {
	return validObservationTicketRecord(record) && len(record.SessionHash) == sha256.Size && record.UsedAt != nil && !record.SessionExpiresAt.IsZero()
}

func validObservationTicketRecord(record TaskWorkbenchObservationRecord) bool {
	return len(record.TicketHash) == sha256.Size && len(record.Nonce) == sha256.Size && domain.ValidTaskID(record.TaskID) && domain.ValidHostID(record.HostID) &&
		domain.ValidUserID(record.ActorID) && domain.ValidTokenID(record.SessionTokenID) && record.RuntimeGeneration > 0 &&
		record.Scope == taskWorkbenchObservationScope && !record.ExpiresAt.IsZero()
}

func sameObservationBinding(expected, actual TaskWorkbenchObservationRecord) bool {
	return subtle.ConstantTimeCompare(expected.TicketHash, actual.TicketHash) == 1 &&
		subtle.ConstantTimeCompare(expected.SessionHash, actual.SessionHash) == 1 &&
		subtle.ConstantTimeCompare(expected.Nonce, actual.Nonce) == 1 && actual.UsedAt != nil &&
		expected.TaskID == actual.TaskID && expected.HostID == actual.HostID && expected.ActorID == actual.ActorID &&
		expected.SessionTokenID == actual.SessionTokenID && expected.RuntimeGeneration == actual.RuntimeGeneration &&
		expected.Scope == actual.Scope && expected.ExpiresAt.Equal(actual.ExpiresAt) &&
		actual.SessionExpiresAt.After(*actual.UsedAt)
}

func newObservationSecret() (string, []byte, error) {
	var raw [64]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", nil, err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), append([]byte(nil), raw[32:]...), nil
}

func validObservationSecret(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 64 && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func observationHash(value string) []byte {
	digest := sha256.Sum256([]byte(value))
	return digest[:]
}

func observationBytesHash(value []byte) []byte {
	digest := sha256.Sum256(value)
	return digest[:]
}

func observationNotFound() error {
	return domain.NewError(domain.CodeNotFound, "Task observation session not found")
}

func hideObservationAuthorization(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, identity.ErrInvalidCredential) {
		return observationNotFound()
	}
	switch domain.CodeOf(err) {
	case domain.CodeNotFound, domain.CodeUnauthenticated, domain.CodeForbidden,
		domain.CodeAccountPending, domain.CodeAccountRejected, domain.CodeAccountDisabled:
		return observationNotFound()
	default:
		return err
	}
}
