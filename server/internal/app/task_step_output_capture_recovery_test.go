package app

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/runtimeclient"
)

func TestRuntimeToolExecutorReconcileTerminalShellCollectsCaptureWithoutReplay(t *testing.T) {
	const prefixBytes = 8 << 10
	secret := []byte("terminal-recovery-private-canary")
	prefix := strings.Repeat("x", prefixBytes-len("api_key=")) + "api_key="
	handler := &runtimeToolOutputCaptureHandler{stdoutPrefix: prefix, stdoutSpill: append(secret, '\n'),
		stderrPrefix: "safe stderr", stderrSpill: []byte(" continued")}
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{runtimeAction: handler}})
	fixture.store.details.Task.Authorization.Version = 1
	request, preparation := fixture.prepared(runtimeToolShellExec, `{"command":"printf output"}`, identity.ScopeComputerOperate)
	request.Task = fixture.store.details.Task
	request.StepID = "s1234567"
	fixture.persistOperation(request)

	ctx := context.Background()
	session, err := fixture.executor.connect(ctx, request.Task, preparation.Fence)
	if err != nil {
		t.Fatalf("connect Runtime before terminal recovery: %v", err)
	}
	defer session.Close()
	_, viewID, err := session.SetTaskWorkspaceView(ctx, request.Task, preparation.WorkspaceID, preparation.BindingVersion)
	if err != nil {
		t.Fatalf("set Shell Workspace view: %v", err)
	}
	checkpointRevision := *request.Task.WorkspaceCheckpointRevision
	if err := session.SetTaskLease(ctx, request.Task.ID, request.Task.Lease.Version, *request.Task.Lease.ExpiresAt, true); err != nil {
		t.Fatalf("set task lease before original Shell dispatch: %v", err)
	}
	mapping, ok := runtimeToolMappingByName(runtimeToolShellExec)
	if !ok {
		t.Fatal("Shell runtime mapping is missing")
	}
	runtimeIdentity, appIdentity, err := fixture.executor.taskStepOutputCaptureIdentity(ctx, request.Task, request,
		preparation.WorkspaceID, preparation.BindingVersion, viewID, checkpointRevision)
	if err != nil {
		t.Fatalf("build output capture recovery identity: %v", err)
	}
	runtimeArguments, err := runtimeToolExecutionArguments(mapping, request.Arguments)
	if err != nil {
		t.Fatalf("build Shell runtime arguments: %v", err)
	}
	operation, err := fixture.executor.operations.ExecuteWithSupplementalOutputCapture(ctx, session,
		request.Task.ID, request.OperationID, request.Task.Lease.Version, mapping.capability, mapping.action,
		runtimeArguments, preparation.WorkspaceID, preparation.BindingVersion, viewID, checkpointRevision,
		request.Task.InputRevision, runtimeIdentity)
	if err == nil {
		operation, err = fixture.executor.operations.Poll(ctx, session, request.Task.ID, request.OperationID,
			request.Task.Lease.Version, operation)
	}
	if err != nil || operation.Status != runtimeclient.OperationSucceeded || handler.calls.Load() != 1 || handler.captureCalls.Load() != 1 {
		t.Fatalf("original captured Shell operation=%+v err=%v calls=%d capture requests=%d", operation, err,
			handler.calls.Load(), handler.captureCalls.Load())
	}

	result, err := mapRuntimeToolResult(mapping, operation, request.Arguments)
	if err != nil || result.Status != AgentToolSucceeded || strings.Contains(result.Output, string(secret)) ||
		strings.Contains(result.Output, "api_key="+string(secret)) {
		t.Fatalf("new Shell result was not redacted before persistence: result=%+v err=%v", result, err)
	}
	persisted := fixture.store.operation
	persisted.Status, persisted.ExternalID, persisted.Result = task.OperationSucceeded, operation.OperationID, result.Output
	fixture.store.operation = persisted
	fixture.store.operations[persisted.Key] = persisted

	capture := newTaskStepOutputCaptureExecution(TaskStepOutputCaptureIdentity{})
	recovered, err := fixture.executor.ReconcileOperation(withTaskStepOutputCaptureExecution(ctx, capture), request, persisted)
	if err != nil || recovered.Status != AgentToolSucceeded || recovered.Output != persisted.Result ||
		handler.calls.Load() != 1 || handler.captureCalls.Load() != 1 {
		t.Fatalf("terminal Shell recovery=%+v err=%v calls=%d capture requests=%d", recovered, err,
			handler.calls.Load(), handler.captureCalls.Load())
	}
	identityValue, state, stdoutSpill, stderrSpill := capture.take()
	legacyRaw := capture.takeLegacyRaw()
	defer clear(stdoutSpill)
	defer clear(stderrSpill)
	defer clear(legacyRaw)
	if state != TaskStepOutputCaptureStateComplete || identityValue != appIdentity || len(legacyRaw) == 0 {
		t.Fatalf("recovered capture identity/state/prefix = %#v/%q/%d bytes", identityValue, state, len(legacyRaw))
	}
	stdout, stderr, ok := joinTaskStepOutputCaptureLegacyPrefix(identityValue, legacyRaw, stdoutSpill, stderrSpill)
	if !ok || !bytes.Equal(stdout, append([]byte(prefix), append(secret, '\n')...)) ||
		string(stderr) != "safe stderr continued" {
		t.Fatalf("recovered raw prefix/spill did not preserve exact streams: ok=%t stdout_bytes=%d stderr=%q", ok, len(stdout), stderr)
	}
	sealer, err := NewSealer(bytes.Repeat([]byte{0x44}, 32))
	if err != nil {
		t.Fatal(err)
	}
	record, err := SealTaskStepOutputCapture(sealer, identityValue, stdout, stderr)
	clear(stdout)
	clear(stderr)
	if err != nil || !ValidTaskStepOutputCaptureRecordForStore(record) {
		t.Fatalf("seal recovered output receipt: record valid=%t err=%v", ValidTaskStepOutputCaptureRecordForStore(record), err)
	}
	var disclosed bytes.Buffer
	for _, page := range record.Stdout.Pages {
		plain, err := sealer.OpenWithAAD(page.Ciphertext, taskStepOutputCaptureAAD(page))
		if err != nil {
			t.Fatalf("open recovered stdout page: %v", err)
		}
		disclosed.Write(plain)
	}
	if bytes.Contains(disclosed.Bytes(), secret) || !bytes.Contains(disclosed.Bytes(), []byte("[已隐藏]")) {
		t.Fatalf("recovered receipt did not redact cross-boundary credentials: %q", disclosed.String()[prefixBytes-24:])
	}

	var persistedShell taskStepShellOutput
	if json.Unmarshal([]byte(persisted.Result), &persistedShell) != nil || persistedShell.ExitCode != 0 ||
		!persistedShell.StdoutTruncated || !persistedShell.StderrTruncated ||
		persistedShell.Stdout != prefix || strings.Contains(persisted.Result, string(secret)) {
		t.Fatalf("recovered legacy Operation result lost structure or retained credentials: %s", persisted.Result)
	}
}
