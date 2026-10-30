package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/config"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/runtimeclient"
)

func TestStreamableHTTPAuthToolsUseRuntimeExecutor(t *testing.T) {
	hosts := &hostServiceStub{value: host.Host{ID: "h1234567", OwnerID: "user_test", State: host.StateRunning, Workspaces: []workspace.Binding{{
		ID: "b1234567", Workspace: workspace.Workspace{ID: "w1234567", Name: "code", State: workspace.StateReady, Revision: 3, UsedBytes: 12, QuotaBytes: 1024},
		Directory: "w1234567", State: workspace.BindingPreparing, BindingVersion: 2, DesiredRevision: 3,
	}}}}
	runtime := &runtimeComputerToolsStub{result: app.MCPRuntimeToolResult{Output: json.RawMessage(`{"windows":[]}`)}}
	audits := &auditStub{}
	registry := prometheus.NewRegistry()
	handler, err := New(Deps{
		PreviewPortMin: 1024, PreviewPortDeny: []int{8080},
		Config:  config.MCP{Enabled: true, MaxOutputBytes: 1024, ScreenshotMaxWidth: 2},
		Domains: config.Domains{Scheme: "https", HostSuffix: "hosts.example.test"},
		Hosts:   hosts, Applications: newApplicationStatusServiceStub(), RuntimeTools: runtime, Auth: apiKeyPrincipalVerifier{principal: identity.Principal{
			UserID: "user_test", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "token_test",
			HostIDs: []string{"h1234567"}, Scopes: []identity.Scope{identity.ScopeHostCreate, identity.ScopeComputerObserve, identity.ScopeComputerOperate},
		}}, Audit: audits,
		Logger: zap.NewNop(), Metrics: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1"}, nil)
	transport := &sdk.StreamableClientTransport{
		Endpoint: server.URL, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: bearerTransport{base: http.DefaultTransport}},
	}
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()

	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	wantTools := 7 + len(app.RuntimeComputerTools())
	if len(listed.Tools) != wantTools {
		t.Fatalf("listed %d tools, want %d", len(listed.Tools), wantTools)
	}
	for _, tool := range listed.Tools {
		if strings.HasPrefix(tool.Name, "task_") || strings.HasPrefix(tool.Name, "schedule_") {
			t.Fatalf("retired tool %q remains visible", tool.Name)
		}
	}
	for _, retired := range []string{"task_create", "task_get", "task_cancel", "schedule_create", "schedule_list", "schedule_update"} {
		result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: retired, Arguments: map[string]any{}})
		if err == nil && result != nil && !result.IsError {
			t.Fatalf("retired tool %q remained callable: %+v", retired, result)
		}
	}
	if hosts.createCalls.Load() != 0 || runtime.tool != "" {
		t.Fatal("retired tool calls reached a Host or runtime executor")
	}
	if listed.Tools[0].InputSchema == nil {
		t.Fatal("host_create schema is missing")
	}

	created, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "host_create", Arguments: map[string]any{"workspace_ids": []string{"w1234567"}, "idempotency_key": "host-create-key-001"}})
	if err != nil || created.IsError {
		t.Fatalf("host_create result=%+v err=%v", created, err)
	}
	if hosts.createCalls.Load() != 1 {
		t.Fatalf("Create called %d times, want 1", hosts.createCalls.Load())
	}
	if len(hosts.lastCreate.WorkspaceIDs) != 1 || hosts.lastCreate.WorkspaceIDs[0] != "w1234567" {
		t.Fatalf("workspace IDs = %v, want [w1234567]", hosts.lastCreate.WorkspaceIDs)
	}
	if _, exists := created.StructuredContent.(map[string]any)["workspaces"]; !exists {
		t.Fatalf("host_create response omitted bindings: %#v", created.StructuredContent)
	}

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "window_list", Arguments: map[string]any{
		"host_id": "h1234567", "idempotency_key": "test-window-list-0001",
	}})
	if err != nil || result.IsError {
		t.Fatalf("window_list result=%+v err=%v", result, err)
	}
	if runtime.sessionID == "" || runtime.hostID != "h1234567" || runtime.key != "test-window-list-0001" || runtime.tool != "window_list" {
		t.Fatalf("runtime call identity = %#v", runtime)
	}
	if hosts.computerGetCalls.Load() != 0 {
		t.Fatalf("MCP service performed %d direct computer Host reads", hosts.computerGetCalls.Load())
	}
	if len(audits.entries) != 2 || audits.entries[0].Action != audit.ActionMCPCall || audits.entries[0].ActorKind != audit.ActorAPIKey {
		t.Fatalf("audit entries = %+v", audits.entries)
	}
	for _, entry := range audits.entries {
		if strings.Contains(strings.ToLower(strings.TrimSpace(strings.Join(mapKeys(entry.Detail), " "))), "command") {
			t.Fatalf("audit captured tool arguments: %+v", entry.Detail)
		}
	}
	if calls, labels := gatheredCounter(t, registry, "cloudhost_mcp_tool_calls_total", map[string]string{"tool": "window_list", "result": "ok"}); calls != 1 || labels["host_id"] != "" {
		t.Fatalf("MCP calls=%v labels=%v", calls, labels)
	}
	if count := gatheredHistogramCount(t, registry, "cloudhost_mcp_tool_duration_seconds", map[string]string{"tool": "window_list"}); count != 1 {
		t.Fatalf("window list duration samples = %d, want 1", count)
	}
}

func TestNewDisabledAndInvalidDependencies(t *testing.T) {
	handler, err := New(Deps{Config: config.MCP{Enabled: false}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("disabled status = %d, want %d", response.Code, http.StatusNotFound)
	}
	if _, err := New(Deps{Config: config.MCP{Enabled: true}}); err == nil {
		t.Fatal("New accepted missing dependencies")
	}
}

func TestComputerToolCatalogMatchesSharedRuntimeContract(t *testing.T) {
	expected := make(map[string]app.RuntimeComputerToolDefinition)
	for _, definition := range app.RuntimeComputerTools() {
		if definition.Name == "browser_observe_field" {
			t.Fatal("internal scheduled observation tool was exposed to MCP")
		}
		expected[definition.Name] = definition
	}
	seen := make(map[string]bool)
	for _, spec := range tools {
		if _, duplicate := seen[spec.name]; duplicate {
			t.Fatalf("duplicate MCP tool %q", spec.name)
		}
		seen[spec.name] = true
		if spec.runtime == nil {
			continue
		}
		definition, ok := expected[spec.name]
		if !ok || definition.Capability != spec.runtime.Capability || definition.Action != spec.runtime.Action || definition.Scope != spec.runtime.Scope {
			t.Fatalf("MCP runtime definition %q differs from the shared catalog", spec.name)
		}
		if !contains(spec.required, "host_id") || !contains(spec.required, "idempotency_key") ||
			spec.properties["host_id"] != "string" || spec.properties["idempotency_key"] != "string" {
			t.Fatalf("MCP runtime tool %q lacks the Host/idempotency contract", spec.name)
		}
		if runtimeclient.WorkspaceScopedCapability(definition.Capability) &&
			(!contains(spec.required, "workspace_id") || spec.properties["workspace_id"] != "string") {
			t.Fatalf("MCP runtime tool %q lacks the explicit Workspace contract", spec.name)
		}
		delete(expected, spec.name)
	}
	for _, spec := range tools {
		if spec.name == "browser_observe_field" {
			t.Fatal("internal scheduled observation tool was registered with MCP")
		}
	}
	if len(expected) != 0 {
		t.Fatalf("MCP omitted shared runtime tools: %v", expected)
	}
	for _, legacy := range []string{"browser_elements", "browser_markdown"} {
		if seen[legacy] {
			t.Fatalf("legacy AIO-only MCP tool %q remains exposed", legacy)
		}
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestValidateToolArguments(t *testing.T) {
	tests := []struct {
		name    string
		spec    toolSpec
		json    string
		wantErr bool
	}{
		{name: "runtime defaults", spec: toolByName("window_list"), json: `{"host_id":"h1234567","idempotency_key":"test-window-list-0001"}`},
		{name: "unknown property", spec: toolByName("host_get"), json: `{"host_id":"host_test","token":"secret"}`, wantErr: true},
		{name: "runtime idempotency key required", spec: toolByName("window_list"), json: `{"host_id":"h1234567"}`, wantErr: true},
		{name: "runtime idempotency key invalid", spec: toolByName("window_list"), json: `{"host_id":"h1234567","idempotency_key":"short"}`, wantErr: true},
		{name: "invalid port", spec: toolByName("preview_url"), json: `{"host_id":"host_test","port":65536}`, wantErr: true},
		{name: "valid workspace IDs", spec: toolByName("host_create"), json: `{"workspace_ids":["w1234567"],"idempotency_key":"host-create-key-001"}`},
		{name: "host create idempotency key required", spec: toolByName("host_create"), json: `{}`, wantErr: true},
		{name: "host create idempotency key invalid", spec: toolByName("host_create"), json: `{"idempotency_key":"short"}`, wantErr: true},
		{name: "legacy template rejected", spec: toolByName("host_create"), json: `{"template_id":"python","idempotency_key":"host-create-key-001"}`, wantErr: true},
		{name: "legacy workspace object rejected", spec: toolByName("host_create"), json: `{"workspace":{"mode":"existing","id":"w1234567"}}`, wantErr: true},
		{name: "invalid workspace ID", spec: toolByName("host_create"), json: `{"workspace_ids":["bad"],"idempotency_key":"host-create-key-001"}`, wantErr: true},
		{name: "duplicate workspace IDs", spec: toolByName("host_create"), json: `{"workspace_ids":["w1234567","w1234567"],"idempotency_key":"host-create-key-001"}`, wantErr: true},
		{name: "workspace IDs must be strings", spec: toolByName("host_create"), json: `{"workspace_ids":[1],"idempotency_key":"host-create-key-001"}`, wantErr: true},
		{name: "legacy host delete option rejected", spec: toolByName("host_delete"), json: `{"host_id":"h1234567","keep_workspace":true}`, wantErr: true},
		{name: "trailing JSON", spec: toolByName("host_get"), json: `{"host_id":"host_test"}{}`, wantErr: true},
		{name: "mutually exclusive type selectors", spec: toolByName("browser_type"), json: `{"host_id":"host_test","text":"x","index":1,"selector":"#x"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validateToolArguments(tt.spec, json.RawMessage(tt.json))
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func mapKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	return keys
}

func gatheredCounter(t *testing.T, registry *prometheus.Registry, name string, want map[string]string) (float64, map[string]string) {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			labels := make(map[string]string, len(metric.Label))
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			matches := len(labels) == len(want)
			for key, value := range want {
				matches = matches && labels[key] == value
			}
			if matches {
				return metric.GetCounter().GetValue(), labels
			}
		}
	}
	t.Fatalf("counter %s labels=%v not found", name, want)
	return 0, nil
}

func gatheredHistogramCount(t *testing.T, registry *prometheus.Registry, name string, want map[string]string) uint64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			labels := make(map[string]string, len(metric.Label))
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			matches := len(labels) == len(want)
			for key, value := range want {
				matches = matches && labels[key] == value
			}
			if matches {
				return metric.GetHistogram().GetSampleCount()
			}
		}
	}
	t.Fatalf("histogram %s labels=%v not found", name, want)
	return 0
}

type bearerTransport struct{ base http.RoundTripper }

func (t bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	clone.Header.Set("Authorization", "Bearer chk_test_secret")
	return t.base.RoundTrip(clone)
}

type hostServiceStub struct {
	value            host.Host
	createCalls      atomic.Int32
	computerGetCalls atomic.Int32
	computerScope    identity.Scope
	lastCreate       app.CreateHostCommand
	getErr           error
	panicGet         bool
}

func (s *hostServiceStub) Create(_ context.Context, _ identity.Principal, command app.CreateHostCommand) (host.Host, error) {
	s.createCalls.Add(1)
	s.lastCreate = command
	value := s.value
	value.State = host.StateCreating
	return value, nil
}
func (s *hostServiceStub) EnsureDefaultStopped(context.Context, identity.User) (host.Host, bool, error) {
	return s.value, false, nil
}
func (s *hostServiceStub) List(context.Context, identity.Principal) ([]host.Host, error) {
	return []host.Host{s.value}, nil
}
func (s *hostServiceStub) Get(_ context.Context, _ identity.Principal, id string) (host.Host, error) {
	if s.panicGet {
		panic("unexpected host store panic")
	}
	if s.getErr != nil {
		return host.Host{}, s.getErr
	}
	if id != s.value.ID {
		return host.Host{}, nil
	}
	return s.value, nil
}
func (s *hostServiceStub) BindWorkspace(context.Context, identity.Principal, string, string, *int64, int64, string) (workspace.Binding, error) {
	return workspace.Binding{}, nil
}
func (s *hostServiceStub) UnbindWorkspace(context.Context, identity.Principal, string, string, int64, bool, string) (workspace.Binding, error) {
	return workspace.Binding{}, nil
}
func (s *hostServiceStub) RetryWorkspaceSync(context.Context, identity.Principal, string, string, int64) (workspace.Binding, error) {
	return workspace.Binding{}, nil
}
func (s *hostServiceStub) GetForComputer(ctx context.Context, principal identity.Principal, id string, scope identity.Scope) (host.Host, error) {
	s.computerGetCalls.Add(1)
	s.computerScope = scope
	if principal.Kind != identity.KindAPIKey || !principal.CanAccessHost(id, scope) ||
		s.value.ID != id || s.value.OwnerID != principal.UserID {
		return host.Host{}, domain.NewError(domain.CodeForbidden, "computer scope is insufficient")
	}
	return s.Get(ctx, principal, id)
}
func (s *hostServiceStub) Delete(context.Context, identity.Principal, app.DeleteHostCommand) (host.Host, error) {
	return s.value, nil
}
func (s *hostServiceStub) Release(context.Context, identity.Principal, string, bool) (host.Host, error) {
	return s.value, nil
}
func (s *hostServiceStub) Restore(context.Context, identity.Principal, string) (host.Host, error) {
	return s.value, nil
}
func (s *hostServiceStub) Renew(context.Context, identity.Principal, string) (host.Host, error) {
	return s.value, nil
}
func (s *hostServiceStub) IssueTicket(context.Context, identity.Principal, string) (app.EnterInfo, error) {
	return app.EnterInfo{}, nil
}
func (s *hostServiceStub) IssuePreviewTicket(context.Context, identity.Principal, string, int) (app.EnterInfo, error) {
	return app.EnterInfo{}, nil
}

type runtimeStub struct {
	upstream    app.Upstream
	resolves    atomic.Int32
	marked      atomic.Int32
	invalidated atomic.Int32
}

func (s *runtimeStub) Lookup(context.Context, string) (host.Ref, error) { return host.Ref{}, nil }
func (s *runtimeStub) Resolve(context.Context, string, int) (app.Upstream, error) {
	s.resolves.Add(1)
	return s.upstream, nil
}
func (s *runtimeStub) InvalidateUpstream(string) { s.invalidated.Add(1) }
func (s *runtimeStub) MarkActive(string)         { s.marked.Add(1) }

type verifierStub struct{}

type apiKeyPrincipalVerifier struct {
	verifierStub
	principal identity.Principal
}

func (v apiKeyPrincipalVerifier) VerifyAPIKey(_ context.Context, credential string) (identity.Principal, error) {
	if credential != "chk_test_secret" {
		return identity.Principal{}, identity.ErrInvalidCredential
	}
	return v.principal, nil
}

func (verifierStub) VerifySession(context.Context, string) (identity.Principal, error) {
	return identity.Principal{}, nil
}
func (verifierStub) VerifyAPIKey(_ context.Context, credential string) (identity.Principal, error) {
	if credential != "chk_test_secret" {
		return identity.Principal{}, identity.ErrInvalidCredential
	}
	return identity.Principal{
		UserID: "user_test", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "token_test",
		HostIDs: []string{"host_test"}, Scopes: []identity.Scope{identity.ScopeHostRead, identity.ScopeHostCreate, identity.ScopeComputerOperate},
	}, nil
}
func (verifierStub) ConsumeTicket(context.Context, string) (app.Ticket, error) {
	return app.Ticket{}, nil
}

type auditStub struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (s *auditStub) Write(_ context.Context, entry audit.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entry)
	return nil
}

var _ app.HostService = (*hostServiceStub)(nil)
var _ app.HostRuntime = (*runtimeStub)(nil)
var _ app.Verifier = verifierStub{}
var _ app.AuditWriter = (*auditStub)(nil)
