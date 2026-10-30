package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"morde/server/internal/app"
	"morde/server/internal/config"
	"morde/server/internal/domain/identity"
)

type runtimeComputerToolsStub struct {
	principal identity.Principal
	sessionID string
	hostID    string
	key       string
	tool      string
	arguments json.RawMessage
	result    app.MCPRuntimeToolResult
	err       error
}

func (s *runtimeComputerToolsStub) Execute(_ context.Context, principal identity.Principal, sessionID, hostID, key, tool string, arguments json.RawMessage) (app.MCPRuntimeToolResult, error) {
	s.principal, s.sessionID, s.hostID, s.key, s.tool = principal, sessionID, hostID, key, tool
	s.arguments = append(json.RawMessage(nil), arguments...)
	return s.result, s.err
}

func TestHostToolDelegatesToSharedRuntimeExecutor(t *testing.T) {
	executor := &runtimeComputerToolsStub{result: app.MCPRuntimeToolResult{Output: json.RawMessage(`{"windows":[]}`)}}
	service := &service{deps: Deps{RuntimeTools: executor}}
	principal := identity.Principal{Kind: identity.KindAPIKey, UserID: "user_test"}
	args := map[string]any{"host_id": "h1234567", "idempotency_key": "call-00000001"}
	value, image, err := service.hostTool(context.Background(), principal, "mcp-session-123", toolByName("window_list"), args)
	if err != nil || image != nil {
		t.Fatalf("hostTool() = %#v, %#v, %v", value, image, err)
	}
	if _, ok := value.(map[string]any)["windows"]; !ok {
		t.Fatalf("decoded runtime value = %#v", value)
	}
	if executor.principal.UserID != principal.UserID || executor.sessionID != "mcp-session-123" || executor.hostID != "h1234567" ||
		executor.key != "call-00000001" || executor.tool != "window_list" {
		t.Fatalf("runtime executor request = %#v", executor)
	}
	var got map[string]any
	if err := json.Unmarshal(executor.arguments, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("runtime tool arguments = %#v", got)
	}
}

func TestHostToolReturnsSingleRuntimeImage(t *testing.T) {
	imageData := testJPEG(t, 1, 1)
	executor := &runtimeComputerToolsStub{result: app.MCPRuntimeToolResult{
		Output: json.RawMessage(`{"width":1,"height":1}`),
		Images: []app.ModelImage{{MediaType: "image/jpeg", Data: imageData}},
	}}
	service := &service{deps: Deps{Config: config.MCP{ScreenshotMaxWidth: 1280}, RuntimeTools: executor}}
	_, image, err := service.hostTool(context.Background(), identity.Principal{}, "mcp-session-123", toolByName("browser_screenshot"), map[string]any{
		"host_id": "h1234567", "idempotency_key": "call-00000002",
	})
	if err != nil || image == nil || image.MIMEType != "image/jpeg" || string(image.Data) != string(imageData) {
		t.Fatalf("hostTool image=%#v err=%v", image, err)
	}
}

func TestHostToolRejectsImagesWiderThanConfiguredMaximum(t *testing.T) {
	executor := &runtimeComputerToolsStub{result: app.MCPRuntimeToolResult{
		Output: json.RawMessage(`{"width":3,"height":1}`),
		Images: []app.ModelImage{{MediaType: "image/jpeg", Data: testJPEG(t, 3, 1)}},
	}}
	service := &service{deps: Deps{Config: config.MCP{ScreenshotMaxWidth: 2}, RuntimeTools: executor}}
	_, image, err := service.hostTool(context.Background(), identity.Principal{}, "mcp-session-123", toolByName("browser_screenshot"), map[string]any{
		"host_id": "h1234567", "idempotency_key": "call-00000004",
	})
	if err == nil || image != nil {
		t.Fatalf("hostTool accepted image wider than configured maximum: image=%#v err=%v", image, err)
	}
}

func testJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	imageValue := image.NewRGBA(image.Rect(0, 0, width, height))
	imageValue.Set(0, 0, color.RGBA{R: 20, G: 40, B: 60, A: 255})
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, imageValue, nil); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestHostToolRejectsInvalidSessionAndRuntimeOutput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session string
		output  json.RawMessage
	}{
		{name: "missing session", output: json.RawMessage(`{"ok":true}`)},
		{name: "trailing JSON", session: "mcp-session-123", output: json.RawMessage(`{"ok":true} {}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := &runtimeComputerToolsStub{result: app.MCPRuntimeToolResult{Output: tc.output}}
			service := &service{deps: Deps{RuntimeTools: executor}}
			_, _, err := service.hostTool(context.Background(), identity.Principal{}, tc.session, toolByName("window_list"), map[string]any{
				"host_id": "h1234567", "idempotency_key": "call-00000003",
			})
			if err == nil {
				t.Fatal("hostTool accepted invalid protocol state or output")
			}
		})
	}
}

var _ runtimeComputerTools = (*runtimeComputerToolsStub)(nil)
var _ sdk.Content = (*sdk.ImageContent)(nil)
