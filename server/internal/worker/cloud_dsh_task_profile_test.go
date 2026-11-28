package worker

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

func TestCloudDSHTaskProfileToolDefinitionsFiltersWebResearchTools(t *testing.T) {
	definitions := []app.AgentToolDefinition{
		profileTestTool("file_read"),
		profileTestTool("morde_personal_object_update"),
		profileTestTool(cloudDSHWebResearchSearchToolName),
		profileTestTool("ask_user"),
		profileTestTool(cloudDSHWebResearchReadToolName),
		profileTestTool("ask_user_question"),
		profileTestTool("dsh_delegate"),
	}
	value := task.Task{ConversationContext: &task.ConversationContext{
		AgentToolProfile: task.AgentToolProfileWebResearch,
	}}

	got, allowChildren, err := cloudDSHTaskProfileToolDefinitions(value, definitions)
	if err != nil {
		t.Fatalf("apply web research profile: %v", err)
	}
	if allowChildren {
		t.Fatal("web research profile must disable delegation and background child Agents")
	}
	wantNames := []string{
		cloudDSHWebResearchSearchToolName,
		"ask_user",
		cloudDSHWebResearchReadToolName,
		"ask_user_question",
	}
	if gotNames := profileToolNames(got); !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("filtered tool names = %v, want %v", gotNames, wantNames)
	}
	if !reflect.DeepEqual(got[0], definitions[2]) || !reflect.DeepEqual(got[2], definitions[4]) {
		t.Fatal("profile must retain the exact Broker-authorized search and read definitions")
	}
}

func TestCloudDSHTaskProfileToolDefinitionsRequiresBothAuthorizedWebTools(t *testing.T) {
	tests := []struct {
		name        string
		definitions []app.AgentToolDefinition
	}{
		{
			name: "missing search",
			definitions: []app.AgentToolDefinition{
				profileTestTool(cloudDSHWebResearchReadToolName),
			},
		},
		{
			name: "missing read",
			definitions: []app.AgentToolDefinition{
				profileTestTool(cloudDSHWebResearchSearchToolName),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := task.Task{ConversationContext: &task.ConversationContext{
				AgentToolProfile: task.AgentToolProfileWebResearch,
			}}
			got, allowChildren, err := cloudDSHTaskProfileToolDefinitions(value, test.definitions)
			if !errors.Is(err, errCloudDSHWebResearchToolUnavailable) {
				t.Fatalf("error = %v, want required web research tool unavailable", err)
			}
			if got != nil || allowChildren {
				t.Fatalf("missing required tool returned definitions=%v allowChildren=%t", profileToolNames(got), allowChildren)
			}
		})
	}
}

func TestCloudDSHTaskProfileToolDefinitionsLeavesLaterRevisionUnchanged(t *testing.T) {
	revision := int64(1)
	definitions := []app.AgentToolDefinition{
		profileTestTool("file_read"),
		profileTestTool(cloudDSHWebResearchSearchToolName),
		profileTestTool(cloudDSHWebResearchReadToolName),
		profileTestTool("dsh_delegate"),
	}
	value := task.Task{
		CurrentRevision: &revision,
		ConversationContext: &task.ConversationContext{
			AgentToolProfile: task.AgentToolProfileWebResearch,
		},
	}

	got, allowChildren, err := cloudDSHTaskProfileToolDefinitions(value, definitions)
	if err != nil {
		t.Fatalf("later revision must retain the original tools: %v", err)
	}
	if !allowChildren {
		t.Fatal("later revisions must not inherit the initial web research child restriction")
	}
	if !reflect.DeepEqual(got, definitions) {
		t.Fatalf("later revision tool definitions changed: got %v, want %v", profileToolNames(got), profileToolNames(definitions))
	}
}

func TestCloudDSHTaskProfileToolDefinitionsLeavesOrdinaryTasksUnchanged(t *testing.T) {
	definitions := []app.AgentToolDefinition{
		profileTestTool("file_read"),
		profileTestTool(cloudDSHWebResearchSearchToolName),
		profileTestTool("dsh_delegate"),
	}

	for _, value := range []task.Task{
		{},
		{ConversationContext: &task.ConversationContext{}},
	} {
		got, allowChildren, err := cloudDSHTaskProfileToolDefinitions(value, definitions)
		if err != nil {
			t.Fatalf("ordinary task tool definitions: %v", err)
		}
		if !allowChildren {
			t.Fatal("ordinary tasks must retain delegation and background child policy")
		}
		if !reflect.DeepEqual(got, definitions) {
			t.Fatalf("ordinary task tool definitions changed: got %v, want %v", profileToolNames(got), profileToolNames(definitions))
		}
	}
}

func TestCloudDSHTaskProfileToolDefinitionsAppliesAtExplicitZeroRevision(t *testing.T) {
	zero := int64(0)
	definitions := []app.AgentToolDefinition{
		profileTestTool("file_read"),
		profileTestTool(cloudDSHWebResearchSearchToolName),
		profileTestTool(cloudDSHWebResearchReadToolName),
	}
	value := task.Task{
		CurrentRevision: &zero,
		ConversationContext: &task.ConversationContext{
			AgentToolProfile: task.AgentToolProfileWebResearch,
		},
	}

	got, allowChildren, err := cloudDSHTaskProfileToolDefinitions(value, definitions)
	if err != nil || allowChildren || !reflect.DeepEqual(profileToolNames(got), []string{
		cloudDSHWebResearchSearchToolName, cloudDSHWebResearchReadToolName,
	}) {
		t.Fatalf("explicit zero revision profile result: tools=%v allowChildren=%t err=%v", profileToolNames(got), allowChildren, err)
	}
}

func TestCloudDSHTaskProfileSkipsPersonalContextOnlyForInitialWebResearch(t *testing.T) {
	revision := int64(1)
	for _, test := range []struct {
		name  string
		value task.Task
		want  bool
	}{
		{
			name: "initial web research",
			value: task.Task{ConversationContext: &task.ConversationContext{
				Scope:            "personal",
				AgentToolProfile: task.AgentToolProfileWebResearch,
			}},
			want: true,
		},
		{
			name: "followup uses normal personal context",
			value: task.Task{CurrentRevision: &revision, ConversationContext: &task.ConversationContext{
				Scope:            "personal",
				AgentToolProfile: task.AgentToolProfileWebResearch,
			}},
		},
		{
			name: "project context remains available",
			value: task.Task{ConversationContext: &task.ConversationContext{
				Scope:            "project",
				AgentToolProfile: task.AgentToolProfileWebResearch,
			}},
		},
		{
			name:  "ordinary task uses normal personal context",
			value: task.Task{ConversationContext: &task.ConversationContext{}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := cloudDSHTaskProfileSkipsInitialPersonalContext(test.value); got != test.want {
				t.Fatalf("skip initial personal context = %t, want %t", got, test.want)
			}
		})
	}
}

func TestCloudDSHTaskProfileToolDefinitionsRejectsUnknownInitialProfile(t *testing.T) {
	value := task.Task{ConversationContext: &task.ConversationContext{AgentToolProfile: "unknown"}}
	got, allowChildren, err := cloudDSHTaskProfileToolDefinitions(value, []app.AgentToolDefinition{
		profileTestTool(cloudDSHWebResearchSearchToolName),
		profileTestTool(cloudDSHWebResearchReadToolName),
	})
	if err == nil || got != nil || allowChildren {
		t.Fatalf("unknown profile returned definitions=%v allowChildren=%t err=%v", profileToolNames(got), allowChildren, err)
	}
	revision := int64(1)
	value.CurrentRevision = &revision
	if got, allowChildren, err := cloudDSHTaskProfileToolDefinitions(value, []app.AgentToolDefinition{
		profileTestTool(cloudDSHWebResearchSearchToolName),
		profileTestTool(cloudDSHWebResearchReadToolName),
	}); err == nil || got != nil || allowChildren {
		t.Fatalf("unknown later-revision profile returned definitions=%v allowChildren=%t err=%v", profileToolNames(got), allowChildren, err)
	}
}

func profileTestTool(name string) app.AgentToolDefinition {
	return app.AgentToolDefinition{ModelTool: app.ModelTool{
		Name:        name,
		Description: "authorized test tool " + name,
		Parameters:  []byte(`{"type":"object","properties":{},"additionalProperties":false}`),
	}}
}

func profileToolNames(definitions []app.AgentToolDefinition) []string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	return names
}

type cloudDSHProfileNoChildStore struct{ listCalls int }

func (*cloudDSHProfileNoChildStore) Create(context.Context, app.AgentChildSessionCommand) (app.AgentChildSession, bool, error) {
	return app.AgentChildSession{}, false, errors.New("unexpected child creation")
}

func (store *cloudDSHProfileNoChildStore) List(context.Context, app.AgentInstanceFence, string) ([]app.AgentChildSession, error) {
	store.listCalls++
	return nil, nil
}

func (*cloudDSHProfileNoChildStore) Get(context.Context, app.AgentInstanceFence, string, string) (app.AgentChildSession, error) {
	return app.AgentChildSession{}, errors.New("unexpected child lookup")
}

func (*cloudDSHProfileNoChildStore) RequestCancel(context.Context, app.AgentChildSessionCancelCommand) (app.AgentChildSession, bool, error) {
	return app.AgentChildSession{}, false, errors.New("unexpected child cancellation")
}

func (*cloudDSHProfileNoChildStore) Settle(context.Context, app.AgentChildSessionSettleCommand) (app.AgentChildSession, bool, error) {
	return app.AgentChildSession{}, false, errors.New("unexpected child settlement")
}
