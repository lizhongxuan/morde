package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

type taskSharedHostStore struct {
	HostStore
	value     host.Host
	access    host.ActorAccess
	err       error
	member    project.Member
	memberErr error
}

func (s *taskSharedHostStore) GetHost(context.Context, string) (host.Host, error) {
	return s.value, nil
}

func (s *taskSharedHostStore) GetHostActorAccess(context.Context, string, string) (host.ActorAccess, error) {
	return s.access, s.err
}

func (s *taskSharedHostStore) GetProjectMember(context.Context, string, string) (project.Member, error) {
	return s.member, s.memberErr
}

func TestSharedTaskHostRequiresCurrentActorCredentialAndControlGrant(t *testing.T) {
	ctx := context.Background()
	store := &taskSharedHostStore{value: host.Host{ID: "h1234567", OwnerID: "u7654321", State: host.StateRunning,
		ControlState: host.ControlAI, RuntimeGeneration: 3, RuntimeInstanceID: "runtime-1", ControlVersion: 4},
		access: host.ActorAccess{OwnerID: "u7654321", Permissions: []host.Permission{host.PermissionDesktopControl}}}
	users := &credentialUsers{values: map[string]identity.User{"u1234567": {ID: "u1234567", Status: identity.StatusApproved}}}
	tokens := &credentialTokens{values: map[string]identity.Token{"t1234567": {ID: "t1234567", OwnerID: "u1234567",
		Kind: identity.KindSession, ExpiresAt: time.Now().Add(time.Hour)}}}
	credentials := &CredentialService{users: users, hosts: store, tokens: tokens}
	runtime := &RuntimeControl{hosts: store}
	expires := tokens.values["t1234567"].ExpiresAt
	claimed := task.Task{InitiatorID: "u1234567", HostID: "h1234567", Authorization: task.Authorization{
		Kind: string(identity.KindSession), TokenID: "t1234567", Version: 1, ExpiresAt: &expires}}
	check := func() error {
		if err := credentials.AuthorizeTaskHostAction(ctx, claimed, identity.ScopeComputerObserve); err != nil {
			return err
		}
		_, err := runtime.ReadTaskFence(ctx, "u1234567", "h1234567")
		return err
	}
	if err := check(); err != nil {
		t.Fatalf("active shared Host grant was denied: %v", err)
	}
	claimed.ProjectID = "j1234567"
	store.member = project.Member{ProjectID: claimed.ProjectID, UserID: claimed.InitiatorID, Role: project.RoleMember, Version: 1}
	if err := check(); err != nil {
		t.Fatalf("authorized project member was denied: %v", err)
	}
	store.member.Role = project.RoleViewer
	if err := check(); err == nil {
		t.Fatal("project viewer retained task Host access")
	}
	store.member.Role = project.RoleMember
	store.memberErr = errors.New("project membership revoked")
	if err := check(); err == nil {
		t.Fatal("revoked project member retained task Host access")
	}
	store.memberErr = nil
	claimed.ProjectID = ""
	store.access.Permissions = []host.Permission{host.PermissionDesktopView}
	if err := check(); err == nil {
		t.Fatal("view-only membership allowed Agent control")
	}
	store.access.Permissions = []host.Permission{host.PermissionDesktopControl}
	store.access.OwnerID = "u8765432"
	if err := check(); err == nil {
		t.Fatal("grant for another Host owner was accepted")
	}
	store.access.OwnerID = "u7654321"
	store.access.Disabled = true
	if err := check(); err == nil {
		t.Fatal("disabled actor retained Host access")
	}
	store.access.Disabled = false
	store.err = errors.New("membership unavailable")
	if err := check(); err == nil {
		t.Fatal("unavailable grant was accepted")
	}
	store.err = nil
	now := time.Now()
	token := tokens.values["t1234567"]
	token.RevokedAt = &now
	tokens.values[token.ID] = token
	if err := check(); err == nil {
		t.Fatal("revoked task credential retained Host access")
	}
}
