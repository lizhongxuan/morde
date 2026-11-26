//go:build linux

package runtimeactions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

func TestTaskWorkspaceManifestSHA256IsStableAndTracksMutation(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "src"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "src", "main.go"), []byte("package main\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	first, err := taskWorkspaceManifestSHA256(context.Background(), root)
	if err != nil {
		t.Fatalf("first manifest digest: %v", err)
	}
	second, err := taskWorkspaceManifestSHA256(context.Background(), root)
	if err != nil {
		t.Fatalf("second manifest digest: %v", err)
	}
	if first == "" || first != second {
		t.Fatalf("stable Workspace digests differ: %q != %q", first, second)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "src", "main.go"), []byte("package main\nfunc main() {}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	changed, err := taskWorkspaceManifestSHA256(context.Background(), root)
	if err != nil {
		t.Fatalf("changed manifest digest: %v", err)
	}
	if changed == first {
		t.Fatal("Workspace content mutation did not change the manifest digest")
	}
}

func TestTaskWorkspaceManifestSHA256FailsClosedOnSpecialFile(t *testing.T) {
	rootPath := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(rootPath, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if digest, err := taskWorkspaceManifestSHA256(context.Background(), root); err == nil || digest != "" {
		t.Fatalf("special file produced a manifest proof: digest=%q err=%v", digest, err)
	}
}

func TestTaskWorkspaceManifestSHA256FailsClosedOnScanError(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if digest, err := taskWorkspaceManifestSHA256(context.Background(), root); err == nil || digest != "" {
		t.Fatalf("closed root produced a manifest proof: digest=%q err=%v", digest, err)
	}
}

func TestShellTaskWorkspaceResultPreservesOutputAndSnapshotIdentity(t *testing.T) {
	revision := int64(0)
	result := shellTaskWorkspaceResult{
		ExitCode: 7, Stdout: "stdout", Stderr: "stderr", StdoutTruncated: true, StderrTruncated: false, TimedOut: true,
		TaskID: "q1234567", WorkspaceID: "w1234567", BindingVersion: 3, WorkspaceViewID: "v" + strings.Repeat("a", 64),
		WorkspaceCheckpointRevision:   &revision,
		WorkspaceManifestSHA256Before: strings.Repeat("b", 64), WorkspaceManifestSHA256After: strings.Repeat("c", 64),
		WorkspaceSnapshotSHA256Before: strings.Repeat("d", 64), WorkspaceSnapshotSHA256After: strings.Repeat("e", 64),
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"exit_code": float64(7), "stdout": "stdout", "stderr": "stderr", "stdout_truncated": true,
		"stderr_truncated": false, "timed_out": true, "task_id": "q1234567", "workspace_id": "w1234567",
		"binding_version": float64(3), "workspace_view_id": "v" + strings.Repeat("a", 64),
		"workspace_checkpoint_revision": float64(0), "workspace_manifest_sha256_before": strings.Repeat("b", 64),
		"workspace_manifest_sha256_after":  strings.Repeat("c", 64),
		"workspace_snapshot_sha256_before": strings.Repeat("d", 64),
		"workspace_snapshot_sha256_after":  strings.Repeat("e", 64),
	} {
		if got[key] != want {
			t.Errorf("result field %q = %#v, want %#v", key, got[key], want)
		}
	}
	legacyEncoded, err := json.Marshal(shellResult{ExitCode: 0, Stdout: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(legacyEncoded, &legacy); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"task_id", "workspace_id", "binding_version", "workspace_view_id", "workspace_checkpoint_revision",
		"workspace_manifest_sha256_before", "workspace_manifest_sha256_after",
		"workspace_snapshot_sha256_before", "workspace_snapshot_sha256_after",
	} {
		if _, ok := legacy[key]; ok {
			t.Errorf("non-Task shell result unexpectedly includes %q", key)
		}
	}
}

func TestGoTestASTInventoryMatchesOneStructuredExecutionWithoutListingSideEffects(t *testing.T) {
	workspacePath := t.TempDir()
	packagePath := "example.test/pkg/one"
	noTestPackagePath := "example.test/pkg/empty"
	if err := os.MkdirAll(filepath.Join(workspacePath, "pkg", "one"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspacePath, "pkg", "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"pkg/one/one_test.go":      "package one\nimport \"testing\"\nfunc TestAlpha(t *testing.T) {}\n",
		"pkg/one/external_test.go": "package one_test\nimport check \"testing\"\nfunc TestExternal(t *check.T) {}\n",
	} {
		if err := os.WriteFile(filepath.Join(workspacePath, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	listed := strings.Join([]string{
		`{"ImportPath":"` + packagePath + `","Dir":"` + filepath.Join("/workspace", "pkg/one") + `","Name":"one","TestGoFiles":["one_test.go"],"XTestGoFiles":["external_test.go"]}`,
		`{"ImportPath":"` + noTestPackagePath + `","Dir":"` + filepath.Join("/workspace", "pkg/empty") + `","Name":"empty"}`,
	}, "\n")
	source, _, _, ok := parseGoSourceInventory(listed, "/workspace", "/workspace")
	if !ok || len(source.all) != 2 || len(source.testBearing) != 1 {
		t.Fatalf("source inventory=(%+v,%v)", source, ok)
	}
	discovered, sourceDigest, sourceCount, ok := scanGoTestSourceFiles(root, source)
	if !ok || sourceDigest == "" || sourceCount != 2 || len(flattenGoTestInventory(discovered)) != 2 {
		t.Fatalf("AST inventory=(%v,%q,%d,%v)", discovered, sourceDigest, sourceCount, ok)
	}

	runEvents := []goTestEvent{
		{Action: "start", Package: packagePath},
		{Action: "run", Package: packagePath, Test: "TestAlpha"},
		{Action: "run", Package: packagePath, Test: "TestAlpha/sub"},
		{Action: "pass", Package: packagePath, Test: "TestAlpha/sub"},
		{Action: "pass", Package: packagePath, Test: "TestAlpha"},
		{Action: "run", Package: packagePath, Test: "TestExternal"},
		{Action: "pass", Package: packagePath, Test: "TestExternal"},
		{Action: "output", Package: packagePath, Output: "PASS\n"},
		{Action: "output", Package: packagePath, Output: "ok  \t" + packagePath + "\t0.001s\n"},
		{Action: "pass", Package: packagePath},
		{Action: "start", Package: noTestPackagePath},
		{Action: "output", Package: noTestPackagePath, Output: "? " + noTestPackagePath + " [no test files]\n"},
		{Action: "skip", Package: noTestPackagePath},
	}
	executed, executedPackages, stats, ok := parseGoTestRun(encodeGoTestEvents(t, runEvents), discovered, source)
	if !ok || stats.passed != 3 || stats.failed != 0 || stats.skipped != 0 || len(executedPackages) != 1 || !executedPackages[packagePath] {
		t.Fatalf("execution inventory=(%v,%v,%+v,%v)", executed, executedPackages, stats, ok)
	}
	if !sameStringSet(flattenGoTestInventory(discovered), flattenGoTestInventory(executed)) {
		t.Fatalf("discovered roots %v do not match executed roots %v", discovered, executed)
	}
}

func TestGoTestASTInventoryFailsClosedForUnsupportedDefinitions(t *testing.T) {
	for _, sourceText := range []string{
		"package pkg\nimport \"testing\"\nfunc TestAlpha() {}\n",
		"package pkg\nfunc ExampleAlpha() {}\n",
		"package pkg\nimport \"testing\"\nfunc TestMain(m *testing.M) {}\n",
	} {
		t.Run(strings.ReplaceAll(sourceText, "\n", "_"), func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "pkg_test.go"), []byte(sourceText), 0o600); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(workspace)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			inventory := goSourceInventory{
				all: map[string]goPackageInventory{"example.test/pkg": {
					importPath: "example.test/pkg", relativeDir: ".", testGoFiles: []string{"pkg_test.go"},
				}},
				testBearing: map[string]bool{"example.test/pkg": true},
			}
			if tests, _, _, ok := scanGoTestSourceFiles(root, inventory); ok {
				t.Fatalf("unsupported source unexpectedly produced inventory: %v", tests)
			}
		})
	}
}

func TestGoTestRunParserFailsClosedOnIncompleteOrSkippedEvents(t *testing.T) {
	packagePath := "example.test/pkg"
	source := goSourceInventory{
		all: map[string]goPackageInventory{packagePath: {
			importPath: packagePath, relativeDir: ".", testGoFiles: []string{"pkg_test.go"},
		}},
		testBearing: map[string]bool{packagePath: true},
	}
	discovered := map[string]map[string]bool{packagePath: {"TestAlpha": true}}
	for _, test := range []struct {
		name   string
		output string
	}{
		{name: "missing terminal", output: encodeGoTestEvents(t, []goTestEvent{
			{Action: "start", Package: packagePath}, {Action: "run", Package: packagePath, Test: "TestAlpha"}, {Action: "pass", Package: packagePath},
		})},
		{name: "unknown event", output: encodeGoTestEvents(t, []goTestEvent{
			{Action: "start", Package: packagePath}, {Action: "bogus", Package: packagePath},
		})},
		{name: "malformed trailing json", output: encodeGoTestEvents(t, []goTestEvent{
			{Action: "start", Package: packagePath}, {Action: "run", Package: packagePath, Test: "TestAlpha"},
			{Action: "pass", Package: packagePath, Test: "TestAlpha"}, {Action: "pass", Package: packagePath},
		}) + `{"Action":`},
		{name: "test before package start", output: encodeGoTestEvents(t, []goTestEvent{
			{Action: "run", Package: packagePath, Test: "TestAlpha"},
		})},
		{name: "event after package terminal", output: encodeGoTestEvents(t, []goTestEvent{
			{Action: "start", Package: packagePath}, {Action: "run", Package: packagePath, Test: "TestAlpha"},
			{Action: "pass", Package: packagePath, Test: "TestAlpha"}, {Action: "pass", Package: packagePath},
			{Action: "output", Package: packagePath, Output: "PASS\n"},
		})},
		{name: "duplicate package start", output: encodeGoTestEvents(t, []goTestEvent{
			{Action: "start", Package: packagePath}, {Action: "start", Package: packagePath},
		})},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, _, valid := parseGoTestRun(test.output, discovered, source); valid {
				t.Fatal("incomplete runner events produced a valid inventory")
			}
		})
	}

	skipEvents := []goTestEvent{
		{Action: "start", Package: packagePath},
		{Action: "run", Package: packagePath, Test: "TestAlpha"},
		{Action: "skip", Package: packagePath, Test: "TestAlpha"},
		{Action: "pass", Package: packagePath},
	}
	_, _, stats, valid := parseGoTestRun(encodeGoTestEvents(t, skipEvents), discovered, source)
	if !valid || stats.skipped != 1 {
		t.Fatalf("skip event parse=(%+v,%v), want one recorded skip", stats, valid)
	}
}

func TestTrustedGoVersionRequiresAndReturnsCompleteOutput(t *testing.T) {
	version, ok := parseTrustedGoVersion("go version go1.25.3 linux/amd64\n", "", 0, false, false, true)
	if !ok || version != "go version go1.25.3 linux/amd64" {
		t.Fatalf("version=(%q,%v), want complete go version output", version, ok)
	}
	for _, stdout := range []string{
		"go1.25.3 linux/amd64\n",
		"go version go1.25.3 linux/amd64",
		"go version go1.25.3 linux/amd64\nextra\n",
	} {
		if version, ok := parseTrustedGoVersion(stdout, "", 0, false, false, true); ok {
			t.Fatalf("incomplete version output %q accepted as %q", stdout, version)
		}
	}
}

func TestTrustedGoStageKeepsVisibleAndParserCaptureLimitsSeparate(t *testing.T) {
	stage := newShellGoStageResult(4, 8)
	if _, err := stage.stdoutWriter().Write([]byte("123456")); err != nil {
		t.Fatal(err)
	}
	if got := stage.stdout.String(); got != "123456" || stage.stdout.truncated {
		t.Fatalf("parser capture=(%q,truncated=%v), want complete 6-byte capture", got, stage.stdout.truncated)
	}
	if got := stage.visibleStdout.String(); got != "1234" || !stage.visibleStdout.truncated {
		t.Fatalf("visible output=(%q,truncated=%v), want bounded 4-byte prefix", got, stage.visibleStdout.truncated)
	}
	stage.clean = true
	if !successfulTrustedGoStage(stage) {
		t.Fatal("user-visible truncation incorrectly invalidated complete internal parser capture")
	}

	truncatedCapture := newShellGoStageResult(4, 5)
	_, _ = truncatedCapture.stdoutWriter().Write([]byte("123456"))
	truncatedCapture.clean = true
	if successfulTrustedGoStage(truncatedCapture) {
		t.Fatal("internal parser capture truncation was treated as complete")
	}
}

func encodeGoTestEvents(t *testing.T, events []goTestEvent) string {
	t.Helper()
	lines := make([]string, 0, len(events))
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(encoded))
	}
	return strings.Join(lines, "\n") + "\n"
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
