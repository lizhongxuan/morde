package app

import (
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

func TestConversationRouteDecisionFencesModelTargets(t *testing.T) {
	candidates := []ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 4}}
	for _, test := range []struct {
		name   string
		output string
		action ConversationRouteAction
		taskID string
	}{
		{"update", `{"action":"update","task_id":"q1234567","question":""}`, ConversationRouteUpdate, "q1234567"},
		{"independent", `{"action":"new","task_id":"","question":""}`, ConversationRouteNew, ""},
		{"status", `{"action":"status","task_id":"q1234567","question":""}`, ConversationRouteStatus, "q1234567"},
		{"stop", `{"action":"stop","task_id":"q1234567","question":""}`, ConversationRouteStop, "q1234567"},
		{"direct reply", `{"action":"reply","task_id":"","question":"","reply":"你好，答案是 42。"}`, ConversationRouteReply, ""},
		{"ambiguous", `{"action":"clarify","task_id":"","question":"你指的是哪个任务？"}`, ConversationRouteClarify, ""},
		{"unknown task", `{"action":"stop","task_id":"q7654321","question":""}`, ConversationRouteClarify, ""},
		{"extra fields", `{"action":"update","task_id":"q1234567","question":"","admin":true}`, ConversationRouteClarify, ""},
		{"malformed", `{"action":"update"`, ConversationRouteClarify, ""},
		{"trailing data", `{"action":"new","task_id":"","question":""} {}`, ConversationRouteClarify, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := ParseConversationRouteDecision(test.output, candidates)
			if decision.Action != test.action || decision.TaskID != test.taskID {
				t.Fatalf("decision = %+v", decision)
			}
			if test.taskID != "" && decision.ExpectedRevision != 4 {
				t.Fatalf("revision was not taken from the authorized snapshot: %+v", decision)
			}
			if test.action == ConversationRouteClarify && decision.Question == "" {
				t.Fatal("clarification must have a visible question")
			}
			if test.action == ConversationRouteReply && (decision.Reply != "你好，答案是 42。" || decision.ModelOutput != test.output) {
				t.Fatalf("direct reply or original model output was not preserved: %+v", decision)
			}
		})
	}
}

func TestConversationRouteDirectReplyRequiresValidTargetlessText(t *testing.T) {
	for _, output := range []string{
		`{"action":"reply","task_id":"q1234567","question":"","reply":"answer"}`,
		`{"action":"reply","task_id":"","question":"which?","reply":"answer"}`,
		`{"action":"reply","task_id":"","question":"","reply":""}`,
		`{"action":"reply","task_id":"","question":"","reply":"   "}`,
		`{"action":"reply","task_id":"","question":"","reply":"\u0000"}`,
		`{"action":"reply","task_id":"","question":"","reply":"` + strings.Repeat("x", MaxConversationRouteReplyRunes+1) + `"}`,
		`{"action":"new","task_id":"","question":"","reply":"must not be smuggled"}`,
	} {
		decision := ParseConversationRouteDecision(output, nil)
		if decision.Action != ConversationRouteClarify || decision.Reply != "" {
			t.Errorf("unsafe output was accepted as a reply: %q => %+v", output, decision)
		}
	}
	invalidUTF8 := string([]byte{'{', '"', 'a', 'c', 't', 'i', 'o', 'n', '"', ':', '"', 'r', 'e', 'p', 'l', 'y', '"', ',', 0xff, '}'})
	if decision := ParseConversationRouteDecision(invalidUTF8, nil); decision.Action != ConversationRouteClarify {
		t.Fatalf("invalid UTF-8 model output was accepted: %+v", decision)
	}
	if ValidConversationRouteModelOutput(strings.Repeat("x", MaxConversationRouteModelOutputBytes+1)) ||
		ValidConversationRouteModelOutput(string([]byte{0xff})) || ValidConversationRouteModelOutput("model\x00output") {
		t.Fatal("invalid durable model output passed validation")
	}
}

func TestConversationRouteDirectReplyCannotHideUnknownTaskControl(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", Content: "停止任务", ReferenceTaskID: "q1234567", ReferenceUnavailable: true}
	decision := ParseConversationRouteDecisionForContext(`{"action":"reply","task_id":"","question":"","reply":"I cannot stop it."}`, value)
	if decision.Action != ConversationRouteClarify || decision.Question != UnavailableRouteReferenceMessage || decision.ModelOutput == "" {
		t.Fatalf("reply bypassed unavailable-reference safety or lost source output: %+v", decision)
	}
	value.ReferenceUnavailable = false
	value.ReferenceTaskID = ""
	value.PreviousMissing = true
	value.PreviousUnresolved = true
	decision = ParseConversationRouteDecisionForContext(`{"action":"reply","task_id":"","question":"","reply":"The answer is 42."}`, value)
	if decision.Action != ConversationRouteClarify || decision.Question != "前一条消息尚未确认，请先处理或重试。" || decision.ModelOutput == "" {
		t.Fatalf("reply used unavailable predecessor context or lost source output: %+v", decision)
	}
}

func TestConversationRouteRequestUsesOnlyAuthorizedTaskContext(t *testing.T) {
	value := ConversationRouteContext{
		ActorID: "u1234567", Content: "只看收入和净利润", ReferenceTaskID: "q1234567",
		History: []ConversationRouteHistory{{MessageID: "m1234567", Sequence: 1, Role: conversation.RoleUser, Content: "整理财报"}},
		Candidates: []ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567",
			Objective: "整理财报", Status: task.StatusRunning, Revision: 2}},
	}
	request, err := ConversationRouteModelRequest(value)
	if err != nil || len(request.Input) != 1 || len(request.Tools) != 0 ||
		!strings.Contains(request.Input[0].Text, "只看收入和净利润") || !strings.Contains(request.Input[0].Text, "q1234567") ||
		strings.Contains(request.Input[0].Text, "u1234567") {
		t.Fatalf("routing request is invalid: %+v err=%v", request, err)
	}
	value.Candidates[0].InitiatorID = "u7654321"
	if _, err := ConversationRouteModelRequest(value); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("another actor's task was accepted into routing context: %v", err)
	}
	value.Candidates[0].InitiatorID = value.ActorID
	value.History = append(value.History, ConversationRouteHistory{MessageID: "m2345678", Sequence: 1, Role: conversation.RoleAssistant, Content: "重复序号"})
	if _, err := ConversationRouteModelRequest(value); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("unordered history evidence was accepted: %v", err)
	}
	value.History = value.History[:1]
	value.Content = strings.Repeat("中", maxAgentInputBytes)
	if _, err := ConversationRouteModelRequest(value); domain.CodeOf(err) != domain.CodeResultTooLarge {
		t.Fatalf("oversized routing context should fail without truncation: %v", err)
	}
}

func TestUnavailableRouteReferenceIsHiddenAndControlsAreRejected(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", Content: "重新整理财报", ReferenceTaskID: "q1234567", ReferenceUnavailable: true}
	request, err := ConversationRouteModelRequest(value)
	if err != nil || len(request.Input) != 1 || !strings.Contains(request.Input[0].Text, `"reference_unavailable":true`) ||
		strings.Contains(request.Input[0].Text, "q1234567") || strings.Contains(request.Input[0].Text, `"reference_task_id"`) {
		t.Fatalf("model input exposed an unavailable reference: request=%+v err=%v", request, err)
	}
	for _, action := range []string{"update", "status", "stop"} {
		decision := ParseConversationRouteDecisionForContext(`{"action":"`+action+`","task_id":"q1234567","question":""}`, value)
		if decision.Action != ConversationRouteClarify || decision.Question != UnavailableRouteReferenceMessage || decision.TaskID != "" {
			t.Fatalf("unavailable control was accepted: action=%s decision=%+v", action, decision)
		}
	}
	if decision := ParseConversationRouteDecisionForContext(`{"action":"new","task_id":"","question":""}`, value); decision.Action != ConversationRouteNew {
		t.Fatalf("new work was not allowed: %+v", decision)
	}
	for _, content := range []string{"停止任务", "请帮我暂停当前任务", "stop this task", "what is the task status?"} {
		value.Content = content
		decision := ParseConversationRouteDecisionForContext(`{"action":"new","task_id":"","question":""}`, value)
		if decision.Action != ConversationRouteClarify || decision.Question != UnavailableRouteReferenceMessage {
			t.Fatalf("explicit control intent with an unavailable reference became new work: content=%q decision=%+v", content, decision)
		}
	}
	for _, content := range []string{"重新整理财报", "搜索一下停止任务的新闻", "show me how to cancel a task"} {
		value.Content = content
		decision := ParseConversationRouteDecisionForContext(`{"action":"new","task_id":"","question":""}`, value)
		if decision.Action != ConversationRouteNew {
			t.Fatalf("independent work was blocked by a stale reference: content=%q decision=%+v", content, decision)
		}
	}
	value.Content = "重新整理财报"
	value.ReferenceUnavailable = false
	value.ReferenceTaskID = ""
	value.PreviousMissing = true
	value.PreviousUnresolved = true
	value.Content = "停止任务"
	if decision := ParseConversationRouteDecisionForContext(`{"action":"new","task_id":"","question":""}`, value); decision.Action != ConversationRouteClarify || decision.Question != "前一条消息尚未确认，请先处理或重试。" {
		t.Fatalf("missing predecessor control intent became new work: %+v", decision)
	}
	value.Content = "另外写一首春天的短诗"
	if decision := ParseConversationRouteDecisionForContext(`{"action":"new","task_id":"","question":""}`, value); decision.Action != ConversationRouteNew {
		t.Fatalf("independent work was blocked by a missing predecessor: %+v", decision)
	}
	value.PreviousMissing = false
	value.PreviousUnresolved = false
	value.ReferenceTaskID = "q1234567"
	value.ReferenceUnavailable = true
	value.Content = "重新整理财报"
	value.Candidates = []ConversationRouteCandidate{{TaskID: value.ReferenceTaskID, InitiatorID: value.ActorID, Status: task.StatusWaitingUser}}
	if _, err := ConversationRouteModelRequest(value); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("unavailable Task leaked through the candidate list: %v", err)
	}
	value.Candidates = nil
	value.ReferenceTaskID = ""
	if _, err := ConversationRouteModelRequest(value); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("unavailable flag without a reference was accepted: %v", err)
	}
}
