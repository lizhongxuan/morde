package app

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

func TestConversationRouteModelRequestCarriesStableLocalizedTime(t *testing.T) {
	receivedAt := time.Date(2026, 11, 12, 3, 4, 5, 0, time.UTC)
	request, err := ConversationRouteModelRequest(ConversationRouteContext{ActorID: "u1234567", Content: "现在几点？",
		TimeZone: "Asia/Shanghai", ServerReceivedAt: receivedAt})
	if err != nil {
		t.Fatal(err)
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(request.Input[0].Text), &input); err != nil {
		t.Fatal(err)
	}
	if input["reference_time"] != "2026-11-12T11:04:05+08:00" || input["time_zone"] != "Asia/Shanghai" || input["time_zone_provided"] != true {
		t.Fatalf("localized server receipt time missing: %+v", input)
	}
	if !strings.Contains(request.Instructions, "jokes") || !strings.Contains(request.Instructions, "server received") ||
		!strings.Contains(request.Instructions, "must not create a Task") ||
		!strings.Contains(request.Instructions, "acknowledgements") ||
		!strings.Contains(request.Instructions, "text-only requests for small code snippets") ||
		!strings.Contains(request.Instructions, "Explicit requests to delegate") ||
		!strings.Contains(request.Instructions, "do not replace the requested delegation with a static text-only answer") ||
		!strings.Contains(request.Instructions, "ask for the exact missing information") {
		t.Fatalf("direct chat and time rules missing from router instructions: %s", request.Instructions)
	}
}

func TestConversationRouteModelRequestUsesUTCWhenTimeZoneMissing(t *testing.T) {
	request, err := ConversationRouteModelRequest(ConversationRouteContext{ActorID: "u1234567", Content: "现在几点？",
		ServerReceivedAt: time.Date(2026, 11, 12, 3, 4, 5, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(request.Input[0].Text), &input); err != nil {
		t.Fatal(err)
	}
	if input["reference_time"] != "2026-11-12T03:04:05Z" || input["time_zone_provided"] != false {
		t.Fatalf("missing time zone did not use explicit UTC reference: %+v", input)
	}
	if _, exists := input["time_zone"]; exists {
		t.Fatalf("missing time zone was misrepresented: %+v", input)
	}
	if _, err := ConversationRouteModelRequest(ConversationRouteContext{ActorID: "u1234567", Content: "现在几点？", TimeZone: "invalid/zone"}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("invalid route time zone accepted: %v", err)
	}
}

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

func TestExplicitParallelDelegationStartsTaskWhenRouterMisses(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", Content: "商品页支持含税价格和分类筛选。请并行委派两个互不重叠的只读子任务，一个分析客户端，另一个分析服务端/API，由两个子Agent分别返回结果。"}
	for _, output := range []string{
		`{"action":"clarify","task_id":"","question":"请说明要修改哪个任务，或是否开始一个新任务。"}`,
		`{"action":"reply","task_id":"","question":"","reply":"下面是静态建议清单。"}`,
		"not-json",
	} {
		decision := ParseConversationRouteDecisionForContext(output, value)
		if decision.Action != ConversationRouteNew || decision.TaskID != "" || decision.Question != "" || decision.Reply != "" ||
			decision.ModelOutput != output {
			t.Errorf("explicit parallel delegation did not start independent Task: %q => %+v", output, decision)
		}
	}
}

func TestExplicitParallelDelegationFallbackAvoidsQueriesAndNegation(t *testing.T) {
	for _, content := range []string{
		"解释一下“请并行委派两个子Agent”是什么意思？",
		"不要请并行委派子任务，只说明并行委派的含义。",
	} {
		value := ConversationRouteContext{ActorID: "u1234567", Content: content}
		output := `{"action":"reply","task_id":"","question":"","reply":"这是在说并行分配工作。"}`
		decision := ParseConversationRouteDecisionForContext(output, value)
		if decision.Action != ConversationRouteReply || decision.Reply == "" {
			t.Errorf("non-delegation query was turned into a Task: %q => %+v", content, decision)
		}
	}
}

func TestConversationRouteDecisionFencesDependenciesToActiveTasks(t *testing.T) {
	candidates := []ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 4}}
	decision := ParseConversationRouteDecision(`{"action":"new","task_id":"","question":"","dependency_task_ids":["q1234567"]}`, candidates)
	if decision.Action != ConversationRouteNew || len(decision.DependencyTaskIDs) != 1 || decision.DependencyTaskIDs[0] != "q1234567" {
		t.Fatalf("authorized dependency was not retained: %+v", decision)
	}
	for _, output := range []string{
		`{"action":"new","task_id":"","question":"","dependency_task_ids":["q7654321"]}`,
		`{"action":"new","task_id":"","question":"","dependency_task_ids":["q1234567","q1234567"]}`,
		`{"action":"reply","task_id":"","question":"","reply":"done","dependency_task_ids":["q1234567"]}`,
		`{"action":"stop","task_id":"q1234567","question":"","dependency_task_ids":["q1234567"]}`,
	} {
		if decision := ParseConversationRouteDecision(output, candidates); decision.Action != ConversationRouteClarify || len(decision.DependencyTaskIDs) != 0 {
			t.Errorf("invalid dependency selection was accepted: %q => %+v", output, decision)
		}
	}
	request, err := ConversationRouteModelRequest(ConversationRouteContext{ActorID: "u1234567", Content: "等前置任务完成再整理",
		Candidates: candidates})
	if err != nil || !strings.Contains(request.Instructions, "dependency_task_ids") || !strings.Contains(request.Input[0].Text, "q1234567") {
		t.Fatalf("dependency context was not presented to the router: request=%+v err=%v", request, err)
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

func TestConversationRouteRequestBindsSummaryAndUsesOnlyUncoveredHistory(t *testing.T) {
	summary := "已确认的旧结论"
	summaryDigest := sha256.Sum256([]byte(summary))
	sourceDigest := sha256.Sum256([]byte("已发布结果"))
	value := ConversationRouteContext{ActorID: "u1234567", Content: "继续", SummaryVersion: 3,
		SummarySHA256: summaryDigest[:], SummarySourcesSHA256: make([]byte, sha256.Size), SummaryText: summary,
		History: []ConversationRouteHistory{{MessageID: "m1234567", Sequence: 1, Role: conversation.RoleUser, Content: "旧消息"},
			{MessageID: "m2345678", Sequence: 2, TaskID: "q1234567", TaskRevision: 2, EventSequence: 5,
				Role: conversation.RoleAssistant, Content: "已发布结果"},
			{MessageID: "m3456789", Sequence: 3, Role: conversation.RoleUser, Content: "尾部"}}}
	request, err := ConversationRouteModelRequest(value)
	if err != nil || !strings.Contains(request.Input[0].Text, `"summary":{"sha256"`) ||
		!strings.Contains(request.Input[0].Text, summary) {
		t.Fatalf("summary was not bound into the route request: %+v err=%v", request, err)
	}
	value.SummarySHA256 = append([]byte(nil), value.SummarySHA256...)
	value.SummarySHA256[0] ^= 0xff
	if _, err := ConversationRouteModelRequest(value); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("summary with a mismatched digest was accepted: %v", err)
	}
	history := ConversationRouteHistoryWithoutSummarySources(value.History, []ConversationSummarySource{
		{Kind: ConversationSummarySourceMessage, ID: "m1234567", SHA256: make([]byte, sha256.Size)},
		{Kind: ConversationSummarySourceTask, ID: "q1234567", Version: 2, Sequence: 5, SHA256: sourceDigest[:]},
	})
	if len(history) != 1 || history[0].MessageID != "m3456789" || len(value.History) != 3 {
		t.Fatalf("summary overlap removed the wrong raw history or mutated the snapshot: %+v / %+v", history, value.History)
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
