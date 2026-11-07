package postgres

import (
	"context"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

func TestPostgresWorkspaceShareClaimIsSingleUse(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	first := createTestUser(t, store, 3, 10_000, 20_000)
	second := createTestUser(t, store, 3, 10_000, 20_000)
	source := createTestWorkspace(t, ctx, store, owner.ID, "workspace share source")
	target := createTestWorkspace(t, ctx, store, owner.ID, "workspace share copy")
	shareID, err := domain.NewPendingRequestID()
	if err != nil {
		t.Fatal(err)
	}
	tokenHash := sha256.Sum256([]byte("local-only share token"))
	link, err := store.CreateWorkspaceShare(ctx, workspace.ShareLink{
		ID: shareID, OwnerID: owner.ID, SourceWorkspaceID: source.ID, SourceRevision: source.Revision,
		Permission: workspace.ShareEdit, TokenHash: tokenHash[:], ExpiresAt: time.Now().Add(time.Hour),
		State: workspace.SharePending,
	})
	if err != nil {
		t.Fatal(err)
	}
	if link.WorkspaceName != source.Name || len(link.TokenHash) != sha256.Size {
		t.Fatalf("created share = %#v", link)
	}

	start := make(chan struct{})
	results := make(chan struct {
		claimant string
		link     workspace.ShareLink
		err      error
	}, 2)
	var workers sync.WaitGroup
	for _, userID := range []string{first.ID, second.ID} {
		workers.Add(1)
		go func(userID string) {
			defer workers.Done()
			<-start
			claimed, claimErr := store.StartWorkspaceShareClaim(ctx, tokenHash[:], userID)
			results <- struct {
				claimant string
				link     workspace.ShareLink
				err      error
			}{claimant: userID, link: claimed, err: claimErr}
		}(userID)
	}
	close(start)
	workers.Wait()
	close(results)
	var winner string
	for result := range results {
		if result.err == nil {
			if winner != "" || result.link.State != workspace.ShareClaiming || result.link.ClaimantID != result.claimant {
				t.Fatalf("unexpected concurrent claim success: %#v", result)
			}
			winner = result.claimant
		} else if domain.CodeOf(result.err) != domain.CodeNotFound {
			t.Fatalf("concurrent claim error = %v, want not found", result.err)
		}
	}
	if winner == "" {
		t.Fatal("neither recipient claimed the link")
	}
	if _, err := store.StartWorkspaceShareClaim(ctx, tokenHash[:], winner); err != nil {
		t.Fatalf("claim retry for reserved recipient failed: %v", err)
	}
	if err := store.SetWorkspaceShareTarget(ctx, shareID, winner, target.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteWorkspaceShareClaim(ctx, shareID, winner, target.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.GetWorkspaceShareByToken(ctx, tokenHash[:])
	if err != nil || claimed.State != workspace.ShareClaimed || claimed.TargetWorkspaceID != target.ID || claimed.ClaimantID != winner {
		t.Fatalf("completed share = %#v, err=%v", claimed, err)
	}
	if _, err := store.StartWorkspaceShareClaim(ctx, tokenHash[:], winner); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("claim after successful acceptance error = %v, want not found", err)
	}
	var storedHash []byte
	if err := store.pool.QueryRow(ctx, `SELECT token_hash FROM workspace_share_links WHERE id=$1`, shareID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if len(storedHash) != sha256.Size || storedHash == nil {
		t.Fatalf("stored token hash length = %d", len(storedHash))
	}
}

func TestPostgresWorkspaceShareExpiredUnclaimedLinkCannotBeClaimed(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	recipient := createTestUser(t, store, 3, 10_000, 20_000)
	source := createTestWorkspace(t, ctx, store, owner.ID, "expired workspace share source")
	shareID, err := domain.NewPendingRequestID()
	if err != nil {
		t.Fatal(err)
	}
	tokenHash := sha256.Sum256([]byte("local-only expired share token"))
	if _, err := store.CreateWorkspaceShare(ctx, workspace.ShareLink{
		ID: shareID, OwnerID: owner.ID, SourceWorkspaceID: source.ID, SourceRevision: source.Revision,
		Permission: workspace.ShareView, TokenHash: tokenHash[:], ExpiresAt: time.Now().Add(-time.Minute),
		State: workspace.SharePending,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetWorkspaceShareByToken(ctx, tokenHash[:]); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("expired link preview error = %v, want not found", err)
	}
	if _, err := store.StartWorkspaceShareClaim(ctx, tokenHash[:], recipient.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("expired link claim error = %v, want not found", err)
	}
}
