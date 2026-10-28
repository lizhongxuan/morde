package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func TestMCPConnectorOAuthFlowIsSingleUseAndRefreshCredentialUsesCAS(t *testing.T) {
	store, _, _, owner := clientLoginFixture(t)
	ctx := context.Background()
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewMCPConnectorService(store, sealer, "https://portal.example.test/api/mcp/oauth/callback")
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: owner.ID, Role: owner.Role, Kind: identity.KindSession, TokenID: "oauth-test"}
	connector, err := service.Create(ctx, principal, app.CreateMCPConnectorCommand{
		Name: "OAuth connector", EndpointURL: "https://mcp.example.test/mcp", CredentialJSON: []byte(`{"token":"old-token"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	state := sha256.Sum256([]byte("single-use-oauth-state"))
	flow := app.MCPConnectorOAuthFlow{
		ID: "oabcdefghijklmnopqrstuvwx", OwnerID: owner.ID, ConnectorID: connector.ID, ConnectorRevision: connector.Revision,
		StateHash: state[:], PayloadCipher: bytes.Repeat([]byte{0x31}, 48), ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	if err := store.CreateMCPConnectorOAuthFlow(ctx, flow); err != nil {
		t.Fatalf("create OAuth flow: %v", err)
	}
	consumed, err := store.ConsumeMCPConnectorOAuthFlow(ctx, state[:])
	if err != nil || consumed.ID != flow.ID || consumed.ConnectorRevision != connector.Revision || !bytes.Equal(consumed.PayloadCipher, flow.PayloadCipher) {
		t.Fatalf("consume OAuth flow = %#v, %v", consumed, err)
	}
	if _, err := store.ConsumeMCPConnectorOAuthFlow(ctx, state[:]); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("OAuth state replay error = %v", err)
	}

	record, err := store.GetMCPConnector(ctx, owner.ID, connector.ID)
	if err != nil || record.CredentialVersion != 1 || len(record.CredentialCipher) < 29 {
		t.Fatalf("read OAuth credential = %#v, %v", record, err)
	}
	start := make(chan struct{})
	type refreshResult struct {
		saved bool
		err   error
	}
	results := make(chan refreshResult, 2)
	var workers sync.WaitGroup
	for _, nextCipher := range [][]byte{bytes.Repeat([]byte{0x51}, 48), bytes.Repeat([]byte{0x61}, 48)} {
		workers.Add(1)
		go func(next []byte) {
			defer workers.Done()
			<-start
			saved, err := store.ReplaceMCPConnectorOAuthCredential(ctx, owner.ID, connector.ID,
				record.CredentialVersion, record.CredentialCipher, next)
			results <- refreshResult{saved: saved, err: err}
		}(nextCipher)
	}
	close(start)
	workers.Wait()
	close(results)
	savedCount := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("CAS refresh: %v", result.err)
		}
		if result.saved {
			savedCount++
		}
	}
	if savedCount != 1 {
		t.Fatalf("successful concurrent credential refreshes = %d, want 1", savedCount)
	}
	latest, err := store.GetMCPConnector(ctx, owner.ID, connector.ID)
	if err != nil || len(latest.CredentialCipher) != 48 || bytes.Equal(latest.CredentialCipher, record.CredentialCipher) {
		t.Fatalf("credential after CAS refresh = %#v, %v", latest, err)
	}
}
