package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

func TestFitConversationRouteContextDropsHistoryTextButKeepsReferences(t *testing.T) {
	value := app.ConversationRouteContext{ActorID: "u1234567", ModelID: "p1234567", Content: "short current message"}
	for sequence := int64(1); sequence <= 8; sequence++ {
		value.History = append(value.History, app.ConversationRouteHistory{
			MessageID: fmt.Sprintf("m%07d", sequence), Sequence: sequence,
			Role: conversation.RoleAssistant, Content: strings.Repeat("x", 1800), Truncated: true,
		})
	}
	if err := fitConversationRouteContext(&value, 4096); err != nil {
		t.Fatalf("fit history context: %v", err)
	}
	if value.Content != "short current message" || !value.HistoryIncomplete {
		t.Fatalf("context fitting changed the current message or omitted the incomplete marker: %+v", value)
	}
	trimmed := false
	for i, history := range value.History {
		if history.MessageID == "" || history.Sequence != int64(i+1) {
			t.Fatalf("history identity/order was lost: %+v", value.History)
		}
		if history.Content == "" {
			trimmed = true
			if !history.Truncated {
				t.Fatalf("trimmed history was not marked truncated: %+v", history)
			}
		}
	}
	if !trimmed {
		t.Fatal("over-budget history text was not removed")
	}
	request, err := app.ConversationRouteModelRequest(value)
	if err != nil {
		t.Fatal(err)
	}
	var encoded struct {
		History []struct {
			MessageID string `json:"message_id"`
		} `json:"history"`
	}
	if err := json.Unmarshal([]byte(request.Input[0].Text), &encoded); err != nil {
		t.Fatal(err)
	}
	if len(encoded.History) != len(value.History) {
		t.Fatalf("fitted request lost history references: %d/%d", len(encoded.History), len(value.History))
	}
	for index, item := range encoded.History {
		if item.MessageID != value.History[index].MessageID {
			t.Fatalf("history reference %d = %q, want %q", index, item.MessageID, value.History[index].MessageID)
		}
	}
}

func TestFitConversationRouteContextDropsOversizedSummary(t *testing.T) {
	value := app.ConversationRouteContext{ActorID: "u1234567", ModelID: "p1234567", Content: "short current message",
		SummaryVersion: 1, SummaryText: strings.Repeat("s", 16<<10)}
	summaryDigest, sourcesDigest := sha256.Sum256([]byte(value.SummaryText)), sha256.Sum256([]byte("sources"))
	value.SummarySHA256, value.SummarySourcesSHA256 = summaryDigest[:], sourcesDigest[:]
	if err := fitConversationRouteContext(&value, 4096); err != nil {
		t.Fatalf("fit oversized summary: %v", err)
	}
	if value.Content != "short current message" || value.SummaryVersion != 0 || value.SummaryText != "" ||
		len(value.SummarySHA256) != 0 || len(value.SummarySourcesSHA256) != 0 || !value.HistoryIncomplete {
		t.Fatalf("oversized summary was not removed with an incomplete-history marker: %+v", value)
	}
}

func TestFitConversationRouteContextFitsMinimumWindowWithIncompleteHistoryAndCandidates(t *testing.T) {
	value := app.ConversationRouteContext{ActorID: "u1234567", Content: "A30 path traversal: A30_TRAVERSAL_FETCH ../outside.pdf",
		HistoryIncomplete: true, ServerReceivedAt: time.Now(), RequestCandidates: []app.ConversationRouteRequestCandidate{{
			MessageID: "m8765432", Sequence: 9, Content: strings.Repeat("previous request ", 400), RoutingStatus: conversation.RoutingPending,
		}}, Candidates: []app.ConversationRouteCandidate{{
			TaskID: "q1234567", InitiatorID: "u1234567", Objective: strings.Repeat("task objective ", 400),
			Status: task.StatusRunning, Revision: 1,
		}}}
	for sequence := int64(1); sequence <= 7; sequence++ {
		value.History = append(value.History, app.ConversationRouteHistory{
			MessageID: fmt.Sprintf("m%07d", sequence), Sequence: sequence, TaskID: "t1234567",
			TaskRevision: 1, EventSequence: sequence, Role: "assistant", Truncated: true,
		})
	}
	if err := fitConversationRouteContext(&value, 4096); err != nil {
		t.Fatalf("short message with bounded history and task candidates did not fit minimum window: %v", err)
	}
	if !value.CandidateSetIncomplete || len(value.RequestCandidates) != 0 || len(value.Candidates) != 0 || !value.HistoryIncomplete {
		t.Fatalf("oversized candidates/history were not safely marked and trimmed: %+v", value)
	}
	request, err := app.ConversationRouteModelRequest(value)
	if err != nil || !app.ConversationRouteInputFitsContextWindow(request, 4096) {
		t.Fatalf("fitted incomplete-history request exceeds minimum window: err=%v", err)
	}
}

func TestConversationRouteWorkerUsesMinimumWindowWhenCapacityIsUnknown(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	stub.contextWindow, stub.capacityKnown = 4096, false
	stub.claim.Message = conversation.Message{ID: "m1234567", Role: conversation.RoleUser, Sequence: 1,
		CreatedAt: time.Now(), Content: strings.Repeat("汉", 3200)}
	stub.snapshot.Context.Content = stub.claim.Message.Content
	if found, err := worker.RunOnce(context.Background()); err != nil || !found || stub.classifies != 0 ||
		len(stub.failures) != 1 || stub.failures[0] != "message_too_long" {
		t.Fatalf("unknown capacity did not fail safely at the minimum window: found=%t classifications=%d failures=%v err=%v",
			found, stub.classifies, stub.failures, err)
	}
	if domain.CodeOf(fitConversationRouteContext(&stub.snapshot.Context, 4096)) != domain.CodeResultTooLarge {
		t.Fatal("minimum supported window unexpectedly accepted the current message")
	}
}

func TestConversationRouteWorkerMarksUncoveredOlderHistoryIncomplete(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	stub.claim.Message.Sequence = 10
	for sequence := int64(1); sequence <= 8; sequence++ {
		stub.snapshot.Context.History = append(stub.snapshot.Context.History, app.ConversationRouteHistory{
			MessageID: fmt.Sprintf("m%07d", sequence), Sequence: sequence, Role: conversation.RoleAssistant, Content: "recent history",
		})
	}
	if found, err := worker.RunOnce(context.Background()); err != nil || !found || stub.classifies != 1 ||
		!stub.classified.HistoryIncomplete || len(stub.classified.History) != 8 {
		t.Fatalf("history beyond the recent window was presented as complete: found=%t classifications=%d context=%+v err=%v",
			found, stub.classifies, stub.classified, err)
	}

	worker, stub = newRouteWorkerTest(t)
	stub.claim.Message.Sequence = 2
	if _, err := worker.RunOnce(context.Background()); err != nil || stub.classifies != 1 || stub.classified.HistoryIncomplete {
		t.Fatalf("complete short history was marked incomplete: classifications=%d context=%+v err=%v",
			stub.classifies, stub.classified, err)
	}
}
