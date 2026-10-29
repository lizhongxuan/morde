package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
)

func TestAccountAgentRuntimeContracts(t *testing.T) {
	request := AccountAgentRuntimeStart{OwnerID: "u1234567", RuntimeID: "runtime-1", LeaseOwner: "portal-1",
		SupervisorNonce: []byte("0123456789abcdef0123456789abcdef"), MaxConcurrentTasks: 2, LeaseDuration: 15 * time.Second}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid account runtime start: %v", err)
	}
	request.MaxConcurrentTasks = maxAccountAgentRuntimeTasks + 1
	if err := request.Validate(); err == nil {
		t.Fatal("account runtime accepted a concurrency limit above the protocol cap")
	}
	request.MaxConcurrentTasks = 2
	request.SupervisorNonce = make([]byte, 32)
	if err := request.Validate(); err == nil {
		t.Fatal("account runtime accepted an all-zero supervisor nonce")
	}
	fence := AccountAgentRuntimeFence{ServiceInstanceID: "123e4567-e89b-12d3-a456-426614174000", OwnerID: "u1234567",
		RuntimeID: "runtime-1", RuntimeGeneration: 1, LeaseOwner: "portal-1", LeaseVersion: 1, ExecutionEpoch: 1}
	if err := fence.Validate(); err != nil {
		t.Fatalf("valid account runtime fence: %v", err)
	}
	fence.ServiceInstanceID = "not-a-uuid"
	if err := fence.Validate(); err == nil {
		t.Fatal("account runtime accepted a noncanonical service identity")
	}
}

type accountAgentRuntimeStopCandidateStoreStub struct {
	AccountAgentRuntimeStore
	limit      int
	candidates []AccountAgentRuntimeStopIdentity
}

func (s *accountAgentRuntimeStopCandidateStoreStub) ListAccountAgentRuntimeStopCandidates(_ context.Context, limit int) ([]AccountAgentRuntimeStopIdentity, error) {
	s.limit = limit
	return s.candidates, nil
}

func TestAccountAgentRuntimeStopCandidateQueryBounds(t *testing.T) {
	candidate := AccountAgentRuntimeStopIdentity{Fence: AccountAgentRuntimeFence{RuntimeID: "runtime-1"}}
	store := &accountAgentRuntimeStopCandidateStoreStub{candidates: []AccountAgentRuntimeStopIdentity{candidate}}
	service, err := NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{0, MaxAccountAgentRuntimeStopCandidates + 1} {
		if _, err := service.ListAccountAgentRuntimeStopCandidates(context.Background(), limit); err == nil {
			t.Fatalf("accepted invalid stop candidate limit %d", limit)
		}
	}
	var missingContext context.Context
	if _, err := service.ListAccountAgentRuntimeStopCandidates(missingContext, 1); err == nil {
		t.Fatal("accepted nil context")
	}
	got, err := service.ListAccountAgentRuntimeStopCandidates(context.Background(), 7)
	if err != nil || store.limit != 7 || len(got) != 1 || got[0] != candidate {
		t.Fatalf("stop candidate query: got=%+v limit=%d err=%v", got, store.limit, err)
	}
}

func TestAccountAgentRuntimeGuardianProofScopesAndSigning(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	fence := AccountAgentRuntimeFence{ServiceInstanceID: "123e4567-e89b-12d3-a456-426614174000", OwnerID: "u1234567",
		RuntimeID: "runtime-1", RuntimeGeneration: 1, LeaseOwner: "portal-1", LeaseVersion: 1, ExecutionEpoch: 1}
	nonce := sha256.Sum256([]byte("runtime-supervisor-nonce"))
	nonceSHA256 := hex.EncodeToString(nonce[:])
	attestation := AccountAgentRuntimeStopAttestation{Fence: fence, ContainerID: strings.Repeat("a", 64),
		ContainerName: expectedAccountAgentRuntimeContainerName(fence, nonceSHA256), NonceSHA256: nonceSHA256,
		ExpiresAt: now.Add(time.Second), DestroyedAt: now, ProofSHA256: strings.Repeat("b", 64), Destroyed: true}
	payload, err := AccountAgentRuntimeStopAttestationSigningPayload(attestation)
	if err != nil {
		t.Fatal(err)
	}
	attestation.Signature = ed25519.Sign(privateKey, payload)
	authenticator, err := NewAccountAgentRuntimeGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := authenticator.VerifyAccountAgentRuntimeStop(context.Background(), attestation)
	if err != nil {
		t.Fatalf("verify account Runtime stop proof: %v", err)
	}
	stored, encoded := verified.Value()
	if stored.Fence != fence || len(encoded) == 0 || len(stored.Signature) != ed25519.SignatureSize {
		t.Fatalf("verified Runtime proof lost signed fields: %+v", stored)
	}
	aborted := attestation
	aborted.ContainerID = ""
	aborted.ExpiresAt = time.Time{}
	aborted.StartAborted = true
	payload, err = AccountAgentRuntimeStopAttestationSigningPayload(aborted)
	if err != nil {
		t.Fatal(err)
	}
	aborted.Signature = ed25519.Sign(privateKey, payload)
	if _, err := authenticator.VerifyAccountAgentRuntimeStop(context.Background(), aborted); err != nil {
		t.Fatalf("verify aborted account Runtime start proof: %v", err)
	}
	aborted.StartAborted = false
	if _, err := authenticator.VerifyAccountAgentRuntimeStop(context.Background(), aborted); err == nil {
		t.Fatal("aborted account Runtime start proof was accepted as a container stop proof")
	}

	taskFence := AgentInstanceFence{TaskID: "q1234567", OwnerID: fence.OwnerID, AgentInstanceID: "agent-1", AgentGeneration: 1,
		AgentSessionID: "session-1", SupervisorID: "supervisor-1", LeaseOwner: "worker-1", LeaseVersion: 1,
		AuthorizationVersion: 1, EngineVersion: "v1", ProfileVersion: "p1"}
	taskNonce := sha256.Sum256([]byte("task-supervisor-nonce"))
	taskAttestation := AccountAgentTaskStopAttestation{RuntimeFence: fence, TaskFence: taskFence,
		RuntimeContainerID: strings.Repeat("c", 64), ContainerPID: 8, HostPID: 80, StartTimeTick: 123,
		CgroupPath: "/sys/fs/cgroup/morde/task-agent", NonceSHA256: hex.EncodeToString(taskNonce[:]),
		ExpiresAt: now.Add(time.Second), DestroyedAt: now, ProofSHA256: strings.Repeat("d", 64), Destroyed: true}
	taskPayload, err := AccountAgentTaskStopAttestationSigningPayload(taskAttestation)
	if err != nil {
		t.Fatal(err)
	}
	taskAttestation.Signature = ed25519.Sign(privateKey, taskPayload)
	taskAuthenticator, err := NewAccountAgentTaskGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := taskAuthenticator.VerifyAccountAgentTaskStop(context.Background(), taskAttestation); err != nil {
		t.Fatalf("verify account Task stop proof: %v", err)
	}
	if _, err := authenticator.VerifyAccountAgentRuntimeStop(context.Background(), AccountAgentRuntimeStopAttestation{
		Fence: fence, ContainerID: strings.Repeat("a", 64), ContainerName: "wrong", NonceSHA256: nonceSHA256,
		ExpiresAt: now.Add(time.Second), DestroyedAt: now, ProofSHA256: strings.Repeat("b", 64), Destroyed: true,
		Signature: attestation.Signature,
	}); err == nil {
		t.Fatal("Runtime proof accepted an invalid container name or wrong signed scope")
	}
}

type accountAgentRuntimeStopAckStoreStub struct {
	AccountAgentRuntimeStore
	limit     int
	pending   []AccountAgentRuntimeStopAttestation
	marked    VerifiedAccountAgentRuntimeStop
	markCalls int
	replayed  bool
}

func (s *accountAgentRuntimeStopAckStoreStub) ListPendingAccountAgentRuntimeStopAttestations(_ context.Context,
	limit int) ([]AccountAgentRuntimeStopAttestation, error) {
	s.limit = limit
	return append([]AccountAgentRuntimeStopAttestation(nil), s.pending...), nil
}

func (s *accountAgentRuntimeStopAckStoreStub) MarkAccountAgentRuntimeStopAttestationAcknowledged(_ context.Context,
	proof VerifiedAccountAgentRuntimeStop) (bool, error) {
	s.marked = proof
	s.markCalls++
	return s.replayed, nil
}

func signedAccountAgentRuntimeStopTestAttestation(t *testing.T, key ed25519.PrivateKey) AccountAgentRuntimeStopAttestation {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	fence := AccountAgentRuntimeFence{ServiceInstanceID: "123e4567-e89b-12d3-a456-426614174000", OwnerID: "u1234567",
		RuntimeID: "runtime-ack", RuntimeGeneration: 1, LeaseOwner: "portal-ack", LeaseVersion: 1, ExecutionEpoch: 1}
	nonce := sha256.Sum256([]byte("runtime-stop-ack-nonce"))
	attestation := AccountAgentRuntimeStopAttestation{Fence: fence, ContainerID: strings.Repeat("a", 64),
		NonceSHA256: hex.EncodeToString(nonce[:]), ProofSHA256: strings.Repeat("b", 64), Destroyed: true,
		ExpiresAt: now.Add(time.Minute), DestroyedAt: now}
	attestation.ContainerName = expectedAccountAgentRuntimeContainerName(fence, attestation.NonceSHA256)
	payload, err := AccountAgentRuntimeStopAttestationSigningPayload(attestation)
	if err != nil {
		t.Fatal(err)
	}
	attestation.Signature = ed25519.Sign(key, payload)
	return attestation
}

func TestAccountAgentRuntimeServiceVerifiesPendingStopProofsBeforeAcknowledging(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authenticator, err := NewAccountAgentRuntimeGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	attestation := signedAccountAgentRuntimeStopTestAttestation(t, privateKey)
	store := &accountAgentRuntimeStopAckStoreStub{pending: []AccountAgentRuntimeStopAttestation{attestation}}
	service, err := NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{0, MaxPendingAccountAgentRuntimeStopAttestations + 1} {
		if _, err := service.ListPendingAccountAgentRuntimeStopAttestations(context.Background(), limit, authenticator); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Fatalf("accepted invalid Runtime stop outbox limit %d: %v", limit, err)
		}
	}
	if _, err := service.ListPendingAccountAgentRuntimeStopAttestations(context.Background(), 1, nil); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("accepted missing Runtime stop verifier: %v", err)
	}
	pending, err := service.ListPendingAccountAgentRuntimeStopAttestations(context.Background(), 1, authenticator)
	if err != nil || store.limit != 1 || len(pending) != 1 || pending[0].Fence != attestation.Fence {
		t.Fatalf("list verified pending Runtime proofs: pending=%+v limit=%d err=%v", pending, store.limit, err)
	}
	store.pending[0].Signature = append([]byte(nil), store.pending[0].Signature...)
	store.pending[0].Signature[0] ^= 1
	if _, err := service.ListPendingAccountAgentRuntimeStopAttestations(context.Background(), 1, authenticator); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("accepted invalid stored Runtime stop proof: %v", err)
	}
	store.pending[0] = attestation

	store.replayed = true
	replayed, err := service.MarkAccountAgentRuntimeStopAttestationAcknowledged(context.Background(), attestation, authenticator)
	if err != nil || !replayed || store.markCalls != 1 {
		t.Fatalf("mark verified Runtime proof acknowledged: replayed=%t calls=%d err=%v", replayed, store.markCalls, err)
	}
	marked, encoded := store.marked.Value()
	if marked.Fence != attestation.Fence || len(encoded) == 0 {
		t.Fatalf("store did not receive verified Runtime proof: attestation=%+v encoded=%d", marked, len(encoded))
	}
	altered := attestation
	altered.ProofSHA256 = strings.Repeat("c", 64)
	payload, err := AccountAgentRuntimeStopAttestationSigningPayload(altered)
	if err != nil {
		t.Fatal(err)
	}
	altered.Signature = ed25519.Sign(privateKey, payload)
	if _, err := service.MarkAccountAgentRuntimeStopAttestationAcknowledged(context.Background(), altered, authenticator); err != nil {
		t.Fatalf("valid alternate proof should reach the exact-persistence CAS: %v", err)
	}
	if _, err := service.MarkAccountAgentRuntimeStopAttestationAcknowledged(context.Background(), attestation, nil); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("accepted missing verifier when acknowledging Runtime proof: %v", err)
	}
}
