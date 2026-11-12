package runtimeactions

import (
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseShellArguments(t *testing.T) {
	tests := []struct {
		name        string
		arguments   string
		wantTimeout time.Duration
		wantErr     bool
	}{
		{name: "defaults", arguments: `{"command":"printf ok"}`, wantTimeout: defaultTimeout},
		{name: "explicit timeout", arguments: `{"command":"true","timeout":12}`, wantTimeout: 12 * time.Second},
		{name: "cwd", arguments: `{"command":"pwd","cwd":"/home/gem/workspace","timeout":1}`, wantTimeout: time.Second},
		{name: "unknown field", arguments: `{"command":"true","env":{"X":"y"}}`, wantErr: true},
		{name: "relative cwd", arguments: `{"command":"true","cwd":"workspace"}`, wantErr: true},
		{name: "empty command", arguments: `{"command":"  "}`, wantErr: true},
		{name: "nul command", arguments: `{"command":"true\u0000false"}`, wantErr: true},
		{name: "zero timeout", arguments: `{"command":"true","timeout":0}`, wantErr: true},
		{name: "null timeout", arguments: `{"command":"true","timeout":null}`, wantErr: true},
		{name: "timeout over cap", arguments: `{"command":"true","timeout":301}`, wantErr: true},
		{name: "malformed json", arguments: `{"command":`, wantErr: true},
		{name: "trailing json", arguments: `{"command":"true"}{}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args, timeout, err := parseShellArguments(json.RawMessage(test.arguments), maxShellTimeout)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseShellArguments error=%v, wantErr=%v", err, test.wantErr)
			}
			if err == nil {
				if args.Command == "" {
					t.Fatal("accepted empty command")
				}
				if timeout != test.wantTimeout {
					t.Fatalf("timeout=%s, want %s", timeout, test.wantTimeout)
				}
			}
		})
	}
}

func TestParseShellArgumentsBounds(t *testing.T) {
	command := strings.Repeat("x", maxCommandBytes+1)
	arguments, err := json.Marshal(shellArguments{Command: command})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := parseShellArguments(arguments, maxShellTimeout); err == nil {
		t.Fatal("accepted oversized command")
	}
	if _, _, err := parseShellArguments(json.RawMessage(`{"command":"true","timeout":9999999999999999999}`), maxShellTimeout); err == nil {
		t.Fatal("accepted overflowing timeout")
	}
}

func TestValidOperationID(t *testing.T) {
	for _, test := range []struct {
		id   string
		want bool
	}{
		{id: "o_abcdef", want: false},
		{id: "oabc1234", want: true},
		{id: "oabc123", want: false},
		{id: "oabc123/", want: false},
		{id: "Oabc1234", want: false},
	} {
		if got := validOperationID(test.id); got != test.want {
			t.Errorf("validOperationID(%q)=%v, want %v", test.id, got, test.want)
		}
	}
}

func TestBoundedBufferRetainsPrefixAndDrainsInput(t *testing.T) {
	buffer := &boundedBuffer{limit: 4}
	if n, err := buffer.Write([]byte("abcd")); n != 4 || err != nil {
		t.Fatalf("first Write=(%d,%v)", n, err)
	}
	if buffer.truncated {
		t.Fatal("buffer truncated an exact fit")
	}
	if n, err := buffer.Write([]byte("ef")); n != 2 || err != nil {
		t.Fatalf("second Write=(%d,%v)", n, err)
	}
	if got := buffer.String(); got != "abcd" {
		t.Fatalf("buffer=%q, want prefix %q", got, "abcd")
	}
	if !buffer.truncated {
		t.Fatal("buffer did not report truncated output")
	}
}

func TestStdoutAndStderrShareOneOutputLimit(t *testing.T) {
	budget := &outputBudget{limit: 5}
	stdout := &boundedBuffer{limit: 5, budget: budget}
	stderr := &boundedBuffer{limit: 5, budget: budget}
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		_, _ = stdout.Write([]byte("stdout"))
	}()
	go func() {
		defer wait.Done()
		_, _ = stderr.Write([]byte("stderr"))
	}()
	wait.Wait()
	if retained := len(stdout.String()) + len(stderr.String()); retained != 5 {
		t.Fatalf("combined retained output=%d bytes, want limit 5", retained)
	}
	if budget.used != 5 {
		t.Fatalf("shared budget used=%d, want 5", budget.used)
	}
	if !stdout.truncated && !stderr.truncated {
		t.Fatal("neither output stream reported truncation")
	}
}

func TestNewShellHandlerFailsClosedWithoutDelegatedCgroup(t *testing.T) {
	handler, err := NewShellHandler(ShellOptions{CgroupRoot: t.TempDir()})
	if err == nil || handler != nil {
		t.Fatalf("NewShellHandler()=(%v,%v), want fail-closed error", handler, err)
	}
	want := ShellFailureOther
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		want = ShellFailureCgroup
	}
	if got := ShellFailureCategoryOf(err); got != want {
		t.Fatalf("ShellFailureCategoryOf(NewShellHandler error)=%q, want %q", got, want)
	}
}

func TestShellFailureCategoryOfReturnsOnlyStaticCategories(t *testing.T) {
	for _, category := range []ShellFailureCategory{
		ShellFailureCgroup, ShellFailureRequiredTool, ShellFailureWorkspaceProbe,
	} {
		err := shellStartupUnavailable(category)
		if got := ShellFailureCategoryOf(err); got != category {
			t.Errorf("ShellFailureCategoryOf(%v)=%q, want %q", err, got, category)
		}
		if !errors.Is(err, ErrProcessDomainUnavailable) {
			t.Errorf("categorized error %q no longer matches ErrProcessDomainUnavailable", category)
		}
	}
	if got := ShellFailureCategoryOf(shellStartupUnavailable(ShellFailureCategory("secret/path"))); got != ShellFailureOther {
		t.Fatalf("invalid category=%q, want %q", got, ShellFailureOther)
	}
	if got := ShellFailureCategoryOf(errors.New("secret/path")); got != ShellFailureOther {
		t.Fatalf("raw error category=%q, want %q", got, ShellFailureOther)
	}
}
