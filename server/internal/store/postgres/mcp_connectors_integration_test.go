package postgres

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
)

func TestMCPConnectorOwnerIsolationRevisionAndAudit(t *testing.T) {
	store, _, _, owner := clientLoginFixture(t)
	other := createTestUser(t, store, 1, 10_000, 20_000)
	ctx := context.Background()
	sealer, err := app.NewSealer(bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewMCPConnectorService(store, sealer, "https://portal.example.test/api/mcp/oauth/callback")
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: owner.ID, Role: owner.Role, Kind: identity.KindSession, TokenID: "t1234567"}
	if _, err := store.pool.Exec(ctx, `CREATE FUNCTION reject_mcp_connector_audit() RETURNS trigger AS $$
BEGIN
  IF NEW.action LIKE 'mcp.connector.%' THEN RAISE EXCEPTION 'test audit rejection'; END IF;
  RETURN NEW;
END
$$ LANGUAGE plpgsql`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `CREATE TRIGGER reject_mcp_connector_audit BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_mcp_connector_audit()`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ctx, principal, app.CreateMCPConnectorCommand{
		Name: "Rejected", EndpointURL: "https://mcp.example.com/mcp", CredentialJSON: []byte(`{"token":"rollback-secret"}`),
	}); err == nil {
		t.Fatal("connector creation succeeded when its audit insert failed")
	}
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM mcp_connectors WHERE owner_id=$1`, owner.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("connector remained after audit failure: count=%d err=%v", count, err)
	}
	if _, err := store.pool.Exec(ctx, `DROP TRIGGER reject_mcp_connector_audit ON audit_logs`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `DROP FUNCTION reject_mcp_connector_audit()`); err != nil {
		t.Fatal(err)
	}
	credential := []byte(`{"kind":"oauth","access_token":"postgres-secret"}`)
	created, err := service.Create(ctx, principal, app.CreateMCPConnectorCommand{
		Name: "Notion", EndpointURL: "https://mcp.notion.com/mcp", CredentialJSON: credential,
	})
	if err != nil || created.Enabled || created.Revision != 1 || created.CredentialVersion != 1 || !created.HasCredential {
		t.Fatalf("Create() = %#v, %v", created, err)
	}
	var cipher []byte
	if err := store.pool.QueryRow(ctx, `SELECT credential_cipher FROM mcp_connectors WHERE id=$1 AND owner_id=$2`, created.ID, owner.ID).Scan(&cipher); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(cipher, []byte("postgres-secret")) || len(cipher) < 29 {
		t.Fatal("database credential is not encrypted")
	}
	if items, err := service.List(ctx, principal); err != nil || len(items) != 1 || items[0].ID != created.ID {
		t.Fatalf("owner List() = %#v, %v", items, err)
	}
	if items, err := service.List(ctx, identity.Principal{UserID: other.ID, Role: other.Role, Kind: identity.KindSession}); err != nil || len(items) != 0 {
		t.Fatalf("other owner List() = %#v, %v", items, err)
	}
	if _, err := store.GetMCPConnector(ctx, other.ID, created.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner store Get() = %v", err)
	}
	updated, err := service.Update(ctx, principal, created.ID, app.UpdateMCPConnectorCommand{
		Name: "Notion Work", EndpointURL: "https://mcp.notion.com/mcp/v2", ExpectedRevision: 1,
	})
	if err != nil || updated.Revision != 2 || updated.CredentialVersion != 2 || updated.HasCredential {
		t.Fatalf("Update() = %#v, %v", updated, err)
	}
	reloaded, err := store.GetMCPConnector(ctx, owner.ID, created.ID)
	if err != nil || len(reloaded.CredentialCipher) != 0 || reloaded.CredentialVersion != updated.CredentialVersion ||
		reloaded.EndpointURL != updated.EndpointURL || reloaded.Revision != updated.Revision {
		t.Fatalf("connector tombstone did not survive a store reload: %#v, %v", reloaded, err)
	}
	restartedService, err := app.NewMCPConnectorService(store, sealer, "https://portal.example.test/api/mcp/oauth/callback")
	if err != nil {
		t.Fatal(err)
	}
	if headers, err := restartedService.CredentialHeaders(ctx, reloaded); err != nil || headers.Get("Authorization") != "" {
		t.Fatalf("cleared credential became usable after reload: headers=%v err=%v", headers, err)
	}
	enabled, err := service.SetEnabled(ctx, principal, created.ID, true, app.MCPConnectorRevisionCommand{ExpectedRevision: 2})
	if err != nil || !enabled.Enabled || enabled.Revision != 3 {
		t.Fatalf("SetEnabled() = %#v, %v", enabled, err)
	}
	if _, err := service.RotateCredential(ctx, principal, created.ID, app.RotateMCPConnectorCredentialCommand{
		CredentialJSON: []byte(`{"kind":"oauth","access_token":"rotated-secret"}`), ExpectedRevision: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetEnabled(ctx, principal, created.ID, false, app.MCPConnectorRevisionCommand{ExpectedRevision: 3}); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale enable change = %v", err)
	}
	latest, err := service.Get(ctx, principal, created.ID)
	if err != nil || latest.Revision != 4 || latest.CredentialVersion != 3 || !latest.Enabled {
		t.Fatalf("Get() after rotate = %#v, %v", latest, err)
	}
	if err := service.Delete(ctx, principal, created.ID, app.MCPConnectorRevisionCommand{ExpectedRevision: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(ctx, principal, created.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("Get() after delete = %v", err)
	}
	rows, err := store.pool.Query(ctx, `SELECT action, detail::text FROM audit_logs WHERE target=$1 ORDER BY id`, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var action, detail string
		if err := rows.Scan(&action, &detail); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, action)
		if strings.Contains(detail, "mcp.notion.com") || strings.Contains(detail, "postgres-secret") || strings.Contains(detail, "rotated-secret") {
			t.Fatalf("connector audit detail contains endpoint or credential: %s", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		string(audit.ActionMCPConnectorCreate), string(audit.ActionMCPConnectorUpdate),
		string(audit.ActionMCPConnectorEnable), string(audit.ActionMCPConnectorCredentialRotate), string(audit.ActionMCPConnectorDelete),
	}
	if len(actions) != len(want) {
		t.Fatalf("audit actions = %v, want %v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("audit actions = %v, want %v", actions, want)
		}
	}
}
