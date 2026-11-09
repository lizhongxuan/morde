// Package runtimecore implements the Host-scoped operation and control state
// machine. Platform actions are explicit handlers; absent handlers fail closed.
package runtimecore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"morde-runtime/contract"
	"morde-runtime/internal/operationstore"
)

const (
	MaxRequestBytes         = 1 << 20
	MaxResponseBytes        = 8 << 20
	MaxOperationResultBytes = MaxResponseBytes - (64 << 10)
	MaxFileReadResultBytes  = 64 << 10
)

var (
	ErrInvalidRequest          = &RuntimeError{Code: "invalid_request"}
	ErrRuntimeAuthentication   = &RuntimeError{Code: "runtime_authentication_failed"}
	ErrFenceRejected           = &RuntimeError{Code: "fence_rejected"}
	ErrOperationConflict       = &RuntimeError{Code: "operation_conflict"}
	ErrProtocolIncompatible    = &RuntimeError{Code: "protocol_incompatible"}
	ErrCapabilityUnavailable   = &RuntimeError{Code: "capability_unavailable"}
	ErrOperationNotFound       = &RuntimeError{Code: "operation_not_found"}
	ErrResultTooLarge          = &RuntimeError{Code: "result_too_large"}
	ErrRuntimeStateUnavailable = &RuntimeError{Code: "runtime_state_unavailable"}
	ErrDesktopFocusChanged     = &RuntimeError{Code: "desktop_focus_changed"}
	ErrDesktopGeometryChanged  = &RuntimeError{Code: "desktop_geometry_changed"}
	// ErrOperationUncertain marks an action whose side effect may have started,
	// but whose handler could not prove a terminal result. Such operations must
	// remain queryable as unknown instead of being reported as ordinary failure.
	ErrOperationUncertain = errors.New("operation result is uncertain")
	ErrOperationsPending  = errors.New("operations remain unresolved")
)

type RuntimeError struct{ Code string }

func (e *RuntimeError) Error() string { return e.Code }

// ActionHandler implementations must return from Execute only after the whole
// operation tree has ended. Stop may report VerifiedStopped only after it has
// proved that every attributed process and queued action has stopped.
type ActionHandler interface {
	Capability() string
	Action() string
	Execute(context.Context, contract.ExecuteRequest) (json.RawMessage, error)
	Stop(context.Context, contract.Operation) (StopResult, error)
}

type StopResult struct {
	VerifiedStopped bool
	PendingIDs      []string
}

type Options struct {
	StoreDir           string
	ExpectedOwnerUID   uint32
	Store              operationstore.EventStore
	HostID             string
	Generation         int64
	ProtocolVersion    string
	Handlers           []ActionHandler
	TaskWorkspaceViews TaskWorkspaceViewStore
	Now                func() time.Time
}

type TaskWorkspaceViewStore interface {
	PrepareTaskWorkspaceView(context.Context, string, string, string, string, int64, int64, int64, string) error
	DeleteTaskWorkspaceView(context.Context, string, string, string) error
	ExportTaskWorkspaceManifestPage(context.Context, contract.TaskWorkspaceManifestRequest) (contract.TaskWorkspaceManifestResponse, error)
	ReadTaskWorkspaceBlock(context.Context, string, string, string, string) ([]byte, error)
}

type taskWorkspaceView struct {
	workspaceID        string
	bindingVersion     int64
	inputRevision      int64
	checkpointRevision int64
	manifestSHA256     string
	viewID             string
}

type taskWorkspacePublish struct {
	workspaceID        string
	bindingVersion     int64
	inputRevision      int64
	checkpointRevision int64
	inputManifestSHA   string
	viewID             string
	manifestSHA        string
}

type taskLease struct {
	Fence         contract.HostFence `json:"fence"`
	Version       int64              `json:"lease_version"`
	WorkspaceTask bool               `json:"workspace_task,omitempty"`
	ExpiresAt     time.Time          `json:"expires_at"`
}

type receiptState struct {
	Receipt  contract.DrainedReceipt `json:"receipt"`
	Consumed bool                    `json:"consumed"`
}

type runtimeState struct {
	HostID         string               `json:"host_id"`
	Generation     int64                `json:"runtime_generation"`
	InstanceID     string               `json:"runtime_instance_id"`
	ControlVersion int64                `json:"control_version"`
	Mode           contract.Mode        `json:"mode"`
	Leases         map[string]taskLease `json:"leases"`
	Receipt        *receiptState        `json:"drained_receipt,omitempty"`
}

type operationRecord struct {
	Operation   contract.Operation       `json:"operation"`
	RequestHash string                   `json:"request_hash"`
	Request     *contract.ExecuteRequest `json:"request,omitempty"`
}

type activeOperation struct {
	cancel  context.CancelFunc
	done    chan struct{}
	started bool
}

type Manager struct {
	closeMu                   sync.Mutex
	mu                        sync.Mutex
	workspaceBindingMu        sync.Mutex
	store                     operationstore.EventStore
	legacyStore               *operationstore.Store
	state                     runtimeState
	stateSet                  bool
	workspaceBindings         map[string]contract.WorkspaceBinding
	workspaceBindingVersions  map[string]workspaceBindingVersion
	taskWorkspaceViews        map[string]taskWorkspaceView
	taskWorkspaceAdvancing    map[string]bool
	taskWorkspacePublishing   map[string]taskWorkspacePublish
	taskWorkspaceDeleting     map[string]string
	taskWorkspaceDeleted      map[string]string
	taskWorkspaceViewStore    TaskWorkspaceViewStore
	blockedWorkspaceBindings  map[string]struct{}
	workspaceBindingsUpdating bool
	ops                       map[string]operationRecord
	handlers                  map[string]ActionHandler
	active                    map[string]activeOperation
	protocol                  string
	now                       func() time.Time
	closed                    bool
	poison                    error
	wg                        sync.WaitGroup
}

type workspaceBindingVersion struct {
	version int64
	active  bool
}

func Open(options Options) (*Manager, error) {
	if options.StoreDir == "" || !validIdentifier(options.HostID, 'h') || options.Generation <= 0 {
		return nil, ErrInvalidRequest
	}
	if options.ProtocolVersion == "" {
		options.ProtocolVersion = contract.ProtocolVersion
	}
	if options.ProtocolVersion != contract.ProtocolVersion {
		return nil, ErrProtocolIncompatible
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	legacyStore, err := operationstore.Open(options.StoreDir, options.ExpectedOwnerUID)
	if err != nil {
		return nil, ErrRuntimeStateUnavailable
	}
	store := operationstore.EventStore(legacyStore)
	var preservedLegacy *operationstore.Store
	if options.Store != nil {
		reconciler, ok := options.Store.(interface {
			Reconcile(operationstore.EventStore) error
		})
		if !ok {
			_ = legacyStore.Close()
			_ = options.Store.Close()
			return nil, ErrRuntimeStateUnavailable
		}
		if err := reconciler.Reconcile(legacyStore); err != nil {
			_ = legacyStore.Close()
			_ = options.Store.Close()
			return nil, ErrRuntimeStateUnavailable
		}
		store = options.Store
		preservedLegacy = legacyStore
	}
	m := &Manager{
		store: store, legacyStore: preservedLegacy, ops: make(map[string]operationRecord), handlers: make(map[string]ActionHandler),
		active: make(map[string]activeOperation), protocol: options.ProtocolVersion, now: options.Now,
		workspaceBindings: make(map[string]contract.WorkspaceBinding), workspaceBindingVersions: make(map[string]workspaceBindingVersion),
		taskWorkspaceViews: make(map[string]taskWorkspaceView), taskWorkspaceAdvancing: make(map[string]bool),
		taskWorkspacePublishing:  make(map[string]taskWorkspacePublish),
		taskWorkspaceDeleting:    make(map[string]string),
		taskWorkspaceDeleted:     make(map[string]string),
		taskWorkspaceViewStore:   options.TaskWorkspaceViews,
		blockedWorkspaceBindings: make(map[string]struct{}),
	}
	for _, handler := range options.Handlers {
		if handler == nil || strings.TrimSpace(handler.Capability()) == "" || strings.TrimSpace(handler.Action()) == "" {
			m.closeStores()
			return nil, ErrInvalidRequest
		}
		key := handlerKey(handler.Capability(), handler.Action())
		if _, exists := m.handlers[key]; exists {
			m.closeStores()
			return nil, ErrInvalidRequest
		}
		m.handlers[key] = handler
	}
	events, err := store.Events()
	if err != nil {
		m.closeStores()
		return nil, ErrRuntimeStateUnavailable
	}
	for _, event := range events {
		if err := m.applyEvent(event); err != nil {
			m.closeStores()
			return nil, ErrRuntimeStateUnavailable
		}
	}
	for _, record := range m.ops {
		if record.Operation.Status == contract.OperationUnknown {
			m.closeStores()
			return nil, ErrRuntimeStateUnavailable
		}
	}
	instanceID, err := randomID("ri_")
	if err != nil {
		m.closeStores()
		return nil, ErrRuntimeStateUnavailable
	}
	if !m.stateSet {
		m.state = runtimeState{HostID: options.HostID, Generation: options.Generation, InstanceID: instanceID, Mode: contract.ModeBlocked, Leases: make(map[string]taskLease)}
		if err := m.appendStateLocked(); err != nil {
			m.closeStores()
			return nil, ErrRuntimeStateUnavailable
		}
		return m, nil
	}
	if m.state.HostID != options.HostID || options.Generation < m.state.Generation {
		m.closeStores()
		return nil, ErrFenceRejected
	}
	for id, op := range m.ops {
		switch op.Operation.Status {
		case contract.OperationAccepted:
			// accepted proves that no side effect started. Since the prior instance
			// fence is obsolete, close this attempt without replaying it.
			if err := m.setOperationLocked(id, contract.OperationFailed, errorResult(ErrFenceRejected.Code)); err != nil {
				m.closeStores()
				return nil, ErrRuntimeStateUnavailable
			}
		case contract.OperationRunning, contract.OperationCancelling:
			// The process state cannot be inferred after a runtime restart.
			if err := m.setOperationLocked(id, contract.OperationUnknown, nil); err != nil {
				m.closeStores()
				return nil, ErrRuntimeStateUnavailable
			}
		}
	}
	for _, record := range m.ops {
		if record.Operation.Status == contract.OperationUnknown {
			m.closeStores()
			return nil, ErrRuntimeStateUnavailable
		}
	}
	m.state.Generation = options.Generation
	m.state.InstanceID = instanceID
	m.state.Mode = contract.ModeBlocked
	m.state.Receipt = nil
	// Task leases are fenced to the old runtime instance. The Morde server must
	// reissue its current lease after the fresh capabilities handshake.
	m.state.Leases = make(map[string]taskLease)
	if err := m.appendStateLocked(); err != nil {
		m.closeStores()
		return nil, ErrRuntimeStateUnavailable
	}
	return m, nil
}

// SetWorkspaceBindings drains operations that use changed bindings before
// installing Morde's authoritative Workspace snapshot.
func (m *Manager) SetWorkspaceBindings(ctx context.Context, request contract.SetWorkspaceBindingsRequest) (contract.SetWorkspaceBindingsResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.workspaceBindingMu.Lock()
	defer m.workspaceBindingMu.Unlock()
	if err := ctx.Err(); err != nil {
		return contract.SetWorkspaceBindingsResponse{}, err
	}
	m.mu.Lock()
	if err := m.readyLocked(); err != nil {
		m.mu.Unlock()
		return contract.SetWorkspaceBindingsResponse{}, err
	}
	if err := m.checkProtocol(request.ProtocolVersion); err != nil {
		m.mu.Unlock()
		return contract.SetWorkspaceBindingsResponse{}, err
	}
	if !validFenceShape(request.HostFence) || !validWorkspaceBindingSnapshot(request.Bindings) {
		m.mu.Unlock()
		return contract.SetWorkspaceBindingsResponse{}, ErrInvalidRequest
	}
	if !m.matchesCurrentFenceLocked(request.HostFence) {
		m.mu.Unlock()
		return contract.SetWorkspaceBindingsResponse{}, ErrFenceRejected
	}
	next := make(map[string]contract.WorkspaceBinding, len(request.Bindings))
	for _, binding := range request.Bindings {
		previous, known := m.workspaceBindingVersions[binding.WorkspaceID]
		if known && (binding.BindingVersion < previous.version ||
			(binding.BindingVersion == previous.version && !previous.active) ||
			(binding.BindingVersion == previous.version && previous.active && m.workspaceBindings[binding.WorkspaceID].Directory != binding.Directory)) {
			m.mu.Unlock()
			return contract.SetWorkspaceBindingsResponse{}, ErrFenceRejected
		}
		next[binding.WorkspaceID] = binding
	}
	affected := make(map[string]struct{})
	for id, binding := range m.workspaceBindings {
		updated, exists := next[id]
		if !exists || binding.BindingVersion != updated.BindingVersion || binding.Directory != updated.Directory {
			affected[id] = struct{}{}
		}
	}
	for id := range m.blockedWorkspaceBindings {
		affected[id] = struct{}{}
	}
	for _, record := range m.ops {
		operation := record.Operation
		if operation.WorkspaceID == "" || !isPending(operation.Status) {
			continue
		}
		binding, exists := next[operation.WorkspaceID]
		if !exists || binding.BindingVersion != operation.BindingVersion {
			affected[operation.WorkspaceID] = struct{}{}
		}
	}
	m.workspaceBindingsUpdating = true
	defer func() {
		m.mu.Lock()
		m.workspaceBindingsUpdating = false
		m.mu.Unlock()
	}()
	operationIDs := make([]string, 0)
	for id, record := range m.ops {
		if _, changed := affected[record.Operation.WorkspaceID]; changed && isPending(record.Operation.Status) {
			operationIDs = append(operationIDs, id)
		}
	}
	sort.Strings(operationIDs)
	m.mu.Unlock()

	var stopErr error
	for _, operationID := range operationIDs {
		if err := ctx.Err(); err != nil {
			stopErr = err
			break
		}
		operation, err := m.CancelOperation(ctx, request.ProtocolVersion, operationID, contract.CancelOperationRequest{
			ProtocolVersion: request.ProtocolVersion, HostFence: request.HostFence,
		})
		if err != nil && stopErr == nil {
			stopErr = err
		}
		if err == nil && isPending(operation.Status) && stopErr == nil {
			stopErr = ErrOperationsPending
		}
	}

	m.mu.Lock()
	for id := range affected {
		if m.hasPendingWorkspaceOperationLocked(id) {
			m.blockedWorkspaceBindings[id] = struct{}{}
		} else {
			delete(m.blockedWorkspaceBindings, id)
		}
	}
	if stopErr != nil {
		m.mu.Unlock()
		return contract.SetWorkspaceBindingsResponse{}, stopErr
	}
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		return contract.SetWorkspaceBindingsResponse{}, err
	}
	if err := m.readyLocked(); err != nil {
		m.mu.Unlock()
		return contract.SetWorkspaceBindingsResponse{}, err
	}
	if !m.matchesCurrentFenceLocked(request.HostFence) {
		m.mu.Unlock()
		return contract.SetWorkspaceBindingsResponse{}, ErrFenceRejected
	}
	for id, previous := range m.workspaceBindingVersions {
		if _, stillActive := next[id]; !stillActive && previous.active {
			previous.active = false
			m.workspaceBindingVersions[id] = previous
		}
	}
	for id, binding := range next {
		m.workspaceBindingVersions[id] = workspaceBindingVersion{version: binding.BindingVersion, active: true}
	}
	m.workspaceBindings = next
	bindings := append([]contract.WorkspaceBinding(nil), request.Bindings...)
	if bindings == nil {
		bindings = []contract.WorkspaceBinding{}
	}
	response := contract.SetWorkspaceBindingsResponse{
		ProtocolVersion: m.protocol,
		HostFence:       contract.HostFence{HostID: m.state.HostID, RuntimeGeneration: m.state.Generation, RuntimeInstanceID: m.state.InstanceID, ControlVersion: m.state.ControlVersion},
		Bindings:        bindings,
	}
	m.mu.Unlock()
	return response, nil
}

// RegisterHandlers installs optional capability providers before the HTTP
// listener starts. It rejects registration after any operation has become
// active so that a capability cannot change under an executing request.
func (m *Manager) RegisterHandlers(handlers []ActionHandler) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.readyLocked(); err != nil {
		return err
	}
	if len(m.active) != 0 {
		return ErrOperationsPending
	}
	for _, handler := range handlers {
		if handler == nil || strings.TrimSpace(handler.Capability()) == "" || strings.TrimSpace(handler.Action()) == "" {
			return ErrInvalidRequest
		}
		key := handlerKey(handler.Capability(), handler.Action())
		if _, exists := m.handlers[key]; exists {
			return ErrInvalidRequest
		}
		m.handlers[key] = handler
	}
	return nil
}

func (m *Manager) hasPendingWorkspaceOperationLocked(workspaceID string) bool {
	for _, record := range m.ops {
		if record.Operation.WorkspaceID == workspaceID && isPending(record.Operation.Status) {
			return true
		}
	}
	return false
}

func (m *Manager) Capabilities(request contract.CapabilitiesRequest) (contract.CapabilitiesResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.readyLocked(); err != nil {
		return contract.CapabilitiesResponse{}, err
	}
	if err := m.checkProtocol(request.ProtocolVersion); err != nil {
		return contract.CapabilitiesResponse{}, err
	}
	if !validIdentifier(request.HostID, 'h') || request.RuntimeGeneration <= 0 || request.ControlVersion < 0 {
		return contract.CapabilitiesResponse{}, ErrInvalidRequest
	}
	if request.HostID != m.state.HostID || request.RuntimeGeneration != m.state.Generation {
		return contract.CapabilitiesResponse{}, ErrFenceRejected
	}
	capabilities := []string{"operation.execute", "operation.query", "operation.cancel", "control.fence", "control.quiesce", "task_lease.fence"}
	if m.taskWorkspaceViewStore != nil {
		capabilities = append(capabilities, "workspace.task_view")
	}
	seen := make(map[string]struct{}, len(capabilities)+len(m.handlers))
	for _, capability := range capabilities {
		seen[capability] = struct{}{}
	}
	for _, handler := range m.handlers {
		if _, exists := seen[handler.Capability()]; !exists {
			capabilities = append(capabilities, handler.Capability())
			seen[handler.Capability()] = struct{}{}
		}
	}
	sort.Strings(capabilities)
	return contract.CapabilitiesResponse{
		ProtocolVersion: m.protocol, HostID: m.state.HostID, RuntimeGeneration: m.state.Generation,
		RuntimeInstanceID: m.state.InstanceID, ControlVersion: m.state.ControlVersion, Mode: m.state.Mode,
		OperationStoreReady: true, Capabilities: capabilities,
	}, nil
}

// RunIfCurrentFence checks and commits a short, synchronous action while the
// host control fence cannot change. The action must not call back into Manager.
// This lets callers durably accept an asynchronous operation without a stale
// fence slipping between validation and its start.
func (m *Manager) RunIfCurrentFence(protocol string, fence contract.HostFence, action func() error) (contract.HostFence, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.readyLocked(); err != nil {
		return contract.HostFence{}, err
	}
	if err := m.checkProtocol(protocol); err != nil {
		return contract.HostFence{}, err
	}
	if !validFenceShape(fence) || action == nil {
		return contract.HostFence{}, ErrInvalidRequest
	}
	if !m.matchesCurrentFenceLocked(fence) {
		return contract.HostFence{}, ErrFenceRejected
	}
	if err := action(); err != nil {
		return contract.HostFence{}, err
	}
	if !m.matchesCurrentFenceLocked(fence) {
		return contract.HostFence{}, ErrFenceRejected
	}
	return contract.HostFence{
		HostID: m.state.HostID, RuntimeGeneration: m.state.Generation,
		RuntimeInstanceID: m.state.InstanceID, ControlVersion: m.state.ControlVersion,
	}, nil
}

func (m *Manager) SetControl(request contract.SetControlRequest) (contract.SetControlResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.readyLocked(); err != nil {
		return contract.SetControlResponse{}, err
	}
	if err := m.checkProtocol(request.ProtocolVersion); err != nil {
		return contract.SetControlResponse{}, err
	}
	if !validFenceShape(request.HostFence) {
		return contract.SetControlResponse{}, ErrInvalidRequest
	}
	if !m.matchesRuntimeLocked(request.HostFence) || request.ControlVersion < m.state.ControlVersion {
		return contract.SetControlResponse{}, ErrFenceRejected
	}
	if request.Mode != contract.ModeBlocked && request.Mode != contract.ModeAI && request.Mode != contract.ModeHuman {
		return contract.SetControlResponse{}, ErrInvalidRequest
	}
	if request.Mode == contract.ModeHuman && request.DrainedReceipt == nil || request.Mode != contract.ModeHuman && request.DrainedReceipt != nil {
		return contract.SetControlResponse{}, ErrInvalidRequest
	}
	if request.ControlVersion == m.state.ControlVersion {
		if request.Mode != m.state.Mode {
			// A restarted runtime starts blocked but keeps the Host's accepted
			// control version. Permit only blocked→AI reconciliation at that
			// version, after the caller has verified this new runtime instance.
			// Human input always requires a higher-version drained receipt.
			if m.state.Mode != contract.ModeBlocked || request.Mode != contract.ModeAI || m.hasUnresolvedLocked() {
				return contract.SetControlResponse{}, ErrFenceRejected
			}
			m.state.Mode = contract.ModeAI
			if err := m.appendStateLocked(); err != nil {
				return contract.SetControlResponse{}, ErrRuntimeStateUnavailable
			}
			return controlResponse(m.protocol, m.state), nil
		}
		if request.Mode == contract.ModeHuman && (m.state.Receipt == nil || !sameReceipt(*request.DrainedReceipt, m.state.Receipt.Receipt)) {
			return contract.SetControlResponse{}, ErrFenceRejected
		}
		return controlResponse(m.protocol, m.state), nil
	}
	switch request.Mode {
	case contract.ModeBlocked:
		m.state.ControlVersion = request.ControlVersion
		m.state.Mode = contract.ModeBlocked
		m.state.Receipt = nil
	case contract.ModeAI:
		if m.hasUnresolvedLocked() {
			return contract.SetControlResponse{}, ErrFenceRejected
		}
		m.state.ControlVersion = request.ControlVersion
		m.state.Mode = contract.ModeAI
		m.state.Receipt = nil
	case contract.ModeHuman:
		if m.state.Mode != contract.ModeBlocked || request.DrainedReceipt == nil || !m.receiptMatchesLocked(*request.DrainedReceipt, request.ControlVersion) || m.hasUnresolvedLocked() {
			return contract.SetControlResponse{}, ErrFenceRejected
		}
		m.state.ControlVersion = request.ControlVersion
		m.state.Mode = contract.ModeHuman
		m.state.Receipt.Consumed = true
	}
	if err := m.appendStateLocked(); err != nil {
		return contract.SetControlResponse{}, ErrRuntimeStateUnavailable
	}
	return controlResponse(m.protocol, m.state), nil
}

func (m *Manager) SetTaskLease(taskID string, request contract.SetTaskLeaseRequest) (contract.SetTaskLeaseResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.readyLocked(); err != nil {
		return contract.SetTaskLeaseResponse{}, err
	}
	if err := m.checkProtocol(request.ProtocolVersion); err != nil {
		return contract.SetTaskLeaseResponse{}, err
	}
	if !validIdentifier(taskID, 'q') || request.LeaseVersion <= 0 || !isUTC(request.ExpiresAt) || !validFenceShape(request.HostFence) {
		return contract.SetTaskLeaseResponse{}, ErrInvalidRequest
	}
	now := m.now()
	if !m.matchesCurrentFenceLocked(request.HostFence) || m.state.Mode != contract.ModeAI ||
		!request.ExpiresAt.After(now) || request.ExpiresAt.After(now.Add(contract.MaxTaskLeaseDuration)) {
		return contract.SetTaskLeaseResponse{}, ErrFenceRejected
	}
	if m.taskWorkspaceDeleting[taskID] != "" || m.taskWorkspaceDeleted[taskID] != "" {
		return contract.SetTaskLeaseResponse{}, ErrFenceRejected
	}
	previous, exists := m.state.Leases[taskID]
	if exists {
		switch {
		case request.LeaseVersion < previous.Version:
			return contract.SetTaskLeaseResponse{}, ErrFenceRejected
		case request.LeaseVersion == previous.Version:
			if !sameFence(previous.Fence, request.HostFence) || previous.WorkspaceTask != request.WorkspaceTask {
				return contract.SetTaskLeaseResponse{}, ErrFenceRejected
			}
			if request.ExpiresAt.Equal(previous.ExpiresAt) {
				return leaseResponse(m.protocol, taskID, previous), nil
			}
			if !request.ExpiresAt.After(previous.ExpiresAt) || !previous.ExpiresAt.After(now) {
				return contract.SetTaskLeaseResponse{}, ErrFenceRejected
			}
		}
	}
	lease := taskLease{Fence: request.HostFence, Version: request.LeaseVersion, WorkspaceTask: request.WorkspaceTask, ExpiresAt: request.ExpiresAt.UTC()}
	m.state.Leases[taskID] = lease
	if err := m.appendStateLocked(); err != nil {
		return contract.SetTaskLeaseResponse{}, ErrRuntimeStateUnavailable
	}
	return leaseResponse(m.protocol, taskID, lease), nil
}

func (m *Manager) SetTaskWorkspaceView(ctx context.Context, taskID string, request contract.SetTaskWorkspaceViewRequest) (contract.SetTaskWorkspaceViewResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.checkProtocol(request.ProtocolVersion); err != nil {
		return contract.SetTaskWorkspaceViewResponse{}, err
	}
	if !validIdentifier(taskID, 'q') || request.TaskID != taskID || !validIdentifier(request.WorkspaceID, 'w') ||
		request.BindingVersion < 1 || request.InputRevision < 0 || request.WorkspaceCheckpointRevision < request.InputRevision ||
		!validLowerSHA256(request.ManifestSHA256) || !validFenceShape(request.HostFence) {
		return contract.SetTaskWorkspaceViewResponse{}, ErrInvalidRequest
	}
	if m.taskWorkspaceViewStore == nil {
		return contract.SetTaskWorkspaceViewResponse{}, ErrCapabilityUnavailable
	}
	viewID, err := taskWorkspaceViewID(request)
	if err != nil {
		return contract.SetTaskWorkspaceViewResponse{}, ErrInvalidRequest
	}
	m.workspaceBindingMu.Lock()
	defer m.workspaceBindingMu.Unlock()

	m.mu.Lock()
	if err := m.readyLocked(); err != nil {
		m.mu.Unlock()
		return contract.SetTaskWorkspaceViewResponse{}, err
	}
	binding, exists := m.workspaceBindings[request.WorkspaceID]
	if !m.matchesCurrentFenceLocked(request.HostFence) || m.state.Mode != contract.ModeAI || m.workspaceBindingsUpdating ||
		!exists || binding.BindingVersion != request.BindingVersion || binding.Directory != request.WorkspaceID ||
		m.taskWorkspaceDeleting[taskID] != "" || m.taskWorkspaceDeleted[taskID] != "" {
		m.mu.Unlock()
		return contract.SetTaskWorkspaceViewResponse{}, ErrFenceRejected
	}
	if m.taskWorkspaceAdvancing[taskID] {
		m.mu.Unlock()
		return contract.SetTaskWorkspaceViewResponse{}, ErrFenceRejected
	}
	if previous, ok := m.taskWorkspaceViews[taskID]; ok {
		if previous.workspaceID != request.WorkspaceID || previous.bindingVersion != request.BindingVersion ||
			previous.inputRevision != request.InputRevision || previous.manifestSHA256 != request.ManifestSHA256 ||
			previous.viewID != viewID || request.WorkspaceCheckpointRevision < previous.checkpointRevision {
			m.mu.Unlock()
			return contract.SetTaskWorkspaceViewResponse{}, ErrFenceRejected
		}
		if previous.checkpointRevision == request.WorkspaceCheckpointRevision {
			if publishing, exists := m.taskWorkspacePublishing[taskID]; exists {
				if !sameTaskWorkspacePublish(publishing, request, viewID) || publishing.checkpointRevision != request.WorkspaceCheckpointRevision {
					m.mu.Unlock()
					return contract.SetTaskWorkspaceViewResponse{}, ErrFenceRejected
				}
				delete(m.taskWorkspacePublishing, taskID)
			}
			m.mu.Unlock()
			return taskWorkspaceViewResponse(m.protocol, request, viewID), nil
		}
		if publishing, exists := m.taskWorkspacePublishing[taskID]; exists &&
			(!sameTaskWorkspacePublish(publishing, request, viewID) || publishing.checkpointRevision != previous.checkpointRevision) {
			m.mu.Unlock()
			return contract.SetTaskWorkspaceViewResponse{}, ErrFenceRejected
		}
		if m.hasPendingTaskOperationLocked(taskID) {
			m.mu.Unlock()
			return contract.SetTaskWorkspaceViewResponse{}, ErrOperationsPending
		}
	}
	m.taskWorkspaceAdvancing[taskID] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.taskWorkspaceAdvancing, taskID)
		m.mu.Unlock()
	}()

	if err := m.taskWorkspaceViewStore.PrepareTaskWorkspaceView(ctx, request.HostID, taskID, request.WorkspaceID, viewID,
		request.BindingVersion, request.InputRevision, request.WorkspaceCheckpointRevision, request.ManifestSHA256); err != nil {
		return contract.SetTaskWorkspaceViewResponse{}, ErrRuntimeStateUnavailable
	}

	m.mu.Lock()
	binding, exists = m.workspaceBindings[request.WorkspaceID]
	if !m.matchesCurrentFenceLocked(request.HostFence) || m.state.Mode != contract.ModeAI || !exists ||
		binding.BindingVersion != request.BindingVersion || binding.Directory != request.WorkspaceID {
		m.mu.Unlock()
		return contract.SetTaskWorkspaceViewResponse{}, ErrFenceRejected
	}
	if previous, ok := m.taskWorkspaceViews[taskID]; ok && (previous.viewID != viewID || request.WorkspaceCheckpointRevision < previous.checkpointRevision) {
		m.mu.Unlock()
		return contract.SetTaskWorkspaceViewResponse{}, ErrFenceRejected
	}
	m.taskWorkspaceViews[taskID] = taskWorkspaceView{
		workspaceID: request.WorkspaceID, bindingVersion: request.BindingVersion,
		inputRevision: request.InputRevision, checkpointRevision: request.WorkspaceCheckpointRevision,
		manifestSHA256: request.ManifestSHA256, viewID: viewID,
	}
	delete(m.taskWorkspacePublishing, taskID)
	m.mu.Unlock()
	return taskWorkspaceViewResponse(m.protocol, request, viewID), nil
}

func (m *Manager) DeleteTaskWorkspaceView(ctx context.Context, taskID string, request contract.DeleteTaskWorkspaceViewRequest) (contract.DeleteTaskWorkspaceViewResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.checkProtocol(request.ProtocolVersion); err != nil {
		return contract.DeleteTaskWorkspaceViewResponse{}, err
	}
	if !contract.ValidDeleteTaskWorkspaceViewRequest(request, taskID) {
		return contract.DeleteTaskWorkspaceViewResponse{}, ErrInvalidRequest
	}
	if m.taskWorkspaceViewStore == nil {
		return contract.DeleteTaskWorkspaceViewResponse{}, ErrCapabilityUnavailable
	}

	// Serialize deletion with binding replacement and view preparation. The
	// per-Task marker rejects new leases, operations, exports, and view setup
	// while the durable filesystem tombstone and recursive removal are written.
	m.workspaceBindingMu.Lock()
	defer m.workspaceBindingMu.Unlock()

	m.mu.Lock()
	if err := m.readyLocked(); err != nil {
		m.mu.Unlock()
		return contract.DeleteTaskWorkspaceViewResponse{}, err
	}
	if !m.matchesCurrentFenceLocked(request.HostFence) {
		m.mu.Unlock()
		return contract.DeleteTaskWorkspaceViewResponse{}, ErrFenceRejected
	}
	if existing, deleted := m.taskWorkspaceDeleted[taskID]; deleted {
		m.mu.Unlock()
		if existing == request.RequestID {
			return deletedTaskWorkspaceViewResponse(m.protocol, request), nil
		}
		return contract.DeleteTaskWorkspaceViewResponse{}, ErrFenceRejected
	}
	if existing, deleting := m.taskWorkspaceDeleting[taskID]; deleting && existing != request.RequestID {
		m.mu.Unlock()
		return contract.DeleteTaskWorkspaceViewResponse{}, ErrFenceRejected
	}
	if m.taskWorkspaceAdvancing[taskID] || m.taskWorkspacePublishing[taskID].viewID != "" || m.hasPendingTaskOperationLocked(taskID) {
		m.mu.Unlock()
		return contract.DeleteTaskWorkspaceViewResponse{}, ErrOperationsPending
	}
	if lease, exists := m.state.Leases[taskID]; exists {
		if lease.Version < 1 || !isUTC(lease.ExpiresAt) || !validFenceShape(lease.Fence) || !sameFence(lease.Fence, request.HostFence) {
			m.mu.Unlock()
			return contract.DeleteTaskWorkspaceViewResponse{}, ErrRuntimeStateUnavailable
		}
		if lease.ExpiresAt.After(m.now()) {
			m.mu.Unlock()
			return contract.DeleteTaskWorkspaceViewResponse{}, ErrOperationsPending
		}
	}
	for operationID := range m.active {
		operation, exists := m.ops[operationID]
		if !exists {
			m.mu.Unlock()
			return contract.DeleteTaskWorkspaceViewResponse{}, ErrRuntimeStateUnavailable
		}
		if operation.Operation.TaskID == taskID {
			m.mu.Unlock()
			return contract.DeleteTaskWorkspaceViewResponse{}, ErrOperationsPending
		}
	}
	m.taskWorkspaceDeleting[taskID] = request.RequestID
	m.mu.Unlock()

	if err := m.taskWorkspaceViewStore.DeleteTaskWorkspaceView(ctx, request.HostID, taskID, request.RequestID); err != nil {
		// Keep the Task fenced until the same persisted request retries. The
		// filesystem may already have written its tombstone before returning an
		// I/O error, so allowing new Task work here would be unsafe.
		return contract.DeleteTaskWorkspaceViewResponse{}, ErrRuntimeStateUnavailable
	}

	m.mu.Lock()
	delete(m.taskWorkspaceViews, taskID)
	delete(m.taskWorkspacePublishing, taskID)
	delete(m.taskWorkspaceDeleting, taskID)
	m.taskWorkspaceDeleted[taskID] = request.RequestID
	m.mu.Unlock()
	return deletedTaskWorkspaceViewResponse(m.protocol, request), nil
}

func deletedTaskWorkspaceViewResponse(protocol string, request contract.DeleteTaskWorkspaceViewRequest) contract.DeleteTaskWorkspaceViewResponse {
	return contract.DeleteTaskWorkspaceViewResponse{
		ProtocolVersion: protocol, HostFence: request.HostFence, TaskID: request.TaskID, RequestID: request.RequestID, Deleted: true,
	}
}

func sameTaskWorkspacePublish(publish taskWorkspacePublish, request contract.SetTaskWorkspaceViewRequest, viewID string) bool {
	return publish.workspaceID == request.WorkspaceID && publish.bindingVersion == request.BindingVersion &&
		publish.inputRevision == request.InputRevision && publish.inputManifestSHA == request.ManifestSHA256 && publish.viewID == viewID
}

func taskWorkspaceViewResponse(protocol string, request contract.SetTaskWorkspaceViewRequest, viewID string) contract.SetTaskWorkspaceViewResponse {
	return contract.SetTaskWorkspaceViewResponse{
		ProtocolVersion: protocol, HostFence: request.HostFence, TaskID: request.TaskID,
		WorkspaceID: request.WorkspaceID, BindingVersion: request.BindingVersion,
		InputRevision: request.InputRevision, WorkspaceCheckpointRevision: request.WorkspaceCheckpointRevision,
		ManifestSHA256: request.ManifestSHA256, ViewID: viewID,
	}
}

func (m *Manager) hasPendingTaskOperationLocked(taskID string) bool {
	for _, operation := range m.ops {
		if operation.Operation.TaskID == taskID && isPending(operation.Operation.Status) {
			return true
		}
	}
	return false
}

func (m *Manager) TaskWorkspaceManifestPage(ctx context.Context, taskID string, request contract.TaskWorkspaceManifestRequest) (contract.TaskWorkspaceManifestResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.checkProtocol(request.ProtocolVersion); err != nil {
		return contract.TaskWorkspaceManifestResponse{}, err
	}
	if request.TaskID != taskID || !validTaskWorkspaceExportRequestShape(taskWorkspaceBlockRequestForManifest(request), taskID) ||
		!validLowerSHA256(request.InputManifestSHA256) || request.Limit < 1 || request.Limit > 1000 || len(request.AfterPath) > 4096 ||
		request.ManifestSHA256 != "" && !validLowerSHA256(request.ManifestSHA256) || request.ManifestSHA256 == "" && request.AfterPath != "" {
		return contract.TaskWorkspaceManifestResponse{}, ErrInvalidRequest
	}
	if m.taskWorkspaceViewStore == nil {
		return contract.TaskWorkspaceManifestResponse{}, ErrCapabilityUnavailable
	}
	pinnedManifest, err := m.beginTaskWorkspacePublish(taskID, request)
	if err != nil {
		return contract.TaskWorkspaceManifestResponse{}, err
	}
	effective := request
	if effective.ManifestSHA256 == "" && pinnedManifest != "" {
		effective.ManifestSHA256 = pinnedManifest
	}
	page, err := m.taskWorkspaceViewStore.ExportTaskWorkspaceManifestPage(ctx, effective)
	if err != nil {
		return contract.TaskWorkspaceManifestResponse{}, ErrRuntimeStateUnavailable
	}
	if page.TaskID != taskID || page.WorkspaceID != request.WorkspaceID || page.BindingVersion != request.BindingVersion ||
		page.InputRevision != request.InputRevision || page.WorkspaceCheckpointRevision != request.WorkspaceCheckpointRevision ||
		page.ViewID != request.ViewID || !validLowerSHA256(page.InputManifestSHA256) || !validLowerSHA256(page.ManifestSHA256) ||
		len(page.Files) > request.Limit || len(page.NextCursor) > 4096 {
		return contract.TaskWorkspaceManifestResponse{}, ErrRuntimeStateUnavailable
	}
	if err := m.setTaskWorkspacePublishManifest(taskID, request, page.ManifestSHA256); err != nil {
		return contract.TaskWorkspaceManifestResponse{}, err
	}
	page.ProtocolVersion, page.HostFence = m.protocol, request.HostFence
	return page, nil
}

func (m *Manager) ReadTaskWorkspaceBlock(ctx context.Context, taskID string, request contract.TaskWorkspaceBlockRequest) (contract.TaskWorkspaceBlockResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.checkProtocol(request.ProtocolVersion); err != nil {
		return contract.TaskWorkspaceBlockResponse{}, err
	}
	if request.TaskID != taskID || !validTaskWorkspaceExportRequestShape(request, taskID) ||
		!validLowerSHA256(request.InputManifestSHA256) || !validLowerSHA256(request.ManifestSHA256) || !validLowerSHA256(request.SHA256) {
		return contract.TaskWorkspaceBlockResponse{}, ErrInvalidRequest
	}
	if err := m.validateTaskWorkspaceExportFence(taskID, request.HostFence, request.WorkspaceID, request.BindingVersion,
		request.InputRevision, request.WorkspaceCheckpointRevision, request.InputManifestSHA256, request.ViewID); err != nil {
		return contract.TaskWorkspaceBlockResponse{}, err
	}
	m.mu.Lock()
	publishing, exists := m.taskWorkspacePublishing[taskID]
	if !exists || !sameTaskWorkspacePublishExport(publishing, request.WorkspaceID, request.BindingVersion,
		request.InputRevision, request.WorkspaceCheckpointRevision, request.InputManifestSHA256, request.ViewID) ||
		publishing.manifestSHA != request.ManifestSHA256 {
		m.mu.Unlock()
		return contract.TaskWorkspaceBlockResponse{}, ErrFenceRejected
	}
	m.mu.Unlock()
	if m.taskWorkspaceViewStore == nil {
		return contract.TaskWorkspaceBlockResponse{}, ErrCapabilityUnavailable
	}
	data, err := m.taskWorkspaceViewStore.ReadTaskWorkspaceBlock(ctx, taskID, request.ViewID, request.ManifestSHA256, request.SHA256)
	if err != nil || len(data) == 0 || len(data) > 4<<20 {
		return contract.TaskWorkspaceBlockResponse{}, ErrRuntimeStateUnavailable
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != request.SHA256 {
		return contract.TaskWorkspaceBlockResponse{}, ErrRuntimeStateUnavailable
	}
	return contract.TaskWorkspaceBlockResponse{
		ProtocolVersion: m.protocol, HostFence: request.HostFence, TaskID: taskID,
		WorkspaceID: request.WorkspaceID, BindingVersion: request.BindingVersion, InputRevision: request.InputRevision,
		WorkspaceCheckpointRevision: request.WorkspaceCheckpointRevision, ViewID: request.ViewID,
		InputManifestSHA256: request.InputManifestSHA256, ManifestSHA256: request.ManifestSHA256,
		SHA256: request.SHA256, SizeBytes: int64(len(data)), Data: data,
	}, nil
}

func (m *Manager) beginTaskWorkspacePublish(taskID string, request contract.TaskWorkspaceManifestRequest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.readyLocked(); err != nil {
		return "", err
	}
	if !m.matchesCurrentFenceLocked(request.HostFence) || m.state.Mode != contract.ModeAI || m.workspaceBindingsUpdating ||
		m.taskWorkspaceAdvancing[taskID] || m.taskWorkspaceDeleting[taskID] != "" || m.taskWorkspaceDeleted[taskID] != "" {
		return "", ErrFenceRejected
	}
	binding, bindingOK := m.workspaceBindings[request.WorkspaceID]
	view, viewOK := m.taskWorkspaceViews[taskID]
	if !bindingOK || binding.BindingVersion != request.BindingVersion || binding.Directory != request.WorkspaceID || !viewOK ||
		view.workspaceID != request.WorkspaceID || view.bindingVersion != request.BindingVersion || view.inputRevision != request.InputRevision ||
		view.checkpointRevision != request.WorkspaceCheckpointRevision || view.manifestSHA256 != request.InputManifestSHA256 || view.viewID != request.ViewID {
		return "", ErrFenceRejected
	}
	if publish, exists := m.taskWorkspacePublishing[taskID]; exists {
		if !sameTaskWorkspacePublishExport(publish, request.WorkspaceID, request.BindingVersion, request.InputRevision,
			request.WorkspaceCheckpointRevision, request.InputManifestSHA256, request.ViewID) ||
			(request.ManifestSHA256 != "" && publish.manifestSHA != request.ManifestSHA256) {
			return "", ErrFenceRejected
		}
		return publish.manifestSHA, nil
	}
	if request.ManifestSHA256 != "" {
		return "", ErrFenceRejected
	}
	if m.hasPendingTaskOperationLocked(taskID) {
		return "", ErrOperationsPending
	}
	m.taskWorkspacePublishing[taskID] = taskWorkspacePublish{
		workspaceID: request.WorkspaceID, bindingVersion: request.BindingVersion, inputRevision: request.InputRevision,
		checkpointRevision: request.WorkspaceCheckpointRevision, inputManifestSHA: request.InputManifestSHA256, viewID: request.ViewID,
	}
	return "", nil
}

func sameTaskWorkspacePublishExport(publish taskWorkspacePublish, workspaceID string, bindingVersion, inputRevision, checkpointRevision int64, inputManifestSHA, viewID string) bool {
	return publish.workspaceID == workspaceID && publish.bindingVersion == bindingVersion && publish.inputRevision == inputRevision &&
		publish.checkpointRevision == checkpointRevision && publish.inputManifestSHA == inputManifestSHA && publish.viewID == viewID
}

func (m *Manager) setTaskWorkspacePublishManifest(taskID string, request contract.TaskWorkspaceManifestRequest, manifestSHA string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	publish, exists := m.taskWorkspacePublishing[taskID]
	if !exists || !sameTaskWorkspacePublishExport(publish, request.WorkspaceID, request.BindingVersion, request.InputRevision,
		request.WorkspaceCheckpointRevision, request.InputManifestSHA256, request.ViewID) ||
		!m.matchesCurrentFenceLocked(request.HostFence) || m.taskWorkspaceDeleting[taskID] != "" || m.taskWorkspaceDeleted[taskID] != "" ||
		!validLowerSHA256(manifestSHA) || publish.manifestSHA != "" && publish.manifestSHA != manifestSHA {
		return ErrFenceRejected
	}
	publish.manifestSHA = manifestSHA
	m.taskWorkspacePublishing[taskID] = publish
	return nil
}

func (m *Manager) validateTaskWorkspaceExportFence(taskID string, fence contract.HostFence, workspaceID string, bindingVersion, inputRevision, checkpointRevision int64, inputManifestSHA256, viewID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.readyLocked(); err != nil {
		return err
	}
	if !m.matchesCurrentFenceLocked(fence) || m.state.Mode != contract.ModeAI || m.workspaceBindingsUpdating ||
		m.taskWorkspaceDeleting[taskID] != "" || m.taskWorkspaceDeleted[taskID] != "" {
		return ErrFenceRejected
	}
	binding, bindingOK := m.workspaceBindings[workspaceID]
	view, viewOK := m.taskWorkspaceViews[taskID]
	if !validIdentifier(workspaceID, 'w') || bindingVersion < 1 || inputRevision < 0 || checkpointRevision < inputRevision ||
		!validTaskWorkspaceViewID(viewID) || !bindingOK || binding.BindingVersion != bindingVersion || binding.Directory != workspaceID ||
		!viewOK || view.workspaceID != workspaceID || view.bindingVersion != bindingVersion || view.inputRevision != inputRevision ||
		view.checkpointRevision != checkpointRevision || view.viewID != viewID ||
		inputManifestSHA256 != "" && view.manifestSHA256 != inputManifestSHA256 {
		return ErrFenceRejected
	}
	for operationID := range m.active {
		if operation, ok := m.ops[operationID]; ok && operation.Operation.TaskID == taskID && operation.Operation.WorkspaceID == workspaceID {
			return ErrRuntimeStateUnavailable
		}
	}
	return nil
}

func validTaskWorkspaceExportRequestShape(request contract.TaskWorkspaceBlockRequest, taskID string) bool {
	return validIdentifier(taskID, 'q') && validIdentifier(request.WorkspaceID, 'w') && request.BindingVersion >= 1 &&
		request.InputRevision >= 0 && request.WorkspaceCheckpointRevision >= request.InputRevision && validTaskWorkspaceViewID(request.ViewID) && validFenceShape(request.HostFence)
}

func taskWorkspaceBlockRequestForManifest(request contract.TaskWorkspaceManifestRequest) contract.TaskWorkspaceBlockRequest {
	return contract.TaskWorkspaceBlockRequest{
		HostFence: request.HostFence, TaskID: request.TaskID, WorkspaceID: request.WorkspaceID, BindingVersion: request.BindingVersion,
		InputRevision: request.InputRevision, WorkspaceCheckpointRevision: request.WorkspaceCheckpointRevision, ViewID: request.ViewID,
		InputManifestSHA256: request.InputManifestSHA256,
	}
}

func taskWorkspaceViewID(request contract.SetTaskWorkspaceViewRequest) (string, error) {
	identity := struct {
		Version            string `json:"version"`
		HostID             string `json:"host_id"`
		TaskID             string `json:"task_id"`
		WorkspaceID        string `json:"workspace_id"`
		BindingVersion     int64  `json:"binding_version"`
		InputRevision      int64  `json:"input_revision"`
		CheckpointRevision int64  `json:"workspace_checkpoint_revision"`
		ManifestSHA256     string `json:"manifest_sha256"`
	}{"morde-task-workspace-view/v1", request.HostID, request.TaskID, request.WorkspaceID, request.BindingVersion,
		request.InputRevision, request.InputRevision, request.ManifestSHA256}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "v" + hex.EncodeToString(digest[:]), nil
}

func validLowerSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func (m *Manager) Submit(ctx context.Context, request contract.ExecuteRequest) (contract.Operation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.checkProtocol(request.ProtocolVersion); err != nil {
		return contract.Operation{}, err
	}
	arguments, requestHash, err := normalizedRequest(request)
	if err != nil {
		return contract.Operation{}, ErrInvalidRequest
	}
	request.Arguments = arguments

	m.mu.Lock()
	if err := m.readyLocked(); err != nil {
		m.mu.Unlock()
		return contract.Operation{}, err
	}
	if m.taskWorkspaceAdvancing[request.TaskID] || m.taskWorkspacePublishing[request.TaskID].viewID != "" ||
		m.taskWorkspaceDeleting[request.TaskID] != "" || m.taskWorkspaceDeleted[request.TaskID] != "" {
		m.mu.Unlock()
		return contract.Operation{}, ErrFenceRejected
	}
	if !validIdentifier(request.OperationID, 'o') || !validIdentifier(request.TaskID, 'q') || strings.TrimSpace(request.Capability) == "" || strings.TrimSpace(request.Action) == "" || !validFenceShape(request.HostFence) || request.LeaseVersion <= 0 {
		m.mu.Unlock()
		return contract.Operation{}, ErrInvalidRequest
	}
	if !m.matchesCurrentFenceLocked(request.HostFence) || m.state.Mode != contract.ModeAI {
		m.mu.Unlock()
		return contract.Operation{}, ErrFenceRejected
	}
	if err := m.validateLeaseLocked(request.TaskID, request.LeaseVersion, request.HostFence); err != nil {
		m.mu.Unlock()
		return contract.Operation{}, err
	}
	lease := m.state.Leases[request.TaskID]
	if lease.WorkspaceTask && request.Capability == "desktop.input" {
		m.mu.Unlock()
		return contract.Operation{}, ErrFenceRejected
	}
	if contract.WorkspaceScopedCapability(request.Capability) {
		if m.workspaceBindingsUpdating {
			m.mu.Unlock()
			return contract.Operation{}, ErrFenceRejected
		}
		if _, blocked := m.blockedWorkspaceBindings[request.WorkspaceID]; blocked {
			m.mu.Unlock()
			return contract.Operation{}, ErrFenceRejected
		}
		binding, ok := m.workspaceBindings[request.WorkspaceID]
		view, viewOK := m.taskWorkspaceViews[request.TaskID]
		if !validIdentifier(request.WorkspaceID, 'w') || request.BindingVersion < 1 || !validTaskWorkspaceViewID(request.WorkspaceViewID) ||
			request.WorkspaceCheckpointRevision < 0 || !ok || binding.BindingVersion != request.BindingVersion || !viewOK ||
			view.workspaceID != request.WorkspaceID || view.bindingVersion != request.BindingVersion ||
			view.checkpointRevision != request.WorkspaceCheckpointRevision || view.viewID != request.WorkspaceViewID {
			m.mu.Unlock()
			return contract.Operation{}, ErrFenceRejected
		}
		request.WorkspaceDirectory = request.TaskID + "/" + request.WorkspaceViewID
	} else if request.WorkspaceID != "" || request.BindingVersion != 0 || request.WorkspaceViewID != "" || request.WorkspaceCheckpointRevision != 0 {
		m.mu.Unlock()
		return contract.Operation{}, ErrInvalidRequest
	}
	if previous, exists := m.ops[request.OperationID]; exists {
		m.mu.Unlock()
		if subtle.ConstantTimeCompare([]byte(previous.RequestHash), []byte(requestHash)) != 1 {
			return contract.Operation{}, ErrOperationConflict
		}
		return cloneOperation(previous.Operation), nil
	}
	operation := contract.Operation{
		HostID:            request.HostID,
		RuntimeGeneration: request.RuntimeGeneration, RuntimeInstanceID: request.RuntimeInstanceID,
		ControlVersion: request.ControlVersion, TaskID: request.TaskID,
		WorkspaceID: request.WorkspaceID, BindingVersion: request.BindingVersion,
		WorkspaceViewID: request.WorkspaceViewID, WorkspaceCheckpointRevision: request.WorkspaceCheckpointRevision,
		OperationID:  request.OperationID,
		LeaseVersion: request.LeaseVersion, Capability: request.Capability, Action: request.Action,
		Status: contract.OperationAccepted,
	}
	record := operationRecord{Operation: operation, RequestHash: requestHash, Request: cloneRequest(&request)}
	if err := m.appendOperationLocked(record); err != nil {
		m.mu.Unlock()
		return contract.Operation{}, ErrRuntimeStateUnavailable
	}
	handler := m.handlers[handlerKey(request.Capability, request.Action)]
	if handler == nil {
		record = m.ops[request.OperationID]
		record.Operation.Status = contract.OperationFailed
		record.Operation.Result = errorResult(ErrCapabilityUnavailable.Code)
		if err := m.appendOperationLocked(record); err != nil {
			m.mu.Unlock()
			return contract.Operation{}, ErrRuntimeStateUnavailable
		}
		m.mu.Unlock()
		return cloneOperation(record.Operation), ErrCapabilityUnavailable
	}
	record = m.ops[request.OperationID]
	record.Operation.Status = contract.OperationRunning
	if err := m.appendOperationLocked(record); err != nil {
		m.mu.Unlock()
		return contract.Operation{}, ErrRuntimeStateUnavailable
	}
	workerContext, cancel := context.WithCancel(context.Background())
	active := activeOperation{cancel: cancel, done: make(chan struct{})}
	m.active[request.OperationID] = active
	m.wg.Add(2)
	response := cloneOperation(record.Operation)
	m.mu.Unlock()
	go m.runOperation(workerContext, request.OperationID, handler, request, active)
	go m.watchLeaseExpiry(request.OperationID, lease.ExpiresAt, handler, active)
	return response, nil
}

func (m *Manager) QueryOperation(protocolVersion, operationID string, request contract.OperationQuery) (contract.Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.readyLocked(); err != nil {
		return contract.Operation{}, err
	}
	if err := m.checkProtocol(protocolVersion); err != nil {
		return contract.Operation{}, err
	}
	if !validIdentifier(operationID, 'o') || !validIdentifier(request.TaskID, 'q') || request.LeaseVersion <= 0 || !validFenceShape(request.HostFence) {
		return contract.Operation{}, ErrInvalidRequest
	}
	if !m.matchesCurrentFenceLocked(request.HostFence) {
		return contract.Operation{}, ErrFenceRejected
	}
	operation, exists := m.ops[operationID]
	if !exists {
		return contract.Operation{}, ErrOperationNotFound
	}
	// Query fences authorize the current runtime instance. The operation keeps
	// its original fences so callers can reconcile across generation changes.
	if request.TaskID == "" || request.TaskID != operation.Operation.TaskID || request.LeaseVersion != operation.Operation.LeaseVersion {
		return contract.Operation{}, ErrFenceRejected
	}
	return cloneOperation(operation.Operation), nil
}

func (m *Manager) CancelOperation(ctx context.Context, protocolVersion, operationID string, request contract.CancelOperationRequest) (contract.Operation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	if err := m.readyLocked(); err != nil {
		m.mu.Unlock()
		return contract.Operation{}, err
	}
	if err := m.checkProtocol(protocolVersion); err != nil {
		m.mu.Unlock()
		return contract.Operation{}, err
	}
	if !validIdentifier(operationID, 'o') || !validFenceShape(request.HostFence) {
		m.mu.Unlock()
		return contract.Operation{}, ErrInvalidRequest
	}
	if !m.matchesCurrentFenceLocked(request.HostFence) {
		m.mu.Unlock()
		return contract.Operation{}, ErrFenceRejected
	}
	operation, exists := m.ops[operationID]
	if !exists {
		m.mu.Unlock()
		return contract.Operation{}, ErrOperationNotFound
	}
	if isTerminal(operation.Operation.Status) {
		m.mu.Unlock()
		return cloneOperation(operation.Operation), nil
	}
	if operation.Operation.Status == contract.OperationAccepted {
		operation.Operation.Status = contract.OperationCancelled
		if err := m.appendOperationLocked(operation); err != nil {
			m.mu.Unlock()
			return contract.Operation{}, ErrRuntimeStateUnavailable
		}
		m.mu.Unlock()
		return cloneOperation(operation.Operation), nil
	}
	active, hasActive := m.active[operationID]
	if hasActive && !active.started {
		active.cancel()
		operation.Operation.Status = contract.OperationCancelled
		if err := m.appendOperationLocked(operation); err != nil {
			m.mu.Unlock()
			return contract.Operation{}, ErrRuntimeStateUnavailable
		}
		m.mu.Unlock()
		<-active.done
		return cloneOperation(operation.Operation), nil
	}
	operation.Operation.Status = contract.OperationCancelling
	if err := m.appendOperationLocked(operation); err != nil {
		m.mu.Unlock()
		return contract.Operation{}, ErrRuntimeStateUnavailable
	}
	handler := m.handlers[handlerKey(operation.Operation.Capability, operation.Operation.Action)]
	if hasActive {
		active.cancel()
	}
	m.mu.Unlock()
	if handler == nil {
		return m.markUnknown(operationID)
	}
	stop, stopErr := handler.Stop(ctx, cloneOperation(operation.Operation))
	if stopErr != nil || !stop.VerifiedStopped {
		return m.markUnknown(operationID)
	}
	if hasActive {
		select {
		case <-active.done:
		case <-ctx.Done():
			return m.markUnknown(operationID)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.readyLocked(); err != nil {
		return contract.Operation{}, err
	}
	operation = m.ops[operationID]
	if operation.Operation.Status != contract.OperationCancelling {
		return cloneOperation(operation.Operation), nil
	}
	operation.Operation.Status = contract.OperationCancelled
	if err := m.appendOperationLocked(operation); err != nil {
		return contract.Operation{}, ErrRuntimeStateUnavailable
	}
	return cloneOperation(operation.Operation), nil
}

func (m *Manager) Quiesce(ctx context.Context, request contract.QuiesceRequest) (contract.QuiesceResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	failed := contract.QuiesceResponse{ProtocolVersion: m.protocol, Status: contract.QuiesceFailed}
	m.mu.Lock()
	if err := m.readyLocked(); err != nil {
		m.mu.Unlock()
		return failed, err
	}
	if err := m.checkProtocol(request.ProtocolVersion); err != nil {
		m.mu.Unlock()
		return failed, err
	}
	if !validFenceShape(request.HostFence) || request.TargetControlVersion < 0 || request.TaskIDs == nil {
		m.mu.Unlock()
		return failed, ErrInvalidRequest
	}
	if !m.matchesCurrentFenceLocked(request.HostFence) || m.state.Mode != contract.ModeBlocked || request.TargetControlVersion <= m.state.ControlVersion {
		m.mu.Unlock()
		return failed, ErrFenceRejected
	}
	tasks := make(map[string]struct{}, len(request.TaskIDs))
	for _, taskID := range request.TaskIDs {
		if !validIdentifier(taskID, 'q') {
			m.mu.Unlock()
			return failed, ErrInvalidRequest
		}
		if _, duplicate := tasks[taskID]; duplicate {
			m.mu.Unlock()
			return failed, ErrInvalidRequest
		}
		tasks[taskID] = struct{}{}
	}
	ids := m.pendingIDsLocked()
	sort.Strings(ids)
	fence := request.HostFence
	m.mu.Unlock()

	pending := make([]string, 0)
	for _, id := range ids {
		m.mu.Lock()
		op := m.ops[id]
		m.mu.Unlock()
		if _, included := tasks[op.Operation.TaskID]; !included {
			pending = append(pending, id)
			continue
		}
		_, _ = m.CancelOperation(ctx, request.ProtocolVersion, id, contract.CancelOperationRequest{ProtocolVersion: request.ProtocolVersion, HostFence: fence})
		m.mu.Lock()
		stillPending := isPending(m.ops[id].Operation.Status)
		m.mu.Unlock()
		if stillPending {
			pending = append(pending, id)
		}
	}
	if len(pending) > 0 {
		sort.Strings(pending)
		return contract.QuiesceResponse{ProtocolVersion: m.protocol, Status: contract.QuiesceDraining, PendingOperationIDs: pending}, nil
	}
	if ctx.Err() != nil {
		return contract.QuiesceResponse{ProtocolVersion: m.protocol, Status: contract.QuiesceDraining, PendingOperationIDs: []string{}}, nil
	}
	receiptID, err := randomID("dr_")
	if err != nil {
		return failed, ErrRuntimeStateUnavailable
	}
	sortedTasks := append([]string(nil), request.TaskIDs...)
	sort.Strings(sortedTasks)
	receipt := contract.DrainedReceipt{
		ReceiptID: receiptID, HostID: fence.HostID, RuntimeGeneration: fence.RuntimeGeneration,
		RuntimeInstanceID: fence.RuntimeInstanceID, BlockedControlVersion: fence.ControlVersion,
		TargetControlVersion: request.TargetControlVersion, TaskIDs: sortedTasks, IssuedAt: m.now().UTC(),
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.readyLocked(); err != nil {
		return failed, err
	}
	if !m.matchesCurrentFenceLocked(fence) || m.state.Mode != contract.ModeBlocked || m.hasUnresolvedLocked() {
		return contract.QuiesceResponse{ProtocolVersion: m.protocol, Status: contract.QuiesceDraining, PendingOperationIDs: m.pendingIDsLocked()}, nil
	}
	m.state.Receipt = &receiptState{Receipt: receipt}
	if err := m.appendStateLocked(); err != nil {
		return failed, ErrRuntimeStateUnavailable
	}
	return contract.QuiesceResponse{ProtocolVersion: m.protocol, Status: contract.QuiesceDrained, PendingOperationIDs: []string{}, DrainedReceipt: &receipt}, nil
}

func (m *Manager) Ready() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.closed && m.poison == nil && m.store != nil && m.store.Ready()
}

// Close stops every unresolved operation before releasing the process lock.
// If any handler cannot prove that work has stopped, the journal remains open
// and Close returns ErrOperationsPending.
func (m *Manager) Close() error {
	m.closeMu.Lock()
	defer m.closeMu.Unlock()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	pending := m.pendingIDsLocked()
	fence := contract.HostFence{
		HostID: m.state.HostID, RuntimeGeneration: m.state.Generation,
		RuntimeInstanceID: m.state.InstanceID, ControlVersion: m.state.ControlVersion,
	}
	m.mu.Unlock()

	if len(pending) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, id := range pending {
			_, _ = m.CancelOperation(ctx, m.protocol, id, contract.CancelOperationRequest{
				ProtocolVersion: m.protocol, HostFence: fence,
			})
			if ctx.Err() != nil {
				break
			}
		}
	}

	m.mu.Lock()
	if m.hasUnresolvedLocked() {
		for _, active := range m.active {
			active.cancel()
		}
		m.mu.Unlock()
		return ErrOperationsPending
	}
	m.closed = true
	m.mu.Unlock()
	m.wg.Wait()
	return m.closeStores()
}

func (m *Manager) closeStores() error {
	var err error
	if m.store != nil {
		err = errors.Join(err, m.store.Close())
	}
	if m.legacyStore != nil {
		err = errors.Join(err, m.legacyStore.Close())
	}
	return err
}

func (m *Manager) runOperation(ctx context.Context, id string, handler ActionHandler, request contract.ExecuteRequest, active activeOperation) {
	defer m.wg.Done()
	defer active.cancel()
	defer close(active.done)
	m.mu.Lock()
	operation, exists := m.ops[id]
	if !exists || operation.Operation.Status != contract.OperationRunning || m.closed || m.poison != nil || !m.store.Ready() {
		delete(m.active, id)
		m.mu.Unlock()
		return
	}
	active.started = true
	m.active[id] = active
	m.mu.Unlock()
	result, err := handler.Execute(ctx, request)
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.active, id)
	if m.closed || m.poison != nil || !m.store.Ready() {
		return
	}
	operation, exists = m.ops[id]
	if !exists || operation.Operation.Status != contract.OperationRunning {
		return
	}
	if ctx.Err() != nil {
		// Cancellation without a matching Stop proof is not a terminal failure:
		// the side-effect result and process state are unknown.
		operation.Operation.Status = contract.OperationUnknown
		operation.Operation.Result = nil
	} else if errors.Is(err, ErrOperationUncertain) {
		operation.Operation.Status = contract.OperationUnknown
		operation.Operation.Result = nil
	} else if err != nil {
		operation.Operation.Status = contract.OperationFailed
		operation.Operation.Result = errorResult(operationFailureCode(err))
	} else if !isJSONObject(result) || len(result) > MaxOperationResultBytes || request.Capability == "file.read" && fileReadBytes(result) > MaxFileReadResultBytes {
		operation.Operation.Status = contract.OperationFailed
		operation.Operation.Result = errorResult(ErrResultTooLarge.Code)
	} else {
		operation.Operation.Status = contract.OperationSucceeded
		operation.Operation.Result = append(json.RawMessage(nil), result...)
	}
	if err := m.appendOperationLocked(operation); err != nil {
		m.poison = err
	}
}

func operationFailureCode(err error) string {
	if errors.Is(err, ErrDesktopFocusChanged) {
		return ErrDesktopFocusChanged.Code
	}
	if errors.Is(err, ErrDesktopGeometryChanged) {
		return ErrDesktopGeometryChanged.Code
	}
	return "operation_failed"
}

func (m *Manager) watchLeaseExpiry(id string, expiresAt time.Time, handler ActionHandler, active activeOperation) {
	defer m.wg.Done()
	var operation operationRecord
	var exists bool
	for {
		timer := time.NewTimer(time.Until(expiresAt))
		select {
		case <-active.done:
			timer.Stop()
			return
		case <-timer.C:
		}

		m.mu.Lock()
		if err := m.readyLocked(); err != nil {
			m.mu.Unlock()
			return
		}
		operation, exists = m.ops[id]
		if !exists || operation.Operation.Status != contract.OperationRunning && operation.Operation.Status != contract.OperationUnknown {
			m.mu.Unlock()
			return
		}
		if operation.Operation.Status == contract.OperationRunning {
			lease, hasLease := m.state.Leases[operation.Operation.TaskID]
			operationFence := contract.HostFence{
				HostID: operation.Operation.HostID, RuntimeGeneration: operation.Operation.RuntimeGeneration,
				RuntimeInstanceID: operation.Operation.RuntimeInstanceID, ControlVersion: operation.Operation.ControlVersion,
			}
			if hasLease && lease.Version == operation.Operation.LeaseVersion && sameFence(lease.Fence, operationFence) && lease.ExpiresAt.After(m.now()) {
				expiresAt = lease.ExpiresAt
				m.mu.Unlock()
				continue
			}
			operation.Operation.Status = contract.OperationCancelling
			if err := m.appendOperationLocked(operation); err != nil {
				m.mu.Unlock()
				return
			}
		}
		active.cancel()
		m.mu.Unlock()
		break
	}

	stopContext, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	stop, err := handler.Stop(stopContext, cloneOperation(operation.Operation))
	if err != nil || !stop.VerifiedStopped {
		_, _ = m.markUnknown(id)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readyLocked() != nil {
		return
	}
	operation, exists = m.ops[id]
	if !exists || operation.Operation.Status != contract.OperationCancelling && operation.Operation.Status != contract.OperationUnknown {
		return
	}
	operation.Operation.Status = contract.OperationCancelled
	if err := m.appendOperationLocked(operation); err != nil {
		m.poison = err
	}
}

func (m *Manager) markUnknown(id string) (contract.Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.readyLocked(); err != nil {
		return contract.Operation{}, err
	}
	operation, exists := m.ops[id]
	if !exists {
		return contract.Operation{}, ErrOperationNotFound
	}
	if operation.Operation.Status != contract.OperationUnknown {
		operation.Operation.Status = contract.OperationUnknown
		if err := m.appendOperationLocked(operation); err != nil {
			return contract.Operation{}, ErrRuntimeStateUnavailable
		}
	}
	return cloneOperation(operation.Operation), nil
}

func (m *Manager) validateLeaseLocked(taskID string, version int64, fence contract.HostFence) error {
	lease, exists := m.state.Leases[taskID]
	if !exists || lease.Version != version || !sameFence(lease.Fence, fence) || !lease.ExpiresAt.After(m.now()) {
		return ErrFenceRejected
	}
	return nil
}

func (m *Manager) matchesRuntimeLocked(fence contract.HostFence) bool {
	return fence.HostID == m.state.HostID && fence.RuntimeGeneration == m.state.Generation && fence.RuntimeInstanceID == m.state.InstanceID
}

func (m *Manager) matchesCurrentFenceLocked(fence contract.HostFence) bool {
	return m.matchesRuntimeLocked(fence) && fence.ControlVersion == m.state.ControlVersion
}

func (m *Manager) hasUnresolvedLocked() bool {
	for _, operation := range m.ops {
		if isPending(operation.Operation.Status) {
			return true
		}
	}
	return false
}

func (m *Manager) pendingIDsLocked() []string {
	ids := make([]string, 0)
	for id, operation := range m.ops {
		if isPending(operation.Operation.Status) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (m *Manager) receiptMatchesLocked(receipt contract.DrainedReceipt, targetVersion int64) bool {
	if m.state.Receipt == nil || m.state.Receipt.Consumed {
		return false
	}
	stored := m.state.Receipt.Receipt
	return sameReceipt(receipt, stored) && receipt.HostID == m.state.HostID && receipt.RuntimeGeneration == m.state.Generation && receipt.RuntimeInstanceID == m.state.InstanceID && receipt.BlockedControlVersion == m.state.ControlVersion && receipt.TargetControlVersion == targetVersion && stored.TargetControlVersion == targetVersion
}

func (m *Manager) appendStateLocked() error {
	if err := m.store.Append("runtime_state", m.state); err != nil {
		m.poison = err
		return err
	}
	m.stateSet = true
	return nil
}

func (m *Manager) appendOperationLocked(operation operationRecord) error {
	if previous, exists := m.ops[operation.Operation.OperationID]; exists && operation.Request == nil {
		operation.Request = previous.Request
	}
	if err := m.store.Append("operation", operation); err != nil {
		m.poison = err
		return err
	}
	m.ops[operation.Operation.OperationID] = operation
	return nil
}

func (m *Manager) setOperationLocked(id string, status contract.OperationStatus, result json.RawMessage) error {
	operation, exists := m.ops[id]
	if !exists {
		return ErrOperationNotFound
	}
	operation.Operation.Status = status
	operation.Operation.Result = append(json.RawMessage(nil), result...)
	return m.appendOperationLocked(operation)
}

func (m *Manager) applyEvent(event operationstore.Event) error {
	switch event.Kind {
	case "runtime_state":
		var state runtimeState
		if err := decodeStrict(event.Data, &state); err != nil {
			return err
		}
		if state.HostID == "" || state.Generation <= 0 || state.InstanceID == "" || state.ControlVersion < 0 || (state.Mode != contract.ModeBlocked && state.Mode != contract.ModeAI && state.Mode != contract.ModeHuman) {
			return errors.New("invalid persisted runtime state")
		}
		if m.stateSet && (state.HostID != m.state.HostID || state.Generation < m.state.Generation || state.ControlVersion < m.state.ControlVersion) {
			return errors.New("runtime state fence regressed")
		}
		if state.Leases == nil {
			state.Leases = make(map[string]taskLease)
		}
		m.state = state
		m.stateSet = true
	case "operation":
		var operation operationRecord
		if err := decodeStrict(event.Data, &operation); err != nil {
			return err
		}
		if operation.Operation.OperationID == "" || operation.RequestHash == "" || !validStatus(operation.Operation.Status) {
			return errors.New("invalid persisted operation")
		}
		previous, exists := m.ops[operation.Operation.OperationID]
		if !exists {
			if operation.Operation.Status != contract.OperationAccepted {
				return errors.New("operation journal must start accepted")
			}
		} else {
			if previous.RequestHash != operation.RequestHash || !validTransition(previous.Operation.Status, operation.Operation.Status) {
				return errors.New("invalid persisted operation transition")
			}
			if operation.Request == nil {
				operation.Request = previous.Request
			}
		}
		if operation.Operation.HostID == "" || operation.Operation.RuntimeGeneration <= 0 || operation.Operation.TaskID == "" {
			return errors.New("invalid persisted operation fences")
		}
		m.ops[operation.Operation.OperationID] = operation
	default:
		return fmt.Errorf("unknown journal event kind %q", event.Kind)
	}
	return nil
}

func normalizedRequest(request contract.ExecuteRequest) (json.RawMessage, string, error) {
	if len(request.Arguments) == 0 || len(request.Arguments) > MaxRequestBytes || !isJSONObject(request.Arguments) {
		return nil, "", ErrInvalidRequest
	}
	decoder := json.NewDecoder(bytes.NewReader(request.Arguments))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, "", err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, "", err
	}
	arguments, err := json.Marshal(value)
	if err != nil {
		return nil, "", err
	}
	stable := struct {
		ProtocolVersion    string          `json:"protocol_version"`
		HostID             string          `json:"host_id"`
		Generation         int64           `json:"runtime_generation"`
		InstanceID         string          `json:"runtime_instance_id"`
		ControlVersion     int64           `json:"control_version"`
		TaskID             string          `json:"task_id"`
		WorkspaceID        string          `json:"workspace_id,omitempty"`
		BindingVersion     int64           `json:"binding_version,omitempty"`
		WorkspaceViewID    string          `json:"workspace_view_id,omitempty"`
		CheckpointRevision int64           `json:"workspace_checkpoint_revision,omitempty"`
		OperationID        string          `json:"operation_id"`
		LeaseVersion       int64           `json:"lease_version"`
		Capability         string          `json:"capability"`
		Action             string          `json:"action"`
		Arguments          json.RawMessage `json:"arguments"`
	}{
		ProtocolVersion: request.ProtocolVersion, HostID: request.HostID,
		Generation: request.RuntimeGeneration, InstanceID: request.RuntimeInstanceID,
		ControlVersion: request.ControlVersion, TaskID: request.TaskID,
		WorkspaceID: request.WorkspaceID, BindingVersion: request.BindingVersion,
		WorkspaceViewID: request.WorkspaceViewID, CheckpointRevision: request.WorkspaceCheckpointRevision,
		OperationID: request.OperationID, LeaseVersion: request.LeaseVersion,
		Capability: request.Capability, Action: request.Action, Arguments: arguments,
	}
	encoded, err := json.Marshal(stable)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(encoded)
	return arguments, hex.EncodeToString(digest[:]), nil
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return ensureJSONEOF(decoder)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func controlResponse(protocol string, state runtimeState) contract.SetControlResponse {
	return contract.SetControlResponse{
		ProtocolVersion: protocol,
		HostFence:       contract.HostFence{HostID: state.HostID, RuntimeGeneration: state.Generation, RuntimeInstanceID: state.InstanceID, ControlVersion: state.ControlVersion},
		Mode:            state.Mode,
	}
}

func leaseResponse(protocol, taskID string, lease taskLease) contract.SetTaskLeaseResponse {
	return contract.SetTaskLeaseResponse{ProtocolVersion: protocol, HostFence: lease.Fence, TaskID: taskID, LeaseVersion: lease.Version, ExpiresAt: lease.ExpiresAt}
}

func sameFence(left, right contract.HostFence) bool {
	return left.HostID == right.HostID && left.RuntimeGeneration == right.RuntimeGeneration && left.RuntimeInstanceID == right.RuntimeInstanceID && left.ControlVersion == right.ControlVersion
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	a := append([]string(nil), left...)
	b := append([]string(nil), right...)
	sort.Strings(a)
	sort.Strings(b)
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func sameReceipt(left, right contract.DrainedReceipt) bool {
	return left.ReceiptID != "" && left.ReceiptID == right.ReceiptID && left.HostID == right.HostID &&
		left.RuntimeGeneration == right.RuntimeGeneration && left.RuntimeInstanceID == right.RuntimeInstanceID &&
		left.BlockedControlVersion == right.BlockedControlVersion && left.TargetControlVersion == right.TargetControlVersion &&
		left.IssuedAt.Equal(right.IssuedAt) && sameStrings(left.TaskIDs, right.TaskIDs)
}

func validIdentifier(value string, prefix byte) bool {
	if len(value) != 8 || value[0] != prefix {
		return false
	}
	for index := 1; index < len(value); index++ {
		if !(value[index] >= 'a' && value[index] <= 'z') && !(value[index] >= '0' && value[index] <= '9') {
			return false
		}
	}
	return true
}

func validTaskWorkspaceViewID(value string) bool {
	return len(value) == 65 && value[0] == 'v' && validLowerSHA256(value[1:])
}

func validWorkspaceBindingSnapshot(bindings []contract.WorkspaceBinding) bool {
	if bindings == nil || len(bindings) > 256 {
		return false
	}
	ids := make(map[string]struct{}, len(bindings))
	directories := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		if !validIdentifier(binding.WorkspaceID, 'w') || binding.BindingVersion < 1 || binding.Directory != binding.WorkspaceID {
			return false
		}
		if _, exists := ids[binding.WorkspaceID]; exists {
			return false
		}
		if _, exists := directories[binding.Directory]; exists {
			return false
		}
		ids[binding.WorkspaceID] = struct{}{}
		directories[binding.Directory] = struct{}{}
	}
	return true
}

func validFenceShape(fence contract.HostFence) bool {
	return validIdentifier(fence.HostID, 'h') && fence.RuntimeGeneration > 0 && validRuntimeInstanceID(fence.RuntimeInstanceID) && fence.ControlVersion >= 0
}

func validRuntimeInstanceID(value string) bool {
	if len(value) == 0 || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char > 0x7e {
			return false
		}
	}
	return true
}

func isUTC(value time.Time) bool {
	_, offset := value.Zone()
	return !value.IsZero() && offset == 0
}

func isTerminal(status contract.OperationStatus) bool {
	return status == contract.OperationSucceeded || status == contract.OperationFailed || status == contract.OperationCancelled
}

func isPending(status contract.OperationStatus) bool {
	return status == contract.OperationAccepted || status == contract.OperationRunning || status == contract.OperationCancelling || status == contract.OperationUnknown
}

func validStatus(status contract.OperationStatus) bool {
	switch status {
	case contract.OperationAccepted, contract.OperationRunning, contract.OperationSucceeded, contract.OperationFailed, contract.OperationCancelling, contract.OperationCancelled, contract.OperationUnknown:
		return true
	default:
		return false
	}
}

func validTransition(from, to contract.OperationStatus) bool {
	switch from {
	case contract.OperationAccepted:
		return to == contract.OperationRunning || to == contract.OperationFailed || to == contract.OperationCancelled
	case contract.OperationRunning:
		return to == contract.OperationSucceeded || to == contract.OperationFailed || to == contract.OperationCancelling || to == contract.OperationCancelled || to == contract.OperationUnknown
	case contract.OperationCancelling:
		return to == contract.OperationCancelled || to == contract.OperationUnknown || to == contract.OperationFailed
	case contract.OperationUnknown:
		return to == contract.OperationCancelled || to == contract.OperationFailed || to == contract.OperationSucceeded
	default:
		return false
	}
}

func handlerKey(capability, action string) string { return capability + "\x00" + action }

func randomID(prefix string) (string, error) {
	value := make([]byte, 18)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(value), nil
}

func errorResult(code string) json.RawMessage {
	value, _ := json.Marshal(struct {
		Code string `json:"code"`
	}{Code: code})
	return value
}

func cloneOperation(operation contract.Operation) contract.Operation {
	operation.Result = append(json.RawMessage(nil), operation.Result...)
	return operation
}

func cloneRequest(request *contract.ExecuteRequest) *contract.ExecuteRequest {
	if request == nil {
		return nil
	}
	copyOfRequest := *request
	copyOfRequest.Arguments = append(json.RawMessage(nil), request.Arguments...)
	return &copyOfRequest
}

func fileReadBytes(result json.RawMessage) int {
	var value struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(result, &value); err != nil {
		return MaxFileReadResultBytes + 1
	}
	return len([]byte(value.Text))
}

func isJSONObject(value []byte) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && json.Valid(trimmed)
}

func (m *Manager) readyLocked() error {
	if m.closed || m.poison != nil || m.store == nil || !m.store.Ready() {
		return ErrRuntimeStateUnavailable
	}
	return nil
}

func (m *Manager) checkProtocol(version string) error {
	if version != m.protocol {
		return ErrProtocolIncompatible
	}
	return nil
}
