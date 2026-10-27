package app

import (
	"testing"

	"morde/server/internal/domain/identity"
)

func TestClientAccountPrincipalRequiresUserInstallation(t *testing.T) {
	client := identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: "t1234567", ClientInstallationID: "i1234567",
	}
	if err := validateClientAccountPrincipal(client); err != nil {
		t.Fatalf("valid client principal rejected: %v", err)
	}
	for name, principal := range map[string]identity.Principal{
		"admin client":    {UserID: client.UserID, Role: identity.RoleAdmin, Kind: client.Kind, TokenID: client.TokenID, ClientInstallationID: client.ClientInstallationID},
		"no installation": {UserID: client.UserID, Role: client.Role, Kind: client.Kind, TokenID: client.TokenID},
		"no token":        {UserID: client.UserID, Role: client.Role, Kind: client.Kind, ClientInstallationID: client.ClientInstallationID},
		"API key":         {UserID: client.UserID, Role: client.Role, Kind: identity.KindAPIKey, TokenID: client.TokenID},
		"scoped client":   {UserID: client.UserID, Role: client.Role, Kind: client.Kind, TokenID: client.TokenID, ClientInstallationID: client.ClientInstallationID, WorkspaceIDs: []string{"w1234567"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateClientAccountPrincipal(principal); err == nil {
				t.Fatal("invalid client principal was accepted")
			}
		})
	}
	portal := identity.Principal{UserID: client.UserID, Role: identity.RoleAdmin, Kind: identity.KindSession, TokenID: client.TokenID}
	if err := validateClientAccountPrincipal(portal); err != nil {
		t.Fatalf("valid portal session rejected: %v", err)
	}
}
