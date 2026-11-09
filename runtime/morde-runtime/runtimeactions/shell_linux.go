//go:build linux

package runtimeactions

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"morde-runtime/contract"
	"morde-runtime/runtimecore"
)

const (
	cgroup2SuperMagic    = 0x63677270
	defaultCgroupRoot    = "/sys/fs/cgroup"
	cgroupName           = "morde-runtime-shell"
	maxPIDsPerAction     = 128
	maxParallelShells    = 2
	maxTrackedOperations = 1 << 16
	maxMemoryPerAction   = 1 << 30
	maxOutputBytes       = 512 << 10
	maxTempBytes         = 64 << 20
	cpuQuota             = "50000 100000"
	maxStopCleanup       = 5 * time.Second
)

type operationPhase uint8

const (
	operationPreparing operationPhase = iota + 1
	operationRunning
	operationStopRequested
	operationStoppedBeforeExecute
	operationComplete
)

type operationRecord struct {
	phase     operationPhase
	groupPath string
}

// ShellHandler requires a delegated cgroup v2 hierarchy. It does not fall back
// to process groups because descendants can escape those with setsid(2).
type ShellHandler struct {
	basePath      string
	bwrapPath     string
	bwrapBase     []string
	setprivPath   string
	setsidPath    string
	credential    *syscall.Credential
	workDir       string
	workspaceRoot *os.Root
	maxTimeout    time.Duration
	pollInterval  time.Duration
	outputLimit   int
	parallel      chan struct{}

	mu         sync.Mutex
	operations map[string]operationRecord
}

// NewShellHandler advertises shell.exec only when the current Linux runtime can
// create per-operation cgroups, use CLONE_INTO_CGROUP, apply bounded resource
// limits, and prevent setuid privilege gains for gem.
func NewShellHandler(options ShellOptions) (*ShellHandler, error) {
	if os.Geteuid() != 0 {
		return nil, ErrProcessDomainUnavailable
	}
	if options.CgroupRoot == "" {
		options.CgroupRoot = defaultCgroupRoot
	}
	if options.MaxTimeout == 0 {
		options.MaxTimeout = maxShellTimeout
	}
	if options.MaxTimeout <= 0 || options.MaxTimeout > maxShellTimeout {
		return nil, ErrInvalidShellRequest
	}
	if options.PollInterval == 0 {
		options.PollInterval = 10 * time.Millisecond
	}
	if options.PollInterval < time.Millisecond || options.PollInterval > time.Second {
		return nil, ErrInvalidShellRequest
	}
	if options.MaxOutputBytes == 0 {
		options.MaxOutputBytes = defaultOutputLimit
	}
	if options.MaxOutputBytes < 1024 || options.MaxOutputBytes > maxOutputBytes {
		return nil, ErrInvalidShellRequest
	}
	if options.DefaultWorkDir == "" {
		options.DefaultWorkDir = defaultShellWorkDir
	}
	if !filepath.IsAbs(options.DefaultWorkDir) || strings.IndexByte(options.DefaultWorkDir, 0) >= 0 {
		return nil, ErrInvalidShellRequest
	}
	workDir, err := filepath.EvalSymlinks(options.DefaultWorkDir)
	if err != nil {
		return nil, ErrProcessDomainUnavailable
	}
	workInfo, err := os.Stat(workDir)
	if err != nil || !workInfo.IsDir() {
		return nil, ErrProcessDomainUnavailable
	}

	root, err := filepath.EvalSymlinks(options.CgroupRoot)
	if err != nil {
		return nil, ErrProcessDomainUnavailable
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(root, &fs); err != nil || uint64(fs.Type) != cgroup2SuperMagic {
		return nil, ErrProcessDomainUnavailable
	}
	base := filepath.Join(root, cgroupName)
	if err := makeTrustedCgroupDir(base); err != nil {
		return nil, ErrProcessDomainUnavailable
	}
	if err := enableResourceControllers(base); err != nil {
		return nil, ErrProcessDomainUnavailable
	}
	setpriv, err := findTrustedSetpriv()
	if err != nil {
		return nil, ErrProcessDomainUnavailable
	}
	bwrap, err := findTrustedBubblewrap()
	if err != nil {
		return nil, ErrProcessDomainUnavailable
	}
	bwrapBase, err := bubblewrapBaseArguments()
	if err != nil {
		return nil, ErrProcessDomainUnavailable
	}
	setsid, err := findTrustedExecutable([]string{"/usr/bin/setsid", "/bin/setsid"})
	if err != nil {
		return nil, ErrProcessDomainUnavailable
	}
	credential, _, err := gemCredential()
	if err != nil {
		return nil, ErrProcessDomainUnavailable
	}
	rootInfo, err := os.Lstat(workDir)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, ErrProcessDomainUnavailable
	}
	workspaceRoot, err := os.OpenRoot(workDir)
	if err != nil {
		return nil, ErrProcessDomainUnavailable
	}
	openedRootInfo, err := workspaceRoot.Stat(".")
	if err != nil || !os.SameFile(rootInfo, openedRootInfo) {
		_ = workspaceRoot.Close()
		return nil, ErrProcessDomainUnavailable
	}
	handler := &ShellHandler{
		basePath: base, bwrapPath: bwrap, bwrapBase: bwrapBase, setprivPath: setpriv,
		setsidPath: setsid, credential: credential, workDir: workDir, workspaceRoot: workspaceRoot, maxTimeout: options.MaxTimeout,
		pollInterval: options.PollInterval, outputLimit: options.MaxOutputBytes,
		parallel: make(chan struct{}, maxParallelShells), operations: make(map[string]operationRecord),
	}
	// The probe proves that bubblewrap can build the restricted mount namespace,
	// that sibling workspaces and Host secrets are hidden, and that cgroup.kill
	// removes detached descendants in this exact container.
	if err := handler.probe(); err != nil {
		_ = workspaceRoot.Close()
		return nil, ErrProcessDomainUnavailable
	}
	return handler, nil
}

func GemCredentialIDs() (uint32, uint32, error) {
	credential, _, err := gemCredential()
	if err != nil {
		return 0, 0, ErrProcessDomainUnavailable
	}
	return credential.Uid, credential.Gid, nil
}

func (h *ShellHandler) Capability() string { return shellCapability }

func (h *ShellHandler) Action() string { return shellAction }

func (h *ShellHandler) Execute(ctx context.Context, request contract.ExecuteRequest) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if request.Capability != shellCapability || request.Action != shellAction || !validOperationID(request.OperationID) ||
		!validWorkspaceDirectory(request.WorkspaceID) || request.BindingVersion < 1 ||
		(request.WorkspaceViewID == "" && request.WorkspaceDirectory != request.WorkspaceID) ||
		(request.WorkspaceViewID != "" && (!validTaskWorkspaceDirectory(request.TaskID, request.WorkspaceViewID) || request.WorkspaceDirectory != request.TaskID+"/"+request.WorkspaceViewID)) {
		return nil, ErrInvalidShellRequest
	}
	if err := h.beginOperation(request.OperationID); err != nil {
		return nil, err
	}
	defer h.completeOperation(request.OperationID)

	args, timeout, err := parseShellArguments(request.Arguments, h.maxTimeout)
	if err != nil {
		return nil, err
	}
	var workspaceRoot *os.Root
	if request.WorkspaceViewID == "" {
		workspaceRoot, err = h.ensureWorkspaceDirectory(request.WorkspaceDirectory)
	} else {
		workspaceRoot, err = h.ensureTaskWorkspaceDirectory(request.TaskID, request.WorkspaceViewID)
	}
	if err != nil {
		return nil, ErrInvalidShellRequest
	}
	defer workspaceRoot.Close()
	workspaceDir, err := workspaceRoot.Open(".")
	if err != nil {
		return nil, ErrInvalidShellRequest
	}
	defer workspaceDir.Close()
	workspacePath := filepath.Join(h.workDir, request.WorkspaceDirectory)
	logicalWorkspacePath := filepath.Join(h.workDir, request.WorkspaceID)
	if request.WorkspaceViewID != "" {
		workspacePath = filepath.Join(h.workDir, ".morde-task-views", request.TaskID, request.WorkspaceViewID)
	}
	if args.CWD == "" {
		args.CWD = workspacePath
	} else if request.WorkspaceViewID != "" {
		logicalCWD := filepath.Clean(args.CWD)
		relative, relErr := filepath.Rel(logicalWorkspacePath, logicalCWD)
		if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, ErrInvalidShellRequest
		}
		args.CWD = filepath.Join(workspacePath, relative)
	}
	cleanCWD := filepath.Clean(args.CWD)
	if !filepath.IsAbs(cleanCWD) {
		return nil, ErrInvalidShellRequest
	}
	relativeCWD, relErr := filepath.Rel(workspacePath, cleanCWD)
	info, statErr := workspaceRoot.Stat(relativeCWD)
	if relErr != nil || relativeCWD == ".." || strings.HasPrefix(relativeCWD, ".."+string(filepath.Separator)) || statErr != nil || !info.IsDir() {
		return nil, ErrInvalidShellRequest
	}
	jailWorkspace := filepath.Join("/home/gem/workspace", request.WorkspaceID)
	jailCWD := filepath.Join(jailWorkspace, relativeCWD)
	operationContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case h.parallel <- struct{}{}:
		defer func() { <-h.parallel }()
	case <-operationContext.Done():
		return shellResultIfTimedOut(operationContext)
	}
	if operationContext.Err() != nil {
		return shellResultIfTimedOut(operationContext)
	}

	operationID := request.OperationID
	groupPath := h.groupPath(operationID)
	h.mu.Lock()
	if !h.operationMayStartLocked(operationID) {
		h.mu.Unlock()
		return nil, ErrShellOperationCancelled
	}
	if operationContext.Err() != nil {
		h.mu.Unlock()
		return shellResultIfTimedOut(operationContext)
	}
	if err := createOperationGroup(groupPath); err != nil {
		h.mu.Unlock()
		return nil, ErrProcessDomainUnavailable
	}
	command := h.command(args, workspaceDir, jailWorkspace, jailCWD)
	command.Dir = string(filepath.Separator)
	command.Env = []string{"PATH=/usr/bin:/bin"}
	budget := &outputBudget{limit: h.outputLimit}
	stdout := &boundedBuffer{limit: h.outputLimit, budget: budget}
	stderr := &boundedBuffer{limit: h.outputLimit, budget: budget}
	command.Stdout = stdout
	command.Stderr = stderr
	command.SysProcAttr = &syscall.SysProcAttr{
		Credential:  h.credential,
		UseCgroupFD: true,
		CgroupFD:    -1,
	}
	group, err := os.Open(groupPath)
	if err != nil {
		h.mu.Unlock()
		_ = os.Remove(groupPath)
		return nil, ErrProcessDomainUnavailable
	}
	command.SysProcAttr.CgroupFD = int(group.Fd())
	if err := command.Start(); err != nil {
		_ = group.Close()
		h.mu.Unlock()
		_ = os.Remove(groupPath)
		return nil, ErrProcessDomainUnavailable
	}
	_ = group.Close()
	h.operations[operationID] = operationRecord{phase: operationRunning, groupPath: groupPath}
	h.mu.Unlock()

	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()

	var waitErr error
	timedOut := false
	select {
	case waitErr = <-wait:
	case <-operationContext.Done():
		timedOut = errors.Is(operationContext.Err(), context.DeadlineExceeded)
		// Execute never returns while any process in this operation's cgroup
		// remains alive, even if an earlier Stop request timed out.
		h.killUntilEmpty(groupPath)
		waitErr = <-wait
	}
	if operationContext.Err() != nil {
		timedOut = errors.Is(operationContext.Err(), context.DeadlineExceeded)
	}
	// This loop only returns after an empty cgroup is proven; a persistent
	// kernel failure keeps the operation unresolved rather than claiming stop.
	h.killUntilEmpty(groupPath)
	if ctx.Err() != nil && !timedOut {
		return nil, ErrShellOperationCancelled
	}

	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, ErrProcessDomainUnavailable
		}
	}
	result := shellResult{
		ExitCode: exitCode, Stdout: stdout.String(), Stderr: stderr.String(),
		StdoutTruncated: stdout.truncated, StderrTruncated: stderr.truncated,
		TimedOut: timedOut,
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, ErrProcessDomainUnavailable
	}
	return encoded, nil
}

func validWorkspaceDirectory(value string) bool {
	if len(value) != 8 || value[0] != 'w' {
		return false
	}
	for _, char := range value[1:] {
		if !((char >= 'a' && char <= 'z') || (char >= '0' && char <= '9')) {
			return false
		}
	}
	return true
}

func validTaskWorkspaceDirectory(taskID, viewID string) bool {
	if !validIdentifier(taskID, 'q') || len(viewID) != 65 || viewID[0] != 'v' {
		return false
	}
	for _, char := range viewID[1:] {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func validIdentifier(value string, prefix byte) bool {
	if len(value) != 8 || value[0] != prefix {
		return false
	}
	for _, char := range value[1:] {
		if !(char >= 'a' && char <= 'z') && !(char >= '0' && char <= '9') {
			return false
		}
	}
	return true
}

func (h *ShellHandler) ensureWorkspaceDirectory(directory string) (*os.Root, error) {
	if h.workspaceRoot == nil || !validWorkspaceDirectory(directory) {
		return nil, ErrInvalidShellRequest
	}
	info, err := h.workspaceRoot.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		if err := h.workspaceRoot.Mkdir(directory, 0o770); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, ErrInvalidShellRequest
		}
		info, err = h.workspaceRoot.Lstat(directory)
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrInvalidShellRequest
	}
	workspaceRoot, err := openShellWorkspaceRoot(h.workspaceRoot, directory, info)
	if err != nil {
		return nil, ErrInvalidShellRequest
	}
	openedInfo, err := workspaceRoot.Stat(".")
	if err != nil {
		_ = workspaceRoot.Close()
		return nil, ErrInvalidShellRequest
	}
	stat, ok := openedInfo.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != 0 && stat.Uid != h.credential.Uid) {
		_ = workspaceRoot.Close()
		return nil, ErrInvalidShellRequest
	}
	if stat.Uid == 0 {
		if err := workspaceRoot.Chown(".", 0, int(h.credential.Gid)); err != nil {
			_ = workspaceRoot.Close()
			return nil, ErrInvalidShellRequest
		}
		if err := workspaceRoot.Chmod(".", 0o770|os.ModeSetgid); err != nil {
			_ = workspaceRoot.Close()
			return nil, ErrInvalidShellRequest
		}
		openedInfo, err = workspaceRoot.Stat(".")
		if err != nil {
			_ = workspaceRoot.Close()
			return nil, ErrInvalidShellRequest
		}
		stat, ok = openedInfo.Sys().(*syscall.Stat_t)
	}
	ownerAccessible := ok && stat.Uid == h.credential.Uid && openedInfo.Mode().Perm()&0o700 == 0o700
	groupAccessible := ok && stat.Gid == h.credential.Gid && openedInfo.Mode().Perm()&0o070 == 0o070
	if !ownerAccessible && !groupAccessible {
		_ = workspaceRoot.Close()
		return nil, ErrInvalidShellRequest
	}
	return workspaceRoot, nil
}

func (h *ShellHandler) ensureTaskWorkspaceDirectory(taskID, viewID string) (*os.Root, error) {
	if h.workspaceRoot == nil || !validTaskWorkspaceDirectory(taskID, viewID) {
		return nil, ErrInvalidShellRequest
	}
	taskViewsInfo, err := h.workspaceRoot.Lstat(".morde-task-views")
	if err != nil || !taskViewsInfo.IsDir() || taskViewsInfo.Mode()&os.ModeSymlink != 0 {
		return nil, ErrInvalidShellRequest
	}
	taskViewsRoot, err := openShellWorkspaceRoot(h.workspaceRoot, ".morde-task-views", taskViewsInfo)
	if err != nil {
		return nil, ErrInvalidShellRequest
	}
	defer taskViewsRoot.Close()
	taskInfo, err := taskViewsRoot.Lstat(taskID)
	if err != nil || !taskInfo.IsDir() || taskInfo.Mode()&os.ModeSymlink != 0 {
		return nil, ErrInvalidShellRequest
	}
	taskRoot, err := openShellWorkspaceRoot(taskViewsRoot, taskID, taskInfo)
	if err != nil {
		return nil, ErrInvalidShellRequest
	}
	defer taskRoot.Close()
	viewInfo, err := taskRoot.Lstat(viewID)
	if err != nil || !viewInfo.IsDir() || viewInfo.Mode()&os.ModeSymlink != 0 {
		return nil, ErrInvalidShellRequest
	}
	viewRoot, err := openShellWorkspaceRoot(taskRoot, viewID, viewInfo)
	if err != nil {
		return nil, ErrInvalidShellRequest
	}
	openedInfo, err := viewRoot.Stat(".")
	stat, ok := openedInfo.Sys().(*syscall.Stat_t)
	if err != nil || !ok || stat.Uid != h.credential.Uid || stat.Gid != h.credential.Gid || openedInfo.Mode().Perm()&0o770 != 0o770 {
		_ = viewRoot.Close()
		return nil, ErrInvalidShellRequest
	}
	return viewRoot, nil
}

func openShellWorkspaceRoot(parent *os.Root, directory string, expected os.FileInfo) (*os.Root, error) {
	workspaceRoot, err := parent.OpenRoot(directory)
	if err != nil {
		return nil, ErrInvalidShellRequest
	}
	openedInfo, err := workspaceRoot.Stat(".")
	if err != nil || !os.SameFile(expected, openedInfo) {
		_ = workspaceRoot.Close()
		return nil, ErrInvalidShellRequest
	}
	return workspaceRoot, nil
}

func (h *ShellHandler) Stop(ctx context.Context, operation contract.Operation) (runtimecore.StopResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if operation.Capability != shellCapability || operation.Action != shellAction || !validOperationID(operation.OperationID) {
		return runtimecore.StopResult{}, ErrInvalidShellRequest
	}
	id := operation.OperationID
	groupPath, tracked := h.requestStop(id)
	if !tracked {
		return runtimecore.StopResult{PendingIDs: []string{id}}, nil
	}
	cleanupContext, cancel := context.WithTimeout(ctx, maxStopCleanup)
	defer cancel()
	if err := h.killAndWait(cleanupContext, groupPath); err != nil {
		return runtimecore.StopResult{PendingIDs: []string{id}}, nil
	}
	return runtimecore.StopResult{VerifiedStopped: true, PendingIDs: []string{}}, nil
}

func (h *ShellHandler) command(args shellArguments, workspaceDir *os.File, jailWorkspace, jailCWD string) *exec.Cmd {
	commandArgs := append([]string(nil), h.bwrapBase...)
	commandArgs = append(commandArgs,
		"--dir", "/dev", "--dev", "/dev",
		"--dir", "/proc", "--proc", "/proc",
		"--size", strconv.Itoa(maxTempBytes), "--tmpfs", "/tmp",
		"--dir", "/run", "--tmpfs", "/run",
		"--dir", "/var", "--dir", "/var/tmp", "--tmpfs", "/var/tmp",
		"--dir", "/home", "--dir", "/home/gem", "--dir", "/home/gem/workspace",
		"--clearenv",
		"--setenv", "HOME", "/home/gem",
		"--setenv", "USER", "gem",
		"--setenv", "LOGNAME", "gem",
		"--setenv", "SHELL", "/bin/bash",
		"--setenv", "PATH", "/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin",
		"--setenv", "LANG", "C.UTF-8",
		"--setenv", "TERM", "xterm-256color",
		"--setenv", "TZ", "Asia/Shanghai",
		"--setenv", "TMPDIR", "/tmp",
		"--setenv", "PWD", jailCWD,
	)
	relativeJail, _ := filepath.Rel("/home/gem/workspace", jailWorkspace)
	parent := "/home/gem/workspace"
	for _, component := range strings.Split(relativeJail, string(filepath.Separator)) {
		parent = filepath.Join(parent, component)
		commandArgs = append(commandArgs, "--dir", parent)
	}
	commandArgs = append(commandArgs, "--bind", "/proc/self/fd/3", jailWorkspace,
		"--", h.setprivPath, "--no-new-privs", "--", "/bin/bash", "-c", args.Command)
	command := exec.Command(h.bwrapPath, commandArgs...)
	command.ExtraFiles = []*os.File{workspaceDir}
	return command
}

func (h *ShellHandler) groupPath(operationID string) string {
	digest := sha256.Sum256([]byte(operationID))
	return filepath.Join(h.basePath, "op-"+hex.EncodeToString(digest[:]))
}

func (h *ShellHandler) beginOperation(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if existing, exists := h.operations[id]; exists {
		if existing.phase == operationStoppedBeforeExecute {
			// Consume the transient stop tombstone but retain the seen ID so a
			// replay cannot start the same operation later.
			h.operations[id] = operationRecord{phase: operationComplete}
			return ErrShellOperationCancelled
		}
		return runtimecore.ErrOperationConflict
	}
	if len(h.operations) >= maxTrackedOperations {
		// Evicting an ID would allow a direct handler replay. Refuse unseen work
		// once the bounded replay-protection ledger is full.
		return ErrProcessDomainUnavailable
	}
	h.operations[id] = operationRecord{phase: operationPreparing}
	return nil
}

func (h *ShellHandler) completeOperation(id string) {
	h.mu.Lock()
	if _, exists := h.operations[id]; exists {
		h.operations[id] = operationRecord{phase: operationComplete}
	}
	h.mu.Unlock()
}

// operationMayStartLocked is checked while the same mutex protects Stop's
// transition to operationStopRequested, making the launch/stop race atomic.
func (h *ShellHandler) operationMayStartLocked(id string) bool {
	state, exists := h.operations[id]
	return exists && state.phase == operationPreparing
}

func (h *ShellHandler) requestStop(id string) (string, bool) {
	groupPath := h.groupPath(id)
	h.mu.Lock()
	defer h.mu.Unlock()
	state, exists := h.operations[id]
	if !exists {
		if len(h.operations) >= maxTrackedOperations {
			return groupPath, false
		}
		// Keeping this bounded tombstone closes the Stop-before-Execute race.
		h.operations[id] = operationRecord{phase: operationStoppedBeforeExecute}
		return groupPath, true
	}
	switch state.phase {
	case operationPreparing:
		state.phase = operationStopRequested
	case operationRunning, operationStopRequested:
		state.phase = operationStopRequested
		if state.groupPath != "" {
			groupPath = state.groupPath
		}
	case operationStoppedBeforeExecute, operationComplete:
		// A repeated stop is idempotent. Verification below still checks the
		// canonical operation cgroup before reporting success.
	default:
		return groupPath, false
	}
	h.operations[id] = state
	return groupPath, true
}

func shellResultIfTimedOut(ctx context.Context) (json.RawMessage, error) {
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, ErrShellOperationCancelled
	}
	result, err := json.Marshal(shellResult{ExitCode: -1, TimedOut: true})
	if err != nil {
		return nil, ErrProcessDomainUnavailable
	}
	return result, nil
}

func (h *ShellHandler) probe() error {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	hexRandom := hex.EncodeToString(random[:])
	workspaceID := "w" + hexRandom[:7]
	siblingID := "w" + hexRandom[7:14]
	workspacePath := filepath.Join(h.workDir, workspaceID)
	siblingRoot := filepath.Join(h.workDir, siblingID)
	if err := h.workspaceRoot.Mkdir(workspaceID, 0o700); err != nil {
		return err
	}
	defer h.workspaceRoot.RemoveAll(workspaceID)
	workspaceRoot, err := h.ensureWorkspaceDirectory(workspaceID)
	if err != nil {
		return err
	}
	defer workspaceRoot.Close()
	workspaceDir, err := workspaceRoot.Open(".")
	if err != nil {
		return err
	}
	defer workspaceDir.Close()
	if err := h.workspaceRoot.Mkdir(siblingID, 0o700); err != nil {
		return err
	}
	defer h.workspaceRoot.RemoveAll(siblingID)
	if err := os.Chown(siblingRoot, int(h.credential.Uid), int(h.credential.Gid)); err != nil {
		return err
	}
	if err := writeProbeFile(filepath.Join(workspacePath, "marker"), "workspace-visible", h.credential); err != nil {
		return err
	}
	siblingSecret := filepath.Join(siblingRoot, "secret")
	if err := writeProbeFile(siblingSecret, "sibling-secret", h.credential); err != nil {
		return err
	}
	if err := os.Symlink(siblingSecret, filepath.Join(workspacePath, "escape")); err != nil {
		return err
	}

	groupPath := filepath.Join(h.basePath, "probe-"+hexRandom)
	if err := createOperationGroup(groupPath); err != nil {
		return err
	}
	cleaned := false
	defer func() {
		if !cleaned {
			h.killUntilEmpty(groupPath)
		}
	}()
	group, err := os.Open(groupPath)
	if err != nil {
		return err
	}
	jailWorkspace := filepath.Join("/home/gem/workspace", workspaceID)
	jailSiblingSecret := filepath.Join("/home/gem/workspace", siblingID, "secret")
	script := `set -eu
test "$EUID" -eq "$1"
test "$(cat marker)" = workspace-visible
test -w .
test ! -r "../$2/secret"
test ! -r "$3"
if cat escape >/dev/null 2>&1; then exit 81; fi
test ! -e /run/morde-runtime/credential
test ! -e /var/lib/morde-runtime
test ! -e /root
if touch /usr/.morde-shell-probe-write >/dev/null 2>&1; then rm -f /usr/.morde-shell-probe-write; exit 82; fi
"$4" "$5" 30 >/dev/null 2>&1 &
echo ready`
	command := h.command(shellArguments{Command: script}, workspaceDir, jailWorkspace, jailWorkspace)
	command.Args = append(command.Args, "morde-shell-probe", strconv.FormatUint(uint64(h.credential.Uid), 10), siblingID, jailSiblingSecret, h.setsidPath, "/bin/sleep")
	command.Dir = string(filepath.Separator)
	command.Env = []string{"PATH=/usr/bin:/bin"}
	command.Stderr = io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{Credential: h.credential, UseCgroupFD: true, CgroupFD: int(group.Fd())}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = group.Close()
		return err
	}
	if err := command.Start(); err != nil {
		_ = group.Close()
		return err
	}
	_ = group.Close()
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err == nil && line != "ready\n" {
			err = ErrProcessDomainUnavailable
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			return err
		}
	case err := <-wait:
		if err == nil {
			return ErrProcessDomainUnavailable
		}
		return err
	case <-time.After(time.Second):
		return ErrProcessDomainUnavailable
	}
	deadline := time.Now().Add(time.Second)
	for {
		populated, err := cgroupPopulated(filepath.Join(groupPath, "cgroup.events"))
		if err != nil {
			return err
		}
		if populated {
			break
		}
		if time.Now().After(deadline) {
			return ErrProcessDomainUnavailable
		}
		select {
		case err := <-wait:
			if err == nil {
				return ErrProcessDomainUnavailable
			}
			return err
		case <-time.After(h.pollInterval):
		}
	}
	probeContext, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	err = h.killAndWait(probeContext, groupPath)
	cancel()
	if err != nil {
		return err
	}
	cleaned = true
	select {
	case err := <-wait:
		if err != nil && !wasKilledBySIGKILL(err) {
			return ErrProcessDomainUnavailable
		}
	case <-time.After(time.Second):
		_ = command.Process.Kill()
		return ErrProcessDomainUnavailable
	}
	return nil
}

func wasKilledBySIGKILL(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGKILL
}

func writeProbeFile(path, content string, credential *syscall.Credential) error {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Chown(path, int(credential.Uid), int(credential.Gid))
}

func (h *ShellHandler) killUntilEmpty(groupPath string) {
	// This retry has no deadline only on Execute's completion path (and during
	// startup probe cleanup): ActionHandler requires Execute to wait until the
	// entire cgroup is empty, while startup refuses to return a handler unless
	// the probe finishes. Stop does not call this helper; it has a fixed cleanup
	// deadline and returns Pending without asserting VerifiedStopped on failure.
	for {
		if err := h.killAndWait(context.Background(), groupPath); err == nil {
			return
		}
		time.Sleep(h.pollInterval)
	}
}

func (h *ShellHandler) killAndWait(ctx context.Context, groupPath string) error {
	for {
		if err := cgroupMountStillAvailable(h.basePath); err != nil {
			return ErrProcessDomainUnavailable
		}
		info, err := os.Lstat(groupPath)
		if errors.Is(err, os.ErrNotExist) {
			// A cgroup can only be removed when its process population is empty.
			return nil
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrProcessDomainUnavailable
		}
		populated, err := cgroupPopulated(filepath.Join(groupPath, "cgroup.events"))
		if err != nil {
			return ErrProcessDomainUnavailable
		}
		if !populated {
			// Removal is cleanup only. The kernel's populated=0 receipt already
			// proves there are no members in this operation domain.
			_ = os.Remove(groupPath)
			return nil
		}
		if err := writeCgroupFile(filepath.Join(groupPath, "cgroup.kill"), "1"); err != nil {
			return ErrProcessDomainUnavailable
		}
		select {
		case <-ctx.Done():
			return ErrProcessDomainUnavailable
		case <-time.After(h.pollInterval):
		}
	}
}

func makeTrustedCgroupDir(path string) error {
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrProcessDomainUnavailable
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
		return ErrProcessDomainUnavailable
	}
	if _, err := os.Stat(filepath.Join(path, "cgroup.events")); err != nil {
		return err
	}
	return nil
}

func enableResourceControllers(base string) error {
	if _, err := os.Stat(filepath.Join(base, "cgroup.procs")); err != nil {
		return err
	}
	controllers, err := os.ReadFile(filepath.Join(base, "cgroup.controllers"))
	if err != nil {
		return err
	}
	available := make(map[string]bool)
	for _, name := range strings.Fields(string(controllers)) {
		available[name] = true
	}
	for _, name := range []string{"cpu", "memory", "pids"} {
		if !available[name] {
			return ErrProcessDomainUnavailable
		}
	}
	procs, err := os.ReadFile(filepath.Join(base, "cgroup.procs"))
	if err != nil || strings.TrimSpace(string(procs)) != "" {
		return ErrProcessDomainUnavailable
	}
	if err := writeCgroupFile(filepath.Join(base, "cgroup.subtree_control"), "+cpu +memory +pids"); err != nil {
		return err
	}
	return nil
}

func createOperationGroup(path string) error {
	if err := os.Mkdir(path, 0700); err != nil {
		return err
	}
	for _, name := range []string{"cpu.max", "memory.max", "pids.max", "cgroup.events", "cgroup.kill", "cgroup.procs"} {
		file := filepath.Join(path, name)
		info, err := os.Stat(file)
		if err != nil {
			_ = os.Remove(path)
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
			_ = os.Remove(path)
			return ErrProcessDomainUnavailable
		}
	}
	populated, err := cgroupPopulated(filepath.Join(path, "cgroup.events"))
	if err != nil || populated {
		_ = os.Remove(path)
		return ErrProcessDomainUnavailable
	}
	if err := writeCgroupFile(filepath.Join(path, "cpu.max"), cpuQuota); err != nil {
		_ = os.Remove(path)
		return err
	}
	if err := writeCgroupFile(filepath.Join(path, "memory.max"), strconv.Itoa(maxMemoryPerAction)); err != nil {
		_ = os.Remove(path)
		return err
	}
	if err := writeCgroupFile(filepath.Join(path, "pids.max"), strconv.Itoa(maxPIDsPerAction)); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func cgroupPopulated(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "populated" {
			switch fields[1] {
			case "0":
				return false, nil
			case "1":
				return true, nil
			default:
				return false, ErrProcessDomainUnavailable
			}
		}
	}
	return false, ErrProcessDomainUnavailable
}

func writeCgroupFile(path, value string) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, writeErr := fmt.Fprintln(file, value)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func cgroupMountStillAvailable(base string) error {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(base, &fs); err != nil || uint64(fs.Type) != cgroup2SuperMagic {
		return ErrProcessDomainUnavailable
	}
	return nil
}

func findTrustedSetpriv() (string, error) {
	return findTrustedExecutable([]string{"/usr/bin/setpriv", "/bin/setpriv"})
}

func findTrustedBubblewrap() (string, error) {
	for _, candidate := range []string{"/usr/bin/bwrap", "/bin/bwrap"} {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || info.Mode()&os.ModeSetgid != 0 {
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if ok && stat.Uid == 0 {
			// Bubblewrap's root-owned setuid installation is trusted to create
			// namespaces when unprivileged user namespaces are unavailable. Its
			// flags and effects are still proved by the startup probe.
			return resolved, nil
		}
	}
	return "", ErrProcessDomainUnavailable
}

func bubblewrapBaseArguments() ([]string, error) {
	args := []string{
		"--die-with-parent", "--new-session",
		"--unshare-user", "--unshare-ipc", "--unshare-pid", "--unshare-net", "--unshare-uts",
		"--disable-userns", "--cap-drop", "ALL",
	}
	for _, path := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64"} {
		var err error
		args, err = appendBubblewrapReadonlySystemPath(args, path, path, path != "/sbin" && path != "/lib64")
		if err != nil {
			return nil, err
		}
	}
	args = append(args, "--dir", "/etc")
	for _, path := range []struct {
		path     string
		required bool
	}{
		{path: "/etc/passwd", required: true},
		{path: "/etc/group", required: true},
		{path: "/etc/nsswitch.conf", required: true},
		{path: "/etc/ld.so.cache"},
	} {
		info, err := os.Stat(path.path)
		if errors.Is(err, os.ErrNotExist) && !path.required {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return nil, ErrProcessDomainUnavailable
		}
		args = append(args, "--ro-bind", path.path, path.path)
	}
	return args, nil
}

func appendBubblewrapReadonlySystemPath(args []string, source, destination string, required bool) ([]string, error) {
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) && !required {
		return args, nil
	}
	if err != nil {
		return nil, ErrProcessDomainUnavailable
	}
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil || resolved == string(filepath.Separator) {
		return nil, ErrProcessDomainUnavailable
	}
	resolvedInfo, err := os.Stat(resolved)
	if err != nil || !resolvedInfo.IsDir() {
		return nil, ErrProcessDomainUnavailable
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := filepath.Rel(string(filepath.Separator), resolved)
		if err != nil || target == "." || target == ".." || strings.HasPrefix(target, ".."+string(filepath.Separator)) {
			return nil, ErrProcessDomainUnavailable
		}
		return append(args, "--symlink", filepath.ToSlash(target), destination), nil
	}
	return append(args, "--ro-bind", resolved, destination), nil
}

func findTrustedExecutable(candidates []string) (string, error) {
	for _, candidate := range candidates {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if ok && stat.Uid == 0 {
			return resolved, nil
		}
	}
	return "", ErrProcessDomainUnavailable
}

func gemCredential() (*syscall.Credential, string, error) {
	account, err := user.Lookup("gem")
	if err != nil {
		return nil, "", err
	}
	credential, err := gemCredentialFromIDs(account.Uid, account.Gid)
	if err != nil {
		return nil, "", err
	}
	return credential, account.HomeDir, nil
}

func gemCredentialFromIDs(uidText, gidText string) (*syscall.Credential, error) {
	uid, err := strconv.ParseUint(uidText, 10, 32)
	if err != nil || uid == 0 {
		return nil, ErrProcessDomainUnavailable
	}
	gid, err := strconv.ParseUint(gidText, 10, 32)
	if err != nil || gid == 0 {
		return nil, ErrProcessDomainUnavailable
	}
	// A non-nil empty Groups slice makes os/exec call setgroups([]), clearing
	// inherited supplementary groups. NoSetGroups stays false, so lack of
	// permission to clear them makes Start fail closed.
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: make([]uint32, 0)}, nil
}

func shellEnvironment(cwd string) []string {
	return []string{
		"HOME=/home/gem", "USER=gem", "LOGNAME=gem", "SHELL=/bin/bash",
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin",
		"LANG=C.UTF-8", "TERM=xterm-256color", "PWD=" + cwd,
	}
}

var _ runtimecore.ActionHandler = (*ShellHandler)(nil)
