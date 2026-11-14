package app

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"morde/server/internal/domain/conversation"
)

func TestConversationRouteAggregationUsesFixed350MillisecondWindow(t *testing.T) {
	arrivals := []time.Duration{0, 120 * time.Millisecond, 280 * time.Millisecond, 420 * time.Millisecond}
	want := map[time.Duration][][]int{
		200 * time.Millisecond: {{0, 1}, {2}, {3}},
		350 * time.Millisecond: {{0, 1, 2}, {3}},
		500 * time.Millisecond: {{0, 1, 2, 3}},
	}
	for window, expected := range want {
		if got := fixedWindowGroups(arrivals, window); !reflect.DeepEqual(got, expected) {
			t.Fatalf("window %s groups = %v, want %v", window, got, expected)
		}
	}

	// A slow first classifier does not move the acceptance-time deadline. At
	// 420 ms it is still running, but the later request is no longer joinable.
	candidate := ConversationRouteRequestCandidate{MessageID: "m1234567", Sequence: 1, Content: "root",
		RoutingStatus: conversation.RoutingProcessing, SameExecutionScope: true, SameAuthorization: true,
		AggregationClosed: true}
	if ValidConversationRouteJoinTarget(candidate.MessageID, []ConversationRouteRequestCandidate{candidate}) {
		t.Fatal("420ms message joined a root whose first classification was delayed beyond the fixed window")
	}
	candidate.AggregationWaiting, candidate.SameControlScope = true, true
	if !ValidConversationRoutePendingRootStop(candidate.MessageID, []ConversationRouteRequestCandidate{candidate}) {
		t.Fatal("a taskless root stopped being cancellable solely because the aggregation window elapsed")
	}
}

func fixedWindowGroups(arrivals []time.Duration, window time.Duration) [][]int {
	if len(arrivals) == 0 {
		return [][]int{}
	}
	groups := [][]int{{0}}
	for index := 1; index < len(arrivals); index++ {
		if arrivals[index]-arrivals[0] < window {
			groups[0] = append(groups[0], index)
		} else {
			groups = append(groups, []int{index})
		}
	}
	return groups
}

func TestConversationRouteDecisionAllowsOnlyExplicitAuthorizedAggregationAndPendingStop(t *testing.T) {
	root := ConversationRouteRequestCandidate{MessageID: "m1234567", Sequence: 1, Content: "root",
		RoutingStatus: conversation.RoutingProcessing, SameExecutionScope: true, SameControlScope: true, SameAuthorization: true,
		AggregationWaiting: true}
	request := ConversationRouteContext{Content: "继续刚才这项工作", RequestCandidates: []ConversationRouteRequestCandidate{root},
		AggregationCandidates: []ConversationRouteAggregationCandidate{{MessageID: "m2345678", Sequence: 2, Content: "also include"}}}
	stop := ParseConversationRouteDecisionForContext(`{"action":"stop","task_id":"","target_message_id":"m1234567"}`, request)
	if stop.Action != ConversationRouteStop || stop.TargetMessageID != root.MessageID || stop.TaskID != "" {
		t.Fatalf("explicit pending-root stop was rejected: %+v", stop)
	}
	attachedRoot := root
	attachedRoot.SameExecutionScope = false
	request.RequestCandidates = []ConversationRouteRequestCandidate{attachedRoot}
	stop = ParseConversationRouteDecisionForContext(`{"action":"stop","task_id":"","target_message_id":"m1234567"}`, request)
	if stop.Action != ConversationRouteStop || stop.TargetMessageID != root.MessageID {
		t.Fatalf("attachment-free stop could not target a same-control-scope request: %+v", stop)
	}

	for _, candidate := range []ConversationRouteRequestCandidate{
		{MessageID: root.MessageID, Sequence: root.Sequence, Content: root.Content, RoutingStatus: root.RoutingStatus,
			SameExecutionScope: false, SameAuthorization: true, AggregationWaiting: true},
		{MessageID: root.MessageID, Sequence: root.Sequence, Content: root.Content, RoutingStatus: root.RoutingStatus,
			SameExecutionScope: true, SameAuthorization: false, AggregationWaiting: true},
		{MessageID: root.MessageID, Sequence: root.Sequence, Content: root.Content, RoutingStatus: root.RoutingStatus,
			SameExecutionScope: true, SameControlScope: false, SameAuthorization: true, AggregationWaiting: true},
		{MessageID: root.MessageID, Sequence: root.Sequence, Content: root.Content, RoutingStatus: root.RoutingStatus,
			SameExecutionScope: true, SameControlScope: true, SameAuthorization: true, AggregationWaiting: true, TaskID: "q1234567"},
	} {
		context := request
		context.RequestCandidates = []ConversationRouteRequestCandidate{candidate}
		decision := ParseConversationRouteDecisionForContext(`{"action":"stop","task_id":"","target_message_id":"m1234567"}`, context)
		if decision.Action == ConversationRouteStop {
			t.Errorf("unauthorized or task-backed pending stop was accepted: %+v", candidate)
		}
	}

	decision := ParseConversationRouteDecisionForContext(
		`{"action":"new","task_id":"","target_message_id":"","aggregate_message_ids":["m2345678"]}`, request)
	if decision.Action != ConversationRouteNew || !reflect.DeepEqual(decision.AggregateMessageIDs, []string{"m2345678"}) {
		t.Fatalf("explicit same-work supplement was not accepted: %+v", decision)
	}
	for _, output := range []string{
		`{"action":"new","task_id":"","aggregate_message_ids":["m9999999"]}`,
		`{"action":"new","task_id":"","aggregate_message_ids":["m2345678","m2345678"]}`,
		`{"action":"status","task_id":"q1234567","aggregate_message_ids":["m2345678"]}`,
		`{"action":"new","task_id":"","explicit_new":true,"aggregate_message_ids":["m2345678"]}`,
	} {
		if decision := ParseConversationRouteDecisionForContext(output, request); decision.Action == ConversationRouteNew &&
			len(decision.AggregateMessageIDs) != 0 {
			t.Errorf("invalid aggregation selection was accepted: %s => %+v", output, decision)
		}
	}

	modelRequest, err := ConversationRouteModelRequest(ConversationRouteContext{ActorID: "u1234567", Content: "root",
		AggregationCandidates: request.AggregationCandidates})
	if err != nil {
		t.Fatal(err)
	}
	var input struct {
		AggregationCandidates []ConversationRouteAggregationCandidate `json:"aggregation_candidates"`
	}
	if err := json.Unmarshal([]byte(modelRequest.Input[0].Text), &input); err != nil || !reflect.DeepEqual(input.AggregationCandidates, request.AggregationCandidates) {
		t.Fatalf("aggregation candidate context missing from model request: %+v err=%v", input, err)
	}
}

func TestConversationRouteControlScopeAllowsStoppingAttachedRequest(t *testing.T) {
	root := ConversationRoutingInput{ProjectID: "p1234567", HostID: "h1234567", WorkspaceID: "w1234567",
		ApplicationID: "app1", MCPConnectorIDs: []string{"mcp1"}, ClientAttachmentIDs: []string{"attachment1"}}
	stop := root
	stop.ClientAttachmentIDs = nil
	rootDigest, err := ConversationRouteControlScopeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	stopDigest, err := ConversationRouteControlScopeDigest(stop)
	if err != nil || string(rootDigest) != string(stopDigest) {
		t.Fatalf("attachment-free stop did not match the accepted execution destination: %x %x err=%v", rootDigest, stopDigest, err)
	}
	stop.HostID = "h7654321"
	otherDigest, err := ConversationRouteControlScopeDigest(stop)
	if err != nil || string(rootDigest) == string(otherDigest) {
		t.Fatalf("stop crossed to a different Host scope: %x %x err=%v", rootDigest, otherDigest, err)
	}
}

func TestConversationRouteModelRequestExposesOnlyAuthorizedPendingStops(t *testing.T) {
	digest := make([]byte, 32)
	root := ConversationRouteRequestCandidate{MessageID: "m1234567", Sequence: 1, Content: "root",
		RoutingStatus: conversation.RoutingProcessing, SameExecutionScope: true, SameControlScope: true,
		SameAuthorization: true, AggregationWaiting: true, ExecutionScopeDigest: digest, ControlScopeDigest: digest}
	for _, authorized := range []bool{true, false} {
		root.SameAuthorization = authorized
		request, err := ConversationRouteModelRequest(ConversationRouteContext{ActorID: "u1234567", Content: "Stop that work",
			ExecutionScopeDigest: digest, ControlScopeDigest: digest, RequestCandidates: []ConversationRouteRequestCandidate{root}})
		if err != nil {
			t.Fatal(err)
		}
		var input struct {
			PriorRequests []struct {
				AggregationWaiting bool `json:"aggregation_waiting"`
			} `json:"prior_requests"`
		}
		if err := json.Unmarshal([]byte(request.Input[0].Text), &input); err != nil || len(input.PriorRequests) != 1 ||
			input.PriorRequests[0].AggregationWaiting != authorized {
			t.Fatalf("pending stop eligibility not presented to classifier: authorized=%t input=%+v err=%v", authorized, input, err)
		}
	}
}
