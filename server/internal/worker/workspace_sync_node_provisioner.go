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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

type workspaceSyncNodeSupervisor interface {
	Ensure(context.Context, string, string) (DockerWorkspaceSyncNode, error)
	Observe(context.Context, string, string) (workspace.SyncNodeObservation, error)
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
	supervisor             workspaceSyncNodeSupervisor
	store                  workspaceSyncNodeReconciliationStore
	relayGate              WorkspaceSyncRelayGate
	quotaSockets           WorkspaceSyncQuotaSocketManager
	startGuard             WorkspaceSyncNodeStartGuard
	dataKey                []byte
	relay                  workspaceSyncRelayConfig
	retryMu                sync.Mutex
	retryOwners            map[string]struct{}
	startupRecoveryPending bool
}

type workspaceSyncNodeReconciliationStore interface {
	RevokeExpiredWorkspaceSyncDevices(context.Context, func(context.Context, string) error) error
	ListWorkspaceSyncNodeOwnersToReconcile(context.Context) ([]string, error)
	ListWorkspaceSyncNodeOwners(context.Context) ([]string, error)
	ReconcileWorkspaceSyncNodeWithRelayGate(context.Context, string,
		func(context.Context, workspace.SyncNodeDesiredState) error,
		func(context.Context, workspace.SyncNodeDesiredState) error,
		func(context.Context) error) error
}

type WorkspaceSyncRelayGate interface {
	Enable(context.Context, string) error
	Disable(context.Context, string) error
}

type WorkspaceSyncNodeStartGuard interface {
	CheckWorkspaceSyncNodeStart(context.Context, string) error
}

type WorkspaceSyncNodeStartGuardFunc func(context.Context, string) error

func (f WorkspaceSyncNodeStartGuardFunc) CheckWorkspaceSyncNodeStart(ctx context.Context, ownerID string) error {
	return f(ctx, ownerID)
}

type WorkspaceSyncNodeProvisionerOptions struct {
	Supervisor   workspaceSyncNodeSupervisor
	Store        workspaceSyncNodeReconciliationStore
	RelayGate    WorkspaceSyncRelayGate
	QuotaSockets WorkspaceSyncQuotaSocketManager
	StartGuard   WorkspaceSyncNodeStartGuard
	DataKey      []byte
}

func NewWorkspaceSyncNodeProvisioner(options WorkspaceSyncNodeProvisionerOptions) (*WorkspaceSyncNodeProvisioner, error) {
	if options.Supervisor == nil || options.Store == nil || options.RelayGate == nil || options.QuotaSockets == nil || options.StartGuard == nil || len(options.DataKey) != 32 {
		return nil, errors.New("workspace sync node provisioner requires a supervisor, store, relay gate, quota sockets, start guard, and data key")
	}
	relay := options.Supervisor.relayConfig()
	if !filepath.IsAbs(relay.StateRoot) || filepath.Clean(relay.StateRoot) != relay.StateRoot || relay.StateRoot == string(filepath.Separator) ||
		!validWorkspaceSyncRelayHost(relay.PublicAddress) || !validWorkspaceSyncRelayBindAddress(relay.BindAddress) ||
		!workspaceSyncRelayTokenPattern.MatchString(relay.Token) {
		return nil, errors.New("workspace sync node supervisor relay configuration is invalid")
	}
	return &WorkspaceSyncNodeProvisioner{
		supervisor: options.Supervisor, store: options.Store, relayGate: options.RelayGate,
		quotaSockets: options.QuotaSockets, startGuard: options.StartGuard, dataKey: append([]byte(nil), options.DataKey...),
		relay: relay, retryOwners: make(map[string]struct{}),
	}, nil
}

// EnsureWorkspaceSyncNode creates or reuses the owner's persistent node and
// applies the current authorized device and folder snapshot before returning
// connection details to the desktop client.
func (p *WorkspaceSyncNodeProvisioner) EnsureWorkspaceSyncNode(ctx context.Context, ownerID string) (connection workspace.SyncNodeConnection, retErr error) {
	if ctx == nil || !domain.ValidUserID(ownerID) {
		return workspace.SyncNodeConnection{}, errors.New("invalid workspace sync node owner")
	}
	p.rememberRetryOwner(ownerID)
	key := p.apiKey(ownerID)
	defer func() {
		if retErr != nil {
			if domain.CodeOf(retErr) == domain.CodeWorkspaceBusy {
				retErr = redactWorkspaceSyncNodeSecrets(retErr, key, p.relay.Token)
			} else {
				retErr = p.stopOwnerAfterFailure(ownerID, key, retErr)
			}
		}
	}()
	if err := p.startGuard.CheckWorkspaceSyncNodeStart(ctx, ownerID); err != nil {
		return workspace.SyncNodeConnection{}, fmt.Errorf("workspace sync node cannot start while receive state is unresolved: %w", err)
	}
	if err := p.quotaSockets.EnsureOwner(ctx, ownerID); err != nil {
		return workspace.SyncNodeConnection{}, fmt.Errorf("prepare Workspace sync receive endpoint: %w", err)
	}

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
	apply := func(applyCtx context.Context, state workspace.SyncNodeDesiredState) error {
		return applyWorkspaceSyncNodeDesiredState(applyCtx, client, state, nodeID, nodeRelay)
	}
	enableRelay := func(gateCtx context.Context, state workspace.SyncNodeDesiredState) error {
		if len(state.Folders) == 0 {
			return nil
		}
		return p.relayGate.Enable(gateCtx, ownerID)
	}
	disableRelay := func(gateCtx context.Context) error { return p.relayGate.Disable(gateCtx, ownerID) }
	if err := p.store.ReconcileWorkspaceSyncNodeWithRelayGate(readyCtx, ownerID, apply, enableRelay, disableRelay); err != nil {
		return workspace.SyncNodeConnection{}, fmt.Errorf("reconcile Workspace sync node: %w", err)
	}
	p.completeRetryOwner(ownerID)
	return workspace.SyncNodeConnection{DeviceID: nodeID, RelayAddress: publicRelay}, nil
}

// ObserveWorkspaceSyncNode returns a read-only observation of the owner's
// existing node. It never ensures, starts, stops, configures, or gates a node.
func (p *WorkspaceSyncNodeProvisioner) ObserveWorkspaceSyncNode(ctx context.Context, ownerID string) (workspace.SyncNodeObservation, error) {
	if p == nil || ctx == nil || !domain.ValidUserID(ownerID) {
		return workspace.SyncNodeObservation{}, errWorkspaceSyncObservationInvalid
	}
	return p.supervisor.Observe(ctx, ownerID, p.apiKey(ownerID))
}

func (p *WorkspaceSyncNodeProvisioner) rememberRetryOwner(ownerID string) {
	p.retryMu.Lock()
	p.retryOwners[ownerID] = struct{}{}
	p.retryMu.Unlock()
}

func (p *WorkspaceSyncNodeProvisioner) completeRetryOwner(ownerID string) {
	p.retryMu.Lock()
	delete(p.retryOwners, ownerID)
	p.retryMu.Unlock()
}

func (p *WorkspaceSyncNodeProvisioner) pendingRetryOwners() []string {
	p.retryMu.Lock()
	defer p.retryMu.Unlock()
	owners := make([]string, 0, len(p.retryOwners))
	for ownerID := range p.retryOwners {
		owners = append(owners, ownerID)
	}
	sort.Strings(owners)
	return owners
}

func (p *WorkspaceSyncNodeProvisioner) restoreStartupRetryOwners(ctx context.Context) error {
	p.retryMu.Lock()
	pending := p.startupRecoveryPending
	p.retryMu.Unlock()
	if !pending {
		return nil
	}
	owners, err := p.store.ListWorkspaceSyncNodeOwners(ctx)
	if err != nil {
		return err
	}
	p.retryMu.Lock()
	if p.startupRecoveryPending {
		for _, ownerID := range owners {
			p.retryOwners[ownerID] = struct{}{}
		}
		p.startupRecoveryPending = false
	}
	p.retryMu.Unlock()
	return nil
}

func (p *WorkspaceSyncNodeProvisioner) stopOwnerAfterFailure(ownerID, apiKey string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), workspaceSyncNodeStopTimeout)
	defer cancel()
	if err := p.relayGate.Disable(ctx, ownerID); err != nil {
		cause = errors.Join(cause, fmt.Errorf("close Workspace sync relay gate after provisioning failure: %w", err))
	}
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
	if err := p.store.RevokeExpiredWorkspaceSyncDevices(ctx, p.DisableWorkspaceSyncNodeRelay); err != nil {
		return fmt.Errorf("revoke expired Workspace sync devices: %w", err)
	}
	if err := p.restoreStartupRetryOwners(ctx); err != nil {
		return fmt.Errorf("restore Workspace sync startup owner list: %w", err)
	}
	owners, err := p.store.ListWorkspaceSyncNodeOwnersToReconcile(ctx)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(owners))
	for _, ownerID := range owners {
		seen[ownerID] = struct{}{}
	}
	for _, ownerID := range p.pendingRetryOwners() {
		if _, ok := seen[ownerID]; !ok {
			owners = append(owners, ownerID)
		}
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

// StopAllWorkspaceSyncNodesForStartup closes every owner's relay route before
// startup recovery rewrites a current directory that Syncthing can modify.
func (p *WorkspaceSyncNodeProvisioner) StopAllWorkspaceSyncNodesForStartup(ctx context.Context) error {
	if ctx == nil {
		return errors.New("workspace sync startup stop context is required")
	}
	owners, err := p.store.ListWorkspaceSyncNodeOwners(ctx)
	if err != nil {
		return fmt.Errorf("list Workspace sync owners before startup recovery: %w", err)
	}
	sort.Strings(owners)
	var failures []error
	for _, ownerID := range owners {
		p.rememberRetryOwner(ownerID)
		if err := p.relayGate.Disable(ctx, ownerID); err != nil {
			failures = append(failures, fmt.Errorf("close Workspace sync relay gate for owner %s: %w", ownerID, err))
		}
		if err := p.supervisor.StopOwner(ctx, ownerID); err != nil {
			failures = append(failures, fmt.Errorf("stop Workspace sync node for owner %s: %w", ownerID, err))
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return errors.Join(failures...)
}

// ReconcileWorkspaceSyncNodesOnStartup reapplies persisted owner state after
// the firewall preload has reset every dynamic relay allow entry.
func (p *WorkspaceSyncNodeProvisioner) ReconcileWorkspaceSyncNodesOnStartup(ctx context.Context) error {
	if ctx == nil {
		return errors.New("workspace sync startup reconciliation context is required")
	}
	p.retryMu.Lock()
	p.startupRecoveryPending = true
	p.retryMu.Unlock()
	if err := p.store.RevokeExpiredWorkspaceSyncDevices(ctx, p.DisableWorkspaceSyncNodeRelay); err != nil {
		return fmt.Errorf("revoke expired Workspace sync devices on startup: %w", err)
	}
	owners, err := p.store.ListWorkspaceSyncNodeOwners(ctx)
	if err != nil {
		return err
	}
	p.retryMu.Lock()
	for _, ownerID := range owners {
		p.retryOwners[ownerID] = struct{}{}
	}
	p.startupRecoveryPending = false
	p.retryMu.Unlock()
	var failures []error
	for _, ownerID := range owners {
		if _, err := p.EnsureWorkspaceSyncNode(ctx, ownerID); err != nil {
			failures = append(failures, fmt.Errorf("restore Workspace sync node for owner %s: %w", ownerID, err))
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return errors.Join(failures...)
}

func (p *WorkspaceSyncNodeProvisioner) DisableWorkspaceSyncNodeRelay(ctx context.Context, ownerID string) error {
	if ctx == nil || !domain.ValidUserID(ownerID) {
		return errors.New("invalid Workspace sync relay owner")
	}
	return p.relayGate.Disable(ctx, ownerID)
}

func (p *WorkspaceSyncNodeProvisioner) ReconcileWorkspaceSyncNodeOwner(ctx context.Context, ownerID string) error {
	if ctx == nil || !domain.ValidUserID(ownerID) {
		return errors.New("invalid workspace sync node owner")
	}
	owners, err := p.store.ListWorkspaceSyncNodeOwners(ctx)
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
				return "", errors.New("workspace sync relay certificate is unreadable")
			}
			block, _ := pem.Decode(data)
			if block == nil || block.Type != "CERTIFICATE" {
				return "", errors.New("workspace sync relay certificate is invalid")
			}
			id, parseErr := workspace.SyncthingDeviceIDFromCertificateDER(block.Bytes)
			if parseErr != nil {
				return "", errors.New("workspace sync relay certificate is invalid")
			}
			return id, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", errors.New("workspace sync relay certificate is unavailable")
		}
		select {
		case <-ctx.Done():
			return "", errors.New("workspace sync relay certificate is not ready")
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
