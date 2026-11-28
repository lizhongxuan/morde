// Package runtimeactions contains fail-closed handlers for managed runtime actions.
package runtimeactions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	mordesync "morde-sync"
)

const (
	shellCapability     = "shell.exec"
	shellAction         = "run"
	defaultTimeout      = 30 * time.Second
	maxShellTimeout     = 5 * time.Minute
	defaultOutputLimit  = 64 << 10
	maxCommandBytes     = 64 << 10
	maxArgumentBytes    = 1 << 20
	defaultShellWorkDir = "/home/gem/workspace"
)

var (
	ErrInvalidShellRequest      = errors.New("invalid shell request")
	ErrProcessDomainUnavailable = errors.New("verified process control unavailable")
	ErrShellOperationCancelled  = errors.New("shell operation cancelled")
)

func taskWorkspaceSnapshotSHA256(ctx context.Context, root *os.Root) (string, error) {
	return taskWorkspaceSnapshotSHA256WithScanner(ctx, root, func(ctx context.Context, root *os.Root) (mordesync.Manifest, error) {
		return mordesync.ScanOpenedRoot(ctx, root, nil)
	})
}

func taskWorkspaceSnapshotSHA256WithScanner(ctx context.Context, root *os.Root,
	scan func(context.Context, *os.Root) (mordesync.Manifest, error)) (string, error) {
	if scan == nil {
		return "", errors.New("Task Workspace snapshot scanner is required")
	}
	manifest, err := scan(ctx, root)
	if err != nil {
		return "", err
	}
	digest, err := mordesync.TaskWorkspaceSnapshotSHA256(manifest)
	if err != nil {
		return "", err
	}
	return digest, nil
}

// shellTaskWorkspaceResult carries the shared v2 digest of the exact private
// Task Workspace before and after shell execution. It intentionally contains
// no file names or contents. Legacy manifest fields remain decodable below,
// but new results leave them empty so they are omitted from JSON.
type shellTaskWorkspaceResult struct {
	ExitCode                      int                         `json:"exit_code"`
	Stdout                        string                      `json:"stdout"`
	Stderr                        string                      `json:"stderr"`
	StdoutTruncated               bool                        `json:"stdout_truncated"`
	StderrTruncated               bool                        `json:"stderr_truncated"`
	TimedOut                      bool                        `json:"timed_out"`
	TaskID                        string                      `json:"task_id"`
	WorkspaceID                   string                      `json:"workspace_id"`
	BindingVersion                int64                       `json:"binding_version"`
	WorkspaceViewID               string                      `json:"workspace_view_id"`
	WorkspaceCheckpointRevision   *int64                      `json:"workspace_checkpoint_revision"`
	WorkspaceManifestSHA256Before string                      `json:"workspace_manifest_sha256_before,omitempty"`
	WorkspaceManifestSHA256After  string                      `json:"workspace_manifest_sha256_after,omitempty"`
	WorkspaceSnapshotSHA256Before string                      `json:"workspace_snapshot_sha256_before"`
	WorkspaceSnapshotSHA256After  string                      `json:"workspace_snapshot_sha256_after"`
	CoverageReceipt               *shellRunnerCoverageReceipt `json:"coverage_receipt,omitempty"`
}

// shellRunnerCoverageReceipt is emitted only for the Runtime's direct, bounded
// Go test adapter. It is evidence about the runner's discovered and executed
// test roots and package terminals; it is not inferred from a command string,
// exit code, or ordinary shell output. Consumers must independently validate
// every fence and inventory field.
type shellRunnerCoverageReceipt struct {
	Version                          int    `json:"version"`
	Runner                           string `json:"runner"`
	RunnerVersion                    string `json:"runner_version"`
	ScopeMode                        string `json:"scope_mode"`
	ScopeContract                    string `json:"scope_contract"`
	PackageSelector                  string `json:"package_selector"`
	TestSelector                     string `json:"test_selector,omitempty"`
	ExactCommandSHA256               string `json:"exact_command_sha256"`
	OperationID                      string `json:"operation_id"`
	TaskID                           string `json:"task_id"`
	WorkspaceID                      string `json:"workspace_id"`
	WorkspaceDirectory               string `json:"workspace_directory"`
	BindingVersion                   int64  `json:"binding_version"`
	WorkspaceViewID                  string `json:"workspace_view_id"`
	WorkspaceCheckpointRevision      int64  `json:"workspace_checkpoint_revision"`
	WorkspaceCWD                     string `json:"workspace_cwd"`
	WorkspaceSnapshotSHA256Before    string `json:"workspace_snapshot_sha256_before"`
	WorkspaceSnapshotSHA256After     string `json:"workspace_snapshot_sha256_after"`
	DiscoveredSourceInventorySHA256  string `json:"discovered_source_inventory_sha256"`
	DiscoveredSourceInventoryCount   int    `json:"discovered_source_inventory_count"`
	DiscoveredPackageInventorySHA256 string `json:"discovered_package_inventory_sha256"`
	DiscoveredPackageInventoryCount  int    `json:"discovered_package_inventory_count"`
	ExecutedPackageInventorySHA256   string `json:"executed_package_inventory_sha256"`
	ExecutedPackageInventoryCount    int    `json:"executed_package_inventory_count"`
	DiscoveredTestInventorySHA256    string `json:"discovered_test_inventory_sha256"`
	DiscoveredTestInventoryCount     int    `json:"discovered_test_inventory_count"`
	ExecutedTestInventorySHA256      string `json:"executed_test_inventory_sha256"`
	ExecutedTestInventoryCount       int    `json:"executed_test_inventory_count"`
	PassedTestCount                  int    `json:"passed_test_count"`
	FailedTestCount                  int    `json:"failed_test_count"`
	SkippedTestCount                 int    `json:"skipped_test_count"`
	ParserComplete                   bool   `json:"parser_complete"`
}

type shellGoTestSpec struct {
	ScopeMode       string
	PackageSelector string
	TestSelector    string
}

// parseShellGoTestSpec recognizes only the adapter's exact command grammar.
// Everything else stays on the general shell path and can never receive a
// runner coverage receipt.
func parseShellGoTestSpec(command string) (shellGoTestSpec, bool) {
	if command == "go test ./..." {
		return shellGoTestSpec{ScopeMode: "full_suite", PackageSelector: "./..."}, true
	}
	fields := strings.Fields(command)
	testSelector := ""
	if len(fields) == 4 {
		testSelector = strings.TrimPrefix(fields[2], "-run=")
	}
	if len(fields) != 4 || strings.Join(fields, " ") != command || fields[0] != "go" || fields[1] != "test" ||
		!validGoTestPackageSelector(fields[3]) || !strings.HasPrefix(fields[2], "-run=^") || !strings.HasSuffix(fields[2], "$") {
		return shellGoTestSpec{}, false
	}
	testName := strings.TrimSuffix(strings.TrimPrefix(testSelector, "^"), "$")
	if !validGoTestName(testName) || !strings.HasPrefix(testName, "Test") || len(testName) == len("Test") {
		return shellGoTestSpec{}, false
	}
	if testName[len("Test")] < 'A' || testName[len("Test")] > 'Z' {
		return shellGoTestSpec{}, false
	}
	return shellGoTestSpec{ScopeMode: "targeted", PackageSelector: fields[3], TestSelector: testSelector}, true
}

func validGoTestPackageSelector(value string) bool {
	if value == "." {
		return true
	}
	if !strings.HasPrefix(value, "./") || strings.HasSuffix(value, "/") || strings.Contains(value, "//") || strings.Contains(value, "...") {
		return false
	}
	for _, component := range strings.Split(strings.TrimPrefix(value, "./"), "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
		for _, character := range component {
			if !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') &&
				!(character >= '0' && character <= '9') && character != '_' && character != '-' && character != '.' {
				return false
			}
		}
	}
	return true
}

func validGoTestName(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') && character != '_' {
			return false
		}
	}
	return true
}

type ShellFailureCategory string

const (
	ShellFailureOther          ShellFailureCategory = "other"
	ShellFailureCgroup         ShellFailureCategory = "cgroup"
	ShellFailureRequiredTool   ShellFailureCategory = "required_tool"
	ShellFailureWorkspaceProbe ShellFailureCategory = "workspace_or_probe"
)

type shellStartupFailure struct{ category ShellFailureCategory }

func (e *shellStartupFailure) Error() string { return ErrProcessDomainUnavailable.Error() }

func (e *shellStartupFailure) Unwrap() error { return ErrProcessDomainUnavailable }

func shellStartupUnavailable(category ShellFailureCategory) error {
	switch category {
	case ShellFailureCgroup, ShellFailureRequiredTool, ShellFailureWorkspaceProbe:
		return &shellStartupFailure{category: category}
	default:
		return &shellStartupFailure{category: ShellFailureOther}
	}
}

// ShellFailureCategoryOf returns only a static startup category; it never exposes the underlying failure text.
func ShellFailureCategoryOf(err error) ShellFailureCategory {
	var failure *shellStartupFailure
	if !errors.As(err, &failure) {
		return ShellFailureOther
	}
	switch failure.category {
	case ShellFailureCgroup, ShellFailureRequiredTool, ShellFailureWorkspaceProbe:
		return failure.category
	default:
		return ShellFailureOther
	}
}

// ShellOptions bounds a shell handler and points it at the delegated cgroup v2
// mount. The action always runs as the fixed, non-root gem account.
type ShellOptions struct {
	CgroupRoot     string
	DefaultWorkDir string
	MaxTimeout     time.Duration
	PollInterval   time.Duration
	MaxOutputBytes int
}

// ApplicationProcessDomain starts the fixed native application inside a
// per-launch cgroup v2 and reports only kernel-observed population state.
type ApplicationProcessDomain interface {
	Prepare(launchID string) error
	Start(launchID string, command *exec.Cmd, uid, gid uint32) error
	Populated(launchID string) (bool, error)
	Stop(context.Context, string) error
	RemoveEmpty(string) error
}

type ApplicationProcessDomainOptions struct {
	CgroupRoot   string
	PollInterval time.Duration
}

type shellArguments struct {
	Command string `json:"command"`
	CWD     string `json:"cwd,omitempty"`
	Timeout int64  `json:"-"`
}

type shellArgumentPayload struct {
	Command string          `json:"command"`
	CWD     string          `json:"cwd,omitempty"`
	Timeout json.RawMessage `json:"timeout,omitempty"`
}

type shellResult struct {
	ExitCode        int    `json:"exit_code"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
	TimedOut        bool   `json:"timed_out"`
}

func parseShellArguments(raw json.RawMessage, maxTimeout time.Duration) (shellArguments, time.Duration, error) {
	var payload shellArgumentPayload
	if len(raw) == 0 || len(raw) > maxArgumentBytes || maxTimeout <= 0 || maxTimeout > maxShellTimeout {
		return shellArguments{}, 0, ErrInvalidShellRequest
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil || decoder.Decode(new(any)) != io.EOF {
		return shellArguments{}, 0, ErrInvalidShellRequest
	}
	args := shellArguments{Command: payload.Command, CWD: payload.CWD}
	if args.Command == "" || strings.TrimSpace(args.Command) == "" || len(args.Command) > maxCommandBytes || strings.IndexByte(args.Command, 0) >= 0 {
		return shellArguments{}, 0, ErrInvalidShellRequest
	}
	if args.CWD != "" && (!filepath.IsAbs(args.CWD) || strings.IndexByte(args.CWD, 0) >= 0) {
		return shellArguments{}, 0, ErrInvalidShellRequest
	}
	if len(payload.Timeout) == 0 {
		return args, min(defaultTimeout, maxTimeout), nil
	}
	if err := json.Unmarshal(payload.Timeout, &args.Timeout); err != nil || args.Timeout < 1 || args.Timeout > int64(maxTimeout/time.Second) {
		return shellArguments{}, 0, ErrInvalidShellRequest
	}
	return args, time.Duration(args.Timeout) * time.Second, nil
}

func validOperationID(value string) bool {
	if len(value) != 8 || value[0] != 'o' {
		return false
	}
	for index := 1; index < len(value); index++ {
		c := value[index]
		if c < 'a' || c > 'z' {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

type boundedBuffer struct {
	limit     int
	budget    *outputBudget
	buffer    bytes.Buffer
	truncated bool
}

// outputBudget applies one combined retention limit to stdout and stderr.
// exec.Cmd may copy the two streams concurrently, so the shared counter and
// each writer's buffer are updated under the same lock.
type outputBudget struct {
	mu    sync.Mutex
	limit int
	used  int
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	if b.budget != nil {
		b.budget.mu.Lock()
		defer b.budget.mu.Unlock()
	}
	remaining := b.limit - b.buffer.Len()
	if b.budget != nil && b.budget.limit-b.budget.used < remaining {
		remaining = b.budget.limit - b.budget.used
	}
	if remaining > 0 {
		if len(value) > remaining {
			_, _ = b.buffer.Write(value[:remaining])
			if b.budget != nil {
				b.budget.used += remaining
			}
			b.truncated = true
		} else {
			_, _ = b.buffer.Write(value)
			if b.budget != nil {
				b.budget.used += len(value)
			}
		}
	} else if len(value) > 0 {
		b.truncated = true
	}
	// Keep draining the child's pipe after the retained output reaches its cap.
	return len(value), nil
}

func (b *boundedBuffer) String() string { return b.buffer.String() }
