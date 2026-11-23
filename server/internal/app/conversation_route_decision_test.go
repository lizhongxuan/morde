package app

import (
	"crypto/sha256"
	"encoding/hex"
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
	request, err := ConversationRouteModelRequest(ConversationRouteContext{ActorID: "u1234567", RouteAttempt: 1, Content: "现在几点？",
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
	if !strings.Contains(request.Instructions, "jokes") || !strings.Contains(request.Instructions, "reference_time and time_zone") ||
		!strings.Contains(request.Instructions, "server time is unavailable") ||
		!strings.Contains(request.Instructions, "acknowledgements") ||
		!strings.Contains(request.Instructions, "only on expected duration, never on the topic") ||
		!strings.Contains(request.Instructions, "text-only requests for small code snippets") ||
		!strings.Contains(request.Instructions, "An explicit request to create, write, export, save, or deliver a file or document") ||
		!strings.Contains(request.Instructions, "unless one bounded public read-only lookup is sufficient for the tool action") ||
		strings.Contains(request.Instructions, conversationRouteDirectToolInstructions) ||
		!strings.Contains(request.Instructions, "require a Task; do not replace requested work with a text-only answer") ||
		!strings.Contains(request.Instructions, "ask for that exact information") {
		t.Fatalf("direct chat and time rules missing from router instructions: %s", request.Instructions)
	}
}

func TestConversationRouteModelRequestKeepsMinimumWindowOutputBudget(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", Content: "你好"}
	request, err := ConversationRouteModelRequest(value)
	if err != nil {
		t.Fatal(err)
	}
	if request.MaxOutputTokens != 512 || MaxConversationRouteModelOutputTokens <= request.MaxOutputTokens {
		t.Fatalf("route output budget left the minimum-window-safe default: MaxOutputTokens=%d ceiling=%d",
			request.MaxOutputTokens, MaxConversationRouteModelOutputTokens)
	}
	review, err := ConversationRouteModelRequest(ConversationRouteContext{ActorID: "u1234567", Content: "你好",
		ResultReview: &ConversationTaskResultReview{Sources: []ConversationTaskResultSource{{
			TaskID: "q1234567", EventSequence: 1, AuthorizationVersion: 1, Objective: "do work", Content: "result"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if review.MaxOutputTokens != 512 {
		t.Fatalf("result review route output budget differs from the minimum-window default: MaxOutputTokens=%d", review.MaxOutputTokens)
	}
}

func TestConversationRouteInvalidOutputStoresOnlyMetadata(t *testing.T) {
	context := ConversationRouteContext{ActorID: "u1234567", Content: "你看看上面那条信息说了什么"}
	oversized := strings.Repeat("你", 6000)
	decision := ParseConversationRouteDecisionForContext(oversized, context)
	if !decision.InvalidOutput || decision.ModelOutput != "" || decision.InvalidOutputLengthBytes != len(oversized) || !decision.InvalidOutputUTF8Valid {
		t.Fatalf("oversized model output metadata is incorrect: %+v", decision)
	}
	secret := "synthetic-private-model-output"
	decision = ParseConversationRouteDecisionForContext(secret, context)
	encoded, err := json.Marshal(decision)
	if err != nil || !decision.InvalidOutput || decision.ModelOutput != "" || decision.InvalidOutputLengthBytes != len(secret) ||
		!decision.InvalidOutputUTF8Valid || strings.Contains(string(encoded), secret) {
		t.Fatalf("invalid model text was persisted with its metadata: decision=%+v encoded=%s err=%v", decision, encoded, err)
	}
	decision = ParseConversationRouteDecisionForContext("", context)
	if !decision.InvalidOutput || decision.ModelOutput != "" || decision.InvalidOutputLengthBytes != 0 || !decision.InvalidOutputUTF8Valid {
		t.Fatalf("empty model output metadata is incorrect: %+v", decision)
	}
	decision = ParseConversationRouteDecisionForContext(string([]byte{0xff, 0xfe})+"abc", context)
	if !decision.InvalidOutput || decision.ModelOutput != "" || decision.InvalidOutputLengthBytes != 5 || decision.InvalidOutputUTF8Valid {
		t.Fatalf("invalid UTF-8 model output metadata is incorrect: %+v", decision)
	}
}

func TestConversationRouteModelRequestAddsBoundedRepairInstructions(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", Content: "你好"}
	initial, err := ConversationRouteModelRequest(value)
	if err != nil {
		t.Fatal(err)
	}
	value.Repair = true
	repair, err := ConversationRouteModelRequest(value)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Input[0].Text != repair.Input[0].Text || strings.Contains(initial.Instructions, "previous response") ||
		!strings.Contains(repair.Instructions, "previous response to this exact request failed strict JSON or schema validation") ||
		!strings.Contains(repair.Instructions, "Do not call tools or take actions") {
		t.Fatalf("repair prompt changed source content or missed its bounded guard: initial=%+v repair=%+v", initial, repair)
	}
}

func TestConversationRouteAcknowledgementIsExplicitAndLimitedToBackgroundWork(t *testing.T) {
	context := ConversationRouteContext{ActorID: "u1234567", Content: "Please handle this request", Candidates: []ConversationRouteCandidate{{
		TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 2,
	}}}
	for _, test := range []struct {
		name, output string
		action       ConversationRouteAction
		acknowledge  bool
		invalid      bool
	}{
		{"new work acknowledged", `{"action":"new","task_id":"","acknowledge":true}`, ConversationRouteNew, true, false},
		{"new work without decision", `{"action":"new","task_id":""}`, ConversationRouteNew, false, false},
		{"update work acknowledged", `{"action":"update","task_id":"q1234567","acknowledge":true}`, ConversationRouteUpdate, true, false},
		{"quick reply cannot acknowledge", `{"action":"reply","task_id":"","reply":"Done.","acknowledge":true}`, ConversationRouteClarify, false, true},
		{"status cannot acknowledge", `{"action":"status","task_id":"q1234567","acknowledge":true}`, ConversationRouteClarify, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := ParseConversationRouteDecisionForContext(test.output, context)
			if decision.Action != test.action || decision.Acknowledge != test.acknowledge || decision.InvalidOutput != test.invalid {
				t.Fatalf("decision = %+v, want action=%s acknowledge=%t invalid=%t", decision, test.action, test.acknowledge, test.invalid)
			}
		})
	}
	completeNew := ConversationRouteContext{ActorID: "u1234567", Content: "Morde browser acceptance: long background task"}
	decision := ParseConversationRouteDecisionForContext(`{"action":"new","task_id":"","target_message_id":"","aggregate_message_ids":[],"explicit_new":false,"question":"","reply":"","dependency_task_ids":[],"acknowledge":true}`, completeNew)
	if decision.Action != ConversationRouteNew || !decision.Acknowledge || decision.InvalidOutput {
		t.Fatalf("complete acknowledged new-task decision was rejected without active candidates: %+v", decision)
	}
}

func TestTruncatedAggregateClarificationDoesNotAskToResendSavedSources(t *testing.T) {
	root, supplement := "root source", "supplement source"
	rootSHA := sha256.Sum256([]byte(root))
	supplementSHA := sha256.Sum256([]byte(supplement))
	full := "用户为同一项工作提交了以下消息，请按顺序作为一个请求处理：\n\n" + root + "\n\n--- 用户补充 ---\n\n" + supplement
	fullSHA := sha256.Sum256([]byte(full))
	value := ConversationRouteContext{ActorID: "u1234567", Content: "saved aggregate preview",
		ContentBytes: int64(len(full)), ContentSHA256: fullSHA[:], ContentTruncated: true, ContentSources: []ConversationRouteMessageSource{
			{MessageID: "m1234567", Sequence: 1, ContentBytes: int64(len(root)), ContentSHA256: rootSHA[:]},
			{MessageID: "m2345678", Sequence: 2, ContentBytes: int64(len(supplement)), ContentSHA256: supplementSHA[:]},
		}}
	decision := ParseConversationRouteDecisionForContext(`{"action":"reply","task_id":"","question":"","reply":"这是一个简单问题。"}`, value)
	if decision.Action != ConversationRouteClarify || !strings.Contains(decision.Question, "完整消息已保存") ||
		strings.Contains(decision.Question, "重新说明") || decision.ModelOutput == "" {
		t.Fatalf("aggregate preview clarification asked for unavailable input: %+v", decision)
	}
}

func TestTruncatedLongMessageTaskIntentOverridesReplyOnlyWithoutTaskAmbiguity(t *testing.T) {
	content := "好现在你帮我看下这篇文章说了什么：\n# Morde MVP：从一句需求到可验收的 PR\n" + strings.Repeat("文章正文。", 1200)
	makeTruncatedContext := func(content string) ConversationRouteContext {
		preview, _ := ConversationRouteTextPreview(content, 256)
		digest := sha256.Sum256([]byte(content))
		return ConversationRouteContext{ActorID: "u1234567", Content: preview, ContentBytes: int64(len(content)),
			ContentSHA256: digest[:], ContentTruncated: true, CurrentMessageID: "m1234567"}
	}
	for _, output := range []string{
		`{"action":"reply","task_id":"","question":"","reply":"当前请求内容只显示了有界预览。"}`,
		`{"action":"tool","tool_name":"morde_web_search","tool_arguments":{"query":"文章主题"}}`,
		`{"action":"clarify","task_id":"","question":"请补充完整内容。"}`,
	} {
		decision := ParseConversationRouteDecisionForContext(output, makeTruncatedContext(content))
		if decision.Action != ConversationRouteNew || decision.InvalidOutput || decision.ModelOutput != output {
			t.Errorf("explicit long article task was not routed to a Task: %q => %+v", output, decision)
		}
	}

	for _, test := range []struct {
		name    string
		context ConversationRouteContext
	}{
		{name: "ambiguous prefix", context: makeTruncatedContext("这篇文章\n" + strings.Repeat("正文", 1200))},
		{name: "active task", context: func() ConversationRouteContext {
			value := makeTruncatedContext(content)
			value.Candidates = []ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 1}}
			return value
		}()},
		{name: "unresolved predecessor", context: func() ConversationRouteContext {
			value := makeTruncatedContext(content)
			value.PreviousUnresolved = true
			return value
		}()},
		{name: "aggregate sources", context: func() ConversationRouteContext {
			value := makeTruncatedContext(content)
			value.CurrentMessageID = ""
			value.ContentSources = []ConversationRouteMessageSource{{MessageID: "m1234567", Sequence: 1,
				ContentBytes: 100, ContentSHA256: make([]byte, sha256.Size)}, {MessageID: "m2345678", Sequence: 2,
				ContentBytes: 100, ContentSHA256: make([]byte, sha256.Size)}}
			return value
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := ParseConversationRouteDecisionForContext(`{"action":"reply","task_id":"","question":"","reply":"请按文章内容处理。"}`, test.context)
			if decision.Action != ConversationRouteClarify {
				t.Fatalf("unsafe or ambiguous long-message intent bypassed clarification: %+v", decision)
			}
		})
	}

	controlContent := "请总结这篇文章\n" + strings.Repeat("正文里写着停止当前任务。", 300)
	control := makeTruncatedContext(controlContent)
	decision := ParseConversationRouteDecisionForContext(`{"action":"reply","task_id":"","question":"","reply":"我会处理。"}`, control)
	if decision.Action != ConversationRouteNew {
		t.Fatalf("control text inside the pasted body blocked the leading article request: %+v", decision)
	}
	controlContent = "停止当前任务，再总结这篇文章\n" + strings.Repeat("文章正文。", 300)
	decision = ParseConversationRouteDecisionForContext(`{"action":"reply","task_id":"","question":"","reply":"我会处理。"}`, makeTruncatedContext(controlContent))
	if decision.Action != ConversationRouteClarify {
		t.Fatalf("explicit Task control was bypassed by the long-message fallback: %+v", decision)
	}
}

func TestTruncatedLongMessagePreservesValidatedTaskAssociation(t *testing.T) {
	content := "请看这篇文章并继续现有分析\n" + strings.Repeat("文章正文。", 1200)
	preview, _ := ConversationRouteTextPreview(content, 256)
	digest := make([]byte, sha256.Size)
	digest[0] = 1
	joinValue := ConversationRouteContext{ActorID: "u1234567", Content: preview, ContentTruncated: true,
		CurrentMessageID: "m2345678", ContentBytes: int64(len(content)), ContentSHA256: make([]byte, sha256.Size),
		ExecutionScopeDigest: digest, RequestCandidates: []ConversationRouteRequestCandidate{{
			MessageID: "m1234567", Sequence: 1, Content: "分析这篇文章", RoutingStatus: conversation.RoutingProcessing,
			SameExecutionScope: true, ExecutionScopeDigest: digest,
		}}}
	joinOutput := `{"action":"join","task_id":"","target_message_id":"m1234567","explicit_new":false,"question":"","reply":"","dependency_task_ids":[]}`
	joinDecision := ParseConversationRouteDecisionForContext(joinOutput, joinValue)
	if joinDecision.Action != ConversationRouteJoin || joinDecision.TargetMessageID != "m1234567" {
		t.Fatalf("valid duplicate long-message association was replaced: %+v", joinDecision)
	}

	updateValue := ConversationRouteContext{ActorID: "u1234567", Content: preview, ContentTruncated: true,
		CurrentMessageID: "m3456789", ContentBytes: int64(len(content)), ContentSHA256: make([]byte, sha256.Size),
		Candidates: []ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 4}}}
	updateDecision := ParseConversationRouteDecisionForContext(`{"action":"update","task_id":"q1234567"}`, updateValue)
	if updateDecision.Action != ConversationRouteUpdate || updateDecision.TaskID != "q1234567" {
		t.Fatalf("valid task update from a long message was replaced: %+v", updateDecision)
	}
}

func TestConversationRouteModelRequestNormalizesUnresolvedCandidateState(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", Content: "帮我查一个新问题", RequestCandidates: []ConversationRouteRequestCandidate{{
		MessageID: "m1234567", Sequence: 1, Content: "前一条独立请求", RoutingStatus: conversation.RoutingProcessing,
	}}}
	processing, err := ConversationRouteModelRequest(value)
	if err != nil {
		t.Fatal(err)
	}
	value.RequestCandidates[0].RoutingStatus = conversation.RoutingPending
	pending, err := ConversationRouteModelRequest(value)
	if err != nil {
		t.Fatal(err)
	}
	if processing.Input[0].Text != pending.Input[0].Text {
		t.Fatal("pending and processing receipts produced different classification contexts")
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

func TestConversationRouteModelRequestMarksIncompleteCandidates(t *testing.T) {
	request, err := ConversationRouteModelRequest(ConversationRouteContext{ActorID: "u1234567", Content: "处理这个独立问题",
		CandidateSetIncomplete: true})
	if err != nil {
		t.Fatal(err)
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(request.Input[0].Text), &input); err != nil {
		t.Fatal(err)
	}
	if input["candidate_set_incomplete"] != true || !strings.Contains(request.Instructions, "lists are partial") ||
		!strings.Contains(request.Instructions, "Use supplied IDs only") ||
		!strings.Contains(request.Instructions, "never infer omitted Task/message IDs") ||
		!strings.Contains(request.Instructions, "history search required by incomplete history") {
		t.Fatalf("incomplete candidate boundary missing from model request: input=%+v instructions=%s", input, request.Instructions)
	}
	legacy, err := legacyConversationRouteModelRequest(ConversationRouteContext{ActorID: "u1234567", Content: "处理这个独立问题",
		CandidateSetIncomplete: true}, request)
	if err != nil {
		t.Fatal(err)
	}
	var legacyInput map[string]any
	if err := json.Unmarshal([]byte(legacy.Input[0].Text), &legacyInput); err != nil {
		t.Fatal(err)
	}
	if legacyInput["candidate_set_incomplete"] != true || !strings.Contains(legacy.Instructions, "lists are partial") ||
		!strings.Contains(legacy.Instructions, "Use supplied IDs only") {
		t.Fatalf("legacy model request lost incomplete candidate boundary: input=%+v instructions=%s", legacyInput, legacy.Instructions)
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
		{"reply with unused question", `{"action":"reply","task_id":"","question":"为什么？","reply":"你好，答案是 42。"}`, ConversationRouteReply, ""},
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
			if test.action == ConversationRouteReply && (decision.Reply != "你好，答案是 42。" || decision.Question != "" || decision.ModelOutput != test.output) {
				t.Fatalf("direct reply or original model output was not preserved: %+v", decision)
			}
		})
	}
}

func TestExplicitParallelDelegationStartsTaskWhenRouterMisses(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", Content: "商品页支持含税价格和分类筛选。请并行委派两个互不重叠的只读子任务，一个分析客户端，另一个分析服务端/API，由两个子Agent分别返回结果。",
		Candidates: []ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 1}}}
	for _, output := range []string{
		`{"action":"clarify","task_id":"","question":"请说明要修改哪个任务，或是否开始一个新任务。"}`,
		`{"action":"reply","task_id":"","question":"","reply":"下面是静态建议清单。"}`,
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

func TestExplicitSearchStartsTaskWhenRouterMisses(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", Content: "查阅 OpenAI 官方文档，说明 Structured Outputs 用来解决什么问题，并给出可打开的来源链接。"}
	for _, output := range []string{
		`{"action":"clarify","task_id":"","question":"请说明要修改哪个任务，或是否开始一个新任务。"}`,
		`{"action":"reply","task_id":"","question":"","reply":"Structured Outputs 用于约束输出格式。"}`,
	} {
		decision := ParseConversationRouteDecisionForContext(output, value)
		if decision.Action != ConversationRouteNew || decision.TaskID != "" || decision.Question != "" || decision.Reply != "" ||
			decision.ModelOutput != output {
			t.Errorf("explicit source lookup did not start independent Task: %q => %+v", output, decision)
		}
	}
}

func TestExplicitDownloadableDocumentStartsTaskWhenRouterMisses(t *testing.T) {
	for _, output := range []string{
		`{"action":"clarify","task_id":"","question":"请说明要修改哪个任务，或是否开始一个新任务。"}`,
		`{"action":"reply","task_id":"","question":"","reply":"我可以直接把设计方案写在聊天里。"}`,
	} {
		decision := ParseConversationRouteDecisionForContext(output, ConversationRouteContext{
			ActorID: "u1234567", Content: "帮助我写一个设计方案文档，并且可以下载的",
		})
		if decision.Action != ConversationRouteNew || decision.InvalidOutput || decision.ModelOutput != output {
			t.Errorf("explicit downloadable document did not start a Task: %q => %+v", output, decision)
		}
	}
	for _, content := range []string{
		"请告诉我怎么写一个 Markdown 文档",
		"不要生成文档，只用聊天文字说明",
	} {
		decision := ParseConversationRouteDecisionForContext(`{"action":"reply","task_id":"","question":"","reply":"这是说明。"}`,
			ConversationRouteContext{ActorID: "u1234567", Content: content})
		if decision.Action != ConversationRouteReply {
			t.Errorf("conversational or negative document request became a Task: %q => %+v", content, decision)
		}
	}
	value := ConversationRouteContext{ActorID: "u1234567", Content: "帮助我写一个设计方案文档，并且可以下载的",
		PreviousUnresolved: true}
	decision := ParseConversationRouteDecisionForContext(`{"action":"clarify","task_id":"","question":"先确认上一条消息"}`, value)
	if decision.Action != ConversationRouteClarify {
		t.Fatalf("file intent bypassed an unresolved predecessor: %+v", decision)
	}
	value.PreviousUnresolved = false
	value.ReferenceTaskID, value.ReferenceUnavailable = "q1234567", true
	decision = ParseConversationRouteDecisionForContext(`{"action":"clarify","task_id":"","question":"先恢复任务"}`, value)
	if decision.Action != ConversationRouteClarify {
		t.Fatalf("file intent bypassed an unavailable task reference: %+v", decision)
	}
}

func TestConversationRouteJoinRequiresAuthorizedSameScopeCandidate(t *testing.T) {
	digest := make([]byte, sha256.Size)
	digest[0] = 1
	candidate := ConversationRouteRequestCandidate{MessageID: "m1234567", Sequence: 3, Content: "分析 Morde 项目",
		RoutingStatus: conversation.RoutingProcessing, SameExecutionScope: true, ExecutionScopeDigest: digest}
	value := ConversationRouteContext{ActorID: "u1234567", RouteAttempt: 1, Content: "分析 Morde 项目", ExecutionScopeDigest: digest,
		RequestCandidates: []ConversationRouteRequestCandidate{candidate}}
	decision := ParseConversationRouteDecisionForContext(`{"action":"join","task_id":"","target_message_id":"m1234567","explicit_new":false,"question":"","reply":"","dependency_task_ids":[]}`, value)
	if decision.Action != ConversationRouteJoin || decision.TargetMessageID != candidate.MessageID || decision.InvalidOutput {
		t.Fatalf("valid same-scope join was rejected: %+v", decision)
	}
	for _, invalid := range []struct {
		name      string
		candidate ConversationRouteRequestCandidate
	}{
		{name: "another actor target", candidate: ConversationRouteRequestCandidate{MessageID: "m7654321", Sequence: 2,
			Content: "分析 Morde 项目", RoutingStatus: conversation.RoutingProcessing, SameExecutionScope: true, ExecutionScopeDigest: digest}},
		{name: "different execution scope", candidate: ConversationRouteRequestCandidate{MessageID: "m1234567", Sequence: 3,
			Content: "分析 Morde 项目", RoutingStatus: conversation.RoutingProcessing}},
		{name: "completed task", candidate: ConversationRouteRequestCandidate{MessageID: "m1234567", Sequence: 3,
			Content: "分析 Morde 项目", RoutingStatus: conversation.RoutingRouted, TaskID: "q1234567",
			TaskStatus: task.StatusCompleted, SameExecutionScope: true, ExecutionScopeDigest: digest}},
		{name: "failed route without task", candidate: ConversationRouteRequestCandidate{MessageID: "m1234567", Sequence: 3,
			Content: "分析 Morde 项目", RoutingStatus: conversation.RoutingFailed, SameExecutionScope: true, ExecutionScopeDigest: digest}},
		{name: "routed non-reply without task", candidate: ConversationRouteRequestCandidate{MessageID: "m1234567", Sequence: 3,
			Content: "分析 Morde 项目", RoutingStatus: conversation.RoutingRouted, SameExecutionScope: true, ExecutionScopeDigest: digest}},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			value.RequestCandidates = []ConversationRouteRequestCandidate{invalid.candidate}
			decision := ParseConversationRouteDecisionForContext(`{"action":"join","task_id":"","target_message_id":"m1234567","explicit_new":false,"question":"","reply":"","dependency_task_ids":[]}`, value)
			if decision.Action != ConversationRouteClarify || !decision.InvalidOutput {
				t.Fatalf("unauthorized join target was accepted: %+v", decision)
			}
		})
	}
	for _, status := range []task.Status{task.StatusQueued, task.StatusRunning} {
		candidate := ConversationRouteRequestCandidate{MessageID: "m2345678", Sequence: 4, Content: "补充同一目标的要求",
			RoutingStatus: conversation.RoutingRouted, TaskID: "q1234567", TaskStatus: status,
			SameExecutionScope: true, ExecutionScopeDigest: digest}
		if !ValidConversationRouteJoinTarget(candidate.MessageID, []ConversationRouteRequestCandidate{candidate}) {
			t.Errorf("active %s Task was not a valid merge target", status)
		}
	}
	directReply := ConversationRouteRequestCandidate{MessageID: "m3456789", Sequence: 5, Content: "分析 Morde 项目",
		RoutingStatus: conversation.RoutingRouted, SameExecutionScope: true, DirectReply: true, ExecutionScopeDigest: digest}
	if !ValidConversationRouteJoinTarget(directReply.MessageID, []ConversationRouteRequestCandidate{directReply}) {
		t.Fatal("direct-reply route was not eligible for safe reclassification and join")
	}
}

func TestConversationRouteExplicitNewIntentRequiresPositiveRerun(t *testing.T) {
	for _, test := range []struct {
		content string
		want    bool
	}{
		{content: "重新开始一个新任务", want: true},
		{content: "Start another task for the same request", want: true},
		{content: "请明确重做一遍", want: true},
		{content: "Do not start another task", want: false},
		{content: "Don't start a new task", want: false},
		{content: "继续相同目标，并补充失败处理", want: false},
		{content: "另一个目标：生成测试方案", want: false},
	} {
		t.Run(test.content, func(t *testing.T) {
			if got := ConversationRouteHasExplicitNewIntent(test.content); got != test.want {
				t.Fatalf("ConversationRouteHasExplicitNewIntent(%q)=%t, want %t", test.content, got, test.want)
			}
		})
	}
}

func TestConversationRouteExplicitRerunAndInvalidOutputStayDistinct(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", RouteAttempt: 1, Content: "请从头再做，开始一个新任务"}
	decision := ParseConversationRouteDecisionForContext(`{"action":"new","task_id":"","target_message_id":"","explicit_new":true,"question":"","reply":"","dependency_task_ids":[]}`, value)
	if decision.Action != ConversationRouteNew || !decision.ExplicitNew {
		t.Fatalf("explicit rerun was not retained: %+v", decision)
	}
	value.Content = "请继续原来的分析"
	decision = ParseConversationRouteDecisionForContext(`{"action":"new","task_id":"","target_message_id":"","explicit_new":true,"question":"","reply":"","dependency_task_ids":[]}`, value)
	if !decision.InvalidOutput {
		t.Fatalf("unsupported explicit_new flag was treated as user ambiguity: %+v", decision)
	}
	value.Content = "换一个目标，帮我设计 Morde 的发布流程"
	decision = ParseConversationRouteDecisionForContext(`{"action":"new","task_id":"","target_message_id":"","explicit_new":false,"question":"","reply":"","dependency_task_ids":[]}`, value)
	if decision.Action != ConversationRouteNew || decision.ExplicitNew || decision.InvalidOutput {
		t.Fatalf("independent target was confused with an explicit rerun: %+v", decision)
	}
	decision = ParseConversationRouteDecisionForContext("not-json", value)
	if !decision.InvalidOutput {
		t.Fatalf("malformed model output was treated as valid clarification: %+v", decision)
	}
}

func TestConversationRouteExecutionScopeIgnoresOnlyCausalPredecessor(t *testing.T) {
	first := ConversationRoutingInput{ProjectID: "pr123456", HostID: "h1234567", PreviousClientMessageID: "prior-001",
		ClientAttachmentIDs: []string{"pa_0123456789abcdef0123456789abcdef"}}
	second := first
	second.PreviousClientMessageID = "prior-002"
	firstDigest, err := ConversationRouteExecutionScopeDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := ConversationRouteExecutionScopeDigest(second)
	if err != nil || string(firstDigest) != string(secondDigest) {
		t.Fatalf("causal predecessor changed execution scope: %x %x err=%v", firstDigest, secondDigest, err)
	}
	second.HostID = "h7654321"
	otherDigest, err := ConversationRouteExecutionScopeDigest(second)
	if err != nil || string(firstDigest) == string(otherDigest) {
		t.Fatalf("different execution target shared scope digest: %x %x err=%v", firstDigest, otherDigest, err)
	}
}

func TestExplicitSearchFallbackAvoidsQuestionsNegationAndActiveTasks(t *testing.T) {
	for _, content := range []string{
		"搜索算法是什么？",
		"请不要搜索 OpenAI 文档，只解释 Structured Outputs。",
		"Explain what the word search means.",
	} {
		value := ConversationRouteContext{ActorID: "u1234567", Content: content}
		output := `{"action":"reply","task_id":"","question":"","reply":"这是一个解释性问题。"}`
		decision := ParseConversationRouteDecisionForContext(output, value)
		if decision.Action != ConversationRouteReply || decision.Reply == "" {
			t.Errorf("query or negation was turned into a Task: %q => %+v", content, decision)
		}
	}
	value := ConversationRouteContext{ActorID: "u1234567", Content: "查阅 OpenAI 官方文档", Candidates: []ConversationRouteCandidate{{
		TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 1,
	}}}
	decision := ParseConversationRouteDecisionForContext(`{"action":"clarify","task_id":"","question":"请说明要修改哪个任务，或是否开始一个新任务。"}`, value)
	if decision.Action != ConversationRouteClarify {
		t.Fatalf("search fallback overrode active-task ambiguity: %+v", decision)
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

func TestConversationRouteDirectToolIsExplicitAndReadOnly(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", Content: "请搜索公开网站关于日本央行利率政策的最新信息"}
	for _, test := range []struct {
		name   string
		output string
	}{
		{"search", `{"action":"tool","tool_name":"morde_web_search","tool_arguments":{"query":"日本央行利率政策"}}`},
		{"read", `{"action":"tool","tool_name":"morde_web_read","tool_arguments":{"url":"https://example.com/article"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := ParseConversationRouteDecisionForContext(test.output, value)
			if decision.Action != ConversationRouteTool || decision.ToolName == "" ||
				!ValidConversationRouteDirectToolCall(decision.ToolName, json.RawMessage(decision.ToolArguments)) || decision.ModelOutput != test.output {
				t.Fatalf("valid direct read-only tool call was rejected: %+v", decision)
			}
		})
	}
	request, err := ConversationRouteModelRequest(value)
	if err != nil || !strings.Contains(request.Instructions, `"tool_name"`) ||
		!strings.Contains(request.Instructions, `"morde_web_search"`) || !strings.Contains(request.Instructions, `action "tool"`) {
		t.Fatalf("direct tool contract missing from model request: err=%v instructions=%s", err, request.Instructions)
	}
	truncatedDigest := sha256.Sum256([]byte(value.Content))
	for _, restricted := range []struct {
		name    string
		context ConversationRouteContext
	}{
		{"ordinary chat", ConversationRouteContext{ActorID: value.ActorID, Content: "你好"}},
		{"task reference", ConversationRouteContext{ActorID: value.ActorID, Content: value.Content, ReferenceTaskID: "q1234567"}},
		{"attachments", ConversationRouteContext{ActorID: value.ActorID, Content: value.Content, CurrentMessageHasAttachments: true}},
		{"unresolved predecessor", ConversationRouteContext{ActorID: value.ActorID, Content: value.Content, PreviousUnresolved: true}},
		{"missing predecessor", ConversationRouteContext{ActorID: value.ActorID, Content: value.Content, PreviousMissing: true, PreviousUnresolved: true}},
		{"unavailable reference", ConversationRouteContext{ActorID: value.ActorID, Content: value.Content, ReferenceTaskID: "q1234567", ReferenceUnavailable: true}},
		{"truncated message", ConversationRouteContext{ActorID: value.ActorID, Content: value.Content, CurrentMessageID: "m1234567",
			ContentBytes: int64(len(value.Content)), ContentSHA256: truncatedDigest[:], ContentTruncated: true}},
		{"task control", ConversationRouteContext{ActorID: value.ActorID, Content: value.Content + "，顺便停止当前任务"}},
	} {
		t.Run("instruction omitted: "+restricted.name, func(t *testing.T) {
			request, err := ConversationRouteModelRequest(restricted.context)
			if err != nil {
				t.Fatalf("build route request: %v", err)
			}
			if strings.Contains(request.Instructions, conversationRouteDirectToolInstructions) {
				t.Fatalf("ineligible request received the direct tool contract: %s", request.Instructions)
			}
		})
	}
	legacyRequest, err := legacyConversationRouteModelRequest(value, request)
	if err != nil || !strings.Contains(legacyRequest.Instructions, conversationRouteDirectToolInstructions) {
		t.Fatalf("legacy model request lost the bounded direct tool contract: err=%v instructions=%s", err, legacyRequest.Instructions)
	}

	for _, output := range []string{
		`{"action":"tool","tool_name":"morde_personal_update","tool_arguments":{"field":"name"}}`,
		`{"action":"tool","tool_name":"morde_web_search","tool_arguments":{"query":"news","unexpected":true}}`,
		`{"action":"tool","tool_name":"morde_web_search","tool_arguments":{"query":"news"},"reply":"invented"}`,
		`{"action":"tool","tool_name":"morde_web_search","tool_arguments":{"query":"news"},"acknowledge":true}`,
	} {
		if decision := ParseConversationRouteDecisionForContext(output, value); decision.Action != ConversationRouteClarify {
			t.Errorf("unsafe direct tool output was accepted: %s => %+v", output, decision)
		}
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
	if decision.Action != ConversationRouteClarify || decision.Question != "我无法确认你要操作哪项任务。请指定任务，或重新描述操作。" || decision.ModelOutput == "" {
		t.Fatalf("reply used unavailable predecessor context or lost source output: %+v", decision)
	}
}

func TestConversationRouteAcceptsIndependentReplyWithoutPredecessor(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", Content: "2+2 等于多少？", PreviousMissing: true, PreviousUnresolved: true}
	request, err := ConversationRouteModelRequest(value)
	if err != nil || !strings.Contains(request.Instructions, conversationRouteUnresolvedReplyInstructions) {
		t.Fatalf("independence instructions unavailable: %v", err)
	}
	decision := ParseConversationRouteDecisionForContext(`{"action":"reply","task_id":"","question":"","reply":"答案是 4。"}`, value)
	if decision.Action != ConversationRouteReply || decision.Reply != "答案是 4。" || decision.TaskID != "" {
		t.Fatalf("independent conversation answer was blocked: %+v", decision)
	}
	value.Content = "把上面的内容缩短"
	decision = ParseConversationRouteDecisionForContext(`{"action":"clarify","task_id":"","question":"请提供需要缩短的原文。"}`, value)
	if decision.Action != ConversationRouteClarify || decision.Question != "请提供需要缩短的原文。" {
		t.Fatalf("specific clarification was lost: %+v", decision)
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
		!request.DisableThinking || !request.JSONMode || request.Temperature == nil || *request.Temperature != 0 ||
		!strings.Contains(request.Instructions, conversationRouteJSONModeExample) ||
		!strings.Contains(request.Instructions, conversationRouteReversibleChoiceInstructions) ||
		!strings.Contains(request.Instructions, `"acknowledge" is the only acceptance signal`) ||
		!strings.Contains(request.Input[0].Text, "只看收入和净利润") || !strings.Contains(request.Input[0].Text, "q1234567") ||
		strings.Contains(request.Input[0].Text, "u1234567") {
		t.Fatalf("routing request is invalid: %+v err=%v", request, err)
	}
	var historyInput struct {
		History []struct {
			MessageID     string `json:"message_id"`
			Content       string `json:"content"`
			ContentBytes  int64  `json:"content_bytes"`
			ContentSHA256 string `json:"content_sha256"`
		} `json:"history"`
	}
	if err := json.Unmarshal([]byte(request.Input[0].Text), &historyInput); err != nil {
		t.Fatal(err)
	}
	historyDigest := sha256.Sum256([]byte("整理财报"))
	if len(historyInput.History) != 1 || historyInput.History[0].MessageID != "m1234567" ||
		historyInput.History[0].Content != "整理财报" || historyInput.History[0].ContentBytes != int64(len("整理财报")) ||
		historyInput.History[0].ContentSHA256 != hex.EncodeToString(historyDigest[:]) {
		t.Fatalf("history source provenance was not passed to the classifier: %+v", historyInput.History)
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
	value.CurrentMessageID = "m1234567"
	value.Content = strings.Repeat("中", maxAgentInputBytes)
	request, err = ConversationRouteModelRequest(value)
	var largeInput struct {
		MessageID        string `json:"message_id"`
		MessageBytes     int64  `json:"message_bytes"`
		MessageTruncated bool   `json:"message_truncated"`
	}
	if err == nil {
		err = json.Unmarshal([]byte(request.Input[0].Text), &largeInput)
	}
	if err != nil || !largeInput.MessageTruncated || largeInput.MessageID != value.CurrentMessageID || largeInput.MessageBytes != int64(len(value.Content)) {
		t.Fatalf("oversized routing context was not converted to a verifiable source reference: input=%+v err=%v", largeInput, err)
	}
}

func TestConversationRouteHistoryPreviewCarriesOriginalSourceWithoutFullContent(t *testing.T) {
	original := strings.Repeat("早期要求：保留合同中的期限、金额和付款条件。", 120)
	digest := sha256.Sum256([]byte(original))
	preview, truncated := ConversationRouteTextPreview(original, 128)
	if !truncated {
		t.Fatal("synthetic history was not truncated")
	}
	value := ConversationRouteContext{ActorID: "u1234567", Content: "继续处理", History: []ConversationRouteHistory{{
		MessageID: "m1234567", Sequence: 1, Role: conversation.RoleUser, Content: preview,
		ContentBytes: int64(len(original)), SourceSHA256: digest[:], Truncated: true,
	}}}
	request, err := ConversationRouteModelRequest(value)
	if err != nil {
		t.Fatal(err)
	}
	var input struct {
		HistoryIncomplete bool `json:"history_incomplete"`
		History           []struct {
			MessageID     string `json:"message_id"`
			Content       string `json:"content"`
			ContentBytes  int64  `json:"content_bytes"`
			ContentSHA256 string `json:"content_sha256"`
			Truncated     bool   `json:"content_truncated"`
		} `json:"history"`
	}
	if err := json.Unmarshal([]byte(request.Input[0].Text), &input); err != nil {
		t.Fatal(err)
	}
	if !input.HistoryIncomplete || len(input.History) != 1 || input.History[0].MessageID != "m1234567" ||
		input.History[0].Content != preview || input.History[0].ContentBytes != int64(len(original)) ||
		input.History[0].ContentSHA256 != hex.EncodeToString(digest[:]) || !input.History[0].Truncated ||
		strings.Contains(request.Input[0].Text, original) || !strings.Contains(request.Instructions, "morde_current_conversation_history_search") {
		t.Fatalf("history preview lost its source integrity or exposed full text: input=%+v instructions=%q", input, request.Instructions)
	}
}

func TestConversationRouteHistoryRejectsUnverifiableTruncation(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", Content: "继续", History: []ConversationRouteHistory{{
		MessageID: "m1234567", Sequence: 1, Role: conversation.RoleUser, Content: "仅预览", Truncated: true,
	}}}
	if _, err := ConversationRouteModelRequest(value); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("truncated history without original digest was accepted: %v", err)
	}
	value.History[0] = ConversationRouteHistory{MessageID: "m1234567", Sequence: 1, Role: conversation.RoleUser,
		Content: "完整内容", ContentBytes: int64(len("完整内容")), SourceSHA256: make([]byte, sha256.Size)}
	if _, err := ConversationRouteModelRequest(value); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("history whose source hash does not match content was accepted: %v", err)
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
		!strings.Contains(request.Input[0].Text, `"sources_sha256"`) ||
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
	if decision := ParseConversationRouteDecisionForContext(`{"action":"new","task_id":"","question":""}`, value); decision.Action != ConversationRouteClarify || decision.Question != "我无法确认你要操作哪项任务。请指定任务，或重新描述操作。" {
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
