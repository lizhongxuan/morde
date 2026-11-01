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
	mu          sync.Mutex
	size        displaySize
	image       []byte
	windows     []window
	activated   string
	lastClick   [4]any
	lastTarget  string
	text        string
	key         string
	probeErr    error
	blockClick  bool
	clickCalled chan struct{}
}

func (b *fakeBackend) probe(context.Context) (displaySize, error) {
	if b.probeErr != nil {
		return displaySize{}, b.probeErr
	}
	return b.size, nil
}
func (b *fakeBackend) display(context.Context) (displaySize, error) { return b.size, nil }
func (b *fakeBackend) screenshot(context.Context) ([]byte, error)   { return b.image, nil }
func (b *fakeBackend) listWindows(context.Context) ([]window, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]window(nil), b.windows...), nil
}
func (b *fakeBackend) activate(_ context.Context, id string) error {
	b.mu.Lock()
	b.activated = id
	b.mu.Unlock()
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
	imageBytes := testJPEG(t, 7, 3)
	backend := &fakeBackend{size: displaySize{width: 1600, height: 900}, image: imageBytes}
	controller := newTestController(t, backend)
	defer controller.Close()
	result, err := executeDesktop(t, controller, "desktop.observe", "screenshot", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		MIMEType   string `json:"mime_type"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		DataBase64 string `json:"data_base64"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(response.DataBase64)
	if err != nil {
		t.Fatal(err)
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(decoded))
	if err != nil || response.MIMEType != "image/jpeg" || response.Width != config.Width || response.Height != config.Height || response.Width != 7 || response.Height != 3 {
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

func TestWindowListReturnsAnEmptyArrayForNoWindows(t *testing.T) {
	controller := newTestController(t, &fakeBackend{size: displaySize{width: 100, height: 50}})
	defer controller.Close()
	result, err := executeDesktop(t, controller, "window.list", "list", `{}`)
	if err != nil || string(result) != `{"windows":[]}` {
		t.Fatalf("empty window list = %s, err = %v", result, err)
	}
}

func TestClickTypeAndKeyValidationAndResultShapes(t *testing.T) {
	backend := &fakeBackend{size: displaySize{width: 100, height: 50}, windows: []window{{ID: "0x2a", X: 10, Y: 10, Width: 60, Height: 30, Active: true}}}
	controller := newTestController(t, backend)
	defer controller.Close()
	result, err := executeDesktop(t, controller, "desktop.input", "click", `{"window_id":"0x2a","x":69,"y":39}`)
	if err != nil || string(result) != `{"window_id":"0x2a","x":69,"y":39,"button":"left","clicked":true}` || backend.lastClick != [4]any{"0x2a", 59, 29, "left"} {
		t.Fatalf("click result=%s recorded=%#v err=%v", result, backend.lastClick, err)
	}
	backend.size = displaySize{width: 10, height: 10}
	for _, args := range []string{
		`{"window_id":"0x2a","x":10,"y":0}`, `{"window_id":"0x2a","x":-1,"y":0}`, `{"window_id":"0x2a","x":1.0,"y":0}`, `{"window_id":"0x2a","y":0}`, `{"window_id":"0x2a","x":1,"y":2,"button":"side"}`, `{"window_id":"0x2a","x":1,"y":2,"extra":0}`, `{"window_id":"0x2a","x":1,"y":2,"button":null}`, `{"window_id":"0x2a","x":5,"y":5}`,
	} {
		if _, err := executeDesktop(t, controller, "desktop.input", "click", args); !errors.Is(err, runtimecore.ErrInvalidRequest) {
			t.Errorf("invalid click args accepted %s: %v", args, err)
		}
	}

	backend.size = displaySize{width: 100, height: 50}
	backend.windows[0].Active = false
	lastClick := backend.lastClick
	if _, err := executeDesktop(t, controller, "desktop.input", "click", `{"window_id":"0x2a","x":20,"y":20}`); !errors.Is(err, errDesktopFocusChanged) || backend.lastClick != lastClick {
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
	backend := &fakeBackend{size: displaySize{width: 10, height: 10}, windows: []window{{ID: "0x2a", X: 0, Y: 0, Width: 10, Height: 10, Active: true}}, blockClick: true, clickCalled: make(chan struct{}, 1)}
	controller := newTestController(t, backend)
	defer controller.Close()
	handler := findHandler(t, controller, "desktop.input", "click")
	ctx, cancel := context.WithCancel(context.Background())
	request := contract.ExecuteRequest{OperationID: "o0000001", TaskID: "q0000001", Capability: "desktop.input", Action: "click", Arguments: json.RawMessage(`{"window_id":"0x2a","x":1,"y":2}`)}
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
	return findHandler(t, controller, capability, action).Execute(context.Background(), contract.ExecuteRequest{
		OperationID: "o0000001", TaskID: "q0000001", Capability: capability, Action: action, Arguments: json.RawMessage(args),
	})
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
