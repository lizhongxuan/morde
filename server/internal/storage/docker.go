package storage

import (
	"context"
	"encoding/json"
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

	"morde/server/internal/app"
	"morde/server/internal/dockerapi"
	"morde/server/internal/domain"
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
	if available < uint64(d.minFreeBytes) {
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

// OpenLegacyVolume returns a read-only filesystem view of an unattached Docker
// volume retained for legacy workspace import. The caller must close the
// returned closer when finished.
func (d *Docker) OpenLegacyVolume(ctx context.Context, name string) (fs.FS, io.Closer, error) {
	if !volumeNamePattern.MatchString(name) {
		return nil, nil, fmt.Errorf("open legacy docker volume: invalid volume name")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	var info struct {
		DockerRootDir string `json:"DockerRootDir"`
	}
	if err := d.getJSON(ctx, "/info", &info, "read docker data root"); err != nil {
		return nil, nil, err
	}
	actualRoot, err := filepath.EvalSymlinks(info.DockerRootDir)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve docker data root: %w", err)
	}
	expectedRoot, err := filepath.EvalSymlinks(d.dataDir)
	if err != nil || actualRoot != expectedRoot {
		return nil, nil, fmt.Errorf("configured docker data directory does not match the running engine")
	}

	var volume struct {
		Name       string `json:"Name"`
		Driver     string `json:"Driver"`
		Mountpoint string `json:"Mountpoint"`
	}
	if err := d.getJSON(ctx, "/volumes/"+url.PathEscape(name), &volume, "inspect legacy docker volume"); err != nil {
		return nil, nil, err
	}
	if volume.Name != name || volume.Driver != "local" {
		return nil, nil, fmt.Errorf("legacy docker volume identity or driver does not match")
	}
	expectedMountpoint := filepath.Join(actualRoot, "volumes", name, "_data")
	if !filepath.IsAbs(volume.Mountpoint) || filepath.Clean(volume.Mountpoint) != expectedMountpoint {
		return nil, nil, fmt.Errorf("legacy docker volume mountpoint is outside its expected location")
	}
	resolvedMountpoint, err := filepath.EvalSymlinks(volume.Mountpoint)
	if err != nil || resolvedMountpoint != expectedMountpoint {
		return nil, nil, fmt.Errorf("legacy docker volume mountpoint does not resolve to its expected location")
	}
	if err := d.rejectAttachedVolume(ctx, name); err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	dockerRootHandle, err := os.OpenRoot(actualRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("open docker data root: %w", err)
	}
	root, err := dockerRootHandle.OpenRoot(filepath.Join("volumes", name, "_data"))
	_ = dockerRootHandle.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("open legacy docker volume: %w", err)
	}
	rootInfo, rootErr := root.Stat(".")
	pathInfo, pathErr := os.Stat(expectedMountpoint)
	if rootErr != nil || pathErr != nil || !os.SameFile(rootInfo, pathInfo) {
		_ = root.Close()
		return nil, nil, fmt.Errorf("legacy docker volume changed while opening")
	}
	return root.FS(), root, nil
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
