package app

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
)

type mcpConnectorStoreStub struct {
	values map[string]MCPConnectorRecord
	audits []audit.Entry
	flows  map[string]MCPConnectorOAuthFlow
}

func (s *mcpConnectorStoreStub) CreateMCPConnector(_ context.Context, value MCPConnectorRecord, entry audit.Entry) (MCPConnectorRecord, error) {
	for _, existing := range s.values {
		if existing.OwnerID == value.OwnerID && strings.EqualFold(existing.Name, value.Name) {
			return MCPConnectorRecord{}, domain.NewError(domain.CodeConflict, "MCP connector name already exists")
		}
	}
	value.Revision, value.CreatedAt, value.UpdatedAt = 1, time.Now().UTC(), time.Now().UTC()
	s.values[value.ID] = cloneMCPConnectorRecord(value)
	s.audits = append(s.audits, entry)
	return cloneMCPConnectorRecord(value), nil
}

func (s *mcpConnectorStoreStub) ListMCPConnectors(_ context.Context, ownerID string) ([]MCPConnectorRecord, error) {
	values := make([]MCPConnectorRecord, 0)
	for _, value := range s.values {
		if value.OwnerID == ownerID {
			values = append(values, cloneMCPConnectorRecord(value))
		}
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Name < values[j].Name })
	return values, nil
}

func (s *mcpConnectorStoreStub) GetMCPConnector(_ context.Context, ownerID, id string) (MCPConnectorRecord, error) {
	value, ok := s.values[id]
	if !ok || value.OwnerID != ownerID {
		return MCPConnectorRecord{}, domain.NewError(domain.CodeNotFound, "MCP connector not found")
	}
	return cloneMCPConnectorRecord(value), nil
}

func (s *mcpConnectorStoreStub) UpdateMCPConnector(_ context.Context, ownerID string, value MCPConnectorRecord, expectedRevision int64, entry audit.Entry) (MCPConnectorRecord, error) {
	current, ok := s.values[value.ID]
	if !ok || current.OwnerID != ownerID {
		return MCPConnectorRecord{}, domain.NewError(domain.CodeNotFound, "MCP connector not found")
	}
	if current.Revision != expectedRevision {
		return MCPConnectorRecord{}, domain.NewError(domain.CodeConflict, "MCP connector changed during update")
	}
	value.Revision, value.CreatedAt, value.UpdatedAt = current.Revision+1, current.CreatedAt, time.Now().UTC()
	s.values[value.ID] = cloneMCPConnectorRecord(value)
	s.audits = append(s.audits, entry)
	return cloneMCPConnectorRecord(value), nil
}

func (s *mcpConnectorStoreStub) DeleteMCPConnector(_ context.Context, ownerID, id string, expectedRevision int64, entry audit.Entry) error {
	current, ok := s.values[id]
	if !ok || current.OwnerID != ownerID {
		return domain.NewError(domain.CodeNotFound, "MCP connector not found")
	}
	if current.Revision != expectedRevision {
		return domain.NewError(domain.CodeConflict, "MCP connector changed before deletion")
	}
	delete(s.values, id)
	s.audits = append(s.audits, entry)
	return nil
}

func (s *mcpConnectorStoreStub) CreateMCPConnectorOAuthFlow(_ context.Context, flow MCPConnectorOAuthFlow) error {
	if s.flows == nil {
		s.flows = make(map[string]MCPConnectorOAuthFlow)
	}
	for _, existing := range s.flows {
		if bytes.Equal(existing.StateHash, flow.StateHash) {
			return domain.NewError(domain.CodeConflict, "duplicate OAuth state")
		}
	}
	s.flows[flow.ID] = flow
	return nil
}

func (s *mcpConnectorStoreStub) ConsumeMCPConnectorOAuthFlow(_ context.Context, stateHash []byte) (MCPConnectorOAuthFlow, error) {
	for id, flow := range s.flows {
		if bytes.Equal(flow.StateHash, stateHash) && flow.ExpiresAt.After(time.Now()) {
			delete(s.flows, id)
			return flow, nil
		}
	}
	return MCPConnectorOAuthFlow{}, domain.NewError(domain.CodeNotFound, "MCP OAuth flow not found")
}

func (s *mcpConnectorStoreStub) ReplaceMCPConnectorOAuthCredential(_ context.Context, ownerID, id string, version int64, expected, next []byte) (bool, error) {
	value, ok := s.values[id]
	if !ok || value.OwnerID != ownerID || value.CredentialVersion != version || !bytes.Equal(value.CredentialCipher, expected) {
		return false, nil
	}
	value.CredentialCipher = append([]byte(nil), next...)
	value.UpdatedAt = time.Now().UTC()
	s.values[id] = value
	return true, nil
}

func TestMCPConnectorCredentialIsBoundAndNeverReturned(t *testing.T) {
	service, store, sealer := mcpConnectorTestService(t)
	secret := []byte(`{"kind":"oauth","access_token":"connector-secret"}`)
	created, err := service.Create(context.Background(), mcpConnectorPrincipal("u1234567"), CreateMCPConnectorCommand{
		Name: "  Notion  ", EndpointURL: "https://MCP.Notion.com/mcp", CredentialJSON: secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Name != "Notion" || created.EndpointURL != "https://mcp.notion.com/mcp" || created.Enabled || !created.HasCredential ||
		created.CredentialVersion != 1 || created.Revision != 1 {
		t.Fatalf("created connector = %#v", created)
	}
	stored := store.values[created.ID]
	if bytes.Contains(stored.CredentialCipher, []byte("connector-secret")) {
		t.Fatal("credential was stored as plaintext")
	}
	plain, err := sealer.openWithAAD(stored.CredentialCipher, mcpConnectorCredentialAAD(stored.OwnerID, stored.ID, stored.CredentialVersion))
	if err != nil || !bytes.Equal(plain, secret) {
		t.Fatalf("decrypt credential = %q, %v", plain, err)
	}
	for _, aad := range [][]byte{
		mcpConnectorCredentialAAD("u7654321", stored.ID, stored.CredentialVersion),
		mcpConnectorCredentialAAD(stored.OwnerID, "k7654321", stored.CredentialVersion),
		mcpConnectorCredentialAAD(stored.OwnerID, stored.ID, stored.CredentialVersion+1),
	} {
		if _, err := sealer.openWithAAD(stored.CredentialCipher, aad); err == nil {
			t.Fatal("credential decrypted with different owner, connector, or version AAD")
		}
	}
	encoded, err := json.Marshal(created)
	if err != nil || strings.Contains(string(encoded), "connector-secret") || strings.Contains(string(encoded), "CredentialCipher") {
		t.Fatalf("view exposed connector credential: %s, %v", encoded, err)
	}
	for _, entry := range store.audits {
		encoded, err := json.Marshal(entry)
		if err != nil || strings.Contains(string(encoded), "connector-secret") || strings.Contains(string(encoded), "mcp.notion.com") {
			t.Fatalf("audit contains credential or endpoint: %s, %v", encoded, err)
		}
	}
}

func TestMCPConnectorRevisionRotationEnableAndDelete(t *testing.T) {
	service, store, sealer := mcpConnectorTestService(t)
	principal := mcpConnectorPrincipal("u1234567")
	created, err := service.Create(context.Background(), principal, CreateMCPConnectorCommand{
		Name: "Linear", EndpointURL: "https://mcp.linear.app/mcp", CredentialJSON: []byte(`{"token":"old"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := service.Update(context.Background(), principal, created.ID, UpdateMCPConnectorCommand{
		Name: "Linear Team", EndpointURL: "https://mcp.linear.app/mcp", ExpectedRevision: 1,
	})
	if err != nil || updated.Revision != 2 || updated.CredentialVersion != 1 || !updated.HasCredential {
		t.Fatalf("Update() = %#v, %v", updated, err)
	}
	enabled, err := service.SetEnabled(context.Background(), principal, created.ID, true, MCPConnectorRevisionCommand{ExpectedRevision: 2})
	if err != nil || !enabled.Enabled || enabled.Revision != 3 {
		t.Fatalf("SetEnabled(true) = %#v, %v", enabled, err)
	}
	disabled, err := service.SetEnabled(context.Background(), principal, created.ID, false, MCPConnectorRevisionCommand{ExpectedRevision: 3})
	if err != nil || disabled.Enabled || disabled.Revision != 4 {
		t.Fatalf("SetEnabled(false) = %#v, %v", disabled, err)
	}
	rotated, err := service.RotateCredential(context.Background(), principal, created.ID, RotateMCPConnectorCredentialCommand{
		CredentialJSON: []byte(`{"token":"new"}`), ExpectedRevision: 4,
	})
	if err != nil || rotated.CredentialVersion != 2 || rotated.Revision != 5 {
		t.Fatalf("RotateCredential() = %#v, %v", rotated, err)
	}
	stored := store.values[created.ID]
	plain, err := sealer.openWithAAD(stored.CredentialCipher, mcpConnectorCredentialAAD(stored.OwnerID, stored.ID, 2))
	if err != nil || string(plain) != `{"token":"new"}` {
		t.Fatalf("rotated credential = %q, %v", plain, err)
	}
	if _, err := service.Update(context.Background(), principal, created.ID, UpdateMCPConnectorCommand{
		Name: "stale", EndpointURL: "https://mcp.linear.app/mcp", ExpectedRevision: 4,
	}); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale update error = %v", err)
	}
	if err := service.Delete(context.Background(), principal, created.ID, MCPConnectorRevisionCommand{ExpectedRevision: 4}); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale delete error = %v", err)
	}
	if err := service.Delete(context.Background(), principal, created.ID, MCPConnectorRevisionCommand{ExpectedRevision: 5}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.values[created.ID]; ok || len(store.audits) != 6 {
		t.Fatalf("deleted=%t audit count=%d", !ok, len(store.audits))
	}
	for _, entry := range store.audits {
		if entry.Action == audit.ActionMCPCall {
			t.Fatal("connector management used the inbound Host MCP call audit action")
		}
	}
}

func TestMCPConnectorEndpointChangeInvalidatesCredential(t *testing.T) {
	service, store, _ := mcpConnectorTestService(t)
	principal := mcpConnectorPrincipal("u1234567")
	created, err := service.Create(context.Background(), principal, CreateMCPConnectorCommand{
		Name: "Linear", EndpointURL: "https://old.example.test/mcp", CredentialJSON: []byte(`{"Authorization":"Bearer old-token"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := service.Update(context.Background(), principal, created.ID, UpdateMCPConnectorCommand{
		Name: "Linear", EndpointURL: "https://new.example.test/mcp", ExpectedRevision: created.Revision,
	})
	if err != nil || updated.Revision != created.Revision+1 || updated.CredentialVersion != created.CredentialVersion+1 || updated.HasCredential {
		t.Fatalf("endpoint update = %#v, %v", updated, err)
	}
	stored := store.values[created.ID]
	if len(stored.CredentialCipher) != 0 || stored.CredentialVersion != updated.CredentialVersion {
		t.Fatalf("old credential remained after endpoint change: %#v", stored)
	}
	headers, err := service.CredentialHeaders(context.Background(), stored)
	if err != nil || headers.Get("Authorization") != "" {
		t.Fatalf("old credential remained usable after endpoint change: headers=%v err=%v", headers, err)
	}
	if got := store.audits[len(store.audits)-1].Detail["credential_invalidated"]; got != true {
		t.Fatalf("endpoint update audit does not record credential invalidation: %#v", store.audits[len(store.audits)-1].Detail)
	}
}

func TestMCPConnectorValidationAndOwnerIsolation(t *testing.T) {
	service, _, _ := mcpConnectorTestService(t)
	principal := mcpConnectorPrincipal("u1234567")
	for _, endpoint := range []string{
		"http://mcp.example.com", "https://user:pass@mcp.example.com/mcp", "https://mcp.example.com/mcp?token=secret",
		"https://mcp.example.com/mcp#fragment", "https://127.0.0.1/mcp", "https://mcp.example.com:8443/mcp",
		"https://mcp.example.com/a/../mcp", "https://mcp.example.com/mcp?",
	} {
		if _, err := service.Create(context.Background(), principal, CreateMCPConnectorCommand{Name: "bad", EndpointURL: endpoint}); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Errorf("Create(endpoint=%q) error = %v", endpoint, err)
		}
	}
	for _, value := range [][]byte{[]byte(`"token"`), []byte(`null`), []byte(`[]`), []byte(`{`), bytes.Repeat([]byte("x"), mcpConnectorCredentialMaxBytes+1)} {
		if _, err := service.Create(context.Background(), principal, CreateMCPConnectorCommand{
			Name: "invalid credential", EndpointURL: "https://mcp.example.com/mcp", CredentialJSON: value,
		}); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Errorf("Create(credential bytes=%d) error = %v", len(value), err)
		}
	}
	created, err := service.Create(context.Background(), principal, CreateMCPConnectorCommand{Name: "Personal", EndpointURL: "https://mcp.example.com/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if created.HasCredential || created.CredentialVersion != 0 || created.Enabled {
		t.Fatalf("unauthenticated connector default = %#v", created)
	}
	other := mcpConnectorPrincipal("u7654321")
	if _, err := service.Get(context.Background(), other, created.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner get error = %v", err)
	}
	if items, err := service.List(context.Background(), other); err != nil || len(items) != 0 {
		t.Fatalf("other owner list leaked connector: %#v", items)
	}
	if err := service.Delete(context.Background(), other, created.ID, MCPConnectorRevisionCommand{ExpectedRevision: 1}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner delete error = %v", err)
	}
	if items, err := service.List(context.Background(), principal); err != nil || len(items) != 1 {
		t.Fatalf("owner list = %#v, %v", items, err)
	}
}

func TestMCPConnectorRejectsNonSessionPrincipals(t *testing.T) {
	service, _, _ := mcpConnectorTestService(t)
	principal := mcpConnectorPrincipal("u1234567")
	for _, invalid := range []identity.Principal{
		{UserID: principal.UserID, Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t1234567"},
		{UserID: principal.UserID, Role: identity.RoleUser, Kind: identity.KindClient, TokenID: "t1234567", ClientInstallationID: "i1234567"},
		{UserID: principal.UserID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567", HostIDs: []string{"h1234567"}},
		{UserID: principal.UserID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567", Scopes: []identity.Scope{identity.ScopeComputerObserve}},
	} {
		if _, err := service.List(context.Background(), invalid); domain.CodeOf(err) != domain.CodeForbidden {
			t.Errorf("List(%#v) error = %v", invalid, err)
		}
	}
}

func mcpConnectorTestService(t *testing.T) (*MCPConnectorService, *mcpConnectorStoreStub, *Sealer) {
	t.Helper()
	sealer, err := NewSealer(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := &mcpConnectorStoreStub{values: make(map[string]MCPConnectorRecord)}
	service, err := NewMCPConnectorService(store, sealer, "https://portal.example.test/api/mcp/oauth/callback")
	if err != nil {
		t.Fatal(err)
	}
	return service, store, sealer
}

func mcpConnectorPrincipal(ownerID string) identity.Principal {
	return identity.Principal{UserID: ownerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
}

func cloneMCPConnectorRecord(value MCPConnectorRecord) MCPConnectorRecord {
	value.CredentialCipher = append([]byte(nil), value.CredentialCipher...)
	return value
}
