package app

import (
	"context"
	"errors"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type hostCopyHostsStub struct {
	source     host.Host
	created    host.Host
	getErr     error
	createErr  error
	getCalls   int
	createCall int
	command    CreateHostCommand
}

func (s *hostCopyHostsStub) Get(_ context.Context, _ identity.Principal, id string) (host.Host, error) {
	s.getCalls++
	if id != s.source.ID {
		return host.Host{}, errors.New("unexpected source Host")
	}
	return s.source, s.getErr
}

func (s *hostCopyHostsStub) Create(_ context.Context, _ identity.Principal, command CreateHostCommand) (host.Host, error) {
	s.createCall++
	s.command = command
	return s.created, s.createErr
}

type hostCopyWorkspacesStub struct {
	result       WorkspaceCopyResult
	replayResult WorkspaceCopyResult
	err          error
	replayErr    error
	calls        int
	replayCalls  int
	replayFound  bool
	hostID       string
	version      int64
	workspaceID  string
	command      WorkspaceCopyCommand
	key          string
}

func (s *hostCopyWorkspacesStub) ResumeOrReplayHostCopy(_ context.Context, _ identity.Principal, _ string,
	command WorkspaceCopyCommand, key string) (WorkspaceCopyResult, bool, error) {
	s.replayCalls++
	if s.replayFound {
		s.command, s.key = command, key
	}
	return s.replayResult, s.replayFound, s.replayErr
}

func (s *hostCopyWorkspacesStub) CopyFromHost(_ context.Context, _ identity.Principal, hostID string, version int64,
	workspaceID string, command WorkspaceCopyCommand, key string) (WorkspaceCopyResult, error) {
	s.calls++
	s.hostID, s.version, s.workspaceID, s.command, s.key = hostID, version, workspaceID, command, key
	return s.result, s.err
}

func TestHostCopyCreatesIndependentHostFromConfirmedWorkspace(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	source := host.Host{
		ID: "h1234567", OwnerID: principal.UserID, State: host.StateStopped, ControlState: host.ControlAI,
		Workspaces: []workspace.Binding{{
			ID: "b-source", HostID: "h1234567", OwnerID: principal.UserID, State: workspace.BindingActive,
			BindingVersion: 4, DesiredRevision: 8, AppliedRevision: 8, SyncState: workspace.BindingSyncHealthy,
			Workspace: workspace.Workspace{ID: "w1234567", OwnerID: principal.UserID, Name: "Source", State: workspace.StateReady, Revision: 8},
		}},
	}
	workspaces := &hostCopyWorkspacesStub{result: WorkspaceCopyResult{
		SourceWorkspaceID: "w1234567", Workspace: DeviceWorkspaceInfo{ID: "w7654321", Name: "Clone", Revision: 1, State: workspace.StateReady},
		SourceRevision: 8, State: workspace.CopyCompleted, reservationID: "r1234567",
	}}
	hosts := &hostCopyHostsStub{source: source, created: host.Host{ID: "h7654321", OwnerID: principal.UserID}}
	service, err := NewHostCopyService(hosts, workspaces)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Copy(context.Background(), principal, source.ID, HostCopyCommand{Name: "Clone", ExpectedCurrentRevision: 8}, "copy-0001")
	if err != nil {
		t.Fatal(err)
	}
	if hosts.getCalls != 1 || workspaces.calls != 1 || workspaces.replayCalls != 1 || hosts.createCall != 1 ||
		workspaces.hostID != source.ID || workspaces.version != 4 || workspaces.workspaceID != "w1234567" ||
		workspaces.command.Name != "Clone" || workspaces.command.ExpectedCurrentRevision != 8 || workspaces.key != "copy-0001" {
		t.Fatalf("Host copy stages = host=%#v workspace=%#v", hosts, workspaces)
	}
	if hosts.command.Name != "Clone" || len(hosts.command.WorkspaceIDs) != 1 || hosts.command.WorkspaceIDs[0] != "w7654321" ||
		hosts.command.IdempotencyKey != hostCopyCreateKey("copy-0001") || hosts.command.workspaceCopyReservationID != "r1234567" {
		t.Fatalf("new Host command = %#v", hosts.command)
	}
	if result.Host.ID != "h7654321" || result.SourceHostID != source.ID || result.SourceWorkspaceID != "w1234567" ||
		result.Workspace.ID != "w7654321" || result.SourceRevision != 8 || result.State != workspace.CopyCompleted {
		t.Fatalf("Host copy result = %#v", result)
	}
}

func TestHostCopyReplaysCompletedReservationBeforeCheckingLiveSource(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	workspaces := &hostCopyWorkspacesStub{replayFound: true, replayResult: WorkspaceCopyResult{
		SourceWorkspaceID: "w1234567", Workspace: DeviceWorkspaceInfo{ID: "w7654321", Name: "Clone", Revision: 1, State: workspace.StateReady},
		SourceRevision: 8, State: workspace.CopyCompleted, Replayed: true, reservationID: "r1234567",
	}}
	hosts := &hostCopyHostsStub{
		source:  host.Host{ID: "h1234567", OwnerID: principal.UserID, State: host.StateRunning},
		created: host.Host{ID: "h7654321", OwnerID: principal.UserID},
	}
	service, err := NewHostCopyService(hosts, workspaces)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Copy(context.Background(), principal, "h1234567", HostCopyCommand{Name: "Clone", ExpectedCurrentRevision: 8}, "copy-0003")
	if err != nil {
		t.Fatal(err)
	}
	if hosts.getCalls != 0 || workspaces.calls != 0 || workspaces.replayCalls != 1 || hosts.createCall != 1 ||
		!result.Replayed || result.SourceWorkspaceID != "w1234567" || result.Host.ID != "h7654321" {
		t.Fatalf("completed Host copy replay checked source or repeated copy: result=%#v hosts=%#v workspaces=%#v", result, hosts, workspaces)
	}
}

func TestHostCopyRejectsAPIKeyBeforeReservationOrHostAccess(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t1234567"}
	hosts := &hostCopyHostsStub{source: host.Host{ID: "h1234567", OwnerID: principal.UserID}}
	workspaces := &hostCopyWorkspacesStub{}
	service, err := NewHostCopyService(hosts, workspaces)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Copy(context.Background(), principal, "h1234567", HostCopyCommand{Name: "Clone", ExpectedCurrentRevision: 2}, "copy-0004")
	if domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("Copy() error = %v, want forbidden", err)
	}
	if workspaces.replayCalls != 0 || workspaces.calls != 0 || hosts.getCalls != 0 || hosts.createCall != 0 {
		t.Fatalf("API key reached reservation or Host access: workspace=%#v hosts=%#v", workspaces, hosts)
	}
}

func TestHostCopyRejectsUnfencedOrUnconfirmedSource(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	base := host.Host{
		ID: "h1234567", OwnerID: principal.UserID, State: host.StateStopped, ControlState: host.ControlAI,
		Workspaces: []workspace.Binding{{
			HostID: "h1234567", OwnerID: principal.UserID, State: workspace.BindingActive,
			BindingVersion: 1, DesiredRevision: 2, AppliedRevision: 2, SyncState: workspace.BindingSyncHealthy,
			Workspace: workspace.Workspace{ID: "w1234567", OwnerID: principal.UserID, State: workspace.StateReady, Revision: 2},
		}},
	}
	cases := []struct {
		name   string
		change func(*host.Host)
	}{
		{name: "running", change: func(value *host.Host) { value.State = host.StateRunning }},
		{name: "sandbox still present", change: func(value *host.Host) { value.SandboxID = "sandbox" }},
		{name: "runtime instance still present", change: func(value *host.Host) { value.RuntimeInstanceID = "runtime" }},
		{name: "user control", change: func(value *host.Host) { value.ControlState = host.ControlUser }},
		{name: "invalid binding version", change: func(value *host.Host) { value.Workspaces[0].BindingVersion = 0 }},
		{name: "stale applied revision", change: func(value *host.Host) { value.Workspaces[0].AppliedRevision-- }},
		{name: "sync conflict", change: func(value *host.Host) {
			value.Workspaces[0].SyncState = workspace.BindingSyncConflict
			value.Workspaces[0].SyncConflicts = []string{"src/file.txt"}
		}},
		{name: "multiple bindings", change: func(value *host.Host) {
			value.Workspaces = append(value.Workspaces, value.Workspaces[0])
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := base
			source.Workspaces = append([]workspace.Binding(nil), base.Workspaces...)
			tc.change(&source)
			hosts := &hostCopyHostsStub{source: source}
			workspaces := &hostCopyWorkspacesStub{}
			service, err := NewHostCopyService(hosts, workspaces)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.Copy(context.Background(), principal, source.ID, HostCopyCommand{Name: "Clone", ExpectedCurrentRevision: 2}, "copy-0002"); err == nil {
				t.Fatal("Copy() error = nil, want rejected source")
			}
			if workspaces.calls != 0 || hosts.createCall != 0 {
				t.Fatalf("unsafe source reached copy/create: workspace calls=%d host create calls=%d", workspaces.calls, hosts.createCall)
			}
		})
	}
}
