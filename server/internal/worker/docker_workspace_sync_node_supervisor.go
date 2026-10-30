package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"morde/server/internal/dockerapi"
)

const (
	dockerWorkspaceSyncConfigPath     = "/var/syncthing"
	dockerWorkspaceSyncFilesPath      = "/workspaces"
	dockerWorkspaceSyncAPIPort        = "8384/tcp"
	dockerWorkspaceSyncContainerAlias = "morde-syncthing"
	dockerWorkspaceSyncLabelManaged   = "morde.workspace-sync.managed"
	dockerWorkspaceSyncLabelOwner     = "morde.workspace-sync.owner-sha256"
	dockerWorkspaceSyncLabelImage     = "morde.workspace-sync.image-digest"
)

var (
	dockerWorkspaceSyncImageDigest = regexp.MustCompile(`^(?:[a-zA-Z0-9][a-zA-Z0-9._:/-]*@)?sha256:[a-f0-9]{64}$`)
	dockerWorkspaceSyncContainerID = regexp.MustCompile(`^[a-f0-9]{64}$`)
	dockerWorkspaceSyncRelayName   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	workspaceSyncRelayTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{32,128}$`)
)

// DockerWorkspaceSyncNodeResourceLimits are fixed for the lifetime of a
// supervisor. They are not selected per owner or changed when Ensure runs.
type DockerWorkspaceSyncNodeResourceLimits struct {
	MemoryBytes int64
	NanoCPUs    int64
	PIDsLimit   int64
}

// DockerWorkspaceSyncNodeSupervisorOptions contains the pinned image and the
// dedicated host paths used for persistent Syncthing state and current files.
type DockerWorkspaceSyncNodeSupervisorOptions struct {
	ImageDigest        string
	RelayImageDigest   string
	RelayPublicAddress string
	RelayBindAddress   string
	RelayToken         string
	DockerSocket       string
	RelayStateRoot     string
	StateRoot          string
	WorkspaceRoot      string
	RelayContainerName string
	Resources          DockerWorkspaceSyncNodeResourceLimits
}

// DockerWorkspaceSyncNode contains the container identity and its account-private
// network GUI/API endpoint. The owner API key remains required for REST access.
type DockerWorkspaceSyncNode struct {
	ContainerID string
	APIURL      string
}

// DockerWorkspaceSyncNodeSupervisor creates or reuses one isolated persistent
// Syncthing container for each owner ID.
type DockerWorkspaceSyncNodeSupervisor struct {
	options DockerWorkspaceSyncNodeSupervisorOptions
	client  *http.Client
	api     *dockerapi.EndpointResolver
	user    string
	uid     int
	gid     int
	mu      sync.Mutex
}

// NewDockerWorkspaceSyncNodeSupervisor constructs a supervisor for a Unix
// Docker socket. Resource values are fixed in options and bounded to 512 MiB,
// 500 milli-CPU, and 128 PIDs. The pinned image must not declare Docker VOLUME
// paths; its state and current files must use only the two explicit binds.
func NewDockerWorkspaceSyncNodeSupervisor(options DockerWorkspaceSyncNodeSupervisorOptions) (*DockerWorkspaceSyncNodeSupervisor, error) {
	if !dockerWorkspaceSyncImageDigest.MatchString(options.ImageDigest) ||
		!dockerWorkspaceSyncImageDigest.MatchString(options.RelayImageDigest) ||
		!validWorkspaceSyncRelayHost(options.RelayPublicAddress) ||
		!validWorkspaceSyncRelayBindAddress(options.RelayBindAddress) ||
		!workspaceSyncRelayTokenPattern.MatchString(options.RelayToken) ||
		!cleanDockerWorkspaceSyncPath(options.DockerSocket) ||
		!cleanDockerWorkspaceSyncPath(options.RelayStateRoot) ||
		!cleanDockerWorkspaceSyncPath(options.StateRoot) ||
		!cleanDockerWorkspaceSyncPath(options.WorkspaceRoot) ||
		workspaceSyncPathsOverlap(options.RelayStateRoot, options.StateRoot) ||
		workspaceSyncPathsOverlap(options.RelayStateRoot, options.WorkspaceRoot) ||
		workspaceSyncPathsOverlap(options.StateRoot, options.WorkspaceRoot) ||
		!dockerWorkspaceSyncRelayName.MatchString(options.RelayContainerName) ||
		!validDockerWorkspaceSyncResources(options.Resources) {
		return nil, errors.New("invalid docker workspace sync node supervisor options")
	}

	uid, gid := os.Geteuid(), os.Getegid()
	if uid == 0 {
		uid, gid = 65532, 65532
	}
	transport := &http.Transport{
		Proxy:              nil,
		DisableCompression: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", options.DockerSocket)
		},
	}
	client := &http.Client{Transport: transport}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &DockerWorkspaceSyncNodeSupervisor{
		options: options,
		client:  client,
		api:     dockerapi.NewEndpointResolver(client),
		user:    strconv.Itoa(uid) + ":" + strconv.Itoa(gid),
		uid:     uid,
		gid:     gid,
	}, nil
}

func (s *DockerWorkspaceSyncNodeSupervisor) relayConfig() workspaceSyncRelayConfig {
	return workspaceSyncRelayConfig{
		StateRoot: s.options.RelayStateRoot, PublicAddress: s.options.RelayPublicAddress,
		BindAddress: s.options.RelayBindAddress, Token: s.options.RelayToken,
	}
}

// Ensure returns the persistent Syncthing node for ownerID, creating and
// starting it when necessary. apiKey is supplied per call and is used only for
// that owner's container configuration; the supervisor does not retain it.
func (s *DockerWorkspaceSyncNodeSupervisor) Ensure(ctx context.Context, ownerID, apiKey string) (DockerWorkspaceSyncNode, error) {
	if ctx == nil || ownerID == "" || len(ownerID) > 256 || strings.TrimSpace(ownerID) != ownerID ||
		strings.ContainsAny(ownerID, "\x00\r\n") || !validDockerWorkspaceSyncAPIKey(apiKey) {
		return DockerWorkspaceSyncNode{}, errors.New("invalid workspace sync node identity")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	ownerHash := sha256.Sum256([]byte(ownerID))
	ownerHashHex := hex.EncodeToString(ownerHash[:])
	containerName := "morde-sync-" + ownerHashHex[:32]
	networkName := "morde-sync-net-" + ownerHashHex[:32]
	if s.options.RelayContainerName == containerName {
		return DockerWorkspaceSyncNode{}, errors.New("workspace sync relay name conflicts with account node")
	}
	statePath, workspacePath, err := s.prepareOwnerPaths(ownerHashHex)
	if err != nil {
		return DockerWorkspaceSyncNode{}, err
	}

	imageEnv, imageLabels, err := s.validateImage(ctx)
	if err != nil {
		return DockerWorkspaceSyncNode{}, err
	}
	relay, err := s.inspectRelay(ctx)
	if err != nil {
		return DockerWorkspaceSyncNode{}, err
	}
	relayID := relay.ID
	node, nodeExists, err := s.inspectContainerByName(ctx, containerName)
	if err != nil && !errors.Is(err, errDockerObjectNotFound) {
		return DockerWorkspaceSyncNode{}, err
	}
	if !nodeExists {
		node = dockerWorkspaceSyncInspect{}
	}

	network, err := s.ensureNetwork(ctx, ownerHashHex, networkName)
	if err != nil {
		return DockerWorkspaceSyncNode{}, err
	}
	bridgeName := dockerWorkspaceSyncBridgeName(ownerHashHex)
	if !validDockerWorkspaceSyncNetwork(network, networkName, ownerHashHex, bridgeName) {
		return DockerWorkspaceSyncNode{}, errors.New("workspace sync network isolation contract mismatch")
	}
	if err := validWorkspaceSyncNetworkMembers(network, node.ID, containerName, nodeExists); err != nil {
		return DockerWorkspaceSyncNode{}, err
	}

	if !nodeExists {
		created, createErr := s.createNode(ctx, ownerHashHex, containerName, networkName, statePath, workspacePath, apiKey)
		if createErr != nil {
			// A concurrent supervisor may have created the deterministic name. Reuse
			// it only after inspecting the complete isolation contract below.
			candidate, exists, inspectErr := s.inspectContainerByName(ctx, containerName)
			if inspectErr != nil || !exists {
				return DockerWorkspaceSyncNode{}, errors.New("workspace sync node creation failed")
			}
			node = candidate
		} else {
			node = created
		}
		nodeExists = true
	}

	if err := validDockerWorkspaceSyncContainer(node, dockerWorkspaceSyncExpected{
		ID: node.ID, Name: containerName, OwnerHash: ownerHashHex, Image: s.options.ImageDigest, ImageLabels: imageLabels,
		ImageEnv: imageEnv, APIKey: apiKey, User: s.user, NetworkName: networkName,
		StatePath: statePath, WorkspacePath: workspacePath, Resources: s.options.Resources,
	}); err != nil {
		return DockerWorkspaceSyncNode{}, err
	}
	if node.State == nil || node.State.Running == nil || node.State.Restarting == nil || node.State.Paused == nil ||
		*node.State.Restarting || *node.State.Paused {
		return DockerWorkspaceSyncNode{}, errors.New("workspace sync node state is incompatible")
	}
	if !*node.State.Running {
		if err := s.requestJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(node.ID)+"/start", nil, http.StatusNoContent, nil); err != nil {
			return DockerWorkspaceSyncNode{}, errors.New("workspace sync node start failed")
		}
		node, err = s.inspectContainer(ctx, node.ID)
		if err != nil || validDockerWorkspaceSyncContainer(node, dockerWorkspaceSyncExpected{
			ID: node.ID, Name: containerName, OwnerHash: ownerHashHex, Image: s.options.ImageDigest, ImageLabels: imageLabels,
			ImageEnv: imageEnv, APIKey: apiKey, User: s.user, NetworkName: networkName,
			StatePath: statePath, WorkspacePath: workspacePath, Resources: s.options.Resources,
		}) != nil || node.State == nil || node.State.Running == nil || !*node.State.Running {
			return DockerWorkspaceSyncNode{}, errors.New("started workspace sync node failed isolation inspection")
		}
	}

	currentRelay, relayErr := s.inspectRelay(ctx)
	if relayErr != nil || currentRelay.ID != relayID {
		return DockerWorkspaceSyncNode{}, errors.New("dedicated workspace sync relay changed during startup")
	}
	network, err = s.inspectNetwork(ctx, networkName)
	if err != nil || !validDockerWorkspaceSyncNetwork(network, networkName, ownerHashHex, bridgeName) ||
		validWorkspaceSyncNetworkMembers(network, node.ID, containerName, true) != nil {
		return DockerWorkspaceSyncNode{}, errors.New("workspace sync network membership mismatch")
	}
	address, ok := dockerWorkspaceSyncPrivateIPv4(node, networkName)
	if !ok {
		return DockerWorkspaceSyncNode{}, errors.New("workspace sync node private API address is unavailable")
	}
	return DockerWorkspaceSyncNode{ContainerID: node.ID, APIURL: "http://" + net.JoinHostPort(address, "8384")}, nil
}

func validDockerWorkspaceSyncResources(value DockerWorkspaceSyncNodeResourceLimits) bool {
	return value.MemoryBytes >= 128<<20 && value.MemoryBytes <= 512<<20 &&
		value.NanoCPUs >= 100_000_000 && value.NanoCPUs <= 500_000_000 &&
		value.PIDsLimit >= 32 && value.PIDsLimit <= 128
}

func validDockerWorkspaceSyncAPIKey(value string) bool {
	if len(value) < 32 || len(value) > 256 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char > 0x7e {
			return false
		}
	}
	return true
}

func cleanDockerWorkspaceSyncPath(value string) bool {
	return filepath.IsAbs(value) && filepath.Clean(value) == value && value != "/" && !strings.ContainsAny(value, "\x00\r\n")
}

func workspaceSyncPathsOverlap(first, second string) bool {
	within := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
	}
	return within(first, second) || within(second, first)
}

func (s *DockerWorkspaceSyncNodeSupervisor) prepareOwnerPaths(ownerHash string) (string, string, error) {
	roots := []string{s.options.StateRoot, s.options.WorkspaceRoot}
	canonical := make([]string, 0, len(roots))
	for _, path := range roots {
		if err := os.MkdirAll(path, 0750); err != nil {
			return "", "", errors.New("workspace sync root is unavailable")
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", "", errors.New("workspace sync root is incompatible")
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || !filepath.IsAbs(resolved) {
			return "", "", errors.New("workspace sync root is incompatible")
		}
		canonical = append(canonical, resolved)
	}
	if workspaceSyncPathsOverlap(canonical[0], canonical[1]) {
		return "", "", errors.New("workspace sync roots overlap")
	}
	statePath := filepath.Join(canonical[0], ownerHash)
	workspacePath := filepath.Join(canonical[1], ownerHash)
	for _, path := range []string{statePath, workspacePath} {
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", "", errors.New("workspace sync owner directory is unavailable")
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", "", errors.New("workspace sync owner directory is incompatible")
		}
		if os.Geteuid() == 0 {
			if err := os.Chown(path, s.uid, s.gid); err != nil {
				return "", "", errors.New("workspace sync owner directory ownership is unavailable")
			}
		}
		if err := os.Chmod(path, 0700); err != nil {
			return "", "", errors.New("workspace sync owner directory permissions are unavailable")
		}
	}
	return statePath, workspacePath, nil
}

type dockerWorkspaceSyncImage struct {
	ID          string   `json:"Id"`
	RepoDigests []string `json:"RepoDigests"`
	Config      *struct {
		Env          []string                   `json:"Env"`
		Volumes      map[string]json.RawMessage `json:"Volumes"`
		ExposedPorts map[string]json.RawMessage `json:"ExposedPorts"`
		Labels       map[string]string          `json:"Labels"`
	} `json:"Config"`
}

func (s *DockerWorkspaceSyncNodeSupervisor) validateImage(ctx context.Context) ([]string, map[string]string, error) {
	var image dockerWorkspaceSyncImage
	if err := s.requestJSON(ctx, http.MethodGet, "/images/"+url.PathEscape(s.options.ImageDigest)+"/json", nil, http.StatusOK, &image); err != nil {
		return nil, nil, errors.New("pinned workspace sync image is unavailable")
	}
	// An image-declared VOLUME can add anonymous storage and violate the exact
	// two-bind contract, so the pinned image must be built without VOLUME paths.
	if image.Config == nil || !workspaceSyncImageMatches(s.options.ImageDigest, image.ID, image.RepoDigests) || len(image.Config.Volumes) != 0 ||
		(len(image.Config.ExposedPorts) != 0 && (len(image.Config.ExposedPorts) != 1 || image.Config.ExposedPorts[dockerWorkspaceSyncAPIPort] == nil)) {
		return nil, nil, errors.New("workspace sync image digest or volume declarations are invalid")
	}
	for _, value := range image.Config.Env {
		key, _, ok := strings.Cut(value, "=")
		if !ok || key == "STGUIAPIKEY" || key == "STGUIADDRESS" || key == "STNORESTART" {
			return nil, nil, errors.New("workspace sync image contains an unapproved environment variable")
		}
	}
	for key := range image.Config.Labels {
		if key == dockerWorkspaceSyncLabelManaged || key == dockerWorkspaceSyncLabelOwner || key == dockerWorkspaceSyncLabelImage {
			return nil, nil, errors.New("workspace sync image contains a reserved label")
		}
	}
	labels := make(map[string]string, len(image.Config.Labels))
	for key, value := range image.Config.Labels {
		labels[key] = value
	}
	return append([]string(nil), image.Config.Env...), labels, nil
}

type dockerWorkspaceSyncNetwork struct {
	ID         string            `json:"Id"`
	Name       string            `json:"Name"`
	Scope      string            `json:"Scope"`
	Driver     string            `json:"Driver"`
	Internal   bool              `json:"Internal"`
	EnableIPv6 bool              `json:"EnableIPv6"`
	Attachable bool              `json:"Attachable"`
	Ingress    bool              `json:"Ingress"`
	ConfigOnly bool              `json:"ConfigOnly"`
	Options    map[string]string `json:"Options"`
	IPAM       struct {
		Driver  string            `json:"Driver"`
		Options map[string]string `json:"Options"`
	} `json:"IPAM"`
	Labels     map[string]string `json:"Labels"`
	Containers map[string]struct {
		Name string `json:"Name"`
	} `json:"Containers"`
}

func validDockerWorkspaceSyncNetwork(value dockerWorkspaceSyncNetwork, name, ownerHash, bridgeName string) bool {
	return dockerWorkspaceSyncContainerID.MatchString(value.ID) && value.Name == name && value.Scope == "local" && value.Driver == "bridge" && !value.Internal &&
		!value.EnableIPv6 && !value.Attachable && !value.Ingress && !value.ConfigOnly && validWorkspaceSyncNetworkOptions(value.Options, bridgeName) &&
		value.IPAM.Driver == "default" && len(value.IPAM.Options) == 0 && value.Labels[dockerWorkspaceSyncLabelManaged] == "true" &&
		value.Labels[dockerWorkspaceSyncLabelOwner] == ownerHash && len(value.Labels) == 2
}

func dockerWorkspaceSyncBridgeName(ownerHash string) string {
	return "msync" + ownerHash[:10]
}

func validWorkspaceSyncNetworkOptions(options map[string]string, bridgeName string) bool {
	return len(options) == 2 && options["com.docker.network.bridge.name"] == bridgeName &&
		options["com.docker.network.bridge.enable_ip_masquerade"] == "true"
}

func (s *DockerWorkspaceSyncNodeSupervisor) ensureNetwork(ctx context.Context, ownerHash, name string) (dockerWorkspaceSyncNetwork, error) {
	network, err := s.inspectNetwork(ctx, name)
	if err == nil {
		return network, nil
	}
	if !errors.Is(err, errDockerObjectNotFound) {
		return dockerWorkspaceSyncNetwork{}, err
	}
	bridgeName := dockerWorkspaceSyncBridgeName(ownerHash)
	body := map[string]any{
		"Name": name, "Driver": "bridge", "Internal": false, "EnableIPv6": false,
		"Attachable": false, "Labels": map[string]string{
			dockerWorkspaceSyncLabelManaged: "true", dockerWorkspaceSyncLabelOwner: ownerHash,
		},
		"Options": map[string]string{
			"com.docker.network.bridge.name":                 bridgeName,
			"com.docker.network.bridge.enable_ip_masquerade": "true",
		},
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := s.requestJSON(ctx, http.MethodPost, "/networks/create", body, http.StatusCreated, &created); err != nil {
		// A competing Ensure may have created it. Accept only if inspection proves
		// the resulting network has this owner's complete expected shape.
		network, inspectErr := s.inspectNetwork(ctx, name)
		if inspectErr != nil {
			return dockerWorkspaceSyncNetwork{}, errors.New("workspace sync network creation failed")
		}
		return network, nil
	}
	if created.ID == "" {
		return dockerWorkspaceSyncNetwork{}, errors.New("docker returned an invalid workspace sync network identity")
	}
	return s.inspectNetwork(ctx, name)
}

func (s *DockerWorkspaceSyncNodeSupervisor) inspectNetwork(ctx context.Context, name string) (dockerWorkspaceSyncNetwork, error) {
	var network dockerWorkspaceSyncNetwork
	err := s.requestJSON(ctx, http.MethodGet, "/networks/"+url.PathEscape(name), nil, http.StatusOK, &network)
	return network, err
}

type dockerWorkspaceSyncContainerState struct {
	Running    *bool `json:"Running"`
	Restarting *bool `json:"Restarting"`
	Paused     *bool `json:"Paused"`
	PID        *int  `json:"Pid"`
}

type dockerWorkspaceSyncPort struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type dockerWorkspaceSyncMount struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

type dockerWorkspaceSyncMountSpec struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Target      string `json:"Target"`
	ReadOnly    bool   `json:"ReadOnly"`
	BindOptions struct {
		Propagation string `json:"Propagation"`
	} `json:"BindOptions"`
}

type dockerWorkspaceSyncInspect struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Image        string                     `json:"Image"`
		User         string                     `json:"User"`
		Env          []string                   `json:"Env"`
		Labels       map[string]string          `json:"Labels"`
		Volumes      map[string]json.RawMessage `json:"Volumes"`
		ExposedPorts map[string]json.RawMessage `json:"ExposedPorts"`
	} `json:"Config"`
	HostConfig struct {
		NetworkMode     string                               `json:"NetworkMode"`
		ReadonlyRootfs  bool                                 `json:"ReadonlyRootfs"`
		Privileged      bool                                 `json:"Privileged"`
		CapDrop         []string                             `json:"CapDrop"`
		CapAdd          []string                             `json:"CapAdd"`
		SecurityOpt     []string                             `json:"SecurityOpt"`
		Memory          int64                                `json:"Memory"`
		MemorySwap      int64                                `json:"MemorySwap"`
		NanoCPUs        int64                                `json:"NanoCpus"`
		PidsLimit       *int64                               `json:"PidsLimit"`
		IpcMode         string                               `json:"IpcMode"`
		CgroupnsMode    string                               `json:"CgroupnsMode"`
		Init            *bool                                `json:"Init"`
		AutoRemove      bool                                 `json:"AutoRemove"`
		Binds           []string                             `json:"Binds"`
		Mounts          []dockerWorkspaceSyncMountSpec       `json:"Mounts"`
		Devices         []json.RawMessage                    `json:"Devices"`
		DeviceRequests  []json.RawMessage                    `json:"DeviceRequests"`
		VolumesFrom     []string                             `json:"VolumesFrom"`
		PortBindings    map[string][]dockerWorkspaceSyncPort `json:"PortBindings"`
		PublishAllPorts bool                                 `json:"PublishAllPorts"`
		LogConfig       struct {
			Type string `json:"Type"`
		} `json:"LogConfig"`
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
	State           *dockerWorkspaceSyncContainerState `json:"State"`
	Mounts          []dockerWorkspaceSyncMount         `json:"Mounts"`
	NetworkSettings struct {
		Ports    map[string][]dockerWorkspaceSyncPort          `json:"Ports"`
		Networks map[string]dockerWorkspaceSyncNetworkEndpoint `json:"Networks"`
	} `json:"NetworkSettings"`
}

type dockerWorkspaceSyncNetworkEndpoint struct {
	Aliases   []string `json:"Aliases"`
	IPAddress string   `json:"IPAddress"`
}

type dockerWorkspaceSyncExpected struct {
	ID            string
	Name          string
	OwnerHash     string
	Image         string
	ImageLabels   map[string]string
	ImageEnv      []string
	APIKey        string
	User          string
	NetworkName   string
	StatePath     string
	WorkspacePath string
	Resources     DockerWorkspaceSyncNodeResourceLimits
}

func validDockerWorkspaceSyncContainer(value dockerWorkspaceSyncInspect, expected dockerWorkspaceSyncExpected) error {
	if !dockerWorkspaceSyncContainerID.MatchString(value.ID) || value.ID != expected.ID || value.Name != "/"+expected.Name ||
		value.Config.Image != expected.Image || value.Config.User != expected.User ||
		!validDockerWorkspaceSyncContainerLabels(value.Config.Labels, expected.ImageLabels, expected.OwnerHash, expected.Image) ||
		!equalDockerWorkspaceSyncEnv(value.Config.Env, expected.ImageEnv, expected.APIKey) || len(value.Config.Volumes) != 0 ||
		len(value.Config.ExposedPorts) != 1 || value.Config.ExposedPorts[dockerWorkspaceSyncAPIPort] == nil ||
		value.HostConfig.NetworkMode != expected.NetworkName || !value.HostConfig.ReadonlyRootfs || value.HostConfig.Privileged ||
		!slices.Equal(value.HostConfig.CapDrop, []string{"ALL"}) || len(value.HostConfig.CapAdd) != 0 ||
		!slices.Equal(value.HostConfig.SecurityOpt, []string{"no-new-privileges:true"}) || value.HostConfig.PublishAllPorts ||
		value.HostConfig.Memory != expected.Resources.MemoryBytes || value.HostConfig.MemorySwap != expected.Resources.MemoryBytes ||
		value.HostConfig.NanoCPUs != expected.Resources.NanoCPUs || value.HostConfig.PidsLimit == nil ||
		*value.HostConfig.PidsLimit != expected.Resources.PIDsLimit || value.HostConfig.IpcMode != "private" ||
		value.HostConfig.CgroupnsMode != "private" || value.HostConfig.Init == nil || !*value.HostConfig.Init ||
		value.HostConfig.AutoRemove || len(value.HostConfig.Binds) != 0 ||
		len(value.HostConfig.Devices) != 0 || len(value.HostConfig.DeviceRequests) != 0 || len(value.HostConfig.VolumesFrom) != 0 ||
		value.HostConfig.LogConfig.Type != "none" || value.HostConfig.RestartPolicy.Name != "unless-stopped" ||
		!validDockerWorkspaceSyncMounts(value, expected.StatePath, expected.WorkspacePath) ||
		!dockerWorkspaceSyncHasNoPublishedPorts(value.HostConfig.PortBindings, value.NetworkSettings.Ports) ||
		len(value.NetworkSettings.Networks) != 1 || !hasWorkspaceSyncNetworkAlias(value.NetworkSettings.Networks, expected.NetworkName, dockerWorkspaceSyncContainerAlias) {
		return errors.New("workspace sync container isolation contract mismatch")
	}
	if value.State == nil || value.State.Running == nil || value.State.Restarting == nil || value.State.Paused == nil || value.State.PID == nil ||
		*value.State.Restarting || *value.State.Paused {
		return errors.New("workspace sync container state is incompatible")
	}
	return nil
}

func validDockerWorkspaceSyncContainerLabels(got, imageLabels map[string]string, ownerHash, image string) bool {
	if len(got) != len(imageLabels)+3 || got[dockerWorkspaceSyncLabelManaged] != "true" ||
		got[dockerWorkspaceSyncLabelOwner] != ownerHash || got[dockerWorkspaceSyncLabelImage] != image {
		return false
	}
	for key, value := range imageLabels {
		if got[key] != value {
			return false
		}
	}
	return true
}
func equalDockerWorkspaceSyncEnv(got, imageEnv []string, apiKey string) bool {
	want := append(append([]string(nil), imageEnv...), "STGUIADDRESS=0.0.0.0:8384", "STGUIAPIKEY="+apiKey, "STNORESTART=1")
	if len(got) != len(want) {
		return false
	}
	for _, value := range want {
		if !slices.Contains(got, value) {
			return false
		}
	}
	return true
}

func validDockerWorkspaceSyncMounts(value dockerWorkspaceSyncInspect, statePath, workspacePath string) bool {
	if len(value.Mounts) != 2 || len(value.HostConfig.Mounts) != 2 {
		return false
	}
	want := map[string]string{dockerWorkspaceSyncConfigPath: statePath, dockerWorkspaceSyncFilesPath: workspacePath}
	requestWant := map[string]string{dockerWorkspaceSyncConfigPath: statePath, dockerWorkspaceSyncFilesPath: workspacePath}
	for _, mount := range value.HostConfig.Mounts {
		path, ok := requestWant[mount.Target]
		if !ok || mount.Type != "bind" || mount.Source != path || mount.ReadOnly || mount.BindOptions.Propagation != "rprivate" {
			return false
		}
		delete(requestWant, mount.Target)
	}
	for _, mount := range value.Mounts {
		path, ok := want[mount.Destination]
		if !ok || mount.Type != "bind" || mount.Source != path || !mount.RW {
			return false
		}
		delete(want, mount.Destination)
	}
	return len(want) == 0 && len(requestWant) == 0
}

func dockerWorkspaceSyncHasNoPublishedPorts(configured, published map[string][]dockerWorkspaceSyncPort) bool {
	if len(configured) != 0 {
		return false
	}
	for _, bindings := range published {
		if len(bindings) != 0 {
			return false
		}
	}
	return true
}

func dockerWorkspaceSyncPrivateIPv4(value dockerWorkspaceSyncInspect, networkName string) (string, bool) {
	endpoint, ok := value.NetworkSettings.Networks[networkName]
	if !ok {
		return "", false
	}
	ip := net.ParseIP(endpoint.IPAddress)
	if ip == nil || ip.To4() == nil || !ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return "", false
	}
	return ip.To4().String(), true
}

func validWorkspaceSyncNetworkMembers(network dockerWorkspaceSyncNetwork, nodeID, nodeName string, nodeExists bool) error {
	if (!nodeExists && len(network.Containers) != 0) || (nodeExists && len(network.Containers) != 1) {
		return errors.New("workspace sync network has unexpected members")
	}
	if nodeExists {
		node, ok := network.Containers[nodeID]
		if !ok || node.Name != nodeName {
			return errors.New("workspace sync node attachment is incompatible")
		}
	}
	return nil
}

type dockerWorkspaceSyncRelayInspect struct {
	ID     string `json:"Id"`
	Image  string `json:"Image"`
	Name   string `json:"Name"`
	Config struct {
		Image        string                     `json:"Image"`
		User         string                     `json:"User"`
		Cmd          []string                   `json:"Cmd"`
		Volumes      map[string]json.RawMessage `json:"Volumes"`
		ExposedPorts map[string]json.RawMessage `json:"ExposedPorts"`
	} `json:"Config"`
	Mounts []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
	HostConfig struct {
		NetworkMode    string                         `json:"NetworkMode"`
		Privileged     bool                           `json:"Privileged"`
		ReadonlyRootfs bool                           `json:"ReadonlyRootfs"`
		CapAdd         []string                       `json:"CapAdd"`
		CapDrop        []string                       `json:"CapDrop"`
		SecurityOpt    []string                       `json:"SecurityOpt"`
		Memory         int64                          `json:"Memory"`
		NanoCPUs       int64                          `json:"NanoCpus"`
		PidsLimit      int64                          `json:"PidsLimit"`
		RestartPolicy  struct{ Name string }          `json:"RestartPolicy"`
		PortBindings   map[string][]dockerPortBinding `json:"PortBindings"`
	} `json:"HostConfig"`
	State *struct {
		Running    *bool `json:"Running"`
		Restarting *bool `json:"Restarting"`
		Paused     *bool `json:"Paused"`
	} `json:"State"`
	NetworkSettings struct {
		Networks map[string]dockerWorkspaceSyncNetworkEndpoint `json:"Networks"`
	} `json:"NetworkSettings"`
}

func hasWorkspaceSyncNetworkAlias(networks map[string]dockerWorkspaceSyncNetworkEndpoint, name, alias string) bool {
	return slices.Contains(networks[name].Aliases, alias)
}

type dockerPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

func (s *DockerWorkspaceSyncNodeSupervisor) inspectRelay(ctx context.Context) (dockerWorkspaceSyncRelayInspect, error) {
	var relay dockerWorkspaceSyncRelayInspect
	if err := s.requestJSON(ctx, http.MethodGet, "/containers/"+url.PathEscape(s.options.RelayContainerName)+"/json", nil, http.StatusOK, &relay); err != nil {
		return dockerWorkspaceSyncRelayInspect{}, errors.New("dedicated workspace sync relay is unavailable")
	}
	var image struct {
		ID          string   `json:"Id"`
		RepoDigests []string `json:"RepoDigests"`
	}
	if err := s.requestJSON(ctx, http.MethodGet, "/images/"+url.PathEscape(s.options.RelayImageDigest)+"/json", nil, http.StatusOK, &image); err != nil ||
		!workspaceSyncImageMatches(s.options.RelayImageDigest, image.ID, image.RepoDigests) ||
		(relay.Config.Image != s.options.RelayImageDigest && relay.Image != s.options.RelayImageDigest) {
		return dockerWorkspaceSyncRelayInspect{}, errors.New("dedicated workspace sync relay image digest is unavailable")
	}
	if !dockerWorkspaceSyncContainerID.MatchString(relay.ID) || relay.Name != "/"+s.options.RelayContainerName ||
		!validWorkspaceSyncRelay(relay, s.options.RelayStateRoot, s.options.RelayToken, s.options.RelayPublicAddress, s.options.RelayBindAddress) || relay.State == nil ||
		relay.State.Running == nil || relay.State.Restarting == nil || relay.State.Paused == nil || !*relay.State.Running ||
		*relay.State.Restarting || *relay.State.Paused {
		return dockerWorkspaceSyncRelayInspect{}, errors.New("dedicated workspace sync relay is not running")
	}
	return relay, nil
}

func validWorkspaceSyncRelay(relay dockerWorkspaceSyncRelayInspect, relayStateRoot, relayToken, relayPublicAddress, relayBindAddress string) bool {
	cmd := relay.Config.Cmd
	if relay.Config.User != "65532:65532" || len(relay.Config.Volumes) != 0 || len(relay.Config.ExposedPorts) != 1 ||
		relay.Config.ExposedPorts["22067/tcp"] == nil || relay.HostConfig.NetworkMode == "host" || relay.HostConfig.Privileged ||
		!relay.HostConfig.ReadonlyRootfs || len(relay.HostConfig.CapAdd) != 0 || !slices.Contains(relay.HostConfig.CapDrop, "ALL") ||
		!slices.Contains(relay.HostConfig.SecurityOpt, "no-new-privileges:true") || relay.HostConfig.Memory != 256<<20 ||
		relay.HostConfig.NanoCPUs != 250_000_000 || relay.HostConfig.PidsLimit != 64 || relay.HostConfig.RestartPolicy.Name != "unless-stopped" ||
		len(relay.HostConfig.PortBindings) != 1 || len(relay.HostConfig.PortBindings["22067/tcp"]) != 1 || len(relay.Mounts) != 1 {
		return false
	}
	mount := relay.Mounts[0]
	if mount.Type != "bind" || mount.Source != relayStateRoot || mount.Destination != "/var/lib/morde-relay" || !mount.RW {
		return false
	}
	binding := relay.HostConfig.PortBindings["22067/tcp"][0]
	if !validWorkspaceSyncRelayHostBinding(binding, relayPublicAddress, relayBindAddress) {
		return false
	}
	return hasWorkspaceSyncRelayArg(cmd, "-listen", ":22067") &&
		hasWorkspaceSyncRelayArg(cmd, "-keys", "/var/lib/morde-relay") &&
		hasWorkspaceSyncRelayArg(cmd, "-token", relayToken) && !hasWorkspaceSyncRelayTokenEqualsFlag(cmd) &&
		hasExactWorkspaceSyncRelayFlag(cmd, "-pools=") && hasExactWorkspaceSyncRelayFlag(cmd, "-status-srv=") &&
		hasWorkspaceSyncRelayArg(cmd, "-ext-address", relayPublicAddress)
}

func validWorkspaceSyncRelayHostBinding(binding dockerPortBinding, publicAddress, bindAddress string) bool {
	_, publicPort, err := net.SplitHostPort(publicAddress)
	if err != nil || !validWorkspaceSyncRelayHost(publicAddress) || !validWorkspaceSyncRelayBindAddress(bindAddress) {
		return false
	}
	port, err := strconv.Atoi(binding.HostPort)
	if err != nil || port < 1 || port > 65535 || binding.HostPort != publicPort {
		return false
	}
	actualBindIP, expectedBindIP := net.ParseIP(binding.HostIP), net.ParseIP(bindAddress)
	if actualBindIP == nil || actualBindIP.To4() == nil || expectedBindIP == nil || expectedBindIP.To4() == nil || !actualBindIP.Equal(expectedBindIP) {
		return false
	}
	// The advertised public IPv4 may be mapped to a private host IPv4 by cloud NAT.
	return validWorkspaceSyncRelayBindAddress(binding.HostIP)
}

func hasExactWorkspaceSyncRelayFlag(args []string, expected string) bool {
	prefix := strings.TrimSuffix(expected, "=")
	count := 0
	for _, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			count++
			if arg != expected {
				return false
			}
		}
	}
	return count == 1
}

func hasWorkspaceSyncRelayArg(args []string, name, value string) bool {
	count := 0
	for i, arg := range args {
		if arg == name {
			count++
			if i+1 >= len(args) || args[i+1] != value {
				return false
			}
		}
	}
	return count == 1
}

func hasWorkspaceSyncRelayTokenEqualsFlag(args []string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-token=") {
			return true
		}
	}
	return false
}

func workspaceSyncImageMatches(configured, imageID string, repoDigests []string) bool {
	if strings.HasPrefix(configured, "sha256:") {
		return imageID == configured
	}
	return slices.Contains(repoDigests, configured)
}

func (s *DockerWorkspaceSyncNodeSupervisor) inspectContainerByName(ctx context.Context, name string) (dockerWorkspaceSyncInspect, bool, error) {
	value, err := s.inspectContainer(ctx, name)
	if errors.Is(err, errDockerObjectNotFound) {
		return dockerWorkspaceSyncInspect{}, false, err
	}
	return value, err == nil, err
}

func (s *DockerWorkspaceSyncNodeSupervisor) inspectContainer(ctx context.Context, id string) (dockerWorkspaceSyncInspect, error) {
	var value dockerWorkspaceSyncInspect
	err := s.requestJSON(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, http.StatusOK, &value)
	return value, err
}

func (s *DockerWorkspaceSyncNodeSupervisor) createNode(ctx context.Context, ownerHash, name, networkName, statePath, workspacePath, apiKey string) (dockerWorkspaceSyncInspect, error) {
	labels := map[string]string{
		dockerWorkspaceSyncLabelManaged: "true",
		dockerWorkspaceSyncLabelOwner:   ownerHash,
		dockerWorkspaceSyncLabelImage:   s.options.ImageDigest,
	}
	body := map[string]any{
		"Image": s.options.ImageDigest,
		"User":  s.user,
		"Env": []string{
			"STGUIADDRESS=0.0.0.0:8384",
			"STGUIAPIKEY=" + apiKey,
			"STNORESTART=1",
		},
		"Labels":       labels,
		"ExposedPorts": map[string]any{dockerWorkspaceSyncAPIPort: map[string]any{}},
		"HostConfig": map[string]any{
			"NetworkMode": networkName, "ReadonlyRootfs": true, "Privileged": false,
			"CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges:true"},
			"Memory": s.options.Resources.MemoryBytes, "MemorySwap": s.options.Resources.MemoryBytes,
			"NanoCpus": s.options.Resources.NanoCPUs, "PidsLimit": s.options.Resources.PIDsLimit,
			"IpcMode": "private", "CgroupnsMode": "private", "Init": true,
			"AutoRemove": false, "LogConfig": map[string]any{"Type": "none", "Config": map[string]string{}},
			"RestartPolicy":   map[string]string{"Name": "unless-stopped"},
			"PublishAllPorts": false,
			"Mounts": []map[string]any{
				{"Type": "bind", "Source": statePath, "Target": dockerWorkspaceSyncConfigPath, "ReadOnly": false,
					"BindOptions": map[string]string{"Propagation": "rprivate"}},
				{"Type": "bind", "Source": workspacePath, "Target": dockerWorkspaceSyncFilesPath, "ReadOnly": false,
					"BindOptions": map[string]string{"Propagation": "rprivate"}},
			},
		},
		"NetworkingConfig": map[string]any{
			"EndpointsConfig": map[string]any{
				networkName: map[string]any{"Aliases": []string{dockerWorkspaceSyncContainerAlias}},
			},
		},
	}
	var result struct {
		ID string `json:"Id"`
	}
	if err := s.requestJSON(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), body, http.StatusCreated, &result); err != nil {
		return dockerWorkspaceSyncInspect{}, errors.New("workspace sync node create request failed")
	}
	if !dockerWorkspaceSyncContainerID.MatchString(result.ID) {
		return dockerWorkspaceSyncInspect{}, errors.New("docker returned an invalid workspace sync node identity")
	}
	return s.inspectContainer(ctx, result.ID)
}

func (s *DockerWorkspaceSyncNodeSupervisor) requestJSON(ctx context.Context, method, path string, body any, status int, result any) error {
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return errors.New("cannot encode docker request")
		}
		reader = bytes.NewReader(encoded)
	}
	endpoint, err := s.api.Endpoint(requestCtx, path)
	if err != nil {
		return errors.New("docker Engine API version is unsupported")
	}
	request, err := http.NewRequestWithContext(requestCtx, method, endpoint, reader)
	if err != nil {
		return errors.New("invalid docker request")
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := s.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("docker request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != status {
		if response.StatusCode == http.StatusNotFound {
			return errDockerObjectNotFound
		}
		return fmt.Errorf("docker request returned status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return errors.New("docker response is invalid")
	}
	if result != nil && json.Unmarshal(data, result) != nil {
		return errors.New("docker response JSON is invalid")
	}
	return nil
}
