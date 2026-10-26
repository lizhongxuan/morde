package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/config"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

func TestDesktopObservationMCPAppIsReadOnlyAndKeepsImageFallback(t *testing.T) {
	image := testJPEG(t, 1, 1)
	runtime := &runtimeComputerToolsStub{result: app.MCPRuntimeToolResult{
		Output: json.RawMessage(`{"width":1,"height":1}`),
		Images: []app.ModelImage{{MediaType: "image/jpeg", Data: image}},
	}}
	principal := identity.Principal{
		UserID: "user_test", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "token_test",
		HostIDs: []string{"h1234567"}, Scopes: []identity.Scope{identity.ScopeComputerObserve},
	}
	handler, err := New(Deps{
		PreviewPortMin: 1024, Config: config.MCP{Enabled: true, MaxOutputBytes: 1024, ScreenshotMaxWidth: 1280},
		Domains:      config.Domains{Scheme: "https", Portal: "portal.example.test", HostSuffix: "hosts.example.test"},
		Hosts:        &hostServiceStub{value: host.Host{ID: "h1234567", OwnerID: principal.UserID, State: host.StateRunning}},
		Applications: newApplicationStatusServiceStub(), RuntimeTools: runtime, Tasks: newTaskServiceStub(), Schedules: scheduleServiceFake{},
		Auth: apiKeyPrincipalVerifier{principal: principal}, Audit: &auditStub{}, Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	capabilities := &sdk.ClientCapabilities{}
	capabilities.AddExtension("io.modelcontextprotocol/ui", map[string]any{"mimeTypes": []string{observationResourceMIME}})
	client := sdk.NewClient(&sdk.Implementation{Name: "observation-test", Version: "1"}, &sdk.ClientOptions{Capabilities: capabilities})
	session, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint: server.URL, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: bearerTransport{base: http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()

	serverUI, ok := session.InitializeResult().Capabilities.Extensions["io.modelcontextprotocol/ui"].(map[string]any)
	mimeTypes, hasMIMETypes := serverUI["mimeTypes"].([]any)
	foundMIME := false
	for _, mimeType := range mimeTypes {
		foundMIME = foundMIME || mimeType == observationResourceMIME
	}
	if !ok || !hasMIMETypes || !foundMIME {
		t.Fatalf("server MCP Apps capabilities = %#v", session.InitializeResult().Capabilities.Extensions)
	}
	resources, err := session.ListResources(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources.Resources) != 1 || resources.Resources[0].URI != observationResourceURI || resources.Resources[0].MIMEType != observationResourceMIME {
		t.Fatalf("resources = %#v", resources.Resources)
	}
	resource, err := session.ReadResource(context.Background(), &sdk.ReadResourceParams{URI: observationResourceURI})
	if err != nil || len(resource.Contents) != 1 {
		t.Fatalf("read observation resource = %#v, %v", resource, err)
	}
	view := resource.Contents[0].Text
	for _, required := range []string{"目标主机", "画面时间", "快照新鲜 · 非实时", "截图进行中", "result.isError === true", "截图操作未成功", "已断流", "ping", "ui/notifications/tool-result", "ui/notifications/tool-cancelled", "截图已取消"} {
		if !strings.Contains(view, required) {
			t.Errorf("observation view is missing %q", required)
		}
	}
	for _, forbidden := range []string{"tools/call", "ui/message", "<button", "authorization", "chk_"} {
		if strings.Contains(strings.ToLower(view), strings.ToLower(forbidden)) {
			t.Errorf("read-only observation view contains %q", forbidden)
		}
	}

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var screenshotFound bool
	for _, tool := range tools.Tools {
		ui, hasUI := tool.Meta["ui"].(map[string]any)
		if tool.Name == desktopScreenshotToolName {
			screenshotFound = true
			if !hasUI || ui["resourceUri"] != observationResourceURI {
				t.Fatalf("desktop screenshot UI metadata = %#v", tool.Meta)
			}
			if visibility, ok := ui["visibility"].([]any); !ok || len(visibility) != 1 || visibility[0] != "model" {
				t.Fatalf("desktop screenshot visibility = %#v", ui["visibility"])
			}
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Fatalf("desktop screenshot is not annotated read-only: %#v", tool.Annotations)
			}
		} else if hasUI {
			t.Errorf("unexpected MCP App UI metadata on %q", tool.Name)
		}
	}
	if !screenshotFound {
		t.Fatal("desktop screenshot tool is missing")
	}
	if err := authorizeTool(principal, desktopScreenshotToolName, map[string]any{"host_id": "h1234567"}); err != nil {
		t.Fatalf("observe-only key cannot take screenshot: %v", err)
	}
	if err := authorizeTool(principal, "desktop_click", map[string]any{"host_id": "h1234567"}); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("observe-only key authorized desktop_click: %v", err)
	}

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: desktopScreenshotToolName, Arguments: map[string]any{"host_id": "h1234567", "idempotency_key": "observe-call-0001"},
	})
	if err != nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("desktop screenshot fallback = %#v, %v", result, err)
	}
	gotImage, ok := result.Content[0].(*sdk.ImageContent)
	if !ok || gotImage.MIMEType != "image/jpeg" || string(gotImage.Data) != string(image) {
		t.Fatalf("desktop screenshot image fallback = %#v", result.Content[0])
	}
	metadata, ok := result.Meta[observationMetaKey].(map[string]any)
	if !ok || metadata["host_id"] != "h1234567" {
		t.Fatalf("desktop screenshot observation metadata = %#v", result.Meta)
	}
	observedAt, err := time.Parse(time.RFC3339Nano, metadata["observed_at"].(string))
	if err != nil || time.Since(observedAt) > time.Minute || time.Until(observedAt) > time.Second {
		t.Fatalf("observed_at = %v, %v", metadata["observed_at"], err)
	}
	if runtime.tool != desktopScreenshotToolName || runtime.principal.UserID != principal.UserID || runtime.hostID != "h1234567" {
		t.Fatalf("runtime screenshot authorization = tool %q, host %q, principal %+v", runtime.tool, runtime.hostID, runtime.principal)
	}

	plainClient := sdk.NewClient(&sdk.Implementation{Name: "plain-mcp-test", Version: "1"}, &sdk.ClientOptions{Capabilities: &sdk.ClientCapabilities{}})
	plainSession, err := plainClient.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint: server.URL, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: bearerTransport{base: http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plainSession.Close() }()
	fallback, err := plainSession.CallTool(context.Background(), &sdk.CallToolParams{
		Name: desktopScreenshotToolName, Arguments: map[string]any{"host_id": "h1234567", "idempotency_key": "observe-call-0002"},
	})
	if err != nil || fallback.IsError || len(fallback.Content) != 1 {
		t.Fatalf("plain-client screenshot fallback = %#v, %v", fallback, err)
	}
	if got, ok := fallback.Content[0].(*sdk.ImageContent); !ok || got.MIMEType != "image/jpeg" || string(got.Data) != string(image) {
		t.Fatalf("plain-client screenshot content = %#v", fallback.Content[0])
	}
}
