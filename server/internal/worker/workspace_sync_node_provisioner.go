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
	relayConfig() workspaceSyncRelayConfig
}

type workspaceSyncRelayConfig struct {
	StateRoot     string
	PublicAddress string
	Token         string
}

type WorkspaceSyncNodeProvisioner struct {
	supervisor workspaceSyncNodeSupervisor
	store      workspaceSyncNodeReconciliationStore
	dataKey    []byte
	relay      workspaceSyncRelayConfig
}

type workspaceSyncNodeReconciliationStore interface {
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
		!validWorkspaceSyncRelayHost(relay.PublicAddress) || !workspaceSyncRelayTokenPattern.MatchString(relay.Token) {
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
func (p *WorkspaceSyncNodeProvisioner) EnsureWorkspaceSyncNode(ctx context.Context, ownerID string) (workspace.SyncNodeConnection, error) {
	if ctx == nil || !domain.ValidUserID(ownerID) {
		return workspace.SyncNodeConnection{}, errors.New("invalid workspace sync node owner")
	}
	key := p.apiKey(ownerID)
	node, err := p.supervisor.Ensure(ctx, ownerID, key)
	if err != nil {
		return workspace.SyncNodeConnection{}, errors.New("workspace sync node provisioning failed")
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
	internalRelay, err := staticRelayURI("morde-relay:22067", relayID, p.relay.Token)
	if err != nil {
		return workspace.SyncNodeConnection{}, err
	}
	if err := client.ConfigurePrivateRelay(readyCtx, internalRelay); err != nil {
		return workspace.SyncNodeConnection{}, fmt.Errorf("configure private Workspace sync relay: %w", err)
	}
	if err := p.store.ReconcileWorkspaceSyncNode(readyCtx, ownerID, func(applyCtx context.Context, state workspace.SyncNodeDesiredState) error {
		return applyWorkspaceSyncNodeDesiredState(applyCtx, client, state, nodeID)
	}); err != nil {
		return workspace.SyncNodeConnection{}, fmt.Errorf("reconcile Workspace sync node: %w", err)
	}
	publicRelay, err := staticRelayURI(p.relay.PublicAddress, relayID, p.relay.Token)
	if err != nil {
		return workspace.SyncNodeConnection{}, err
	}
	return workspace.SyncNodeConnection{DeviceID: nodeID, RelayAddress: publicRelay}, nil
}

// ReconcilePendingWorkspaceSyncNodes reapplies durable owner snapshots after
// process restart or an earlier node API failure.
func (p *WorkspaceSyncNodeProvisioner) ReconcilePendingWorkspaceSyncNodes(ctx context.Context) error {
	if ctx == nil {
		return errors.New("workspace sync reconciliation context is required")
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
	if !validSyncthingDeviceID(relayID) || !validWorkspaceSyncRelayHost(hostPort) || !workspaceSyncRelayTokenPattern.MatchString(relayToken) {
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

func validWorkspaceSyncRelayHost(hostPort string) bool {
	if strings.TrimSpace(hostPort) != hostPort || strings.ContainsAny(hostPort, "/?#@\\") {
		return false
	}
	host, portText, err := net.SplitHostPort(hostPort)
	if err != nil || host == "" {
		return false
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
				(char < '0' || char > '9') && char != '-' {
				return false
			}
		}
	}
	return true
}
