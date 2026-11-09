//go:build linux

package runtime_desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"morde-runtime/runtimecore"
)

func TestLinuxClickRejectsWindowThatLostFocusBeforeDispatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xdotool")
	script := "#!/bin/sh\nif [ \"$1\" = \"getactivewindow\" ]; then printf '43\\n'; exit 0; fi\nexit 99\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	backend := &linuxBackend{displayName: ":9", paths: executablePaths{xdotool: path}}
	if err := backend.click(context.Background(), "0x2a", 5, 6, "left"); !errors.Is(err, errDesktopFocusChanged) {
		t.Fatalf("click with changed focus error = %v, want focus changed", err)
	}
}

func TestLinuxDesktopInputRemainsWindowTargetedIfFocusChangesBeforeDispatch(t *testing.T) {
	tests := []struct {
		name string
		args []string
		call func(*linuxBackend) error
	}{
		{
			name: "type",
			args: []string{"type", "--window", "42", "--clearmodifiers", "--delay", "0", "--", "中文 hi"},
			call: func(backend *linuxBackend) error { return backend.typeText(context.Background(), "0x2a", "中文 hi") },
		},
		{
			name: "key",
			args: []string{"key", "--window", "42", "--clearmodifiers", "Return"},
			call: func(backend *linuxBackend) error { return backend.pressKey(context.Background(), "0x2a", "Enter") },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "xdotool")
			focusPath := filepath.Join(t.TempDir(), "active-window")
			script := "#!/bin/sh\nfocus=" + shellQuote(focusPath) + "\n" +
				"if [ \"$1\" = \"getactivewindow\" ]; then printf '42\\n'; exit 0; fi\n" +
				"# Model another window taking focus after validation but before dispatch.\n" +
				"printf '43\\n' > \"$focus\"\n"
			for index, arg := range test.args {
				script += fmt.Sprintf("[ \"${%d}\" = %s ] || exit %d\n", index+1, shellQuote(arg), 80+index)
			}
			script += "[ \"$(cat \"$focus\")\" = 43 ] || exit 82\n"
			script += fmt.Sprintf("[ \"$#\" -eq %d ] || exit 81\n", len(test.args))
			if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			backend := &linuxBackend{displayName: ":9", paths: executablePaths{xdotool: path}}
			if err := test.call(backend); err != nil {
				t.Fatalf("targeted %s returned error: %v", test.name, err)
			}
		})
	}
}

func TestLinuxClickVerifiesPointerPositionBeforeDispatch(t *testing.T) {
	tests := []struct {
		name        string
		pointer     string
		wantErr     error
		wantClicked bool
	}{
		{name: "clicks after pointer arrives", pointer: "X=115\nY=125\nSCREEN=0\nWINDOW=42\n", wantClicked: true},
		{name: "accepts pointer in nested target child", pointer: "X=115\nY=125\nSCREEN=0\nWINDOW=45\n", wantClicked: true},
		{name: "refuses when pointer is constrained", pointer: "X=116\nY=125\nSCREEN=0\nWINDOW=42\n", wantErr: runtimecore.ErrDesktopGeometryChanged},
		{name: "refuses pointer over unrelated window", pointer: "X=115\nY=125\nSCREEN=0\nWINDOW=43\n", wantErr: runtimecore.ErrDesktopGeometryChanged},
		{name: "refuses malformed location", pointer: "X=115\nSCREEN=0\n", wantErr: ErrDesktopUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "xdotool")
			xwininfoPath := filepath.Join(t.TempDir(), "xwininfo")
			clickMarker := filepath.Join(t.TempDir(), "click-dispatched")
			script := "#!/bin/sh\ncase \"$1\" in\n" +
				"getactivewindow) printf '42\\n' ;;\n" +
				"mousemove) [ \"$#\" -eq 5 ] && [ \"$2\" = --window ] && [ \"$3\" = 42 ] && [ \"$4\" = 15 ] && [ \"$5\" = 25 ] || exit 81 ;;\n" +
				"getmouselocation) printf " + shellQuote(test.pointer) + " ;;\n" +
				"click) [ \"$#\" -eq 2 ] && [ \"$2\" = 1 ] || exit 83; : > " + shellQuote(clickMarker) + " ;;\n" +
				"*) exit 84 ;;\nesac\n"
			if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			xwininfoScript := "#!/bin/sh\n[ \"$1\" = -id ] || exit 82\ncase \"$2\" in\n" +
				"0x2a) if [ \"$3\" = -tree ]; then printf 'Parent window id: 0x519 (the root window)\\n'; else printf 'Absolute upper-left X: 100\\nAbsolute upper-left Y: 100\\nRelative upper-left X: 1\\nRelative upper-left Y: 20\\nWidth: 800\\nHeight: 600\\n'; fi ;;\n" +
				"0x2b) printf 'Parent window id: 0x519 (the root window)\\n' ;;\n" +
				"0x2c) printf 'Parent window id: 0x2a (target window)\\n' ;;\n" +
				"0x2d) printf 'Parent window id: 0x2c (target child)\\n' ;;\n" +
				"*) exit 82 ;;\nesac\n"
			if err := os.WriteFile(xwininfoPath, []byte(xwininfoScript), 0o700); err != nil {
				t.Fatal(err)
			}
			backend := &linuxBackend{displayName: ":9", paths: executablePaths{xdotool: path, xwininfo: xwininfoPath}}
			err := backend.click(context.Background(), "0x2a", 15, 25, "left")
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("click error = %v, want %v", err, test.wantErr)
			}
			_, markerErr := os.Stat(clickMarker)
			clicked := markerErr == nil
			if clicked != test.wantClicked {
				t.Fatalf("click dispatched = %v, want %v", clicked, test.wantClicked)
			}
		})
	}
}

func TestParseXWindowInfoGeometryUsesAbsoluteClientOrigin(t *testing.T) {
	geometry, err := parseXWindowInfoGeometry([]byte("Absolute upper-left X: 61\nAbsolute upper-left Y: 80\nRelative upper-left X: 1\nRelative upper-left Y: 20\nWidth: 640\nHeight: 220\n"))
	if err != nil || geometry != (windowGeometry{x: 61, y: 80, width: 640, height: 220}) {
		t.Fatalf("geometry=%#v err=%v", geometry, err)
	}
	for _, output := range []string{
		"Absolute upper-left X: 61\nAbsolute upper-left Y: 80\nWidth: 640\n",
		"Absolute upper-left X: invalid\nAbsolute upper-left Y: 80\nWidth: 640\nHeight: 220\n",
		"Absolute upper-left X: 61\nAbsolute upper-left Y: 80\nWidth: 0\nHeight: 220\n",
	} {
		if _, err := parseXWindowInfoGeometry([]byte(output)); !errors.Is(err, ErrDesktopUnavailable) {
			t.Errorf("invalid xwininfo geometry %q returned %v", output, err)
		}
	}
}

func TestLinuxPointerWindowTreeWalkIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xwininfo")
	queriesPath := filepath.Join(t.TempDir(), "queries")
	script := "#!/bin/sh\n[ \"$1\" = -id ] && [ \"$3\" = -tree ] || exit 81\nprintf '%s\\n' \"$2\" >> " + shellQuote(queriesPath) + "\ncase \"$2\" in\n" +
		"0x3d) parent=0x3c ;;\n0x3c) parent=0x3b ;;\n0x3b) parent=0x3a ;;\n0x3a) parent=0x39 ;;\n" +
		"0x39) parent=0x38 ;;\n0x38) parent=0x37 ;;\n0x37) parent=0x36 ;;\n0x36) parent=0x35 ;;\n" +
		"0x35) parent=0x2a ;;\n*) exit 82 ;;\nesac\nprintf 'Parent window id: %s\\n' \"$parent\"\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	backend := &linuxBackend{paths: executablePaths{xwininfo: path}}
	belongs, err := backend.pointerWindowBelongsToTarget(context.Background(), "0x3d", "0x2a", "0x519")
	if err != nil || belongs {
		t.Fatalf("deep pointer window accepted: belongs=%v err=%v", belongs, err)
	}
	queries, err := os.ReadFile(queriesPath)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(queries), "\n"); count != maxPointerWindowDepth {
		t.Fatalf("xwininfo calls=%d, want bound %d", count, maxPointerWindowDepth)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
