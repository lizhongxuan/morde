package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"

	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

func TestConversationRouteFirstAttemptIncludesSteerSafetyContext(t *testing.T) {
	claim, value := routeGatewayFixture()
	claim.RouteAttempt, value.RouteAttempt = 0, 0
	claim.Input.ReferenceTaskID, value.ReferenceTaskID = "q1234567", "q1234567"
	claim.Input.ModelID, value.CurrentRequestModelID = "p1234567", "p1234567"
	claim.Input.TimeZone = "Asia/Shanghai"
	claim.Message.Content, value.Content = "Also include a short introduction", "Also include a short introduction"
	value.TimeZone = "Asia/Shanghai"
	value.Candidates = []ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: value.ActorID,
		Status: task.StatusRunning, Revision: 3}}
	scope := make([]byte, sha256.Size)
	scope[0] = 1
	value.ControlScopeDigest = scope
	value.RequestCandidates = []ConversationRouteRequestCandidate{{MessageID: "m7654321", Sequence: 1,
		Content: "Create a short report", RoutingStatus: conversation.RoutingRouted, TaskID: "q1234567",
		TaskStatus: task.StatusRunning, SameControlScope: true, ControlScopeDigest: scope, SameAuthorization: true,
		RequestInput: ConversationRoutingInput{ModelID: "p1234567", TimeZone: "Asia/Shanghai"}}}
	store := &routeGatewayStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}
	client := &modelGatewayClientTest{response: ModelResponse{Text: `{"action":"steer","task_id":"q1234567","acknowledge":false}`,
		StopReason: "stop", Usage: ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}}}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{personal: true}, client)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := service.ClassifyConversationRoute(context.Background(), claim, value)
	if err != nil || decision.Action != ConversationRouteSteer || decision.TaskID != "q1234567" || client.calls != 1 ||
		!strings.Contains(client.request.Instructions, conversationRouteInstructions) ||
		strings.Contains(client.request.Instructions, legacyConversationRouteInstructions) {
		t.Fatalf("first route attempt did not classify with safe steer context: decision=%+v request=%+v err=%v",
			decision, client.request, err)
	}
	var request struct {
		PriorRequests []conversationRouteModelRequestCandidate `json:"prior_requests"`
	}
	if err := json.Unmarshal([]byte(client.request.Input[0].Text), &request); err != nil || len(request.PriorRequests) != 1 ||
		!request.PriorRequests[0].SameControlScope || !request.PriorRequests[0].SameAuthorization {
		t.Fatalf("first route attempt omitted steer scope/authorization evidence: prior=%+v err=%v input=%s",
			request.PriorRequests, err, client.request.Input[0].Text)
	}
}
