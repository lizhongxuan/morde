package app

import (
	"context"
	"crypto/subtle"
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
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (c *workspaceShareTestCopier) Copy(_ context.Context, _ identity.Principal, sourceID string,
	command WorkspaceCopyCommand, _ string) (WorkspaceCopyResult, error) {
	c.mu.Lock()
	c.calls++
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
	return WorkspaceCopyResult{SourceWorkspaceID: sourceID, SourceRevision: command.ExpectedCurrentRevision,
		Workspace: DeviceWorkspaceInfo{ID: "w0000002", Name: command.Name, Revision: 1, State: workspace.StateReady},
		State:     workspace.CopyCompleted}, nil
}

type workspaceShareTestTransfers struct {
	mu       sync.Mutex
	readOnly bool
}

func (*workspaceShareTestTransfers) RequestWorkspaceTransfer(context.Context, identity.Principal, string, string,
	int64, string) (workspace.OwnershipTransfer, bool, error) {
	return workspace.OwnershipTransfer{ID: "r0000001"}, false, nil
}

func (s *workspaceShareTestTransfers) AcceptWorkspaceTransfer(context.Context, identity.Principal, string, string, string) (workspace.OwnershipTransfer, error) {
	s.mu.Lock()
	s.readOnly = false
	s.mu.Unlock()
	return workspace.OwnershipTransfer{}, nil
}

func (s *workspaceShareTestTransfers) AcceptWorkspaceTransferReadOnly(context.Context, identity.Principal, string, string,
	string) (workspace.OwnershipTransfer, error) {
	s.mu.Lock()
	s.readOnly = true
	s.mu.Unlock()
	return workspace.OwnershipTransfer{}, nil
}

func newWorkspaceShareTestService(t *testing.T, copier *workspaceShareTestCopier) (*WorkspaceShareService, *workspaceShareTestStore, *workspaceShareTestTransfers, string) {
	t.Helper()
	store := &workspaceShareTestStore{sourceCurrent: true}
	transfers := &workspaceShareTestTransfers{}
	service, err := NewWorkspaceShareService(store, copier, transfers)
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(context.Background(), identity.Principal{UserID: "u0000001", Role: identity.RoleUser, Kind: identity.KindSession},
		"w0000001", 7, workspace.ShareView)
	if err != nil {
		t.Fatal(err)
	}
	return service, store, transfers, created.Token
}

func TestWorkspaceShareCreatesReadOnlyCopyAndInvalidatesLinkAfterSingleClaim(t *testing.T) {
	copier := &workspaceShareTestCopier{}
	service, store, transfers, token := newWorkspaceShareTestService(t, copier)
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
	if !transfers.readOnly || store.link.State != workspace.ShareClaimed || store.link.ClaimantID != recipient.UserID {
		t.Fatalf("claimed share state = %#v, transfer readOnly=%v", store.link, transfers.readOnly)
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
}

func TestWorkspaceShareReservesAnInProgressClaimForOneRecipient(t *testing.T) {
	copier := &workspaceShareTestCopier{entered: make(chan struct{}, 1), release: make(chan struct{})}
	service, _, _, token := newWorkspaceShareTestService(t, copier)
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
