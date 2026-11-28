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
	if !strings.Contains(request.Instructions, "jokes") || !strings.Contains(request.Instructions, "reference_time/time_zone") ||
		!strings.Contains(request.Instructions, "absent time means unavailable") ||
		!strings.Contains(request.Instructions, "file_delivery_intent") ||
		!strings.Contains(request.Instructions, "acknowledgements") ||
		!strings.Contains(request.Instructions, "only on expected duration, never on the topic") ||
		!strings.Contains(request.Instructions, "small code") ||
		!strings.Contains(request.Instructions, "File/document create, write, export, save or delivery") ||
		!strings.Contains(request.Instructions, "public web search/page reading") ||
		!strings.Contains(request.Instructions, conversationRouteDirectToolInstructions) ||
		!strings.Contains(request.Instructions, "delegation requires a Task") ||
		!strings.Contains(request.Instructions, "smallest exact question") {
		t.Fatalf("direct chat and time rules missing from router instructions: %s", request.Instructions)
	}
	if !strings.Contains(request.Instructions, `{"action":"new","file_delivery_intent":"none","acknowledge":true}`) ||
		!strings.Contains(request.Instructions, `{"action":"tool","tool_name":"morde_web_search"`) ||
		strings.Contains(request.Instructions, `choose action "new" so the DSH Task Agent`) ||
		!strings.Contains(request.Instructions, "Include only fields relevant to the selected action") {
		t.Fatalf("JSON examples do not match the action-scoped tool schema: %s", request.Instructions)
	}
}

func TestConversationRouteSearchIntentUsesDSHTaskAgent(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", Content: "搜索 DeepSeek 官方网站并核实链接。"}
	output := `{"action":"tool","tool_name":"morde_web_search","tool_arguments":{"query":"DeepSeek 官方网站"}}`
	decision := ParseConversationRouteDecisionForContext(output, value)
	if decision.InvalidOutput || decision.Action != ConversationRouteNew || !decision.Acknowledge || decision.ToolName != "" || decision.ToolArguments != "" ||
		decision.AgentToolProfile != task.AgentToolProfileWebResearch {
		t.Fatalf("valid search intent was not assigned to the DSH Task Agent: %+v", decision)
	}
	if !ValidConversationRouteAgentToolProfile(decision, value) {
		t.Fatal("validated search route did not retain its bounded DSH tool profile")
	}
	forged := decision
	forged.ModelOutput = `{"action":"new","task_id":"","acknowledge":true}`
	if ValidConversationRouteAgentToolProfile(forged, value) {
		t.Fatal("a web research profile not backed by a route search/read intent was accepted")
	}
	quotedArguments := `{"action":"tool","tool_name":"morde_web_search","tool_arguments":"{\"query\":\"DeepSeek 官方网站\"}"}`
	decision = ParseConversationRouteDecisionForContext(quotedArguments, value)
	if !decision.InvalidOutput || decision.InvalidOutputCategory != "action_fields_invalid" {
		t.Fatalf("quoted tool arguments did not fail with a safe structural category: %+v", decision)
	}
}

func TestConversationRouteNewsPromptUsesValidatedResearchProfileMarker(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", Content: "搜索今天的科技新闻，给我三条，每条附来源链接。"}
	request, err := ConversationRouteModelRequest(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`return action "tool"`,
		`"action":"tool","tool_name":"morde_web_search"`,
		`category="news"`,
		`time_range`,
		`The server validates the marker and creates a DSH Task restricted to those read-only tools`,
	} {
		if !strings.Contains(request.Instructions, required) {
			t.Fatalf("news route instructions omitted %q: %s", required, request.Instructions)
		}
	}
	if strings.Contains(request.Instructions, `choose action "new" so the DSH Task Agent`) {
		t.Fatalf("news route instructions still direct the model away from the profile trigger: %s", request.Instructions)
	}

	output := `{"action":"tool","tool_name":"morde_web_search","tool_arguments":{"query":"today technology news","category":"news","time_range":"day"}}`
	decision := ParseConversationRouteDecisionForContext(output, value)
	if decision.InvalidOutput || decision.Action != ConversationRouteNew || !decision.Acknowledge ||
		decision.AgentToolProfile != task.AgentToolProfileWebResearch || !ValidConversationRouteAgentToolProfile(decision, value) {
		t.Fatalf("validated news marker did not create a profile-bound DSH Task: %+v", decision)
	}
}

func TestConversationRouteFileDeliveryIntentIsStructuredAndActionScoped(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", Content: "请生成 PDF 文件并提供下载。",
		Candidates: []ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 4}}}
	for _, intent := range []string{"", "none", "required", "unclear"} {
		for _, test := range []struct {
			name   string
			output string
			action ConversationRouteAction
			taskID string
		}{
			{name: "new", output: `{"action":"new","task_id":"","file_delivery_intent":"` + intent + `"}`, action: ConversationRouteNew},
			{name: "update", output: `{"action":"update","task_id":"q1234567","file_delivery_intent":"` + intent + `"}`, action: ConversationRouteUpdate, taskID: "q1234567"},
		} {
			t.Run(test.name+"/"+intent, func(t *testing.T) {
				decision := ParseConversationRouteDecisionForContext(test.output, value)
				if decision.InvalidOutput || decision.Action != test.action || decision.TaskID != test.taskID ||
					string(decision.FileDeliveryIntent) != intent || decision.ModelOutput != test.output {
					t.Fatalf("structured file intent was not retained: got=%+v output=%s", decision, test.output)
				}
				if test.action == ConversationRouteUpdate && decision.ExpectedRevision != 4 {
					t.Fatalf("update file intent was not bound to the authorized revision: %+v", decision)
				}
			})
		}
	}
	for _, output := range []string{
		`{"action":"new","file_delivery_intent":"maybe"}`,
		`{"action":"new","file_delivery_intent":null}`,
		`{"action":"update","task_id":"q1234567","file_delivery_intent":1}`,
		`{"action":"reply","task_id":"","question":"","reply":"Done.","file_delivery_intent":"required"}`,
	} {
		decision := ParseConversationRouteDecisionForContext(output, value)
		if !decision.InvalidOutput || decision.Action == ConversationRouteNew || decision.Action == ConversationRouteUpdate {
			t.Errorf("invalid or action-incompatible file intent was accepted: output=%s decision=%+v", output, decision)
		}
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
	for _, required := range []string{
		"source positions explicitly present",
		"Never invent a source position",
		"not read or could not be located",
		"never expose message IDs, source hashes, checkpoint markers, internal tool names, or internal status/error codes",
		"what remains unconfirmed, and one safe next step",
	} {
		if !strings.Contains(review.Instructions, required) {
			t.Errorf("result review instructions omit safe delivery rule %q", required)
		}
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
		!decision.InvalidOutputUTF8Valid || decision.InvalidOutputCategory != "invalid_json" || strings.Contains(string(encoded), secret) {
		t.Fatalf("invalid model text was persisted with its metadata: decision=%+v encoded=%s err=%v", decision, encoded, err)
	}
	decision = ParseConversationRouteDecisionForContext("", context)
	if !decision.InvalidOutput || decision.ModelOutput != "" || decision.InvalidOutputLengthBytes != 0 || !decision.InvalidOutputUTF8Valid ||
		decision.InvalidOutputCategory != "empty_output" {
		t.Fatalf("empty model output metadata is incorrect: %+v", decision)
	}
	decision = ParseConversationRouteDecisionForContext(string([]byte{0xff, 0xfe})+"abc", context)
	if !decision.InvalidOutput || decision.ModelOutput != "" || decision.InvalidOutputLengthBytes != 5 || decision.InvalidOutputUTF8Valid ||
		decision.InvalidOutputCategory != "output_size_or_encoding" {
		t.Fatalf("invalid UTF-8 model output metadata is incorrect: %+v", decision)
	}
	for _, test := range []struct{ output, category string }{
		{`{"action":"reply","mystery":"private content"}`, "unknown_json_field"},
		{`{"action":7}`, "json_field_type"},
		{`{"action":"reply","reply":"你好。"} {}`, "trailing_json_data"},
	} {
		got := ParseConversationRouteDecisionForContext(test.output, context)
		if !got.InvalidOutput || got.InvalidOutputCategory != test.category || strings.Contains(got.ModelOutput, "private content") {
			t.Errorf("structural failure category = %+v, want %q", got, test.category)
		}
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

func TestConversationRouteModelRequestsUseConsistentJSONFieldContract(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", Content: "分析这份报告"}
	request, err := ConversationRouteModelRequest(value)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := legacyConversationRouteModelRequest(value, request)
	if err != nil {
		t.Fatal(err)
	}
	fieldList := `keys "action", "task_id", "target_message_id", "aggregate_message_ids", "explicit_new", "question", "reply", "dependency_task_ids", "acknowledge", "file_delivery_intent", "tool_name", "tool_arguments"`
	for _, prompt := range []struct {
		name         string
		instructions string
		actionRule   string
	}{
		{name: "primary", instructions: request.Instructions,
			actionRule: "Actions: reply (direct self-contained conversation), new (independent work), update (clear change to one identified Task), join"},
		{name: "legacy", instructions: legacy.Instructions,
			actionRule: "Actions: reply (direct, self-contained conversation), new (independent work), update (clear change to one identified Task), steer"},
	} {
		t.Run(prompt.name, func(t *testing.T) {
			for _, required := range []string{
				fieldList,
				prompt.actionRule,
				`Always include "acknowledge" as a JSON boolean`,
				`It may be true only for new, update, or join actions`,
				`"acknowledge" is the only acceptance signal`,
				`For action "new" or "update", include the string field "file_delivery_intent"`,
				`including Mermaid diagrams or flowcharts shown inline in the chat`,
				`Use "required" only when the user requests a file to be delivered, downloaded, exported, or saved, or a Workspace artifact`,
				`Empty string means no explicit override`,
				conversationRouteExplicitNewInstructions,
				`For eligible public web search/read, use action "tool" with tool_name and tool_arguments`,
				`Actions:`,
			} {
				if !strings.Contains(prompt.instructions, required) {
					t.Errorf("%s route prompt omitted %q: %s", prompt.name, required, prompt.instructions)
				}
			}
		})
	}
}

func TestConversationRouteExplicitNewDistinguishesDuplicateReplayFromRerun(t *testing.T) {
	const original = "请分析这份报告并列出重点。"
	scopeDigest := sha256.Sum256([]byte("authorized execution scope"))
	prior := ConversationRouteRequestCandidate{MessageID: "m1234567", Sequence: 1, Content: original,
		RoutingStatus: conversation.RoutingRouted, SameExecutionScope: true, SameAuthorization: true, DirectReply: true,
		ExecutionScopeDigest: scopeDigest[:]}
	replayContext := ConversationRouteContext{ActorID: "u1234567", Content: original, ExecutionScopeDigest: scopeDigest[:],
		RequestCandidates: []ConversationRouteRequestCandidate{prior}}
	request, err := ConversationRouteModelRequest(replayContext)
	if err != nil {
		t.Fatal(err)
	}
	var input struct {
		PriorRequests []conversationRouteModelRequestCandidate `json:"prior_requests"`
	}
	if err := json.Unmarshal([]byte(request.Input[0].Text), &input); err != nil {
		t.Fatal(err)
	}
	if len(input.PriorRequests) != 1 || !input.PriorRequests[0].Joinable {
		t.Fatalf("exact same-scope replay was not exposed as an idempotent candidate: %+v", input.PriorRequests)
	}

	for _, test := range []struct {
		name          string
		content       string
		modelOutput   string
		wantExplicit  bool
		wantDuplicate bool
	}{
		{name: "identical replay defaults to idempotent association", content: original,
			modelOutput: `{"action":"new","explicit_new":false}`, wantDuplicate: true},
		{name: "clear rerun intent remains a fresh task", content: "请把之前的同一项分析重新执行为一个新任务。",
			modelOutput: `{"action":"new","explicit_new":true}`, wantExplicit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ValidConversationRouteExactDuplicateTarget(prior.MessageID, test.content, []ConversationRouteRequestCandidate{prior}); got != test.wantDuplicate {
				t.Fatalf("exact duplicate eligibility = %t, want %t", got, test.wantDuplicate)
			}
			value := replayContext
			value.Content = test.content
			decision := ParseConversationRouteDecisionForContext(test.modelOutput, value)
			if decision.Action != ConversationRouteNew || decision.InvalidOutput || decision.ExplicitNew != test.wantExplicit {
				t.Fatalf("structured explicit_new decision was not preserved: %+v", decision)
			}
		})
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

func TestTruncatedLongMessageModelDecisionStillHonorsStructuralClarification(t *testing.T) {
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
		if decision.Action != ConversationRouteClarify || decision.InvalidOutput || decision.ModelOutput != output {
			t.Errorf("incomplete message did not remain a safe clarification: %q => %+v", output, decision)
		}
	}

	for _, test := range []struct {
		name    string
		context ConversationRouteContext
	}{
		{name: "ambiguous prefix", context: makeTruncatedContext("这篇文章\n" + strings.Repeat("正文", 1200))},
		{name: "explicit task reference", context: func() ConversationRouteContext {
			value := makeTruncatedContext(content)
			value.Candidates = []ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 1}}
			value.ReferenceTaskID = "q1234567"
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
	if decision.Action != ConversationRouteClarify {
		t.Fatalf("incomplete request was converted into a task action: %+v", decision)
	}
	controlContent = "停止当前任务，再总结这篇文章\n" + strings.Repeat("文章正文。", 300)
	decision = ParseConversationRouteDecisionForContext(`{"action":"reply","task_id":"","question":"","reply":"我会处理。"}`, makeTruncatedContext(controlContent))
	if decision.Action != ConversationRouteClarify {
		t.Fatalf("truncated Task control did not remain a clarification: %+v", decision)
	}
}

func TestTruncatedLongArticlePreservesModelDecisionAndDefersDuplicateHandling(t *testing.T) {
	body := "\n# 长文章\n" + strings.Repeat("文章内容。", 1000)
	content := "感动你终于不报错了.好现在你帮我看下这篇文章说了什么：" + body
	preview, _ := ConversationRouteTextPreview(content, 2048)
	digest := sha256.Sum256([]byte(content))
	value := ConversationRouteContext{ActorID: "u1234567", Content: preview, ContentTruncated: true,
		CurrentMessageID: "m2345678", ContentBytes: int64(len(content)), ContentSHA256: digest[:],
		Candidates: []ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusWaitingUser, Revision: 1}}}
	for _, test := range []struct {
		output string
		want   ConversationRouteAction
	}{
		{`{"action":"reply","reply":"这是文章摘要。"}`, ConversationRouteClarify},
		{`{"action":"clarify","question":"请补充全文。"}`, ConversationRouteClarify},
		{`{"action":"new"}`, ConversationRouteNew},
	} {
		decision := ParseConversationRouteDecisionForContext(test.output, value)
		if decision.Action != test.want || decision.ModelOutput != test.output {
			t.Fatalf("model decision changed beyond the incomplete-context guard: output=%s decision=%+v", test.output, decision)
		}
	}
	for _, prefix := range []string{"继续根据这篇文章修改任务", "请看这篇文章并继续现有分析", "请总结这篇文章并更新上一个任务", "不要总结这篇文章，改做别的", "请总结这篇文章并发邮件"} {
		ambiguous := value
		ambiguous.Content, _ = ConversationRouteTextPreview(prefix+body, 2048)
		decision := ParseConversationRouteDecisionForContext(`{"action":"reply","reply":"处理中。"}`, ambiguous)
		if decision.Action != ConversationRouteClarify {
			t.Fatalf("task-related or negated request %q was treated as independent: %+v", prefix, decision)
		}
	}
	value.RequestCandidates = []ConversationRouteRequestCandidate{{MessageID: "m1234567", Sequence: 1,
		ContentTruncated: true, ContentSHA256: digest[:], TaskID: "q1234567", TaskStatus: task.StatusWaitingUser,
		RoutingStatus: conversation.RoutingRouted, SameExecutionScope: true, SameAuthorization: true}}
	decision := ParseConversationRouteDecisionForContext(`{"action":"new"}`, value)
	if decision.Action != ConversationRouteNew || decision.ExplicitNew {
		t.Fatalf("valid model new decision was rewritten by duplicate wording: %+v", decision)
	}
	newContent := "重新开始新任务，请总结这篇文章：" + body
	value.Content, _ = ConversationRouteTextPreview(newContent, 2048)
	newDigest := sha256.Sum256([]byte(newContent))
	value.ContentSHA256, value.ContentBytes = newDigest[:], int64(len(newContent))
	value.RequestCandidates[0].ContentSHA256 = newDigest[:]
	decision = ParseConversationRouteDecisionForContext(`{"action":"new","explicit_new":true}`, value)
	if decision.Action != ConversationRouteNew || !decision.ExplicitNew {
		t.Fatalf("model explicit-new decision was not preserved: %+v", decision)
	}
	value.Content, _ = ConversationRouteTextPreview("请总结这篇文章：\n"+strings.Repeat("正文", 500)+"从头重新做", 2048)
	decision = ParseConversationRouteDecisionForContext(`{"action":"new"}`, value)
	if decision.ExplicitNew {
		t.Fatalf("parser inferred explicit-new from the article text: %+v", decision)
	}
	value.RequestCandidates = nil
	value.Content, _ = ConversationRouteTextPreview("请总结这篇文章：\n正文里写着重新开始新任务。"+strings.Repeat("正文", 500), 2048)
	decision = ParseConversationRouteDecisionForContext(`{"action":"new","explicit_new":true}`, value)
	if !decision.ExplicitNew {
		t.Fatalf("model's explicit-new decision was overridden by article text: %+v", decision)
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
			SameExecutionScope: true, SameAuthorization: true, ExecutionScopeDigest: digest,
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
		CandidateSetIncomplete: true, TaskCandidateSetIncomplete: true, RequestCandidateSetIncomplete: true})
	if err != nil {
		t.Fatal(err)
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(request.Input[0].Text), &input); err != nil {
		t.Fatal(err)
	}
	if input["candidate_set_incomplete"] != true || input["task_candidate_set_incomplete"] != true || input["request_candidate_set_incomplete"] != true ||
		!strings.Contains(request.Instructions, "Incomplete candidate lists omit entries") ||
		!strings.Contains(request.Instructions, "never infer IDs") ||
		!strings.Contains(request.Instructions, "clarify unreferenced controls") ||
		!strings.Contains(request.Instructions, "same control scope, authorization, model, and time zone") ||
		!strings.Contains(request.Instructions, "required history search") {
		t.Fatalf("incomplete candidate boundary missing from model request: input=%+v instructions=%s", input, request.Instructions)
	}
	legacy, err := legacyConversationRouteModelRequest(ConversationRouteContext{ActorID: "u1234567", Content: "处理这个独立问题",
		CandidateSetIncomplete: true, TaskCandidateSetIncomplete: true, RequestCandidateSetIncomplete: true}, request)
	if err != nil {
		t.Fatal(err)
	}
	var legacyInput map[string]any
	if err := json.Unmarshal([]byte(legacy.Input[0].Text), &legacyInput); err != nil {
		t.Fatal(err)
	}
	if legacyInput["candidate_set_incomplete"] != true || legacyInput["task_candidate_set_incomplete"] != true ||
		legacyInput["request_candidate_set_incomplete"] != true ||
		!strings.Contains(legacy.Instructions, "Incomplete candidate lists omit entries") ||
		!strings.Contains(legacy.Instructions, "never infer IDs") {
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

func TestExplicitParallelDelegationUsesStructuredRouteDecision(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", Content: "独立的新工作：商品页支持含税价格和分类筛选。请并行委派两个互不重叠的只读子任务，一个分析客户端，另一个分析服务端/API，由两个子Agent分别返回结果。",
		Candidates: []ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 1}}}
	for _, test := range []struct {
		name   string
		output string
		want   ConversationRouteAction
	}{
		{name: "agent selects background work", output: `{"action":"new","task_id":"","question":"","reply":"","dependency_task_ids":[]}`, want: ConversationRouteNew},
		{name: "agent asks for clarification", output: `{"action":"clarify","task_id":"","question":"请先确认两个子任务的边界。"}`, want: ConversationRouteClarify},
		{name: "agent chooses a conversational response", output: `{"action":"reply","task_id":"","question":"","reply":"下面是静态建议清单。"}`, want: ConversationRouteReply},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := ParseConversationRouteDecisionForContext(test.output, value)
			if decision.Action != test.want || decision.ModelOutput != test.output {
				t.Fatalf("delegation wording changed the structured decision: got=%+v want=%s", decision, test.want)
			}
			if test.want == ConversationRouteNew && (decision.TaskID != "" || decision.Question != "" || decision.Reply != "") {
				t.Fatalf("valid New decision acquired fields from message text: %+v", decision)
			}
		})
	}
	if decision := ParseConversationRouteDecisionForContext("not-json", value); !decision.InvalidOutput || decision.Action == ConversationRouteNew {
		t.Fatalf("invalid Agent output was converted to a Task by delegation wording: %+v", decision)
	}
	for _, content := range []string{
		"独立商品页需求：请安排两个 Agent 分别检查客户端和服务端。",
		"请把商品页的含税价格与分类筛选分给两个互不重叠的分析步骤。",
	} {
		decision := ParseConversationRouteDecisionForContext(`{"action":"new","task_id":"","question":"","reply":"","dependency_task_ids":[]}`,
			ConversationRouteContext{ActorID: "u1234567", Content: content})
		if decision.Action != ConversationRouteNew {
			t.Errorf("same structured New decision changed for delegation rephrase %q: %+v", content, decision)
		}
	}
}

func TestParallelDelegationReplyIsPreservedForQueriesAndNegation(t *testing.T) {
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

func TestExplicitSearchUsesStructuredRouteDecision(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", Content: "查阅 OpenAI 官方文档，说明 Structured Outputs 用来解决什么问题，并给出可打开的来源链接。"}
	for _, test := range []struct {
		name   string
		output string
		want   ConversationRouteAction
	}{
		{name: "agent selects a Task for research", output: `{"action":"new","task_id":"","question":"","reply":"","dependency_task_ids":[]}`, want: ConversationRouteNew},
		{name: "agent answers conversationally", output: `{"action":"reply","task_id":"","question":"","reply":"Structured Outputs 用于约束输出格式。"}`, want: ConversationRouteReply},
		{name: "agent asks for clarification", output: `{"action":"clarify","task_id":"","question":"你希望我只查官方文档，还是还要比较其他来源？"}`, want: ConversationRouteClarify},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := ParseConversationRouteDecisionForContext(test.output, value)
			if decision.Action != test.want || decision.InvalidOutput || decision.ModelOutput != test.output {
				t.Fatalf("search wording changed the structured decision: got=%+v want=%s", decision, test.want)
			}
		})
	}
	for _, content := range []string{
		"查阅 OpenAI 官方文档，说明 Structured Outputs 用来解决什么问题，并给出可打开的来源链接。",
		"能否帮我找 OpenAI 官方关于 Structured Outputs 的说明，解释用途并附来源？",
		"OpenAI 官方文档里 Structured Outputs 的作用是什么，请核对并给我来源链接。",
	} {
		decision := ParseConversationRouteDecisionForContext(`{"action":"new","task_id":"","question":"","reply":"","dependency_task_ids":[]}`,
			ConversationRouteContext{ActorID: "u1234567", Content: content})
		if decision.Action != ConversationRouteNew {
			t.Errorf("same structured New decision changed for research rephrase %q: %+v", content, decision)
		}
	}
}

func TestExplicitDownloadableDocumentUsesStructuredRouteDecision(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", Content: "帮助我写一个设计方案文档，并且可以下载的"}
	for _, test := range []struct {
		name   string
		output string
		want   ConversationRouteAction
	}{
		{name: "Agent selects a Task", output: `{"action":"new","task_id":"","question":"","reply":"","dependency_task_ids":[]}`, want: ConversationRouteNew},
		{name: "Agent chooses a conversational reply", output: `{"action":"reply","task_id":"","question":"","reply":"我可以直接把设计方案写在聊天里。"}`, want: ConversationRouteReply},
		{name: "Agent clarifies", output: `{"action":"clarify","task_id":"","question":"请确认文档格式和下载位置。"}`, want: ConversationRouteClarify},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := ParseConversationRouteDecisionForContext(test.output, value)
			if decision.Action != test.want || decision.InvalidOutput || decision.ModelOutput != test.output {
				t.Fatalf("file-delivery wording changed the structured decision: got=%+v want=%s", decision, test.want)
			}
		})
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
	value.PreviousUnresolved = true
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
		RoutingStatus: conversation.RoutingProcessing, SameExecutionScope: true, SameAuthorization: true, ExecutionScopeDigest: digest}
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
		{name: "different authorization", candidate: ConversationRouteRequestCandidate{MessageID: "m1234567", Sequence: 3,
			Content: "分析 Morde 项目", RoutingStatus: conversation.RoutingProcessing, SameExecutionScope: true,
			ExecutionScopeDigest: digest}},
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
			SameExecutionScope: true, SameAuthorization: true, ExecutionScopeDigest: digest}
		if !ValidConversationRouteJoinTarget(candidate.MessageID, []ConversationRouteRequestCandidate{candidate}) {
			t.Errorf("active %s Task was not a valid merge target", status)
		}
	}
	directReply := ConversationRouteRequestCandidate{MessageID: "m3456789", Sequence: 5, Content: "分析 Morde 项目",
		RoutingStatus: conversation.RoutingRouted, SameExecutionScope: true, SameAuthorization: true, DirectReply: true, ExecutionScopeDigest: digest}
	if !ValidConversationRouteJoinTarget(directReply.MessageID, []ConversationRouteRequestCandidate{directReply}) {
		t.Fatal("direct-reply route was not eligible for safe reclassification and join")
	}
}

func TestConversationRouteModelActionIsPreservedAcrossSemanticRewrites(t *testing.T) {
	candidate := ConversationRouteCandidate{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 2}
	for _, test := range []struct {
		name    string
		content string
		output  string
		want    ConversationRouteAction
	}{
		{name: "reply with task vocabulary", content: "请搜索后停止当前任务，再重新开始新任务并把工作目录换到 /workspace/new。", output: `{"action":"reply","reply":"我会直接回答。"}`, want: ConversationRouteReply},
		{name: "new without restart phrase", content: "请按需求完整完成这项独立工作。", output: `{"action":"new","explicit_new":false,"dependency_task_ids":[]}`, want: ConversationRouteNew},
		{name: "explicit new without keyword", content: "请继续原来的分析。", output: `{"action":"new","explicit_new":true,"dependency_task_ids":[]}`, want: ConversationRouteNew},
		{name: "status paraphrase", content: "这件事目前到哪一步了？", output: `{"action":"status","task_id":"q1234567"}`, want: ConversationRouteStatus},
		{name: "stop paraphrase", content: "把这项工作结束掉。", output: `{"action":"stop","task_id":"q1234567"}`, want: ConversationRouteStop},
		{name: "stop all paraphrase", content: "把还没有结束的工作全部撤掉。", output: `{"action":"stop_all"}`, want: ConversationRouteStopAll},
		{name: "tool paraphrase", content: "请帮我查找日本央行利率政策的公开网页", output: `{"action":"tool","tool_name":"morde_web_search","tool_arguments":{"query":"日本央行利率政策"}}`, want: ConversationRouteNew},
		{name: "news routes to multi-step task", content: "帮我搜索今天的 AI 新闻", output: `{"action":"new","explicit_new":false,"dependency_task_ids":[]}`, want: ConversationRouteNew},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := ConversationRouteContext{ActorID: "u1234567", Content: test.content, Candidates: []ConversationRouteCandidate{candidate}}
			decision := ParseConversationRouteDecisionForContext(test.output, value)
			if decision.Action != test.want || decision.InvalidOutput {
				t.Fatalf("semantic wording rewrote the structured action: action=%s want=%s decision=%+v", decision.Action, test.want, decision)
			}
			if test.name == "explicit new without keyword" && !decision.ExplicitNew {
				t.Fatalf("model explicit_new bit was not preserved: %+v", decision)
			}
			if test.name == "new without restart phrase" && decision.ExplicitNew {
				t.Fatalf("message wording changed the model explicit_new bit: %+v", decision)
			}
		})
	}

	toolOutput := `{"action":"tool","tool_name":"morde_web_search","tool_arguments":{"query":"日本央行利率政策"}}`
	for _, content := range []string{
		"请搜索公开网站关于日本央行利率政策的最新信息。",
		"能否帮我找一下日本央行利率政策的最新公开资料？",
		"有没有关于日本央行利率政策的最新公开信息？",
	} {
		decision := ParseConversationRouteDecisionForContext(toolOutput, ConversationRouteContext{ActorID: "u1234567", Content: content})
		if decision.Action != ConversationRouteNew {
			t.Errorf("lookup intent did not reach DSH Task Agent for semantic rephrase %q: %+v", content, decision)
		}
	}
}

func TestConversationRouteTaskKeywordsDoNotOverrideModelReply(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", Content: "请帮我修复一个独立的服务端任务误分类问题", Candidates: []ConversationRouteCandidate{{
		TaskID: "q1234567", InitiatorID: "u1234567", Objective: "an unrelated running task", Status: task.StatusRunning, Revision: 2,
	}}}
	decision := ParseConversationRouteDecisionForContext(`{"action":"reply","task_id":"","question":"","reply":"我会直接回答。"}`, value)
	if decision.Action != ConversationRouteReply || decision.Reply != "我会直接回答。" || decision.InvalidOutput {
		t.Fatalf("task-work wording overrode the model reply: %+v", decision)
	}

	value.ReferenceTaskID = "q1234567"
	decision = ParseConversationRouteDecisionForContext(`{"action":"reply","task_id":"","question":"","reply":"我会直接回答。"}`, value)
	if decision.Action != ConversationRouteReply || decision.Reply != "我会直接回答。" {
		t.Fatalf("task reference wording overrode the model reply: %+v", decision)
	}
	value.ReferenceTaskID = ""
	value.Candidates = nil
	value.PreviousTaskID = "q1234567"
	value.Content = "请开发一个验收工具并修改项目文件"
	decision = ParseConversationRouteDecisionForContext(`{"action":"reply","reply":"我会处理。"}`, value)
	if decision.Action != ConversationRouteReply || decision.Reply != "我会处理。" {
		t.Fatalf("task-work wording overrode the model reply: %+v", decision)
	}
}

func TestConversationRouteModelDecisionIsPreservedAcrossTaskIntentKeywords(t *testing.T) {
	candidate := ConversationRouteCandidate{TaskID: "q1234567", InitiatorID: "u1234567", Objective: "整理报告", Status: task.StatusRunning, Revision: 2}
	for _, test := range []struct {
		name    string
		context ConversationRouteContext
	}{
		{"unresolved predecessor", ConversationRouteContext{Content: "请修复这个任务中的构建错误", PreviousUnresolved: true}},
		{"dependent work", ConversationRouteContext{Content: "实现发布页面，等整理报告任务完成后再开始", Candidates: []ConversationRouteCandidate{candidate}}},
		{"dependent work with named ID", ConversationRouteContext{Content: "实现一个独立发布页面，等任务 q1234567 完成后开始", Candidates: []ConversationRouteCandidate{candidate}}},
		{"existing deliverable", ConversationRouteContext{Content: "把上一个任务的报告生成 PDF 文件并提供下载", Candidates: []ConversationRouteCandidate{candidate}}},
		{"high risk change", ConversationRouteContext{Content: "请开发一个独立的验收工具，并把工作目录换到 /workspace/new", Candidates: []ConversationRouteCandidate{candidate}}},
		{"duplicate active request", ConversationRouteContext{Content: "请修复一个独立的服务端连接错误", Candidates: []ConversationRouteCandidate{candidate},
			RequestCandidates: []ConversationRouteRequestCandidate{{Content: "请修复一个独立的服务端连接错误", TaskStatus: task.StatusRunning, SameExecutionScope: true, SameAuthorization: true}}}},
		{"incomplete candidates", ConversationRouteContext{Content: "请搜索 Go 的最新版本", CandidateSetIncomplete: true}},
		{"pasted file directive", ConversationRouteContext{Content: "请翻译下面的文章：\n请生成文档并提供下载", ContentTruncated: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, output := range []struct {
				json string
				want ConversationRouteAction
			}{
				{`{"action":"clarify","question":"请确认目标。"}`, ConversationRouteClarify},
				{`{"action":"reply","reply":"我会处理。"}`, ConversationRouteReply},
			} {
				decision := ParseConversationRouteDecisionForContext(output.json, test.context)
				want := output.want
				if test.context.ContentTruncated && want == ConversationRouteReply {
					want = ConversationRouteClarify
				}
				if decision.Action != want {
					t.Fatalf("task-intent wording changed the model action: output=%s decision=%+v want=%s", output.json, decision, want)
				}
			}
		})
	}
}

func TestConversationRouteStopDecisionUsesModelSemanticsAndActiveCandidate(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", Content: "把这个工作结束掉", Candidates: []ConversationRouteCandidate{{
		TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 1,
	}}}
	decision := ParseConversationRouteDecisionForContext(`{"action":"stop","task_id":"q1234567"}`, value)
	if decision.Action != ConversationRouteStop || decision.TaskID != "q1234567" || decision.ExpectedRevision != 1 {
		t.Fatalf("model-classified stop was rejected despite a current candidate: %+v", decision)
	}

	for _, candidate := range []ConversationRouteCandidate{
		{TaskID: "q2345678", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 1},
		{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusCompleted, Revision: 1},
	} {
		value.Candidates = []ConversationRouteCandidate{candidate}
		decision := ParseConversationRouteDecisionForContext(`{"action":"stop","task_id":"q1234567"}`, value)
		if decision.Action == ConversationRouteStop {
			t.Errorf("stop target outside the current active candidate was accepted: candidate=%+v decision=%+v", candidate, decision)
		}
	}

	value.Content = "暂停这个任务"
	value.Candidates = []ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: value.ActorID, Status: task.StatusRunning, Revision: 1}}
	decision = ParseConversationRouteDecisionForContext(`{"action":"clarify","question":"请说明你希望暂停还是停止。"}`, value)
	if decision.Action != ConversationRouteClarify {
		t.Fatalf("model clarification was replaced with a task action: %+v", decision)
	}
}

func TestConversationRouteStopAllUsesModelSemanticsAndStructuralGuards(t *testing.T) {
	const output = `{"action":"stop_all","task_id":"","target_message_id":"","aggregate_message_ids":[],"explicit_new":false,"question":"","reply":"","dependency_task_ids":[]}`
	value := ConversationRouteContext{ActorID: "u1234567", Content: "把还没有结束的工作全部撤掉"}
	decision := ParseConversationRouteDecisionForContext(output, value)
	if decision.Action != ConversationRouteStopAll || decision.InvalidOutput || decision.ModelOutput != output {
		t.Fatalf("semantic stop-all decision was rejected because its wording did not match keywords: %+v", decision)
	}
	incomplete := value
	incomplete.CandidateSetIncomplete = true
	incomplete.TaskCandidateSetIncomplete = true
	incomplete.RequestCandidateSetIncomplete = true
	if !ValidConversationRouteStopDecision(incomplete, decision) {
		t.Fatal("explicit model stop-all was rejected because its bounded candidate list was incomplete")
	}

	for _, test := range []struct {
		name    string
		context ConversationRouteContext
	}{
		{name: "empty message", context: ConversationRouteContext{ActorID: value.ActorID}},
		{name: "truncated message", context: ConversationRouteContext{ActorID: value.ActorID, Content: value.Content, ContentTruncated: true}},
		{name: "attached message", context: ConversationRouteContext{ActorID: value.ActorID, Content: value.Content, CurrentMessageHasAttachments: true}},
		{name: "result review", context: ConversationRouteContext{ActorID: value.ActorID, Content: value.Content, ResultReview: &ConversationTaskResultReview{}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := ParseConversationRouteDecisionForContext(output, test.context)
			if decision.Action != ConversationRouteClarify || !strings.Contains(decision.Question, "没有停止任何任务") {
				t.Fatalf("unsafe context did not fail closed: %+v", decision)
			}
		})
	}

	for _, test := range []struct {
		name    string
		content string
		output  string
		want    ConversationRouteAction
	}{
		{name: "quoted phrase is not an automatic stop", content: "“stop all tasks”是什么意思？", output: `{"action":"reply","task_id":"","reply":"这是询问停止全部任务的含义。"}`, want: ConversationRouteReply},
		{name: "negated request", content: "不要停止所有任务，只告诉我当前状态。", output: `{"action":"reply","task_id":"","reply":"我会说明当前状态。"}`, want: ConversationRouteReply},
		{name: "model asks for clarification", content: "把还没有结束的工作全部撤掉", output: `{"action":"clarify","task_id":"","question":"你希望停止哪些工作？"}`, want: ConversationRouteClarify},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := ParseConversationRouteDecisionForContext(test.output, ConversationRouteContext{ActorID: value.ActorID, Content: test.content})
			if decision.Action != test.want || decision.Action == ConversationRouteStopAll {
				t.Fatalf("quoted or negated text triggered an automatic stop action: %+v", decision)
			}
		})
	}

	for _, malformed := range []string{
		`{"action":"stop_all","task_id":"q1234567"}`,
		`{"action":"stop_all","target_message_id":"m1234567"}`,
		`{"action":"stop_all","tool_name":"morde_web_search"}`,
		`{"action":"stop_all","reply":"我正在停止全部任务。"}`,
	} {
		decision := ParseConversationRouteDecisionForContext(malformed, value)
		if decision.Action == ConversationRouteStopAll {
			t.Errorf("structurally invalid stop-all action was accepted: output=%s decision=%+v", malformed, decision)
		}
	}
}

func TestConversationRoutePreservesModelExplicitNewAndSeparatesInvalidOutput(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", RouteAttempt: 1, Content: "请从头再做，开始一个新任务"}
	decision := ParseConversationRouteDecisionForContext(`{"action":"new","task_id":"","target_message_id":"","explicit_new":true,"question":"","reply":"","dependency_task_ids":[]}`, value)
	if decision.Action != ConversationRouteNew || !decision.ExplicitNew {
		t.Fatalf("explicit rerun was not retained: %+v", decision)
	}
	value.Content = "请继续原来的分析"
	decision = ParseConversationRouteDecisionForContext(`{"action":"new","task_id":"","target_message_id":"","explicit_new":true,"question":"","reply":"","dependency_task_ids":[]}`, value)
	if decision.Action != ConversationRouteNew || !decision.ExplicitNew || decision.InvalidOutput {
		t.Fatalf("model explicit-new decision was overridden by wording: %+v", decision)
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

func TestSearchReplyAndClarificationFollowStructuredDecision(t *testing.T) {
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
		t.Fatalf("structured clarification for an ambiguous active-task target was changed: %+v", decision)
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

func TestConversationRouteWebLookupUsesTaskAgentAndSynthesis(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", Content: "请搜索公开网站关于日本央行利率政策的说明页"}
	for _, test := range []struct {
		name   string
		output string
	}{
		{"search", `{"action":"tool","tool_name":"morde_web_search","tool_arguments":{"query":"日本央行利率政策"}}`},
		{"read", `{"action":"tool","tool_name":"morde_web_read","tool_arguments":{"url":"https://example.com/article"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := ParseConversationRouteDecisionForContext(test.output, value)
			if decision.Action != ConversationRouteNew || !decision.Acknowledge || decision.ToolName != "" ||
				decision.ToolArguments != "" || decision.ModelOutput != test.output {
				t.Fatalf("valid lookup intent was not handed to the DSH Task Agent: %+v", decision)
			}
		})
	}
	request, err := ConversationRouteModelRequest(value)
	if err != nil || !strings.Contains(request.Instructions, `"tool_name"`) ||
		!strings.Contains(request.Instructions, `return action "tool"`) ||
		!strings.Contains(request.Instructions, `"action":"tool","tool_name":"morde_web_search"`) ||
		strings.Contains(request.Instructions, `choose action "new" so the DSH Task Agent`) ||
		!strings.Contains(request.Instructions, `DSH Task Agent`) ||
		strings.Contains(request.Instructions, `one-call search`) {
		t.Fatalf("DSH lookup handoff contract missing or direct shortcut still advertised: err=%v instructions=%s", err, request.Instructions)
	}
	if ValidConversationRouteDirectToolCall(cloudDSHWebSearchToolName,
		json.RawMessage(`{"query":"AI news","category":"news","time_range":"day"}`)) {
		t.Fatal("news search was allowed onto the one-call path that cannot read and verify source pages")
	}
	newsAsDirectTool := ParseConversationRouteDecisionForContext(
		`{"action":"tool","tool_name":"morde_web_search","tool_arguments":{"query":"AI news","category":"news","time_range":"day"}}`,
		ConversationRouteContext{ActorID: value.ActorID, Content: "帮我搜索今天的 AI 新闻"})
	if newsAsDirectTool.Action != ConversationRouteNew {
		t.Fatalf("structured news search was not promoted to a source-verifying Task: %+v", newsAsDirectTool)
	}
	publisherLookupArgs := json.RawMessage(`{"query":"\"Example article\" Publisher","lookup_purpose":"publisher_verification"}`)
	if ValidConversationRouteDirectToolCall(cloudDSHWebSearchToolName, publisherLookupArgs) {
		t.Fatal("publisher verification was allowed onto the one-call path without a Task Agent")
	}
	publisherLookup := ParseConversationRouteDecisionForContext(
		`{"action":"tool","tool_name":"morde_web_search","tool_arguments":{"query":"\"Example article\" Publisher","lookup_purpose":"publisher_verification"}}`,
		ConversationRouteContext{ActorID: value.ActorID, Content: "核实这条新闻的发布来源"})
	if publisherLookup.Action != ConversationRouteNew {
		t.Fatalf("structured publisher verification was not promoted to a Task: %+v", publisherLookup)
	}
	mixedControl := value
	mixedControl.Content += "，顺便停止当前任务"
	decision := ParseConversationRouteDecisionForContext(`{"action":"tool","tool_name":"morde_web_search","tool_arguments":{"query":"日本央行利率政策"}}`, mixedControl)
	if decision.Action != ConversationRouteNew || !decision.Acknowledge {
		t.Fatalf("valid lookup intent was not delegated to DSH Task Agent: %+v", decision)
	}
	truncatedDigest := sha256.Sum256([]byte(value.Content))
	for _, restricted := range []struct {
		name             string
		context          ConversationRouteContext
		toolInstructions bool
	}{
		{"ordinary chat", ConversationRouteContext{ActorID: value.ActorID, Content: "你好"}, true},
		{"task reference", ConversationRouteContext{ActorID: value.ActorID, Content: value.Content, ReferenceTaskID: "q1234567"}, false},
		{"attachments", ConversationRouteContext{ActorID: value.ActorID, Content: value.Content, CurrentMessageHasAttachments: true}, false},
		{"unresolved predecessor", ConversationRouteContext{ActorID: value.ActorID, Content: value.Content, PreviousUnresolved: true}, false},
		{"missing predecessor", ConversationRouteContext{ActorID: value.ActorID, Content: value.Content, PreviousMissing: true, PreviousUnresolved: true}, false},
		{"unavailable reference", ConversationRouteContext{ActorID: value.ActorID, Content: value.Content, ReferenceTaskID: "q1234567", ReferenceUnavailable: true}, false},
		{"truncated message", ConversationRouteContext{ActorID: value.ActorID, Content: value.Content, CurrentMessageID: "m1234567",
			ContentBytes: int64(len(value.Content)), ContentSHA256: truncatedDigest[:], ContentTruncated: true}, false},
		{"incomplete candidates", ConversationRouteContext{ActorID: value.ActorID, Content: value.Content, CandidateSetIncomplete: true}, false},
		{"incomplete history", ConversationRouteContext{ActorID: value.ActorID, Content: value.Content, HistoryIncomplete: true}, false},
		{"task control wording", ConversationRouteContext{ActorID: value.ActorID, Content: value.Content + "，顺便停止当前任务"}, true},
		{"unfamiliar search phrasing", ConversationRouteContext{ActorID: value.ActorID, Content: "有没有关于日本央行利率政策的最新公开信息？"}, true},
	} {
		t.Run("instruction omitted: "+restricted.name, func(t *testing.T) {
			request, err := ConversationRouteModelRequest(restricted.context)
			if err != nil {
				t.Fatalf("build route request: %v", err)
			}
			got := strings.Contains(request.Instructions, conversationRouteDirectToolInstructions)
			if got != restricted.toolInstructions {
				t.Fatalf("direct tool contract eligibility = %t, want %t: %s", got, restricted.toolInstructions, request.Instructions)
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
	decision := ParseConversationRouteDecisionForContext(`{"action":"reply","task_id":"","question":"","reply":"我无法确认该任务，因此没有发送停止请求。"}`, value)
	if decision.Action != ConversationRouteReply || decision.TaskID != "" || decision.Reply == "" || decision.ModelOutput == "" {
		t.Fatalf("model's non-action reply was rewritten by a task-control keyword: %+v", decision)
	}
	value.ReferenceUnavailable = false
	value.ReferenceTaskID = ""
	value.PreviousMissing = true
	value.PreviousUnresolved = true
	value.Content = "2+2 等于多少？"
	decision = ParseConversationRouteDecisionForContext(`{"action":"reply","task_id":"","question":"","reply":"答案是 4。"}`, value)
	if decision.Action != ConversationRouteReply || decision.Reply != "答案是 4。" || decision.ModelOutput == "" {
		t.Fatalf("independent reply was blocked by a missing predecessor: %+v", decision)
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
	for _, test := range []struct {
		action  string
		content string
	}{
		{action: "update", content: value.Content},
		{action: "status", content: value.Content},
		{action: "stop", content: "停止任务"},
	} {
		controlContext := value
		controlContext.Content = test.content
		decision := ParseConversationRouteDecisionForContext(`{"action":"`+test.action+`","task_id":"q1234567","question":""}`, controlContext)
		if decision.Action != ConversationRouteClarify || decision.Question != UnavailableRouteReferenceMessage || decision.TaskID != "" {
			t.Fatalf("unavailable structured control was not clarified: action=%s decision=%+v", test.action, decision)
		}
	}
	if decision := ParseConversationRouteDecisionForContext(`{"action":"new","task_id":"","question":""}`, value); decision.Action != ConversationRouteNew {
		t.Fatalf("new work was not allowed: %+v", decision)
	}
	for _, content := range []string{"停止任务", "请帮我暂停当前任务", "stop this task", "what is the task status?"} {
		value.Content = content
		decision := ParseConversationRouteDecisionForContext(`{"action":"clarify","task_id":"","question":"请明确任务操作范围。"}`, value)
		if decision.Action != ConversationRouteClarify || decision.TaskID != "" {
			t.Fatalf("model clarification for an unavailable reference was not preserved: content=%q decision=%+v", content, decision)
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
	if decision := ParseConversationRouteDecisionForContext(`{"action":"clarify","task_id":"","question":"请补充任务上下文。"}`, value); decision.Action != ConversationRouteClarify || decision.Question != "请补充任务上下文。" {
		t.Fatalf("model clarification for missing predecessor was not preserved: %+v", decision)
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
