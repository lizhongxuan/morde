package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

type cloudDSHPersonalSnapshotStub struct {
	principal identity.Principal
	items     []personal.Object
	calls     int
}

type cloudDSHPersonalCommitTaskStore struct {
	*cloudDSHBrokerTaskStore
	commits []CloudDSHPersonalMutationCommit
}

func (s *cloudDSHPersonalCommitTaskStore) CommitTaskProgress(ctx context.Context, progress TaskProgress) (task.Event, error) {
	if progress.CloudDSHPersonalMutation != nil {
		if !ValidCloudDSHPersonalMutationCommit(*progress.CloudDSHPersonalMutation) ||
			ValidateCloudDSHPersonalMutationEvent(progress.Event.Payload, *progress.CloudDSHPersonalMutation) != nil {
			return task.Event{}, domain.NewError(domain.CodeInvalidState, "invalid personal mutation event")
		}
		s.commits = append(s.commits, *progress.CloudDSHPersonalMutation)
	}
	return s.cloudDSHBrokerTaskStore.CommitTaskProgress(ctx, progress)
}

func (s *cloudDSHPersonalSnapshotStub) GetSnapshot(_ context.Context, principal identity.Principal, _ string, _ int32) (PersonalSnapshotPage, error) {
	s.calls++
	s.principal = principal
	return PersonalSnapshotPage{Watermark: 1, Items: s.items}, nil
}

func enabledMemorySetting() personal.Object {
	return personal.Object{ObjectType: personal.ObjectTypeSkillSetting, ObjectID: cloudDSHMemorySettingID,
		Revision: 1, Body: json.RawMessage(`{"enabled":true}`)}
}

func TestCloudDSHMemoryChangePauseIsVisibleAndIdempotent(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	broker := &CloudDSHToolBroker{store: store}
	claimed := store.details.Task
	for i := 0; i < 2; i++ {
		if err := broker.PausePersonalMemoryRecovery(context.Background(), claimed); err != nil {
			t.Fatalf("pause %d: %v", i, err)
		}
	}
	if store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != "dsh_recovery_unresolved" ||
		len(store.events) != 1 || store.events[0].Type != "task.waiting_user" || !strings.Contains(string(store.events[0].Payload), "请重新发送消息") {
		t.Fatalf("unexpected memory pause: task=%+v events=%+v", store.details.Task, store.events)
	}
}

func TestCloudDSHPersonalContextRequiresCurrentPersonalOwnerAuthorization(t *testing.T) {
	newID := func(t *testing.T, create func() (string, error)) string {
		t.Helper()
		value, err := create()
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	ownerID := newID(t, domain.NewUserID)
	otherID := newID(t, domain.NewUserID)
	conversationID := newID(t, domain.NewConversationID)
	taskID := newID(t, domain.NewTaskID)
	tokenID := newID(t, domain.NewTokenID)
	installationID := newID(t, domain.NewClientInstallationID)
	expiresAt := time.Now().Add(time.Hour)
	base := task.Task{
		ID: taskID, OwnerID: ownerID, InitiatorID: ownerID, ConversationID: conversationID,
		Authorization: task.Authorization{
			Kind: string(identity.KindSession), TokenID: tokenID, Version: 1, ExpiresAt: &expiresAt,
			Scopes: []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)},
		},
		ConversationContext: &task.ConversationContext{
			OwnerID: ownerID, InitiatorID: ownerID, ConversationID: conversationID, Scope: "personal",
		},
	}
	if !cloudDSHPersonalContextEligible(base) {
		t.Fatal("valid personal owner context was rejected")
	}

	tests := map[string]func(*task.Task){
		"cross-account owner": func(value *task.Task) { value.ConversationContext.OwnerID = otherID },
		"cross-account initiator": func(value *task.Task) {
			value.InitiatorID = otherID
			value.ConversationContext.InitiatorID = otherID
		},
		"project task":       func(value *task.Task) { value.ProjectID = "project_shared" },
		"project context":    func(value *task.Task) { value.ConversationContext.ProjectID = "project_shared" },
		"wrong conversation": func(value *task.Task) { value.ConversationContext.ConversationID = otherID },
		"project scope":      func(value *task.Task) { value.ConversationContext.Scope = "project" },
		"missing read scope": func(value *task.Task) {
			value.Authorization.Kind = string(identity.KindClient)
			value.Authorization.ClientInstallationID = installationID
			value.Authorization.Scopes = []string{string(identity.ScopeTaskSubmit)}
		},
		"expired authorization": func(value *task.Task) {
			expired := time.Now().Add(-time.Second)
			value.Authorization.ExpiresAt = &expired
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			value := base
			contextValue := *base.ConversationContext
			value.ConversationContext = &contextValue
			mutate(&value)
			if cloudDSHPersonalContextEligible(value) {
				t.Fatal("unauthorized personal context was accepted")
			}
		})
	}
}

func TestCloudDSHInitialContextLoadsAuthorizedProfileMemoryAndGoals(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	store.details.Task.HostID = ""
	store.details.Task.ExecutionLocation = "server"
	store.details.Task.InitiatorID = store.details.Task.OwnerID
	store.details.Task.ConversationID = "c1234567"
	store.details.Task.AgentSessionID = "session_personal_context"
	store.details.Task.Authorization = task.Authorization{
		Kind: string(identity.KindSession), TokenID: "t1234567", Version: 1,
		Scopes: []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)},
	}
	expiresAt := time.Now().Add(time.Hour)
	store.details.Task.Authorization.ExpiresAt = &expiresAt
	store.details.Task.ConversationContext = &task.ConversationContext{
		OwnerID: store.details.Task.OwnerID, InitiatorID: store.details.Task.OwnerID,
		ConversationID: store.details.Task.ConversationID, Scope: "personal",
	}
	reader := &cloudDSHPersonalSnapshotStub{items: []personal.Object{
		{ObjectType: personal.ObjectTypeProfile, ObjectID: personal.ProfileObjectID, Revision: 1,
			Body: json.RawMessage(`{"name":"Muse","avatar":"dragon"}`)},
		{ObjectType: personal.ObjectTypeMemory, ObjectID: "global", Revision: 2,
			Body: json.RawMessage(`{"content":"prefers concise explanations"}`)},
		{ObjectType: personal.ObjectTypeSoul, ObjectID: "default", Revision: 1,
			Body: json.RawMessage(`{"name":"Muse","content":"Be careful with private data."}`)},
	}}
	goalStore := &goalListStub{result: GoalPage{Items: []personal.Object{
		{ObjectType: personal.ObjectTypeGoal, ObjectID: "g_context1", Revision: 3,
			Body: json.RawMessage(`{"title":"Learn Go","status":"active","steps":[]}`)},
	}}}
	goals, err := NewGoalService(&goalMutationStub{}, goalStore)
	if err != nil {
		t.Fatal(err)
	}
	settingStore := &personalMutationStoreStub{object: enabledMemorySetting()}
	settingStore.objects = map[string]personal.Object{
		"skill_setting/builtin_memory":        enabledMemorySetting(),
		"memory/global":                       reader.items[1],
		"profile/" + personal.ProfileObjectID: reader.items[0],
		"soul/default":                        reader.items[2],
		"goal/g_context1":                     goalStore.result.Items[0],
	}
	personalData, err := NewPersonalMutationService(settingStore)
	if err != nil {
		t.Fatal(err)
	}
	broker := &CloudDSHToolBroker{store: store, personalSync: reader, personalData: personalData, goals: goals}
	claimed := store.details.Task
	store.details.Task.ConversationContext = nil
	contextValue, err := broker.InitialContext(context.Background(), claimed)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Muse", "prefers concise explanations", "Be careful with private data", "Learn Go"} {
		if !strings.Contains(contextValue, want) {
			t.Errorf("personal context omitted %q: %s", want, contextValue)
		}
	}
	proof := strings.TrimPrefix(contextValue[strings.LastIndex(contextValue, "\n")+1:], CloudDSHMemoryProofPrefix)
	if proof == "" || strings.Contains(proof, "concise") || broker.ValidatePersonalMemoryProof(context.Background(), claimed, proof, nil) != nil {
		t.Fatalf("initial memory proof was missing, exposed text, or invalid: %q", proof)
	}
	for _, key := range []string{"profile/" + personal.ProfileObjectID, "soul/default", "goal/g_context1"} {
		original := settingStore.objects[key]
		delete(settingStore.objects, key)
		if err := broker.ValidatePersonalMemoryProof(context.Background(), claimed, proof, nil); domain.CodeOf(err) != domain.CodeInvalidState {
			t.Fatalf("deleted %s remained in a resumable personal context: %v", key, err)
		}
		updatedObject := original
		updatedObject.Revision++
		settingStore.objects[key] = updatedObject
		if err := broker.ValidatePersonalMemoryProof(context.Background(), claimed, proof, nil); domain.CodeOf(err) != domain.CodeInvalidState {
			t.Fatalf("changed %s remained in a resumable personal context: %v", key, err)
		}
		if err := broker.ValidatePersonalMemoryProof(context.Background(), claimed, proof, []CloudDSHMemoryVersion{{
			ObjectType: original.ObjectType, ID: original.ObjectID, Revision: updatedObject.Revision,
		}}); err != nil {
			t.Fatalf("confirmed in-task change to %s was rejected: %v", key, err)
		}
		settingStore.objects[key] = original
	}
	legacyProof := base64.RawURLEncoding.EncodeToString([]byte(`{"enabled":true,"revision":1,"items":[]}`))
	if err := broker.ValidatePersonalMemoryProof(context.Background(), claimed, legacyProof, nil); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("old proof without personal object versions was accepted: %v", err)
	}
	updated := reader.items[1]
	updated.Revision = 3
	settingStore.objects["memory/global"] = updated
	if err := broker.ValidatePersonalMemoryProof(context.Background(), claimed, proof, nil); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("changed memory was accepted: %v", err)
	}
	if err := broker.ValidatePersonalMemoryProof(context.Background(), claimed, proof, []CloudDSHMemoryVersion{{ID: "global", Revision: 3}}); err != nil {
		t.Fatalf("confirmed in-task memory update was rejected: %v", err)
	}
	delete(settingStore.objects, "memory/global")
	if err := broker.ValidatePersonalMemoryProof(context.Background(), claimed, proof, []CloudDSHMemoryVersion{{ID: "global", Revision: 3}}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("deleted memory was accepted: %v", err)
	}
	settingStore.objects["memory/global"] = reader.items[1]
	if reader.calls != 1 || reader.principal.UserID != store.details.Task.OwnerID || goalStore.calls != 1 ||
		goalStore.principal.UserID != store.details.Task.OwnerID || goalStore.page.Limit != 20 {
		t.Fatalf("context reads were not owner-scoped/bounded: snapshot=%+v goals=%+v", reader, goalStore)
	}
	disabledSetting := enabledMemorySetting()
	disabledSetting.Body = json.RawMessage(`{"enabled":false}`)
	settingStore.objects["skill_setting/builtin_memory"] = disabledSetting
	if err := broker.ValidatePersonalMemoryProof(context.Background(), claimed, proof, nil); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("previously authorized memory remained valid after consent was revoked: %v", err)
	}
	withoutMemory, err := broker.InitialContext(context.Background(), claimed)
	if err != nil || strings.Contains(withoutMemory, "prefers concise explanations") || !strings.Contains(withoutMemory, "Be careful with private data") {
		t.Fatalf("disabled memory leaked or affected SOUL: %q, %v", withoutMemory, err)
	}
	delete(settingStore.objects, "skill_setting/builtin_memory")
	withoutMemory, err = broker.InitialContext(context.Background(), claimed)
	if err != nil || strings.Contains(withoutMemory, "prefers concise explanations") {
		t.Fatalf("missing setting did not default to disabled: %q, %v", withoutMemory, err)
	}
	store.details.Task.Authorization.Version++
	if _, err := broker.InitialContext(context.Background(), claimed); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("changed authorization accepted the claimed snapshot: %v", err)
	}
	store.details.Task.Authorization.Version--
	foreignClaim := claimed
	foreignContext := *claimed.ConversationContext
	foreignContext.OwnerID = "u8765432"
	foreignClaim.ConversationContext = &foreignContext
	if _, err := broker.currentTaskDetails(context.Background(), foreignClaim); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("foreign conversation context accepted: %v", err)
	}
	store.details.Task.ConversationContext = claimed.ConversationContext
	projectStore := &projectServiceStoreStub{
		members: map[string]project.Member{
			store.details.Task.InitiatorID: {ProjectID: "j1234567", UserID: store.details.Task.InitiatorID, Role: project.RoleMember, Version: 1},
		},
		contextItems: []project.Content{
			{ProjectID: "j1234567", Kind: project.ContentMemory, ID: "global", Revision: 1, Body: json.RawMessage(`{"content":"shared project memory"}`)},
			{ProjectID: "j1234567", Kind: project.ContentDocument, ID: "doc-1", Revision: 2, Body: json.RawMessage(`{"title":"Roadmap","content":"shared project document"}`)},
		},
	}
	projects, err := NewProjectService(projectStore)
	if err != nil {
		t.Fatal(err)
	}
	broker.projectContext = projects
	store.details.Task.ProjectID = "j1234567"
	projectConversationContext := *store.details.Task.ConversationContext
	projectConversationContext.ProjectID = "j1234567"
	projectConversationContext.Scope = "project"
	store.details.Task.ConversationContext = &projectConversationContext
	projectContext, err := broker.InitialContext(context.Background(), store.details.Task)
	if err != nil || !strings.Contains(projectContext, "shared project memory") || !strings.Contains(projectContext, "shared project document") ||
		strings.Contains(projectContext, "prefers concise explanations") || projectStore.contextPrincipal.UserID != store.details.Task.InitiatorID ||
		reader.calls != 3 || goalStore.calls != 3 {
		t.Fatalf("project task context = %q err=%v store=%+v personal calls=%d/%d", projectContext, err, projectStore, reader.calls, goalStore.calls)
	}
	delete(projectStore.members, store.details.Task.InitiatorID)
	if _, err := broker.InitialContext(context.Background(), store.details.Task); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("revoked project member read context: %v", err)
	}
}

func TestCloudDSHToolBrokerRoutesAuthorizedPersonalUpdateThroughTaskProgress(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	baseStore := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = baseStore
	baseStore.details.Task.HostID = ""
	baseStore.details.Task.ExecutionLocation = "server"
	baseStore.details.Task.InitiatorID = baseStore.details.Task.OwnerID
	baseStore.details.Task.ConversationID = "c1234567"
	baseStore.details.Task.AgentSessionID = "session_personal_update"
	baseStore.details.Task.Authorization = task.Authorization{
		Kind: string(identity.KindSession), TokenID: "t1234567", Version: 1,
		Scopes: []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)},
	}
	expiresAt := time.Now().Add(time.Hour)
	baseStore.details.Task.Authorization.ExpiresAt = &expiresAt
	baseStore.details.Task.ConversationContext = &task.ConversationContext{
		OwnerID: baseStore.details.Task.OwnerID, InitiatorID: baseStore.details.Task.OwnerID,
		ConversationID: baseStore.details.Task.ConversationID, Scope: "personal",
	}
	store := &cloudDSHPersonalCommitTaskStore{cloudDSHBrokerTaskStore: baseStore}
	mutationStore := &personalMutationStoreStub{object: enabledMemorySetting()}
	personalData, err := NewPersonalMutationService(mutationStore)
	if err != nil {
		t.Fatal(err)
	}
	reader := &cloudDSHPersonalSnapshotStub{}
	goalStore := &goalListStub{}
	goals, err := NewGoalService(&goalMutationStub{}, goalStore)
	if err != nil {
		t.Fatal(err)
	}
	broker := &CloudDSHToolBroker{store: store, runtime: fixture.executor, personalData: personalData, personalSync: reader, goals: goals}
	claimed := store.details.Task
	definitions, err := broker.Definitions(context.Background(), claimed)
	if err != nil {
		t.Fatal(err)
	}
	var updateDefinition *AgentToolDefinition
	for index := range definitions {
		if definitions[index].Name == cloudDSHPersonalUpdateToolName {
			updateDefinition = &definitions[index]
			break
		}
	}
	if updateDefinition == nil || updateDefinition.RequiredScope != identity.ScopeTaskSubmit {
		t.Fatalf("authorized personal update tool definition = %+v", updateDefinition)
	}
	body := `{"content":"private body must stay out of audit"}`
	call := ModelToolCall{CallID: "call_personal_update_1", Name: cloudDSHPersonalUpdateToolName,
		Arguments: `{"object_type":"memory","object_id":"global","base_revision":0,"body":"{\"content\":\"private body must stay out of audit\"}"}`}
	invalid := call
	invalid.Arguments = `{"object_type":"memory","object_id":"global","base_revision":0,"body":"[]"}`
	if _, err := broker.Execute(context.Background(), claimed, invalid); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("non-object encoded body accepted: %v", err)
	}
	result, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || result.Status != AgentToolSucceeded || result.Output != `{"object_type":"memory","object_id":"global","revision":1}` {
		t.Fatalf("personal update result = %+v, err=%v", result, err)
	}
	if len(store.commits) != 1 || store.details.Task.Usage.ToolCalls != 1 || len(store.events) != 1 || mutationStore.calls != 0 {
		t.Fatalf("personal update bypassed Task transaction: commits=%d usage=%+v events=%d direct mutations=%d",
			len(store.commits), store.details.Task.Usage, len(store.events), mutationStore.calls)
	}
	if strings.Contains(string(store.events[0].Payload), body) || strings.Contains(result.Output, body) {
		t.Fatalf("personal update audit/receipt leaked the body: event=%s output=%s", store.events[0].Payload, result.Output)
	}
	if string(store.commits[0].Mutation.Body) != body {
		t.Fatalf("encoded body was not decoded into the personal mutation")
	}
	call.CallID = "call_personal_update_legacy"
	call.Arguments = `{"object_type":"memory","object_id":"global","base_revision":0,"body":{"content":"private body must stay out of audit"}}`
	if _, err := broker.Execute(context.Background(), claimed, call); err != nil {
		t.Fatalf("legacy object body was not accepted: %v", err)
	}
	mutationStore.object.Body = json.RawMessage(`{"enabled":false}`)
	call.CallID = "call_disabled_memory"
	if _, err := broker.Execute(context.Background(), claimed, call); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("disabled memory write was accepted: %v", err)
	}
	if len(store.commits) != 2 {
		t.Fatalf("disabled memory reached transaction: %d commits", len(store.commits))
	}
	soul := ModelToolCall{CallID: "call_soul_update", Name: cloudDSHPersonalUpdateToolName,
		Arguments: `{"object_type":"soul","object_id":"default","base_revision":0,"body":"{\"name\":\"Morde\",\"content\":\"Be helpful.\"}"}`}
	if _, err := broker.Execute(context.Background(), claimed, soul); err != nil {
		t.Fatalf("SOUL update with memory disabled: %v", err)
	}
	if !strings.Contains(string(store.events[len(store.events)-1].Payload), "已更新 SOUL") {
		t.Fatalf("SOUL update did not notify via task activity: %s", store.events[len(store.events)-1].Payload)
	}
}
