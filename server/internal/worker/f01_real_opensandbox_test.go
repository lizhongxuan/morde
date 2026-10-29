package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"morde/server/internal/app"
	"morde/server/internal/config"
	"morde/server/internal/dockerapi"
	"morde/server/internal/domain"
	domainTemplate "morde/server/internal/domain/template"
	"morde/server/internal/orchestrator"
	"morde/server/internal/storage"
)

const (
	f01RealOpenSandboxEnv       = "CH_F01_REAL_OPEN_SANDBOX"
	f01RealOpenSandboxURL       = "CH_F01_OSB_LOCAL_URL"
	f01RealOpenSandboxAPIKey    = "CH_F01_OSB_LOCAL_API_KEY"
	f01RealOpenSandboxDocker    = "CH_F01_OSB_LOCAL_DOCKER_SOCKET"
	f01RealOpenSandboxTestImage = "CH_F01_OSB_LOCAL_TEST_IMAGE"
)

type f01LocalOpenSandboxFixture struct {
	test         *testing.T
	client       *orchestrator.Client
	volumes      *storage.Docker
	dockerClient *http.Client
	dockerAPI    *dockerapi.EndpointResolver
	dockerSocket string
	image        string
	instance     string
	hostID       string
	ownerID      string
	claims       []string
	createdIDs   map[string]struct{}
	cleanupSafe  bool
	marker       string
}

func newF01LocalOpenSandboxFixture(t *testing.T, hostID, ownerID string) (*f01LocalOpenSandboxFixture, bool) {
	t.Helper()
	if os.Getenv(f01RealOpenSandboxEnv) != "1" {
		return nil, false
	}

	baseURL := strings.TrimSpace(os.Getenv(f01RealOpenSandboxURL))
	apiKey := os.Getenv(f01RealOpenSandboxAPIKey)
	dockerSocket := strings.TrimSpace(os.Getenv(f01RealOpenSandboxDocker))
	image := strings.TrimSpace(os.Getenv(f01RealOpenSandboxTestImage))
	if apiKey == "" || image == "" {
		t.Fatalf("%s, %s, and %s are required when %s=1", f01RealOpenSandboxAPIKey, f01RealOpenSandboxDocker, f01RealOpenSandboxTestImage, f01RealOpenSandboxEnv)
	}
	digestMarker := "@sha256:"
	digestIndex := strings.LastIndex(image, digestMarker)
	if digestIndex <= 0 || len(image)-digestIndex-len(digestMarker) != 64 {
		t.Fatalf("%s must be a locally cached image pinned by sha256 digest", f01RealOpenSandboxTestImage)
	}
	if _, err := hex.DecodeString(image[digestIndex+len(digestMarker):]); err != nil {
		t.Fatalf("%s must be a locally cached image pinned by sha256 digest", f01RealOpenSandboxTestImage)
	}
	if err := requireF01LoopbackURL(baseURL); err != nil {
		t.Fatalf("%s must be a direct HTTP(S) URL on literal 127.0.0.1 with an explicit port", f01RealOpenSandboxURL)
	}
	if !filepath.IsAbs(dockerSocket) {
		t.Fatalf("%s must be an absolute local Docker Unix socket", f01RealOpenSandboxDocker)
	}
	socketInfo, err := os.Lstat(dockerSocket)
	if err != nil || socketInfo.Mode()&os.ModeSocket == 0 || socketInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s must name an existing Unix socket, not a symlink or Docker context", f01RealOpenSandboxDocker)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("the Docker CLI is required for the explicit local volume probe")
	}

	dockerClient := f01LocalDockerHTTPClient(dockerSocket)
	dockerAPI := dockerapi.NewEndpointResolver(dockerClient)
	imageExists, err := f01LocalDockerResourceExists(t.Context(), dockerClient, dockerAPI, "/images/"+url.PathEscape(image)+"/json")
	if err != nil {
		t.Fatalf("inspect image through the explicitly supplied local Docker socket: %v", err)
	}
	if !imageExists {
		t.Skip("configured local OpenSandbox test image is not cached; the opt-in check never pulls images")
	}
	entrypoint, err := f01LocalDockerImageEntrypoint(t.Context(), dockerClient, dockerAPI, image)
	if err != nil {
		t.Fatalf("inspect configured local OpenSandbox test image entrypoint: %v", err)
	}
	if err := requireF01MordeHostEntrypoint(entrypoint); err != nil {
		t.Fatalf("%s does not satisfy the Morde Host image contract: %v", f01RealOpenSandboxTestImage, err)
	}
	claims := []string{
		domain.ProfileVolumeName(hostID), domain.WorkspaceRootVolumeName(hostID),
		domain.RuntimeVolumeName(hostID), domain.HostAppsVolumeName(hostID),
	}
	for _, claim := range claims {
		exists, err := f01LocalDockerResourceExists(t.Context(), dockerClient, dockerAPI, "/volumes/"+url.PathEscape(claim))
		if err != nil {
			t.Fatalf("check unique local Host volume %s: %v", claim, err)
		}
		if exists {
			t.Skip("a generated Host volume already exists in the supplied local Docker engine; refusing to use or delete it")
		}
	}

	guardPath := filepath.Join(t.TempDir(), "loopback-network-guard")
	guard := []byte("#!/bin/sh\n[ \"$1\" = check ] || exit 1\ncase \"$CH_OSB_URL\" in http://127.0.0.1:*|https://127.0.0.1:*) exit 0;; *) exit 1;; esac\n")
	if err := os.WriteFile(guardPath, guard, 0o700); err != nil {
		t.Fatal(err)
	}
	profile := config.Profile{
		Name: "f01-local-release", BaseURL: baseURL, APIKey: apiKey,
		RequestTimeout: 30 * time.Second, NetworkGuardPath: guardPath,
	}
	client := orchestrator.NewClient(profile, newF01LocalOpenSandboxHTTPClient(), prometheus.NewRegistry())
	volumeClient, err := storage.NewDocker((&url.URL{Scheme: "unix", Path: dockerSocket}).String(), os.TempDir(), 1)
	if err != nil {
		t.Fatalf("configure explicit local Docker volume cleanup: %v", err)
	}
	markerBytes := make([]byte, 16)
	if _, err := rand.Read(markerBytes); err != nil {
		t.Fatalf("create a unique local volume marker: %v", err)
	}
	fixture := &f01LocalOpenSandboxFixture{
		test:   t,
		client: client, volumes: volumeClient, dockerClient: dockerClient, dockerAPI: dockerAPI, dockerSocket: dockerSocket,
		image: image, instance: "f01-release-" + hostID, hostID: hostID, ownerID: ownerID,
		claims: claims, createdIDs: make(map[string]struct{}), marker: "f01-volume-" + hex.EncodeToString(markerBytes),
	}
	t.Cleanup(fixture.cleanup)
	return fixture, true
}

func newF01LocalOpenSandboxHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{Proxy: nil}, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func TestF01LocalOpenSandboxHTTPClientDoesNotFollowRedirects(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Store(true) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer origin.Close()

	response, err := newF01LocalOpenSandboxHTTPClient().Get(origin.URL)
	if err != nil {
		t.Fatalf("request local OpenSandbox redirect: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusFound || redirected.Load() {
		t.Fatalf("redirect response status=%d, target reached=%t", response.StatusCode, redirected.Load())
	}
}

func TestRequireF01MordeHostEntrypoint(t *testing.T) {
	tests := []struct {
		name       string
		entrypoint []string
		wantErr    bool
	}{
		{name: "Morde Host bootstrap", entrypoint: []string{domainTemplate.RuntimeBootstrapEntrypoint}},
		{name: "Alpine shell", entrypoint: []string{"/bin/sh"}, wantErr: true},
		{name: "missing", wantErr: true},
		{name: "extra arguments", entrypoint: []string{domainTemplate.RuntimeBootstrapEntrypoint, "sleep"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := requireF01MordeHostEntrypoint(test.entrypoint)
			if (err != nil) != test.wantErr {
				t.Fatalf("requireF01MordeHostEntrypoint(%#v) error = %v, wantErr %t", test.entrypoint, err, test.wantErr)
			}
		})
	}
}

func TestF01WorkspaceProbeCommandsSafelyQuoteContent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace root's files")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	relativePath := "reports/pending-after-release.txt"
	content := []byte("pending 'quoted'; $(printf injected)\n")
	command, err := f01WorkspaceFileWriteProbeCommand(root, relativePath, content)
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(t.Context(), "sh", "-c", command).Output()
	if err != nil || string(output) != string(content) {
		t.Fatalf("quoted Workspace write output = %q, err=%v; want exact file content", output, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relativePath))); err != nil || string(got) != string(content) {
		t.Fatalf("quoted Workspace file = %q, err=%v; want %q", got, err, content)
	}
	readCommand, err := f01WorkspaceFileReadProbeCommand(root, relativePath)
	if err != nil {
		t.Fatal(err)
	}
	readback, err := exec.CommandContext(t.Context(), "sh", "-c", readCommand).Output()
	if err != nil || string(readback) != string(content) {
		t.Fatalf("quoted Workspace readback = %q, err=%v; want exact file content", readback, err)
	}
	if _, err := f01WorkspaceFileReadProbeCommand(root, "../outside.txt"); err == nil {
		t.Fatal("Workspace probe accepted a path outside its mounted volume")
	}
}

func (f *f01LocalOpenSandboxFixture) runtimeTemplate(value domainTemplate.Template) domainTemplate.Template {
	value.Image = f.image
	value.Workspace.Size = "64Mi"
	return value
}

func (f *f01LocalOpenSandboxFixture) createInitial(ctx context.Context, value domainTemplate.Template, generation int64) app.Sandbox {
	request := app.SandboxCreateRequest{
		Image: f.image, Entrypoint: []string{"/bin/sh", "-c", "sleep 600"}, Timeout: value.Lifecycle.TTL,
		Requests: app.SandboxResources{CPU: "100m", Memory: "128Mi"},
		Limits:   app.SandboxResources{CPU: "1", Memory: "512Mi"},
		Env:      map[string]string{"EXECD_ACCESS_TOKEN": f.marker},
		Metadata: map[string]string{
			"ch-env": f.instance, "ch-host": f.hostID, "ch-owner": f.ownerID,
			"ch-generation": strconv.FormatInt(generation, 10),
		},
		Volumes: []app.SandboxVolume{
			{Name: "profile-data", MountPath: app.ProfileVolumeMountPath, ClaimName: f.claims[0], Storage: app.ProfileVolumeStorage},
			{Name: "workspace-root", MountPath: value.Workspace.MountPath, ClaimName: f.claims[1], Storage: value.Workspace.Size},
			{Name: "runtime-data", MountPath: app.RuntimeDataVolumeMountPath, ClaimName: f.claims[2], Storage: app.RuntimeDataVolumeStorage},
			{Name: "host-apps", MountPath: app.HostAppsVolumeMountPath, ClaimName: f.claims[3], Storage: app.HostAppsVolumeStorage},
		},
	}
	sandbox, err := f.client.Create(ctx, request)
	f.recordCreate(sandbox, err)
	if err != nil {
		f.test.Fatalf("create initial local OpenSandbox Host: %v", err)
		return app.Sandbox{}
	}
	if !f.owns(sandbox) {
		f.test.Fatal("OpenSandbox Create response did not match the unique Host identity")
		return app.Sandbox{}
	}
	if err := f.waitRunning(ctx, sandbox.ID); err != nil {
		f.test.Fatalf("wait for initial local OpenSandbox Host: %v", err)
		return app.Sandbox{}
	}
	exists, err := f.volumeExists(ctx, f.claims[1])
	if err != nil || !exists {
		f.test.Fatalf("verify Workspace volume on the explicitly supplied Docker engine: %v", errOr(err, errors.New("OpenSandbox did not create the Host Workspace volume in the supplied engine")))
		return app.Sandbox{}
	}
	return sandbox
}

func (f *f01LocalOpenSandboxFixture) recordCreate(value app.Sandbox, err error) {
	if err != nil {
		f.cleanupSafe = false
		return
	}
	if !f.owns(value) {
		f.cleanupSafe = false
		return
	}
	f.createdIDs[value.ID] = struct{}{}
	f.cleanupSafe = true
}

func (f *f01LocalOpenSandboxFixture) owns(value app.Sandbox) bool {
	return value.ID != "" && value.Metadata["ch-env"] == f.instance && value.Metadata["ch-host"] == f.hostID &&
		value.Metadata["ch-owner"] == f.ownerID && value.Metadata["ch-generation"] != ""
}

func (f *f01LocalOpenSandboxFixture) writeMarkerAfterRelease(ctx context.Context) {
	output, err := f.runVolumeProbe(ctx, f.claims[1], false,
		"printf '%s' '"+f.marker+"' > /workspace/.morde-f01-marker && cat /workspace/.morde-f01-marker")
	if err != nil || output != f.marker {
		f.test.Fatalf("write marker to released Host Workspace volume: %v", errOr(err, errors.New("local Workspace volume marker did not round-trip")))
	}
}

func (f *f01LocalOpenSandboxFixture) verifyMarkerAfterRestore(ctx context.Context) {
	exists, err := f.volumeExists(ctx, f.claims[1])
	if err != nil || !exists {
		f.test.Fatalf("verify restored Host Workspace volume: %v", errOr(err, errors.New("Host Workspace volume disappeared during release")))
	}
	output, err := f.runVolumeProbe(ctx, f.claims[1], true, "cat /workspace/.morde-f01-marker")
	if err != nil || output != f.marker {
		f.test.Fatalf("read marker from restored Host Workspace volume: %v", errOr(err, errors.New("restored Workspace volume marker did not match")))
	}
}

func (f *f01LocalOpenSandboxFixture) writeWorkspaceFileBeforeRelease(ctx context.Context, relativePath string, content []byte) {
	command, err := f01WorkspaceFileWriteProbeCommand("/workspace", relativePath, content)
	if err != nil {
		f.test.Fatalf("build Host Workspace file write probe: %v", err)
		return
	}
	output, err := f.runVolumeProbeOutput(ctx, f.claims[1], false, command)
	if err != nil || string(output) != string(content) {
		f.test.Fatalf("write pending file to Host Workspace volume before release: %v", errOr(err, errors.New("Host Workspace file did not round-trip")))
	}
}

func (f *f01LocalOpenSandboxFixture) verifyWorkspaceFileAfterRestore(ctx context.Context, relativePath string, content []byte) {
	command, err := f01WorkspaceFileReadProbeCommand("/workspace", relativePath)
	if err != nil {
		f.test.Fatalf("build Host Workspace file read probe: %v", err)
		return
	}
	output, err := f.runVolumeProbeOutput(ctx, f.claims[1], true, command)
	if err != nil || string(output) != string(content) {
		f.test.Fatalf("read pending file from Host Workspace volume after restore: %v", errOr(err, errors.New("restored Host Workspace file did not match")))
	}
}

func (f *f01LocalOpenSandboxFixture) runVolumeProbe(ctx context.Context, claim string, readOnly bool, command string) (string, error) {
	output, err := f.runVolumeProbeOutput(ctx, claim, readOnly, command)
	return strings.TrimSpace(string(output)), err
}

func (f *f01LocalOpenSandboxFixture) runVolumeProbeOutput(ctx context.Context, claim string, readOnly bool, command string) ([]byte, error) {
	name := "f01-volume-probe-" + f.hostID
	exists, err := f01LocalDockerResourceExists(ctx, f.dockerClient, f.dockerAPI, "/containers/"+url.PathEscape(name)+"/json")
	if err != nil {
		return nil, errors.New("inspect unique Docker volume probe container")
	}
	if exists {
		return nil, errors.New("unique Docker volume probe container already exists")
	}
	args := []string{
		"--host", "unix://" + f.dockerSocket, "run", "--name", name, "--pull=never", "--network", "none", "--read-only",
		"--cap-drop=ALL", "--security-opt=no-new-privileges", "--tmpfs", "/tmp:rw,noexec,nosuid,size=16m",
		"--entrypoint", "/bin/sh",
		"--volume", claim + ":/workspace",
	}
	if readOnly {
		args[len(args)-1] += ":ro"
		// Read restored files as the fixed AIO user, not root.
		args = append(args, "--user", "1000:1000")
	}
	args = append(args, f.image, "-c", command)
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, "docker", args...)
	cmd.Env = f.localDockerEnvironment()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, runErr := cmd.Output()
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cleanupCancel()
	cleanup := exec.CommandContext(cleanupCtx, "docker", "--host", "unix://"+f.dockerSocket, "container", "rm", "--force", name)
	cleanup.Env = f.localDockerEnvironment()
	_ = cleanup.Run()
	if runErr != nil {
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 512 {
			detail = detail[:512]
		}
		return nil, fmt.Errorf("execute bounded command through the explicit local Docker socket: %w: %s", runErr, detail)
	}
	return output, nil
}

func f01WorkspaceFileWriteProbeCommand(root, relativePath string, content []byte) (string, error) {
	target, parent, err := f01WorkspaceProbeTarget(root, relativePath)
	if err != nil {
		return "", err
	}
	return "mkdir -p " + f01ShellQuote(parent) + " && printf '%s' " + f01ShellQuote(string(content)) +
		" > " + f01ShellQuote(target) + " && cat " + f01ShellQuote(target), nil
}

func f01WorkspaceFileReadProbeCommand(root, relativePath string) (string, error) {
	target, _, err := f01WorkspaceProbeTarget(root, relativePath)
	if err != nil {
		return "", err
	}
	return "cat " + f01ShellQuote(target), nil
}

func f01WorkspaceProbeTarget(root, relativePath string) (string, string, error) {
	if !path.IsAbs(root) || path.Clean(root) != root || strings.IndexByte(root, 0) >= 0 {
		return "", "", errors.New("Workspace probe root must be a clean absolute path")
	}
	if relativePath == "" || path.IsAbs(relativePath) || strings.IndexByte(relativePath, 0) >= 0 {
		return "", "", errors.New("Workspace probe path must be relative to the mounted volume")
	}
	cleanRelative := path.Clean(relativePath)
	if cleanRelative == "." || cleanRelative == ".." || strings.HasPrefix(cleanRelative, "../") {
		return "", "", errors.New("Workspace probe path escapes the mounted volume")
	}
	target := path.Join(root, cleanRelative)
	rootPrefix := strings.TrimSuffix(root, "/") + "/"
	if target == root || !strings.HasPrefix(target, rootPrefix) {
		return "", "", errors.New("Workspace probe path escapes the mounted volume")
	}
	return target, path.Dir(target), nil
}

func f01ShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func (f *f01LocalOpenSandboxFixture) volumeExists(ctx context.Context, name string) (bool, error) {
	return f01LocalDockerResourceExists(ctx, f.dockerClient, f.dockerAPI, "/volumes/"+url.PathEscape(name))
}

func (f *f01LocalOpenSandboxFixture) waitRunning(ctx context.Context, id string) error {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		value, err := f.client.Get(ctx, id)
		if err == nil {
			if !f.owns(value) {
				return errors.New("OpenSandbox Host metadata changed during startup")
			}
			if value.State == app.SandboxRunning {
				return nil
			}
			if value.State == app.SandboxFailed || value.State == app.SandboxTerminated {
				return fmt.Errorf("OpenSandbox Host entered state %q: %s", value.State, strings.TrimSpace(value.Reason))
			}
		} else if !errors.Is(err, app.ErrSandboxUnavailable) {
			return errors.New("read local OpenSandbox Host state")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return errors.New("timed out waiting for local OpenSandbox Host")
}

func (f *f01LocalOpenSandboxFixture) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	filter := map[string]string{"ch-env": f.instance, "ch-host": f.hostID}
	listed, err := f.client.List(ctx, filter)
	if err != nil {
		f.test.Errorf("list only this local OpenSandbox Host during cleanup: %v", err)
		return
	}
	ids := make(map[string]struct{}, len(f.createdIDs)+len(listed))
	for id := range f.createdIDs {
		ids[id] = struct{}{}
	}
	for _, value := range listed {
		if !f.owns(value) {
			f.test.Errorf("OpenSandbox cleanup listed a sandbox outside this test identity; refusing to delete it")
			return
		}
		ids[value.ID] = struct{}{}
	}
	cleanupSafe := true
	for id := range ids {
		value, err := f.client.Get(ctx, id)
		if errors.Is(err, app.ErrSandboxNotFound) {
			continue
		}
		if err != nil || !f.owns(value) {
			f.test.Errorf("could not verify local OpenSandbox sandbox ownership during cleanup")
			cleanupSafe = false
			continue
		}
		if err := f.client.Delete(ctx, id); err != nil {
			f.test.Errorf("delete owned local OpenSandbox sandbox: %v", err)
			cleanupSafe = false
			continue
		}
		if err := f.waitTerminated(ctx, id); err != nil {
			f.test.Errorf("confirm local OpenSandbox sandbox deletion: %v", err)
			cleanupSafe = false
		}
	}
	remaining, err := f.client.List(ctx, filter)
	if err != nil || len(remaining) != 0 {
		f.test.Errorf("confirm no owned local OpenSandbox sandbox remains: count=%d err=%v", len(remaining), err)
		cleanupSafe = false
	}
	if !cleanupSafe || !f.cleanupSafe {
		f.test.Errorf("OpenSandbox create or cleanup ownership was uncertain; refusing to delete test Host volumes")
		return
	}
	for _, claim := range f.claims {
		exists, err := f.volumeExists(ctx, claim)
		if err != nil {
			f.test.Errorf("inspect owned local Docker volume %s: %v", claim, err)
			continue
		}
		if exists {
			if err := f.volumes.DeleteVolume(ctx, claim); err != nil {
				f.test.Errorf("delete only this test Host volume %s: %v", claim, err)
			}
		}
	}
}

func (f *f01LocalOpenSandboxFixture) waitTerminated(ctx context.Context, id string) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		value, err := f.client.Get(ctx, id)
		if errors.Is(err, app.ErrSandboxNotFound) {
			return nil
		}
		if err == nil {
			if !f.owns(value) {
				return errors.New("OpenSandbox metadata changed during deletion")
			}
			if value.State == app.SandboxTerminated {
				return nil
			}
		} else if !errors.Is(err, app.ErrSandboxUnavailable) {
			return errors.New("read local OpenSandbox deletion state")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return errors.New("timed out confirming local OpenSandbox deletion")
}

func f01LocalDockerHTTPClient(socketPath string) *http.Client {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socketPath)
		},
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}
}

func f01LocalDockerResourceExists(ctx context.Context, client *http.Client, api *dockerapi.EndpointResolver, path string) (bool, error) {
	endpoint, err := api.Endpoint(ctx, path)
	if err != nil {
		return false, errors.New("resolve local Docker Engine API version")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, errors.New("build local Docker inspection request")
	}
	response, err := client.Do(request)
	if err != nil {
		return false, errors.New("connect to the explicitly supplied local Docker socket")
	}
	defer func() { _ = response.Body.Close() }()
	switch response.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("local Docker inspection returned HTTP %d", response.StatusCode)
	}
}

func f01LocalDockerImageEntrypoint(ctx context.Context, client *http.Client, api *dockerapi.EndpointResolver, image string) ([]string, error) {
	endpoint, err := api.Endpoint(ctx, "/images/"+url.PathEscape(image)+"/json")
	if err != nil {
		return nil, errors.New("resolve local Docker Engine API version")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("build local Docker image inspection request")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("inspect image through the explicitly supplied local Docker socket")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("local Docker image inspection returned HTTP %d", response.StatusCode)
	}
	var inspect struct {
		Config *struct {
			Entrypoint []string `json:"Entrypoint"`
		} `json:"Config"`
	}
	if err := json.NewDecoder(response.Body).Decode(&inspect); err != nil {
		return nil, errors.New("decode local Docker image inspection")
	}
	if inspect.Config == nil {
		return nil, errors.New("local Docker image inspection omitted Config")
	}
	return inspect.Config.Entrypoint, nil
}

func requireF01MordeHostEntrypoint(entrypoint []string) error {
	if len(entrypoint) != 1 || entrypoint[0] != domainTemplate.RuntimeBootstrapEntrypoint {
		return fmt.Errorf("configured entrypoint is %#v; want [%q]", entrypoint, domainTemplate.RuntimeBootstrapEntrypoint)
	}
	return nil
}

func (f *f01LocalOpenSandboxFixture) localDockerEnvironment() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if name != "DOCKER_HOST" && name != "DOCKER_CONTEXT" {
			env = append(env, value)
		}
	}
	return env
}

func requireF01LoopbackURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("expected direct HTTP(S) URL")
	}
	ip := net.ParseIP(parsed.Hostname())
	port, portErr := strconv.Atoi(parsed.Port())
	if ip == nil || !ip.IsLoopback() || parsed.Hostname() != "127.0.0.1" || portErr != nil || port < 1 || port > 65535 {
		return errors.New("expected literal IPv4 loopback and explicit port")
	}
	return nil
}

func errOr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}
