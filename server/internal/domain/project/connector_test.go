package project

import (
	"testing"
	"time"
)

func TestMCPConnectorGrantValidation(t *testing.T) {
	now := time.Now().UTC()
	valid := MCPConnectorGrant{
		ProjectID: "j1234567", ConnectorOwnerID: "u1234567", ConnectorID: "k1234567",
		Version: 1, AuthorizedBy: "u1234567", Active: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid grant: %v", err)
	}

	for name, mutate := range map[string]func(*MCPConnectorGrant){
		"invalid project":              func(value *MCPConnectorGrant) { value.ProjectID = "p1234567" },
		"invalid connector owner":      func(value *MCPConnectorGrant) { value.ConnectorOwnerID = "" },
		"invalid connector":            func(value *MCPConnectorGrant) { value.ConnectorID = "bad" },
		"invalid version":              func(value *MCPConnectorGrant) { value.Version = 0 },
		"different author":             func(value *MCPConnectorGrant) { value.AuthorizedBy = "u7654321" },
		"active but revoked":           func(value *MCPConnectorGrant) { value.RevokedAt = &now },
		"inactive without revoke time": func(value *MCPConnectorGrant) { value.Active = false },
	} {
		t.Run(name, func(t *testing.T) {
			value := valid
			mutate(&value)
			if err := value.Validate(); err == nil {
				t.Fatal("expected invalid grant")
			}
		})
	}
}
