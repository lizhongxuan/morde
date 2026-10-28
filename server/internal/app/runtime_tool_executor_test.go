package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"morde-runtime/contract"
	"morde-runtime/runtimecore"
	"morde-runtime/runtimefiles"
	"morde-runtime/runtimehttp"
	"morde-runtime/runtimeidentity"

	"morde/server/internal/domain"
	"morde/server/internal/domain/execution"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/runtimeclient"
)

func TestRuntimeToolExecutorDefinitionsReflectRegisteredCapabilitiesAndScopes(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{
		{capability: "shell.exec", action: "run", result: `{"exit_code":0,"stdout":"ok","stderr":"","stdout_truncated":false,"stderr_truncated":false,"timed_out":false}`},
		{capability: "file.read", action: "read", result: `{"path":"README.md","text":"readable"}`},
		{capability: "file.find", action: "find", result: `{"matches":[]}`},
	})
	fixture.authorizer.errors[identity.ScopeComputerOperate] = domain.NewError(domain.CodeForbidden, "scope denied")

	definitions, err := fixture.executor.Definitions(context.Background(), fixture.store.details.Task)
	if err != nil {
		t.Fatalf("Definitions() error = %v", err)
	}
	if len(definitions) != 2 || definitions[0].Name != runtimeToolFileRead || definitions[1].Name != runtimeToolFileFind {
		t.Fatalf("definitions = %#v, want only registered observe tools", definitions)
	}
	for _, definition := range definitions {
		if strings.Contains(definition.Name, "browser") || strings.Contains(definition.Name, "gui") {
			t.Fatalf("unregistered GUI tool was exposed: %q", definition.Name)
		}
	}
}

func TestRuntimeToolExecutorExposesBrowserToolsOnlyWhenRuntimeCapabilitiesExist(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{
		{capability: "browser.actions", action: "open_tab", result: `{"target_id":"target_1","url":"https://example.test/","title":"Example"}`},
		{capability: "browser.observe", action: "snapshot", result: `{"target_id":"target_1","url":"https://example.test/","title":"Example","text":"hello"}`},
	})
	definitions, err := fixture.executor.Definitions(context.Background(), fixture.store.details.Task)
	if err != nil {
		t.Fatalf("Definitions() error = %v", err)
	}
	want := []string{
		runtimeToolBrowserOpenTab, runtimeToolBrowserNavigate, runtimeToolBrowserSnapshot,
		runtimeToolBrowserClick, runtimeToolBrowserType, runtimeToolBrowserPressKey, runtimeToolBrowserScreenshot,
	}
	got := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		got = append(got, definition.Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("browser definitions = %v, want %v", got, want)
	}
}

func TestRuntimeToolExecutorExposesDesktopToolsOnlyWhenRuntimeCapabilitiesExist(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{
		{capability: "desktop.observe", action: "screenshot", result: `{"mime_type":"image/jpeg","width":1,"height":1,"data_base64":"unused"}`},
		{capability: "window.list", action: "list", result: `{"windows":[]}`},
		{capability: "desktop.input", action: "click", result: `{"x":0,"y":0,"button":"left","clicked":true}`},
		{capability: "desktop.input", action: "type", result: `{"typed_chars":1}`},
		{capability: "desktop.input", action: "press_key", result: `{"key":"Enter","pressed":true}`},
		{capability: "window.activate", action: "activate", result: `{"window_id":"0x1","activated":true}`},
	})
	definitions, err := fixture.executor.Definitions(context.Background(), fixture.store.details.Task)
	if err != nil {
		t.Fatalf("Definitions() error = %v", err)
	}
	want := []string{
		runtimeToolDesktopScreenshot, runtimeToolWindowList, runtimeToolWindowActivate,
		runtimeToolDesktopClick, runtimeToolDesktopType, runtimeToolDesktopPressKey,
	}
	got := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		got = append(got, definition.Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("desktop definitions = %v, want %v", got, want)
	}
}

func TestRuntimeDesktopToolArgumentsAreBounded(t *testing.T) {
	cases := []struct {
		tool  string
		input string
		valid bool
	}{
		{tool: runtimeToolDesktopScreenshot, input: `{}`, valid: true},
		{tool: runtimeToolDesktopScreenshot, input: `{"target_id":"user-tab"}`},
		{tool: runtimeToolWindowList, input: `{}`, valid: true},
		{tool: runtimeToolWindowActivate, input: `{"window_id":"0x1"}`, valid: true},
		{tool: runtimeToolWindowActivate, input: `{"window_id":"0x01"}`},
		{tool: runtimeToolDesktopClick, input: `{"window_id":"0x1","x":0,"y":0}`, valid: true},
		{tool: runtimeToolDesktopClick, input: `{"window_id":"0x1","x":65536,"y":0}`},
		{tool: runtimeToolDesktopClick, input: `{"window_id":"0x1","x":1,"y":2,"button":"right"}`, valid: true},
		{tool: runtimeToolDesktopClick, input: `{"window_id":"0x1","x":1,"y":2,"button":"4"}`},
		{tool: runtimeToolDesktopClick, input: `{"x":0,"y":0}`},
		{tool: runtimeToolDesktopType, input: `{"window_id":"0x1","text":"你好"}`, valid: true},
		{tool: runtimeToolDesktopType, input: `{"window_id":"0x1","text":"x","shell":"echo bad"}`},
		{tool: runtimeToolDesktopPressKey, input: `{"window_id":"0x1","key":"ArrowLeft"}`, valid: true},
		{tool: runtimeToolDesktopPressKey, input: `{"window_id":"0x1","key":"Control+L"}`},
	}
	for _, tc := range cases {
		t.Run(tc.tool+tc.input, func(t *testing.T) {
			mapping, ok := runtimeToolMappingByName(tc.tool)
			if !ok {
				t.Fatalf("missing mapping %q", tc.tool)
			}
			if got := validRuntimeToolArguments(mapping, json.RawMessage(tc.input)); got != tc.valid {
				t.Fatalf("validRuntimeToolArguments(%s) = %v, want %v", tc.input, got, tc.valid)
			}
		})
	}
}

func TestRuntimeToolRiskTiers(t *testing.T) {
	cases := []struct {
		name      string
		arguments string
		want      runtimeToolRiskTier
	}{
		{runtimeToolFileRead, `{}`, runtimeToolRiskSafe},
		{runtimeToolFileList, `{}`, runtimeToolRiskSafe},
		{runtimeToolFileFind, `{}`, runtimeToolRiskSafe},
		{runtimeToolFileWrite, `{}`, runtimeToolRiskModerate},
		{runtimeToolBrowserOpenTab, `{}`, runtimeToolRiskModerate},
		{runtimeToolBrowserNavigate, `{}`, runtimeToolRiskModerate},
		{runtimeToolBrowserClick, `{}`, runtimeToolRiskModerate},
		{runtimeToolBrowserType, `{}`, runtimeToolRiskSensitive},
		{runtimeToolBrowserPressKey, `{"key":"Enter"}`, runtimeToolRiskModerate},
		{runtimeToolDesktopClick, `{}`, runtimeToolRiskSensitive},
		{runtimeToolDesktopType, `{}`, runtimeToolRiskSensitive},
		{runtimeToolDesktopPressKey, `{"key":"Enter"}`, runtimeToolRiskSensitive},
		{runtimeToolDesktopPressKey, `{"key":"BackSpace"}`, runtimeToolRiskSensitive},
		{runtimeToolDesktopPressKey, `{"key":"Delete"}`, runtimeToolRiskSensitive},
		{runtimeToolDesktopPressKey, `{"key":"Tab"}`, runtimeToolRiskModerate},
		{runtimeToolDesktopPressKey, `{"key":"ArrowLeft"}`, runtimeToolRiskModerate},
		{runtimeToolDesktopScreenshot, `{}`, runtimeToolRiskSafe},
		{runtimeToolWindowList, `{}`, runtimeToolRiskSafe},
		{runtimeToolWindowActivate, `{}`, runtimeToolRiskModerate},
		{"unknown", `{}`, runtimeToolRiskSensitive},
	}
	for _, tc := range cases {
		t.Run(tc.name+tc.arguments, func(t *testing.T) {
			if got := runtimeToolRisk(tc.name, json.RawMessage(tc.arguments)); got != tc.want {
				t.Fatalf("runtimeToolRisk(%q) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestRuntimeToolExecutorPrepareRequiresSensitiveApprovalWithoutPersistingArguments(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: "shell.exec", action: "run",
		result: `{"exit_code":0,"stdout":"ok","stderr":"","stdout_truncated":false,"stderr_truncated":false,"timed_out":false}`,
	}})
	request := fixture.request(runtimeToolShellExec, `{"command":"echo private-value"}`, identity.ScopeComputerOperate)
	preparation, err := fixture.executor.Prepare(context.Background(), request)
	if err != nil || preparation.Status != AgentToolApprovalRequired {
		t.Fatalf("Prepare() = %#v, %v; want approval required", preparation, err)
	}
	if preparation.Token != "" || preparation.Approval.Object == "" || preparation.Approval.Content == "" || preparation.Approval.Impact == "" {
		t.Fatalf("sensitive preparation = %#v; want a non-executable approval request", preparation)
	}
	if strings.Contains(preparation.Approval.Object+preparation.Approval.Content+preparation.Approval.Impact, "private-value") ||
		fixture.handlers["shell.exec"].calls.Load() != 0 || len(fixture.store.operations) != 0 {
		t.Fatalf("approval exposed arguments or executed work: approval=%#v calls=%d operations=%d", preparation.Approval,
			fixture.handlers["shell.exec"].calls.Load(), len(fixture.store.operations))
	}
}

func TestRuntimeToolExecutionArgumentsKeepWorkspaceIDOutOfToolPayload(t *testing.T) {
	mapping, ok := runtimeToolMappingByName(runtimeToolFileWrite)
	if !ok {
		t.Fatal("file_write mapping is missing")
	}
	raw := json.RawMessage(`{"workspace_id":"w1234567","path":"task-note.txt","content":"saved"}`)
	got, err := runtimeToolExecutionArguments(mapping, raw)
	if err != nil {
		t.Fatalf("runtimeToolExecutionArguments() error = %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["workspace_id"]; exists || string(fields["path"]) != `"task-note.txt"` || string(fields["content"]) != `"saved"` {
		t.Fatalf("Host tool arguments = %s, want path/content without workspace_id", got)
	}

	other, _ := runtimeToolMappingByName(runtimeToolBrowserOpenTab)
	unchanged := json.RawMessage(`{"url":"https://example.test"}`)
	got, err = runtimeToolExecutionArguments(other, unchanged)
	if err != nil || !bytes.Equal(got, unchanged) {
		t.Fatalf("non-Workspace runtime arguments = %s, %v; want unchanged %s", got, err, unchanged)
	}
	if _, err := runtimeToolExecutionArguments(mapping, json.RawMessage(`{"path":"task-note.txt","content":"saved"}`)); err == nil {
		t.Fatal("Workspace-scoped tool payload without workspace_id was accepted")
	}
}

func TestRuntimeDesktopScreenshotMapsValidatedJPEGWithoutBase64InOutput(t *testing.T) {
	imageBuffer := new(bytes.Buffer)
	frame := image.NewRGBA(image.Rect(0, 0, 2, 3))
	frame.Set(0, 0, color.RGBA{R: 0xff, A: 0xff})
	if err := jpeg.Encode(imageBuffer, frame, &jpeg.Options{Quality: 75}); err != nil {
		t.Fatal(err)
	}
	imageBytes := imageBuffer.Bytes()
	encoded, err := json.Marshal(struct {
		MIMEType   string `json:"mime_type"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		DataBase64 string `json:"data_base64"`
	}{MIMEType: "image/jpeg", Width: 2, Height: 3, DataBase64: base64.StdEncoding.EncodeToString(imageBytes)})
	if err != nil {
		t.Fatal(err)
	}
	mapping, ok := runtimeToolMappingByName(runtimeToolDesktopScreenshot)
	if !ok {
		t.Fatal("desktop screenshot mapping missing")
	}
	result, err := mapRuntimeToolResult(mapping, runtimeclient.Operation{
		OperationID: "o1234567", Status: runtimeclient.OperationSucceeded, Result: encoded,
	}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("mapRuntimeToolResult() error = %v", err)
	}
	if result.Status != AgentToolSucceeded || len(result.Images) != 1 || result.Images[0].MediaType != "image/jpeg" || !bytes.Equal(result.Images[0].Data, imageBytes) {
		t.Fatalf("desktop screenshot result = %#v, image bytes not preserved", result)
	}
	if strings.Contains(result.Output, base64.StdEncoding.EncodeToString(imageBytes)) {
		t.Fatal("desktop screenshot base64 leaked into tool output")
	}

	badDimensions := bytes.Replace(encoded, []byte(`"width":2`), []byte(`"width":3`), 1)
	result, err = mapRuntimeToolResult(mapping, runtimeclient.Operation{
		OperationID: "o1234567", Status: runtimeclient.OperationSucceeded, Result: badDimensions,
	}, json.RawMessage(`{}`))
	if err != nil || result.Status != AgentToolFailed || !strings.Contains(result.Output, "invalid_desktop_screenshot") {
		t.Fatalf("mismatched screenshot dimensions = %#v, %v; want failure", result, err)
	}
}

func TestRuntimeDesktopResultsMustMatchBoundWindowAndArguments(t *testing.T) {
	cases := []struct {
		name      string
		tool      string
		arguments string
		result    string
		want      string
		valid     bool
	}{
		{
			name: "window list", tool: runtimeToolWindowList, arguments: `{}`,
			result: `{"windows":[{"id":"0x1","title":"Terminal","x":0,"y":0,"width":800,"height":600,"active":true}]}`,
			want:   `{"windows":[{"id":"0x1","title":"Terminal","x":0,"y":0,"width":800,"height":600,"active":true}]}`, valid: true,
		},
		{
			name: "window activation", tool: runtimeToolWindowActivate, arguments: `{"window_id":"0x1"}`,
			result: `{"window_id":"0x1","activated":true}`, want: `{"window_id":"0x1","activated":true}`, valid: true,
		},
		{
			name: "click defaults to left", tool: runtimeToolDesktopClick, arguments: `{"window_id":"0x1","x":12,"y":34}`,
			result: `{"window_id":"0x1","x":12,"y":34,"button":"left","clicked":true}`,
			want:   `{"window_id":"0x1","x":12,"y":34,"button":"left","clicked":true}`, valid: true,
		},
		{
			name: "click cannot change target", tool: runtimeToolDesktopClick, arguments: `{"window_id":"0x1","x":12,"y":34}`,
			result: `{"window_id":"0x2","x":12,"y":34,"button":"left","clicked":true}`,
		},
		{
			name: "type counts Unicode characters", tool: runtimeToolDesktopType, arguments: `{"window_id":"0x1","text":"你好🙂"}`,
			result: `{"window_id":"0x1","typed_chars":3}`, want: `{"window_id":"0x1","typed_chars":3}`, valid: true,
		},
		{
			name: "press key", tool: runtimeToolDesktopPressKey, arguments: `{"window_id":"0x1","key":"ArrowLeft"}`,
			result: `{"window_id":"0x1","key":"ArrowLeft","pressed":true}`,
			want:   `{"window_id":"0x1","key":"ArrowLeft","pressed":true}`, valid: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mapping, ok := runtimeToolMappingByName(tc.tool)
			if !ok {
				t.Fatalf("missing mapping %q", tc.tool)
			}
			result, err := mapRuntimeToolResult(mapping, runtimeclient.Operation{
				OperationID: "o1234567", Status: runtimeclient.OperationSucceeded, Result: json.RawMessage(tc.result),
			}, json.RawMessage(tc.arguments))
			if err != nil {
				t.Fatalf("mapRuntimeToolResult() error = %v", err)
			}
			if tc.valid {
				if result.Status != AgentToolSucceeded || result.Output != tc.want {
					t.Fatalf("mapped result = %#v, want successful %s", result, tc.want)
				}
				return
			}
			if result.Status != AgentToolFailed || !strings.Contains(result.Output, "desktop_click_failed") {
				t.Fatalf("mismatched target result = %#v, want failed click", result)
			}
		})
	}
}

func TestDesktopFocusChangeIsAUserPauseOnlyForInputOperations(t *testing.T) {
	mapping, ok := runtimeToolMappingByName(runtimeToolDesktopClick)
	if !ok {
		t.Fatal("desktop click mapping is missing")
	}
	operation := runtimeclient.Operation{Status: runtimeclient.OperationFailed, Result: json.RawMessage(`{"code":"desktop_focus_changed"}`)}
	if !desktopFocusChanged(mapping, operation) {
		t.Fatal("focus change did not request a user pause")
	}
	fileMapping, ok := runtimeToolMappingByName(runtimeToolFileRead)
	if !ok {
		t.Fatal("file read mapping is missing")
	}
	if desktopFocusChanged(fileMapping, operation) {
		t.Fatal("focus change was accepted for a non-desktop tool")
	}
	operation.Result = json.RawMessage(`{"code":"invalid_request"}`)
	if desktopFocusChanged(mapping, operation) {
		t.Fatal("ordinary input validation errors were treated as focus changes")
	}
}

func TestMCPImageResultsStayInTheRuntimeOperationStore(t *testing.T) {
	imageResult := json.RawMessage(`{"mime_type":"image/jpeg","width":1,"height":1,"data_base64":"c2Vuc2l0aXZlLWltYWdl"}`)
	for _, name := range []string{runtimeToolBrowserScreenshot, runtimeToolDesktopScreenshot} {
		mapping, ok := runtimeToolMappingByName(name)
		if !ok {
			t.Fatalf("missing mapping %q", name)
		}
		for _, status := range []runtimeclient.OperationStatus{runtimeclient.OperationSucceeded, runtimeclient.OperationFailed} {
			persisted := mcpRuntimeResultForPersistence(mapping, runtimeclient.Operation{
				Status: status, Result: imageResult,
			})
			if strings.Contains(string(persisted), "c2Vuc2l0aXZlLWltYWdl") || string(persisted) != `{"image_result_in_runtime_store":true}` {
				t.Fatalf("persisted %s %s result = %s, want only runtime journal reference", name, status, persisted)
			}
			if status == runtimeclient.OperationFailed {
				replayed, err := mapMCPRuntimeOperation(mapping, MCPRuntimeOperation{
					Key: MCPRuntimeOperationKey{HostID: "h1234567"}, RuntimeTaskID: "q1234567", OperationID: "o1234567",
					LeaseVersion: 1, Status: task.OperationFailed, Result: persisted,
				})
				if err != nil || !replayed.IsError || len(replayed.Images) != 0 || string(replayed.Output) != `{"error":"runtime_operation_failed"}` {
					t.Fatalf("failed screenshot replay = %#v, %v; want bounded failure without image", replayed, err)
				}
			}
		}
	}
	textMapping, _ := runtimeToolMappingByName(runtimeToolWindowList)
	textResult := json.RawMessage(`{"windows":[]}`)
	if got := mcpRuntimeResultForPersistence(textMapping, runtimeclient.Operation{Status: runtimeclient.OperationSucceeded, Result: textResult}); !bytes.Equal(got, textResult) {
		t.Fatalf("non-image result changed during persistence: %s", got)
	}
}

func TestMCPRuntimeReplayAfterRuntimeRestartReconcilesTerminalJournalResults(t *testing.T) {
	for _, tc := range []struct {
		name          string
		runtimeStatus runtimeclient.OperationStatus
		wantStatus    task.OperationStatus
		wantError     bool
	}{
		{name: "succeeded", runtimeStatus: runtimeclient.OperationSucceeded, wantStatus: task.OperationSucceeded},
		{name: "failed", runtimeStatus: runtimeclient.OperationFailed, wantStatus: task.OperationFailed},
		{name: "cancelled", runtimeStatus: runtimeclient.OperationCancelled, wantStatus: task.OperationCancelled, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture, executor, store, principal, currentFence, operation := newMCPRuntimeReplayFixture(t, task.OperationDispatched)
			value := mcpReplayRuntimeOperation(operation, tc.runtimeStatus)
			var queries, dispatches atomic.Int32
			fixture.wrap = interceptMCPRuntimeOperation(&currentFence, &queries, &dispatches, &value, nil)

			result, err := executor.Execute(context.Background(), principal, operation.Key.SessionID, operation.Key.HostID,
				operation.Key.IdempotencyKey, runtimeToolFileRead, operation.Arguments)
			if tc.wantError {
				if domain.CodeOf(err) != domain.CodeHostNotReady {
					t.Fatalf("Execute() error = %v, want unknown cancellation result", err)
				}
			} else if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if tc.runtimeStatus == runtimeclient.OperationSucceeded && (result.IsError || string(result.Output) != `{"path":"README.md","text":"recovered"}`) {
				t.Fatalf("Execute() result = %#v, want recovered Runtime result", result)
			}
			if tc.runtimeStatus == runtimeclient.OperationFailed && (!result.IsError || string(result.Output) != `{"error":"runtime_operation_failed"}`) {
				t.Fatalf("Execute() result = %#v, want recovered Runtime failure", result)
			}
			if queries.Load() != 1 || dispatches.Load() != 0 {
				t.Fatalf("Runtime query/dispatch counts = %d/%d, want 1/0", queries.Load(), dispatches.Load())
			}
			if store.operation.Status != tc.wantStatus {
				t.Fatalf("durable MCP status = %q, want %q", store.operation.Status, tc.wantStatus)
			}
			if tc.runtimeStatus == runtimeclient.OperationCancelled {
				if !validTestMCPRuntimeCancellationReceipt(store.operation.Result, operation) {
					t.Fatalf("cancelled result %s is not a matching cancellation receipt", store.operation.Result)
				}
			}
			if fixture.handlers["file.read"].calls.Load() != 0 {
				t.Fatal("old-fence replay dispatched the Runtime handler")
			}
		})
	}
}

func TestMCPRuntimeReplayRechecksAuthorizationBeforeReturningResult(t *testing.T) {
	_, executor, store, principal, currentFence, operation := newMCPRuntimeReplayFixture(t, task.OperationSucceeded)
	operation.Fence = currentFence
	operation.Result = mcpReplayRuntimeOperation(operation, runtimeclient.OperationSucceeded).Result
	store.operation = operation
	hosts := &mcpRuntimeRevokingHostAuthorizer{}
	executor.hosts = hosts

	result, err := executor.Execute(context.Background(), principal, operation.Key.SessionID, operation.Key.HostID,
		operation.Key.IdempotencyKey, runtimeToolFileRead, operation.Arguments)
	if domain.CodeOf(err) != domain.CodeNotFound || len(result.Output) != 0 {
		t.Fatalf("revoked replay returned result=%s, err=%v; want no result and not-found", result.Output, err)
	}
	if got := hosts.calls.Load(); got != 2 {
		t.Fatalf("authorization checks = %d, want initial check and pre-return check", got)
	}
}

func TestMCPRuntimeReplayAfterRuntimeRestartKeepsNonterminalResultsUnknown(t *testing.T) {
	for _, status := range []runtimeclient.OperationStatus{
		runtimeclient.OperationAccepted, runtimeclient.OperationRunning, runtimeclient.OperationCancelling, runtimeclient.OperationUnknown,
	} {
		t.Run(string(status), func(t *testing.T) {
			fixture, executor, store, principal, currentFence, operation := newMCPRuntimeReplayFixture(t, task.OperationDispatched)
			value := mcpReplayRuntimeOperation(operation, status)
			var queries, dispatches atomic.Int32
			fixture.wrap = interceptMCPRuntimeOperation(&currentFence, &queries, &dispatches, &value, nil)

			_, err := executor.Execute(context.Background(), principal, operation.Key.SessionID, operation.Key.HostID,
				operation.Key.IdempotencyKey, runtimeToolFileRead, operation.Arguments)
			if domain.CodeOf(err) != domain.CodeHostNotReady {
				t.Fatalf("Execute() error = %v, want unknown operation", err)
			}
			if queries.Load() != 1 || dispatches.Load() != 0 {
				t.Fatalf("Runtime query/dispatch counts = %d/%d, want 1/0", queries.Load(), dispatches.Load())
			}
			if store.operation.Status != task.OperationUnknown {
				t.Fatalf("durable MCP status = %q, want unknown", store.operation.Status)
			}
			if len(store.settlements) != 1 || store.settlements[0] != task.OperationUnknown {
				t.Fatalf("settlements = %v, want dispatched→unknown", store.settlements)
			}
			if fixture.handlers["file.read"].calls.Load() != 0 {
				t.Fatal("old-fence replay dispatched the Runtime handler")
			}
		})
	}

	t.Run("query not found stays unknown", func(t *testing.T) {
		fixture, executor, store, principal, currentFence, operation := newMCPRuntimeReplayFixture(t, task.OperationDispatched)
		var queries, dispatches atomic.Int32
		fixture.wrap = interceptMCPRuntimeOperation(&currentFence, &queries, &dispatches, nil, errors.New("operation not found"))

		_, err := executor.Execute(context.Background(), principal, operation.Key.SessionID, operation.Key.HostID,
			operation.Key.IdempotencyKey, runtimeToolFileRead, operation.Arguments)
		if domain.CodeOf(err) != domain.CodeHostNotReady {
			t.Fatalf("Execute() error = %v, want unknown operation", err)
		}
		if queries.Load() != 1 || dispatches.Load() != 0 || store.operation.Status != task.OperationUnknown {
			t.Fatalf("query/dispatch/status = %d/%d/%q, want 1/0/unknown", queries.Load(), dispatches.Load(), store.operation.Status)
		}
		if fixture.handlers["file.read"].calls.Load() != 0 {
			t.Fatal("missing old-fence operation was redispatched")
		}
	})

	t.Run("prepared was never dispatched", func(t *testing.T) {
		fixture, executor, store, principal, currentFence, operation := newMCPRuntimeReplayFixture(t, task.OperationPrepared)
		var queries, dispatches atomic.Int32
		fixture.wrap = interceptMCPRuntimeOperation(&currentFence, &queries, &dispatches, nil, nil)

		_, err := executor.Execute(context.Background(), principal, operation.Key.SessionID, operation.Key.HostID,
			operation.Key.IdempotencyKey, runtimeToolFileRead, operation.Arguments)
		if domain.CodeOf(err) != domain.CodeHostNotReady {
			t.Fatalf("Execute() error = %v, want unknown operation", err)
		}
		if queries.Load() != 0 || dispatches.Load() != 0 || store.operation.Status != task.OperationCancelled {
			t.Fatalf("query/dispatch/status = %d/%d/%q, want 0/0/cancelled", queries.Load(), dispatches.Load(), store.operation.Status)
		}
		if len(store.settlements) != 1 || store.settlements[0] != task.OperationCancelled || len(store.operation.Result) != 0 {
			t.Fatalf("prepared settlements/result = %v/%s, want safe cancellation without receipt", store.settlements, store.operation.Result)
		}
	})
}

func TestMCPRuntimeReplayAfterRuntimeRestartRechecksWorkspaceBindingBeforeReadingResult(t *testing.T) {
	fixture, executor, store, principal, currentFence, operation := newMCPRuntimeReplayFixture(t, task.OperationDispatched)
	hosts := fixture.executor.runtime.hosts.(*hostServiceTestHostStore)
	value, err := hosts.GetHost(context.Background(), operation.Key.HostID)
	if err != nil {
		t.Fatal(err)
	}
	value.Workspaces[0].BindingVersion++
	hosts.addHost(value)

	runtimeResult := mcpReplayRuntimeOperation(operation, runtimeclient.OperationSucceeded)
	var queries, dispatches atomic.Int32
	fixture.wrap = interceptMCPRuntimeOperation(&currentFence, &queries, &dispatches, &runtimeResult, nil)
	_, err = executor.Execute(context.Background(), principal, operation.Key.SessionID, operation.Key.HostID,
		operation.Key.IdempotencyKey, runtimeToolFileRead, operation.Arguments)
	if domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("Execute() error = %v, want conflict after Workspace binding changed", err)
	}
	if queries.Load() != 0 || dispatches.Load() != 0 || store.operation.Status != task.OperationDispatched {
		t.Fatalf("query/dispatch/status = %d/%d/%q, want 0/0/dispatched", queries.Load(), dispatches.Load(), store.operation.Status)
	}
}

func TestMCPRuntimeOwnerStopCancelsOldFenceOperationAfterRuntimeRestart(t *testing.T) {
	fixture, executor, store, _, currentFence, operation := newMCPRuntimeReplayFixture(t, task.OperationUnknown)
	store.pending = []MCPRuntimeOperation{operation}
	observed := mcpReplayRuntimeOperation(operation, runtimeclient.OperationUnknown)
	cancelled := mcpReplayRuntimeOperation(operation, runtimeclient.OperationCancelled)
	var queries, cancellations atomic.Int32
	fixture.wrap = interceptMCPRuntimeCancellation(&currentFence, &queries, &cancellations, &observed, &cancelled)

	if err := executor.ReconcileMCPRuntimeCancellations(context.Background()); err != nil {
		t.Fatalf("ReconcileMCPRuntimeCancellations() error = %v", err)
	}
	if queries.Load() != 1 || cancellations.Load() != 1 {
		t.Fatalf("Runtime query/cancel counts = %d/%d, want 1/1", queries.Load(), cancellations.Load())
	}
	if store.operation.Status != task.OperationCancelled || !validTestMCPRuntimeCancellationReceipt(store.operation.Result, operation) {
		t.Fatalf("settled operation = %q, result=%s; want cancelled with matching receipt", store.operation.Status, store.operation.Result)
	}
}

func TestMCPRuntimeOwnerStopDoesNotCancelMismatchedOldFenceOperation(t *testing.T) {
	fixture, executor, store, _, currentFence, operation := newMCPRuntimeReplayFixture(t, task.OperationUnknown)
	store.pending = []MCPRuntimeOperation{operation}
	observed := mcpReplayRuntimeOperation(operation, runtimeclient.OperationUnknown)
	observed.OperationID = "o7654321"
	cancelled := mcpReplayRuntimeOperation(operation, runtimeclient.OperationCancelled)
	var queries, cancellations atomic.Int32
	fixture.wrap = interceptMCPRuntimeCancellation(&currentFence, &queries, &cancellations, &observed, &cancelled)

	if err := executor.ReconcileMCPRuntimeCancellations(context.Background()); err == nil {
		t.Fatal("ReconcileMCPRuntimeCancellations() succeeded with a mismatched Runtime operation")
	}
	if queries.Load() != 1 || cancellations.Load() != 0 || store.operation.Status != task.OperationUnknown {
		t.Fatalf("query/cancel/status = %d/%d/%q, want 1/0/unknown", queries.Load(), cancellations.Load(), store.operation.Status)
	}
}

type mcpRuntimeReplayStore struct {
	operation   MCPRuntimeOperation
	settlements []task.OperationStatus
	pending     []MCPRuntimeOperation
}

func (s *mcpRuntimeReplayStore) BeginMCPRuntimeOperation(_ context.Context, _ BeginMCPRuntimeOperation) (MCPRuntimeOperation, bool, error) {
	return s.operation, true, nil
}
func (*mcpRuntimeReplayStore) MarkMCPRuntimeOperationDispatched(context.Context, MCPRuntimeOperationKey, string, AgentHostFence, int64) (MCPRuntimeOperation, error) {
	return MCPRuntimeOperation{}, errors.New("unexpected Runtime dispatch mark")
}
func (*mcpRuntimeReplayStore) RenewMCPRuntimeOperationLease(context.Context, MCPRuntimeOperationKey, string, AgentHostFence, int64, int64, time.Duration) (MCPRuntimeOperation, error) {
	return MCPRuntimeOperation{}, errors.New("unexpected Runtime lease renewal")
}
func (s *mcpRuntimeReplayStore) SettleMCPRuntimeOperation(_ context.Context, _ MCPRuntimeOperationKey, _ string, status task.OperationStatus, result json.RawMessage) (MCPRuntimeOperation, error) {
	if !task.CanTransitionOperation(s.operation.Status, status) {
		return MCPRuntimeOperation{}, domain.NewError(domain.CodeInvalidState, "invalid test MCP operation transition")
	}
	s.operation.Status = status
	s.operation.Result = append(json.RawMessage(nil), result...)
	s.settlements = append(s.settlements, status)
	return s.operation, nil
}
func (s *mcpRuntimeReplayStore) GetMCPRuntimeOperation(context.Context, MCPRuntimeOperationKey) (MCPRuntimeOperation, error) {
	return s.operation, nil
}
func (s *mcpRuntimeReplayStore) ListMCPRuntimeOperationsPendingCancellation(context.Context, int) ([]MCPRuntimeOperation, error) {
	return append([]MCPRuntimeOperation(nil), s.pending...), nil
}
func (*mcpRuntimeReplayStore) SettleMCPRuntimeOperationNotFound(context.Context, MCPRuntimeOperationKey, string) (MCPRuntimeOperation, error) {
	return MCPRuntimeOperation{}, errors.New("unexpected Runtime not-found settlement")
}
func (*mcpRuntimeReplayStore) ListActiveMCPRuntimeTaskIDs(context.Context, string, string, AgentHostFence) ([]string, error) {
	return nil, nil
}
func (*mcpRuntimeReplayStore) ListOwnerActiveMCPRuntimeOperations(context.Context, string) ([]MCPRuntimeOperation, error) {
	return nil, nil
}

type mcpRuntimeReplayHostAuthorizer struct{}

func (mcpRuntimeReplayHostAuthorizer) GetForComputer(_ context.Context, _ identity.Principal, hostID string, _ identity.Scope) (host.Host, error) {
	value := hostServiceTestHost(host.StateRunning)
	if value.ID != hostID {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "Host not found")
	}
	return value, nil
}

type mcpRuntimeRevokingHostAuthorizer struct {
	calls atomic.Int32
}

func (a *mcpRuntimeRevokingHostAuthorizer) GetForComputer(ctx context.Context, principal identity.Principal, hostID string, scope identity.Scope) (host.Host, error) {
	if a.calls.Add(1) > 1 {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "Host access was revoked")
	}
	return mcpRuntimeReplayHostAuthorizer{}.GetForComputer(ctx, principal, hostID, scope)
}

type mcpRuntimeReplayGate struct{}

func (mcpRuntimeReplayGate) GetExecutionControl(_ context.Context, ownerID string) (execution.Control, error) {
	return execution.Control{OwnerID: ownerID, State: execution.StateEnabled, Epoch: 1}, nil
}

func newMCPRuntimeReplayFixture(t *testing.T, status task.OperationStatus) (*runtimeToolExecutorFixture, *MCPRuntimeToolExecutor, *mcpRuntimeReplayStore, identity.Principal, AgentHostFence, MCPRuntimeOperation) {
	t.Helper()
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{capability: "file.read", action: "read", result: `{"path":"README.md","text":"should not dispatch"}`}})
	session, err := fixture.executor.runtime.Connect(context.Background(), "h1234567", nil)
	if err != nil {
		t.Fatal(err)
	}
	currentFence := agentFenceFromRuntime(session.Fence)
	session.Close()
	oldFence := currentFence
	oldFence.RuntimeInstanceID = "ri_previous"
	arguments := json.RawMessage(`{"workspace_id":"w1234567","path":"README.md"}`)
	hash, err := agentOperationHash(runtimeToolFileRead, arguments)
	if err != nil {
		t.Fatal(err)
	}
	key := MCPRuntimeOperationKey{OwnerID: "u1234567", APIKeyID: "t1234567", SessionID: "session_123", HostID: "h1234567", IdempotencyKey: "retry-key-123"}
	mapping, _ := runtimeToolMappingByName(runtimeToolFileRead)
	operation := MCPRuntimeOperation{
		Key: key, RuntimeTaskID: "q8765432", OperationID: "o1234567", RequestHash: hash[:],
		Capability: mapping.capability, Action: mapping.action, Arguments: arguments,
		WorkspaceID: "w1234567", BindingVersion: 1, Fence: oldFence, LeaseVersion: 1, Status: status,
	}
	store := &mcpRuntimeReplayStore{operation: operation}
	executor, err := NewMCPRuntimeToolExecutor(store, mcpRuntimeReplayHostAuthorizer{}, fixture.authorizer, mcpRuntimeReplayGate{}, fixture.executor.runtime)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{
		UserID: key.OwnerID, Kind: identity.KindAPIKey, TokenID: key.APIKeyID,
		HostIDs: []string{key.HostID}, Scopes: []identity.Scope{identity.ScopeComputerObserve},
	}
	return fixture, executor, store, principal, currentFence, operation
}

func mcpReplayRuntimeOperation(operation MCPRuntimeOperation, status runtimeclient.OperationStatus) runtimeclient.Operation {
	result := json.RawMessage(`{"path":"README.md","text":"recovered"}`)
	if status == runtimeclient.OperationFailed || status == runtimeclient.OperationCancelled {
		result = json.RawMessage(`{}`)
	}
	return runtimeclient.Operation{
		HostID: operation.Key.HostID, RuntimeGeneration: operation.Fence.RuntimeGeneration,
		RuntimeInstanceID: operation.Fence.RuntimeInstanceID, ControlVersion: operation.Fence.ControlVersion,
		TaskID: operation.RuntimeTaskID, OperationID: operation.OperationID, LeaseVersion: operation.LeaseVersion,
		Capability: operation.Capability, Action: operation.Action, WorkspaceID: operation.WorkspaceID,
		BindingVersion: operation.BindingVersion, Status: status, Result: result,
	}
}

func interceptMCPRuntimeOperation(current *AgentHostFence, queries, dispatches *atomic.Int32, value *runtimeclient.Operation, queryErr error) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path == "/runtime/v1/operations/o1234567" && request.Method == http.MethodGet {
				queries.Add(1)
				if queryErr != nil {
					writeRuntimeToolError(writer, http.StatusNotFound, "operation_not_found")
					return
				}
				response := runtimeclient.OperationResponse{
					ProtocolVersion: runtimeclient.ProtocolVersion,
					RequestFence: runtimeclient.HostFence{
						HostID: "h1234567", RuntimeGeneration: current.RuntimeGeneration,
						RuntimeInstanceID: current.RuntimeInstanceID, ControlVersion: current.ControlVersion,
					},
					Operation: *value,
				}
				writer.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(writer).Encode(response)
				return
			}
			if request.URL.Path == "/runtime/v1/operations" && request.Method == http.MethodPost {
				dispatches.Add(1)
			}
			next.ServeHTTP(writer, request)
		})
	}
}

func interceptMCPRuntimeCancellation(current *AgentHostFence, queries, cancellations *atomic.Int32, observed, cancelled *runtimeclient.Operation) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			var operation *runtimeclient.Operation
			switch {
			case request.Method == http.MethodGet && request.URL.Path == "/runtime/v1/operations/o1234567":
				queries.Add(1)
				operation = observed
			case request.Method == http.MethodPost && request.URL.Path == "/runtime/v1/operations/o1234567/cancel":
				cancellations.Add(1)
				operation = cancelled
			}
			if operation == nil {
				next.ServeHTTP(writer, request)
				return
			}
			response := runtimeclient.OperationResponse{
				ProtocolVersion: runtimeclient.ProtocolVersion,
				RequestFence: contract.HostFence{
					HostID: "h1234567", RuntimeGeneration: current.RuntimeGeneration,
					RuntimeInstanceID: current.RuntimeInstanceID, ControlVersion: current.ControlVersion,
				},
				Operation: *operation,
			}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(response)
		})
	}
}

func validTestMCPRuntimeCancellationReceipt(raw json.RawMessage, operation MCPRuntimeOperation) bool {
	var receipt struct {
		RuntimeStatus     string `json:"runtime_status"`
		OperationID       string `json:"operation_id"`
		RuntimeGeneration int64  `json:"runtime_generation"`
		RuntimeInstanceID string `json:"runtime_instance_id"`
		ControlVersion    int64  `json:"control_version"`
		LeaseVersion      int64  `json:"lease_version"`
		HostID            string `json:"host_id"`
		RuntimeTaskID     string `json:"runtime_task_id"`
	}
	return json.Unmarshal(raw, &receipt) == nil && receipt.RuntimeStatus == string(runtimeclient.OperationCancelled) &&
		receipt.OperationID == operation.OperationID && receipt.RuntimeGeneration == operation.Fence.RuntimeGeneration &&
		receipt.RuntimeInstanceID == operation.Fence.RuntimeInstanceID && receipt.ControlVersion == operation.Fence.ControlVersion &&
		receipt.LeaseVersion == operation.LeaseVersion && receipt.HostID == operation.Key.HostID && receipt.RuntimeTaskID == operation.RuntimeTaskID
}

func TestMCPRuntimeRequestMatchAllowsPostgresJSONBFormatting(t *testing.T) {
	mapping, ok := runtimeToolMappingByName(runtimeToolBrowserOpenTab)
	if !ok {
		t.Fatal("browser open tab mapping is unavailable")
	}
	arguments := json.RawMessage(`{"url":"https://example.com"}`)
	hash, err := agentOperationHash(mapping.name, arguments)
	if err != nil {
		t.Fatal(err)
	}
	key := MCPRuntimeOperationKey{
		OwnerID: "u1234567", APIKeyID: "t1234567", SessionID: "sdk-session", HostID: "h1234567", IdempotencyKey: "request-1",
	}
	operation := MCPRuntimeOperation{
		Key: key, RuntimeTaskID: "q1234567", OperationID: "o1234567", RequestHash: hash[:],
		Capability: mapping.capability, Action: mapping.action,
		// PostgreSQL jsonb returns normalized spacing after loading the stored arguments.
		Arguments:    json.RawMessage(`{"url": "https://example.com"}`),
		Fence:        AgentHostFence{RuntimeGeneration: 1, RuntimeInstanceID: "runtime-instance", ControlVersion: 0, AIAllowed: true},
		LeaseVersion: 1,
	}
	if !mcpOperationRequestMatches(operation, key, mapping.name, mapping, hash[:], "", 0) {
		t.Fatal("matching operation was rejected after PostgreSQL jsonb normalization")
	}
	otherHash := append([]byte(nil), hash[:]...)
	otherHash[0] ^= 0xff
	if mcpOperationRequestMatches(operation, key, mapping.name, mapping, otherHash, "", 0) {
		t.Fatal("operation with a different request hash was accepted")
	}
}

func TestRuntimeBrowserToolArgumentsAreBoundedAndDoNotAcceptRawSelectors(t *testing.T) {
	cases := []struct {
		name  string
		input string
		valid bool
		tool  string
	}{
		{name: "open https", tool: runtimeToolBrowserOpenTab, input: `{"url":"https://example.test/path"}`, valid: true},
		{name: "reject javascript", tool: runtimeToolBrowserOpenTab, input: `{"url":"javascript:alert(1)"}`},
		{name: "reject userinfo", tool: runtimeToolBrowserOpenTab, input: `{"url":"https://user:pass@example.test/"}`},
		{name: "navigate owned target", tool: runtimeToolBrowserNavigate, input: `{"target_id":"target_1","url":"https://example.test/"}`, valid: true},
		{name: "reject invalid target", tool: runtimeToolBrowserSnapshot, input: `{"target_id":"../target"}`},
		{name: "reject arbitrary selector", tool: runtimeToolBrowserClick, input: `{"target_id":"target_1","selector":"#send"}`},
		{name: "click by accessible name", tool: runtimeToolBrowserClick, input: `{"target_id":"target_1","role":"button","name":"Continue"}`, valid: true},
		{name: "reject raw key", tool: runtimeToolBrowserPressKey, input: `{"target_id":"target_1","key":"Control+L"}`},
		{name: "allow navigation key", tool: runtimeToolBrowserPressKey, input: `{"target_id":"target_1","key":"Enter"}`, valid: true},
		{name: "reject extra field", tool: runtimeToolBrowserType, input: `{"target_id":"target_1","role":"textbox","name":"Search","text":"query","javascript":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mapping, ok := runtimeToolMappingByName(tc.tool)
			if !ok {
				t.Fatalf("missing mapping %q", tc.tool)
			}
			if got := validRuntimeToolArguments(mapping, json.RawMessage(tc.input)); got != tc.valid {
				t.Fatalf("validRuntimeToolArguments(%s) = %v, want %v", tc.input, got, tc.valid)
			}
		})
	}
}

func TestRuntimeBrowserScreenshotMapsImageWithoutPersistingBase64AsToolOutput(t *testing.T) {
	imageBytes := []byte{0xff, 0xd8, 0xff, 0xd9}
	encoded, err := json.Marshal(struct {
		TargetID   string `json:"target_id"`
		MIMEType   string `json:"mime_type"`
		DataBase64 string `json:"data_base64"`
	}{TargetID: "target_1", MIMEType: "image/jpeg", DataBase64: base64.StdEncoding.EncodeToString(imageBytes)})
	if err != nil {
		t.Fatal(err)
	}
	mapping, ok := runtimeToolMappingByName(runtimeToolBrowserScreenshot)
	if !ok {
		t.Fatal("browser screenshot mapping missing")
	}
	result, err := mapRuntimeToolResult(mapping, runtimeclient.Operation{
		OperationID: "o1234567", Status: runtimeclient.OperationSucceeded, Result: encoded,
	}, json.RawMessage(`{"target_id":"target_1"}`))
	if err != nil {
		t.Fatalf("mapRuntimeToolResult() error = %v", err)
	}
	if result.Status != AgentToolSucceeded || len(result.Images) != 1 || result.Images[0].MediaType != "image/jpeg" || !bytes.Equal(result.Images[0].Data, imageBytes) {
		t.Fatalf("screenshot result = %#v, image bytes not preserved", result)
	}
	if strings.Contains(result.Output, base64.StdEncoding.EncodeToString(imageBytes)) {
		t.Fatal("screenshot base64 leaked into persisted tool output")
	}
}

func TestRuntimeBrowserResultMustMatchRequestedOwnedTarget(t *testing.T) {
	mapping, ok := runtimeToolMappingByName(runtimeToolBrowserNavigate)
	if !ok {
		t.Fatal("browser navigate mapping missing")
	}
	operation := runtimeclient.Operation{
		OperationID: "o1234567", Status: runtimeclient.OperationSucceeded,
		Result: json.RawMessage(`{"target_id":"target_2","url":"https://example.test/","title":"Example"}`),
	}
	result, err := mapRuntimeToolResult(mapping, operation, json.RawMessage(`{"target_id":"target_1","url":"https://example.test/next"}`))
	if err != nil || result.Status != AgentToolFailed || !strings.Contains(result.Output, "invalid_browser_result") {
		t.Fatalf("mismatched target result = %#v, %v; want failed", result, err)
	}

	operation.Result = json.RawMessage(`{"target_id":"target_1","url":"https://example.test/next","title":"Example"}`)
	result, err = mapRuntimeToolResult(mapping, operation, json.RawMessage(`{"target_id":"target_1","url":"https://example.test/next"}`))
	if err != nil || result.Status != AgentToolSucceeded {
		t.Fatalf("matching target result = %#v, %v; want succeeded", result, err)
	}
}

func TestRuntimeToolExecutorPrepareRechecksAuthorizationFenceAndCapability(t *testing.T) {
	t.Run("authorization", func(t *testing.T) {
		fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{capability: "file.read", action: "read", result: `{"path":"a","text":"b"}`}})
		fixture.authorizer.errors[identity.ScopeComputerObserve] = domain.NewError(domain.CodeForbidden, "scope denied")
		_, err := fixture.executor.Prepare(context.Background(), fixture.request(runtimeToolFileRead, `{"path":"README.md"}`, identity.ScopeComputerObserve))
		if domain.CodeOf(err) != domain.CodeForbidden {
			t.Fatalf("Prepare() error = %v, want forbidden", err)
		}
	})

	t.Run("changed host fence", func(t *testing.T) {
		fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{capability: "file.read", action: "read", result: `{"path":"a","text":"b"}`}})
		request := fixture.request(runtimeToolFileRead, `{"path":"README.md"}`, identity.ScopeComputerObserve)
		request.Fence.ControlVersion++
		_, err := fixture.executor.Prepare(context.Background(), request)
		if domain.CodeOf(err) != domain.CodeInvalidState {
			t.Fatalf("Prepare() error = %v, want stale fence", err)
		}
	})

	t.Run("capability not registered", func(t *testing.T) {
		fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{capability: "file.read", action: "read", result: `{"path":"a","text":"b"}`}})
		_, err := fixture.executor.Prepare(context.Background(), fixture.request(runtimeToolShellExec, `{"command":"id"}`, identity.ScopeComputerOperate))
		if domain.CodeOf(err) != domain.CodeUnsupported {
			t.Fatalf("Prepare() error = %v, want unsupported", err)
		}
	})
}

func TestRuntimeToolExecutorRejectsWorkspaceRevisionChangedAfterTaskSubmission(t *testing.T) {
	t.Run("before preparation", func(t *testing.T) {
		fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
			capability: "file.read", action: "read", result: `{"path":"README.md","text":"hello"}`,
		}})
		setRuntimeToolWorkspaceRevision(t, fixture, 1)
		_, err := fixture.executor.Prepare(context.Background(), fixture.request(runtimeToolFileRead,
			`{"path":"README.md"}`, identity.ScopeComputerObserve))
		if domain.CodeOf(err) != domain.CodeWorkspaceConflict || fixture.handlers["file.read"].calls.Load() != 0 {
			t.Fatalf("Prepare() error/calls = %v/%d, want Workspace conflict and no dispatch", err, fixture.handlers["file.read"].calls.Load())
		}
	})

	t.Run("after preparation before dispatch", func(t *testing.T) {
		fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
			capability: "file.read", action: "read", result: `{"path":"README.md","text":"hello"}`,
		}})
		request, preparation := fixture.prepared(runtimeToolFileRead, `{"path":"README.md"}`, identity.ScopeComputerObserve)
		fixture.persistOperation(request)
		setRuntimeToolWorkspaceRevision(t, fixture, 1)

		result, err := fixture.executor.Execute(context.Background(), request, preparation)
		if err != nil || result.Status != AgentToolNeedsUser || result.WaitingReason != "workspace_input_changed" ||
			!strings.Contains(result.Output, "Submit a new task") || fixture.handlers["file.read"].calls.Load() != 0 {
			t.Fatalf("Execute() = %#v, %v; calls=%d, want a resubmission pause and no dispatch", result, err, fixture.handlers["file.read"].calls.Load())
		}
	})

	t.Run("at runtime dispatch", func(t *testing.T) {
		fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
			capability: "file.read", action: "read", result: `{"path":"README.md","text":"hello"}`,
		}})
		setRuntimeToolWorkspaceRevision(t, fixture, 1)
		current := fixture.store.details.Task
		session, err := fixture.executor.runtime.Connect(context.Background(), current.HostID, &fixture.fence)
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		if err := session.SetTaskLease(context.Background(), current.ID, current.Lease.Version, *current.Lease.ExpiresAt); err != nil {
			t.Fatal(err)
		}
		_, err = fixture.executor.operations.Execute(context.Background(), session, current.ID, "o1234567", current.Lease.Version,
			"file.read", "read", json.RawMessage(`{"path":"README.md"}`), current.WorkspaceID, 1, current.InputRevision)
		if domain.CodeOf(err) != domain.CodeWorkspaceConflict || fixture.handlers["file.read"].calls.Load() != 0 {
			t.Fatalf("RuntimeOperationRunner.Execute() error/calls = %v/%d, want Workspace conflict and no dispatch", err, fixture.handlers["file.read"].calls.Load())
		}
	})
}

func TestRuntimeToolExecutorExecuteRechecksAuthorizationAndFence(t *testing.T) {
	t.Run("authorization revoked after prepare", func(t *testing.T) {
		fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
			capability: "file.read", action: "read", result: `{"path":"README.md","text":"hello"}`,
		}})
		request, preparation := fixture.prepared(runtimeToolFileRead, `{"path":"README.md"}`, identity.ScopeComputerObserve)
		fixture.persistOperation(request)
		fixture.authorizer.errors[identity.ScopeComputerObserve] = domain.NewError(domain.CodeForbidden, "scope revoked")

		_, err := fixture.executor.Execute(context.Background(), request, preparation)
		if domain.CodeOf(err) != domain.CodeForbidden || fixture.handlers["file.read"].calls.Load() != 0 {
			t.Fatalf("Execute() error = %v, handler calls = %d; want forbidden and no dispatch", err, fixture.handlers["file.read"].calls.Load())
		}
	})

	t.Run("fence changed after prepare", func(t *testing.T) {
		fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
			capability: "file.read", action: "read", result: `{"path":"README.md","text":"hello"}`,
		}})
		request, preparation := fixture.prepared(runtimeToolFileRead, `{"path":"README.md"}`, identity.ScopeComputerObserve)
		fixture.persistOperation(request)
		fixture.fences.fence.ControlVersion++

		_, err := fixture.executor.Execute(context.Background(), request, preparation)
		if domain.CodeOf(err) != domain.CodeInvalidState || fixture.handlers["file.read"].calls.Load() != 0 {
			t.Fatalf("Execute() error = %v, handler calls = %d; want fence rejection and no dispatch", err, fixture.handlers["file.read"].calls.Load())
		}
	})
}

func TestRuntimeToolExecutorUnknownRequestResultIsNotReplayed(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{capability: "shell.exec", action: "run", result: `{"exit_code":0,"stdout":"ok","stderr":"","stdout_truncated":false,"stderr_truncated":false,"timed_out":false}`}})
	var posts atomic.Int32
	fixture.wrap = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method == http.MethodPost && request.URL.Path == "/runtime/v1/operations" {
				posts.Add(1)
				writeRuntimeToolError(writer, http.StatusServiceUnavailable, runtimeclient.ErrorRuntimeStateUnavailable)
				return
			}
			if request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/runtime/v1/operations/") {
				writeRuntimeToolError(writer, http.StatusServiceUnavailable, runtimeclient.ErrorRuntimeStateUnavailable)
				return
			}
			next.ServeHTTP(writer, request)
		})
	}
	request, preparation := fixture.prepared(runtimeToolShellExec, `{"command":"id"}`, identity.ScopeComputerOperate)
	fixture.persistOperation(request)

	result, err := fixture.executor.Execute(context.Background(), request, preparation)
	if err != nil || result.Status != AgentToolUnknown {
		t.Fatalf("Execute() = %#v, %v; want unknown", result, err)
	}
	if posts.Load() != 1 || fixture.handlers["shell.exec"].calls.Load() != 0 {
		t.Fatalf("unknown result replayed: POSTs=%d handler_calls=%d", posts.Load(), fixture.handlers["shell.exec"].calls.Load())
	}
}

func TestRuntimeToolExecutorReconcilesUsingOriginalRuntimeLeaseAfterMordeReclaim(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: "file.read", action: "read", result: `{"path":"README.md","text":"recovered"}`,
	}})
	request, preparation := fixture.prepared(runtimeToolFileRead, `{"path":"README.md"}`, identity.ScopeComputerObserve)
	fixture.persistOperation(request)

	firstResult, err := fixture.executor.Execute(context.Background(), request, preparation)
	if err != nil || firstResult.Status != AgentToolSucceeded || fixture.handlers["file.read"].calls.Load() != 1 {
		t.Fatalf("initial Execute() = %#v, %v; handler calls = %d", firstResult, err, fixture.handlers["file.read"].calls.Load())
	}

	newExpiry := time.Now().Add(time.Minute).UTC()
	fixture.store.mu.Lock()
	fixture.store.details.Task.Lease = task.Lease{Owner: "worker-two", Version: 2, ExpiresAt: &newExpiry}
	// The recovered journal query is not a new tool call and must still run
	// after the original task has exhausted its tool-call budget.
	fixture.store.details.Task.Budget.ToolCalls = 1
	fixture.store.details.Task.Usage.ToolCalls = 1
	fixture.store.operation.Status = task.OperationUnknown
	fixture.store.operation.LeaseVersion = 2
	fixture.store.mu.Unlock()
	request.Task = fixture.store.details.Task
	persisted := fixture.store.operation

	result, err := fixture.executor.ReconcileOperation(context.Background(), request, persisted)
	if err != nil || result.Status != AgentToolSucceeded || !strings.Contains(result.Output, "recovered") {
		t.Fatalf("ReconcileOperation() = %#v, %v; want recovered success", result, err)
	}
	if fixture.handlers["file.read"].calls.Load() != 1 {
		t.Fatalf("reconciliation replayed the runtime action: handler calls = %d", fixture.handlers["file.read"].calls.Load())
	}
}

func TestRuntimeToolExecutorReadsTerminalOperationAfterWorkspaceRevisionChanges(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: "file.read", action: "read", result: `{"path":"README.md","text":"from pinned revision"}`,
	}})
	request, _ := fixture.prepared(runtimeToolFileRead, `{"path":"README.md"}`, identity.ScopeComputerObserve)
	fixture.persistOperation(request)
	fixture.store.operation.Status = task.OperationSucceeded
	fixture.store.operation.Result = `{"path":"README.md","text":"from pinned revision"}`
	setRuntimeToolWorkspaceRevision(t, fixture, 1)

	result, err := fixture.executor.ReconcileOperation(context.Background(), request, fixture.store.operation)
	if err != nil || result.Status != AgentToolSucceeded || result.Output != fixture.store.operation.Result || fixture.handlers["file.read"].calls.Load() != 0 {
		t.Fatalf("ReconcileOperation() = %#v, %v; calls=%d, want stored terminal result without runtime dispatch", result, err, fixture.handlers["file.read"].calls.Load())
	}
}

func TestRuntimeToolExecutorTreatsCancelledOperationAsUnknown(t *testing.T) {
	result, err := mapRuntimeToolResult(runtimeToolMapping{capability: "shell.exec"}, runtimeclient.Operation{
		OperationID: "o1234567", Status: runtimeclient.OperationCancelled,
	}, nil)
	if err != nil || result.Status != AgentToolUnknown {
		t.Fatalf("cancelled operation mapped to %#v, %v; want unknown", result, err)
	}
}

func TestRuntimeToolExecutorReturnsNotFoundOnlyAfterExactOriginalLeaseQuery(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{capability: "file.read", action: "read", result: `{"path":"README.md","text":"unused"}`}})
	request, _ := fixture.prepared(runtimeToolFileRead, `{"path":"README.md"}`, identity.ScopeComputerObserve)
	hash, err := agentOperationHash(request.Call.Name, request.Arguments)
	if err != nil {
		t.Fatal(err)
	}
	newExpiry := time.Now().Add(time.Minute).UTC()
	fixture.store.mu.Lock()
	fixture.store.details.Task.Lease = task.Lease{Owner: "worker-two", Version: 2, ExpiresAt: &newExpiry}
	fixture.store.operation = task.Operation{
		ID: request.OperationID, TaskID: request.Task.ID, Key: request.OperationKey,
		RequestHash: hash[:],
		HostID:      request.Task.HostID, WorkspaceID: request.WorkspaceID, BindingVersion: 1,
		RuntimeGeneration: request.Fence.RuntimeGeneration,
		RuntimeInstanceID: request.Fence.RuntimeInstanceID, ControlVersion: request.Fence.ControlVersion,
		LeaseVersion: 2, RuntimeLeaseVersion: 1, Status: task.OperationUnknown,
	}
	fixture.store.mu.Unlock()
	request.Task = fixture.store.details.Task

	result, err := fixture.executor.ReconcileOperation(context.Background(), request, fixture.store.operation)
	if err != nil || result.Status != AgentToolNotFound {
		t.Fatalf("ReconcileOperation() = %#v, %v; want explicit not-found", result, err)
	}
	if fixture.handlers["file.read"].calls.Load() != 0 {
		t.Fatalf("not-found reconciliation dispatched the handler %d times", fixture.handlers["file.read"].calls.Load())
	}
}

func TestRuntimeToolExecutorRetriesSameIDOnlyAfterExplicitNotFound(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{capability: "shell.exec", action: "run", result: `{"exit_code":0,"stdout":"ok","stderr":"","stdout_truncated":false,"stderr_truncated":false,"timed_out":false}`}})
	var posts atomic.Int32
	fixture.wrap = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method == http.MethodPost && request.URL.Path == "/runtime/v1/operations" && posts.Add(1) == 1 {
				writeRuntimeToolError(writer, http.StatusServiceUnavailable, runtimeclient.ErrorRuntimeStateUnavailable)
				return
			}
			next.ServeHTTP(writer, request)
		})
	}
	request, preparation := fixture.prepared(runtimeToolShellExec, `{"command":"id"}`, identity.ScopeComputerOperate)
	fixture.persistOperation(request)

	result, err := fixture.executor.Execute(context.Background(), request, preparation)
	if err != nil || result.Status != AgentToolSucceeded {
		t.Fatalf("Execute() = %#v, %v; want success", result, err)
	}
	if posts.Load() != 2 || fixture.handlers["shell.exec"].calls.Load() != 1 {
		t.Fatalf("same-ID retry = POSTs %d / handler calls %d, want 2 / 1", posts.Load(), fixture.handlers["shell.exec"].calls.Load())
	}
}

func TestRuntimeToolExecutorPollsAndMapsShellExitCode(t *testing.T) {
	handler := &runtimeToolTestHandler{
		capability: "shell.exec", action: "run",
		result: `{"exit_code":7,"stdout":"partial","stderr":"failed","stdout_truncated":false,"stderr_truncated":false,"timed_out":false}`,
		delay:  20 * time.Millisecond,
	}
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{handler: handler}})
	fixture.executor.operations.pollEvery = time.Millisecond
	request, preparation := fixture.prepared(runtimeToolShellExec, `{"command":"false"}`, identity.ScopeComputerOperate)
	fixture.persistOperation(request)

	result, err := fixture.executor.Execute(context.Background(), request, preparation)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Status != AgentToolFailed || !strings.Contains(result.Output, `"exit_code":7`) || !strings.Contains(result.Output, `"stderr":"failed"`) {
		t.Fatalf("result = %#v, want failed shell result with exit code 7", result)
	}
	if handler.calls.Load() != 1 {
		t.Fatalf("runtime command calls = %d, want 1", handler.calls.Load())
	}
}

func TestRuntimeToolExecutorRenewsRuntimeLeaseDuringLongOperation(t *testing.T) {
	handler := &runtimeToolTestHandler{
		capability: "shell.exec", action: "run",
		result: `{"exit_code":0,"stdout":"done","stderr":"","stdout_truncated":false,"stderr_truncated":false,"timed_out":false}`,
		delay:  700 * time.Millisecond,
	}
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{handler: handler}})
	fixture.executor.operations.pollEvery = 5 * time.Millisecond
	fixture.executor.leaseEvery = 10 * time.Millisecond
	initialExpiry := time.Now().Add(500 * time.Millisecond).UTC()
	fixture.store.setLeaseExpiry(initialExpiry)
	request, preparation := fixture.prepared(runtimeToolShellExec, `{"command":"sleep 1"}`, identity.ScopeComputerOperate)
	fixture.persistOperation(request)

	resultCh := make(chan struct {
		result AgentToolResult
		err    error
	}, 1)
	go func() {
		result, err := fixture.executor.Execute(context.Background(), request, preparation)
		resultCh <- struct {
			result AgentToolResult
			err    error
		}{result: result, err: err}
	}()
	time.Sleep(120 * time.Millisecond)
	fixture.store.setLeaseExpiry(time.Now().Add(time.Second).UTC())

	select {
	case got := <-resultCh:
		if got.err != nil || got.result.Status != AgentToolSucceeded {
			t.Fatalf("Execute() = %#v, %v; want success after lease renewal", got.result, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("long operation did not complete after runtime lease renewal")
	}
	if handler.calls.Load() != 1 {
		t.Fatalf("runtime command calls = %d, want exactly one", handler.calls.Load())
	}
}

func TestRuntimeToolExecutorMapsFileResultAndRuntimeFailure(t *testing.T) {
	t.Run("file result", func(t *testing.T) {
		fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
			capability: "file.read", action: "read", result: `{"path":"README.md","text":"hello"}`,
		}})
		request, preparation := fixture.prepared(runtimeToolFileRead, `{"path":"README.md"}`, identity.ScopeComputerObserve)
		fixture.persistOperation(request)

		result, err := fixture.executor.Execute(context.Background(), request, preparation)
		if err != nil || result.Status != AgentToolSucceeded || result.Output != `{"path":"README.md","text":"hello"}` {
			t.Fatalf("Execute() = %#v, %v; want successful file result", result, err)
		}
	})

	t.Run("runtime handler failure", func(t *testing.T) {
		handler := &runtimeToolTestHandler{capability: "file.read", action: "read", err: errors.New("private handler detail")}
		fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{handler: handler}})
		request, preparation := fixture.prepared(runtimeToolFileRead, `{"path":"README.md"}`, identity.ScopeComputerObserve)
		fixture.persistOperation(request)

		result, err := fixture.executor.Execute(context.Background(), request, preparation)
		if err != nil || result.Status != AgentToolFailed || result.Output != `{"error":"runtime_operation_failed"}` {
			t.Fatalf("Execute() = %#v, %v; want sanitized operation failure", result, err)
		}
		if strings.Contains(result.Output, "private handler detail") {
			t.Fatal("runtime handler detail escaped to the Agent")
		}
	})
}

func TestRuntimeToolExecutorVerifyDoesNotTrustClaim(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	result, err := fixture.executor.Verify(context.Background(), AgentVerificationRequest{Claim: "Everything is complete."})
	if err != nil || result.Status != AgentVerifyUnknown {
		t.Fatalf("Verify() = %#v, %v; want unknown", result, err)
	}
}

func TestRuntimeToolExecutorScheduledObservationUsesPrivateFingerprintTool(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: "browser.observe", action: "field",
		result: `{"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","origin":"https://example.test","match_count":1}`,
	}})
	spec := schedule.ObservationSpec{URL: "https://example.test/status", Selector: "#status", Field: schedule.ObservationText}
	arguments, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := fixture.executor.Prepare(context.Background(), AgentToolRequest{
		Task:      fixture.store.details.Task,
		Call:      ModelToolCall{CallID: "scheduled-observation:s1234567", Name: runtimeToolBrowserObserveField},
		Arguments: arguments, OperationKey: "agent:observation:s1234567", RequiredScope: identity.ScopeComputerObserve,
		Fence: fixture.fence,
	})
	if err != nil || prepared.Status != AgentToolReady {
		t.Fatalf("Prepare() = %#v, %v; want ready", prepared, err)
	}
	fixture.fence = prepared.Fence
	fixture.fences.fence = prepared.Fence
	result, err := fixture.executor.Verify(context.Background(), AgentVerificationRequest{
		Task: fixture.store.details.Task, StepID: "s1234567", ScheduleObservation: &spec, Fence: fixture.fence,
	})
	if err != nil || result.Status != AgentVerified || result.ObservationFingerprint != strings.Repeat("a", 64) || strings.Contains(result.Summary, "example") {
		t.Fatalf("scheduled Verify() = %#v, %v; operation=%#v; handler calls=%d; want verified fingerprint without observed page content", result, err, fixture.store.operation, fixture.handlers["browser.observe"].calls.Load())
	}
	if fixture.store.operation.Status != task.OperationSucceeded || strings.Contains(fixture.store.operation.Result, "text") || strings.Contains(fixture.store.operation.Result, "value") {
		t.Fatalf("persisted observation = %#v; want only a private fingerprint result", fixture.store.operation)
	}
	definitions, err := fixture.executor.Definitions(context.Background(), fixture.store.details.Task)
	if err != nil {
		t.Fatalf("Definitions() error = %v", err)
	}
	for _, definition := range definitions {
		if definition.Name == runtimeToolBrowserObserveField {
			t.Fatal("internal scheduled observation tool was exposed to the model")
		}
	}
}

func TestRuntimeToolExecutorVerifyRequiresRuntimeFileMatchAndPersistsIdempotentOperation(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: runtimeFileVerifyCapability, action: runtimeFileVerifyAction,
		result: `{"files":[{"path":"notes/hello.txt","size":5,"sha256":"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824","matches":true}]}`,
	}})
	evidence := []AgentFileEvidence{seedSuccessfulFileWrite(t, fixture, "w1234567", "notes/hello.txt", "hello")}
	request := AgentVerificationRequest{Task: fixture.store.details.Task, StepID: "s1234567", Claim: "ignore this claim", Evidence: evidence, Fence: fixture.fence}
	result, err := fixture.executor.Verify(context.Background(), request)
	if err != nil || result.Status != AgentVerified || len(result.Artifacts) != 1 {
		t.Fatalf("Verify() = %#v, %v; want verified matching file", result, err)
	}
	artifact := result.Artifacts[0]
	if artifact.Path != "notes/hello.txt" || artifact.Label != "hello.txt" || !strings.HasPrefix(artifact.MediaType, "text/plain") ||
		artifact.WorkspaceID != "w1234567" || artifact.Revision != 4 || artifact.SizeBytes != 5 ||
		artifact.SHA256 != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("artifact = %#v; want basename label, Workspace path, and extension-derived MIME", artifact)
	}
	operationKey := runtimeVerificationOperationKey(request.StepID, "w1234567", 1)
	if operationKey != "agent:verify:"+request.StepID+":w1234567:1" {
		t.Fatalf("verification operation key = %q; want step ID, Workspace ID, and binding version", operationKey)
	}
	if fixture.store.operation.Key != operationKey || fixture.store.operation.Status != task.OperationSucceeded ||
		!reflect.DeepEqual(fixture.store.operationTransitions, []task.OperationStatus{task.OperationPrepared, task.OperationDispatched, task.OperationSucceeded}) {
		t.Fatalf("persisted operation = %#v with transitions %#v; want prepared -> dispatched -> succeeded", fixture.store.operation, fixture.store.operationTransitions)
	}
	if fixture.handlers[runtimeFileVerifyCapability].calls.Load() != 1 {
		t.Fatalf("runtime verify calls = %d; want one", fixture.handlers[runtimeFileVerifyCapability].calls.Load())
	}

	request.Claim = "different claim must not change evidence or re-run verification"
	replayed, err := fixture.executor.Verify(context.Background(), request)
	if err != nil || replayed.Status != AgentVerified || fixture.handlers[runtimeFileVerifyCapability].calls.Load() != 1 {
		t.Fatalf("replayed Verify() = %#v, %v; runtime calls = %d; want stored verified result and no replay", replayed, err, fixture.handlers[runtimeFileVerifyCapability].calls.Load())
	}
	request.Evidence = []AgentFileEvidence{evidence[0]}
	request.Evidence[0].Size = 4
	conflict, err := fixture.executor.Verify(context.Background(), request)
	if err != nil || conflict.Status != AgentVerifyUnknown || fixture.handlers[runtimeFileVerifyCapability].calls.Load() != 1 {
		t.Fatalf("same-step changed evidence Verify() = %#v, %v; runtime calls = %d; want fail-closed idempotency conflict", conflict, err, fixture.handlers[runtimeFileVerifyCapability].calls.Load())
	}
}

func TestRuntimeToolExecutorVerifyRejectsFileWriteOutputNotInLedger(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: runtimeFileVerifyCapability, action: runtimeFileVerifyAction,
		result: `{"files":[{"path":"notes/hello.txt","size":5,"sha256":"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824","matches":true}]}`,
	}})
	evidence := seedSuccessfulFileWrite(t, fixture, "w1234567", "notes/hello.txt", "hello")
	fixture.store.mu.Lock()
	operation := fixture.store.operations[evidence.OperationKey]
	operation.Result = `{"path":"notes/other.txt","bytes_written":5}`
	fixture.store.operations[evidence.OperationKey] = operation
	fixture.store.mu.Unlock()

	result, err := fixture.executor.Verify(context.Background(), AgentVerificationRequest{
		Task: fixture.store.details.Task, StepID: "s1234567", Evidence: []AgentFileEvidence{evidence}, Fence: fixture.fence,
	})
	if err != nil || result.Status != AgentVerifyUnknown || fixture.handlers[runtimeFileVerifyCapability].calls.Load() != 0 {
		t.Fatalf("Verify() = %#v, %v; runtime calls = %d; want ledger mismatch rejected before verification",
			result, err, fixture.handlers[runtimeFileVerifyCapability].calls.Load())
	}
}

func TestRuntimeToolExecutorVerifyRequiresCommittedWorkspaceBlockReadback(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: runtimeFileVerifyCapability, action: runtimeFileVerifyAction,
		result: `{"files":[{"path":"notes/hello.txt","size":5,"sha256":"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824","matches":true}]}`,
	}})
	fixture.workspaceBlocks.data["2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"] = []byte("jello")
	evidence := []AgentFileEvidence{seedSuccessfulFileWrite(t, fixture, "w1234567", "notes/hello.txt", "hello")}
	result, err := fixture.executor.Verify(context.Background(), AgentVerificationRequest{
		Task: fixture.store.details.Task, StepID: "s1234567", Evidence: evidence, Fence: fixture.fence,
	})
	if err != nil || result.Status != AgentVerifyUnknown || len(result.Artifacts) != 0 {
		t.Fatalf("Verify() with corrupt Morde block = %#v, %v; want unknown without artifact", result, err)
	}
}

func TestRuntimeToolExecutorVerifyAgainstWorkspaceFileHandler(t *testing.T) {
	workspacePath := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspacePath, 0o700); err != nil {
		t.Fatal(err)
	}
	workspaceDirectory := filepath.Join(workspacePath, "w1234567")
	if err := os.Mkdir(workspaceDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspaceDirectory, "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(workspaceDirectory, "notes", "hello.txt")
	if err := os.WriteFile(filePath, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := runtimefiles.OpenWorkspace(workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := workspace.Close(); err != nil {
			t.Errorf("close runtime file workspace: %v", err)
		}
	})
	handlers := workspace.Handlers()
	var verifyHandler runtimecore.ActionHandler
	for _, handler := range handlers {
		if handler.Capability() == runtimeFileVerifyCapability {
			verifyHandler = handler
			break
		}
	}
	if verifyHandler == nil {
		t.Fatal("runtime workspace did not register file.verify")
	}
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{runtimeAction: verifyHandler}})
	request := AgentVerificationRequest{
		Task: fixture.store.details.Task, StepID: "sverfy01",
		Evidence: []AgentFileEvidence{seedSuccessfulFileWrite(t, fixture, "w1234567", "notes/hello.txt", "hello")},
		Fence:    fixture.fence,
	}
	matched, err := fixture.executor.Verify(context.Background(), request)
	if err != nil || matched.Status != AgentVerified || len(matched.Artifacts) != 1 || matched.Artifacts[0].Path != "notes/hello.txt" {
		t.Fatalf("Verify() against current workspace = %#v, %v; want independently verified file artifact", matched, err)
	}

	if err := os.WriteFile(filePath, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	changedWorkspace, err := runtimefiles.OpenWorkspace(workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := changedWorkspace.Close(); err != nil {
			t.Errorf("close changed runtime file workspace: %v", err)
		}
	})
	var changedVerifyHandler runtimecore.ActionHandler
	for _, handler := range changedWorkspace.Handlers() {
		if handler.Capability() == runtimeFileVerifyCapability {
			changedVerifyHandler = handler
			break
		}
	}
	if changedVerifyHandler == nil {
		t.Fatal("changed runtime workspace did not register file.verify")
	}
	changedFixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{runtimeAction: changedVerifyHandler}})
	request = AgentVerificationRequest{
		Task: changedFixture.store.details.Task, StepID: "sverfy02",
		Evidence: []AgentFileEvidence{seedSuccessfulFileWrite(t, changedFixture, "w1234567", "notes/hello.txt", "hello")},
		Fence:    changedFixture.fence,
	}
	changed, err := changedFixture.executor.Verify(context.Background(), request)
	if err != nil || changed.Status != AgentNotVerified || len(changed.Artifacts) != 0 {
		t.Fatalf("Verify() after Workspace content changed = %#v, %v; want no artifact", changed, err)
	}
}

func TestRuntimeToolExecutorVerifyRejectsMismatchedRuntimeEvidence(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: runtimeFileVerifyCapability, action: runtimeFileVerifyAction,
		result: `{"files":[{"path":"notes/hello.txt","size":4,"sha256":"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824","matches":false}]}`,
	}})
	evidence := []AgentFileEvidence{seedSuccessfulFileWrite(t, fixture, "w1234567", "notes/hello.txt", "hello")}
	result, err := fixture.executor.Verify(context.Background(), AgentVerificationRequest{
		Task: fixture.store.details.Task, StepID: "s1234567", Evidence: evidence, Fence: fixture.fence,
	})
	if err != nil || result.Status != AgentNotVerified || len(result.Artifacts) != 0 || result.Observation == "" {
		t.Fatalf("Verify() = %#v, %v; want not_verified without artifacts", result, err)
	}
	if fixture.store.operation.Status != task.OperationSucceeded {
		t.Fatalf("operation status = %q; want persisted runtime response status succeeded", fixture.store.operation.Status)
	}
}

func TestRuntimeToolExecutorVerifyFailsClosedOnUnknownWithoutReplay(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: runtimeFileVerifyCapability, action: runtimeFileVerifyAction,
		result: `{"files":[{"path":"notes/hello.txt","size":5,"sha256":"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824","matches":true}]}`,
	}})
	evidence := []AgentFileEvidence{seedSuccessfulFileWrite(t, fixture, "w1234567", "notes/hello.txt", "hello")}
	var posts atomic.Int32
	fixture.wrap = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method == http.MethodPost && request.URL.Path == "/runtime/v1/operations" {
				posts.Add(1)
				writeRuntimeToolError(writer, http.StatusServiceUnavailable, runtimeclient.ErrorRuntimeStateUnavailable)
				return
			}
			next.ServeHTTP(writer, request)
		})
	}
	request := AgentVerificationRequest{Task: fixture.store.details.Task, StepID: "s1234567", Evidence: evidence, Fence: fixture.fence}
	result, err := fixture.executor.Verify(context.Background(), request)
	if err != nil || result.Status != AgentVerifyUnknown || fixture.store.operation.Status != task.OperationUnknown {
		t.Fatalf("Verify() = %#v, %v; operation = %q; want unknown persisted", result, err, fixture.store.operation.Status)
	}
	replayed, err := fixture.executor.Verify(context.Background(), request)
	if err != nil || replayed.Status != AgentVerifyUnknown || posts.Load() != 1 || fixture.handlers[runtimeFileVerifyCapability].calls.Load() != 0 {
		t.Fatalf("replayed Verify() = %#v, %v; posts = %d / handler calls = %d; want unknown without replay", replayed, err, posts.Load(), fixture.handlers[runtimeFileVerifyCapability].calls.Load())
	}
}

func TestRuntimeToolExecutorVerifyAdoptsOnlyPreparedOperationAfterLeaseRecovery(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: runtimeFileVerifyCapability, action: runtimeFileVerifyAction,
		result: `{"files":[{"path":"notes/hello.txt","size":5,"sha256":"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824","matches":true}]}`,
	}})
	evidence := []AgentFileEvidence{seedSuccessfulFileWrite(t, fixture, "w1234567", "notes/hello.txt", "hello")}
	fixture.store.failNextDispatch = true
	request := AgentVerificationRequest{Task: fixture.store.details.Task, StepID: "s1234567", Evidence: evidence, Fence: fixture.fence}
	first, err := fixture.executor.Verify(context.Background(), request)
	if err != nil || first.Status != AgentVerifyUnknown || fixture.store.operation.Status != task.OperationPrepared || fixture.handlers[runtimeFileVerifyCapability].calls.Load() != 0 {
		t.Fatalf("first Verify() = %#v, %v; operation=%q, handler calls=%d; want undispatched prepared operation", first, err, fixture.store.operation.Status, fixture.handlers[runtimeFileVerifyCapability].calls.Load())
	}

	expires := time.Now().Add(time.Minute).UTC()
	fixture.store.mu.Lock()
	fixture.store.details.Task.Lease = task.Lease{Owner: "worker-two", Version: 2, ExpiresAt: &expires}
	request.Task = fixture.store.details.Task
	fixture.store.mu.Unlock()
	second, err := fixture.executor.Verify(context.Background(), request)
	if err != nil || second.Status != AgentVerified || fixture.store.operation.Status != task.OperationSucceeded ||
		fixture.store.operation.LeaseVersion != 2 || fixture.handlers[runtimeFileVerifyCapability].calls.Load() != 1 {
		t.Fatalf("recovered Verify() = %#v, %v; operation=%#v, handler calls=%d; want safe adoption and one dispatch", second, err, fixture.store.operation, fixture.handlers[runtimeFileVerifyCapability].calls.Load())
	}
	if !reflect.DeepEqual(fixture.store.operationTransitions, []task.OperationStatus{task.OperationPrepared, task.OperationPrepared, task.OperationDispatched, task.OperationSucceeded}) {
		t.Fatalf("operation transitions = %#v; want prepared, adopted prepared, dispatched, succeeded", fixture.store.operationTransitions)
	}
}

func TestRuntimeToolExecutorVerifyReconcilesSucceededRuntimeOperationAfterWorkerRestart(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: runtimeFileVerifyCapability, action: runtimeFileVerifyAction,
		result: `{"files":[{"path":"notes/hello.txt","size":5,"sha256":"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824","matches":true}]}`,
	}})
	evidence := []AgentFileEvidence{seedSuccessfulFileWrite(t, fixture, "w1234567", "notes/hello.txt", "hello")}
	var posts atomic.Int32
	fixture.wrap = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method == http.MethodPost && request.URL.Path == "/runtime/v1/operations" {
				posts.Add(1)
			}
			next.ServeHTTP(writer, request)
		})
	}
	fixture.store.failNextSucceededUpdate = true
	request := AgentVerificationRequest{Task: fixture.store.details.Task, StepID: "s1234567", Evidence: evidence, Fence: fixture.fence}
	first, err := fixture.executor.Verify(context.Background(), request)
	if err != nil || first.Status != AgentVerifyUnknown || fixture.store.operation.Status != task.OperationDispatched ||
		fixture.handlers[runtimeFileVerifyCapability].calls.Load() != 1 || posts.Load() != 1 {
		t.Fatalf("first Verify() = %#v, %v; operation=%q, posts=%d, handler calls=%d; want runtime success with uncommitted DB status", first, err, fixture.store.operation.Status, posts.Load(), fixture.handlers[runtimeFileVerifyCapability].calls.Load())
	}

	// Model the durable task-claim recovery transition: the dispatched
	// operation becomes unknown on the new task lease, but retains the original
	// runtime lease and operation ID for a read-only journal query.
	expires := time.Now().Add(time.Minute).UTC()
	fixture.store.mu.Lock()
	fixture.store.details.Task.Lease = task.Lease{Owner: "worker-two", Version: 2, ExpiresAt: &expires}
	fixture.store.operation.Status = task.OperationUnknown
	fixture.store.operation.LeaseVersion = 2
	request.Task = fixture.store.details.Task
	fixture.store.mu.Unlock()

	reconciled, err := fixture.executor.Verify(context.Background(), request)
	if err != nil || reconciled.Status != AgentVerified || fixture.store.operation.Status != task.OperationSucceeded ||
		fixture.handlers[runtimeFileVerifyCapability].calls.Load() != 1 || posts.Load() != 1 {
		t.Fatalf("reconciled Verify() = %#v, %v; operation=%q, posts=%d, handler calls=%d; want recovered success without dispatch", reconciled, err, fixture.store.operation.Status, posts.Load(), fixture.handlers[runtimeFileVerifyCapability].calls.Load())
	}
}

func TestRuntimeToolExecutorVerifyRequiresCurrentFenceAndEvidence(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: runtimeFileVerifyCapability, action: runtimeFileVerifyAction,
		result: `{"files":[{"path":"notes/hello.txt","size":5,"sha256":"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824","matches":true}]}`,
	}})
	missing, err := fixture.executor.Verify(context.Background(), AgentVerificationRequest{
		Task: fixture.store.details.Task, StepID: "s1234567", Claim: "the claim says done", Fence: fixture.fence,
	})
	if err != nil || missing.Status != AgentVerifyUnknown || fixture.store.operation.ID != "" {
		t.Fatalf("Verify() without evidence = %#v, %v; operation = %#v; want unknown without dispatch", missing, err, fixture.store.operation)
	}
	evidence := []AgentFileEvidence{seedSuccessfulFileWrite(t, fixture, "w1234567", "notes/hello.txt", "hello")}
	fixture.fences.fence.ControlVersion++
	stale, err := fixture.executor.Verify(context.Background(), AgentVerificationRequest{
		Task: fixture.store.details.Task, StepID: "s1234567", Evidence: evidence, Fence: fixture.fence,
	})
	if err != nil || stale.Status != AgentVerifyUnknown || fixture.store.operation.ID != "" || fixture.handlers[runtimeFileVerifyCapability].calls.Load() != 0 {
		t.Fatalf("Verify() on changed fence = %#v, %v; operation = %#v; handler calls = %d; want fail-closed without dispatch", stale, err, fixture.store.operation, fixture.handlers[runtimeFileVerifyCapability].calls.Load())
	}
}

func TestRuntimeToolExecutorVerifyFailsClosedOnCrossWorkspaceArtifactPathConflict(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: runtimeFileVerifyCapability, action: runtimeFileVerifyAction,
		result: `{"files":[]}`,
	}})
	evidence := []AgentFileEvidence{
		seedSuccessfulFileWrite(t, fixture, "w1234567", "notes/hello.txt", "hello"),
		seedSuccessfulFileWrite(t, fixture, "w7654321", "notes/hello.txt", "other"),
	}
	result, err := fixture.executor.Verify(context.Background(), AgentVerificationRequest{
		Task: fixture.store.details.Task, StepID: "s1234567", Evidence: evidence, Fence: fixture.fence,
	})
	if err != nil || result.Status != AgentNotVerified || !strings.Contains(result.Summary, "notes/hello.txt") ||
		!strings.Contains(result.Summary, "w1234567") || !strings.Contains(result.Summary, "w7654321") {
		t.Fatalf("Verify() = %#v, %v; want located fail-closed path conflict", result, err)
	}
	if fixture.handlers[runtimeFileVerifyCapability].calls.Load() != 0 || fixture.store.operation.ID != "" {
		t.Fatalf("verification handler calls=%d; operation=%#v; path conflict must not dispatch or create verification operation",
			fixture.handlers[runtimeFileVerifyCapability].calls.Load(), fixture.store.operation)
	}
}

func seedSuccessfulFileWrite(t *testing.T, fixture *runtimeToolExecutorFixture, workspaceID, filePath, content string) AgentFileEvidence {
	t.Helper()
	arguments, err := json.Marshal(struct {
		WorkspaceID string `json:"workspace_id"`
		Path        string `json:"path"`
		Content     string `json:"content"`
	}{WorkspaceID: workspaceID, Path: filePath, Content: content})
	if err != nil {
		t.Fatal(err)
	}
	callID := "test-file-write:" + workspaceID + ":" + filePath
	output, err := json.Marshal(struct {
		Path         string `json:"path"`
		BytesWritten int    `json:"bytes_written"`
	}{Path: filePath, BytesWritten: len([]byte(content))})
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := agentFileWriteEvidence([]ModelInput{
		{Type: "function_call", CallID: callID, Name: runtimeToolFileWrite, Arguments: string(arguments)},
		{Type: "function_call_output", CallID: callID, Output: string(output)},
	}, nil)
	if err != nil || len(evidence) != 1 {
		t.Fatalf("agentFileWriteEvidence() = %#v, %v; want one file candidate", evidence, err)
	}
	operationID, err := domain.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	operation := task.Operation{
		ID: operationID, TaskID: fixture.store.details.Task.ID, Key: evidence[0].OperationKey,
		RequestHash: append([]byte(nil), evidence[0].RequestHash...), HostID: fixture.store.details.Task.HostID,
		WorkspaceID: workspaceID, BindingVersion: 1, Status: task.OperationSucceeded, Result: string(output),
	}
	fixture.store.mu.Lock()
	fixture.store.operations[operation.Key] = operation
	fixture.store.mu.Unlock()
	return evidence[0]
}

type runtimeToolExecutorFixture struct {
	executor        *RuntimeToolExecutor
	store           *runtimeToolTaskStore
	authorizer      *runtimeToolAuthorizer
	fence           AgentHostFence
	fences          *runtimeToolFenceReader
	handlers        map[string]*runtimeToolTestHandler
	wrap            func(http.Handler) http.Handler
	manager         *runtimecore.Manager
	server          *httptest.Server
	workspaceBlocks *proofBlocks
}

type runtimeToolHandlerSpec struct {
	capability    string
	action        string
	result        string
	delay         time.Duration
	handler       *runtimeToolTestHandler
	runtimeAction runtimecore.ActionHandler
}

type runtimeToolTestHandler struct {
	capability string
	action     string
	result     string
	delay      time.Duration
	err        error
	calls      atomic.Int32
}

func (h *runtimeToolTestHandler) Capability() string { return h.capability }
func (h *runtimeToolTestHandler) Action() string     { return h.action }
func (h *runtimeToolTestHandler) Execute(ctx context.Context, _ contract.ExecuteRequest) (json.RawMessage, error) {
	h.calls.Add(1)
	if h.delay > 0 {
		timer := time.NewTimer(h.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if h.err != nil {
		return nil, h.err
	}
	return json.RawMessage(h.result), nil
}
func (*runtimeToolTestHandler) Stop(context.Context, contract.Operation) (runtimecore.StopResult, error) {
	return runtimecore.StopResult{VerifiedStopped: true}, nil
}

type runtimeToolTaskStore struct {
	TaskStore
	mu                      sync.RWMutex
	details                 TaskDetails
	operation               task.Operation
	operations              map[string]task.Operation
	operationTransitions    []task.OperationStatus
	failNextDispatch        bool
	failNextSucceededUpdate bool
}

func (s *runtimeToolTaskStore) GetTask(ctx context.Context, id, ownerID string) (TaskDetails, error) {
	if err := ctx.Err(); err != nil {
		return TaskDetails{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id != s.details.Task.ID || ownerID != s.details.Task.OwnerID {
		return TaskDetails{}, domain.NewError(domain.CodeNotFound, "task not found")
	}
	return s.details, nil
}

func (s *runtimeToolTaskStore) setLeaseExpiry(expiresAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.details.Task.Lease.ExpiresAt = &expiresAt
}

func (s *runtimeToolTaskStore) GetOperation(ctx context.Context, ownerID, taskID, key string) (task.Operation, error) {
	if err := ctx.Err(); err != nil {
		return task.Operation{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if ownerID != s.details.Task.OwnerID || taskID != s.details.Task.ID {
		return task.Operation{}, domain.NewError(domain.CodeNotFound, "operation not found")
	}
	if s.operation.Key == key {
		return s.operation, nil
	}
	if operation, ok := s.operations[key]; ok {
		return operation, nil
	}
	return task.Operation{}, domain.NewError(domain.CodeNotFound, "operation not found")
}

func (s *runtimeToolTaskStore) CreateOperation(ctx context.Context, value task.Operation, leaseOwner string) (task.Operation, bool, error) {
	if err := ctx.Err(); err != nil {
		return task.Operation{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.details.Task
	if current.ID != value.TaskID || current.OwnerID == "" || current.Lease.Owner != leaseOwner || current.Lease.Version != value.LeaseVersion ||
		current.Lease.ExpiresAt == nil || !current.Lease.ExpiresAt.After(time.Now()) || value.HostID != current.HostID {
		return task.Operation{}, false, domain.NewError(domain.CodeInvalidState, "stale operation task lease")
	}
	if s.operation.Key == value.Key {
		if !bytes.Equal(s.operation.RequestHash, value.RequestHash) || s.operation.HostID != value.HostID ||
			s.operation.WorkspaceID != value.WorkspaceID || s.operation.BindingVersion != value.BindingVersion {
			return task.Operation{}, false, domain.NewError(domain.CodeConflict, "operation request changed")
		}
		return s.operation, true, nil
	}
	if operation, ok := s.operations[value.Key]; ok {
		if !bytes.Equal(operation.RequestHash, value.RequestHash) || operation.HostID != value.HostID ||
			operation.WorkspaceID != value.WorkspaceID || operation.BindingVersion != value.BindingVersion {
			return task.Operation{}, false, domain.NewError(domain.CodeConflict, "operation request changed")
		}
		return operation, true, nil
	}
	if value.Status != task.OperationPrepared {
		return task.Operation{}, false, domain.NewError(domain.CodeInvalidArgument, "new operation must be prepared")
	}
	s.operation = value
	if s.operations == nil {
		s.operations = make(map[string]task.Operation)
	}
	s.operations[value.Key] = value
	s.operationTransitions = append(s.operationTransitions, value.Status)
	return value, false, nil
}

func (s *runtimeToolTaskStore) UpdateOperation(ctx context.Context, update OperationUpdate) (task.Operation, error) {
	if err := ctx.Err(); err != nil {
		return task.Operation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.details.Task
	operation, ok := s.operations[update.OperationKey]
	if s.operation.Key == update.OperationKey {
		operation, ok = s.operation, true
	}
	if !ok || operation.ID == "" || current.ID != update.TaskID ||
		current.Lease.Owner != update.LeaseOwner || current.Lease.Version != update.LeaseVersion ||
		current.Lease.ExpiresAt == nil || !current.Lease.ExpiresAt.After(time.Now()) {
		return task.Operation{}, domain.NewError(domain.CodeInvalidState, "stale operation update lease")
	}
	if operation.LeaseVersion != update.LeaseVersion {
		if operation.Status != task.OperationPrepared || !update.AdoptPrepared || update.Status != task.OperationPrepared {
			return task.Operation{}, domain.NewError(domain.CodeInvalidState, "operation lease changed")
		}
		operation.LeaseVersion = update.LeaseVersion
		operation.RuntimeLeaseVersion = update.RuntimeLeaseVersion
		operation.RuntimeGeneration = update.RuntimeGeneration
		operation.RuntimeInstanceID = update.RuntimeInstanceID
		operation.ControlVersion = update.ControlVersion
		s.operationTransitions = append(s.operationTransitions, task.OperationPrepared)
	} else if update.AdoptPrepared {
		return task.Operation{}, domain.NewError(domain.CodeInvalidState, "cannot adopt an operation in the same lease")
	}
	if update.Status == operation.Status {
		if update.ExternalID == operation.ExternalID && update.Result == operation.Result {
			if update.AdoptPrepared {
				s.operation = operation
				if s.operations == nil {
					s.operations = make(map[string]task.Operation)
				}
				s.operations[operation.Key] = operation
			}
			return operation, nil
		}
		return task.Operation{}, domain.NewError(domain.CodeInvalidState, "operation result changed")
	}
	if !task.CanTransitionOperation(operation.Status, update.Status) {
		return task.Operation{}, domain.NewError(domain.CodeInvalidState, "invalid operation state transition")
	}
	if update.Status == task.OperationDispatched && s.failNextDispatch {
		s.failNextDispatch = false
		return task.Operation{}, domain.NewError(domain.CodeInvalidState, "simulated dispatch fence failure")
	}
	if update.Status == task.OperationSucceeded && s.failNextSucceededUpdate {
		s.failNextSucceededUpdate = false
		return task.Operation{}, domain.NewError(domain.CodeInvalidState, "simulated result persistence failure")
	}
	if update.Status == task.OperationDispatched {
		operation.RuntimeGeneration = update.RuntimeGeneration
		operation.RuntimeInstanceID = update.RuntimeInstanceID
		operation.ControlVersion = update.ControlVersion
		operation.RuntimeLeaseVersion = update.RuntimeLeaseVersion
	}
	operation.Status = update.Status
	operation.ExternalID = update.ExternalID
	operation.Result = update.Result
	s.operation = operation
	if s.operations == nil {
		s.operations = make(map[string]task.Operation)
	}
	s.operations[operation.Key] = operation
	s.operationTransitions = append(s.operationTransitions, update.Status)
	return operation, nil
}

type runtimeToolAuthorizer struct {
	errors map[identity.Scope]error
}

func (a *runtimeToolAuthorizer) AuthorizeTaskAction(_ context.Context, _ string, _ identity.Kind, _ string, _ string, _ []identity.Scope, scope identity.Scope) error {
	return a.errors[scope]
}

type runtimeToolFenceReader struct{ fence AgentHostFence }

func (r runtimeToolFenceReader) ReadTaskFence(context.Context, string, string) (AgentHostFence, error) {
	return r.fence, nil
}

func newRuntimeToolExecutorFixture(t *testing.T, specs []runtimeToolHandlerSpec) *runtimeToolExecutorFixture {
	t.Helper()
	fixture := &runtimeToolExecutorFixture{handlers: make(map[string]*runtimeToolTestHandler)}

	expires := time.Now().Add(time.Minute).UTC()
	inputRevision := int64(0)
	value := task.Task{
		ID: "q1234567", OwnerID: "u1234567", HostID: "h1234567", ModelID: "p1234567", WorkspaceID: "w1234567",
		InputRevision: &inputRevision, ContextDigest: make([]byte, 32),
		Status: task.StatusRunning, Authorization: task.Authorization{Kind: string(identity.KindSession)},
		Budget: task.Budget{ActiveSeconds: 1800, ToolCalls: 100, ModelTokens: 50000},
		Lease:  task.Lease{Owner: "worker-1", Version: 1, ExpiresAt: &expires},
	}
	fixture.store = &runtimeToolTaskStore{details: TaskDetails{Task: value}, operations: make(map[string]task.Operation)}
	fixture.authorizer = &runtimeToolAuthorizer{errors: make(map[identity.Scope]error)}
	fixture.fence = AgentHostFence{RuntimeGeneration: 1, ControlVersion: 0, AIAllowed: true}
	fixture.fences = &runtimeToolFenceReader{fence: fixture.fence}

	credential := testRuntimeCredential(0x3d)
	storeDir, err := os.MkdirTemp("", "morde-runtime-tool-")
	if err != nil {
		t.Fatal(err)
	}
	storeDir, err = filepath.EvalSymlinks(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(storeDir) })

	registered := make([]runtimecore.ActionHandler, 0, len(specs))
	for index, spec := range specs {
		if spec.runtimeAction != nil {
			if spec.runtimeAction.Capability() == "" || spec.runtimeAction.Action() == "" {
				t.Fatalf("runtime handler spec %d lacks capability/action", index)
			}
			registered = append(registered, spec.runtimeAction)
			continue
		}
		handler := spec.handler
		if handler == nil {
			handler = &runtimeToolTestHandler{capability: spec.capability, action: spec.action, result: spec.result, delay: spec.delay}
		}
		if handler.capability == "" || handler.action == "" {
			t.Fatalf("handler spec %d lacks capability/action", index)
		}
		fixture.handlers[handler.capability] = handler
		registered = append(registered, handler)
	}
	manager, err := runtimecore.Open(runtimecore.Options{
		StoreDir: storeDir, ExpectedOwnerUID: uint32(os.Geteuid()), HostID: value.HostID, Generation: 1,
		ProtocolVersion: runtimeclient.ProtocolVersion, Handlers: registered,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.manager = manager
	t.Cleanup(func() {
		fixture.server.Close()
		if err := fixture.manager.Close(); err != nil && !errors.Is(err, runtimecore.ErrOperationsPending) {
			t.Errorf("close runtime manager: %v", err)
		}
	})

	baseHandler, err := runtimehttp.NewHandler(manager, credential)
	if err != nil {
		t.Fatal(err)
	}
	fixture.wrap = func(next http.Handler) http.Handler { return next }
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fixture.wrap(baseHandler).ServeHTTP(writer, request)
	}))
	serverTLS, err := runtimeidentity.ServerTLSConfig(credential, value.HostID)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.TLS = serverTLS
	server.StartTLS()
	fixture.server = server

	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealer.Seal([]byte(credential))
	if err != nil {
		t.Fatal(err)
	}
	hostValue := hostServiceTestHost(host.StateRunning)
	hostValue.ControlState = host.ControlAI
	hostValue.RuntimeCredentialEnc = sealed
	hostValue.Workspaces = []workspace.Binding{testWorkspaceBinding(hostValue.ID, hostValue.OwnerID, "w1234567", workspace.BindingActive)}
	hosts := &hostServiceTestHostStore{values: make(map[string]host.Host)}
	hosts.addHost(hostValue)
	sandbox := &runtimeControlTestSandbox{hostServiceTestSandbox: &hostServiceTestSandbox{values: make(map[string]Sandbox)}, endpoint: server.URL}
	runtime, err := NewRuntimeControl(hosts, &hostServiceTestRouter{client: sandbox}, sealer)
	if err != nil {
		t.Fatal(err)
	}
	workspaceService, manifestStore, workspaceBlocks, _ := proofFixture(t)
	const helloSHA256 = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	manifestStore.snapshots[4] = []workspace.File{{
		Path: "notes/hello.txt", SizeBytes: 5, SHA256: helloSHA256, Mode: 0o644,
		Chunks: []workspace.Chunk{{SHA256: helloSHA256, SizeBytes: 5}},
	}}
	workspaceBlocks.data[helloSHA256] = []byte("hello")
	fixture.workspaceBlocks = workspaceBlocks
	fixture.executor, err = NewRuntimeToolExecutor(fixture.store, fixture.authorizer, fixture.fences, runtime, workspaceService)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func setRuntimeToolWorkspaceRevision(t *testing.T, fixture *runtimeToolExecutorFixture, revision int64) {
	t.Helper()
	hosts := fixture.executor.runtime.hosts.(*hostServiceTestHostStore)
	value, err := hosts.GetHost(context.Background(), fixture.store.details.Task.HostID)
	if err != nil {
		t.Fatal(err)
	}
	value.Workspaces[0].DesiredRevision = revision
	value.Workspaces[0].AppliedRevision = revision
	value.Workspaces[0].Workspace.Revision = revision
	value.Workspaces[0].SyncState = workspace.BindingSyncHealthy
	hosts.addHost(value)
}

func (f *runtimeToolExecutorFixture) request(name string, arguments string, scope identity.Scope) AgentToolRequest {
	workspaceID := ""
	if mapping, ok := runtimeToolMappingByName(name); ok && runtimeclient.WorkspaceScopedCapability(mapping.capability) {
		workspaceID = "w1234567"
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(arguments), &fields); err == nil {
			if _, exists := fields["workspace_id"]; !exists {
				fields["workspace_id"], _ = json.Marshal(workspaceID)
				if encoded, err := json.Marshal(fields); err == nil {
					arguments = string(encoded)
				}
			}
		}
	}
	return AgentToolRequest{
		Task:      f.store.details.Task,
		Call:      ModelToolCall{CallID: "call-runtime-tool", Name: name, Arguments: arguments},
		Arguments: json.RawMessage(arguments), OperationKey: "agent:runtime-test-call", RequiredScope: scope, Fence: f.fence,
		WorkspaceID: workspaceID,
	}
}

func (f *runtimeToolExecutorFixture) prepared(name, arguments string, scope identity.Scope) (AgentToolRequest, AgentToolPreparation) {
	request := f.request(name, arguments, scope)
	preparation, err := f.executor.Prepare(context.Background(), request)
	if err != nil {
		panic(fmt.Sprintf("Prepare(%s) failed: %v", name, err))
	}
	if preparation.Status == AgentToolApprovalRequired {
		hash, hashErr := agentOperationHash(request.Call.Name, request.Arguments)
		if hashErr != nil {
			panic(hashErr)
		}
		pending, pendingErr := newPendingApproval(request.Task, request.Call, hash[:], preparation.Approval, preparation.Fence, 1)
		if pendingErr != nil {
			panic(pendingErr)
		}
		pending.Status, pending.Decision = task.PendingRequestApproved, task.PendingDecisionApprove
		f.store.details.PendingRequest = pending
		f.fences.fence = preparation.Fence
		request.Fence = preparation.Fence
		request.Approval = &AgentToolApprovalDecision{PendingID: pending.ID, Version: pending.Version,
			Decision: task.PendingDecisionApprove, RequestHash: append([]byte(nil), hash[:]...)}
		preparation, err = f.executor.Prepare(context.Background(), request)
		if err != nil {
			panic(fmt.Sprintf("Prepare(%s) with approved action failed: %v", name, err))
		}
	}
	request.Fence = preparation.Fence
	request.WorkspaceID = preparation.WorkspaceID
	f.fence = preparation.Fence
	f.fences.fence = preparation.Fence
	request.OperationID = "o1234567"
	return request, preparation
}

func (f *runtimeToolExecutorFixture) persistOperation(request AgentToolRequest) {
	hash, err := agentOperationHash(request.Call.Name, request.Arguments)
	if err != nil {
		panic(err)
	}
	bindingVersion := int64(0)
	if request.WorkspaceID != "" {
		bindingVersion = 1
	}
	f.store.operation = task.Operation{
		ID: request.OperationID, TaskID: request.Task.ID, Key: request.OperationKey,
		RequestHash: hash[:], HostID: request.Task.HostID, WorkspaceID: request.WorkspaceID, BindingVersion: bindingVersion,
		RuntimeGeneration: request.Fence.RuntimeGeneration, RuntimeInstanceID: request.Fence.RuntimeInstanceID,
		ControlVersion: request.Fence.ControlVersion, LeaseVersion: request.Task.Lease.Version,
		RuntimeLeaseVersion: request.Task.Lease.Version, Status: task.OperationDispatched,
	}
	if f.store.operations == nil {
		f.store.operations = make(map[string]task.Operation)
	}
	f.store.operations[f.store.operation.Key] = f.store.operation
}

func writeRuntimeToolError(writer http.ResponseWriter, status int, code string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]string{"code": code})
}
