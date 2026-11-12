package app

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type syncDeviceMemoryStore struct {
	challenge      workspace.SyncDeviceChallenge
	binding        workspace.SyncDeviceBinding
	bindings       []workspace.SyncDeviceBinding
	folders        map[string]workspace.SyncFolder
	registered     int
	challengeReads int
	keyHash        []byte
	requestHash    []byte
}

type syncNodeEnsurerStub struct {
	ownerID string
	calls   int
	err     error
	store   *syncDeviceMemoryStore
}

func (s *syncNodeEnsurerStub) EnsureWorkspaceSyncNode(_ context.Context, ownerID string) (workspace.SyncNodeConnection, error) {
	s.calls++
	s.ownerID = ownerID
	if s.err != nil {
		return workspace.SyncNodeConnection{}, s.err
	}
	if s.store != nil {
		s.store.binding.State = workspace.SyncDeviceBindingActive
	}
	return workspace.SyncNodeConnection{DeviceID: "NODE-1", RelayAddress: "relay://relay.example.test:22067/?id=RELAY-1"}, nil
}

func (s *syncNodeEnsurerStub) DisableWorkspaceSyncNodeRelay(context.Context, string) error {
	return nil
}

func (s *syncDeviceMemoryStore) CreateWorkspaceSyncDeviceChallenge(_ context.Context, _ identity.Principal, value workspace.SyncDeviceChallenge) error {
	s.challenge = value
	return nil
}

func (s *syncDeviceMemoryStore) RegisterWorkspaceSyncFolder(_ context.Context, _ identity.Principal, _ string, value workspace.SyncFolder, _, _ []byte) (workspace.SyncFolder, error) {
	if s.folders == nil {
		s.folders = make(map[string]workspace.SyncFolder)
	}
	s.folders[value.FolderID] = value
	return value, nil
}

func (s *syncDeviceMemoryStore) ListWorkspaceSyncFolders(context.Context, identity.Principal, string) ([]workspace.SyncFolder, error) {
	items := make([]workspace.SyncFolder, 0, len(s.folders))
	for _, item := range s.folders {
		items = append(items, item)
	}
	return items, nil
}

func (s *syncDeviceMemoryStore) GetWorkspaceSyncFolder(_ context.Context, _ identity.Principal, _, folderID string) (workspace.SyncFolder, error) {
	item, ok := s.folders[folderID]
	if !ok {
		return workspace.SyncFolder{}, domain.NewError(domain.CodeNotFound, "workspace sync folder not found")
	}
	return item, nil
}

func (s *syncDeviceMemoryStore) GetWorkspaceSyncDeviceChallenge(_ context.Context, _ identity.Principal, _, _ string) (workspace.SyncDeviceChallenge, error) {
	s.challengeReads++
	return s.challenge, nil
}

func (s *syncDeviceMemoryStore) GetWorkspaceSyncDeviceRegistration(_ context.Context, _ identity.Principal, _, _ string, keyHash, requestHash []byte) (workspace.SyncDeviceBinding, bool, error) {
	if !bytes.Equal(s.keyHash, keyHash) {
		return workspace.SyncDeviceBinding{}, false, nil
	}
	if !bytes.Equal(s.requestHash, requestHash) {
		return workspace.SyncDeviceBinding{}, false, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key conflict")
	}
	return s.binding, true, nil
}

func (s *syncDeviceMemoryStore) RegisterWorkspaceSyncDevice(_ context.Context, _ identity.Principal, _, _ string, keyHash, requestHash []byte, bindingID, folderID string) (workspace.SyncDeviceBinding, error) {
	s.registered++
	s.keyHash = append([]byte(nil), keyHash...)
	s.requestHash = append([]byte(nil), requestHash...)
	s.binding = workspace.SyncDeviceBinding{ID: bindingID, DeviceID: s.challenge.DeviceID, FolderID: folderID,
		State: workspace.SyncDeviceBindingPendingNode}
	return s.binding, nil
}

func (s *syncDeviceMemoryStore) RevokeWorkspaceSyncDevice(context.Context, identity.Principal, string, string, func(context.Context) error) error {
	return nil
}

func (s *syncDeviceMemoryStore) ListWorkspaceSyncDeviceBindings(context.Context, identity.Principal, string) ([]workspace.SyncDeviceBinding, error) {
	return s.bindings, nil
}

func TestWorkspaceSyncDeviceChallengeRequiresCertificatePrivateKeyProof(t *testing.T) {
	store := &syncDeviceMemoryStore{}
	nodes := &syncNodeEnsurerStub{store: store}
	service, err := NewWorkspaceSyncDeviceService(store, nodes)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: "t1234567", ClientInstallationID: "i1234567"}
	privateKey, certificateDER := syncDeviceCertificate(t)
	deviceID, err := workspace.SyncthingDeviceIDFromCertificateDER(certificateDER)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := service.CreateChallenge(context.Background(), principal, "w1234567", "", deviceID, certificateDER)
	if err != nil || challenge.ID == "" || challenge.DeviceID != deviceID || len(challenge.Nonce) != 32 || len(challenge.SigningPayload) == 0 {
		t.Fatalf("CreateChallenge() = %#v, %v", challenge, err)
	}
	digest := sha256.Sum256(challenge.SigningPayload)
	signature, err := ecdsa.SignASN1(rand.Reader, privateKey, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	binding, err := service.Register(context.Background(), principal, "w1234567", "", challenge.ID, "sync-device-key-001", signature)
	if err != nil || binding.Binding.ID == "" || binding.Binding.FolderID == "" || binding.Binding.State != workspace.SyncDeviceBindingActive || binding.Node.DeviceID != "NODE-1" || store.registered != 1 || nodes.calls != 1 || nodes.ownerID != principal.UserID {
		t.Fatalf("Register() = %#v, %v, registered=%d node calls=%d owner=%q", binding, err, store.registered, nodes.calls, nodes.ownerID)
	}
	store.challengeReads = 0
	store.challenge.ExpiresAt = time.Now().Add(-time.Minute)
	replayed, err := service.Register(context.Background(), principal, "w1234567", "", challenge.ID, "sync-device-key-001", signature)
	if err != nil || replayed.Binding.ID != binding.Binding.ID || store.registered != 1 || store.challengeReads != 0 || nodes.calls != 2 {
		t.Fatalf("idempotent replay after challenge expiry = %#v, %v, registered=%d challenge_reads=%d node_calls=%d", replayed, err, store.registered, store.challengeReads, nodes.calls)
	}
	nodes.err = errors.New("offline")
	if _, err := service.Register(context.Background(), principal, "w1234567", "", challenge.ID, "sync-device-key-001", signature); domain.CodeOf(err) != domain.CodeUpstreamError || store.registered != 1 {
		t.Fatalf("replay with unavailable node = %v, registrations=%d", err, store.registered)
	}

	store.registered = 0
	store.challenge.ExpiresAt = time.Now().Add(time.Hour)
	if _, err := service.Register(context.Background(), principal, "w1234567", "", challenge.ID, "sync-device-key-002", []byte("forged")); err == nil || store.registered != 0 {
		t.Fatalf("forged proof accepted: err=%v, registered=%d", err, store.registered)
	}
	if _, err := service.CreateChallenge(context.Background(), principal, "w1234567", "", "not-the-certificate-device", certificateDER); err == nil {
		t.Fatal("device ID not derived from the supplied certificate was accepted")
	}
}

func TestWorkspaceSyncProjectChallengeCannotRegisterButLegacyRootStillCan(t *testing.T) {
	store := &syncDeviceMemoryStore{folders: map[string]workspace.SyncFolder{
		"morde-11111111111111111111111111111111": {
			OwnerID: "u1234567", WorkspaceID: "w1234567", FolderID: "morde-11111111111111111111111111111111",
			Kind: workspace.SyncFolderKindProject, ProjectName: "demo", RelativePath: "project/demo", State: "pending_node",
		},
	}}
	nodes := &syncNodeEnsurerStub{store: store}
	service, err := NewWorkspaceSyncDeviceService(store, nodes)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: "t1234567", ClientInstallationID: "i1234567"}
	privateKey, certificateDER := syncDeviceCertificate(t)
	deviceID, err := workspace.SyncthingDeviceIDFromCertificateDER(certificateDER)
	if err != nil {
		t.Fatal(err)
	}
	projectFolderID := "morde-11111111111111111111111111111111"
	projectChallenge, err := service.CreateChallenge(context.Background(), principal, "w1234567", projectFolderID, deviceID, certificateDER)
	if err != nil || projectChallenge.FolderID != projectFolderID || projectChallenge.FolderKind != workspace.SyncFolderKindProject || projectChallenge.RelativePath != "project/demo" {
		t.Fatalf("project CreateChallenge() = %#v, %v", projectChallenge, err)
	}
	projectDigest := sha256.Sum256(projectChallenge.SigningPayload)
	projectSignature, err := ecdsa.SignASN1(rand.Reader, privateKey, projectDigest[:])
	if err != nil {
		t.Fatal(err)
	}
	store.challenge.RelativePath = "project/another"
	if _, err := service.Register(context.Background(), principal, "w1234567", projectFolderID, projectChallenge.ID, "tampered-path-key-001", projectSignature); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("project registration with changed challenged path = %v, want forbidden", err)
	}
	store.challenge.RelativePath = projectChallenge.RelativePath
	if _, err := service.Register(context.Background(), principal, "w1234567", projectFolderID, projectChallenge.ID, "project-key-001", projectSignature); domain.CodeOf(err) != domain.CodeWorkspaceUnavailable {
		t.Fatalf("project registration error = %v, want unavailable", err)
	}
	if store.registered != 0 || nodes.calls != 0 || store.challengeReads != 2 || store.challenge.ID != projectChallenge.ID || store.challenge.FolderID != projectFolderID {
		t.Fatalf("project registration mutated sync state: registered=%d node_calls=%d challenge_reads=%d challenge=%#v", store.registered, nodes.calls, store.challengeReads, store.challenge)
	}
	// The same challenge remains unconsumed; the existing root registration flow still works.
	rootChallenge, err := service.CreateChallenge(context.Background(), principal, "w1234567", "", deviceID, certificateDER)
	if err != nil || rootChallenge.FolderID != "" || rootChallenge.FolderKind != workspace.SyncFolderKindWorkspaceRoot || rootChallenge.RelativePath != "." {
		t.Fatalf("root CreateChallenge() = %#v, %v", rootChallenge, err)
	}
	rootDigest := sha256.Sum256(rootChallenge.SigningPayload)
	rootSignature, err := ecdsa.SignASN1(rand.Reader, privateKey, rootDigest[:])
	if err != nil {
		t.Fatal(err)
	}
	registration, err := service.Register(context.Background(), principal, "w1234567", "", rootChallenge.ID, "root-key-001", rootSignature)
	if err != nil || registration.Binding.ID == "" || store.registered != 1 || nodes.calls != 1 {
		t.Fatalf("legacy root registration = %#v, %v, registered=%d node_calls=%d", registration, err, store.registered, nodes.calls)
	}
}

func TestWorkspaceSyncDeviceListRequiresPortalSession(t *testing.T) {
	store := &syncDeviceMemoryStore{bindings: []workspace.SyncDeviceBinding{{ID: "d1234567", WorkspaceID: "w1234567"}}}
	service, err := NewWorkspaceSyncDeviceService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	session := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	items, err := service.List(context.Background(), session, "w1234567")
	if err != nil || len(items) != 1 || items[0].ID != "d1234567" {
		t.Fatalf("List() = %#v, %v", items, err)
	}
	client := session
	client.Kind = identity.KindClient
	if _, err := service.List(context.Background(), client, "w1234567"); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("client credential list error = %v, want forbidden", err)
	}
}

func TestWorkspaceSyncProjectFolderNameIsCanonicalizedBeforeRegistration(t *testing.T) {
	store := &syncDeviceMemoryStore{}
	service, err := NewWorkspaceSyncDeviceService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	folder, err := service.CreateFolder(context.Background(), principal, "h1234567", "e\u0301", "folder-key-001")
	if err != nil || folder.ProjectName != "é" || folder.RelativePath != "project/é" || folder.SyncAvailable() {
		t.Fatalf("CreateFolder() = %#v, %v", folder, err)
	}
	if len(store.folders) != 1 {
		t.Fatalf("stored folder count = %d, want 1", len(store.folders))
	}
	for _, stored := range store.folders {
		if stored.ProjectName != "é" || stored.RelativePath != "project/é" {
			t.Fatalf("stored non-canonical project path = %#v", stored)
		}
	}
	for _, invalid := range []string{"../escape", "a/b", "CON", "."} {
		if _, err := service.CreateFolder(context.Background(), principal, "h1234567", invalid, "invalid-folder-key"); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Fatalf("CreateFolder(%q) error = %v, want invalid argument", invalid, err)
		}
	}
	if len(store.folders) != 1 {
		t.Fatalf("invalid project folder names changed stored count to %d", len(store.folders))
	}
}

func syncDeviceCertificate(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "syncthing"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, SignatureAlgorithm: x509.ECDSAWithSHA256,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return privateKey, certificateDER
}
