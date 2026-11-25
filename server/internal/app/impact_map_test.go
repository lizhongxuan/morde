package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"

	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

// memoryImpactFileIndex is the in-memory FileIndex fake for impact tests: no
// database, no DSH, no real workspace.
type memoryImpactFileIndex struct {
	files map[string]string
}

func (m memoryImpactFileIndex) Files() []string {
	paths := make([]string, 0, len(m.files))
	for path := range m.files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func (m memoryImpactFileIndex) ReadFile(path string) (string, error) {
	content, ok := m.files[path]
	if !ok {
		return "", errors.New("file not found")
	}
	return content, nil
}

func goImpactFixture() memoryImpactFileIndex {
	return memoryImpactFileIndex{files: map[string]string{
		"internal/core/engine.go": `package core

import (
	"internal/store"
	"morde/server/internal/store"
	"internal/util"
)

type Engine struct{}

func RunEngine() {}

func (e *Engine) Start() {}
`,
		"internal/store/store.go": `package store

type Store struct{}

func OpenStore() *Store { return &Store{} }
`,
		"internal/store/store_helper.go": `package store

func FormatStore() string { return "" }
`,
		"internal/util/util.go": `package util

func Format() string { return "" }
`,
		"internal/app/runner.go": `package app

import "internal/core"

func Run() { core.RunEngine() }
`,
		"internal/app/helper.go": `package app

import "internal/store"

func Help() { store.OpenStore() }
`,
		"internal/other/other.go": `package other

func Other() {}
`,
		"internal/core/engine_test.go": `package core

func TestEngine() {}
`,
		"internal/app/runner_test.go": `package app

import "internal/core"

func TestRunner() { core.RunEngine() }
`,
		"internal/other/other_test.go": `package other

func TestOther() {}
`,
	}}
}

func scriptImpactFixture() memoryImpactFileIndex {
	return memoryImpactFileIndex{files: map[string]string{
		"src/foo.ts": `import { bar } from './bar';
import { helper } from './lib/helper';

export function Foo() { return bar() + helper(); }
export class FooWidget {}
`,
		"src/bar.ts": `export function bar() { return 1; }
`,
		"src/lib/helper.ts": `export function helper() { return 2; }
`,
		"src/app.ts": `import { Foo } from './foo';

export function run() { return Foo(); }
`,
		"src/other.ts": `export function other() { return 0; }
`,
		"src/foo.test.ts": `import { Foo } from './foo';
`,
		"src/other.test.ts": ``,
		"test/app.spec.ts": `import { Foo } from '../src/foo';
`,
	}}
}

func TestAnalyzeImpactMap(t *testing.T) {
	tests := []struct {
		name      string
		files     FileIndex
		component string
		want      ImpactMap
		wantErr   bool
	}{
		{
			name:      "go core component",
			files:     goImpactFixture(),
			component: "internal/core/engine.go",
			want: ImpactMap{Component: "internal/core/engine.go",
				Dependencies:  []string{"internal/store/store.go", "internal/store/store_helper.go", "internal/util/util.go"},
				Callers:       []string{"internal/app/runner.go"},
				CoveringTests: []string{"internal/app/runner_test.go", "internal/core/engine_test.go"},
			},
		},
		{
			name:      "typescript component",
			files:     scriptImpactFixture(),
			component: "src/foo.ts",
			want: ImpactMap{Component: "src/foo.ts",
				Dependencies:  []string{"src/bar.ts", "src/lib/helper.ts"},
				Callers:       []string{"src/app.ts"},
				CoveringTests: []string{"src/foo.test.ts", "src/other.test.ts", "test/app.spec.ts"},
			},
		},
		{
			name:      "component without references",
			files:     memoryImpactFileIndex{files: map[string]string{"pkg/solo.go": "package solo\n\nfunc Solo() {}\n"}},
			component: "pkg/solo.go",
			want:      ImpactMap{Component: "pkg/solo.go", Dependencies: []string{}, Callers: []string{}, CoveringTests: []string{}},
		},
		{
			name:      "empty workspace returns empty lists",
			files:     memoryImpactFileIndex{files: map[string]string{}},
			component: "pkg/solo.go",
			want:      ImpactMap{Component: "pkg/solo.go", Dependencies: []string{}, Callers: []string{}, CoveringTests: []string{}},
		},
		{
			name:      "component missing from workspace",
			files:     goImpactFixture(),
			component: "internal/missing.go",
			wantErr:   true,
		},
		{
			name:      "absolute component path",
			files:     goImpactFixture(),
			component: "/internal/core/engine.go",
			wantErr:   true,
		},
		{
			name:      "traversal component path",
			files:     goImpactFixture(),
			component: "../engine.go",
			wantErr:   true,
		},
		{
			name:      "empty component path",
			files:     goImpactFixture(),
			component: "",
			wantErr:   true,
		},
		{
			name:      "unclean component path",
			files:     goImpactFixture(),
			component: "internal/core/../core/engine.go",
			wantErr:   true,
		},
		{
			name:      "nil file index",
			files:     nil,
			component: "internal/core/engine.go",
			wantErr:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AnalyzeImpactMap(tc.files, tc.component)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestAnalyzeImpactMapDoesNotModifyIndex(t *testing.T) {
	fixture := goImpactFixture()
	before := make(map[string]string, len(fixture.files))
	for path, content := range fixture.files {
		before[path] = content
	}
	if _, err := AnalyzeImpactMap(fixture, "internal/core/engine.go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(fixture.files, before) {
		t.Fatal("impact analysis modified the file index")
	}
}

func TestAnalyzeImpactMapExternalDependenciesAreExcluded(t *testing.T) {
	files := memoryImpactFileIndex{files: map[string]string{
		"internal/core/engine.go": "package core\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n\t\"internal/util\"\n)\n\nfunc RunEngine() { fmt.Println(strings.TrimSpace(\"x\")) }\n",
		"internal/util/util.go":   "package util\n\nfunc Format() string { return \"\" }\n",
	}}
	got, err := AnalyzeImpactMap(files, "internal/core/engine.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ImpactMap{Component: "internal/core/engine.go",
		Dependencies: []string{"internal/util/util.go"}, Callers: []string{}, CoveringTests: []string{}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestCheckImpactCoverage(t *testing.T) {
	impact := ImpactMap{Component: "internal/core/engine.go",
		Dependencies:  []string{"internal/store/store.go", "internal/util/util.go"},
		Callers:       []string{"internal/app/runner.go"},
		CoveringTests: []string{"internal/core/engine_test.go"},
	}
	tests := []struct {
		name    string
		changed []string
		tested  []string
		want    ImpactCoverage
	}{
		{
			name:    "fully covered by changes",
			changed: []string{"internal/core/engine.go", "internal/store/store.go", "internal/util/util.go", "internal/app/runner.go", "internal/core/engine_test.go"},
			want: ImpactCoverage{
				Covered:   []string{"internal/app/runner.go", "internal/core/engine_test.go", "internal/store/store.go", "internal/util/util.go"},
				Uncovered: []string{},
			},
		},
		{
			name:    "covered by tests counts as covered",
			changed: []string{"internal/store/store.go"},
			tested:  []string{"internal/app/runner.go", "internal/core/engine_test.go"},
			want: ImpactCoverage{
				Covered:   []string{"internal/app/runner.go", "internal/core/engine_test.go", "internal/store/store.go"},
				Uncovered: []string{"internal/util/util.go"},
			},
		},
		{
			name:    "partially uncovered",
			changed: []string{"internal/store/store.go"},
			want: ImpactCoverage{
				Covered:   []string{"internal/store/store.go"},
				Uncovered: []string{"internal/app/runner.go", "internal/core/engine_test.go", "internal/util/util.go"},
			},
		},
		{
			name:    "changed file outside the impact surface is ignored",
			changed: []string{"internal/other/other.go"},
			want: ImpactCoverage{
				Covered:   []string{},
				Uncovered: []string{"internal/app/runner.go", "internal/core/engine_test.go", "internal/store/store.go", "internal/util/util.go"},
			},
		},
		{
			name:    "empty impact has nothing uncovered",
			changed: []string{"internal/other/other.go"},
			want:    ImpactCoverage{Covered: []string{}, Uncovered: []string{}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			subject := impact
			if tc.name == "empty impact has nothing uncovered" {
				subject = ImpactMap{Component: "internal/core/engine.go", Dependencies: []string{}, Callers: []string{}, CoveringTests: []string{}}
			}
			if got := CheckImpactCoverage(subject, tc.changed, tc.tested); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCloudDSHImpactMapDefinition(t *testing.T) {
	definition := cloudDSHImpactMapDefinition()
	if definition.Name != "morde_impact_map" {
		t.Fatalf("unexpected impact map tool name %q", definition.Name)
	}
	if definition.RequiredScope != identity.ScopeTaskRead {
		t.Fatalf("impact map must be read-only, got scope %q", definition.RequiredScope)
	}
	if _, err := validateAgentTools([]AgentToolDefinition{definition}); err != nil {
		t.Fatalf("impact map tool definition is invalid: %v", err)
	}
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
		t.Fatalf("impact map parameters are not valid JSON: %v", err)
	}
	if len(schema.Required) != 2 || schema.Required[0] != "expected_revision" || schema.Required[1] != "component" {
		t.Fatalf("impact map must require expected_revision and component, got %v", schema.Required)
	}
	if _, ok := schema.Properties["expected_revision"]; !ok {
		t.Fatal("impact map parameters are missing expected_revision")
	}
	if _, ok := schema.Properties["component"]; !ok {
		t.Fatal("impact map parameters are missing component")
	}
	if !goalFollowupReadOnlyToolAllowed(definition.Name) {
		t.Fatal("read-only impact map must stay available to read-only goal follow-up tasks")
	}
	deferred, ok := deferredCloudDSHImpactMapDefinition(task.Authorization{Kind: string(identity.KindClient), Scopes: []string{string(identity.ScopeTaskRead)}})
	if !ok {
		t.Fatal("deferred impact map definition was rejected for a read-scoped authorization")
	}
	if deferred.Name != definition.Name {
		t.Fatalf("deferred impact map has unexpected name %q", deferred.Name)
	}
	var deferredSchema struct {
		Properties map[string]struct {
			Minimum *int64 `json:"minimum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(deferred.Parameters, &deferredSchema); err != nil {
		t.Fatalf("deferred impact map parameters are not valid JSON: %v", err)
	}
	if minimum := deferredSchema.Properties["expected_revision"].Minimum; minimum == nil || *minimum != -1 {
		t.Fatalf("deferred impact map must allow expected_revision -1, got %v", minimum)
	}
	if _, ok := deferredCloudDSHImpactMapDefinition(task.Authorization{Kind: string(identity.KindClient), Scopes: []string{string(identity.ScopeTaskSubmit)}}); ok {
		t.Fatal("deferred impact map was offered without the read scope")
	}
}

func TestParseCloudDSHImpactMapInput(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    cloudDSHImpactMapInput
		wantErr bool
	}{
		{
			name: "valid arguments",
			raw:  `{"expected_revision":3,"component":"internal/core/engine.go"}`,
			want: cloudDSHImpactMapInput{ExpectedRevision: 3, Component: "internal/core/engine.go"},
		},
		{
			name:    "missing component",
			raw:     `{"expected_revision":3}`,
			wantErr: true,
		},
		{
			name:    "empty component",
			raw:     `{"expected_revision":3,"component":""}`,
			wantErr: true,
		},
		{
			name:    "negative revision",
			raw:     `{"expected_revision":-1,"component":"internal/core/engine.go"}`,
			wantErr: true,
		},
		{
			name:    "malformed json",
			raw:     `{"expected_revision":3,"component":}`,
			wantErr: true,
		},
		{
			name:    "empty arguments",
			raw:     ``,
			wantErr: true,
		},
		{
			name:    "unknown field is rejected",
			raw:     `{"expected_revision":3,"component":"internal/core/engine.go","write":true}`,
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCloudDSHImpactMapInput(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestExecuteImpactMapRequiresWorkspaceTools(t *testing.T) {
	broker := &CloudDSHToolBroker{}
	call := ModelToolCall{CallID: "call-impact", Name: cloudDSHImpactMapToolName,
		Arguments: `{"expected_revision":3,"component":"internal/core/engine.go"}`}
	if _, err := broker.executeImpactMap(t.Context(), task.Task{}, call, false, nil); err == nil {
		t.Fatal("impact map executed without workspace tools")
	}
}

func TestExecuteImpactMapPersistsMapForRegressionGate(t *testing.T) {
	const componentContent = "package auth\n\nfunc Session() string { return \"session\" }\n"
	const callerContent = "package auth\n\nfunc Login() string { return Session() }\n"
	const coveringContent = "package auth\n\nfunc TestSession() {}\n"
	component := syncServiceFile("auth/session.go", []byte(componentContent))
	caller := syncServiceFile("auth/login.go", []byte(callerContent))
	covering := syncServiceFile("auth/session_test.go", []byte(coveringContent))
	tools, _, blocks, current := newCloudDSHT08WorkspaceTools(t, []workspace.File{component, caller, covering}, nil, nil)
	blocks.data[component.SHA256] = []byte(componentContent)
	blocks.data[caller.SHA256] = []byte(callerContent)
	blocks.data[covering.SHA256] = []byte(coveringContent)
	fixture := newRuntimeToolExecutorFixture(t, nil)
	current.AgentInstanceID, current.AgentGeneration, current.AgentSessionID = "agent-workspace", 1, "session-root"
	current.CurrentRevision = int64PointerForWorkspaceTools(0)
	current.Authorization.Version = 1
	fixture.store.details.Task = current
	fixture.executor.workspaces = tools.workspaces
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := NewSealer(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.SetTaskStepDetailSealer(sealer); err != nil {
		t.Fatal(err)
	}
	call := ModelToolCall{CallID: "impact-call", Name: cloudDSHImpactMapToolName,
		Arguments: `{"expected_revision":0,"component":"auth/session.go"}`,
		TaskFence: &ModelTaskFence{TaskID: current.ID, OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
			LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version, AgentInstanceID: current.AgentInstanceID,
			AgentGeneration: current.AgentGeneration, AgentSessionID: current.AgentSessionID,
			AuthorizationVersion: current.Authorization.Version}}
	ctx := withCloudDSHToolActivity(context.Background(), current, call, sealer)
	result, err := broker.executeImpactMap(ctx, current, call, false, nil)
	if err != nil || result.Status != AgentToolSucceeded {
		t.Fatalf("impact map result = %#v, err = %v", result, err)
	}
	impact, ok := impactMapFromEvents(store.events)
	if !ok {
		t.Fatalf("no impact map recorded in events: %d events", len(store.events))
	}
	if impact.Component != "auth/session.go" || len(impact.Callers) != 1 || impact.Callers[0] != "auth/login.go" ||
		len(impact.CoveringTests) != 1 || impact.CoveringTests[0] != "auth/session_test.go" {
		t.Fatalf("recorded impact map = %#v; want the session surface", impact)
	}
}
