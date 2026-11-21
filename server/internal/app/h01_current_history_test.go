package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
)

func TestH01CurrentHistoryReadsAroundOldMessageWithBoundCursor(t *testing.T) {
	broker, baseStore, claimed, _, _ := cloudDSHGoalScheduleTaskFixture(t)
	claimed.ConversationContext.ThroughSequence = 900
	authorizer := configureCloudDSHCurrentHistory(t, broker, baseStore, &claimed)
	items := make([]conversation.Message, 0, 900)
	anchorID := ""
	for sequence := int64(900); sequence > 0; sequence-- {
		id, err := domain.NewMessageID()
		if err != nil {
			t.Fatal(err)
		}
		if sequence == 100 {
			anchorID = id
		}
		items = append(items, conversation.Message{ID: id, ConversationID: claimed.ConversationID, OwnerID: claimed.OwnerID,
			ActorID: claimed.InitiatorID, Role: conversation.RoleUser, Sequence: sequence, Content: "message in cedar history",
			DeliveryStatus: conversation.DeliveryApplied, CreatedAt: time.Unix(sequence, 0).UTC()})
	}
	historyStore := &cloudDSHHistoryTaskStore{
		cloudDSHBrokerTaskStore: baseStore,
		messages:                map[string]MessagePage{claimed.ConversationID: {Items: items}},
		suppressed:              map[string]struct{}{},
	}
	broker.store = &cloudDSHGoalTaskStore{cloudDSHBrokerTaskStore: baseStore}
	// The current-history fixture wraps TaskStore while replacing only its message reader.
	store := &h01CurrentHistoryTaskStore{TaskStore: broker.store, history: historyStore}
	broker.store = store
	first, err := broker.execute(context.Background(), claimed, ModelToolCall{CallID: "h01_anchor_first",
		Name: cloudDSHCurrentHistorySearchToolName, Arguments: h01CurrentHistoryArguments(t, anchorID, "")}, false, nil)
	if err != nil || first.Status != AgentToolSucceeded {
		t.Fatalf("first anchor scan=%+v err=%v", first, err)
	}
	var scan cloudDSHCurrentHistorySearchResult
	if err := json.Unmarshal([]byte(first.Output), &scan); err != nil {
		t.Fatal(err)
	}
	if len(scan.Matches) != 0 || scan.ScannedMessages != cloudDSHCurrentHistoryMaxMessages || !scan.Partial ||
		scan.NextCursor == "" || scan.TruncationReason != "scan_limit" {
		t.Fatalf("old anchor scan was not bounded and resumable: %+v", scan)
	}
	second, err := broker.execute(context.Background(), claimed, ModelToolCall{CallID: "h01_anchor_read",
		Name: cloudDSHCurrentHistorySearchToolName, Arguments: h01CurrentHistoryArguments(t, anchorID, scan.NextCursor)}, false, nil)
	if err != nil || second.Status != AgentToolSucceeded {
		t.Fatalf("anchor read=%+v err=%v", second, err)
	}
	var page cloudDSHCurrentHistorySearchResult
	if err := json.Unmarshal([]byte(second.Output), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Matches) != cloudDSHHistorySearchMaxResults || !page.Partial || page.NextCursor == "" ||
		page.TruncationReason != "match_limit" || !page.Matches[0].CreatedAt.Equal(time.Unix(100, 0).UTC()) ||
		!page.Matches[1].CreatedAt.Equal(time.Unix(101, 0).UTC()) {
		t.Fatalf("anchor neighborhood was not returned nearest-first with continuation: %+v", page)
	}
	if authorizer.calls < 2 || historyStore.pageCalls < 9 {
		t.Fatalf("anchor scans did not recheck authorization or reach old history: checks=%d pages=%d", authorizer.calls, historyStore.pageCalls)
	}
	if _, err := broker.execute(context.Background(), claimed, ModelToolCall{CallID: "h01_anchor_cursor_scope",
		Name: cloudDSHCurrentHistorySearchToolName, Arguments: h01CurrentHistoryArguments(t, items[0].ID, page.NextCursor)}, false, nil); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("anchor cursor was accepted with a different message ID: %v", err)
	}
}

func TestH01CurrentHistoryUnavailableAnchorDoesNotInventMessages(t *testing.T) {
	broker, baseStore, claimed, _, _ := cloudDSHGoalScheduleTaskFixture(t)
	claimed.ConversationContext.ThroughSequence = 3
	configureCloudDSHCurrentHistory(t, broker, baseStore, &claimed)
	items := make([]conversation.Message, 0, 3)
	for sequence := int64(3); sequence > 0; sequence-- {
		id, err := domain.NewMessageID()
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, conversation.Message{ID: id, ConversationID: claimed.ConversationID, OwnerID: claimed.OwnerID,
			ActorID: claimed.InitiatorID, Role: conversation.RoleUser, Sequence: sequence, Content: "available history",
			DeliveryStatus: conversation.DeliveryApplied})
	}
	historyStore := &cloudDSHHistoryTaskStore{cloudDSHBrokerTaskStore: baseStore,
		messages: map[string]MessagePage{claimed.ConversationID: {Items: items}}, suppressed: map[string]struct{}{}}
	broker.store = &h01CurrentHistoryTaskStore{TaskStore: broker.store, history: historyStore}
	missingID, err := domain.NewMessageID()
	if err != nil {
		t.Fatal(err)
	}
	result, err := broker.execute(context.Background(), claimed, ModelToolCall{CallID: "h01_anchor_missing",
		Name: cloudDSHCurrentHistorySearchToolName, Arguments: h01CurrentHistoryArguments(t, missingID, "")}, false, nil)
	if err != nil || result.Status != AgentToolSucceeded {
		t.Fatalf("unavailable anchor search=%+v err=%v", result, err)
	}
	var output cloudDSHCurrentHistorySearchResult
	if err := json.Unmarshal([]byte(result.Output), &output); err != nil {
		t.Fatal(err)
	}
	if len(output.Matches) != 0 || !output.Partial || output.NextCursor != "" || output.TruncationReason != "anchor_unavailable" {
		t.Fatalf("unavailable anchor returned fabricated or unmarked history: %+v", output)
	}
}

type h01CurrentHistoryTaskStore struct {
	TaskStore
	history *cloudDSHHistoryTaskStore
}

func (s *h01CurrentHistoryTaskStore) ListMessages(ctx context.Context, ownerID, conversationID string, page PageRequest) (MessagePage, error) {
	return s.history.ListMessages(ctx, ownerID, conversationID, page)
}

func (s *h01CurrentHistoryTaskStore) ListSuppressedPersonalHistoryMessageIDs(ctx context.Context, ownerID string,
	messageIDs []string) (map[string]struct{}, error) {
	return s.history.ListSuppressedPersonalHistoryMessageIDs(ctx, ownerID, messageIDs)
}

func h01CurrentHistoryArguments(t *testing.T, messageID, cursor string) string {
	t.Helper()
	value, err := json.Marshal(map[string]any{"query": "", "message_id": messageID, "limit": cloudDSHHistorySearchMaxResults,
		"cursor": cursor})
	if err != nil {
		t.Fatal(err)
	}
	return string(value)
}
