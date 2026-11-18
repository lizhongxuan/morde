package app

import (
	"context"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func TestHostCopyRejectsIndependentWorkspaceWithoutSideEffects(t *testing.T) {
	service := NewHostCopyService()
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}

	err := service.Copy(context.Background(), principal, "h1234567", HostCopyCommand{
		Name: "Clone", ExpectedCurrentRevision: 8,
	}, "copy-0001")
	if domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("Host copy error = %v, want workspace conflict", err)
	}
}

func TestHostCopyValidatesCallerAndRequestBeforeConflict(t *testing.T) {
	service := NewHostCopyService()
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey}
	if err := service.Copy(context.Background(), principal, "h1234567", HostCopyCommand{
		Name: "Clone", ExpectedCurrentRevision: 8,
	}, "copy-0001"); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("API key Host copy error = %v, want forbidden", err)
	}

	principal.Kind = identity.KindSession
	if err := service.Copy(context.Background(), principal, "bad", HostCopyCommand{
		Name: "Clone", ExpectedCurrentRevision: 8,
	}, "copy-0001"); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("invalid Host copy request error = %v, want invalid argument", err)
	}
}
