package app

import (
	"strings"
	"testing"

	"morde/server/internal/domain/conversation"
)

func TestConversationLongReplyOutputTokensUsesVerifiedModelLimit(t *testing.T) {
	context := ConversationRouteContext{ActorID: "u1234567"}
	const modelOutputLimit = 8192
	got, err := ConversationLongReplyOutputTokens(context, "Please explain the design choices.", 32768, modelOutputLimit)
	if err != nil {
		t.Fatal(err)
	}
	if got != modelOutputLimit {
		t.Fatalf("output token budget = %d, want verified model limit %d", got, modelOutputLimit)
	}
	request, err := ConversationLongReplyModelRequest(context, "Please explain the design choices.", got)
	if err != nil || request.MaxOutputTokens != modelOutputLimit || len(request.Tools) != 0 || len(request.Input) != 1 ||
		request.Input[0].Type != "message" || request.Input[0].Role != string(conversation.RoleUser) ||
		request.Input[0].Text != "Please explain the design choices." ||
		!ConversationRouteInputFitsContextWindow(request, 32768) {
		t.Fatalf("long-form request did not preserve the exact text-only budget: %+v err=%v", request, err)
	}
}

func TestConversationLongReplyOutputTokensRespectsInputContext(t *testing.T) {
	context := ConversationRouteContext{ActorID: "u1234567", History: []ConversationRouteHistory{{
		MessageID: "m1234567", Sequence: 1, Role: conversation.RoleAssistant, Content: strings.Repeat("long context ", 4000),
	}}}
	currentMessage := strings.Repeat("question ", 4000)
	got, err := ConversationLongReplyOutputTokens(context, currentMessage, 32768, 8192)
	if err != nil {
		t.Fatal(err)
	}
	request, err := ConversationLongReplyModelRequest(context, currentMessage, got)
	if err != nil || got < 1 || got >= 8192 || len(request.Input) != 2 || request.Input[0].Type != "message" ||
		request.Input[1].Type != "message" || !ConversationRouteInputFitsContextWindow(request, 32768) {
		t.Fatalf("context-aware output budget = %d request=%+v err=%v", got, request, err)
	}
}

func TestValidConversationLongReply(t *testing.T) {
	for _, value := range []string{"A complete answer.", "  已整理完成。  "} {
		if !ValidConversationLongReply(value) {
			t.Fatalf("valid long reply rejected: %q", value)
		}
	}
	for _, value := range []string{"", "  ", "bad\x00reply", string([]byte{0xff}), strings.Repeat("x", MaxAssistantReplyBytes+1)} {
		if ValidConversationLongReply(value) {
			t.Fatalf("invalid long reply accepted: %q", value[:min(len(value), 40)])
		}
	}
}
