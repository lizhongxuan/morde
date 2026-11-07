package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/personal"
)

type cloudDSHHistoryTaskStore struct {
	*cloudDSHBrokerTaskStore
	conversations ConversationPage
	messages      map[string]MessagePage
	listOwner     string
	messageOwners []string
	messageIDs    []string
}

func (s *cloudDSHHistoryTaskStore) ListConversations(ctx context.Context, ownerID string, _ []string, page PageRequest) (ConversationPage, error) {
	if err := ctx.Err(); err != nil {
		return ConversationPage{}, err
	}
	s.listOwner = ownerID
	if page.Limit != cloudDSHHistoryConversationLimit || page.Cursor != "" {
		return ConversationPage{}, fmt.Errorf("unexpected history conversation page: %+v", page)
	}
	return s.conversations, nil
}

func (s *cloudDSHHistoryTaskStore) ListMessages(ctx context.Context, ownerID, conversationID string, page PageRequest) (MessagePage, error) {
	if err := ctx.Err(); err != nil {
		return MessagePage{}, err
	}
	s.messageOwners = append(s.messageOwners, ownerID)
	s.messageIDs = append(s.messageIDs, conversationID)
	if (page.Limit != cloudDSHHistoryMessageLimit && page.Limit != cloudDSHHistorySummaryMessageLimit) || page.Cursor != "" {
		return MessagePage{}, fmt.Errorf("unexpected history message page: %+v", page)
	}
	return s.messages[conversationID], nil
}

func TestCloudDSHConversationHistorySearchIsConsentGatedAndOwnerScoped(t *testing.T) {
	broker, baseStore, claimed, _, _ := cloudDSHGoalScheduleTaskFixture(t)
	settingStore := &personalMutationStoreStub{objects: map[string]personal.Object{}}
	personalData, err := NewPersonalMutationService(settingStore)
	if err != nil {
		t.Fatal(err)
	}
	broker.personalData = personalData
	definitions, err := broker.Definitions(context.Background(), claimed)
	if err != nil {
		t.Fatal(err)
	}
	if containsAgentTool(definitions, cloudDSHConversationHistorySearchToolName) || containsAgentTool(definitions, cloudDSHRecentHistorySummaryToolName) {
		t.Fatal("history tools were exposed without explicit memory consent")
	}
	call := ModelToolCall{CallID: "call_history_disabled", Name: cloudDSHConversationHistorySearchToolName, Arguments: `{"query":"茶叶"}`}
	if _, err := broker.execute(context.Background(), claimed, call, false, nil); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("history search without consent error = %v, want forbidden", err)
	}
	if len(baseStore.events) != 0 {
		t.Fatalf("denied search wrote task progress: %+v", baseStore.events)
	}

	settingStore.objects[personal.ObjectTypeSkillSetting+"/"+cloudDSHMemorySettingID] = enabledMemorySetting()
	previousID, err := domain.NewConversationID()
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := domain.NewConversationID()
	if err != nil {
		t.Fatal(err)
	}
	messageID := func() string {
		t.Helper()
		id, err := domain.NewMessageID()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	previous := conversation.Conversation{ID: previousID, OwnerID: claimed.OwnerID, Title: "上次聊茶叶"}
	projectConversation := conversation.Conversation{ID: projectID, OwnerID: claimed.OwnerID, ProjectID: "j1234567", Title: "项目中的茶叶"}
	baseStore.details.Task.Usage.ToolCalls = 0
	baseStore.details.Task.Budget.ToolCalls = 12
	baseStore.details.Task.Authorization.Version = claimed.Authorization.Version
	store := &cloudDSHHistoryTaskStore{
		cloudDSHBrokerTaskStore: baseStore,
		conversations: ConversationPage{Items: []conversation.Conversation{
			previous,
			{ID: claimed.ConversationID, OwnerID: claimed.OwnerID, Title: "当前对话"},
			projectConversation,
		}},
		messages: map[string]MessagePage{
			previousID: {Items: []conversation.Message{
				{ID: messageID(), ConversationID: previousID, OwnerID: claimed.OwnerID, Role: conversation.RoleUser,
					Content: "上次我们聊了茶叶的冲泡方法，先用 85 度水温。", DeliveryStatus: conversation.DeliveryApplied, CreatedAt: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)},
				{ID: messageID(), ConversationID: previousID, OwnerID: claimed.OwnerID, Role: conversation.RoleAssistant,
					Content: "更早的记录提到茶叶的价格。", DeliveryStatus: conversation.DeliveryApplied, CreatedAt: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)},
				{ID: messageID(), ConversationID: previousID, OwnerID: claimed.OwnerID, Role: conversation.RoleAssistant,
					Content: "茶叶需要用 85 度水冲泡。", DeliveryStatus: conversation.DeliveryPending},
				{ID: messageID(), ConversationID: previousID, OwnerID: claimed.OwnerID, Role: conversation.RoleAssistant,
					Content: "茶叶的第二条建议。", DeliveryStatus: conversation.DeliveryUnknown},
				{ID: messageID(), ConversationID: previousID, OwnerID: claimed.OwnerID, Role: conversation.RoleUser,
					Content: "茶叶图片里的内容", AttachmentIDs: []string{"pa_0123456789abcdef0123456789abcdef"}, DeliveryStatus: conversation.DeliveryApplied},
			}},
			claimed.ConversationID: {Items: []conversation.Message{{
				ID: messageID(), ConversationID: claimed.ConversationID, OwnerID: claimed.OwnerID,
				Role: conversation.RoleUser, Content: "茶叶不应从当前对话重复检索。", DeliveryStatus: conversation.DeliveryApplied,
			}}},
			projectID: {Items: []conversation.Message{{
				ID: messageID(), ConversationID: projectID, OwnerID: claimed.OwnerID,
				Role: conversation.RoleUser, Content: "茶叶不应读取项目会话。", DeliveryStatus: conversation.DeliveryApplied,
			}}},
		},
	}
	broker.store = store
	definitions, err = broker.Definitions(context.Background(), claimed)
	if err != nil {
		t.Fatal(err)
	}
	if !containsAgentTool(definitions, cloudDSHConversationHistorySearchToolName) {
		t.Fatal("history search was not exposed after explicit memory consent")
	}
	if !containsAgentTool(definitions, cloudDSHRecentHistorySummaryToolName) {
		t.Fatal("recent history summary was not exposed after explicit memory consent")
	}
	call = ModelToolCall{CallID: "call_history_enabled", Name: cloudDSHConversationHistorySearchToolName,
		Arguments: `{"query":"茶叶","limit":1}`}
	result, err := broker.execute(context.Background(), claimed, call, false, nil)
	if err != nil || result.Status != AgentToolSucceeded {
		t.Fatalf("history search result=%+v err=%v", result, err)
	}
	var output cloudDSHHistorySearchResult
	if err := json.Unmarshal([]byte(result.Output), &output); err != nil {
		t.Fatal(err)
	}
	if len(output.Matches) != 1 || output.Matches[0].Content != "上次我们聊了茶叶的冲泡方法，先用 85 度水温。" ||
		output.Matches[0].ConversationTitle != "上次聊茶叶" || output.ScannedConversations != 1 || !output.Partial {
		t.Fatalf("history search leaked excluded data or returned wrong match: %+v", output)
	}
	if store.listOwner != claimed.OwnerID || len(store.messageIDs) != 1 || store.messageIDs[0] != previousID ||
		len(store.messageOwners) != 1 || store.messageOwners[0] != claimed.OwnerID {
		t.Fatalf("history search scope is wrong: owner=%s conversations=%v message owners=%v", store.listOwner, store.messageIDs, store.messageOwners)
	}
	if baseStore.details.Task.Usage.ToolCalls != 1 || len(baseStore.events) != 1 || baseStore.events[0].Type != "task.step" {
		t.Fatalf("successful history search was not checkpointed and charged exactly once: usage=%+v events=%+v", baseStore.details.Task.Usage, baseStore.events)
	}

	summaryCall := ModelToolCall{CallID: "call_history_summary", Name: cloudDSHRecentHistorySummaryToolName, Arguments: `{}`}
	summary, err := broker.execute(context.Background(), claimed, summaryCall, false, nil)
	if err != nil || summary.Status != AgentToolSucceeded {
		t.Fatalf("recent history summary=%+v err=%v", summary, err)
	}
	var summaryOutput cloudDSHHistorySummaryResult
	if err := json.Unmarshal([]byte(summary.Output), &summaryOutput); err != nil {
		t.Fatal(err)
	}
	if len(summaryOutput.Conversations) != 1 || summaryOutput.Conversations[0].Title != "上次聊茶叶" ||
		len(summaryOutput.Conversations[0].Messages) != 2 || summaryOutput.Scanned != 1 || summaryOutput.Partial {
		t.Fatalf("summary did not return only complete, safe personal history excerpts: %+v", summaryOutput)
	}
	if baseStore.details.Task.Usage.ToolCalls != 2 || len(baseStore.events) != 2 {
		t.Fatalf("summary was not checkpointed and charged exactly once: usage=%+v events=%+v", baseStore.details.Task.Usage, baseStore.events)
	}
	recoveredSummary, err := broker.execute(context.Background(), claimed, summaryCall, true, &summary.Output)
	if err != nil || recoveredSummary.Output != summary.Output || baseStore.details.Task.Usage.ToolCalls != 2 || len(baseStore.events) != 2 {
		t.Fatalf("checkpointed summary recovery=%+v err=%v usage=%+v events=%d", recoveredSummary, err, baseStore.details.Task.Usage, len(baseStore.events))
	}
	recovered, err := broker.execute(context.Background(), claimed, call, true, &result.Output)
	if err != nil || recovered.Output != result.Output || baseStore.details.Task.Usage.ToolCalls != 2 || len(baseStore.events) != 2 {
		t.Fatalf("checkpointed history search recovery=%+v err=%v usage=%+v events=%d", recovered, err, baseStore.details.Task.Usage, len(baseStore.events))
	}
	changedCheckpoint := "{}"
	if _, err := broker.execute(context.Background(), claimed, call, true, &changedCheckpoint); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("changed history result was accepted during recovery: %v", err)
	}
}

func TestCloudDSHRecentHistorySummaryRejectsForeignAndMalformedRows(t *testing.T) {
	broker, baseStore, claimed, _, _ := cloudDSHGoalScheduleTaskFixture(t)
	previousID, err := domain.NewConversationID()
	if err != nil {
		t.Fatal(err)
	}
	store := &cloudDSHHistoryTaskStore{
		cloudDSHBrokerTaskStore: baseStore,
		conversations:           ConversationPage{Items: []conversation.Conversation{{ID: previousID, OwnerID: "u7654321", Title: "other account"}}},
	}
	broker.store = store
	principal, ok := cloudDSHPersonalPrincipal(claimed)
	if !ok {
		t.Fatal("fixture did not produce a personal principal")
	}
	if _, err := broker.summarizablePersonalHistory(context.Background(), principal, claimed.ConversationID); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("foreign conversation row error = %v, want internal fail-closed", err)
	}
	store.conversations = ConversationPage{Items: []conversation.Conversation{{ID: previousID, OwnerID: claimed.OwnerID, Title: "malformed"}}}
	store.messages = map[string]MessagePage{previousID: {Items: []conversation.Message{{
		ID: "invalid", ConversationID: previousID, OwnerID: claimed.OwnerID, Role: conversation.RoleUser,
		Content: "history", DeliveryStatus: conversation.DeliveryApplied,
	}}}}
	if _, err := broker.summarizablePersonalHistory(context.Background(), principal, claimed.ConversationID); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("malformed message row error = %v, want internal fail-closed", err)
	}
}

func TestCloudDSHConversationHistoryRejectsForeignAndMalformedRows(t *testing.T) {
	broker, baseStore, claimed, _, _ := cloudDSHGoalScheduleTaskFixture(t)
	previousID, err := domain.NewConversationID()
	if err != nil {
		t.Fatal(err)
	}
	store := &cloudDSHHistoryTaskStore{
		cloudDSHBrokerTaskStore: baseStore,
		conversations:           ConversationPage{Items: []conversation.Conversation{{ID: previousID, OwnerID: "u7654321", Title: "other account"}}},
	}
	broker.store = store
	principal, ok := cloudDSHPersonalPrincipal(claimed)
	if !ok {
		t.Fatal("fixture did not produce a personal principal")
	}
	if _, err := broker.searchPersonalConversationHistory(context.Background(), principal, claimed.ConversationID, "茶叶", 5); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("foreign conversation row error = %v, want internal fail-closed", err)
	}
	store.conversations = ConversationPage{Items: []conversation.Conversation{{ID: previousID, OwnerID: claimed.OwnerID, Title: "malformed"}}}
	store.messages = map[string]MessagePage{previousID: {Items: []conversation.Message{{
		ID: "invalid", ConversationID: previousID, OwnerID: claimed.OwnerID, Role: conversation.RoleUser,
		Content: "茶叶", DeliveryStatus: conversation.DeliveryApplied,
	}}}}
	if _, err := broker.searchPersonalConversationHistory(context.Background(), principal, claimed.ConversationID, "茶叶", 5); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("malformed message row error = %v, want internal fail-closed", err)
	}
}

func TestCloudDSHHistorySearchChineseAndEnglishTokenization(t *testing.T) {
	if score := cloudDSHHistoryMatchScore("之前聊过茶叶价格", "我们上次讨论茶叶价格和产地"); score < 2 {
		t.Fatalf("Chinese history query score = %d, want at least 2", score)
	}
	if score := cloudDSHHistoryMatchScore("release planning", "The release planning notes are ready."); score < 2 {
		t.Fatalf("English history query score = %d, want at least 2", score)
	}
	if terms := cloudDSHHistoryQueryTerms(strings.Repeat("茶叶", 100)); len(terms) > cloudDSHHistoryMaxQueryTerms {
		t.Fatalf("history query generated %d terms, limit is %d", len(terms), cloudDSHHistoryMaxQueryTerms)
	}
}

func TestCloudDSHHistorySearchSchemaUsesStrictNullableLimit(t *testing.T) {
	definition := cloudDSHConversationHistorySearchDefinition()
	if !definition.Strict {
		t.Fatal("history search schema must use strict tool calling")
	}
	var schema struct {
		Properties map[string]struct {
			Type any `json:"type"`
		} `json:"properties"`
		Required             []string `json:"required"`
		AdditionalProperties bool     `json:"additionalProperties"`
	}
	if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Required) != 2 || schema.Required[0] != "query" || schema.Required[1] != "limit" ||
		schema.AdditionalProperties || schema.Properties["limit"].Type == nil {
		t.Fatalf("history search schema is not strict-compatible: %s", definition.Parameters)
	}
	var limitTypes []string
	encoded, err := json.Marshal(schema.Properties["limit"].Type)
	if err != nil || json.Unmarshal(encoded, &limitTypes) != nil || len(limitTypes) != 2 ||
		limitTypes[0] != "integer" || limitTypes[1] != "null" {
		t.Fatalf("history search limit schema is not nullable: %s", definition.Parameters)
	}
}

func TestCloudDSHRecentHistorySummarySchemaIsStrictAndArgumentFree(t *testing.T) {
	definition := cloudDSHRecentHistorySummaryDefinition()
	if !definition.Strict {
		t.Fatal("recent history summary schema must use strict tool calling")
	}
	var schema struct {
		Properties           map[string]any `json:"properties"`
		Required             []string       `json:"required"`
		AdditionalProperties bool           `json:"additionalProperties"`
	}
	if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Properties) != 0 || len(schema.Required) != 0 || schema.AdditionalProperties {
		t.Fatalf("recent history summary schema is not strict-compatible: %s", definition.Parameters)
	}
}

func TestCloudDSHHistorySearchSkipsCredentialLikeMessagesAndTitles(t *testing.T) {
	broker, baseStore, claimed, _, _ := cloudDSHGoalScheduleTaskFixture(t)
	previousID, err := domain.NewConversationID()
	if err != nil {
		t.Fatal(err)
	}
	messageID1, err := domain.NewMessageID()
	if err != nil {
		t.Fatal(err)
	}
	messageID2, err := domain.NewMessageID()
	if err != nil {
		t.Fatal(err)
	}
	credential := "sk-proj-ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefghijklmnopqrstuvwxyz"
	store := &cloudDSHHistoryTaskStore{
		cloudDSHBrokerTaskStore: baseStore,
		conversations:           ConversationPage{Items: []conversation.Conversation{{ID: previousID, OwnerID: claimed.OwnerID, Title: credential}}},
		messages: map[string]MessagePage{previousID: {Items: []conversation.Message{
			{ID: messageID1, ConversationID: previousID, OwnerID: claimed.OwnerID, Role: conversation.RoleUser,
				Content: "茶叶 API 凭据是 " + credential, DeliveryStatus: conversation.DeliveryApplied},
			{ID: messageID2, ConversationID: previousID, OwnerID: claimed.OwnerID, Role: conversation.RoleAssistant,
				Content: "这次我们讨论了茶叶冲泡。", DeliveryStatus: conversation.DeliveryApplied},
		}}},
	}
	broker.store = store
	principal, ok := cloudDSHPersonalPrincipal(claimed)
	if !ok {
		t.Fatal("fixture did not produce a personal principal")
	}
	result, err := broker.searchPersonalConversationHistory(context.Background(), principal, claimed.ConversationID, "茶叶", 5)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(result.Matches) != 1 || result.Matches[0].Content != "这次我们讨论了茶叶冲泡。" ||
		result.Matches[0].ConversationTitle != "对话" || string(encoded) == "" ||
		cloudDSHHistoryCredentialPattern.Match(encoded) {
		t.Fatalf("credential-like history was not excluded: result=%+v err=%v", result, err)
	}
}

func containsAgentTool(definitions []AgentToolDefinition, name string) bool {
	for _, definition := range definitions {
		if definition.Name == name {
			return true
		}
	}
	return false
}
