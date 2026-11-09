//go:build linux

package runtime_desktop

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"morde-runtime/contract"
)

// Set MORDE_RUNTIME_DESKTOP_LIVE_X11=1 to exercise the real Linux backend
// against a local X11 server and window manager. The test does not use network
// services and requires python3 with tkinter, xdotool, xdpyinfo, xwininfo, and ImageMagick.
func TestLiveX11DesktopSmoke(t *testing.T) {
	if os.Getenv("MORDE_RUNTIME_DESKTOP_LIVE_X11") != "1" {
		t.Skip("set MORDE_RUNTIME_DESKTOP_LIVE_X11=1 to run the local X11 smoke test")
	}

	controller, err := NewHandlers(Options{})
	if err != nil {
		t.Fatalf("initialize Linux desktop backend: %v", err)
	}
	defer controller.Close()

	directory := t.TempDir()
	textPath := filepath.Join(directory, "typed-text.txt")
	targetTitle := fmt.Sprintf("Morde T07 target %d", time.Now().UnixNano())
	distractorTitle := fmt.Sprintf("Morde T07 distractor %d", time.Now().UnixNano())
	const textToType = "morde-live-x11-smoke"
	const uiScript = `import sys, tkinter as tk
root = tk.Tk()
root.title(sys.argv[2])
root.geometry("640x220+60+60")
value = tk.StringVar()
entry = tk.Entry(root, textvariable=value, width=44, font=("DejaVu Sans", 16))
entry.pack(pady=98)
def persist(*_):
    with open(sys.argv[1], "w", encoding="utf-8") as output:
        output.write(value.get())
value.trace_add("write", persist)
other = tk.Toplevel(root)
other.title(sys.argv[3])
other.geometry("300x120+800+60")
tk.Label(other, text="focus target").pack(pady=35)
root.mainloop()
`
	app := exec.Command("python3", "-c", uiScript, textPath, targetTitle, distractorTitle)
	app.Stdout, app.Stderr = io.Discard, io.Discard
	if err := app.Start(); err != nil {
		t.Fatalf("start Tkinter smoke windows: %v", err)
	}
	t.Cleanup(func() {
		_ = app.Process.Kill()
		_ = app.Wait()
	})

	var windows []window
	waitFor(t, 10*time.Second, "both Tkinter windows to become visible", func() bool {
		var err error
		windows, err = liveDesktopWindows(controller)
		if err != nil {
			return false
		}
		return findWindow(windows, targetTitle) != nil && findWindow(windows, distractorTitle) != nil
	})
	target := findWindow(windows, targetTitle)
	distractor := findWindow(windows, distractorTitle)
	if target == nil || distractor == nil {
		t.Fatalf("smoke windows not found: target=%#v distractor=%#v", target, distractor)
	}

	if _, err := liveDesktopAction(controller, "window.activate", "activate", jsonObject(t, map[string]any{"window_id": distractor.ID})); err != nil {
		t.Fatalf("activate distractor window: %v", err)
	}
	active := waitForActiveWindow(t, controller, distractorTitle)
	if active == nil {
		t.Fatalf("distractor did not become active")
	}
	if _, err := liveDesktopAction(controller, "window.activate", "activate", jsonObject(t, map[string]any{"window_id": target.ID})); err != nil {
		t.Fatalf("activate target window: %v", err)
	}
	active = waitForActiveWindow(t, controller, targetTitle)
	if active == nil {
		t.Fatalf("target did not become active")
	}
	target = active

	screenshotRaw, err := liveDesktopAction(controller, "desktop.observe", "screenshot", `{}`)
	if err != nil {
		t.Fatalf("capture screenshot: %v", err)
	}
	var screenshot struct {
		MIMEType        string `json:"mime_type"`
		Width           int    `json:"width"`
		Height          int    `json:"height"`
		ScreenWidth     int    `json:"screen_width"`
		ScreenHeight    int    `json:"screen_height"`
		ScreenshotToken string `json:"screenshot_token"`
		DataBase64      string `json:"data_base64"`
	}
	if err := json.Unmarshal(screenshotRaw, &screenshot); err != nil {
		t.Fatalf("decode screenshot response: %v", err)
	}
	data, err := base64.StdEncoding.DecodeString(screenshot.DataBase64)
	if err != nil {
		t.Fatalf("decode screenshot image: %v", err)
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || screenshot.MIMEType != "image/jpeg" || config.Width != screenshot.Width || config.Height != screenshot.Height {
		t.Fatalf("screenshot is not a valid JPEG: response=%#v config=%#v err=%v", screenshot, config, err)
	}

	clickArgs := map[string]any{
		"window_id":        target.ID,
		"x":                scaleScreenshotCoordinate(target.X+target.Width/2, screenshot.ScreenWidth, screenshot.Width),
		"y":                scaleScreenshotCoordinate(target.Y+target.Height/2, screenshot.ScreenHeight, screenshot.Height),
		"screenshot_token": screenshot.ScreenshotToken,
	}
	if _, err := liveDesktopAction(controller, "desktop.input", "click", jsonObject(t, clickArgs)); err != nil {
		backend := controller.backend.(*linuxBackend)
		pointer, _ := backend.run(context.Background(), backend.paths.xdotool, 4<<10, "getmouselocation", "--shell")
		geometry, _ := backend.run(context.Background(), backend.paths.xwininfo, 4<<10, "-id", target.ID)
		t.Fatalf("click target entry: %v; window=%#v pointer=%s xwininfo=%s", err, target, pointer, geometry)
	}
	backend := controller.backend.(*linuxBackend)
	pointerOutput, err := backend.run(context.Background(), backend.paths.xdotool, 4<<10, "getmouselocation", "--shell")
	if err != nil {
		t.Fatalf("read pointer after click: %v", err)
	}
	pointer, err := parsePointerLocation(pointerOutput)
	if err != nil {
		t.Fatalf("parse pointer after click: %v", err)
	}
	parentOutput, err := backend.run(context.Background(), backend.paths.xwininfo, maxWindowTreeOutput, "-id", target.ID, "-tree")
	if err != nil {
		t.Fatalf("read target parent after click: %v", err)
	}
	parent, err := parseXWindowInfoParent(parentOutput)
	if err != nil {
		t.Fatalf("parse target parent after click: %v", err)
	}
	t.Logf("real X11 pointer window=%s target=%s frame=%s", pointer.windowID, target.ID, parent)
	if _, err := liveDesktopAction(controller, "desktop.input", "type", jsonObject(t, map[string]any{"window_id": target.ID, "text": textToType})); err != nil {
		t.Fatalf("type into target entry: %v", err)
	}
	waitFor(t, 5*time.Second, "Tkinter entry to contain typed text", func() bool {
		data, err := os.ReadFile(textPath)
		return err == nil && string(data) == textToType
	})
}

// Set MORDE_RUNTIME_DESKTOP_NO_WM=1 on a headless X server without a window
// manager to verify client coordinates stay absolute when no frame is present.
func TestLiveX11WindowGeometryWithoutWindowManager(t *testing.T) {
	if os.Getenv("MORDE_RUNTIME_DESKTOP_NO_WM") != "1" {
		t.Skip("set MORDE_RUNTIME_DESKTOP_NO_WM=1 on an X server without a window manager")
	}
	controller, err := NewHandlers(Options{})
	if err != nil {
		t.Fatalf("initialize Linux desktop backend: %v", err)
	}
	defer controller.Close()

	title := fmt.Sprintf("Morde T07 no-WM %d", time.Now().UnixNano())
	const uiScript = `import sys, tkinter as tk
root = tk.Tk()
root.title(sys.argv[1])
root.geometry("640x220+60+60")
tk.Entry(root, width=44).pack(pady=98)
root.mainloop()
`
	app := exec.Command("python3", "-c", uiScript, title)
	app.Stdout, app.Stderr = io.Discard, io.Discard
	if err := app.Start(); err != nil {
		t.Fatalf("start Tkinter smoke window: %v", err)
	}
	t.Cleanup(func() {
		_ = app.Process.Kill()
		_ = app.Wait()
	})

	var windows []window
	waitFor(t, 10*time.Second, "Tkinter window to become visible", func() bool {
		var err error
		windows, err = liveDesktopWindows(controller)
		return err == nil && findWindow(windows, title) != nil
	})
	target := findWindow(windows, title)
	if target == nil {
		t.Fatal("Tkinter window not found")
	}
	backend := controller.backend.(*linuxBackend)
	info, err := backend.run(context.Background(), backend.paths.xwininfo, 4<<10, "-id", target.ID)
	if err != nil {
		t.Fatalf("read X11 window geometry: %v", err)
	}
	want, err := parseXWindowInfoGeometry(info)
	if err != nil || target.X != want.x || target.Y != want.y || target.Width != want.width || target.Height != want.height {
		t.Fatalf("window list geometry=%#v; xwininfo=%#v err=%v", target, want, err)
	}
	if _, err := liveDesktopAction(controller, "desktop.observe", "screenshot", `{}`); err != nil {
		t.Fatalf("capture no-WM screenshot: %v", err)
	}
}

func liveDesktopAction(controller *Controller, capability, action, arguments string) (json.RawMessage, error) {
	for _, handler := range controller.Handlers() {
		if handler.Capability() == capability && handler.Action() == action {
			return handler.Execute(context.Background(), contract.ExecuteRequest{
				OperationID: "o0000001", TaskID: "q0000001", Capability: capability, Action: action, Arguments: json.RawMessage(arguments),
			})
		}
	}
	return nil, fmt.Errorf("handler %s/%s not found", capability, action)
}

func waitForActiveWindow(t *testing.T, controller *Controller, title string) *window {
	t.Helper()
	var active *window
	waitFor(t, 5*time.Second, "window manager to activate "+title, func() bool {
		windows, err := liveDesktopWindows(controller)
		if err != nil {
			return false
		}
		active = findWindow(windows, title)
		return active != nil && active.Active
	})
	return active
}

func liveDesktopWindows(controller *Controller) ([]window, error) {
	result, err := liveDesktopAction(controller, "window.list", "list", `{}`)
	if err != nil {
		return nil, err
	}
	var response struct {
		Windows []window `json:"windows"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return nil, err
	}
	return response.Windows, nil
}

func findWindow(windows []window, title string) *window {
	for index := range windows {
		if windows[index].Title == title {
			return &windows[index]
		}
	}
	return nil
}

func waitFor(t *testing.T, timeout time.Duration, description string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func jsonObject(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
