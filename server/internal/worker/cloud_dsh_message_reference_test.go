package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

type cloudDSHReferenceTestCheckpoints struct {
	*cloudDSHTestCheckpoints
	task task.Task
}

func (s *cloudDSHReferenceTestCheckpoints) PendingMessages(ctx context.Context, fence app.AgentCheckpointFence) ([]app.AgentConversationMessage, error) {
	messages, err := s.cloudDSHTestCheckpoints.PendingMessages(ctx, fence)
	if err != nil {
		return nil, err
	}
	for index := range messages {
		if len(messages[index].Content) <= app.MaxConversationAgentInputBytes {
			continue
		}
		reference, err := cloudDSHTestReference(messages[index], s.task, fence)
		if err != nil {
			return nil, err
		}
		messages[index].Reference = &reference
		messages[index].TaskRevision = reference.TaskRevision
		messages[index].SourceSequence = reference.MessageSequence
		messages[index].SourceContentBytes = reference.ContentBytes
		messages[index].SourceSHA256, _ = hex.DecodeString(reference.SourceSHA256)
		messages[index].ManifestVersion = reference.ManifestVersion
		messages[index].ManifestSHA256, _ = hex.DecodeString(reference.ManifestSHA256)
		messages[index].RepresentationVersion = 1
		messages[index].RepresentationKind = "reference"
		messages[index].RepresentationSHA256, _ = hex.DecodeString(reference.RepresentationSHA256)
	}
	return messages, nil
}

func cloudDSHTestReference(message app.AgentConversationMessage, value task.Task, fence app.AgentCheckpointFence) (app.AgentMessageReferenceEnvelope, error) {
	runes := []rune(message.Content)
	sourceDigest := sha256.Sum256([]byte(message.Content))
	pages := make([]app.AgentMessageSourcePage, 0, (len(runes)+app.MaxConversationMessageChunkRunes-1)/app.MaxConversationMessageChunkRunes)
	for offset := 0; offset < len(runes); {
		next := offset
		pageBytes := 0
		for next < len(runes) && next-offset < app.MaxConversationMessageChunkRunes {
			width := len(string(runes[next]))
			if pageBytes+width > app.MaxConversationMessageChunkBytes {
				break
			}
			pageBytes += width
			next++
		}
		if next == offset {
			return app.AgentMessageReferenceEnvelope{}, errors.New("source rune exceeds page byte limit")
		}
		pageDigest := sha256.Sum256([]byte(string(runes[offset:next])))
		pages = append(pages, app.AgentMessageSourcePage{RuneOffset: int64(offset), NextRuneOffset: int64(next), SHA256: hex.EncodeToString(pageDigest[:])})
		offset = next
	}
	sequence := int64(1)
	if value.CurrentRevision != nil && *value.CurrentRevision >= 0 {
		sequence = *value.CurrentRevision + 1
	}
	attachments := append([]string{}, message.AttachmentIDs...)
	manifestDigest, err := app.ComputeAgentMessageManifestSHA256(message.ID, sequence, int64(len(message.Content)), sourceDigest[:], int64(len(runes)), pages)
	if err != nil {
		return app.AgentMessageReferenceEnvelope{}, err
	}
	envelope := app.AgentMessageReferenceEnvelope{Version: app.AgentMessageReferenceEnvelopeVersion,
		TaskID: value.ID, TaskRevision: cloudDSHTaskRevision(value), OwnerID: value.OwnerID, ConversationID: value.ConversationID,
		MessageID: message.ID, MessageSequence: sequence, AuthorizationVersion: fence.AuthorizationVersion,
		AgentGeneration: fence.AgentGeneration, RootSessionID: fence.AgentSessionID, Mode: app.AgentMessageDeliveryFollowup,
		AttachmentIDs: attachments, ContentBytes: int64(len(message.Content)), SourceSHA256: hex.EncodeToString(sourceDigest[:]),
		ManifestVersion: app.AgentMessageManifestVersion, ManifestSHA256: hex.EncodeToString(manifestDigest),
		TotalRunes: int64(len(runes)), Pages: pages}
	if message.Mode != "" {
		envelope.Mode = message.Mode
	}
	representationDigest, err := app.ComputeAgentMessageReferenceRepresentationSHA256(envelope)
	if err != nil {
		return app.AgentMessageReferenceEnvelope{}, err
	}
	envelope.RepresentationSHA256 = hex.EncodeToString(representationDigest)
	if err := app.ValidateAgentMessageReferenceEnvelope(envelope); err != nil {
		return app.AgentMessageReferenceEnvelope{}, err
	}
	return envelope, nil
}

func TestCloudDSHInitialMessageAboveInlineLimitUsesSourceReference(t *testing.T) {
	for _, tc := range []struct {
		name      string
		messageID string
		bytes     int
	}{
		{name: "128KiB+1", messageID: "m128k003", bytes: app.MaxConversationAgentInputBytes + 1},
		{name: "1MiB", messageID: "m1m00001", bytes: 1 << 20},
		{name: "1MiB+1", messageID: "m1m00002", bytes: (1 << 20) + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const head, tail = "SOURCE-HEAD-", "-SOURCE-TAIL"
			content := head + strings.Repeat("x", tc.bytes-len(head)-len(tail)) + tail
			if len(content) != tc.bytes {
				t.Fatalf("source fixture bytes=%d, want %d", len(content), tc.bytes)
			}

			executor, _, _, checkpoints, models, value := cloudDSHFixture(t)
			value.Objective = app.ConversationRouteTaskMessageObjective(tc.messageID, content)
			checkpoints.pending = []app.AgentConversationMessage{{ID: tc.messageID, Content: content, Initial: true}}
			executor.options.Checkpoints = &cloudDSHReferenceTestCheckpoints{cloudDSHTestCheckpoints: checkpoints, task: value}

			if err := executor.ExecuteTask(context.Background(), value); !errors.Is(err, ErrCloudDSHCompletionPending) {
				t.Fatalf("oversized initial Task result = %v, want a completed model turn pending Task completion", err)
			}
			if len(models.calls) != 1 {
				t.Fatalf("oversized initial message produced %d model calls, want 1", len(models.calls))
			}
			request := string(models.calls[0].RawRequest)
			if len(request) >= tc.bytes || !strings.Contains(request, app.AgentMessageReferenceMarkerPrefix) {
				t.Fatalf("model request bytes=%d contains reference marker=%t, want a bounded reference", len(request), strings.Contains(request, app.AgentMessageReferenceMarkerPrefix))
			}
			if strings.Contains(request, content) || strings.Contains(request, head) {
				t.Fatal("model request contains the original oversized source instead of its bounded reference")
			}
			var decoded any
			if err := json.Unmarshal(models.calls[0].RawRequest, &decoded); err != nil {
				t.Fatal(err)
			}
			marker, ok := findCloudDSHReferenceMarker(decoded)
			if !ok {
				t.Fatal("model request did not contain a readable reference marker")
			}
			reference, err := app.ParseAgentMessageReferenceEnvelope(marker)
			if err != nil {
				t.Fatalf("parse model request source reference: %v", err)
			}
			if reference.MessageID != tc.messageID || reference.ContentBytes != int64(len(content)) || reference.SourceSHA256 != hex.EncodeToString(sha256Sum(content)) {
				t.Fatalf("model request source reference does not bind the original message: %+v", reference)
			}
		})
	}
}

func TestCloudDSHLongTaskObjectiveWithoutMessageReceiptFailsClosed(t *testing.T) {
	executor, _, _, _, models, value := cloudDSHFixture(t)
	value.Objective = strings.Repeat("x", app.MaxConversationAgentInputBytes+1)
	err := executor.ExecuteTask(context.Background(), value)
	if err == nil || !strings.Contains(err.Error(), "long initial Task message source reference is unavailable") {
		t.Fatalf("long Task objective without a message receipt error = %v, want fail-closed source lookup", err)
	}
	if len(models.calls) != 0 {
		t.Fatalf("unreceipted long Task objective reached the model %d times", len(models.calls))
	}
}

func TestCloudDSHLongPendingDeliveryUsesFencedReferenceAndDeduplicates(t *testing.T) {
	value := task.Task{ID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", ConversationID: "c1234567"}
	fence := app.AgentInstanceFence{TaskID: value.ID, OwnerID: value.OwnerID, InitiatorID: value.InitiatorID,
		AgentInstanceID: "agent_123", AgentGeneration: 2, AgentSessionID: "session_123", AuthorizationVersion: 3,
		LeaseOwner: "worker-123", LeaseVersion: 4, EngineVersion: "0.2.2-rc.1", ProfileVersion: "v1"}
	checkpointFence := app.AgentCheckpointFence{TaskID: fence.TaskID, OwnerID: fence.OwnerID, InitiatorID: fence.InitiatorID,
		AgentInstanceID: fence.AgentInstanceID, AgentGeneration: fence.AgentGeneration, AgentSessionID: fence.AgentSessionID,
		AgentRootSessionID: fence.AgentSessionID, AuthorizationVersion: fence.AuthorizationVersion}
	content := "long-followup-" + strings.Repeat("x", (1<<20)+1-len("long-followup-"))
	message := app.AgentConversationMessage{ID: "m128k004", Content: content, Mode: app.AgentMessageDeliveryFollowup}
	checkpoints := &cloudDSHReferenceTestCheckpoints{cloudDSHTestCheckpoints: &cloudDSHTestCheckpoints{pending: []app.AgentConversationMessage{message}}, task: value}
	messages, err := checkpoints.PendingMessages(context.Background(), checkpointFence)
	if err != nil {
		t.Fatal(err)
	}
	executor := &CloudDSHExecutor{}
	writer := &cloudDSHFrameBuffer{}
	seen := make(map[string]string)
	if err := executor.deliverPendingConversationMessages(context.Background(), fence, writer, messages, seen); err != nil {
		t.Fatal(err)
	}
	var first struct {
		Version        int      `json:"version"`
		MessageID      string   `json:"message_id"`
		Content        string   `json:"content"`
		Mode           string   `json:"mode"`
		TaskRevision   int64    `json:"task_revision"`
		ConversationID string   `json:"conversation_id"`
		AttachmentIDs  []string `json:"attachment_ids"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(writer.Bytes()), &first); err != nil {
		t.Fatal(err)
	}
	reference, err := app.ParseAgentMessageReferenceEnvelope(first.Content)
	if err != nil {
		t.Fatalf("parse live message reference: %v", err)
	}
	if first.Version != 3 || first.MessageID != message.ID || first.Mode != string(message.Mode) || first.TaskRevision != 0 ||
		first.ConversationID != value.ConversationID || first.AttachmentIDs == nil || reference.MessageID != message.ID ||
		reference.AgentGeneration != fence.AgentGeneration || reference.RootSessionID != fence.AgentSessionID {
		t.Fatalf("live delivery lost its source or Task fence: frame=%+v ref=%+v", first, reference)
	}
	if err := executor.deliverPendingConversationMessages(context.Background(), fence, writer, messages, seen); err != nil || strings.Count(writer.String(), "message_delivery") != 1 {
		t.Fatalf("identical reference delivery replay was not deduplicated: err=%v frames=%q", err, writer.String())
	}
}

func TestCloudDSHLongSupersedingDeliveryRetainsReferenceFence(t *testing.T) {
	value := task.Task{ID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", ConversationID: "c1234567"}
	fence := app.AgentInstanceFence{TaskID: value.ID, OwnerID: value.OwnerID, InitiatorID: value.InitiatorID,
		AgentInstanceID: "agent_123", AgentGeneration: 2, AgentSessionID: "session_123", AuthorizationVersion: 3,
		LeaseOwner: "worker-123", LeaseVersion: 4, EngineVersion: "0.2.2-rc.1", ProfileVersion: "v1"}
	checkpointFence := app.AgentCheckpointFence{TaskID: fence.TaskID, OwnerID: fence.OwnerID, InitiatorID: fence.InitiatorID,
		AgentInstanceID: fence.AgentInstanceID, AgentGeneration: fence.AgentGeneration, AgentSessionID: fence.AgentSessionID,
		AgentRootSessionID: fence.AgentSessionID, AuthorizationVersion: fence.AuthorizationVersion}
	content := "long-steer-" + strings.Repeat("x", (1<<20)+1-len("long-steer-"))
	message := app.AgentConversationMessage{ID: "m128k005", Content: content,
		Mode: app.AgentMessageDeliverySteer}
	checkpoints := &cloudDSHReferenceTestCheckpoints{cloudDSHTestCheckpoints: &cloudDSHTestCheckpoints{pending: []app.AgentConversationMessage{message}}, task: value}
	messages, err := checkpoints.PendingMessages(context.Background(), checkpointFence)
	if err != nil {
		t.Fatal(err)
	}
	executor := &CloudDSHExecutor{}
	writer := &cloudDSHFrameBuffer{}
	seen := make(map[string]string)
	if err := executor.deliverPendingConversationMessages(context.Background(), fence, writer, messages, seen); err != nil {
		t.Fatal(err)
	}
	if err := executor.deliverSupersedingMessages(context.Background(), fence, writer, messages, seen, fence.AgentSessionID, "call_supersede_1"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(writer.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("delivery frame count=%d, want v3 then v4", len(lines))
	}
	var first, second struct {
		Version        int    `json:"version"`
		Content        string `json:"content"`
		TaskRevision   int64  `json:"task_revision"`
		ConversationID string `json:"conversation_id"`
		Mode           string `json:"mode"`
		Supersede      bool   `json:"supersede_active"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if _, err := app.ParseAgentMessageReferenceEnvelope(first.Content); err != nil {
		t.Fatalf("parse initial steer reference: %v", err)
	}
	if _, err := app.ParseAgentMessageReferenceEnvelope(second.Content); err != nil {
		t.Fatalf("parse superseding steer reference: %v", err)
	}
	if first.Version != 3 || second.Version != 4 || second.Content != first.Content || second.TaskRevision != first.TaskRevision ||
		second.ConversationID != first.ConversationID || second.Mode != first.Mode || !second.Supersede {
		t.Fatalf("superseding frame changed message identity or fence: first=%+v second=%+v", first, second)
	}
}

func TestCloudDSHRevisionBatchOverflowDeliversContiguousPrefixThenRemainder(t *testing.T) {
	executor, _, _, checkpoints, models, value := cloudDSHFixture(t)
	firstID, secondID := "m128k006", "m128k007"
	firstContent := "FIRST-REVISION-" + strings.Repeat("a", app.MaxConversationAgentInputBytes/2)
	secondContent := "SECOND-REVISION-" + strings.Repeat("b", app.MaxConversationAgentInputBytes/2)
	checkpoints.pending = []app.AgentConversationMessage{
		{ID: firstID, Content: firstContent, Mode: app.AgentMessageDeliveryFollowup, RevisionUpdate: true},
		{ID: secondID, Content: secondContent, Mode: app.AgentMessageDeliveryFollowup, RevisionUpdate: true},
	}
	if err := executor.ExecuteTask(context.Background(), value); !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("revision batch Task result = %v, want a completed model turn pending Task completion", err)
	}
	if len(models.calls) == 0 {
		t.Fatal("revision batch produced no model calls")
	}
	firstRequest := string(models.calls[0].RawRequest)
	lastRequest := string(models.calls[len(models.calls)-1].RawRequest)
	firstPosition, secondPosition := strings.Index(lastRequest, firstID), strings.Index(lastRequest, secondID)
	if firstPosition < 0 || secondPosition < 0 || firstPosition >= secondPosition ||
		strings.Count(firstRequest, firstID) != 1 || strings.Contains(firstRequest, secondID) ||
		strings.Count(lastRequest, firstID) != 1 || strings.Count(lastRequest, secondID) != 1 {
		t.Fatalf("overflowing revision batch lost, reordered, or duplicated a message: first_request_counts=(%d,%d) last_request_positions=(%d,%d) last_request_counts=(%d,%d) calls=%d",
			strings.Count(firstRequest, firstID), strings.Count(firstRequest, secondID), firstPosition, secondPosition,
			strings.Count(lastRequest, firstID), strings.Count(lastRequest, secondID), len(models.calls))
	}
}

func findCloudDSHReferenceMarker(value any) (string, bool) {
	switch value := value.(type) {
	case string:
		return value, strings.HasPrefix(value, app.AgentMessageReferenceMarkerPrefix+" ")
	case []any:
		for _, item := range value {
			if marker, ok := findCloudDSHReferenceMarker(item); ok {
				return marker, true
			}
		}
	case map[string]any:
		for _, item := range value {
			if marker, ok := findCloudDSHReferenceMarker(item); ok {
				return marker, true
			}
		}
	}
	return "", false
}

func sha256Sum(content string) []byte {
	digest := sha256.Sum256([]byte(content))
	return digest[:]
}
