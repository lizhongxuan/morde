package app

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

type outputCaptureBrokerStore struct {
	*cloudDSHBrokerTaskStore
	captures []TaskStepOutputCaptureRecord
	statuses []TaskStepOutputCaptureStatusRecord
	writeErr error
}

func (s *outputCaptureBrokerStore) SaveTaskStepOutputCapture(ctx context.Context, record TaskStepOutputCaptureRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !ValidTaskStepOutputCaptureRecordForStore(record) {
		return domain.InvalidArgument("capture", "record is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	stepTerminal := false
	for _, step := range s.details.Steps {
		if step.ID == record.Identity.StepID && (step.Status == task.StepSucceeded || step.Status == task.StepFailed) {
			stepTerminal = true
		}
	}
	operation, exists := s.operations[agentOperationKey(record.Identity.CallID)]
	if !stepTerminal || !exists || operation.Status != task.OperationSucceeded && operation.Status != task.OperationFailed {
		return domain.NewError(domain.CodeInvalidState, "terminal operation and step are required")
	}
	s.captures = append(s.captures, record)
	return nil
}

func (s *outputCaptureBrokerStore) SaveTaskStepOutputCaptureStatus(ctx context.Context, record TaskStepOutputCaptureStatusRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !ValidTaskStepOutputCaptureStatusRecordForStore(record) {
		return domain.InvalidArgument("capture_status", "record is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses = append(s.statuses, record)
	return nil
}

func TestCloudDSHToolBrokerOptInCaptureStoresRedactedReceiptAfterTerminalCommit(t *testing.T) {
	const legacyPrefixBytes = 8 << 10
	secret := []byte("cross-prefix-private-token")
	legacySecret := "stderr-password-canary"
	legacyKey := "api_key=" + legacySecret + "\n"
	legacySuffix := "api_key="
	prefix := legacyKey + strings.Repeat("x", legacyPrefixBytes-len(legacyKey)-len(legacySuffix)) + legacySuffix
	handler := &runtimeToolOutputCaptureHandler{stdoutPrefix: prefix, stdoutSpill: append(secret, '\n'),
		stderrPrefix: "password=" + legacySecret}
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{runtimeAction: handler}})
	baseStore := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	store := &outputCaptureBrokerStore{cloudDSHBrokerTaskStore: baseStore}
	fixture.executor.store = store
	store.details.Task.Authorization.Version = 1
	sealer, err := NewSealer(bytes.Repeat([]byte{23}, 32))
	if err != nil {
		t.Fatal(err)
	}
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if broker.stepOutputCaptureEnabled {
		t.Fatal("output capture must default to disabled")
	}
	if err := broker.SetTaskStepDetailSealer(sealer); err != nil {
		t.Fatal(err)
	}
	if err := broker.SetTaskStepOutputCaptureEnabled(true); err != nil {
		t.Fatalf("enable output capture: %v", err)
	}
	call := ModelToolCall{CallID: "call-output-capture", Name: runtimeToolShellExec,
		Arguments: `{"workspace_id":"w1234567","command":"printf output"}`}
	initial, err := broker.Execute(context.Background(), store.details.Task, call)
	if err != nil || initial.Status != AgentToolNeedsUser || handler.calls.Load() != 0 {
		t.Fatalf("initial Shell authorization = %#v, %v; calls=%d", initial, err, handler.calls.Load())
	}
	resumeCloudDSHApproval(fixture, store.cloudDSHBrokerTaskStore, task.PendingRequestApproved, task.PendingDecisionApprove)
	result, err := broker.Reconcile(context.Background(), store.details.Task, call, nil)
	if err != nil || result.Status != AgentToolSucceeded || handler.calls.Load() != 1 || handler.captureCalls.Load() != 1 {
		t.Fatalf("Broker.Execute() = %#v, %v; shell calls=%d capture requests=%d", result, err, handler.calls.Load(), handler.captureCalls.Load())
	}
	if len(store.captures) != 1 || len(store.statuses) != 0 {
		t.Fatalf("capture receipts/statuses = %d/%d", len(store.captures), len(store.statuses))
	}
	record := store.captures[0]
	if record.Identity.CallID != call.CallID || record.Identity.TaskID != store.details.Task.ID || record.Identity.StepID == "" ||
		record.Stdout.SourceBytes != len(prefix)+len(secret)+1 || record.Stdout.TotalBytes >= record.Stdout.SourceBytes {
		t.Fatalf("saved capture identity/accounting = %#v, stdout=%+v", record.Identity, record.Stdout)
	}
	var disclosed bytes.Buffer
	for _, page := range record.Stdout.Pages {
		plain, err := sealer.OpenWithAAD(page.Ciphertext, taskStepOutputCaptureAAD(page))
		if err != nil {
			t.Fatal(err)
		}
		disclosed.Write(plain)
	}
	if bytes.Contains(disclosed.Bytes(), secret) || !bytes.Contains(disclosed.Bytes(), []byte("[已隐藏]")) {
		t.Fatalf("supplemental receipt leaked or failed to redact boundary secret: %q", disclosed.String()[legacyPrefixBytes-24:])
	}
	operation := store.operations[agentOperationKey(call.CallID)]
	if operation.Status != task.OperationSucceeded || operation.Result != result.Output || strings.Contains(operation.Result, string(secret)) ||
		bytes.Contains([]byte(operation.Result), secret) || strings.Contains(operation.Result, legacySecret) {
		t.Fatalf("legacy result/operation changed or contains spill: result=%q operation=%+v", result.Output, operation)
	}
	var legacyResult taskStepShellOutput
	if decodeStrictRuntimeResult([]byte(operation.Result), &legacyResult) != nil || legacyResult.ExitCode != 0 ||
		legacyResult.StdoutTruncated != true || legacyResult.StderrTruncated ||
		!strings.Contains(legacyResult.Stdout, "api_key=[已隐藏]") || !strings.Contains(legacyResult.Stderr, "password=[已隐藏]") {
		t.Fatalf("legacy shell response lost structure or retained credentials: %#v (%s)", legacyResult, operation.Result)
	}
	stepTerminal := false
	for _, step := range store.details.Steps {
		if step.ID == record.Identity.StepID && step.Status == task.StepSucceeded {
			stepTerminal = true
		}
	}
	if !stepTerminal || record.Identity.TaskRevision != *store.details.Task.CurrentRevision {
		t.Fatalf("saved capture was not bound to the terminal persisted step: step=%+v identity=%+v", store.details.Steps, record.Identity)
	}
}

func TestCloudDSHToolBrokerOutputCaptureEnablementFailsClosedWithoutStorePorts(t *testing.T) {
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: &runtimeToolTaskStore{details: TaskDetails{}}}
	broker := &CloudDSHToolBroker{store: store}
	sealer, err := NewSealer(bytes.Repeat([]byte{24}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.SetTaskStepDetailSealer(sealer); err != nil {
		t.Fatal(err)
	}
	if err := broker.SetTaskStepOutputCaptureEnabled(true); err == nil || broker.stepOutputCaptureEnabled {
		t.Fatalf("capture enablement without storage ports = %v, enabled=%t", err, broker.stepOutputCaptureEnabled)
	}
	if err := broker.SetTaskStepOutputCaptureEnabled(false); err != nil || broker.stepOutputCaptureEnabled {
		t.Fatalf("disabled default changed: err=%v enabled=%t", err, broker.stepOutputCaptureEnabled)
	}
}
