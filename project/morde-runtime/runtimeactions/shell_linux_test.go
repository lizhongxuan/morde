//go:build linux

package runtimeactions

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"morde-runtime/contract"
	"morde-runtime/runtimecore"
)

func TestCgroupPopulated(t *testing.T) {
	for _, test := range []struct {
		name      string
		content   string
		populated bool
		wantErr   bool
	}{
		{name: "empty", content: "populated 0\nfrozen 0\n"},
		{name: "live", content: "populated 1\nfrozen 0\n", populated: true},
		{name: "missing field", content: "frozen 0\n", wantErr: true},
		{name: "invalid value", content: "populated 2\n", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cgroup.events")
			if err := os.WriteFile(path, []byte(test.content), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := cgroupPopulated(path)
			if (err != nil) != test.wantErr || got != test.populated {
				t.Fatalf("cgroupPopulated()=(%v,%v), want (%v,error=%v)", got, err, test.populated, test.wantErr)
			}
		})
	}
}

func TestCreateOperationGroupFailsClosedOnOrdinaryFilesystem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operation")
	if err := createOperationGroup(path); err == nil {
		t.Fatal("created an operation domain without cgroup v2 controls")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("partial operation domain remains after failure: %v", err)
	}
}

func TestWasKilledBySIGKILL(t *testing.T) {
	err := exec.Command("/bin/sh", "-c", "kill -KILL $$").Run()
	if !wasKilledBySIGKILL(err) {
		t.Fatalf("SIGKILL process error %v was not recognized", err)
	}
	if wasKilledBySIGKILL(errors.New("unrelated failure")) {
		t.Fatal("non-process error was treated as SIGKILL")
	}
}

func TestBubblewrapBaseArgumentsNeverExposeHostRoot(t *testing.T) {
	args, err := bubblewrapBaseArguments()
	if err != nil {
		t.Skipf("host does not have the Linux system layout required for a shell jail: %v", err)
	}
	for index, arg := range args {
		if arg == "--bind" || arg == "--ro-bind" {
			if index+2 >= len(args) {
				t.Fatalf("incomplete bind arguments: %v", args)
			}
			if args[index+1] == string(filepath.Separator) || args[index+2] == string(filepath.Separator) {
				t.Fatalf("bubblewrap configuration exposes the Host root: %v", args[index:index+3])
			}
			if arg == "--bind" {
				t.Fatalf("base filesystem configuration contains a writable bind: %v", args[index:index+3])
			}
		}
	}
	for _, flag := range []string{"--unshare-user", "--unshare-pid", "--unshare-net", "--disable-userns", "--cap-drop"} {
		if !containsArgument(args, flag) {
			t.Errorf("bubblewrap base arguments omit %q: %v", flag, args)
		}
	}
}

func TestBubblewrapWorkspaceBindUsesInheritedDirectoryDescriptor(t *testing.T) {
	workspace, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	command := (&ShellHandler{bwrapPath: "/usr/bin/bwrap"}).command(shellArguments{Command: "true"}, workspace, "/workspace", "/workspace")
	if len(command.ExtraFiles) != 1 || command.ExtraFiles[0] != workspace {
		t.Fatalf("inherited descriptors=%v, want the verified Workspace directory", command.ExtraFiles)
	}
	for index, argument := range command.Args {
		if argument == "--bind" && index+2 < len(command.Args) && command.Args[index+1] == "/proc/self/fd/3" && command.Args[index+2] == "/workspace" {
			return
		}
	}
	t.Fatalf("bubblewrap did not bind from the inherited directory descriptor: %v", command.Args)
}

func TestShellWorkspaceRootRejectsDirectoryReplacement(t *testing.T) {
	parentPath := t.TempDir()
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	workspacePath := filepath.Join(parentPath, "w1234567")
	siblingPath := filepath.Join(parentPath, "wsibling")
	replacedPath := filepath.Join(parentPath, "replaced")
	for _, path := range []string{workspacePath, siblingPath} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := parent.Lstat("w1234567")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(workspacePath, replacedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(siblingPath, workspacePath); err != nil {
		t.Fatal(err)
	}
	if _, err := openShellWorkspaceRoot(parent, "w1234567", expected); !errors.Is(err, ErrInvalidShellRequest) {
		t.Fatalf("opening replaced Workspace error=%v, want ErrInvalidShellRequest", err)
	}
}

func TestEnsureWorkspaceDirectoryChangesOwnershipThroughOpenedRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to assign the Workspace group")
	}
	parent, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	handler := &ShellHandler{workspaceRoot: parent, credential: &syscall.Credential{Uid: 1000, Gid: 1000}}
	workspace, err := handler.ensureWorkspaceDirectory("w1234567")
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	info, err := workspace.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || stat.Gid != 1000 || info.Mode().Perm() != 0o770 || info.Mode()&os.ModeSetgid == 0 {
		t.Fatalf("Workspace ownership/mode=%v, want root:1000 2770", info)
	}
}

func TestAppendBubblewrapReadonlySystemPathRejectsRoot(t *testing.T) {
	if _, err := appendBubblewrapReadonlySystemPath(nil, string(filepath.Separator), "/host-root", true); err == nil {
		t.Fatal("accepted Host root as a system mount")
	}
}

func containsArgument(args []string, target string) bool {
	for _, arg := range args {
		if arg == target {
			return true
		}
	}
	return false
}

func TestGemCredentialClearsSupplementaryGroups(t *testing.T) {
	credential, err := gemCredentialFromIDs("1000", "1001")
	if err != nil {
		t.Fatal(err)
	}
	if credential.Uid != 1000 || credential.Gid != 1001 {
		t.Fatalf("credential IDs=(%d,%d), want (1000,1001)", credential.Uid, credential.Gid)
	}
	if credential.Groups == nil || len(credential.Groups) != 0 {
		t.Fatalf("supplementary groups=%v (nil=%v), want explicit empty slice", credential.Groups, credential.Groups == nil)
	}
	if credential.NoSetGroups {
		t.Fatal("NoSetGroups=true would preserve inherited supplementary groups")
	}
	for _, test := range [][2]string{{"0", "1001"}, {"1000", "0"}, {"bad", "1001"}} {
		if _, err := gemCredentialFromIDs(test[0], test[1]); err == nil {
			t.Errorf("gemCredentialFromIDs(%q,%q) accepted invalid or privileged IDs", test[0], test[1])
		}
	}
}

func TestStopBeforeExecuteConsumesTombstoneAndRejectsReplay(t *testing.T) {
	handler := stateTestHandler(t)
	const id = "oabc1234"
	if _, tracked := handler.requestStop(id); !tracked {
		t.Fatal("failed to retain Stop-before-Execute tombstone")
	}
	if err := handler.beginOperation(id); !errors.Is(err, ErrShellOperationCancelled) {
		t.Fatalf("Execute after Stop error=%v, want cancellation", err)
	}
	if got := handler.operations[id].phase; got != operationComplete {
		t.Fatalf("consumed tombstone phase=%v, want complete", got)
	}
	if err := handler.beginOperation(id); !errors.Is(err, runtimecore.ErrOperationConflict) {
		t.Fatalf("duplicate Execute error=%v, want operation conflict", err)
	}
}

func TestCompletedOperationIDRejectedOnReplay(t *testing.T) {
	handler := stateTestHandler(t)
	const id = "oabc1234"
	if err := handler.beginOperation(id); err != nil {
		t.Fatal(err)
	}
	handler.completeOperation(id)
	if err := handler.beginOperation(id); !errors.Is(err, runtimecore.ErrOperationConflict) {
		t.Fatalf("replayed completed ID error=%v, want operation conflict", err)
	}
}

func TestOperationLedgerCapacityFailsClosed(t *testing.T) {
	handler := stateTestHandler(t)
	for index := 0; index < maxTrackedOperations; index++ {
		handler.operations[strconv.Itoa(index)] = operationRecord{phase: operationComplete}
	}
	if err := handler.beginOperation("oabc1234"); !errors.Is(err, ErrProcessDomainUnavailable) {
		t.Fatalf("beginOperation at capacity error=%v, want fail-closed error", err)
	}
	if _, tracked := handler.requestStop("oabc1234"); tracked {
		t.Fatal("unknown Stop was retained after the bounded ledger filled")
	}
	if len(handler.operations) != maxTrackedOperations {
		t.Fatalf("ledger size=%d, want fixed cap %d", len(handler.operations), maxTrackedOperations)
	}
}

func TestConcurrentStopAndExecuteCannotLeaveOperationLaunchable(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		handler := stateTestHandler(t)
		id := "oabc1234"
		start := make(chan struct{})
		var beginErr error
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			beginErr = handler.beginOperation(id)
		}()
		go func() {
			defer wait.Done()
			<-start
			handler.requestStop(id)
		}()
		close(start)
		wait.Wait()
		if beginErr != nil && !errors.Is(beginErr, ErrShellOperationCancelled) {
			t.Fatalf("beginOperation error=%v, want nil or cancellation", beginErr)
		}
		handler.mu.Lock()
		mayStart := handler.operationMayStartLocked(id)
		handler.mu.Unlock()
		if mayStart {
			t.Fatal("concurrent Stop left the operation launchable")
		}
	}
}

func TestStopDoesNotClaimVerificationWithoutCgroupProof(t *testing.T) {
	handler := &ShellHandler{
		basePath: t.TempDir(), pollInterval: time.Millisecond,
		operations: make(map[string]operationRecord),
	}
	result, err := handler.Stop(context.Background(), contract.Operation{
		OperationID: "oabc1234", Capability: shellCapability, Action: shellAction,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.VerifiedStopped || len(result.PendingIDs) != 1 || result.PendingIDs[0] != "oabc1234" {
		t.Fatalf("Stop result=%+v, want unverified pending operation", result)
	}
}

func stateTestHandler(t *testing.T) *ShellHandler {
	t.Helper()
	return &ShellHandler{basePath: t.TempDir(), operations: make(map[string]operationRecord)}
}
