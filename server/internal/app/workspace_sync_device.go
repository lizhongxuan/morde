package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

const (
	MaxWorkspaceSyncDeviceCertificateDER                   = 16 << 10
	MaxPendingWorkspaceSyncDeviceChallengesPerInstallation = 8
	syncDeviceChallengeTTL                                 = 5 * time.Minute
)

type WorkspaceSyncDeviceStore interface {
	CreateWorkspaceSyncDeviceChallenge(context.Context, identity.Principal, workspace.SyncDeviceChallenge) error
	GetWorkspaceSyncDeviceChallenge(context.Context, identity.Principal, string, string) (workspace.SyncDeviceChallenge, error)
	GetWorkspaceSyncDeviceRegistration(context.Context, identity.Principal, string, []byte, []byte) (workspace.SyncDeviceBinding, bool, error)
	ListWorkspaceSyncDeviceBindings(context.Context, identity.Principal, string) ([]workspace.SyncDeviceBinding, error)
	RegisterWorkspaceSyncDevice(context.Context, identity.Principal, string, string, []byte, []byte, string, string) (workspace.SyncDeviceBinding, error)
	RevokeWorkspaceSyncDevice(context.Context, identity.Principal, string, string) error
}

type WorkspaceSyncNodeEnsurer interface {
	EnsureWorkspaceSyncNode(context.Context, string) (workspace.SyncNodeConnection, error)
}

type WorkspaceSyncDeviceChallengeResult struct {
	ID             string
	DeviceID       string
	Nonce          []byte
	SigningPayload []byte
	ExpiresAt      time.Time
}

type WorkspaceSyncDeviceRegistration struct {
	Binding workspace.SyncDeviceBinding
	Node    workspace.SyncNodeConnection
}

type WorkspaceSyncDeviceService struct {
	store     WorkspaceSyncDeviceStore
	nodeMaker WorkspaceSyncNodeEnsurer
}

func NewWorkspaceSyncDeviceService(store WorkspaceSyncDeviceStore, nodeMaker WorkspaceSyncNodeEnsurer) (*WorkspaceSyncDeviceService, error) {
	if store == nil {
		return nil, domain.InvalidArgument("store", "workspace sync device store is required")
	}
	return &WorkspaceSyncDeviceService{store: store, nodeMaker: nodeMaker}, nil
}

func (s *WorkspaceSyncDeviceService) CreateChallenge(ctx context.Context, principal identity.Principal, workspaceID, deviceID string, certificateDER []byte) (WorkspaceSyncDeviceChallengeResult, error) {
	if s.nodeMaker == nil {
		return WorkspaceSyncDeviceChallengeResult{}, domain.NewError(domain.CodeUpstreamError, "workspace sync is not enabled")
	}
	if err := validateSyncDeviceClient(principal); err != nil {
		return WorkspaceSyncDeviceChallengeResult{}, err
	}
	if !domain.ValidWorkspaceID(workspaceID) {
		return WorkspaceSyncDeviceChallengeResult{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	if len(certificateDER) == 0 || len(certificateDER) > MaxWorkspaceSyncDeviceCertificateDER {
		return WorkspaceSyncDeviceChallengeResult{}, domain.InvalidArgument("certificate_der", "device certificate is invalid")
	}
	certificate, canonicalID, certificateHash, err := parseSyncthingDeviceCertificate(certificateDER, time.Now())
	if err != nil || !strings.EqualFold(strings.TrimSpace(deviceID), canonicalID) {
		return WorkspaceSyncDeviceChallengeResult{}, domain.InvalidArgument("device_id", "device ID does not match the certificate")
	}
	challengeID, err := domain.NewWorkspaceSyncDeviceChallengeID()
	if err != nil {
		return WorkspaceSyncDeviceChallengeResult{}, err
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return WorkspaceSyncDeviceChallengeResult{}, fmt.Errorf("generate workspace sync device challenge: %w", err)
	}
	expiresAt := time.Now().UTC().Add(syncDeviceChallengeTTL)
	challenge := workspace.SyncDeviceChallenge{
		ID: challengeID, OwnerID: principal.UserID, WorkspaceID: workspaceID,
		ClientInstallationID: principal.ClientInstallationID, DeviceID: canonicalID,
		CertificateDER: certificate.Raw, CertificateSHA256: certificateHash[:], Nonce: nonce, ExpiresAt: expiresAt,
	}
	if err := s.store.CreateWorkspaceSyncDeviceChallenge(ctx, principal, challenge); err != nil {
		return WorkspaceSyncDeviceChallengeResult{}, err
	}
	return WorkspaceSyncDeviceChallengeResult{
		ID: challenge.ID, DeviceID: challenge.DeviceID, Nonce: nonce,
		SigningPayload: syncDeviceProofPayload(challenge), ExpiresAt: expiresAt,
	}, nil
}

func (s *WorkspaceSyncDeviceService) Register(ctx context.Context, principal identity.Principal, workspaceID, challengeID, idempotencyKey string, signature []byte) (WorkspaceSyncDeviceRegistration, error) {
	if s.nodeMaker == nil {
		return WorkspaceSyncDeviceRegistration{}, domain.NewError(domain.CodeUpstreamError, "workspace sync is not enabled")
	}
	if err := validateSyncDeviceClient(principal); err != nil {
		return WorkspaceSyncDeviceRegistration{}, err
	}
	if !domain.ValidWorkspaceID(workspaceID) || !domain.ValidWorkspaceSyncDeviceChallengeID(challengeID) {
		return WorkspaceSyncDeviceRegistration{}, domain.NewError(domain.CodeNotFound, "workspace sync device challenge not found")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return WorkspaceSyncDeviceRegistration{}, err
	}
	keyHash := sha256.Sum256([]byte(idempotencyKey))
	signatureHash := sha256.Sum256(signature)
	requestHash := sha256.Sum256(append([]byte("morde.workspace-sync-device.register.v1\x00"+workspaceID+"\x00"+
		principal.ClientInstallationID+"\x00"+challengeID+"\x00"), signatureHash[:]...))
	if _, found, err := s.store.GetWorkspaceSyncDeviceRegistration(ctx, principal, workspaceID, keyHash[:], requestHash[:]); err != nil {
		return WorkspaceSyncDeviceRegistration{}, err
	} else if found {
		node, err := s.ensureOwnerNode(ctx, principal.UserID)
		if err != nil {
			return WorkspaceSyncDeviceRegistration{}, err
		}
		binding, _, err := s.store.GetWorkspaceSyncDeviceRegistration(ctx, principal, workspaceID, keyHash[:], requestHash[:])
		return WorkspaceSyncDeviceRegistration{Binding: binding, Node: node}, err
	}
	challenge, err := s.store.GetWorkspaceSyncDeviceChallenge(ctx, principal, workspaceID, challengeID)
	if err != nil {
		return WorkspaceSyncDeviceRegistration{}, err
	}
	if !challenge.ExpiresAt.After(time.Now()) {
		return WorkspaceSyncDeviceRegistration{}, domain.NewError(domain.CodeTransferExpired, "workspace sync device challenge expired")
	}
	certificate, canonicalID, certificateHash, err := parseSyncthingDeviceCertificate(challenge.CertificateDER, time.Now())
	if err != nil || canonicalID != challenge.DeviceID || !bytes.Equal(certificateHash[:], challenge.CertificateSHA256) ||
		certificate.CheckSignature(certificate.SignatureAlgorithm, syncDeviceProofPayload(challenge), signature) != nil {
		return WorkspaceSyncDeviceRegistration{}, domain.NewError(domain.CodeForbidden, "device private-key proof is invalid")
	}
	bindingID, err := domain.NewWorkspaceSyncDeviceBindingID()
	if err != nil {
		return WorkspaceSyncDeviceRegistration{}, err
	}
	folderBytes := make([]byte, 16)
	if _, err := rand.Read(folderBytes); err != nil {
		return WorkspaceSyncDeviceRegistration{}, fmt.Errorf("generate workspace sync folder ID: %w", err)
	}
	folderID := "morde-" + hex.EncodeToString(folderBytes)
	binding, err := s.store.RegisterWorkspaceSyncDevice(ctx, principal, workspaceID, challengeID, keyHash[:], requestHash[:], bindingID, folderID)
	if err != nil {
		return WorkspaceSyncDeviceRegistration{}, err
	}
	node, err := s.ensureOwnerNode(ctx, principal.UserID)
	if err != nil {
		return WorkspaceSyncDeviceRegistration{}, err
	}
	binding, _, err = s.store.GetWorkspaceSyncDeviceRegistration(ctx, principal, workspaceID, keyHash[:], requestHash[:])
	if err != nil {
		return WorkspaceSyncDeviceRegistration{}, err
	}
	return WorkspaceSyncDeviceRegistration{Binding: binding, Node: node}, nil
}

func (s *WorkspaceSyncDeviceService) ensureOwnerNode(ctx context.Context, ownerID string) (workspace.SyncNodeConnection, error) {
	if s.nodeMaker == nil {
		return workspace.SyncNodeConnection{}, domain.NewError(domain.CodeUpstreamError, "workspace sync is not enabled")
	}
	node, err := s.nodeMaker.EnsureWorkspaceSyncNode(ctx, ownerID)
	if err != nil {
		return workspace.SyncNodeConnection{}, domain.NewError(domain.CodeUpstreamError, "workspace sync node is not ready")
	}
	return node, nil
}

func (s *WorkspaceSyncDeviceService) Revoke(ctx context.Context, principal identity.Principal, workspaceID, bindingID string) error {
	if err := requireSession(principal); err != nil {
		return err
	}
	if !domain.ValidWorkspaceID(workspaceID) || !domain.ValidWorkspaceSyncDeviceBindingID(bindingID) {
		return domain.NewError(domain.CodeNotFound, "workspace sync device binding not found")
	}
	if err := s.store.RevokeWorkspaceSyncDevice(ctx, principal, workspaceID, bindingID); err != nil {
		return err
	}
	_, err := s.ensureOwnerNode(ctx, principal.UserID)
	return err
}

func (s *WorkspaceSyncDeviceService) List(ctx context.Context, principal identity.Principal, workspaceID string) ([]workspace.SyncDeviceBinding, error) {
	if err := requireSession(principal); err != nil {
		return nil, err
	}
	if !domain.ValidWorkspaceID(workspaceID) {
		return nil, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return s.store.ListWorkspaceSyncDeviceBindings(ctx, principal, workspaceID)
}

func validateSyncDeviceClient(principal identity.Principal) error {
	if principal.Kind != identity.KindClient || principal.Role != identity.RoleUser ||
		!domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) ||
		!domain.ValidClientInstallationID(principal.ClientInstallationID) || len(principal.WorkspaceIDs) != 0 || len(principal.HostIDs) != 0 {
		return domain.NewError(domain.CodeForbidden, "desktop client credential required")
	}
	return nil
}

func parseSyncthingDeviceCertificate(der []byte, now time.Time) (*x509.Certificate, string, [32]byte, error) {
	certificate, err := x509.ParseCertificate(der)
	if err != nil || !bytes.Equal(certificate.Raw, der) || now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) ||
		certificate.CheckSignature(certificate.SignatureAlgorithm, certificate.RawTBSCertificate, certificate.Signature) != nil {
		return nil, "", [32]byte{}, fmt.Errorf("invalid self-signed Syncthing certificate")
	}
	certificateHash := sha256.Sum256(certificate.Raw)
	deviceID, err := workspace.SyncthingDeviceIDFromCertificateDER(der)
	if err != nil {
		return nil, "", [32]byte{}, err
	}
	return certificate, deviceID, certificateHash, nil
}

func syncDeviceProofPayload(challenge workspace.SyncDeviceChallenge) []byte {
	return []byte("morde.workspace-sync-device.v1\x00" + challenge.ID + "\x00" + challenge.WorkspaceID + "\x00" +
		challenge.ClientInstallationID + "\x00" + challenge.DeviceID + "\x00" + hex.EncodeToString(challenge.CertificateSHA256) + "\x00" +
		base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(challenge.Nonce))
}
