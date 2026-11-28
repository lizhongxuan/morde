package runtimecore

import (
	"sync"
	"time"

	"morde-runtime/contract"
)

const (
	maxSupplementalOutputCaptures        = 256
	maxSupplementalOutputCaptureBytes    = contract.SupplementalOutputCaptureMaxBytes
	maxSupplementalOutputCaptureAllBytes = 64 << 20
	supplementalOutputCaptureTTL         = 10 * time.Minute
)

type supplementalOutputCaptureStore struct {
	mu          sync.Mutex
	items       map[string]*supplementalOutputCapture
	retained    int
	maxRetained int
	now         func() time.Time
}

type supplementalOutputCapture struct {
	store    *supplementalOutputCaptureStore
	id       string
	identity contract.SupplementalOutputCaptureIdentity
	state    contract.SupplementalOutputCaptureState
	reason   string
	stdout   []byte
	stderr   []byte
	created  time.Time
	expires  time.Time
	timer    *time.Timer
}

func newSupplementalOutputCaptureStore(now func() time.Time) *supplementalOutputCaptureStore {
	if now == nil {
		now = time.Now
	}
	return &supplementalOutputCaptureStore{items: make(map[string]*supplementalOutputCapture), maxRetained: maxSupplementalOutputCaptureAllBytes, now: now}
}

func (s *supplementalOutputCaptureStore) begin(id string, identity contract.SupplementalOutputCaptureIdentity) *supplementalOutputCapture {
	if s == nil || id == "" || !contract.ValidSupplementalOutputCaptureIdentity(identity) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeExpiredLocked(s.now())
	if len(s.items) >= maxSupplementalOutputCaptures {
		return nil
	}
	value := &supplementalOutputCapture{store: s, id: id, identity: identity,
		state: contract.SupplementalOutputCaptureCollecting, created: s.now(), expires: s.now().Add(supplementalOutputCaptureTTL)}
	s.items[id] = value
	value.timer = time.AfterFunc(supplementalOutputCaptureTTL, func() { s.expire(id, value) })
	return value
}

func (capture *supplementalOutputCapture) WriteSupplementalOutput(stream contract.SupplementalOutputCaptureStream, value []byte) bool {
	if capture == nil || len(value) == 0 || !contract.ValidSupplementalOutputCaptureStream(stream) {
		return len(value) == 0
	}
	s := capture.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items[capture.id] != capture || capture.state != contract.SupplementalOutputCaptureCollecting {
		return false
	}
	current := len(capture.stdout) + len(capture.stderr)
	if len(value) > maxSupplementalOutputCaptureBytes-current || len(value) > s.maxRetained-s.retained {
		capture.state = contract.SupplementalOutputCaptureIncomplete
		capture.reason = "capacity_exceeded"
		return false
	}
	if stream == contract.SupplementalOutputCaptureStdout {
		capture.stdout = append(capture.stdout, value...)
	} else {
		capture.stderr = append(capture.stderr, value...)
	}
	s.retained += len(value)
	return true
}

func (capture *supplementalOutputCapture) MarkSupplementalOutputIncomplete(reason string) {
	capture.markIncomplete(reason)
}

func (capture *supplementalOutputCapture) MarkSupplementalOutputUnsupported(reason string) {
	capture.markIncomplete(reason)
}

func (capture *supplementalOutputCapture) markIncomplete(reason string) {
	if capture == nil || capture.store == nil {
		return
	}
	if reason == "" || len(reason) > 64 {
		reason = "capture_interrupted"
	}
	s := capture.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items[capture.id] == capture && capture.state == contract.SupplementalOutputCaptureCollecting {
		capture.state = contract.SupplementalOutputCaptureIncomplete
		capture.reason = reason
	}
}

func (s *supplementalOutputCaptureStore) finish(id string, complete bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	capture := s.items[id]
	if capture == nil || capture.state != contract.SupplementalOutputCaptureCollecting {
		return
	}
	if complete {
		capture.state = contract.SupplementalOutputCaptureComplete
		return
	}
	capture.state = contract.SupplementalOutputCaptureFailed
	capture.reason = "operation_incomplete"
}

func (s *supplementalOutputCaptureStore) page(id string, identity contract.SupplementalOutputCaptureIdentity,
	stream contract.SupplementalOutputCaptureStream, offset int) (contract.SupplementalOutputCapturePage, bool) {
	if s == nil || !contract.ValidSupplementalOutputCaptureStream(stream) || offset < 0 {
		return contract.SupplementalOutputCapturePage{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeExpiredLocked(s.now())
	capture := s.items[id]
	if capture == nil || capture.identity != identity {
		return contract.SupplementalOutputCapturePage{}, false
	}
	data := capture.stdout
	if stream == contract.SupplementalOutputCaptureStderr {
		data = capture.stderr
	}
	if offset > len(data) {
		return contract.SupplementalOutputCapturePage{}, false
	}
	end := offset + contract.SupplementalOutputCapturePageBytes
	if end > len(data) {
		end = len(data)
	}
	pageData := append([]byte{}, data[offset:end]...)
	return contract.SupplementalOutputCapturePage{State: capture.state, Reason: capture.reason, Stream: stream,
		Offset: offset, NextOffset: end, TotalBytes: len(data), Data: pageData, EOF: end == len(data)}, true
}

func (s *supplementalOutputCaptureStore) release(id string, identity contract.SupplementalOutputCaptureIdentity) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	capture := s.items[id]
	if capture == nil {
		return true
	}
	if capture.identity != identity {
		return false
	}
	s.deleteLocked(id, capture)
	return true
}

func (s *supplementalOutputCaptureStore) deleteTask(taskID string) {
	if s == nil || taskID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, capture := range s.items {
		if capture.identity.TaskID == taskID {
			s.deleteLocked(id, capture)
		}
	}
}

func (s *supplementalOutputCaptureStore) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, capture := range s.items {
		s.deleteLocked(id, capture)
	}
}

func (s *supplementalOutputCaptureStore) removeExpiredLocked(now time.Time) {
	for id, capture := range s.items {
		if !capture.expires.After(now) {
			s.deleteLocked(id, capture)
		}
	}
}

func (s *supplementalOutputCaptureStore) expire(id string, expected *supplementalOutputCapture) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items[id] == expected {
		s.deleteLocked(id, expected)
	}
}

func (s *supplementalOutputCaptureStore) deleteLocked(id string, capture *supplementalOutputCapture) {
	delete(s.items, id)
	if capture.timer != nil {
		capture.timer.Stop()
		capture.timer = nil
	}
	s.retained -= len(capture.stdout) + len(capture.stderr)
	if s.retained < 0 {
		s.retained = 0
	}
	clear(capture.stdout)
	clear(capture.stderr)
	capture.stdout, capture.stderr = nil, nil
}

func (m *Manager) SupplementalOutputCapturePage(request contract.SupplementalOutputCapturePageRequest) (contract.SupplementalOutputCapturePage, error) {
	if request.ProtocolVersion != m.protocol || !validFenceShape(request.HostFence) || !validIdentifier(request.TaskID, 'q') ||
		!validIdentifier(request.OperationID, 'o') || request.LeaseVersion < 1 || !contract.ValidSupplementalOutputCaptureIdentity(request.Identity) ||
		request.Identity.TaskID != request.TaskID || !contract.ValidSupplementalOutputCaptureStream(request.Stream) || request.Offset < 0 {
		return contract.SupplementalOutputCapturePage{}, ErrInvalidRequest
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.readyLocked(); err != nil {
		return contract.SupplementalOutputCapturePage{}, err
	}
	if !m.matchesCurrentFenceLocked(request.HostFence) {
		return contract.SupplementalOutputCapturePage{}, ErrFenceRejected
	}
	if err := m.validateLeaseLocked(request.TaskID, request.LeaseVersion, request.HostFence); err != nil {
		return contract.SupplementalOutputCapturePage{}, err
	}
	operation, exists := m.ops[request.OperationID]
	if !exists || !operationMatchesOutputCapture(operation, request.HostFence, request.TaskID, request.LeaseVersion, request.Identity) {
		return contract.SupplementalOutputCapturePage{}, ErrFenceRejected
	}
	if !isTerminal(operation.Operation.Status) {
		return contract.SupplementalOutputCapturePage{}, ErrOperationsPending
	}
	if operation.Operation.Status != contract.OperationSucceeded {
		return failedSupplementalOutputPage(request), nil
	}
	page, ok := m.outputCaptures.page(request.OperationID, request.Identity, request.Stream, request.Offset)
	if !ok {
		return failedSupplementalOutputPage(request), nil
	}
	page.ProtocolVersion, page.HostFence, page.TaskID = m.protocol, request.HostFence, request.TaskID
	page.LeaseVersion, page.OperationID, page.Identity = request.LeaseVersion, request.OperationID, request.Identity
	return page, nil
}

func (m *Manager) ReleaseSupplementalOutputCapture(operationID string, request contract.SupplementalOutputCaptureReleaseRequest) (contract.SupplementalOutputCaptureReleaseResponse, error) {
	if request.ProtocolVersion != m.protocol || !validFenceShape(request.HostFence) || !validIdentifier(request.TaskID, 'q') ||
		!validIdentifier(operationID, 'o') || request.LeaseVersion < 1 || !contract.ValidSupplementalOutputCaptureIdentity(request.Identity) ||
		request.Identity.TaskID != request.TaskID {
		return contract.SupplementalOutputCaptureReleaseResponse{}, ErrInvalidRequest
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.readyLocked(); err != nil {
		return contract.SupplementalOutputCaptureReleaseResponse{}, err
	}
	if !m.matchesCurrentFenceLocked(request.HostFence) {
		return contract.SupplementalOutputCaptureReleaseResponse{}, ErrFenceRejected
	}
	if err := m.validateLeaseLocked(request.TaskID, request.LeaseVersion, request.HostFence); err != nil {
		return contract.SupplementalOutputCaptureReleaseResponse{}, err
	}
	operation, exists := m.ops[operationID]
	if !exists || !operationMatchesOutputCapture(operation, request.HostFence, request.TaskID, request.LeaseVersion, request.Identity) || !isTerminal(operation.Operation.Status) {
		return contract.SupplementalOutputCaptureReleaseResponse{}, ErrFenceRejected
	}
	if !m.outputCaptures.release(operationID, request.Identity) {
		return contract.SupplementalOutputCaptureReleaseResponse{}, ErrFenceRejected
	}
	return contract.SupplementalOutputCaptureReleaseResponse{ProtocolVersion: m.protocol, HostFence: request.HostFence,
		TaskID: request.TaskID, LeaseVersion: request.LeaseVersion, OperationID: operationID, Released: true}, nil
}

func operationMatchesOutputCapture(operation operationRecord, fence contract.HostFence, taskID string, leaseVersion int64,
	identity contract.SupplementalOutputCaptureIdentity) bool {
	request := operation.Request
	return request != nil && request.OutputCapture != nil && *request.OutputCapture == identity &&
		operation.Operation.HostID == fence.HostID && operation.Operation.RuntimeGeneration == fence.RuntimeGeneration &&
		operation.Operation.RuntimeInstanceID == fence.RuntimeInstanceID && operation.Operation.ControlVersion == fence.ControlVersion &&
		operation.Operation.TaskID == taskID && operation.Operation.LeaseVersion == leaseVersion &&
		operation.Operation.Capability == "shell.exec" && operation.Operation.Action == "run"
}

func failedSupplementalOutputPage(request contract.SupplementalOutputCapturePageRequest) contract.SupplementalOutputCapturePage {
	return contract.SupplementalOutputCapturePage{ProtocolVersion: request.ProtocolVersion, HostFence: request.HostFence,
		TaskID: request.TaskID, LeaseVersion: request.LeaseVersion, OperationID: request.OperationID, Identity: request.Identity,
		State: contract.SupplementalOutputCaptureFailed, Reason: "capture_unavailable", Stream: request.Stream,
		Offset: request.Offset, NextOffset: request.Offset, Data: []byte{}, EOF: true}
}

var _ contract.SupplementalOutputSink = (*supplementalOutputCapture)(nil)
