package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"strings"
	"testing"

	"morde-runtime/runtimecore"

	"morde/server/internal/domain/task"
)

func TestT07BrokerWorksThroughBrowserAndPrivateWorkspace(t *testing.T) {
	var screenshot bytes.Buffer
	if err := jpeg.Encode(&screenshot, image.NewRGBA(image.Rect(0, 0, 8, 4)), nil); err != nil {
		t.Fatal(err)
	}
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{
		{capability: "desktop.observe", action: "screenshot", result: `{"mime_type":"image/jpeg","width":8,"height":4,"screen_width":1600,"screen_height":800,"data_base64":"` + base64.StdEncoding.EncodeToString(screenshot.Bytes()) + `"}`},
		{capability: "browser.actions", action: "open_tab", result: `{"target_id":"target_1","url":"https://example.test/","title":"Local test"}`},
		{capability: "browser.observe", action: "snapshot", result: `{"target_id":"target_1","url":"https://example.test/","title":"Local test","text":"synthetic page"}`},
		{taskWorkspaceAction: "write"}, {taskWorkspaceAction: "read"},
	})
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	t07SyncRuntimeFence(t, fixture, store.details.Task)
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		call ModelToolCall
		want string
	}{
		{call: ModelToolCall{CallID: "t07-screenshot", Name: runtimeToolDesktopScreenshot, Arguments: `{}`}, want: `"screen_width":1600`},
		{call: ModelToolCall{CallID: "t07-open", Name: runtimeToolBrowserOpenTab, Arguments: `{"url":"https://example.test/"}`}, want: `"target_id":"target_1"`},
		{call: ModelToolCall{CallID: "t07-page", Name: runtimeToolBrowserSnapshot, Arguments: `{"target_id":"target_1"}`}, want: "synthetic page"},
		{call: ModelToolCall{CallID: "t07-write", Name: runtimeToolFileWrite, Arguments: `{"workspace_id":"w1234567","path":"note.txt","content":"synthetic workspace text"}`}, want: "note.txt"},
		{call: ModelToolCall{CallID: "t07-read", Name: runtimeToolFileRead, Arguments: `{"workspace_id":"w1234567","path":"note.txt"}`}, want: "synthetic workspace text"},
	}
	for _, step := range steps {
		result, err := broker.Execute(context.Background(), store.details.Task, step.call)
		if err != nil || result.Status != AgentToolSucceeded || !strings.Contains(result.Output, step.want) {
			t.Fatalf("%s = %#v, %v; want successful output containing %q", step.call.Name, result, err, step.want)
		}
		if step.call.Name == runtimeToolDesktopScreenshot && (len(result.Images) != 1 || !bytes.Equal(result.Images[0].Data, screenshot.Bytes())) {
			t.Fatal("screenshot was not passed to the model")
		}
	}
	if len(store.details.Steps) != len(steps) {
		t.Fatalf("work chain has %d steps, want %d", len(store.details.Steps), len(steps))
	}
	for _, step := range store.details.Steps {
		if step.Status != task.StepSucceeded {
			t.Fatalf("work chain reports an unsuccessful step: %#v", step)
		}
	}
}

func TestT07BrokerReturnsGeometryChangeAsFailedWithoutClaimingClick(t *testing.T) {
	handler := &runtimeToolTestHandler{capability: "desktop.input", action: "click", err: runtimecore.ErrDesktopGeometryChanged}
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{handler: handler}})
	fixture.store.details.Task.WorkspaceID = ""
	fixture.store.details.Task.InputRevision = nil
	fixture.store.details.Task.WorkspaceCheckpointRevision = nil
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	t07SyncRuntimeFence(t, fixture, store.details.Task)
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := ModelToolCall{CallID: "t07-size-changed", Name: runtimeToolDesktopClick,
		Arguments: `{"window_id":"0x1","x":4,"y":2,"screenshot_width":8,"screenshot_height":4,"screen_width":1600,"screen_height":800}`}
	waiting, err := broker.Execute(context.Background(), store.details.Task, call)
	if err != nil || waiting.Status != AgentToolNeedsUser || handler.calls.Load() != 0 {
		t.Fatalf("approval = %#v, %v; calls=%d", waiting, err, handler.calls.Load())
	}
	resumeCloudDSHApproval(fixture, store, task.PendingRequestApproved, task.PendingDecisionApprove)
	result, err := broker.Reconcile(context.Background(), store.details.Task, call, nil)
	if err != nil || result.Status != AgentToolFailed || !strings.Contains(result.Output, "desktop_geometry_changed") || handler.calls.Load() != 1 {
		t.Fatalf("resized display result = %#v, %v; calls=%d", result, err, handler.calls.Load())
	}
	operation, err := store.GetOperation(context.Background(), store.details.Task.OwnerID, store.details.Task.ID, agentOperationKey(call.CallID))
	if err != nil || operation.Status != task.OperationFailed {
		t.Fatalf("resized display operation = %#v, %v; want failed", operation, err)
	}
}

func TestT07ScaledScreenshotAndDesktopClickApproval(t *testing.T) {
	for _, approve := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected", true: "approved"}[approve], func(t *testing.T) {
			var screenshot bytes.Buffer
			if err := jpeg.Encode(&screenshot, image.NewRGBA(image.Rect(0, 0, 8, 4)), nil); err != nil {
				t.Fatal(err)
			}
			fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{
				{capability: "desktop.observe", action: "screenshot", result: `{"mime_type":"image/jpeg","width":8,"height":4,"screen_width":1600,"screen_height":800,"data_base64":"` + base64.StdEncoding.EncodeToString(screenshot.Bytes()) + `"}`},
				{capability: "desktop.input", action: "click", result: `{"window_id":"0x1","x":800,"y":400,"button":"left","clicked":true}`},
			})
			fixture.store.details.Task.WorkspaceID = ""
			fixture.store.details.Task.InputRevision = nil
			fixture.store.details.Task.WorkspaceCheckpointRevision = nil
			store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
			fixture.executor.store = store
			t07SyncRuntimeFence(t, fixture, store.details.Task)
			broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			imageResult, err := broker.Execute(context.Background(), store.details.Task, ModelToolCall{CallID: "t07-screen", Name: runtimeToolDesktopScreenshot, Arguments: `{}`})
			if err != nil || imageResult.Status != AgentToolSucceeded {
				t.Fatalf("screenshot = %#v, %v", imageResult, err)
			}
			var geometry struct {
				Width        int `json:"width"`
				Height       int `json:"height"`
				ScreenWidth  int `json:"screen_width"`
				ScreenHeight int `json:"screen_height"`
			}
			if err := json.Unmarshal([]byte(imageResult.Output), &geometry); err != nil || geometry.Width != 8 || geometry.Height != 4 ||
				geometry.ScreenWidth != 1600 || geometry.ScreenHeight != 800 {
				t.Fatalf("screenshot geometry = %#v, %v", geometry, err)
			}
			x, y := 4, 2
			arguments, _ := json.Marshal(map[string]any{"window_id": "0x1", "x": x, "y": y,
				"screenshot_width": geometry.Width, "screenshot_height": geometry.Height,
				"screen_width": geometry.ScreenWidth, "screen_height": geometry.ScreenHeight})
			call := ModelToolCall{CallID: "t07-click", Name: runtimeToolDesktopClick, Arguments: string(arguments)}
			waiting, err := broker.Execute(context.Background(), store.details.Task, call)
			if err != nil || waiting.Status != AgentToolNeedsUser || fixture.handlers["desktop.input"].calls.Load() != 0 {
				t.Fatalf("desktop click approval = %#v, %v", waiting, err)
			}
			if pending := store.details.PendingRequest; pending == nil || !strings.Contains(pending.Content, "(4, 2)") ||
				!strings.Contains(pending.Content, "screen point 800, 400") || !strings.Contains(pending.Content, "8 × 4 image") ||
				!strings.Contains(pending.Content, "0x1") || !strings.Contains(pending.Content, "left button") {
				t.Fatalf("desktop click approval is missing target or geometry: %#v", pending)
			}
			status, decision, want, calls := task.PendingRequestRejected, task.PendingDecisionReject, AgentToolFailed, int32(0)
			if approve {
				status, decision, want, calls = task.PendingRequestApproved, task.PendingDecisionApprove, AgentToolSucceeded, 1
			}
			resumeCloudDSHApproval(fixture, store, status, decision)
			result, err := broker.Reconcile(context.Background(), store.details.Task, call, nil)
			if err != nil || result.Status != want || fixture.handlers["desktop.input"].calls.Load() != calls {
				t.Fatalf("desktop click after approval = %#v, %v; dispatches=%d", result, err, fixture.handlers["desktop.input"].calls.Load())
			}
		})
	}
}

func t07SyncRuntimeFence(t *testing.T, fixture *runtimeToolExecutorFixture, current task.Task) {
	t.Helper()
	session, err := fixture.executor.connect(context.Background(), current, fixture.fence)
	if err != nil {
		t.Fatal(err)
	}
	fixture.fences.fence = agentFenceFromRuntime(session.Fence)
	session.Close()
}
