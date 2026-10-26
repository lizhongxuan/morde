package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"morde/server/internal/domain/host"
)

func TestSealerAuthenticatesCiphertext(t *testing.T) {
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := sealer.Seal([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := sealer.Open(ciphertext)
	if err != nil || string(plain) != "secret" {
		t.Fatalf("open = %q, %v", plain, err)
	}
	ciphertext[len(ciphertext)-1] ^= 1
	if _, err := sealer.Open(ciphertext); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}

func TestRuntimeResolvesCachesAndInvalidatesEndpoint(t *testing.T) {
	sealer, _ := NewSealer(make([]byte, 32))
	secret, _ := sealer.Seal([]byte("aio-secret"))
	store := &runtimeHostStore{value: host.Host{ID: "h1234567", OwnerID: "u1234567", State: host.StateRunning, RuntimeProfile: "container", SandboxID: "sandbox", AIOKeyEnc: secret}}
	sandbox := &runtimeSandbox{}
	runtime, err := NewRuntime(store, runtimeRouter{sandbox}, sealer, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	first, err := runtime.Resolve(context.Background(), store.value.ID, 8080)
	if err != nil || first.APIKey != "aio-secret" {
		t.Fatalf("first resolve = %#v, %v", first, err)
	}
	if _, err := runtime.Resolve(context.Background(), store.value.ID, 8080); err != nil {
		t.Fatal(err)
	}
	if sandbox.endpoints.Load() != 1 {
		t.Fatalf("endpoint calls = %d, want 1", sandbox.endpoints.Load())
	}
	runtime.InvalidateUpstream(store.value.ID)
	if _, err := runtime.Resolve(context.Background(), store.value.ID, 8080); err != nil {
		t.Fatal(err)
	}
	if sandbox.endpoints.Load() != 2 {
		t.Fatalf("endpoint calls after invalidation = %d, want 2", sandbox.endpoints.Load())
	}
	runtime.MarkActive(store.value.ID)
	active := runtime.TakeActivity()
	if _, ok := active[store.value.ID]; !ok || len(runtime.TakeActivity()) != 0 {
		t.Fatalf("activity batch = %#v", active)
	}
}

func TestRuntimeLookupIncludesActivityLeaseFences(t *testing.T) {
	sealer, _ := NewSealer(make([]byte, 32))
	store := &runtimeHostStore{value: host.Host{
		ID: "h1234567", OwnerID: "u1234567", State: host.StateRunning,
		RuntimeGeneration: 7, ControlVersion: 12, ControlState: host.ControlUser,
	}}
	runtime, err := NewRuntime(store, runtimeRouter{&runtimeSandbox{}}, sealer, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := runtime.Lookup(context.Background(), store.value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ref.RuntimeGeneration != 7 || ref.ControlVersion != 12 || ref.ControlState != host.ControlUser {
		t.Fatalf("Host ref fences/control = generation %d/control %d/state %q", ref.RuntimeGeneration, ref.ControlVersion, ref.ControlState)
	}
}

func TestRuntimeFlushActivityPreservesFailedBatch(t *testing.T) {
	sealer, _ := NewSealer(make([]byte, 32))
	store := &runtimeHostStore{touchErr: errors.New("database unavailable")}
	runtime, err := NewRuntime(store, runtimeRouter{&runtimeSandbox{}}, sealer, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	runtime.MarkActive("h1234567")
	if err := runtime.FlushActivity(context.Background()); err == nil {
		t.Fatal("FlushActivity() error = nil")
	}
	if len(runtime.TakeActivity()) != 1 {
		t.Fatal("failed activity batch was lost")
	}
	runtime.MarkActive("h1234567")
	store.touchErr = nil
	if err := runtime.FlushActivity(context.Background()); err != nil {
		t.Fatalf("FlushActivity() error = %v", err)
	}
	if len(store.touched) != 1 || store.touched[0] != "h1234567" || len(runtime.TakeActivity()) != 0 {
		t.Fatalf("flushed IDs=%v, pending=%v", store.touched, runtime.TakeActivity())
	}
}

type runtimeHostStore struct {
	HostStore
	mu       sync.Mutex
	value    host.Host
	touchErr error
	touched  []string
}

func (s *runtimeHostStore) GetHost(context.Context, string) (host.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value, nil
}

func (s *runtimeHostStore) TransitionHost(_ context.Context, _ string, from, to host.State, _ string) (host.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.value.State != from {
		return host.Host{}, errors.New("state changed")
	}
	s.value.State = to
	return s.value, nil
}

func (s *runtimeHostStore) TouchHosts(_ context.Context, ids []string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.touchErr != nil {
		return s.touchErr
	}
	s.touched = append(s.touched, ids...)
	return nil
}

type runtimeRouter struct{ sandbox SandboxClient }

func (r runtimeRouter) For(string) (SandboxClient, error) { return r.sandbox, nil }
func (runtimeRouter) Profiles() []string                  { return []string{"container"} }

type runtimeSandbox struct {
	SandboxClient
	endpoints atomic.Int32
}

func (s *runtimeSandbox) Endpoint(context.Context, string, int) (Upstream, error) {
	s.endpoints.Add(1)
	return Upstream{BaseURL: "http://127.0.0.1:8080"}, nil
}
