package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/personal"
)

const maxAgentCheckpointBytes = 1 << 20
const AgentMessageManifestVersion = 1
const AgentMessageReferenceEnvelopeVersion = 1

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
	InboxSplices                []AgentInboxSplice
	InboxSplicesInvalid         bool
	RootTurnStarted             bool
}

type AgentMessageDeliveryMode string

const (
	AgentMessageDeliverySteer    AgentMessageDeliveryMode = "steer"
	AgentMessageDeliveryFollowup AgentMessageDeliveryMode = "followup"
)

type AgentConversationMessage struct {
	ID                    string
	Content               string
	AttachmentIDs         []string
	Mode                  AgentMessageDeliveryMode
	Sequence              int64
	SourceSequence        int64
	SourceContentBytes    int64
	SourceSHA256          []byte
	ManifestVersion       int
	ManifestSHA256        []byte
	RepresentationVersion int
	RepresentationSHA256  []byte
	RepresentationKind    string
	SourceAgentGeneration int64
	TaskRevision          int64
	Reference             *AgentMessageReferenceEnvelope
	CheckpointEvidence    bool
	Initial               bool
	RevisionUpdate        bool
}

// AgentMessageSourcePage is the immutable rune range and page digest used by
// the R21 source manifest. SHA256 is lowercase hexadecimal for canonical JSON.
type AgentMessageSourcePage struct {
	RuneOffset     int64  `json:"rune_offset"`
	NextRuneOffset int64  `json:"next_rune_offset"`
	SHA256         string `json:"sha256"`
}

const AgentMessageReferenceMarkerPrefix = "MORDE_SOURCE_REFERENCE_V1"

// AgentMessageReferenceEnvelope is the bounded, versioned representation of
// an original conversation message sent to DSH. The original source digest is
// distinct from RepresentationSHA256, which binds this complete envelope.
type AgentMessageReferenceEnvelope struct {
	Version              int                      `json:"version"`
	TaskID               string                   `json:"task_id"`
	TaskRevision         int64                    `json:"task_revision"`
	OwnerID              string                   `json:"owner_id"`
	ConversationID       string                   `json:"conversation_id"`
	MessageID            string                   `json:"message_id"`
	MessageSequence      int64                    `json:"message_sequence"`
	AuthorizationVersion int64                    `json:"authorization_version"`
	AgentGeneration      int64                    `json:"agent_generation"`
	RootSessionID        string                   `json:"root_session_id"`
	Mode                 AgentMessageDeliveryMode `json:"mode"`
	AttachmentIDs        []string                 `json:"attachment_ids"`
	ContentBytes         int64                    `json:"content_bytes"`
	SourceSHA256         string                   `json:"source_sha256"`
	ManifestVersion      int                      `json:"manifest_version"`
	ManifestSHA256       string                   `json:"manifest_sha256"`
	TotalRunes           int64                    `json:"total_runes"`
	Pages                []AgentMessageSourcePage `json:"pages"`
	RepresentationSHA256 string                   `json:"representation_sha256"`
}

// AgentMessageSourceReference is the durable receipt and source metadata
// returned to a fenced Task. It deliberately does not contain message text.
type AgentMessageSourceReference struct {
	Envelope                 AgentMessageReferenceEnvelope
	RepresentationKind       string
	RepresentationVersion    int
	RepresentationSHA256     []byte
	AppliedCheckpointThrough int64
	// AuthorizedAgentGeneration is the current generation that the Store
	// rechecked before returning this immutable historical envelope. It can be
	// newer than Envelope.AgentGeneration only after checkpoint proof.
	AuthorizedAgentGeneration int64
}

// AgentMessageSourcePageRead is one verified canonical source page. Content
// is bounded by MaxConversationMessageChunkBytes and never includes other pages.
type AgentMessageSourcePageRead struct {
	MessageID          string
	SourceSHA256       []byte
	PageSHA256         []byte
	PageRuneOffset     int64
	PageNextRuneOffset int64
	FragmentSHA256     []byte
	RuneOffset         int64
	NextRuneOffset     int64
	TotalRunes         int64
	Content            string
}

type AgentInboxSplice struct {
	Sequence int64
	Target   string
	Messages []AgentConversationMessage
}

const checkpointAttachmentMarkerPrefix = "[MORDE_TASK_ATTACHMENT_IDS_V1:"

type AgentCheckpointStore interface {
	SaveAgentCheckpoint(context.Context, StoredAgentCheckpoint) (bool, error)
	ReadAgentCheckpointPage(context.Context, AgentCheckpointPageRequest) (StoredAgentCheckpoint, error)
	PendingMessages(context.Context, AgentCheckpointFence) ([]AgentConversationMessage, error)
}

// AgentMessageSourceStore exposes only authorized, individually receipted
// source metadata and bounded page reads. Implementations must recheck the
// complete current Task and Agent fence for every call.
type AgentMessageSourceStore interface {
	ReadAgentMessageSourceReference(context.Context, AgentCheckpointFence, string) (AgentMessageSourceReference, error)
	ReadAgentMessageSourcePage(context.Context, AgentCheckpointFence, string, []byte, int64) (AgentMessageSourcePageRead, error)
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
	var inboxSplices []AgentInboxSplice
	var invalidInboxSplices bool
	var turnStarted bool
	if command.Fence.AgentSessionID == command.Fence.AgentRootSessionID {
		messages, invalidMessages = checkpointConversationMessages(command.Payload)
		inboxSplices, invalidInboxSplices = checkpointInboxSplices(command.Payload)
		var err error
		turnStarted, err = checkpointRootTurnStarted(command.Payload)
		if err != nil {
			return false, err
		}
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
		ConversationMessages: messages, ConversationMessagesInvalid: invalidMessages, RootTurnStarted: turnStarted,
		InboxSplices: inboxSplices, InboxSplicesInvalid: invalidInboxSplices,
	})
}

// Only events in this newly committed segment count; reading old checkpoints
// during recovery must not claim a new revision has begun.
func checkpointRootTurnStarted(payload []byte) (bool, error) {
	var body struct {
		Events []struct {
			Sequence *int64          `json:"seq"`
			Type     string          `json:"type"`
			Data     json.RawMessage `json:"data"`
		} `json:"events"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return false, domain.InvalidArgument("checkpoint", "checkpoint turn event is invalid")
	}
	started := false
	for _, event := range body.Events {
		if event.Type == "turn/start" {
			var data struct {
				Turn int64 `json:"turn"`
			}
			if json.Unmarshal(event.Data, &data) != nil || data.Turn < 1 {
				return false, domain.InvalidArgument("checkpoint", "checkpoint turn event is invalid")
			}
			started = true
		}
	}
	return started, nil
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

// ReadMessageSourceReference returns only the verified R21 receipt metadata
// for a user message already accepted by the root checkpoint.
func (s *AgentCheckpointService) ReadMessageSourceReference(ctx context.Context, fence AgentCheckpointFence,
	messageID string) (AgentMessageSourceReference, error) {
	if fence.AgentRootSessionID == "" {
		fence.AgentRootSessionID = fence.AgentSessionID
	}
	if !validAgentCheckpointFence(fence) || fence.AgentSessionID != fence.AgentRootSessionID || !domain.ValidMessageID(messageID) {
		return AgentMessageSourceReference{}, domain.InvalidArgument("message_source", "message source identity is invalid")
	}
	reader, ok := s.store.(AgentMessageSourceStore)
	if !ok {
		return AgentMessageSourceReference{}, domain.NewError(domain.CodeInvalidState, "message source receipt reader is unavailable")
	}
	result, err := reader.ReadAgentMessageSourceReference(ctx, fence, messageID)
	if err != nil {
		return AgentMessageSourceReference{}, err
	}
	if err := ValidateAgentMessageSourceReference(result); err != nil ||
		result.Envelope.TaskID != fence.TaskID || result.Envelope.OwnerID != fence.OwnerID ||
		result.Envelope.AuthorizationVersion != fence.AuthorizationVersion ||
		result.Envelope.AgentGeneration > fence.AgentGeneration || result.AuthorizedAgentGeneration != fence.AgentGeneration ||
		result.Envelope.RootSessionID != fence.AgentRootSessionID || result.Envelope.MessageID != messageID ||
		result.AppliedCheckpointThrough < 0 {
		return AgentMessageSourceReference{}, domain.NewError(domain.CodeInvalidState, "message source receipt is unavailable or changed")
	}
	return result, nil
}

// ReadMessageSourcePage returns one canonical page for an individually
// receipted message after the Store rechecks the current Task and Agent fence.
func (s *AgentCheckpointService) ReadMessageSourcePage(ctx context.Context, fence AgentCheckpointFence,
	messageID string, expectedSourceSHA256 []byte, runeOffset int64) (AgentMessageSourcePageRead, error) {
	if fence.AgentRootSessionID == "" {
		fence.AgentRootSessionID = fence.AgentSessionID
	}
	if !validAgentCheckpointFence(fence) || fence.AgentSessionID != fence.AgentRootSessionID ||
		!domain.ValidMessageID(messageID) || len(expectedSourceSHA256) != sha256.Size || runeOffset < 0 {
		return AgentMessageSourcePageRead{}, domain.InvalidArgument("message_source_page", "message source page identity is invalid")
	}
	reader, ok := s.store.(AgentMessageSourceStore)
	if !ok {
		return AgentMessageSourcePageRead{}, domain.NewError(domain.CodeInvalidState, "message source page reader is unavailable")
	}
	result, err := reader.ReadAgentMessageSourcePage(ctx, fence, messageID, append([]byte(nil), expectedSourceSHA256...), runeOffset)
	if err != nil {
		return AgentMessageSourcePageRead{}, err
	}
	fragmentDigest := sha256.Sum256([]byte(result.Content))
	runeCount := int64(utf8.RuneCountInString(result.Content))
	if result.MessageID != messageID || !subtleCompareCheckpointDigest(result.SourceSHA256, expectedSourceSHA256) ||
		len(result.PageSHA256) != sha256.Size || len(result.FragmentSHA256) != sha256.Size ||
		!subtleCompareCheckpointDigest(fragmentDigest[:], result.FragmentSHA256) ||
		result.PageRuneOffset < 0 || result.PageNextRuneOffset <= result.PageRuneOffset ||
		result.RuneOffset != runeOffset || result.NextRuneOffset <= result.RuneOffset ||
		result.RuneOffset < result.PageRuneOffset || result.NextRuneOffset > result.PageNextRuneOffset ||
		result.NextRuneOffset-result.RuneOffset != runeCount || result.TotalRunes < result.PageNextRuneOffset ||
		!utf8.ValidString(result.Content) || strings.ContainsRune(result.Content, '\x00') ||
		len(result.Content) > MaxConversationMessageChunkBytes || len(result.Content) > 64<<10 {
		return AgentMessageSourcePageRead{}, domain.NewError(domain.CodeInvalidState, "message source page is invalid or changed")
	}
	return result, nil
}

// ComputeAgentMessageManifestSHA256 hashes the canonical R21 manifest JSON.
// Field order is version, message_id, sequence, content_bytes, source_sha256,
// total_runes, pages; each page orders rune_offset, next_rune_offset, sha256.
func ComputeAgentMessageManifestSHA256(messageID string, sequence, contentBytes int64, sourceSHA256 []byte,
	totalRunes int64, pages []AgentMessageSourcePage) ([]byte, error) {
	if !domain.ValidMessageID(messageID) || sequence < 1 || contentBytes < 0 || contentBytes > MaxConversationMessageBytes ||
		totalRunes < 0 || totalRunes > contentBytes || len(sourceSHA256) != sha256.Size ||
		!validAgentMessageSourcePages(contentBytes, totalRunes, pages) ||
		contentBytes == 0 && !validCanonicalEmptyAgentMessageSource(contentBytes, totalRunes, sourceSHA256, pages) {
		return nil, domain.InvalidArgument("message_source_manifest", "message source manifest fields are invalid")
	}
	canonical := struct {
		Version      int                      `json:"version"`
		MessageID    string                   `json:"message_id"`
		Sequence     int64                    `json:"sequence"`
		ContentBytes int64                    `json:"content_bytes"`
		SourceSHA256 string                   `json:"source_sha256"`
		TotalRunes   int64                    `json:"total_runes"`
		Pages        []AgentMessageSourcePage `json:"pages"`
	}{AgentMessageManifestVersion, messageID, sequence, contentBytes, hex.EncodeToString(sourceSHA256), totalRunes, append([]AgentMessageSourcePage{}, pages...)}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, domain.NewError(domain.CodeInvalidArgument, "message source manifest could not be encoded")
	}
	digest := sha256.Sum256(encoded)
	return append([]byte(nil), digest[:]...), nil
}

// ComputeAgentMessageReferenceRepresentationSHA256 hashes the canonical JSON
// representation envelope, excluding only representation_sha256. The marker
// header is a checked rendering of this envelope and never a second identity.
func ComputeAgentMessageReferenceRepresentationSHA256(envelope AgentMessageReferenceEnvelope) ([]byte, error) {
	if err := validateAgentMessageReferenceEnvelopeFields(envelope); err != nil {
		return nil, err
	}
	canonical := struct {
		Version              int                      `json:"version"`
		TaskID               string                   `json:"task_id"`
		TaskRevision         int64                    `json:"task_revision"`
		OwnerID              string                   `json:"owner_id"`
		ConversationID       string                   `json:"conversation_id"`
		MessageID            string                   `json:"message_id"`
		MessageSequence      int64                    `json:"message_sequence"`
		AuthorizationVersion int64                    `json:"authorization_version"`
		AgentGeneration      int64                    `json:"agent_generation"`
		RootSessionID        string                   `json:"root_session_id"`
		Mode                 AgentMessageDeliveryMode `json:"mode"`
		AttachmentIDs        []string                 `json:"attachment_ids"`
		ContentBytes         int64                    `json:"content_bytes"`
		SourceSHA256         string                   `json:"source_sha256"`
		ManifestVersion      int                      `json:"manifest_version"`
		ManifestSHA256       string                   `json:"manifest_sha256"`
		TotalRunes           int64                    `json:"total_runes"`
		Pages                []AgentMessageSourcePage `json:"pages"`
	}{envelope.Version, envelope.TaskID, envelope.TaskRevision, envelope.OwnerID, envelope.ConversationID,
		envelope.MessageID, envelope.MessageSequence, envelope.AuthorizationVersion, envelope.AgentGeneration,
		envelope.RootSessionID, envelope.Mode, append([]string{}, envelope.AttachmentIDs...), envelope.ContentBytes,
		envelope.SourceSHA256, envelope.ManifestVersion, envelope.ManifestSHA256, envelope.TotalRunes,
		append([]AgentMessageSourcePage{}, envelope.Pages...)}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, domain.NewError(domain.CodeInvalidArgument, "message reference representation could not be encoded")
	}
	digest := sha256.Sum256(encoded)
	return append([]byte(nil), digest[:]...), nil
}

// ComputeAgentMessageInlineRepresentationSHA256 binds the inline delivery
// fence to the original source hash and manifest without storing the body in
// the receipt. The source digest is what proves the exact UTF-8 content.
func ComputeAgentMessageInlineRepresentationSHA256(envelope AgentMessageReferenceEnvelope) ([]byte, error) {
	if err := validateAgentMessageReferenceEnvelopeFields(envelope); err != nil {
		return nil, err
	}
	canonical := struct {
		Version              int                      `json:"version"`
		RepresentationKind   string                   `json:"representation_kind"`
		TaskID               string                   `json:"task_id"`
		TaskRevision         int64                    `json:"task_revision"`
		OwnerID              string                   `json:"owner_id"`
		ConversationID       string                   `json:"conversation_id"`
		MessageID            string                   `json:"message_id"`
		MessageSequence      int64                    `json:"message_sequence"`
		AuthorizationVersion int64                    `json:"authorization_version"`
		AgentGeneration      int64                    `json:"agent_generation"`
		RootSessionID        string                   `json:"root_session_id"`
		Mode                 AgentMessageDeliveryMode `json:"mode"`
		AttachmentIDs        []string                 `json:"attachment_ids"`
		ContentBytes         int64                    `json:"content_bytes"`
		SourceSHA256         string                   `json:"source_sha256"`
		ManifestVersion      int                      `json:"manifest_version"`
		ManifestSHA256       string                   `json:"manifest_sha256"`
		TotalRunes           int64                    `json:"total_runes"`
		Pages                []AgentMessageSourcePage `json:"pages"`
	}{AgentMessageReferenceEnvelopeVersion, "inline", envelope.TaskID, envelope.TaskRevision, envelope.OwnerID,
		envelope.ConversationID, envelope.MessageID, envelope.MessageSequence, envelope.AuthorizationVersion,
		envelope.AgentGeneration, envelope.RootSessionID, envelope.Mode, append([]string{}, envelope.AttachmentIDs...),
		envelope.ContentBytes, envelope.SourceSHA256, envelope.ManifestVersion, envelope.ManifestSHA256,
		envelope.TotalRunes, append([]AgentMessageSourcePage{}, envelope.Pages...)}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, domain.NewError(domain.CodeInvalidArgument, "inline message representation could not be encoded")
	}
	digest := sha256.Sum256(encoded)
	return append([]byte(nil), digest[:]...), nil
}

// ValidateAgentMessageReferenceEnvelope verifies the source manifest and the
// separate representation digest. It does not imply the source was read.
func ValidateAgentMessageReferenceEnvelope(envelope AgentMessageReferenceEnvelope) error {
	if err := validateAgentMessageReferenceEnvelopeFields(envelope); err != nil {
		return err
	}
	if !validLowerHexDigest(envelope.RepresentationSHA256) {
		return domain.InvalidArgument("message_representation", "message reference representation digest is invalid")
	}
	sourceDigest, _ := hex.DecodeString(envelope.SourceSHA256)
	manifestDigest, err := ComputeAgentMessageManifestSHA256(envelope.MessageID, envelope.MessageSequence,
		envelope.ContentBytes, sourceDigest, envelope.TotalRunes, envelope.Pages)
	if err != nil || hex.EncodeToString(manifestDigest) != envelope.ManifestSHA256 {
		return domain.InvalidArgument("message_source_manifest", "message source manifest digest is invalid")
	}
	representationDigest, err := ComputeAgentMessageReferenceRepresentationSHA256(envelope)
	if err != nil || hex.EncodeToString(representationDigest) != envelope.RepresentationSHA256 {
		return domain.InvalidArgument("message_representation", "message reference representation digest is invalid")
	}
	return nil
}

// ValidateAgentMessageSourceReference verifies a stored receipt for either
// canonical reference delivery or legacy inline delivery.
func ValidateAgentMessageSourceReference(reference AgentMessageSourceReference) error {
	envelope := reference.Envelope
	if err := validateAgentMessageReferenceEnvelopeFields(envelope); err != nil {
		return err
	}
	if reference.RepresentationVersion != 1 || len(reference.RepresentationSHA256) != sha256.Size ||
		!validLowerHexDigest(envelope.RepresentationSHA256) ||
		!subtleCompareCheckpointDigest(reference.RepresentationSHA256, mustDecodeHexDigest(envelope.RepresentationSHA256)) {
		return domain.InvalidArgument("message_representation", "message source receipt representation is invalid")
	}
	var expected []byte
	var err error
	switch reference.RepresentationKind {
	case "inline":
		expected, err = ComputeAgentMessageInlineRepresentationSHA256(envelope)
	case "reference":
		expected, err = ComputeAgentMessageReferenceRepresentationSHA256(envelope)
	default:
		return domain.InvalidArgument("message_representation", "message source receipt representation kind is invalid")
	}
	if err != nil || !subtleCompareCheckpointDigest(expected, reference.RepresentationSHA256) {
		return domain.InvalidArgument("message_representation", "message source receipt representation digest changed")
	}
	manifestDigest, err := ComputeAgentMessageManifestSHA256(envelope.MessageID, envelope.MessageSequence,
		envelope.ContentBytes, mustDecodeHexDigest(envelope.SourceSHA256), envelope.TotalRunes, envelope.Pages)
	if err != nil || hex.EncodeToString(manifestDigest) != envelope.ManifestSHA256 {
		return domain.InvalidArgument("message_source_manifest", "message source receipt manifest digest changed")
	}
	return nil
}

// FormatAgentMessageReferenceEnvelope returns the exact readable marker used
// for a DSH user/message event. The JSON body is compact, ordered, and encoded
// as unpadded base64url; the header is checked against the hashed envelope.
func FormatAgentMessageReferenceEnvelope(envelope AgentMessageReferenceEnvelope) (string, error) {
	representationDigest, err := ComputeAgentMessageReferenceRepresentationSHA256(envelope)
	if err != nil {
		return "", err
	}
	representationHex := hex.EncodeToString(representationDigest)
	if envelope.RepresentationSHA256 != "" && envelope.RepresentationSHA256 != representationHex {
		return "", domain.InvalidArgument("message_representation", "message reference representation digest changed")
	}
	envelope.RepresentationSHA256 = representationHex
	if err := ValidateAgentMessageReferenceEnvelope(envelope); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(envelope)
	if err != nil || len(encoded) > 64<<10 {
		return "", domain.InvalidArgument("message_reference", "message reference envelope exceeds its limit")
	}
	return fmt.Sprintf("%s message_id=%s source_sha256=%s content_bytes=%d manifest_sha256=%s representation_sha256=%s\n%s",
		AgentMessageReferenceMarkerPrefix, envelope.MessageID, envelope.SourceSHA256, envelope.ContentBytes,
		envelope.ManifestSHA256, envelope.RepresentationSHA256,
		base64.RawURLEncoding.EncodeToString(encoded)), nil
}

// ParseAgentMessageReferenceEnvelope strictly parses and validates the marker
// emitted by FormatAgentMessageReferenceEnvelope.
func ParseAgentMessageReferenceEnvelope(content string) (AgentMessageReferenceEnvelope, error) {
	var envelope AgentMessageReferenceEnvelope
	lineEnd := strings.IndexByte(content, '\n')
	if lineEnd < 0 || strings.Contains(content[lineEnd+1:], "\n") {
		return envelope, domain.InvalidArgument("message_reference", "message reference marker is malformed")
	}
	header := strings.Fields(content[:lineEnd])
	if len(header) != 6 || header[0] != AgentMessageReferenceMarkerPrefix ||
		!strings.HasPrefix(header[1], "message_id=") || !strings.HasPrefix(header[2], "source_sha256=") ||
		!strings.HasPrefix(header[3], "content_bytes=") || !strings.HasPrefix(header[4], "manifest_sha256=") ||
		!strings.HasPrefix(header[5], "representation_sha256=") {
		return envelope, domain.InvalidArgument("message_reference", "message reference header is malformed")
	}
	body, err := base64.RawURLEncoding.DecodeString(content[lineEnd+1:])
	if err != nil || len(body) == 0 || len(body) > 64<<10 {
		return envelope, domain.InvalidArgument("message_reference", "message reference payload is malformed")
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return AgentMessageReferenceEnvelope{}, domain.InvalidArgument("message_reference", "message reference payload is malformed")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return AgentMessageReferenceEnvelope{}, domain.InvalidArgument("message_reference", "message reference payload has trailing data")
	}
	canonical, err := json.Marshal(envelope)
	if err != nil || !bytes.Equal(canonical, body) || header[1] != "message_id="+envelope.MessageID ||
		header[2] != "source_sha256="+envelope.SourceSHA256 ||
		header[3] != fmt.Sprintf("content_bytes=%d", envelope.ContentBytes) ||
		header[4] != "manifest_sha256="+envelope.ManifestSHA256 ||
		header[5] != "representation_sha256="+envelope.RepresentationSHA256 {
		return AgentMessageReferenceEnvelope{}, domain.InvalidArgument("message_reference", "message reference header or canonical payload changed")
	}
	if err := ValidateAgentMessageReferenceEnvelope(envelope); err != nil {
		return AgentMessageReferenceEnvelope{}, err
	}
	return envelope, nil
}

func validateAgentMessageReferenceEnvelopeFields(envelope AgentMessageReferenceEnvelope) error {
	emptySource := validCanonicalEmptyAgentMessageSource(envelope.ContentBytes, envelope.TotalRunes,
		mustDecodeHexDigest(envelope.SourceSHA256), envelope.Pages)
	if envelope.Version != AgentMessageReferenceEnvelopeVersion || !domain.ValidTaskID(envelope.TaskID) || envelope.TaskRevision < 0 ||
		!domain.ValidUserID(envelope.OwnerID) || !domain.ValidConversationID(envelope.ConversationID) ||
		!domain.ValidMessageID(envelope.MessageID) || envelope.MessageSequence < 1 ||
		envelope.AuthorizationVersion < 1 || envelope.AgentGeneration < 1 ||
		!agentCheckpointIDPattern.MatchString(envelope.RootSessionID) ||
		(envelope.Mode != AgentMessageDeliverySteer && envelope.Mode != AgentMessageDeliveryFollowup) ||
		envelope.ContentBytes < 1 && !emptySource || envelope.ContentBytes > MaxConversationMessageBytes ||
		envelope.ManifestVersion != AgentMessageManifestVersion || !validLowerHexDigest(envelope.SourceSHA256) ||
		!validLowerHexDigest(envelope.ManifestSHA256) ||
		envelope.TotalRunes < 1 && !emptySource || envelope.TotalRunes > envelope.ContentBytes ||
		len(envelope.AttachmentIDs) > conversation.MaxHistoryImportAttachments || envelope.AttachmentIDs == nil ||
		!validAgentMessageSourcePages(envelope.ContentBytes, envelope.TotalRunes, envelope.Pages) ||
		emptySource && len(envelope.AttachmentIDs) == 0 {
		return domain.InvalidArgument("message_reference", "message reference envelope fields are invalid")
	}
	for index, id := range envelope.AttachmentIDs {
		valid := len(id) == len("pa_")+32 && strings.HasPrefix(id, "pa_") ||
			len(id) == len("pca_")+32 && strings.HasPrefix(id, "pca_")
		if !valid || strings.ToLower(id) != id || index > 0 && envelope.AttachmentIDs[index-1] >= id {
			return domain.InvalidArgument("message_reference.attachment_ids", "message reference attachments are invalid")
		}
		if _, err := hex.DecodeString(id[strings.IndexByte(id, '_')+1:]); err != nil {
			return domain.InvalidArgument("message_reference.attachment_ids", "message reference attachments are invalid")
		}
	}
	return nil
}

func validAgentMessageSourcePages(contentBytes, totalRunes int64, pages []AgentMessageSourcePage) bool {
	if contentBytes == 0 || totalRunes == 0 || len(pages) == 0 {
		return contentBytes == 0 && totalRunes == 0 && pages != nil && len(pages) == 0
	}
	if contentBytes < 1 || totalRunes < 1 || len(pages) == 0 ||
		int64(len(pages)) > (contentBytes+MaxConversationMessageChunkBytes-1)/MaxConversationMessageChunkBytes+1 {
		return false
	}
	var next int64
	for _, page := range pages {
		span := page.NextRuneOffset - page.RuneOffset
		if page.RuneOffset != next || page.NextRuneOffset <= page.RuneOffset || page.NextRuneOffset > totalRunes ||
			span > MaxConversationMessageChunkRunes || !validLowerHexDigest(page.SHA256) {
			return false
		}
		next = page.NextRuneOffset
	}
	return next == totalRunes
}

func validCanonicalEmptyAgentMessageSource(contentBytes, totalRunes int64, sourceSHA256 []byte,
	pages []AgentMessageSourcePage) bool {
	emptyDigest := sha256.Sum256(nil)
	return contentBytes == 0 && totalRunes == 0 && len(sourceSHA256) == sha256.Size &&
		subtleCompareCheckpointDigest(sourceSHA256, emptyDigest[:]) && pages != nil && len(pages) == 0
}

func validLowerHexDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func mustDecodeHexDigest(value string) []byte {
	decoded, _ := hex.DecodeString(value)
	return decoded
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
			Sequence *int64          `json:"seq"`
			Type     string          `json:"type"`
			Data     json.RawMessage `json:"data"`
		} `json:"events"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, true
	}
	messages := make([]AgentConversationMessage, 0)
	byID := make(map[string]AgentConversationMessage)
	for _, event := range body.Events {
		if event.Type != "user/message" {
			continue
		}
		if event.Sequence == nil || *event.Sequence < 0 {
			return nil, true
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
		content, attachmentIDs, ok := checkpointMessageContent(data.Content)
		if !ok || (strings.TrimSpace(content) == "" && len(attachmentIDs) == 0) || !utf8.ValidString(content) {
			return nil, true
		}
		message := AgentConversationMessage{ID: data.ID, Content: content, AttachmentIDs: attachmentIDs, Sequence: *event.Sequence,
			RepresentationKind: "inline", CheckpointEvidence: true}
		if hasAgentMessageReferenceEnvelopeHeader(content) {
			reference, err := ParseAgentMessageReferenceEnvelope(content)
			if err != nil || reference.MessageID != data.ID || !sameCheckpointAttachmentIDs(reference.AttachmentIDs, attachmentIDs) {
				return nil, true
			}
			message.Reference = &reference
			message.Mode = reference.Mode
			message.RepresentationKind = "reference"
			message.SourceSequence = reference.MessageSequence
			message.SourceContentBytes = reference.ContentBytes
			message.SourceSHA256, _ = hex.DecodeString(reference.SourceSHA256)
			message.ManifestVersion = reference.ManifestVersion
			message.ManifestSHA256, _ = hex.DecodeString(reference.ManifestSHA256)
			message.RepresentationVersion = 1
			message.RepresentationSHA256, _ = hex.DecodeString(reference.RepresentationSHA256)
			message.TaskRevision = reference.TaskRevision
		}
		if previous, exists := byID[data.ID]; exists {
			if previous.Content != content || !sameCheckpointAttachmentIDs(previous.AttachmentIDs, attachmentIDs) ||
				!sameAgentMessageReference(previous.Reference, message.Reference) {
				return nil, true
			}
			continue
		}
		byID[data.ID] = message
		messages = append(messages, message)
	}
	return messages, false
}

func checkpointInboxSplices(payload []byte) ([]AgentInboxSplice, bool) {
	var body struct {
		Events []struct {
			Sequence *int64          `json:"seq"`
			Type     string          `json:"type"`
			Data     json.RawMessage `json:"data"`
		} `json:"events"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, true
	}
	splices := make([]AgentInboxSplice, 0)
	for _, event := range body.Events {
		if event.Type != "agent/inbox/spliced" {
			continue
		}
		var data struct {
			Target   string `json:"target"`
			Inserted []struct {
				ID      string          `json:"id"`
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
				Source  struct {
					Kind string `json:"kind"`
				} `json:"source"`
			} `json:"inserted"`
		}
		if event.Sequence == nil || *event.Sequence < 0 || json.Unmarshal(event.Data, &data) != nil ||
			(data.Target != "next-step" && data.Target != "next-turn") {
			return nil, true
		}
		splice := AgentInboxSplice{Sequence: *event.Sequence, Target: data.Target, Messages: make([]AgentConversationMessage, 0, len(data.Inserted))}
		for _, inserted := range data.Inserted {
			if inserted.Source.Kind != "user" {
				continue
			}
			if !domain.ValidMessageID(inserted.ID) {
				if checkpointUUID(inserted.ID) {
					continue
				}
				return nil, true
			}
			if inserted.Role != "" && inserted.Role != "user" {
				return nil, true
			}
			content, attachmentIDs, ok := checkpointMessageContent(inserted.Content)
			if !ok || (strings.TrimSpace(content) == "" && len(attachmentIDs) == 0) || !utf8.ValidString(content) {
				return nil, true
			}
			mode := AgentMessageDeliveryFollowup
			if data.Target == "next-step" {
				mode = AgentMessageDeliverySteer
			}
			message := AgentConversationMessage{ID: inserted.ID, Content: content, AttachmentIDs: attachmentIDs, Mode: mode,
				RepresentationKind: "inline", CheckpointEvidence: true}
			if hasAgentMessageReferenceEnvelopeHeader(content) {
				reference, err := ParseAgentMessageReferenceEnvelope(content)
				if err != nil || reference.MessageID != inserted.ID || reference.Mode != mode ||
					!sameCheckpointAttachmentIDs(reference.AttachmentIDs, attachmentIDs) {
					return nil, true
				}
				message.Reference = &reference
				message.RepresentationKind = "reference"
				message.SourceSequence = reference.MessageSequence
				message.SourceContentBytes = reference.ContentBytes
				message.SourceSHA256, _ = hex.DecodeString(reference.SourceSHA256)
				message.ManifestVersion = reference.ManifestVersion
				message.ManifestSHA256, _ = hex.DecodeString(reference.ManifestSHA256)
				message.RepresentationVersion = 1
				message.RepresentationSHA256, _ = hex.DecodeString(reference.RepresentationSHA256)
				message.TaskRevision = reference.TaskRevision
			}
			splice.Messages = append(splice.Messages, message)
		}
		if len(splice.Messages) != 0 {
			splices = append(splices, splice)
		}
	}
	return splices, false
}

func sameAgentMessageReference(left, right *AgentMessageReferenceEnvelope) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftBytes, leftErr := json.Marshal(left)
	rightBytes, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && subtleCompareCheckpointDigest(sha256DigestBytes(leftBytes), sha256DigestBytes(rightBytes))
}

func hasAgentMessageReferenceEnvelopeHeader(content string) bool {
	lineEnd := strings.IndexByte(content, '\n')
	if lineEnd < 0 {
		return false
	}
	header := content[:lineEnd]
	fields := strings.Fields(header)
	if len(fields) != 6 || strings.Join(fields, " ") != header || fields[0] != AgentMessageReferenceMarkerPrefix ||
		!strings.HasPrefix(fields[1], "message_id=") || !strings.HasPrefix(fields[2], "source_sha256=") ||
		!strings.HasPrefix(fields[3], "content_bytes=") || !strings.HasPrefix(fields[4], "manifest_sha256=") ||
		!strings.HasPrefix(fields[5], "representation_sha256=") {
		return false
	}
	messageID := strings.TrimPrefix(fields[1], "message_id=")
	sourceSHA256 := strings.TrimPrefix(fields[2], "source_sha256=")
	contentBytesText := strings.TrimPrefix(fields[3], "content_bytes=")
	manifestSHA256 := strings.TrimPrefix(fields[4], "manifest_sha256=")
	representationSHA256 := strings.TrimPrefix(fields[5], "representation_sha256=")
	contentBytes, err := strconv.ParseInt(contentBytesText, 10, 64)
	return domain.ValidMessageID(messageID) && validLowerHexDigest(sourceSHA256) && err == nil && contentBytes > 0 &&
		strconv.FormatInt(contentBytes, 10) == contentBytesText && validLowerHexDigest(manifestSHA256) &&
		validLowerHexDigest(representationSHA256)
}

func sha256DigestBytes(value []byte) []byte {
	digest := sha256.Sum256(value)
	return digest[:]
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

func checkpointMessageContent(raw json.RawMessage) (string, []string, bool) {
	var content string
	if json.Unmarshal(raw, &content) == nil {
		return content, nil, true
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &blocks) != nil {
		return "", nil, false
	}
	var attachmentIDs []string
	if len(blocks) >= 2 && blocks[len(blocks)-1].Type == "text" && strings.HasPrefix(blocks[len(blocks)-1].Text, checkpointAttachmentMarkerPrefix) {
		marker := blocks[len(blocks)-1].Text
		if !strings.HasSuffix(marker, "]") {
			return "", nil, false
		}
		encoded := strings.TrimSuffix(strings.TrimPrefix(marker, checkpointAttachmentMarkerPrefix), "]")
		if encoded == "" {
			return "", nil, false
		}
		attachmentIDs = strings.Split(encoded, ",")
		if len(attachmentIDs) > conversation.MaxHistoryImportAttachments || !sort.StringsAreSorted(attachmentIDs) {
			return "", nil, false
		}
		for index, id := range attachmentIDs {
			if (!personal.ValidPersonalAttachmentID(id) && !ValidProjectConversationAttachmentID(id)) || index > 0 && attachmentIDs[index-1] == id {
				return "", nil, false
			}
		}
		blocks = blocks[:len(blocks)-1]
	}
	var text strings.Builder
	found := false
	for _, block := range blocks {
		if block.Type == "text" {
			if !utf8.ValidString(block.Text) {
				return "", nil, false
			}
			text.WriteString(block.Text)
			found = true
		}
	}
	return text.String(), attachmentIDs, found
}

func sameCheckpointAttachmentIDs(left, right []string) bool {
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
