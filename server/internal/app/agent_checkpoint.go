package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
)

const maxAgentCheckpointBytes = 1 << 20

var agentCheckpointIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type AgentCheckpointFence struct {
	TaskID               string
	OwnerID              string
	InitiatorID          string
	AgentInstanceID      string
	AgentGeneration      int64
	AgentSessionID       string
	AgentRootSessionID   string
	AuthorizationVersion int64
	LeaseOwner           string
	LeaseVersion         int64
	EngineVersion        string
	ProfileVersion       string
}

type AgentCheckpointCommand struct {
	Fence   AgentCheckpointFence
	From    int64
	Through int64
	Digest  []byte
	Payload []byte
}

type AgentCheckpointPageRequest struct {
	Fence         AgentInstanceFence
	SessionID     string
	From          int64
	Through       int64
	ThroughDigest []byte
}

type AgentCheckpointPage struct {
	From    int64
	Through int64
	Digest  []byte
	Payload []byte
}

type StoredAgentCheckpoint struct {
	Fence                       AgentCheckpointFence
	From                        int64
	Through                     int64
	Digest                      []byte
	Ciphertext                  []byte
	ConversationMessages        []AgentConversationMessage
	ConversationMessagesInvalid bool
}

type AgentConversationMessage struct {
	ID      string
	Content string
}

type AgentCheckpointStore interface {
	SaveAgentCheckpoint(context.Context, StoredAgentCheckpoint) (bool, error)
	ReadAgentCheckpointPage(context.Context, AgentCheckpointPageRequest) (StoredAgentCheckpoint, error)
	PendingMessages(context.Context, AgentCheckpointFence) ([]AgentConversationMessage, error)
}

type AgentCheckpointService struct {
	store  AgentCheckpointStore
	sealer *Sealer
}

func NewAgentCheckpointService(store AgentCheckpointStore, sealer *Sealer) (*AgentCheckpointService, error) {
	if store == nil || sealer == nil {
		return nil, domain.NewError(domain.CodeInvalidArgument, "checkpoint dependencies are required")
	}
	return &AgentCheckpointService{store: store, sealer: sealer}, nil
}

// Save confirms a checkpoint only after the fenced store commits it.
func (s *AgentCheckpointService) Save(ctx context.Context, command AgentCheckpointCommand) (bool, error) {
	if command.Fence.AgentRootSessionID == "" {
		command.Fence.AgentRootSessionID = command.Fence.AgentSessionID
	}
	if err := validateAgentCheckpoint(command.Fence, command.From, command.Through, command.Digest, command.Payload); err != nil {
		return false, err
	}
	var messages []AgentConversationMessage
	var invalidMessages bool
	if command.Fence.AgentSessionID == command.Fence.AgentRootSessionID {
		messages, invalidMessages = checkpointConversationMessages(command.Payload)
	}
	aad, err := checkpointAAD(command.Fence, command.From, command.Through)
	if err != nil {
		return false, err
	}
	ciphertext, err := s.sealer.sealWithAAD(command.Payload, aad)
	if err != nil {
		return false, fmt.Errorf("seal agent checkpoint: %w", err)
	}
	return s.store.SaveAgentCheckpoint(ctx, StoredAgentCheckpoint{
		Fence: command.Fence, From: command.From, Through: command.Through,
		Digest: append([]byte(nil), command.Digest...), Ciphertext: ciphertext,
		ConversationMessages: messages, ConversationMessagesInvalid: invalidMessages,
	})
}

func (s *AgentCheckpointService) PendingMessages(ctx context.Context, fence AgentCheckpointFence) ([]AgentConversationMessage, error) {
	if fence.AgentRootSessionID == "" {
		fence.AgentRootSessionID = fence.AgentSessionID
	}
	if !validAgentCheckpointFence(fence) || fence.AgentRootSessionID != fence.AgentSessionID {
		return nil, domain.InvalidArgument("checkpoint", "pending message identity is invalid")
	}
	return s.store.PendingMessages(ctx, fence)
}

// Decode is for checkpoints already selected through the authorized task store.
func (s *AgentCheckpointService) Decode(stored StoredAgentCheckpoint) ([]byte, error) {
	aad, err := checkpointAAD(stored.Fence, stored.From, stored.Through)
	if err != nil {
		return nil, err
	}
	plain, err := s.sealer.openWithAAD(stored.Ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("open agent checkpoint: %w", err)
	}
	if err := validateAgentCheckpoint(stored.Fence, stored.From, stored.Through, stored.Digest, plain); err != nil {
		return nil, err
	}
	return plain, nil
}

// ReadPage only exposes prior checkpoint data to the current, fenced agent lease.
func (s *AgentCheckpointService) ReadPage(ctx context.Context, request AgentCheckpointPageRequest) (AgentCheckpointPage, error) {
	if request.SessionID == "" {
		request.SessionID = request.Fence.AgentSessionID
	}
	if err := request.Fence.Validate(); err != nil || request.From < 0 || request.Through < request.From ||
		len(request.ThroughDigest) != sha256.Size || !agentCheckpointIDPattern.MatchString(request.SessionID) {
		return AgentCheckpointPage{}, domain.InvalidArgument("checkpoint_page", "checkpoint page request is invalid")
	}
	stored, err := s.store.ReadAgentCheckpointPage(ctx, request)
	if err != nil {
		return AgentCheckpointPage{}, err
	}
	if stored.Fence.AgentRootSessionID == "" && stored.Fence.AgentSessionID == request.Fence.AgentSessionID {
		stored.Fence.AgentRootSessionID = stored.Fence.AgentSessionID
	}
	if stored.Fence.AgentSessionID != request.SessionID || stored.Fence.AgentRootSessionID != request.Fence.AgentSessionID ||
		stored.From != request.From || stored.Through > request.Through {
		return AgentCheckpointPage{}, domain.NewError(domain.CodeInvalidState, "checkpoint page is outside the pinned history")
	}
	payload, err := s.Decode(stored)
	if err != nil {
		return AgentCheckpointPage{}, err
	}
	if stored.Through == request.Through && !subtleCompareCheckpointDigest(stored.Digest, request.ThroughDigest) {
		return AgentCheckpointPage{}, domain.NewError(domain.CodeInvalidState, "checkpoint history tail changed")
	}
	return AgentCheckpointPage{From: stored.From, Through: stored.Through,
		Digest: append([]byte(nil), stored.Digest...), Payload: payload}, nil
}

func subtleCompareCheckpointDigest(left, right []byte) bool {
	return len(left) == sha256.Size && len(right) == sha256.Size && subtle.ConstantTimeCompare(left, right) == 1
}

func validateAgentCheckpoint(fence AgentCheckpointFence, from, through int64, digest, payload []byte) error {
	if !validAgentCheckpointFence(fence) || from < 0 || through < from ||
		len(digest) != sha256.Size || len(payload) == 0 || len(payload) > maxAgentCheckpointBytes || !utf8.Valid(payload) {
		return domain.InvalidArgument("checkpoint", "checkpoint fields are invalid")
	}
	sum := sha256.Sum256(payload)
	if subtle.ConstantTimeCompare(sum[:], digest) != 1 {
		return domain.InvalidArgument("checkpoint", "checkpoint digest does not match")
	}
	var body struct {
		Header struct {
			ID string `json:"id"`
		} `json:"header"`
		From   *int64 `json:"from_seq"`
		Events []struct {
			Sequence *int64 `json:"seq"`
		} `json:"events"`
	}
	if err := json.Unmarshal(payload, &body); err != nil || body.Header.ID != fence.AgentSessionID || body.From == nil || *body.From != from ||
		len(body.Events) == 0 || through-from != int64(len(body.Events))-1 {
		return domain.InvalidArgument("checkpoint", "checkpoint event range is invalid")
	}
	for i, event := range body.Events {
		if event.Sequence == nil || *event.Sequence != from+int64(i) {
			return domain.InvalidArgument("checkpoint", "checkpoint events are not contiguous")
		}
	}
	return nil
}

func validAgentCheckpointFence(fence AgentCheckpointFence) bool {
	return domain.ValidTaskID(fence.TaskID) && domain.ValidUserID(fence.OwnerID) && domain.ValidUserID(fence.InitiatorID) &&
		agentCheckpointIDPattern.MatchString(fence.AgentInstanceID) && agentCheckpointIDPattern.MatchString(fence.AgentSessionID) &&
		(fence.AgentRootSessionID == "" || agentCheckpointIDPattern.MatchString(fence.AgentRootSessionID)) &&
		fence.AgentGeneration >= 1 && fence.AuthorizationVersion >= 1 && fence.LeaseVersion >= 1 &&
		!invalidCheckpointLabel(fence.LeaseOwner, 255) && !invalidCheckpointLabel(fence.EngineVersion, 128) &&
		!invalidCheckpointLabel(fence.ProfileVersion, 128)
}

func checkpointConversationMessages(payload []byte) ([]AgentConversationMessage, bool) {
	var body struct {
		Events []struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		} `json:"events"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, true
	}
	messages := make([]AgentConversationMessage, 0)
	byID := make(map[string]string)
	for _, event := range body.Events {
		if event.Type != "user/message" {
			continue
		}
		var data struct {
			ID      string          `json:"id"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if len(event.Data) == 0 || json.Unmarshal(event.Data, &data) != nil {
			return nil, true
		}
		if data.Role != "user" {
			return nil, true
		}
		if !domain.ValidMessageID(data.ID) {
			if checkpointUUID(data.ID) {
				continue
			}
			return nil, true
		}
		content, ok := checkpointTextContent(data.Content)
		if !ok || strings.TrimSpace(content) == "" || !utf8.ValidString(content) {
			return nil, true
		}
		if previous, exists := byID[data.ID]; exists {
			if previous != content {
				return nil, true
			}
			continue
		}
		byID[data.ID] = content
		messages = append(messages, AgentConversationMessage{ID: data.ID, Content: content})
	}
	return messages, false
}

func checkpointUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' || character >= 'A' && character <= 'F' {
			continue
		}
		return false
	}
	return true
}

func checkpointTextContent(raw json.RawMessage) (string, bool) {
	var content string
	if json.Unmarshal(raw, &content) == nil {
		return content, true
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &blocks) != nil {
		return "", false
	}
	var text strings.Builder
	found := false
	for _, block := range blocks {
		if block.Type == "text" {
			if !utf8.ValidString(block.Text) {
				return "", false
			}
			text.WriteString(block.Text)
			found = true
		}
	}
	return text.String(), found
}

func invalidCheckpointLabel(value string, max int) bool {
	return value == "" || len(value) > max || !utf8.ValidString(value)
}

func checkpointAAD(fence AgentCheckpointFence, from, through int64) ([]byte, error) {
	rootSessionID := fence.AgentRootSessionID
	if rootSessionID == fence.AgentSessionID {
		rootSessionID = ""
	}
	value, err := json.Marshal(struct {
		TaskID        string `json:"task_id"`
		OwnerID       string `json:"owner_id"`
		InitiatorID   string `json:"initiator_id,omitempty"`
		SessionID     string `json:"session_id"`
		RootSessionID string `json:"root_session_id,omitempty"`
		From          int64  `json:"from"`
		Through       int64  `json:"through"`
	}{TaskID: fence.TaskID, OwnerID: fence.OwnerID, InitiatorID: func() string {
		if fence.InitiatorID != fence.OwnerID {
			return fence.InitiatorID
		}
		return ""
	}(), SessionID: fence.AgentSessionID,
		RootSessionID: rootSessionID, From: from, Through: through})
	if err != nil {
		return nil, fmt.Errorf("encode checkpoint identity: %w", err)
	}
	return value, nil
}
