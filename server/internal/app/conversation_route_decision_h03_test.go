package app

import (
	"bytes"
	"encoding/json"
	"testing"

	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

func safeSteerRouteContext() ConversationRouteContext {
	digest := bytes.Repeat([]byte{1}, 32)
	return ConversationRouteContext{
		ActorID: "u1234567", Content: "补充：也请把成本写进报告。", CurrentRequestModelID: "p1234567", TimeZone: "Asia/Shanghai",
		ControlScopeDigest: digest,
		Candidates:         []ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 3}},
		RequestCandidates: []ConversationRouteRequestCandidate{{
			MessageID: "m1234567", Sequence: 1, Content: "整理财报", RoutingStatus: conversation.RoutingRouted,
			TaskID: "q1234567", TaskStatus: task.StatusRunning, SameControlScope: true, SameAuthorization: true,
			ControlScopeDigest: digest, RequestInput: ConversationRoutingInput{ModelID: "p1234567", TimeZone: "Asia/Shanghai"},
		}},
	}
}

func TestConversationRouteSteerRequiresOneAuthorizedRunningTask(t *testing.T) {
	value := safeSteerRouteContext()
	request, err := ConversationRouteModelRequest(value)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains([]byte(request.Instructions), []byte("action must be update, steer, new")) ||
		!bytes.Contains([]byte(request.Instructions), []byte("same_control_scope and same_authorization")) {
		t.Fatalf("steer safety rules missing from classifier instructions: %s", request.Instructions)
	}
	var body struct {
		PriorRequests []struct {
			SameAuthorization bool `json:"same_authorization"`
		} `json:"prior_requests"`
	}
	if err := json.Unmarshal([]byte(request.Input[0].Text), &body); err != nil || len(body.PriorRequests) != 1 || !body.PriorRequests[0].SameAuthorization {
		t.Fatalf("authorization evidence missing from classifier input: %+v err=%v", body, err)
	}
	decision := ParseConversationRouteDecisionForContext(`{"action":"steer","task_id":"q1234567"}`, value)
	if decision.Action != ConversationRouteSteer || decision.TaskID != "q1234567" || decision.ExpectedRevision != 3 || decision.Acknowledge {
		t.Fatalf("safe supplement did not steer to the active Task: %+v", decision)
	}
	if decision := ParseConversationRouteDecision(`{"action":"steer","task_id":"q1234567"}`, value.Candidates); decision.Action != ConversationRouteClarify {
		t.Fatalf("context-free parser accepted a steer without scope/auth evidence: %+v", decision)
	}
}

func TestConversationRouteSteerFailsClosedOnStaleOrAmbiguousTargets(t *testing.T) {
	output := `{"action":"steer","task_id":"q1234567"}`
	for _, test := range []struct {
		name   string
		mutate func(*ConversationRouteContext)
	}{
		{name: "paused task", mutate: func(value *ConversationRouteContext) { value.Candidates[0].Status = task.StatusPaused }},
		{name: "different initiator", mutate: func(value *ConversationRouteContext) { value.Candidates[0].InitiatorID = "u7654321" }},
		{name: "different control scope", mutate: func(value *ConversationRouteContext) { value.RequestCandidates[0].SameControlScope = false }},
		{name: "different authorization", mutate: func(value *ConversationRouteContext) { value.RequestCandidates[0].SameAuthorization = false }},
		{name: "different model", mutate: func(value *ConversationRouteContext) { value.CurrentRequestModelID = "p7654321" }},
		{name: "different time zone", mutate: func(value *ConversationRouteContext) { value.TimeZone = "UTC" }},
		{name: "current attachments", mutate: func(value *ConversationRouteContext) { value.CurrentMessageHasAttachments = true }},
		{name: "different explicit task", mutate: func(value *ConversationRouteContext) { value.ReferenceTaskID = "q7654321" }},
		{name: "unavailable reference", mutate: func(value *ConversationRouteContext) { value.ReferenceUnavailable = true }},
		{name: "missing predecessor", mutate: func(value *ConversationRouteContext) { value.PreviousMissing = true }},
		{name: "incomplete Task candidate set", mutate: func(value *ConversationRouteContext) {
			value.CandidateSetIncomplete = true
			value.TaskCandidateSetIncomplete = true
		}},
		{name: "two eligible tasks", mutate: func(value *ConversationRouteContext) {
			value.Candidates = append(value.Candidates, ConversationRouteCandidate{TaskID: "q7654321", InitiatorID: value.ActorID,
				Status: task.StatusRunning, Revision: 1})
			value.RequestCandidates = append(value.RequestCandidates, ConversationRouteRequestCandidate{TaskID: "q7654321",
				TaskStatus: task.StatusRunning, SameControlScope: true, SameAuthorization: true,
				ControlScopeDigest: value.ControlScopeDigest, RequestInput: ConversationRoutingInput{ModelID: value.CurrentRequestModelID, TimeZone: value.TimeZone}})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := safeSteerRouteContext()
			test.mutate(&value)
			decision := ParseConversationRouteDecisionForContext(output, value)
			if decision.Action != ConversationRouteClarify || decision.TaskID != "" {
				t.Fatalf("unsafe steer was accepted: %+v", decision)
			}
		})
	}
}

func TestConversationRouteUsesAgentActionInsteadOfStatusKeywordOverride(t *testing.T) {
	value := safeSteerRouteContext()
	value.Content = "请查看任务进度"
	decision := ParseConversationRouteDecisionForContext(`{"action":"steer","task_id":"q1234567"}`, value)
	if decision.Action != ConversationRouteSteer || decision.TaskID != "q1234567" || decision.ExpectedRevision != 3 {
		t.Fatalf("a structurally valid Agent action was overridden by message wording: %+v", decision)
	}
}

func TestConversationRouteSteerUsesRetainedEvidenceWhenPriorRequestsWereTrimmed(t *testing.T) {
	output := `{"action":"steer","task_id":"q1234567"}`
	value := safeSteerRouteContext()
	value.CandidateSetIncomplete = true
	value.RequestCandidateSetIncomplete = true
	decision := ParseConversationRouteDecisionForContext(output, value)
	if decision.Action != ConversationRouteSteer || decision.TaskID != "q1234567" || decision.ExpectedRevision != 3 {
		t.Fatalf("complete active Task proof was rejected because unrelated prior requests were trimmed: %+v", decision)
	}

	value.Candidates = append(value.Candidates, ConversationRouteCandidate{TaskID: "q7654321", InitiatorID: value.ActorID,
		Status: task.StatusRunning, Revision: 1})
	decision = ParseConversationRouteDecisionForContext(output, value)
	if decision.Action != ConversationRouteClarify || decision.TaskID != "" {
		t.Fatalf("partial prior requests made an unproved active Task appear unambiguous: %+v", decision)
	}
}

func TestConversationRouteSteerKeepsOtherIntentsSeparate(t *testing.T) {
	value := safeSteerRouteContext()
	for _, test := range []struct {
		name, content, output string
		want                  ConversationRouteAction
	}{
		{"answer language preference", "请用中文回答", `{"action":"reply","task_id":"","reply":"好的，我会用中文回答。"}`, ConversationRouteReply},
		{"progress query", "查一下这个任务进度", `{"action":"status","task_id":"q1234567"}`, ConversationRouteStatus},
		{"plan replacement", "把报告目标改成预测明年收入", `{"action":"update","task_id":"q1234567"}`, ConversationRouteUpdate},
		{"stop", "停止这个任务", `{"action":"stop","task_id":"q1234567"}`, ConversationRouteStop},
		{"independent work", "另外查询另一家公司的收入", `{"action":"new","task_id":""}`, ConversationRouteNew},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := value
			current.Content = test.content
			decision := ParseConversationRouteDecisionForContext(test.output, current)
			if decision.Action != test.want {
				t.Fatalf("intent was routed as %s, want %s: %+v", decision.Action, test.want, decision)
			}
		})
	}
	if decision := ParseConversationRouteDecisionForContext(`{"action":"steer","task_id":"q1234567","acknowledge":true}`, value); decision.Action != ConversationRouteClarify {
		t.Fatalf("steer acknowledged background work: %+v", decision)
	}
}
