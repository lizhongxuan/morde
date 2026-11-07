package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type dockerDSHFakeGuardian struct {
	mu                               sync.Mutex
	armed                            map[string]DockerCloudDSHGuard
	started, renewals, confirmations int
	destroyed                        bool
	armError                         bool
}

func (g *dockerDSHFakeGuardian) Arm(_ context.Context, guard DockerCloudDSHGuard) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.armError {
		return errors.New("independent guard unavailable")
	}
	if guard.ContainerID == "" || guard.ExpiresAt.IsZero() {
		return errors.New("invalid guard")
	}
	g.armed[guard.ContainerName] = guard
	return nil
}

func (g *dockerDSHFakeGuardian) Started(_ context.Context, guard DockerCloudDSHGuard, pid int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if original, ok := g.armed[guard.ContainerName]; !ok || original.Fence != guard.Fence || original.ContainerID != guard.ContainerID || pid != 1234 {
		return errors.New("guard identity mismatch")
	}
	g.started++
	return nil
}

func (g *dockerDSHFakeGuardian) Renew(_ context.Context, guard DockerCloudDSHGuard) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if original, ok := g.armed[guard.ContainerName]; !ok || original.Fence != guard.Fence || original.ContainerID != guard.ContainerID {
		return errors.New("guard identity mismatch")
	}
	g.renewals++
	g.armed[guard.ContainerName] = guard
	return nil
}

func (g *dockerDSHFakeGuardian) ConfirmDestroyed(_ context.Context, guard DockerCloudDSHGuard) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if original, ok := g.armed[guard.ContainerName]; !ok || original.Fence != guard.Fence || original.ContainerID != guard.ContainerID || original.NonceSHA256 != guard.NonceSHA256 {
		return false, errors.New("guard identity mismatch")
	}
	g.confirmations++
	return g.destroyed, nil
}

type dockerDSHFakeEngine struct {
	mu              sync.Mutex
	guardian        *dockerDSHFakeGuardian
	image           string
	id              string
	name            string
	created         map[string]any
	running         bool
	removed         bool
	keepAfterDelete bool
	waitHang        bool
	unsafeImageEnv  bool
	barePathEnv     bool
	badLabel        bool
	badAccountLabel bool
	missingPID      bool
	kills, creates  int
	input           chan string
	connections     []net.Conn
}

func (f *dockerDSHFakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/version" {
		_ = json.NewEncoder(w).Encode(map[string]string{"ApiVersion": "1.52", "MinAPIVersion": "1.44"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1.44")
	f.mu.Lock()
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
		env := []string{"PATH=/usr/bin:/bin"}
		if f.barePathEnv {
			env = []string{"PATH"}
		}
		if f.unsafeImageEnv {
			env = append(env, "LD_PRELOAD=/hostile.so")
		}
		image := f.image
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"RepoDigests": []string{image}, "Config": map[string]any{"Env": env, "Volumes": nil}})
		return
	case r.Method == http.MethodPost && path == "/containers/create":
		var created map[string]any
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&created) != nil {
			f.mu.Unlock()
			http.Error(w, "invalid", 400)
			return
		}
		f.created, f.name, f.creates = created, r.URL.Query().Get("name"), f.creates+1
		id := f.id
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": id})
		return
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
		if f.removed || f.created == nil {
			f.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
		if ref != f.id && ref != f.name {
			f.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		encoded, _ := json.Marshal(f.created)
		var config map[string]any
		_ = json.Unmarshal(encoded, &config)
		if f.badAccountLabel {
			config["Labels"].(map[string]any)[dockerAccountRuntimeNonceLabel] = strings.Repeat("0", 64)
		}
		pid := 0
		if f.running {
			pid = 1234
		}
		state := map[string]any{"Running": f.running, "Restarting": false, "Paused": false, "Pid": pid}
		if f.missingPID {
			delete(state, "Pid")
		}
		result := map[string]any{"Id": f.id, "Name": "/" + f.name, "Config": config, "HostConfig": config["HostConfig"],
			"State": state, "Mounts": []any{}}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(result)
		return
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/attach"):
		f.mu.Unlock()
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.connections = append(f.connections, conn)
		f.mu.Unlock()
		_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		_ = rw.Flush()
		line, err := rw.ReadString('\n')
		if err != nil {
			return
		}
		f.input <- line
		payload := []byte("dsh-output\n")
		header := make([]byte, 8)
		header[0] = 1
		binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
		_, _ = rw.Write(header)
		_, _ = rw.Write(payload)
		_ = rw.Flush()
		return
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/start"):
		f.guardian.mu.Lock()
		_, armed := f.guardian.armed[f.name]
		f.guardian.mu.Unlock()
		if !armed {
			f.mu.Unlock()
			http.Error(w, "not guarded", 500)
			return
		}
		f.running = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/kill"):
		f.running = false
		f.kills++
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/wait"):
		hang := f.waitHang
		f.mu.Unlock()
		if hang {
			<-r.Context().Done()
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"StatusCode": 0})
		return
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/containers/"):
		if !f.keepAfterDelete {
			f.removed = true
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		f.mu.Unlock()
		http.NotFound(w, r)
	}
}

func newDockerDSHFixture(t *testing.T) (*DockerCloudDSHSupervisor, *dockerDSHFakeEngine, *dockerDSHFakeGuardian) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "morde-dsh-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	guardian := &dockerDSHFakeGuardian{armed: make(map[string]DockerCloudDSHGuard), destroyed: true}
	image := "local/morde-dsh@sha256:" + strings.Repeat("a", 64)
	engine := &dockerDSHFakeEngine{guardian: guardian, image: image, id: strings.Repeat("b", 64), input: make(chan string, 1)}
	server := httptest.NewUnstartedServer(engine)
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(func() {
		engine.mu.Lock()
		for _, conn := range engine.connections {
			_ = conn.Close()
		}
		engine.mu.Unlock()
		server.Close()
	})
	supervisor, err := NewDockerCloudDSHSupervisor(DockerCloudDSHSupervisorOptions{DockerHost: "unix://" + socket,
		Image: image, NodePath: "/usr/local/bin/node", ScriptPath: "/opt/morde-cloud-runtime/bin.mjs", Guardian: guardian})
	if err != nil {
		t.Fatal(err)
	}
	return supervisor, engine, guardian
}

func TestDockerCloudDSHRejectsUnsafeConfiguration(t *testing.T) {
	if _, err := NewDockerCloudDSHSupervisor(DockerCloudDSHSupervisorOptions{}); err == nil {
		t.Fatal("missing guard and digest accepted")
	}
	supervisor, engine, _ := newDockerDSHFixture(t)
	engine.mu.Lock()
	engine.unsafeImageEnv = true
	engine.mu.Unlock()
	if err := supervisor.validateImage(context.Background()); err == nil {
		t.Fatal("image with inherited loader setting accepted")
	}
	engine.mu.Lock()
	creates := engine.creates
	engine.mu.Unlock()
	if creates != 0 {
		t.Fatal("unsafe image created a container")
	}
	if _, err := NewDockerCloudDSHSupervisor(DockerCloudDSHSupervisorOptions{DockerHost: "tcp://127.0.0.1:2375", Image: "mutable", Guardian: &dockerDSHFakeGuardian{}}); err == nil {
		t.Fatal("non-Unix endpoint and mutable image accepted")
	}
}

func TestDockerCloudDSHAcceptsBareDefaultPathEnvironment(t *testing.T) {
	supervisor, engine, _ := newDockerDSHFixture(t)
	engine.mu.Lock()
	engine.barePathEnv = true
	engine.mu.Unlock()
	if err := supervisor.validateImage(context.Background()); err != nil {
		t.Fatalf("official slim image PATH declaration should be accepted: %v", err)
	}
}

func TestDockerCloudDSHRejectsInvalidMultiplexedOutput(t *testing.T) {
	for _, header := range [][]byte{
		{2, 0, 0, 0, 0, 0, 0, 1},
		{1, 1, 0, 0, 0, 0, 0, 1},
		{1, 0, 0, 0, 0, 0x40, 0, 1},
	} {
		stream := &dockerDSHOutput{reader: bufio.NewReader(bytes.NewReader(header))}
		if _, err := stream.Read(make([]byte, 1)); err == nil {
			t.Fatalf("invalid stream header accepted: %v", header)
		}
	}
}
