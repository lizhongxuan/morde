// Package contract defines the stable JSON wire types for morde-runtime v1.
package contract

import (
	"encoding/json"
	"strings"
	"time"
)

const (
	ProtocolVersion      = "2.0"
	ProtocolHeader       = "X-Morde-Runtime-Protocol"
	MaxTaskLeaseDuration = 30 * time.Second
)

type Mode string

const (
	ModeBlocked Mode = "blocked"
	ModeAI      Mode = "ai"
	ModeHuman   Mode = "human"
)

type OperationStatus string

const (
	OperationAccepted   OperationStatus = "accepted"
	OperationRunning    OperationStatus = "running"
	OperationSucceeded  OperationStatus = "succeeded"
	OperationFailed     OperationStatus = "failed"
	OperationCancelling OperationStatus = "cancelling"
	OperationCancelled  OperationStatus = "cancelled"
	OperationUnknown    OperationStatus = "unknown"
)

// WorkspaceScopedCapability identifies actions that must name an active
// Workspace binding for every operation.
func WorkspaceScopedCapability(capability string) bool {
	return strings.HasPrefix(capability, "file.") || capability == "shell.exec"
}

type QuiesceStatus string

const (
	QuiesceDraining QuiesceStatus = "draining"
	QuiesceDrained  QuiesceStatus = "drained"
	QuiesceFailed   QuiesceStatus = "failed"
)

// HostFence is present on every authenticated request after capabilities
// handshake. CapabilitiesRequest deliberately uses its own type because it
// must not carry a runtime instance ID.
type HostFence struct {
	HostID            string `json:"host_id"`
	RuntimeGeneration int64  `json:"runtime_generation"`
	RuntimeInstanceID string `json:"runtime_instance_id"`
	ControlVersion    int64  `json:"control_version"`
}

type CapabilitiesRequest struct {
	ProtocolVersion   string `json:"protocol_version"`
	HostID            string `json:"host_id"`
	RuntimeGeneration int64  `json:"runtime_generation"`
	ControlVersion    int64  `json:"control_version"`
}

type CapabilitiesResponse struct {
	ProtocolVersion     string   `json:"protocol_version"`
	HostID              string   `json:"host_id"`
	RuntimeGeneration   int64    `json:"runtime_generation"`
	RuntimeInstanceID   string   `json:"runtime_instance_id"`
	ControlVersion      int64    `json:"control_version"`
	Mode                Mode     `json:"mode"`
	OperationStoreReady bool     `json:"operation_store_ready"`
	Capabilities        []string `json:"capabilities"`
}

type ApplicationLoginStatus string

const (
	ApplicationLoginLoggedIn      ApplicationLoginStatus = "logged_in"
	ApplicationLoginNotLoggedIn   ApplicationLoginStatus = "not_logged_in"
	ApplicationLoginUnknown       ApplicationLoginStatus = "unknown"
	ApplicationLoginNotApplicable ApplicationLoginStatus = "not_applicable"
)

type ApplicationStatusItem struct {
	ID               string                       `json:"id"`
	Installed        bool                         `json:"installed"`
	Version          string                       `json:"version"`
	LoginStatus      ApplicationLoginStatus       `json:"login_status"`
	Installable      bool                         `json:"installable"`
	InstallRequestID string                       `json:"install_request_id"`
	InstallOperation *ApplicationInstallOperation `json:"install_operation"`
}

type ApplicationInstallOperation struct {
	ID              string          `json:"id"`
	ApplicationID   string          `json:"application_id"`
	Status          OperationStatus `json:"status"`
	ProgressPercent int             `json:"progress_percent"`
	ErrorCode       string          `json:"error_code"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type ApplicationInstallRequest struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	RequestID string `json:"request_id"`
}

type ApplicationInstallResponse struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	Operation ApplicationInstallOperation `json:"operation"`
}

type ApplicationLaunchStatus string

const (
	ApplicationLaunchStarting ApplicationLaunchStatus = "starting"
	ApplicationLaunchRunning  ApplicationLaunchStatus = "running"
	ApplicationLaunchStopping ApplicationLaunchStatus = "stopping"
	ApplicationLaunchStopped  ApplicationLaunchStatus = "stopped"
	ApplicationLaunchExited   ApplicationLaunchStatus = "exited"
	ApplicationLaunchFailed   ApplicationLaunchStatus = "failed"
	ApplicationLaunchUnknown  ApplicationLaunchStatus = "unknown"
)

type ApplicationLaunchOperation struct {
	ID            string                  `json:"id"`
	ApplicationID string                  `json:"application_id"`
	Status        ApplicationLaunchStatus `json:"status"`
	ErrorCode     string                  `json:"error_code"`
	CreatedAt     time.Time               `json:"created_at"`
	UpdatedAt     time.Time               `json:"updated_at"`
}

type ApplicationLaunchRequest struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	LaunchID string `json:"launch_id"`
}

type ApplicationLaunchResponse struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	Operation ApplicationLaunchOperation `json:"operation"`
}

func ValidApplicationLaunchOperation(operation ApplicationLaunchOperation) bool {
	if !validApplicationLaunchID(operation.ID) || !validApplicationID(operation.ApplicationID) ||
		operation.CreatedAt.IsZero() || operation.UpdatedAt.Before(operation.CreatedAt) {
		return false
	}
	switch operation.Status {
	case ApplicationLaunchStarting, ApplicationLaunchRunning, ApplicationLaunchStopping,
		ApplicationLaunchStopped, ApplicationLaunchExited:
		return operation.ErrorCode == ""
	case ApplicationLaunchFailed:
		return validApplicationLaunchError(operation.ErrorCode) && operation.ErrorCode != "stop_unconfirmed"
	case ApplicationLaunchUnknown:
		return operation.ErrorCode == "" || operation.ErrorCode == "stop_unconfirmed"
	default:
		return false
	}
}

func ValidApplicationLaunchRequest(request ApplicationLaunchRequest) bool {
	return request.ProtocolVersion == ProtocolVersion && validHostFence(request.HostFence) && validApplicationLaunchID(request.LaunchID)
}

func ValidApplicationLaunchResponse(response ApplicationLaunchResponse) bool {
	return response.ProtocolVersion == ProtocolVersion && validHostFence(response.HostFence) && ValidApplicationLaunchOperation(response.Operation)
}

func validApplicationLaunchError(value string) bool {
	switch value {
	case "not_installed", "architecture_unsupported", "cgroup_unavailable", "launch_failed", "stop_unconfirmed", "storage_error":
		return true
	default:
		return false
	}
}

func validApplicationLaunchID(value string) bool {
	if len(value) < 8 || len(value) > 128 || !strings.HasPrefix(value, "launch_") || !isASCIIAlphaNumeric(value[7]) {
		return false
	}
	for _, character := range value[7:] {
		if !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') && !strings.ContainsRune("._:-", character) {
			return false
		}
	}
	return true
}

type ApplicationsResponse struct {
	ProtocolVersion   string                  `json:"protocol_version"`
	HostID            string                  `json:"host_id"`
	RuntimeGeneration int64                   `json:"runtime_generation"`
	RuntimeInstanceID string                  `json:"runtime_instance_id"`
	ControlVersion    int64                   `json:"control_version"`
	CheckedAt         time.Time               `json:"checked_at"`
	Items             []ApplicationStatusItem `json:"items"`
}

func ValidApplicationStatusItem(item ApplicationStatusItem) bool {
	if item.InstallOperation == nil {
		if item.InstallRequestID != "" {
			return false
		}
	} else if !ValidApplicationInstallOperation(*item.InstallOperation) || !validRequestID(item.InstallRequestID) {
		return false
	}
	switch item.ID {
	case "codex":
		if item.Installable || item.InstallOperation != nil || item.InstallRequestID != "" || item.Version != "" && !validSemver(item.Version) {
			return false
		}
		switch item.LoginStatus {
		case ApplicationLoginLoggedIn, ApplicationLoginNotLoggedIn:
			return item.Installed && validSemver(item.Version)
		case ApplicationLoginUnknown:
			return item.Installed
		case ApplicationLoginNotApplicable:
			return !item.Installed && item.Version == ""
		default:
			return false
		}
	case "wechat":
		if item.InstallOperation != nil && item.InstallOperation.ApplicationID != item.ID {
			return false
		}
		if item.Installed {
			return item.LoginStatus == ApplicationLoginUnknown && item.Version == "4.1.13.26"
		}
		return item.LoginStatus == ApplicationLoginNotApplicable && item.Version == ""
	default:
		if !validApplicationID(item.ID) || item.InstallOperation != nil && item.InstallOperation.ApplicationID != item.ID ||
			item.Version != "" && !validSemver(item.Version) || !item.Installed && item.Version != "" {
			return false
		}
		switch item.LoginStatus {
		case ApplicationLoginLoggedIn, ApplicationLoginNotLoggedIn:
			return item.Installed && item.Version != ""
		case ApplicationLoginUnknown:
			return item.Installed
		case ApplicationLoginNotApplicable:
			return true
		default:
			return false
		}
	}
}

func ValidApplicationInstallOperation(operation ApplicationInstallOperation) bool {
	if len(operation.ID) < 5 || len(operation.ID) > 64 || !strings.HasPrefix(operation.ID, "app_") || !validApplicationID(operation.ApplicationID) ||
		operation.ProgressPercent < 0 || operation.ProgressPercent > 100 || operation.CreatedAt.IsZero() ||
		operation.UpdatedAt.Before(operation.CreatedAt) {
		return false
	}
	for index := 4; index < len(operation.ID); index++ {
		character := operation.ID[index]
		if !(character >= 'a' && character <= 'z') && !(character >= '0' && character <= '9') {
			return false
		}
	}
	switch operation.Status {
	case OperationAccepted, OperationRunning:
		return operation.ErrorCode == ""
	case OperationSucceeded:
		return operation.ProgressPercent == 100 && operation.ErrorCode == ""
	case OperationFailed:
		return validApplicationInstallError(operation.ErrorCode)
	default:
		return false
	}
}

func validApplicationID(value string) bool {
	if len(value) < 1 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	previousHyphen := false
	for index := 1; index < len(value); index++ {
		character := value[index]
		if character == '-' {
			if previousHyphen || index == len(value)-1 {
				return false
			}
			previousHyphen = true
			continue
		}
		if !(character >= 'a' && character <= 'z') && !(character >= '0' && character <= '9') {
			return false
		}
		previousHyphen = false
	}
	return true
}

func ValidApplicationInstallRequest(request ApplicationInstallRequest) bool {
	return request.ProtocolVersion == ProtocolVersion && validHostFence(request.HostFence) && validRequestID(request.RequestID)
}

func ValidApplicationInstallResponse(response ApplicationInstallResponse) bool {
	return response.ProtocolVersion == ProtocolVersion && validHostFence(response.HostFence) && ValidApplicationInstallOperation(response.Operation)
}

func validHostFence(fence HostFence) bool {
	if len(fence.HostID) != 8 || fence.HostID[0] != 'h' || fence.RuntimeGeneration <= 0 ||
		len(fence.RuntimeInstanceID) == 0 || len(fence.RuntimeInstanceID) > 128 || strings.TrimSpace(fence.RuntimeInstanceID) != fence.RuntimeInstanceID || fence.ControlVersion < 0 {
		return false
	}
	for index := 1; index < len(fence.HostID); index++ {
		character := fence.HostID[index]
		if !(character >= 'a' && character <= 'z') && !(character >= '0' && character <= '9') {
			return false
		}
	}
	for _, character := range fence.RuntimeInstanceID {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func validRequestID(value string) bool {
	if len(value) < 1 || len(value) > 128 || !isASCIIAlphaNumeric(value[0]) {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') && !strings.ContainsRune("._:-", character) {
			return false
		}
	}
	return true
}

func isASCIIAlphaNumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
}

func validApplicationInstallError(value string) bool {
	switch value {
	case "download_failed", "digest_mismatch", "invalid_package", "install_failed", "storage_error", "operation_interrupted", "architecture_unsupported":
		return true
	default:
		return false
	}
}

func validSemver(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || len(part) > 1 && part[0] == '0' {
			return false
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return false
			}
		}
	}
	return true
}

type DrainedReceipt struct {
	ReceiptID             string    `json:"receipt_id"`
	HostID                string    `json:"host_id"`
	RuntimeGeneration     int64     `json:"runtime_generation"`
	RuntimeInstanceID     string    `json:"runtime_instance_id"`
	BlockedControlVersion int64     `json:"blocked_control_version"`
	TargetControlVersion  int64     `json:"target_control_version"`
	TaskIDs               []string  `json:"task_ids"`
	IssuedAt              time.Time `json:"issued_at"`
}

type SetControlRequest struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	Mode           Mode            `json:"mode"`
	DrainedReceipt *DrainedReceipt `json:"drained_receipt,omitempty"`
}

type SetControlResponse struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	Mode Mode `json:"mode"`
}

type SetTaskLeaseRequest struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	LeaseVersion  int64     `json:"lease_version"`
	WorkspaceTask bool      `json:"workspace_task,omitempty"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type SetTaskLeaseResponse struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	TaskID       string    `json:"task_id"`
	LeaseVersion int64     `json:"lease_version"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// SetTaskWorkspaceViewRequest binds a Task to a verified snapshot of one Host
// Workspace revision. The Runtime chooses the local directory; no host path is
// accepted on the wire.
type SetTaskWorkspaceViewRequest struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	TaskID                      string `json:"task_id"`
	WorkspaceID                 string `json:"workspace_id"`
	BindingVersion              int64  `json:"binding_version"`
	InputRevision               int64  `json:"input_revision"`
	WorkspaceCheckpointRevision int64  `json:"workspace_checkpoint_revision"`
	ManifestSHA256              string `json:"manifest_sha256"`
}

type SetTaskWorkspaceViewResponse struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	TaskID                      string `json:"task_id"`
	WorkspaceID                 string `json:"workspace_id"`
	BindingVersion              int64  `json:"binding_version"`
	InputRevision               int64  `json:"input_revision"`
	WorkspaceCheckpointRevision int64  `json:"workspace_checkpoint_revision"`
	ManifestSHA256              string `json:"manifest_sha256"`
	ViewID                      string `json:"view_id"`
}

// DeleteTaskWorkspaceViewRequest deletes every private Workspace view for one
// Task after the caller has persisted and confirmed the request. RequestID is
// an opaque, stable idempotency key supplied by the server.
type DeleteTaskWorkspaceViewRequest struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	TaskID    string `json:"task_id"`
	RequestID string `json:"request_id"`
}

type DeleteTaskWorkspaceViewResponse struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	TaskID    string `json:"task_id"`
	RequestID string `json:"request_id"`
	Deleted   bool   `json:"deleted"`
}

// PendingTaskWorkspaceViewDeletion is returned by the authenticated Host
// poller. The Portal retains each item until the matching applied receipt.
type PendingTaskWorkspaceViewDeletion struct {
	RequestID string `json:"request_id"`
	TaskID    string `json:"task_id"`
}

type TaskWorkspaceViewDeletionSnapshot struct {
	RuntimeGeneration int64                              `json:"runtime_generation"`
	Items             []PendingTaskWorkspaceViewDeletion `json:"items"`
}

func ValidDeleteTaskWorkspaceViewRequest(request DeleteTaskWorkspaceViewRequest, taskID string) bool {
	return request.ProtocolVersion == ProtocolVersion && validHostFence(request.HostFence) &&
		validTaskID(taskID) && request.TaskID == taskID && validTaskWorkspaceViewDeletionID(request.RequestID)
}

func validTaskID(value string) bool {
	if len(value) != 8 || value[0] != 'q' {
		return false
	}
	for index := 1; index < len(value); index++ {
		character := value[index]
		if !(character >= 'a' && character <= 'z') && !(character >= '0' && character <= '9') {
			return false
		}
	}
	return true
}

func validTaskWorkspaceViewDeletionID(value string) bool {
	return validShortID(value, 'z')
}

func validShortID(value string, prefix byte) bool {
	if len(value) != 8 || value[0] != prefix {
		return false
	}
	for index := 1; index < len(value); index++ {
		character := value[index]
		if !(character >= 'a' && character <= 'z') && !(character >= '0' && character <= '9') {
			return false
		}
	}
	return true
}

func ValidTaskWorkspaceViewDeletionSnapshot(snapshot TaskWorkspaceViewDeletionSnapshot, generation int64) bool {
	if generation < 1 || snapshot.RuntimeGeneration != generation || snapshot.Items == nil || len(snapshot.Items) > 10_000 {
		return false
	}
	seen := make(map[string]struct{}, len(snapshot.Items))
	seenTasks := make(map[string]struct{}, len(snapshot.Items))
	for _, item := range snapshot.Items {
		if !validTaskID(item.TaskID) || !validTaskWorkspaceViewDeletionID(item.RequestID) {
			return false
		}
		if _, exists := seen[item.RequestID]; exists {
			return false
		}
		if _, exists := seenTasks[item.TaskID]; exists {
			return false
		}
		seen[item.RequestID] = struct{}{}
		seenTasks[item.TaskID] = struct{}{}
	}
	return true
}

type TaskWorkspaceManifestRequest struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	TaskID                      string `json:"task_id"`
	WorkspaceID                 string `json:"workspace_id"`
	BindingVersion              int64  `json:"binding_version"`
	InputRevision               int64  `json:"input_revision"`
	WorkspaceCheckpointRevision int64  `json:"workspace_checkpoint_revision"`
	InputManifestSHA256         string `json:"input_manifest_sha256"`
	ViewID                      string `json:"view_id"`
	ManifestSHA256              string `json:"manifest_sha256,omitempty"`
	AfterPath                   string `json:"after_path,omitempty"`
	Limit                       int    `json:"limit"`
}

type TaskWorkspaceBlockRequest struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	TaskID                      string `json:"task_id"`
	WorkspaceID                 string `json:"workspace_id"`
	BindingVersion              int64  `json:"binding_version"`
	InputRevision               int64  `json:"input_revision"`
	WorkspaceCheckpointRevision int64  `json:"workspace_checkpoint_revision"`
	InputManifestSHA256         string `json:"input_manifest_sha256"`
	ViewID                      string `json:"view_id"`
	ManifestSHA256              string `json:"manifest_sha256"`
	SHA256                      string `json:"sha256"`
}

// TaskWorkspaceManifestChunk is one content-addressed block in a private
// Task Workspace view.
type TaskWorkspaceManifestChunk struct {
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

type WorkspaceExtendedAttribute struct {
	Name  string `json:"name"`
	Value []byte `json:"value"`
}

type WorkspaceNativeACL struct {
	Platform string `json:"platform"`
	Format   string `json:"format"`
	Data     []byte `json:"data"`
}

// TaskWorkspaceManifestFile is a workspace-relative file in an export page.
type TaskWorkspaceManifestFile struct {
	Path          string                       `json:"path"`
	Kind          string                       `json:"kind"`
	SymlinkTarget string                       `json:"symlink_target,omitempty"`
	HardlinkTo    string                       `json:"hardlink_to,omitempty"`
	SizeBytes     int64                        `json:"size_bytes"`
	SHA256        string                       `json:"sha256"`
	Mode          uint32                       `json:"mode"`
	ModTimeNS     *int64                       `json:"mod_time_ns,omitempty"`
	MetadataKnown bool                         `json:"metadata_known,omitempty"`
	Xattrs        []WorkspaceExtendedAttribute `json:"xattrs,omitempty"`
	ACL           *WorkspaceNativeACL          `json:"acl,omitempty"`
	Chunks        []TaskWorkspaceManifestChunk `json:"chunks"`
}

type TaskWorkspaceManifestDirectory struct {
	Path          string                       `json:"path"`
	Mode          uint32                       `json:"mode"`
	ModTimeNS     *int64                       `json:"mod_time_ns,omitempty"`
	MetadataKnown bool                         `json:"metadata_known,omitempty"`
	Xattrs        []WorkspaceExtendedAttribute `json:"xattrs,omitempty"`
	ACL           *WorkspaceNativeACL          `json:"acl,omitempty"`
}

// TaskWorkspaceManifestResponse identifies a bounded page from one exact
// Task Workspace output snapshot. NextCursor is empty when the page is final.
type TaskWorkspaceManifestResponse struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	TaskID                      string                           `json:"task_id"`
	WorkspaceID                 string                           `json:"workspace_id"`
	BindingVersion              int64                            `json:"binding_version"`
	InputRevision               int64                            `json:"input_revision"`
	WorkspaceCheckpointRevision int64                            `json:"workspace_checkpoint_revision"`
	InputManifestSHA256         string                           `json:"input_manifest_sha256"`
	ViewID                      string                           `json:"view_id"`
	ManifestSHA256              string                           `json:"manifest_sha256"`
	Files                       []TaskWorkspaceManifestFile      `json:"files"`
	EmptyDirectories            []string                         `json:"empty_directories"`
	Directories                 []TaskWorkspaceManifestDirectory `json:"directories"`
	NextCursor                  string                           `json:"next_cursor,omitempty"`
}

// TaskWorkspaceBlockResponse returns one exact morde-sync block as base64 JSON
// bytes, with its Task view and output manifest identity.
type TaskWorkspaceBlockResponse struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	TaskID                      string `json:"task_id"`
	WorkspaceID                 string `json:"workspace_id"`
	BindingVersion              int64  `json:"binding_version"`
	InputRevision               int64  `json:"input_revision"`
	WorkspaceCheckpointRevision int64  `json:"workspace_checkpoint_revision"`
	InputManifestSHA256         string `json:"input_manifest_sha256"`
	ViewID                      string `json:"view_id"`
	ManifestSHA256              string `json:"manifest_sha256"`
	SHA256                      string `json:"sha256"`
	SizeBytes                   int64  `json:"size_bytes"`
	Data                        []byte `json:"data"`
}

// WorkspaceBinding is one active Workspace directory under the Host's
// workspace root. The complete snapshot is replaced atomically per fence.
type WorkspaceBinding struct {
	WorkspaceID    string `json:"workspace_id"`
	BindingVersion int64  `json:"binding_version"`
	Directory      string `json:"directory"`
}

type SetWorkspaceBindingsRequest struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	Bindings []WorkspaceBinding `json:"bindings"`
}

type SetWorkspaceBindingsResponse struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	Bindings []WorkspaceBinding `json:"bindings"`
}

type ExecuteRequest struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	TaskID                      string `json:"task_id"`
	WorkspaceID                 string `json:"workspace_id,omitempty"`
	BindingVersion              int64  `json:"binding_version,omitempty"`
	WorkspaceViewID             string `json:"workspace_view_id,omitempty"`
	WorkspaceCheckpointRevision int64  `json:"workspace_checkpoint_revision,omitempty"`
	// WorkspaceDirectory is injected by runtimecore after validating the
	// Task, Workspace, binding, view, and checkpoint. It is never accepted on wire.
	WorkspaceDirectory string          `json:"-"`
	OperationID        string          `json:"operation_id"`
	LeaseVersion       int64           `json:"lease_version"`
	Capability         string          `json:"capability"`
	Action             string          `json:"action"`
	Arguments          json.RawMessage `json:"arguments"`
}

type Operation struct {
	HostID                      string          `json:"host_id"`
	RuntimeGeneration           int64           `json:"runtime_generation"`
	RuntimeInstanceID           string          `json:"runtime_instance_id"`
	ControlVersion              int64           `json:"control_version"`
	TaskID                      string          `json:"task_id"`
	WorkspaceID                 string          `json:"workspace_id,omitempty"`
	BindingVersion              int64           `json:"binding_version,omitempty"`
	WorkspaceViewID             string          `json:"workspace_view_id,omitempty"`
	WorkspaceCheckpointRevision int64           `json:"workspace_checkpoint_revision,omitempty"`
	OperationID                 string          `json:"operation_id"`
	LeaseVersion                int64           `json:"lease_version"`
	Capability                  string          `json:"capability,omitempty"`
	Action                      string          `json:"action,omitempty"`
	Status                      OperationStatus `json:"status"`
	Result                      json.RawMessage `json:"result,omitempty"`
}

type OperationResponse struct {
	ProtocolVersion string    `json:"protocol_version"`
	RequestFence    HostFence `json:"request_fence"`
	Operation       Operation `json:"operation"`
}

type OperationQuery struct {
	HostFence
	TaskID       string `url:"task_id"`
	LeaseVersion int64  `url:"lease_version"`
}

type CancelOperationRequest struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
}

type QuiesceRequest struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	TargetControlVersion int64    `json:"target_control_version"`
	TaskIDs              []string `json:"task_ids"`
}

type QuiesceResponse struct {
	ProtocolVersion     string          `json:"protocol_version"`
	Status              QuiesceStatus   `json:"status"`
	PendingOperationIDs []string        `json:"pending_operation_ids"`
	DrainedReceipt      *DrainedReceipt `json:"drained_receipt,omitempty"`
}

type ErrorResponse struct {
	ProtocolVersion string `json:"protocol_version"`
	Code            string `json:"code"`
}
