package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type coverageTokenStore struct {
	*credentialTokens
	getErr     error
	revokeErr  error
	consumeErr error
}

func (s *coverageTokenStore) GetTokenByID(ctx context.Context, id string) (identity.Token, error) {
	if s.getErr != nil {
		return identity.Token{}, s.getErr
	}
	return s.credentialTokens.GetTokenByID(ctx, id)
}

func (s *coverageTokenStore) ListTokensByOwner(_ context.Context, ownerID string) ([]identity.Token, error) {
	values := make([]identity.Token, 0)
	for _, value := range s.values {
		if value.OwnerID == ownerID {
			values = append(values, value)
		}
	}
	return values, nil
}

func (s *coverageTokenStore) RevokeToken(ctx context.Context, id, ownerID string) error {
	if s.revokeErr != nil {
		return s.revokeErr
	}
	return s.credentialTokens.RevokeToken(ctx, id, ownerID)
}

func (s *coverageTokenStore) ConsumeTicket(ctx context.Context, id string) (Ticket, error) {
	if s.consumeErr != nil {
		return Ticket{}, s.consumeErr
	}
	return s.credentialTokens.ConsumeTicket(ctx, id)
}

func TestCoverageAPIKeyLifecycleAndCurrentUser(t *testing.T) {
	credentials, users, _, _, tokens, _ := newCredentialFixture(t)
	store := &coverageTokenStore{credentialTokens: tokens}
	writer := &credentialAudit{}
	service, err := NewTokenService(store, writer, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567",
	}
	created, err := service.CreateAPIKey(context.Background(), principal, "  editor  ", 0)
	if err != nil || created.ID == "" || created.Key == "" {
		t.Fatalf("CreateAPIKey() = %#v, %v", created, err)
	}
	if values, err := service.ListAPIKeys(context.Background(), principal); err != nil || len(values) != 1 || values[0].ID != created.ID {
		t.Fatalf("ListAPIKeys() = %#v, %v", values, err)
	}
	if _, _, err := issueCredential(context.Background(), store, principal.UserID, identity.KindTicket, "h1234567", "", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if values, err := service.ListAPIKeys(context.Background(), principal); err != nil || len(values) != 1 {
		t.Fatalf("ListAPIKeys() included non-key credentials: %#v, %v", values, err)
	}
	verified, err := credentials.VerifyAPIKey(context.Background(), created.Key)
	if err != nil || verified.Kind != identity.KindAPIKey || verified.UserID != principal.UserID || verified.TokenID != created.ID ||
		len(verified.HostIDs) != 0 || len(verified.Scopes) != 0 {
		t.Fatalf("VerifyAPIKey() = %#v, %v", verified, err)
	}
	current, err := credentials.CurrentUser(context.Background(), verified)
	if err != nil || current.ID != principal.UserID || current.Name != users.values[principal.UserID].Name {
		t.Fatalf("CurrentUser() = %#v, %v", current, err)
	}
	if err := service.DeleteAPIKey(context.Background(), principal, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.VerifyAPIKey(context.Background(), created.Key); !errors.Is(err, identity.ErrInvalidCredential) {
		t.Fatalf("VerifyAPIKey() after revoke = %v", err)
	}
	if _, err := service.ListAPIKeys(context.Background(), identity.Principal{}); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("ListAPIKeys() without credential = %v", err)
	}
	if _, err := service.CreateAPIKey(context.Background(), principal, "", time.Hour); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("CreateAPIKey() with empty name = %v", err)
	}
	if _, err := service.CreateAPIKey(context.Background(), principal, "too long", 25*time.Hour); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("CreateAPIKey() above max ttl = %v", err)
	}
	if _, err := NewTokenService(nil, writer, time.Hour, time.Hour); err == nil {
		t.Fatal("NewTokenService() accepted a missing store")
	}
	if _, err := NewTokenService(store, writer, 0, time.Hour); err == nil {
		t.Fatal("NewTokenService() accepted invalid lifetimes")
	}
	if err := service.DeleteAPIKey(context.Background(), principal, "t7654321"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("DeleteAPIKey() for missing key = %v", err)
	}
	_, foreignKey, err := issueCredential(context.Background(), store, "u2222222", identity.KindAPIKey, "", "foreign", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, foreignID, _, err := identity.ParseCredential(foreignKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.DeleteAPIKey(context.Background(), principal, foreignID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("DeleteAPIKey() for another owner = %v", err)
	}
	failedAudit := &credentialAudit{err: errors.New("audit unavailable")}
	failedService, err := NewTokenService(store, failedAudit, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failedService.CreateAPIKey(context.Background(), principal, "rollback", time.Hour); err == nil {
		t.Fatal("CreateAPIKey() ignored audit failure")
	}
}

func TestCoverageTicketAndLogoutFailurePaths(t *testing.T) {
	credentials, _, _, _, tokens, auditor := newCredentialFixture(t)
	store := &coverageTokenStore{credentialTokens: tokens}
	credentials.tokens = store
	if _, err := credentials.ConsumeTicket(context.Background(), "bad"); !errors.Is(err, identity.ErrInvalidCredential) {
		t.Fatalf("ConsumeTicket(malformed) = %v", err)
	}
	_, apiKey, err := issueCredential(context.Background(), store, "u1234567", identity.KindAPIKey, "", "key", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.ConsumeTicket(context.Background(), apiKey); !errors.Is(err, identity.ErrInvalidCredential) {
		t.Fatalf("ConsumeTicket(API key) = %v", err)
	}
	noHost, noHostHash, err := identity.NewCredential(identity.KindTicket, "t1234567")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateToken(context.Background(), identity.Token{
		ID: "t1234567", OwnerID: "u1234567", Kind: identity.KindTicket, SecretHash: noHostHash,
		ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.ConsumeTicket(context.Background(), noHost); !errors.Is(err, identity.ErrInvalidCredential) {
		t.Fatalf("ConsumeTicket(ticket without host) = %v", err)
	}
	_, ticket, err := issueCredential(context.Background(), store, "u1234567", identity.KindTicket, "h1234567", "", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	store.getErr = errors.New("database unavailable")
	if _, err := credentials.ConsumeTicket(context.Background(), ticket); err == nil || err.Error() != "database unavailable" {
		t.Fatalf("ConsumeTicket(store read error) = %v", err)
	}
	store.getErr = nil
	store.consumeErr = errors.New("ticket transaction failed")
	if _, err := credentials.ConsumeTicket(context.Background(), ticket); err == nil || err.Error() != "ticket transaction failed" {
		t.Fatalf("ConsumeTicket(consume error) = %v", err)
	}
	store.consumeErr = nil
	wrongSuffix := "A"
	if ticket[len(ticket)-1:] == wrongSuffix {
		wrongSuffix = "B"
	}
	badSecret := ticket[:len(ticket)-1] + wrongSuffix
	if _, err := credentials.ConsumeTicket(context.Background(), badSecret); !errors.Is(err, identity.ErrInvalidCredential) {
		t.Fatalf("ConsumeTicket(wrong secret) = %v", err)
	}
	if err := credentials.Logout(context.Background(), identity.Principal{Kind: identity.KindAPIKey, UserID: "u1234567", TokenID: "t1234567"}); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("Logout(non-session) = %v", err)
	}
	_, session, err := issueCredential(context.Background(), store, "u1234567", identity.KindSession, "", "session", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, tokenID, _, err := identity.ParseCredential(session)
	if err != nil {
		t.Fatal(err)
	}
	store.revokeErr = errors.New("revoke failed")
	if err := credentials.Logout(context.Background(), identity.Principal{Kind: identity.KindSession, UserID: "u1234567", TokenID: tokenID}); err == nil {
		t.Fatal("Logout() ignored revoke error")
	}
	store.revokeErr = nil
	auditor.err = errors.New("audit unavailable")
	if err := credentials.Logout(context.Background(), identity.Principal{Kind: identity.KindSession, UserID: "u1234567", TokenID: tokenID}); err == nil {
		t.Fatal("Logout() ignored audit error")
	}
}

func TestCoverageUserUsageOwnerIsolationAndUpdates(t *testing.T) {
	credentials, users, hosts, workspaces, _, _ := newCredentialFixture(t)
	attached := workspace.Workspace{ID: "w1234567", OwnerID: "u1234567", State: workspace.StateReady}
	workspaces.values[attached.ID] = attached
	workspaces.values["w1234571"] = workspace.Workspace{ID: "w1234571", OwnerID: "u1234567", State: workspace.StateReady}
	workspaces.values["w2222222"] = workspace.Workspace{ID: "w2222222", State: workspace.StateReady}
	hosts.values = []host.Host{
		{ID: "h1234567", OwnerID: "u1234567", State: host.StateCreating, CPUMilli: 100, MemMiB: 200},
		{ID: "h1234568", OwnerID: "u1234567", State: host.StateRunning, CPUMilli: 200, MemMiB: 300},
		{ID: "h1234569", OwnerID: "u1234567", State: host.StateDeleting, CPUMilli: 300, MemMiB: 400},
		{ID: "h1234570", OwnerID: "u1234567", State: host.StateFailed, SandboxID: "stranded", CPUMilli: 400, MemMiB: 500},
		{ID: "h1234571", OwnerID: "u1234567", Workspaces: []workspace.Binding{testWorkspaceBinding("h1234571", "u1234567", "w1234571", workspace.BindingPreparing)}, State: host.StateFailed, CPUMilli: 500, MemMiB: 600},
		{ID: "h1234572", OwnerID: "u1234567", Workspaces: []workspace.Binding{testWorkspaceBinding("h1234572", "u1234567", "w2222222", workspace.BindingDetached)}, State: host.StateFailed, CPUMilli: 600, MemMiB: 700},
		{ID: "h1234573", OwnerID: "u1234567", State: host.StateFailed, CPUMilli: 700, MemMiB: 800},
		{ID: "h1234574", OwnerID: "u1234567", State: host.StateDeleted, CPUMilli: 800, MemMiB: 900},
	}
	userService, err := NewUserService(users, hosts)
	if err != nil {
		t.Fatal(err)
	}
	admin := identity.Principal{Role: identity.RoleAdmin, Kind: identity.KindSystem}
	items, err := userService.List(context.Background(), admin, nil)
	want := ResourceUsage{Hosts: 7, CPUMilli: 1500, MemMiB: 2000}
	if err != nil || len(items) != 1 || items[0].Usage != want {
		t.Fatalf("UserService.List() = %#v, %v", items, err)
	}
	member := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	current, err := credentials.CurrentUser(context.Background(), member)
	if err != nil || current.Usage != want {
		t.Fatalf("CurrentUser() = %#v, %v", current, err)
	}
	role := identity.RoleAdmin
	quota := ResourceUsage{Hosts: 7, CPUMilli: 9000, MemMiB: 16000}
	updated, err := userService.Update(context.Background(), admin, member.UserID, UpdateUserCommand{
		ExpectedStatus: identity.StatusApproved, Role: &role, Quota: &quota,
	})
	if err != nil || updated.Role != role || updated.Quota != quota || updated.Usage != want {
		t.Fatalf("UserService.Update() = %#v, %v", updated, err)
	}
	badRole := identity.Role("owner")
	if _, err := userService.Update(context.Background(), admin, member.UserID, UpdateUserCommand{ExpectedStatus: identity.StatusApproved, Role: &badRole}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("Update() with invalid role = %v", err)
	}
	badQuota := ResourceUsage{Hosts: -1}
	if _, err := userService.Update(context.Background(), admin, member.UserID, UpdateUserCommand{ExpectedStatus: identity.StatusApproved, Quota: &badQuota}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("Update() with negative quota = %v", err)
	}
	if _, err := userService.Update(context.Background(), admin, member.UserID, UpdateUserCommand{}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("Update() with no fields = %v", err)
	}
	if _, err := userService.Update(context.Background(), admin, "bad-id", UpdateUserCommand{ExpectedStatus: identity.StatusApproved, Role: &role}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("Update() with bad user id = %v", err)
	}
	if _, err := credentials.CurrentUser(context.Background(), identity.Principal{UserID: member.UserID, Kind: identity.KindSession}); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("CurrentUser() without token id = %v", err)
	}
	disabled := identity.StatusDisabled
	if _, err := userService.Update(context.Background(), admin, member.UserID, UpdateUserCommand{ExpectedStatus: identity.StatusApproved, Status: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.CurrentUser(context.Background(), member); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("CurrentUser() for disabled account = %v", err)
	}
}

func TestCoverageCapacityCountsAllocationsAndStates(t *testing.T) {
	service, deps := newHostServiceTest(t)
	active := hostServiceTestHost(host.StateRunning)
	failed := hostServiceTestHost(host.StateFailed)
	failed.ID, failed.SandboxID, failed.CPUMilli = "h7654321", "sandbox-stuck", 100
	deleted := hostServiceTestHost(host.StateDeleted)
	deleted.ID = "h8765432"
	deps.hosts.addHost(active)
	deps.hosts.addHost(failed)
	deps.hosts.addHost(deleted)
	deps.workspaces.add(workspace.Workspace{ID: testHostWorkspaceID(active), State: workspace.StateReady})
	deps.workspaces.add(workspace.Workspace{ID: testHostWorkspaceID(failed), State: workspace.StateReady})
	snapshot, err := service.Capacity(context.Background(), identity.Principal{Role: identity.RoleAdmin, Kind: identity.KindSystem})
	if err != nil || snapshot.Limit != (Capacity{CPUMilli: 8000, MemMiB: 16384}) || snapshot.Allocated.Hosts != 2 || snapshot.HostsByState[host.StateDeleted] != 1 {
		t.Fatalf("Capacity() = %#v, %v", snapshot, err)
	}
	if _, err := service.Capacity(context.Background(), hostServiceTestPrincipal()); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("Capacity() for non-admin = %v", err)
	}
}

func TestCoverageHostErrorMappingsAndURL(t *testing.T) {
	service, _ := newHostServiceTest(t)
	if got := service.hostURL("h1234567"); got != "https://h1234567.hosts.test" {
		t.Fatalf("hostURL() = %q", got)
	}
	var domainErr *domain.Error
	if err := invalidHostState(host.StateCreating); domain.CodeOf(err) != domain.CodeInvalidState || !errors.As(err, &domainErr) || domainErr.Details["state"] != string(host.StateCreating) {
		t.Fatalf("invalidHostState() = %v", err)
	}
	if err := hostStateError(host.StateFailed); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("hostStateError() = %v", err)
	}
	for _, test := range []struct {
		cause error
		code  domain.ErrorCode
	}{
		{ErrSandboxUnsupported, domain.CodeUnsupported},
		{ErrSandboxNotFound, domain.CodeNotFound},
		{ErrSandboxConflict, domain.CodeInvalidState},
		{errors.New("network timeout"), domain.CodeUpstreamError},
	} {
		if got := domain.CodeOf(sandboxOperationError("renew sandbox", test.cause)); got != test.code {
			t.Errorf("sandboxOperationError(%v) code = %s, want %s", test.cause, got, test.code)
		}
	}
}

func TestCoverageAuditSanitizesNestedCredentials(t *testing.T) {
	detail := sanitizeAuditDetail(map[string]any{
		"message": "ok", "api-key": "secret", "nested": map[string]any{"password": "hidden", "count": 2},
		"headers": map[string]string{"Authorization": "hidden", "accept": "json"},
		"values":  []any{"visible", map[string]any{"cookie": "hidden", "name": "demo"}},
		"rows":    []map[string]any{{"token_id": "hidden", "state": "running"}},
	})
	if _, exists := detail["api-key"]; exists {
		t.Fatalf("top-level key retained: %#v", detail)
	}
	if nested := detail["nested"].(map[string]any); len(nested) != 1 || nested["count"] != 2 {
		t.Fatalf("nested detail = %#v", nested)
	}
	if headers := detail["headers"].(map[string]string); len(headers) != 1 || headers["accept"] != "json" {
		t.Fatalf("sanitized headers = %#v", headers)
	}
	values := detail["values"].([]any)
	if values[0] != "visible" || len(values[1].(map[string]any)) != 1 {
		t.Fatalf("sanitized values = %#v", values)
	}
	rows := detail["rows"].([]map[string]any)
	if len(rows) != 1 || len(rows[0]) != 1 || rows[0]["state"] != "running" {
		t.Fatalf("sanitized rows = %#v", rows)
	}
}

func TestCoverageDeleteHostBeginsDeletionAndRejectsHostCredential(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateRunning)
	value.Workspaces[0].State = workspace.BindingDetached
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	})
	deleted, err := service.Delete(context.Background(), hostServiceTestPrincipal(), DeleteHostCommand{HostID: value.ID})
	if err != nil || deleted.State != host.StateDeleting {
		t.Fatalf("Delete() = %#v, %v", deleted, err)
	}
	if _, err := service.Delete(context.Background(), hostServiceTestPrincipal(), DeleteHostCommand{HostID: value.ID}); err != nil {
		t.Fatalf("Delete(already deleting) = %v", err)
	}
	principal := hostServiceTestPrincipal()
	principal.Kind, principal.HostScope = identity.KindHostCookie, value.ID
	if _, err := service.Delete(context.Background(), principal, DeleteHostCommand{HostID: value.ID}); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("Delete(host cookie) = %v", err)
	}
}

func TestCoverageRuntimeLookupAndSandboxErrors(t *testing.T) {
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := &runtimeHostStore{value: host.Host{ID: "h1234567", OwnerID: "u1234567", State: host.StateRunning}}
	runtime, err := NewRuntime(store, runtimeRouter{&runtimeSandbox{}}, sealer, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := runtime.Lookup(context.Background(), store.value.ID)
	if err != nil || ref.ID != store.value.ID || ref.OwnerID != store.value.OwnerID || ref.State != host.StateRunning {
		t.Fatalf("Lookup() = %#v, %v", ref, err)
	}
	store.value.State = host.StateDeleted
	if _, err := runtime.Lookup(context.Background(), store.value.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("Lookup() deleted host = %v", err)
	}
	store.value.State = host.StateRunning
	store.value.DeletedAt = ptrTime(time.Now())
	if _, err := runtime.Lookup(context.Background(), store.value.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("Lookup() soft-deleted host = %v", err)
	}
	store.value.DeletedAt = nil
	store.value.State = host.StateCreating
	if _, err := runtime.Resolve(context.Background(), store.value.ID, 8080); domain.CodeOf(err) != domain.CodeHostNotReady {
		t.Fatalf("Resolve(creating host) = %v", err)
	}
	store.value.State = host.StateRunning
	if _, err := runtime.Resolve(context.Background(), store.value.ID, 8080); domain.CodeOf(err) != domain.CodeUpstreamError {
		t.Fatalf("Resolve(host without sandbox) = %v", err)
	}
	for _, test := range []struct {
		cause error
		code  domain.ErrorCode
	}{
		{ErrSandboxNotFound, domain.CodeNotFound},
		{ErrSandboxUnsupported, domain.CodeUnsupported},
		{ErrSandboxConflict, domain.CodeInvalidState},
		{errors.New("network timeout"), domain.CodeUpstreamError},
	} {
		if got := domain.CodeOf(sandboxError("resolve endpoint", test.cause)); got != test.code {
			t.Errorf("sandboxError(%v) code = %s, want %s", test.cause, got, test.code)
		}
	}
}

func TestCoverageLoadSealerAndWorkerAppFacade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.key")
	if err := os.WriteFile(path, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSealer(path); err != nil {
		t.Fatalf("LoadSealer(valid key) = %v", err)
	}
	badPath := filepath.Join(t.TempDir(), "bad.key")
	if err := os.WriteFile(badPath, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSealer(badPath); err == nil {
		t.Fatal("LoadSealer() accepted a short key")
	}
	if _, err := LoadSealer(filepath.Join(t.TempDir(), "missing.key")); err == nil {
		t.Fatal("LoadSealer() accepted a missing file")
	}

	service, deps := newHostServiceTest(t)
	creating := hostServiceTestHost(host.StateCreating)
	creating.ID = "h1234567"
	running := hostServiceTestHost(host.StateRunning)
	running.ID = "h7654321"
	deleted := hostServiceTestHost(host.StateDeleted)
	deleted.ID = "h8765432"
	deps.hosts.addHost(creating)
	deps.hosts.addHost(running)
	deps.hosts.addHost(deleted)
	worker := newWorkerAppTest(t, service)
	ids, err := worker.ListCreatingHosts(context.Background())
	if err != nil || len(ids) != 1 || ids[0] != creating.ID {
		t.Fatalf("ListCreatingHosts() = %v, %v", ids, err)
	}
	counts, err := worker.HostCounts(context.Background())
	if err != nil || counts[host.StateCreating] != 1 || counts[host.StateRunning] != 1 || counts[host.StateDeleted] != 1 {
		t.Fatalf("HostCounts() = %#v, %v", counts, err)
	}
	if _, acquired, err := worker.TryReconcilerLock(context.Background()); err != nil || acquired {
		t.Fatalf("TryReconcilerLock() = %v, %v", acquired, err)
	}
	if err := worker.FlushActivity(context.Background()); err != nil {
		t.Fatalf("FlushActivity(empty) = %v", err)
	}
	if _, err := worker.AdvanceCreate(context.Background(), running.ID); err != nil {
		t.Fatalf("AdvanceCreate(running) = %v", err)
	}
	if err := waitContext(context.Background(), time.Now().Add(-time.Second), time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitContext(expired) = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitContext(ctx, time.Now().Add(time.Second), time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitContext(canceled) = %v", err)
	}
}

func TestCoverageWorkerPreservesWorkspaceAcrossRootVolumeDeleteRetry(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateDeleting)
	value.SandboxID = ""
	value.Workspaces[0].State = workspace.BindingDetached
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID,
		State: workspace.StateReady,
	})
	volume := service.volumes.(*hostServiceTestVolume)
	volume.errByName = map[string]error{domain.WorkspaceRootVolumeName(value.ID): errors.New("volume is still mounted")}
	worker := newWorkerAppTest(t, service)
	if _, err := worker.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() with busy volume returned no error")
	}
	workspaceAfterFailure, err := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	if err != nil || workspaceAfterFailure.State != workspace.StateReady {
		t.Fatalf("workspace after failed volume delete = %#v, %v", workspaceAfterFailure, err)
	}
	hostAfterFailure, err := deps.hosts.GetHost(context.Background(), value.ID)
	if err != nil || hostAfterFailure.State != host.StateDeleting {
		t.Fatalf("host after failed volume delete = %#v, %v", hostAfterFailure, err)
	}
	binding, err := deps.workspaces.GetWorkspaceBinding(context.Background(), value.ID, testHostWorkspaceID(value))
	if err != nil || binding.State != workspace.BindingDetached {
		t.Fatalf("binding after root volume failure = %#v, %v", binding, err)
	}
	volume.errByName = nil
	if _, err := worker.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() retry = %v", err)
	}
	workspaceAfterRetry, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	hostAfterRetry, _ := deps.hosts.GetHost(context.Background(), value.ID)
	if workspaceAfterRetry.State != workspace.StateReady || hostAfterRetry.State != host.StateDeleted {
		t.Fatalf("host/workspace after retry = %#v / %#v", hostAfterRetry, workspaceAfterRetry)
	}
}

func TestCoverageConstructorsRejectInvalidDependencies(t *testing.T) {
	credentials, users, hosts, workspaces, tokens, auditor := newCredentialFixture(t)
	if credentials == nil {
		t.Fatal("fixture credential service was nil")
	}
	options := CredentialOptions{SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 2, LoginWindow: time.Minute}
	if _, err := NewCredentialService(nil, hosts, tokens, auditor, options, hosts); err == nil {
		t.Fatal("NewCredentialService() accepted a missing user store")
	}
	if _, err := NewCredentialService(users, hosts, tokens, auditor, CredentialOptions{}, hosts); err == nil {
		t.Fatal("NewCredentialService() accepted invalid limits")
	}
	if _, err := NewUserService(nil, hosts); err == nil {
		t.Fatal("NewUserService() accepted a missing user store")
	}
	if _, err := NewWorkspaceService(workspaces, nil, &workspaceCapacityStub{}, auditor, 1); err == nil {
		t.Fatal("NewWorkspaceService() accepted a missing volume deleter")
	}
	service, deps := newHostServiceTest(t)
	if _, err := NewRuntime(nil, service.sandboxes, deps.sealer, time.Minute); err == nil {
		t.Fatal("NewRuntime() accepted a missing host store")
	}
	runtime, err := NewRuntime(service.hosts, service.sandboxes, deps.sealer, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewWorkerApp(nil, runtime, &workerTestOrphanCleanupStore{}, workerAppTestLocks{}, &workerTestModelCallRecovery{}, time.Minute); err == nil {
		t.Fatal("NewWorkerApp() accepted a missing host service")
	}
	if _, err := NewWorkerApp(service, runtime, &workerTestOrphanCleanupStore{}, workerAppTestLocks{}, nil, time.Minute); err == nil {
		t.Fatal("NewWorkerApp() accepted a missing model call recovery store")
	}
	if _, err := NewWorkerApp(service, runtime, &workerTestOrphanCleanupStore{}, workerAppTestLocks{}, &workerTestModelCallRecovery{}, 0); err == nil {
		t.Fatal("NewWorkerApp() accepted a non-positive renewal window")
	}
}

func TestCoverageWorkerRenewAuditFailureIsReportedAndCounted(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateRunning)
	value.CreatedAt = time.Now().Add(-2 * time.Hour)
	value.LastActiveAt = ptrTime(time.Now().Add(-2 * time.Hour))
	deps.hosts.addHost(value)
	deps.sandbox.add(Sandbox{ID: value.SandboxID, State: SandboxRunning, Metadata: hostServiceMetadata(value.ID)})
	deps.audit.err = errors.New("audit unavailable")
	auditFailures := 0
	service.countAudit = func() { auditFailures++ }
	worker := newWorkerAppTest(t, service)
	actions, err := worker.Reconcile(context.Background())
	if err == nil || actions["renew"] != 1 || auditFailures != 1 {
		t.Fatalf("Reconcile() = %#v, %v, audit failures=%d", actions, err, auditFailures)
	}
	updated, getErr := deps.hosts.GetHost(context.Background(), value.ID)
	if getErr != nil || updated.State != host.StateRunning {
		t.Fatalf("host after renew audit failure = %#v, %v", updated, getErr)
	}
}

func TestCoverageCreateCleanupFailureKeepsWorkspaceAttached(t *testing.T) {
	service, deps := newHostServiceTest(t)
	service.readyTimeout = 3 * time.Millisecond
	service.pollInterval = time.Millisecond
	value := hostServiceTestHost(host.StateCreating)
	sealHostServiceTestKey(t, deps.sealer, &value)
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	})
	deps.probe.err = errors.New("AIO not ready")
	deps.sandbox.deleteErr = errors.New("orchestrator unavailable")
	if err := service.AdvanceCreate(context.Background(), value.ID); err == nil {
		t.Fatal("AdvanceCreate() ignored sandbox cleanup failure")
	}
	got, _ := deps.hosts.GetHost(context.Background(), value.ID)
	ws, _ := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	binding, _ := deps.workspaces.GetWorkspaceBinding(context.Background(), value.ID, testHostWorkspaceID(value))
	if got.State != host.StateCreating || got.SandboxID == "" || ws.State != workspace.StateReady || binding.State != workspace.BindingActive {
		t.Fatalf("host/workspace after uncertain cleanup = %#v / %#v", got, ws)
	}
}

func TestCoverageWorkerReconcileHonorsCancellation(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestHost(host.StateRunning)
	deps.hosts.addHost(value)
	worker := newWorkerAppTest(t, service)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := worker.Reconcile(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile(canceled) = %v", err)
	}
}

func TestCoverageHostPrincipalValidation(t *testing.T) {
	valid := hostServiceTestPrincipal()
	if err := validatePrincipal(valid); err != nil {
		t.Fatalf("validatePrincipal(valid) = %v", err)
	}
	invalidUser := valid
	invalidUser.UserID = ""
	if domain.CodeOf(validatePrincipal(invalidUser)) != domain.CodeUnauthenticated {
		t.Fatalf("validatePrincipal(empty user) = %v", validatePrincipal(invalidUser))
	}
	invalidRole := valid
	invalidRole.Role = identity.Role("owner")
	if domain.CodeOf(validatePrincipal(invalidRole)) != domain.CodeForbidden {
		t.Fatalf("validatePrincipal(invalid role) = %v", validatePrincipal(invalidRole))
	}
	scopedSession := valid
	scopedSession.HostScope = "h1234567"
	if domain.CodeOf(validatePrincipal(scopedSession)) != domain.CodeUnauthenticated {
		t.Fatalf("validatePrincipal(scoped session) = %v", validatePrincipal(scopedSession))
	}
	hostCookie := valid
	hostCookie.Kind, hostCookie.HostScope = identity.KindHostCookie, "h1234567"
	if err := validatePrincipal(hostCookie); err != nil {
		t.Fatalf("validatePrincipal(scoped host cookie) = %v", err)
	}
	hostCookie.HostScope = "bad"
	if domain.CodeOf(validatePrincipal(hostCookie)) != domain.CodeUnauthenticated {
		t.Fatalf("validatePrincipal(invalid host scope) = %v", validatePrincipal(hostCookie))
	}
}

func TestCoverageUserCreateValidationAndAuditOutcome(t *testing.T) {
	_, users, hosts, _, _, _ := newCredentialFixture(t)
	service, err := NewUserService(users, hosts)
	if err != nil {
		t.Fatal(err)
	}
	member := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	if _, err := service.List(context.Background(), member, nil); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("List(non-admin) = %v", err)
	}
	if _, err := service.Register(context.Background(), "  ", "password123", "192.0.2.1"); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("Register(blank name) = %v", err)
	}
	if _, err := service.Register(context.Background(), "bob", "short", "192.0.2.1"); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("Register(short password) = %v", err)
	}
	if _, err := service.Register(context.Background(), "admin", "password123", "192.0.2.1"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("Register(reserved admin) = %v", err)
	}
}

type coverageAuditStore struct {
	writeErr   error
	listErr    error
	lastFilter AuditFilter
}

func (s *coverageAuditStore) WriteAudit(context.Context, audit.Entry) error { return s.writeErr }

func (s *coverageAuditStore) ListAuditLogs(_ context.Context, filter AuditFilter) ([]AuditLog, error) {
	s.lastFilter = filter
	if s.listErr != nil {
		return nil, s.listErr
	}
	return []AuditLog{}, nil
}

func TestCoverageAuditServiceValidationAndDefaults(t *testing.T) {
	if _, err := NewAuditService(nil); err == nil {
		t.Fatal("NewAuditService() accepted a nil store")
	}
	store := &coverageAuditStore{}
	service, err := NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Write(context.Background(), audit.Entry{Action: "not-a-real-action", ActorKind: audit.ActorSession}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("Write(invalid action) = %v", err)
	}
	if err := service.Write(context.Background(), audit.Entry{Action: audit.ActionUserCreate, ActorKind: audit.ActorKind("unknown")}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("Write(invalid actor) = %v", err)
	}
	if err := service.Write(context.Background(), audit.Entry{Action: audit.ActionUserCreate, ActorKind: audit.ActorSession}); err != nil {
		t.Fatalf("Write(valid entry) = %v", err)
	}
	admin := identity.Principal{Role: identity.RoleAdmin, Kind: identity.KindSystem}
	member := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	if _, err := service.List(context.Background(), member, AuditFilter{}); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("List(non-admin) = %v", err)
	}
	if _, err := service.List(context.Background(), admin, AuditFilter{Limit: 501}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("List(out-of-range limit) = %v", err)
	}
	if _, err := service.List(context.Background(), admin, AuditFilter{Action: "not-a-real-action"}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("List(invalid action) = %v", err)
	}
	since := time.Date(2025, time.January, 1, 0, 0, 0, 0, time.FixedZone("custom", 3600))
	if _, err := service.List(context.Background(), admin, AuditFilter{Since: &since}); err != nil {
		t.Fatalf("List(valid filters) = %v", err)
	}
	if store.lastFilter.Limit != 100 || store.lastFilter.Since == nil || store.lastFilter.Since.Location() != time.UTC {
		t.Fatalf("normalized audit filter = %#v", store.lastFilter)
	}
}

type coverageSandboxRouter struct{ clients map[string]SandboxClient }

func (r coverageSandboxRouter) For(profile string) (SandboxClient, error) {
	client, ok := r.clients[profile]
	if !ok {
		return nil, ErrSandboxUnsupported
	}
	return client, nil
}

func (r coverageSandboxRouter) Profiles() []string {
	profiles := make([]string, 0, len(r.clients))
	for profile := range r.clients {
		profiles = append(profiles, profile)
	}
	return profiles
}

func TestCoverageWorkerCleansMatchingSandboxFromOtherProfile(t *testing.T) {
	service, deps := newHostServiceTest(t)
	value := hostServiceTestTestHostForCoverage(host.StateDeleting, "h1234567")
	value.SandboxID = ""
	value.Workspaces[0].State = workspace.BindingDetached
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	})
	other := &hostServiceTestSandbox{values: make(map[string]Sandbox), getAfterDelete: true}
	other.add(Sandbox{ID: "other-profile", State: SandboxRunning, Metadata: hostServiceMetadata(value.ID)})
	other.add(Sandbox{ID: "foreign-env", State: SandboxRunning, Metadata: map[string]string{"ch-env": "elsewhere", "ch-host": value.ID}})
	service.sandboxes = coverageSandboxRouter{clients: map[string]SandboxClient{"container": deps.sandbox, "alternate": other}}
	worker := newWorkerAppTest(t, service)
	if _, err := worker.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() = %v", err)
	}
	if other.values["other-profile"].State != SandboxTerminated || other.values["foreign-env"].State != SandboxRunning {
		t.Fatalf("other-profile sandbox cleanup = %#v", other.values)
	}
	got, err := deps.hosts.GetHost(context.Background(), value.ID)
	if err != nil || got.State != host.StateDeleted {
		t.Fatalf("host after profile cleanup = %#v, %v", got, err)
	}
}

func hostServiceTestTestHostForCoverage(state host.State, id string) host.Host {
	value := hostServiceTestHost(state)
	value.ID = id
	return value
}

func TestCoverageCreateProbeFailureCleansSandboxAndDetachesWorkspace(t *testing.T) {
	service, deps := newHostServiceTest(t)
	core, logs := observer.New(zap.WarnLevel)
	service.logger = zap.New(core)
	value := hostServiceTestHost(host.StateCreating)
	sealHostServiceTestKey(t, deps.sealer, &value)
	deps.hosts.addHost(value)
	deps.workspaces.add(workspace.Workspace{
		ID: testHostWorkspaceID(value), OwnerID: value.OwnerID, State: workspace.StateReady,
	})
	deps.probe.err = errors.New("private AIO error detail")
	if err := service.AdvanceCreate(context.Background(), value.ID); err != nil {
		t.Fatalf("AdvanceCreate() after probe timeout = %v", err)
	}
	got, err := deps.hosts.GetHost(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := deps.workspaces.GetWorkspace(context.Background(), testHostWorkspaceID(value))
	if err != nil {
		t.Fatal(err)
	}
	if got.State != host.StateFailed || got.StateReason != "ready_timeout" || got.SandboxID != "" || ws.State != workspace.StateReady {
		t.Fatalf("host/workspace after readiness failure = %#v / %#v", got, ws)
	}
	if deps.sandbox.values["sandbox-1"].State != SandboxTerminated {
		t.Fatalf("sandbox after readiness failure = %#v", deps.sandbox.values["sandbox-1"])
	}
	if logs.FilterMessage("host readiness timed out").Len() != 1 ||
		logs.FilterField(zap.String("stage", "aio_health")).Len() != 1 {
		t.Fatalf("readiness diagnostics = %#v, want one safe AIO health stage", logs.All())
	}
	if _, leaked := logs.All()[0].ContextMap()["error"]; leaked {
		t.Fatalf("readiness log exposed probe error: %#v", logs.All()[0].ContextMap())
	}
}
