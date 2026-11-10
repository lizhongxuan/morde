package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
)

const maxConversationCoordinatorCheckpointBytes = 1 << 20

var conversationCoordinatorIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ConversationCoordinatorIdentity is scoped to one accepted message. It is
// deliberately independent of Task and Task Agent identities.
type ConversationCoordinatorIdentity struct {
	ServiceInstanceID    string
	OwnerID              string
	ActorID              string
	ConversationID       string
	MessageID            string
	AgentID              string
	Generation           int64
	AuthorizationVersion int64
	SessionID            string
}

type ConversationCoordinatorCheckpointCommand struct {
	Claim         ConversationRouteClaim
	Identity      ConversationCoordinatorIdentity
	ContextDigest []byte
	From          int64
	Through       int64
	Digest        []byte
	Payload       []byte
}

type ConversationCoordinatorCheckpointHead struct {
	Through       int64
	Digest        []byte
	ContextDigest []byte
}

type ConversationCoordinatorCheckpointPageRequest struct {
	Claim         ConversationRouteClaim
	Identity      ConversationCoordinatorIdentity
	ContextDigest []byte
	From          int64
	Through       int64
	ThroughDigest []byte
}

type ConversationCoordinatorCheckpointPage struct {
	From    int64
	Through int64
	Digest  []byte
	Payload []byte
}

type StoredConversationCoordinatorCheckpoint struct {
	Identity      ConversationCoordinatorIdentity
	ContextDigest []byte
	From          int64
	Through       int64
	Digest        []byte
	Ciphertext    []byte
}

type ConversationCoordinatorCheckpointStore interface {
	SaveConversationCoordinatorCheckpoint(context.Context, ConversationRouteClaim, StoredConversationCoordinatorCheckpoint) (bool, error)
	ReadConversationCoordinatorCheckpointHead(context.Context, ConversationRouteClaim, ConversationCoordinatorIdentity, []byte) (ConversationCoordinatorCheckpointHead, error)
	ReadConversationCoordinatorCheckpointPage(context.Context, ConversationRouteClaim, ConversationCoordinatorIdentity, []byte, int64, int64, []byte) (StoredConversationCoordinatorCheckpoint, error)
}

type ConversationCoordinatorCheckpointService struct {
	store  ConversationCoordinatorCheckpointStore
	sealer *Sealer
}

func NewConversationCoordinatorCheckpointService(store ConversationCoordinatorCheckpointStore, sealer *Sealer) (*ConversationCoordinatorCheckpointService, error) {
	if store == nil || sealer == nil {
		return nil, domain.NewError(domain.CodeInvalidArgument, "coordinator checkpoint dependencies are required")
	}
	return &ConversationCoordinatorCheckpointService{store: store, sealer: sealer}, nil
}

func ValidateConversationCoordinatorIdentity(claim ConversationRouteClaim, identity ConversationCoordinatorIdentity) error {
	if claim.Message.Role != conversation.RoleUser || claim.Message.Sequence < 1 || claim.LeaseOwner == "" || claim.LeaseVersion < 1 ||
		!domain.ValidServiceInstanceID(claim.ServiceInstanceID) || !domain.ValidUserID(claim.Message.OwnerID) ||
		!domain.ValidUserID(claim.Message.ActorID) || !domain.ValidConversationID(claim.Message.ConversationID) ||
		!domain.ValidMessageID(claim.Message.ID) || !conversationCoordinatorIDPattern.MatchString(identity.AgentID) ||
		!conversationCoordinatorIDPattern.MatchString(identity.SessionID) || identity.Generation < 1 || identity.AuthorizationVersion < 1 ||
		identity.ServiceInstanceID != claim.ServiceInstanceID || identity.OwnerID != claim.Message.OwnerID ||
		identity.ActorID != claim.Message.ActorID || identity.ConversationID != claim.Message.ConversationID ||
		identity.MessageID != claim.Message.ID || identity.AuthorizationVersion != claim.Authorization.Version {
		return domain.InvalidArgument("coordinator_identity", "coordinator identity does not match the authorized route claim")
	}
	return nil
}

// ConversationCoordinatorContextDigest binds a resumable DSH turn to the
// authorized route context and selected model. The canonical input is a
// versioned struct containing only deterministic JSON values.
func ConversationCoordinatorContextDigest(value ConversationRouteContext) ([]byte, error) {
	if !domain.ValidModelConnectionID(value.ModelID) {
		return nil, domain.InvalidArgument("coordinator_context", "coordinator model identity is invalid")
	}
	request, err := ConversationRouteModelRequest(value)
	if err != nil {
		return nil, err
	}
	if len(request.Input) != 1 || request.Input[0].Type != "message" || request.Input[0].Role != "user" ||
		request.Input[0].Text == "" || len(request.Input[0].Images) != 0 || len(request.Input[0].Parts) != 0 {
		return nil, domain.NewError(domain.CodeInvalidState, "coordinator route request is not a text-only message")
	}
	type canonicalInput struct {
		Type string `json:"type"`
		Role string `json:"role"`
		Text string `json:"text"`
	}
	encoded, err := json.Marshal(struct {
		Version         int              `json:"version"`
		ModelID         string           `json:"model_id"`
		Instructions    string           `json:"instructions"`
		Input           []canonicalInput `json:"input"`
		MaxOutputTokens int              `json:"max_output_tokens"`
	}{Version: 1, ModelID: value.ModelID, Instructions: request.Instructions,
		Input:           []canonicalInput{{Type: request.Input[0].Type, Role: request.Input[0].Role, Text: request.Input[0].Text}},
		MaxOutputTokens: request.MaxOutputTokens})
	if err != nil {
		return nil, fmt.Errorf("encode coordinator context digest: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return append([]byte(nil), digest[:]...), nil
}

func (s *ConversationCoordinatorCheckpointService) Head(ctx context.Context, claim ConversationRouteClaim,
	identity ConversationCoordinatorIdentity, contextDigest []byte) (ConversationCoordinatorCheckpointHead, error) {
	if ValidateConversationCoordinatorIdentity(claim, identity) != nil || ctx == nil || len(contextDigest) != sha256.Size {
		return ConversationCoordinatorCheckpointHead{}, domain.InvalidArgument("coordinator_checkpoint", "coordinator checkpoint identity is invalid")
	}
	head, err := s.store.ReadConversationCoordinatorCheckpointHead(ctx, claim, identity, contextDigest)
	if err != nil {
		return ConversationCoordinatorCheckpointHead{}, err
	}
	if (head.Through == -1 && (len(head.Digest) != 0 || len(head.ContextDigest) != 0)) ||
		(head.Through < -1 || head.Through >= 0 && (len(head.Digest) != sha256.Size || len(head.ContextDigest) != sha256.Size)) {
		return ConversationCoordinatorCheckpointHead{}, domain.NewError(domain.CodeInvalidState, "coordinator checkpoint head is invalid")
	}
	if head.Through >= 0 && !bytes.Equal(head.ContextDigest, contextDigest) {
		return ConversationCoordinatorCheckpointHead{}, domain.NewError(domain.CodeConflict, "coordinator context changed since its checkpoint")
	}
	head.Digest = append([]byte(nil), head.Digest...)
	head.ContextDigest = append([]byte(nil), head.ContextDigest...)
	return head, nil
}

func (s *ConversationCoordinatorCheckpointService) Save(ctx context.Context, command ConversationCoordinatorCheckpointCommand) (bool, error) {
	if ctx == nil || ValidateConversationCoordinatorIdentity(command.Claim, command.Identity) != nil ||
		len(command.ContextDigest) != sha256.Size || command.From < 0 ||
		command.Through < command.From || command.Through > 1<<53-1 || len(command.Payload) == 0 ||
		len(command.Payload) > maxConversationCoordinatorCheckpointBytes || !utf8.Valid(command.Payload) || !json.Valid(command.Payload) ||
		len(command.Digest) != sha256.Size {
		return false, domain.InvalidArgument("coordinator_checkpoint", "coordinator checkpoint is invalid")
	}
	digest := sha256.Sum256(command.Payload)
	if !bytes.Equal(digest[:], command.Digest) {
		return false, domain.InvalidArgument("coordinator_checkpoint", "coordinator checkpoint digest is invalid")
	}
	envelope, err := json.Marshal(struct {
		Identity      ConversationCoordinatorIdentity `json:"identity"`
		ContextDigest []byte                          `json:"context_digest"`
		From          int64                           `json:"from"`
		Through       int64                           `json:"through"`
		Digest        []byte                          `json:"digest"`
		Payload       []byte                          `json:"payload"`
	}{command.Identity, command.ContextDigest, command.From, command.Through, command.Digest, command.Payload})
	if err != nil {
		return false, fmt.Errorf("encode coordinator checkpoint: %w", err)
	}
	ciphertext, err := s.sealer.Seal(envelope)
	if err != nil {
		return false, fmt.Errorf("seal coordinator checkpoint: %w", err)
	}
	return s.store.SaveConversationCoordinatorCheckpoint(ctx, command.Claim, StoredConversationCoordinatorCheckpoint{
		Identity: command.Identity, ContextDigest: append([]byte(nil), command.ContextDigest...),
		From: command.From, Through: command.Through, Digest: append([]byte(nil), command.Digest...), Ciphertext: ciphertext,
	})
}

func (s *ConversationCoordinatorCheckpointService) ReadPage(ctx context.Context, request ConversationCoordinatorCheckpointPageRequest) (ConversationCoordinatorCheckpointPage, error) {
	if ctx == nil || ValidateConversationCoordinatorIdentity(request.Claim, request.Identity) != nil ||
		len(request.ContextDigest) != sha256.Size || request.From < 0 ||
		request.Through < request.From || request.Through > 1<<53-1 || len(request.ThroughDigest) != sha256.Size {
		return ConversationCoordinatorCheckpointPage{}, domain.InvalidArgument("coordinator_checkpoint", "coordinator checkpoint page request is invalid")
	}
	stored, err := s.store.ReadConversationCoordinatorCheckpointPage(ctx, request.Claim, request.Identity,
		request.ContextDigest, request.From, request.Through, request.ThroughDigest)
	if err != nil {
		return ConversationCoordinatorCheckpointPage{}, err
	}
	if stored.Identity != request.Identity || !bytes.Equal(stored.ContextDigest, request.ContextDigest) ||
		stored.From != request.From || stored.Through < stored.From || stored.Through > request.Through ||
		len(stored.Digest) != sha256.Size || len(stored.Ciphertext) == 0 {
		return ConversationCoordinatorCheckpointPage{}, domain.NewError(domain.CodeInvalidState, "coordinator checkpoint page is invalid")
	}
	plain, err := s.sealer.Open(stored.Ciphertext)
	if err != nil {
		return ConversationCoordinatorCheckpointPage{}, errors.New("coordinator checkpoint page could not be opened")
	}
	var envelope struct {
		Identity      ConversationCoordinatorIdentity `json:"identity"`
		ContextDigest []byte                          `json:"context_digest"`
		From          int64                           `json:"from"`
		Through       int64                           `json:"through"`
		Digest        []byte                          `json:"digest"`
		Payload       []byte                          `json:"payload"`
	}
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&envelope) != nil || envelope.Identity != stored.Identity || !bytes.Equal(envelope.ContextDigest, stored.ContextDigest) || envelope.From != stored.From ||
		envelope.Through != stored.Through || !bytes.Equal(envelope.Digest, stored.Digest) ||
		len(envelope.Payload) == 0 || len(envelope.Payload) > maxConversationCoordinatorCheckpointBytes || !utf8.Valid(envelope.Payload) || !json.Valid(envelope.Payload) {
		return ConversationCoordinatorCheckpointPage{}, domain.NewError(domain.CodeInvalidState, "coordinator checkpoint plaintext does not match its identity")
	}
	digest := sha256.Sum256(envelope.Payload)
	if !bytes.Equal(digest[:], stored.Digest) || (stored.Through == request.Through && !bytes.Equal(stored.Digest, request.ThroughDigest)) {
		return ConversationCoordinatorCheckpointPage{}, domain.NewError(domain.CodeInvalidState, "coordinator checkpoint digest chain is invalid")
	}
	return ConversationCoordinatorCheckpointPage{From: stored.From, Through: stored.Through,
		Digest: append([]byte(nil), stored.Digest...), Payload: append([]byte(nil), envelope.Payload...)}, nil
}
