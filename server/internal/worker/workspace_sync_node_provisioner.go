package worker

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

type workspaceSyncNodeSupervisor interface {
	Ensure(context.Context, string, string) (DockerWorkspaceSyncNode, error)
	StopOwner(context.Context, string) error
	relayConfig() workspaceSyncRelayConfig
}

const workspaceSyncNodeStopTimeout = 5 * time.Second

type workspaceSyncRelayConfig struct {
	StateRoot     string
	PublicAddress string
	BindAddress   string
	Token         string
}

type WorkspaceSyncNodeProvisioner struct {
	supervisor workspaceSyncNodeSupervisor
	store      workspaceSyncNodeReconciliationStore
	dataKey    []byte
	relay      workspaceSyncRelayConfig
}

type workspaceSyncNodeReconciliationStore interface {
	RevokeExpiredWorkspaceSyncDevices(context.Context) error
	ListWorkspaceSyncNodeOwnersToReconcile(context.Context) ([]string, error)
	ReconcileWorkspaceSyncNode(context.Context, string, func(context.Context, workspace.SyncNodeDesiredState) error) error
}

type WorkspaceSyncNodeProvisionerOptions struct {
	Supervisor workspaceSyncNodeSupervisor
	Store      workspaceSyncNodeReconciliationStore
	DataKey    []byte
}

func NewWorkspaceSyncNodeProvisioner(options WorkspaceSyncNodeProvisionerOptions) (*WorkspaceSyncNodeProvisioner, error) {
	if options.Supervisor == nil || options.Store == nil || len(options.DataKey) != 32 {
		return nil, errors.New("workspace sync node provisioner requires a supervisor, store, and data key")
	}
	relay := options.Supervisor.relayConfig()
	if !filepath.IsAbs(relay.StateRoot) || filepath.Clean(relay.StateRoot) != relay.StateRoot || relay.StateRoot == string(filepath.Separator) ||
		!validWorkspaceSyncRelayHost(relay.PublicAddress) || !validWorkspaceSyncRelayBindAddress(relay.BindAddress) ||
		!workspaceSyncRelayTokenPattern.MatchString(relay.Token) {
		return nil, errors.New("workspace sync node supervisor relay configuration is invalid")
	}
	return &WorkspaceSyncNodeProvisioner{
		supervisor: options.Supervisor, store: options.Store, dataKey: append([]byte(nil), options.DataKey...),
		relay: relay,
	}, nil
}

// EnsureWorkspaceSyncNode creates or reuses the owner's persistent node and
// applies the current authorized device and folder snapshot before returning
// connection details to the desktop client.
func (p *WorkspaceSyncNodeProvisioner) EnsureWorkspaceSyncNode(ctx context.Context, ownerID string) (connection workspace.SyncNodeConnection, retErr error) {
	if ctx == nil || !domain.ValidUserID(ownerID) {
		return workspace.SyncNodeConnection{}, errors.New("invalid workspace sync node owner")
	}
	key := p.apiKey(ownerID)
	defer func() {
		if retErr != nil {
			retErr = p.stopOwnerAfterFailure(ownerID, key, retErr)
		}
	}()

	node, err := p.supervisor.Ensure(ctx, ownerID, key)
	if err != nil {
		return workspace.SyncNodeConnection{}, fmt.Errorf("workspace sync node provisioning failed: %w", err)
	}
	client, err := NewSyncthingAPIClient(node.APIURL, key)
	if err != nil {
		return workspace.SyncNodeConnection{}, errors.New("workspace sync node API configuration is invalid")
	}
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := waitWorkspaceSyncNodeAPI(readyCtx, client); err != nil {
		return workspace.SyncNodeConnection{}, err
	}
	nodeID, err := client.NodeID(readyCtx)
	if err != nil {
		return workspace.SyncNodeConnection{}, fmt.Errorf("read Workspace sync node identity: %w", err)
	}
	relayID, err := p.relayDeviceID(readyCtx)
	if err != nil {
		return workspace.SyncNodeConnection{}, err
	}
	publicRelay, err := staticRelayURI(p.relay.PublicAddress, relayID, p.relay.Token)
	if err != nil {
		return workspace.SyncNodeConnection{}, err
	}
	nodeRelay, err := staticRelayURI(net.JoinHostPort(p.relay.BindAddress, "22067"), relayID, p.relay.Token)
	if err != nil {
		return workspace.SyncNodeConnection{}, err
	}
	if err := client.ConfigurePrivateRelay(readyCtx, nodeRelay); err != nil {
		return workspace.SyncNodeConnection{}, fmt.Errorf("configure private Workspace sync relay: %w", err)
	}
	if err := p.store.ReconcileWorkspaceSyncNode(readyCtx, ownerID, func(applyCtx context.Context, state workspace.SyncNodeDesiredState) error {
		return applyWorkspaceSyncNodeDesiredState(applyCtx, client, state, nodeID, nodeRelay)
	}); err != nil {
		return workspace.SyncNodeConnection{}, fmt.Errorf("reconcile Workspace sync node: %w", err)
	}
	return workspace.SyncNodeConnection{DeviceID: nodeID, RelayAddress: publicRelay}, nil
}

func (p *WorkspaceSyncNodeProvisioner) stopOwnerAfterFailure(ownerID, apiKey string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), workspaceSyncNodeStopTimeout)
	defer cancel()
	if err := p.supervisor.StopOwner(ctx, ownerID); err != nil {
		cause = errors.Join(cause, fmt.Errorf("stop Workspace sync node after provisioning failure: %w", err))
	}
	return redactWorkspaceSyncNodeSecrets(cause, apiKey, p.relay.Token)
}

func redactWorkspaceSyncNodeSecrets(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	if message == err.Error() {
		return err
	}
	return workspaceSyncNodeRedactedError{message: message, cause: err}
}

type workspaceSyncNodeRedactedError struct {
	message string
	cause   error
}

func (e workspaceSyncNodeRedactedError) Error() string { return e.message }

func (e workspaceSyncNodeRedactedError) Unwrap() error { return e.cause }

// ReconcilePendingWorkspaceSyncNodes reapplies durable owner snapshots after
// process restart or an earlier node API failure.
func (p *WorkspaceSyncNodeProvisioner) ReconcilePendingWorkspaceSyncNodes(ctx context.Context) error {
	if ctx == nil {
		return errors.New("workspace sync reconciliation context is required")
	}
	if err := p.store.RevokeExpiredWorkspaceSyncDevices(ctx); err != nil {
		return fmt.Errorf("revoke expired Workspace sync devices: %w", err)
	}
	owners, err := p.store.ListWorkspaceSyncNodeOwnersToReconcile(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, ownerID := range owners {
		if _, err := p.EnsureWorkspaceSyncNode(ctx, ownerID); err != nil {
			failures = append(failures, fmt.Errorf("reconcile Workspace sync node for owner %s: %w", ownerID, err))
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return errors.Join(failures...)
}

func (p *WorkspaceSyncNodeProvisioner) ReconcileWorkspaceSyncNodeOwner(ctx context.Context, ownerID string) error {
	if ctx == nil || !domain.ValidUserID(ownerID) {
		return errors.New("invalid workspace sync node owner")
	}
	owners, err := p.store.ListWorkspaceSyncNodeOwnersToReconcile(ctx)
	if err != nil {
		return err
	}
	for _, candidate := range owners {
		if candidate == ownerID {
			_, err := p.EnsureWorkspaceSyncNode(ctx, ownerID)
			return err
		}
	}
	return nil
}

func (p *WorkspaceSyncNodeProvisioner) apiKey(ownerID string) string {
	mac := hmac.New(sha256.New, p.dataKey)
	_, _ = mac.Write([]byte("morde.workspace-sync-node-api.v1\x00" + ownerID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func waitWorkspaceSyncNodeAPI(ctx context.Context, client *SyncthingAPIClient) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := client.SystemStatus(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("workspace sync node API is not ready")
		case <-ticker.C:
		}
	}
}

func (p *WorkspaceSyncNodeProvisioner) relayDeviceID(ctx context.Context) (string, error) {
	path := filepath.Join(p.relay.StateRoot, "cert.pem")
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := os.Lstat(path)
		if err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return "", errors.New("Workspace sync relay certificate is unreadable")
			}
			block, _ := pem.Decode(data)
			if block == nil || block.Type != "CERTIFICATE" {
				return "", errors.New("Workspace sync relay certificate is invalid")
			}
			id, parseErr := workspace.SyncthingDeviceIDFromCertificateDER(block.Bytes)
			if parseErr != nil {
				return "", errors.New("Workspace sync relay certificate is invalid")
			}
			return id, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", errors.New("Workspace sync relay certificate is unavailable")
		}
		select {
		case <-ctx.Done():
			return "", errors.New("Workspace sync relay certificate is not ready")
		case <-ticker.C:
		}
	}
}

func staticRelayURI(hostPort, relayID, relayToken string) (string, error) {
	if !validSyncthingDeviceID(relayID) || !validWorkspaceSyncRelayURIHost(hostPort) || !workspaceSyncRelayTokenPattern.MatchString(relayToken) {
		return "", errors.New("invalid Workspace sync relay identity")
	}
	u, err := url.Parse("relay://" + hostPort)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return "", errors.New("invalid Workspace sync relay address")
	}
	query := url.Values{"id": []string{relayID}, "token": []string{relayToken}}
	u.Path = "/"
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func validWorkspaceSyncRelayURIHost(hostPort string) bool {
	if strings.TrimSpace(hostPort) != hostPort || strings.ContainsAny(hostPort, "/?#@\\") {
		return false
	}
	host, portText, err := net.SplitHostPort(hostPort)
	ip := net.ParseIP(host)
	if err != nil || strings.Contains(host, ":") || ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	port, err := strconv.Atoi(portText)
	return err == nil && port == 22067
}

func validWorkspaceSyncRelayBindAddress(address string) bool {
	if strings.TrimSpace(address) != address || strings.ContainsAny(address, ":/?#@\\") {
		return false
	}
	ip := net.ParseIP(address)
	return ip != nil && ip.To4() != nil && ip.IsGlobalUnicast() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()
}

func validWorkspaceSyncRelayHost(hostPort string) bool {
	if strings.TrimSpace(hostPort) != hostPort || strings.ContainsAny(hostPort, "/?#@\\") {
		return false
	}
	host, portText, err := net.SplitHostPort(hostPort)
	ip := net.ParseIP(host)
	if err != nil || strings.Contains(host, ":") || ip == nil || ip.To4() == nil {
		return false
	}
	port, err := strconv.Atoi(portText)
	return err == nil && port == 22067 && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() &&
		!ip.IsUnspecified() && !ip.IsMulticast() && !ip.IsLinkLocalUnicast()
}
