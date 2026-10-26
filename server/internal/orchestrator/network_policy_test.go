package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
)

func TestCheckNetworkPolicyRunsFixedCheckCommand(t *testing.T) {
	t.Setenv("CH_OSB_URL", "http://attacker.example:9000")
	path := writeNetworkGuard(t, "#!/bin/sh\n[ \"$#\" = 1 ] && [ \"$1\" = check ] && [ \"$CH_OSB_URL\" = http://127.0.0.1:18080 ]\n")
	if err := CheckNetworkPolicy(context.Background(), path, "http://127.0.0.1:18080"); err != nil {
		t.Fatalf("CheckNetworkPolicy() = %v", err)
	}
}

func TestCheckNetworkPolicyFailsClosed(t *testing.T) {
	temp := t.TempDir()
	nonExecutable := filepath.Join(temp, "not-executable.sh")
	if err := os.WriteFile(nonExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "relative.sh", filepath.Join(temp, "missing.sh"), temp, nonExecutable} {
		t.Run(path, func(t *testing.T) {
			if err := CheckNetworkPolicy(context.Background(), path, "http://127.0.0.1:18080"); !errors.Is(err, app.ErrSandboxUnavailable) {
				t.Fatalf("CheckNetworkPolicy(%q) = %v, want ErrSandboxUnavailable", path, err)
			}
		})
	}

	failing := writeNetworkGuard(t, "#!/bin/sh\necho guard-output\necho guard-error >&2\nexit 7\n")
	err := CheckNetworkPolicy(context.Background(), failing, "http://127.0.0.1:18080")
	if !errors.Is(err, app.ErrSandboxUnavailable) || strings.Contains(err.Error(), "guard-output") || strings.Contains(err.Error(), "guard-error") {
		t.Fatalf("failed guard error = %v, want safe ErrSandboxUnavailable", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CheckNetworkPolicy(canceled, writeNetworkGuard(t, "#!/bin/sh\nexit 0\n"), "http://127.0.0.1:18080"); !errors.Is(err, app.ErrSandboxUnavailable) {
		t.Fatalf("canceled guard error = %v, want ErrSandboxUnavailable", err)
	}

	ctx, stop := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer stop()
	started := time.Now()
	err = CheckNetworkPolicy(ctx, writeNetworkGuard(t, "#!/bin/sh\nexec sleep 2\n"), "http://127.0.0.1:18080")
	if !errors.Is(err, app.ErrSandboxUnavailable) {
		t.Fatalf("timed out guard error = %v, want ErrSandboxUnavailable", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("guard did not stop promptly after context timeout")
	}
}

func writeNetworkGuard(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "network-policy.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
