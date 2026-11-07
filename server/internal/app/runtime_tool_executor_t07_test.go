package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"morde-runtime/contract"
	"morde-runtime/runtimecore"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/runtimeclient"
)

type t07RevocableAuthorizer struct{ revoked atomic.Bool }

func (a *t07RevocableAuthorizer) AuthorizeTaskAction(context.Context, string, identity.Kind, string, string, []identity.Scope, identity.Scope) error {
	if a.revoked.Load() {
		return domain.NewError(domain.CodeForbidden, "computer authorization revoked")
	}
	return nil
}

type t07MutableFence struct {
	mu    sync.RWMutex
	fence AgentHostFence
}

func (f *t07MutableFence) ReadTaskFence(context.Context, string, string) (AgentHostFence, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.fence, nil
}

type t07BlockingBrowserHandler struct {
	started  chan struct{}
	finished chan struct{}
}

type t07RevokingBrowserHandler struct{ authorizer *t07RevocableAuthorizer }

type t07DesktopClickCaptureHandler struct {
	mu      sync.Mutex
	request contract.ExecuteRequest
}

func (*t07DesktopClickCaptureHandler) Capability() string { return "desktop.input" }
func (*t07DesktopClickCaptureHandler) Action() string     { return "click" }
func (h *t07DesktopClickCaptureHandler) Execute(_ context.Context, request contract.ExecuteRequest) (json.RawMessage, error) {
	h.mu.Lock()
	h.request = request
	h.mu.Unlock()
	var args struct {
		WindowID         string `json:"window_id"`
		X                int    `json:"x"`
		Y                int    `json:"y"`
		ScreenshotWidth  int    `json:"screenshot_width"`
		ScreenshotHeight int    `json:"screenshot_height"`
		ScreenWidth      int    `json:"screen_width"`
		ScreenHeight     int    `json:"screen_height"`
		Button           string `json:"button"`
	}
	if err := json.Unmarshal(request.Arguments, &args); err != nil {
		return nil, err
	}
	if args.Button == "" {
		args.Button = "left"
	}
	return json.Marshal(map[string]any{
		"window_id": args.WindowID,
		"x":         screenshotPointToScreen(args.X, args.ScreenshotWidth, args.ScreenWidth),
		"y":         screenshotPointToScreen(args.Y, args.ScreenshotHeight, args.ScreenHeight),
		"button":    args.Button,
		"clicked":   true,
	})
}
func (*t07DesktopClickCaptureHandler) Stop(context.Context, contract.Operation) (runtimecore.StopResult, error) {
	return runtimecore.StopResult{VerifiedStopped: true}, nil
}

func (*t07RevokingBrowserHandler) Capability() string { return "browser.actions" }
func (*t07RevokingBrowserHandler) Action() string     { return "open_tab" }
func (h *t07RevokingBrowserHandler) Execute(context.Context, contract.ExecuteRequest) (json.RawMessage, error) {
	h.authorizer.revoked.Store(true)
	return json.RawMessage(`{"target_id":"target_1","url":"https://example.test/","title":"local"}`), nil
}
func (*t07RevokingBrowserHandler) Stop(context.Context, contract.Operation) (runtimecore.StopResult, error) {
	return runtimecore.StopResult{VerifiedStopped: true}, nil
}

func (*t07BlockingBrowserHandler) Capability() string { return "browser.actions" }
func (*t07BlockingBrowserHandler) Action() string     { return "open_tab" }
func (h *t07BlockingBrowserHandler) Execute(ctx context.Context, _ contract.ExecuteRequest) (json.RawMessage, error) {
	close(h.started)
	<-ctx.Done()
	close(h.finished)
	return nil, ctx.Err()
}
func (*t07BlockingBrowserHandler) Stop(context.Context, contract.Operation) (runtimecore.StopResult, error) {
	return runtimecore.StopResult{VerifiedStopped: true}, nil
}

func TestT07RunningHostActionStopsOnRevocationFenceAndCancellation(t *testing.T) {
	for _, reason := range []string{"authorization revoked", "host control changed", "task stopping", "context canceled"} {
		t.Run(reason, func(t *testing.T) {
			handler := &t07BlockingBrowserHandler{started: make(chan struct{}), finished: make(chan struct{})}
			fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{runtimeAction: handler}})
			fixture.store.details.Task.WorkspaceID = ""
			fixture.store.details.Task.InputRevision = nil
			fixture.store.details.Task.WorkspaceCheckpointRevision = nil
			authorizer := &t07RevocableAuthorizer{}
			fixture.executor.authorizer = authorizer
			fixture.executor.operations.pollEvery = time.Millisecond
			fixture.executor.leaseEvery = 10 * time.Millisecond
			request, preparation := fixture.prepared(runtimeToolBrowserOpenTab, `{"url":"https://example.test/"}`, identity.ScopeComputerOperate)
			fixture.persistOperation(request)
			fences := &t07MutableFence{fence: request.Fence}
			fixture.executor.fences = fences
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type outcome struct {
				result AgentToolResult
				err    error
			}
			resultCh := make(chan outcome, 1)
			go func() {
				result, err := fixture.executor.Execute(ctx, request, preparation)
				resultCh <- outcome{result: result, err: err}
			}()
			select {
			case <-handler.started:
			case <-time.After(3 * time.Second):
				t.Fatal("host action did not start")
			}
			switch reason {
			case "authorization revoked":
				authorizer.revoked.Store(true)
			case "host control changed":
				fences.mu.Lock()
				fences.fence.ControlVersion++
				fences.mu.Unlock()
			case "task stopping":
				fixture.store.mu.Lock()
				fixture.store.details.Task.Status = task.StatusCancelling
				fixture.store.mu.Unlock()
			case "context canceled":
				cancel()
			}
			select {
			case got := <-resultCh:
				if got.err != nil || got.result.Status != AgentToolUnknown {
					t.Fatalf("action after %s = %#v, %v; want unknown", reason, got.result, got.err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("host action did not return after cancellation")
			}
			select {
			case <-handler.finished:
			case <-time.After(3 * time.Second):
				t.Fatal("host runtime did not receive cancellation")
			}
		})
	}
}

func TestT07CompletedHostActionIsNotReportedAfterRevocation(t *testing.T) {
	authorizer := &t07RevocableAuthorizer{}
	handler := &t07RevokingBrowserHandler{authorizer: authorizer}
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{runtimeAction: handler}})
	fixture.store.details.Task.WorkspaceID = ""
	fixture.store.details.Task.InputRevision = nil
	fixture.store.details.Task.WorkspaceCheckpointRevision = nil
	fixture.executor.authorizer = authorizer
	request, preparation := fixture.prepared(runtimeToolBrowserOpenTab, `{"url":"https://example.test/"}`, identity.ScopeComputerOperate)
	fixture.persistOperation(request)
	result, err := fixture.executor.Execute(context.Background(), request, preparation)
	if err != nil || result.Status != AgentToolUnknown || result.Output != "" {
		t.Fatalf("result after authorization revocation = %#v, %v; want unknown without output", result, err)
	}
}

func TestT07ScreenshotValidationFailsBeforeReportingSuccess(t *testing.T) {
	makeJPEG := func(width, height int) []byte {
		t.Helper()
		var out bytes.Buffer
		if err := jpeg.Encode(&out, image.NewRGBA(image.Rect(0, 0, width, height)), nil); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	valid := makeJPEG(8, 4)
	truncated := append(append([]byte(nil), valid[:len(valid)/2]...), 0xff, 0xd9)
	tests := []struct {
		name       string
		tool       string
		imageBytes []byte
		width      int
		height     int
		screenW    int
		screenH    int
	}{
		{name: "browser malformed jpeg", tool: runtimeToolBrowserScreenshot, imageBytes: []byte{0xff, 0xd8, 0xff, 0xd9}},
		{name: "browser truncated jpeg", tool: runtimeToolBrowserScreenshot, imageBytes: truncated},
		{name: "browser dimensions too large", tool: runtimeToolBrowserScreenshot, imageBytes: makeJPEG(1281, 1)},
		{name: "desktop missing original size", tool: runtimeToolDesktopScreenshot, imageBytes: valid, width: 8, height: 4},
		{name: "desktop invalid original size", tool: runtimeToolDesktopScreenshot, imageBytes: valid, width: 8, height: 4, screenW: 16385, screenH: 8192},
		{name: "desktop aspect mismatch", tool: runtimeToolDesktopScreenshot, imageBytes: valid, width: 8, height: 4, screenW: 1000, screenH: 1000},
		{name: "desktop truncated jpeg", tool: runtimeToolDesktopScreenshot, imageBytes: truncated, width: 8, height: 4, screenW: 1600, screenH: 800},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var value map[string]any
			arguments := json.RawMessage(`{}`)
			if test.tool == runtimeToolBrowserScreenshot {
				arguments = json.RawMessage(`{"target_id":"target_1"}`)
				value = map[string]any{"target_id": "target_1", "mime_type": "image/jpeg", "data_base64": base64.StdEncoding.EncodeToString(test.imageBytes)}
			} else {
				value = map[string]any{"mime_type": "image/jpeg", "width": test.width, "height": test.height,
					"screen_width": test.screenW, "screen_height": test.screenH, "data_base64": base64.StdEncoding.EncodeToString(test.imageBytes)}
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			mapping, _ := runtimeToolMappingByName(test.tool)
			result, err := mapRuntimeToolResult(mapping, runtimeclient.Operation{OperationID: "o1234567", Status: runtimeclient.OperationSucceeded, Result: raw}, arguments)
			if err != nil || result.Status != AgentToolFailed || len(result.Images) != 0 {
				t.Fatalf("unsafe screenshot result = %#v, %v; want failure without image", result, err)
			}
		})
	}
}

func TestT07BrowserTypingRequiresExactCharacterCount(t *testing.T) {
	mapping, _ := runtimeToolMappingByName(runtimeToolBrowserType)
	arguments := json.RawMessage(`{"target_id":"target_1","role":"textbox","name":"search","text":"你好"}`)
	for _, test := range []struct {
		count int
		want  AgentToolResultStatus
	}{{count: 1, want: AgentToolFailed}, {count: 2, want: AgentToolSucceeded}} {
		raw, err := json.Marshal(map[string]any{"target_id": "target_1", "typed_chars": test.count})
		if err != nil {
			t.Fatal(err)
		}
		result, err := mapRuntimeToolResult(mapping, runtimeclient.Operation{OperationID: "o1234567", Status: runtimeclient.OperationSucceeded, Result: raw}, arguments)
		if err != nil || result.Status != test.want {
			t.Fatalf("typed count %d = %#v, %v; want %s", test.count, result, err, test.want)
		}
	}
}

func TestT07DesktopClickRequiresSharedScreenSizeAndReportsChanges(t *testing.T) {
	var schema struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Maximum int `json:"maximum"`
		} `json:"properties"`
	}
	for _, definition := range RuntimeComputerTools() {
		if definition.Name == runtimeToolDesktopClick {
			if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	joined := "," + strings.Join(schema.Required, ",") + ","
	for _, required := range []string{"screen_width", "screen_height", "screenshot_width", "screenshot_height"} {
		if !strings.Contains(joined, ","+required+",") {
			t.Errorf("MCP and Agent shared click schema lacks %s: %v", required, schema.Required)
		}
	}
	if schema.Properties["x"].Maximum != maxDesktopScreenshotWidth-1 || schema.Properties["y"].Maximum != maxDesktopScreenshotHeight-1 {
		t.Fatalf("click schema does not bound x/y to screenshot pixels: %+v", schema.Properties)
	}
	mapping, _ := runtimeToolMappingByName(runtimeToolDesktopClick)
	for _, test := range []struct {
		arguments string
		valid     bool
	}{
		{arguments: `{"window_id":"0x1","x":5,"y":5}`},
		{arguments: `{"window_id":"0x1","x":5,"y":5,"screen_width":20,"screen_height":20}`},
		{arguments: `{"window_id":"0x1","x":5,"y":5,"screenshot_width":10,"screenshot_height":10,"screen_width":0,"screen_height":20}`},
		{arguments: `{"window_id":"0x1","x":5,"y":5,"screenshot_width":10,"screenshot_height":10,"screen_width":20,"screen_height":40}`},
		{arguments: `{"window_id":"0x1","x":10,"y":5,"screenshot_width":10,"screenshot_height":10,"screen_width":20,"screen_height":20}`},
		{arguments: `{"window_id":"0x1","x":5,"y":10,"screenshot_width":10,"screenshot_height":10,"screen_width":20,"screen_height":20}`},
		{arguments: `{"window_id":"0x1","x":5,"y":5,"screenshot_width":10,"screenshot_height":10,"screen_width":20,"screen_height":20}`, valid: true},
	} {
		if valid := validRuntimeToolArguments(mapping, json.RawMessage(test.arguments)); valid != test.valid {
			t.Errorf("click argument validity = %t, want %t: %s", valid, test.valid, test.arguments)
		}
	}
	arguments := json.RawMessage(`{"window_id":"0x1","x":5,"y":5,"screenshot_width":10,"screenshot_height":10,"screen_width":20,"screen_height":20}`)
	for _, test := range []struct {
		result string
		want   string
	}{
		{result: `{"code":"desktop_geometry_changed"}`, want: "desktop_geometry_changed"},
		{result: `{"code":"private_runtime_error"}`, want: "runtime_operation_failed"},
	} {
		result, err := mapRuntimeToolResult(mapping, runtimeclient.Operation{OperationID: "o1234567", Status: runtimeclient.OperationFailed, Result: json.RawMessage(test.result)}, arguments)
		if err != nil || result.Status != AgentToolFailed || !strings.Contains(result.Output, test.want) || strings.Contains(result.Output, "private_runtime_error") {
			t.Fatalf("failed click = %#v, %v; want safe code %s", result, err, test.want)
		}
	}
}

func TestT07DesktopClickForwardsScreenshotGeometryAndMapsResult(t *testing.T) {
	handler := &t07DesktopClickCaptureHandler{}
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{runtimeAction: handler}})
	fixture.store.details.Task.WorkspaceID = ""
	fixture.store.details.Task.InputRevision = nil
	fixture.store.details.Task.WorkspaceCheckpointRevision = nil
	request, preparation := fixture.prepared(runtimeToolDesktopClick,
		`{"window_id":"0x1","x":200,"y":100,"screenshot_width":400,"screenshot_height":200,"screen_width":800,"screen_height":400}`,
		identity.ScopeComputerOperate)
	fixture.persistOperation(request)

	result, err := fixture.executor.Execute(context.Background(), request, preparation)
	if err != nil || result.Status != AgentToolSucceeded {
		t.Fatalf("desktop click result = %#v, %v; want success", result, err)
	}
	handler.mu.Lock()
	forwarded := append(json.RawMessage(nil), handler.request.Arguments...)
	handler.mu.Unlock()
	var forwardedArgs map[string]json.RawMessage
	if json.Unmarshal(forwarded, &forwardedArgs) != nil {
		t.Fatalf("runtime arguments are not JSON: %s", forwarded)
	}
	for name, want := range map[string]int{"x": 200, "y": 100, "screenshot_width": 400, "screenshot_height": 200, "screen_width": 800, "screen_height": 400} {
		var got int
		if json.Unmarshal(forwardedArgs[name], &got) != nil || got != want {
			t.Errorf("runtime argument %s = %d, want %d; args=%s", name, got, want, forwarded)
		}
	}
	var output struct {
		X int `json:"x"`
		Y int `json:"y"`
	}
	if json.Unmarshal([]byte(result.Output), &output) != nil || output.X != 400 || output.Y != 200 {
		t.Fatalf("mapped screen result = %s; want (400, 200)", result.Output)
	}
}
