//go:build linux

package runtimeactions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"morde-runtime/contract"
)

func TestShellJailIntegration(t *testing.T) {
	if os.Getenv("MORDE_RUNTIME_SHELL_INTEGRATION") != "1" {
		t.Skip("set MORDE_RUNTIME_SHELL_INTEGRATION=1 in the Linux Host image")
	}
	if runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64" {
		t.Fatalf("unsupported integration architecture %q", runtime.GOARCH)
	}
	account, err := user.Lookup("gem")
	if err != nil {
		t.Fatal("Host image must provision the gem user before running the shell jail integration test")
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	workspaceID := "w1234567"
	workspaceRoot := filepath.Join(defaultShellWorkDir, workspaceID)
	if err := os.Mkdir(workspaceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(workspaceRoot)
	if err := os.Chown(workspaceRoot, int(uid), int(gid)); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(workspaceRoot, "morde-jail-probe")
	if err := os.WriteFile(marker, []byte("workspace-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(marker, int(uid), int(gid)); err != nil {
		t.Fatal(err)
	}
	handler, err := NewShellHandler(ShellOptions{})
	if err != nil {
		t.Fatalf("Host image did not pass the real shell jail startup probe: %v", err)
	}
	script := fmt.Sprintf(`set -eu
test "$(id -u)" = "%d"
test "$PWD" = "/home/gem/workspace/%s"
test "$(cat morde-jail-probe)" = workspace-only
test ! -r /run/morde-runtime/credential
test ! -e /var/lib/morde-runtime
test ! -e /root
test ! -e ../w7654321/secret
echo shell-jail-ok`, uid, workspaceID)
	arguments, err := json.Marshal(map[string]any{"command": script, "timeout": 10})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := handler.Execute(context.Background(), contract.ExecuteRequest{
		TaskID: "t1234567", WorkspaceID: workspaceID, WorkspaceDirectory: workspaceID,
		BindingVersion: 1, OperationID: "o1234567", LeaseVersion: 1,
		Capability: shellCapability, Action: shellAction, Arguments: arguments,
	})
	if err != nil {
		t.Fatalf("isolated shell execution failed: %v", err)
	}
	var result shellResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.TimedOut || !strings.Contains(result.Stdout, "shell-jail-ok") {
		t.Fatalf("isolated shell result = %+v", result)
	}
}
