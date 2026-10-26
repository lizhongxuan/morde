package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func TestClientAuthorizationAtomicClaimRotationAndRevocation(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 2, 10_000, 20_000)
	workspace := createTestWorkspace(t, ctx, store, owner.ID, "paired-sync")
	authID, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	deviceCode, secretHash, err := identity.NewCredential(identity.KindClient, authID)
	if err != nil {
		t.Fatal(err)
	}
	_, _, secret, err := identity.ParseCredential(deviceCode)
	if err != nil || !identity.VerifySecret(secret, secretHash) {
		t.Fatal("invalid pairing fixture")
	}
	codeHash := sha256.Sum256([]byte("ABCDEFGH"))
	if err := store.CreateClientAuthorization(ctx, identity.ClientAuthorization{ID: authID, DisplayName: "desktop", Platform: "desktop",
		WantsWorkspaceSync: true, SecretHash: secretHash, UserCodeHash: codeHash[:], ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	installID, err := domain.NewClientInstallationID()
	if err != nil {
		t.Fatal(err)
	}
	approvedExpiry := time.Now().UTC().Add(30 * 24 * time.Hour)
	installation, err := store.ApproveClientAuthorization(ctx, codeHash[:], owner.ID, []string{workspace.ID}, installID, "127.0.0.1", approvedExpiry)
	if err != nil || installation.ID != installID {
		t.Fatalf("approve = %#v %v", installation, err)
	}
	listed, err := store.ListClientInstallations(ctx, owner.ID)
	if err != nil || len(listed) != 1 || !listed[0].ExpiresAt.Equal(installation.ExpiresAt) || !installation.ExpiresAt.Equal(approvedExpiry) ||
		!equalStringSlices(listed[0].WorkspaceIDs, []string{workspace.ID}) {
		t.Fatalf("approved installation/list = %#v %#v %v", installation, listed, err)
	}
	clientToken := clientTestToken(t, owner.ID, installID, identity.KindClient, nil)
	syncToken := clientTestToken(t, owner.ID, installID, identity.KindDevice, []string{workspace.ID})
	clientToken.ExpiresAt, syncToken.ExpiresAt = installation.ExpiresAt, installation.ExpiresAt
	wrongExpiry := clientToken
	wrongExpiry.ExpiresAt = clientToken.ExpiresAt.Add(time.Hour)
	wrongSyncExpiry := syncToken
	wrongSyncExpiry.ExpiresAt = wrongExpiry.ExpiresAt
	if _, err := store.ClaimClientAuthorization(ctx, authID, secretHash, wrongExpiry, &wrongSyncExpiry); !errors.Is(err, identity.ErrInvalidCredential) {
		t.Fatalf("claim accepted mismatched installation expiry: %v", err)
	}
	var wg sync.WaitGroup
	claimResults := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.ClaimClientAuthorization(ctx, authID, secretHash, clientToken, &syncToken)
			claimResults <- err
		}()
	}
	wg.Wait()
	close(claimResults)
	successes := 0
	for err := range claimResults {
		if err == nil {
			successes++
		} else if !errors.Is(err, identity.ErrInvalidCredential) {
			t.Fatalf("claim error = %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("claim successes = %d", successes)
	}
	client, err := store.GetTokenByID(ctx, clientToken.ID)
	if err != nil || client.ClientInstallationID != installID || client.Kind != identity.KindClient {
		t.Fatalf("client token = %#v %v", client, err)
	}
	device, err := store.GetTokenByID(ctx, syncToken.ID)
	if err != nil || device.ClientInstallationID != installID || device.Kind != identity.KindDevice {
		t.Fatalf("sync token = %#v %v", device, err)
	}
	active, err := store.ActiveClientInstallation(ctx, installID, owner.ID)
	if err != nil || !active {
		t.Fatalf("active = %v %v", active, err)
	}

	renewalID, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	_, renewalHash, err := identity.NewCredential(identity.KindClient, renewalID)
	if err != nil {
		t.Fatal(err)
	}
	renewalCodeHash := sha256.Sum256([]byte("BCDEFGHJ"))
	if err := store.CreateClientAuthorization(ctx, identity.ClientAuthorization{ID: renewalID, DisplayName: "renamed desktop",
		Platform: "desktop", WantsWorkspaceSync: true, RenewalInstallationID: installID,
		SecretHash: renewalHash, UserCodeHash: renewalCodeHash[:], ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	renewedExpiry := time.Now().UTC().Add(30 * 24 * time.Hour)
	renewed, err := store.ApproveClientAuthorization(ctx, renewalCodeHash[:], owner.ID, []string{}, "i7654321", "127.0.0.1", renewedExpiry)
	if err != nil || renewed.ID != installID || !renewed.ExpiresAt.Equal(renewedExpiry) {
		t.Fatalf("approve renewal = %#v %v", renewed, err)
	}
	listed, err = store.ListClientInstallations(ctx, owner.ID)
	if err != nil || len(listed) != 1 || listed[0].DisplayName != renewed.DisplayName ||
		!listed[0].ExpiresAt.Equal(renewed.ExpiresAt) || len(listed[0].WorkspaceIDs) != 0 {
		t.Fatalf("renewal approval differs from list: %#v %#v %v", renewed, listed, err)
	}
	oldClient, err := store.GetTokenByID(ctx, clientToken.ID)
	if err != nil || oldClient.RevokedAt == nil {
		t.Fatalf("renewal approval did not revoke old client credential before claim: %#v %v", oldClient, err)
	}
	oldSync, err := store.GetTokenByID(ctx, syncToken.ID)
	if err != nil || oldSync.RevokedAt == nil {
		t.Fatalf("renewal approval did not revoke old sync credential before claim: %#v %v", oldSync, err)
	}
	renewedClient := clientTestToken(t, owner.ID, installID, identity.KindClient, nil)
	renewedSync := clientTestToken(t, owner.ID, installID, identity.KindDevice, nil)
	renewedClient.ExpiresAt, renewedSync.ExpiresAt = renewed.ExpiresAt, renewed.ExpiresAt
	if renewed, err := store.ClaimClientAuthorization(ctx, renewalID, renewalHash, renewedClient, &renewedSync); err != nil || renewed.ID != installID || renewed.DisplayName != "renamed desktop" {
		t.Fatalf("claim renewal = %#v %v", renewed, err)
	}
	if err := store.RevokeClientInstallation(ctx, installID, owner.ID, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	active, err = store.ActiveClientInstallation(ctx, installID, owner.ID)
	if err != nil || active {
		t.Fatalf("after revoke = %v %v", active, err)
	}
	client, _ = store.GetTokenByID(ctx, renewedClient.ID)
	device, _ = store.GetTokenByID(ctx, renewedSync.ID)
	if client.RevokedAt == nil || device.RevokedAt == nil {
		t.Fatalf("credentials not revoked: %#v %#v", client, device)
	}
}

func TestClientAuthorizationRenewalRejectsNonOwner(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 2, 10_000, 20_000)
	other := createTestUser(t, store, 2, 10_000, 20_000)
	installationID := createTestClientInstallation(t, pool, owner.ID, nil)
	installationBefore, err := store.GetClientInstallation(ctx, installationID)
	if err != nil {
		t.Fatal(err)
	}
	oldClient := clientTestToken(t, owner.ID, installationID, identity.KindClient, nil)
	oldDevice := clientTestToken(t, owner.ID, installationID, identity.KindDevice, nil)
	if _, err := store.CreateToken(ctx, oldClient); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateToken(ctx, oldDevice); err != nil {
		t.Fatal(err)
	}

	authorizationID, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	_, secretHash, err := identity.NewCredential(identity.KindClient, authorizationID)
	if err != nil {
		t.Fatal(err)
	}
	codeHash := sha256.Sum256([]byte("DEFGHJKM"))
	if err := store.CreateClientAuthorization(ctx, identity.ClientAuthorization{
		ID: authorizationID, DisplayName: "attacker renewal", Platform: "desktop", WantsWorkspaceSync: true,
		RenewalInstallationID: installationID, SecretHash: secretHash, UserCodeHash: codeHash[:], ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	newID, err := domain.NewClientInstallationID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApproveClientAuthorization(ctx, codeHash[:], other.ID, []string{}, newID, "127.0.0.1", time.Now().UTC().Add(30*24*time.Hour)); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("non-owner renewal approval error = %v, want not found", err)
	}

	pairing, err := store.GetClientAuthorization(ctx, authorizationID)
	if err != nil || pairing.State != "pending" || pairing.InstallationID != "" || pairing.OwnerID != "" {
		t.Fatalf("pairing after non-owner approval = %#v, %v", pairing, err)
	}
	installationAfter, err := store.GetClientInstallation(ctx, installationID)
	if err != nil || installationAfter.OwnerID != installationBefore.OwnerID || installationAfter.DisplayName != installationBefore.DisplayName ||
		installationAfter.Platform != installationBefore.Platform || !installationAfter.ExpiresAt.Equal(installationBefore.ExpiresAt) ||
		!equalStringSlices(installationAfter.WorkspaceIDs, installationBefore.WorkspaceIDs) || installationAfter.RevokedAt != nil {
		t.Fatalf("installation changed after non-owner approval: before=%#v after=%#v err=%v", installationBefore, installationAfter, err)
	}
	for _, id := range []string{oldClient.ID, oldDevice.ID} {
		credential, err := store.GetTokenByID(ctx, id)
		if err != nil || credential.RevokedAt != nil {
			t.Fatalf("credential %s after non-owner approval = %#v %v", id, credential, err)
		}
	}
	assertClientAuditCount(t, pool, "client.pair", installationID, 0)
}

func TestClientAuthorizationRenewalAuditFailureRollsBackCredentials(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 2, 10_000, 20_000)
	installationID := createTestClientInstallation(t, pool, owner.ID, nil)
	installationBefore, err := store.GetClientInstallation(ctx, installationID)
	if err != nil {
		t.Fatal(err)
	}
	oldClient := clientTestToken(t, owner.ID, installationID, identity.KindClient, nil)
	oldDevice := clientTestToken(t, owner.ID, installationID, identity.KindDevice, nil)
	if _, err := store.CreateToken(ctx, oldClient); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateToken(ctx, oldDevice); err != nil {
		t.Fatal(err)
	}

	authorizationID, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	_, secretHash, err := identity.NewCredential(identity.KindClient, authorizationID)
	if err != nil {
		t.Fatal(err)
	}
	codeHash := sha256.Sum256([]byte("EFGHJKLM"))
	if err := store.CreateClientAuthorization(ctx, identity.ClientAuthorization{
		ID: authorizationID, DisplayName: "renewed desktop", Platform: "desktop", WantsWorkspaceSync: true,
		RenewalInstallationID: installationID, SecretHash: secretHash, UserCodeHash: codeHash[:], ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	installClientAuditFailure(t, pool)
	_, approvalErr := store.ApproveClientAuthorization(ctx, codeHash[:], owner.ID, []string{}, installationID, "127.0.0.1", time.Now().UTC().Add(30*24*time.Hour))
	removeClientAuditFailure(t, pool)
	if approvalErr == nil || !strings.Contains(approvalErr.Error(), "injected client audit failure") {
		t.Fatalf("renewal error = %v, want injected audit failure", approvalErr)
	}

	pairing, err := store.GetClientAuthorization(ctx, authorizationID)
	if err != nil || pairing.State != "pending" || pairing.InstallationID != "" || pairing.OwnerID != "" {
		t.Fatalf("pairing after audit failure = %#v, %v", pairing, err)
	}
	installationAfter, err := store.GetClientInstallation(ctx, installationID)
	if err != nil || installationAfter.OwnerID != installationBefore.OwnerID || installationAfter.DisplayName != installationBefore.DisplayName ||
		installationAfter.Platform != installationBefore.Platform || !installationAfter.ExpiresAt.Equal(installationBefore.ExpiresAt) ||
		!equalStringSlices(installationAfter.WorkspaceIDs, installationBefore.WorkspaceIDs) || installationAfter.RevokedAt != nil {
		t.Fatalf("installation after audit failure = before %#v, after %#v, err %v", installationBefore, installationAfter, err)
	}
	active, err := store.ActiveClientInstallation(ctx, installationID, owner.ID)
	if err != nil || !active {
		t.Fatalf("installation active after audit failure = %v %v", active, err)
	}
	for _, id := range []string{oldClient.ID, oldDevice.ID} {
		credential, err := store.GetTokenByID(ctx, id)
		if err != nil || credential.RevokedAt != nil {
			t.Fatalf("credential %s after audit failure = %#v %v", id, credential, err)
		}
	}
	assertClientAuditCount(t, pool, "client.pair", installationID, 0)
}

func TestClientAuthorizationClaimDeviceTokenFailureRollsBackBothCredentials(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 2, 10_000, 20_000)
	authorizationID, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	_, secretHash, err := identity.NewCredential(identity.KindClient, authorizationID)
	if err != nil {
		t.Fatal(err)
	}
	codeHash := sha256.Sum256([]byte("FGHJKLMN"))
	if err := store.CreateClientAuthorization(ctx, identity.ClientAuthorization{
		ID: authorizationID, DisplayName: "desktop with sync", Platform: "desktop", WantsWorkspaceSync: true,
		SecretHash: secretHash, UserCodeHash: codeHash[:], ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	installationID, err := domain.NewClientInstallationID()
	if err != nil {
		t.Fatal(err)
	}
	installation, err := store.ApproveClientAuthorization(ctx, codeHash[:], owner.ID, []string{}, installationID, "127.0.0.1", time.Now().UTC().Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	clientToken := clientTestToken(t, owner.ID, installationID, identity.KindClient, nil)
	deviceToken := clientTestToken(t, owner.ID, installationID, identity.KindDevice, nil)
	clientToken.ExpiresAt, deviceToken.ExpiresAt = installation.ExpiresAt, installation.ExpiresAt
	installDeviceTokenInsertFailure(t, pool)
	_, claimErr := store.ClaimClientAuthorization(ctx, authorizationID, secretHash, clientToken, &deviceToken)
	removeDeviceTokenInsertFailure(t, pool)
	if claimErr == nil || !strings.Contains(claimErr.Error(), "injected device token insert failure") {
		t.Fatalf("claim error = %v, want injected device token insert failure", claimErr)
	}

	pairing, err := store.GetClientAuthorization(ctx, authorizationID)
	if err != nil || pairing.State != "approved" || pairing.InstallationID != installationID || pairing.OwnerID != owner.ID {
		t.Fatalf("pairing after partial claim failure = %#v, %v", pairing, err)
	}
	for _, id := range []string{clientToken.ID, deviceToken.ID} {
		if _, err := store.GetTokenByID(ctx, id); domain.CodeOf(err) != domain.CodeNotFound {
			t.Fatalf("credential %s persisted after partial claim failure: %v", id, err)
		}
	}
	active, err := store.ActiveClientInstallation(ctx, installationID, owner.ID)
	if err != nil || !active {
		t.Fatalf("installation after partial claim failure = active %v, err %v", active, err)
	}
}

func installDeviceTokenInsertFailure(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `CREATE FUNCTION reject_client_device_token() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.kind = 'device' THEN RAISE EXCEPTION 'injected device token insert failure'; END IF;
  RETURN NEW;
END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `CREATE TRIGGER reject_client_device_token
BEFORE INSERT ON tokens FOR EACH ROW EXECUTE FUNCTION reject_client_device_token()`); err != nil {
		t.Fatal(err)
	}
}

func removeDeviceTokenInsertFailure(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DROP TRIGGER reject_client_device_token ON tokens`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `DROP FUNCTION reject_client_device_token()`); err != nil {
		t.Fatal(err)
	}
}

func clientTestToken(t *testing.T, ownerID, installID string, kind identity.Kind, grants []string) identity.Token {
	t.Helper()
	id, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	_, hash, err := identity.NewCredential(kind, id)
	if err != nil {
		t.Fatal(err)
	}
	value := identity.Token{ID: id, OwnerID: ownerID, ClientInstallationID: installID, Kind: kind, SecretHash: hash, ExpiresAt: time.Now().Add(30 * 24 * time.Hour)}
	if kind == identity.KindDevice {
		value.Scopes = []identity.Scope{identity.ScopeWorkspaceSync}
		value.WorkspaceIDs = append([]string{}, grants...)
	}
	return value
}

func createTestClientInstallation(t *testing.T, pool *pgxpool.Pool, ownerID string, grants []string) string {
	t.Helper()
	id, err := domain.NewClientInstallationID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO client_installations
  (id,owner_id,display_name,platform,workspace_ids,expires_at)
  VALUES ($1,$2,'test desktop','desktop',$3,now()+interval '30 days')`, id, ownerID, append([]string{}, grants...)); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestMigrationRetiresUnlinkedDeviceTokens(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.Down(ctx); err != nil {
		t.Fatal(err)
	}
	owner := createTestUser(t, store, 2, 10_000, 20_000)
	legacy := clientTestToken(t, owner.ID, "", identity.KindDevice, nil)
	if _, err := store.CreateToken(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	installationID := createTestClientInstallation(t, pool, owner.ID, nil)
	bound := clientTestToken(t, owner.ID, installationID, identity.KindDevice, nil)
	if _, err := store.CreateToken(ctx, bound); err != nil {
		t.Fatal(err)
	}
	approvedID, err := domain.NewClientInstallationID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO client_installations
  (id,owner_id,display_name,platform,workspace_ids) VALUES ($1,$2,'old name','desktop','{}'::text[])`, approvedID, owner.ID); err != nil {
		t.Fatal(err)
	}
	pairingID, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	_, pairingHash, err := identity.NewCredential(identity.KindClient, pairingID)
	if err != nil {
		t.Fatal(err)
	}
	pairingCodeHash := sha256.Sum256([]byte("CDEFGHJK"))
	if err := store.CreateClientAuthorization(ctx, identity.ClientAuthorization{ID: pairingID, DisplayName: "approved desktop",
		Platform: "desktop", WantsWorkspaceSync: true, SecretHash: pairingHash, UserCodeHash: pairingCodeHash[:],
		ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE client_authorizations SET state='approved',installation_id=$2,owner_id=$3,
  workspace_ids=ARRAY['w1234567']::text[] WHERE id=$1`, pairingID, approvedID, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	approved, err := store.GetClientInstallation(ctx, approvedID)
	if err != nil || approved.DisplayName != "approved desktop" || !approved.ExpiresAt.After(time.Now()) ||
		!equalStringSlices(approved.WorkspaceIDs, []string{"w1234567"}) {
		t.Fatalf("unclaimed approval backfill = %#v %v", approved, err)
	}
	retired, err := store.GetTokenByID(ctx, legacy.ID)
	if err != nil || retired.RevokedAt == nil {
		t.Fatalf("legacy device token = %#v %v", retired, err)
	}
	active, err := store.GetTokenByID(ctx, bound.ID)
	if err != nil || active.RevokedAt != nil {
		t.Fatalf("bound device token = %#v %v", active, err)
	}
	if _, err := store.CreateToken(ctx, clientTestToken(t, owner.ID, "", identity.KindDevice, nil)); err == nil {
		t.Fatal("new unlinked device token passed database constraint")
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatal(err)
	}
	retired, err = store.GetTokenByID(ctx, legacy.ID)
	if err != nil || retired.RevokedAt == nil {
		t.Fatalf("rollback reactivated retired device token: %#v %v", retired, err)
	}
}

func TestMigrationBackfillsClientTaskInstallationSnapshot(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("roll back task installation snapshot migration: %v", err)
	}

	owner := createTestUser(t, store, 2, 10_000, 20_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	installationID := createTestClientInstallation(t, pool, owner.ID, nil)
	clientToken := clientTestToken(t, owner.ID, installationID, identity.KindClient, nil)
	if _, err := store.CreateToken(ctx, clientToken); err != nil {
		t.Fatalf("create client token: %v", err)
	}
	conversationID := mustID(t, domain.NewConversationID)
	if _, err := pool.Exec(ctx, `INSERT INTO conversations (id,owner_id,host_id,title) VALUES ($1,$2,$3,'pre-migration')`,
		conversationID, owner.ID, hostValue.ID); err != nil {
		t.Fatalf("create pre-migration conversation: %v", err)
	}
	taskID := mustID(t, domain.NewTaskID)
	if _, err := pool.Exec(ctx, `INSERT INTO tasks (
  id, owner_id, conversation_id, host_id, model_id, objective, status, auth_kind, auth_token_id, auth_scopes,
  budget_active_seconds, budget_tool_calls, budget_model_tokens
) VALUES ($1,$2,$3,$4,'model-test','pre-migration client task','queued','client',$5,'{}',1800,100,50000)`,
		taskID, owner.ID, conversationID, hostValue.ID, clientToken.ID); err != nil {
		t.Fatalf("create pre-migration client task: %v", err)
	}

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply task installation snapshot migration: %v", err)
	}
	loaded, err := store.GetTask(ctx, taskID, owner.ID)
	if err != nil || loaded.Task.Authorization.ClientInstallationID != installationID {
		t.Fatalf("backfilled client task authorization = %#v, err=%v", loaded.Task.Authorization, err)
	}
}
