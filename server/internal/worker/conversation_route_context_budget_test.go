package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

func TestFitConversationRouteContextDropsHistoryTextButKeepsReferences(t *testing.T) {
	value := app.ConversationRouteContext{ActorID: "u1234567", ModelID: "p1234567", Content: "short current message"}
	for sequence := int64(1); sequence <= 8; sequence++ {
		value.History = append(value.History, app.ConversationRouteHistory{
			MessageID: fmt.Sprintf("m%07d", sequence), Sequence: sequence,
			Role: conversation.RoleAssistant, Content: strings.Repeat("x", 1800),
		})
	}
	if err := fitConversationRouteContext(&value, 4096); err != nil {
		t.Fatalf("fit history context: %v", err)
	}
	if value.Content != "short current message" || !value.HistoryIncomplete {
		t.Fatalf("context fitting changed the current message or omitted the incomplete marker: %+v", value)
	}
	trimmed := false
	var previousSequence int64
	for _, history := range value.History {
		if history.MessageID == "" || history.Sequence <= previousSequence {
			t.Fatalf("history identity/order was lost: %+v", value.History)
		}
		previousSequence = history.Sequence
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
			MessageID     string `json:"message_id"`
			ContentBytes  int64  `json:"content_bytes"`
			ContentSHA256 string `json:"content_sha256"`
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
		wantDigest := sha256.Sum256([]byte(strings.Repeat("x", 1800)))
		if item.ContentBytes != 1800 || item.ContentSHA256 != hex.EncodeToString(wantDigest[:]) {
			t.Fatalf("history source metadata %d changed while fitting: %+v", index, item)
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

func TestTrimConversationRouteHistoryReferencesMarksAuthorizedSearch(t *testing.T) {
	firstSource := strings.Repeat("早期原始要求：保留版本号和数字。", 400)
	secondSource := strings.Repeat("中间原始要求：保留交付格式。", 300)
	firstDigest, secondDigest := sha256.Sum256([]byte(firstSource)), sha256.Sum256([]byte(secondSource))
	original := []app.ConversationRouteHistory{
		{MessageID: "m0000001", Sequence: 1, Role: conversation.RoleUser, Content: firstSource,
			ContentBytes: int64(len(firstSource)), SourceSHA256: firstDigest[:]},
		{MessageID: "m0000002", Sequence: 2, Role: conversation.RoleAssistant, Content: secondSource,
			ContentBytes: int64(len(secondSource)), SourceSHA256: secondDigest[:]},
		{MessageID: "m0000003", Sequence: 3, Role: conversation.RoleUser, Content: "最近的完整要求", ContentBytes: int64(len("最近的完整要求"))},
	}
	value := app.ConversationRouteContext{ActorID: "u1234567", ModelID: "p1234567", Content: "请回忆早期要求",
		History: append([]app.ConversationRouteHistory(nil), original...)}
	if !blankConversationRouteHistory(&value) || !blankConversationRouteHistory(&value) ||
		value.History[0].Content != "" || value.History[1].Content != "" || !value.HistoryIncomplete {
		t.Fatalf("history text was not safely blanked before reference trimming: %+v", value.History)
	}
	if !trimConversationRouteHistoryReferences(&value) || len(value.History) != 2 || !value.HistoryIncomplete ||
		value.History[0].MessageID != "m0000002" || value.History[1].MessageID != "m0000003" {
		t.Fatalf("oldest blank reference was not dropped with an incomplete marker: %+v", value)
	}
	if original[0].MessageID != "m0000001" || original[0].Content != firstSource || original[0].ContentBytes != int64(len(firstSource)) ||
		!bytes.Equal(original[0].SourceSHA256, firstDigest[:]) || original[1].Content != secondSource {
		t.Fatalf("dropping a model-context reference mutated the source snapshot: %+v", original[0])
	}
	request, err := app.ConversationRouteModelRequest(value)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(request.Instructions, "morde_current_conversation_history_search") ||
		!strings.Contains(request.Instructions, "authorized conversation originals") ||
		!strings.Contains(request.Input[0].Text, `"history_incomplete":true`) ||
		strings.Contains(request.Input[0].Text, "m0000001") || !strings.Contains(request.Input[0].Text, "m0000002") {
		t.Fatalf("incomplete history did not direct the classifier to authorized source search: instructions=%q input=%q",
			request.Instructions, request.Input[0].Text)
	}
}

func TestFitConversationRouteContextKeepsSummaryWhilePreviewingHistory(t *testing.T) {
	value := app.ConversationRouteContext{ActorID: "u1234567", ModelID: "p1234567", Content: "short current message",
		SummaryVersion: 1, SummaryText: strings.Repeat("s", 2048)}
	summaryDigest, sourcesDigest := sha256.Sum256([]byte(value.SummaryText)), sha256.Sum256([]byte("sources"))
	value.SummarySHA256, value.SummarySourcesSHA256 = summaryDigest[:], sourcesDigest[:]
	for sequence := int64(1); sequence <= 8; sequence++ {
		value.History = append(value.History, app.ConversationRouteHistory{
			MessageID: fmt.Sprintf("m%07d", sequence), Sequence: sequence,
			Role: conversation.RoleAssistant, Content: strings.Repeat("x", 4000),
		})
	}
	if err := fitConversationRouteContext(&value, 8192); err != nil {
		t.Fatalf("fit history context with summary: %v", err)
	}
	if value.Content != "short current message" || value.SummaryVersion != 1 || value.SummaryText == "" || !value.HistoryIncomplete {
		t.Fatalf("fitting dropped the summary before shrinking history: %+v", value)
	}
	previewed := false
	var previousSequence int64
	for _, history := range value.History {
		if history.MessageID == "" || history.Sequence <= previousSequence {
			t.Fatalf("history identity/order was lost: %+v", value.History)
		}
		previousSequence = history.Sequence
		originalDigest := sha256.Sum256([]byte(strings.Repeat("x", 4000)))
		if history.ContentBytes != 4000 || string(history.SourceSHA256) != string(originalDigest[:]) ||
			history.ContentSHA256 != hex.EncodeToString(originalDigest[:]) {
			t.Fatalf("history source metadata was not kept through preview/blanking: %+v", history)
		}
		if history.Content != "" && len(history.Content) < 4000 && history.Truncated {
			previewed = true
		}
	}
	if !previewed {
		t.Fatal("over-budget history was not shrunk to a preview before the summary was kept")
	}
	request, err := app.ConversationRouteModelRequest(value)
	if err != nil || !app.ConversationRouteInputFitsContextWindow(request, 8192) {
		t.Fatalf("fitted request exceeds the window: err=%v", err)
	}
}

func TestPreviewConversationRouteHistoryKeepsStoredSourceMetadata(t *testing.T) {
	original := strings.Repeat("历史要求：保留开票日期、金额和付款方式。", 180)
	digest := sha256.Sum256([]byte(original))
	storedPreview, truncated := app.ConversationRouteTextPreview(original, 4096)
	if !truncated {
		t.Fatal("synthetic source did not require a stored preview")
	}
	value := app.ConversationRouteContext{History: []app.ConversationRouteHistory{{
		MessageID: "m1234567", Sequence: 1, Role: conversation.RoleUser, Content: storedPreview,
		ContentBytes: int64(len(original)), SourceSHA256: digest[:], Truncated: true,
	}}}
	wantPreview, _ := app.ConversationRouteTextPreview(storedPreview, app.ConversationRouteHistoryPreviewBytes)
	if !previewConversationRouteHistory(&value) || value.History[0].Content != wantPreview ||
		value.History[0].ContentBytes != int64(len(original)) || string(value.History[0].SourceSHA256) != string(digest[:]) ||
		!value.History[0].Truncated || !value.HistoryIncomplete || int64(len(value.History[0].Content)) > value.History[0].ContentBytes {
		t.Fatalf("stored history preview lost original source metadata: %+v", value)
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
		original := fmt.Sprintf("synthetic source %d with additional historical requirements", sequence)
		digest := sha256.Sum256([]byte(original))
		value.History = append(value.History, app.ConversationRouteHistory{
			MessageID: fmt.Sprintf("m%07d", sequence), Sequence: sequence, TaskID: "t1234567",
			TaskRevision: 1, EventSequence: sequence, Role: "assistant", Truncated: true,
			ContentBytes: int64(len(original)), SourceSHA256: digest[:],
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

func TestFitConversationRouteContextShrinksReferencedTaskForShortFollowup(t *testing.T) {
	value := app.ConversationRouteContext{ActorID: "u1234567", Content: "这", ReferenceTaskID: "q1234567",
		Candidates: []app.ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567",
			Objective: strings.Repeat("请保留这个任务的原始约束并继续执行。", 300), Status: task.StatusRunning, Revision: 1}}}
	if err := fitConversationRouteContext(&value, 4096); err != nil {
		t.Fatalf("short follow-up with a referenced long-running task did not fit minimum window: %v", err)
	}
	if len(value.Candidates) != 1 || value.Candidates[0].TaskID != value.ReferenceTaskID || value.Candidates[0].Objective == "" ||
		!value.Candidates[0].ObjectiveTruncated || !value.CandidateSetIncomplete {
		t.Fatalf("fitting removed or silently truncated the referenced task: %+v", value)
	}
	request, err := app.ConversationRouteModelRequest(value)
	if err != nil || !app.ConversationRouteInputFitsContextWindow(request, 4096) ||
		!strings.Contains(request.Input[0].Text, `"objective_truncated":true`) {
		t.Fatalf("short follow-up route request still exceeds the minimum window: err=%v", err)
	}
}

func TestFitConversationRouteContextShrinksPreviousRequestWithoutLosingItsDigest(t *testing.T) {
	const previousID = "m7654321"
	previousContent := strings.Repeat("这是之前的完整要求及其约束。", 300)
	digest := sha256.Sum256([]byte(previousContent))
	scope := make([]byte, sha256.Size)
	value := app.ConversationRouteContext{ActorID: "u1234567", Content: "这", PreviousMessageID: previousID,
		ExecutionScopeDigest: scope, RequestCandidates: []app.ConversationRouteRequestCandidate{{MessageID: previousID,
			Sequence: 1, Content: previousContent, RoutingStatus: conversation.RoutingProcessing,
			SameExecutionScope: true, SameAuthorization: true, ExecutionScopeDigest: scope}}}
	if err := fitConversationRouteContext(&value, 4096); err != nil {
		t.Fatalf("short follow-up with a referenced long prior request did not fit minimum window: %v", err)
	}
	candidate := value.RequestCandidates[0]
	if candidate.MessageID != previousID || !candidate.ContentTruncated || len(candidate.Content) >= len(previousContent) ||
		!bytes.Equal(candidate.ContentSHA256, digest[:]) || !app.ValidConversationRouteExactDuplicateTarget(previousID, previousContent, value.RequestCandidates) {
		t.Fatalf("fitting lost the prior request identity or original-content proof: %+v", candidate)
	}
	request, err := app.ConversationRouteModelRequest(value)
	if err != nil || !app.ConversationRouteInputFitsContextWindow(request, 4096) {
		t.Fatalf("short follow-up route request still exceeds the minimum window: err=%v", err)
	}
}

func TestConversationRouteWorkerFitsReferencedLongTaskAtUnknownCapacity(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	stub.contextWindow, stub.capacityKnown = 4096, false
	stub.claim.Message = conversation.Message{ID: "m1234567", Role: conversation.RoleUser, Sequence: 2,
		CreatedAt: time.Now(), Content: "这"}
	stub.snapshot.Context = app.ConversationRouteContext{ActorID: "u1234567", Content: "这", ReferenceTaskID: "q1234567",
		Candidates: []app.ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567",
			Objective: strings.Repeat("请保留这个任务的原始约束并继续执行。", 300), Status: task.StatusRunning, Revision: 1}}}
	if found, err := worker.RunOnce(context.Background()); err != nil || !found || stub.classifies != 1 || len(stub.failures) != 0 ||
		len(stub.classified.Candidates) != 1 || stub.classified.Candidates[0].TaskID != "q1234567" ||
		!stub.classified.Candidates[0].ObjectiveTruncated {
		t.Fatalf("short follow-up did not route with its pinned task at unknown 4K capacity: found=%t classified=%+v failures=%v err=%v",
			found, stub.classified, stub.failures, err)
	}
}

func TestConversationRouteWorkerPreviewsCurrentMessageAtMinimumWindowWhenCapacityIsUnknown(t *testing.T) {
	for _, size := range []int{8<<10 + 1, 16<<10 + 1, 200<<10 + 1} {
		t.Run(fmt.Sprintf("%d_bytes", size), func(t *testing.T) {
			worker, stub := newRouteWorkerTest(t)
			stub.contextWindow, stub.capacityKnown = 4096, false
			prefix, middle, suffix := "START-ANCHOR\n", "MIDDLE-ANCHOR\n", "END-ANCHOR"
			padding := size - len(prefix) - len(middle) - len(suffix)
			content := prefix + strings.Repeat("x", padding/2) + middle + strings.Repeat("y", padding-padding/2) + suffix
			stub.claim.Message = conversation.Message{ID: "m1234567", Role: conversation.RoleUser, Sequence: 1,
				CreatedAt: time.Now(), Content: content}
			stub.snapshot.Context.Content = content
			stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteNew}
			if found, err := worker.RunOnce(context.Background()); err != nil || !found || stub.classifies != 1 ||
				len(stub.failures) != 0 || len(stub.applied) != 1 || stub.applied[0].Action != app.ConversationRouteNew ||
				!stub.classified.ContentTruncated || stub.classified.CurrentMessageID != stub.claim.Message.ID ||
				stub.classified.ContentBytes != int64(len(stub.claim.Message.Content)) {
				t.Fatalf("unknown capacity did not route the full source through a verified preview: found=%t classifications=%d classified=%+v failures=%v err=%v",
					found, stub.classifies, stub.classified, stub.failures, err)
			}
			request, err := app.ConversationRouteModelRequest(stub.classified)
			var routed struct {
				Message          string `json:"message"`
				MessageBytes     int64  `json:"message_bytes"`
				MessageTruncated bool   `json:"message_truncated"`
			}
			if err == nil {
				err = json.Unmarshal([]byte(request.Input[0].Text), &routed)
			}
			digest := sha256.Sum256([]byte(content))
			context := stub.snapshot.Context
			context.CurrentMessageID = stub.claim.Message.ID
			fitErr := fitConversationRouteContext(&context, 4096)
			if err != nil || fitErr != nil || !context.ContentTruncated || !bytes.Equal(context.ContentSHA256, digest[:]) ||
				context.CurrentMessageID != stub.claim.Message.ID || routed.MessageBytes != int64(size) || !routed.MessageTruncated ||
				len(routed.Message) >= size || !strings.Contains(routed.Message, "START-ANCHOR") ||
				!strings.Contains(routed.Message, "END-ANCHOR") || !app.ConversationRouteInputFitsContextWindow(request, 4096) {
				t.Fatalf("minimum-window preview lost its source proof or exceeded 4K: context=%+v routed=%+v requestBytes=%d err=%v fitErr=%v",
					context, routed, len(request.Input[0].Text), err, fitErr)
			}
		})
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
