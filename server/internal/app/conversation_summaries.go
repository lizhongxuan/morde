package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

const (
	ConversationSummaryPersonalScope     = "personal"
	ConversationSummaryProjectScope      = "project"
	ConversationSummarySourceMessage     = "message"
	ConversationSummarySourceTask        = "task_result"
	ConversationSummarySourceMemory      = "memory"
	MaxConversationSummarySources        = 8192
	MaxConversationSummaryBytes          = 256 << 10
	MaxConversationSummaryCiphertext     = 1 << 20
	MaxConversationSummaryInputPageBytes = 64 << 10
)

type ConversationSummaryIdentity struct {
	ServiceInstanceID    string
	OwnerID              string
	ActorID              string
	ConversationID       string
	ScopeKind            string
	ScopeID              string
	AuthorizationVersion int64
	ScopeVersion         int64
}

type ConversationSummaryHead struct {
	Identity         ConversationSummaryIdentity
	Version          int64
	Digest           []byte
	LeaseOperationID string
	LeaseOwner       string
	LeaseVersion     int64
	LeaseExpiresAt   *time.Time
}

type ConversationSummarySource struct {
	Kind       string
	ID         string
	ObjectType string
	Version    int64
	Sequence   int64
	SHA256     []byte
}

type ConversationSummaryPromptSource struct {
	Source     ConversationSummarySource
	Role       string
	Content    string
	ByteOffset int64
}

type StoredConversationSummary struct {
	Identity      ConversationSummaryIdentity
	Version       int64
	Previous      *int64
	OperationID   string
	SummarySHA256 []byte
	SourcesSHA256 []byte
	Ciphertext    []byte
	Sources       []ConversationSummarySource
}

type ConversationSummaryDocument struct {
	Identity    ConversationSummaryIdentity
	Version     int64
	OperationID string
	Text        string
	Sources     []ConversationSummarySource
}

type ConversationSummaryOperation struct {
	Identity            ConversationSummaryIdentity
	OperationID         string
	BaselineVersion     int64
	InputSHA256         []byte
	State               string
	LeaseOwner          string
	LeaseVersion        int64
	LeaseExpiresAt      *time.Time
	CheckpointSessionID string
	CheckpointThrough   int64
	CheckpointSHA256    []byte
	ModelCallID         string
}

type ConversationSummaryModelFence struct {
	Claim               ConversationRouteClaim
	Identity            ConversationSummaryIdentity
	OperationID         string
	InputSHA256         []byte
	LeaseOwner          string
	LeaseVersion        int64
	SessionID           string
	ThroughSequence     int64
	BaselineVersion     int64
	ContextWindowTokens int
}

type ConversationSummaryModelOutput struct {
	Identity    ConversationSummaryIdentity
	OperationID string
	CallID      string
	ModelID     string
	Text        string
	Usage       ModelUsage
	Ciphertext  []byte
	Digest      []byte
}

type ClaimConversationSummaryOperation struct {
	Claim           ConversationRouteClaim
	OperationID     string
	InputSHA256     []byte
	BaselineVersion int64
	LeaseOwner      string
	LeaseDuration   time.Duration
}

type ConversationSummaryCheckpoint struct {
	Claim        ConversationRouteClaim
	OperationID  string
	LeaseOwner   string
	LeaseVersion int64
	SessionID    string
	Through      int64
	SHA256       []byte
}

type ConversationSummaryCommit struct {
	Claim           ConversationRouteClaim
	Identity        ConversationSummaryIdentity
	OperationID     string
	LeaseOwner      string
	LeaseVersion    int64
	BaselineVersion int64
	SummarySHA256   []byte
	SourcesSHA256   []byte
	Ciphertext      []byte
	Sources         []ConversationSummarySource
}

type ConversationSummaryOperationRelease struct {
	Claim        ConversationRouteClaim
	OperationID  string
	LeaseOwner   string
	LeaseVersion int64
	State        string
}

type ConversationSummarySourceInvalidation struct {
	OwnerID    string
	Kind       string
	ID         string
	ObjectType string
	Version    int64
	Sequence   int64
	Reason     string
}

type ConversationSummaryStore interface {
	ReadConversationSummaryHead(context.Context, ConversationRouteClaim) (ConversationSummaryHead, error)
	ReadConversationSummary(context.Context, ConversationRouteClaim) (StoredConversationSummary, bool, error)
	ClaimConversationSummaryOperation(context.Context, ClaimConversationSummaryOperation) (ConversationSummaryOperation, bool, error)
	SaveConversationSummaryCheckpoint(context.Context, ConversationSummaryCheckpoint) (bool, error)
	AttachConversationSummaryModelCall(context.Context, ConversationRouteClaim, string, string, int64, string) error
	CommitConversationSummary(context.Context, ConversationSummaryCommit) error
	ReleaseConversationSummaryOperation(context.Context, ConversationSummaryOperationRelease) error
	InvalidateConversationSummarySource(context.Context, ConversationSummarySourceInvalidation) (int64, error)
}

type TaskConversationSummaryValidator interface {
	ValidateTaskConversationSummary(context.Context, task.Task) error
}

func ConversationSummaryScopeForClaim(claim ConversationRouteClaim) (ConversationSummaryIdentity, error) {
	if claim.Message.Role != conversation.RoleUser || claim.Message.Sequence < 1 || claim.Input.ResultReviewID != "" ||
		!domain.ValidServiceInstanceID(claim.ServiceInstanceID) || !domain.ValidUserID(claim.Message.OwnerID) ||
		!domain.ValidUserID(claim.Message.ActorID) || !domain.ValidConversationID(claim.Message.ConversationID) ||
		claim.Authorization.Version < 1 || claim.LeaseOwner == "" || claim.LeaseVersion < 1 {
		return ConversationSummaryIdentity{}, domain.InvalidArgument("conversation_summary", "conversation summary route is invalid")
	}
	identity := ConversationSummaryIdentity{
		ServiceInstanceID: claim.ServiceInstanceID, OwnerID: claim.Message.OwnerID, ActorID: claim.Message.ActorID,
		ConversationID: claim.Message.ConversationID, AuthorizationVersion: claim.Authorization.Version,
	}
	if claim.Input.ProjectID == "" {
		if identity.ActorID != identity.OwnerID {
			return ConversationSummaryIdentity{}, domain.InvalidArgument("conversation_summary", "personal summary actor is invalid")
		}
		identity.ScopeKind = ConversationSummaryPersonalScope
		return identity, nil
	}
	if !project.ValidID(claim.Input.ProjectID) {
		return ConversationSummaryIdentity{}, domain.InvalidArgument("conversation_summary", "project summary scope is invalid")
	}
	identity.ScopeKind = ConversationSummaryProjectScope
	identity.ScopeID = claim.Input.ProjectID
	return identity, nil
}

func ValidateConversationSummarySource(source ConversationSummarySource) error {
	if source.ID == "" || len(source.ID) > 128 || strings.TrimSpace(source.ID) != source.ID ||
		!utf8.ValidString(source.ID) || source.Version < 0 || source.Sequence < 0 || len(source.SHA256) != sha256.Size {
		return domain.InvalidArgument("conversation_summary_source", "conversation summary source is invalid")
	}
	switch source.Kind {
	case ConversationSummarySourceMessage:
		if !domain.ValidMessageID(source.ID) || source.ObjectType != "" || source.Version != 0 || source.Sequence < 1 {
			return domain.InvalidArgument("conversation_summary_source", "conversation summary source is invalid")
		}
	case ConversationSummarySourceTask:
		if !domain.ValidTaskID(source.ID) || source.ObjectType != "" || source.Sequence < 1 {
			return domain.InvalidArgument("conversation_summary_source", "conversation summary source is invalid")
		}
	case ConversationSummarySourceMemory:
		if strings.TrimSpace(source.ObjectType) != source.ObjectType || source.ObjectType == "" || len(source.ObjectType) > 64 || source.Version < 1 || source.Sequence != 0 {
			return domain.InvalidArgument("conversation_summary_source", "conversation summary source is invalid")
		}
	default:
		return domain.InvalidArgument("conversation_summary_source", "conversation summary source is invalid")
	}
	return nil
}

func ConversationSummarySourcesDigest(sources []ConversationSummarySource) ([]byte, error) {
	if len(sources) == 0 || len(sources) > MaxConversationSummarySources {
		return nil, domain.InvalidArgument("conversation_summary_source", "conversation summary sources are invalid")
	}
	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		if err := ValidateConversationSummarySource(source); err != nil {
			return nil, err
		}
		key := strings.Join([]string{source.Kind, source.ID, source.ObjectType, strconv.FormatInt(source.Version, 10), strconv.FormatInt(source.Sequence, 10)}, "\x00")
		if _, ok := seen[key]; ok {
			return nil, domain.InvalidArgument("conversation_summary_source", "conversation summary sources are duplicated")
		}
		seen[key] = struct{}{}
	}
	encoded, err := json.Marshal(sources)
	if err != nil {
		return nil, domain.InvalidArgument("conversation_summary_source", "conversation summary sources cannot be encoded")
	}
	digest := sha256.Sum256(encoded)
	return append([]byte(nil), digest[:]...), nil
}

func ConversationSummaryPromptDigest(sources []ConversationSummaryPromptSource) ([]byte, error) {
	if len(sources) == 0 || len(sources) > MaxConversationSummarySources {
		return nil, domain.InvalidArgument("conversation_summary_source", "conversation summary prompt sources are invalid")
	}
	proofs := make([]ConversationSummarySource, 0, len(sources))
	proofByKey := make(map[string]ConversationSummarySource, len(sources))
	partEnds := make(map[string]int64, len(sources))
	type promptSource struct {
		Kind       string `json:"kind"`
		ID         string `json:"id"`
		ObjectType string `json:"object_type"`
		Version    int64  `json:"version"`
		Sequence   int64  `json:"sequence"`
		SHA256     []byte `json:"sha256"`
		Role       string `json:"role"`
		ByteOffset int64  `json:"byte_offset"`
		Content    string `json:"content"`
	}
	canonical := make([]promptSource, 0, len(sources))
	for _, item := range sources {
		if err := ValidateConversationSummarySource(item.Source); err != nil ||
			(item.Role != "user" && item.Role != "assistant") || item.Content == "" || len(item.Content) > MaxConversationSummaryInputPageBytes ||
			item.ByteOffset < 0 || item.ByteOffset > int64(^uint64(0)>>1)-int64(len(item.Content)) ||
			!utf8.ValidString(item.Content) || strings.ContainsRune(item.Content, '\x00') {
			return nil, domain.InvalidArgument("conversation_summary_source", "conversation summary prompt source is invalid")
		}
		key := summarySourceKey(item.Source)
		if existing, ok := proofByKey[key]; ok {
			if existing.Kind != item.Source.Kind || existing.ID != item.Source.ID || existing.ObjectType != item.Source.ObjectType ||
				existing.Version != item.Source.Version || existing.Sequence != item.Source.Sequence || !bytes.Equal(existing.SHA256, item.Source.SHA256) {
				return nil, domain.InvalidArgument("conversation_summary_source", "conversation summary prompt source proof changed")
			}
		} else {
			proofByKey[key] = item.Source
			proofs = append(proofs, item.Source)
		}
		if previousEnd, ok := partEnds[key]; ok && previousEnd != item.ByteOffset {
			return nil, domain.InvalidArgument("conversation_summary_source", "conversation summary prompt parts are not contiguous")
		}
		partEnds[key] = item.ByteOffset + int64(len(item.Content))
		canonical = append(canonical, promptSource{item.Source.Kind, item.Source.ID, item.Source.ObjectType,
			item.Source.Version, item.Source.Sequence, item.Source.SHA256, item.Role, item.ByteOffset, item.Content})
	}
	if _, err := ConversationSummarySourcesDigest(proofs); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(struct {
		Version int            `json:"version"`
		Sources []promptSource `json:"sources"`
	}{1, canonical})
	if err != nil || len(encoded) > MaxConversationSummaryCiphertext {
		return nil, domain.InvalidArgument("conversation_summary_source", "conversation summary prompt exceeds its limit")
	}
	digest := sha256.Sum256(encoded)
	return append([]byte(nil), digest[:]...), nil
}

func summarySourceKey(source ConversationSummarySource) string {
	return strings.Join([]string{source.Kind, source.ID, source.ObjectType, strconv.FormatInt(source.Version, 10), strconv.FormatInt(source.Sequence, 10)}, "\x00")
}

// ConversationSummaryInputPageBudget leaves room for DSH metadata and the
// summarizer prompt while staying bounded by the Morde gateway input limit.
func ConversationSummaryInputPageBudget(contextWindowTokens int) (int, error) {
	if contextWindowTokens < 4096 || contextWindowTokens > 1_000_000 {
		return 0, domain.InvalidArgument("conversation_summary_source", "conversation summary model capacity is invalid")
	}
	return min(contextWindowTokens*2, MaxConversationSummaryInputPageBytes), nil
}

func ConversationSummaryPromptProofs(sources []ConversationSummaryPromptSource) ([]ConversationSummarySource, error) {
	if _, err := ConversationSummaryPromptDigest(sources); err != nil {
		return nil, err
	}
	proofs := make([]ConversationSummarySource, 0, len(sources))
	seen := make(map[string]struct{}, len(sources))
	for _, item := range sources {
		key := summarySourceKey(item.Source)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		proofs = append(proofs, item.Source)
	}
	return proofs, nil
}

func ConversationSummaryMemorySourceDigest(identity ConversationSummaryIdentity, source ConversationSummarySource) []byte {
	encoded, _ := json.Marshal(struct {
		ServiceInstanceID string `json:"service_instance_id"`
		OwnerID           string `json:"owner_id"`
		ActorID           string `json:"actor_id"`
		ConversationID    string `json:"conversation_id"`
		ScopeKind         string `json:"scope_kind"`
		ScopeID           string `json:"scope_id"`
		ObjectType        string `json:"object_type"`
		ObjectID          string `json:"object_id"`
		Version           int64  `json:"version"`
	}{identity.ServiceInstanceID, identity.OwnerID, identity.ActorID, identity.ConversationID,
		identity.ScopeKind, identity.ScopeID, source.ObjectType, source.ID, source.Version})
	digest := sha256.Sum256(encoded)
	return append([]byte(nil), digest[:]...)
}

func DSHConversationSummaryModelCallID(conversationID, operationID, sessionID string, throughSequence int64) (string, error) {
	if !domain.ValidConversationID(conversationID) || !domain.ValidOperationID(operationID) ||
		!dshModelIdentityPattern.MatchString(sessionID) || throughSequence < 0 || throughSequence > 1<<53-1 {
		return "", domain.InvalidArgument("model_call", "invalid conversation summary model call identity")
	}
	digest := sha256.Sum256([]byte("morde-dsh-summary-model-v1\n" + conversationID + "\n" + operationID + "\n" + sessionID + "\n" + strconv.FormatInt(throughSequence, 10)))
	return "cmp_" + hex.EncodeToString(digest[:]), nil
}

func SealConversationSummaryModelOutput(sealer *Sealer, output ConversationSummaryModelOutput) ([]byte, []byte, error) {
	if sealer == nil || !validConversationSummaryIdentity(output.Identity) || !domain.ValidOperationID(output.OperationID) ||
		len(output.CallID) < 8 || len(output.CallID) > 128 || strings.TrimSpace(output.CallID) != output.CallID ||
		!domain.ValidModelConnectionID(output.ModelID) || strings.TrimSpace(output.Text) == "" || !utf8.ValidString(output.Text) ||
		strings.ContainsRune(output.Text, '\x00') || len(output.Text) > MaxConversationSummaryBytes || !output.Usage.Known ||
		output.Usage.InputTokens < 0 || output.Usage.OutputTokens < 0 || output.Usage.InputTokens > int64(^uint64(0)>>1)-output.Usage.OutputTokens {
		return nil, nil, domain.InvalidArgument("conversation_summary_model_output", "conversation summary model output is invalid")
	}
	plain, err := json.Marshal(output)
	if err != nil || len(plain) > MaxConversationSummaryCiphertext {
		return nil, nil, domain.InvalidArgument("conversation_summary_model_output", "conversation summary model output is too large")
	}
	ciphertext, err := sealer.Seal(plain)
	if err != nil {
		return nil, nil, err
	}
	digest := sha256.Sum256(plain)
	return ciphertext, append([]byte(nil), digest[:]...), nil
}

func OpenConversationSummaryModelOutput(sealer *Sealer, ciphertext, digest []byte,
	identity ConversationSummaryIdentity, operationID, callID, modelID string) (ConversationSummaryModelOutput, error) {
	if sealer == nil || len(ciphertext) == 0 || len(ciphertext) > MaxConversationSummaryCiphertext || len(digest) != sha256.Size {
		return ConversationSummaryModelOutput{}, domain.NewError(domain.CodeInvalidState, "stored conversation summary model output is invalid")
	}
	plain, err := sealer.Open(ciphertext)
	if err != nil {
		return ConversationSummaryModelOutput{}, domain.NewError(domain.CodeInvalidState, "stored conversation summary model output could not be opened")
	}
	actualDigest := sha256.Sum256(plain)
	if !bytes.Equal(actualDigest[:], digest) {
		return ConversationSummaryModelOutput{}, domain.NewError(domain.CodeInvalidState, "stored conversation summary model output digest changed")
	}
	var output ConversationSummaryModelOutput
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&output) != nil || output.Identity != identity || output.OperationID != operationID ||
		output.CallID != callID || output.ModelID != modelID || strings.TrimSpace(output.Text) == "" ||
		!utf8.ValidString(output.Text) || strings.ContainsRune(output.Text, '\x00') || len(output.Text) > MaxConversationSummaryBytes ||
		!output.Usage.Known || output.Usage.InputTokens < 0 || output.Usage.OutputTokens < 0 ||
		output.Usage.InputTokens > int64(^uint64(0)>>1)-output.Usage.OutputTokens {
		return ConversationSummaryModelOutput{}, domain.NewError(domain.CodeInvalidState, "stored conversation summary model output identity changed")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return ConversationSummaryModelOutput{}, domain.NewError(domain.CodeInvalidState, "stored conversation summary model output has trailing data")
	}
	return output, nil
}

func SealConversationSummary(sealer *Sealer, document ConversationSummaryDocument) (StoredConversationSummary, error) {
	if sealer == nil || !validConversationSummaryIdentity(document.Identity) || document.Version < 1 ||
		!domain.ValidOperationID(document.OperationID) || strings.TrimSpace(document.Text) == "" ||
		!utf8.ValidString(document.Text) || strings.ContainsRune(document.Text, '\x00') || len(document.Text) > MaxConversationSummaryBytes {
		return StoredConversationSummary{}, domain.InvalidArgument("conversation_summary", "conversation summary is invalid")
	}
	sourceDigest, err := ConversationSummarySourcesDigest(document.Sources)
	if err != nil {
		return StoredConversationSummary{}, err
	}
	encoded, err := json.Marshal(document)
	if err != nil || len(encoded) > MaxConversationSummaryCiphertext {
		return StoredConversationSummary{}, domain.InvalidArgument("conversation_summary", "conversation summary is too large")
	}
	ciphertext, err := sealer.Seal(encoded)
	if err != nil {
		return StoredConversationSummary{}, err
	}
	return StoredConversationSummary{
		Identity: document.Identity, Version: document.Version, OperationID: document.OperationID,
		SummarySHA256: digestBytes([]byte(document.Text)), SourcesSHA256: sourceDigest, Ciphertext: ciphertext,
		Sources: append([]ConversationSummarySource(nil), document.Sources...),
	}, nil
}

func OpenConversationSummary(sealer *Sealer, stored StoredConversationSummary) (ConversationSummaryDocument, error) {
	if sealer == nil || !validConversationSummaryIdentity(stored.Identity) || stored.Version < 1 ||
		!domain.ValidOperationID(stored.OperationID) || len(stored.SummarySHA256) != sha256.Size ||
		len(stored.SourcesSHA256) != sha256.Size || len(stored.Ciphertext) == 0 || len(stored.Ciphertext) > MaxConversationSummaryCiphertext {
		return ConversationSummaryDocument{}, domain.NewError(domain.CodeInvalidState, "stored conversation summary is invalid")
	}
	plain, err := sealer.Open(stored.Ciphertext)
	if err != nil {
		return ConversationSummaryDocument{}, domain.NewError(domain.CodeInvalidState, "stored conversation summary could not be opened")
	}
	var document ConversationSummaryDocument
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&document) != nil || document.Identity != stored.Identity || document.Version != stored.Version ||
		document.OperationID != stored.OperationID || strings.TrimSpace(document.Text) == "" || !utf8.ValidString(document.Text) ||
		strings.ContainsRune(document.Text, '\x00') || len(document.Text) > MaxConversationSummaryBytes ||
		!bytes.Equal(digestBytes([]byte(document.Text)), stored.SummarySHA256) {
		return ConversationSummaryDocument{}, domain.NewError(domain.CodeInvalidState, "stored conversation summary does not match its identity")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return ConversationSummaryDocument{}, domain.NewError(domain.CodeInvalidState, "stored conversation summary has trailing data")
	}
	sourceDigest, err := ConversationSummarySourcesDigest(document.Sources)
	if err != nil || !bytes.Equal(sourceDigest, stored.SourcesSHA256) || !sameConversationSummarySources(document.Sources, stored.Sources) {
		return ConversationSummaryDocument{}, domain.NewError(domain.CodeInvalidState, "stored conversation summary sources do not match their proof")
	}
	return document, nil
}

func validConversationSummaryIdentity(value ConversationSummaryIdentity) bool {
	if !domain.ValidServiceInstanceID(value.ServiceInstanceID) || !domain.ValidUserID(value.OwnerID) ||
		!domain.ValidUserID(value.ActorID) || !domain.ValidConversationID(value.ConversationID) || value.AuthorizationVersion < 1 {
		return false
	}
	if value.ScopeKind == ConversationSummaryPersonalScope {
		return value.ScopeID == "" && value.ActorID == value.OwnerID && value.ScopeVersion == 0
	}
	return value.ScopeKind == ConversationSummaryProjectScope && project.ValidID(value.ScopeID) && value.ScopeVersion > 0
}

func sameConversationSummarySources(left, right []ConversationSummarySource) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Kind != right[index].Kind || left[index].ID != right[index].ID ||
			left[index].ObjectType != right[index].ObjectType ||
			left[index].Version != right[index].Version || left[index].Sequence != right[index].Sequence ||
			!bytes.Equal(left[index].SHA256, right[index].SHA256) {
			return false
		}
	}
	return true
}

func ConversationRouteHistoryWithoutSummarySources(history []ConversationRouteHistory, sources []ConversationSummarySource) []ConversationRouteHistory {
	messages := make(map[string]struct{}, len(sources))
	type taskResultKey struct {
		id       string
		version  int64
		sequence int64
	}
	results := make(map[taskResultKey]struct{}, len(sources))
	for _, source := range sources {
		switch source.Kind {
		case ConversationSummarySourceMessage:
			messages[source.ID] = struct{}{}
		case ConversationSummarySourceTask:
			results[taskResultKey{id: source.ID, version: source.Version, sequence: source.Sequence}] = struct{}{}
		}
	}
	uncovered := make([]ConversationRouteHistory, 0, len(history))
	for _, item := range history {
		if _, covered := messages[item.MessageID]; covered {
			continue
		}
		if item.Role == conversation.RoleAssistant && item.TaskID != "" {
			if _, covered := results[taskResultKey{id: item.TaskID, version: item.TaskRevision, sequence: item.EventSequence}]; covered {
				continue
			}
		}
		uncovered = append(uncovered, item)
	}
	return uncovered
}

func digestBytes(value []byte) []byte {
	digest := sha256.Sum256(value)
	return append([]byte(nil), digest[:]...)
}
