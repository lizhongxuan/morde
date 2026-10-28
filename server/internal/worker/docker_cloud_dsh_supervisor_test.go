package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
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
	badLabel        bool
	missingPID      bool
	kills, creates  int
	input           chan string
	connections     []net.Conn
}

func (f *dockerDSHFakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, dockerDSHAPIVersion)
	f.mu.Lock()
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
		env := []string{"PATH=/usr/bin:/bin"}
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
		if f.badLabel {
			config["Labels"].(map[string]any)[dockerDSHNonceLabel] = strings.Repeat("0", 64)
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

func newDockerDSHFixture(t *testing.T) (*DockerCloudDSHSupervisor, *dockerDSHFakeEngine, *dockerDSHFakeGuardian, CloudDSHLaunch, []byte) {
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
	key := bytes.Repeat([]byte{5}, 32)
	supervisor, err := NewDockerCloudDSHSupervisor(DockerCloudDSHSupervisorOptions{DockerHost: "unix://" + socket,
		Image: image, NodePath: "/usr/local/bin/node", ScriptPath: "/opt/morde-cloud-runtime/bin.mjs", SupervisorID: "supervisor-test", SigningKey: key, Guardian: guardian})
	if err != nil {
		t.Fatal(err)
	}
	fence := app.AgentInstanceFence{TaskID: "q1234567", OwnerID: "u1234567", AgentInstanceID: "agent-1", AgentGeneration: 1,
		AgentSessionID: "session-1", SupervisorID: "supervisor-test", LeaseOwner: "worker-1", LeaseVersion: 1, AuthorizationVersion: 1,
		EngineVersion: cloudDSHEngineVersion, ProfileVersion: cloudDSHProfileVersion}
	launch := CloudDSHLaunch{NodePath: "/usr/local/bin/node", ScriptPath: "/opt/morde-cloud-runtime/bin.mjs",
		Environment: []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "NODE_ENV=production"}, Fence: fence,
		Nonce: bytes.Repeat([]byte{8}, 32), ExpiresAt: time.Now().Add(10 * time.Second)}
	return supervisor, engine, guardian, launch, key
}

func TestDockerCloudDSHIsolationAttachAndVerifiedStop(t *testing.T) {
	supervisor, engine, guardian, launch, key := newDockerDSHFixture(t)
	input, output, err := supervisor.Start(context.Background(), launch)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close(); _ = output.Close() }()
	if _, err := input.Write([]byte("dsh-input\n")); err != nil {
		t.Fatal(err)
	}
	if got := <-engine.input; got != "dsh-input\n" {
		t.Fatalf("attach input = %q", got)
	}
	got, err := bufio.NewReader(output).ReadString('\n')
	if err != nil || got != "dsh-output\n" {
		t.Fatalf("attach output = %q, %v", got, err)
	}
	engine.mu.Lock()
	config := engine.created
	encoded, _ := json.Marshal(config)
	engine.mu.Unlock()
	host := config["HostConfig"].(map[string]any)
	if host["NetworkMode"] != "none" || host["ReadonlyRootfs"] != true || host["Privileged"] != false || host["Memory"].(float64) <= 0 || host["NanoCpus"].(float64) <= 0 || host["PidsLimit"].(float64) <= 0 {
		t.Fatalf("container isolation config = %#v", host)
	}
	if config["User"] != dockerDSHUser || !strings.Contains(string(encoded), "no-new-privileges:true") || !strings.Contains(string(encoded), `"ALL"`) || bytes.Contains(encoded, key) || bytes.Contains(encoded, launch.Nonce) {
		t.Fatal("container config violated identity or secret isolation")
	}
	labels := config["Labels"].(map[string]any)
	fenceJSON, _ := json.Marshal(launch.Fence)
	nonceHash := sha256.Sum256(launch.Nonce)
	if labels[dockerDSHFenceLabel] != string(fenceJSON) || labels[dockerDSHNonceLabel] != fmt.Sprintf("%x", nonceHash) ||
		!strings.HasPrefix(engine.name, "morde-dsh-") || !strings.Contains(string(encoded), `"-i"`) {
		t.Fatal("container does not bind its fence, nonce digest and clean environment")
	}
	updatedExpiry := time.Now().Add(15 * time.Second)
	if err := supervisor.Renew(context.Background(), launch.Fence, updatedExpiry); err != nil {
		t.Fatal(err)
	}
	receipt, err := supervisor.Stop(context.Background(), launch.Fence, launch.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := app.AgentStopReceiptSigningPayload(receipt)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	if !receipt.TreeStopped || !hmac.Equal(receipt.Signature, mac.Sum(nil)) || guardian.confirmations != 1 || guardian.renewals != 1 {
		t.Fatal("stop receipt, guard confirmation, or renewal is invalid")
	}
	store := &cloudDSHTestInstanceStore{}
	store.active, store.generation = launch.Fence, launch.Fence.AgentGeneration
	auth, _ := app.NewAgentStopAuthenticator("supervisor-test", key)
	service, _ := app.NewAgentInstanceService(store, auth)
	if _, err := service.ConfirmStopped(context.Background(), receipt); err != nil || store.stopped != 1 {
		t.Fatalf("receipt verification = %v", err)
	}
	supervisor.AgentStopConfirmed(launch.Fence, launch.Nonce)
	supervisor.mu.Lock()
	active := len(supervisor.active)
	supervisor.mu.Unlock()
	if active != 0 {
		t.Fatalf("confirmed Docker supervisor state retained: %d", active)
	}
}

func TestDockerCloudDSHRejectsForgedIdentityAndNonce(t *testing.T) {
	supervisor, engine, _, launch, _ := newDockerDSHFixture(t)
	in, out, err := supervisor.Start(context.Background(), launch)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close(); _ = out.Close() }()
	badNonce := bytes.Repeat([]byte{9}, 32)
	if receipt, err := supervisor.Stop(context.Background(), launch.Fence, badNonce); err == nil || len(receipt.Signature) != 0 {
		t.Fatal("forged nonce was accepted")
	}
	engine.mu.Lock()
	engine.badLabel = true
	engine.mu.Unlock()
	if receipt, err := supervisor.Stop(context.Background(), launch.Fence, launch.Nonce); err == nil || len(receipt.Signature) != 0 {
		t.Fatal("forged container label was accepted")
	}
	engine.mu.Lock()
	kills := engine.kills
	engine.mu.Unlock()
	if kills != 0 {
		t.Fatal("forged identity affected a container")
	}
}

func TestDockerCloudDSHDoesNotSignWithoutStopAndNamespaceProof(t *testing.T) {
	for _, scenario := range []string{"wait_timeout", "not_removed", "namespace_unknown", "missing_pid"} {
		t.Run(scenario, func(t *testing.T) {
			supervisor, engine, guardian, launch, _ := newDockerDSHFixture(t)
			in, out, err := supervisor.Start(context.Background(), launch)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = in.Close(); _ = out.Close() }()
			engine.mu.Lock()
			engine.waitHang = scenario == "wait_timeout"
			engine.keepAfterDelete = scenario == "not_removed"
			engine.missingPID = scenario == "missing_pid"
			engine.mu.Unlock()
			guardian.mu.Lock()
			guardian.destroyed = scenario != "namespace_unknown"
			guardian.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
			defer cancel()
			receipt, err := supervisor.Stop(ctx, launch.Fence, launch.Nonce)
			if err == nil || len(receipt.Signature) != 0 || receipt.TreeStopped {
				t.Fatalf("unsafe stop receipt: %#v, %v", receipt, err)
			}
			guardian.mu.Lock()
			confirmations := guardian.confirmations
			guardian.mu.Unlock()
			if scenario != "namespace_unknown" && confirmations != 0 {
				t.Fatal("guardian was consulted before container removal was confirmed")
			}
		})
	}
}

func TestDockerCloudDSHRejectsUnsafeConfiguration(t *testing.T) {
	if _, err := NewDockerCloudDSHSupervisor(DockerCloudDSHSupervisorOptions{}); err == nil {
		t.Fatal("missing guard and digest accepted")
	}
	supervisor, engine, _, launch, _ := newDockerDSHFixture(t)
	engine.mu.Lock()
	engine.unsafeImageEnv = true
	engine.mu.Unlock()
	if _, _, err := supervisor.Start(context.Background(), launch); err == nil {
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

func TestDockerCloudDSHStartFailureBeforeRunIsSafelyStopped(t *testing.T) {
	supervisor, engine, guardian, launch, _ := newDockerDSHFixture(t)
	guardian.mu.Lock()
	guardian.armError = true
	guardian.mu.Unlock()
	if _, _, err := supervisor.Start(context.Background(), launch); err == nil {
		t.Fatal("container started without an independent expiry guard")
	}
	engine.mu.Lock()
	running := engine.running
	engine.mu.Unlock()
	if running {
		t.Fatal("container is running without a guard")
	}
	receipt, err := supervisor.Stop(context.Background(), launch.Fence, launch.Nonce)
	if err != nil || !receipt.TreeStopped || len(receipt.Signature) != sha256.Size {
		t.Fatalf("never-started process did not produce a verified stop receipt: %#v, %v", receipt, err)
	}
	guardian.mu.Lock()
	confirmations := guardian.confirmations
	guardian.mu.Unlock()
	if confirmations != 0 {
		t.Fatal("guardian was asked to prove a process that was never started")
	}
	supervisor.AgentStopConfirmed(launch.Fence, launch.Nonce)
	supervisor.mu.Lock()
	active := len(supervisor.active)
	supervisor.mu.Unlock()
	if active != 0 {
		t.Fatalf("confirmed failed startup state retained: %d", active)
	}
}

func TestDockerCloudDSHImageValidationFailureHasNoProcessToStop(t *testing.T) {
	supervisor, engine, _, launch, _ := newDockerDSHFixture(t)
	engine.mu.Lock()
	engine.unsafeImageEnv = true
	engine.mu.Unlock()
	if _, _, err := supervisor.Start(context.Background(), launch); err == nil {
		t.Fatal("unsafe image passed validation")
	}
	receipt, err := supervisor.Stop(context.Background(), launch.Fence, launch.Nonce)
	if err != nil || !receipt.TreeStopped || len(receipt.Signature) != sha256.Size {
		t.Fatalf("failed pre-create startup was not safely stopped: %#v, %v", receipt, err)
	}
	engine.mu.Lock()
	creates := engine.creates
	engine.mu.Unlock()
	if creates != 0 {
		t.Fatalf("unsafe image created %d containers", creates)
	}
}

func TestDockerCloudDSHRejectsExpiredLeaseRenewal(t *testing.T) {
	supervisor, _, guardian, launch, _ := newDockerDSHFixture(t)
	launch.ExpiresAt = time.Now().Add(300 * time.Millisecond)
	in, out, err := supervisor.Start(context.Background(), launch)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close(); _ = out.Close() }()
	time.Sleep(time.Until(launch.ExpiresAt) + 20*time.Millisecond)
	if err := supervisor.Renew(context.Background(), launch.Fence, time.Now().Add(5*time.Second)); err == nil {
		t.Fatal("expired lease was renewed")
	}
	guardian.mu.Lock()
	renewals := guardian.renewals
	guardian.mu.Unlock()
	if renewals != 0 {
		t.Fatal("expired lease was sent to the guardian")
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
