package runtime_desktop

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"sync"
	"testing"
	"time"

	"morde-runtime/contract"
	"morde-runtime/runtimecore"
)

type fakeBackend struct {
	mu                   sync.Mutex
	size                 displaySize
	image                []byte
	windows              []window
	windowsAfterActivate *[]window
	activated            string
	lastClick            [4]any
	lastTarget           string
	text                 string
	key                  string
	probeErr             error
	blockClick           bool
	clickCalled          chan struct{}
	sizeAfterScreenshot  *displaySize
	displayCalls         int
	changeDisplayAt      int
	sizeAfterDisplay     displaySize
	focusChangeAtDisplay int
}

func (b *fakeBackend) probe(context.Context) (displaySize, error) {
	if b.probeErr != nil {
		return displaySize{}, b.probeErr
	}
	return b.size, nil
}
func (b *fakeBackend) display(context.Context) (displaySize, error) {
	b.displayCalls++
	if b.displayCalls == b.changeDisplayAt {
		b.size = b.sizeAfterDisplay
	}
	if b.displayCalls == b.focusChangeAtDisplay && len(b.windows) > 0 {
		b.windows[0].Active = false
	}
	return b.size, nil
}
func (b *fakeBackend) screenshot(context.Context) ([]byte, error) {
	if b.sizeAfterScreenshot != nil {
		b.size = *b.sizeAfterScreenshot
	}
	return b.image, nil
}
func (b *fakeBackend) listWindows(context.Context) ([]window, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]window(nil), b.windows...), nil
}
func (b *fakeBackend) activate(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.activated = id
	if b.windowsAfterActivate != nil {
		b.windows = append([]window(nil), (*b.windowsAfterActivate)...)
		return nil
	}
	for index := range b.windows {
		b.windows[index].Active = b.windows[index].ID == id
	}
	return nil
}
func (b *fakeBackend) click(ctx context.Context, id string, x, y int, button string) error {
	b.mu.Lock()
	b.lastClick = [4]any{id, x, y, button}
	called := b.clickCalled
	block := b.blockClick
	b.mu.Unlock()
	if called != nil {
		select {
		case called <- struct{}{}:
		default:
		}
	}
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
func (b *fakeBackend) typeText(_ context.Context, id, text string) error {
	b.lastTarget, b.text = id, text
	return nil
}
func (b *fakeBackend) pressKey(_ context.Context, id, key string) error {
	b.lastTarget, b.key = id, key
	return nil
}

func TestHandlersExposeFrozenDesktopActions(t *testing.T) {
	controller := newTestController(t, &fakeBackend{size: displaySize{width: 100, height: 50}})
	defer controller.Close()
	want := [][2]string{
		{"desktop.observe", "screenshot"}, {"window.list", "list"}, {"window.activate", "activate"},
		{"desktop.input", "click"}, {"desktop.input", "type"}, {"desktop.input", "press_key"},
	}
	if len(controller.Handlers()) != len(want) {
		t.Fatalf("handler count = %d, want %d", len(controller.Handlers()), len(want))
	}
	for i, handler := range controller.Handlers() {
		if handler.Capability() != want[i][0] || handler.Action() != want[i][1] {
			t.Errorf("handler %d = %s/%s, want %s/%s", i, handler.Capability(), handler.Action(), want[i][0], want[i][1])
		}
	}
}

func TestScreenshotIsBoundedJPEGAndReportsHeaderDimensions(t *testing.T) {
	imageBytes := testJPEG(t, 16, 9)
	backend := &fakeBackend{size: displaySize{width: 1600, height: 900}, image: imageBytes}
	controller := newTestController(t, backend)
	defer controller.Close()
	result, err := executeDesktop(t, controller, "desktop.observe", "screenshot", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		MIMEType        string `json:"mime_type"`
		Width           int    `json:"width"`
		Height          int    `json:"height"`
		ScreenWidth     int    `json:"screen_width"`
		ScreenHeight    int    `json:"screen_height"`
		ScreenshotToken string `json:"screenshot_token"`
		DataBase64      string `json:"data_base64"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(response.DataBase64)
	if err != nil {
		t.Fatal(err)
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(decoded))
	if err != nil || response.MIMEType != "image/jpeg" || response.Width != config.Width || response.Height != config.Height ||
		response.Width != 16 || response.Height != 9 || response.ScreenWidth != 1600 || response.ScreenHeight != 900 || !validScreenshotToken(response.ScreenshotToken) {
		t.Fatalf("screenshot metadata = %#v, header = %#v, error = %v", response, config, err)
	}
	if !bytes.Equal(decoded, imageBytes) || len(decoded) > maxScreenshotBytes || len(result) > maxScreenshotJSONBytes {
		t.Fatalf("screenshot output is not the bounded JPEG: bytes=%d JSON=%d", len(decoded), len(result))
	}

	backend.image = bytes.Repeat([]byte{0xff}, maxScreenshotBytes+1)
	if _, err := executeDesktop(t, controller, "desktop.observe", "screenshot", `{}`); !errors.Is(err, runtimecore.ErrResultTooLarge) {
		t.Fatalf("oversized screenshot error = %v, want result-too-large", err)
	}
}

func TestScreenshotRejectsChangedMissingOrInvalidDisplayGeometry(t *testing.T) {
	tests := []struct {
		name   string
		before displaySize
		after  *displaySize
		image  []byte
	}{
		{name: "missing size", before: displaySize{}, image: testJPEG(t, 16, 9)},
		{name: "out of bounds", before: displaySize{width: 16385, height: 900}, image: testJPEG(t, 16, 9)},
		{name: "changed during capture", before: displaySize{width: 1600, height: 900}, after: &displaySize{width: 1920, height: 1080}, image: testJPEG(t, 16, 9)},
		{name: "image aspect differs", before: displaySize{width: 1600, height: 900}, image: testJPEG(t, 9, 9)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeBackend{size: displaySize{width: 1600, height: 900}, image: test.image}
			controller := newTestController(t, backend)
			defer controller.Close()
			backend.size, backend.sizeAfterScreenshot = test.before, test.after
			if _, err := executeDesktop(t, controller, "desktop.observe", "screenshot", `{}`); err == nil {
				t.Fatal("screenshot with invalid or changing geometry was accepted")
			}
		})
	}
}

func TestDesktopClickRejectsGeometryChangedImmediatelyBeforeDispatch(t *testing.T) {
	backend := &fakeBackend{size: displaySize{width: 100, height: 50}, image: testJPEG(t, 50, 25),
		windows:         []window{{ID: "0x2a", Width: 100, Height: 50, Active: true}},
		changeDisplayAt: 4, sizeAfterDisplay: displaySize{width: 200, height: 100}}
	controller := newTestController(t, backend)
	defer controller.Close()
	token := screenshotTokenForTask(t, controller, "q0000001")
	_, err := executeDesktop(t, controller, "desktop.input", "click", clickJSON(t, "0x2a", 20, 20, token))
	if !errors.Is(err, runtimecore.ErrDesktopGeometryChanged) || backend.lastClick != [4]any{} {
		t.Fatalf("click after geometry changed = %#v, %v; want rejected without input", backend.lastClick, err)
	}
}

func TestDesktopClickRejectsFocusChangeAfterGeometryValidation(t *testing.T) {
	backend := &fakeBackend{
		size:                 displaySize{width: 100, height: 50},
		image:                testJPEG(t, 50, 25),
		windows:              []window{{ID: "0x2a", X: 10, Y: 10, Width: 60, Height: 30, Active: true}},
		focusChangeAtDisplay: 4,
	}
	controller := newTestController(t, backend)
	defer controller.Close()
	token := screenshotTokenForTask(t, controller, "q0000001")
	_, err := executeDesktop(t, controller, "desktop.input", "click", clickJSON(t, "0x2a", 20, 15, token))
	if !errors.Is(err, errDesktopFocusChanged) || backend.lastClick != [4]any{} {
		t.Fatalf("click after focus changed was dispatched: click=%#v err=%v", backend.lastClick, err)
	}
}

func TestDesktopClickMapsScreenshotCoordinatesToDisplayPixels(t *testing.T) {
	backend := &fakeBackend{size: displaySize{width: 1600, height: 900}, image: testJPEG(t, 800, 450), windows: []window{{
		ID: "0x2a", X: 600, Y: 300, Width: 500, Height: 300, Active: true,
	}}}
	controller := newTestController(t, backend)
	defer controller.Close()
	token := screenshotTokenForTask(t, controller, "q0000001")
	result, err := executeDesktop(t, controller, "desktop.input", "click", clickJSON(t, "0x2a", 400, 225, token))
	if err != nil || string(result) != `{"window_id":"0x2a","x":800,"y":450,"button":"left","clicked":true,"screenshot_width":800,"screenshot_height":450,"screen_width":1600,"screen_height":900}` ||
		backend.lastClick != [4]any{"0x2a", 200, 150, "left"} {
		t.Fatalf("scaled click result=%s recorded=%#v err=%v", result, backend.lastClick, err)
	}
}

func TestDesktopClickRejectsInvalidScreenshotCoordinatesAndNeverDispatchesOutOfBounds(t *testing.T) {
	backend := &fakeBackend{size: displaySize{width: 1600, height: 900}, image: testJPEG(t, 800, 450), windows: []window{{
		ID: "0x2a", X: 600, Y: 300, Width: 500, Height: 300, Active: true,
	}}}
	controller := newTestController(t, backend)
	defer controller.Close()
	for _, point := range [][2]int{{800, 225}, {400, 450}, {799, 449}} {
		token := screenshotTokenForTask(t, controller, "q0000001")
		if _, err := executeDesktop(t, controller, "desktop.input", "click", clickJSON(t, "0x2a", point[0], point[1], token)); !errors.Is(err, runtimecore.ErrInvalidRequest) || backend.lastClick != [4]any{} {
			t.Errorf("invalid or out-of-window screenshot click dispatched (%v): click=%#v err=%v", point, backend.lastClick, err)
		}
	}
	// Geometry comes from the one-time proof, so caller-supplied dimensions are rejected.
	for _, args := range []string{
		`{"window_id":"0x2a","x":1,"y":1}`,
		`{"window_id":"0x2a","x":1,"y":1,"screenshot_token":"not-a-token"}`,
		`{"window_id":"0x2a","x":1,"y":1,"screenshot_token":"` + strings.Repeat("a", 43) + `","screen_width":1600}`,
	} {
		if _, err := executeDesktop(t, controller, "desktop.input", "click", args); !errors.Is(err, runtimecore.ErrInvalidRequest) || backend.lastClick != [4]any{} {
			t.Errorf("invalid click arguments dispatched (%s): click=%#v err=%v", args, backend.lastClick, err)
		}
	}
}

func TestDesktopScreenshotProofIsTaskBoundSingleUseAndExpires(t *testing.T) {
	backend := &fakeBackend{size: displaySize{width: 100, height: 50}, image: testJPEG(t, 100, 50), windows: []window{{ID: "0x2a", X: 0, Y: 0, Width: 100, Height: 50, Active: true}}}
	controller := newTestController(t, backend)
	defer controller.Close()

	tokenA := screenshotTokenForTask(t, controller, "q0000001")
	if _, err := executeDesktopForTask(t, controller, "q0000002", "desktop.input", "click", clickJSON(t, "0x2a", 10, 10, tokenA)); !errors.Is(err, runtimecore.ErrInvalidRequest) {
		t.Fatalf("Task B accepted Task A screenshot proof: %v", err)
	}
	if _, err := executeDesktopForTask(t, controller, "q0000001", "desktop.input", "click", clickJSON(t, "0x2a", 10, 10, tokenA)); err != nil {
		t.Fatalf("Task A could not use its screenshot proof after a cross-task attempt: %v", err)
	}
	if _, err := executeDesktopForTask(t, controller, "q0000001", "desktop.input", "click", clickJSON(t, "0x2a", 10, 10, tokenA)); !errors.Is(err, runtimecore.ErrInvalidRequest) {
		t.Fatalf("replayed screenshot proof accepted: %v", err)
	}

	tokenB := screenshotTokenForTask(t, controller, "q0000001")
	controller.mu.Lock()
	proof := controller.screenshots["q0000001"]
	proof.expiresAt = time.Now().Add(-time.Second)
	controller.screenshots["q0000001"] = proof
	controller.mu.Unlock()
	if _, err := executeDesktopForTask(t, controller, "q0000001", "desktop.input", "click", clickJSON(t, "0x2a", 10, 10, tokenB)); !errors.Is(err, runtimecore.ErrInvalidRequest) {
		t.Fatalf("expired screenshot proof accepted: %v", err)
	}

	oldToken := screenshotTokenForTask(t, controller, "q0000001")
	newToken := screenshotTokenForTask(t, controller, "q0000001")
	if _, err := executeDesktopForTask(t, controller, "q0000001", "desktop.input", "click", clickJSON(t, "0x2a", 10, 10, oldToken)); !errors.Is(err, runtimecore.ErrInvalidRequest) {
		t.Fatalf("superseded screenshot proof accepted: %v", err)
	}
	if _, err := executeDesktopForTask(t, controller, "q0000001", "desktop.input", "click", clickJSON(t, "0x2a", 10, 10, newToken)); err != nil {
		t.Fatalf("new screenshot proof was invalidated by prior proof rejection: %v", err)
	}
}

func TestInvalidDesktopClickButtonDoesNotConsumeScreenshotProof(t *testing.T) {
	backend := &fakeBackend{size: displaySize{width: 100, height: 50}, image: testJPEG(t, 100, 50), windows: []window{{ID: "0x2a", X: 0, Y: 0, Width: 100, Height: 50, Active: true}}}
	controller := newTestController(t, backend)
	defer controller.Close()
	token := screenshotTokenForTask(t, controller, "q0000001")
	invalidArgs, err := json.Marshal(map[string]any{"window_id": "0x2a", "x": 10, "y": 10, "screenshot_token": token, "button": "side"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executeDesktopForTask(t, controller, "q0000001", "desktop.input", "click", string(invalidArgs)); !errors.Is(err, runtimecore.ErrInvalidRequest) {
		t.Fatalf("invalid button error = %v, want invalid request", err)
	}
	if backend.lastClick != [4]any{} {
		t.Fatalf("invalid button dispatched click: %#v", backend.lastClick)
	}
	if _, err := executeDesktopForTask(t, controller, "q0000001", "desktop.input", "click", clickJSON(t, "0x2a", 10, 10, token)); err != nil {
		t.Fatalf("invalid button consumed screenshot proof: %v", err)
	}
}

func TestDesktopScreenshotProofIsBoundToHostFenceAndLease(t *testing.T) {
	backend := &fakeBackend{size: displaySize{width: 100, height: 50}, image: testJPEG(t, 100, 50), windows: []window{{ID: "0x2a", X: 0, Y: 0, Width: 100, Height: 50, Active: true}}}
	controller := newTestController(t, backend)
	defer controller.Close()

	fence := contract.HostFence{HostID: "h1234567", RuntimeGeneration: 4, RuntimeInstanceID: "runtime-proof-test", ControlVersion: 9}
	screenshotRequest := contract.ExecuteRequest{
		HostFence: fence, TaskID: "q0000001", OperationID: "o0000001", Capability: "desktop.observe", Action: "screenshot", Arguments: json.RawMessage(`{}`),
		LeaseVersion: 12,
	}
	result, err := findHandler(t, controller, "desktop.observe", "screenshot").Execute(context.Background(), screenshotRequest)
	if err != nil {
		t.Fatalf("capture screenshot under Host fence and lease: %v", err)
	}
	var screenshot struct {
		Token string `json:"screenshot_token"`
	}
	if json.Unmarshal(result, &screenshot) != nil || !validScreenshotToken(screenshot.Token) {
		t.Fatalf("screenshot proof = %#v", screenshot)
	}

	for _, test := range []struct {
		name   string
		mutate func(*contract.ExecuteRequest)
	}{
		{name: "Host identity", mutate: func(request *contract.ExecuteRequest) { request.HostID = "h7654321" }},
		{name: "runtime generation", mutate: func(request *contract.ExecuteRequest) { request.RuntimeGeneration++ }},
		{name: "runtime instance", mutate: func(request *contract.ExecuteRequest) { request.RuntimeInstanceID = "runtime-other" }},
		{name: "control fence", mutate: func(request *contract.ExecuteRequest) { request.ControlVersion++ }},
		{name: "task lease", mutate: func(request *contract.ExecuteRequest) { request.LeaseVersion++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := screenshotRequest
			request.Capability, request.Action = "desktop.input", "click"
			request.Arguments = json.RawMessage(clickJSON(t, "0x2a", 10, 10, screenshot.Token))
			test.mutate(&request)
			if _, err := findHandler(t, controller, "desktop.input", "click").Execute(context.Background(), request); !errors.Is(err, runtimecore.ErrInvalidRequest) {
				t.Fatalf("proof crossed %s fence: %v", test.name, err)
			}
		})
	}

	validClick := screenshotRequest
	validClick.Capability, validClick.Action = "desktop.input", "click"
	validClick.Arguments = json.RawMessage(clickJSON(t, "0x2a", 10, 10, screenshot.Token))
	if _, err := findHandler(t, controller, "desktop.input", "click").Execute(context.Background(), validClick); err != nil {
		t.Fatalf("mismatched fence attempts consumed the valid screenshot proof: %v", err)
	}
}

func TestDesktopInputInvalidatesPendingScreenshotProof(t *testing.T) {
	for _, action := range []struct {
		capability string
		name       string
		arguments  string
	}{
		{capability: "desktop.input", name: "type", arguments: `{"window_id":"0x2a","text":"changed"}`},
		{capability: "desktop.input", name: "press_key", arguments: `{"window_id":"0x2a","key":"Enter"}`},
		{capability: "window.activate", name: "activate", arguments: `{"window_id":"0x2a"}`},
	} {
		t.Run(action.name, func(t *testing.T) {
			backend := &fakeBackend{size: displaySize{width: 100, height: 50}, image: testJPEG(t, 100, 50), windows: []window{{ID: "0x2a", X: 0, Y: 0, Width: 100, Height: 50, Active: true}}}
			controller := newTestController(t, backend)
			defer controller.Close()
			token := screenshotTokenForTask(t, controller, "q0000001")
			if _, err := executeDesktopForTask(t, controller, "q0000002", action.capability, action.name, action.arguments); err != nil {
				t.Fatal(err)
			}
			if _, err := executeDesktopForTask(t, controller, "q0000001", "desktop.input", "click", clickJSON(t, "0x2a", 10, 10, token)); !errors.Is(err, runtimecore.ErrInvalidRequest) {
				t.Fatalf("Task 1 screenshot proof remained valid after Task 2 %s: %v", action.name, err)
			}
		})
	}
}

func TestDesktopClickInvalidatesOtherTaskScreenshotProof(t *testing.T) {
	backend := &fakeBackend{size: displaySize{width: 100, height: 50}, image: testJPEG(t, 100, 50), windows: []window{{ID: "0x2a", X: 0, Y: 0, Width: 100, Height: 50, Active: true}}}
	controller := newTestController(t, backend)
	defer controller.Close()
	taskOneToken := screenshotTokenForTask(t, controller, "q0000001")
	taskTwoToken := screenshotTokenForTask(t, controller, "q0000002")
	if _, err := executeDesktopForTask(t, controller, "q0000002", "desktop.input", "click", clickJSON(t, "0x2a", 10, 10, taskTwoToken)); err != nil {
		t.Fatalf("Task 2 click failed: %v", err)
	}
	if _, err := executeDesktopForTask(t, controller, "q0000001", "desktop.input", "click", clickJSON(t, "0x2a", 10, 10, taskOneToken)); !errors.Is(err, runtimecore.ErrInvalidRequest) {
		t.Fatalf("Task 1 screenshot proof remained valid after Task 2 click: %v", err)
	}
}

func TestInvalidDesktopInputDoesNotInvalidateOtherTaskScreenshotProof(t *testing.T) {
	backend := &fakeBackend{size: displaySize{width: 100, height: 50}, image: testJPEG(t, 100, 50), windows: []window{{ID: "0x2a", X: 0, Y: 0, Width: 100, Height: 50, Active: true}}}
	controller := newTestController(t, backend)
	defer controller.Close()
	taskOneToken := screenshotTokenForTask(t, controller, "q0000001")
	if _, err := executeDesktopForTask(t, controller, "q0000002", "desktop.input", "type", `{"window_id":"0x2a","text":null}`); !errors.Is(err, runtimecore.ErrInvalidRequest) {
		t.Fatalf("invalid Task 2 input error = %v, want invalid request", err)
	}
	if _, err := executeDesktopForTask(t, controller, "q0000001", "desktop.input", "click", clickJSON(t, "0x2a", 10, 10, taskOneToken)); err != nil {
		t.Fatalf("invalid Task 2 input unexpectedly invalidated Task 1 screenshot proof: %v", err)
	}
}

func TestWindowActivationUsesCanonicalVisibleIDAndRevalidates(t *testing.T) {
	backend := &fakeBackend{size: displaySize{width: 100, height: 50}, windows: []window{{ID: "0x2a", Title: "visible", Width: 50, Height: 30}}}
	controller := newTestController(t, backend)
	defer controller.Close()
	result, err := executeDesktop(t, controller, "window.activate", "activate", `{"window_id":"0x2a"}`)
	if err != nil || string(result) != `{"window_id":"0x2a","activated":true}` || backend.activated != "0x2a" {
		t.Fatalf("activation result=%s activated=%q err=%v", result, backend.activated, err)
	}
	backend.windows = []window{{ID: "0x2b", Title: "different"}}
	if _, err := executeDesktop(t, controller, "window.activate", "activate", `{"window_id":"0x2a"}`); !errors.Is(err, runtimecore.ErrInvalidRequest) {
		t.Fatalf("stale window activation error = %v", err)
	}
	for _, id := range []string{"0X2a", "0x02a", "0x0", "0x100000000"} {
		args, _ := json.Marshal(map[string]string{"window_id": id})
		if _, err := executeDesktop(t, controller, "window.activate", "activate", string(args)); !errors.Is(err, runtimecore.ErrInvalidRequest) {
			t.Errorf("non-canonical window ID %q accepted: %v", id, err)
		}
	}
}

func TestWindowActivationFailsClosedWhenPostconditionDoesNotHold(t *testing.T) {
	tests := []struct {
		name         string
		windowsAfter []window
		wantErr      error
	}{
		{
			name:         "window manager did not activate target",
			windowsAfter: []window{{ID: "0x2a", Title: "visible", Width: 50, Height: 30}},
			wantErr:      errDesktopFocusChanged,
		},
		{
			name:         "target disappeared",
			windowsAfter: []window{},
			wantErr:      runtimecore.ErrInvalidRequest,
		},
		{
			name: "focus transferred to another window",
			windowsAfter: []window{
				{ID: "0x2a", Title: "visible", Width: 50, Height: 30},
				{ID: "0x2b", Title: "other", Width: 50, Height: 30, Active: true},
			},
			wantErr: errDesktopFocusChanged,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeBackend{
				size:                 displaySize{width: 100, height: 50},
				windows:              []window{{ID: "0x2a", Title: "visible", Width: 50, Height: 30}},
				windowsAfterActivate: &test.windowsAfter,
			}
			controller := newTestController(t, backend)
			defer controller.Close()
			result, err := executeDesktop(t, controller, "window.activate", "activate", `{"window_id":"0x2a"}`)
			if !errors.Is(err, test.wantErr) || result != nil || backend.activated != "0x2a" {
				t.Fatalf("activation result=%s activated=%q err=%v; want error %v and no result", result, backend.activated, err, test.wantErr)
			}
		})
	}
}

func TestWindowListReturnsAnEmptyArrayForNoWindows(t *testing.T) {
	controller := newTestController(t, &fakeBackend{size: displaySize{width: 100, height: 50}})
	defer controller.Close()
	result, err := executeDesktop(t, controller, "window.list", "list", `{}`)
	if err != nil || string(result) != `{"windows":[]}` {
		t.Fatalf("empty window list = %s, err = %v", result, err)
	}
}

func TestWindowListRejectsMalformedBackendRecords(t *testing.T) {
	valid := window{ID: "0x2a", Title: "visible", X: 0, Y: 0, Width: 50, Height: 30}
	maxInt := int(^uint(0) >> 1)
	tests := []struct {
		name    string
		windows []window
	}{
		{name: "invalid ID", windows: []window{{ID: "invalid", Width: 1, Height: 1}}},
		{name: "invalid geometry", windows: []window{{ID: "0x2a", Width: 0, Height: 1}}},
		{name: "overflowing horizontal geometry", windows: []window{{ID: "0x2a", X: maxInt - 1, Width: 2, Height: 1}}},
		{name: "overflowing vertical geometry", windows: []window{{ID: "0x2a", Y: maxInt - 1, Width: 1, Height: 2}}},
		{name: "oversized title", windows: []window{{ID: "0x2a", Title: strings.Repeat("x", maxWindowTitleBytes+1), Width: 1, Height: 1}}},
		{name: "invalid UTF-8 title", windows: []window{{ID: "0x2a", Title: string([]byte{0xff}), Width: 1, Height: 1}}},
		{name: "duplicate ID", windows: []window{valid, valid}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller := newTestController(t, &fakeBackend{size: displaySize{width: 100, height: 50}, windows: test.windows})
			defer controller.Close()
			result, err := executeDesktop(t, controller, "window.list", "list", `{}`)
			if result != nil || !errors.Is(err, ErrDesktopUnavailable) {
				t.Fatalf("window.list result=%s err=%v; want unavailable and no result", result, err)
			}
		})
	}
}

func TestClickTypeAndKeyValidationAndResultShapes(t *testing.T) {
	backend := &fakeBackend{size: displaySize{width: 100, height: 50}, image: testJPEG(t, 100, 50), windows: []window{{ID: "0x2a", X: 10, Y: 10, Width: 60, Height: 30, Active: true}}}
	controller := newTestController(t, backend)
	defer controller.Close()
	token := screenshotTokenForTask(t, controller, "q0000001")
	result, err := executeDesktop(t, controller, "desktop.input", "click", clickJSON(t, "0x2a", 69, 39, token))
	if err != nil || string(result) != `{"window_id":"0x2a","x":69,"y":39,"button":"left","clicked":true,"screenshot_width":100,"screenshot_height":50,"screen_width":100,"screen_height":50}` || backend.lastClick != [4]any{"0x2a", 59, 29, "left"} {
		t.Fatalf("click result=%s recorded=%#v err=%v", result, backend.lastClick, err)
	}
	token = screenshotTokenForTask(t, controller, "q0000001")
	backend.size = displaySize{width: 10, height: 10}
	lastClick := backend.lastClick
	if _, err := executeDesktop(t, controller, "desktop.input", "click", clickJSON(t, "0x2a", 1, 1, token)); !errors.Is(err, runtimecore.ErrDesktopGeometryChanged) || backend.lastClick != lastClick {
		t.Fatalf("click after screen resize was dispatched: click=%#v err=%v", backend.lastClick, err)
	}
	for _, args := range []string{
		`{"window_id":"0x2a","x":10,"y":0,"screen_width":10,"screen_height":10}`,
		`{"window_id":"0x2a","x":-1,"y":0,"screen_width":10,"screen_height":10}`,
		`{"window_id":"0x2a","x":1.0,"y":0,"screen_width":10,"screen_height":10}`,
		`{"window_id":"0x2a","y":0,"screen_width":10,"screen_height":10}`,
		`{"window_id":"0x2a","x":1,"y":2,"screen_width":10,"screen_height":10,"button":"side"}`,
		`{"window_id":"0x2a","x":1,"y":2,"screen_width":10,"screen_height":10,"extra":0}`,
		`{"window_id":"0x2a","x":1,"y":2,"screen_width":10,"screen_height":10,"button":null}`,
		`{"window_id":"0x2a","x":5,"y":5,"screen_width":10,"screen_height":10}`,
		`{"window_id":"0x2a","x":1,"y":2}`,
	} {
		if _, err := executeDesktop(t, controller, "desktop.input", "click", args); !errors.Is(err, runtimecore.ErrInvalidRequest) {
			t.Errorf("invalid click args accepted %s: %v", args, err)
		}
	}

	backend.size = displaySize{width: 100, height: 50}
	backend.windows[0].Active = false
	lastClick = backend.lastClick
	token = screenshotTokenForTask(t, controller, "q0000001")
	if _, err := executeDesktop(t, controller, "desktop.input", "click", clickJSON(t, "0x2a", 20, 20, token)); !errors.Is(err, errDesktopFocusChanged) || backend.lastClick != lastClick {
		t.Fatalf("click on an inactive window was dispatched: click=%#v err=%v", backend.lastClick, err)
	}
	backend.windows[0].Active = true
	result, err = executeDesktop(t, controller, "desktop.input", "type", `{"window_id":"0x2a","text":"你好🙂"}`)
	if err != nil || string(result) != `{"window_id":"0x2a","typed_chars":3}` || backend.text != "你好🙂" || backend.lastTarget != "0x2a" {
		t.Fatalf("type result=%s recorded=%q err=%v", result, backend.text, err)
	}
	tooLong, _ := json.Marshal(map[string]any{"window_id": "0x2a", "text": strings.Repeat("x", maxDesktopTextBytes+1)})
	for _, args := range []string{string(tooLong), `{"window_id":"0x2a","text":"x","extra":true}`, `{"window_id":"0x2a","text":null}`} {
		if _, err := executeDesktop(t, controller, "desktop.input", "type", args); !errors.Is(err, runtimecore.ErrInvalidRequest) {
			t.Errorf("invalid text args accepted: %v", err)
		}
	}
	if _, err := executeDesktop(t, controller, "desktop.input", "type", string([]byte{'{', '"', 'w', 'i', 'n', 'd', 'o', 'w', '_', 'i', 'd', '"', ':', '"', '0', 'x', '2', 'a', '"', ',', '"', 't', 'e', 'x', 't', '"', ':', '"', 0xff, '"', '}'})); !errors.Is(err, runtimecore.ErrInvalidRequest) {
		t.Fatalf("non-UTF-8 text accepted: %v", err)
	}

	result, err = executeDesktop(t, controller, "desktop.input", "press_key", `{"window_id":"0x2a","key":"Page_Up"}`)
	if err != nil || string(result) != `{"window_id":"0x2a","key":"Page_Up","pressed":true}` || backend.key != "Page_Up" {
		t.Fatalf("key result=%s recorded=%q err=%v", result, backend.key, err)
	}
	backend.windows[0].Active = false
	if _, err := executeDesktop(t, controller, "desktop.input", "type", `{"window_id":"0x2a","text":"x"}`); !errors.Is(err, errDesktopFocusChanged) {
		t.Fatalf("typing to inactive window accepted: %v", err)
	}
	if _, err := executeDesktop(t, controller, "desktop.input", "press_key", `{"window_id":"0x2a","key":"Enter"}`); !errors.Is(err, errDesktopFocusChanged) {
		t.Fatalf("key to inactive window accepted: %v", err)
	}
	if _, err := executeDesktop(t, controller, "desktop.input", "press_key", `{"window_id":"0x2a","key":"Space"}`); !errors.Is(err, runtimecore.ErrInvalidRequest) {
		t.Fatalf("key outside allowlist accepted: %v", err)
	}
}

func TestConcurrentCancellationWaitsForInputProcessToStop(t *testing.T) {
	backend := &fakeBackend{size: displaySize{width: 10, height: 10}, image: testJPEG(t, 10, 10), windows: []window{{ID: "0x2a", X: 0, Y: 0, Width: 10, Height: 10, Active: true}}, blockClick: true, clickCalled: make(chan struct{}, 1)}
	controller := newTestController(t, backend)
	defer controller.Close()
	token := screenshotTokenForTask(t, controller, "q0000001")
	handler := findHandler(t, controller, "desktop.input", "click")
	ctx, cancel := context.WithCancel(context.Background())
	request := contract.ExecuteRequest{OperationID: "o0000002", TaskID: "q0000001", Capability: "desktop.input", Action: "click", Arguments: json.RawMessage(clickJSON(t, "0x2a", 1, 2, token))}
	executeDone := make(chan error, 1)
	go func() { _, err := handler.Execute(ctx, request); executeDone <- err }()
	select {
	case <-backend.clickCalled:
	case <-time.After(time.Second):
		t.Fatal("click was not started")
	}
	cancel()
	stopDone := make(chan error, 1)
	go func() {
		_, err := handler.Stop(context.Background(), contract.Operation{OperationID: request.OperationID, TaskID: request.TaskID, Capability: request.Capability, Action: request.Action})
		stopDone <- err
	}()
	select {
	case err := <-executeDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled click error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("click did not stop on context cancellation")
	}
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop did not wait for the action to finish")
	}
}

func TestProbeFailureDoesNotAdvertiseCapabilities(t *testing.T) {
	if _, err := NewHandlers(Options{backend: &fakeBackend{probeErr: ErrDesktopUnavailable}}); !errors.Is(err, ErrDesktopUnavailable) {
		t.Fatalf("failed probe error = %v", err)
	}
}

func TestStrictArgumentsAndGeometryHelpers(t *testing.T) {
	for _, raw := range []string{`{"x":1,"x":2,"y":3}`, `{"x":1} {}`, `[]`, `null`, `{"X":1}`} {
		var args clickArguments
		if err := decodeObject([]byte(raw), []string{"window_id", "x", "y", "button"}, &args); err == nil {
			t.Errorf("invalid strict JSON accepted: %s", raw)
		}
	}
	geometry, err := parseGeometry([]byte("WINDOW=1\nX=-4\nY=7\nWIDTH=80\nHEIGHT=40\nSCREEN=0\n"))
	if err != nil || geometry != (windowGeometry{x: -4, y: 7, width: 80, height: 40}) {
		t.Fatalf("geometry=%#v err=%v", geometry, err)
	}
	if _, err := parseGeometry([]byte("X=0\nY=0\nWIDTH=0\nHEIGHT=1\n")); err == nil {
		t.Fatal("zero-sized window geometry accepted")
	}
}

func newTestController(t *testing.T, backend *fakeBackend) *Controller {
	t.Helper()
	controller, err := NewHandlers(Options{backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func executeDesktop(t *testing.T, controller *Controller, capability, action, args string) (json.RawMessage, error) {
	t.Helper()
	return executeDesktopForTask(t, controller, "q0000001", capability, action, args)
}

func executeDesktopForTask(t *testing.T, controller *Controller, taskID, capability, action, args string) (json.RawMessage, error) {
	t.Helper()
	return findHandler(t, controller, capability, action).Execute(context.Background(), contract.ExecuteRequest{
		OperationID: "o0000001", TaskID: taskID, Capability: capability, Action: action, Arguments: json.RawMessage(args),
	})
}

func screenshotTokenForTask(t *testing.T, controller *Controller, taskID string) string {
	t.Helper()
	result, err := executeDesktopForTask(t, controller, taskID, "desktop.observe", "screenshot", `{}`)
	if err != nil {
		t.Fatalf("capture screenshot for Task %s: %v", taskID, err)
	}
	var value struct {
		ScreenshotToken string `json:"screenshot_token"`
	}
	if err := json.Unmarshal(result, &value); err != nil || !validScreenshotToken(value.ScreenshotToken) {
		t.Fatalf("screenshot proof for Task %s is invalid: %+v, err=%v", taskID, value, err)
	}
	return value.ScreenshotToken
}

func clickJSON(t *testing.T, windowID string, x, y int, screenshotToken string) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"window_id": windowID, "x": x, "y": y, "screenshot_token": screenshotToken})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func findHandler(t *testing.T, controller *Controller, capability, action string) runtimecore.ActionHandler {
	t.Helper()
	for _, handler := range controller.Handlers() {
		if handler.Capability() == capability && handler.Action() == action {
			return handler
		}
	}
	t.Fatalf("handler %s/%s not found", capability, action)
	return nil
}

func testJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 30), G: uint8(y * 50), B: 90, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
