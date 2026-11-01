package runtimeapplications

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"morde-runtime/contract"
)

func TestProbeMissingCodexIsNotApplicable(t *testing.T) {
	called := false
	probe := codexProbe{
		binaryPath: filepath.Join(t.TempDir(), "codex"),
		timeout:    time.Second,
		run: func(context.Context, commandSpec) commandOutcome {
			called = true
			return commandOutcome{}
		},
	}

	got := probe.probe(context.Background())
	if got != (contract.ApplicationStatusItem{ID: "codex", LoginStatus: contract.ApplicationLoginNotApplicable}) {
		t.Fatalf("missing Codex item = %#v", got)
	}
	if called {
		t.Fatal("runner was called for missing Codex binary")
	}
}

func TestProbeCodexUsesFixedBoundedCommandsAndSanitizesOutput(t *testing.T) {
	binaryPath := makeExecutable(t)
	for _, test := range []struct {
		name        string
		loginExit   int
		loginStderr string
		wantStatus  contract.ApplicationLoginStatus
	}{
		{name: "logged in", loginExit: 0, wantStatus: contract.ApplicationLoginLoggedIn},
		{name: "logged out", loginExit: 1, loginStderr: "Not logged in\n", wantStatus: contract.ApplicationLoginNotLoggedIn},
		{name: "logged out after known arg0 warning", loginExit: 1, loginStderr: arg0CleanupWarningPrefix + "\nNot logged in\n", wantStatus: contract.ApplicationLoginNotLoggedIn},
		{name: "exit one diagnostic error", loginExit: 1, loginStderr: "could not read auth config\n", wantStatus: contract.ApplicationLoginUnknown},
		{name: "other exit", loginExit: 7, wantStatus: contract.ApplicationLoginUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			probe := codexProbe{
				binaryPath: binaryPath,
				timeout:    time.Second,
				run: func(_ context.Context, spec commandSpec) commandOutcome {
					calls++
					if spec.path != binaryPath || spec.dir != codexHome || spec.uid != 1000 || spec.gid != 1000 {
						t.Errorf("command identity = %#v", spec)
					}
					if !reflect.DeepEqual(spec.env, []string{"HOME=/home/gem", "PATH=/opt/nodejs/22/bin:/usr/bin:/bin", "USER=gem", "LOGNAME=gem"}) {
						t.Errorf("environment = %#v", spec.env)
					}
					switch {
					case reflect.DeepEqual(spec.args, []string{"--version"}):
						if !spec.captureStdout {
							t.Error("version stdout was not captured for validated semver parsing")
						}
						return commandOutcome{stdout: []byte("codex-cli 1.2.3\n"), exitCode: 0}
					case reflect.DeepEqual(spec.args, []string{"login", "status"}):
						if spec.captureStdout {
							t.Error("login stdout must be discarded")
						}
						if !spec.captureStderr {
							t.Error("bounded login status stderr must be captured for exact unauthenticated diagnosis")
						}
						return commandOutcome{stdout: []byte("account-secret token-secret"), stderr: []byte(test.loginStderr), exitCode: test.loginExit}
					default:
						t.Errorf("unexpected command args: %#v", spec.args)
						return commandOutcome{err: context.Canceled}
					}
				},
			}

			got := probe.probe(context.Background())
			if calls != 2 {
				t.Fatalf("runner calls = %d, want version and login probes", calls)
			}
			want := contract.ApplicationStatusItem{ID: "codex", Installed: true, Version: "1.2.3", LoginStatus: test.wantStatus}
			if got != want {
				t.Fatalf("probe item = %#v, want %#v", got, want)
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"account-secret", "token-secret", "stdout", "stderr"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("response leaked %q: %s", secret, encoded)
				}
			}
		})
	}
}

func TestParseLoginStatusRequiresExactUnauthenticatedDiagnostic(t *testing.T) {
	for _, test := range []struct {
		name   string
		result commandOutcome
		want   contract.ApplicationLoginStatus
	}{
		{name: "exact diagnostic", result: commandOutcome{exitCode: 1, stderr: []byte("Not logged in")}, want: contract.ApplicationLoginNotLoggedIn},
		{name: "diagnostic with newline", result: commandOutcome{exitCode: 1, stderr: []byte("Not logged in\n")}, want: contract.ApplicationLoginNotLoggedIn},
		{name: "known arg0 warning before diagnostic", result: commandOutcome{exitCode: 1, stderr: []byte(arg0CleanupWarningPrefix + "\nNot logged in\n")}, want: contract.ApplicationLoginNotLoggedIn},
		{name: "known arg0 warning with extra text", result: commandOutcome{exitCode: 1, stderr: []byte(arg0CleanupWarningPrefix + ": unexpected detail\nNot logged in\n")}, want: contract.ApplicationLoginUnknown},
		{name: "known warning alone", result: commandOutcome{exitCode: 1, stderr: []byte(arg0CleanupWarningPrefix + "\n")}, want: contract.ApplicationLoginUnknown},
		{name: "unexpected stderr line", result: commandOutcome{exitCode: 1, stderr: []byte("WARNING: auth config unavailable\nNot logged in\n")}, want: contract.ApplicationLoginUnknown},
		{name: "extra stderr line", result: commandOutcome{exitCode: 1, stderr: []byte(arg0CleanupWarningPrefix + "\nNot logged in\nextra\n")}, want: contract.ApplicationLoginUnknown},
		{name: "unexpected stderr on success", result: commandOutcome{exitCode: 0, stderr: []byte("unexpected diagnostic\n")}, want: contract.ApplicationLoginUnknown},
		{name: "configuration failure", result: commandOutcome{exitCode: 1, stderr: []byte("Failed reading credentials")}, want: contract.ApplicationLoginUnknown},
		{name: "truncated diagnostic", result: commandOutcome{exitCode: 1, stderr: []byte("Not logged in"), stderrTruncated: true}, want: contract.ApplicationLoginUnknown},
		{name: "runner failure", result: commandOutcome{exitCode: 1, stderr: []byte("Not logged in"), err: context.DeadlineExceeded}, want: contract.ApplicationLoginUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := parseLoginStatus(test.result); got != test.want {
				t.Fatalf("parseLoginStatus(%#v) = %q, want %q", test.result, got, test.want)
			}
		})
	}
}

func TestProbeCodexTimeoutAndInvalidVersionFailClosed(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		probe := codexProbe{
			binaryPath: makeExecutable(t),
			timeout:    10 * time.Millisecond,
			run: func(ctx context.Context, _ commandSpec) commandOutcome {
				<-ctx.Done()
				return commandOutcome{stdout: []byte("codex-cli 1.2.3\n"), exitCode: 0}
			},
		}
		got := probe.probe(context.Background())
		want := contract.ApplicationStatusItem{ID: "codex", Installed: true, LoginStatus: contract.ApplicationLoginUnknown}
		if got != want {
			t.Fatalf("timed out probe = %#v, want %#v", got, want)
		}
	})

	t.Run("unvalidated version", func(t *testing.T) {
		probe := codexProbe{
			binaryPath: makeExecutable(t),
			timeout:    time.Second,
			run: func(context.Context, commandSpec) commandOutcome {
				return commandOutcome{stdout: []byte("codex-cli 1.2.3 account-secret\n"), exitCode: 0}
			},
		}
		got := probe.probe(context.Background())
		want := contract.ApplicationStatusItem{ID: "codex", Installed: true, LoginStatus: contract.ApplicationLoginUnknown}
		if got != want {
			t.Fatalf("invalid version probe = %#v, want %#v", got, want)
		}
	})
}

func TestParseVersionAcceptsOnlyPlainCodexSemver(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{input: "codex-cli 1.2.3\n", want: "1.2.3"},
		{input: "codex-cli 01.2.3\n"},
		{input: "codex-cli 1.2.3-beta.1\n"},
		{input: "codex-cli 1.2.3 token"},
	} {
		if got := parseVersion([]byte(test.input)); got != test.want {
			t.Errorf("parseVersion(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func makeExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("test executable placeholder"), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}
