package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

type cloudDSHGoalScheduleStore struct {
	objects        map[string]personal.Object
	mutations      map[string]personal.MutationReceipt
	mutationHashes map[string][32]byte
	mutationCalls  int
	sequence       int64
}

type cloudDSHGoalTaskStore struct {
	*cloudDSHBrokerTaskStore
	goals     *cloudDSHGoalScheduleStore
	committed map[string]task.Event
}

func (s *cloudDSHGoalTaskStore) CommitTaskProgress(ctx context.Context, progress TaskProgress) (task.Event, error) {
	if progress.CloudDSHPersonalMutation == nil {
		return s.cloudDSHBrokerTaskStore.CommitTaskProgress(ctx, progress)
	}
	commit := *progress.CloudDSHPersonalMutation
	if !ValidCloudDSHPersonalMutationCommit(commit) || ValidateCloudDSHPersonalMutationEvent(progress.Event.Payload, commit) != nil {
		return task.Event{}, domain.NewError(domain.CodeInvalidState, "invalid atomic goal write")
	}
	if prior, ok := s.committed[commit.CallID]; ok {
		if ValidateCloudDSHPersonalMutationEvent(prior.Payload, commit) != nil {
			return task.Event{}, domain.NewError(domain.CodeInvalidState, "goal call changed")
		}
		return prior, nil
	}
	principal := identity.Principal{UserID: commit.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}
	if _, err := s.goals.ApplyPersonalMutation(ctx, principal, commit.Mutation); err != nil {
		return task.Event{}, err
	}
	event, err := s.cloudDSHBrokerTaskStore.CommitTaskProgress(ctx, progress)
	if err == nil {
		s.committed[commit.CallID] = event
	}
	return event, err
}

func (s *cloudDSHGoalTaskStore) ConfirmCloudDSHGoalMutationReceipt(ctx context.Context, request CloudDSHGoalReceiptRequest) (CloudDSHGoalReceipt, error) {
	if err := ctx.Err(); err != nil {
		return CloudDSHGoalReceipt{}, err
	}
	event, ok := s.committed[request.CallID]
	if !ok {
		return CloudDSHGoalReceipt{}, domain.NewError(domain.CodeInvalidState, "goal event is unavailable")
	}
	receipt, err := ValidateCloudDSHGoalReceiptEvent(event.Payload, request)
	if err != nil {
		return CloudDSHGoalReceipt{}, err
	}
	key := receipt.Mutation.OwnerID + "\x00" + receipt.Mutation.MutationID
	ledger, ok := s.goals.mutations[key]
	if !ok || s.goals.mutationHashes[key] != sha256.Sum256(receipt.Mutation.RequestHash) ||
		ledger.ObjectID != request.GoalID || ledger.ResultRevision != request.BaseRevision+1 {
		return CloudDSHGoalReceipt{}, domain.NewError(domain.CodeInvalidState, "goal mutation receipt is unavailable")
	}
	return receipt, nil
}

func newCloudDSHGoalScheduleStore() *cloudDSHGoalScheduleStore {
	return &cloudDSHGoalScheduleStore{
		objects: make(map[string]personal.Object), mutations: make(map[string]personal.MutationReceipt),
		mutationHashes: make(map[string][32]byte),
	}
}

func (s *cloudDSHGoalScheduleStore) ApplyPersonalMutation(_ context.Context, principal identity.Principal, mutation personal.Mutation) (personal.MutationReceipt, error) {
	s.mutationCalls++
	if err := personal.ValidateMutation(mutation); err != nil {
		return personal.MutationReceipt{}, err
	}
	key := principal.UserID + "\x00" + mutation.MutationID
	if prior, ok := s.mutations[key]; ok {
		if s.mutationHashes[key] != sha256.Sum256(mutation.RequestHash) {
			return personal.MutationReceipt{}, domain.NewError(domain.CodeIdempotencyConflict, "goal mutation key reused")
		}
		prior.Replay = true
		return prior, nil
	}
	objectKey := principal.UserID + "\x00" + mutation.ObjectType + "\x00" + mutation.ObjectID
	current, exists := s.objects[objectKey]
	if mutation.Delete {
		if !exists || current.Revision != mutation.BaseRevision {
			return personal.MutationReceipt{}, domain.NewError(domain.CodeConflict, "goal revision changed")
		}
		delete(s.objects, objectKey)
	} else {
		if mutation.BaseRevision == 0 && exists || mutation.BaseRevision > 0 && (!exists || current.Revision != mutation.BaseRevision) {
			return personal.MutationReceipt{}, domain.NewError(domain.CodeConflict, "goal revision changed")
		}
		if err := personal.ValidateObjectBody(mutation.ObjectType, mutation.ObjectID, mutation.Body); err != nil {
			return personal.MutationReceipt{}, err
		}
		s.objects[objectKey] = personal.Object{ObjectType: mutation.ObjectType, ObjectID: mutation.ObjectID,
			Revision: mutation.BaseRevision + 1, Body: append(json.RawMessage(nil), mutation.Body...)}
	}
	s.sequence++
	receipt := personal.MutationReceipt{ObjectType: mutation.ObjectType, ObjectID: mutation.ObjectID,
		ResultRevision: mutation.BaseRevision + 1, AccountChangeSequence: s.sequence}
	s.mutations[key], s.mutationHashes[key] = receipt, sha256.Sum256(mutation.RequestHash)
	return receipt, nil
}

func (s *cloudDSHGoalScheduleStore) GetPersonalObject(_ context.Context, principal identity.Principal, objectType, objectID string) (personal.Object, error) {
	object, ok := s.objects[principal.UserID+"\x00"+objectType+"\x00"+objectID]
	if !ok {
		return personal.Object{}, domain.NewError(domain.CodeNotFound, "personal object not found")
	}
	return object, nil
}

func (s *cloudDSHGoalScheduleStore) ListPersonalGoals(_ context.Context, principal identity.Principal, _ PageRequest) (GoalPage, error) {
	items := make([]personal.Object, 0)
	prefix := principal.UserID + "\x00" + personal.ObjectTypeGoal + "\x00"
	for key, value := range s.objects {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			items = append(items, value)
		}
	}
	return GoalPage{Items: items}, nil
}

func cloudDSHGoalScheduleTaskFixture(t *testing.T) (*CloudDSHToolBroker, *cloudDSHBrokerTaskStore, task.Task, *cloudDSHGoalScheduleStore, *scheduleServiceStoreFake) {
	t.Helper()
	runtimeFixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: runtimeFixture.store}
	ownerID, conversationID := store.details.Task.OwnerID, "c1234567"
	expiresAt := time.Now().Add(time.Hour).UTC()
	store.details.Task.ExecutionLocation = "server"
	store.details.Task.InitiatorID = ownerID
	store.details.Task.ConversationID = conversationID
	store.details.Task.AgentSessionID = "session_goal_schedule"
	store.details.Task.Authorization = task.Authorization{
		Kind: string(identity.KindSession), TokenID: "t1234567", Version: 1, ExpiresAt: &expiresAt,
		Scopes: []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)},
	}
	store.details.Task.ConversationContext = &task.ConversationContext{
		OwnerID: ownerID, InitiatorID: ownerID, ConversationID: conversationID, Scope: "personal",
	}
	goalStore := newCloudDSHGoalScheduleStore()
	personalService, err := NewPersonalMutationService(goalStore)
	if err != nil {
		t.Fatal(err)
	}
	goals, err := NewGoalService(personalService, goalStore)
	if err != nil {
		t.Fatal(err)
	}
	scheduleStore := newScheduleServiceStoreFake()
	schedules := newTestScheduleService(t, scheduleStore, &scheduleServiceAuthorizerFake{}, &scheduleServiceModelCatalogFake{configured: true})
	goalTaskStore := &cloudDSHGoalTaskStore{cloudDSHBrokerTaskStore: store, goals: goalStore, committed: make(map[string]task.Event)}
	broker := &CloudDSHToolBroker{store: goalTaskStore, runtime: runtimeFixture.executor, goals: goals, schedules: schedules, personalData: personalService}
	return broker, store, store.details.Task, goalStore, scheduleStore
}

func TestCloudDSHGoalAndScheduleToolDefinitionsArePersonalAndValid(t *testing.T) {
	broker, _, claimed, _, _ := cloudDSHGoalScheduleTaskFixture(t)
	definitions, err := broker.Definitions(context.Background(), claimed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateAgentTools(definitions); err != nil {
		t.Fatalf("validateAgentTools() = %v", err)
	}
	seen := make(map[string]identity.Scope)
	for _, definition := range definitions {
		seen[definition.Name] = definition.RequiredScope
	}
	for _, name := range []string{cloudDSHGoalListToolName, cloudDSHGoalCreateToolName, cloudDSHGoalUpdateToolName, cloudDSHGoalDeleteToolName} {
		if seen[name] == "" {
			t.Errorf("missing goal tool %q", name)
		}
	}
	for _, name := range []string{cloudDSHScheduleListToolName, cloudDSHScheduleCreateToolName, cloudDSHScheduleUpdateToolName,
		cloudDSHScheduleActionToolName, cloudDSHScheduleRunsToolName} {
		if seen[name] == "" {
			t.Errorf("missing schedule tool %q", name)
		}
	}
}

func TestCloudDSHGoalContextAcceptsSubmitScopeButRejectsUnknownScope(t *testing.T) {
	definition := cloudDSHPersonalUpdateDefinition()
	if definition.RequiredScope != identity.ScopeTaskSubmit {
		t.Fatalf("personal write scope changed: %q", definition.RequiredScope)
	}
	if _, err := validateAgentTools([]AgentToolDefinition{definition}); err != nil {
		t.Fatalf("personal write tool was rejected: %v", err)
	}
	definition.RequiredScope = identity.Scope("invalid:scope")
	if _, err := validateAgentTools([]AgentToolDefinition{definition}); err == nil {
		t.Fatal("unknown scope was accepted")
	}
}

func TestCloudDSHGoalToolsUseGoalServiceAndReplayCreateIdempotently(t *testing.T) {
	broker, _, claimed, store, _ := cloudDSHGoalScheduleTaskFixture(t)
	call := ModelToolCall{CallID: "call_goal_create_1", Name: cloudDSHGoalCreateToolName,
		Arguments: `{"title":"完成季度计划","description":"","status":"active","steps":[{"title":"梳理目标","status":"pending","note":""}],"related_material_ids":[]}`}
	first, err := broker.execute(context.Background(), claimed, call, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := broker.execute(context.Background(), claimed, call, true, &first.Output)
	if err != nil {
		t.Fatal(err)
	}
	if first.Output != second.Output || len(store.objects) != 1 || store.mutationCalls != 1 {
		t.Fatalf("goal create replay output=%s/%s effects=%d mutation calls=%d", first.Output, second.Output, len(store.objects), store.mutationCalls)
	}
	var result struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	}
	if err := json.Unmarshal([]byte(first.Output), &result); err != nil || result.ID == "" || result.Revision != 1 {
		t.Fatalf("goal create result = %s; err=%v", first.Output, err)
	}
	if claimed.OwnerID != "u1234567" || store.objects[claimed.OwnerID+"\x00goal\x00"+result.ID].Revision != 1 {
		t.Fatalf("goal was not stored under task owner %q", claimed.OwnerID)
	}
}

func TestCloudDSHGoalUpdatePreservesOtherPlanFieldsAndRejectsInvalidArguments(t *testing.T) {
	broker, _, claimed, store, _ := cloudDSHGoalScheduleTaskFixture(t)
	create := ModelToolCall{CallID: "call_goal_create_2", Name: cloudDSHGoalCreateToolName,
		Arguments: `{"title":"季度计划","description":"保留说明","status":"active","steps":[{"title":"梳理","status":"pending","note":"保持原注释"}],"related_material_ids":[]}`}
	created, err := broker.execute(context.Background(), claimed, create, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	var goal struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(created.Output), &goal); err != nil {
		t.Fatal(err)
	}
	update := ModelToolCall{CallID: "call_goal_update_1", Name: cloudDSHGoalUpdateToolName,
		Arguments: fmt.Sprintf(`{"goal_id":%q,"expected_revision":1,"title":null,"description":null,"status":"paused","steps":[{"id":%q,"status":"in_progress"}]}`,
			goal.ID, cloudDSHStablePersonalID("step", claimed.ID, claimed.AgentSessionID, cloudDSHPersonalMutationID(claimed.ID, claimed.AgentSessionID, create.CallID), 0))}
	if _, err := broker.execute(context.Background(), claimed, update, false, nil); err != nil {
		t.Fatal(err)
	}
	stored := store.objects[claimed.OwnerID+"\x00goal\x00"+goal.ID]
	var body cloudDSHGoalBody
	if err := json.Unmarshal(stored.Body, &body); err != nil || stored.Revision != 2 || body.Title != "季度计划" ||
		body.Description != "保留说明" || body.Status != "paused" || body.Steps[0].Status != "in_progress" || body.Steps[0].Note != "保持原注释" {
		t.Fatalf("partial goal update did not preserve plan data: revision=%d body=%s err=%v", stored.Revision, stored.Body, err)
	}
	bad := ModelToolCall{CallID: "call_goal_bad_1", Name: cloudDSHGoalCreateToolName,
		Arguments: `{"title":"invalid","description":"","status":"running","steps":[],"related_material_ids":[]}`}
	if _, err := broker.execute(context.Background(), claimed, bad, false, nil); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("invalid goal status error = %v, want invalid argument", err)
	}
	if store.mutationCalls != 2 {
		t.Fatalf("invalid goal input reached the mutation store; calls=%d", store.mutationCalls)
	}
}

func TestCloudDSHGoalReceiptsBindArgumentsAndSurviveLaterMutations(t *testing.T) {
	broker, _, claimed, store, _ := cloudDSHGoalScheduleTaskFixture(t)
	ctx := context.Background()
	create := ModelToolCall{CallID: "goal-receipt-create", Name: cloudDSHGoalCreateToolName,
		Arguments: `{"title":"synthetic private goal","description":"private body","status":"active","steps":[],"related_material_ids":[]}`}
	created, err := broker.execute(ctx, claimed, create, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	var goal struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(created.Output), &goal); err != nil {
		t.Fatal(err)
	}
	update := ModelToolCall{CallID: "goal-receipt-update", Name: cloudDSHGoalUpdateToolName,
		Arguments: fmt.Sprintf(`{"goal_id":%q,"expected_revision":1,"title":null,"description":"changed","status":null,"steps":null}`, goal.ID)}
	updated, err := broker.execute(ctx, claimed, update, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	remove := ModelToolCall{CallID: "goal-receipt-delete", Name: cloudDSHGoalDeleteToolName,
		Arguments: fmt.Sprintf(`{"goal_id":%q,"expected_revision":2}`, goal.ID)}
	deleted, err := broker.execute(ctx, claimed, remove, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	fence := AgentInstanceFence{TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		AgentSessionID: claimed.AgentSessionID, AuthorizationVersion: claimed.Authorization.Version}
	for index, entry := range []struct {
		call   ModelToolCall
		output string
	}{{create, created.Output}, {update, updated.Output}, {remove, deleted.Output}} {
		version, output, err := broker.ConfirmPersonalContextWrite(ctx, claimed, fence, 4, make([]byte, 32), entry.call)
		if err != nil || version.ID != goal.ID || version.Revision != int64(index+1) || output != entry.output {
			t.Fatalf("goal receipt %s: version=%+v output=%q err=%v", entry.call.Name, version, output, err)
		}
		changed := entry.call
		changed.Arguments = strings.Replace(changed.Arguments, "}", `,"unexpected":true}`, 1)
		if _, _, err := broker.ConfirmPersonalContextWrite(ctx, claimed, fence, 4, make([]byte, 32), changed); err == nil {
			t.Fatalf("changed %s arguments reused a receipt", entry.call.Name)
		}
	}
	goalTaskStore := broker.store.(*cloudDSHGoalTaskStore)
	for _, event := range goalTaskStore.committed {
		if strings.Contains(string(event.Payload), "synthetic private goal") || strings.Contains(string(event.Payload), "private body") {
			t.Fatalf("goal content leaked into Task event: %s", event.Payload)
		}
	}
	if store.mutationCalls != 3 {
		t.Fatalf("receipt confirmation unexpectedly wrote goal data: mutations=%d", store.mutationCalls)
	}
	delete(store.mutations, claimed.OwnerID+"\x00"+cloudDSHPersonalMutationID(claimed.ID, claimed.AgentSessionID, update.CallID))
	if _, _, err := broker.ConfirmPersonalContextWrite(ctx, claimed, fence, 4, make([]byte, 32), update); err == nil {
		t.Fatal("goal Task event without personal mutation receipt was trusted")
	}
}

func TestCloudDSHScheduleToolsUseScheduleServiceIdempotencyAndActionFlow(t *testing.T) {
	broker, _, claimed, _, store := cloudDSHGoalScheduleTaskFixture(t)
	runAt := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	create := ModelToolCall{CallID: "call_schedule_create_1", Name: cloudDSHScheduleCreateToolName,
		Arguments: fmt.Sprintf(`{"title":"每日复盘","objective":"整理今天的工作","timing":{"kind":"once","run_at":%q,"starts_on":"","local_time":"","time_zone":"UTC","weekdays":[],"interval_hours":0,"anchor_at":""}}`, runAt)}
	first, err := broker.execute(context.Background(), claimed, create, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := broker.execute(context.Background(), claimed, create, true, &first.Output)
	if err != nil {
		t.Fatal(err)
	}
	if first.Output != second.Output || len(store.values) != 1 || store.createCalls != 2 || store.createRecords[0].Key != store.createRecords[1].Key {
		t.Fatalf("schedule create replay output=%s/%s schedules=%d creates=%d", first.Output, second.Output, len(store.values), store.createCalls)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(first.Output), &created); err != nil {
		t.Fatal(err)
	}
	scheduleID, ok := created["id"].(string)
	if !ok || !schedule.ValidID(scheduleID) {
		t.Fatalf("schedule create result = %s", first.Output)
	}
	list := ModelToolCall{CallID: "call_schedule_list_1", Name: cloudDSHScheduleListToolName,
		Arguments: `{"status":"all","limit":10,"cursor":""}`}
	if _, err := broker.execute(context.Background(), claimed, list, false, nil); err != nil || store.listOwner != claimed.OwnerID {
		t.Fatalf("schedule list err=%v owner=%q", err, store.listOwner)
	}
	update := ModelToolCall{CallID: "call_schedule_update_1", Name: cloudDSHScheduleUpdateToolName,
		Arguments: fmt.Sprintf(`{"schedule_id":%q,"expected_version":1,"title":"更新后的复盘","objective":null,"timing":null}`, scheduleID)}
	if _, err := broker.execute(context.Background(), claimed, update, false, nil); err != nil || store.updateCalls != 1 {
		t.Fatalf("schedule update err=%v update calls=%d", err, store.updateCalls)
	}
	action := ModelToolCall{CallID: "call_schedule_action_1", Name: cloudDSHScheduleActionToolName,
		Arguments: fmt.Sprintf(`{"schedule_id":%q,"expected_version":2,"action":"disable","current_run_action":""}`, scheduleID)}
	if _, err := broker.execute(context.Background(), claimed, action, false, nil); err != nil || store.setStatusCalls != 1 || store.statusTarget != schedule.StatusDisabled {
		t.Fatalf("schedule action err=%v calls=%d target=%q", err, store.setStatusCalls, store.statusTarget)
	}
	runs := ModelToolCall{CallID: "call_schedule_runs_1", Name: cloudDSHScheduleRunsToolName,
		Arguments: fmt.Sprintf(`{"schedule_id":%q,"limit":20,"cursor":""}`, scheduleID)}
	if _, err := broker.execute(context.Background(), claimed, runs, false, nil); err != nil || store.runsOwner != claimed.OwnerID || store.runsID != scheduleID {
		t.Fatalf("schedule run-history err=%v owner=%q id=%q", err, store.runsOwner, store.runsID)
	}
}

func TestCloudDSHGoalAndScheduleToolsFailClosedWhenUnassembledOrProjectScoped(t *testing.T) {
	broker, store, claimed, goalStore, scheduleStore := cloudDSHGoalScheduleTaskFixture(t)
	goalService, scheduleService := broker.goals, broker.schedules
	broker.goals, broker.schedules = nil, nil
	definitions, err := broker.Definitions(context.Background(), claimed)
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range definitions {
		if cloudDSHGoalScheduleToolName(definition.Name) {
			t.Fatalf("unassembled broker exposed tool %q", definition.Name)
		}
	}
	for _, name := range []string{cloudDSHGoalCreateToolName, cloudDSHScheduleCreateToolName} {
		if _, err := broker.execute(context.Background(), claimed, ModelToolCall{CallID: "call_unassembled_1", Name: name,
			Arguments: `{}`}, false, nil); domain.CodeOf(err) != domain.CodeForbidden {
			t.Fatalf("unassembled %s error = %v, want forbidden", name, err)
		}
	}
	if goalStore.mutationCalls != 0 || scheduleStore.createCalls != 0 {
		t.Fatal("unassembled services performed mutations")
	}

	projectClaimed := claimed
	projectClaimed.ProjectID = "p1234567"
	projectClaimed.ConversationContext = &task.ConversationContext{OwnerID: claimed.OwnerID, InitiatorID: claimed.OwnerID,
		ProjectID: projectClaimed.ProjectID, ConversationID: claimed.ConversationID, Scope: "project"}
	store.details.Task.ProjectID = projectClaimed.ProjectID
	store.details.Task.ConversationContext = projectClaimed.ConversationContext
	broker.goals, broker.schedules = goalService, scheduleService
	if _, err := broker.execute(context.Background(), projectClaimed, ModelToolCall{CallID: "call_project_1", Name: cloudDSHGoalCreateToolName,
		Arguments: `{"title":"x","description":"","status":"active","steps":[],"related_material_ids":[]}`}, false, nil); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("project goal tool error = %v, want forbidden", err)
	}
	if _, err := broker.execute(context.Background(), projectClaimed, ModelToolCall{CallID: "call_project_2", Name: cloudDSHScheduleCreateToolName,
		Arguments: `{}`}, false, nil); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("project schedule tool error = %v, want forbidden", err)
	}
}

func TestCloudDSHScheduleToolsRejectClientWithoutScheduleGrant(t *testing.T) {
	broker, store, claimed, _, scheduleStore := cloudDSHGoalScheduleTaskFixture(t)
	expiresAt := time.Now().Add(time.Hour).UTC()
	claimed.Authorization = task.Authorization{Kind: string(identity.KindClient), TokenID: "t1234567", Version: 2,
		ClientInstallationID: "i1234567", ExpiresAt: &expiresAt, Scopes: []string{string(identity.ScopeTaskRead)}}
	store.details.Task.Authorization = claimed.Authorization
	definitions, err := broker.Definitions(context.Background(), claimed)
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range definitions {
		if strings.HasPrefix(definition.Name, "morde_schedule_") {
			t.Fatalf("client without schedule grant exposed tool %q", definition.Name)
		}
	}
	if _, err := broker.execute(context.Background(), claimed, ModelToolCall{CallID: "call_schedule_denied", Name: cloudDSHScheduleCreateToolName,
		Arguments: `{}`}, false, nil); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("schedule tool error = %v, want forbidden", err)
	}
	if scheduleStore.createCalls != 0 {
		t.Fatal("unauthorized schedule call reached ScheduleService store")
	}
}

func TestCloudDSHScheduleCreateRejectsInvalidTimingBeforeStoreMutation(t *testing.T) {
	broker, _, claimed, _, store := cloudDSHGoalScheduleTaskFixture(t)
	call := ModelToolCall{CallID: "call_schedule_invalid", Name: cloudDSHScheduleCreateToolName,
		Arguments: `{"title":"","objective":"check","timing":{"kind":"daily","run_at":"","starts_on":"2026-11-06","local_time":"09:00","time_zone":"Not/AZone","weekdays":[],"interval_hours":0,"anchor_at":""}}`}
	if _, err := broker.execute(context.Background(), claimed, call, false, nil); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("invalid schedule timing error = %v, want invalid argument", err)
	}
	if store.createCalls != 0 {
		t.Fatal("invalid schedule timing reached schedule store")
	}
}
