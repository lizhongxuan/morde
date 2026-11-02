package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type credentialUsers struct {
	UserStore
	values map[string]identity.User
}

type credentialRevocationUsers struct {
	*credentialUsers
	events *[]string
}

func (s *credentialRevocationUsers) UpdateUserWithAudit(ctx context.Context, actorID string, expected identity.AccountStatus, user identity.User, entry audit.Entry, disableRelay func(context.Context) error) (identity.User, error) {
	if disableRelay != nil {
		if err := disableRelay(ctx); err != nil {
			return identity.User{}, err
		}
	}
	updated, err := s.credentialUsers.UpdateUserWithAudit(ctx, actorID, expected, user, entry, nil)
	if err == nil {
		*s.events = append(*s.events, "commit:"+user.ID)
	}
	return updated, err
}

func (s *credentialUsers) GetUserByID(_ context.Context, id string) (identity.User, error) {
	user, ok := s.values[id]
	if !ok {
		return identity.User{}, domain.NewError(domain.CodeNotFound, "user not found")
	}
	return user, nil
}

func (s *credentialUsers) GetUserByName(_ context.Context, name string) (identity.User, error) {
	for _, user := range s.values {
		if user.Name == name {
			return user, nil
		}
	}
	return identity.User{}, domain.NewError(domain.CodeNotFound, "user not found")
}

func (s *credentialUsers) ListUsersByStatus(_ context.Context, status *identity.AccountStatus) ([]identity.User, error) {
	values := make([]identity.User, 0, len(s.values))
	for _, user := range s.values {
		if status != nil && user.Status != *status {
			continue
		}
		values = append(values, user)
	}
	return values, nil
}

func (s *credentialUsers) CreateUser(_ context.Context, user identity.User) (identity.User, error) {
	for _, existing := range s.values {
		if existing.Name == user.Name {
			return identity.User{}, uniqueViolationError{}
		}
	}
	if user.CreatedAt.IsZero() {
		user.CreatedAt = time.Now().UTC()
	}
	s.values[user.ID] = user
	return user, nil
}

func (s *credentialUsers) UpdateUserWithAudit(_ context.Context, _ string, expected identity.AccountStatus, user identity.User, _ audit.Entry, _ func(context.Context) error) (identity.User, error) {
	current, ok := s.values[user.ID]
	if !ok {
		return identity.User{}, domain.NewError(domain.CodeNotFound, "user not found")
	}
	if current.Status != expected {
		return identity.User{}, domain.NewError(domain.CodeConflict, "account status changed")
	}
	user.UpdatedAt = time.Now().UTC()
	s.values[user.ID] = user
	return user, nil
}

type uniqueViolationError struct{}

func (uniqueViolationError) Error() string    { return "unique constraint" }
func (uniqueViolationError) SQLState() string { return "23505" }

type credentialHosts struct {
	HostStore
	mu        sync.Mutex
	values    []host.Host
	ensureErr error
	listErr   error
}

func (s *credentialHosts) EnsureDefaultStopped(_ context.Context, user identity.User) (host.Host, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ensureErr != nil {
		return host.Host{}, false, s.ensureErr
	}
	for _, value := range s.values {
		if value.OwnerID == user.ID && value.DeletedAt == nil && value.State != host.StateDeleted {
			return value, false, nil
		}
	}
	if user.QuotaHosts == 0 {
		return host.Host{}, false, nil
	}
	value := host.Host{ID: "h1234567", OwnerID: user.ID, State: host.StateStopped, CPUMilli: 1000, MemMiB: 2048}
	s.values = append(s.values, value)
	return value, true, nil
}

func TestCredentialLoginEnsuresOneStoppedHostBeforeIssuingSession(t *testing.T) {
	service, _, hosts, _, tokens, _ := newCredentialFixture(t)
	for _, ip := range []string{"127.0.0.1", "127.0.0.2"} {
		if _, err := service.Login(context.Background(), "alice", "correct horse", ip); err != nil {
			t.Fatalf("Login() error = %v", err)
		}
	}
	if len(hosts.values) != 1 || hosts.values[0].State != host.StateStopped {
		t.Fatalf("login default hosts = %#v", hosts.values)
	}
	hosts.ensureErr = errors.New("storage unavailable")
	before := len(tokens.values)
	if _, err := service.Login(context.Background(), "alice", "correct horse", "127.0.0.3"); err == nil {
		t.Fatal("Login() succeeded when default host storage failed")
	}
	if len(tokens.values) != before {
		t.Fatal("Login() issued a session after default host storage failed")
	}
}

func (s *credentialHosts) GetHost(_ context.Context, id string) (host.Host, error) {
	for _, value := range s.values {
		if value.ID == id {
			return value, nil
		}
	}
	return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
}

func (s *credentialHosts) ListHostsByOwner(_ context.Context, ownerID string) ([]host.Host, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	values := make([]host.Host, 0)
	for _, value := range s.values {
		if value.OwnerID == ownerID {
			values = append(values, value)
		}
	}
	return values, nil
}

type credentialRevocationWorkspaceSync struct {
	events       *[]string
	reconcileErr error
}

func (s *credentialRevocationWorkspaceSync) DisableWorkspaceSyncNodeRelay(_ context.Context, ownerID string) error {
	*s.events = append(*s.events, "disable:"+ownerID)
	return nil
}

func (s *credentialRevocationWorkspaceSync) ReconcileWorkspaceSyncNodeOwner(_ context.Context, ownerID string) error {
	*s.events = append(*s.events, "reconcile:"+ownerID)
	return s.reconcileErr
}

type credentialWorkspaces struct {
	WorkspaceStore
	values map[string]workspace.Workspace
}

func (s *credentialWorkspaces) GetWorkspace(_ context.Context, id string) (workspace.Workspace, error) {
	value, ok := s.values[id]
	if !ok {
		return workspace.Workspace{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return value, nil
}

type credentialTokens struct {
	TokenStore
	values         map[string]identity.Token
	consumeReceipt *Ticket
	err            error
	clientActive   bool
}

func (s *credentialTokens) ActiveClientInstallation(_ context.Context, installationID, ownerID string) (bool, error) {
	return s.clientActive && installationID == "i1234567" && ownerID == "u1234567", nil
}

func (s *credentialTokens) GetTokenByID(_ context.Context, id string) (identity.Token, error) {
	if s.err != nil {
		return identity.Token{}, s.err
	}
	value, ok := s.values[id]
	if !ok {
		return identity.Token{}, domain.NewError(domain.CodeNotFound, "token not found")
	}
	return value, nil
}

func (s *credentialTokens) ListTokensByOwner(_ context.Context, ownerID string) ([]identity.Token, error) {
	values := make([]identity.Token, 0)
	for _, token := range s.values {
		if token.OwnerID == ownerID {
			values = append(values, token)
		}
	}
	return values, nil
}

func (s *credentialTokens) CreateToken(_ context.Context, value identity.Token) (identity.Token, error) {
	if _, exists := s.values[value.ID]; exists {
		return identity.Token{}, uniqueViolationError{}
	}
	value.CreatedAt = time.Now().UTC()
	s.values[value.ID] = value
	return value, nil
}

func (s *credentialTokens) RevokeToken(_ context.Context, id, ownerID string) error {
	value, ok := s.values[id]
	if !ok || value.OwnerID != ownerID {
		return domain.NewError(domain.CodeNotFound, "token not found")
	}
	now := time.Now().UTC()
	value.RevokedAt = &now
	s.values[id] = value
	return nil
}

func (s *credentialTokens) ConsumeTicket(_ context.Context, id string) (Ticket, error) {
	value, ok := s.values[id]
	if !ok || value.Kind != identity.KindTicket || value.UsedAt != nil || value.RevokedAt != nil || !value.ExpiresAt.After(time.Now()) {
		return Ticket{}, identity.ErrInvalidCredential
	}
	now := time.Now().UTC()
	value.UsedAt = &now
	s.values[id] = value
	if s.consumeReceipt != nil {
		return *s.consumeReceipt, nil
	}
	actorUserID := value.ActorUserID
	if actorUserID == "" {
		actorUserID = value.OwnerID
	}
	return Ticket{TokenID: id, UserID: value.OwnerID, ActorUserID: actorUserID, HostID: value.HostID, PreviewPort: value.PreviewPort}, nil
}

type credentialAudit struct {
	entries []audit.Entry
	err     error
}

func (s *credentialAudit) Write(_ context.Context, entry audit.Entry) error {
	if s.err != nil {
		return s.err
	}
	s.entries = append(s.entries, entry)
	return nil
}

func newCredentialFixture(t *testing.T) (*CredentialService, *credentialUsers, *credentialHosts, *credentialWorkspaces, *credentialTokens, *credentialAudit) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	users := &credentialUsers{values: map[string]identity.User{"u1234567": {
		ID: "u1234567", Name: "alice", PasswordHash: string(hash), Role: identity.RoleUser, Status: identity.StatusApproved,
		QuotaHosts: 1, QuotaCPUMilli: 4000, QuotaMemMiB: 8192, DefaultHostCPUMilli: 1000, DefaultHostMemMiB: 2048,
	}}}
	hosts := &credentialHosts{}
	workspaces := &credentialWorkspaces{values: map[string]workspace.Workspace{}}
	tokens := &credentialTokens{values: map[string]identity.Token{}}
	auditor := &credentialAudit{}
	service, err := NewCredentialService(users, hosts, tokens, auditor, CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 2, LoginWindow: time.Minute,
	}, hosts)
	if err != nil {
		t.Fatal(err)
	}
	return service, users, hosts, workspaces, tokens, auditor
}

func TestUserServiceClosesOwnerHubAfterStatusOrRoleCommit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command UpdateUserCommand
	}{
		{name: "status", command: UpdateUserCommand{ExpectedStatus: identity.StatusApproved, Status: statusPointer(identity.StatusDisabled)}},
		{name: "role", command: UpdateUserCommand{ExpectedStatus: identity.StatusApproved, Role: rolePointer(identity.RoleAdmin)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []string{}
			member := identity.User{ID: "u1234567", Name: "alice", Role: identity.RoleUser, Status: identity.StatusApproved,
				QuotaHosts: 1, QuotaCPUMilli: 4000, QuotaMemMiB: 8192, DefaultHostCPUMilli: 1000, DefaultHostMemMiB: 2048}
			users := &credentialRevocationUsers{
				credentialUsers: &credentialUsers{values: map[string]identity.User{member.ID: member}}, events: &events,
			}
			hosts := &credentialHosts{}
			hub := &clientRevocationHub{events: &events}
			controller := &credentialRevocationWorkspaceSync{events: &events, reconcileErr: errors.New("reconcile failed")}
			service, err := NewUserServiceWithHub(users, hosts, hub, controller)
			if err != nil {
				t.Fatal(err)
			}
			admin := identity.Principal{UserID: "u7654321", Role: identity.RoleSuperAdmin, Kind: identity.KindSession, TokenID: "t7654321"}

			_, err = service.Update(context.Background(), admin, member.ID, tc.command)
			if domain.CodeOf(err) != domain.CodeUpstreamError {
				t.Fatalf("Update() error = %v, want workspace sync reconciliation failure", err)
			}
			if got, want := strings.Join(events, ","), "disable:u1234567,commit:u1234567,hub-owner-close:u1234567,reconcile:u1234567"; got != want {
				t.Fatalf("update events = %q, want %q", got, want)
			}
		})
	}
}

func TestCredentialPasswordChangeClosesHubBeforeFailedReconcile(t *testing.T) {
	events := []string{}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte("old password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	admin := identity.User{ID: "u1234567", Name: "admin", PasswordHash: string(passwordHash), Role: identity.RoleSuperAdmin, Status: identity.StatusApproved}
	users := &credentialRevocationUsers{
		credentialUsers: &credentialUsers{values: map[string]identity.User{admin.ID: admin}}, events: &events,
	}
	hosts := &credentialHosts{}
	tokens := &credentialTokens{values: map[string]identity.Token{}}
	auditor := &credentialAudit{}
	controller := &credentialRevocationWorkspaceSync{events: &events, reconcileErr: errors.New("reconcile failed")}
	hub := &clientRevocationHub{events: &events}
	service, err := NewCredentialServiceWithHub(users, hosts, tokens, auditor, CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 2, LoginWindow: time.Minute,
	}, hosts, hub, controller)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: admin.ID, Role: identity.RoleSuperAdmin, Kind: identity.KindSession, TokenID: "t1234567"}

	err = service.ChangePassword(context.Background(), principal, "old password", "new secure password")
	if domain.CodeOf(err) != domain.CodeUpstreamError {
		t.Fatalf("ChangePassword() error = %v, want reconcile failure", err)
	}
	if got, want := strings.Join(events, ","), "disable:u1234567,commit:u1234567,hub-owner-close:u1234567,reconcile:u1234567"; got != want {
		t.Fatalf("password change events = %q, want %q", got, want)
	}
	if bcrypt.CompareHashAndPassword([]byte(users.values[admin.ID].PasswordHash), []byte("new secure password")) != nil {
		t.Fatal("password change was not committed before reconciliation failed")
	}
}

func statusPointer(value identity.AccountStatus) *identity.AccountStatus { return &value }

func rolePointer(value identity.Role) *identity.Role { return &value }

func TestCredentialServiceLoginVerifyLogoutAndRateLimit(t *testing.T) {
	service, _, _, _, tokens, auditor := newCredentialFixture(t)
	result, err := service.Login(context.Background(), " alice ", "correct horse", "127.0.0.1")
	if err != nil || result.Credential == "" || result.User.Name != "alice" {
		t.Fatalf("Login() = %#v, %v", result, err)
	}
	principal, err := service.VerifySession(context.Background(), result.Credential)
	if err != nil || principal.UserID != "u1234567" || principal.Kind != identity.KindSession {
		t.Fatalf("VerifySession() = %#v, %v", principal, err)
	}
	if err := service.Logout(context.Background(), principal); err != nil {
		t.Fatal(err)
	}
	if _, err := service.VerifySession(context.Background(), result.Credential); !errors.Is(err, identity.ErrInvalidCredential) {
		t.Fatalf("revoked session verification error = %v", err)
	}
	if _, err := service.Login(context.Background(), "alice", "wrong", "127.0.0.2"); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("bad password error = %v", err)
	}
	if _, err := service.Login(context.Background(), "alice", "wrong", "127.0.0.2"); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("second bad password error = %v", err)
	}
	if _, err := service.Login(context.Background(), "alice", "wrong", "127.0.0.2"); domain.CodeOf(err) != domain.CodeRateLimited {
		t.Fatalf("rate limit error = %v", err)
	}
	if len(tokens.values) != 1 || len(auditor.entries) != 4 {
		t.Fatalf("tokens/audit = %d / %d", len(tokens.values), len(auditor.entries))
	}
}

func TestCredentialServiceVerifiesRevocableDynamicWorkspaceDevice(t *testing.T) {
	service, _, _, _, tokens, _ := newCredentialFixture(t)
	raw, secretHash, err := identity.NewCredential(identity.KindDevice, "tdevice1")
	if err != nil {
		t.Fatal(err)
	}
	tokens.values["tdevice1"] = identity.Token{
		ID: "tdevice1", OwnerID: "u1234567", Kind: identity.KindDevice, Name: "PC",
		SecretHash: secretHash, Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if _, err := service.VerifyDevice(context.Background(), raw); !errors.Is(err, identity.ErrInvalidCredential) {
		t.Fatalf("unlinked device token was accepted: %v", err)
	}
	token := tokens.values["tdevice1"]
	token.ClientInstallationID = "i1234567"
	tokens.values[token.ID] = token
	tokens.clientActive = true
	principal, err := service.VerifyDevice(context.Background(), raw)
	if err != nil || principal.Kind != identity.KindDevice || principal.Role != identity.RoleUser ||
		!principal.AllowsWorkspace("w1234567") || !principal.AllowsWorkspace("w7654321") || principal.HasScope(identity.ScopeComputerOperate) {
		t.Fatalf("VerifyDevice() principal=%#v err=%v", principal, err)
	}
	if _, err := service.VerifyAPIKey(context.Background(), raw); !errors.Is(err, identity.ErrInvalidCredential) {
		t.Fatalf("VerifyAPIKey(device) error = %v", err)
	}
	token = tokens.values["tdevice1"]
	token.WorkspaceIDs = []string{"w1234567"}
	tokens.values[token.ID] = token
	if _, err := service.VerifyDevice(context.Background(), raw); !errors.Is(err, identity.ErrInvalidCredential) {
		t.Fatalf("VerifyDevice(static grants) err=%v", err)
	}
	token.WorkspaceIDs = nil
	token.ExpiresAt = time.Now().Add(-time.Second)
	tokens.values[token.ID] = token
	if _, err := service.VerifyDevice(context.Background(), raw); !errors.Is(err, identity.ErrInvalidCredential) {
		t.Fatalf("VerifyDevice(expired) error = %v", err)
	}
	token.ExpiresAt = time.Now().Add(time.Hour)
	tokens.values[token.ID] = token
	if err := tokens.RevokeToken(context.Background(), "tdevice1", "u1234567"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.VerifyDevice(context.Background(), raw); !errors.Is(err, identity.ErrInvalidCredential) {
		t.Fatalf("VerifyDevice(revoked) error = %v", err)
	}
}

func TestClientCredentialRevocationStopsTaskAuthorization(t *testing.T) {
	service, users, hosts, _, tokens, _ := newCredentialFixture(t)
	user := users.values["u1234567"]
	user.Role = identity.RoleAdmin
	users.values[user.ID] = user
	hosts.values = []host.Host{{ID: "h1234567", OwnerID: user.ID, State: host.StateRunning}}
	raw, hash, err := identity.NewCredential(identity.KindClient, "tclient1")
	if err != nil {
		t.Fatal(err)
	}
	tokens.values["tclient1"] = identity.Token{ID: "tclient1", OwnerID: user.ID, Kind: identity.KindClient,
		ClientInstallationID: "i1234567", SecretHash: hash, ExpiresAt: time.Now().Add(time.Hour)}
	tokens.clientActive = true
	principal, err := service.VerifyClient(context.Background(), raw)
	if err != nil || principal.Kind != identity.KindClient || principal.Role != identity.RoleUser || principal.ClientInstallationID != "i1234567" {
		t.Fatalf("VerifyClient() = %#v, %v", principal, err)
	}
	if err := service.AuthorizeTaskAction(context.Background(), user.ID, identity.KindClient, "tclient1", "h1234567", nil, identity.ScopeTaskSubmit); err != nil {
		t.Fatalf("active client task authorization: %v", err)
	}
	tokens.clientActive = false
	if err := service.AuthorizeTaskAction(context.Background(), user.ID, identity.KindClient, "tclient1", "h1234567", nil, identity.ScopeTaskSubmit); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked client task authorization = %v", err)
	}
	if _, err := service.VerifyClient(context.Background(), raw); !errors.Is(err, identity.ErrInvalidCredential) {
		t.Fatalf("revoked client verification = %v", err)
	}
}

func TestCredentialServiceTicketConsumesOnceAndPropagatesStoreErrors(t *testing.T) {
	service, _, _, _, tokens, _ := newCredentialFixture(t)
	_, raw, err := issueCredentialWithPreviewPort(context.Background(), tokens, "u1234567", identity.KindTicket, "h1234567", "", time.Now().Add(time.Minute), nil, nil, 5173)
	if err != nil {
		t.Fatal(err)
	}
	if ticket, err := service.ConsumeTicket(context.Background(), raw); err != nil || ticket.HostID != "h1234567" || ticket.PreviewPort != 5173 {
		t.Fatalf("ConsumeTicket() = %#v, %v", ticket, err)
	}
	if _, err := service.ConsumeTicket(context.Background(), raw); !errors.Is(err, identity.ErrInvalidCredential) {
		t.Fatalf("second ConsumeTicket() error = %v", err)
	}
}

func TestCredentialServiceRejectsMismatchedTicketReceiptAudience(t *testing.T) {
	service, _, _, _, tokens, _ := newCredentialFixture(t)
	token, raw, err := issueCredentialWithPreviewPort(context.Background(), tokens, "u1234567", identity.KindTicket, "h1234567", "", time.Now().Add(time.Minute), nil, nil, 5173)
	if err != nil {
		t.Fatal(err)
	}
	tokens.consumeReceipt = &Ticket{TokenID: token.ID, UserID: token.OwnerID, HostID: token.HostID, PreviewPort: 5174}
	if _, err := service.ConsumeTicket(context.Background(), raw); !errors.Is(err, identity.ErrInvalidCredential) {
		t.Fatalf("ConsumeTicket(mismatched preview port) error = %v, want invalid credential", err)
	}
}

func TestRegistrationHashesPasswordAndCreatesPendingUser(t *testing.T) {
	users := &credentialUsers{values: map[string]identity.User{}}
	service, err := NewUserService(users, &credentialHosts{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Register(context.Background(), "  bob ", "safe password", "192.0.2.1")
	if err != nil || created.Name != "bob" || created.Role != identity.RoleUser || created.Status != identity.StatusPending ||
		created.Quota != (ResourceUsage{Hosts: 1, CPUMilli: 4000, MemMiB: 8192}) ||
		created.DefaultHostResources != (HostResources{CPUMilli: 1000, MemMiB: 2048}) {
		t.Fatalf("Register() = %#v, %v", created, err)
	}
	stored := users.values[created.ID]
	if stored.PasswordHash == "safe password" || bcrypt.CompareHashAndPassword([]byte(stored.PasswordHash), []byte("safe password")) != nil {
		t.Fatal("user password was not bcrypt hashed")
	}
	if _, err := service.Register(context.Background(), "bob", "safe password", "192.0.2.1"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("duplicate user error = %v", err)
	}
	if _, err := service.Register(context.Background(), "long", string(make([]byte, 73)), "192.0.2.1"); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("long password error = %v", err)
	}
}

func TestRegistrationCreatesOnlyOrdinaryUsersAndLimitsAttempts(t *testing.T) {
	users := &credentialUsers{values: map[string]identity.User{}}
	service, err := NewUserService(users, &credentialHosts{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Register(context.Background(), "ab", "password123", "192.0.2.1"); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("short username error = %v", err)
	}
	for i := 0; i < 5; i++ {
		name := "new-user-" + string(rune('a'+i))
		value, err := service.Register(context.Background(), name, "password123", "192.0.2.1")
		if err != nil || value.Role != identity.RoleUser || value.Status != identity.StatusPending {
			t.Fatalf("Register() = %#v, %v", value, err)
		}
	}
	if _, err := service.Register(context.Background(), "new-user-z", "password123", "192.0.2.1"); domain.CodeOf(err) != domain.CodeRateLimited {
		t.Fatalf("sixth registration error = %v", err)
	}
	if _, err := service.Register(context.Background(), "AdMiN", "password123", "192.0.2.2"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("reserved admin name error = %v", err)
	}
}

func TestLoginLimiterUsesWindowClock(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := loginLimiter{attempts: map[string]loginAttempt{}}
	if _, ok := limiter.allow("key", now, 1, time.Minute); !ok {
		t.Fatal("first attempt denied")
	}
	if retry, ok := limiter.allow("key", now.Add(17*time.Second), 1, time.Minute); ok || retry != 43*time.Second {
		t.Fatalf("retry = %v, allowed=%v", retry, ok)
	}
}
