package app

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

func TestOnboardingConcurrentDefaultHostCreatesOnlyOneHost(t *testing.T) {
	service, hosts, _, _ := newOnboardingTestService(t)
	const callers = 12
	var wait sync.WaitGroup
	errs := make(chan error, callers)
	results := make(chan DefaultHostResult, callers)
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := service.EnsureDefaultHost(context.Background(), onboardingTestPrincipal(), DefaultHostCommand{})
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}
	wait.Wait()
	close(errs)
	close(results)
	for err := range errs {
		t.Errorf("EnsureDefaultHost() error = %v", err)
	}
	var created int
	for result := range results {
		if result.Created {
			created++
		}
	}
	if created != 1 || hosts.createCalls != 1 || len(hosts.values) != 1 || hosts.values[0].State != host.StateStopped {
		t.Fatalf("concurrent first use: created results=%d create calls=%d hosts=%d", created, hosts.createCalls, len(hosts.values))
	}
}

func TestOnboardingMultipleHostsRequireExplicitSelection(t *testing.T) {
	service, hosts, _, _ := newOnboardingTestService(t)
	hosts.values = []host.Host{
		{ID: "h1234567", OwnerID: "u1234567", Name: "one", State: host.StateRunning},
		{ID: "h7654321", OwnerID: "u1234567", Name: "two", State: host.StateStopped},
	}

	result, err := service.EnsureDefaultHost(context.Background(), onboardingTestPrincipal(), DefaultHostCommand{})
	if err != nil {
		t.Fatalf("EnsureDefaultHost() error = %v", err)
	}
	if result.Snapshot.ComputerStatus != ComputerSelectionRequired || result.Host != nil || hosts.createCalls != 0 || hosts.restoreCalls != 0 {
		t.Fatalf("multiple host default result = %#v, create=%d restore=%d", result, hosts.createCalls, hosts.restoreCalls)
	}
	if result.Snapshot.Preferences.DefaultHostID != "" {
		t.Fatalf("default host was selected without user choice: %#v", result.Snapshot.Preferences)
	}
}

func TestOnboardingKeepsStoppedDefaultOff(t *testing.T) {
	service, hosts, preferences, _ := newOnboardingTestService(t)
	stopped := host.Host{ID: "h1234567", OwnerID: "u1234567", Name: "my computer", State: host.StateStopped, RuntimeGeneration: 2}
	hosts.values = []host.Host{stopped}
	preferences.value = UserPreferences{OwnerID: "u1234567", DefaultHostID: stopped.ID}
	result, err := service.EnsureDefaultHost(context.Background(), onboardingTestPrincipal(), DefaultHostCommand{})
	if err != nil {
		t.Fatalf("EnsureDefaultHost() error = %v", err)
	}
	if hosts.restoreCalls != 0 || result.Host == nil || result.Host.ID != stopped.ID ||
		result.Snapshot.ComputerStatus != ComputerStopped || preferences.value.LastHostIssue != "" {
		t.Fatalf("stopped default result = %#v; restore calls=%d preferences=%#v", result, hosts.restoreCalls, preferences.value)
	}
	status, err := service.Status(context.Background(), onboardingTestPrincipal())
	if err != nil || status.ComputerStatus != ComputerStopped {
		t.Fatalf("Status() after ensuring stopped default = %#v, %v", status, err)
	}
}

func TestOnboardingStatusDistinguishesCreationAndRestoreFailure(t *testing.T) {
	service, hosts, preferences, _ := newOnboardingTestService(t)
	failed := host.Host{ID: "h1234567", OwnerID: "u1234567", State: host.StateFailed, RuntimeGeneration: 1, StateReason: "private detail"}
	hosts.values = []host.Host{failed}
	preferences.value = UserPreferences{OwnerID: "u1234567", DefaultHostID: failed.ID}

	status, err := service.Status(context.Background(), onboardingTestPrincipal())
	if err != nil || status.ComputerStatus != ComputerCreateFailed {
		t.Fatalf("initial failed host status = %#v, %v", status, err)
	}
	hosts.values[0].RuntimeGeneration = 2
	status, err = service.Status(context.Background(), onboardingTestPrincipal())
	if err != nil || status.ComputerStatus != ComputerRestoreFailed {
		t.Fatalf("restore failed host status = %#v, %v", status, err)
	}
}

func TestOnboardingModelSelectionRequiresConfiguredUsableModel(t *testing.T) {
	service, _, _, models := newOnboardingTestService(t)
	principal := onboardingTestPrincipal()
	if _, err := service.SavePreferences(context.Background(), principal, SaveUserPreferencesCommand{
		SelectedModelID: "p7654321",
	}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("unconfigured model error = %v", err)
	}
	value, err := service.SavePreferences(context.Background(), principal, SaveUserPreferencesCommand{
		AssistantName: "  Morde  ", SelectedModelID: "p1234567",
	})
	if err != nil || value.AssistantName != "Morde" || value.SelectedModelID != "p1234567" || models.lastPrincipal.UserID != principal.UserID {
		t.Fatalf("SavePreferences() = %#v, %v; model principal=%#v", value, err, models.lastPrincipal)
	}
	status, err := service.Status(context.Background(), principal)
	if err != nil || status.ModelStatus != ModelReady || len(status.AvailableModels) != 1 || status.AvailableModels[0].Capabilities.Vision {
		t.Fatalf("no-vision model status = %#v, %v", status, err)
	}
}

func TestOnboardingInstantChatAcceptsCheckedTextOnlyModel(t *testing.T) {
	service, _, preferences, models := newOnboardingTestService(t)
	models.items = append(models.items, ModelConnectionView{
		ID: "p7654321", Name: "text only", CheckedAt: timePointer(time.Now()),
		Capabilities: ModelCapabilities{Text: true},
	})
	preferences.value = UserPreferences{OwnerID: "u1234567", SelectedModelID: "p1234567", DefaultHostID: "h1234567"}

	status, err := service.Status(context.Background(), onboardingTestPrincipal())
	if err != nil || status.ModelStatus != ModelReady || len(status.AvailableModels) != 2 || status.AvailableModels[1].ID != "p7654321" {
		t.Fatalf("checked text-only model was not available: %#v, %v", status, err)
	}
	if status.AvailableModels[0].Capabilities.Vision {
		t.Fatal("no-vision model was reported as vision-capable")
	}
	if _, err := service.SavePreferences(context.Background(), onboardingTestPrincipal(), SaveUserPreferencesCommand{SelectedModelID: "p7654321"}); err != nil {
		t.Fatalf("text-only model was rejected: %v", err)
	}
}

func TestOnboardingStatusRequiresSelectedModelButNotRunningHost(t *testing.T) {
	service, hosts, preferences, _ := newOnboardingTestService(t)
	preferences.value = UserPreferences{OwnerID: "u1234567", DefaultHostID: "h1234567"}
	hosts.values = []host.Host{{ID: "h1234567", OwnerID: "u1234567", State: host.StateRunning}}

	status, err := service.Status(context.Background(), onboardingTestPrincipal())
	if err != nil || status.ModelStatus != ModelSelectionRequired || status.CanSubmit || status.NextAction != "configure_model" {
		t.Fatalf("unselected model status = %#v, %v", status, err)
	}
	preferences.value.SelectedModelID = "p1234567"
	status, err = service.Status(context.Background(), onboardingTestPrincipal())
	if err != nil || !status.CanSubmit || status.ComputerStatus != ComputerReady || status.NextAction != "start_chat" {
		t.Fatalf("ready status = %#v, %v", status, err)
	}
	hosts.values[0].State = host.StateStopped
	status, err = service.Status(context.Background(), onboardingTestPrincipal())
	if err != nil || !status.CanSubmit || status.NextAction != "start_chat" {
		t.Fatalf("stopped host should still allow chat: %#v, %v", status, err)
	}
}

func TestOnboardingRejectsAPIKeyPrincipal(t *testing.T) {
	service, _, _, _ := newOnboardingTestService(t)
	principal := onboardingTestPrincipal()
	principal.Kind = identity.KindAPIKey
	if _, err := service.Status(context.Background(), principal); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("Status(API key) error = %v", err)
	}
}

func newOnboardingTestService(t *testing.T) (*OnboardingService, *onboardingHostStub, *onboardingPreferencesStub, *onboardingModelStub) {
	t.Helper()
	preferences := &onboardingPreferencesStub{value: UserPreferences{OwnerID: "u1234567"}, locks: make(map[string]*sync.Mutex)}
	hosts := &onboardingHostStub{}
	models := &onboardingModelStub{items: []ModelConnectionView{{
		ID: "p1234567", Name: "shared model", CheckedAt: timePointer(time.Now()),
		Capabilities: ModelCapabilities{Text: true, Tools: true, Vision: false},
	}}}
	service, err := NewOnboardingService(preferences, hosts, models, "std-2c4g")
	if err != nil {
		t.Fatalf("NewOnboardingService() error = %v", err)
	}
	return service, hosts, preferences, models
}

func onboardingTestPrincipal() identity.Principal {
	return identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
}

func timePointer(value time.Time) *time.Time {
	return &value
}

type onboardingPreferencesStub struct {
	mu    sync.Mutex
	value UserPreferences
	locks map[string]*sync.Mutex
}

func (s *onboardingPreferencesStub) GetUserPreferences(_ context.Context, ownerID string) (UserPreferences, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := s.value
	if value.OwnerID == "" {
		value.OwnerID = ownerID
	}
	return value, nil
}

func (s *onboardingPreferencesStub) SaveUserPreferences(_ context.Context, ownerID, assistantName, modelID string) (UserPreferences, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value.OwnerID, s.value.AssistantName, s.value.SelectedModelID = ownerID, assistantName, modelID
	return s.value, nil
}

func (s *onboardingPreferencesStub) SetDefaultHost(_ context.Context, ownerID, hostID, issue string) (UserPreferences, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value.OwnerID, s.value.DefaultHostID, s.value.LastHostIssue = ownerID, hostID, issue
	return s.value, nil
}

func (s *onboardingPreferencesStub) AcquireDefaultHostLock(_ context.Context, ownerID string) (func() error, error) {
	s.mu.Lock()
	lock := s.locks[ownerID]
	if lock == nil {
		lock = &sync.Mutex{}
		s.locks[ownerID] = lock
	}
	s.mu.Unlock()
	lock.Lock()
	return func() error { lock.Unlock(); return nil }, nil
}

type onboardingModelStub struct {
	items         []ModelConnectionView
	lastPrincipal identity.Principal
}

func (s *onboardingModelStub) List(_ context.Context, principal identity.Principal) ([]ModelConnectionView, error) {
	s.lastPrincipal = principal
	return append([]ModelConnectionView(nil), s.items...), nil
}

type onboardingHostStub struct {
	mu           sync.Mutex
	values       []host.Host
	createCalls  int
	restoreCalls int
	restoreErr   error
}

func (s *onboardingHostStub) Create(_ context.Context, principal identity.Principal, command CreateHostCommand) (host.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls++
	value := host.Host{ID: fmt.Sprintf("h%07d", s.createCalls), OwnerID: principal.UserID, Name: "Cloud computer", State: host.StateCreating, RuntimeGeneration: 1}
	s.values = append(s.values, value)
	return value, nil
}

func (s *onboardingHostStub) EnsureDefaultStopped(_ context.Context, user identity.User) (host.Host, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, value := range s.values {
		if value.OwnerID == user.ID && value.DeletedAt == nil && value.State != host.StateDeleted {
			return value, false, nil
		}
	}
	s.createCalls++
	value := host.Host{ID: fmt.Sprintf("h%07d", s.createCalls), OwnerID: user.ID, Name: "Cloud computer", State: host.StateStopped, RuntimeGeneration: 1}
	s.values = append(s.values, value)
	return value, true, nil
}

func (s *onboardingHostStub) List(context.Context, identity.Principal) ([]host.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]host.Host(nil), s.values...), nil
}

func (s *onboardingHostStub) Get(_ context.Context, _ identity.Principal, id string) (host.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, value := range s.values {
		if value.ID == id {
			return value, nil
		}
	}
	return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
}

func (*onboardingHostStub) BindWorkspace(context.Context, identity.Principal, string, string, *int64, int64, string) (workspace.Binding, error) {
	return workspace.Binding{}, nil
}

func (*onboardingHostStub) UnbindWorkspace(context.Context, identity.Principal, string, string, int64, bool, string) (workspace.Binding, error) {
	return workspace.Binding{}, nil
}

func (*onboardingHostStub) RetryWorkspaceSync(context.Context, identity.Principal, string, string, int64) (workspace.Binding, error) {
	return workspace.Binding{}, nil
}

func (s *onboardingHostStub) Delete(context.Context, identity.Principal, DeleteHostCommand) (host.Host, error) {
	return host.Host{}, nil
}

func (s *onboardingHostStub) Release(context.Context, identity.Principal, string, bool) (host.Host, error) {
	return host.Host{}, nil
}

func (s *onboardingHostStub) Restore(_ context.Context, _ identity.Principal, hostID string) (host.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restoreCalls++
	if s.restoreErr != nil {
		return host.Host{}, s.restoreErr
	}
	for i := range s.values {
		if s.values[i].ID == hostID {
			s.values[i].RuntimeGeneration++
			s.values[i].State = host.StateCreating
			return s.values[i], nil
		}
	}
	return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
}

func (s *onboardingHostStub) Renew(context.Context, identity.Principal, string) (host.Host, error) {
	return host.Host{}, nil
}

func (s *onboardingHostStub) IssueTicket(context.Context, identity.Principal, string) (EnterInfo, error) {
	return EnterInfo{}, nil
}

func (s *onboardingHostStub) IssuePreviewTicket(context.Context, identity.Principal, string, int) (EnterInfo, error) {
	return EnterInfo{}, nil
}
