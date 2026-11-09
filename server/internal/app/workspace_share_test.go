package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"sync"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type workspaceShareTestStore struct {
	mu            sync.Mutex
	link          workspace.ShareLink
	sourceCurrent bool
}

func (s *workspaceShareTestStore) CreateWorkspaceShare(_ context.Context, value workspace.ShareLink) (workspace.ShareLink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value.WorkspaceName = "Ready Workspace"
	value.CreatedAt = time.Now().UTC()
	s.link = value
	return value, nil
}

func (s *workspaceShareTestStore) GetWorkspaceShareByToken(_ context.Context, tokenHash []byte) (workspace.ShareLink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.link.ID == "" || len(tokenHash) != len(s.link.TokenHash) || subtle.ConstantTimeCompare(tokenHash, s.link.TokenHash) != 1 {
		return workspace.ShareLink{}, domain.NewError(domain.CodeNotFound, "Workspace share link not found")
	}
	return s.link, nil
}

func (s *workspaceShareTestStore) StartWorkspaceShareClaim(_ context.Context, tokenHash []byte, claimantID string) (workspace.ShareLink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.link.ID == "" || subtle.ConstantTimeCompare(tokenHash, s.link.TokenHash) != 1 || !time.Now().Before(s.link.ExpiresAt) {
		return workspace.ShareLink{}, domain.NewError(domain.CodeNotFound, "Workspace share link not found")
	}
	if s.link.State == workspace.SharePending {
		if claimantID == s.link.OwnerID {
			return workspace.ShareLink{}, domain.NewError(domain.CodeNotFound, "Workspace share link not found")
		}
		s.link.State, s.link.ClaimantID = workspace.ShareClaiming, claimantID
	} else if s.link.State != workspace.ShareClaiming || s.link.ClaimantID != claimantID {
		return workspace.ShareLink{}, domain.NewError(domain.CodeNotFound, "Workspace share link not found")
	}
	return s.link, nil
}

func (s *workspaceShareTestStore) SetWorkspaceShareTarget(_ context.Context, id, claimantID, targetID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.link.ID != id || s.link.State != workspace.ShareClaiming || s.link.ClaimantID != claimantID {
		return domain.NewError(domain.CodeNotFound, "Workspace share link not found")
	}
	s.link.TargetWorkspaceID = targetID
	return nil
}

func (s *workspaceShareTestStore) CompleteWorkspaceShareClaim(_ context.Context, id, claimantID, targetID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.link.ID != id || s.link.State != workspace.ShareClaiming || s.link.ClaimantID != claimantID || s.link.TargetWorkspaceID != targetID {
		return domain.NewError(domain.CodeNotFound, "Workspace share link not found")
	}
	claimedAt := time.Now().UTC()
	s.link.State, s.link.ClaimedAt = workspace.ShareClaimed, &claimedAt
	return nil
}

func (s *workspaceShareTestStore) InvalidateWorkspaceShare(_ context.Context, id, claimantID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.link.ID != id || s.link.State != workspace.ShareClaiming || s.link.ClaimantID != claimantID || s.link.TargetWorkspaceID != "" {
		return domain.NewError(domain.CodeNotFound, "Workspace share link not found")
	}
	s.link.State, s.link.ClaimantID = workspace.ShareInvalidated, ""
	return nil
}

func (s *workspaceShareTestStore) WorkspaceShareSourceMatches(context.Context, string, string, int64) (bool, error) {
	return s.sourceCurrent, nil
}

type workspaceShareTestCopier struct {
	mu       sync.Mutex
	calls    int
	entered  chan struct{}
	release  chan struct{}
	store    *workspaceShareTestStore
	shares   []workspace.ShareLink
	failOnce bool
}

func (c *workspaceShareTestCopier) copyForWorkspaceShare(_ context.Context, principal identity.Principal,
	share workspace.ShareLink) (WorkspaceCopyResult, error) {
	c.mu.Lock()
	c.calls++
	c.shares = append(c.shares, share)
	failOnce := c.failOnce
	c.failOnce = false
	if c.entered != nil {
		select {
		case c.entered <- struct{}{}:
		default:
		}
	}
	release := c.release
	c.mu.Unlock()
	if release != nil {
		<-release
	}
	if share.TargetWorkspaceID == "" {
		share.TargetWorkspaceID = "w0000002"
	}
	if c.store != nil {
		c.store.mu.Lock()
		c.store.link.TargetWorkspaceID = share.TargetWorkspaceID
		c.store.mu.Unlock()
	}
	if failOnce {
		return WorkspaceCopyResult{}, errors.New("copy outcome unknown")
	}
	return WorkspaceCopyResult{SourceWorkspaceID: share.SourceWorkspaceID, SourceRevision: share.SourceRevision,
		Workspace: DeviceWorkspaceInfo{ID: share.TargetWorkspaceID, Name: share.WorkspaceName, Revision: 1, State: workspace.StateReady},
		State:     workspace.CopyCompleted}, nil
}

func newWorkspaceShareTestService(t *testing.T, copier *workspaceShareTestCopier) (*WorkspaceShareService, *workspaceShareTestStore, string) {
	t.Helper()
	store := &workspaceShareTestStore{sourceCurrent: true}
	copier.store = store
	service, err := NewWorkspaceShareService(store, copier)
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(context.Background(), identity.Principal{UserID: "u0000001", Role: identity.RoleUser, Kind: identity.KindSession},
		"w0000001", 7, workspace.ShareView)
	if err != nil {
		t.Fatal(err)
	}
	return service, store, created.Token
}

func TestWorkspaceShareCreatesReadOnlyCopyAndInvalidatesLinkAfterSingleClaim(t *testing.T) {
	copier := &workspaceShareTestCopier{}
	service, store, token := newWorkspaceShareTestService(t, copier)
	if len(token) != 43 || len(store.link.TokenHash) != 32 {
		t.Fatalf("share token/hash lengths = %d/%d", len(token), len(store.link.TokenHash))
	}
	lifetime := store.link.ExpiresAt.Sub(store.link.CreatedAt)
	if lifetime > workspaceShareLifetime || lifetime < workspaceShareLifetime-time.Second {
		t.Fatalf("unclaimed link lifetime = %s, want 7 days", lifetime)
	}
	if subtle.ConstantTimeCompare([]byte(token), store.link.TokenHash) == 1 {
		t.Fatal("share token was stored in plaintext")
	}
	recipient := identity.Principal{UserID: "u0000002", Role: identity.RoleUser, Kind: identity.KindSession}
	preview, err := service.Preview(context.Background(), recipient, token)
	if err != nil || preview.Permission != workspace.ShareView || preview.WorkspaceName != "Ready Workspace" {
		t.Fatalf("share preview = %#v, err=%v", preview, err)
	}
	accepted, err := service.Accept(context.Background(), recipient, token)
	if err != nil || accepted.WorkspaceID != "w0000002" || accepted.Permission != workspace.ShareView {
		t.Fatalf("share acceptance = %#v, err=%v", accepted, err)
	}
	if store.link.State != workspace.ShareClaimed || store.link.ClaimantID != recipient.UserID || store.link.TargetWorkspaceID != accepted.WorkspaceID {
		t.Fatalf("claimed share state = %#v", store.link)
	}
	if _, err := service.Preview(context.Background(), identity.Principal{UserID: "u0000003", Role: identity.RoleUser, Kind: identity.KindSession}, token); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("claimed link preview error = %v, want not found", err)
	}
	if _, err := service.Accept(context.Background(), identity.Principal{UserID: "u0000003", Role: identity.RoleUser, Kind: identity.KindSession}, token); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("second recipient claim error = %v, want not found", err)
	}
	if _, err := service.Preview(context.Background(), recipient, token); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("claimed link preview for original recipient error = %v, want not found", err)
	}
	if _, err := service.Accept(context.Background(), recipient, token); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("repeat claim by original recipient error = %v, want not found", err)
	}
	copier.mu.Lock()
	defer copier.mu.Unlock()
	if copier.calls != 1 {
		t.Fatalf("Workspace copy count = %d, want one", copier.calls)
	}
	if len(copier.shares) != 1 || copier.shares[0].OwnerID != "u0000001" || copier.shares[0].ClaimantID != recipient.UserID ||
		copier.shares[0].Permission != workspace.ShareView {
		t.Fatalf("copy was not requested from the recipient's active read-only claim: %#v", copier.shares)
	}
}

func TestWorkspaceShareReservesAnInProgressClaimForOneRecipient(t *testing.T) {
	copier := &workspaceShareTestCopier{entered: make(chan struct{}, 1), release: make(chan struct{})}
	service, _, token := newWorkspaceShareTestService(t, copier)
	winner := identity.Principal{UserID: "u0000002", Role: identity.RoleUser, Kind: identity.KindSession}
	result := make(chan error, 1)
	go func() {
		_, err := service.Accept(context.Background(), winner, token)
		result <- err
	}()
	select {
	case <-copier.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first recipient did not reserve the link")
	}
	other := identity.Principal{UserID: "u0000003", Role: identity.RoleUser, Kind: identity.KindSession}
	if _, err := service.Accept(context.Background(), other, token); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("concurrent recipient claim error = %v, want not found", err)
	}
	close(copier.release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("reserved recipient claim failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reserved recipient claim did not finish")
	}
}

func TestWorkspaceShareClaimRetriesRecipientOwnedCopyWithoutTransfer(t *testing.T) {
	copier := &workspaceShareTestCopier{failOnce: true}
	service, store, token := newWorkspaceShareTestService(t, copier)
	recipient := identity.Principal{UserID: "u0000002", Role: identity.RoleUser, Kind: identity.KindSession}
	if _, err := service.Accept(context.Background(), recipient, token); err == nil {
		t.Fatal("first copy attempt succeeded despite injected uncertain outcome")
	}
	if store.link.TargetWorkspaceID != "w0000002" || store.link.State != workspace.ShareClaiming {
		t.Fatalf("copy reservation was not preserved for retry: %#v", store.link)
	}
	accepted, err := service.Accept(context.Background(), recipient, token)
	if err != nil || accepted.WorkspaceID != store.link.TargetWorkspaceID {
		t.Fatalf("recipient retry = %#v, err=%v", accepted, err)
	}
	copier.mu.Lock()
	defer copier.mu.Unlock()
	if copier.calls != 2 || len(copier.shares) != 2 || copier.shares[0].TargetWorkspaceID != "" ||
		copier.shares[1].TargetWorkspaceID != "w0000002" {
		t.Fatalf("retry did not reuse the reserved recipient copy: calls=%d shares=%#v", copier.calls, copier.shares)
	}
}
