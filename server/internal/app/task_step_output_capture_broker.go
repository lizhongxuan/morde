package app

import (
	"context"
	"time"

	"morde/server/internal/domain"
)

const taskStepOutputCapturePersistTimeout = 2 * time.Second

func (b *CloudDSHToolBroker) persistTaskStepOutputCapture(ctx context.Context, capture *taskStepOutputCaptureExecution) {
	if b == nil || capture == nil {
		return
	}
	persistParent := context.Background()
	if ctx != nil {
		persistParent = context.WithoutCancel(ctx)
	}
	identity, state, stdoutSpill, stderrSpill := capture.take()
	legacyRaw := capture.takeLegacyRaw()
	defer clear(stdoutSpill)
	defer clear(stderrSpill)
	defer clear(legacyRaw)
	if !taskStepOutputCaptureIdentityValid(identity) {
		return
	}
	if ctx == nil || ctx.Err() != nil {
		state = TaskStepOutputCaptureStateFailed
		capture.clearBytes()
	}
	if state == TaskStepOutputCaptureStateComplete {
		stdout, stderr, ok := joinTaskStepOutputCaptureLegacyPrefix(identity, legacyRaw, stdoutSpill, stderrSpill)
		if !ok {
			state = TaskStepOutputCaptureStateSourceTruncated
			clear(stdout)
			clear(stderr)
		} else if len(stdout) > TaskStepOutputCaptureMaxBytes || len(stderr) > TaskStepOutputCaptureMaxBytes-len(stdout) {
			state = TaskStepOutputCaptureStateCapacityTruncated
			clear(stdout)
			clear(stderr)
		} else {
			record, err := SealTaskStepOutputCapture(b.stepDetailSealer, identity, stdout, stderr)
			clear(stdout)
			clear(stderr)
			if err != nil {
				state = TaskStepOutputCaptureStateFailed
			} else if writer, ok := b.store.(TaskStepOutputCaptureWriter); !ok {
				state = TaskStepOutputCaptureStateFailed
			} else {
				persistCtx, cancel := context.WithTimeout(persistParent, taskStepOutputCapturePersistTimeout)
				err = writer.SaveTaskStepOutputCapture(persistCtx, record)
				cancel()
				if err == nil {
					return
				}
				if domain.CodeOf(err) == domain.CodeQuotaExceeded {
					state = TaskStepOutputCaptureStateCapacityTruncated
				} else {
					state = TaskStepOutputCaptureStateFailed
				}
			}
		}
	}
	if state != TaskStepOutputCaptureStateSourceTruncated && state != TaskStepOutputCaptureStateCapacityTruncated &&
		state != TaskStepOutputCaptureStateFailed {
		return
	}
	writer, ok := b.store.(TaskStepOutputCaptureStatusWriter)
	if !ok {
		return
	}
	status := TaskStepOutputCaptureStatusRecord{Identity: identity, State: state,
		ExpiresAt: time.Now().UTC().Add(TaskStepOutputCaptureRetention)}
	persistCtx, cancel := context.WithTimeout(persistParent, taskStepOutputCapturePersistTimeout)
	defer cancel()
	_ = writer.SaveTaskStepOutputCaptureStatus(persistCtx, status)
}

func joinTaskStepOutputCaptureLegacyPrefix(identity TaskStepOutputCaptureIdentity, legacyResult []byte,
	stdoutSpill, stderrSpill []byte) ([]byte, []byte, bool) {
	var legacy taskStepShellOutput
	if decodeStrictRuntimeResult(legacyResult, &legacy) != nil || legacy.TaskID != identity.TaskID ||
		legacy.WorkspaceID != identity.WorkspaceID || legacy.BindingVersion != identity.WorkspaceBindingVersion ||
		legacy.WorkspaceViewID != identity.WorkspaceViewID || legacy.WorkspaceCheckpointRevision == nil ||
		*legacy.WorkspaceCheckpointRevision != identity.WorkspaceCheckpointRevision ||
		legacy.StdoutTruncated != (len(stdoutSpill) > 0) || legacy.StderrTruncated != (len(stderrSpill) > 0) {
		return nil, nil, false
	}
	stdout := make([]byte, 0, len(legacy.Stdout)+len(stdoutSpill))
	stdout = append(stdout, legacy.Stdout...)
	stdout = append(stdout, stdoutSpill...)
	stderr := make([]byte, 0, len(legacy.Stderr)+len(stderrSpill))
	stderr = append(stderr, legacy.Stderr...)
	stderr = append(stderr, stderrSpill...)
	return stdout, stderr, true
}
