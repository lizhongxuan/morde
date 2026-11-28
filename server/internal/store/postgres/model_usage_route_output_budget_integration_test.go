package postgres

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
)

func TestLongFormRouteOutputBudgetIncludesAvailableAccountTokens(t *testing.T) {
	store, _, _, claim, value := routeModelLongFormFixture(t, &routeModelClientTest{})
	ctx := context.Background()
	var version time.Time
	if err := store.pool.QueryRow(ctx, `SELECT updated_at FROM model_connections WHERE id=$1`, value.ModelID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	var accountUsed int64
	if err := store.pool.QueryRow(ctx, `SELECT COALESCE(SUM(CASE WHEN state='settled' THEN input_tokens+output_tokens
WHEN state='rejected' THEN 0 ELSE reserved_tokens END),0) FROM model_call_ledger
WHERE owner_id=$1 AND NOT personal_model`, claim.Message.ActorID).Scan(&accountUsed); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE model_account_budgets SET token_limit=$2 WHERE owner_id=$1`,
		claim.Message.ActorID, accountUsed+100+3000); err != nil {
		t.Fatal(err)
	}
	candidate := &app.ConversationRouteOutputBudgetPlan{
		Stage: app.ConversationResponseLongForm, StrategyVersion: "policy-v1", CapabilityVersion: "capability-v1",
		ModelMaxOutputTokens: 8192, ContextWindowTokens: 32768, ContextInputUpperBoundTokens: 1000,
		ContextSafetyMarginTokens: 4096, ContextSafeTokens: 8192, AccountSafeTokens: 6000, ProjectSafeTokens: 6000,
		GoalTokens: 6000, MinimumTokens: 2048, SafeTokens: 6000,
		SendTokens: 6000, InputReservedTokens: 100,
	}
	call := longFormBudgetTestCall(claim, value, version, "long-route-budget-candidate-001", candidate)
	limits, err := store.LimitConversationRouteOutputBudget(ctx, call, nil)
	if err != nil || limits.AccountSafeTokens != 3000 || limits.ProjectSafeTokens != int64(app.MaxModelOutputTokens) {
		t.Fatalf("long-form account budget headroom = %+v, err=%v; want 3000 token account cap and no project cap", limits, err)
	}
	plan := *candidate
	plan.AccountSafeTokens, plan.ProjectSafeTokens = limits.AccountSafeTokens, limits.ProjectSafeTokens
	plan.SafeTokens = min(plan.ContextSafeTokens, min(plan.AccountSafeTokens, plan.ProjectSafeTokens))
	plan.SendTokens = min(plan.GoalTokens, plan.SafeTokens)
	call = longFormBudgetTestCall(claim, value, version, "long-route-budget-call-001", &plan)
	if prior, err := store.PrepareConversationRouteModelCall(ctx, call, nil); err != nil || prior != nil {
		t.Fatalf("prepare long-form route call = prior:%v err:%v", prior, err)
	}
	var stage, strategyVersion, capabilityVersion string
	var modelMax, contextWindow, contextInput, safetyMargin, contextSafe, accountSafe, projectSafe int64
	var goal, minimum, safe, send, inputHold, ledgerHold int64
	if err := store.pool.QueryRow(ctx, `SELECT c.budget_stage,c.budget_strategy_version,c.budget_capability_version,
c.model_max_output_tokens,c.context_window_tokens,c.context_input_upper_bound_tokens,c.context_safety_margin_tokens,
c.context_safe_tokens,c.account_safe_tokens,c.project_safe_tokens,
c.goal_tokens,c.minimum_tokens,c.safe_tokens,c.send_tokens,c.input_reserved_tokens,l.reserved_tokens
FROM conversation_route_model_calls c JOIN model_call_ledger l
 ON l.owner_id=c.actor_id AND l.source=c.source AND l.call_id=c.call_id WHERE c.message_id=$1`, claim.Message.ID).
		Scan(&stage, &strategyVersion, &capabilityVersion, &modelMax, &contextWindow, &contextInput, &safetyMargin,
			&contextSafe, &accountSafe, &projectSafe,
			&goal, &minimum, &safe, &send, &inputHold, &ledgerHold); err != nil {
		t.Fatal(err)
	}
	if stage != plan.Stage || strategyVersion != plan.StrategyVersion || capabilityVersion != plan.CapabilityVersion ||
		modelMax != plan.ModelMaxOutputTokens || contextWindow != plan.ContextWindowTokens ||
		contextInput != plan.ContextInputUpperBoundTokens || safetyMargin != plan.ContextSafetyMarginTokens ||
		contextSafe != plan.ContextSafeTokens || accountSafe != plan.AccountSafeTokens || projectSafe != plan.ProjectSafeTokens ||
		goal != plan.GoalTokens || minimum != plan.MinimumTokens || safe != plan.SafeTokens ||
		send != plan.SendTokens || inputHold != plan.InputReservedTokens || ledgerHold != inputHold+send {
		t.Fatalf("persisted plan/ledger = stage:%q versions:%q/%q model/context:%d/%d input/safety:%d/%d context/account/project/safe/send:%d/%d/%d/%d/%d goal/min:%d/%d input/ledger hold:%d/%d",
			stage, strategyVersion, capabilityVersion, modelMax, contextWindow, contextInput, safetyMargin,
			contextSafe, accountSafe, projectSafe, safe, send,
			goal, minimum, inputHold, ledgerHold)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE conversation_route_model_calls SET send_tokens=send_tokens+1 WHERE message_id=$1`, claim.Message.ID); err == nil {
		t.Fatal("database accepted a send cap that does not match min(goal,safe)")
	}
	if _, err := store.pool.Exec(ctx, `UPDATE conversation_route_model_calls SET budget_stage=NULL WHERE message_id=$1`, claim.Message.ID); err == nil {
		t.Fatal("database accepted a partial long-form output budget plan")
	}
}

func TestLongFormRouteOutputBudgetIncludesAvailableProjectFunds(t *testing.T) {
	store, _, _, claim, value := routeModelLongFormProjectFixture(t, &routeModelClientTest{})
	ctx := context.Background()
	var version time.Time
	if err := store.pool.QueryRow(ctx, `SELECT updated_at FROM model_connections WHERE id=$1`, value.ModelID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE project_budgets SET limit_micros=3100 WHERE project_id=$1`, claim.Input.ProjectID); err != nil {
		t.Fatal(err)
	}
	candidate := &app.ConversationRouteOutputBudgetPlan{
		Stage: app.ConversationResponseLongForm, StrategyVersion: "policy-v1", CapabilityVersion: "capability-v1",
		ModelMaxOutputTokens: 8192, ContextWindowTokens: 32768, ContextInputUpperBoundTokens: 1000,
		ContextSafetyMarginTokens: 4096, ContextSafeTokens: 8192, AccountSafeTokens: 8192, ProjectSafeTokens: 8192,
		GoalTokens: 6000, MinimumTokens: 2048, SafeTokens: 6000, SendTokens: 6000, InputReservedTokens: 100,
	}
	call := longFormBudgetTestCall(claim, value, version, "long-route-project-candidate-001", candidate)
	rate, err := (routeRateTest{modelID: value.ModelID}).ProjectRateSnapshot(ctx, value.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	limits, err := store.LimitConversationRouteOutputBudget(ctx, call, &rate)
	if err != nil || limits.ProjectSafeTokens != 3000 || limits.AccountSafeTokens < candidate.ContextSafeTokens {
		t.Fatalf("long-form project budget headroom = %+v, err=%v; want 3000 token project cap and enough account headroom", limits, err)
	}
	plan := *candidate
	plan.AccountSafeTokens, plan.ProjectSafeTokens = limits.AccountSafeTokens, limits.ProjectSafeTokens
	plan.SafeTokens = min(plan.ContextSafeTokens, min(plan.AccountSafeTokens, plan.ProjectSafeTokens))
	plan.SendTokens = min(plan.GoalTokens, plan.SafeTokens)
	call = longFormBudgetTestCall(claim, value, version, "long-route-project-call-001", &plan)
	usage := project.Usage{InputTokens: plan.InputReservedTokens, OutputTokens: plan.SendTokens}
	cost, err := rate.EstimateTokenCost(usage)
	if err != nil {
		t.Fatal(err)
	}
	projectDigest := sha256.Sum256([]byte("long-route-project-budget"))
	projectCall := &app.ProjectModelReservation{ProjectID: claim.Input.ProjectID, ActorID: call.OwnerID,
		Source: app.ModelCallRoute, CallID: call.CallID, ModelID: call.ModelID, Usage: usage,
		ReservedTokens: call.ReservedTokens, Rate: rate, ReservedMicros: cost, RequestDigest: projectDigest[:], AccountCall: call}
	if prior, err := store.PrepareConversationRouteModelCall(ctx, call, projectCall); err != nil || prior != nil {
		t.Fatalf("prepare project long-form route call = prior:%v err:%v", prior, err)
	}
	var outputHold, moneyHold int64
	if err := store.pool.QueryRow(ctx, `SELECT reserved_output_tokens,reserved_micros FROM project_budget_ledger
WHERE project_id=$1 AND source='route' AND call_id=$2`, claim.Input.ProjectID, call.CallID).Scan(&outputHold, &moneyHold); err != nil ||
		outputHold != 3000 || moneyHold != 3100 {
		t.Fatalf("project long-form holds = output:%d money:%d err:%v; want 3000/3100", outputHold, moneyHold, err)
	}
}

func longFormBudgetTestCall(claim app.ConversationRouteClaim, value app.ConversationRouteContext, version time.Time,
	callID string, plan *app.ConversationRouteOutputBudgetPlan) app.ModelCallReservation {
	digest := sha256.Sum256([]byte("long-form-route-budget:" + callID))
	return app.ModelCallReservation{Source: app.ModelCallRoute, OwnerID: claim.Message.ActorID, ProjectID: claim.Input.ProjectID,
		ModelID: value.ModelID, ProviderModel: "test-model", ConnectionVersion: version, CallID: callID,
		RequestDigest: digest[:], ReservedTokens: plan.InputReservedTokens + plan.SendTokens,
		Route: &app.ConversationRouteModelFence{Claim: claim, Context: value}, RouteOutputBudget: plan}
}

func TestLongFormRouteWithoutValidOutputBudgetIsRejectedBeforeReservation(t *testing.T) {
	tests := []struct {
		name string
		plan *app.ConversationRouteOutputBudgetPlan
	}{
		{name: "missing plan"},
		{name: "below minimum", plan: &app.ConversationRouteOutputBudgetPlan{Stage: app.ConversationResponseLongForm,
			StrategyVersion: "policy-v1", CapabilityVersion: "capability-v1", ModelMaxOutputTokens: 8192,
			ContextWindowTokens: 32768, ContextInputUpperBoundTokens: 1000, ContextSafetyMarginTokens: 4096,
			ContextSafeTokens: 8192, AccountSafeTokens: 8192, ProjectSafeTokens: 8192,
			GoalTokens: 1024, MinimumTokens: 2048, SafeTokens: 8192, SendTokens: 1024, InputReservedTokens: 100}},
		{name: "reservation mismatch", plan: &app.ConversationRouteOutputBudgetPlan{Stage: app.ConversationResponseLongForm,
			StrategyVersion: "policy-v1", CapabilityVersion: "capability-v1", ModelMaxOutputTokens: 8192,
			ContextWindowTokens: 32768, ContextInputUpperBoundTokens: 1000, ContextSafetyMarginTokens: 4096,
			ContextSafeTokens: 8192, AccountSafeTokens: 8192, ProjectSafeTokens: 8192,
			GoalTokens: 6000, MinimumTokens: 2048, SafeTokens: 8192, SendTokens: 6000, InputReservedTokens: 100}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, _, _, claim, value := routeModelLongFormFixture(t, &routeModelClientTest{})
			ctx := context.Background()
			var version time.Time
			if err := store.pool.QueryRow(ctx, `SELECT updated_at FROM model_connections WHERE id=$1`, value.ModelID).Scan(&version); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256([]byte("long-form-route-budget-invalid:" + test.name))
			reserve := int64(6100)
			if test.plan != nil {
				reserve = test.plan.InputReservedTokens + test.plan.SendTokens
			}
			if test.name == "reservation mismatch" {
				reserve++
			}
			call := app.ModelCallReservation{Source: app.ModelCallRoute, OwnerID: claim.Message.ActorID, ProjectID: claim.Input.ProjectID,
				ModelID: value.ModelID, ProviderModel: "test-model", ConnectionVersion: version,
				CallID: fmt.Sprintf("long-route-invalid-call-%03d", index+1), RequestDigest: digest[:], ReservedTokens: reserve,
				Route: &app.ConversationRouteModelFence{Claim: claim, Context: value}, RouteOutputBudget: test.plan}
			if _, err := store.PrepareConversationRouteModelCall(ctx, call, nil); domain.CodeOf(err) != domain.CodeInvalidArgument {
				t.Fatalf("invalid long-form budget plan error = %v", err)
			}
			var routeRows, ledgerRows int
			if err := store.pool.QueryRow(ctx, `SELECT
(SELECT count(*) FROM conversation_route_model_calls WHERE message_id=$1),
(SELECT count(*) FROM model_call_ledger WHERE owner_id=$2 AND source='route' AND call_id=$3)`,
				claim.Message.ID, call.OwnerID, call.CallID).Scan(&routeRows, &ledgerRows); err != nil || routeRows != 0 || ledgerRows != 0 {
				t.Fatalf("invalid plan created reservations: route=%d ledger=%d err=%v", routeRows, ledgerRows, err)
			}
		})
	}
}
