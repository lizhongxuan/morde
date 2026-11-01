package workspacereplica

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"morde-runtime/contract"
	mordesync "morde-sync/client"
)

type replicaTestManager struct {
	capabilities contract.CapabilitiesResponse
	snapshots    []contract.SetWorkspaceBindingsRequest
	deletions    []contract.DeleteTaskWorkspaceViewRequest
	deleteErr    error
}

func (m *replicaTestManager) Capabilities(contract.CapabilitiesRequest) (contract.CapabilitiesResponse, error) {
	return m.capabilities, nil
}

func (m *replicaTestManager) SetWorkspaceBindings(_ context.Context, request contract.SetWorkspaceBindingsRequest) (contract.SetWorkspaceBindingsResponse, error) {
	m.snapshots = append(m.snapshots, request)
	return contract.SetWorkspaceBindingsResponse{}, nil
}

func (m *replicaTestManager) DeleteTaskWorkspaceView(_ context.Context, taskID string, request contract.DeleteTaskWorkspaceViewRequest) (contract.DeleteTaskWorkspaceViewResponse, error) {
	m.deletions = append(m.deletions, request)
	if m.deleteErr != nil {
		return contract.DeleteTaskWorkspaceViewResponse{}, m.deleteErr
	}
	return contract.DeleteTaskWorkspaceViewResponse{
		ProtocolVersion: contract.ProtocolVersion, HostFence: request.HostFence,
		TaskID: taskID, RequestID: request.RequestID, Deleted: true,
	}, nil
}

func TestReconcileFailsClosedWhenActiveWorkspaceHasNoLiveBaseline(t *testing.T) {
	const hostID = "h1234567"
	const workspaceID = "w1234567"
	syncRequested := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/host-replicas/"+hostID+"/generations/2/workspace-bindings" {
			if r.Header.Get("Authorization") != "Bearer bootstrap" {
				t.Errorf("bootstrap authorization = %q", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(bindingSnapshot{RuntimeGeneration: 2, Items: []replicaItem{{
				WorkspaceID: workspaceID, SyncCredential: "scoped", Directory: workspaceID, State: "active",
				SyncState:      "healthy",
				BindingVersion: 3, DesiredRevision: 8, AppliedRevision: 8,
			}}})
			return
		}
		if r.URL.Path == "/api/host-replicas/"+hostID+"/generations/2/task-workspace-view-deletions" {
			_ = json.NewEncoder(w).Encode(contract.TaskWorkspaceViewDeletionSnapshot{
				RuntimeGeneration: 2, Items: []contract.PendingTaskWorkspaceViewDeletion{},
			})
			return
		}
		syncRequested = true
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	manager := &replicaTestManager{capabilities: contract.CapabilitiesResponse{
		ProtocolVersion: contract.ProtocolVersion, HostID: hostID, RuntimeGeneration: 2,
		RuntimeInstanceID: "instance-1", ControlVersion: 1,
	}}
	controller, err := New(Options{
		HostID: hostID, Generation: 2, PortalURL: server.URL, BootstrapCredential: "bootstrap",
		WorkspaceRoot: t.TempDir(), Manager: manager,
	})
	if err != nil {
		t.Fatal(err)
	}

	workspaceDirectory := filepath.Join(controller.options.WorkspaceRoot, workspaceID)
	if err := os.MkdirAll(workspaceDirectory, 0o750); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(workspaceDirectory, "local-delete.txt")
	if err := os.WriteFile(localPath, []byte("keep local state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := controller.reconcile(context.Background()); err == nil {
		t.Fatal("reconcile succeeded without a durable live baseline")
	}
	if syncRequested {
		t.Fatal("reconcile attempted a fill-missing download without a live baseline")
	}
	if len(manager.snapshots) != 1 || len(manager.snapshots[0].Bindings) != 0 {
		t.Fatalf("unverified workspace was installed in Runtime: %#v", manager.snapshots)
	}
	if len(controller.watchers) != 0 || len(controller.known) != 0 {
		t.Fatalf("unverified workspace became active locally: watchers=%d known=%v", len(controller.watchers), controller.known)
	}
	if data, err := os.ReadFile(localPath); err != nil || string(data) != "keep local state" {
		t.Fatalf("local workspace data changed while baseline was missing: %q, %v", data, err)
	}
}

func TestReconcileDeletesTaskWorkspaceViewBeforeAcknowledgingPersistentRequest(t *testing.T) {
	const hostID = "h1234567"
	const taskID = "q1234567"
	const requestID = "z1234567"
	manager := &replicaTestManager{capabilities: contract.CapabilitiesResponse{
		ProtocolVersion: contract.ProtocolVersion, HostID: hostID, RuntimeGeneration: 2,
		RuntimeInstanceID: "instance-1", ControlVersion: 1,
	}}
	var acknowledgements atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bootstrap" {
			http.Error(w, "missing bootstrap credential", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/host-replicas/"+hostID+"/generations/2/workspace-bindings":
			_ = json.NewEncoder(w).Encode(bindingSnapshot{RuntimeGeneration: 2, Items: []replicaItem{}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/host-replicas/"+hostID+"/generations/2/task-workspace-view-deletions":
			_ = json.NewEncoder(w).Encode(contract.TaskWorkspaceViewDeletionSnapshot{
				RuntimeGeneration: 2, Items: []contract.PendingTaskWorkspaceViewDeletion{{TaskID: taskID, RequestID: requestID}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/host-replicas/"+hostID+"/generations/2/tasks/"+taskID+"/workspace-view-deletions/"+requestID+"/applied":
			if len(manager.deletions) != 1 || manager.deletions[0].TaskID != taskID || manager.deletions[0].RequestID != requestID {
				http.Error(w, "view deletion was not confirmed first", http.StatusConflict)
				return
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 0 {
				http.Error(w, "invalid empty acknowledgement", http.StatusBadRequest)
				return
			}
			acknowledgements.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	controller, err := New(Options{
		HostID: hostID, Generation: 2, PortalURL: server.URL, BootstrapCredential: "bootstrap",
		WorkspaceRoot: t.TempDir(), Manager: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile pending Task view deletion: %v", err)
	}
	if len(manager.deletions) != 1 || manager.deletions[0].HostFence.RuntimeInstanceID != "instance-1" || acknowledgements.Load() != 1 {
		t.Fatalf("delete requests=%#v acknowledgements=%d", manager.deletions, acknowledgements.Load())
	}
}

func TestLiveSyncConflictPausesOneWorkspaceAndResetResumesIt(t *testing.T) {
	const hostID = "h1234567"
	const workspaceA = "w1234567"
	const workspaceB = "w7654321"

	portal := newReplicaSyncTestPortal(hostID, workspaceA, workspaceB)
	server := httptest.NewServer(portal)
	ctx, cancel := context.WithCancel(context.Background())
	errorEvents := make(chan error, 10)
	manager := &replicaTestManager{capabilities: contract.CapabilitiesResponse{
		ProtocolVersion: contract.ProtocolVersion, HostID: hostID, RuntimeGeneration: 2,
		RuntimeInstanceID: "instance-1", ControlVersion: 1,
	}}
	controller, err := New(Options{
		HostID: hostID, Generation: 2, PortalURL: server.URL, BootstrapCredential: "bootstrap",
		WorkspaceRoot: t.TempDir(), Manager: manager, OnError: func(err error) { errorEvents <- err },
	})
	if err != nil {
		t.Fatal(err)
	}
	controller.known[workspaceA] = 1
	controller.known[workspaceB] = 1
	controller.lastFence = &contract.HostFence{
		HostID: hostID, RuntimeGeneration: 2, RuntimeInstanceID: "instance-1", ControlVersion: 1,
	}
	defer func() {
		cancel()
		controller.stopAll()
		server.Close()
	}()

	if err := controller.reconcile(ctx); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	waitSignal(t, portal.watchStarted[workspaceB], "healthy workspace watcher")
	waitStatus(t, portal.statusUpdates, workspaceA, "conflict")

	if err := controller.reconcile(ctx); err != nil {
		t.Fatalf("reconcile after reported conflict: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := controller.reconcile(ctx); err != nil {
			t.Fatalf("conflict reconcile %d: %v", i+1, err)
		}
	}
	if got := portal.sessionCount(workspaceA); got != 1 {
		t.Fatalf("conflicted workspace created %d sessions before reset, want 1", got)
	}
	if got := portal.sessionCount(workspaceB); got != 1 {
		t.Fatalf("unconflicted workspace created %d sessions, want 1", got)
	}
	if !controller.hasRunningWatcher(workspaceB) {
		t.Fatal("conflict in one workspace stopped the other workspace watcher")
	}
	if controller.hasRunningWatcher(workspaceA) {
		t.Fatal("conflicted workspace watcher is still running")
	}
	if update := portal.lastStatus(workspaceA); update.State != "conflict" || len(update.Conflicts) != 1 ||
		update.Conflicts[0] != "main.go" {
		t.Fatalf("reported conflict = %+v", update)
	}

	portal.reset(workspaceA)
	if err := controller.reconcile(ctx); err != nil {
		t.Fatalf("reconcile after explicit reset: %v", err)
	}
	waitSignal(t, portal.watchStarted[workspaceA], "reset workspace watcher")
	healthyUpdate := waitStatus(t, portal.statusUpdates, workspaceA, "healthy")
	if got := portal.sessionCount(workspaceA); got != 2 {
		t.Fatalf("workspace did not create exactly one session after reset: got %d", got)
	}
	if healthyUpdate.Conflicts == nil || len(healthyUpdate.Conflicts) != 0 {
		t.Fatalf("successful sync did not send an empty conflict list: %+v", healthyUpdate)
	}
	if update := portal.lastStatus(workspaceA); update.State != "healthy" || len(update.Conflicts) != 0 {
		t.Fatalf("successful sync did not clear conflict status: %+v", update)
	}
	select {
	case err := <-errorEvents:
		t.Fatalf("unexpected controller error: %v", err)
	default:
	}
}

func TestLiveSyncAppliedReceiptRetriesAndSurvivesRestart(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%t", restart), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			const hostID = "h1234567"
			const workspaceID = "w7654321"
			portal := newReplicaSyncTestPortal(hostID, workspaceID)
			applied := make(chan string, 4)
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/applied") && r.Method == http.MethodPost {
					var body struct {
						SessionID string `json:"session_id"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						http.Error(w, "invalid receipt", http.StatusBadRequest)
						return
					}
					applied <- body.SessionID
					if attempts.Add(1) == 1 {
						http.Error(w, "temporary failure", http.StatusServiceUnavailable)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{}`))
					return
				}
				portal.ServeHTTP(w, r)
			}))
			defer server.Close()
			root := t.TempDir()
			newController := func() *Controller {
				manager := &replicaTestManager{capabilities: contract.CapabilitiesResponse{
					ProtocolVersion: contract.ProtocolVersion, HostID: hostID, RuntimeGeneration: 2,
					RuntimeInstanceID: "instance-1", ControlVersion: 1,
				}}
				controller, err := New(Options{
					HostID: hostID, Generation: 2, PortalURL: server.URL, BootstrapCredential: "bootstrap",
					WorkspaceRoot: root, WorkspaceUID: uint32(os.Getuid()), WorkspaceGID: uint32(os.Getgid()), Manager: manager,
				})
				if err != nil {
					t.Fatal(err)
				}
				return controller
			}
			ctx, cancel := context.WithCancel(context.Background())
			controller := newController()
			controller.known[workspaceID] = 1
			if err := controller.reconcile(ctx); err != nil {
				t.Fatalf("start Host live sync: %v", err)
			}
			first := waitAppliedReceipt(t, applied)
			if restart {
				cancel()
				controller.stopAll()
				ctx, cancel = context.WithCancel(context.Background())
				controller = newController()
				controller.known[workspaceID] = 1 // Keep macOS tests off the Linux-only ownership path.
				if err := controller.reconcile(ctx); err != nil {
					t.Fatalf("restart Host live sync: %v", err)
				}
			}
			second := waitAppliedReceipt(t, applied)
			if first == "" || second != first {
				t.Fatalf("applied receipt session IDs = %q, %q", first, second)
			}
			waitStatus(t, portal.statusUpdates, workspaceID, "healthy")
			if got := portal.sessionCount(workspaceID); got != 1 {
				t.Fatalf("receipt retry created %d sync sessions, want one", got)
			}
			cancel()
			controller.stopAll()
		})
	}
}

func TestCompletedSessionWithConflictDoesNotAcknowledgeHostRevision(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()
	controller, err := New(Options{
		HostID: "h1234567", Generation: 2, PortalURL: server.URL, BootstrapCredential: "bootstrap",
		WorkspaceRoot: t.TempDir(), Manager: &replicaTestManager{},
	})
	if err != nil {
		t.Fatal(err)
	}
	controller.acknowledgeLiveResult(context.Background(), replicaItem{WorkspaceID: "w1234567", BindingVersion: 1}, mordesync.Result{
		Session:   mordesync.SyncSession{ID: "p1234567", State: "completed", TargetRevision: 2},
		Conflicts: []mordesync.Conflict{{Path: "shared.txt", Reason: "content_changed"}},
	})
	if got := requests.Load(); got != 0 {
		t.Fatalf("conflicted completed session sent %d applied receipts", got)
	}
}

func waitAppliedReceipt(t *testing.T, receipts <-chan string) string {
	t.Helper()
	select {
	case sessionID := <-receipts:
		return sessionID
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Host applied revision receipt")
		return ""
	}
}

func (c *Controller) hasRunningWatcher(workspaceID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, exists := c.watchers[workspaceID]
	if !exists {
		return false
	}
	select {
	case <-current.done:
		return false
	default:
		return true
	}
}

type replicaSyncTestPortal struct {
	mu            sync.Mutex
	items         map[string]replicaItem
	sessions      map[string]int
	statusUpdates chan replicaSyncStatusUpdate
	watchStarted  map[string]chan struct{}
}

type replicaSyncStatusUpdate struct {
	WorkspaceID string
	State       string
	Conflicts   []string
}

func newReplicaSyncTestPortal(hostID string, workspaces ...string) *replicaSyncTestPortal {
	portal := &replicaSyncTestPortal{
		items: make(map[string]replicaItem), sessions: make(map[string]int),
		statusUpdates: make(chan replicaSyncStatusUpdate, 20), watchStarted: make(map[string]chan struct{}),
	}
	for _, workspaceID := range workspaces {
		portal.items[workspaceID] = replicaItem{
			WorkspaceID: workspaceID, SyncCredential: "scoped", Directory: workspaceID, State: "active", SyncState: "healthy",
			BindingVersion: 1, DesiredRevision: 1, AppliedRevision: 1,
		}
		portal.watchStarted[workspaceID] = make(chan struct{}, 5)
	}
	return portal
}

func (p *replicaSyncTestPortal) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/host-replicas/h1234567/generations/2/workspace-bindings" && r.Method == http.MethodGet {
		if r.Header.Get("Authorization") != "Bearer bootstrap" {
			http.Error(w, "missing bootstrap credential", http.StatusUnauthorized)
			return
		}
		p.mu.Lock()
		items := make([]replicaItem, 0, len(p.items))
		for _, item := range p.items {
			items = append(items, item)
		}
		p.mu.Unlock()
		_ = json.NewEncoder(w).Encode(bindingSnapshot{RuntimeGeneration: 2, Items: items})
		return
	}
	if r.URL.Path == "/api/host-replicas/h1234567/generations/2/task-workspace-view-deletions" && r.Method == http.MethodGet {
		if r.Header.Get("Authorization") != "Bearer bootstrap" {
			http.Error(w, "missing bootstrap credential", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(contract.TaskWorkspaceViewDeletionSnapshot{
			RuntimeGeneration: 2, Items: []contract.PendingTaskWorkspaceViewDeletion{},
		})
		return
	}

	if strings.HasPrefix(r.URL.Path, "/api/host-replicas/") && strings.HasSuffix(r.URL.Path, "/sync-status") && r.Method == http.MethodPost {
		if r.Header.Get("Authorization") != "Bearer bootstrap" {
			http.Error(w, "missing bootstrap credential", http.StatusUnauthorized)
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		workspaceID := parts[len(parts)-4]
		var body struct {
			State     string   `json:"state"`
			Conflicts []string `json:"conflicts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid status", http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		item := p.items[workspaceID]
		item.SyncState, item.SyncConflicts = body.State, append([]string(nil), body.Conflicts...)
		p.items[workspaceID] = item
		p.mu.Unlock()
		p.statusUpdates <- replicaSyncStatusUpdate{WorkspaceID: workspaceID, State: body.State, Conflicts: body.Conflicts}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/api/host-replicas/") && strings.HasSuffix(r.URL.Path, "/applied") && r.Method == http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
		return
	}

	if strings.HasPrefix(r.URL.Path, "/api/workspaces/") {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 4 {
			http.NotFound(w, r)
			return
		}
		workspaceID := parts[2]
		switch {
		case r.Method == http.MethodGet && parts[3] == "manifest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workspace_id": workspaceID, "revision": 1, "files": []any{}, "next_cursor": nil,
			})
			return
		case r.Method == http.MethodPost && parts[3] == "sync-sessions":
			p.mu.Lock()
			p.sessions[workspaceID]++
			count := p.sessions[workspaceID]
			p.mu.Unlock()
			state := "completed"
			conflicts := []any{}
			if workspaceID == "w1234567" && count == 1 {
				state = "conflict"
				conflicts = []any{map[string]any{"path": "main.go", "reason": "content_changed"}}
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": fmt.Sprintf("p%07d", count), "workspace_id": workspaceID, "direction": "bidirectional", "policy": "fill_missing",
				"state": state, "base_revision": 1, "target_revision": 1, "stage": "complete", "planned_bytes": 0,
				"confirmed_bytes": 0, "transfer_blocks": []any{}, "download_actions": []any{}, "conflicts": conflicts,
			})
			return
		case r.Method == http.MethodGet && parts[3] == "sync-sessions" && len(parts) == 5:
			p.mu.Lock()
			count := p.sessions[workspaceID]
			p.mu.Unlock()
			if parts[4] != fmt.Sprintf("p%07d", count) || count == 0 {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": parts[4], "workspace_id": workspaceID, "direction": "bidirectional", "policy": "fill_missing",
				"state": "completed", "base_revision": 1, "target_revision": 1, "stage": "complete", "planned_bytes": 0,
				"confirmed_bytes": 0, "transfer_blocks": []any{}, "download_actions": []any{}, "conflicts": []any{},
			})
			return
		case r.Method == http.MethodGet && parts[3] == "changes":
			select {
			case p.watchStarted[workspaceID] <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return
		}
	}
	http.NotFound(w, r)
}

func (p *replicaSyncTestPortal) reset(workspaceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	item := p.items[workspaceID]
	item.SyncState = "healthy"
	item.SyncConflicts = []string{}
	p.items[workspaceID] = item
}

func (p *replicaSyncTestPortal) sessionCount(workspaceID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessions[workspaceID]
}

func (p *replicaSyncTestPortal) lastStatus(workspaceID string) replicaSyncStatusUpdate {
	p.mu.Lock()
	defer p.mu.Unlock()
	return replicaSyncStatusUpdate{WorkspaceID: workspaceID, State: p.items[workspaceID].SyncState, Conflicts: append([]string(nil), p.items[workspaceID].SyncConflicts...)}
}

func waitSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func waitStatus(t *testing.T, updates <-chan replicaSyncStatusUpdate, workspaceID, state string) replicaSyncStatusUpdate {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case update := <-updates:
			if update.WorkspaceID == workspaceID && update.State == state {
				return update
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for %s sync state for %s", state, workspaceID)
		}
	}
}
