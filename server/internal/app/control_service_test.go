package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/control"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/runtimeclient"
)

func TestControlServiceTakeoverAndReleaseUseOneRuntimeFence(t *testing.T) {
	ctx := context.Background()
	runtime, hosts, manager, server := newRuntimeControlTest(t, 0, 0, runtimeclient.ModeBlocked, host.ControlAI, "")
	defer func() { _ = manager.Close() }()
	defer server.Close()
	transitionStore := &controlTransitionStoreStub{hosts: hosts}
	connected, err := runtime.Connect(ctx, "h1234567", nil)
	if err != nil {
		t.Fatalf("prepare Host runtime fence: %v", err)
	}
	connected.Close()
	tasks := &controlTaskStoreStub{values: []task.Task{
		{ID: "q0000001", OwnerID: "u1234567", HostID: "h1234567", Status: task.StatusQueued},
		{ID: "q0000002", OwnerID: "u1234567", HostID: "h1234567", Status: task.StatusRunning},
	}}
	mcpTaskIDs := &controlMCPRuntimeTaskIDsStub{ids: []string{"q0000002", "q0000003"}}
	auditWriter := &hostServiceTestAudit{}
	service, err := NewControlService(hosts, &taskAuthorizerStub{}, tasks, transitionStore, &controlActivityDrainerStub{}, runtime, auditWriter, mcpTaskIDs)
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.Act(ctx, taskTestPrincipal(), "h1234567", ControlActionCommand{
		Action: control.ActionTakeOver, RequestID: "takeover-service-1", ExpectedControlVersion: 0,
	})
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if result.Status.ControlState != host.ControlUser || result.Status.ControlVersion != 2 || result.Status.ActiveTransition != nil || len(result.PendingOperationIDs) != 0 {
		t.Fatalf("takeover result = %#v", result)
	}
	if transitionStore.value == nil || transitionStore.value.Phase != control.PhaseCompleted || !transitionStore.value.TaskIDsSaved || len(transitionStore.value.TaskIDs) != 3 ||
		transitionStore.value.TaskIDs[0] != "q0000001" || transitionStore.value.TaskIDs[1] != "q0000002" || transitionStore.value.TaskIDs[2] != "q0000003" || len(transitionStore.value.DrainedReceipt) == 0 {
		t.Fatalf("stored takeover transition = %#v", transitionStore.value)
	}
	if mcpTaskIDs.fence.RuntimeInstanceID == "" || mcpTaskIDs.fence.RuntimeGeneration != 1 || mcpTaskIDs.fence.ControlVersion != 0 || !mcpTaskIDs.fence.AIAllowed {
		t.Fatalf("MCP operation snapshot fence = %+v", mcpTaskIDs.fence)
	}
	runtimeState, err := manager.Capabilities(runtimeclient.CapabilitiesRequest{ProtocolVersion: runtimeclient.ProtocolVersion, HostID: "h1234567", RuntimeGeneration: 1, ControlVersion: 2})
	if err != nil || runtimeState.Mode != runtimeclient.ModeHuman || runtimeState.ControlVersion != 2 {
		t.Fatalf("runtime after takeover = %#v, err=%v", runtimeState, err)
	}

	result, err = service.Act(ctx, taskTestPrincipal(), "h1234567", ControlActionCommand{
		Action: control.ActionRelease, RequestID: "release-service-1", ExpectedControlVersion: 2,
	})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if result.Status.ControlState != host.ControlAI || result.Status.ControlVersion != 4 || result.Status.ActiveTransition != nil {
		t.Fatalf("release result = %#v", result)
	}
	if transitionStore.value.Phase != control.PhaseCompleted || transitionStore.value.Action != control.ActionRelease {
		t.Fatalf("stored release transition = %#v", transitionStore.value)
	}
	runtimeState, err = manager.Capabilities(runtimeclient.CapabilitiesRequest{ProtocolVersion: runtimeclient.ProtocolVersion, HostID: "h1234567", RuntimeGeneration: 1, ControlVersion: 4})
	if err != nil || runtimeState.Mode != runtimeclient.ModeAI || runtimeState.ControlVersion != 4 {
		t.Fatalf("runtime after release = %#v, err=%v", runtimeState, err)
	}
	if len(auditWriter.entries) != 2 {
		t.Fatalf("control audit entries = %d, want 2", len(auditWriter.entries))
	}
}

func TestControlServiceDoesNotBeginWhenAuthorizationFails(t *testing.T) {
	runtime, hosts, manager, server := newRuntimeControlTest(t, 0, 0, runtimeclient.ModeBlocked, host.ControlAI, "")
	defer func() { _ = manager.Close() }()
	defer server.Close()
	transitions := &controlTransitionStoreStub{hosts: hosts}
	service, err := NewControlService(hosts, &taskAuthorizerStub{err: domain.NewError(domain.CodeForbidden, "denied")}, &controlTaskStoreStub{}, transitions, &controlActivityDrainerStub{}, runtime, &hostServiceTestAudit{}, &controlMCPRuntimeTaskIDsStub{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Act(context.Background(), taskTestPrincipal(), "h1234567", ControlActionCommand{
		Action: control.ActionTakeOver, RequestID: "takeover-denied", ExpectedControlVersion: 0,
	}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("unauthorized takeover error = %v", err)
	}
	if transitions.value != nil {
		t.Fatalf("unauthorized request created transition: %#v", transitions.value)
	}
}

func TestControlServiceApplicationsRequiresObserveScopeAndOwnerBeforeRuntime(t *testing.T) {
	runtime, hosts, manager, server := newRuntimeControlTest(t, 0, 0, runtimeclient.ModeBlocked, host.ControlAI, "")
	defer func() { _ = manager.Close() }()
	defer server.Close()

	deniedAuthorizer := &taskAuthorizerStub{err: domain.NewError(domain.CodeForbidden, "denied")}
	deniedService, err := NewControlService(hosts, deniedAuthorizer, &controlTaskStoreStub{}, &controlTransitionStoreStub{hosts: hosts},
		&controlActivityDrainerStub{}, runtime, &hostServiceTestAudit{}, &controlMCPRuntimeTaskIDsStub{})
	if err != nil {
		t.Fatal(err)
	}
	// A mistakenly reordered implementation would attempt a network handshake. Close
	// the test Runtime so the assertion also proves denial precedes Runtime access.
	server.Close()
	if _, err := deniedService.Applications(context.Background(), taskTestPrincipal(), "h1234567"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("Applications() with denied observe scope error = %v, want hidden not-found", err)
	}
	if deniedAuthorizer.calls != 1 || deniedAuthorizer.lastScope != identity.ScopeComputerObserve {
		t.Fatalf("observe authorization calls=%d scope=%q", deniedAuthorizer.calls, deniedAuthorizer.lastScope)
	}

	ownerAuthorizer := &taskAuthorizerStub{}
	ownerService, err := NewControlService(hosts, ownerAuthorizer, &controlTaskStoreStub{}, &controlTransitionStoreStub{hosts: hosts},
		&controlActivityDrainerStub{}, runtime, &hostServiceTestAudit{}, &controlMCPRuntimeTaskIDsStub{})
	if err != nil {
		t.Fatal(err)
	}
	otherUser := taskTestPrincipal()
	otherUser.UserID = "u9999999"
	if _, err := ownerService.Applications(context.Background(), otherUser, "h1234567"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("Applications() for non-owner error = %v, want not-found", err)
	}
	if ownerAuthorizer.calls != 1 || ownerAuthorizer.lastScope != identity.ScopeComputerObserve {
		t.Fatalf("non-owner authorization calls=%d scope=%q", ownerAuthorizer.calls, ownerAuthorizer.lastScope)
	}
}

func TestControlServiceWaitsForOldGatewayLeasesBeforeChangingRuntimeMode(t *testing.T) {
	runtime, hosts, manager, server := newRuntimeControlTest(t, 0, 0, runtimeclient.ModeBlocked, host.ControlAI, "")
	defer func() { _ = manager.Close() }()
	defer server.Close()
	transitions := &controlTransitionStoreStub{hosts: hosts}
	drainer := &controlActivityDrainerStub{pending: true}
	connected, err := runtime.Connect(context.Background(), "h1234567", nil)
	if err != nil {
		t.Fatalf("prepare Host runtime fence: %v", err)
	}
	connected.Close()
	service, err := NewControlService(hosts, &taskAuthorizerStub{}, &controlTaskStoreStub{}, transitions, drainer, runtime, &hostServiceTestAudit{}, &controlMCPRuntimeTaskIDsStub{})
	if err != nil {
		t.Fatal(err)
	}
	service.drainTimeout = 5 * time.Millisecond
	service.drainPollInterval = time.Millisecond
	command := ControlActionCommand{Action: control.ActionTakeOver, RequestID: "takeover-drain-retry", ExpectedControlVersion: 0}
	if _, err := service.Act(context.Background(), taskTestPrincipal(), "h1234567", command); domain.CodeOf(err) != domain.CodeHostNotReady {
		t.Fatalf("takeover with old lease error = %v, want host not ready", err)
	}
	if transitions.value == nil || transitions.value.Phase != control.PhaseDraining {
		t.Fatalf("transition after drain timeout = %#v, want durable draining state", transitions.value)
	}
	if hosts.values["h1234567"].ControlState != host.ControlTakingOver {
		t.Fatalf("host state after drain timeout = %s, want taking_over", hosts.values["h1234567"].ControlState)
	}
	drainer.pending = false
	result, err := service.Act(context.Background(), taskTestPrincipal(), "h1234567", command)
	if err != nil {
		t.Fatalf("retry after old lease release: %v", err)
	}
	if result.Status.ControlState != host.ControlUser || result.Status.ControlVersion != 2 {
		t.Fatalf("takeover after lease drain = %#v", result.Status)
	}
	if drainer.calls < 2 {
		t.Fatalf("drain checks = %d, want repeated checks across retry", drainer.calls)
	}
}

type controlActivityDrainerStub struct {
	pending bool
	err     error
	calls   int
}

func (s *controlActivityDrainerStub) HasOutstandingControlTransitionLeases(context.Context, string, int64) (bool, error) {
	s.calls++
	return s.pending, s.err
}

type controlTaskStoreStub struct {
	TaskStore
	values []task.Task
}

type controlMCPRuntimeTaskIDsStub struct {
	ids   []string
	fence AgentHostFence
	err   error
}

func (s *controlMCPRuntimeTaskIDsStub) ListActiveMCPRuntimeTaskIDs(_ context.Context, _, _ string, fence AgentHostFence) ([]string, error) {
	s.fence = fence
	return append([]string(nil), s.ids...), s.err
}

func (s *controlTaskStoreStub) ListTasks(_ context.Context, ownerID string, filter TaskListRequest) (TaskPage, error) {
	items := make([]task.Task, 0)
	for _, value := range s.values {
		if value.OwnerID == ownerID && value.HostID == filter.HostIDs[0] && value.Status == filter.Status {
			items = append(items, value)
		}
	}
	return TaskPage{Items: items}, nil
}

type controlTransitionStoreStub struct {
	hosts *hostServiceTestHostStore
	value *control.Transition
}

func (s *controlTransitionStoreStub) BeginControlTransition(_ context.Context, request control.BeginRequest) (control.Transition, error) {
	if s.value != nil && s.value.HostID == request.HostID && s.value.RequestID == request.RequestID {
		if s.value.OwnerID != request.OwnerID || s.value.Action != request.Action || s.value.ExpectedControlVersion != request.ExpectedControlVersion {
			return control.Transition{}, domain.NewError(domain.CodeIdempotencyConflict, "control request conflicts")
		}
		return cloneControlTransition(*s.value), nil
	}
	value, err := s.hosts.GetHost(context.Background(), request.HostID)
	if err != nil || value.OwnerID != request.OwnerID {
		return control.Transition{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if value.ControlVersion != request.ExpectedControlVersion || value.State != host.StateRunning {
		return control.Transition{}, domain.NewError(domain.CodeInvalidState, "Host control fence changed")
	}
	var source, blocked host.ControlState
	switch request.Action {
	case control.ActionTakeOver:
		source, blocked = host.ControlAI, host.ControlTakingOver
	case control.ActionRelease:
		source, blocked = host.ControlUser, host.ControlReturning
	default:
		return control.Transition{}, domain.InvalidArgument("action", "action is invalid")
	}
	if value.ControlState != source {
		return control.Transition{}, domain.NewError(domain.CodeInvalidState, "Host control state changed")
	}
	value.ControlState = blocked
	value.ControlVersion++
	s.hosts.addHost(value)
	transition := control.Transition{
		HostID: request.HostID, OwnerID: request.OwnerID, RequestID: request.RequestID, Action: request.Action,
		Phase: control.PhaseBlocking, ExpectedControlVersion: request.ExpectedControlVersion,
		BlockedControlVersion: request.ExpectedControlVersion + 1, TargetControlVersion: request.ExpectedControlVersion + 2,
		TaskIDs: []string{}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.value = &transition
	return cloneControlTransition(transition), nil
}

func (s *controlTransitionStoreStub) GetActiveControlTransition(_ context.Context, hostID string) (control.Transition, error) {
	if s.value == nil || s.value.HostID != hostID || s.value.Phase == control.PhaseCompleted {
		return control.Transition{}, domain.NewError(domain.CodeNotFound, "transition not found")
	}
	return cloneControlTransition(*s.value), nil
}

func (s *controlTransitionStoreStub) SaveControlTaskIDs(_ context.Context, hostID, requestID string, ids []string) (control.Transition, error) {
	value, err := s.lookup(hostID, requestID)
	if err != nil {
		return control.Transition{}, err
	}
	if value.TaskIDsSaved && !sameTestControlStrings(value.TaskIDs, ids) {
		return control.Transition{}, domain.NewError(domain.CodeInvalidState, "task snapshot changed")
	}
	value.TaskIDs = append([]string{}, ids...)
	value.TaskIDsSaved = true
	s.value = &value
	return cloneControlTransition(value), nil
}

func (s *controlTransitionStoreStub) SetControlTransitionPhase(_ context.Context, hostID, requestID string, phase control.Phase) (control.Transition, error) {
	value, err := s.lookup(hostID, requestID)
	if err != nil {
		return control.Transition{}, err
	}
	value.Phase = phase
	s.value = &value
	return cloneControlTransition(value), nil
}

func (s *controlTransitionStoreStub) SaveControlDrainedReceipt(_ context.Context, hostID, requestID string, receipt json.RawMessage) (control.Transition, error) {
	value, err := s.lookup(hostID, requestID)
	if err != nil {
		return control.Transition{}, err
	}
	value.DrainedReceipt = append(json.RawMessage{}, receipt...)
	value.Phase = control.PhaseDrained
	s.value = &value
	return cloneControlTransition(value), nil
}

func (s *controlTransitionStoreStub) CompleteControlTransition(_ context.Context, hostID, requestID string) (control.Transition, error) {
	value, err := s.lookup(hostID, requestID)
	if err != nil {
		return control.Transition{}, err
	}
	hostValue, err := s.hosts.GetHost(context.Background(), hostID)
	if err != nil {
		return control.Transition{}, err
	}
	if value.Action == control.ActionTakeOver {
		hostValue.ControlState = host.ControlUser
	} else {
		hostValue.ControlState = host.ControlAI
	}
	hostValue.ControlVersion = value.TargetControlVersion
	s.hosts.addHost(hostValue)
	value.Phase = control.PhaseCompleted
	s.value = &value
	return cloneControlTransition(value), nil
}

func (s *controlTransitionStoreStub) RecordControlTransitionFailure(_ context.Context, hostID, requestID, reason string) error {
	value, err := s.lookup(hostID, requestID)
	if err != nil {
		return err
	}
	value.FailureReason = reason
	s.value = &value
	return nil
}

func (s *controlTransitionStoreStub) lookup(hostID, requestID string) (control.Transition, error) {
	if s.value == nil || s.value.HostID != hostID || s.value.RequestID != requestID {
		return control.Transition{}, domain.NewError(domain.CodeNotFound, "transition not found")
	}
	return cloneControlTransition(*s.value), nil
}

func cloneControlTransition(value control.Transition) control.Transition {
	value.TaskIDs = append([]string{}, value.TaskIDs...)
	value.DrainedReceipt = append(json.RawMessage{}, value.DrainedReceipt...)
	return value
}

func sameTestControlStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
