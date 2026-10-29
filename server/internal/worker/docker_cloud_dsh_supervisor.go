package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/dockerapi"
)

const (
	dockerDSHUser       = "65532:65532"
	dockerDSHMemory     = int64(512 << 20)
	dockerDSHNanoCPUs   = int64(1_000_000_000)
	dockerDSHPIDs       = int64(64)
	dockerDSHFenceLabel = "morde.dsh.fence"
	dockerDSHNonceLabel = "morde.dsh.nonce-sha256"
)

var dockerDSHImageDigest = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`)
var dockerDSHContainerID = regexp.MustCompile(`^[a-f0-9]{64}$`)
var errDockerObjectNotFound = errors.New("docker object not found")

// DockerCloudDSHGuard identifies exactly one container and its independently
// enforced deadline. The guardian does not receive the supervisor signing key.
type DockerCloudDSHGuard struct {
	ContainerID   string
	ContainerName string
	Fence         app.AgentInstanceFence
	NonceSHA256   string
	ExpiresAt     time.Time
}

// DockerCloudDSHGuardian must run independently of Portal. Arm must durably
// enforce deletion on expiry even if Portal disappears; Started must bind the
// actual container PID namespace/cgroup to that guard. ConfirmDestroyed must
// independently verify all processes and the PID namespace have disappeared.
// This package does not supply a production implementation of the guardian.
type DockerCloudDSHGuardian interface {
	Arm(context.Context, DockerCloudDSHGuard) error
	Started(context.Context, DockerCloudDSHGuard, int) error
	Renew(context.Context, DockerCloudDSHGuard) error
	ConfirmDestroyed(context.Context, DockerCloudDSHGuard) (bool, error)
}

type DockerCloudDSHSupervisorOptions struct {
	DockerHost   string
	Image        string
	NodePath     string
	ScriptPath   string
	SupervisorID string
	SigningKey   []byte
	Guardian     DockerCloudDSHGuardian
}

type DockerCloudDSHSupervisor struct {
	options DockerCloudDSHSupervisorOptions
	socket  string
	client  *http.Client
	api     *dockerapi.EndpointResolver
	mu      sync.Mutex
	active  map[app.AgentInstanceFence]*dockerDSHState
}

type dockerDSHState struct {
	mu             sync.Mutex
	guard          DockerCloudDSHGuard
	startRequested bool
	removed        bool
}

// NewDockerCloudDSHSupervisor is intentionally unavailable without a separately
// deployed guardian capable of independently enforcing expiry and process death.
func NewDockerCloudDSHSupervisor(options DockerCloudDSHSupervisorOptions) (*DockerCloudDSHSupervisor, error) {
	endpoint, err := url.Parse(options.DockerHost)
	if err != nil || endpoint.Scheme != "unix" || endpoint.Host != "" || endpoint.User != nil || endpoint.Opaque != "" || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" ||
		!filepath.IsAbs(endpoint.Path) || filepath.Clean(endpoint.Path) != endpoint.Path || endpoint.Path == "/" ||
		!dockerDSHImageDigest.MatchString(options.Image) || !cleanDockerDSHPath(options.NodePath) || !cleanDockerDSHPath(options.ScriptPath) ||
		!cloudDSHID.MatchString(options.SupervisorID) || len(options.SigningKey) < 32 || bytes.Equal(options.SigningKey, make([]byte, len(options.SigningKey))) || options.Guardian == nil {
		return nil, errors.New("docker dsh supervisor requires a Unix socket, pinned image, fixed command, signing key and independent guardian")
	}
	options.SigningKey = append([]byte(nil), options.SigningKey...)
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", endpoint.Path)
	}, DisableCompression: true}
	client := &http.Client{Transport: transport}
	return &DockerCloudDSHSupervisor{options: options, socket: endpoint.Path, client: client,
		api: dockerapi.NewEndpointResolver(client), active: make(map[app.AgentInstanceFence]*dockerDSHState)}, nil
}

func cleanDockerDSHPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && !strings.ContainsAny(path, "\x00\r\n")
}

func (s *DockerCloudDSHSupervisor) Start(ctx context.Context, launch CloudDSHLaunch) (io.WriteCloser, io.ReadCloser, error) {
	if ctx == nil || launch.Fence.Validate() != nil || launch.Fence.SupervisorID != s.options.SupervisorID ||
		launch.NodePath != s.options.NodePath || launch.ScriptPath != s.options.ScriptPath ||
		len(launch.Nonce) != 32 || bytes.Equal(launch.Nonce, make([]byte, 32)) || !validCloudDSHLease(launch.ExpiresAt) ||
		!slices.Equal(launch.Environment, []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "NODE_ENV=production"}) {
		return nil, nil, errors.New("invalid docker dsh launch")
	}
	guard, labels, err := dockerDSHIdentity(launch.Fence, launch.Nonce)
	if err != nil {
		return nil, nil, err
	}
	guard.ExpiresAt = launch.ExpiresAt
	state := &dockerDSHState{guard: guard}
	state.mu.Lock()
	defer state.mu.Unlock()
	s.mu.Lock()
	if _, exists := s.active[launch.Fence]; exists {
		s.mu.Unlock()
		return nil, nil, errors.New("dsh fence already has a supervised container")
	}
	s.active[launch.Fence] = state
	s.mu.Unlock()
	if err := s.validateImage(ctx); err != nil {
		return nil, nil, err
	}
	create := s.containerCreate(labels)
	var created struct {
		ID string `json:"Id"`
	}
	if err := s.requestJSON(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(guard.ContainerName), create, http.StatusCreated, &created); err != nil {
		return nil, nil, err
	}
	if !dockerDSHContainerID.MatchString(created.ID) {
		return nil, nil, errors.New("docker returned an invalid container identity")
	}
	state.guard.ContainerID = created.ID
	inspected, err := s.inspect(ctx, created.ID)
	if err != nil || s.validateContainer(inspected, state.guard, false) != nil || *inspected.State.Running || *inspected.State.PID != 0 {
		return nil, nil, errors.New("created docker dsh container did not match its isolation contract")
	}
	if err := s.options.Guardian.Arm(ctx, state.guard); err != nil {
		return nil, nil, errors.New("independent dsh expiry guard is unavailable")
	}
	input, output, err := s.attach(ctx, created.ID)
	if err != nil {
		return nil, nil, err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = input.Close()
			_ = output.Close()
		}
	}()
	if !state.guard.ExpiresAt.After(time.Now()) {
		return nil, nil, errors.New("dsh lease expired before container start")
	}
	state.startRequested = true
	if err := s.requestJSON(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil, http.StatusNoContent, nil); err != nil {
		return nil, nil, err
	}
	inspected, err = s.inspect(ctx, created.ID)
	if err != nil || s.validateContainer(inspected, state.guard, true) != nil || !*inspected.State.Running || *inspected.State.PID < 1 {
		return nil, nil, errors.New("started docker dsh container did not match its isolation contract")
	}
	if err := s.options.Guardian.Started(ctx, state.guard, *inspected.State.PID); err != nil {
		return nil, nil, errors.New("independent dsh process-tree observation failed")
	}
	if ctx.Err() != nil || !state.guard.ExpiresAt.After(time.Now()) {
		return nil, nil, errors.New("dsh lease expired during startup")
	}
	succeeded = true
	return input, output, nil
}

func (s *DockerCloudDSHSupervisor) Renew(ctx context.Context, fence app.AgentInstanceFence, expires time.Time) error {
	if ctx == nil || fence.Validate() != nil || fence.SupervisorID != s.options.SupervisorID || !validCloudDSHLease(expires) {
		return errors.New("invalid docker dsh renewal")
	}
	s.mu.Lock()
	state := s.active[fence]
	s.mu.Unlock()
	if state == nil {
		return errors.New("docker dsh guard is not known")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.removed || state.guard.ContainerID == "" || !state.guard.ExpiresAt.After(time.Now()) || expires.Before(state.guard.ExpiresAt) {
		return errors.New("docker dsh lease cannot be renewed")
	}
	updated := state.guard
	updated.ExpiresAt = expires
	if err := s.options.Guardian.Renew(ctx, updated); err != nil {
		return errors.New("independent dsh expiry renewal failed")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	state.guard = updated
	return nil
}

func (s *DockerCloudDSHSupervisor) Stop(ctx context.Context, fence app.AgentInstanceFence, nonce []byte) (app.AgentStopReceipt, error) {
	if ctx == nil || fence.Validate() != nil || fence.SupervisorID != s.options.SupervisorID || len(nonce) != 32 || bytes.Equal(nonce, make([]byte, 32)) {
		return app.AgentStopReceipt{}, errors.New("invalid docker dsh stop identity")
	}
	guard, _, err := dockerDSHIdentity(fence, nonce)
	if err != nil {
		return app.AgentStopReceipt{}, err
	}
	s.mu.Lock()
	state := s.active[fence]
	s.mu.Unlock()
	if state == nil {
		state = &dockerDSHState{guard: guard, startRequested: true}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.guard.ContainerName != guard.ContainerName || subtle.ConstantTimeCompare([]byte(state.guard.NonceSHA256), []byte(guard.NonceSHA256)) != 1 {
		return app.AgentStopReceipt{}, errors.New("docker dsh stop identity does not match")
	}
	if !state.startRequested {
		if !state.removed {
			var inspected dockerDSHInspect
			var err error
			if state.guard.ContainerID == "" {
				inspected, err = s.inspect(ctx, guard.ContainerName)
				if errors.Is(err, errDockerObjectNotFound) {
					state.removed = true
				} else if err != nil || !dockerDSHContainerID.MatchString(inspected.ID) {
					return app.AgentStopReceipt{}, errors.New("docker dsh container identity is unconfirmed")
				} else {
					state.guard.ContainerID = inspected.ID
				}
			} else {
				inspected, err = s.inspect(ctx, state.guard.ContainerID)
			}
			if !state.removed {
				if err != nil || s.validateContainer(inspected, state.guard, false) != nil || *inspected.State.Running || *inspected.State.PID != 0 {
					return app.AgentStopReceipt{}, errors.New("dsh container was started without a recorded start request")
				}
				if err := s.requestJSON(ctx, http.MethodDelete, "/containers/"+state.guard.ContainerID+"?force=0&v=0", nil, http.StatusNoContent, nil); err != nil {
					return app.AgentStopReceipt{}, err
				}
				state.removed = true
			}
		}
		absentID := state.guard.ContainerID
		if absentID == "" {
			absentID = state.guard.ContainerName
		}
		if err := s.requireAbsent(ctx, absentID); err != nil {
			return app.AgentStopReceipt{}, err
		}
		return s.signStopReceipt(fence, nonce)
	}
	if state.guard.ContainerID == "" {
		inspected, err := s.inspect(ctx, guard.ContainerName)
		if err != nil || !dockerDSHContainerID.MatchString(inspected.ID) {
			return app.AgentStopReceipt{}, errors.New("docker dsh container identity is unconfirmed")
		}
		state.guard.ContainerID = inspected.ID
	}
	if !state.removed {
		inspected, err := s.inspect(ctx, state.guard.ContainerID)
		if err != nil || s.validateContainer(inspected, state.guard, false) != nil {
			return app.AgentStopReceipt{}, errors.New("docker dsh container identity or isolation changed")
		}
		if *inspected.State.Running {
			if err := s.requestJSON(ctx, http.MethodPost, "/containers/"+state.guard.ContainerID+"/kill?signal=SIGKILL", nil, http.StatusNoContent, nil); err != nil {
				return app.AgentStopReceipt{}, err
			}
		}
		var waited struct {
			StatusCode *int `json:"StatusCode"`
			Error      *struct {
				Message string `json:"Message"`
			} `json:"Error"`
		}
		if err := s.requestJSON(ctx, http.MethodPost, "/containers/"+state.guard.ContainerID+"/wait?condition=not-running", nil, http.StatusOK, &waited); err != nil {
			return app.AgentStopReceipt{}, err
		}
		if waited.StatusCode == nil || waited.Error != nil && waited.Error.Message != "" {
			return app.AgentStopReceipt{}, errors.New("docker reported a container wait failure")
		}
		inspected, err = s.inspect(ctx, state.guard.ContainerID)
		if err != nil || s.validateContainer(inspected, state.guard, false) != nil || *inspected.State.Running || *inspected.State.PID != 0 {
			return app.AgentStopReceipt{}, errors.New("docker dsh container stop is unconfirmed")
		}
		if err := s.requestJSON(ctx, http.MethodDelete, "/containers/"+state.guard.ContainerID+"?force=0&v=0", nil, http.StatusNoContent, nil); err != nil {
			return app.AgentStopReceipt{}, err
		}
		state.removed = true
	}
	if err := s.requireAbsent(ctx, state.guard.ContainerID); err != nil {
		return app.AgentStopReceipt{}, err
	}
	destroyed, err := s.options.Guardian.ConfirmDestroyed(ctx, state.guard)
	if err != nil || !destroyed {
		return app.AgentStopReceipt{}, errors.New("dsh process tree or PID namespace destruction is unconfirmed")
	}
	if ctx.Err() != nil {
		return app.AgentStopReceipt{}, ctx.Err()
	}
	return s.signStopReceipt(fence, nonce)
}

func (s *DockerCloudDSHSupervisor) signStopReceipt(fence app.AgentInstanceFence, nonce []byte) (app.AgentStopReceipt, error) {
	receipt := app.AgentStopReceipt{Fence: fence, SupervisorNonce: append([]byte(nil), nonce...), TreeStopped: true}
	payload, err := app.AgentStopReceiptSigningPayload(receipt)
	if err != nil {
		return app.AgentStopReceipt{}, err
	}
	mac := hmac.New(sha256.New, s.options.SigningKey)
	_, _ = mac.Write(payload)
	receipt.Signature = mac.Sum(nil)
	return receipt, nil
}

func (s *DockerCloudDSHSupervisor) AgentStopConfirmed(fence app.AgentInstanceFence, nonce []byte) {
	if fence.Validate() != nil || fence.SupervisorID != s.options.SupervisorID || len(nonce) != 32 {
		return
	}
	nonceSum := sha256.Sum256(nonce)
	s.mu.Lock()
	state := s.active[fence]
	s.mu.Unlock()
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.removed || state.guard.Fence != fence || subtle.ConstantTimeCompare([]byte(state.guard.NonceSHA256), []byte(hex.EncodeToString(nonceSum[:]))) != 1 {
		return
	}
	s.mu.Lock()
	if s.active[fence] == state {
		delete(s.active, fence)
	}
	s.mu.Unlock()
}

func dockerDSHIdentity(fence app.AgentInstanceFence, nonce []byte) (DockerCloudDSHGuard, map[string]string, error) {
	encoded, err := json.Marshal(fence)
	if err != nil {
		return DockerCloudDSHGuard{}, nil, errors.New("cannot encode dsh fence")
	}
	nonceSum := sha256.Sum256(nonce)
	nonceHash := hex.EncodeToString(nonceSum[:])
	identity := sha256.Sum256(append(append(encoded, 0), nonceSum[:]...))
	name := "morde-dsh-" + hex.EncodeToString(identity[:])
	return DockerCloudDSHGuard{ContainerName: name, Fence: fence, NonceSHA256: nonceHash},
		map[string]string{dockerDSHFenceLabel: string(encoded), dockerDSHNonceLabel: nonceHash}, nil
}

func (s *DockerCloudDSHSupervisor) validateImage(ctx context.Context) error {
	var image struct {
		RepoDigests []string `json:"RepoDigests"`
		Config      *struct {
			Env     []string                   `json:"Env"`
			Volumes map[string]json.RawMessage `json:"Volumes"`
		} `json:"Config"`
	}
	if err := s.requestJSON(ctx, http.MethodGet, "/images/"+url.PathEscape(s.options.Image)+"/json", nil, http.StatusOK, &image); err != nil {
		return err
	}
	if image.Config == nil || !slices.Contains(image.RepoDigests, s.options.Image) || len(image.Config.Volumes) != 0 {
		return errors.New("dsh image digest or volume declaration is invalid")
	}
	for _, value := range image.Config.Env {
		key, _, ok := strings.Cut(value, "=")
		if !ok || (key != "PATH" && key != "LANG" && key != "NODE_ENV") {
			return errors.New("dsh image contains an unapproved environment variable")
		}
	}
	return nil
}

func (s *DockerCloudDSHSupervisor) containerCreate(labels map[string]string) any {
	return s.containerCreateForScript(labels, s.options.ScriptPath)
}

func (s *DockerCloudDSHSupervisor) containerCreateForScript(labels map[string]string, scriptPath string) any {
	return map[string]any{
		"Image":      s.options.Image,
		"User":       dockerDSHUser,
		"Env":        []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "NODE_ENV=production"},
		"Entrypoint": []string{"/usr/bin/env", "-i", "PATH=/usr/bin:/bin", "LANG=C.UTF-8", "NODE_ENV=production", s.options.NodePath},
		"Cmd":        []string{scriptPath},
		"Labels":     labels,
		"OpenStdin":  true, "AttachStdin": true, "AttachStdout": true, "AttachStderr": false,
		"StdinOnce": true, "Tty": false, "NetworkDisabled": true,
		"Healthcheck": map[string]any{"Test": []string{"NONE"}},
		"HostConfig": map[string]any{
			"NetworkMode": "none", "ReadonlyRootfs": true, "Privileged": false,
			"CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges:true"},
			"Memory": dockerDSHMemory, "MemorySwap": dockerDSHMemory, "NanoCpus": dockerDSHNanoCPUs,
			"PidsLimit": dockerDSHPIDs, "IpcMode": "private", "CgroupnsMode": "private",
			"Init": true, "AutoRemove": false, "LogConfig": map[string]any{"Type": "none", "Config": map[string]string{}},
			"RestartPolicy": map[string]any{"Name": "no"},
		},
	}
}

type dockerDSHInspect struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Image           string                     `json:"Image"`
		User            string                     `json:"User"`
		Env             []string                   `json:"Env"`
		Entrypoint      []string                   `json:"Entrypoint"`
		Cmd             []string                   `json:"Cmd"`
		Labels          map[string]string          `json:"Labels"`
		Volumes         map[string]json.RawMessage `json:"Volumes"`
		OpenStdin       bool                       `json:"OpenStdin"`
		AttachStdin     bool                       `json:"AttachStdin"`
		AttachStdout    bool                       `json:"AttachStdout"`
		AttachStderr    bool                       `json:"AttachStderr"`
		StdinOnce       bool                       `json:"StdinOnce"`
		Tty             bool                       `json:"Tty"`
		NetworkDisabled bool                       `json:"NetworkDisabled"`
		Healthcheck     *struct {
			Test []string `json:"Test"`
		} `json:"Healthcheck"`
	} `json:"Config"`
	HostConfig struct {
		NetworkMode    string            `json:"NetworkMode"`
		ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
		Privileged     bool              `json:"Privileged"`
		CapDrop        []string          `json:"CapDrop"`
		CapAdd         []string          `json:"CapAdd"`
		SecurityOpt    []string          `json:"SecurityOpt"`
		Memory         int64             `json:"Memory"`
		MemorySwap     int64             `json:"MemorySwap"`
		NanoCPUs       int64             `json:"NanoCpus"`
		PidsLimit      *int64            `json:"PidsLimit"`
		IpcMode        string            `json:"IpcMode"`
		CgroupnsMode   string            `json:"CgroupnsMode"`
		PidMode        string            `json:"PidMode"`
		Init           *bool             `json:"Init"`
		AutoRemove     bool              `json:"AutoRemove"`
		Binds          []string          `json:"Binds"`
		Mounts         []json.RawMessage `json:"Mounts"`
		Devices        []json.RawMessage `json:"Devices"`
		DeviceRequests []json.RawMessage `json:"DeviceRequests"`
		VolumesFrom    []string          `json:"VolumesFrom"`
		LogConfig      struct {
			Type string `json:"Type"`
		} `json:"LogConfig"`
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
	State *struct {
		Running    *bool `json:"Running"`
		Restarting *bool `json:"Restarting"`
		Paused     *bool `json:"Paused"`
		PID        *int  `json:"Pid"`
	} `json:"State"`
	Mounts []json.RawMessage `json:"Mounts"`
}

func (s *DockerCloudDSHSupervisor) inspect(ctx context.Context, id string) (dockerDSHInspect, error) {
	var inspected dockerDSHInspect
	err := s.requestJSON(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, http.StatusOK, &inspected)
	return inspected, err
}

func (s *DockerCloudDSHSupervisor) validateContainer(value dockerDSHInspect, guard DockerCloudDSHGuard, running bool) error {
	encoded, err := json.Marshal(guard.Fence)
	if err != nil {
		return err
	}
	if !s.validContainerIsolation(value, s.options.ScriptPath, running) ||
		value.ID != guard.ContainerID || value.Name != "/"+guard.ContainerName ||
		value.Config.Labels[dockerDSHFenceLabel] != string(encoded) || value.Config.Labels[dockerDSHNonceLabel] != guard.NonceSHA256 {
		return errors.New("docker dsh isolation contract mismatch")
	}
	return nil
}

func (s *DockerCloudDSHSupervisor) validContainerIsolation(value dockerDSHInspect, scriptPath string, running bool) bool {
	config, host := value.Config, value.HostConfig
	wantEnv := []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "NODE_ENV=production"}
	wantEntrypoint := []string{"/usr/bin/env", "-i", wantEnv[0], wantEnv[1], wantEnv[2], s.options.NodePath}
	if value.State == nil || value.State.Running == nil || value.State.Restarting == nil || value.State.Paused == nil || value.State.PID == nil ||
		config.Image != s.options.Image || config.User != dockerDSHUser ||
		!equalDockerDSHEnv(config.Env, wantEnv) || !slices.Equal(config.Entrypoint, wantEntrypoint) || !slices.Equal(config.Cmd, []string{scriptPath}) ||
		len(config.Volumes) != 0 || !config.OpenStdin || !config.AttachStdin || !config.AttachStdout || config.AttachStderr || !config.StdinOnce || config.Tty || !config.NetworkDisabled ||
		config.Healthcheck == nil || !slices.Equal(config.Healthcheck.Test, []string{"NONE"}) ||
		host.NetworkMode != "none" || !host.ReadonlyRootfs || host.Privileged || !slices.Contains(host.CapDrop, "ALL") || len(host.CapAdd) != 0 ||
		(!slices.Contains(host.SecurityOpt, "no-new-privileges:true") && !slices.Contains(host.SecurityOpt, "no-new-privileges")) ||
		host.Memory != dockerDSHMemory || host.MemorySwap != dockerDSHMemory || host.NanoCPUs != dockerDSHNanoCPUs ||
		host.PidsLimit == nil || *host.PidsLimit != dockerDSHPIDs || host.IpcMode != "private" || host.CgroupnsMode != "private" ||
		(host.PidMode != "" && host.PidMode != "private") || host.Init == nil || !*host.Init || host.AutoRemove ||
		len(host.Binds) != 0 || len(host.Mounts) != 0 || len(host.Devices) != 0 || len(host.DeviceRequests) != 0 || len(host.VolumesFrom) != 0 || len(value.Mounts) != 0 ||
		host.LogConfig.Type != "none" || host.RestartPolicy.Name != "no" || *value.State.Restarting || *value.State.Paused || (running && !*value.State.Running) {
		return false
	}
	return true
}

func equalDockerDSHEnv(got, expected []string) bool {
	if len(got) != len(expected) {
		return false
	}
	for _, item := range expected {
		count := 0
		for _, candidate := range got {
			if candidate == item {
				count++
			}
		}
		if count != 1 {
			return false
		}
	}
	return true
}

func (s *DockerCloudDSHSupervisor) requestJSON(ctx context.Context, method, path string, body any, status int, result any) error {
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
		return errors.New("docker request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != status {
		if response.StatusCode == http.StatusNotFound {
			return fmt.Errorf("docker request returned status %d: %w", response.StatusCode, errDockerObjectNotFound)
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

func (s *DockerCloudDSHSupervisor) requireAbsent(ctx context.Context, id string) error {
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	endpoint, err := s.api.Endpoint(requestCtx, "/containers/"+url.PathEscape(id)+"/json")
	if err != nil {
		return errors.New("docker Engine API version is unsupported")
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("invalid docker inspection")
	}
	response, err := s.client.Do(request)
	if err != nil {
		return errors.New("docker container removal is unconfirmed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNotFound {
		return errors.New("docker container still exists or removal is unconfirmed")
	}
	return nil
}

func (s *DockerCloudDSHSupervisor) attach(ctx context.Context, id string) (io.WriteCloser, io.ReadCloser, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", s.socket)
	if err != nil {
		return nil, nil, errors.New("docker attach connection failed")
	}
	context.AfterFunc(ctx, func() { _ = conn.Close() })
	deadline := time.Now().Add(5 * time.Second)
	if parentDeadline, ok := ctx.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, nil, errors.New("docker attach deadline failed")
	}
	endpoint, err := s.api.Endpoint(ctx, "/containers/"+id+"/attach?stream=1&stdin=1&stdout=1&stderr=0&logs=0")
	if err != nil {
		_ = conn.Close()
		return nil, nil, errors.New("docker Engine API version is unsupported")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		_ = conn.Close()
		return nil, nil, errors.New("docker attach request is invalid")
	}
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "tcp")
	if err := request.Write(conn); err != nil {
		_ = conn.Close()
		return nil, nil, errors.New("docker attach request failed")
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, request)
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		return nil, nil, errors.New("docker attach handshake failed")
	}
	_ = response.Body.Close()
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, nil, errors.New("docker attach deadline reset failed")
	}
	return dockerDSHInput{conn: conn}, &dockerDSHOutput{conn: conn, reader: reader}, nil
}

type dockerDSHInput struct{ conn net.Conn }

func (p dockerDSHInput) Write(data []byte) (int, error) { return p.conn.Write(data) }
func (p dockerDSHInput) Close() error {
	if half, ok := p.conn.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return p.conn.Close()
}

type dockerDSHOutput struct {
	conn      net.Conn
	reader    *bufio.Reader
	remaining uint32
}

func (p *dockerDSHOutput) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	for p.remaining == 0 {
		var header [8]byte
		if _, err := io.ReadFull(p.reader, header[:]); err != nil {
			return 0, err
		}
		if header[0] != 1 || header[1] != 0 || header[2] != 0 || header[3] != 0 {
			return 0, errors.New("invalid docker stdout stream")
		}
		p.remaining = binary.BigEndian.Uint32(header[4:])
		if p.remaining > cloudDSHMaxFrame {
			return 0, errors.New("docker stdout chunk exceeds limit")
		}
	}
	if len(data) > int(p.remaining) {
		data = data[:int(p.remaining)]
	}
	n, err := p.reader.Read(data)
	p.remaining -= uint32(n)
	return n, err
}

func (p *dockerDSHOutput) Close() error { return p.conn.Close() }

var _ CloudDSHSupervisor = (*DockerCloudDSHSupervisor)(nil)
