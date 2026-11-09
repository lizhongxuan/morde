package dshguardian

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
)

func TestTaskSandboxGuardianRegistersVerifiesAndReplaysDedicatedProof(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	guard := testTaskSandboxGuard(now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	docker := fakeTaskSandboxDocker(guard)
	processes := &fakeProcessInspector{proof: taskSandboxProcessProof(guard), gone: true}
	service := newTaskSandboxTestService(store, docker, processes, now)
	if err := service.RegisterTaskSandbox(context.Background(), guard); err != nil {
		t.Fatalf("register Task Sandbox: %v", err)
	}
	leasePath := filepath.Join(store.dir, "task-sandbox-"+taskSandboxLeaseKey(guard.LifecycleID)+".json")
	if info, err := os.Stat(leasePath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("Task Sandbox lease is not durably private: info=%v err=%v", info, err)
	}
	if _, err := service.StopTaskSandbox(context.Background(), taskSandboxStopRequest(t, guard)); err == nil {
		t.Fatal("Guardian signed a stop proof while the OpenSandbox container still existed")
	}
	if processes.killTreeCalls != 0 {
		t.Fatal("normal stop attempted process cleanup before the provider removed its container")
	}
	docker.exists = false // OpenSandbox's delete completed; Guardian performs independent process verification.
	request := taskSandboxStopRequest(t, guard)
	attestation, err := service.StopTaskSandbox(context.Background(), request)
	if err != nil {
		t.Fatalf("confirm Task Sandbox stop: %v", err)
	}
	if !attestation.Destroyed || attestation.LifecycleID != guard.LifecycleID || attestation.TaskID != guard.TaskID ||
		attestation.OwnerID != guard.OwnerID || attestation.SandboxID != guard.SandboxID ||
		attestation.ContainerID != guard.ContainerID || attestation.NonceSHA256 == "" || len(attestation.Signature) != ed25519.SignatureSize ||
		processes.killTreeCalls != 1 {
		t.Fatalf("Task Sandbox stop proof omitted identity or process verification: proof=%+v kills=%d", attestation, processes.killTreeCalls)
	}
	publicKey := service.taskSandboxSigningKey.Public().(ed25519.PublicKey)
	authenticator, err := app.NewTaskSandboxGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authenticator.VerifyTaskSandboxStop(context.Background(), attestation); err != nil {
		t.Fatalf("verify dedicated Task Sandbox proof: %v", err)
	}
	sharedKeyAuthenticator, err := app.NewTaskSandboxGuardianStopAuthenticator(service.signingKey.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sharedKeyAuthenticator.VerifyTaskSandboxStop(context.Background(), attestation); err == nil {
		t.Fatal("DSH Guardian key verified a Task Sandbox proof")
	}
	persisted, err := os.ReadFile(leasePath)
	if err != nil {
		t.Fatalf("Guardian proof receipt was not persisted: %v", err)
	}
	if bytes.Contains(persisted, []byte(base64.StdEncoding.EncodeToString(request.Nonce))) {
		t.Fatal("raw stop challenge was persisted instead of its digest")
	}

	reopened, err := openLeaseStore(store.dir)
	if err != nil {
		t.Fatalf("reopen Guardian Task Sandbox state: %v", err)
	}
	restarted := newTaskSandboxTestService(reopened, docker, processes, now)
	replayed, err := restarted.StopTaskSandbox(context.Background(), request)
	if err != nil || replayed.ProofSHA256 != attestation.ProofSHA256 || string(replayed.Signature) != string(attestation.Signature) {
		t.Fatalf("Task Sandbox proof changed after Guardian restart: proof=%+v err=%v", replayed, err)
	}
	rotated := taskSandboxStopRequest(t, guard)
	rotatedProof, err := restarted.StopTaskSandbox(context.Background(), rotated)
	if err != nil || rotatedProof.NonceSHA256 == attestation.NonceSHA256 || string(rotatedProof.Signature) == string(attestation.Signature) {
		t.Fatalf("fresh challenge did not rotate the durable proof: proof=%+v err=%v", rotatedProof, err)
	}
}

func TestTaskSandboxGuardianRejectsContainerAlreadyBoundToAnotherLifecycle(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	first := testTaskSandboxGuard(now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	docker := fakeTaskSandboxDocker(first)
	processes := &fakeProcessInspector{proof: taskSandboxProcessProof(first), gone: true}
	service := newTaskSandboxTestService(store, docker, processes, now)
	if err := service.RegisterTaskSandbox(context.Background(), first); err != nil {
		t.Fatalf("register first Task Sandbox: %v", err)
	}

	second := first
	second.LifecycleID = "o7654321"
	second.TaskID = "q7654321"
	if err := service.RegisterTaskSandbox(context.Background(), second); err == nil {
		t.Fatal("Guardian bound one Docker container to two Task lifecycles")
	}
	if len(store.allTaskSandboxes()) != 1 {
		t.Fatalf("duplicate Task Sandbox registration changed leases: %+v", store.allTaskSandboxes())
	}
}

func TestTaskSandboxGuardianDiscoversOnlyOneExactOpenSandboxContainer(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	full := testTaskSandboxGuard(now)
	request := full
	request.ContainerID = ""
	request.ContainerName = ""

	t.Run("unique exact match is registered and returned", func(t *testing.T) {
		store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
		if err != nil {
			t.Fatal(err)
		}
		docker := fakeTaskSandboxDocker(full)
		processes := &fakeProcessInspector{proof: taskSandboxProcessProof(full), gone: true}
		service := newTaskSandboxTestService(store, docker, processes, now)
		resolved, err := service.RegisterDiscoveredTaskSandbox(context.Background(), request)
		if err != nil || resolved != full {
			t.Fatalf("register discovered Task Sandbox: resolved=%+v err=%v", resolved, err)
		}
		if values := store.allTaskSandboxes(); len(values) != 1 || values[0].Guard != full {
			t.Fatalf("Guardian did not persist the exact resolved identity: %+v", values)
		}
	})

	t.Run("lifecycle-only stop lookup survives Guardian restart", func(t *testing.T) {
		store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
		if err != nil {
			t.Fatal(err)
		}
		docker := fakeTaskSandboxDocker(full)
		processes := &fakeProcessInspector{proof: taskSandboxProcessProof(full), gone: true}
		service := newTaskSandboxTestService(store, docker, processes, now)
		if _, err := service.RegisterDiscoveredTaskSandbox(context.Background(), request); err != nil {
			t.Fatalf("register discovered Task Sandbox: %v", err)
		}
		restartedStore, err := openLeaseStore(store.dir)
		if err != nil {
			t.Fatalf("reopen Guardian state: %v", err)
		}
		docker.exists = false
		restarted := newTaskSandboxTestService(restartedStore, docker, processes, now)
		stop := taskSandboxStopRequest(t, full)
		attestation, err := restarted.StopTaskSandbox(context.Background(), TaskSandboxStopRequest{
			Guard: request, Nonce: stop.Nonce,
		})
		if err != nil || attestation.ContainerID != full.ContainerID || !attestation.Destroyed {
			t.Fatalf("stop from persisted Guardian identity: proof=%+v err=%v", attestation, err)
		}
	})

	t.Run("zero matches fail closed", func(t *testing.T) {
		store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
		if err != nil {
			t.Fatal(err)
		}
		docker := fakeTaskSandboxDocker(full)
		docker.exists = false
		service := newTaskSandboxTestService(store, docker, &fakeProcessInspector{gone: true}, now)
		if _, err := service.RegisterDiscoveredTaskSandbox(context.Background(), request); err == nil {
			t.Fatal("Guardian accepted a Task Sandbox with no matching container")
		}
		if len(store.allTaskSandboxes()) != 0 {
			t.Fatal("failed discovery persisted a Guardian lease")
		}
	})

	t.Run("ambiguous matches fail closed", func(t *testing.T) {
		store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
		if err != nil {
			t.Fatal(err)
		}
		docker := fakeTaskSandboxDocker(full)
		docker.listResults = []containerSummary{
			{ID: full.ContainerID, Names: []string{"/" + full.ContainerName}, Labels: map[string]string{openSandboxContainerIDLabel: full.SandboxID}},
			{ID: strings.Repeat("d", 64), Names: []string{"/" + full.ContainerName}, Labels: map[string]string{openSandboxContainerIDLabel: full.SandboxID}},
		}
		service := newTaskSandboxTestService(store, docker, &fakeProcessInspector{proof: taskSandboxProcessProof(full), gone: true}, now)
		if _, err := service.RegisterDiscoveredTaskSandbox(context.Background(), request); err == nil {
			t.Fatal("Guardian accepted ambiguous OpenSandbox container labels")
		}
		if len(store.allTaskSandboxes()) != 0 {
			t.Fatal("ambiguous discovery persisted a Guardian lease")
		}
	})

	t.Run("wrong container name fails closed", func(t *testing.T) {
		store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
		if err != nil {
			t.Fatal(err)
		}
		docker := fakeTaskSandboxDocker(full)
		docker.listResults = []containerSummary{{ID: full.ContainerID, Names: []string{"/other-container"}, Labels: map[string]string{openSandboxContainerIDLabel: full.SandboxID}}}
		service := newTaskSandboxTestService(store, docker, &fakeProcessInspector{proof: taskSandboxProcessProof(full), gone: true}, now)
		if _, err := service.RegisterDiscoveredTaskSandbox(context.Background(), request); err == nil {
			t.Fatal("Guardian accepted a container name that did not match the OpenSandbox ID")
		}
	})
}

func TestTaskSandboxGuardianRejectsWrongContainerAndExpiredLeaseStopsIt(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	guard := testTaskSandboxGuard(now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	docker := fakeTaskSandboxDocker(guard)
	processes := &fakeProcessInspector{proof: taskSandboxProcessProof(guard), gone: true}
	service := newTaskSandboxTestService(store, docker, processes, now)

	for name, mutate := range map[string]func(map[string]string){
		"OpenSandbox ID":      func(labels map[string]string) { labels[openSandboxContainerIDLabel] = "sandbox-other" },
		"owner metadata":      func(labels map[string]string) { labels[appTaskSandboxMetadataOwnerID] = "u7654321" },
		"task metadata":       func(labels map[string]string) { labels[appTaskSandboxMetadataTaskID] = "q7654321" },
		"lifecycle metadata":  func(labels map[string]string) { labels[appTaskSandboxMetadataLifecycle] = "o7654321" },
		"generation metadata": func(labels map[string]string) { labels[appTaskSandboxMetadataGeneration] = "3" },
	} {
		t.Run(name, func(t *testing.T) {
			badStore, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
			if err != nil {
				t.Fatal(err)
			}
			badDocker := fakeTaskSandboxDocker(guard)
			mutate(badDocker.value.Config.Labels)
			badService := newTaskSandboxTestService(badStore, badDocker,
				&fakeProcessInspector{proof: taskSandboxProcessProof(guard), gone: true}, now)
			if err := badService.RegisterTaskSandbox(context.Background(), guard); err == nil {
				t.Fatal("Guardian registered a container with mismatched lifecycle identity labels")
			}
			if len(badStore.allTaskSandboxes()) != 0 {
				t.Fatal("mismatched container created a durable Guardian lease")
			}
		})
	}
	docker = fakeTaskSandboxDocker(guard)
	service.docker = docker
	if err := service.RegisterTaskSandbox(context.Background(), guard); err != nil {
		t.Fatalf("register valid Task Sandbox: %v", err)
	}
	now = guard.ExpiresAt.Add(time.Millisecond)
	service.now = func() time.Time { return now }
	service.sweep(context.Background(), nil)
	value, ok := store.getTaskSandbox(taskSandboxLeaseKey(guard.LifecycleID))
	if !ok || !value.Destroyed || value.UnprovenRunning || docker.exists || docker.killCalls != 1 || docker.removeCalls != 1 || !processes.gone {
		t.Fatalf("expired Task Sandbox was not destroyed and independently verified: value=%+v found=%t docker=%+v process=%+v", value, ok, docker, processes)
	}
	proof, err := service.StopTaskSandbox(context.Background(), taskSandboxStopRequest(t, guard))
	if err != nil || !proof.Destroyed || proof.NonceSHA256 == "" {
		t.Fatalf("expired stop did not return the challenge-bound proof: proof=%+v err=%v", proof, err)
	}
}

func TestTaskSandboxGuardianDoesNotDestroyExpiredContainerWithChangedLifecycleLabels(t *testing.T) {
	for name, mutate := range map[string]func(map[string]string){
		"owner metadata":      func(labels map[string]string) { labels[appTaskSandboxMetadataOwnerID] = "u7654321" },
		"task metadata":       func(labels map[string]string) { labels[appTaskSandboxMetadataTaskID] = "q7654321" },
		"lifecycle metadata":  func(labels map[string]string) { labels[appTaskSandboxMetadataLifecycle] = "o7654321" },
		"generation metadata": func(labels map[string]string) { labels[appTaskSandboxMetadataGeneration] = "3" },
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			guard := testTaskSandboxGuard(now)
			store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
			if err != nil {
				t.Fatal(err)
			}
			docker := fakeTaskSandboxDocker(guard)
			processes := &fakeProcessInspector{proof: taskSandboxProcessProof(guard), gone: true}
			service := newTaskSandboxTestService(store, docker, processes, now)
			if err := service.RegisterTaskSandbox(context.Background(), guard); err != nil {
				t.Fatalf("register valid Task Sandbox: %v", err)
			}

			mutate(docker.value.Config.Labels)
			now = guard.ExpiresAt.Add(time.Millisecond)
			service.now = func() time.Time { return now }
			var sweepErr error
			service.sweep(context.Background(), func(err error) { sweepErr = err })

			value, ok := store.getTaskSandbox(taskSandboxLeaseKey(guard.LifecycleID))
			if sweepErr == nil || !ok || value.Destroyed || !docker.exists || docker.killCalls != 0 || docker.removeCalls != 0 || processes.killTreeCalls != 0 {
				t.Fatalf("Guardian acted on an expired container whose lifecycle labels changed: err=%v lease=%+v found=%t docker=%+v process=%+v",
					sweepErr, value, ok, docker, processes)
			}
		})
	}
}

func TestTaskSandboxGuardianRejectsInvalidOrSharedSigningKey(t *testing.T) {
	key := testAttestationPrivateKey()
	if _, err := OpenWithTaskSandboxGuardianKey("/unused", "/unused", key, key); err == nil {
		t.Fatal("Task Sandbox Guardian accepted a key shared with DSH/account Runtime proofs")
	}
	if _, err := OpenWithTaskSandboxGuardianKey("/unused", "/unused", key, ed25519.PrivateKey(strings.Repeat("x", ed25519.PrivateKeySize))); err == nil {
		t.Fatal("Task Sandbox Guardian accepted a malformed signing key")
	}
}

func TestTaskSandboxGuardianHTTPRoutesFailClosedWithoutDedicatedKey(t *testing.T) {
	now := time.Now().UTC()
	guard := testTaskSandboxGuard(now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	service := newService(store, fakeTaskSandboxDocker(guard), &fakeProcessInspector{proof: taskSandboxProcessProof(guard), gone: true}, func() time.Time { return now })
	request := taskSandboxStopRequest(t, guard)
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest("POST", "/v1/task-sandboxes/stop", strings.NewReader(string(body))))
	if response.Code != 409 {
		t.Fatalf("unconfigured Task Sandbox Guardian stop route status = %d, want 409", response.Code)
	}
}

func TestTaskSandboxGuardianHTTPRegistrationAndStopReturnSignedProof(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	guard := testTaskSandboxGuard(now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	docker := fakeTaskSandboxDocker(guard)
	processes := &fakeProcessInspector{proof: taskSandboxProcessProof(guard), gone: true}
	service := newTaskSandboxTestService(store, docker, processes, now)
	handler := service.Handler()

	registerBody, err := json.Marshal(guard)
	if err != nil {
		t.Fatal(err)
	}
	registerResponse := httptest.NewRecorder()
	handler.ServeHTTP(registerResponse, httptest.NewRequest("POST", "/v1/task-sandboxes", strings.NewReader(string(registerBody))))
	if registerResponse.Code != 204 {
		t.Fatalf("Task Sandbox registration status = %d, body=%s", registerResponse.Code, registerResponse.Body.String())
	}
	docker.exists = false
	stopBody, err := json.Marshal(taskSandboxStopRequest(t, guard))
	if err != nil {
		t.Fatal(err)
	}
	stopResponse := httptest.NewRecorder()
	handler.ServeHTTP(stopResponse, httptest.NewRequest("POST", "/v1/task-sandboxes/stop", strings.NewReader(string(stopBody))))
	if stopResponse.Code != 200 {
		t.Fatalf("Task Sandbox stop status = %d, body=%s", stopResponse.Code, stopResponse.Body.String())
	}
	var attestation app.TaskSandboxGuardianStopAttestation
	if err := json.Unmarshal(stopResponse.Body.Bytes(), &attestation); err != nil || !attestation.Destroyed || len(attestation.Signature) != ed25519.SignatureSize {
		t.Fatalf("Task Sandbox stop response is not a signed destruction attestation: proof=%+v err=%v", attestation, err)
	}
}

func TestTaskSandboxGuardianUnixSocketClientRegistrationAndStop(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	guard := testTaskSandboxGuard(now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	docker := fakeTaskSandboxDocker(guard)
	processes := &fakeProcessInspector{proof: taskSandboxProcessProof(guard), gone: true}
	service := newTaskSandboxTestService(store, docker, processes, now)
	dir, err := os.MkdirTemp("/tmp", "task-sandbox-guardian-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	socket := filepath.Join(dir, "guardian.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- service.Serve(ctx, socket, 0o600, time.Hour, nil) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			select {
			case serveErr := <-serveDone:
				t.Fatalf("Task Sandbox Guardian did not create its Unix socket: %v", serveErr)
			default:
				t.Fatal("Task Sandbox Guardian did not create its Unix socket")
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	client, err := NewClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	discovery := guard
	discovery.ContainerID = ""
	discovery.ContainerName = ""
	resolved, err := client.RegisterDiscoveredTaskSandbox(context.Background(), discovery)
	if err != nil || resolved != guard {
		t.Fatalf("register discovered identity through Guardian client: resolved=%+v err=%v", resolved, err)
	}
	request := taskSandboxStopRequest(t, guard)
	lookup := guard
	lookup.ContainerID = ""
	lookup.ContainerName = ""
	if _, err := client.StopTaskSandbox(context.Background(), lookup, request.Nonce); err == nil {
		t.Fatal("Guardian client received a signed stop proof before the provider deleted its container")
	}
	docker.exists = false
	attestation, err := client.StopTaskSandbox(context.Background(), lookup, request.Nonce)
	if err != nil || !attestation.Destroyed || attestation.LifecycleID != guard.LifecycleID {
		t.Fatalf("stop through Guardian client: proof=%+v err=%v", attestation, err)
	}
	authenticator, err := app.NewTaskSandboxGuardianStopAuthenticator(service.taskSandboxSigningKey.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authenticator.VerifyTaskSandboxStop(context.Background(), attestation); err != nil {
		t.Fatalf("verify proof returned by Guardian client: %v", err)
	}
	cancel()
	if err := <-serveDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Guardian shutdown error = %v", err)
	}
}

func TestTaskSandboxGuardianDoesNotAttestWhileProcessTreeRemains(t *testing.T) {
	now := time.Now().UTC()
	guard := testTaskSandboxGuard(now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	docker := fakeTaskSandboxDocker(guard)
	processes := &fakeProcessInspector{proof: taskSandboxProcessProof(guard), gone: false}
	service := newTaskSandboxTestService(store, docker, processes, now)
	if err := service.RegisterTaskSandbox(context.Background(), guard); err != nil {
		t.Fatalf("register Task Sandbox: %v", err)
	}
	docker.exists = false
	request := taskSandboxStopRequest(t, guard)
	if _, err := service.StopTaskSandbox(context.Background(), request); err == nil {
		t.Fatal("Guardian signed a stop proof while the independent process tree remained")
	}
	value, ok := store.getTaskSandbox(taskSandboxLeaseKey(guard.LifecycleID))
	if !ok || value.Destroyed || !value.UnprovenRunning {
		t.Fatalf("failed process verification did not persist uncertainty: value=%+v found=%t", value, ok)
	}
	processes.gone = true
	proof, err := service.StopTaskSandbox(context.Background(), request)
	if err != nil || !proof.Destroyed {
		t.Fatalf("Guardian did not recover after process exit was independently observed: proof=%+v err=%v", proof, err)
	}
}

func testTaskSandboxGuard(now time.Time) TaskSandboxGuard {
	return TaskSandboxGuard{
		LifecycleID: "o1234567", TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567",
		LeaseOwner: "task-worker-1", LeaseVersion: 3, AuthorizationVersion: 5, AgentInstanceID: "agent-session-1",
		AgentGeneration: 7, SandboxID: "sbx_123456", SandboxGeneration: 2, ContainerID: strings.Repeat("c", 64),
		ContainerName: "sandbox-sbx_123456", ExpiresAt: now.Add(5 * time.Second),
	}
}

func taskSandboxProcessProof(guard TaskSandboxGuard) processProof {
	return processProof{PID: 321, StartTimeTick: 88, CgroupPath: "/system.slice/docker-" + guard.ContainerID + ".scope"}
}

func fakeTaskSandboxDocker(guard TaskSandboxGuard) *fakeDocker {
	running, pid := true, 321
	metadata, _ := guard.lifecycleMetadata()
	metadata[openSandboxContainerIDLabel] = guard.SandboxID
	return &fakeDocker{
		exists: true,
		value: containerInspect{
			ID: guard.ContainerID, Name: "/" + guard.ContainerName,
			Config: struct {
				Labels map[string]string `json:"Labels"`
			}{Labels: metadata},
			State: struct {
				Running *bool `json:"Running"`
				PID     *int  `json:"Pid"`
			}{Running: &running, PID: &pid},
		},
	}
}

const (
	appTaskSandboxMetadataOwnerID    = "morde.owner-id"
	appTaskSandboxMetadataTaskID     = "morde.task-id"
	appTaskSandboxMetadataLifecycle  = "morde.task-lifecycle-id"
	appTaskSandboxMetadataGeneration = "morde.sandbox-generation"
)

func newTaskSandboxTestService(store *leaseStore, docker dockerAPI, processes processInspector, now time.Time) *Service {
	service := newService(store, docker, processes, func() time.Time { return now })
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 0x5a
	service.taskSandboxSigningKey = ed25519.NewKeyFromSeed(seed)
	return service
}

func taskSandboxStopRequest(t *testing.T, guard TaskSandboxGuard) TaskSandboxStopRequest {
	t.Helper()
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	return TaskSandboxStopRequest{Guard: guard, Nonce: nonce}
}
