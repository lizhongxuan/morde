package worker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"morde/server/internal/app"
)

func TestUnixSocketGuardianRecoveryUsesExactFenceAndNonceDigest(t *testing.T) {
	fence := app.AgentInstanceFence{TaskID: "q1234567", OwnerID: "u1234567", AgentInstanceID: "agent-1", AgentGeneration: 3,
		AgentSessionID: "session-1", SupervisorID: "supervisor-1", LeaseOwner: "worker-1", LeaseVersion: 7,
		AuthorizationVersion: 2, EngineVersion: "engine-v1", ProfileVersion: "profile-v1"}
	attestation := app.AgentGuardianStopAttestation{Fence: fence, ContainerID: "container", NonceSHA256: "nonce-digest", Destroyed: true, DestroyedAt: time.Now().UTC()}
	requestSeen := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/guards/recover-stop" {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Fence       app.AgentInstanceFence `json:"fence"`
			NonceSHA256 string                 `json:"nonce_sha256"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Fence != fence || request.NonceSHA256 != attestation.NonceSHA256 {
			http.Error(w, "wrong recovery identity", http.StatusBadRequest)
			return
		}
		requestSeen = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(attestation)
	})
	dir, err := os.MkdirTemp("/tmp", "dg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "guardian.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
		<-serveDone
	})

	guardian, err := NewUnixSocketDockerCloudDSHGuardian(socket)
	if err != nil {
		t.Fatal(err)
	}
	recovery, ok := guardian.(DockerCloudDSHGuardianRecovery)
	if !ok {
		t.Fatal("Unix Guardian client does not expose recovery")
	}
	got, err := recovery.RecoverStopped(context.Background(), fence, attestation.NonceSHA256)
	if err != nil || !requestSeen || got.Fence != fence || got.NonceSHA256 != attestation.NonceSHA256 || !got.Destroyed {
		t.Fatalf("Guardian recovery = %+v, requestSeen=%t err=%v", got, requestSeen, err)
	}
}
