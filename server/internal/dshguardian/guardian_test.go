package dshguardian

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
)

func testAttestationPrivateKey() ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 0xa5
	return ed25519.NewKeyFromSeed(seed)
}

func newService(store *leaseStore, docker dockerAPI, processes processInspector, now func() time.Time) *Service {
	return &Service{store: store, docker: docker, processes: processes, signingKey: testAttestationPrivateKey(), now: now}
}

func TestArmPersistsLeaseAndRejectsMismatchedLabels(t *testing.T) {
	now := time.Now().UTC()
	guard := testGuard(now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	docker := newFakeDocker(guard, false, 0)
	service := newService(store, docker, &fakeProcessInspector{}, func() time.Time { return now })
	if err := service.Arm(context.Background(), guard); err != nil {
		t.Fatalf("arm lease: %v", err)
	}
	if err := service.Arm(context.Background(), guard); err != nil {
		t.Fatalf("idempotent arm: %v", err)
	}
	reopened, err := openLeaseStore(store.dir)
	if err != nil {
		t.Fatalf("reopen durable lease store: %v", err)
	}
	if value, ok := reopened.get(guard.ContainerID); !ok || value.Guard != guard {
		t.Fatal("arm did not durably persist the exact guard")
	}

	badStore, err := openLeaseStore(filepath.Join(t.TempDir(), "bad-state"))
	if err != nil {
		t.Fatal(err)
	}
	badDocker := newFakeDocker(guard, false, 0)
	badDocker.value.Config.Labels["morde.dsh.nonce-sha256"] = strings.Repeat("0", 64)
	badService := newService(badStore, badDocker, &fakeProcessInspector{}, func() time.Time { return now })
	if err := badService.Arm(context.Background(), guard); err == nil {
		t.Fatal("arm accepted a container with a mismatched nonce label")
	}
	if _, ok := badStore.get(guard.ContainerID); ok {
		t.Fatal("mismatched container was persisted")
	}
}

func TestExpiredLeaseKillsContainerAndRequiresIndependentProof(t *testing.T) {
	now := time.Now().UTC()
	guard := testGuard(now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	processes := &fakeProcessInspector{proof: processProof{
		PID: 321, StartTimeTick: 88, CgroupPath: "/system.slice/docker-" + guard.ContainerID + ".scope",
	}, gone: true}
	docker := newFakeDocker(guard, false, 0)
	service := newService(store, docker, processes, func() time.Time { return now })
	if err := service.Arm(context.Background(), guard); err != nil {
		t.Fatal(err)
	}
	docker.setRunning(321)
	if err := service.Started(context.Background(), guard, 321); err != nil {
		t.Fatalf("record started process: %v", err)
	}
	now = guard.ExpiresAt.Add(time.Millisecond)
	var reported []error
	service.sweep(context.Background(), func(err error) { reported = append(reported, err) })
	if len(reported) != 0 {
		t.Fatalf("expiry sweep reported errors: %v", reported)
	}
	if docker.killCalls != 1 || docker.removeCalls != 1 || docker.exists || processes.killTreeCalls != 1 {
		t.Fatalf("expected exact container and cgroup kill/removal, got kill=%d remove=%d exists=%v cgroup_kill=%d", docker.killCalls, docker.removeCalls, docker.exists, processes.killTreeCalls)
	}
	value, ok := store.get(guard.ContainerID)
	if !ok || !value.Destroyed {
		t.Fatal("successful cgroup/PID proof was not persisted")
	}
	destroyed, err := service.ConfirmDestroyed(context.Background(), guard)
	if err != nil || !destroyed {
		t.Fatalf("confirm destroyed: destroyed=%v err=%v", destroyed, err)
	}

	processes.gone = false
	if _, err := service.ConfirmDestroyed(context.Background(), guard); err == nil {
		t.Fatal("confirmation accepted a still-present process tree")
	}
}

func TestRecoverStoppedReopensDurableLeaseAndSignsExactDestructionProof(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	guard := testGuard(now)
	dir := filepath.Join(t.TempDir(), "state")
	store, err := openLeaseStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	docker := newFakeDocker(guard, false, 0)
	processes := &fakeProcessInspector{proof: processProof{
		PID: 321, StartTimeTick: 55, CgroupPath: "/docker/" + guard.ContainerID,
	}, gone: true}
	first := newService(store, docker, processes, func() time.Time { return now })
	if err := first.Arm(context.Background(), guard); err != nil {
		t.Fatal(err)
	}
	docker.setRunning(321)
	if err := first.Started(context.Background(), guard, 321); err != nil {
		t.Fatal(err)
	}

	// Simulate Guardian process restart: all in-memory service/store state is new.
	reopened, err := openLeaseStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newService(reopened, docker, processes, func() time.Time { return now })
	wrongFence := guard.Fence
	wrongFence.AgentGeneration++
	if _, err := restarted.RecoverStopped(context.Background(), wrongFence, guard.NonceSHA256); err == nil {
		t.Fatal("mismatched Agent generation returned a destruction attestation")
	}
	if docker.killCalls != 0 {
		t.Fatal("mismatched recovery identity reached Docker")
	}
	attestation, err := restarted.RecoverStopped(context.Background(), guard.Fence, guard.NonceSHA256)
	if err != nil {
		t.Fatalf("recover exact Agent stop proof: %v", err)
	}
	if !attestation.Destroyed || attestation.Fence != guard.Fence || attestation.ContainerID != guard.ContainerID ||
		attestation.ContainerName != guard.ContainerName || attestation.NonceSHA256 != guard.NonceSHA256 ||
		attestation.DestroyedAt.IsZero() || len(attestation.Signature) != ed25519.SignatureSize ||
		docker.exists || docker.killCalls != 1 || docker.removeCalls != 1 || processes.killTreeCalls != 1 {
		t.Fatalf("recovered attestation or destruction proof is incomplete: attestation=%+v docker=%+v processes=%+v", attestation, docker, processes)
	}
	value, ok := reopened.get(guard.ContainerID)
	if !ok || !value.Destroyed || value.DestroyedAt.IsZero() || value.UnprovenRunning {
		t.Fatalf("Guardian proof was not durably committed: %+v", value)
	}
	publicKey := testAttestationPrivateKey().Public().(ed25519.PublicKey)
	payload, err := app.AgentGuardianStopAttestationSigningPayload(attestation)
	if err != nil || !ed25519.Verify(publicKey, payload, attestation.Signature) {
		t.Fatalf("Guardian attestation signature is invalid: err=%v", err)
	}

	// A second process restart must re-verify persisted absence and produce the same signed proof.
	secondStore, err := openLeaseStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	secondRestart := newService(secondStore, docker, processes, func() time.Time { return now })
	replayed, err := secondRestart.RecoverStopped(context.Background(), guard.Fence, guard.NonceSHA256)
	if err != nil || !bytes.Equal(attestation.Signature, replayed.Signature) || replayed.ProofSHA256 != attestation.ProofSHA256 {
		t.Fatalf("replayed Guardian proof changed: original=%+v replayed=%+v err=%v", attestation, replayed, err)
	}
}

func TestExpiredSweepCapturesProcessIfPortalNeverReportsStarted(t *testing.T) {
	now := time.Now().UTC()
	guard := testGuard(now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	processes := &fakeProcessInspector{proof: processProof{
		PID: 876, StartTimeTick: 92, CgroupPath: "/docker/" + guard.ContainerID,
	}, gone: true}
	docker := newFakeDocker(guard, false, 0)
	service := newService(store, docker, processes, func() time.Time { return now })
	if err := service.Arm(context.Background(), guard); err != nil {
		t.Fatal(err)
	}
	// Simulate Portal dying after Docker start and before its Started API call.
	docker.setRunning(876)
	now = guard.ExpiresAt.Add(time.Millisecond)
	service.sweep(context.Background(), nil)
	value, ok := store.get(guard.ContainerID)
	if !ok || !value.Started || value.Process.PID != 876 || !value.Destroyed {
		t.Fatalf("expiry did not capture and persist process proof: %+v", value)
	}
	if docker.killCalls != 1 || docker.removeCalls != 1 {
		t.Fatalf("expiry did not stop the exact container: kill=%d remove=%d", docker.killCalls, docker.removeCalls)
	}
}

func TestExpiryStopsButDoesNotClaimWhenHostProcessProofIsUnavailable(t *testing.T) {
	now := time.Now().UTC()
	guard := testGuard(now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	processes := &fakeProcessInspector{gone: true}
	docker := newFakeDocker(guard, false, 0)
	service := newService(store, docker, processes, func() time.Time { return now })
	if err := service.Arm(context.Background(), guard); err != nil {
		t.Fatal(err)
	}
	docker.setRunning(777)
	now = guard.ExpiresAt.Add(time.Millisecond)
	var reported []error
	service.sweep(context.Background(), func(err error) { reported = append(reported, err) })
	if len(reported) != 1 || docker.killCalls != 1 || docker.removeCalls != 1 {
		t.Fatalf("missing process proof did not stop the exact container and fail closed: errors=%v kill=%d remove=%d", reported, docker.killCalls, docker.removeCalls)
	}
	if value, ok := store.get(guard.ContainerID); !ok || value.Destroyed || !value.UnprovenRunning {
		t.Fatal("guardian marked an unproven process tree destroyed or lost its uncertainty marker")
	}
	if _, err := service.ConfirmDestroyed(context.Background(), guard); err == nil {
		t.Fatal("guardian issued a success confirmation without process-tree proof")
	}
}

func TestExpiryDoesNotClaimWhenCgroupTreeKillIsUnavailable(t *testing.T) {
	now := time.Now().UTC()
	guard := testGuard(now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	processes := &fakeProcessInspector{proof: processProof{
		PID: 912, StartTimeTick: 44, CgroupPath: "/docker/" + guard.ContainerID,
	}, gone: true, killTreeErr: errors.New("cgroup.kill unavailable")}
	docker := newFakeDocker(guard, false, 0)
	service := newService(store, docker, processes, func() time.Time { return now })
	if err := service.Arm(context.Background(), guard); err != nil {
		t.Fatal(err)
	}
	docker.setRunning(912)
	if err := service.Started(context.Background(), guard, 912); err != nil {
		t.Fatal(err)
	}
	now = guard.ExpiresAt.Add(time.Millisecond)
	var reported []error
	service.sweep(context.Background(), func(err error) { reported = append(reported, err) })
	value, ok := store.get(guard.ContainerID)
	if !ok || !value.UnprovenRunning || value.Destroyed || len(reported) != 1 {
		t.Fatalf("guardian did not retain unproven tree-kill state: lease=%+v errors=%v", value, reported)
	}
	if docker.killCalls != 1 || docker.removeCalls != 1 {
		t.Fatalf("guardian did not still stop the exact Docker container: kill=%d remove=%d", docker.killCalls, docker.removeCalls)
	}
}

func TestMissingContainerRetriesVerifiedProcessTreeUntilGone(t *testing.T) {
	now := time.Now().UTC()
	guard := testGuard(now)
	proof := processProof{PID: 321, StartTimeTick: 88, CgroupPath: "/system.slice/docker-" + guard.ContainerID + ".scope"}
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.create(lease{Guard: guard, Process: proof, Started: true}); err != nil {
		t.Fatal(err)
	}
	docker := newFakeDocker(guard, false, 0)
	docker.exists = false
	processes := &fakeProcessInspector{proof: proof, killTreeErr: errors.New("cgroup.kill unavailable")}
	service := newService(store, docker, processes, func() time.Time { return now })

	if destroyed, err := service.ConfirmDestroyed(context.Background(), guard); err == nil || destroyed {
		t.Fatalf("first ConfirmDestroyed() = %t, %v; want uncertain after cgroup kill failure", destroyed, err)
	}
	value, ok := store.get(guard.ContainerID)
	if !ok || !value.UnprovenRunning || value.Destroyed {
		t.Fatalf("failed cgroup kill did not persist uncertainty: %+v", value)
	}

	processes.killTreeErr = nil
	if destroyed, err := service.ConfirmDestroyed(context.Background(), guard); err == nil || destroyed {
		t.Fatalf("ConfirmDestroyed() before Gone = %t, %v; want an unconfirmed result", destroyed, err)
	}
	value, ok = store.get(guard.ContainerID)
	if !ok || value.UnprovenRunning || value.Destroyed || processes.killTreeCalls != 2 {
		t.Fatalf("successful retry was not persisted or destruction was claimed early: lease=%+v found=%t killCalls=%d", value, ok, processes.killTreeCalls)
	}

	processes.killTreeClears = true
	destroyed, err := service.ConfirmDestroyed(context.Background(), guard)
	value, ok = store.get(guard.ContainerID)
	if err != nil || !destroyed || !processes.gone || processes.killTreeCalls != 3 || !ok || !value.Destroyed {
		t.Fatalf("ConfirmDestroyed() after process exit = %t, %+v, calls=%d, err=%v", destroyed, value, processes.killTreeCalls, err)
	}
}

func TestMissingContainerDoesNotKillWithoutStartedProof(t *testing.T) {
	now := time.Now().UTC()
	guard := testGuard(now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.create(lease{Guard: guard, UnprovenRunning: true}); err != nil {
		t.Fatal(err)
	}
	docker := newFakeDocker(guard, false, 0)
	docker.exists = false
	processes := &fakeProcessInspector{}
	service := newService(store, docker, processes, func() time.Time { return now })
	if destroyed, err := service.ConfirmDestroyed(context.Background(), guard); err == nil || destroyed {
		t.Fatalf("ConfirmDestroyed() = %t, %v; want an unconfirmed result", destroyed, err)
	}
	value, ok := store.get(guard.ContainerID)
	if !ok || processes.killTreeCalls != 0 || !value.UnprovenRunning || value.Destroyed {
		t.Fatalf("missing process proof was not kept fail-closed: lease=%+v found=%t killCalls=%d", value, ok, processes.killTreeCalls)
	}
}

func TestArmRejectsLeaseOutsideWindowAndWrongContainerID(t *testing.T) {
	now := time.Now()
	guard := testGuard(now)
	guard.ExpiresAt = now.Add(app.MaxAgentLease + time.Second)
	if err := guard.validateNew(now); err == nil {
		t.Fatal("accepted a lease longer than the application maximum")
	}
	guard = testGuard(now)
	guard.ContainerID = strings.Repeat("A", 64)
	if err := guard.validateIdentity(); err == nil {
		t.Fatal("accepted an invalid Docker ID")
	}
}

func TestLoadAttestationPrivateKeyRequiresProtectedRawFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "guardian.key")
	want := testAttestationPrivateKey()
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadAttestationPrivateKey(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("load protected key: key matches=%t err=%v", bytes.Equal(got, want), err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAttestationPrivateKey(path); err == nil {
		t.Fatal("loaded a group-readable Guardian signing key")
	}
	link := filepath.Join(dir, "guardian-key-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAttestationPrivateKey(link); err == nil {
		t.Fatal("loaded a symlinked Guardian signing key")
	}
	badLength := filepath.Join(dir, "bad-length.key")
	if err := os.WriteFile(badLength, want[:len(want)-1], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAttestationPrivateKey(badLength); err == nil {
		t.Fatal("loaded a malformed Guardian signing key")
	}
	if _, err := LoadAttestationPrivateKey(filepath.Join(dir, "missing.key")); err == nil {
		t.Fatal("generated or accepted a missing Guardian signing key")
	}
	if _, err := LoadAttestationPrivateKey("relative.key"); err == nil {
		t.Fatal("accepted a relative Guardian signing-key path")
	}
}

func TestServiceRejectsInvalidOrStaleLifecycleRequests(t *testing.T) {
	now := time.Now().UTC()
	guard := testGuard(now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	processes := &fakeProcessInspector{proof: processProof{
		PID: 321, StartTimeTick: 91, CgroupPath: "/docker/" + guard.ContainerID,
	}, gone: true}
	docker := newFakeDocker(guard, false, 0)
	service := newService(store, docker, processes, func() time.Time { return now })

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.Arm(canceled, guard); err == nil {
		t.Fatal("arm accepted a canceled context")
	}
	if err := service.Started(context.Background(), guard, 0); err == nil {
		t.Fatal("started accepted an invalid PID")
	}
	if err := service.Renew(canceled, guard); err == nil {
		t.Fatal("renew accepted a canceled context")
	}
	if _, err := service.ConfirmDestroyed(canceled, guard); err == nil {
		t.Fatal("confirmation accepted a canceled context")
	}
	badGuard := guard
	badGuard.ContainerName = "wrong-name"
	if err := service.Arm(context.Background(), badGuard); err == nil {
		t.Fatal("arm accepted an invalid container name")
	}
	processes.readyErr = errors.New("cgroup v2 unavailable")
	if err := service.Arm(context.Background(), guard); err == nil {
		t.Fatal("arm proceeded without independent process proof support")
	}
	processes.readyErr = nil

	runningDocker := newFakeDocker(guard, true, 888)
	runningService := newService(store, runningDocker, processes, func() time.Time { return now })
	if err := runningService.Arm(context.Background(), guard); err == nil {
		t.Fatal("arm accepted a container that was already running")
	}
	if _, err := service.ConfirmDestroyed(context.Background(), guard); err == nil {
		t.Fatal("confirmation accepted an unknown lease")
	}
	if err := service.Started(context.Background(), guard, 321); err == nil {
		t.Fatal("started accepted an unknown lease")
	}
	if err := service.Renew(context.Background(), guard); err == nil {
		t.Fatal("renew accepted an unknown lease")
	}
	if err := service.Arm(context.Background(), guard); err != nil {
		t.Fatal(err)
	}
	if err := service.Started(context.Background(), guard, 321); err == nil {
		t.Fatal("started accepted a PID when Docker reported the container stopped")
	}
	docker.setRunning(321)
	if err := service.Started(context.Background(), guard, 322); err == nil {
		t.Fatal("started accepted a PID different from Docker inspection")
	}
	processes.proof = processProof{}
	if err := service.Started(context.Background(), guard, 321); err == nil {
		t.Fatal("started accepted unavailable host process evidence")
	}
	processes.proof = processProof{PID: 321, StartTimeTick: 91, CgroupPath: "/docker/" + guard.ContainerID}
	if err := service.Started(context.Background(), guard, 321); err != nil {
		t.Fatal(err)
	}

	updated := guard
	updated.ExpiresAt = now.Add(20 * time.Second)
	if err := service.Renew(context.Background(), updated); err != nil {
		t.Fatalf("valid lease renewal failed: %v", err)
	}
	if err := service.Renew(context.Background(), guard); err == nil {
		t.Fatal("renew accepted an older lease deadline")
	}
	if err := service.Started(context.Background(), guard, 321); err == nil {
		t.Fatal("started accepted a stale lease after renewal")
	}
}

func TestOpenFailsClosedWithoutLinuxProcessTreeSupport(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux host support is verified by the Linux cgroup v2 implementation")
	}
	_, err := Open(filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "docker.sock"), testAttestationPrivateKey())
	if err == nil || !strings.Contains(err.Error(), "Linux cgroup v2") {
		t.Fatalf("Open did not fail closed on %s: %v", runtime.GOOS, err)
	}
}

func TestLeaseStoreRejectsUnsafeAndCorruptState(t *testing.T) {
	t.Run("nonabsolute path", func(t *testing.T) {
		if _, err := openLeaseStore("relative"); err == nil {
			t.Fatal("accepted a relative state path")
		}
	})
	t.Run("root path", func(t *testing.T) {
		if _, err := openLeaseStore("/"); err == nil {
			t.Fatal("accepted the filesystem root as state directory")
		}
	})
	t.Run("world accessible directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := openLeaseStore(dir); err == nil {
			t.Fatal("accepted a nonprivate state directory")
		}
	})
	t.Run("unexpected entry", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "unrelated"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := openLeaseStore(dir); err == nil {
			t.Fatal("accepted an unexpected state entry")
		}
	})
	t.Run("incomplete temp cleanup", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		temp := filepath.Join(dir, ".lease-crash.tmp")
		if err := os.WriteFile(temp, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := openLeaseStore(dir); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(temp); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale temp file remains: %v", err)
		}
	})
	t.Run("bad filename", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := openLeaseStore(dir); err == nil {
			t.Fatal("accepted an invalid lease filename")
		}
	})
	t.Run("insecure lease mode", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, strings.Repeat("a", 64)+".json")
		if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := openLeaseStore(dir); err == nil {
			t.Fatal("accepted a world-readable lease file")
		}
	})
	t.Run("malformed lease", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, strings.Repeat("a", 64)+".json")
		if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := openLeaseStore(dir); err == nil {
			t.Fatal("accepted malformed persisted JSON")
		}
	})
	t.Run("mismatched persisted identity", func(t *testing.T) {
		dir := t.TempDir()
		guard := testGuard(time.Now())
		data, err := json.Marshal(lease{Guard: guard})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, strings.Repeat("b", 64)+".json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := openLeaseStore(dir); err == nil {
			t.Fatal("accepted persisted state under another Docker ID")
		}
	})
}

func TestLeaseStoreCreateReplaceAndMissingLease(t *testing.T) {
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	guard := testGuard(time.Now())
	value := lease{Guard: guard}
	if err := store.create(value); err != nil {
		t.Fatal(err)
	}
	if err := store.create(value); err == nil {
		t.Fatal("created the same Docker ID twice")
	}
	guard.ExpiresAt = guard.ExpiresAt.Add(time.Second)
	value.Guard = guard
	if err := store.replace(value); err != nil {
		t.Fatalf("replace lease: %v", err)
	}
	if got, ok := store.get(guard.ContainerID); !ok || !got.Guard.ExpiresAt.Equal(guard.ExpiresAt) {
		t.Fatal("replacement was not visible in the durable store")
	}
	missing := testGuard(time.Now().Add(time.Second))
	missing.ContainerID = strings.Repeat("b", 64)
	if err := store.replace(lease{Guard: missing}); err == nil {
		t.Fatal("replaced a missing lease")
	}
	if got := store.all(); len(got) != 1 {
		t.Fatalf("store contains %d leases, want one", len(got))
	}
	invalid := lease{Guard: guard}
	invalid.Guard.ContainerID = strings.Repeat("b", 64)
	invalid.Guard.ContainerName = "wrong-name"
	if err := store.create(invalid); err == nil {
		t.Fatal("persisted an invalid lease identity")
	}
}

func TestPersistedLeaseRequiresBoundProcessEvidence(t *testing.T) {
	guard := testGuard(time.Now())
	if err := validPersistedLease(lease{Guard: guard}); err != nil {
		t.Fatalf("valid unstarted lease rejected: %v", err)
	}
	if err := validPersistedLease(lease{}); err == nil {
		t.Fatal("accepted a lease without an expiry or identity")
	}
	startedWithoutProof := lease{Guard: guard, Started: true}
	if err := validPersistedLease(startedWithoutProof); err == nil {
		t.Fatal("accepted a started lease without host process evidence")
	}
	proofWithoutStarted := lease{Guard: guard, Process: processProof{PID: 11}}
	if err := validPersistedLease(proofWithoutStarted); err == nil {
		t.Fatal("accepted process evidence without a started state")
	}
	if expectedContainerName(guard.Fence, "invalid-hash") != "" {
		t.Fatal("derived a container name from an invalid nonce hash")
	}
}

func TestProcessParsersBindCgroupAndPIDStartTime(t *testing.T) {
	id := strings.Repeat("a", 64)
	if !containerCgroupMatches("/system.slice/docker-"+id+".scope", id) {
		t.Fatal("did not recognize systemd Docker cgroup identity")
	}
	if containerCgroupMatches("/system.slice/docker-"+strings.Repeat("b", 64)+".scope", id) {
		t.Fatal("accepted a cgroup for another container")
	}
	if containerCgroupMatches("/../docker-"+id+".scope", id) {
		t.Fatal("accepted a noncanonical cgroup path")
	}
	path, err := parseUnifiedCgroup([]byte("2:cpu,cpuacct:/wrong\n0::/system.slice/docker-" + id + ".scope\n"))
	if err != nil || !containerCgroupMatches(path, id) {
		t.Fatalf("parse unified cgroup: path=%q err=%v", path, err)
	}
	if _, err := parseUnifiedCgroup([]byte("2:cpu:/wrong\n")); err == nil {
		t.Fatal("accepted a host without unified cgroup membership")
	}
	if _, err := parseUnifiedCgroup([]byte("0::/\n")); err == nil {
		t.Fatal("accepted a cgroup root without container identity")
	}
	fields := []string{"S"}
	for len(fields) < 19 {
		fields = append(fields, "0")
	}
	fields = append(fields, "998877")
	stat := "321 (worker with ) in name) " + strings.Join(fields, " ")
	start, err := parseProcStartTime([]byte(stat))
	if err != nil || start != 998877 {
		t.Fatalf("parse PID start time: got=%d err=%v", start, err)
	}
	if _, err := parseProcStartTime([]byte("malformed")); err == nil {
		t.Fatal("accepted a malformed proc stat record")
	}
	if _, err := parseProcStartTime([]byte("321 (short) S 1 2")); err == nil {
		t.Fatal("accepted a proc stat record without start time")
	}
}

func TestUnsupportedProcessInspectorNeverClaimsProof(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux host process proof is implemented in process_linux.go")
	}
	inspector := newProcessInspector()
	if err := inspector.Ready(); err == nil {
		t.Fatal("unsupported host reported process proof ready")
	}
	if _, err := inspector.Capture(strings.Repeat("a", 64), 123); err == nil {
		t.Fatal("unsupported host captured process evidence")
	}
	if gone, err := inspector.Gone(strings.Repeat("a", 64), processProof{}); err == nil || gone {
		t.Fatal("unsupported host claimed process destruction")
	}
	if err := inspector.KillTree(strings.Repeat("a", 64), processProof{}); err == nil {
		t.Fatal("unsupported host accepted a process-tree kill")
	}
	tasks, ok := inspector.(taskProcessInspector)
	if !ok {
		t.Fatal("unsupported host omitted Task process fail-closed methods")
	}
	if _, err := tasks.AttachTaskProcess("", processProof{}, 2, ""); err == nil {
		t.Fatal("unsupported host attached a Task process")
	}
	if err := tasks.KillTaskProcess("", processProof{}, taskProcessProof{}, ""); err == nil {
		t.Fatal("unsupported host accepted a Task process kill")
	}
	if gone, err := tasks.TaskProcessGone("", processProof{}, taskProcessProof{}, ""); err == nil || gone {
		t.Fatal("unsupported host claimed a Task process was gone")
	}
	if err := tasks.RemoveTaskCgroup("", processProof{}, taskProcessProof{}, ""); err == nil {
		t.Fatal("unsupported host accepted Task cgroup removal")
	}
}

func TestUnixListenerUsesPrivatePermissionsAndRemovesOwnedSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "dg-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "guardian.sock")
	listener, cleanup, err := unixListener(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket permissions = %o, want 600", info.Mode().Perm())
	}
	if _, _, err := unixListener(path, 0o600); err == nil {
		t.Fatal("opened an existing socket path")
	}
	if current, err := os.Lstat(path); err != nil || !os.SameFile(info, current) {
		t.Fatalf("live guardian socket changed after duplicate listen: %v", err)
	}
	cleanup()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned socket was not removed on cleanup: %v", err)
	}
	groupPath := filepath.Join(dir, "guardian-group.sock")
	groupListener, groupCleanup, err := unixListener(groupPath, 0o660)
	if err != nil {
		t.Fatal(err)
	}
	groupInfo, err := os.Stat(groupPath)
	if err != nil || groupInfo.Mode().Perm() != 0o660 {
		t.Fatalf("group socket permissions = %v, err=%v; want 660", groupInfo, err)
	}
	groupCleanup()
	_ = groupListener
	if _, _, err := unixListener(filepath.Join(dir, "invalid.sock"), 0o666); err == nil {
		t.Fatal("accepted an unsafe guardian socket mode")
	}
	_ = listener
}

func TestUnixListenerReclaimsOnlyStaleSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "dg-stale-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	stalePath := filepath.Join(dir, "stale.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: stalePath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	listener, cleanup, err := unixListener(stalePath, 0o600)
	if err != nil {
		t.Fatalf("reopen stale guardian socket: %v", err)
	}
	defer cleanup()
	if info, err := os.Lstat(stalePath); err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("reclaimed guardian socket is unsafe: info=%v err=%v", info, err)
	}
	_ = listener
	regularPath := filepath.Join(dir, "regular.sock")
	if err := os.WriteFile(regularPath, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := unixListener(regularPath, 0o600); err == nil {
		t.Fatal("replaced a non-socket guardian path")
	}
	if data, err := os.ReadFile(regularPath); err != nil || string(data) != "keep" {
		t.Fatalf("non-socket guardian path changed: %q, %v", data, err)
	}
}

func TestDockerEngineFailsClosedOnUnexpectedStatuses(t *testing.T) {
	missingID := strings.Repeat("c", 64)
	badJSONID := strings.Repeat("d", 64)
	badKillID := strings.Repeat("f", 64)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, missingID):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, badJSONID):
			_, _ = w.Write([]byte("{broken"))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/kill"):
			if strings.Contains(r.URL.Path, badKillID) {
				http.Error(w, "engine unavailable", http.StatusInternalServerError)
			} else {
				w.WriteHeader(http.StatusConflict)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/wait"):
			_, _ = w.Write([]byte("{}"))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusConflict)
		default:
			http.Error(w, "engine unavailable", http.StatusInternalServerError)
		}
	})
	engine, err := newDockerEngine(startUnixHTTPServer(t, handler))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.inspect(context.Background(), missingID); !errors.Is(err, errContainerNotFound) {
		t.Fatalf("missing-container status error = %v", err)
	}
	if _, err := engine.inspect(context.Background(), badJSONID); err == nil {
		t.Fatal("accepted malformed Docker JSON")
	}
	if _, err := engine.inspect(context.Background(), strings.Repeat("e", 64)); err == nil {
		t.Fatal("accepted a Docker server error")
	}
	if err := engine.kill(context.Background(), strings.Repeat("e", 64)); !errors.Is(err, errDockerConflict) {
		t.Fatalf("Docker kill conflict = %v", err)
	}
	if err := engine.kill(context.Background(), badKillID); err == nil || errors.Is(err, errDockerConflict) {
		t.Fatalf("unexpected Docker kill status error = %v", err)
	}
	if err := engine.wait(context.Background(), strings.Repeat("e", 64)); err == nil {
		t.Fatal("accepted a Docker wait response without an exit status")
	}
	if err := engine.remove(context.Background(), strings.Repeat("e", 64)); err == nil {
		t.Fatal("accepted a Docker remove conflict")
	}
	if err := engine.remove(context.Background(), missingID); !errors.Is(err, errContainerNotFound) {
		t.Fatalf("Docker remove missing error = %v", err)
	}
}

func TestHandlerRejectsUnknownRequestFields(t *testing.T) {
	service := newService(nil, nil, nil, time.Now)
	request := httptest.NewRequest(http.MethodPost, "/v1/guards", strings.NewReader("{\"unexpected\":true}"))
	recorder := httptest.NewRecorder()
	service.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want bad request", recorder.Code)
	}
}

func TestHandlerRejectsWrongRoutesAndMalformedBodies(t *testing.T) {
	service := newService(nil, nil, nil, time.Now)
	id := strings.Repeat("a", 64)
	requests := []struct {
		method string
		path   string
		body   string
		status int
	}{
		{method: http.MethodGet, path: "/v1/guards", status: http.StatusNotFound},
		{method: http.MethodPost, path: "/v1/guards/not-an-id/started", body: "{}", status: http.StatusNotFound},
		{method: http.MethodPost, path: "/v1/guards/" + id + "/started", body: "{", status: http.StatusBadRequest},
		{method: http.MethodPut, path: "/v1/guards/" + id + "/renew", status: http.StatusBadRequest},
		{method: http.MethodPost, path: "/v1/guards", body: strings.Repeat("x", maxRequestBytes+1), status: http.StatusBadRequest},
	}
	for _, item := range requests {
		request := httptest.NewRequest(item.method, item.path, strings.NewReader(item.body))
		recorder := httptest.NewRecorder()
		service.Handler().ServeHTTP(recorder, request)
		if recorder.Code != item.status {
			t.Errorf("%s %s status=%d, want %d", item.method, item.path, recorder.Code, item.status)
		}
	}
	guardData, err := json.Marshal(testGuard(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/guards", strings.NewReader(string(guardData)+" {}"))
	recorder := httptest.NewRecorder()
	service.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON status=%d, want bad request", recorder.Code)
	}
}

func TestUnixAPIClientLifecycle(t *testing.T) {
	now := time.Now().UTC()
	guard := testGuard(now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	docker := newFakeDocker(guard, false, 0)
	processes := &fakeProcessInspector{proof: processProof{
		PID: 765, StartTimeTick: 77, CgroupPath: "/docker/" + guard.ContainerID,
	}, gone: true}
	service := newService(store, docker, processes, func() time.Time { return now })
	dir, err := os.MkdirTemp("/tmp", "dg-")
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
			t.Fatal("guardian server did not create its Unix socket")
		}
		time.Sleep(5 * time.Millisecond)
	}
	client, err := NewClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	//nolint:staticcheck // SA1012: this verifies nil contexts are rejected at the client boundary.
	if err := client.Arm(nil, guard); err == nil {
		t.Fatal("client accepted a nil context")
	}
	if err := client.Arm(context.Background(), guard); err != nil {
		t.Fatalf("API arm: %v", err)
	}
	docker.setRunning(765)
	if err := client.Started(context.Background(), guard, 765); err != nil {
		t.Fatalf("API started: %v", err)
	}
	renewed := guard
	renewed.ExpiresAt = now.Add(15 * time.Second)
	if err := client.Renew(context.Background(), renewed); err != nil {
		t.Fatalf("API renew: %v", err)
	}
	destroyed, err := client.ConfirmDestroyed(context.Background(), renewed)
	if err != nil || !destroyed {
		t.Fatalf("API confirm: destroyed=%v err=%v", destroyed, err)
	}
	attestation, err := client.RecoverStopped(context.Background(), renewed.Fence, renewed.NonceSHA256)
	if err != nil || !attestation.Destroyed || attestation.Fence != renewed.Fence || attestation.NonceSHA256 != renewed.NonceSHA256 {
		t.Fatalf("API recover stop: attestation=%+v err=%v", attestation, err)
	}
	if err := client.Renew(context.Background(), renewed); err == nil {
		t.Fatal("API renewed a destroyed lease")
	}
	if _, err := NewClient("relative.sock"); err == nil {
		t.Fatal("client accepted a relative socket path")
	}
	cancel()
	if err := <-serveDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("server shutdown error = %v", err)
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("guardian socket remained after shutdown: %v", err)
	}
}

func TestServeAndRunRejectInvalidSweepInterval(t *testing.T) {
	service := newService(nil, nil, nil, time.Now)
	if err := service.Run(context.Background(), 0, nil); err == nil {
		t.Fatal("Run accepted a zero sweep interval")
	}
	dir, err := os.MkdirTemp("/tmp", "dg-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	err = service.Serve(context.Background(), filepath.Join(dir, "guardian.sock"), 0o600, 0, nil)
	if err == nil || !strings.Contains(err.Error(), "expiry monitor stopped") {
		t.Fatalf("Serve did not report its stopped expiry monitor: %v", err)
	}
}

func TestDockerEngineUsesOnlyExpectedContainerEndpoints(t *testing.T) {
	guard := testGuard(time.Now())
	state := newFakeDocker(guard, true, 654)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1.44/containers/"+guard.ContainerID) {
			t.Errorf("unexpected Docker API path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if !state.exists && r.Method == http.MethodGet {
			http.NotFound(w, r)
			return
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
			payload, _ := json.Marshal(map[string]any{
				"Id": state.value.ID, "Name": state.value.Name,
				"Config": map[string]any{"Labels": map[string]string{
					"morde.dsh.fence": mustFenceJSON(guard.Fence), "morde.dsh.nonce-sha256": guard.NonceSHA256,
				}, "IgnoredDockerField": true},
				"State": map[string]any{"Running": true, "Pid": 654, "Status": "running"},
			})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(payload)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/kill"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/wait"):
			_, _ = w.Write([]byte("{\"StatusCode\":137}"))
		case r.Method == http.MethodDelete:
			state.exists = false
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected operation", http.StatusBadRequest)
		}
	})
	socket := startUnixHTTPServer(t, handler)
	engine, err := newDockerEngine(socket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newDockerEngine("relative.sock"); err == nil {
		t.Fatal("Docker client accepted a relative socket path")
	}
	inspected, err := engine.inspect(context.Background(), guard.ContainerID)
	if err != nil || inspected.ID != guard.ContainerID || inspected.State.PID == nil || *inspected.State.PID != 654 {
		t.Fatalf("Docker inspect: value=%+v err=%v", inspected, err)
	}
	if err := engine.kill(context.Background(), guard.ContainerID); err != nil {
		t.Fatalf("Docker kill: %v", err)
	}
	if err := engine.wait(context.Background(), guard.ContainerID); err != nil {
		t.Fatalf("Docker wait: %v", err)
	}
	if err := engine.remove(context.Background(), guard.ContainerID); err != nil {
		t.Fatalf("Docker remove: %v", err)
	}
	if _, err := engine.inspect(context.Background(), guard.ContainerID); !errors.Is(err, errContainerNotFound) {
		t.Fatalf("Docker absence error = %v", err)
	}
}

func startUnixHTTPServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "dg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "http.sock")
	listener, cleanup, err := unixListener(socket, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/version" {
			_ = json.NewEncoder(w).Encode(map[string]string{"ApiVersion": "1.52", "MinAPIVersion": "1.44"})
			return
		}
		handler.ServeHTTP(w, r)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		cleanup()
	})
	return socket
}

func testGuard(now time.Time) Guard {
	fence := app.AgentInstanceFence{
		TaskID: "q1234567", OwnerID: "u1234567", AgentInstanceID: "agent-1", AgentGeneration: 1,
		AgentSessionID: "session-1", SupervisorID: "supervisor-test", LeaseOwner: "worker-1", LeaseVersion: 1,
		AuthorizationVersion: 1, EngineVersion: "0.2.0", ProfileVersion: "v1",
	}
	nonce := sha256.Sum256([]byte("guardian-test-nonce"))
	hash := hex.EncodeToString(nonce[:])
	return Guard{
		ContainerID: strings.Repeat("a", 64), ContainerName: expectedContainerName(fence, hash),
		Fence: fence, NonceSHA256: hash, ExpiresAt: now.Add(5 * time.Second),
	}
}

func newFakeDocker(guard Guard, running bool, pid int) *fakeDocker {
	runningValue, pidValue := running, pid
	return &fakeDocker{
		value: containerInspect{ID: guard.ContainerID, Name: "/" + guard.ContainerName,
			Config: struct {
				Labels map[string]string `json:"Labels"`
			}{Labels: map[string]string{
				"morde.dsh.fence":        mustFenceJSON(guard.Fence),
				"morde.dsh.nonce-sha256": guard.NonceSHA256,
			}},
			State: struct {
				Running *bool `json:"Running"`
				PID     *int  `json:"Pid"`
			}{Running: &runningValue, PID: &pidValue}},
		exists: true,
	}
}

func mustFenceJSON(fence app.AgentInstanceFence) string {
	data, _ := json.Marshal(fence)
	return string(data)
}

type fakeDocker struct {
	value       containerInspect
	exists      bool
	killCalls   int
	removeCalls int
}

func (d *fakeDocker) inspect(_ context.Context, id string) (containerInspect, error) {
	if !d.exists || id != d.value.ID && id != strings.TrimPrefix(d.value.Name, "/") {
		return containerInspect{}, errContainerNotFound
	}
	return d.value, nil
}

func (d *fakeDocker) kill(_ context.Context, _ string) error {
	d.killCalls++
	if d.value.State.Running != nil {
		*d.value.State.Running = false
	}
	if d.value.State.PID != nil {
		*d.value.State.PID = 0
	}
	return nil
}

func (*fakeDocker) wait(context.Context, string) error { return nil }

func (d *fakeDocker) remove(_ context.Context, _ string) error {
	d.removeCalls++
	d.exists = false
	return nil
}

func (d *fakeDocker) setRunning(pid int) {
	if d.value.State.Running != nil {
		*d.value.State.Running = true
	}
	if d.value.State.PID != nil {
		*d.value.State.PID = pid
	}
}

type fakeProcessInspector struct {
	proof          processProof
	gone           bool
	readyErr       error
	killTreeErr    error
	killTreeClears bool
	killTreeCalls  int
}

func (p *fakeProcessInspector) Ready() error { return p.readyErr }

func (p fakeProcessInspector) Capture(containerID string, pid int) (processProof, error) {
	if p.proof.PID != pid || !containerCgroupMatches(p.proof.CgroupPath, containerID) {
		return processProof{}, errors.New("fake process evidence mismatch")
	}
	return p.proof, nil
}

func (p *fakeProcessInspector) KillTree(containerID string, proof processProof) error {
	p.killTreeCalls++
	if !containerCgroupMatches(proof.CgroupPath, containerID) {
		return errors.New("fake cgroup mismatch")
	}
	if p.killTreeErr != nil {
		return p.killTreeErr
	}
	if p.killTreeClears {
		p.gone = true
	}
	return nil
}

func (p *fakeProcessInspector) Gone(containerID string, proof processProof) (bool, error) {
	if proof.PID != 0 && !containerCgroupMatches(proof.CgroupPath, containerID) {
		return false, errors.New("fake cgroup mismatch")
	}
	return p.gone, nil
}
