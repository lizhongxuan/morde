package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	mordesync "morde-sync"

	"morde/server/internal/app"
	"morde/server/internal/dockerapi"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacefs"
)

var volumeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Docker deletes named volumes through the Docker Engine API over its Unix socket.
type Docker struct {
	client       *http.Client
	api          *dockerapi.EndpointResolver
	dataDir      string
	minFreeBytes int64
}

var _ app.VolumeDeleter = (*Docker)(nil)
var _ app.WorkspaceVolumeProvisioner = (*Docker)(nil)
var _ app.LegacyWorkspaceVolumeSource = (*Docker)(nil)

// NewDocker constructs a Docker volume deleter for a unix:// socket URL, such as
// unix:///var/run/docker.sock. It negotiates between Docker API v1.43 and v1.44.
func NewDocker(dockerHost, dataDir string, minFreeBytes int64) (*Docker, error) {
	if !filepath.IsAbs(dataDir) || minFreeBytes <= 0 {
		return nil, fmt.Errorf("docker data directory and minimum free bytes are required")
	}
	parsed, err := url.Parse(dockerHost)
	if err != nil {
		return nil, fmt.Errorf("parse docker host: %w", err)
	}
	if parsed.Scheme != "unix" || parsed.Host != "" || !filepath.IsAbs(parsed.Path) || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("docker host must be an absolute unix socket URL")
	}
	socketPath := filepath.Clean(parsed.Path)
	if socketPath == "/" {
		return nil, fmt.Errorf("docker host socket path is invalid")
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, "unix", socketPath)
		},
		MaxIdleConns:          2,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	client := &http.Client{Transport: transport, Timeout: 35 * time.Second}
	return &Docker{
		client:       client,
		api:          dockerapi.NewEndpointResolver(client),
		dataDir:      filepath.Clean(dataDir),
		minFreeBytes: minFreeBytes,
	}, nil
}

func (d *Docker) CheckCapacity(ctx context.Context) error {
	return d.checkCapacity(ctx, 0)
}

func (d *Docker) CheckCapacityForWrite(ctx context.Context, incomingBytes int64) error {
	if incomingBytes < 0 {
		return domain.InvalidArgument("capacity", "incoming Docker volume bytes must not be negative")
	}
	return d.checkCapacity(ctx, incomingBytes)
}

func (d *Docker) checkCapacity(ctx context.Context, incomingBytes int64) error {
	endpoint, err := d.api.Endpoint(ctx, "/info")
	if err != nil {
		return fmt.Errorf("read Docker Engine API version: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create docker info request: %w", err)
	}
	response, err := d.client.Do(request)
	if err != nil {
		return fmt.Errorf("read docker data root: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return fmt.Errorf("read docker data root: engine returned HTTP %d", response.StatusCode)
	}
	var info struct {
		DockerRootDir string `json:"DockerRootDir"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&info); err != nil {
		return fmt.Errorf("read docker data root: invalid engine response")
	}
	actual, err := filepath.EvalSymlinks(info.DockerRootDir)
	if err != nil {
		return fmt.Errorf("resolve docker data root: %w", err)
	}
	expected, err := filepath.EvalSymlinks(d.dataDir)
	if err != nil || actual != expected {
		return fmt.Errorf("configured docker data directory does not match the running engine")
	}
	var stats syscall.Statfs_t
	if err := syscall.Statfs(actual, &stats); err != nil {
		return fmt.Errorf("inspect docker data capacity: %w", err)
	}
	if stats.Bsize <= 0 {
		return fmt.Errorf("inspect docker data capacity: invalid filesystem block size")
	}
	blockSize := uint64(stats.Bsize)
	available := uint64(stats.Bavail)
	if available > ^uint64(0)/blockSize {
		available = ^uint64(0)
	} else {
		available *= blockSize
	}
	minimum := uint64(d.minFreeBytes)
	if uint64(incomingBytes) > ^uint64(0)-minimum {
		return domain.NewError(domain.CodeCapacityExceeded, "docker data filesystem is below its free-space reserve")
	}
	minimum += uint64(incomingBytes)
	if available < minimum {
		return domain.NewError(domain.CodeCapacityExceeded, "docker data filesystem is below its free-space reserve")
	}
	return nil
}

// DeleteVolume removes a named Docker volume. A missing volume is already
// cleaned up, while an attached volume is surfaced as a retryable app error.
func (d *Docker) DeleteVolume(ctx context.Context, name string) error {
	if !volumeNamePattern.MatchString(name) {
		return fmt.Errorf("delete docker volume: invalid volume name")
	}
	requestURL, err := d.api.Endpoint(ctx, "/volumes/"+url.PathEscape(name))
	if err != nil {
		return fmt.Errorf("delete docker volume: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, requestURL, nil)
	if err != nil {
		return fmt.Errorf("create docker volume delete request: %w", err)
	}
	response, err := d.client.Do(request)
	if err != nil {
		return fmt.Errorf("delete docker volume: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode == http.StatusNotFound || response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return nil
	}
	if response.StatusCode == http.StatusConflict {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return fmt.Errorf("delete docker volume %q: %w", name, app.ErrVolumeInUse)
	}
	return fmt.Errorf("delete docker volume %q: docker engine returned HTTP %d%s", name, response.StatusCode, dockerErrorSuffix(response))
}

// EnsureWorkspaceVolume creates the deterministic Workspace volume only for a
// pending registry row, then checks Docker's persisted labels before it can be
// mounted. An existing volume with another identity is never reused.
func (d *Docker) EnsureWorkspaceVolume(ctx context.Context, name, instanceID, ownerID, workspaceID string) error {
	labels, err := workspaceVolumeLabels(name, instanceID, ownerID, workspaceID)
	if err != nil {
		return err
	}
	volume, exists, err := d.inspectWorkspaceVolume(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		return verifyWorkspaceVolume(volume, name, labels)
	}
	body, err := json.Marshal(struct {
		Name   string            `json:"Name"`
		Driver string            `json:"Driver"`
		Labels map[string]string `json:"Labels"`
	}{Name: name, Driver: "local", Labels: labels})
	if err != nil {
		return fmt.Errorf("encode workspace volume identity: %w", err)
	}
	endpoint, err := d.api.Endpoint(ctx, "/volumes/create")
	if err != nil {
		return fmt.Errorf("create workspace volume: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build workspace volume request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := d.client.Do(request)
	if err != nil {
		return fmt.Errorf("create workspace volume: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		if response.StatusCode != http.StatusConflict {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
			return fmt.Errorf("create workspace volume: Docker Engine returned HTTP %d", response.StatusCode)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	}
	volume, exists, err = d.inspectWorkspaceVolume(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("Docker Engine did not create the workspace volume")
	}
	return verifyWorkspaceVolume(volume, name, labels)
}

// VerifyWorkspaceVolume rejects missing volumes; a ready registry row never
// authorizes Docker to create a replacement empty volume.
func (d *Docker) VerifyWorkspaceVolume(ctx context.Context, name, instanceID, ownerID, workspaceID string) error {
	labels, err := workspaceVolumeLabels(name, instanceID, ownerID, workspaceID)
	if err != nil {
		return err
	}
	volume, exists, err := d.inspectWorkspaceVolume(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("registered Workspace volume is missing")
	}
	return verifyWorkspaceVolume(volume, name, labels)
}

type dockerVolumeIdentity struct {
	Name   string            `json:"Name"`
	Driver string            `json:"Driver"`
	Labels map[string]string `json:"Labels"`
}

func workspaceVolumeLabels(name, instanceID, ownerID, workspaceID string) (map[string]string, error) {
	if !domain.ValidWorkspaceID(workspaceID) || !domain.ValidUserID(ownerID) || strings.TrimSpace(instanceID) == "" || len(instanceID) > 128 ||
		name != app.WorkspaceNamedVolumeName(workspaceID) || !volumeNamePattern.MatchString(name) {
		return nil, fmt.Errorf("workspace volume identity is invalid")
	}
	return map[string]string{
		"morde.managed_by":          "portal",
		"morde.service_instance_id": instanceID,
		"morde.owner_id":            ownerID,
		"morde.workspace_id":        workspaceID,
	}, nil
}

func (d *Docker) inspectWorkspaceVolume(ctx context.Context, name string) (dockerVolumeIdentity, bool, error) {
	if !volumeNamePattern.MatchString(name) {
		return dockerVolumeIdentity{}, false, errors.New("workspace volume name is invalid")
	}
	endpoint, err := d.api.Endpoint(ctx, "/volumes/"+url.PathEscape(name))
	if err != nil {
		return dockerVolumeIdentity{}, false, fmt.Errorf("inspect workspace volume: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return dockerVolumeIdentity{}, false, fmt.Errorf("build workspace volume inspection request: %w", err)
	}
	response, err := d.client.Do(request)
	if err != nil {
		return dockerVolumeIdentity{}, false, fmt.Errorf("inspect workspace volume: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return dockerVolumeIdentity{}, false, nil
	}
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return dockerVolumeIdentity{}, false, fmt.Errorf("inspect workspace volume: Docker Engine returned HTTP %d", response.StatusCode)
	}
	var value dockerVolumeIdentity
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&value); err != nil {
		return dockerVolumeIdentity{}, false, errors.New("inspect workspace volume: Docker Engine returned invalid JSON")
	}
	return value, true, nil
}

func verifyWorkspaceVolume(value dockerVolumeIdentity, name string, labels map[string]string) error {
	if value.Name != name || value.Driver != "local" {
		return errors.New("workspace volume name or driver does not match its registry")
	}
	for key, expected := range labels {
		if value.Labels[key] != expected {
			return errors.New("workspace volume labels do not match its registry")
		}
	}
	return nil
}

// OpenLegacyVolume returns a read-only filesystem view of an unattached Docker
// volume retained for legacy workspace import. The caller must close the
// returned closer when finished.
func (d *Docker) OpenLegacyVolume(ctx context.Context, name string) (fs.FS, io.Closer, error) {
	root, _, err := d.openLegacyVolumeRoot(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	return root.FS(), root, nil
}

func (d *Docker) openLegacyVolumeRoot(ctx context.Context, name string) (*os.Root, string, error) {
	if !volumeNamePattern.MatchString(name) {
		return nil, "", fmt.Errorf("open legacy docker volume: invalid volume name")
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	var info struct {
		DockerRootDir string `json:"DockerRootDir"`
	}
	if err := d.getJSON(ctx, "/info", &info, "read docker data root"); err != nil {
		return nil, "", err
	}
	actualRoot, err := filepath.EvalSymlinks(info.DockerRootDir)
	if err != nil {
		return nil, "", fmt.Errorf("resolve docker data root: %w", err)
	}
	expectedRoot, err := filepath.EvalSymlinks(d.dataDir)
	if err != nil || actualRoot != expectedRoot {
		return nil, "", fmt.Errorf("configured docker data directory does not match the running engine")
	}

	var volume struct {
		Name       string `json:"Name"`
		Driver     string `json:"Driver"`
		Mountpoint string `json:"Mountpoint"`
	}
	if err := d.getJSON(ctx, "/volumes/"+url.PathEscape(name), &volume, "inspect legacy docker volume"); err != nil {
		return nil, "", err
	}
	if volume.Name != name || volume.Driver != "local" {
		return nil, "", fmt.Errorf("legacy docker volume identity or driver does not match")
	}
	expectedMountpoint := filepath.Join(actualRoot, "volumes", name, "_data")
	if !filepath.IsAbs(volume.Mountpoint) || filepath.Clean(volume.Mountpoint) != expectedMountpoint {
		return nil, "", fmt.Errorf("legacy docker volume mountpoint is outside its expected location")
	}
	resolvedMountpoint, err := filepath.EvalSymlinks(volume.Mountpoint)
	if err != nil || resolvedMountpoint != expectedMountpoint {
		return nil, "", fmt.Errorf("legacy docker volume mountpoint does not resolve to its expected location")
	}
	if err := d.rejectAttachedVolume(ctx, name); err != nil {
		return nil, "", err
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	dockerRootHandle, err := os.OpenRoot(actualRoot)
	if err != nil {
		return nil, "", fmt.Errorf("open docker data root: %w", err)
	}
	root, err := dockerRootHandle.OpenRoot(filepath.Join("volumes", name, "_data"))
	_ = dockerRootHandle.Close()
	if err != nil {
		return nil, "", fmt.Errorf("open legacy docker volume: %w", err)
	}
	rootInfo, rootErr := root.Stat(".")
	pathInfo, pathErr := os.Stat(expectedMountpoint)
	if rootErr != nil || pathErr != nil || !os.SameFile(rootInfo, pathInfo) {
		_ = root.Close()
		return nil, "", fmt.Errorf("legacy docker volume changed while opening")
	}
	return root, expectedMountpoint, nil
}

func (d *Docker) ValidateLegacyWorkspaceTree(ctx context.Context, name, relativeRoot string) (mordesync.Manifest, error) {
	if ctx == nil {
		return mordesync.Manifest{}, errors.New("validate legacy workspace tree: context is required")
	}
	if relativeRoot != "" {
		if err := workspace.ValidateRelativePath(relativeRoot); err != nil {
			return mordesync.Manifest{}, fmt.Errorf("validate legacy Workspace root: %w", err)
		}
	}
	volumeRoot, mountpoint, err := d.openLegacyVolumeRoot(ctx, name)
	if err != nil {
		return mordesync.Manifest{}, err
	}
	defer func() { _ = volumeRoot.Close() }()
	rootName := filepath.FromSlash(relativeRoot)
	if rootName == "" {
		rootName = "."
	}
	treeRoot, err := volumeRoot.OpenRoot(rootName)
	if err != nil {
		return mordesync.Manifest{}, fmt.Errorf("open legacy Workspace tree root: %w", err)
	}
	defer func() { _ = treeRoot.Close() }()
	rootInfo, err := treeRoot.Stat(".")
	volumeInfo, volumeErr := volumeRoot.Lstat(rootName)
	treePath := mountpoint
	if relativeRoot != "" {
		treePath = filepath.Join(mountpoint, rootName)
	}
	pathInfo, pathErr := os.Lstat(treePath)
	if err != nil || volumeErr != nil || pathErr != nil || !rootInfo.IsDir() || volumeInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(rootInfo, volumeInfo) || !os.SameFile(rootInfo, pathInfo) {
		return mordesync.Manifest{}, errors.Join(errors.New("legacy Workspace tree root changed while opening"), err, volumeErr, pathErr)
	}
	if err := workspacefs.ValidateTreeCopy(ctx, treePath); err != nil {
		return mordesync.Manifest{}, fmt.Errorf("validate legacy Workspace tree metadata: %w", err)
	}
	manifest, err := mordesync.Scan(ctx, treePath, nil)
	if err != nil {
		return mordesync.Manifest{}, fmt.Errorf("scan legacy Workspace metadata: %w", err)
	}
	currentVolumeRoot, currentMountpoint, err := d.openLegacyVolumeRoot(ctx, name)
	if err != nil {
		return mordesync.Manifest{}, fmt.Errorf("recheck legacy Workspace volume after validation: %w", err)
	}
	defer func() { _ = currentVolumeRoot.Close() }()
	currentTreeRoot, err := currentVolumeRoot.OpenRoot(rootName)
	if err != nil {
		return mordesync.Manifest{}, fmt.Errorf("reopen legacy Workspace tree root: %w", err)
	}
	defer func() { _ = currentTreeRoot.Close() }()
	currentInfo, err := currentTreeRoot.Stat(".")
	if err != nil || currentMountpoint != mountpoint || !os.SameFile(rootInfo, currentInfo) {
		return mordesync.Manifest{}, errors.Join(errors.New("legacy Workspace tree changed during metadata validation"), err)
	}
	if err := ctx.Err(); err != nil {
		return mordesync.Manifest{}, err
	}
	return manifest, nil
}

func (d *Docker) rejectAttachedVolume(ctx context.Context, name string) error {
	var containers []struct {
		ID string `json:"Id"`
	}
	if err := d.getJSON(ctx, "/containers/json?all=1", &containers, "list docker containers"); err != nil {
		return err
	}
	for _, container := range containers {
		if container.ID == "" {
			return fmt.Errorf("inspect docker containers: engine returned an empty container ID")
		}
		var inspected struct {
			Mounts []struct {
				Type string `json:"Type"`
				Name string `json:"Name"`
			} `json:"Mounts"`
			HostConfig struct {
				Binds  []string `json:"Binds"`
				Mounts []struct {
					Type   string `json:"Type"`
					Source string `json:"Source"`
				} `json:"Mounts"`
			} `json:"HostConfig"`
		}
		if err := d.getJSON(ctx, "/containers/"+url.PathEscape(container.ID)+"/json", &inspected, "inspect docker container"); err != nil {
			return err
		}
		for _, mount := range inspected.Mounts {
			if mount.Type == "volume" && mount.Name == name {
				return fmt.Errorf("legacy docker volume %q is still referenced by a container", name)
			}
		}
		for _, mount := range inspected.HostConfig.Mounts {
			if mount.Type == "volume" && mount.Source == name {
				return fmt.Errorf("legacy docker volume %q is still referenced by a container", name)
			}
		}
		for _, bind := range inspected.HostConfig.Binds {
			if strings.SplitN(bind, ":", 2)[0] == name {
				return fmt.Errorf("legacy docker volume %q is still referenced by a container", name)
			}
		}
	}
	return nil
}

func (d *Docker) getJSON(ctx context.Context, path string, destination any, operation string) error {
	endpoint, err := d.api.Endpoint(ctx, path)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create docker %s request: %w", operation, err)
	}
	response, err := d.client.Do(request)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		if response.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%s: resource not found", operation)
		}
		return fmt.Errorf("%s: engine returned HTTP %d%s", operation, response.StatusCode, dockerErrorSuffix(response))
	}
	const maxDockerJSONResponse = 16 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxDockerJSONResponse+1))
	if err != nil {
		return fmt.Errorf("read %s response: %w", operation, err)
	}
	if len(body) > maxDockerJSONResponse {
		return fmt.Errorf("read %s response: engine response is too large", operation)
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return fmt.Errorf("read %s response: invalid engine response", operation)
	}
	return nil
}

// dockerErrorSuffix reads only a small, single-line summary from the Engine.
// It is best-effort context for retry logs and never includes an unbounded body.
func dockerErrorSuffix(response *http.Response) string {
	const maxErrorBody = 4096
	buffer, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
	message := strings.TrimSpace(string(buffer))
	if message == "" {
		return ""
	}
	message = strings.ReplaceAll(strings.ReplaceAll(message, "\r", " "), "\n", " ")
	return ": " + message
}

// CloseIdleConnections releases keep-alive connections when the adapter is shut down.
func (d *Docker) CloseIdleConnections() {
	d.client.CloseIdleConnections()
}
