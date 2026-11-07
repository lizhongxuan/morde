package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"strings"
	"unicode"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

const (
	ProjectConversationAttachmentMaxBytes = int64(300 << 20)
	ProjectConversationAttachmentQuota    = int64(5 << 30)
	ProjectConversationAttachmentChunk    = personal.MaxAttachmentChunkBytes
)

type ProjectConversationAttachmentScope struct {
	ProjectID          string
	ConversationID     string
	MessageID          string
	ClientAttachmentID string
	ServiceInstanceID  string
}

type ProjectConversationAttachmentReference struct {
	ProjectID         string
	ConversationID    string
	MessageID         string
	AttachmentID      string
	ServiceInstanceID string
}

type ProjectConversationAttachmentMetadata struct {
	AttachmentID       string `json:"attachment_id"`
	Name               string `json:"name"`
	MediaType          string `json:"media_type"`
	SizeBytes          int64  `json:"size_bytes"`
	SHA256             string `json:"sha256"`
	DerivativeMode     string `json:"derivative_mode,omitempty"`
	ParentAttachmentID string `json:"-"`
	DerivativeKind     string `json:"-"`
	DerivativePage     int    `json:"-"`
}

type ProjectConversationAttachmentStatus struct {
	ProjectConversationAttachmentMetadata
	State             string  `json:"state"`
	TotalChunks       int32   `json:"total_chunks"`
	ReceivedSequences []int32 `json:"received_sequences"`
	Replay            bool    `json:"replay"`
	DerivativeState   string  `json:"derivative_state"`
}

type ProjectConversationAttachmentStore interface {
	InitializeProjectConversationAttachment(context.Context, identity.Principal, ProjectConversationAttachmentScope, ProjectConversationAttachmentMetadata) (ProjectConversationAttachmentStatus, error)
	GetProjectConversationAttachment(context.Context, identity.Principal, ProjectConversationAttachmentScope) (ProjectConversationAttachmentStatus, error)
	PutProjectConversationAttachmentChunk(context.Context, identity.Principal, ProjectConversationAttachmentScope, int32, string, []byte) (bool, error)
	CommitProjectConversationAttachment(context.Context, identity.Principal, ProjectConversationAttachmentScope) (ProjectConversationAttachmentStatus, error)
	OpenProjectConversationAttachment(context.Context, identity.Principal, ProjectConversationAttachmentScope) (io.ReadCloser, ProjectConversationAttachmentMetadata, error)
	GetProjectConversationAttachmentByID(context.Context, identity.Principal, ProjectConversationAttachmentReference) (ProjectConversationAttachmentMetadata, error)
	OpenProjectConversationAttachmentByID(context.Context, identity.Principal, ProjectConversationAttachmentReference) (io.ReadCloser, ProjectConversationAttachmentMetadata, error)
	ListProjectTaskAttachments(context.Context, task.Task) ([]ProjectConversationAttachmentMetadata, error)
	OpenProjectTaskAttachment(context.Context, task.Task, string) (io.ReadCloser, ProjectConversationAttachmentMetadata, error)
}

type ProjectConversationAttachmentService struct {
	store ProjectConversationAttachmentStore
}

func NewProjectConversationAttachmentService(store ProjectConversationAttachmentStore) (*ProjectConversationAttachmentService, error) {
	if store == nil {
		return nil, errors.New("project conversation attachment store is required")
	}
	return &ProjectConversationAttachmentService{store: store}, nil
}

func ValidateProjectConversationAttachmentScope(principal identity.Principal, scope ProjectConversationAttachmentScope) error {
	if err := validatePersonalAttachmentPrincipal(principal); err != nil {
		return err
	}
	if !project.ValidID(scope.ProjectID) || !domain.ValidConversationID(scope.ConversationID) ||
		!domain.ValidMessageID(scope.MessageID) || !domain.ValidServiceInstanceID(scope.ServiceInstanceID) ||
		personal.ValidateAttachmentIdempotencyKey(scope.ClientAttachmentID) != nil {
		return domain.InvalidArgument("attachment", "project conversation attachment identity is invalid")
	}
	return nil
}

func ValidProjectConversationAttachmentID(value string) bool {
	if len(value) != 36 || !strings.HasPrefix(value, "pca_") {
		return false
	}
	decoded, err := hex.DecodeString(value[4:])
	return err == nil && hex.EncodeToString(decoded) == value[4:]
}

func ValidateProjectConversationAttachmentReference(principal identity.Principal, reference ProjectConversationAttachmentReference) error {
	scope := ProjectConversationAttachmentScope{ProjectID: reference.ProjectID, ConversationID: reference.ConversationID,
		MessageID: reference.MessageID, ClientAttachmentID: "lookup-by-id", ServiceInstanceID: reference.ServiceInstanceID}
	if err := ValidateProjectConversationAttachmentScope(principal, scope); err != nil {
		return err
	}
	if !ValidProjectConversationAttachmentID(reference.AttachmentID) {
		return domain.NewError(domain.CodeNotFound, "project attachment not found")
	}
	return nil
}

func ValidateProjectConversationAttachmentMetadata(value ProjectConversationAttachmentMetadata) error {
	if !ValidProjectConversationAttachmentID(value.AttachmentID) || len(value.Name) < 1 || len(value.Name) > 255 ||
		!utf8.ValidString(value.Name) || strings.ContainsAny(value.Name, "/\\") || strings.IndexFunc(value.Name, unicode.IsControl) >= 0 ||
		value.SizeBytes < 0 || value.SizeBytes > ProjectConversationAttachmentMaxBytes || !validProjectAttachmentDigest(value.SHA256) ||
		!ValidProjectDerivativeMode(value.DerivativeMode) ||
		(value.DerivativeMode == "pdf" && value.MediaType != "application/pdf") ||
		(value.DerivativeMode == "image" && !strings.HasPrefix(value.MediaType, "image/")) {
		return domain.InvalidArgument("attachment", "project conversation attachment metadata is invalid")
	}
	parsed, parameters, err := mime.ParseMediaType(value.MediaType)
	if err != nil || parsed != value.MediaType || len(value.MediaType) > 128 || len(parameters) != 0 {
		return domain.InvalidArgument("media_type", "project conversation attachment media type is invalid")
	}
	return nil
}

func validProjectAttachmentDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func (s *ProjectConversationAttachmentService) Initialize(ctx context.Context, principal identity.Principal, scope ProjectConversationAttachmentScope, metadata ProjectConversationAttachmentMetadata) (ProjectConversationAttachmentStatus, error) {
	if err := ValidateProjectConversationAttachmentScope(principal, scope); err != nil {
		return ProjectConversationAttachmentStatus{}, err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return ProjectConversationAttachmentStatus{}, err
	}
	metadata.AttachmentID = "pca_" + hex.EncodeToString(id[:])
	if metadata.DerivativeMode == "" {
		metadata.DerivativeMode = "none"
	}
	if err := ValidateProjectConversationAttachmentMetadata(metadata); err != nil {
		return ProjectConversationAttachmentStatus{}, err
	}
	return s.store.InitializeProjectConversationAttachment(ctx, principal, scope, metadata)
}

func (s *ProjectConversationAttachmentService) Get(ctx context.Context, principal identity.Principal, scope ProjectConversationAttachmentScope) (ProjectConversationAttachmentStatus, error) {
	if err := ValidateProjectConversationAttachmentScope(principal, scope); err != nil {
		return ProjectConversationAttachmentStatus{}, err
	}
	return s.store.GetProjectConversationAttachment(ctx, principal, scope)
}

func (s *ProjectConversationAttachmentService) PutChunk(ctx context.Context, principal identity.Principal, scope ProjectConversationAttachmentScope, sequence int32, digest string, body io.Reader, size int64) (bool, error) {
	if err := ValidateProjectConversationAttachmentScope(principal, scope); err != nil {
		return false, err
	}
	if body == nil || sequence < 0 || sequence >= 75 || size < 1 || size > ProjectConversationAttachmentChunk || !validProjectAttachmentDigest(digest) {
		return false, domain.InvalidArgument("attachment_chunk", "project attachment chunk is invalid")
	}
	content, err := io.ReadAll(io.LimitReader(body, ProjectConversationAttachmentChunk+1))
	if err != nil {
		return false, err
	}
	if int64(len(content)) != size {
		return false, domain.InvalidArgument("attachment_chunk", "project attachment chunk size does not match")
	}
	return s.store.PutProjectConversationAttachmentChunk(ctx, principal, scope, sequence, digest, content)
}

func (s *ProjectConversationAttachmentService) Commit(ctx context.Context, principal identity.Principal, scope ProjectConversationAttachmentScope) (ProjectConversationAttachmentStatus, error) {
	if err := ValidateProjectConversationAttachmentScope(principal, scope); err != nil {
		return ProjectConversationAttachmentStatus{}, err
	}
	return s.store.CommitProjectConversationAttachment(ctx, principal, scope)
}

func (s *ProjectConversationAttachmentService) Open(ctx context.Context, principal identity.Principal, scope ProjectConversationAttachmentScope) (io.ReadCloser, ProjectConversationAttachmentMetadata, error) {
	if err := ValidateProjectConversationAttachmentScope(principal, scope); err != nil {
		return nil, ProjectConversationAttachmentMetadata{}, err
	}
	return s.store.OpenProjectConversationAttachment(ctx, principal, scope)
}

func (s *ProjectConversationAttachmentService) GetByID(ctx context.Context, principal identity.Principal, reference ProjectConversationAttachmentReference) (ProjectConversationAttachmentMetadata, error) {
	if err := ValidateProjectConversationAttachmentReference(principal, reference); err != nil {
		return ProjectConversationAttachmentMetadata{}, err
	}
	return s.store.GetProjectConversationAttachmentByID(ctx, principal, reference)
}

func (s *ProjectConversationAttachmentService) OpenByID(ctx context.Context, principal identity.Principal, reference ProjectConversationAttachmentReference) (io.ReadCloser, ProjectConversationAttachmentMetadata, error) {
	if err := ValidateProjectConversationAttachmentReference(principal, reference); err != nil {
		return nil, ProjectConversationAttachmentMetadata{}, err
	}
	return s.store.OpenProjectConversationAttachmentByID(ctx, principal, reference)
}

func (s *ProjectConversationAttachmentService) ListTaskAttachments(ctx context.Context, current task.Task) ([]ProjectConversationAttachmentMetadata, error) {
	return s.store.ListProjectTaskAttachments(ctx, current)
}

func (s *ProjectConversationAttachmentService) OpenTaskAttachment(ctx context.Context, current task.Task, attachmentID string) (io.ReadCloser, ProjectConversationAttachmentMetadata, error) {
	if !ValidProjectConversationAttachmentID(attachmentID) {
		return nil, ProjectConversationAttachmentMetadata{}, domain.NewError(domain.CodeNotFound, "project attachment not found")
	}
	return s.store.OpenProjectTaskAttachment(ctx, current, attachmentID)
}
