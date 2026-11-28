package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

func TestCloudDSHCapacityWaitSurvivesBudgetDenialPipeFailure(t *testing.T) {
	writeErr := errors.New("runtime pipe closed")
	got := cloudDSHModelBudgetPlanDenialError(app.ErrTaskModelBudgetCapacityWaiting, writeErr)
	if !errors.Is(got, errCloudDSHModelBudgetCapacityWaiting) || errors.Is(got, errCloudDSHModelResultUnavailable) {
		t.Fatalf("capacity wait with failed denial write = %v; want capacity wait only", got)
	}

	otherErr := errors.New("quota exhausted")
	got = cloudDSHModelBudgetPlanDenialError(otherErr, writeErr)
	if !errors.Is(got, writeErr) || errors.Is(got, errCloudDSHModelBudgetCapacityWaiting) {
		t.Fatalf("non-capacity denial with failed write = %v; want original pipe error and no wait sentinel", got)
	}
}

type cloudDSHModelBudgetPlannerStub struct {
	plan CloudDSHModelBudgetPlan
}

func (stub cloudDSHModelBudgetPlannerStub) PrepareModelBudget(context.Context, CloudDSHModelBudgetPlanRequest) (CloudDSHModelBudgetPlan, error) {
	return stub.plan, nil
}

func (cloudDSHModelBudgetPlannerStub) SupportsTaskModelBudgetAdmission(context.Context, task.Task) (bool, error) {
	return true, nil
}

type cloudDSHModelBudgetPreparationServiceStub struct {
	request app.TaskModelBudgetPrepareRequest
	plan    app.TaskModelBudgetPlan
}

func (stub *cloudDSHModelBudgetPreparationServiceStub) PrepareTaskModelBudget(_ context.Context, request app.TaskModelBudgetPrepareRequest) (app.TaskModelBudgetPlan, error) {
	stub.request = request
	return stub.plan, nil
}

func (*cloudDSHModelBudgetPreparationServiceStub) SupportsTaskModelBudgetAdmission(context.Context, task.Task) (bool, error) {
	return true, nil
}

func TestCloudDSHModelBudgetAdmissionIsOptIn(t *testing.T) {
	start := map[string]any{"type": "start"}
	configureCloudDSHModelBudgetAdmission(start, nil, false, 2048)
	if _, exists := start["model_budget_admission"]; exists {
		t.Fatal("default start frame enabled model budget admission")
	}
	if maxTokens, ok := start["max_tokens"].(int64); !ok || maxTokens != 2048 {
		t.Fatalf("default start frame changed its legacy max_tokens: %#v", start["max_tokens"])
	}

	optInStart := map[string]any{"type": "start"}
	configureCloudDSHModelBudgetAdmission(optInStart, cloudDSHModelBudgetPlannerStub{plan: CloudDSHModelBudgetPlan{PlanID: "plan_1", MaxTokens: 64}}, true, 2048)
	if enabled, ok := optInStart["model_budget_admission"].(bool); !ok || !enabled {
		t.Fatal("planner-backed start frame did not opt into model budget admission")
	}
	if _, exists := optInStart["max_tokens"]; exists {
		t.Fatal("planner-backed start frame retained the legacy output cap")
	}
}

func TestCloudDSHModelBudgetAdmissionKeepsUnlistedTaskOnLegacyCap(t *testing.T) {
	start := map[string]any{"type": "start"}
	configureCloudDSHModelBudgetAdmission(start, cloudDSHModelBudgetPlannerStub{}, false, 2048)
	if _, exists := start["model_budget_admission"]; exists {
		t.Fatal("unlisted task route enabled model budget admission")
	}
	if maxTokens, ok := start["max_tokens"].(int64); !ok || maxTokens != 2048 {
		t.Fatalf("unlisted task route did not retain its legacy cap: %#v", start["max_tokens"])
	}
}

func TestCloudDSHModelBudgetDigestMatchesJSONStringifyProjection(t *testing.T) {
	canonical, ok := cloudDSHCanonicalJSONStringify([]byte(`{"z":"<&","a":1e-6,"nested":{"b":2,"a":-0}}`))
	if !ok || string(canonical) != `{"a":0.000001,"nested":{"a":0,"b":2},"z":"<&"}` {
		t.Fatalf("canonical JSON = %s, ok=%v", canonical, ok)
	}

	first := []byte(`{"provider":"morde","model":"m1234567","messages":[{"role":"user","content":[{"type":"text","text":"你好 & <world>"}]}],"tools":[]}`)
	second := []byte(` { "tools" : [], "messages" : [{"content":[{"text":"你好 & <world>","type":"text"}],"role":"user"}], "model":"m1234567", "provider":"morde", "max_tokens": 128, "tool_history": [] } `)
	firstDigest, firstOK := cloudDSHModelBudgetRequestDigest(first)
	secondDigest, secondOK := cloudDSHModelBudgetRequestDigest(second)
	if !firstOK || !secondOK || firstDigest != secondDigest {
		t.Fatalf("input-only request digests differ: first=%q/%v second=%q/%v", firstDigest, firstOK, secondDigest, secondOK)
	}
}

func TestCloudDSHModelBudgetPrepareRequestIsStrictAndAuthorized(t *testing.T) {
	request := json.RawMessage(`{"provider":"morde","model":"m1234567","messages":[{"role":"user","content":[{"type":"text","text":"question"}]}],"tools":[]}`)
	digest, ok := cloudDSHModelBudgetRequestDigest(request)
	if !ok {
		t.Fatal("valid provider request digest was rejected")
	}
	through, turn, step := int64(4), int64(1), int64(0)
	frame := cloudDSHFrame{Type: "model_budget_prepare", ID: "req_1", CallID: "budget_call_1", DSHCallID: "mdl_dsh_call_1", Through: &through,
		Turn: &turn, Step: &step, RequestDigest: digest, Request: request,
		Config: json.RawMessage(`{"provider":"morde","model":"m1234567"}`), ToolHistory: json.RawMessage(`[]`)}
	if got, err := validateCloudDSHModelBudgetPrepareRequest(frame, "m1234567", nil); err != nil || got != digest {
		t.Fatalf("valid budget request rejected: digest=%q err=%v", got, err)
	}

	compaction := frame
	compaction.Config = json.RawMessage(`{"provider":"morde","model":"m1234567","purpose":"compaction"}`)
	if got, err := validateCloudDSHModelBudgetPrepareRequest(compaction, "m1234567", nil); err != nil || got != digest {
		t.Fatalf("valid compaction budget request rejected: digest=%q err=%v", got, err)
	}
	for _, config := range []string{
		`{"provider":"morde","model":"m1234567","purpose":null}`,
		`{"provider":"morde","model":"m1234567","purpose":7}`,
		`{"provider":"morde","model":"m1234567","purpose":"unknown"}`,
		`{"provider":"morde","model":"m1234567","purpose":""}`,
	} {
		badPurpose := frame
		badPurpose.Config = json.RawMessage(config)
		if _, err := validateCloudDSHModelBudgetPrepareRequest(badPurpose, "m1234567", nil); err == nil {
			t.Fatalf("invalid model budget purpose was accepted: %s", config)
		}
	}

	bad := frame
	bad.RequestDigest = strings.Repeat("0", 64)
	if _, err := validateCloudDSHModelBudgetPrepareRequest(bad, "m1234567", nil); err == nil {
		t.Fatal("tampered request digest was accepted")
	}
	bad = frame
	bad.Request = json.RawMessage(`{"provider":"morde","model":"m1234567","messages":[],"tools":[],"max_tokens":128}`)
	if _, err := validateCloudDSHModelBudgetPrepareRequest(bad, "m1234567", nil); err == nil {
		t.Fatal("draft request with a fixed output cap was accepted")
	}
	bad = frame
	bad.Config = json.RawMessage(`{"maxTokens":2048}`)
	if _, err := validateCloudDSHModelBudgetPrepareRequest(bad, "m1234567", nil); err == nil {
		t.Fatal("config carrying a hidden output cap was accepted")
	}
	bad = frame
	bad.Request = json.RawMessage(`{"provider":"morde","model":"m1234567","messages":[{"role":"user","content":[{"type":"text","text":"question"}]}],"tools":[{"name":"morde_unapproved","description":"unapproved","parameters":{"type":"object","properties":{},"additionalProperties":false}}]}`)
	bad.RequestDigest, _ = cloudDSHModelBudgetRequestDigest(bad.Request)
	if _, err := validateCloudDSHModelBudgetPrepareRequest(bad, "m1234567", nil); err == nil {
		t.Fatal("unauthorized draft tool was accepted")
	}
}

func TestCloudDSHModelBudgetPlannerAdapterForwardsCompactionPurpose(t *testing.T) {
	service := &cloudDSHModelBudgetPreparationServiceStub{plan: app.TaskModelBudgetPlan{PlanID: "plan_1", MaxTokens: 512}}
	planner, err := NewCloudDSHModelBudgetPlanner(service)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.PrepareModelBudget(context.Background(), CloudDSHModelBudgetPlanRequest{
		Config:  json.RawMessage(`{"provider":"morde","model":"m1234567","purpose":"compaction"}`),
		Request: json.RawMessage(`{"provider":"morde","model":"m1234567","messages":[],"tools":[]}`),
	})
	if err != nil || plan.PlanID != "plan_1" || plan.MaxTokens != 512 {
		t.Fatalf("compaction adapter plan=%+v err=%v", plan, err)
	}
	if service.request.Purpose != "compaction" {
		t.Fatalf("preparation service purpose = %q, want compaction", service.request.Purpose)
	}

	_, err = planner.PrepareModelBudget(context.Background(), CloudDSHModelBudgetPlanRequest{
		Config: json.RawMessage(`{"provider":"morde","model":"m1234567","purpose":"unknown"}`),
	})
	if err == nil {
		t.Fatal("planner adapter accepted an unknown budget purpose")
	}
}

func TestCloudDSHModelBudgetFramesRequireCompleteIdentity(t *testing.T) {
	request := `{"provider":"morde","model":"m1234567","messages":[{"role":"user","content":[{"type":"text","text":"question"}]}],"tools":[]}`
	digest, _ := cloudDSHModelBudgetRequestDigest([]byte(request))
	prepare := `{"type":"model_budget_prepare","id":"req_1","task_id":"task_1","owner_id":"owner_1","agent_id":"agent_1","generation":2,"authorization_version":3,"session_id":"session_1","through_seq":4,"call_id":"budget_call_1","dsh_call_id":"mdl_dsh_call_1","turn":1,"step":0,"request_digest":"` + digest + `","request":` + request + `,"config":{"provider":"morde","model":"m1234567"},"tool_history":[]}`
	if frame, err := decodeCloudDSHFrame([]byte(prepare)); err != nil || frame.Type != "model_budget_prepare" || frame.CallID != "budget_call_1" || frame.DSHCallID != "mdl_dsh_call_1" {
		t.Fatalf("complete prepare frame rejected: frame=%+v err=%v", frame, err)
	}
	missingFence := strings.Replace(prepare, `,"owner_id":"owner_1"`, "", 1)
	if _, err := decodeCloudDSHFrame([]byte(missingFence)); err == nil {
		t.Fatal("prepare frame without the owner fence was accepted")
	}

	model := `{"type":"model","id":"model_call_1","task_id":"task_1","owner_id":"owner_1","agent_id":"agent_1","generation":2,"authorization_version":3,"session_id":"session_1","through_seq":5,"request":` + request + `,"sha256":"` + strings.Repeat("a", 64) + `","budget_plan_id":"plan_1","budget_call_id":"budget_call_1","request_digest":"` + digest + `"}`
	if frame, err := decodeCloudDSHFrame([]byte(model)); err != nil || frame.BudgetCallID != "budget_call_1" {
		t.Fatalf("complete planned model frame rejected: frame=%+v err=%v", frame, err)
	}
	partialPlan := strings.Replace(model, `,"budget_call_id":"budget_call_1"`, "", 1)
	if _, err := decodeCloudDSHFrame([]byte(partialPlan)); err == nil {
		t.Fatal("model frame with a partial budget identity was accepted")
	}
}

func TestCloudDSHModelBudgetPlanConsumptionIsSessionBoundAndOneShot(t *testing.T) {
	prepared := []byte(`{"provider":"morde","model":"m1234567","messages":[{"role":"user","content":[{"type":"text","text":"question"}]}],"tools":[]}`)
	digest, ok := cloudDSHModelBudgetRequestDigest(prepared)
	if !ok {
		t.Fatal("prepare digest was invalid")
	}
	finaLRequest := []byte(`{"tools":[],"model":"m1234567","provider":"morde","messages":[{"content":[{"text":"question","type":"text"}],"role":"user"}],"max_tokens":128}`)
	if got, valid := cloudDSHModelBudgetRequestDigest(finaLRequest); !valid || got != digest {
		t.Fatalf("final request input projection differs: digest=%q valid=%v", got, valid)
	}
	now := time.Now()
	identity := cloudDSHIdentity{TaskID: "task_1", OwnerID: "owner_1", AgentID: "agent_1", SessionID: "session_1", Generation: 3, AuthorizationVersion: 7}
	preparedDSHCallID, err := app.DSHModelCallID(identity.TaskID, identity.SessionID, 4)
	if err != nil {
		t.Fatal(err)
	}
	finalDSHCallID, err := app.DSHModelCallID(identity.TaskID, identity.SessionID, 6)
	if err != nil {
		t.Fatal(err)
	}
	newPlans := func() map[string]cloudDSHModelBudgetPlanBinding {
		plans := make(map[string]cloudDSHModelBudgetPlanBinding)
		err := storeCloudDSHModelBudgetPlan(plans, cloudDSHModelBudgetPlanBinding{Identity: identity, ThroughSeq: 4,
			CallID: "budget_call_1", DSHCallID: preparedDSHCallID, LedgerCallID: preparedDSHCallID,
			RequestID: "req_1", RequestDigest: digest, PlanID: "plan_1", MaxTokens: 128}, now)
		if err != nil {
			t.Fatalf("store plan: %v", err)
		}
		return plans
	}

	plans := newPlans()
	ledgerCallID, maxTokens, err := consumeCloudDSHModelBudgetPlan(plans, identity, 6, finalDSHCallID, "budget_call_1", "plan_1", digest, finaLRequest, now.Add(time.Second))
	if err != nil || maxTokens != 128 || ledgerCallID != preparedDSHCallID || ledgerCallID == finalDSHCallID {
		t.Fatalf("valid plan did not return its distinct ledger identity: ledger_call_id=%q max_tokens=%d err=%v",
			ledgerCallID, maxTokens, err)
	}
	if _, _, err := consumeCloudDSHModelBudgetPlan(plans, identity, 6, finalDSHCallID, "budget_call_1", "plan_1", digest, finaLRequest, now.Add(time.Second)); err == nil {
		t.Fatal("one-shot budget plan was reused")
	}

	tests := []struct {
		name         string
		identity     cloudDSHIdentity
		through      int64
		budgetCallID string
		planID       string
		digest       string
		request      []byte
	}{
		{name: "different session", identity: cloudDSHIdentity{TaskID: identity.TaskID, OwnerID: identity.OwnerID, AgentID: identity.AgentID, SessionID: "session_2", Generation: identity.Generation, AuthorizationVersion: identity.AuthorizationVersion}, through: 6, budgetCallID: "budget_call_1", planID: "plan_1", digest: digest, request: finaLRequest},
		{name: "before prepared through", identity: identity, through: 3, budgetCallID: "budget_call_1", planID: "plan_1", digest: digest, request: finaLRequest},
		{name: "different budget call", identity: identity, through: 6, budgetCallID: "budget_call_2", planID: "plan_1", digest: digest, request: finaLRequest},
		{name: "different digest echo", identity: identity, through: 6, budgetCallID: "budget_call_1", planID: "plan_1", digest: strings.Repeat("a", 64), request: finaLRequest},
		{name: "tampered input", identity: identity, through: 6, budgetCallID: "budget_call_1", planID: "plan_1", digest: digest, request: []byte(`{"provider":"morde","model":"m1234567","messages":[{"role":"user","content":[{"type":"text","text":"tampered"}]}],"tools":[],"max_tokens":128}`)},
		{name: "different cap", identity: identity, through: 6, budgetCallID: "budget_call_1", planID: "plan_1", digest: digest, request: []byte(`{"provider":"morde","model":"m1234567","messages":[{"role":"user","content":[{"type":"text","text":"question"}]}],"tools":[],"max_tokens":127}`)},
		{name: "extra unsummarized field", identity: identity, through: 6, budgetCallID: "budget_call_1", planID: "plan_1", digest: digest, request: []byte(`{"provider":"morde","model":"m1234567","messages":[{"role":"user","content":[{"type":"text","text":"question"}]}],"tools":[],"max_tokens":128,"temperature":0.4}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := consumeCloudDSHModelBudgetPlan(newPlans(), test.identity, test.through, finalDSHCallID, test.budgetCallID, test.planID, test.digest, test.request, now.Add(time.Second)); err == nil {
				t.Fatal("mismatched plan consumption was accepted")
			}
		})
	}
}

func TestCloudDSHRecoveryModelBudgetPlanBindsLedgerAndFrameIDsSeparately(t *testing.T) {
	request := []byte(`{"provider":"morde","model":"m1234567","messages":[{"role":"user","content":[{"type":"text","text":"question"}]}],"tools":[]}`)
	digest, ok := cloudDSHModelBudgetRequestDigest(request)
	if !ok {
		t.Fatal("recovery request digest was invalid")
	}
	finalRequest := []byte(`{"provider":"morde","model":"m1234567","messages":[{"role":"user","content":[{"type":"text","text":"question"}]}],"tools":[],"max_tokens":128}`)
	identity := cloudDSHIdentity{TaskID: "task_recovery", OwnerID: "owner_1", AgentID: "agent_1", SessionID: "session_1", Generation: 3, AuthorizationVersion: 7}
	dshCallID := "mdr_" + strings.Repeat("d", 64)
	ledgerCallID := "mrc_" + strings.Repeat("c", 64)
	now := time.Now()
	plans := map[string]cloudDSHModelBudgetPlanBinding{}
	if err := storeCloudDSHModelBudgetPlan(plans, cloudDSHModelBudgetPlanBinding{Identity: identity, ThroughSeq: 4,
		CallID: "budget_call_1", DSHCallID: dshCallID, LedgerCallID: ledgerCallID,
		RequestID: "request_1", RequestDigest: digest, PlanID: "plan_1", MaxTokens: 128}, now); err != nil {
		t.Fatalf("store recovery plan: %v", err)
	}
	gotLedgerCallID, maxTokens, err := consumeCloudDSHModelBudgetPlan(plans, identity, 4, dshCallID, "budget_call_1", "plan_1", digest,
		finalRequest, now.Add(time.Second))
	if err != nil || gotLedgerCallID != ledgerCallID || maxTokens != 128 || gotLedgerCallID == dshCallID {
		t.Fatalf("recovery plan conflated DSH and ledger IDs: ledger=%q DSH=%q max_tokens=%d err=%v",
			gotLedgerCallID, dshCallID, maxTokens, err)
	}

	badPlans := map[string]cloudDSHModelBudgetPlanBinding{}
	if err := storeCloudDSHModelBudgetPlan(badPlans, cloudDSHModelBudgetPlanBinding{Identity: identity, ThroughSeq: 4,
		CallID: "budget_call_2", DSHCallID: dshCallID, LedgerCallID: dshCallID,
		RequestID: "request_2", RequestDigest: digest, PlanID: "plan_2", MaxTokens: 128}, now); err != nil {
		t.Fatalf("store candidate recovery plan for fail-closed consumption: %v", err)
	}
	if _, _, err := consumeCloudDSHModelBudgetPlan(badPlans, identity, 4, dshCallID, "budget_call_2", "plan_2", digest,
		finalRequest, now.Add(time.Second)); err == nil {
		t.Fatal("recovery plan with DSH identity reused as ledger identity was accepted")
	}
}

func TestCloudDSHModelBudgetPlanExpiresAndStoreIsBounded(t *testing.T) {
	request := []byte(`{"provider":"morde","model":"m1234567","messages":[{"role":"user","content":[{"type":"text","text":"question"}]}],"tools":[]}`)
	digest, _ := cloudDSHModelBudgetRequestDigest(request)
	now := time.Now()
	identity := cloudDSHIdentity{TaskID: "task_1", OwnerID: "owner_1", AgentID: "agent_1", SessionID: "session_1", Generation: 1, AuthorizationVersion: 1}
	dshCallID, err := app.DSHModelCallID(identity.TaskID, identity.SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	plans := map[string]cloudDSHModelBudgetPlanBinding{}
	if err := storeCloudDSHModelBudgetPlan(plans, cloudDSHModelBudgetPlanBinding{Identity: identity, ThroughSeq: 0,
		CallID: "budget_call_1", DSHCallID: dshCallID, LedgerCallID: dshCallID,
		RequestID: "req_1", RequestDigest: digest, PlanID: "plan_1", MaxTokens: 10}, now); err != nil {
		t.Fatalf("store plan: %v", err)
	}
	if _, _, err := consumeCloudDSHModelBudgetPlan(plans, identity, 0, dshCallID, "budget_call_1", "plan_1", digest,
		[]byte(`{"provider":"morde","model":"m1234567","messages":[{"role":"user","content":[{"type":"text","text":"question"}]}],"tools":[],"max_tokens":10}`), now.Add(cloudDSHModelBudgetPlanTTL)); err == nil {
		t.Fatal("expired plan was accepted")
	}
	if err := storeCloudDSHModelBudgetPlan(plans, cloudDSHModelBudgetPlanBinding{Identity: identity, ThroughSeq: 0,
		CallID: "budget_call_1", DSHCallID: dshCallID, LedgerCallID: dshCallID,
		RequestID: "req_1", RequestDigest: digest, PlanID: "plan_1", MaxTokens: 10}, now); err != nil {
		t.Fatalf("store first session plan: %v", err)
	}
	if err := storeCloudDSHModelBudgetPlan(plans, cloudDSHModelBudgetPlanBinding{Identity: identity, ThroughSeq: 1,
		CallID: "budget_call_2", DSHCallID: dshCallID, LedgerCallID: dshCallID,
		RequestID: "req_2", RequestDigest: digest, PlanID: "plan_2", MaxTokens: 10}, now); err == nil {
		t.Fatal("same session accepted a second unconsumed plan")
	}

	plans = map[string]cloudDSHModelBudgetPlanBinding{}
	if err := storeCloudDSHModelBudgetPlan(plans, cloudDSHModelBudgetPlanBinding{Identity: identity, ThroughSeq: 0,
		CallID: "budget_call_max", DSHCallID: dshCallID, LedgerCallID: dshCallID,
		RequestID: "req_max", RequestDigest: digest, PlanID: "plan_max", MaxTokens: cloudDSHModelBudgetProtocolMaxTokens}, now); err != nil {
		t.Fatalf("protocol maximum output cap was rejected: %v", err)
	}
	plans = map[string]cloudDSHModelBudgetPlanBinding{}
	if err := storeCloudDSHModelBudgetPlan(plans, cloudDSHModelBudgetPlanBinding{Identity: identity, ThroughSeq: 0,
		CallID: "budget_call_over", DSHCallID: dshCallID, LedgerCallID: dshCallID,
		RequestID: "req_over", RequestDigest: digest, PlanID: "plan_over", MaxTokens: cloudDSHModelBudgetProtocolMaxTokens + 1}, now); err == nil {
		t.Fatal("plan above the ModelGateway output maximum was accepted")
	}

	for index := 0; index < cloudDSHMaxPendingModelCalls; index++ {
		candidateIdentity := identity
		candidateIdentity.SessionID = "session_" + strings.Repeat("s", index+1)
		candidate := cloudDSHModelBudgetPlanBinding{Identity: candidateIdentity, ThroughSeq: int64(index), CallID: "budget_call_" + strings.Repeat("a", index+1),
			DSHCallID: "mdl_" + strings.Repeat("a", 64), LedgerCallID: "mdl_" + strings.Repeat("a", 64),
			RequestID: "req_" + strings.Repeat("b", index+1), RequestDigest: digest,
			PlanID: "plan_" + strings.Repeat("c", index+1), MaxTokens: 10}
		if err := storeCloudDSHModelBudgetPlan(plans, candidate, now); err != nil {
			t.Fatalf("store bounded plan %d: %v", index, err)
		}
	}
	if err := storeCloudDSHModelBudgetPlan(plans, cloudDSHModelBudgetPlanBinding{Identity: identity, ThroughSeq: 100,
		CallID: "budget_call_overflow", DSHCallID: dshCallID, LedgerCallID: dshCallID,
		RequestID: "req_overflow", RequestDigest: digest, PlanID: "plan_overflow", MaxTokens: 10}, now); err == nil {
		t.Fatal("plan map exceeded its bound")
	}
}
