package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

func TestWorkspaceSyncNodeProvisionerRealSyncthingDockerE2E(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires a native Linux Docker host that can reach private bridge addresses")
	}
	if os.Getenv("CH_TEST_WORKSPACE_SYNC_DOCKER") != "1" {
		t.Skip("set CH_TEST_WORKSPACE_SYNC_DOCKER=1 to run the local Docker Syncthing integration")
	}
	root := workspaceSyncDockerTestEnv(t, "CH_TEST_WORKSPACE_SYNC_DOCKER_ROOT")
	socket := workspaceSyncDockerTestEnv(t, "CH_TEST_WORKSPACE_SYNC_DOCKER_SOCKET")
	network := workspaceSyncDockerTestEnv(t, "CH_TEST_WORKSPACE_SYNC_DOCKER_NETWORK")
	relayName := workspaceSyncDockerTestEnv(t, "CH_TEST_WORKSPACE_SYNC_DOCKER_RELAY_NAME")
	relayAddress := workspaceSyncDockerTestEnv(t, "CH_TEST_WORKSPACE_SYNC_DOCKER_RELAY_ADDRESS")
	relayBindAddress := workspaceSyncDockerTestEnv(t, "CH_TEST_WORKSPACE_SYNC_DOCKER_RELAY_BIND_ADDRESS")
	relayToken := workspaceSyncDockerTestEnv(t, "CH_TEST_WORKSPACE_SYNC_DOCKER_RELAY_TOKEN")
	relayImage := workspaceSyncDockerTestEnv(t, "CH_TEST_WORKSPACE_SYNC_DOCKER_RELAY_IMAGE")
	nodeImage := workspaceSyncDockerTestEnv(t, "CH_TEST_WORKSPACE_SYNC_DOCKER_NODE_IMAGE")
	if !strings.HasPrefix(socket, string(filepath.Separator)) || !filepath.IsAbs(root) ||
		!dockerWorkspaceSyncImageDigest.MatchString(relayImage) || !dockerWorkspaceSyncImageDigest.MatchString(nodeImage) {
		t.Fatal("local Docker integration configuration is invalid")
	}
	endpointOutput, err := workspaceSyncDockerCommand(context.Background(), "context", "inspect", "--format", "{{ (index .Endpoints \"docker\").Host }}")
	if err != nil || strings.TrimSpace(string(endpointOutput)) != "unix://"+socket {
		t.Fatal("local Docker CLI and supervisor must use the same Unix socket")
	}
	if override := os.Getenv("DOCKER_HOST"); override != "" && override != "unix://"+socket {
		t.Fatal("local Docker integration refuses a non-local Docker host override")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal("create local integration root")
	}
	runRoot, err := os.MkdirTemp(root, "workspace-sync-e2e-")
	if err != nil {
		t.Fatal("create local integration directory")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ownerID, err := domain.NewUserID()
	if err != nil {
		t.Fatal("generate integration owner ID")
	}
	workspaceID, err := domain.NewWorkspaceID()
	if err != nil {
		t.Fatal("generate integration Workspace ID")
	}
	folderID := "morde-" + strings.Repeat("a", 32)
	pcName := fmt.Sprintf("morde-f02-pc-%d", time.Now().UnixNano())
	ownerHash := sha256.Sum256([]byte(ownerID))
	nodeName := "morde-sync-" + hex.EncodeToString(ownerHash[:])[:32]
	networkName := "morde-sync-net-" + hex.EncodeToString(ownerHash[:])[:32]
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		_, _ = workspaceSyncDockerCommand(cleanupCtx, "rm", "--force", pcName)
		_, _ = workspaceSyncDockerCommand(cleanupCtx, "rm", "--force", nodeName)
		_, _ = workspaceSyncDockerCommand(cleanupCtx, "network", "rm", networkName)
		_ = os.RemoveAll(runRoot)
	}()

	pcState := filepath.Join(runRoot, "pc-state")
	pcFiles := filepath.Join(runRoot, "pc-files")
	for _, path := range []string{pcState, pcFiles} {
		if err := os.MkdirAll(path, 0777); err != nil {
			t.Fatal("create PC integration directory")
		}
		if err := os.Chmod(path, 0777); err != nil {
			t.Fatal("prepare PC integration directory")
		}
	}
	pcAPIKey := "morde-f02-local-pc-api-key-0123456789"
	if _, err := workspaceSyncDockerCommand(ctx, "run", "--detach", "--pull=never", "--name", pcName,
		"--network", network, "--network-alias", "morde-f02-pc", "--publish", "127.0.0.1::8384/tcp", "--expose", "8384/tcp",
		"--user", "1000:1000", "--env", "STGUIADDRESS=0.0.0.0:8384", "--env", "STGUIAPIKEY="+pcAPIKey, "--env", "STNORESTART=1",
		"--mount", "type=bind,src="+pcState+",dst=/var/syncthing", "--mount", "type=bind,src="+pcFiles+",dst=/sync",
		nodeImage, "serve", "--no-browser", "--no-restart"); err != nil {
		t.Fatalf("start local PC Syncthing node: %v", err)
	}
	portOutput, err := workspaceSyncDockerCommand(ctx, "port", pcName, "8384/tcp")
	if err != nil {
		t.Fatalf("read local PC Syncthing API port: %v", err)
	}
	_, port, err := net.SplitHostPort(strings.TrimSpace(string(portOutput)))
	if err != nil {
		t.Fatalf("parse local PC Syncthing API port: %v", err)
	}
	pcAPI, err := NewSyncthingAPIClient("http://127.0.0.1:"+port, pcAPIKey)
	if err != nil {
		t.Fatal("configure local PC Syncthing API client")
	}
	if err := waitForSyncthingAPI(ctx, pcAPI); err != nil {
		t.Fatalf("wait for local PC Syncthing API: %v", err)
	}
	pcID, err := pcAPI.NodeID(ctx)
	if err != nil {
		t.Fatalf("read local PC Syncthing identity: %v", err)
	}

	store := &workspaceSyncNodeStoreStub{state: workspace.SyncNodeDesiredState{
		Revision: 1,
		Folders:  []workspace.SyncNodeFolder{{WorkspaceID: workspaceID, FolderID: folderID, DeviceIDs: []string{pcID}}},
	}}
	supervisor, err := NewDockerWorkspaceSyncNodeSupervisor(DockerWorkspaceSyncNodeSupervisorOptions{
		ImageDigest: nodeImage, RelayImageDigest: relayImage, RelayPublicAddress: relayAddress,
		RelayBindAddress: relayBindAddress, RelayToken: relayToken,
		DockerSocket: socket, RelayStateRoot: filepath.Join(root, "relay-state"), StateRoot: filepath.Join(runRoot, "node-state"),
		WorkspaceRoot: filepath.Join(runRoot, "workspaces"), RelayContainerName: relayName,
		Resources: DockerWorkspaceSyncNodeResourceLimits{MemoryBytes: 256 << 20, NanoCPUs: 250_000_000, PIDsLimit: 64},
	})
	if err != nil {
		t.Fatalf("initialize local Docker node supervisor: %v", err)
	}
	provisioner, err := NewWorkspaceSyncNodeProvisioner(WorkspaceSyncNodeProvisionerOptions{
		Supervisor: supervisor, Store: store, DataKey: []byte(strings.Repeat("k", 32)),
	})
	if err != nil {
		t.Fatalf("initialize local node provisioner: %v", err)
	}
	if _, err := supervisor.Ensure(ctx, ownerID, provisioner.apiKey(ownerID)); err != nil {
		t.Fatalf("start local Syncthing supervisor node: %v", err)
	}
	connection, err := provisioner.EnsureWorkspaceSyncNode(ctx, ownerID)
	if err != nil {
		t.Fatalf("provision real Syncthing node: %v", err)
	}
	node, err := supervisor.Ensure(ctx, ownerID, provisioner.apiKey(ownerID))
	if err != nil {
		t.Fatalf("inspect provisioned Syncthing node: %v", err)
	}
	nodeAPI, err := NewSyncthingAPIClient(node.APIURL, provisioner.apiKey(ownerID))
	if err != nil {
		t.Fatal("configure provisioned node API client")
	}
	serverID := connection.DeviceID
	if serverID == pcID || !validSyncthingDeviceID(serverID) {
		t.Fatal("provisioned server node returned an invalid or duplicate device identity")
	}

	// This PC is a container on the same Ubuntu host, so avoid requiring provider NAT hairpin.
	pcRelayURL, err := url.Parse(connection.RelayAddress)
	if err != nil {
		t.Fatal("parse public PC relay address")
	}
	pcRelayURL.Host = net.JoinHostPort(relayBindAddress, "22067")
	pcRelayAddress := pcRelayURL.String()
	if err := pcAPI.ConfigurePrivateRelay(ctx, pcRelayAddress); err != nil {
		t.Fatalf("configure local PC private relay: %v", err)
	}
	deviceRaw, err := pcAPI.GetDefaultDevice(ctx)
	if err != nil {
		t.Fatalf("read local PC default device configuration: %v", err)
	}
	deviceConfig, err := configureSyncthingDevice(deviceRaw, serverID, pcRelayAddress)
	if err != nil {
		t.Fatalf("configure local PC server device: %v", err)
	}
	if err := pcAPI.PutDevice(ctx, serverID, deviceConfig); err != nil {
		t.Fatalf("share local PC with server node: %v", err)
	}
	folderRaw, err := pcAPI.GetDefaultFolder(ctx)
	if err != nil {
		t.Fatalf("read local PC default folder configuration: %v", err)
	}
	folderConfig, err := configureSyncthingFolder(folderRaw, workspace.SyncNodeFolder{
		WorkspaceID: workspaceID, FolderID: folderID, DeviceIDs: []string{pcID, serverID},
	})
	if err != nil {
		t.Fatalf("configure local PC folder: %v", err)
	}
	var folderFields map[string]json.RawMessage
	if err := json.Unmarshal(folderConfig, &folderFields); err != nil {
		t.Fatal("decode local PC folder configuration")
	}
	folderPath, _ := json.Marshal("/sync")
	folderFields["path"] = folderPath
	folderConfig, err = json.Marshal(folderFields)
	if err != nil {
		t.Fatal("encode local PC folder configuration")
	}
	if err := pcAPI.PutFolder(ctx, folderID, folderConfig); err != nil {
		t.Fatalf("share Workspace folder with server node: %v", err)
	}
	if err := waitForSyncthingConnection(ctx, pcAPI, serverID, true); err != nil {
		t.Fatalf("wait for server node to connect through the private relay: %v", err)
	}
	if err := waitForSyncthingConnection(ctx, nodeAPI, pcID, true); err != nil {
		t.Fatalf("wait for PC node to connect through the private relay: %v", err)
	}

	serverFiles := filepath.Join(runRoot, "workspaces", hex.EncodeToString(ownerHash[:]), workspaceID)
	pcToServer := []byte("local PC to Morde server via private relay\n")
	if err := os.WriteFile(filepath.Join(pcFiles, "pc-to-server.txt"), pcToServer, 0600); err != nil {
		t.Fatal("write local PC test file")
	}
	if err := waitForFileContent(ctx, filepath.Join(serverFiles, "pc-to-server.txt"), pcToServer); err != nil {
		t.Fatalf("verify PC-to-server file transfer: %v", err)
	}
	serverToPC := []byte("Morde server to local PC via private relay\n")
	if err := os.WriteFile(filepath.Join(serverFiles, "server-to-pc.txt"), serverToPC, 0600); err != nil {
		t.Fatal("write server test file")
	}
	if err := waitForFileContent(ctx, filepath.Join(pcFiles, "server-to-pc.txt"), serverToPC); err != nil {
		t.Fatalf("verify server-to-PC file transfer: %v", err)
	}

	store.state = workspace.SyncNodeDesiredState{Revision: 2}
	if _, err := provisioner.EnsureWorkspaceSyncNode(ctx, ownerID); err != nil {
		t.Fatalf("revoke local PC device from server node: %v", err)
	}
	if err := waitForSyncthingConnection(ctx, pcAPI, serverID, false); err != nil {
		t.Fatalf("wait for revoked PC connection to stop: %v", err)
	}
	actualFolders, err := nodeAPI.GetFolders(ctx)
	if err != nil || len(actualFolders) != 0 {
		t.Fatalf("server folders after revocation = %d, error = %v", len(actualFolders), err)
	}
	actualDevices, err := nodeAPI.GetDevices(ctx)
	if err != nil {
		t.Fatalf("read server devices after revocation: %v", err)
	}
	if len(actualDevices) != 1 {
		t.Fatalf("server devices after revocation = %d, want only its own device", len(actualDevices))
	}
	var remainingDevice syncthingDeviceConfig
	if err := json.Unmarshal(actualDevices[0], &remainingDevice); err != nil || remainingDevice.DeviceID != serverID {
		t.Fatalf("server retained unexpected device after revocation: %+v, error = %v", remainingDevice, err)
	}
}

func workspaceSyncDockerTestEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if strings.TrimSpace(value) == "" {
		t.Fatalf("%s is required when the local Docker integration is enabled", name)
	}
	return value
}

func workspaceSyncDockerCommand(ctx context.Context, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker %s failed: %w", args[0], err)
	}
	return output, nil
}

func waitForSyncthingAPI(ctx context.Context, client *SyncthingAPIClient) error {
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

func waitForSyncthingConnection(ctx context.Context, client *SyncthingAPIClient, deviceID string, connected bool) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		raw, err := client.Connections(ctx)
		if err == nil {
			var state syncthingConnections
			if json.Unmarshal(raw, &state) == nil && state.Connections != nil {
				connection := state.Connections[deviceID]
				if connection.Connected == connected {
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

func waitForFileContent(ctx context.Context, path string, expected []byte) error {
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
