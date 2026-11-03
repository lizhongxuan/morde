package app

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

type projectMCPConnectorStoreStub struct {
	items      []ProjectMCPConnector
	grant      ProjectMCPConnector
	revoked    ProjectMCPConnector
	actorID    string
	projectID  string
	connector  string
	expected   int64
	authorized bool
}

func (s *projectMCPConnectorStoreStub) ListProjectMCPConnectors(_ context.Context, actorID, projectID string) ([]ProjectMCPConnector, error) {
	s.actorID, s.projectID = actorID, projectID
	return s.items, nil
}

func (s *projectMCPConnectorStoreStub) GrantProjectMCPConnector(_ context.Context, actorID, projectID, connectorID string, expected int64) (ProjectMCPConnector, error) {
	s.actorID, s.projectID, s.connector, s.expected = actorID, projectID, connectorID, expected
	return s.grant, nil
}

func (s *projectMCPConnectorStoreStub) RevokeProjectMCPConnector(_ context.Context, actorID, projectID, connectorID string, expected int64) (ProjectMCPConnector, error) {
	s.actorID, s.projectID, s.connector, s.expected = actorID, projectID, connectorID, expected
	return s.revoked, nil
}

func (s *projectMCPConnectorStoreStub) AuthorizeProjectMCPConnector(context.Context, string, string, string, string, int64) error {
	s.authorized = true
	return nil
}

type projectAccessCheckerStub struct {
	member project.Member
	role   project.Role
}

func (s *projectAccessCheckerStub) RequireAccess(_ context.Context, _ identity.Principal, projectID string, role project.Role) (project.Member, error) {
	s.role = role
	if s.member.ProjectID == "" {
		s.member = project.Member{ProjectID: projectID, UserID: "u1234567", Role: project.RoleMember, Version: 1}
	}
	return s.member, nil
}

func activeProjectConnectorFixture() ProjectMCPConnector {
	now := time.Now().UTC()
	return ProjectMCPConnector{
		Grant: project.MCPConnectorGrant{ProjectID: "j1234567", ConnectorOwnerID: "u1234567", ConnectorID: "k1234567",
			Version: 1, AuthorizedBy: "u1234567", Active: true, CreatedAt: now, UpdatedAt: now},
		Name: "Calendar", Enabled: true, ConnectorRevision: 2,
	}
}

func TestProjectMCPConnectorServiceRequiresMemberAndKeepsOwnerExplicit(t *testing.T) {
	connector := activeProjectConnectorFixture()
	store := &projectMCPConnectorStoreStub{items: []ProjectMCPConnector{connector}, grant: connector}
	access := &projectAccessCheckerStub{}
	service, err := NewProjectMCPConnectorService(store, access)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Role: identity.RoleUser, Kind: identity.KindSession}

	items, err := service.List(context.Background(), principal, "j1234567")
	if err != nil || len(items) != 1 || access.role != project.RoleViewer || store.actorID != principal.UserID {
		t.Fatalf("list project connectors = %#v, err=%v, access=%#v store=%#v", items, err, access, store)
	}
	granted, err := service.Grant(context.Background(), principal, "j1234567", "k1234567", 0)
	if err != nil || granted.Grant.ConnectorOwnerID != principal.UserID || store.expected != 0 || store.connector != "k1234567" {
		t.Fatalf("grant project connector = %#v, err=%v, store=%#v", granted, err, store)
	}

	if _, err := service.Grant(context.Background(), principal, "j1234567", "k1234567", -1); err == nil {
		t.Fatal("negative expected grant version was accepted")
	}
}

func TestProjectMCPConnectorServiceAllowsOwnerToWithdrawAfterLeavingProject(t *testing.T) {
	connector := activeProjectConnectorFixture()
	connector.Grant.Active = false
	revokedAt := time.Now().UTC()
	connector.Grant.RevokedAt = &revokedAt
	connector.Grant.Version = 2
	connector.Name = ""
	connector.ConnectorRevision = 0
	store := &projectMCPConnectorStoreStub{revoked: connector}
	service, err := NewProjectMCPConnectorService(store, &projectAccessCheckerStub{})
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	got, err := service.Revoke(context.Background(), principal, "j1234567", "k1234567", 1)
	if err != nil || got.Grant.Active || store.expected != 1 {
		t.Fatalf("revoke project connector = %#v, err=%v, store=%#v", got, err, store)
	}
}
