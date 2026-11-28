package contract

import (
	"context"
	"strings"
	"unicode/utf8"
)

const (
	SupplementalOutputCaptureCapability = "shell.output_capture"
	SupplementalOutputCaptureMaxBytes   = 2 << 20
	SupplementalOutputCapturePageBytes  = 16 << 10
)

// SupplementalOutputCaptureIdentity fences optional bytes beyond the normal
// Shell result. It is metadata only; the Runtime never receives a data key.
type SupplementalOutputCaptureIdentity struct {
	OwnerID                       string `json:"owner_id"`
	TaskID                        string `json:"task_id"`
	AuthorizationVersion          int64  `json:"authorization_version"`
	TaskRevision                  int64  `json:"task_revision"`
	WorkspaceID                   string `json:"workspace_id"`
	WorkspaceAuthorizationVersion int64  `json:"workspace_authorization_version"`
	WorkspaceBindingVersion       int64  `json:"workspace_binding_version"`
	WorkspaceViewID               string `json:"workspace_view_id"`
	WorkspaceCheckpointRevision   int64  `json:"workspace_checkpoint_revision"`
	StepID                        string `json:"step_id"`
	CallID                        string `json:"call_id"`
}

func ValidSupplementalOutputCaptureIdentity(value SupplementalOutputCaptureIdentity) bool {
	return validFixedID(value.OwnerID, 'u') && validFixedID(value.TaskID, 'q') && value.AuthorizationVersion > 0 && value.TaskRevision >= 0 &&
		validFixedID(value.WorkspaceID, 'w') && value.WorkspaceAuthorizationVersion >= 0 && value.WorkspaceBindingVersion > 0 &&
		validWorkspaceViewID(value.WorkspaceViewID) && value.WorkspaceCheckpointRevision >= 0 && validFixedID(value.StepID, 's') &&
		validOpaqueIdentity(value.CallID, 256)
}

func validFixedID(value string, prefix byte) bool {
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

func validOpaqueIdentity(value string, limit int) bool {
	if value == "" || len(value) > limit || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < 0x21 || character == 0x7f {
			return false
		}
	}
	return true
}

func validWorkspaceViewID(value string) bool {
	if len(value) != 65 || value[0] != 'v' {
		return false
	}
	for _, character := range value[1:] {
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

type SupplementalOutputCaptureState string

const (
	SupplementalOutputCaptureCollecting SupplementalOutputCaptureState = "collecting"
	SupplementalOutputCaptureComplete   SupplementalOutputCaptureState = "complete"
	SupplementalOutputCaptureIncomplete SupplementalOutputCaptureState = "incomplete"
	SupplementalOutputCaptureFailed     SupplementalOutputCaptureState = "failed"
)

type SupplementalOutputCaptureStream string

const (
	SupplementalOutputCaptureStdout SupplementalOutputCaptureStream = "stdout"
	SupplementalOutputCaptureStderr SupplementalOutputCaptureStream = "stderr"
)

func ValidSupplementalOutputCaptureStream(value SupplementalOutputCaptureStream) bool {
	return value == SupplementalOutputCaptureStdout || value == SupplementalOutputCaptureStderr
}

type SupplementalOutputCapturePageRequest struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	TaskID       string                            `json:"task_id"`
	LeaseVersion int64                             `json:"lease_version"`
	OperationID  string                            `json:"operation_id"`
	Identity     SupplementalOutputCaptureIdentity `json:"identity"`
	Stream       SupplementalOutputCaptureStream   `json:"stream"`
	Offset       int                               `json:"offset"`
}

type SupplementalOutputCapturePage struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	TaskID       string                            `json:"task_id"`
	LeaseVersion int64                             `json:"lease_version"`
	OperationID  string                            `json:"operation_id"`
	Identity     SupplementalOutputCaptureIdentity `json:"identity"`
	State        SupplementalOutputCaptureState    `json:"state"`
	Reason       string                            `json:"reason,omitempty"`
	Stream       SupplementalOutputCaptureStream   `json:"stream"`
	Offset       int                               `json:"offset"`
	NextOffset   int                               `json:"next_offset"`
	TotalBytes   int                               `json:"total_bytes"`
	Data         []byte                            `json:"data"`
	EOF          bool                              `json:"eof"`
}

type SupplementalOutputCaptureReleaseRequest struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	TaskID       string                            `json:"task_id"`
	LeaseVersion int64                             `json:"lease_version"`
	Identity     SupplementalOutputCaptureIdentity `json:"identity"`
}

type SupplementalOutputCaptureReleaseResponse struct {
	ProtocolVersion string `json:"protocol_version"`
	HostFence
	TaskID       string `json:"task_id"`
	LeaseVersion int64  `json:"lease_version"`
	OperationID  string `json:"operation_id"`
	Released     bool   `json:"released"`
}

// SupplementalOutputSink is scoped to one operation. Shell writers must treat
// a false return as a lost evidence page and continue draining the child.
type SupplementalOutputSink interface {
	WriteSupplementalOutput(stream SupplementalOutputCaptureStream, value []byte) bool
	MarkSupplementalOutputIncomplete(reason string)
	MarkSupplementalOutputUnsupported(reason string)
}

type supplementalOutputSinkContextKey struct{}

func WithSupplementalOutputSink(ctx context.Context, sink SupplementalOutputSink) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, supplementalOutputSinkContextKey{}, sink)
}

func SupplementalOutputSinkFromContext(ctx context.Context) SupplementalOutputSink {
	if ctx == nil {
		return nil
	}
	sink, _ := ctx.Value(supplementalOutputSinkContextKey{}).(SupplementalOutputSink)
	return sink
}
