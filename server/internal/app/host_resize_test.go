package app

import (
	"context"
	"errors"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

type resizeTestStore struct {
	updated appResizeTestResult
	calls   int
	params  UpdateStoppedHostResourcesParams
}

type appResizeTestResult struct {
	snapshot HostResourceSnapshot
	err      error
}

func (s *resizeTestStore) GetHostResources(context.Context, string) (HostResourceSnapshot, error) {
	return s.updated.snapshot, s.updated.err
}

func (s *resizeTestStore) UpdateStoppedHostResources(_ context.Context, params UpdateStoppedHostResourcesParams) (HostResourceSnapshot, error) {
	s.calls++
	s.params = params
	return s.updated.snapshot, s.updated.err
}

func (s *resizeTestStore) BeginOnlineHostResources(context.Context, UpdateStoppedHostResourcesParams) (OnlineHostResourceUpdate, error) {
	return OnlineHostResourceUpdate{}, domain.NewError(domain.CodeUnsupported, "online runtime unavailable")
}

func (s *resizeTestStore) CompleteOnlineHostResources(context.Context, OnlineHostResourceUpdate) (HostResourceSnapshot, error) {
	return HostResourceSnapshot{}, errors.New("unexpected online completion")
}

func (s *resizeTestStore) AbortOnlineHostResources(context.Context, OnlineHostResourceUpdate, bool) error {
	return errors.New("unexpected online abort")
}

func TestAdminHostResourcesUpdateGuardsAndPropagatesFailure(t *testing.T) {
	id, err := domain.NewHostID()
	if err != nil {
		t.Fatal(err)
	}
	store := &resizeTestStore{}
	service, err := NewAdminHostResourcesService(store, Capacity{CPUMilli: 8000, MemMiB: 16384}, &hostServiceTestRouter{})
	if err != nil {
		t.Fatal(err)
	}
	user := identity.Principal{Kind: identity.KindSession, Role: identity.RoleUser}
	if _, err := service.Update(context.Background(), user, id, HostResources{CPUMilli: 1000, MemMiB: 2048}, 1); domain.CodeOf(err) != domain.CodeForbidden || store.calls != 0 {
		t.Fatalf("user update: err=%v calls=%d", err, store.calls)
	}
	admin := identity.Principal{Kind: identity.KindSystem, Role: identity.RoleAdmin}
	if _, err := service.Update(context.Background(), admin, id, HostResources{CPUMilli: 0, MemMiB: 2048}, 1); domain.CodeOf(err) != domain.CodeInvalidArgument || store.calls != 0 {
		t.Fatalf("invalid update: err=%v calls=%d", err, store.calls)
	}
	unsupported := domain.NewError(domain.CodeUnsupported, "running host requires online resource update")
	store.updated.err = unsupported
	if result, err := service.Update(context.Background(), admin, id, HostResources{CPUMilli: 1000, MemMiB: 2048}, 1); domain.CodeOf(err) != domain.CodeUnsupported || result != (HostResourceSnapshot{}) {
		t.Fatalf("running host update: result=%#v err=%v", result, err)
	}
	if store.calls != 1 || store.params.HostID != id || store.params.ActorID != "system" || store.params.ExpectedVersion != 1 {
		t.Fatalf("unexpected store call: %#v, calls=%d", store.params, store.calls)
	}
	store.updated = appResizeTestResult{snapshot: HostResourceSnapshot{
		HostID: id, State: host.StateStopped, Resources: HostResources{CPUMilli: 1000, MemMiB: 2048}, Version: 2,
	}}
	result, err := service.Update(context.Background(), admin, id, HostResources{CPUMilli: 1000, MemMiB: 2048}, 1)
	if err != nil || result.Version != 2 || result.State != host.StateStopped {
		t.Fatalf("stopped host update: result=%#v err=%v", result, err)
	}
}

type onlineResizeStore struct {
	update    OnlineHostResourceUpdate
	completed int
	aborted   []bool
}

func (s *onlineResizeStore) GetHostResources(context.Context, string) (HostResourceSnapshot, error) {
	return HostResourceSnapshot{}, nil
}
func (s *onlineResizeStore) UpdateStoppedHostResources(context.Context, UpdateStoppedHostResourcesParams) (HostResourceSnapshot, error) {
	return HostResourceSnapshot{}, domain.NewError(domain.CodeUnsupported, "running")
}
func (s *onlineResizeStore) BeginOnlineHostResources(context.Context, UpdateStoppedHostResourcesParams) (OnlineHostResourceUpdate, error) {
	return s.update, nil
}
func (s *onlineResizeStore) CompleteOnlineHostResources(context.Context, OnlineHostResourceUpdate) (HostResourceSnapshot, error) {
	s.completed++
	return HostResourceSnapshot{HostID: s.update.HostID, State: host.StateRunning, Resources: s.update.Target, Version: s.update.ReservedVersion + 1, UpdateState: "idle"}, nil
}
func (s *onlineResizeStore) AbortOnlineHostResources(_ context.Context, _ OnlineHostResourceUpdate, unknown bool) error {
	s.aborted = append(s.aborted, unknown)
	return nil
}

type onlineResizeSandbox struct {
	SandboxClient
	current   SandboxResourceStatus
	failFirst bool
	updates   int
}

func (s *onlineResizeSandbox) ReadResources(context.Context, string) (SandboxResourceStatus, error) {
	return s.current, nil
}
func (s *onlineResizeSandbox) UpdateResources(_ context.Context, _ string, expected SandboxResourceStatus, target HostResources) (SandboxResourceStatus, error) {
	s.updates++
	if s.failFirst && s.updates == 1 {
		s.current.Resources.CPUMilli = target.CPUMilli
		return SandboxResourceStatus{}, ErrSandboxUnavailable
	}
	s.current.Resources = target
	return s.current, nil
}

type onlineResizeRouter struct{ client SandboxClient }

func (r onlineResizeRouter) For(string) (SandboxClient, error) { return r.client, nil }
func (r onlineResizeRouter) Profiles() []string                { return []string{"default"} }

func TestOnlineResourcesRequireRuntimeReadbackBeforeCommit(t *testing.T) {
	id, err := domain.NewHostID()
	if err != nil {
		t.Fatal(err)
	}
	update := OnlineHostResourceUpdate{
		HostID: id, RuntimeProfile: "default", SandboxID: "sandbox-1",
		Old:        HostResources{CPUMilli: 1000, MemMiB: 2048},
		OldRuntime: HostResources{CPUMilli: 1000, MemMiB: 2048},
		Target:     HostResources{CPUMilli: 1500, MemMiB: 3072}, ReservedVersion: 2,
	}
	store := &onlineResizeStore{update: update}
	sandbox := &onlineResizeSandbox{current: SandboxResourceStatus{
		Resources: update.OldRuntime, ContainerID: "container-1", StartedAt: "start-1",
	}}
	service, err := NewAdminHostResourcesService(store, Capacity{CPUMilli: 8000, MemMiB: 16384}, onlineResizeRouter{sandbox})
	if err != nil {
		t.Fatal(err)
	}
	admin := identity.Principal{Kind: identity.KindSystem, Role: identity.RoleAdmin}
	result, err := service.Update(context.Background(), admin, id, update.Target, 1)
	if err != nil || result.Resources != update.Target || store.completed != 1 || len(store.aborted) != 0 {
		t.Fatalf("verified update = %#v, err=%v, completed=%d, aborted=%v", result, err, store.completed, store.aborted)
	}
	store.completed = 0
	sandbox.current.Resources = update.OldRuntime
	sandbox.updates = 0
	sandbox.failFirst = true
	_, err = service.Update(context.Background(), admin, id, update.Target, 1)
	if !errors.Is(err, ErrSandboxUnavailable) || store.completed != 0 || len(store.aborted) != 1 || store.aborted[0] || sandbox.current.Resources != update.OldRuntime {
		t.Fatalf("failed update = err=%v, completed=%d, aborted=%v, runtime=%#v", err, store.completed, store.aborted, sandbox.current)
	}
}
