//go:build linux

package runtime_desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
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
