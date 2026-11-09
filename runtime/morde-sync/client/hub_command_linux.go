//go:build linux

package client

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// ABI 8 adds restrict-self TSYNC. ABI 9 Unix-path and ABI 10 UDP network
	// rights are covered by seccomp's denial of all socket/network syscalls.
	// Unknown later ABIs fail closed until their access rights are reviewed.
	hubCommandLandlockMinABI      = 8
	hubCommandLandlockMaxABI      = 10
	hubLandlockAccessFSIoctl      = 1 << 15
	hubLandlockAccessNetBind      = 1 << 0
	hubLandlockAccessNetConn      = 1 << 1
	hubLandlockScopeUnix          = 1 << 0
	hubLandlockScopeSignals       = 1 << 1
	hubLandlockRestrictTSync      = 1 << 3
	hubSeccompSetModeFilter       = 1
	hubSeccompFilterTSync         = 1
	hubSeccompRetKillProcess      = 0x80000000
	hubSeccompRetErrno            = 0x00050000
	hubSeccompRetAllow            = 0x7fff0000
	hubSeccompArchOffset          = 4
	hubSeccompSyscallOffset       = 0
	hubSeccompArg0Offset          = 16
	hubSeccompX32Bit              = 0x40000000
	hubLandlockCreateVersion      = 1
	hubLandlockPathBeneath        = 1
	hubCommandErrnoReturnMask     = 0xffff
	hubCloneNamespaceFlags        = 0x00000080 | 0x00020000 | 0x02000000 | 0x04000000 | 0x08000000 | 0x10000000 | 0x20000000 | 0x40000000
	hubSysFchmodat2               = 452
	hubAmd64SysChmod              = 90
	hubAmd64SysChown              = 92
	hubAmd64SysLchown             = 94
	hubAmd64SysUtime              = 132
	hubAmd64SysUtimes             = 235
	hubAmd64SysFutimesat          = 261
	hubCommandWorkspaceEntryLimit = 200_000
	hubCommandBootstrapArg        = "--morde-hub-command-bootstrap"
	hubCommandBootstrapFailure    = 125
)

var errHubCommandSandboxUnavailable = errors.New("required Hub command sandbox is unavailable")

type hubLandlockRulesetAttr struct {
	HandledAccessFS  uint64
	HandledAccessNet uint64
	Scoped           uint64
}

type hubCommandBootstrap struct {
	Workspace        string
	ExpectedIdentity string
	PrivateTemp      string
	RootMount        string
	CommandTemp      string
	PrivateTempView  string
	Argv             []string
	Env              []string
}

// The one-shot command is re-execed into new user and mount namespaces. Init
// builds its private filesystem root before the normal program entrypoint runs.
func init() {
	if len(os.Args) < 3 || os.Args[1] != hubCommandBootstrapArg {
		return
	}
	var request hubCommandBootstrap
	if json.Unmarshal([]byte(os.Args[2]), &request) != nil || runHubCommandBootstrap(request) != nil {
		_, _ = fmt.Fprintln(os.Stderr, "required Hub command sandbox is unavailable")
		syscall.Exit(hubCommandBootstrapFailure)
	}
	syscall.Exit(hubCommandBootstrapFailure)
}

// ExecHubCommandLandlocked confines the command to a private filesystem root
// and kernel sandbox. The namespace child replaces itself with argv[0]; this
// helper exits with the command's status so its service manager tracks the call.
func ExecHubCommandLandlocked(workspace, expectedWorkspaceIdentity, privateTemp string, argv, env []string) error {
	if os.Geteuid() == 0 {
		return fmt.Errorf("%w: root execution is unsupported", errHubCommandSandboxUnavailable)
	}
	if err := validateHubCommandExec(argv, env); err != nil {
		return err
	}
	if err := validateHubCommandStdio(); err != nil {
		return err
	}
	if _, err := seccompAuditArch(); err != nil {
		return err
	}
	if len(expectedWorkspaceIdentity) != sha256.Size*2 || !filepath.IsAbs(privateTemp) || filepath.Clean(privateTemp) != privateTemp {
		return fmt.Errorf("%w: invalid command workspace", errHubCommandSandboxUnavailable)
	}
	workspaceFile, workspacePath, err := openHubCommandDirectory(workspace, false)
	if err != nil {
		return err
	}
	if err := verifyHubCommandDirectoryIdentity(workspaceFile, workspacePath, expectedWorkspaceIdentity); err != nil {
		_ = workspaceFile.Close()
		return err
	}
	if err := verifyHubCommandWorkspace(workspaceFile); err != nil {
		return err // verifyHubCommandWorkspace owns and closes the descriptor.
	}
	if err := workspaceFile.Close(); err != nil {
		return fmt.Errorf("%w: close Workspace directory", errHubCommandSandboxUnavailable)
	}
	privateTempFile, privateTempPath, err := openHubCommandDirectory(privateTemp, true)
	if err != nil {
		return err
	}
	if err := privateTempFile.Close(); err != nil {
		return fmt.Errorf("%w: close private temporary directory", errHubCommandSandboxUnavailable)
	}
	if hubCommandPathsOverlap(workspacePath, privateTempPath) || hubCommandPathIsProtected(workspacePath) || hubCommandPathIsProtected(privateTempPath) {
		return fmt.Errorf("%w: workspace or temporary directory overlaps a protected system path", errHubCommandSandboxUnavailable)
	}
	if len(env) > 128 {
		return errors.New("invalid Hub command environment")
	}
	request, err := newHubCommandBootstrap(workspacePath, expectedWorkspaceIdentity, privateTempPath, argv, env)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > 64*1024 {
		_ = os.RemoveAll(request.RootMount)
		_ = os.RemoveAll(request.CommandTemp)
		return errors.New("invalid Hub command request")
	}
	executable, err := os.Executable()
	if err != nil || !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		_ = os.RemoveAll(request.RootMount)
		_ = os.RemoveAll(request.CommandTemp)
		return fmt.Errorf("%w: command helper unavailable", errHubCommandSandboxUnavailable)
	}
	child := exec.Command(executable, hubCommandBootstrapArg, string(encoded))
	child.SysProcAttr = hubCommandNamespaceSysProcAttr()
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	child.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "LC_ALL=C"}
	if err := child.Start(); err != nil {
		_ = os.RemoveAll(request.RootMount)
		_ = os.RemoveAll(request.CommandTemp)
		return fmt.Errorf("%w: create private user and mount namespaces", errHubCommandSandboxUnavailable)
	}
	if err := child.Wait(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Exited() {
				syscall.Exit(status.ExitStatus())
			}
			if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				syscall.Exit(128 + int(status.Signal()))
			}
		}
		return fmt.Errorf("%w: command namespace did not exit cleanly", errHubCommandSandboxUnavailable)
	}
	syscall.Exit(0)
	return nil
}

func hubCommandNamespaceSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Cloneflags:                 unix.CLONE_NEWUSER | unix.CLONE_NEWNS,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
}

func newHubCommandBootstrap(workspace, identity, privateTemp string, argv, env []string) (hubCommandBootstrap, error) {
	privateTempFile, _, err := openHubCommandDirectory(privateTemp, true)
	if err != nil {
		return hubCommandBootstrap{}, fmt.Errorf("%w: private temporary directory is unavailable", errHubCommandSandboxUnavailable)
	}
	if err := privateTempFile.Close(); err != nil {
		return hubCommandBootstrap{}, fmt.Errorf("%w: private temporary directory is unavailable", errHubCommandSandboxUnavailable)
	}
	rootMount, err := os.MkdirTemp(privateTemp, ".hub-command-root-")
	if err != nil {
		return hubCommandBootstrap{}, fmt.Errorf("%w: private filesystem root is unavailable", errHubCommandSandboxUnavailable)
	}
	commandTemp, err := os.MkdirTemp(privateTemp, ".hub-command-temp-")
	if err != nil {
		_ = os.RemoveAll(rootMount)
		return hubCommandBootstrap{}, fmt.Errorf("%w: private command temporary directory is unavailable", errHubCommandSandboxUnavailable)
	}
	for attempt := 0; attempt < 8; attempt++ {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			_ = os.RemoveAll(rootMount)
			_ = os.RemoveAll(commandTemp)
			return hubCommandBootstrap{}, fmt.Errorf("%w: private temporary path is unavailable", errHubCommandSandboxUnavailable)
		}
		privateTempView := filepath.Join(string(filepath.Separator), ".morde-hub-private-"+hex.EncodeToString(nonce[:]))
		if hubCommandPathsOverlap(privateTempView, workspace) || hubCommandPathIsProtected(privateTempView) {
			continue
		}
		return hubCommandBootstrap{
			Workspace: workspace, ExpectedIdentity: identity, PrivateTemp: privateTemp,
			RootMount: rootMount, CommandTemp: commandTemp, PrivateTempView: privateTempView,
			Argv: argv, Env: remapHubCommandTempEnv(env, privateTemp, privateTempView),
		}, nil
	}
	_ = os.RemoveAll(rootMount)
	_ = os.RemoveAll(commandTemp)
	return hubCommandBootstrap{}, fmt.Errorf("%w: private temporary path overlaps the Workspace", errHubCommandSandboxUnavailable)
}

func remapHubCommandTempEnv(env []string, privateTemp, privateTempView string) []string {
	remapped := make([]string, len(env))
	for index, entry := range env {
		key, value, found := strings.Cut(entry, "=")
		if found && value == privateTemp {
			remapped[index] = key + "=" + privateTempView
		} else {
			remapped[index] = entry
		}
	}
	return remapped
}

func runHubCommandBootstrap(request hubCommandBootstrap) error {
	if os.Geteuid() != 0 || os.Getuid() != 0 || request.Workspace == "" || request.ExpectedIdentity == "" || request.PrivateTemp == "" || request.RootMount == "" || request.CommandTemp == "" || request.PrivateTempView == "" || len(request.Argv) == 0 {
		return fmt.Errorf("%w: invalid namespace bootstrap", errHubCommandSandboxUnavailable)
	}
	if err := validateHubCommandExec(request.Argv, request.Env); err != nil {
		return err
	}
	runtime.LockOSThread()
	if err := mountHubCommandRoot(request); err != nil {
		return err
	}
	if err := dropHubCommandCapabilities(); err != nil {
		return err
	}
	ruleset, workspaceFD, err := createHubCommandLandlock(request.Workspace, request.ExpectedIdentity, request.PrivateTempView)
	if err != nil {
		return err
	}
	defer unix.Close(ruleset)
	defer unix.Close(workspaceFD)
	if err := validateHubCommandStdio(); err != nil {
		return err
	}
	if _, err := seccompAuditArch(); err != nil {
		return err
	}
	if err := unix.Fchdir(workspaceFD); err != nil {
		return fmt.Errorf("%w: workspace cwd", errHubCommandSandboxUnavailable)
	}
	if err := unix.CloseRange(3, ^uint(0), unix.CLOSE_RANGE_CLOEXEC); err != nil {
		return fmt.Errorf("%w: close inherited descriptors", errHubCommandSandboxUnavailable)
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("%w: no_new_privs", errHubCommandSandboxUnavailable)
	}
	if err := restrictHubCommandLandlock(ruleset); err != nil {
		return err
	}
	if err := installHubCommandSeccomp(); err != nil {
		return err
	}
	if err := syscall.Exec(request.Argv[0], request.Argv, request.Env); err != nil {
		return fmt.Errorf("%w: command exec", errHubCommandSandboxUnavailable)
	}
	return fmt.Errorf("%w: exec unexpectedly returned", errHubCommandSandboxUnavailable)
}

func mountHubCommandRoot(request hubCommandBootstrap) error {
	if !filepath.IsAbs(request.RootMount) || filepath.Clean(request.RootMount) != request.RootMount ||
		!filepath.IsAbs(request.CommandTemp) || filepath.Clean(request.CommandTemp) != request.CommandTemp ||
		!filepath.IsAbs(request.PrivateTempView) || filepath.Clean(request.PrivateTempView) != request.PrivateTempView {
		return fmt.Errorf("%w: invalid private filesystem paths", errHubCommandSandboxUnavailable)
	}
	rootInfo, err := os.Lstat(request.RootMount)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootInfo.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: private filesystem root is unavailable", errHubCommandSandboxUnavailable)
	}
	tempInfo, err := os.Lstat(request.CommandTemp)
	if err != nil || !tempInfo.IsDir() || tempInfo.Mode()&os.ModeSymlink != 0 || tempInfo.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: private command temporary directory is unavailable", errHubCommandSandboxUnavailable)
	}
	if err := unix.Mount("", string(filepath.Separator), "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("%w: isolate mount propagation", errHubCommandSandboxUnavailable)
	}
	if err := unix.Mount("tmpfs", request.RootMount, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=755,size=64m"); err != nil {
		return fmt.Errorf("%w: create private filesystem root", errHubCommandSandboxUnavailable)
	}
	if err := mountHubCommandRuntimeDirectories(request.RootMount); err != nil {
		return err
	}
	if err := mountHubCommandRuntimeFiles(request.RootMount); err != nil {
		return err
	}
	if err := bindHubCommandDirectory(request.CommandTemp, request.RootMount, request.PrivateTempView, unix.MOUNT_ATTR_NOSUID|unix.MOUNT_ATTR_NODEV|unix.MOUNT_ATTR_NOEXEC); err != nil {
		return fmt.Errorf("%w: expose private command temporary directory", errHubCommandSandboxUnavailable)
	}
	if err := bindHubCommandDirectory(request.Workspace, request.RootMount, request.Workspace, unix.MOUNT_ATTR_NOSUID|unix.MOUNT_ATTR_NODEV); err != nil {
		return fmt.Errorf("%w: expose selected Workspace", errHubCommandSandboxUnavailable)
	}
	if err := bindHubCommandFile("/dev/null", request.RootMount, "/dev/null", unix.MOUNT_ATTR_NOSUID); err != nil {
		return fmt.Errorf("%w: expose null device", errHubCommandSandboxUnavailable)
	}
	oldRoot := filepath.Join(request.RootMount, ".hub-command-old-root")
	if hubCommandPathsOverlap(request.Workspace, "/.hub-command-old-root") || hubCommandPathsOverlap(request.PrivateTempView, "/.hub-command-old-root") {
		return fmt.Errorf("%w: private filesystem path collision", errHubCommandSandboxUnavailable)
	}
	if err := os.Mkdir(oldRoot, 0o700); err != nil {
		return fmt.Errorf("%w: prepare private filesystem root", errHubCommandSandboxUnavailable)
	}
	if err := unix.PivotRoot(request.RootMount, oldRoot); err != nil {
		return fmt.Errorf("%w: enter private filesystem root", errHubCommandSandboxUnavailable)
	}
	if err := os.Chdir(string(filepath.Separator)); err != nil {
		return fmt.Errorf("%w: enter private filesystem root", errHubCommandSandboxUnavailable)
	}
	if err := unix.Unmount("/.hub-command-old-root", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("%w: detach host filesystem root", errHubCommandSandboxUnavailable)
	}
	if err := os.Remove("/.hub-command-old-root"); err != nil {
		return fmt.Errorf("%w: remove old filesystem root", errHubCommandSandboxUnavailable)
	}
	return nil
}

func mountHubCommandRuntimeDirectories(root string) error {
	seen := make([]string, 0, 5)
	for _, path := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64"} {
		canonical, err := filepath.EvalSymlinks(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !filepath.IsAbs(canonical) || filepath.Clean(canonical) != canonical {
			return fmt.Errorf("%w: required runtime directory", errHubCommandSandboxUnavailable)
		}
		info, err := os.Stat(canonical)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("%w: required runtime directory", errHubCommandSandboxUnavailable)
		}
		covered := false
		for _, parent := range seen {
			if hubCommandPathContains(parent, canonical) {
				covered = true
				break
			}
		}
		if !covered {
			if err := bindHubCommandDirectory(canonical, root, canonical, unix.MOUNT_ATTR_RDONLY|unix.MOUNT_ATTR_NOSUID|unix.MOUNT_ATTR_NODEV); err != nil {
				return fmt.Errorf("%w: expose runtime directory", errHubCommandSandboxUnavailable)
			}
			seen = append(seen, canonical)
		}
		if canonical != path {
			if err := createHubCommandRootSymlink(root, path, canonical); err != nil {
				return fmt.Errorf("%w: expose runtime directory alias", errHubCommandSandboxUnavailable)
			}
		}
	}
	return nil
}

func mountHubCommandRuntimeFiles(root string) error {
	for _, path := range []string{"/etc/ld.so.cache"} {
		canonical, err := filepath.EvalSymlinks(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !filepath.IsAbs(canonical) || filepath.Clean(canonical) != canonical {
			return fmt.Errorf("%w: required runtime file", errHubCommandSandboxUnavailable)
		}
		info, err := os.Stat(canonical)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("%w: invalid runtime file", errHubCommandSandboxUnavailable)
		}
		if hubCommandPathCoveredBy([]string{"/usr", "/bin", "/sbin", "/lib", "/lib64"}, canonical) {
			if canonical != path {
				if err := createHubCommandRootSymlink(root, path, canonical); err != nil {
					return fmt.Errorf("%w: expose runtime file alias", errHubCommandSandboxUnavailable)
				}
			}
			continue
		}
		if err := bindHubCommandFile(canonical, root, canonical, unix.MOUNT_ATTR_RDONLY|unix.MOUNT_ATTR_NOSUID|unix.MOUNT_ATTR_NODEV); err != nil {
			return fmt.Errorf("%w: expose runtime file", errHubCommandSandboxUnavailable)
		}
		if canonical != path {
			if err := createHubCommandRootSymlink(root, path, canonical); err != nil {
				return fmt.Errorf("%w: expose runtime file alias", errHubCommandSandboxUnavailable)
			}
		}
	}
	return nil
}

func hubCommandPathCoveredBy(roots []string, path string) bool {
	for _, root := range roots {
		canonical, err := filepath.EvalSymlinks(root)
		if err == nil && hubCommandPathContains(canonical, path) {
			return true
		}
	}
	return false
}

func bindHubCommandDirectory(source, root, virtualPath string, attrSet uint64) error {
	target, err := hubCommandRootPath(root, virtualPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	info, err := os.Lstat(target)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: invalid directory mount point", errHubCommandSandboxUnavailable)
	}
	if err := unix.Mount(source, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return err
	}
	if attrSet != 0 {
		if err := unix.MountSetattr(unix.AT_FDCWD, target, unix.AT_RECURSIVE, &unix.MountAttr{Attr_set: attrSet}); err != nil {
			return err
		}
	}
	return nil
}

func bindHubCommandFile(source, root, virtualPath string, attrSet uint64) error {
	target, err := hubCommandRootPath(root, virtualPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
		file, createErr := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if createErr != nil {
			return createErr
		}
		if err := file.Close(); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	info, err := os.Lstat(target)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: invalid file mount point", errHubCommandSandboxUnavailable)
	}
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	if attrSet != 0 {
		if err := unix.MountSetattr(unix.AT_FDCWD, target, 0, &unix.MountAttr{Attr_set: attrSet}); err != nil {
			return err
		}
	}
	return nil
}

func createHubCommandRootSymlink(root, virtualPath, targetPath string) error {
	target, err := hubCommandRootPath(root, virtualPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: runtime alias already exists", errHubCommandSandboxUnavailable)
	}
	return os.Symlink(targetPath, target)
}

func hubCommandRootPath(root, virtualPath string) (string, error) {
	if !filepath.IsAbs(virtualPath) || filepath.Clean(virtualPath) != virtualPath || virtualPath == string(filepath.Separator) {
		return "", fmt.Errorf("%w: invalid private filesystem path", errHubCommandSandboxUnavailable)
	}
	target := filepath.Join(root, strings.TrimPrefix(virtualPath, string(filepath.Separator)))
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: invalid private filesystem path", errHubCommandSandboxUnavailable)
	}
	return target, nil
}

func dropHubCommandCapabilities() error {
	var capabilities []uintptr
	for capability := uintptr(0); capability < 64; capability++ {
		value, _, errno := unix.RawSyscall(unix.SYS_PRCTL, unix.PR_CAPBSET_READ, capability, 0)
		if errno == unix.EINVAL {
			break
		}
		if errno != 0 {
			return fmt.Errorf("%w: inspect namespace capabilities", errHubCommandSandboxUnavailable)
		}
		if value == 1 {
			capabilities = append(capabilities, capability)
		}
	}
	setpcapPresent := false
	for _, capability := range capabilities {
		if capability == unix.CAP_SETPCAP {
			setpcapPresent = true
			continue
		}
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, capability, 0, 0, 0); err != nil {
			return fmt.Errorf("%w: drop namespace capability", errHubCommandSandboxUnavailable)
		}
	}
	if setpcapPresent {
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, unix.CAP_SETPCAP, 0, 0, 0); err != nil {
			return fmt.Errorf("%w: drop namespace capability", errHubCommandSandboxUnavailable)
		}
	}
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil && err != unix.EINVAL {
		return fmt.Errorf("%w: clear ambient capabilities", errHubCommandSandboxUnavailable)
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3, Pid: 0}
	data := [2]unix.CapUserData{}
	if err := unix.Capset(&header, &data[0]); err != nil {
		return fmt.Errorf("%w: drop namespace capabilities", errHubCommandSandboxUnavailable)
	}
	return nil
}

func validateHubCommandExec(argv, env []string) error {
	if len(argv) == 0 || argv[0] == "" || !filepath.IsAbs(argv[0]) || filepath.Clean(argv[0]) != argv[0] || strings.ContainsRune(argv[0], 0) {
		return errors.New("invalid Hub command exec path")
	}
	for _, value := range argv {
		if strings.ContainsRune(value, 0) {
			return errors.New("invalid Hub command argument")
		}
	}
	for _, value := range env {
		if strings.ContainsRune(value, 0) || !strings.Contains(value, "=") || strings.IndexByte(value, '=') == 0 {
			return errors.New("invalid Hub command environment")
		}
	}
	return nil
}

// The private mount root hides host paths; Landlock then limits accesses within
// the selected Workspace, runtime directories and private temporary directory.
func createHubCommandLandlock(workspace, expectedWorkspaceIdentity, privateTemp string) (int, int, error) {
	abi, err := hubLandlockABIVersion()
	if err != nil || abi < hubCommandLandlockMinABI || abi > hubCommandLandlockMaxABI {
		return -1, -1, fmt.Errorf("%w: Landlock ABI %d", errHubCommandSandboxUnavailable, abi)
	}
	workspaceFile, workspacePath, err := openHubCommandDirectory(workspace, false)
	if err != nil {
		return -1, -1, err
	}
	failed := true
	defer func() {
		if failed {
			_ = workspaceFile.Close()
		}
	}()
	if err := verifyHubCommandDirectoryIdentity(workspaceFile, workspacePath, expectedWorkspaceIdentity); err != nil {
		return -1, -1, err
	}
	if err := verifyHubCommandWorkspace(workspaceFile); err != nil {
		return -1, -1, err
	}
	tempFile, tempPath, err := openHubCommandDirectory(privateTemp, true)
	if err != nil {
		return -1, -1, err
	}
	defer tempFile.Close()
	if hubCommandPathsOverlap(workspacePath, tempPath) || hubCommandPathIsProtected(workspacePath) || hubCommandPathIsProtected(tempPath) {
		return -1, -1, fmt.Errorf("%w: workspace or temporary directory overlaps a protected system path", errHubCommandSandboxUnavailable)
	}

	var knownFS uint64 = unix.LANDLOCK_ACCESS_FS_EXECUTE |
		unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM |
		unix.LANDLOCK_ACCESS_FS_REFER |
		unix.LANDLOCK_ACCESS_FS_TRUNCATE |
		hubLandlockAccessFSIoctl
	attr := hubLandlockRulesetAttr{
		HandledAccessFS:  knownFS,
		HandledAccessNet: hubLandlockAccessNetBind | hubLandlockAccessNetConn,
		Scoped:           hubLandlockScopeUnix | hubLandlockScopeSignals,
	}
	ruleset, err := createHubLandlockRuleset(attr)
	if err != nil {
		return -1, -1, err
	}
	closeRuleset := true
	defer func() {
		if closeRuleset {
			_ = unix.Close(ruleset)
		}
	}()

	readExecute := uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR)
	seenSystemDirs := make([]string, 0, 5)
	for _, path := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64"} {
		file, canonical, _, err := openHubCommandSystemPath(path, true, path == "/usr")
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return -1, -1, err
		}
		covered := false
		for _, parent := range seenSystemDirs {
			if hubCommandPathContains(parent, canonical) {
				covered = true
				break
			}
		}
		if !covered {
			if err := addHubCommandLandlockRule(ruleset, int(file.Fd()), readExecute); err != nil {
				_ = file.Close()
				return -1, -1, err
			}
			seenSystemDirs = append(seenSystemDirs, canonical)
		}
		if err := file.Close(); err != nil {
			return -1, -1, fmt.Errorf("%w: close system path", errHubCommandSandboxUnavailable)
		}
	}
	for _, path := range []string{"/etc/passwd", "/etc/group", "/etc/nsswitch.conf", "/etc/ld.so.cache", "/etc/ld.so.preload"} {
		file, _, _, err := openHubCommandSystemPath(path, false, false)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return -1, -1, err
		}
		if err := addHubCommandLandlockRule(ruleset, int(file.Fd()), uint64(unix.LANDLOCK_ACCESS_FS_READ_FILE)); err != nil {
			_ = file.Close()
			return -1, -1, err
		}
		if err := file.Close(); err != nil {
			return -1, -1, fmt.Errorf("%w: close runtime file", errHubCommandSandboxUnavailable)
		}
	}
	devNull, _, _, err := openHubCommandSystemPath("/dev/null", false, true)
	if err != nil {
		return -1, -1, err
	}
	if err := addHubCommandLandlockRule(ruleset, int(devNull.Fd()), uint64(unix.LANDLOCK_ACCESS_FS_READ_FILE|unix.LANDLOCK_ACCESS_FS_WRITE_FILE)); err != nil {
		_ = devNull.Close()
		return -1, -1, err
	}
	if err := devNull.Close(); err != nil {
		return -1, -1, fmt.Errorf("%w: close null device", errHubCommandSandboxUnavailable)
	}

	workspaceRights := knownFS &^ uint64(
		hubLandlockAccessFSIoctl|
			unix.LANDLOCK_ACCESS_FS_MAKE_CHAR|
			unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK|
			unix.LANDLOCK_ACCESS_FS_MAKE_SOCK,
	)
	tempRights := workspaceRights &^ uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE)
	if err := addHubCommandLandlockRule(ruleset, int(workspaceFile.Fd()), workspaceRights); err != nil {
		return -1, -1, err
	}
	if err := addHubCommandLandlockRule(ruleset, int(tempFile.Fd()), tempRights); err != nil {
		return -1, -1, err
	}
	if err := tempFile.Close(); err != nil {
		return -1, -1, fmt.Errorf("%w: close temporary directory", errHubCommandSandboxUnavailable)
	}
	if _, err := unix.FcntlInt(uintptr(ruleset), unix.F_SETFD, unix.FD_CLOEXEC); err != nil {
		return -1, -1, fmt.Errorf("%w: mark ruleset close-on-exec", errHubCommandSandboxUnavailable)
	}
	closeRuleset = false
	failed = false
	return ruleset, int(workspaceFile.Fd()), nil
}

func verifyHubCommandDirectoryIdentity(workspace *os.File, canonical, expected string) error {
	if len(expected) != sha256.Size*2 {
		return fmt.Errorf("%w: invalid Workspace identity", errHubCommandSandboxUnavailable)
	}
	info, err := workspace.Stat()
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: inspect opened Workspace identity", errHubCommandSandboxUnavailable)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino == 0 {
		return fmt.Errorf("%w: unsupported Workspace file identity", errHubCommandSandboxUnavailable)
	}
	fileIdentity := fmt.Sprintf("unix:%d:%d", stat.Dev, stat.Ino)
	actual := hubIdentityToken(canonical, fileIdentity)
	if subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) != 1 {
		return fmt.Errorf("%w: Workspace identity changed", errHubCommandSandboxUnavailable)
	}
	return nil
}

func verifyHubCommandWorkspace(workspace *os.File) error {
	fd, err := unix.Openat(int(workspace.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("%w: inspect Workspace entries", errHubCommandSandboxUnavailable)
	}
	var entries int
	return verifyHubCommandWorkspaceDirectory(os.NewFile(uintptr(fd), "workspace"), 0, &entries)
}

func openHubCommandWorkspaceEntry(directoryFD int, name string, flags uint64) (int, error) {
	return unix.Openat2(directoryFD, name, &unix.OpenHow{
		Flags: flags,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS |
			unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV,
	})
}

func verifyHubCommandWorkspaceDirectory(directory *os.File, depth int, entries *int) error {
	defer directory.Close()
	if depth > 256 {
		return fmt.Errorf("%w: Workspace directory depth exceeds the command limit", errHubCommandSandboxUnavailable)
	}
	for {
		children, readErr := directory.ReadDir(256)
		for _, child := range children {
			(*entries)++
			if *entries > hubCommandWorkspaceEntryLimit {
				return fmt.Errorf("%w: Workspace entry count exceeds the command limit", errHubCommandSandboxUnavailable)
			}
			var before unix.Stat_t
			if err := unix.Fstatat(int(directory.Fd()), child.Name(), &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return fmt.Errorf("%w: inspect Workspace entry", errHubCommandSandboxUnavailable)
			}
			kind := before.Mode & unix.S_IFMT
			if kind == unix.S_IFLNK {
				continue
			}
			if kind != unix.S_IFREG && kind != unix.S_IFDIR || kind == unix.S_IFREG && before.Nlink != 1 {
				return fmt.Errorf("%w: Workspace contains a special file or hard link", errHubCommandSandboxUnavailable)
			}
			flags := uint64(unix.O_PATH | unix.O_CLOEXEC)
			if kind == unix.S_IFDIR {
				flags = uint64(unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC)
			}
			childFD, err := openHubCommandWorkspaceEntry(int(directory.Fd()), child.Name(), flags)
			if err != nil {
				return fmt.Errorf("%w: Workspace contains a mount point or changed entry", errHubCommandSandboxUnavailable)
			}
			var after unix.Stat_t
			if err := unix.Fstat(childFD, &after); err != nil || before.Dev != after.Dev || before.Ino != after.Ino ||
				before.Mode&unix.S_IFMT != after.Mode&unix.S_IFMT || kind == unix.S_IFREG && after.Nlink != 1 {
				_ = unix.Close(childFD)
				return fmt.Errorf("%w: Workspace entry changed during inspection", errHubCommandSandboxUnavailable)
			}
			if kind == unix.S_IFDIR {
				if err := verifyHubCommandWorkspaceDirectory(os.NewFile(uintptr(childFD), child.Name()), depth+1, entries); err != nil {
					return err
				}
			} else if err := unix.Close(childFD); err != nil {
				return fmt.Errorf("%w: close Workspace entry", errHubCommandSandboxUnavailable)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("%w: read Workspace entries", errHubCommandSandboxUnavailable)
		}
	}
}

func hubLandlockABIVersion() (int, error) {
	version, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, hubLandlockCreateVersion)
	if errno != 0 || version < 1 {
		return 0, fmt.Errorf("%w: Landlock version query", errHubCommandSandboxUnavailable)
	}
	return int(version), nil
}

func createHubLandlockRuleset(attr hubLandlockRulesetAttr) (int, error) {
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	runtime.KeepAlive(&attr)
	if errno != 0 || int64(fd) < 0 {
		return -1, fmt.Errorf("%w: create Landlock ruleset", errHubCommandSandboxUnavailable)
	}
	return int(fd), nil
}

func addHubCommandLandlockRule(ruleset, parentFD int, access uint64) error {
	attr := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(parentFD)}
	_, _, errno := unix.Syscall(unix.SYS_LANDLOCK_ADD_RULE,
		uintptr(ruleset), unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&attr)))
	runtime.KeepAlive(&attr)
	if errno != 0 {
		return fmt.Errorf("%w: add Landlock path rule", errHubCommandSandboxUnavailable)
	}
	return nil
}

func restrictHubCommandLandlock(ruleset int) error {
	_, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF,
		uintptr(ruleset), hubLandlockRestrictTSync, 0)
	if errno != 0 {
		return fmt.Errorf("%w: restrict Landlock domain", errHubCommandSandboxUnavailable)
	}
	return nil
}

func openHubCommandDirectory(path string, private bool) (*os.File, string, error) {
	if path == "" || strings.ContainsRune(path, 0) || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return nil, "", fmt.Errorf("%w: invalid directory", errHubCommandSandboxUnavailable)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || canonical != path {
		return nil, "", fmt.Errorf("%w: non-canonical directory", errHubCommandSandboxUnavailable)
	}
	before, err := os.Lstat(path)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, "", fmt.Errorf("%w: directory unavailable", errHubCommandSandboxUnavailable)
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{
		Flags:   uint64(unix.O_PATH | unix.O_CLOEXEC | unix.O_DIRECTORY),
		Resolve: unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return nil, "", fmt.Errorf("%w: open directory", errHubCommandSandboxUnavailable)
	}
	file := os.NewFile(uintptr(fd), path)
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !after.IsDir() {
		_ = file.Close()
		return nil, "", fmt.Errorf("%w: directory changed", errHubCommandSandboxUnavailable)
	}
	if private {
		stat, ok := after.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || after.Mode().Perm()&0o077 != 0 {
			_ = file.Close()
			return nil, "", fmt.Errorf("%w: temporary directory is not private", errHubCommandSandboxUnavailable)
		}
	}
	return file, path, nil
}

func openHubCommandSystemPath(path string, directory, required bool) (*os.File, string, os.FileInfo, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		if !required && errors.Is(err, os.ErrNotExist) {
			return nil, "", nil, os.ErrNotExist
		}
		return nil, "", nil, fmt.Errorf("%w: required runtime path", errHubCommandSandboxUnavailable)
	}
	if !filepath.IsAbs(canonical) || filepath.Clean(canonical) != canonical {
		return nil, "", nil, fmt.Errorf("%w: invalid runtime path", errHubCommandSandboxUnavailable)
	}
	before, err := os.Stat(canonical)
	if err != nil {
		if !required && errors.Is(err, os.ErrNotExist) {
			return nil, "", nil, os.ErrNotExist
		}
		return nil, "", nil, fmt.Errorf("%w: runtime path unavailable", errHubCommandSandboxUnavailable)
	}
	if directory && !before.IsDir() || !directory && !before.Mode().IsRegular() && path != "/dev/null" {
		if path == "/dev/null" && before.Mode()&(os.ModeDevice|os.ModeCharDevice) == (os.ModeDevice|os.ModeCharDevice) {
			// /dev/null is the only device path exposed to the command.
		} else {
			return nil, "", nil, fmt.Errorf("%w: invalid runtime path type", errHubCommandSandboxUnavailable)
		}
	}
	if path == "/dev/null" && before.Mode()&(os.ModeDevice|os.ModeCharDevice) != (os.ModeDevice|os.ModeCharDevice) {
		return nil, "", nil, fmt.Errorf("%w: null device unavailable", errHubCommandSandboxUnavailable)
	}
	flags := uint64(unix.O_PATH | unix.O_CLOEXEC)
	if directory {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, canonical, &unix.OpenHow{
		Flags: flags, Resolve: unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return nil, "", nil, fmt.Errorf("%w: open runtime path", errHubCommandSandboxUnavailable)
	}
	file := os.NewFile(uintptr(fd), canonical)
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		_ = file.Close()
		return nil, "", nil, fmt.Errorf("%w: runtime path changed", errHubCommandSandboxUnavailable)
	}
	return file, canonical, after, nil
}

func hubCommandPathIsProtected(path string) bool {
	if path == string(filepath.Separator) {
		return true
	}
	for _, protected := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc", "/dev", "/proc", "/sys", "/run"} {
		if hubCommandPathsOverlap(path, protected) {
			return true
		}
	}
	return false
}

func hubCommandPathsOverlap(a, b string) bool {
	return hubCommandPathContains(a, b) || hubCommandPathContains(b, a)
}

func hubCommandPathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && (relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func validateHubCommandStdio() error {
	for fd := 0; fd <= 2; fd++ {
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			if errors.Is(err, unix.EBADF) {
				continue
			}
			return fmt.Errorf("%w: inspect standard descriptor", errHubCommandSandboxUnavailable)
		}
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFIFO, unix.S_IFCHR:
			// Pipes and terminals are the command's explicit stdio channels.
		default:
			return fmt.Errorf("%w: unsafe standard descriptor type", errHubCommandSandboxUnavailable)
		}
	}
	return nil
}

func seccompAuditArch() (uint32, error) {
	switch runtime.GOARCH {
	case "amd64":
		return unix.AUDIT_ARCH_X86_64, nil
	case "arm64":
		return unix.AUDIT_ARCH_AARCH64, nil
	default:
		return 0, fmt.Errorf("%w: unsupported Linux architecture", errHubCommandSandboxUnavailable)
	}
}

func installHubCommandSeccomp() error {
	arch, err := seccompAuditArch()
	if err != nil {
		return err
	}
	filters := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: hubSeccompArchOffset},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: arch},
		{Code: unix.BPF_RET | unix.BPF_K, K: hubSeccompRetKillProcess},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: hubSeccompSyscallOffset},
	}
	if arch == unix.AUDIT_ARCH_X86_64 {
		filters = append(filters,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, Jf: 1, K: hubSeccompX32Bit},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: hubSeccompRetKillProcess},
		)
	}
	for _, number := range hubCommandBlockedSyscalls() {
		filters = append(filters,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: uint32(number)},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: hubSeccompRetErrno | uint32(unix.EPERM)&hubCommandErrnoReturnMask},
		)
	}
	filters = append(filters,
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 3, K: uint32(unix.SYS_CLONE)},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: hubSeccompArg0Offset},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, Jf: 1, K: hubCloneNamespaceFlags},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: hubSeccompRetErrno | uint32(unix.EPERM)&hubCommandErrnoReturnMask},
		// ENOSYS lets Go fall back from clone3 to clone, where namespace flags
		// are inspected above. The compat ABI itself is killed by the arch check.
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: uint32(unix.SYS_CLONE3)},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: hubSeccompRetErrno | uint32(unix.ENOSYS)&hubCommandErrnoReturnMask},
	)
	filters = append(filters, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: hubSeccompRetAllow})
	program := unix.SockFprog{Len: uint16(len(filters)), Filter: &filters[0]}
	result, _, errno := unix.Syscall(unix.SYS_SECCOMP,
		hubSeccompSetModeFilter, hubSeccompFilterTSync, uintptr(unsafe.Pointer(&program)))
	runtime.KeepAlive(filters)
	runtime.KeepAlive(&program)
	if errno != 0 || result != 0 {
		return fmt.Errorf("%w: install synchronized seccomp filter", errHubCommandSandboxUnavailable)
	}
	return nil
}

func hubCommandBlockedSyscalls() []uintptr {
	blocked := []uintptr{
		unix.SYS_SOCKET, unix.SYS_SOCKETPAIR, unix.SYS_CONNECT,
		unix.SYS_ACCEPT, unix.SYS_ACCEPT4, unix.SYS_BIND, unix.SYS_LISTEN,
		unix.SYS_GETSOCKNAME, unix.SYS_GETPEERNAME,
		unix.SYS_SENDTO, unix.SYS_RECVFROM, unix.SYS_SENDMSG, unix.SYS_RECVMSG,
		unix.SYS_SENDMMSG, unix.SYS_RECVMMSG,
		unix.SYS_SHUTDOWN, unix.SYS_SETSOCKOPT, unix.SYS_GETSOCKOPT,
		unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
		unix.SYS_IOCTL,
		unix.SYS_KILL, unix.SYS_TKILL, unix.SYS_TGKILL,
		unix.SYS_RT_SIGQUEUEINFO, unix.SYS_RT_TGSIGQUEUEINFO,
		unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV,
		unix.SYS_PROCESS_MADVISE, unix.SYS_PIDFD_OPEN, unix.SYS_PIDFD_GETFD,
		unix.SYS_PIDFD_SEND_SIGNAL,
		unix.SYS_UNSHARE, unix.SYS_SETNS, unix.SYS_MOUNT, unix.SYS_UMOUNT2,
		unix.SYS_PIVOT_ROOT, unix.SYS_CHROOT, unix.SYS_OPEN_TREE, unix.SYS_MOVE_MOUNT,
		unix.SYS_FSOPEN, unix.SYS_FSCONFIG, unix.SYS_FSMOUNT, unix.SYS_FSPICK,
		unix.SYS_MOUNT_SETATTR, unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN,
	}
	return append(blocked, hubCommandMetadataMutationSyscalls()...)
}

// Landlock does not mediate inode metadata. Deny metadata mutations globally
// because seccomp cannot safely scope pathname arguments to allowed trees.
func hubCommandMetadataMutationSyscalls() []uintptr {
	common := []uintptr{
		unix.SYS_FCHMOD, unix.SYS_FCHMODAT, hubSysFchmodat2,
		unix.SYS_FCHOWN, unix.SYS_FCHOWNAT, unix.SYS_UTIMENSAT,
		unix.SYS_SETXATTR, unix.SYS_LSETXATTR, unix.SYS_FSETXATTR,
		unix.SYS_REMOVEXATTR, unix.SYS_LREMOVEXATTR, unix.SYS_FREMOVEXATTR,
	}
	switch runtime.GOARCH {
	case "amd64":
		return append(common,
			hubAmd64SysChmod, hubAmd64SysChown, hubAmd64SysLchown,
			hubAmd64SysUtime, hubAmd64SysUtimes, hubAmd64SysFutimesat,
		)
	case "arm64":
		return common
	default:
		return nil
	}
}
