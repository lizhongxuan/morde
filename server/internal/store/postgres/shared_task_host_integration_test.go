package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type sharedHostAuditStub struct{}

func (sharedHostAuditStub) Write(context.Context, audit.Entry) error { return nil }

type sharedHostDefaultStub struct{}

func (sharedHostDefaultStub) EnsureDefaultStopped(context.Context, identity.User) (host.Host, bool, error) {
	return host.Host{}, false, nil
}

type sharedHostSandboxStub struct{ app.SandboxRouter }

func TestProjectTaskSharedHostUsesActorCredentialAndRevocableGrant(t *testing.T) {
	ctx := context.Background()
	f := newProjectAttachmentFixture(t)
	hostOwner := createPreRetirementTestUser(t, f.store, 2, 2000, 4096)
	computer := createRunningControlHost(t, ctx, f.store, hostOwner.ID, "ai", 0)
	grant := host.Member{HostID: computer.ID, OwnerID: hostOwner.ID, UserID: f.member.UserID,
		Permissions: []host.Permission{host.PermissionDesktopControl}}
	if _, err := f.store.UpsertHostMember(ctx, grant); err != nil {
		t.Fatal(err)
	}
	credential, err := app.NewCredentialService(f.store, f.store, f.store, sharedHostAuditStub{}, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 1, LoginWindow: time.Minute}, sharedHostDefaultStub{})
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := app.NewRuntimeControl(f.store, &sharedHostSandboxStub{}, sealer)
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.store.GetTokenByID(ctx, f.member.TokenID)
	if err != nil {
		t.Fatal(err)
	}
	claimed := task.Task{OwnerID: f.owner.UserID, InitiatorID: f.member.UserID, ProjectID: f.projectID, HostID: computer.ID,
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt}}
	check := func() error {
		if err := credential.AuthorizeTaskHostAction(ctx, claimed, identity.ScopeComputerObserve); err != nil {
			return err
		}
		_, err := runtime.ReadTaskFence(ctx, claimed.InitiatorID, claimed.HostID)
		return err
	}
	if err := check(); err != nil {
		t.Fatalf("authorized project actor could not use shared Host: %v", err)
	}
	wrong := claimed
	wrong.Authorization.TokenID = f.owner.TokenID
	if err := credential.AuthorizeTaskHostAction(ctx, wrong, identity.ScopeComputerObserve); err == nil {
		t.Fatal("project owner's credential authorized another actor")
	}
	if err := f.store.RevokeHostMember(ctx, computer.ID, hostOwner.ID, f.member.UserID); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Fatal("revoked Host grant remained usable")
	}
	if _, err := f.store.UpsertHostMember(ctx, grant); err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Fatalf("restored Host grant not visible to a new authorization: %v", err)
	}
	p, err := f.store.GetProject(ctx, f.owner.UserID, f.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.RemoveProjectMember(ctx, f.owner.UserID, f.projectID, f.member.UserID, p.Version, "shared-host-project-revoke"); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Fatal("removed project member retained task Host access")
	}
}
