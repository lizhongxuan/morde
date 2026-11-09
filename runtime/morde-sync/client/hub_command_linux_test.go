//go:build linux

package client

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	hubCommandTestStageEnv     = "MORDE_HUB_COMMAND_TEST_STAGE"
	hubCommandTestWorkspaceEnv = "MORDE_HUB_COMMAND_TEST_WORKSPACE"
	hubCommandTestIdentityEnv  = "MORDE_HUB_COMMAND_TEST_IDENTITY"
	hubCommandTestTempEnv      = "MORDE_HUB_COMMAND_TEST_TEMP"
	hubCommandTestOutsideEnv   = "MORDE_HUB_COMMAND_TEST_OUTSIDE"
	hubCommandTestHomeEnv      = "MORDE_HUB_COMMAND_TEST_HOME"
	hubCommandTestExeEnv       = "MORDE_HUB_COMMAND_TEST_EXE"
)

func TestExecHubCommandLandlockedSubprocess(t *testing.T) {
	switch os.Getenv(hubCommandTestStageEnv) {
	case "seccomp-probe":
		testHubCommandSeccompProbe(t)
		return
	case "prepare":
		testHubCommandExecSubprocess(t)
		return
	case "landlocked", "landlocked-child":
		testHubCommandLandlockedAccess(t)
		if os.Getenv(hubCommandTestStageEnv) == "landlocked" {
			testHubCommandLandlockedChild(t)
		}
		return
	case "":
	default:
		t.Fatalf("unexpected test stage %q", os.Getenv(hubCommandTestStageEnv))
	}

	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skipf("unsupported seccomp architecture %s", runtime.GOARCH)
	}
	if os.Geteuid() == 0 {
		t.Skip("sandboxed commands must not run as euid 0")
	}
	abi, err := hubLandlockABIVersion()
	if err != nil || abi < hubCommandLandlockMinABI || abi > hubCommandLandlockMaxABI {
		t.Skipf("Landlock ABI %d is outside supported ABI range %d..%d", abi, hubCommandLandlockMinABI, hubCommandLandlockMaxABI)
	}

	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	privateTemp := filepath.Join(root, "private-temp")
	home := filepath.Join(root, "home")
	for _, path := range []string{workspace, privateTemp, home} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(root, "outside-secret")
	if err := os.WriteFile(outside, []byte("must remain unreadable"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "outside-link")); err != nil {
		t.Fatal(err)
	}
	homeSecret := filepath.Join(home, "secret")
	if err := os.WriteFile(homeSecret, []byte("must remain unreadable"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "seed"), []byte("workspace is readable"), 0o600); err != nil {
		t.Fatal(err)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	commandExe := filepath.Join(workspace, "sandbox-test")
	if err := copyHubCommandTestExecutable(self, commandExe); err != nil {
		t.Fatal(err)
	}
	selection, err := SelectHubRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	replacedWorkspace := workspace + "-selected"
	if err := os.Rename(workspace, replacedWorkspace); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workspace, 0o700); err != nil {
		_ = os.Rename(replacedWorkspace, workspace)
		t.Fatal(err)
	}
	replacementRuleset, replacementWorkspaceFD, replacementErr := createHubCommandLandlock(workspace, selection.Identity, privateTemp)
	if replacementErr == nil {
		_ = unix.Close(replacementRuleset)
		_ = unix.Close(replacementWorkspaceFD)
	}
	if err := os.Remove(workspace); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacedWorkspace, workspace); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(replacementErr, errHubCommandSandboxUnavailable) {
		t.Fatalf("replaced Workspace identity error = %v; want fail-closed rejection", replacementErr)
	}

	probe := exec.Command(commandExe, "-test.run=^TestExecHubCommandLandlockedSubprocess$")
	probe.Env = hubCommandTestEnvironment("seccomp-probe", workspace, selection.Identity, privateTemp, outside, home, commandExe)
	if output, err := probe.CombinedOutput(); err != nil {
		t.Fatalf("seccomp probe subprocess: %v\n%s", err, output)
	}

	runner := exec.Command(commandExe, "-test.run=^TestExecHubCommandLandlockedSubprocess$")
	runner.Env = hubCommandTestEnvironment("prepare", workspace, selection.Identity, privateTemp, outside, home, commandExe)
	if output, err := runner.CombinedOutput(); err != nil {
		t.Fatalf("landlocked runner subprocess: %v\n%s", err, output)
	}
}

func TestExecHubCommandLandlockedRejectsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root rejection check requires euid 0")
	}
	if err := ExecHubCommandLandlocked("", "", "", nil, nil); !errors.Is(err, errHubCommandSandboxUnavailable) {
		t.Fatalf("root sandbox request error = %v; want fail-closed rejection", err)
	}
}

func TestHubCommandWorkspaceRejectsHardlinks(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(root, "external-secret")
	if err := os.WriteFile(external, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(external, filepath.Join(workspace, "alias")); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(workspace, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	err = verifyHubCommandWorkspace(os.NewFile(uintptr(fd), workspace))
	if !errors.Is(err, errHubCommandSandboxUnavailable) {
		t.Fatalf("hard-linked Workspace error = %v; want fail-closed rejection", err)
	}
}

func TestHubCommandDirectoryScanRejectsNestedMountCrossing(t *testing.T) {
	rootFD, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(rootFD)
	_, err = openHubCommandWorkspaceEntry(rootFD, "proc", uint64(unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC))
	if !errors.Is(err, unix.EXDEV) {
		t.Fatalf("openat2 through /proc mount = %v; want EXDEV", err)
	}
}

func testHubCommandExecSubprocess(t *testing.T) {
	t.Helper()
	workspace := os.Getenv(hubCommandTestWorkspaceEnv)
	privateTemp := os.Getenv(hubCommandTestTempEnv)
	expectedIdentity := os.Getenv(hubCommandTestIdentityEnv)
	commandExe := os.Getenv(hubCommandTestExeEnv)
	argv := []string{commandExe, "-test.run=^TestExecHubCommandLandlockedSubprocess$"}
	env := hubCommandTestEnvironment(
		"landlocked",
		workspace,
		expectedIdentity,
		privateTemp,
		os.Getenv(hubCommandTestOutsideEnv),
		os.Getenv(hubCommandTestHomeEnv),
		commandExe,
	)
	if err := ExecHubCommandLandlocked(workspace, expectedIdentity, privateTemp, argv, env); err != nil {
		t.Fatalf("ExecHubCommandLandlocked: %v", err)
	}
	t.Fatal("ExecHubCommandLandlocked returned after successful exec")
}

func testHubCommandLandlockedAccess(t *testing.T) {
	t.Helper()
	workspace := os.Getenv(hubCommandTestWorkspaceEnv)
	privateTemp := os.Getenv(hubCommandTestTempEnv)
	outside := os.Getenv(hubCommandTestOutsideEnv)
	homeSecret := filepath.Join(os.Getenv(hubCommandTestHomeEnv), "secret")

	if data, err := os.ReadFile(filepath.Join(workspace, "seed")); err != nil || string(data) != "workspace is readable" {
		t.Fatalf("read Workspace file: data=%q err=%v", data, err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "written-by-command"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("write Workspace file: %v", err)
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat outside file = %v; want the host path to be absent in the command root", err)
	}
	if _, err := os.Lstat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lstat outside file = %v; want the host path to be absent in the command root", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "outside-link")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat Workspace symlink to outside file = %v; want the host target to be absent", err)
	}
	if _, err := os.ReadFile(filepath.Join(workspace, "outside-link")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read Workspace symlink to outside file = %v; want the host target to be absent", err)
	}
	if _, err := os.Readlink(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("readlink outside file = %v; want the host path to be absent in the command root", err)
	}
	if err := os.Chmod(outside, 0o644); !errors.Is(err, unix.EPERM) {
		t.Fatalf("chmod outside file = %v; want seccomp EPERM", err)
	}
	if err := os.Chtimes(outside, time.Now(), time.Now()); !errors.Is(err, unix.EPERM) {
		t.Fatalf("chtimes outside file = %v; want seccomp EPERM", err)
	}
	if _, err := os.ReadDir(filepath.Dir(workspace)); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("read Workspace parent = %v; want Landlock to deny directory enumeration", err)
	}
	if err := os.WriteFile(filepath.Join(privateTemp, "written-by-command"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("write private temp file: %v", err)
	}
	privateTempExecutable := filepath.Join(privateTemp, "must-not-execute")
	if err := os.WriteFile(privateTempExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("create private temp executable: %v", err)
	}
	if err := exec.Command(privateTempExecutable).Run(); err == nil || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("exec private temp file = %v; want permission denied", err)
	}
	if err := exec.Command("/usr/bin/true").Run(); err != nil {
		t.Fatalf("execute system runtime binary /usr/bin/true: %v", err)
	}
	devNull, err := os.OpenFile("/dev/null", os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open allowed /dev/null: %v", err)
	}
	if err := devNull.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		outside,
		homeSecret,
		"/etc/hosts",
		"/proc/self/status",
	} {
		if _, err := os.ReadFile(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ReadFile(%q) = %v; want the host path to be absent", path, err)
		}
	}
	for _, path := range []string{"/etc/passwd", "/etc/group", "/etc/nsswitch.conf", "/etc/ld.so.preload"} {
		if _, err := os.ReadFile(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ReadFile(%q) = %v; want host account/config file to be absent", path, err)
		}
	}
	for _, path := range []string{"/sys", "/run"} {
		if _, err := os.ReadDir(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ReadDir(%q) = %v; want the host path to be absent", path, err)
		}
	}

	if _, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0); !errors.Is(err, unix.EPERM) {
		t.Fatalf("IPv4 socket = %v; want EPERM", err)
	}
	conn, err := net.DialTimeout("tcp4", "127.0.0.1:9", time.Second)
	if conn != nil {
		_ = conn.Close()
		t.Fatal("IPv4 TCP dial unexpectedly succeeded")
	}
	if !errors.Is(err, unix.EPERM) {
		t.Fatalf("IPv4 TCP dial error = %v; want EPERM", err)
	}
	if _, _, errno := unix.RawSyscall(unix.SYS_IOCTL, 0, 0, 0); errno != unix.EPERM {
		t.Fatalf("ioctl errno = %v; want EPERM", errno)
	}
}

func testHubCommandLandlockedChild(t *testing.T) {
	t.Helper()
	commandExe := os.Getenv(hubCommandTestExeEnv)
	cmd := exec.Command(commandExe, "-test.run=^TestExecHubCommandLandlockedSubprocess$")
	cmd.Env = hubCommandTestEnvironment(
		"landlocked-child",
		os.Getenv(hubCommandTestWorkspaceEnv),
		os.Getenv(hubCommandTestIdentityEnv),
		os.Getenv(hubCommandTestTempEnv),
		os.Getenv(hubCommandTestOutsideEnv),
		os.Getenv(hubCommandTestHomeEnv),
		commandExe,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandboxed child: %v\n%s", err, output)
	}
}

func testHubCommandSeccompProbe(t *testing.T) {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("create pre-filter IPv4 socket: %v", err)
	}
	defer unix.Close(fd)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := installHubCommandSeccomp(); err != nil {
		t.Fatal(err)
	}

	if err := unix.Connect(fd, &unix.SockaddrInet4{Port: 9, Addr: [4]byte{127, 0, 0, 1}}); !errors.Is(err, unix.EPERM) {
		t.Fatalf("connect on pre-filter IPv4 socket = %v; want EPERM", err)
	}
	for _, number := range hubCommandBlockedSyscalls() {
		_, _, errno := unix.RawSyscall(number, 0, 0, 0)
		if errno != unix.EPERM {
			t.Errorf("syscall %d errno = %v; want EPERM", number, errno)
		}
	}
	_, _, errno := unix.RawSyscall(unix.SYS_CLONE3, 0, 0, 0)
	if errno != unix.ENOSYS {
		t.Errorf("clone3 errno = %v; want ENOSYS for Go fallback", errno)
	}
}

func hubCommandTestEnvironment(stage, workspace, identity, privateTemp, outside, home, executable string) []string {
	return []string{
		hubCommandTestStageEnv + "=" + stage,
		hubCommandTestWorkspaceEnv + "=" + workspace,
		hubCommandTestIdentityEnv + "=" + identity,
		hubCommandTestTempEnv + "=" + privateTemp,
		hubCommandTestOutsideEnv + "=" + outside,
		hubCommandTestHomeEnv + "=" + home,
		hubCommandTestExeEnv + "=" + executable,
		"HOME=" + home,
	}
}

func copyHubCommandTestExecutable(source, destination string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.WriteFile(destination, data, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(destination, 0o700); err != nil {
		return err
	}
	return nil
}
