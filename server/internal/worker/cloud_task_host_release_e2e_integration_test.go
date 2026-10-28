package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain/host"
	domainTemplate "morde/server/internal/domain/template"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacefs"
)

func TestHostReleaseAPIWaitsForActiveWorkspaceTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	workspaceValue := createF01Workspace(t, ctx, store, owner.ID)
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	hostValue, _, _ := createF01Host(t, ctx, store, sealer, owner.ID)
	binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: workspaceValue.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0); err != nil {
		t.Fatal(err)
	}

	blocks, err := workspacefs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close test Workspace blocks: %v", err)
		}
	}()
	syncService, err := app.NewWorkspaceSyncService(store, blocks, 0, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	hostReplicas, err := app.NewHostReplicaService(store, store, store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	auditService, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, auditService, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, f01DefaultHosts{})
	if err != nil {
		t.Fatal(err)
	}
	sessionCredential := createF01Session(t, ctx, store, owner.ID)
	taskService, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	hostService, err := app.NewHostService(app.HostServiceOptions{
		Hosts: store, Users: store, Workspaces: store, Tokens: store,
		Sandboxes: f01SandboxRouter{endpoint: "https://runtime.example.test"},
		Volumes:   f01ReleaseNoopVolume{}, DiskCapacity: f01ReleaseNoopCapacity{},
		Probe: f01ReleaseNoopProbe{}, Runtime: f01ReleaseNoopRuntime{}, Audit: auditService,
		CreateLocks: store, Sealer: sealer,
		Templates:         map[string]domainTemplate.Template{"f01-local-template": f01ReleaseTemplate()},
		DefaultTemplateID: "f01-local-template", Capacity: app.Capacity{CPUMilli: 100_000, MemMiB: 200_000},
		Instance: "f01-release-test", Scheme: "https", PortalURL: "https://portal.example.test", HostSuffix: "hosts.example.test",
		TicketTTL: time.Minute, ReadyTimeout: time.Second, ReadyPollInterval: time.Millisecond,
		NotifyCreate: func(context.Context, string) error { return nil }, ObserveHostReady: func(string, time.Duration) {},
		CountAuditFailure: func() {}, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	apiRouter, err := api.New(api.Deps{
		Hosts: hostService, HostReplicas: hostReplicas, TLSAuthorizer: hostService,
		Verifier: credentials, Auth: f01UnusedAuth{}, Tasks: taskService, WorkspaceSync: syncService, MCPDisabled: true,
	}, api.Options{
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), APIMaxBodyBytes: 16 << 20, MCPMaxBodyBytes: 16 << 20,
		SessionTTL: time.Hour, HostScheme: "https", PortalDomain: "portal.example.test", HostSuffix: "hosts.example.test", PreviewPortMin: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	portal := httptest.NewServer(apiRouter)
	defer portal.Close()
	client := portal.Client()

	body, err := json.Marshal(map[string]any{
		"host_id": hostValue.ID, "workspace_id": workspaceValue.ID, "input_revision": workspaceValue.Revision,
		"model_id": "p1234567", "objective": "Write a file in the bound Workspace.",
	})
	if err != nil {
		t.Fatal(err)
	}
	submitRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, portal.URL+"/api/tasks", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	submitRequest.AddCookie(&http.Cookie{Name: "ch_portal", Value: sessionCredential})
	submitRequest.Header.Set("Content-Type", "application/json")
	submitRequest.Header.Set("Origin", portal.URL)
	submitRequest.Header.Set("Idempotency-Key", "f01-host-release-task-001")
	submitResponse, err := client.Do(submitRequest)
	if err != nil {
		t.Fatal(err)
	}
	var accepted struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.NewDecoder(submitResponse.Body).Decode(&accepted); err != nil {
		_ = submitResponse.Body.Close()
		t.Fatal(err)
	}
	_ = submitResponse.Body.Close()
	if submitResponse.StatusCode != http.StatusAccepted || accepted.ID == "" || accepted.Status != "queued" || accepted.WorkspaceID != workspaceValue.ID {
		t.Fatalf("POST /api/tasks = status %d response %#v", submitResponse.StatusCode, accepted)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "f01-host-release-worker", time.Minute)
	if err != nil || !found || claimed.ID != accepted.ID || claimed.Status != "running" {
		t.Fatalf("claim Workspace task before Host release = task %#v found=%t err=%v", claimed, found, err)
	}

	releaseBody := bytes.NewBufferString(`{"confirm_process_termination":true}`)
	releaseRequest, err := http.NewRequestWithContext(ctx, http.MethodPost,
		portal.URL+"/api/hosts/"+hostValue.ID+"/release", releaseBody)
	if err != nil {
		t.Fatal(err)
	}
	releaseRequest.AddCookie(&http.Cookie{Name: "ch_portal", Value: sessionCredential})
	releaseRequest.Header.Set("Content-Type", "application/json")
	releaseRequest.Header.Set("Origin", portal.URL)
	releaseResponse, err := client.Do(releaseRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = releaseResponse.Body.Close()
	if releaseResponse.StatusCode != http.StatusConflict {
		t.Fatalf("POST /api/hosts/{id}/release during active Workspace Task = %d, want %d", releaseResponse.StatusCode, http.StatusConflict)
	}

	storedHost, err := store.GetHost(ctx, hostValue.ID)
	if err != nil || storedHost.State != host.StateRunning {
		t.Fatalf("Host after rejected release = %q, err=%v; want running", storedHost.State, err)
	}
	taskDetails, err := store.GetTask(ctx, accepted.ID, owner.ID)
	if err != nil || taskDetails.Task.Status != "running" {
		t.Fatalf("active Workspace Task after rejected release = %q, err=%v; want running", taskDetails.Task.Status, err)
	}
	storedWorkspace, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || storedWorkspace.State != workspace.StateReady || storedWorkspace.Revision != workspaceValue.Revision {
		t.Fatalf("Workspace after rejected release = %#v, err=%v", storedWorkspace, err)
	}
	storedBinding, err := store.GetWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID)
	if err != nil || storedBinding.State != workspace.BindingActive || storedBinding.AppliedRevision != 0 || storedBinding.DesiredRevision != 0 {
		t.Fatalf("Workspace binding after rejected release = %#v, err=%v", storedBinding, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='completed', finished_at=now() WHERE id=$1`, accepted.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetWorkspaceBindingSyncStatus(ctx, hostValue.ID, workspaceValue.ID, storedBinding.BindingVersion,
		workspace.BindingSyncConflict, []string{"pending.txt"}); err != nil {
		t.Fatal(err)
	}
	conflictRequest, err := http.NewRequestWithContext(ctx, http.MethodPost,
		portal.URL+"/api/hosts/"+hostValue.ID+"/release", bytes.NewBufferString(`{"confirm_process_termination":true}`))
	if err != nil {
		t.Fatal(err)
	}
	conflictRequest.AddCookie(&http.Cookie{Name: "ch_portal", Value: sessionCredential})
	conflictRequest.Header.Set("Content-Type", "application/json")
	conflictRequest.Header.Set("Origin", portal.URL)
	conflictResponse, err := client.Do(conflictRequest)
	if err != nil {
		t.Fatal(err)
	}
	var refused struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	decodeErr := json.NewDecoder(conflictResponse.Body).Decode(&refused)
	_ = conflictResponse.Body.Close()
	if decodeErr != nil || conflictResponse.StatusCode != http.StatusConflict ||
		refused.Error.Code != "conflict" || refused.Error.Details["workspace_id"] != workspaceValue.ID {
		t.Fatalf("POST /api/hosts/{id}/release during Workspace conflict = status %d, error %#v, decode %v",
			conflictResponse.StatusCode, refused.Error, decodeErr)
	}
}

type f01ReleaseNoopVolume struct{}

func (f01ReleaseNoopVolume) DeleteVolume(context.Context, string) error { return nil }

type f01ReleaseNoopCapacity struct{}

func (f01ReleaseNoopCapacity) CheckCapacity(context.Context) error { return nil }

type f01ReleaseNoopProbe struct{}

func (f01ReleaseNoopProbe) Ready(context.Context, app.Upstream) error { return nil }

type f01ReleaseNoopRuntime struct{}

func (f01ReleaseNoopRuntime) Handshake(context.Context, string) error { return nil }

func f01ReleaseTemplate() domainTemplate.Template {
	return domainTemplate.Template{
		ID: "f01-local-template", Name: "F01 local", Image: "example/f01:1@sha256:" + strings.Repeat("a", 64),
		Entrypoint: []string{domainTemplate.RuntimeBootstrapEntrypoint},
		Resources: domainTemplate.Resources{
			Requests: domainTemplate.Resource{CPU: "500m", Memory: "1536Mi"},
			Limits:   domainTemplate.Resource{CPU: "2", Memory: "4Gi"},
		},
		Workspace: domainTemplate.Workspace{MountPath: "/home/gem/workspace", Size: "2Gi"},
		Lifecycle: domainTemplate.Lifecycle{TTL: 8 * time.Hour, IdleTimeout: time.Hour, OnIdle: domainTemplate.OnIdleDisabled},
		Isolation: "f01-local",
	}
}
