package app

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
)

func TestTaskSandboxGuardianStopProofIsIndependentAndFenceBound(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	authenticator, err := NewTaskSandboxGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	attestation := signedTaskSandboxStopForTest(t, privateKey)
	verified, err := authenticator.VerifyTaskSandboxStop(context.Background(), attestation)
	if err != nil {
		t.Fatalf("verify exact Task Sandbox proof: %v", err)
	}
	got, encoded := verified.Value()
	if got.ContainerID != attestation.ContainerID || got.SandboxGeneration != attestation.SandboxGeneration || len(encoded) == 0 {
		t.Fatalf("verified proof = %#v, encoded bytes=%d", got, len(encoded))
	}
	got.Signature[0] ^= 0xff
	if attestation.Signature[0] != verified.value.attestation.Signature[0] {
		t.Fatal("verified proof exposed its mutable signature buffer")
	}
	encoded[0] ^= 0xff
	_, secondEncoding := verified.Value()
	if secondEncoding[0] == encoded[0] {
		t.Fatal("verified proof exposed its mutable evidence buffer")
	}

	for name, mutate := range map[string]func(*TaskSandboxGuardianStopAttestation){
		"task":             func(value *TaskSandboxGuardianStopAttestation) { value.TaskID = "q7654321" },
		"owner":            func(value *TaskSandboxGuardianStopAttestation) { value.OwnerID = "u7654321" },
		"generation":       func(value *TaskSandboxGuardianStopAttestation) { value.SandboxGeneration++ },
		"sandbox":          func(value *TaskSandboxGuardianStopAttestation) { value.SandboxID = "sandbox-other" },
		"docker container": func(value *TaskSandboxGuardianStopAttestation) { value.ContainerID = strings.Repeat("b", 64) },
		"nonce":            func(value *TaskSandboxGuardianStopAttestation) { value.NonceSHA256 = strings.Repeat("c", 64) },
		"destroyed":        func(value *TaskSandboxGuardianStopAttestation) { value.Destroyed = false },
		"signature":        func(value *TaskSandboxGuardianStopAttestation) { value.Signature[0] ^= 0xff },
	} {
		t.Run(name, func(t *testing.T) {
			changed := signedTaskSandboxStopForTest(t, privateKey)
			mutate(&changed)
			if _, err := authenticator.VerifyTaskSandboxStop(context.Background(), changed); err == nil {
				t.Fatal("accepted changed Task Sandbox stop proof")
			}
		})
	}
	if _, err := NewTaskSandboxGuardianStopAuthenticator(make(ed25519.PublicKey, ed25519.PublicKeySize)); err == nil {
		t.Fatal("accepted an all-zero dedicated Guardian key")
	}
	var nilContext context.Context
	if _, err := authenticator.VerifyTaskSandboxStop(nilContext, attestation); err == nil {
		t.Fatal("accepted a nil verification context")
	}
}

func TestTaskSandboxLifecycleServicePersistsOnlyExactGuardianProof(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	authenticator, err := NewTaskSandboxGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	seedAttestation := signedTaskSandboxStopForTest(t, privateKey)
	lifecycle := taskSandboxLifecycleForGuardianTest(seedAttestation, TaskSandboxStopping)
	lifecycle.NonceSHA256 = ""
	store := &taskSandboxGuardianStopStoreStub{lifecycle: lifecycle}
	service, err := NewTaskSandboxLifecycleService(store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	persisted, nonce, err := service.PrepareGuardianStopChallenge(context.Background(), lifecycle.OwnerID, lifecycle.TaskID, lifecycle.SandboxGeneration)
	if err != nil {
		t.Fatalf("prepare Guardian stop challenge: %v", err)
	}
	digest, err := TaskSandboxGuardianNonceSHA256(nonce)
	if err != nil || digest != persisted.NonceSHA256 {
		t.Fatalf("persisted Guardian challenge digest = %q err=%v", persisted.NonceSHA256, err)
	}
	attestation := signedTaskSandboxStopWithNonceAndExpiryForTest(t, privateKey, digest, *lifecycle.ExpiresAt)
	clear(nonce)
	oldAttestation := attestation
	rotatedLifecycle, rotatedNonce, err := service.PrepareGuardianStopChallenge(context.Background(), lifecycle.OwnerID, lifecycle.TaskID, lifecycle.SandboxGeneration)
	if err != nil {
		t.Fatalf("rotate Guardian stop challenge: %v", err)
	}
	rotatedDigest, err := TaskSandboxGuardianNonceSHA256(rotatedNonce)
	if err != nil || rotatedDigest == digest || rotatedLifecycle.NonceSHA256 != rotatedDigest {
		t.Fatalf("rotated Guardian challenge = %q err=%v", rotatedLifecycle.NonceSHA256, err)
	}
	if _, err := service.ConfirmGuardianStopped(context.Background(), oldAttestation); domain.CodeOf(err) != domain.CodeWorkspaceConflict || store.terminateCalls != 0 {
		t.Fatalf("accepted abandoned Guardian challenge: calls=%d err=%v", store.terminateCalls, err)
	}
	attestation = signedTaskSandboxStopWithNonceAndExpiryForTest(t, privateKey, rotatedDigest, *lifecycle.ExpiresAt)
	clear(rotatedNonce)
	terminated, err := service.ConfirmGuardianStopped(context.Background(), attestation)
	if err != nil {
		t.Fatalf("confirm Task Sandbox termination: %v", err)
	}
	if terminated.Status != TaskSandboxTerminated || store.terminateCalls != 1 || len(store.evidence) == 0 {
		t.Fatalf("terminated lifecycle=%#v terminate calls=%d evidence=%d", terminated, store.terminateCalls, len(store.evidence))
	}

	// The same proof is safe to replay after a response is lost.
	if _, err := service.ConfirmGuardianStopped(context.Background(), attestation); err != nil || store.terminateCalls != 2 {
		t.Fatalf("idempotent proof replay: calls=%d err=%v", store.terminateCalls, err)
	}

	wrongLifecycle := taskSandboxLifecycleForGuardianTest(attestation, TaskSandboxStopping)
	wrongLifecycle.SandboxID = "sandbox-other"
	wrongStore := &taskSandboxGuardianStopStoreStub{lifecycle: wrongLifecycle}
	wrongService, err := NewTaskSandboxLifecycleService(wrongStore, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrongService.ConfirmGuardianStopped(context.Background(), attestation); domain.CodeOf(err) != domain.CodeWorkspaceConflict || wrongStore.terminateCalls != 0 {
		t.Fatalf("accepted proof for another sandbox: calls=%d err=%v", wrongStore.terminateCalls, err)
	}

	wrongNonceLifecycle := taskSandboxLifecycleForGuardianTest(attestation, TaskSandboxStopping)
	wrongNonceLifecycle.NonceSHA256 = "a" + attestation.NonceSHA256[1:]
	if wrongNonceLifecycle.NonceSHA256 == attestation.NonceSHA256 {
		wrongNonceLifecycle.NonceSHA256 = "b" + attestation.NonceSHA256[1:]
	}
	wrongNonceStore := &taskSandboxGuardianStopStoreStub{lifecycle: wrongNonceLifecycle}
	wrongNonceService, err := NewTaskSandboxLifecycleService(wrongNonceStore, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrongNonceService.ConfirmGuardianStopped(context.Background(), attestation); domain.CodeOf(err) != domain.CodeWorkspaceConflict || wrongNonceStore.terminateCalls != 0 {
		t.Fatalf("accepted Guardian proof with a different persisted nonce: calls=%d err=%v", wrongNonceStore.terminateCalls, err)
	}
}

type taskSandboxGuardianStopStoreStub struct {
	lifecycle      TaskSandboxLifecycle
	evidence       []byte
	terminateCalls int
}

func (s *taskSandboxGuardianStopStoreStub) GetTaskSandboxLifecycle(_ context.Context, ownerID, taskID string,
	generation int64) (TaskSandboxLifecycle, error) {
	if ownerID != s.lifecycle.OwnerID || taskID != s.lifecycle.TaskID || generation != s.lifecycle.SandboxGeneration {
		return TaskSandboxLifecycle{}, domain.NewError(domain.CodeNotFound, "Task Sandbox lifecycle not found")
	}
	return s.lifecycle, nil
}

func (s *taskSandboxGuardianStopStoreStub) TerminateTaskSandbox(_ context.Context, lifecycle TaskSandboxLifecycle,
	evidence []byte) (TaskSandboxLifecycle, error) {
	s.terminateCalls++
	if s.lifecycle.Status == TaskSandboxTerminated {
		if string(s.evidence) != string(evidence) {
			return TaskSandboxLifecycle{}, domain.NewError(domain.CodeConflict, "Task Sandbox termination evidence differs")
		}
		return s.lifecycle, nil
	}
	if lifecycle.ID != s.lifecycle.ID || (s.lifecycle.Status != TaskSandboxStopping && s.lifecycle.Status != TaskSandboxUnknown) || len(evidence) == 0 {
		return TaskSandboxLifecycle{}, errors.New("unexpected Task Sandbox termination")
	}
	s.lifecycle.Status = TaskSandboxTerminated
	s.lifecycle.TerminationEvidence = append([]byte(nil), evidence...)
	s.evidence = append([]byte(nil), evidence...)
	now := time.Now()
	s.lifecycle.TerminatedAt = &now
	return s.lifecycle, nil
}

func (s *taskSandboxGuardianStopStoreStub) SetTaskSandboxGuardianStopChallenge(_ context.Context,
	lifecycle TaskSandboxLifecycle, nonceSHA256 string) (TaskSandboxLifecycle, error) {
	if s.lifecycle.ID != lifecycle.ID || s.lifecycle.TaskID != lifecycle.TaskID || s.lifecycle.OwnerID != lifecycle.OwnerID ||
		s.lifecycle.InitiatorID != lifecycle.InitiatorID || s.lifecycle.SandboxGeneration != lifecycle.SandboxGeneration ||
		s.lifecycle.SandboxID != lifecycle.SandboxID || !sameOptionalTaskSandboxLifecycleTime(s.lifecycle.ExpiresAt, lifecycle.ExpiresAt) ||
		s.lifecycle.LeaseOwner != lifecycle.LeaseOwner || s.lifecycle.LeaseVersion != lifecycle.LeaseVersion ||
		s.lifecycle.AuthorizationVersion != lifecycle.AuthorizationVersion || s.lifecycle.AgentInstanceID != lifecycle.AgentInstanceID ||
		s.lifecycle.AgentGeneration != lifecycle.AgentGeneration ||
		(s.lifecycle.Status != TaskSandboxStopping && s.lifecycle.Status != TaskSandboxUnknown) ||
		!isLowerHex(nonceSHA256) || len(nonceSHA256) != sha256.Size*2 {
		return TaskSandboxLifecycle{}, domain.NewError(domain.CodeWorkspaceConflict, "Task Sandbox Guardian challenge fence differs")
	}
	s.lifecycle.NonceSHA256 = nonceSHA256
	return s.lifecycle, nil
}

func sameOptionalTaskSandboxLifecycleTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func signedTaskSandboxStopForTest(t *testing.T, privateKey ed25519.PrivateKey) TaskSandboxGuardianStopAttestation {
	t.Helper()
	nonce, nonceSHA256, err := NewTaskSandboxGuardianNonce()
	if err != nil {
		t.Fatal(err)
	}
	clear(nonce)
	return signedTaskSandboxStopWithNonceForTest(t, privateKey, nonceSHA256)
}

func signedTaskSandboxStopWithNonceForTest(t *testing.T, privateKey ed25519.PrivateKey,
	nonceSHA256 string) TaskSandboxGuardianStopAttestation {
	t.Helper()
	return signedTaskSandboxStopWithNonceAndExpiryForTest(t, privateKey, nonceSHA256, time.Now().Add(time.Minute).UTC())
}

func signedTaskSandboxStopWithNonceAndExpiryForTest(t *testing.T, privateKey ed25519.PrivateKey,
	nonceSHA256 string, expiresAt time.Time) TaskSandboxGuardianStopAttestation {
	t.Helper()
	attestation := TaskSandboxGuardianStopAttestation{
		LifecycleID: "o1234567", TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567",
		LeaseOwner: "task-worker-1", LeaseVersion: 3, AuthorizationVersion: 5,
		AgentInstanceID: "agent-session-1", AgentGeneration: 7, SandboxID: "sbx_123456",
		SandboxGeneration: 2, ContainerID: strings.Repeat("a", 64), ContainerName: "sandbox-sbx_123456",
		NonceSHA256: nonceSHA256, ExpiresAt: expiresAt,
		DestroyedAt: time.Now().UTC(), Destroyed: true,
	}
	proof := sha256.Sum256([]byte("task sandbox container absent"))
	attestation.ProofSHA256 = hex.EncodeToString(proof[:])
	payload, err := TaskSandboxGuardianStopAttestationSigningPayload(attestation)
	if err != nil {
		t.Fatal(err)
	}
	attestation.Signature = ed25519.Sign(privateKey, payload)
	return attestation
}

func taskSandboxLifecycleForGuardianTest(attestation TaskSandboxGuardianStopAttestation,
	status TaskSandboxLifecycleStatus) TaskSandboxLifecycle {
	expiresAt := attestation.ExpiresAt
	return TaskSandboxLifecycle{
		ID: attestation.LifecycleID, TaskID: attestation.TaskID, OwnerID: attestation.OwnerID, InitiatorID: attestation.InitiatorID,
		LeaseOwner: attestation.LeaseOwner, LeaseVersion: attestation.LeaseVersion,
		AuthorizationVersion: attestation.AuthorizationVersion, AgentInstanceID: attestation.AgentInstanceID,
		AgentGeneration: attestation.AgentGeneration, SandboxID: attestation.SandboxID,
		SandboxGeneration: attestation.SandboxGeneration, NonceSHA256: attestation.NonceSHA256,
		Status: status, ExpiresAt: &expiresAt,
	}
}

func TestTaskSandboxGuardianNonceIsHighEntropyAndOnlyDigestIsDerived(t *testing.T) {
	first, firstDigest, err := NewTaskSandboxGuardianNonce()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(first)
	second, secondDigest, err := NewTaskSandboxGuardianNonce()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(second)
	if len(first) != 32 || len(second) != 32 || firstDigest == secondDigest {
		t.Fatalf("invalid or repeated Guardian nonce: first=%d second=%d", len(first), len(second))
	}
	derived, err := TaskSandboxGuardianNonceSHA256(first)
	if err != nil || derived != firstDigest {
		t.Fatalf("derive nonce digest = %q err=%v", derived, err)
	}
	if _, err := TaskSandboxGuardianNonceSHA256(make([]byte, 32)); err == nil {
		t.Fatal("accepted an all-zero Guardian nonce")
	}
}

func TestTaskSandboxMetadataCannotAdoptAnotherOwnerOrGeneration(t *testing.T) {
	attestation := TaskSandboxGuardianStopAttestation{
		LifecycleID: "o1234567", TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567",
		LeaseOwner: "task-worker-1", LeaseVersion: 3, AuthorizationVersion: 5,
		AgentInstanceID: "agent-session-1", AgentGeneration: 7, SandboxGeneration: 2,
	}
	lifecycle := TaskSandboxLifecycle{
		ID: attestation.LifecycleID, TaskID: attestation.TaskID, OwnerID: attestation.OwnerID, InitiatorID: attestation.InitiatorID,
		LeaseOwner: attestation.LeaseOwner, LeaseVersion: attestation.LeaseVersion,
		AuthorizationVersion: attestation.AuthorizationVersion, AgentInstanceID: attestation.AgentInstanceID,
		AgentGeneration: attestation.AgentGeneration, SandboxGeneration: attestation.SandboxGeneration,
	}
	metadata, err := TaskSandboxLifecycleMetadata(lifecycle)
	if err != nil {
		t.Fatalf("create Task Sandbox metadata: %v", err)
	}
	sandbox := Sandbox{ID: "sbx_123456", Metadata: metadata}
	if !TaskSandboxMatchesLifecycle(sandbox, lifecycle) {
		t.Fatal("provider sandbox metadata did not match its Task lifecycle")
	}
	for name, mutate := range map[string]func(map[string]string){
		"owner":      func(value map[string]string) { value[taskSandboxMetadataOwnerID] = "u7654321" },
		"task":       func(value map[string]string) { value[taskSandboxMetadataTaskID] = "q7654321" },
		"generation": func(value map[string]string) { value[taskSandboxMetadataGeneration] = "3" },
		"lifecycle":  func(value map[string]string) { value[taskSandboxMetadataLifecycle] = "o7654321" },
		"managed-by": func(value map[string]string) { value[taskSandboxMetadataManagedBy] = "another-service" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := make(map[string]string, len(metadata))
			for key, value := range metadata {
				changed[key] = value
			}
			mutate(changed)
			if TaskSandboxMatchesLifecycle(Sandbox{ID: sandbox.ID, Metadata: changed}, lifecycle) {
				t.Fatal("accepted provider metadata from another Task sandbox lifecycle")
			}
		})
	}
	otherSandbox := Sandbox{ID: "sbx_765432", Metadata: metadata}
	boundLifecycle := lifecycle
	boundLifecycle.SandboxID = sandbox.ID
	if TaskSandboxMatchesLifecycle(otherSandbox, boundLifecycle) {
		t.Fatal("accepted a different provider sandbox ID after the lifecycle was bound")
	}
}
