//go:build linux

package runtimeactions

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"hash"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

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
	goPath        string
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
		return nil, shellStartupUnavailable(ShellFailureOther)
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
		return nil, shellStartupUnavailable(ShellFailureWorkspaceProbe)
	}
	workInfo, err := os.Stat(workDir)
	if err != nil || !workInfo.IsDir() {
		return nil, shellStartupUnavailable(ShellFailureWorkspaceProbe)
	}

	root, err := filepath.EvalSymlinks(options.CgroupRoot)
	if err != nil {
		return nil, shellStartupUnavailable(ShellFailureCgroup)
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(root, &fs); err != nil || uint64(fs.Type) != cgroup2SuperMagic {
		return nil, shellStartupUnavailable(ShellFailureCgroup)
	}
	base := filepath.Join(root, cgroupName)
	if err := makeTrustedCgroupDir(base); err != nil {
		return nil, shellStartupUnavailable(ShellFailureCgroup)
	}
	if err := enableResourceControllers(base); err != nil {
		return nil, shellStartupUnavailable(ShellFailureCgroup)
	}
	setpriv, err := findTrustedSetpriv()
	if err != nil {
		return nil, shellStartupUnavailable(ShellFailureRequiredTool)
	}
	bwrap, err := findTrustedBubblewrap()
	if err != nil {
		return nil, shellStartupUnavailable(ShellFailureRequiredTool)
	}
	bwrapBase, err := bubblewrapBaseArguments()
	if err != nil {
		return nil, shellStartupUnavailable(ShellFailureRequiredTool)
	}
	setsid, err := findTrustedExecutable([]string{"/usr/bin/setsid", "/bin/setsid"})
	if err != nil {
		return nil, shellStartupUnavailable(ShellFailureRequiredTool)
	}
	// Go is optional for the generic shell capability. Its absence only disables
	// the trusted test adapter; it must not disable ordinary shell execution.
	goTool, _ := findTrustedExecutable([]string{"/usr/local/go/bin/go", "/usr/bin/go", "/bin/go", "/usr/local/bin/go"})
	credential, _, err := gemCredential()
	if err != nil {
		return nil, shellStartupUnavailable(ShellFailureOther)
	}
	rootInfo, err := os.Lstat(workDir)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, shellStartupUnavailable(ShellFailureWorkspaceProbe)
	}
	workspaceRoot, err := os.OpenRoot(workDir)
	if err != nil {
		return nil, shellStartupUnavailable(ShellFailureWorkspaceProbe)
	}
	openedRootInfo, err := workspaceRoot.Stat(".")
	if err != nil || !os.SameFile(rootInfo, openedRootInfo) {
		_ = workspaceRoot.Close()
		return nil, shellStartupUnavailable(ShellFailureWorkspaceProbe)
	}
	handler := &ShellHandler{
		basePath: base, bwrapPath: bwrap, bwrapBase: bwrapBase, setprivPath: setpriv, goPath: goTool,
		setsidPath: setsid, credential: credential, workDir: workDir, workspaceRoot: workspaceRoot, maxTimeout: options.MaxTimeout,
		pollInterval: options.PollInterval, outputLimit: options.MaxOutputBytes,
		parallel: make(chan struct{}, maxParallelShells), operations: make(map[string]operationRecord),
	}
	// The probe proves that bubblewrap can build the restricted mount namespace,
	// that sibling workspaces and Host secrets are hidden, and that cgroup.kill
	// removes detached descendants in this exact container.
	if err := handler.probe(); err != nil {
		_ = workspaceRoot.Close()
		return nil, shellStartupUnavailable(ShellFailureWorkspaceProbe)
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
	workspaceSnapshotBefore := ""
	if request.WorkspaceViewID != "" {
		workspaceSnapshotBefore, err = taskWorkspaceSnapshotSHA256(operationContext, workspaceRoot)
		if err != nil {
			return nil, fmt.Errorf("scan Task Workspace snapshot before shell execution: %w", err)
		}
	}
	if spec, recognized := parseShellGoTestSpec(args.Command); recognized && h.goPath != "" &&
		request.WorkspaceViewID != "" && relativeCWD == "." {
		result, receipt, runErr := h.executeTrustedGoTest(operationContext, request, args.Command, spec,
			workspaceRoot, workspaceDir, jailWorkspace, jailCWD, workspaceSnapshotBefore)
		if runErr != nil {
			return nil, runErr
		}
		workspaceSnapshotAfter, snapshotErr := taskWorkspaceSnapshotSHA256(ctx, workspaceRoot)
		if snapshotErr != nil {
			return nil, fmt.Errorf("scan Task Workspace snapshot after shell execution: %w", snapshotErr)
		}
		if receipt != nil {
			receipt.WorkspaceSnapshotSHA256After = workspaceSnapshotAfter
		}
		revision := request.WorkspaceCheckpointRevision
		encoded, marshalErr := json.Marshal(shellTaskWorkspaceResult{
			ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr,
			StdoutTruncated: result.StdoutTruncated, StderrTruncated: result.StderrTruncated,
			TimedOut: result.TimedOut, TaskID: request.TaskID, WorkspaceID: request.WorkspaceID,
			BindingVersion: request.BindingVersion, WorkspaceViewID: request.WorkspaceViewID,
			WorkspaceCheckpointRevision:   &revision,
			WorkspaceSnapshotSHA256Before: workspaceSnapshotBefore, WorkspaceSnapshotSHA256After: workspaceSnapshotAfter,
			CoverageReceipt: receipt,
		})
		if marshalErr != nil {
			return nil, ErrProcessDomainUnavailable
		}
		return encoded, nil
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
	var encoded []byte
	if request.WorkspaceViewID != "" {
		workspaceSnapshotAfter, snapshotErr := taskWorkspaceSnapshotSHA256(ctx, workspaceRoot)
		if snapshotErr != nil {
			return nil, fmt.Errorf("scan Task Workspace snapshot after shell execution: %w", snapshotErr)
		}
		revision := request.WorkspaceCheckpointRevision
		encoded, err = json.Marshal(shellTaskWorkspaceResult{
			ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr,
			StdoutTruncated: result.StdoutTruncated, StderrTruncated: result.StderrTruncated,
			TimedOut: result.TimedOut, TaskID: request.TaskID, WorkspaceID: request.WorkspaceID,
			BindingVersion: request.BindingVersion, WorkspaceViewID: request.WorkspaceViewID,
			WorkspaceCheckpointRevision:   &revision,
			WorkspaceSnapshotSHA256Before: workspaceSnapshotBefore, WorkspaceSnapshotSHA256After: workspaceSnapshotAfter,
		})
	} else {
		encoded, err = json.Marshal(result)
	}
	if err != nil {
		return nil, ErrProcessDomainUnavailable
	}
	return encoded, nil
}

const taskWorkspaceManifestDomain = "morde-task-workspace-manifest-v1"

// taskWorkspaceManifestSHA256 hashes a canonical, content-addressed manifest
// through the already-opened Workspace root. It never follows symlinks while
// walking: targets are included as data. Unsupported filesystem objects or
// concurrent changes fail closed so they cannot become an empty/partial proof.
func taskWorkspaceManifestSHA256(ctx context.Context, root *os.Root) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if root == nil {
		return "", ErrInvalidShellRequest
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	rootInfo, err := root.Stat(".")
	if err != nil || !rootInfo.IsDir() {
		return "", ErrInvalidShellRequest
	}
	manifest := sha256.New()
	writeTaskWorkspaceManifestField(manifest, 'v', []byte(taskWorkspaceManifestDomain))
	if err := writeTaskWorkspaceManifestEntry(manifest, '.', ".", rootInfo); err != nil {
		return "", err
	}
	var walk func(string) error
	walk = func(directory string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		directoryHandle, err := root.Open(directory)
		if err != nil {
			return err
		}
		entries, readErr := directoryHandle.ReadDir(-1)
		closeErr := directoryHandle.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := entry.Name()
			relative := name
			if directory != "." {
				relative = filepath.Join(directory, name)
			}
			info, err := root.Lstat(relative)
			if err != nil {
				return err
			}
			switch {
			case info.IsDir():
				if err := writeTaskWorkspaceManifestEntry(manifest, 'd', relative, info); err != nil {
					return err
				}
				if err := walk(relative); err != nil {
					return err
				}
				current, err := root.Lstat(relative)
				if err != nil || !sameTaskWorkspaceManifestStat(info, current) {
					return ErrInvalidShellRequest
				}
			case info.Mode()&os.ModeSymlink != 0:
				target, err := root.Readlink(relative)
				if err != nil {
					return err
				}
				current, err := root.Lstat(relative)
				if err != nil || !sameTaskWorkspaceManifestStat(info, current) {
					return ErrInvalidShellRequest
				}
				if err := writeTaskWorkspaceManifestEntry(manifest, 'l', relative, info); err != nil {
					return err
				}
				writeTaskWorkspaceManifestField(manifest, 't', []byte(target))
			case info.Mode().IsRegular():
				file, err := root.Open(relative)
				if err != nil {
					return err
				}
				openedInfo, err := file.Stat()
				if err != nil || !os.SameFile(info, openedInfo) || !sameTaskWorkspaceManifestStat(info, openedInfo) {
					_ = file.Close()
					return ErrInvalidShellRequest
				}
				contentDigest, hashErr := taskWorkspaceFileSHA256(ctx, file)
				finalInfo, statErr := file.Stat()
				closeErr := file.Close()
				current, currentErr := root.Lstat(relative)
				if hashErr != nil {
					return hashErr
				}
				if statErr != nil || closeErr != nil || currentErr != nil ||
					!sameTaskWorkspaceManifestStat(openedInfo, finalInfo) || !sameTaskWorkspaceManifestStat(openedInfo, current) {
					return ErrInvalidShellRequest
				}
				if err := writeTaskWorkspaceManifestEntry(manifest, 'f', relative, info); err != nil {
					return err
				}
				writeTaskWorkspaceManifestField(manifest, 'c', contentDigest)
			default:
				return ErrInvalidShellRequest
			}
		}
		return nil
	}
	if err := walk("."); err != nil {
		return "", err
	}
	finalRootInfo, err := root.Stat(".")
	if err != nil || !sameTaskWorkspaceManifestStat(rootInfo, finalRootInfo) {
		return "", ErrInvalidShellRequest
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(manifest.Sum(nil)), nil
}

func taskWorkspaceFileSHA256(ctx context.Context, file *os.File) ([]byte, error) {
	if file == nil {
		return nil, ErrInvalidShellRequest
	}
	digest := sha256.New()
	buffer := make([]byte, 128<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count, err := file.Read(buffer)
		if count > 0 {
			_, _ = digest.Write(buffer[:count])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if count == 0 {
			return nil, io.ErrNoProgress
		}
	}
	return digest.Sum(nil), nil
}

func writeTaskWorkspaceManifestEntry(manifest hash.Hash, kind byte, relative string, info os.FileInfo) error {
	if info == nil {
		return ErrInvalidShellRequest
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ErrInvalidShellRequest
	}
	writeTaskWorkspaceManifestField(manifest, 'k', []byte{kind})
	writeTaskWorkspaceManifestField(manifest, 'p', []byte(relative))
	writeTaskWorkspaceManifestField(manifest, 'm', []byte(strconv.FormatUint(uint64(info.Mode()), 10)))
	writeTaskWorkspaceManifestField(manifest, 's', []byte(strconv.FormatInt(info.Size(), 10)))
	writeTaskWorkspaceManifestField(manifest, 't', []byte(strconv.FormatInt(info.ModTime().UnixNano(), 10)))
	writeTaskWorkspaceManifestField(manifest, 'u', []byte(strconv.FormatUint(uint64(stat.Uid), 10)))
	writeTaskWorkspaceManifestField(manifest, 'g', []byte(strconv.FormatUint(uint64(stat.Gid), 10)))
	return nil
}

func writeTaskWorkspaceManifestField(manifest hash.Hash, field byte, value []byte) {
	_, _ = manifest.Write([]byte{field})
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = manifest.Write(size[:])
	_, _ = manifest.Write(value)
}

func sameTaskWorkspaceManifestStat(left, right os.FileInfo) bool {
	if left == nil || right == nil || !os.SameFile(left, right) || left.Mode() != right.Mode() ||
		left.Size() != right.Size() || !left.ModTime().Equal(right.ModTime()) {
		return false
	}
	leftStat, leftOK := left.Sys().(*syscall.Stat_t)
	rightStat, rightOK := right.Sys().(*syscall.Stat_t)
	return leftOK && rightOK && leftStat.Uid == rightStat.Uid && leftStat.Gid == rightStat.Gid
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

type shellGoStageResult struct {
	stdout          *boundedBuffer
	stderr          *boundedBuffer
	visibleStdout   *boundedBuffer
	visibleStderr   *boundedBuffer
	exitCode        int
	timedOut        bool
	clean           bool
	stdoutTruncated bool
	stderrTruncated bool
}

const maxTrustedGoPackages = 256
const maxTrustedGoTestSources = 4096
const maxTrustedGoCaptureBytes = 8 << 20

func newShellGoStageResult(visibleLimit, captureLimit int) shellGoStageResult {
	captureBudget := &outputBudget{limit: captureLimit}
	visibleBudget := &outputBudget{limit: visibleLimit}
	return shellGoStageResult{
		stdout:        &boundedBuffer{limit: captureLimit, budget: captureBudget},
		stderr:        &boundedBuffer{limit: captureLimit, budget: captureBudget},
		visibleStdout: &boundedBuffer{limit: visibleLimit, budget: visibleBudget},
		visibleStderr: &boundedBuffer{limit: visibleLimit, budget: visibleBudget},
	}
}

func (stage shellGoStageResult) stdoutWriter() io.Writer {
	return io.MultiWriter(stage.stdout, stage.visibleStdout)
}

func (stage shellGoStageResult) stderrWriter() io.Writer {
	return io.MultiWriter(stage.stderr, stage.visibleStderr)
}

type goListError struct {
	Err string
}

type goListPackage struct {
	ImportPath   string
	Dir          string
	Name         string
	TestGoFiles  []string
	XTestGoFiles []string
	Incomplete   bool
	Error        *goListError
	DepsErrors   []goListError
}

type goPackageInventory struct {
	importPath   string
	relativeDir  string
	testGoFiles  []string
	xTestGoFiles []string
}

type goSourceInventory struct {
	all         map[string]goPackageInventory
	testBearing map[string]bool
}

type goTestEvent struct {
	Action  string
	Package string
	Test    string
	Output  string
}

type goTestEventStats struct {
	passed  int
	failed  int
	skipped int
}

func successfulTrustedGoStage(stage shellGoStageResult) bool {
	return stage.exitCode == 0 && !stage.timedOut && stage.clean &&
		!stage.stdoutTruncated && !stage.stderrTruncated && !stage.stdout.truncated && !stage.stderr.truncated
}

func parseTrustedGoVersion(stdout, stderr string, exitCode int, stdoutTruncated, stderrTruncated, clean bool) (string, bool) {
	if exitCode != 0 || stdoutTruncated || stderrTruncated || !clean || strings.TrimSpace(stderr) != "" {
		return "", false
	}
	if !strings.HasSuffix(stdout, "\n") {
		return "", false
	}
	line := strings.TrimSuffix(stdout, "\n")
	if strings.Contains(line, "\n") || strings.Contains(line, "\r") {
		return "", false
	}
	fields := strings.Fields(line)
	if len(fields) != 4 || fields[0] != "go" || fields[1] != "version" || !strings.HasPrefix(fields[2], "go1.") ||
		!(fields[3] == "linux/amd64" || fields[3] == "linux/arm64") {
		return "", false
	}
	for _, character := range fields[2][2:] {
		if !(character >= '0' && character <= '9') && character != '.' && character != 'a' && character != 'b' && character != 'c' {
			return "", false
		}
	}
	return line, true
}

func parseGoSourceInventory(output, jailWorkspace, jailCWD string) (goSourceInventory, string, int, bool) {
	inventory := goSourceInventory{all: make(map[string]goPackageInventory), testBearing: make(map[string]bool)}
	decoder := json.NewDecoder(strings.NewReader(output))
	for {
		var listed goListPackage
		err := decoder.Decode(&listed)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || listed.ImportPath == "" || listed.Dir == "" || listed.Name == "" || listed.Incomplete ||
			listed.Error != nil || len(listed.DepsErrors) != 0 || len(inventory.all) >= maxTrustedGoPackages {
			return goSourceInventory{}, "", 0, false
		}
		if _, duplicate := inventory.all[listed.ImportPath]; duplicate {
			return goSourceInventory{}, "", 0, false
		}
		cleanDir := filepath.Clean(listed.Dir)
		relativeDir, err := filepath.Rel(jailCWD, cleanDir)
		if err != nil || !filepath.IsAbs(cleanDir) || relativeDir == ".." || strings.HasPrefix(relativeDir, ".."+string(filepath.Separator)) ||
			!pathWithinRoot(jailWorkspace, cleanDir) {
			return goSourceInventory{}, "", 0, false
		}
		if err := validGoListFiles(listed.TestGoFiles); err != nil {
			return goSourceInventory{}, "", 0, false
		}
		if err := validGoListFiles(listed.XTestGoFiles); err != nil {
			return goSourceInventory{}, "", 0, false
		}
		listed.TestGoFiles = append([]string(nil), listed.TestGoFiles...)
		listed.XTestGoFiles = append([]string(nil), listed.XTestGoFiles...)
		sort.Strings(listed.TestGoFiles)
		sort.Strings(listed.XTestGoFiles)
		packageInventory := goPackageInventory{importPath: listed.ImportPath, relativeDir: filepath.ToSlash(relativeDir),
			testGoFiles: listed.TestGoFiles, xTestGoFiles: listed.XTestGoFiles}
		inventory.all[listed.ImportPath] = packageInventory
		if len(listed.TestGoFiles)+len(listed.XTestGoFiles) > 0 {
			inventory.testBearing[listed.ImportPath] = true
		}
	}
	if len(inventory.all) == 0 {
		return goSourceInventory{}, "", 0, false
	}
	sourceItems := make([]string, 0, len(inventory.testBearing))
	sourceCount := 0
	for importPath := range inventory.testBearing {
		packageInventory := inventory.all[importPath]
		for _, file := range packageInventory.testGoFiles {
			sourceItems = append(sourceItems, importPath+"\x00"+packageInventory.relativeDir+"\x00test\x00"+file)
			sourceCount++
		}
		for _, file := range packageInventory.xTestGoFiles {
			sourceItems = append(sourceItems, importPath+"\x00"+packageInventory.relativeDir+"\x00xtest\x00"+file)
			sourceCount++
		}
	}
	if sourceCount == 0 || sourceCount > maxTrustedGoTestSources {
		return goSourceInventory{}, "", 0, false
	}
	return inventory, hashStringInventory(sourceItems), sourceCount, true
}

func scanGoTestSourceFiles(root *os.Root, source goSourceInventory) (map[string]map[string]bool, string, int, bool) {
	if root == nil || len(source.testBearing) == 0 {
		return nil, "", 0, false
	}
	const maxSourceFileBytes = 8 << 20
	const maxSourceTotalBytes = 64 << 20
	tests := make(map[string]map[string]bool)
	sourceItems := make([]string, 0)
	sourceCount, totalBytes := 0, 0
	for packagePath, packageInventory := range source.all {
		if !source.testBearing[packagePath] {
			continue
		}
		for _, entry := range []struct {
			kind  string
			files []string
		}{{kind: "test", files: packageInventory.testGoFiles}, {kind: "xtest", files: packageInventory.xTestGoFiles}} {
			for _, sourceFile := range entry.files {
				relativePath := filepath.Join(packageInventory.relativeDir, sourceFile)
				file, err := root.Open(relativePath)
				if err != nil {
					return nil, "", 0, false
				}
				info, err := file.Stat()
				if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxSourceFileBytes {
					_ = file.Close()
					return nil, "", 0, false
				}
				contents, readErr := io.ReadAll(io.LimitReader(file, maxSourceFileBytes+1))
				closeErr := file.Close()
				if readErr != nil || closeErr != nil || len(contents) > maxSourceFileBytes {
					return nil, "", 0, false
				}
				totalBytes += len(contents)
				if totalBytes > maxSourceTotalBytes {
					return nil, "", 0, false
				}
				parsed, parseErr := parser.ParseFile(token.NewFileSet(), relativePath, contents, parser.AllErrors)
				if parseErr != nil || parsed == nil {
					return nil, "", 0, false
				}
				aliases, dotTesting, aliasesOK := goTestingImportAliases(parsed)
				if !aliasesOK {
					return nil, "", 0, false
				}
				for _, declaration := range parsed.Decls {
					function, ok := declaration.(*ast.FuncDecl)
					if !ok || function.Recv != nil || function.Name == nil {
						continue
					}
					name := function.Name.Name
					if name == "TestMain" {
						if !validGoTestFunctionSignature(function, "T", aliases, dotTesting) {
							return nil, "", 0, false
						}
					} else if isGoTestNameCandidate(name, "Example") || isGoTestNameCandidate(name, "Benchmark") || isGoTestNameCandidate(name, "Fuzz") {
						// These runner kinds are intentionally outside this receipt contract.
						return nil, "", 0, false
					} else if !isGoTestNameCandidate(name, "Test") {
						continue
					}
					if !validGoTestName(name) || !validGoTestFunctionSignature(function, "T", aliases, dotTesting) {
						return nil, "", 0, false
					}
					if tests[packagePath] == nil {
						tests[packagePath] = make(map[string]bool)
					}
					if tests[packagePath][name] {
						return nil, "", 0, false
					}
					tests[packagePath][name] = true
				}
				digest := sha256.Sum256(contents)
				sourceItems = append(sourceItems, packagePath+"\x00"+packageInventory.relativeDir+"\x00"+entry.kind+"\x00"+sourceFile+"\x00"+hex.EncodeToString(digest[:]))
				sourceCount++
			}
		}
	}
	if sourceCount == 0 || sourceCount > maxTrustedGoTestSources || len(flattenGoTestInventory(tests)) == 0 {
		return nil, "", 0, false
	}
	return tests, hashStringInventory(sourceItems), sourceCount, true
}

func goTestingImportAliases(file *ast.File) (map[string]bool, bool, bool) {
	aliases := make(map[string]bool)
	dotTesting := false
	for _, imported := range file.Imports {
		pathValue, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			return nil, false, false
		}
		if pathValue != "testing" {
			continue
		}
		if imported.Name == nil {
			aliases["testing"] = true
			continue
		}
		switch imported.Name.Name {
		case ".":
			dotTesting = true
		case "_":
			return nil, false, false
		default:
			aliases[imported.Name.Name] = true
		}
	}
	return aliases, dotTesting, true
}

func validGoTestFunctionSignature(function *ast.FuncDecl, testType string, aliases map[string]bool, dotTesting bool) bool {
	if function == nil || function.Type == nil || function.Type.TypeParams != nil && len(function.Type.TypeParams.List) != 0 ||
		function.Type.Results != nil && len(function.Type.Results.List) != 0 || function.Type.Params == nil ||
		len(function.Type.Params.List) != 1 || len(function.Type.Params.List[0].Names) > 1 {
		return false
	}
	pointer, ok := function.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	switch target := pointer.X.(type) {
	case *ast.Ident:
		return target.Name == testType && dotTesting
	case *ast.SelectorExpr:
		packageName, ok := target.X.(*ast.Ident)
		return ok && target.Sel.Name == testType && aliases[packageName.Name]
	default:
		return false
	}
}

func isGoTestNameCandidate(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(name, prefix)
	if suffix == "" {
		return true
	}
	runeValue, _ := utf8.DecodeRuneInString(suffix)
	return !unicode.IsLower(runeValue)
}

func validGoListFiles(files []string) error {
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		if file == "" || filepath.Base(file) != file || !strings.HasSuffix(file, ".go") || file == ".go" || seen[file] {
			return ErrInvalidShellRequest
		}
		seen[file] = true
	}
	return nil
}

func pathWithinRoot(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func parseGoTestRun(output string, discovered map[string]map[string]bool, source goSourceInventory) (map[string]map[string]bool, map[string]bool, goTestEventStats, bool) {
	executed := make(map[string]map[string]bool)
	packageTerminal := make(map[string]string)
	packageStarted := make(map[string]bool)
	testState := make(map[string]string)
	stats := goTestEventStats{}
	decoder := json.NewDecoder(strings.NewReader(output))
	for {
		var event goTestEvent
		err := decoder.Decode(&event)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || event.Action == "" || event.Package == "" {
			return nil, nil, stats, false
		}
		if _, exists := source.all[event.Package]; !exists {
			return nil, nil, stats, false
		}
		if packageTerminal[event.Package] != "" {
			return nil, nil, stats, false
		}
		if event.Action == "start" {
			if event.Test != "" || packageStarted[event.Package] {
				return nil, nil, stats, false
			}
			packageStarted[event.Package] = true
			continue
		}
		if !packageStarted[event.Package] {
			return nil, nil, stats, false
		}
		if event.Test == "" {
			switch event.Action {
			case "output":
				line := strings.TrimSuffix(event.Output, "\n")
				if !strings.HasSuffix(event.Output, "\n") || strings.Count(event.Output, "\n") != 1 || strings.Contains(event.Output, "\r") ||
					!(line == "PASS" || isGoTestSummaryLine(line, event.Package, true)) {
					return nil, nil, stats, false
				}
			case "pass":
				if packageTerminal[event.Package] != "" {
					return nil, nil, stats, false
				}
				packageTerminal[event.Package] = "pass"
			case "skip":
				if packageTerminal[event.Package] != "" {
					return nil, nil, stats, false
				}
				packageTerminal[event.Package] = "skip"
			case "fail":
				return nil, nil, stats, false
			default:
				return nil, nil, stats, false
			}
			continue
		}
		if !source.testBearing[event.Package] {
			return nil, nil, stats, false
		}
		rootName := strings.SplitN(event.Test, "/", 2)[0]
		if !discovered[event.Package][rootName] {
			return nil, nil, stats, false
		}
		key := event.Package + "\x00" + event.Test
		switch event.Action {
		case "run":
			if testState[key] != "" {
				return nil, nil, stats, false
			}
			testState[key] = "running"
			if event.Test == rootName {
				if executed[event.Package] == nil {
					executed[event.Package] = make(map[string]bool)
				}
				if executed[event.Package][rootName] {
					return nil, nil, stats, false
				}
				executed[event.Package][rootName] = true
			}
		case "pause":
			if testState[key] != "running" {
				return nil, nil, stats, false
			}
			testState[key] = "paused"
		case "cont":
			if testState[key] != "paused" {
				return nil, nil, stats, false
			}
			testState[key] = "running"
		case "output":
			if testState[key] != "running" && testState[key] != "paused" {
				return nil, nil, stats, false
			}
		case "pass":
			if testState[key] != "running" && testState[key] != "paused" {
				return nil, nil, stats, false
			}
			testState[key] = "pass"
			stats.passed++
		case "fail":
			if testState[key] != "running" && testState[key] != "paused" {
				return nil, nil, stats, false
			}
			testState[key] = "fail"
			stats.failed++
		case "skip":
			if testState[key] != "running" && testState[key] != "paused" {
				return nil, nil, stats, false
			}
			testState[key] = "skip"
			stats.skipped++
		default:
			return nil, nil, stats, false
		}
	}
	for _, state := range testState {
		if state == "running" || state == "paused" {
			return nil, nil, stats, false
		}
	}
	for packagePath := range source.all {
		terminal := packageTerminal[packagePath]
		if source.testBearing[packagePath] {
			if terminal != "pass" || !sameStringSet(mapKeysBool(discovered[packagePath]), mapKeysBool(executed[packagePath])) {
				return nil, nil, stats, false
			}
		} else if terminal != "pass" && terminal != "skip" {
			return nil, nil, stats, false
		}
	}
	packageTerminalIDs := make([]string, 0, len(packageTerminal))
	for packagePath := range packageTerminal {
		packageTerminalIDs = append(packageTerminalIDs, packagePath)
	}
	sourcePackageIDs := make([]string, 0, len(source.all))
	for packagePath := range source.all {
		sourcePackageIDs = append(sourcePackageIDs, packagePath)
	}
	if !sameStringSet(packageTerminalIDs, sourcePackageIDs) {
		return nil, nil, stats, false
	}
	observedTestPackages := make(map[string]bool)
	for packagePath := range packageTerminal {
		if source.testBearing[packagePath] {
			observedTestPackages[packagePath] = true
		}
	}
	return executed, observedTestPackages, stats, true
}

func selectGoTestRoot(discovered map[string]map[string]bool, testName string) map[string]map[string]bool {
	selected := make(map[string]map[string]bool)
	for packagePath, tests := range discovered {
		if tests[testName] {
			selected[packagePath] = map[string]bool{testName: true}
		}
	}
	return selected
}

func isGoTestSummaryLine(line, packagePath string, includePass bool) bool {
	if includePass && line == "PASS" {
		return true
	}
	fields := strings.Fields(line)
	if len(fields) == 3 && fields[0] == "ok" && fields[1] == packagePath {
		_, err := time.ParseDuration(fields[2])
		return err == nil
	}
	if len(fields) >= 5 && fields[0] == "?" && fields[1] == packagePath && strings.Join(fields[2:], " ") == "[no test files]" {
		return true
	}
	return false
}

func flattenGoTestInventory(inventory map[string]map[string]bool) []string {
	items := make([]string, 0)
	for packagePath, tests := range inventory {
		for testName := range tests {
			items = append(items, packagePath+"\x00"+testName)
		}
	}
	sort.Strings(items)
	return items
}

func sortedGoInventoryKeys(inventory map[string]bool) []string {
	items := make([]string, 0, len(inventory))
	for item := range inventory {
		items = append(items, item)
	}
	sort.Strings(items)
	return items
}

func mapKeysBool(values map[string]bool) []string {
	return sortedGoInventoryKeys(values)
}

func cloneBoolSet(values map[string]bool) map[string]bool {
	clone := make(map[string]bool, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := append([]string(nil), left...)
	rightCopy := append([]string(nil), right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	for index := range leftCopy {
		if leftCopy[index] != rightCopy[index] {
			return false
		}
	}
	return true
}

func hashStringInventory(items []string) string {
	values := append([]string(nil), items...)
	sort.Strings(values)
	digest := sha256.New()
	for _, item := range values {
		writeTaskWorkspaceManifestField(digest, 'i', []byte(item))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func (h *ShellHandler) goCommand(argv []string, workspaceDir *os.File, jailWorkspace, jailCWD string) *exec.Cmd {
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
		"--setenv", "GOENV", "off",
		"--setenv", "GOTOOLCHAIN", "local",
		"--setenv", "GOPROXY", "off",
		"--setenv", "GOSUMDB", "off",
		"--setenv", "GOCACHE", "/tmp/morde-go-build-cache",
		"--setenv", "GOPATH", "/tmp/morde-go-path",
		"--setenv", "GOMODCACHE", "/tmp/morde-go-path/pkg/mod",
	)
	relativeJail, _ := filepath.Rel("/home/gem/workspace", jailWorkspace)
	parent := "/home/gem/workspace"
	for _, component := range strings.Split(relativeJail, string(filepath.Separator)) {
		parent = filepath.Join(parent, component)
		commandArgs = append(commandArgs, "--dir", parent)
	}
	commandArgs = append(commandArgs, "--bind", "/proc/self/fd/3", jailWorkspace,
		"--chdir", jailCWD, "--", h.setprivPath, "--no-new-privs", "--", h.goPath)
	commandArgs = append(commandArgs, argv...)
	command := exec.Command(h.bwrapPath, commandArgs...)
	command.ExtraFiles = []*os.File{workspaceDir}
	return command
}

func (h *ShellHandler) executeTrustedGoTest(ctx context.Context, request contract.ExecuteRequest, exactCommand string,
	spec shellGoTestSpec, workspaceRoot *os.Root, workspaceDir *os.File, jailWorkspace, jailCWD, snapshotBefore string) (shellResult, *shellRunnerCoverageReceipt, error) {
	noReceipt := func(stage shellGoStageResult) (shellResult, *shellRunnerCoverageReceipt, error) {
		return shellResult{ExitCode: stage.exitCode, Stdout: stage.visibleStdout.String(), Stderr: stage.visibleStderr.String(),
			StdoutTruncated: stage.visibleStdout.truncated, StderrTruncated: stage.visibleStderr.truncated, TimedOut: stage.timedOut}, nil, nil
	}
	run := func(name string, argv []string) (shellGoStageResult, error) {
		return h.runTrustedGoStage(ctx, request.OperationID, name, argv, workspaceDir, jailWorkspace, jailCWD)
	}
	runWithoutReceipt := func() (shellResult, *shellRunnerCoverageReceipt, error) {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return shellResult{ExitCode: -1, TimedOut: true}, nil, nil
		}
		if ctx.Err() != nil {
			return shellResult{}, nil, ErrShellOperationCancelled
		}
		argv := []string{"test", "-json"}
		if spec.TestSelector != "" {
			argv = append(argv, "-run="+spec.TestSelector)
		}
		argv = append(argv, spec.PackageSelector)
		stage, err := run("execute-tests", argv)
		if err != nil {
			return shellResult{}, nil, err
		}
		return noReceipt(stage)
	}
	versionStage, err := run("version", []string{"version"})
	if err != nil {
		return shellResult{}, nil, err
	}
	version, versionOK := parseTrustedGoVersion(versionStage.stdout.String(), versionStage.stderr.String(), versionStage.exitCode,
		versionStage.stdoutTruncated, versionStage.stderrTruncated, versionStage.clean)
	if !versionOK {
		return runWithoutReceipt()
	}
	listStage, err := run("packages", []string{"list", "-json", spec.PackageSelector})
	if err != nil {
		return shellResult{}, nil, err
	}
	if !successfulTrustedGoStage(listStage) {
		return runWithoutReceipt()
	}
	sourceInventory, _, _, inventoryOK := parseGoSourceInventory(listStage.stdout.String(), jailWorkspace, jailCWD)
	if !inventoryOK || len(sourceInventory.testBearing) == 0 || spec.ScopeMode == "targeted" && len(sourceInventory.all) != 1 {
		return runWithoutReceipt()
	}
	discoveredTests, sourceSHA, sourceCount, sourceFilesOK := scanGoTestSourceFiles(workspaceRoot, sourceInventory)
	if !sourceFilesOK || len(sourceInventory.testBearing) == 0 {
		return runWithoutReceipt()
	}
	discoveredTestIDs := flattenGoTestInventory(discoveredTests)
	discoveredPackages := cloneBoolSet(sourceInventory.testBearing)
	if spec.TestSelector != "" {
		name := strings.TrimSuffix(strings.TrimPrefix(spec.TestSelector, "^"), "$")
		expectedTests := selectGoTestRoot(discoveredTests, name)
		if len(flattenGoTestInventory(expectedTests)) == 0 {
			return runWithoutReceipt()
		}
		discoveredTests = expectedTests
		discoveredTestIDs = flattenGoTestInventory(discoveredTests)
	}
	if len(discoveredTestIDs) == 0 {
		return runWithoutReceipt()
	}
	runArgs := []string{"test", "-json"}
	if spec.TestSelector != "" {
		runArgs = append(runArgs, "-run="+spec.TestSelector)
	}
	runArgs = append(runArgs, spec.PackageSelector)
	testStage, err := run("execute-tests", runArgs)
	if err != nil {
		return shellResult{}, nil, err
	}
	result := shellResult{ExitCode: testStage.exitCode, Stdout: testStage.visibleStdout.String(), Stderr: testStage.visibleStderr.String(),
		StdoutTruncated: testStage.visibleStdout.truncated, StderrTruncated: testStage.visibleStderr.truncated, TimedOut: testStage.timedOut}
	if !successfulTrustedGoStage(testStage) {
		return result, nil, nil
	}
	executedTests, executedPackages, testStats, runOK := parseGoTestRun(testStage.stdout.String(), discoveredTests, sourceInventory)
	if !runOK || len(executedTests) == 0 || len(executedPackages) == 0 || testStats.failed != 0 || testStats.skipped != 0 {
		return result, nil, nil
	}
	executedTestIDs := flattenGoTestInventory(executedTests)
	discoveredPackageIDs := sortedGoInventoryKeys(discoveredPackages)
	executedPackageIDs := sortedGoInventoryKeys(executedPackages)
	if !sameStringSet(discoveredTestIDs, executedTestIDs) || !sameStringSet(discoveredPackageIDs, executedPackageIDs) {
		return result, nil, nil
	}
	postTests, postSourceSHA, postSourceCount, postSourcesOK := scanGoTestSourceFiles(workspaceRoot, sourceInventory)
	if !postSourcesOK || postSourceSHA != sourceSHA || postSourceCount != sourceCount ||
		!sameStringSet(discoveredTestIDs, flattenGoTestInventory(postTests)) {
		return result, nil, nil
	}
	commandDigest := sha256.Sum256([]byte(exactCommand))
	receipt := &shellRunnerCoverageReceipt{
		Version: 1, Runner: "go", RunnerVersion: version, ScopeMode: spec.ScopeMode,
		ScopeContract:   "go_test_test_functions_only; examples/benchmarks/fuzz-target unsupported",
		PackageSelector: spec.PackageSelector, TestSelector: spec.TestSelector,
		ExactCommandSHA256: hex.EncodeToString(commandDigest[:]), OperationID: request.OperationID,
		TaskID: request.TaskID, WorkspaceID: request.WorkspaceID,
		WorkspaceDirectory: request.WorkspaceDirectory, BindingVersion: request.BindingVersion,
		WorkspaceViewID: request.WorkspaceViewID, WorkspaceCheckpointRevision: request.WorkspaceCheckpointRevision,
		WorkspaceCWD: ".", WorkspaceSnapshotSHA256Before: snapshotBefore,
		DiscoveredSourceInventorySHA256: sourceSHA, DiscoveredSourceInventoryCount: sourceCount,
		DiscoveredPackageInventorySHA256: hashStringInventory(discoveredPackageIDs), DiscoveredPackageInventoryCount: len(discoveredPackageIDs),
		ExecutedPackageInventorySHA256: hashStringInventory(executedPackageIDs), ExecutedPackageInventoryCount: len(executedPackageIDs),
		DiscoveredTestInventorySHA256: hashStringInventory(discoveredTestIDs), DiscoveredTestInventoryCount: len(discoveredTestIDs),
		ExecutedTestInventorySHA256: hashStringInventory(executedTestIDs), ExecutedTestInventoryCount: len(executedTestIDs),
		PassedTestCount: testStats.passed, FailedTestCount: testStats.failed, SkippedTestCount: testStats.skipped,
		ParserComplete: true,
	}
	return result, receipt, nil
}

func (h *ShellHandler) runTrustedGoStage(ctx context.Context, operationID, stageName string, argv []string,
	workspaceDir *os.File, jailWorkspace, jailCWD string) (shellGoStageResult, error) {
	stage := newShellGoStageResult(h.outputLimit, maxTrustedGoCaptureBytes)
	if ctx.Err() != nil {
		return stage, ErrShellOperationCancelled
	}
	groupPath := h.groupPath(operationID + "-go-" + stageName)
	h.mu.Lock()
	state, exists := h.operations[operationID]
	if !exists || state.phase == operationStopRequested || state.phase == operationStoppedBeforeExecute || state.phase == operationComplete {
		h.mu.Unlock()
		return stage, ErrShellOperationCancelled
	}
	if state.groupPath != "" {
		h.mu.Unlock()
		return stage, ErrProcessDomainUnavailable
	}
	if err := createOperationGroup(groupPath); err != nil {
		h.mu.Unlock()
		return stage, ErrProcessDomainUnavailable
	}
	command := h.goCommand(argv, workspaceDir, jailWorkspace, jailCWD)
	command.Dir = string(filepath.Separator)
	command.Env = []string{"PATH=/usr/bin:/bin"}
	command.Stdout = stage.stdoutWriter()
	command.Stderr = stage.stderrWriter()
	group, err := os.Open(groupPath)
	if err != nil {
		h.mu.Unlock()
		_ = os.Remove(groupPath)
		return stage, ErrProcessDomainUnavailable
	}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: h.credential, UseCgroupFD: true, CgroupFD: int(group.Fd())}
	if err := command.Start(); err != nil {
		_ = group.Close()
		h.mu.Unlock()
		_ = os.Remove(groupPath)
		return stage, ErrProcessDomainUnavailable
	}
	_ = group.Close()
	state.phase = operationRunning
	state.groupPath = groupPath
	h.operations[operationID] = state
	h.mu.Unlock()

	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	var waitErr error
	select {
	case waitErr = <-wait:
	case <-ctx.Done():
		stage.timedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		h.killUntilEmpty(groupPath)
		waitErr = <-wait
	}
	if ctx.Err() != nil {
		stage.timedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	}
	waitFailure := false
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			stage.exitCode = exitErr.ExitCode()
		} else {
			waitFailure = true
		}
	}
	populated, populatedErr := cgroupPopulated(filepath.Join(groupPath, "cgroup.events"))
	stage.clean = populatedErr == nil && !populated && ctx.Err() == nil && !waitFailure
	if populatedErr != nil || populated || ctx.Err() != nil || waitFailure {
		h.killUntilEmpty(groupPath)
	} else {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), maxStopCleanup)
		cleanupErr := h.killAndWait(cleanupCtx, groupPath)
		cancel()
		if cleanupErr != nil {
			stage.clean = false
			h.killUntilEmpty(groupPath)
		}
	}
	stage.stdoutTruncated = stage.stdout.truncated
	stage.stderrTruncated = stage.stderr.truncated
	h.mu.Lock()
	state = h.operations[operationID]
	if state.groupPath == groupPath {
		state.groupPath = ""
		h.operations[operationID] = state
	}
	h.mu.Unlock()
	if waitFailure {
		return stage, ErrProcessDomainUnavailable
	}
	if ctx.Err() != nil && !stage.timedOut {
		return stage, ErrShellOperationCancelled
	}
	return stage, nil
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
