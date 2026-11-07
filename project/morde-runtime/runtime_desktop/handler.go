// Package runtime_desktop provides fenced native desktop actions for the Host.
package runtime_desktop

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"morde-runtime/contract"
	"morde-runtime/runtimecore"
)

const (
	probeTimeout             = 5 * time.Second
	capabilityDesktopObserve = "desktop.observe"
	capabilityDesktopInput   = "desktop.input"
	capabilityWindowList     = "window.list"
	capabilityWindowActivate = "window.activate"
	maxScreenshotBytes       = 512 << 10
	maxScreenshotJSONBytes   = 700 << 10
	maxDisplayWidth          = 16384
	maxDisplayHeight         = 16384
	maxDisplayPixels         = 64 << 20
	maxDesktopTextBytes      = 4096
	maxDesktopWindows        = 128
	maxWindowTitleBytes      = 512
)

var (
	ErrDesktopUnavailable  = errors.New("desktop control unavailable")
	errDesktopFocusChanged = runtimecore.ErrDesktopFocusChanged
)

type displaySize struct{ width, height int }

type window struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Active bool   `json:"active"`
}

type desktopBackend interface {
	probe(context.Context) (displaySize, error)
	display(context.Context) (displaySize, error)
	screenshot(context.Context) ([]byte, error)
	listWindows(context.Context) ([]window, error)
	activate(context.Context, string) error
	click(context.Context, string, int, int, string) error
	typeText(context.Context, string, string) error
	pressKey(context.Context, string, string) error
}

type Options struct{ backend desktopBackend }

type Controller struct {
	backend desktopBackend

	serial chan struct{}
	mu     sync.Mutex
	active map[string]chan struct{}
}

type actionHandler struct {
	controller *Controller
	capability string
	action     string
}

var _ runtimecore.ActionHandler = (*actionHandler)(nil)

func NewHandlers(options Options) (*Controller, error) {
	backend := options.backend
	if backend == nil {
		var err error
		backend, err = newSystemBackend()
		if err != nil {
			return nil, ErrDesktopUnavailable
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	size, err := backend.probe(ctx)
	if err != nil || size.width < 1 || size.height < 1 {
		return nil, ErrDesktopUnavailable
	}
	return &Controller{backend: backend, serial: make(chan struct{}, 1), active: make(map[string]chan struct{})}, nil
}

func (c *Controller) Handlers() []runtimecore.ActionHandler {
	if c == nil {
		return nil
	}
	definitions := [][2]string{
		{capabilityDesktopObserve, "screenshot"},
		{capabilityWindowList, "list"},
		{capabilityWindowActivate, "activate"},
		{capabilityDesktopInput, "click"},
		{capabilityDesktopInput, "type"},
		{capabilityDesktopInput, "press_key"},
	}
	handlers := make([]runtimecore.ActionHandler, 0, len(definitions))
	for _, definition := range definitions {
		handlers = append(handlers, &actionHandler{controller: c, capability: definition[0], action: definition[1]})
	}
	return handlers
}

func (c *Controller) Close() error { return nil }

func (h *actionHandler) Capability() string { return h.capability }
func (h *actionHandler) Action() string     { return h.action }

func (h *actionHandler) Execute(ctx context.Context, request contract.ExecuteRequest) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if h == nil || h.controller == nil || request.Capability != h.capability || request.Action != h.action ||
		!validShortID(request.OperationID, 'o') || !validShortID(request.TaskID, 'q') {
		return nil, runtimecore.ErrInvalidRequest
	}
	return h.controller.execute(ctx, request, h.action)
}

func (h *actionHandler) Stop(ctx context.Context, operation contract.Operation) (runtimecore.StopResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if h == nil || h.controller == nil || operation.Capability != h.capability || operation.Action != h.action ||
		!validShortID(operation.OperationID, 'o') || !validShortID(operation.TaskID, 'q') {
		return runtimecore.StopResult{}, runtimecore.ErrInvalidRequest
	}
	h.controller.mu.Lock()
	done := h.controller.active[operation.OperationID]
	h.controller.mu.Unlock()
	if done == nil {
		return runtimecore.StopResult{VerifiedStopped: true}, nil
	}
	select {
	case <-done:
		return runtimecore.StopResult{VerifiedStopped: true}, nil
	case <-ctx.Done():
		return runtimecore.StopResult{}, ctx.Err()
	}
}

type clickArguments struct {
	WindowID         string          `json:"window_id"`
	X                *int            `json:"x"`
	Y                *int            `json:"y"`
	ScreenWidth      *int            `json:"screen_width"`
	ScreenHeight     *int            `json:"screen_height"`
	ScreenshotWidth  *int            `json:"screenshot_width"`
	ScreenshotHeight *int            `json:"screenshot_height"`
	Button           json.RawMessage `json:"button"`
}

type typeArguments struct {
	WindowID string  `json:"window_id"`
	Text     *string `json:"text"`
}
type keyArguments struct {
	WindowID string `json:"window_id"`
	Key      string `json:"key"`
}
type activateArguments struct {
	WindowID string `json:"window_id"`
}

func (c *Controller) execute(ctx context.Context, request contract.ExecuteRequest, action string) (json.RawMessage, error) {
	allowed := map[string][]string{
		"screenshot": {}, "list": {}, "activate": {"window_id"},
		"click": {"window_id", "x", "y", "screen_width", "screen_height", "screenshot_width", "screenshot_height", "button"}, "type": {"window_id", "text"}, "press_key": {"window_id", "key"},
	}
	keys, ok := allowed[action]
	if !ok {
		return nil, runtimecore.ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.acquire(ctx, request.OperationID); err != nil {
		return nil, err
	}
	defer c.release(request.OperationID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var err error
	var result any
	switch action {
	case "screenshot":
		if err := decodeObject(request.Arguments, nil, &struct{}{}); err != nil {
			return nil, runtimecore.ErrInvalidRequest
		}
		result, err = c.screenshot(ctx)
		if errors.Is(err, runtimecore.ErrResultTooLarge) {
			return nil, err
		}
	case "list":
		if err := decodeObject(request.Arguments, nil, &struct{}{}); err != nil {
			return nil, runtimecore.ErrInvalidRequest
		}
		windows, err := c.backend.listWindows(ctx)
		if err != nil {
			return nil, contextOrActionError(ctx, err)
		}
		if len(windows) > maxDesktopWindows {
			return nil, runtimecore.ErrResultTooLarge
		}
		if windows == nil {
			windows = []window{}
		}
		result = struct {
			Windows []window `json:"windows"`
		}{windows}
	case "activate":
		var args activateArguments
		if err := decodeObject(request.Arguments, keys, &args); err != nil || !validWindowID(args.WindowID) {
			return nil, runtimecore.ErrInvalidRequest
		}
		if _, err := c.visibleWindow(ctx, args.WindowID); err != nil {
			return nil, err
		}
		if err := c.backend.activate(ctx, args.WindowID); err != nil {
			return nil, contextOrActionError(ctx, err)
		}
		result = struct {
			WindowID  string `json:"window_id"`
			Activated bool   `json:"activated"`
		}{args.WindowID, true}
	case "click":
		var args clickArguments
		if err := decodeObject(request.Arguments, keys, &args); err != nil || !validWindowID(args.WindowID) || args.X == nil || args.Y == nil ||
			args.ScreenWidth == nil || args.ScreenHeight == nil || *args.X < 0 || *args.Y < 0 ||
			(args.ScreenshotWidth == nil) != (args.ScreenshotHeight == nil) {
			return nil, runtimecore.ErrInvalidRequest
		}
		expected := displaySize{width: *args.ScreenWidth, height: *args.ScreenHeight}
		if !validScreenshotScreen(expected) {
			return nil, runtimecore.ErrInvalidRequest
		}
		pointSpace := expected
		if args.ScreenshotWidth != nil {
			pointSpace = displaySize{width: *args.ScreenshotWidth, height: *args.ScreenshotHeight}
			if !validScreenshotScreen(pointSpace) || pointSpace.width > 1280 || pointSpace.height > 720 || !validScreenshotAspect(pointSpace.width, pointSpace.height, expected) {
				return nil, runtimecore.ErrInvalidRequest
			}
		}
		if *args.X >= pointSpace.width || *args.Y >= pointSpace.height {
			return nil, runtimecore.ErrInvalidRequest
		}
		button := "left"
		if len(args.Button) != 0 {
			if bytes.Equal(bytes.TrimSpace(args.Button), []byte("null")) || json.Unmarshal(args.Button, &button) != nil || (button != "left" && button != "right" && button != "middle") {
				return nil, runtimecore.ErrInvalidRequest
			}
		}
		size, err := c.backend.display(ctx)
		if err != nil {
			return nil, contextOrUnavailable(ctx)
		}
		if !validScreenshotScreen(size) {
			return nil, ErrDesktopUnavailable
		}
		if size != expected {
			return nil, runtimecore.ErrDesktopGeometryChanged
		}
		target, err := c.visibleWindow(ctx, args.WindowID)
		if err != nil {
			return nil, err
		}
		if !target.Active {
			return nil, errDesktopFocusChanged
		}
		screenX, screenY := *args.X, *args.Y
		if args.ScreenshotWidth != nil {
			screenX = scaleScreenshotCoordinate(screenX, pointSpace.width, expected.width)
			screenY = scaleScreenshotCoordinate(screenY, pointSpace.height, expected.height)
		}
		if !validWindowGeometry(target) || screenX < target.X || screenY < target.Y ||
			int64(screenX) >= int64(target.X)+int64(target.Width) || int64(screenY) >= int64(target.Y)+int64(target.Height) {
			return nil, runtimecore.ErrInvalidRequest
		}
		latest, err := c.backend.display(ctx)
		if err != nil || !validScreenshotScreen(latest) {
			return nil, contextOrUnavailable(ctx)
		}
		if latest != expected {
			return nil, runtimecore.ErrDesktopGeometryChanged
		}
		finalTarget, err := c.visibleWindow(ctx, args.WindowID)
		if err != nil {
			return nil, err
		}
		if !finalTarget.Active {
			return nil, errDesktopFocusChanged
		}
		if finalTarget.X != target.X || finalTarget.Y != target.Y || finalTarget.Width != target.Width || finalTarget.Height != target.Height {
			return nil, runtimecore.ErrDesktopGeometryChanged
		}
		if err := c.backend.click(ctx, args.WindowID, screenX-target.X, screenY-target.Y, button); err != nil {
			return nil, contextOrActionError(ctx, err)
		}
		result = struct {
			WindowID string `json:"window_id"`
			X        int    `json:"x"`
			Y        int    `json:"y"`
			Button   string `json:"button"`
			Clicked  bool   `json:"clicked"`
		}{args.WindowID, screenX, screenY, button, true}
	case "type":
		var args typeArguments
		if err := decodeObject(request.Arguments, keys, &args); err != nil || !validWindowID(args.WindowID) || args.Text == nil || len(*args.Text) > maxDesktopTextBytes ||
			!utf8.ValidString(*args.Text) || strings.IndexByte(*args.Text, 0) >= 0 {
			return nil, runtimecore.ErrInvalidRequest
		}
		target, err := c.visibleWindow(ctx, args.WindowID)
		if err != nil {
			return nil, err
		}
		if !target.Active {
			return nil, errDesktopFocusChanged
		}
		if err := c.backend.typeText(ctx, args.WindowID, *args.Text); err != nil {
			return nil, contextOrActionError(ctx, err)
		}
		result = struct {
			WindowID   string `json:"window_id"`
			TypedChars int    `json:"typed_chars"`
		}{args.WindowID, utf8.RuneCountInString(*args.Text)}
	case "press_key":
		var args keyArguments
		if err := decodeObject(request.Arguments, keys, &args); err != nil || !validWindowID(args.WindowID) || !validDesktopKey(args.Key) {
			return nil, runtimecore.ErrInvalidRequest
		}
		target, err := c.visibleWindow(ctx, args.WindowID)
		if err != nil {
			return nil, err
		}
		if !target.Active {
			return nil, errDesktopFocusChanged
		}
		if err := c.backend.pressKey(ctx, args.WindowID, args.Key); err != nil {
			return nil, contextOrActionError(ctx, err)
		}
		result = struct {
			WindowID string `json:"window_id"`
			Key      string `json:"key"`
			Pressed  bool   `json:"pressed"`
		}{args.WindowID, args.Key, true}
	}
	if err != nil {
		return nil, ErrDesktopUnavailable
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, ErrDesktopUnavailable
	}
	if len(encoded) > runtimecore.MaxOperationResultBytes {
		return nil, runtimecore.ErrResultTooLarge
	}
	if action == "screenshot" && len(encoded) > maxScreenshotJSONBytes {
		return nil, runtimecore.ErrResultTooLarge
	}
	return encoded, nil
}

func (c *Controller) screenshot(ctx context.Context) (any, error) {
	screen, err := c.backend.display(ctx)
	if err != nil || !validScreenshotScreen(screen) {
		return nil, contextOrUnavailable(ctx)
	}
	data, err := c.backend.screenshot(ctx)
	if err != nil {
		return nil, contextOrUnavailable(ctx)
	}
	after, err := c.backend.display(ctx)
	if err != nil || after != screen {
		return nil, contextOrUnavailable(ctx)
	}
	if len(data) < 4 || len(data) > maxScreenshotBytes {
		return nil, runtimecore.ErrResultTooLarge
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 1280 || config.Height > 720 ||
		config.Width > screen.width || config.Height > screen.height || !validScreenshotAspect(config.Width, config.Height, screen) {
		return nil, runtimecore.ErrResultTooLarge
	}
	if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
		return nil, runtimecore.ErrResultTooLarge
	}
	return struct {
		MIMEType     string `json:"mime_type"`
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		ScreenWidth  int    `json:"screen_width"`
		ScreenHeight int    `json:"screen_height"`
		DataBase64   string `json:"data_base64"`
	}{"image/jpeg", config.Width, config.Height, screen.width, screen.height, base64.StdEncoding.EncodeToString(data)}, nil
}

func validScreenshotScreen(screen displaySize) bool {
	return screen.width > 0 && screen.width <= maxDisplayWidth && screen.height > 0 && screen.height <= maxDisplayHeight &&
		int64(screen.width)*int64(screen.height) <= maxDisplayPixels
}

func validScreenshotAspect(width, height int, screen displaySize) bool {
	difference := int64(width)*int64(screen.height) - int64(height)*int64(screen.width)
	if difference < 0 {
		difference = -difference
	}
	return difference <= int64(max(screen.width, screen.height))
}

func scaleScreenshotCoordinate(value, screenshotSize, screenSize int) int {
	return int(int64(value) * int64(screenSize) / int64(screenshotSize))
}

func (c *Controller) visibleWindow(ctx context.Context, id string) (window, error) {
	windows, err := c.backend.listWindows(ctx)
	if err != nil {
		return window{}, contextOrActionError(ctx, err)
	}
	for _, candidate := range windows {
		if candidate.ID == id {
			if !validWindowGeometry(candidate) {
				return window{}, ErrDesktopUnavailable
			}
			return candidate, nil
		}
	}
	return window{}, runtimecore.ErrInvalidRequest
}

func (c *Controller) acquire(ctx context.Context, operationID string) error {
	done := make(chan struct{})
	c.mu.Lock()
	if _, exists := c.active[operationID]; exists {
		c.mu.Unlock()
		return runtimecore.ErrInvalidRequest
	}
	c.active[operationID] = done
	c.mu.Unlock()
	select {
	case c.serial <- struct{}{}:
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.active, operationID)
		close(done)
		c.mu.Unlock()
		return ctx.Err()
	}
}

func (c *Controller) release(operationID string) {
	<-c.serial
	c.mu.Lock()
	if done := c.active[operationID]; done != nil {
		delete(c.active, operationID)
		close(done)
	}
	c.mu.Unlock()
}

func decodeObject(raw []byte, allowed []string, target any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || len(trimmed) > 16<<10 || trimmed[0] != '{' || !utf8.Valid(trimmed) {
		return errors.New("invalid desktop arguments")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('{') {
		return errors.New("invalid desktop arguments")
	}
	permitted := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		permitted[key] = true
	}
	seen := make(map[string]bool, len(allowed))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok || !permitted[key] || seen[key] {
			return errors.New("invalid desktop field")
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if err := ensureEOF(decoder); err != nil {
		return err
	}
	decoder = json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return ensureEOF(decoder)
}

func ensureEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing desktop JSON")
	}
	return nil
}

func contextOrUnavailable(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrDesktopUnavailable
}

func contextOrActionError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, runtimecore.ErrInvalidRequest) {
		return runtimecore.ErrInvalidRequest
	}
	if errors.Is(err, errDesktopFocusChanged) {
		return errDesktopFocusChanged
	}
	if errors.Is(err, runtimecore.ErrResultTooLarge) {
		return runtimecore.ErrResultTooLarge
	}
	return ErrDesktopUnavailable
}

func validWindowGeometry(target window) bool {
	return target.Width > 0 && target.Height > 0 &&
		int64(target.X)+int64(target.Width) <= int64(^uint(0)>>1) &&
		int64(target.Y)+int64(target.Height) <= int64(^uint(0)>>1)
}

func validShortID(value string, prefix byte) bool {
	if len(value) != 8 || value[0] != prefix {
		return false
	}
	for index := 1; index < len(value); index++ {
		c := value[index]
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func validWindowID(value string) bool {
	if len(value) < 3 || !strings.HasPrefix(value, "0x") || strings.ToLower(value) != value {
		return false
	}
	parsed, err := strconv.ParseUint(value[2:], 16, 32)
	return err == nil && parsed != 0 && fmt.Sprintf("0x%x", parsed) == value
}

type windowGeometry struct{ x, y, width, height int }

func parseGeometry(output []byte) (windowGeometry, error) {
	values := make(map[string]int, 4)
	for _, line := range strings.Split(string(output), "\n") {
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "X", "Y", "WIDTH", "HEIGHT":
			value, err := strconv.Atoi(parts[1])
			if err != nil {
				return windowGeometry{}, ErrDesktopUnavailable
			}
			values[parts[0]] = value
		}
	}
	if len(values) != 4 || values["WIDTH"] < 1 || values["HEIGHT"] < 1 {
		return windowGeometry{}, ErrDesktopUnavailable
	}
	return windowGeometry{x: values["X"], y: values["Y"], width: values["WIDTH"], height: values["HEIGHT"]}, nil
}

func validDesktopKey(value string) bool {
	switch value {
	case "Enter", "Tab", "Escape", "BackSpace", "Delete", "Home", "End", "Page_Up", "Page_Down", "ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight":
		return true
	default:
		return false
	}
}
