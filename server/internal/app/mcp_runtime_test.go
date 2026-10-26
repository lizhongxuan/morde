package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/runtimeclient"
)

type mcpRuntimeHostStore struct {
	HostStore
	value host.Host
}

func (s mcpRuntimeHostStore) GetHost(_ context.Context, hostID string) (host.Host, error) {
	if hostID != s.value.ID {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	return s.value, nil
}

type mcpRuntimeSharingAuthorizer struct {
	actorID    string
	hostID     string
	allowed    bool
	permission host.Permission
}

func (a *mcpRuntimeSharingAuthorizer) Authorize(_ context.Context, hostID, actorID string, permission host.Permission) (bool, error) {
	if hostID != a.hostID || actorID != a.actorID || permission != a.permission || !a.allowed {
		return false, domain.NewError(domain.CodeNotFound, "host not found")
	}
	return false, nil
}

func TestMCPComputerHostAuthorizerRequiresComputerScopeHostGrantAndLiveMCPShare(t *testing.T) {
	ownerID, actorID, hostID := "u7654321", "u1234567", "h1234567"
	hostValue := host.Host{ID: hostID, OwnerID: ownerID, State: host.StateRunning}
	sharing := &mcpRuntimeSharingAuthorizer{actorID: actorID, hostID: hostID, permission: host.PermissionMCPControl, allowed: true}
	authorizer, err := NewMCPComputerHostAuthorizer(mcpRuntimeHostStore{value: hostValue}, sharing)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{
		UserID: actorID, Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t1234567",
		HostIDs: []string{hostID}, Scopes: []identity.Scope{identity.ScopeComputerOperate},
	}
	value, err := authorizer.GetForComputer(context.Background(), principal, hostID, identity.ScopeComputerOperate)
	if err != nil || value.OwnerID != ownerID {
		t.Fatalf("authorized shared Host = %#v, err=%v", value, err)
	}

	sharing.allowed = false
	if _, err := authorizer.GetForComputer(context.Background(), principal, hostID, identity.ScopeComputerOperate); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("revoked MCP share error = %v", err)
	}
	sharing.allowed = true
	principal.HostIDs = nil
	if _, err := authorizer.GetForComputer(context.Background(), principal, hostID, identity.ScopeComputerOperate); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("missing Host allow-list error = %v", err)
	}
	principal.HostIDs = []string{hostID}
	if _, err := authorizer.GetForComputer(context.Background(), principal, hostID, identity.ScopeComputerObserve); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("missing key scope error = %v", err)
	}
	if _, err := authorizer.GetForComputer(context.Background(), principal, hostID, identity.ScopeTaskSubmit); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("non-computer scope error = %v", err)
	}
}

func TestMCPSharedMemberCanReadApplicationStatusWithObserveGrant(t *testing.T) {
	runtime, hosts, manager, server := newRuntimeControlTest(t, 0, 0, runtimeclient.ModeBlocked, host.ControlAI, "")
	defer func() { _ = manager.Close() }()
	defer server.Close()

	const ownerID, actorID, hostID = "u7654321", "u1234567", "h1234567"
	sharing := &mcpRuntimeSharingAuthorizer{actorID: actorID, hostID: hostID, permission: host.PermissionMCPControl, allowed: true}
	hostValue, err := hosts.GetHost(context.Background(), hostID)
	if err != nil {
		t.Fatal(err)
	}
	hostValue.OwnerID = ownerID
	hosts.addHost(hostValue)
	authorizer, err := NewMCPComputerHostAuthorizer(hosts, sharing)
	if err != nil {
		t.Fatal(err)
	}
	executor := &MCPRuntimeToolExecutor{hosts: authorizer, runtime: runtime}
	principal := identity.Principal{
		UserID: actorID, Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t1234567",
		HostIDs: []string{hostID}, Scopes: []identity.Scope{identity.ScopeComputerObserve},
	}

	status, err := executor.Applications(context.Background(), principal, hostID)
	if err != nil {
		t.Fatalf("shared member application status: %v", err)
	}
	if status.HostID != hostID || len(status.Items) != 2 || status.Items[0].ID != "codex" {
		t.Fatalf("shared member application status = %#v", status)
	}

	sharing.allowed = false
	if _, err := executor.Applications(context.Background(), principal, hostID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("revoked shared member application status error = %v", err)
	}
}

func TestAuthorizeMCPRuntimeActionRevalidatesActorKeyAndOwner(t *testing.T) {
	service, users, hosts, _, tokens, _ := newCredentialFixture(t)
	ownerID, actorID, hostID, tokenID := "u7654321", "u1234567", "h1234567", "t1234567"
	hosts.values = append(hosts.values, host.Host{ID: hostID, OwnerID: ownerID, State: host.StateRunning})
	tokens.values[tokenID] = identity.Token{
		ID: tokenID, OwnerID: actorID, Kind: identity.KindAPIKey, HostIDs: []string{hostID},
		Scopes: []identity.Scope{identity.ScopeComputerOperate}, ExpiresAt: time.Now().Add(time.Hour),
	}
	approved := []identity.Scope{identity.ScopeComputerOperate}
	check := func() error {
		return service.AuthorizeMCPRuntimeAction(context.Background(), ownerID, actorID, tokenID, hostID, approved, identity.ScopeComputerOperate)
	}
	if err := check(); err != nil {
		t.Fatalf("valid member key: %v", err)
	}

	if err := service.AuthorizeMCPRuntimeAction(context.Background(), ownerID, actorID, tokenID, hostID, nil, identity.ScopeComputerOperate); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("scope outside key's original grant = %v", err)
	}
	key := tokens.values[tokenID]
	key.Scopes = nil
	tokens.values[tokenID] = key
	if err := check(); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("scope removed from current key = %v", err)
	}
	key.Scopes = approved
	key.HostIDs = nil
	tokens.values[tokenID] = key
	if err := check(); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("Host removed from key allow-list = %v", err)
	}
	key.HostIDs = []string{hostID}
	now := time.Now()
	key.RevokedAt = &now
	tokens.values[tokenID] = key
	if err := check(); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked member key = %v", err)
	}
	key.RevokedAt = nil
	key.ExpiresAt = time.Now().Add(-time.Second)
	tokens.values[tokenID] = key
	if err := check(); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("expired member key = %v", err)
	}
	key.ExpiresAt = time.Now().Add(time.Hour)
	key.OwnerID = ownerID
	tokens.values[tokenID] = key
	if err := check(); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("key actor does not match member = %v", err)
	}
	key.OwnerID = actorID
	tokens.values[tokenID] = key
	if err := service.AuthorizeMCPRuntimeAction(context.Background(), actorID, actorID, tokenID, hostID, approved, identity.ScopeComputerOperate); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("Host owner changed = %v", err)
	}
	users.values[actorID] = identity.User{ID: actorID, Disabled: true}
	if err := check(); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("disabled member = %v", err)
	}
}

type mcpRuntimeLeaseStore struct {
	MCPRuntimeOperationStore
	renewals atomic.Int32
}

type mcpRuntimeBlockingHostAuthorizer struct {
	started  chan struct{}
	finished chan error
	calls    atomic.Int32
}

func (a *mcpRuntimeBlockingHostAuthorizer) GetForComputer(ctx context.Context, _ identity.Principal, _ string, _ identity.Scope) (host.Host, error) {
	if a.calls.Add(1) == 1 {
		close(a.started)
	}
	<-ctx.Done()
	a.finished <- ctx.Err()
	return host.Host{}, ctx.Err()
}

func (s *mcpRuntimeLeaseStore) RenewMCPRuntimeOperationLease(context.Context, MCPRuntimeOperationKey, string, AgentHostFence, int64, int64, time.Duration) (MCPRuntimeOperation, error) {
	s.renewals.Add(1)
	return MCPRuntimeOperation{}, nil
}

func TestMCPRuntimeLeaseCancelsWhenShareOrKeyIsRevokedOrExpired(t *testing.T) {
	const ownerID, actorID, hostID, tokenID = "u7654321", "u1234567", "h1234567", "t1234567"
	for _, scenario := range []string{"share revoked", "key revoked", "key expired"} {
		t.Run(scenario, func(t *testing.T) {
			service, _, hosts, _, tokens, _ := newCredentialFixture(t)
			hostValue := host.Host{ID: hostID, OwnerID: ownerID, State: host.StateRunning}
			hosts.values = append(hosts.values, hostValue)
			token := identity.Token{
				ID: tokenID, OwnerID: actorID, Kind: identity.KindAPIKey, HostIDs: []string{hostID},
				Scopes: []identity.Scope{identity.ScopeComputerOperate}, ExpiresAt: time.Now().Add(time.Hour),
			}
			sharing := &mcpRuntimeSharingAuthorizer{actorID: actorID, hostID: hostID, allowed: scenario != "share revoked", permission: host.PermissionMCPControl}
			switch scenario {
			case "key revoked":
				revokedAt := time.Now()
				token.RevokedAt = &revokedAt
			case "key expired":
				token.ExpiresAt = time.Now().Add(-time.Second)
			}
			tokens.values[tokenID] = token
			hostAuthorizer, err := NewMCPComputerHostAuthorizer(mcpRuntimeHostStore{value: hostValue}, sharing)
			if err != nil {
				t.Fatal(err)
			}
			store := &mcpRuntimeLeaseStore{}
			executor := &MCPRuntimeToolExecutor{store: store, hosts: hostAuthorizer, authorizer: service}
			principal := identity.Principal{
				UserID: actorID, Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: tokenID,
				HostIDs: []string{hostID}, Scopes: []identity.Scope{identity.ScopeComputerOperate},
			}
			operation := MCPRuntimeOperation{
				Key:           MCPRuntimeOperationKey{OwnerID: ownerID, ActorUserID: actorID, APIKeyID: tokenID, SessionID: "mcp-lease-session", HostID: hostID, IdempotencyKey: "mcp-lease-operation"},
				RuntimeTaskID: "q1234567", Fence: AgentHostFence{RuntimeGeneration: 1, RuntimeInstanceID: "lease-test", ControlVersion: 0, AIAllowed: true}, LeaseVersion: 1,
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			leaseLost := make(chan struct{}, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				executor.keepMCPRuntimeLease(ctx, cancel, leaseLost, &RuntimeSession{}, operation, 1, principal, identity.ScopeComputerOperate)
			}()
			select {
			case <-leaseLost:
			case <-time.After(mcpRuntimeAuthorizationCheckInterval + 2*time.Second):
				t.Fatal("in-flight MCP lease was not revoked after authorization changed")
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("lease cancellation did not cancel the runtime context")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("lease keeper did not stop after revocation")
			}
			if got := store.renewals.Load(); got != 0 {
				t.Fatalf("renewed lease %d times after authorization was revoked", got)
			}
		})
	}
}

func TestMCPRuntimeLeaseStopDoesNotTreatContextCancellationAsRevocation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	leaseLost := make(chan struct{}, 1)
	executor := &MCPRuntimeToolExecutor{hosts: mcpRuntimeAlwaysDeniedHostAuthorizer{}}
	operation := MCPRuntimeOperation{Key: MCPRuntimeOperationKey{OwnerID: "u7654321", HostID: "h1234567"}}

	if executor.mcpRuntimeLeaseAuthorized(ctx, cancel, leaseLost, identity.Principal{}, operation, identity.ScopeComputerOperate) {
		t.Fatal("canceled authorization check succeeded")
	}
	if len(leaseLost) != 0 {
		t.Fatal("stopping the lease keeper was reported as authorization revocation")
	}
}

func TestMCPRuntimeLeaseCancelsWhenAuthorizationLookupTimesOut(t *testing.T) {
	const ownerID, actorID, hostID, tokenID = "u7654321", "u1234567", "h1234567", "t1234567"
	store := &mcpRuntimeLeaseStore{}
	hosts := &mcpRuntimeBlockingHostAuthorizer{started: make(chan struct{}), finished: make(chan error, 1)}
	executor := &MCPRuntimeToolExecutor{store: store, hosts: hosts}
	principal := identity.Principal{UserID: actorID, Kind: identity.KindAPIKey, TokenID: tokenID, HostIDs: []string{hostID}, Scopes: []identity.Scope{identity.ScopeComputerOperate}}
	operation := MCPRuntimeOperation{Key: MCPRuntimeOperationKey{OwnerID: ownerID, ActorUserID: actorID, APIKeyID: tokenID, HostID: hostID}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leaseLost := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		executor.keepMCPRuntimeLease(ctx, cancel, leaseLost, &RuntimeSession{}, operation, 1, principal, identity.ScopeComputerOperate)
	}()

	select {
	case <-hosts.started:
	case <-time.After(mcpRuntimeAuthorizationCheckInterval + time.Second):
		t.Fatal("authorization lookup did not start")
	}
	lookupStartedAt := time.Now()
	select {
	case <-ctx.Done():
	case <-time.After(mcpRuntimeAuthorizationTimeout + time.Second):
		t.Fatal("authorization timeout did not cancel the in-flight runtime operation")
	}
	if elapsed := time.Since(lookupStartedAt); elapsed > mcpRuntimeAuthorizationTimeout+time.Second {
		t.Fatalf("authorization timeout canceled after %s", elapsed)
	}
	select {
	case err := <-hosts.finished:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("authorization lookup error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("authorization lookup did not return after its deadline")
	}
	select {
	case <-leaseLost:
	case <-time.After(time.Second):
		t.Fatal("authorization timeout was not reported as a lost lease")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("lease keeper continued after the authorization timeout")
	}
	if got := store.renewals.Load(); got != 0 {
		t.Fatalf("renewed lease %d times after the authorization timeout", got)
	}
}

type mcpRuntimeAlwaysDeniedHostAuthorizer struct{}

func (mcpRuntimeAlwaysDeniedHostAuthorizer) GetForComputer(context.Context, identity.Principal, string, identity.Scope) (host.Host, error) {
	return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
}
