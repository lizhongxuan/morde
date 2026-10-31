package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"morde-sync/client"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacecurrent"
	"morde/server/internal/storage/workspacefs"
	"morde/server/internal/worker"
)

func TestWorkspaceSyncPostgresQuotaUDSDockerAndHostE2E(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires a native Ubuntu Docker host")
	}
	if os.Getenv("CH_TEST_WORKSPACE_SYNC_POSTGRES_DOCKER") != "1" {
		t.Skip("set CH_TEST_WORKSPACE_SYNC_POSTGRES_DOCKER=1 to run the Ubuntu PostgreSQL/Syncthing E2E")
	}
	if !f02IsUbuntuHost() {
		t.Fatal("refusing to run the Workspace sync Docker E2E outside a native Ubuntu host")
	}

	root := f02WorkspaceSyncDockerEnv(t, "CH_TEST_WORKSPACE_SYNC_DOCKER_ROOT")
	socket := f02WorkspaceSyncDockerEnv(t, "CH_TEST_WORKSPACE_SYNC_DOCKER_SOCKET")
	network := f02WorkspaceSyncDockerEnv(t, "CH_TEST_WORKSPACE_SYNC_DOCKER_NETWORK")
	relayName := f02WorkspaceSyncDockerEnv(t, "CH_TEST_WORKSPACE_SYNC_DOCKER_RELAY_NAME")
	relayAddress := f02WorkspaceSyncDockerEnv(t, "CH_TEST_WORKSPACE_SYNC_DOCKER_RELAY_ADDRESS")
	relayBindAddress := f02WorkspaceSyncDockerEnv(t, "CH_TEST_WORKSPACE_SYNC_DOCKER_RELAY_BIND_ADDRESS")
	t.Setenv("CH_WORKSPACE_SYNC_RELAY_PUBLIC_ADDRESS", relayAddress)
	t.Setenv("CH_WORKSPACE_SYNC_RELAY_BIND_ADDRESS", relayBindAddress)
	relayToken := f02WorkspaceSyncDockerEnv(t, "CH_TEST_WORKSPACE_SYNC_DOCKER_RELAY_TOKEN")
	relayImage := f02WorkspaceSyncDockerEnv(t, "CH_TEST_WORKSPACE_SYNC_DOCKER_RELAY_IMAGE")
	nodeImage := f02WorkspaceSyncDockerEnv(t, "CH_TEST_WORKSPACE_SYNC_DOCKER_NODE_IMAGE")
	if !filepath.IsAbs(root) || !filepath.IsAbs(socket) || !f02IsImageDigest(relayImage) || !f02IsImageDigest(nodeImage) {
		t.Fatal("Ubuntu Docker integration configuration is invalid")
	}
	endpoint, err := f02WorkspaceSyncDockerCommand(context.Background(), "context", "inspect", "--format", `{{ (index .Endpoints "docker").Host }}`)
	if err != nil || strings.TrimSpace(string(endpoint)) != "unix://"+socket {
		t.Fatal("Docker CLI and Syncthing supervisor must use the same Unix socket")
	}
	engineOS, err := f02WorkspaceSyncDockerCommand(context.Background(), "info", "--format", "{{.OperatingSystem}}")
	if err != nil || !strings.Contains(strings.ToLower(string(engineOS)), "ubuntu") {
		t.Fatalf("refusing a non-Ubuntu Docker engine: %q, err=%v", strings.TrimSpace(string(engineOS)), err)
	}
	if override := os.Getenv("DOCKER_HOST"); override != "" && override != "unix://"+socket {
		t.Fatal("refusing a non-local Docker host override")
	}
	policyContext, policyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	policyOutput, policyErr := exec.CommandContext(policyContext, "/opt/cloudhost/network-policy.sh", "check").CombinedOutput()
	policyCancel()
	if policyErr != nil {
		t.Fatalf("require the installed default-deny Workspace sync network policy before creating nodes: %v: %s", policyErr, strings.TrimSpace(string(policyOutput)))
	}
	store, pool := newTemporaryStore(t)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatalf("create integration root: %v", err)
	}
	runRoot, err := os.MkdirTemp(root, "workspace-sync-postgres-e2e-")
	if err != nil {
		t.Fatalf("create integration directory: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var cleanupOwnerID, pcName, nodeName, networkName string
	var quotaSockets *worker.WorkspaceSyncNodeQuotaSockets
	relayGate := worker.NewCommandWorkspaceSyncRelayGate()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if cleanupOwnerID != "" {
			if err := relayGate.Disable(cleanupCtx, cleanupOwnerID); err != nil {
				t.Errorf("disable test relay gate: %v", err)
			}
		}
		if pcName != "" {
			_, _ = f02WorkspaceSyncDockerCommand(cleanupCtx, "rm", "--force", pcName)
		}
		if nodeName != "" {
			_, _ = f02WorkspaceSyncDockerCommand(cleanupCtx, "rm", "--force", nodeName)
		}
		if quotaSockets != nil {
			if err := quotaSockets.Close(); err != nil {
				t.Errorf("close quota UDS: %v", err)
			}
		}
		if networkName != "" {
			_, _ = f02WorkspaceSyncDockerCommand(cleanupCtx, "network", "rm", networkName)
		}
		if err := os.RemoveAll(runRoot); err != nil {
			t.Errorf("remove integration directory: %v", err)
		}
	})

	owner := createTestUser(t, store, 3, 10_000, 20_000)
	cleanupOwnerID = owner.ID
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "postgres-syncthing-e2e")
	ownerHash := sha256.Sum256([]byte(owner.ID))
	ownerHashHex := hex.EncodeToString(ownerHash[:])
	pcName = fmt.Sprintf("morde-f02-pc-%d", time.Now().UnixNano())
	nodeName = "morde-sync-" + ownerHashHex[:32]
	networkName = "morde-sync-net-" + ownerHashHex[:32]
	quotaSocketRoot := filepath.Join(runRoot, "quota-sockets")
	pcState := filepath.Join(runRoot, "pc-state")
	pcFiles := filepath.Join(runRoot, "pc-files")
	currentRoot := filepath.Join(runRoot, "current")
	blockRoot := filepath.Join(runRoot, "blocks")
	nodeStateRoot := filepath.Join(runRoot, "node-state")
	relayStateRoot := filepath.Join(root, "relay-state")
	for _, dir := range []string{quotaSocketRoot, pcState, pcFiles} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatalf("create integration directory %q: %v", dir, err)
		}
	}
	if err := os.Chmod(pcState, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pcFiles, 0777); err != nil {
		t.Fatal(err)
	}
	blocks, err := workspacefs.Open(blockRoot)
	if err != nil {
		t.Fatalf("open Workspace block store: %v", err)
	}
	t.Cleanup(func() { _ = blocks.Close() })
	current, err := workspacecurrent.Open(currentRoot)
	if err != nil {
		t.Fatalf("open Workspace current store: %v", err)
	}
	t.Cleanup(func() { _ = current.Close() })
	syncService, err := app.NewWorkspaceSyncService(store, blocks, workspace.MaxSyncManifestBytes, 5<<30)
	if err != nil {
		t.Fatalf("create Workspace sync service: %v", err)
	}
	if err := syncService.SetWorkspaceCurrentMaterializer(func(ctx context.Context, ownerID, workspaceID string, revision int64, files []workspace.File, emptyDirectories []string) error {
		return current.Materialize(ctx, ownerID, workspaceID, revision, files, emptyDirectories, blocks)
	}); err != nil {
		t.Fatal(err)
	}
	if err := syncService.SetWorkspaceCurrentRevisionReader(current.CurrentRevision); err != nil {
		t.Fatal(err)
	}
	if err := syncService.InitializeWorkspaceCurrent(ctx, workspaceValue.ID); err != nil {
		t.Fatalf("initialize Workspace current directory: %v", err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	hostReplicas, err := app.NewHostReplicaService(store, store, store, sealer)
	if err != nil {
		t.Fatalf("create Host replica service: %v", err)
	}
	router, err := api.New(api.Deps{
		Hosts: unusedWorkspaceSyncHostService{}, HostReplicas: hostReplicas,
		TLSAuthorizer: unusedWorkspaceSyncTLSAuthorizer{}, Verifier: unusedWorkspaceSyncVerifier{},
		Auth: unusedWorkspaceSyncAuth{}, WorkspaceSync: syncService, MCPDisabled: true,
	}, api.Options{
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), APIMaxBodyBytes: 16 << 20,
		MCPMaxBodyBytes: 16 << 20, SessionTTL: time.Hour, HostScheme: "https",
		PortalDomain: "portal.example.test", HostSuffix: "example-hosts.test", PreviewPortMin: 1024,
	})
	if err != nil {
		t.Fatalf("create Portal API: %v", err)
	}
	portal := httptest.NewServer(router)
	defer portal.Close()

	pcAPIKey := "morde-f02-postgres-pc-api-key-0123456789"
	if _, err := f02WorkspaceSyncDockerCommand(ctx, "run", "--detach", "--pull=never", "--name", pcName,
		"--network", network, "--network-alias", "morde-f02-postgres-pc", "--publish", "127.0.0.1::8384/tcp", "--expose", "8384/tcp",
		"--user", "1000:1000", "--env", "STGUIADDRESS=0.0.0.0:8384", "--env", "STGUIAPIKEY="+pcAPIKey, "--env", "STNORESTART=1",
		"--mount", "type=bind,src="+pcState+",dst=/var/syncthing", "--mount", "type=bind,src="+pcFiles+",dst=/sync",
		nodeImage, "serve", "--no-browser", "--no-restart"); err != nil {
		t.Fatalf("start PC Syncthing node: %v", err)
	}
	portOutput, err := f02WorkspaceSyncDockerCommand(ctx, "port", pcName, "8384/tcp")
	if err != nil {
		t.Fatalf("read PC Syncthing API port: %v", err)
	}
	_, port, err := net.SplitHostPort(strings.TrimSpace(string(portOutput)))
	if err != nil {
		t.Fatalf("parse PC Syncthing API port: %v", err)
	}
	pcAPI, err := worker.NewSyncthingAPIClient("http://127.0.0.1:"+port, pcAPIKey)
	if err != nil {
		t.Fatalf("configure PC Syncthing API: %v", err)
	}
	if err := f02WaitForSyncthingAPI(ctx, pcAPI); err != nil {
		t.Fatalf("wait for PC Syncthing API: %v", err)
	}
	pcID, err := pcAPI.NodeID(ctx)
	if err != nil {
		t.Fatalf("read PC Syncthing Device ID: %v", err)
	}
	principal := syncDeviceTestPrincipal(t, store, owner.ID)
	deviceBinding := registerTestWorkspaceSyncDevice(t, store, principal, workspaceValue.ID, pcID, "f02-postgres-e2e:"+pcID)

	quotaService, err := app.NewWorkspaceSyncQuotaService(syncService, current)
	if err != nil {
		t.Fatalf("create production quota service: %v", err)
	}
	quotaSockets, err = worker.NewWorkspaceSyncNodeQuotaSockets(quotaSocketRoot, quotaService)
	if err != nil {
		t.Fatalf("create production quota UDS: %v", err)
	}
	supervisor, err := worker.NewDockerWorkspaceSyncNodeSupervisor(worker.DockerWorkspaceSyncNodeSupervisorOptions{
		ImageDigest: nodeImage, RelayImageDigest: relayImage, RelayPublicAddress: relayAddress,
		RelayBindAddress: relayBindAddress, RelayToken: relayToken, DockerSocket: socket,
		RelayStateRoot: relayStateRoot, StateRoot: nodeStateRoot, WorkspaceRoot: currentRoot,
		QuotaSocketRoot: quotaSocketRoot, RelayContainerName: relayName,
		Resources: worker.DockerWorkspaceSyncNodeResourceLimits{MemoryBytes: 256 << 20, NanoCPUs: 250_000_000, PIDsLimit: 64},
	})
	if err != nil {
		t.Fatalf("create production Syncthing supervisor: %v", err)
	}
	startGuard := worker.WorkspaceSyncNodeStartGuardFunc(func(ctx context.Context, ownerID string) error {
		owners, err := syncService.ListActiveWorkspaceSyncReceiveOwners(ctx)
		if err != nil {
			return err
		}
		for _, activeOwner := range owners {
			if activeOwner == ownerID {
				return domain.NewError(domain.CodeWorkspaceBusy, "Workspace has an unresolved file receive")
			}
		}
		return nil
	})
	provisioner, err := worker.NewWorkspaceSyncNodeProvisioner(worker.WorkspaceSyncNodeProvisionerOptions{
		Supervisor: supervisor, Store: store, RelayGate: relayGate,
		QuotaSockets: quotaSockets, StartGuard: startGuard, DataKey: []byte(strings.Repeat("k", 32)),
	})
	if err != nil {
		t.Fatalf("create production Syncthing provisioner: %v", err)
	}
	connection, err := provisioner.EnsureWorkspaceSyncNode(ctx, owner.ID)
	if err != nil {
		t.Fatalf("provision Workspace Syncthing node: %v", err)
	}
	if connection.DeviceID == pcID {
		t.Fatal("PC and server Syncthing identities are the same")
	}
	pcRelayURL, err := url.Parse(connection.RelayAddress)
	if err != nil {
		t.Fatalf("parse private relay address: %v", err)
	}
	pcRelayURL.Host = net.JoinHostPort(relayBindAddress, "22067")
	pcRelayAddress := pcRelayURL.String()
	if err := pcAPI.ConfigurePrivateRelay(ctx, pcRelayAddress); err != nil {
		t.Fatalf("configure PC private relay: %v", err)
	}
	deviceRaw, err := pcAPI.GetDefaultDevice(ctx)
	if err != nil {
		t.Fatalf("read PC default device configuration: %v", err)
	}
	deviceConfig, err := f02ConfigureSyncthingDevice(deviceRaw, connection.DeviceID, pcRelayAddress)
	if err != nil {
		t.Fatalf("configure server device on PC: %v", err)
	}
	if err := pcAPI.PutDevice(ctx, connection.DeviceID, deviceConfig); err != nil {
		t.Fatalf("share PC with server node: %v", err)
	}
	folderRaw, err := pcAPI.GetDefaultFolder(ctx)
	if err != nil {
		t.Fatalf("read PC default folder configuration: %v", err)
	}
	folderConfig, err := f02ConfigureSyncthingFolder(folderRaw, deviceBinding.FolderID, "/sync", []string{pcID, connection.DeviceID})
	if err != nil {
		t.Fatalf("configure PC Workspace folder: %v", err)
	}
	if err := pcAPI.PutFolder(ctx, deviceBinding.FolderID, folderConfig); err != nil {
		t.Fatalf("share Workspace folder with server: %v", err)
	}
	if err := f02WaitForSyncthingConnection(ctx, pcAPI, connection.DeviceID, true); err != nil {
		t.Fatalf("wait for private relay connection: %v", err)
	}

	pcFile := []byte("PC to production quota UDS, PostgreSQL, and current projection\n")
	if err := os.WriteFile(filepath.Join(pcFiles, "from-pc.txt"), pcFile, 0600); err != nil {
		t.Fatalf("write PC test file: %v", err)
	}
	serverFile := filepath.Join(currentRoot, ownerHashHex, workspaceValue.ID, "from-pc.txt")
	if err := f02WaitForFileContent(ctx, serverFile, pcFile); err != nil {
		t.Fatalf("wait for server current file: %v", err)
	}
	if resultRevision := f02WaitForReceiveSettlement(t, ctx, pool, owner.ID, workspaceValue.ID, "from-pc.txt"); resultRevision != 1 {
		t.Fatalf("Syncthing receive result revision = %d, want 1", resultRevision)
	}
	workspaceAfterPC, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || workspaceAfterPC.Revision != 1 {
		t.Fatalf("Workspace after PC receive = revision %d, err=%v", workspaceAfterPC.Revision, err)
	}
	manifest, err := store.GetWorkspaceManifestPage(ctx, workspaceValue.ID, 1, "", "", 100)
	if err != nil || len(manifest.Files) != 1 || manifest.Files[0].Path != "from-pc.txt" {
		t.Fatalf("Workspace manifest after PC receive = %+v, err=%v", manifest, err)
	}
	pcDigest := sha256.Sum256(pcFile)
	if manifest.Files[0].SHA256 != hex.EncodeToString(pcDigest[:]) {
		t.Fatalf("Workspace manifest digest = %s, want %x", manifest.Files[0].SHA256, pcDigest)
	}
	var desiredRevision, appliedRevision int64
	if err := pool.QueryRow(ctx, `SELECT desired_revision, applied_revision FROM workspace_current_materializations WHERE workspace_id=$1`, workspaceValue.ID).
		Scan(&desiredRevision, &appliedRevision); err != nil || desiredRevision != 1 || appliedRevision != 1 {
		t.Fatalf("current materialization revisions = desired:%d applied:%d err=%v", desiredRevision, appliedRevision, err)
	}

	hostValue, rootCredential := createHostHandoffTestHost(t, ctx, store, sealer, owner.ID,
		app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{workspaceValue.ID})
	hostCredential, binding := hostHandoffBindingCredential(t, portal.Client(), portal.URL, hostValue, rootCredential, workspaceValue.ID)
	hostDir := filepath.Join(runRoot, "host-files")
	if err := os.Mkdir(hostDir, 0700); err != nil {
		t.Fatalf("create Host replica directory: %v", err)
	}
	hostClient, err := client.New(portal.URL, hostCredential, portal.Client())
	if err != nil {
		t.Fatalf("create Host sync client: %v", err)
	}
	zeroRevision := int64(0)
	initialHost, err := hostClient.Sync(ctx, client.SyncOptions{
		WorkspaceID: workspaceValue.ID, Directory: hostDir, Direction: "download", Policy: "fill_missing", BaseRevision: &zeroRevision,
	})
	if err != nil || initialHost.Session.State != "completed" || initialHost.Session.TargetRevision != 1 {
		t.Fatalf("Host initial download = %#v, err=%v", initialHost.Session, err)
	}
	hostHandoffRecordApplied(t, portal.Client(), portal.URL, hostValue, binding.BindingVersion, workspaceValue.ID,
		rootCredential, initialHost.Session.ID, initialHost.Session.TargetRevision)
	hostHandoffActivate(t, portal.Client(), portal.URL, hostValue, binding.BindingVersion, workspaceValue.ID, rootCredential)
	var baseline client.SyncSession
	baselineRevision, err := hostClient.HandoffCheckpoint(ctx, client.LiveSyncOptions{
		WorkspaceID: workspaceValue.ID, Directory: hostDir,
		OnResult: func(result client.Result) { baseline = result.Session },
	})
	if err != nil || baselineRevision != 1 || baseline.ID == "" {
		t.Fatalf("Host baseline checkpoint = revision %d, session %#v, err=%v", baselineRevision, baseline, err)
	}
	hostHandoffRecordApplied(t, portal.Client(), portal.URL, hostValue, binding.BindingVersion, workspaceValue.ID,
		rootCredential, baseline.ID, baseline.TargetRevision)
	writeWorkspaceSyncE2EFile(t, hostDir, "from-host.txt", []byte("Host API to Workspace current tree to PC\n"))
	activeBinding, err := store.GetWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID)
	if err != nil {
		t.Fatalf("read Host binding before handoff: %v", err)
	}
	unbinding, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, activeBinding.BindingVersion,
		workspace.BindingActive, workspace.BindingUnbinding, activeBinding.DesiredRevision, activeBinding.AppliedRevision)
	if err != nil || unbinding.State != workspace.BindingUnbinding {
		t.Fatalf("begin Host unbinding = %+v, err=%v", unbinding, err)
	}
	unbindingCredential, unbindingBinding := hostHandoffBindingCredential(t, portal.Client(), portal.URL, hostValue, rootCredential, workspaceValue.ID)
	hostClient, err = client.New(portal.URL, unbindingCredential, portal.Client())
	if err != nil {
		t.Fatalf("create unbinding Host sync client: %v", err)
	}
	var finalCheckpoint client.SyncSession
	checkpointRevision, err := hostClient.HandoffCheckpoint(ctx, client.LiveSyncOptions{
		WorkspaceID: workspaceValue.ID, Directory: hostDir,
		OnResult: func(result client.Result) { finalCheckpoint = result.Session },
	})
	if err != nil || checkpointRevision != 2 || finalCheckpoint.ID == "" || finalCheckpoint.TargetRevision != 2 {
		t.Fatalf("Host final checkpoint = revision %d, session %#v, err=%v", checkpointRevision, finalCheckpoint, err)
	}
	hostHandoffRecordApplied(t, portal.Client(), portal.URL, hostValue, unbindingBinding.BindingVersion, workspaceValue.ID,
		rootCredential, finalCheckpoint.ID, checkpointRevision)
	hostHandoffDetach(t, portal.Client(), portal.URL, hostValue, unbindingBinding.BindingVersion, workspaceValue.ID, rootCredential, checkpointRevision)
	if err := f02WaitForFileContent(ctx, filepath.Join(pcFiles, "from-host.txt"), []byte("Host API to Workspace current tree to PC\n")); err != nil {
		t.Fatalf("wait for Host file on PC: %v", err)
	}
	workspaceAfterHost, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || workspaceAfterHost.Revision != 2 {
		t.Fatalf("Workspace after Host checkpoint = revision %d, err=%v", workspaceAfterHost.Revision, err)
	}
	if _, err := store.GetWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("Host binding after applied detach = %v, want not found", err)
	}
}

func f02WorkspaceSyncDockerEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("%s is required when the Ubuntu Docker E2E is enabled", name)
	}
	return value
}

func f02IsUbuntuHost() bool {
	release, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(release), "\n") {
		if strings.TrimSpace(strings.Trim(strings.TrimPrefix(line, "ID="), `"`)) == "ubuntu" {
			return true
		}
	}
	return false
}

func f02IsImageDigest(value string) bool {
	digest := value
	if !strings.HasPrefix(digest, "sha256:") {
		at := strings.LastIndex(digest, "@sha256:")
		if at < 1 {
			return false
		}
		digest = digest[at+1:]
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
	return strings.HasPrefix(digest, "sha256:") && err == nil && len(decoded) == sha256.Size
}

func f02WorkspaceSyncDockerCommand(ctx context.Context, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func f02WaitForSyncthingAPI(ctx context.Context, client *worker.SyncthingAPIClient) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := client.SystemStatus(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func f02WaitForSyncthingConnection(ctx context.Context, client *worker.SyncthingAPIClient, deviceID string, connected bool) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		raw, err := client.Connections(ctx)
		if err == nil {
			var state struct {
				Connections map[string]struct {
					Connected bool `json:"connected"`
				} `json:"connections"`
			}
			if json.Unmarshal(raw, &state) == nil && state.Connections != nil {
				if connection, ok := state.Connections[deviceID]; ok && connection.Connected == connected {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func f02WaitForFileContent(ctx context.Context, path string, expected []byte) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		actual, err := os.ReadFile(path)
		if err == nil && bytes.Equal(actual, expected) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func f02WaitForReceiveSettlement(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ownerID, workspaceID, filePath string) int64 {
	t.Helper()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		var state string
		var revision int64
		err := pool.QueryRow(ctx, `SELECT state, COALESCE(result_revision,-1) FROM workspace_sync_receive_reservations
WHERE owner_id=$1 AND workspace_id=$2 AND file_path=$3 ORDER BY created_at DESC LIMIT 1`, ownerID, workspaceID, filePath).
			Scan(&state, &revision)
		if err == nil && state == "settled" {
			return revision
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("read Workspace receive settlement: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Workspace receive did not settle: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func f02ConfigureSyncthingDevice(raw json.RawMessage, deviceID, relayAddress string) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) == 0 {
		return nil, errors.New("invalid Syncthing default device configuration")
	}
	values := map[string]any{
		"deviceID": deviceID, "name": "Morde", "addresses": []string{relayAddress},
		"autoAcceptFolders": false, "introducer": false, "paused": false,
	}
	for key, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		fields[key] = encoded
	}
	return json.Marshal(fields)
}

func f02ConfigureSyncthingFolder(raw json.RawMessage, folderID, folderPath string, deviceIDs []string) (json.RawMessage, error) {
	if len(deviceIDs) != 2 {
		return nil, errors.New("syncthing Workspace folder requires exactly two devices")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) == 0 {
		return nil, errors.New("invalid Syncthing default folder configuration")
	}
	for key, value := range map[string]any{
		"id": folderID, "label": "Morde Workspace", "path": folderPath, "type": "sendreceive", "paused": false,
		"devices": []map[string]string{{"deviceID": deviceIDs[0]}, {"deviceID": deviceIDs[1]}},
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		fields[key] = encoded
	}
	delete(fields, "mordeQuotaSocket")
	delete(fields, "mordeWorkspaceId")
	return json.Marshal(fields)
}
