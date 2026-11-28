package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

func TestCloudDSHCurrentHistorySearchReturnsStableSourceProofsForUserMatches(t *testing.T) {
	broker, baseStore, claimed, _, _ := cloudDSHGoalScheduleTaskFixture(t)
	claimed.ConversationContext.ThroughSequence = 5
	baseStore.details.Task.ConversationContext = claimed.ConversationContext
	configureCloudDSHCurrentHistory(t, broker, baseStore, &claimed)
	userID, err := domain.NewMessageID()
	if err != nil {
		t.Fatal(err)
	}
	assistantID, err := domain.NewMessageID()
	if err != nil {
		t.Fatal(err)
	}
	oldUserID, err := domain.NewMessageID()
	if err != nil {
		t.Fatal(err)
	}
	const userContent = "The user explicitly requires the focused verification tests."
	historyStore := &cloudDSHHistoryTaskStore{cloudDSHBrokerTaskStore: baseStore,
		messages: map[string]MessagePage{claimed.ConversationID: {Items: []conversation.Message{
			{ID: userID, ConversationID: claimed.ConversationID, OwnerID: claimed.OwnerID, Role: conversation.RoleUser,
				Sequence: 4, Content: userContent, DeliveryStatus: conversation.DeliveryApplied},
			{ID: assistantID, ConversationID: claimed.ConversationID, OwnerID: claimed.OwnerID, Role: conversation.RoleAssistant,
				Sequence: 3, Content: "I will check the requested tests.", DeliveryStatus: conversation.DeliveryApplied},
			{ID: oldUserID, ConversationID: claimed.ConversationID, OwnerID: claimed.OwnerID, Role: conversation.RoleUser,
				Sequence: 2, Content: "The earlier discussion mentions verification coverage.", DeliveryStatus: conversation.DeliveryApplied},
		}}}, suppressed: map[string]struct{}{}}
	broker.store = historyStore
	arguments, err := json.Marshal(map[string]any{"query": "verification tests", "limit": 5, "cursor": nil, "message_id": nil})
	if err != nil {
		t.Fatal(err)
	}
	call := ModelToolCall{CallID: "call_history_source_proof", Name: cloudDSHCurrentHistorySearchToolName, Arguments: string(arguments)}
	result, err := broker.execute(context.Background(), claimed, call, false, nil)
	if err != nil || result.Status != AgentToolSucceeded {
		t.Fatalf("authorized history search result=%+v err=%v", result, err)
	}
	var output cloudDSHCurrentHistorySearchResult
	if err := json.Unmarshal([]byte(result.Output), &output); err != nil {
		t.Fatal(err)
	}
	var userProof, assistantMatch *cloudDSHCurrentHistoryMatch
	for index := range output.Matches {
		match := &output.Matches[index]
		switch match.Role {
		case conversation.RoleUser:
			if match.MessageID == userID {
				userProof = match
			}
		case conversation.RoleAssistant:
			assistantMatch = match
		}
	}
	if userProof == nil || userProof.HistoryResultToken == "" {
		t.Fatalf("user match did not receive signed source token: %+v", output.Matches)
	}
	if assistantMatch == nil || assistantMatch.HistoryResultToken != "" {
		t.Fatalf("assistant match received a user requirement source token: %+v", assistantMatch)
	}
	digest := sha256.Sum256([]byte(userContent))
	resolved, err := broker.resolveCloudDSHTaskPlanHistorySource(context.Background(), claimed, userProof.HistoryResultToken)
	if err != nil || resolved.MessageID != userID || resolved.ContentSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("resolved user history source=%+v err=%v", resolved, err)
	}
	checkpointOutput := result.Output
	replayed, err := broker.execute(context.Background(), claimed, call, true, &checkpointOutput)
	if err != nil || replayed.Output != result.Output {
		t.Fatalf("history result replay=%+v err=%v", replayed, err)
	}
}

func TestCloudDSHTaskPlanHistorySourceProofBindsAuthorizedSearchResult(t *testing.T) {
	broker := &CloudDSHToolBroker{}
	if err := broker.SetHistorySearchCursorSigning("11111111-1111-1111-1111-111111111111", bytes.Repeat([]byte("k"), sha256.Size)); err != nil {
		t.Fatal(err)
	}
	current := cloudDSHHistorySourceProofTestTask()
	const content = "Run the focused tests and report failures."
	digest := sha256.Sum256([]byte(content))
	match := cloudDSHCurrentHistoryMatch{MessageID: "m1234567", Role: conversation.RoleUser,
		Content: "Run the focused tests", Sequence: 12, ContentSHA256: hex.EncodeToString(digest[:])}
	token, err := broker.signCloudDSHTaskPlanHistorySource(current, match)
	if err != nil || token == "" {
		t.Fatalf("sign history source token=%q err=%v", token, err)
	}
	if strings.Contains(token, content) || strings.Contains(token, match.MessageID) {
		t.Fatalf("source token exposes source text or message ID: %q", token)
	}
	resolved, err := broker.resolveCloudDSHTaskPlanHistorySource(context.Background(), current, token)
	if err != nil || resolved.Kind != task.PlanVerificationRequirementUserMessage ||
		resolved.MessageID != match.MessageID || resolved.ContentSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("resolved historical source=%#v err=%v", resolved, err)
	}
}

func TestCloudDSHTaskPlanHistorySourceProofRejectsTamperingAndScopeChanges(t *testing.T) {
	broker := &CloudDSHToolBroker{}
	if err := broker.SetHistorySearchCursorSigning("11111111-1111-1111-1111-111111111111", bytes.Repeat([]byte("k"), sha256.Size)); err != nil {
		t.Fatal(err)
	}
	current := cloudDSHHistorySourceProofTestTask()
	digest := sha256.Sum256([]byte("original message"))
	token, err := broker.signCloudDSHTaskPlanHistorySource(current, cloudDSHCurrentHistoryMatch{
		MessageID: "m1234567", Role: conversation.RoleUser, Sequence: 12, ContentSHA256: hex.EncodeToString(digest[:]),
	})
	if err != nil || token == "" {
		t.Fatalf("sign history source token=%q err=%v", token, err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[1] == "" {
		t.Fatalf("history source token has invalid envelope: %q", token)
	}
	tampered := "A" + token[1:]
	if _, err := broker.resolveCloudDSHTaskPlanHistorySource(context.Background(), current, tampered); err == nil {
		t.Fatal("tampered history source token was accepted")
	}

	changedRevision := cloudDSHHistorySourceProofTestTask()
	revision := *changedRevision.CurrentRevision + 1
	changedRevision.CurrentRevision = &revision
	changedAuth := cloudDSHHistorySourceProofTestTask()
	changedAuth.Authorization.Version++
	changedWatermark := cloudDSHHistorySourceProofTestTask()
	changedWatermark.ConversationContext.ThroughSequence++
	changedConversation := cloudDSHHistorySourceProofTestTask()
	changedConversation.ConversationID = "c7654321"
	for name, value := range map[string]task.Task{
		"revision": changedRevision, "authorization": changedAuth,
		"watermark": changedWatermark, "conversation": changedConversation,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := broker.resolveCloudDSHTaskPlanHistorySource(context.Background(), value, token); err == nil {
				t.Fatal("history source token crossed a Task identity or revision boundary")
			}
		})
	}
}

func TestCloudDSHTaskPlanHistorySourceProofRequiresUserMessageWithinWatermark(t *testing.T) {
	broker := &CloudDSHToolBroker{}
	if err := broker.SetHistorySearchCursorSigning("11111111-1111-1111-1111-111111111111", bytes.Repeat([]byte("k"), sha256.Size)); err != nil {
		t.Fatal(err)
	}
	current := cloudDSHHistorySourceProofTestTask()
	digest := sha256.Sum256([]byte("message"))
	base := cloudDSHCurrentHistoryMatch{MessageID: "m1234567", Role: conversation.RoleUser,
		Sequence: 12, ContentSHA256: hex.EncodeToString(digest[:])}
	for name, match := range map[string]cloudDSHCurrentHistoryMatch{
		"assistant message": func() cloudDSHCurrentHistoryMatch { v := base; v.Role = conversation.RoleAssistant; return v }(),
		"outside watermark": func() cloudDSHCurrentHistoryMatch { v := base; v.Sequence = 16; return v }(),
		"missing digest":    func() cloudDSHCurrentHistoryMatch { v := base; v.ContentSHA256 = ""; return v }(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := broker.signCloudDSHTaskPlanHistorySource(current, match); err == nil {
				t.Fatal("ineligible history result received a source proof")
			}
		})
	}
}

func TestCloudDSHTaskPlanHistorySourceProofIsUnavailableWithoutSigningKey(t *testing.T) {
	broker := &CloudDSHToolBroker{}
	current := cloudDSHHistorySourceProofTestTask()
	digest := sha256.Sum256([]byte("message"))
	token, err := broker.signCloudDSHTaskPlanHistorySource(current, cloudDSHCurrentHistoryMatch{
		MessageID: "m1234567", Role: conversation.RoleUser, Sequence: 12, ContentSHA256: hex.EncodeToString(digest[:]),
	})
	if err != nil || token != "" {
		t.Fatalf("unconfigured signer returned token=%q err=%v", token, err)
	}
}

func cloudDSHHistorySourceProofTestTask() task.Task {
	revision := int64(4)
	return task.Task{ID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", ConversationID: "c1234567",
		ExecutionLocation: "server", Status: task.StatusRunning, AgentSessionID: "session-1", CurrentRevision: &revision,
		Authorization: task.Authorization{Version: 3},
		ConversationContext: &task.ConversationContext{OwnerID: "u1234567", InitiatorID: "u1234567", ConversationID: "c1234567",
			Scope: "personal", ThroughSequence: 15, Digest: bytes.Repeat([]byte{0x7a}, sha256.Size)}}
}
