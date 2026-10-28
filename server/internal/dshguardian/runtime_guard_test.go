package dshguardian

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
)

func TestRuntimeAndTaskStopAttestationsHaveSeparateScopesAndPersist(t *testing.T) {
	now := time.Now().UTC()
	runtimeGuard := testAccountRuntimeGuard(now)
	taskGuard := testTaskProcessGuard(runtimeGuard, now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	runtimeProof := processProof{PID: 321, StartTimeTick: 88, CgroupPath: "/system.slice/docker-" + runtimeGuard.ContainerID + ".scope"}
	processes := &fakeProcessInspector{proof: runtimeProof, gone: true, killTreeClears: true}
	taskProcesses := &fakeTaskProcessInspector{proof: taskProcessProof{
		ContainerPID: taskGuard.ContainerPID, HostPID: 654, StartTimeTick: 1001,
		CgroupPath: taskCgroupPath(runtimeProof.CgroupPath, taskProcessKey(taskGuard)), NamespaceInode: 77,
	}}
	docker := newFakeAccountRuntimeDocker(runtimeGuard, false, 0)
	service := newService(store, docker, combinedFakeProcessInspector{processes, taskProcesses}, func() time.Time { return now })

	if err := service.ArmAccountRuntime(context.Background(), runtimeGuard); err != nil {
		t.Fatalf("arm account Runtime: %v", err)
	}
	docker.setRunning(runtimeProof.PID)
	if err := service.StartedAccountRuntime(context.Background(), runtimeGuard); err != nil {
		t.Fatalf("record account Runtime process: %v", err)
	}
	if err := service.ArmTaskProcess(context.Background(), taskGuard); err != nil {
		t.Fatalf("arm child Task cgroup: %v", err)
	}
	duplicateTaskGuard := taskGuard
	duplicateTaskGuard.NonceSHA256 = testNonceHash("different-task-nonce")
	if err := service.ArmTaskProcess(context.Background(), duplicateTaskGuard); err == nil {
		t.Fatal("Guardian accepted a second process nonce for the same AgentInstance fence")
	}
	if err := service.StartedTaskProcess(context.Background(), taskGuard); err != nil {
		t.Fatalf("record Task start: %v", err)
	}
	if err := service.AcknowledgeTaskProcessStopped(context.Background(), runtimeGuard.Fence, taskGuard.TaskFence,
		taskGuard.NonceSHA256, strings.Repeat("a", 64)); err == nil {
		t.Fatal("Guardian acknowledged a Task process before destruction was proven")
	}
	if _, ok := store.getTask(taskProcessKey(taskGuard)); !ok {
		t.Fatal("unconfirmed Task proof acknowledgement removed the Task lease")
	}
	taskAttestation, err := service.StopTaskProcess(context.Background(), taskGuard)
	if err != nil {
		t.Fatalf("stop one Task process: %v", err)
	}
	if !taskAttestation.Destroyed || taskAttestation.RuntimeFence != runtimeGuard.Fence || taskAttestation.TaskFence != taskGuard.TaskFence ||
		taskAttestation.RuntimeContainerID != runtimeGuard.ContainerID || taskAttestation.HostPID != 654 || taskAttestation.ContainerPID != taskGuard.ContainerPID ||
		taskAttestation.CgroupPath != taskProcesses.proof.CgroupPath || docker.killCalls != 0 || !docker.exists || taskProcesses.killCalls != 1 {
		t.Fatalf("Task stop crossed its cgroup scope or omitted identity: attestation=%+v docker=%+v task=%+v", taskAttestation, docker, taskProcesses)
	}
	verifyTaskAttestation(t, taskAttestation)

	// The task lease and signed proof survive a Guardian process restart.
	reopened, err := openLeaseStore(store.dir)
	if err != nil {
		t.Fatalf("reopen Guardian state: %v", err)
	}
	restarted := newService(reopened, docker, combinedFakeProcessInspector{processes, taskProcesses}, func() time.Time { return now })
	replayedTask, err := restarted.RecoverTaskProcessStopped(context.Background(), runtimeGuard.Fence, taskGuard.TaskFence, taskGuard.NonceSHA256)
	if err != nil || string(replayedTask.Signature) != string(taskAttestation.Signature) || replayedTask.ProofSHA256 != taskAttestation.ProofSHA256 {
		t.Fatalf("recovered Task proof changed: proof=%+v err=%v", replayedTask, err)
	}
	taskLeasePath := filepath.Join(store.dir, "task-"+taskProcessKey(taskGuard)+".json")
	wrongNonce := testNonceHash("wrong-task-nonce")
	if err := restarted.AcknowledgeTaskProcessStopped(context.Background(), runtimeGuard.Fence, taskGuard.TaskFence,
		wrongNonce, replayedTask.ProofSHA256); err == nil {
		t.Fatal("Guardian acknowledged a Task proof under a different nonce")
	}
	if _, err := os.Stat(taskLeasePath); err != nil {
		t.Fatalf("wrong nonce removed the durable Task lease: %v", err)
	}
	wrongDigest := strings.Repeat("f", 64)
	if wrongDigest == replayedTask.ProofSHA256 {
		wrongDigest = strings.Repeat("e", 64)
	}
	if err := restarted.AcknowledgeTaskProcessStopped(context.Background(), runtimeGuard.Fence, taskGuard.TaskFence,
		taskGuard.NonceSHA256, wrongDigest); err == nil {
		t.Fatal("Guardian acknowledged a Task stop proof with a different digest")
	}
	if _, err := os.Stat(taskLeasePath); err != nil {
		t.Fatalf("wrong proof digest removed the durable Task lease: %v", err)
	}
	if _, ok := reopened.getTask(taskProcessKey(taskGuard)); !ok {
		t.Fatal("wrong proof digest removed the in-memory Task lease")
	}
	if err := restarted.AcknowledgeTaskProcessStopped(context.Background(), runtimeGuard.Fence, taskGuard.TaskFence,
		taskGuard.NonceSHA256, replayedTask.ProofSHA256); err != nil {
		t.Fatalf("acknowledge persisted Task stop proof: %v", err)
	}
	if _, err := os.Stat(taskLeasePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("acknowledged Task lease file remains: err=%v", err)
	}
	if _, ok := reopened.getTask(taskProcessKey(taskGuard)); ok {
		t.Fatal("acknowledged Task lease remains in memory")
	}
	if err := restarted.AcknowledgeTaskProcessStopped(context.Background(), runtimeGuard.Fence, taskGuard.TaskFence,
		taskGuard.NonceSHA256, replayedTask.ProofSHA256); err != nil {
		t.Fatalf("repeated Task stop proof acknowledgement: %v", err)
	}
	afterAckRestart, err := openLeaseStore(store.dir)
	if err != nil {
		t.Fatalf("reopen Guardian state after Task proof acknowledgement: %v", err)
	}
	restartedAfterAck := newService(afterAckRestart, docker, combinedFakeProcessInspector{processes, taskProcesses}, func() time.Time { return now })
	if err := restartedAfterAck.AcknowledgeTaskProcessStopped(context.Background(), runtimeGuard.Fence, taskGuard.TaskFence,
		taskGuard.NonceSHA256, replayedTask.ProofSHA256); err != nil {
		t.Fatalf("repeated Task stop proof acknowledgement after Guardian restart: %v", err)
	}

	runtimeAttestation, err := restartedAfterAck.StopAccountRuntime(context.Background(), runtimeGuard.Fence, runtimeGuard.NonceSHA256)
	if err != nil {
		t.Fatalf("stop account Runtime: %v", err)
	}
	if !runtimeAttestation.Destroyed || runtimeAttestation.Fence != runtimeGuard.Fence || runtimeAttestation.ContainerID != runtimeGuard.ContainerID ||
		runtimeAttestation.ContainerName != runtimeGuard.ContainerName || runtimeAttestation.NonceSHA256 != runtimeGuard.NonceSHA256 ||
		!runtimeAttestation.DestroyedAt.After(now.Add(-time.Second)) || docker.exists || docker.killCalls != 1 || processes.killTreeCalls != 1 {
		t.Fatalf("Runtime stop scope or proof is invalid: attestation=%+v docker=%+v processes=%+v", runtimeAttestation, docker, processes)
	}
	verifyRuntimeAttestation(t, runtimeAttestation)
	replayedRuntime, err := restartedAfterAck.RecoverAccountRuntimeStopped(context.Background(), runtimeGuard.Fence, runtimeGuard.NonceSHA256)
	if err != nil || string(replayedRuntime.Signature) != string(runtimeAttestation.Signature) || replayedRuntime.ProofSHA256 != runtimeAttestation.ProofSHA256 {
		t.Fatalf("recovered Runtime proof changed: proof=%+v err=%v", replayedRuntime, err)
	}
}

func TestGuardianClientRunsRuntimeAndTaskControlRoutes(t *testing.T) {
	now := time.Now().UTC()
	runtimeGuard := testAccountRuntimeGuard(now)
	taskGuard := testTaskProcessGuard(runtimeGuard, now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	runtimeProof := processProof{PID: 321, StartTimeTick: 88, CgroupPath: "/system.slice/docker-" + runtimeGuard.ContainerID + ".scope"}
	processes := &fakeProcessInspector{proof: runtimeProof, gone: true, killTreeClears: true}
	taskProcesses := &fakeTaskProcessInspector{proof: taskProcessProof{
		ContainerPID: taskGuard.ContainerPID, HostPID: 654, StartTimeTick: 1001,
		CgroupPath: taskCgroupPath(runtimeProof.CgroupPath, taskProcessKey(taskGuard)), NamespaceInode: 77,
	}}
	docker := newFakeAccountRuntimeDocker(runtimeGuard, false, 0)
	service := newService(store, docker, combinedFakeProcessInspector{processes, taskProcesses}, func() time.Time { return now })
	client, err := NewClient(startUnixHTTPServer(t, service.Handler()))
	if err != nil {
		t.Fatal(err)
	}

	if err := client.ArmAccountRuntime(context.Background(), runtimeGuard); err != nil {
		t.Fatalf("client arm account Runtime: %v", err)
	}
	docker.setRunning(runtimeProof.PID)
	if err := client.StartedAccountRuntime(context.Background(), runtimeGuard); err != nil {
		t.Fatalf("client record account Runtime: %v", err)
	}
	lookedUp, err := client.LookupAccountRuntime(context.Background(), runtimeGuard.Fence, runtimeGuard.NonceSHA256)
	if err != nil || lookedUp != runtimeGuard {
		t.Fatalf("client lookup account Runtime: guard=%+v err=%v", lookedUp, err)
	}
	if err := client.RenewAccountRuntime(context.Background(), runtimeGuard); err != nil {
		t.Fatalf("client renew account Runtime: %v", err)
	}
	if err := client.RenewAccountRuntimeByIdentity(context.Background(), runtimeGuard.Fence, runtimeGuard.NonceSHA256, now.Add(25*time.Second)); err != nil {
		t.Fatalf("client renew account Runtime by recovery identity: %v", err)
	}
	if err := client.ArmTaskProcess(context.Background(), taskGuard); err != nil {
		t.Fatalf("client arm Task process: %v", err)
	}
	if err := client.StartedTaskProcess(context.Background(), taskGuard); err != nil {
		t.Fatalf("client record Task start: %v", err)
	}
	if err := client.RenewTaskProcess(context.Background(), taskGuard); err != nil {
		t.Fatalf("client renew Task: %v", err)
	}
	if _, err := client.StopTaskProcess(context.Background(), taskGuard); err != nil {
		t.Fatalf("client stop Task: %v", err)
	}
	taskProof, err := client.RecoverTaskProcessStopped(context.Background(), runtimeGuard.Fence, taskGuard.TaskFence, taskGuard.NonceSHA256)
	if err != nil {
		t.Fatalf("client recover Task proof: %v", err)
	}
	wrongProofDigest := strings.Repeat("f", 64)
	if wrongProofDigest == taskProof.ProofSHA256 {
		wrongProofDigest = strings.Repeat("e", 64)
	}
	if err := client.AcknowledgeTaskProcessStopped(context.Background(), runtimeGuard.Fence, taskGuard.TaskFence,
		taskGuard.NonceSHA256, wrongProofDigest); err == nil {
		t.Fatal("Guardian HTTP client accepted a different Task proof digest")
	}
	if err := client.AcknowledgeTaskProcessStopped(context.Background(), runtimeGuard.Fence, taskGuard.TaskFence,
		taskGuard.NonceSHA256, taskProof.ProofSHA256); err != nil {
		t.Fatalf("Guardian HTTP client acknowledged Task proof: %v", err)
	}
	if err := client.AcknowledgeTaskProcessStopped(context.Background(), runtimeGuard.Fence, taskGuard.TaskFence,
		taskGuard.NonceSHA256, taskProof.ProofSHA256); err != nil {
		t.Fatalf("Guardian HTTP client repeated Task proof acknowledgement: %v", err)
	}
	if _, err := client.StopAccountRuntime(context.Background(), runtimeGuard.Fence, runtimeGuard.NonceSHA256); err != nil {
		t.Fatalf("client stop account Runtime: %v", err)
	}
	if _, err := client.RecoverAccountRuntimeStopped(context.Background(), runtimeGuard.Fence, runtimeGuard.NonceSHA256); err != nil {
		t.Fatalf("client recover account Runtime proof: %v", err)
	}
}

func TestAccountRuntimeAndTaskHTTPRejectMalformedAndUnknownLeases(t *testing.T) {
	now := time.Now().UTC()
	runtimeGuard := testAccountRuntimeGuard(now)
	taskGuard := testTaskProcessGuard(runtimeGuard, now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	handler := newService(store, nil, nil, func() time.Time { return now }).Handler()
	runtimeBody, err := json.Marshal(runtimeGuard)
	if err != nil {
		t.Fatal(err)
	}
	taskBody, err := json.Marshal(taskGuard)
	if err != nil {
		t.Fatal(err)
	}
	taskPath := "/v1/task-processes/" + runtimeGuard.ContainerID + "/" + taskProcessKey(taskGuard)
	cases := []struct {
		name, method, path, unknownBody string
	}{
		{"arm Runtime", http.MethodPost, "/v1/account-runtimes", "{}"},
		{"ack Runtime", http.MethodPost, "/v1/account-runtimes/ack-stop", "{}"},
		{"lookup Runtime", http.MethodPost, "/v1/account-runtimes/lookup", "{}"},
		{"renew Runtime identity", http.MethodPut, "/v1/account-runtimes/renew-by-identity", "{}"},
		{"stop Runtime", http.MethodPost, "/v1/account-runtimes/stop", "{}"},
		{"recover Runtime stop", http.MethodPost, "/v1/account-runtimes/recover-stop", "{}"},
		{"start Runtime", http.MethodPost, "/v1/account-runtimes/" + runtimeGuard.ContainerID + "/started", string(runtimeBody)},
		{"renew Runtime", http.MethodPut, "/v1/account-runtimes/" + runtimeGuard.ContainerID + "/renew", string(runtimeBody)},
		{"arm Task", http.MethodPost, "/v1/task-processes", "{}"},
		{"ack Task", http.MethodPost, "/v1/task-processes/ack-stop", "{}"},
		{"recover Task stop", http.MethodPost, "/v1/task-processes/recover-stop", "{}"},
		{"start Task", http.MethodPost, taskPath + "/started", string(taskBody)},
		{"renew Task", http.MethodPut, taskPath + "/renew", string(taskBody)},
		{"stop Task", http.MethodPost, taskPath + "/stop", string(taskBody)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, attempt := range []struct {
				body   string
				status int
			}{{"{", http.StatusBadRequest}, {tc.unknownBody, http.StatusConflict}} {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, strings.NewReader(attempt.body)))
				if recorder.Code != attempt.status || recorder.Header().Get("Content-Type") != "application/json" {
					t.Fatalf("%s %s status=%d content-type=%q, want %d JSON", tc.method, tc.path,
						recorder.Code, recorder.Header().Get("Content-Type"), attempt.status)
				}
				var result struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil || result.Error == "" {
					t.Fatalf("%s %s returned no structured error: %q, %v", tc.method, tc.path, recorder.Body.String(), err)
				}
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.path, strings.NewReader(tc.unknownBody)))
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("GET %s status=%d, want not found", tc.path, recorder.Code)
			}
		})
	}
	for _, tc := range []struct {
		path, body string
	}{
		{"/v1/account-runtimes/" + strings.Repeat("d", 64) + "/started", string(runtimeBody)},
		{"/v1/task-processes/" + strings.Repeat("d", 64) + "/" + taskProcessKey(taskGuard) + "/started", string(taskBody)},
		{"/v1/task-processes/" + runtimeGuard.ContainerID + "/" + strings.Repeat("e", 64) + "/started", string(taskBody)},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("foreign lease path %s status=%d, want bad request", tc.path, recorder.Code)
		}
	}
}

func TestAcknowledgeAccountRuntimeStoppedCleansOnlyExactRuntimeRecords(t *testing.T) {
	now := time.Now().UTC()
	runtimeGuard := testAccountRuntimeGuard(now)
	runtimeLease := accountRuntimeLease{Guard: runtimeGuard, Destroyed: true, DestroyedAt: now}
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.createRuntime(runtimeLease); err != nil {
		t.Fatalf("persist destroyed Runtime proof: %v", err)
	}
	taskGuard := testTaskProcessGuard(runtimeGuard, now)
	secondTaskGuard := taskGuard
	secondTaskGuard.TaskFence.TaskID = "q2345678"
	secondTaskGuard.TaskFence.AgentInstanceID = "agent-2"
	secondTaskGuard.TaskFence.AgentSessionID = "session-2"
	otherRuntimeGuard := testAccountRuntimeGuard(now)
	otherRuntimeGuard.Fence.RuntimeID = "runtime-2"
	otherRuntimeGuard.Fence.RuntimeGeneration = 2
	otherRuntimeGuard.NonceSHA256 = testNonceHash("other-runtime-nonce")
	otherRuntimeGuard.ContainerID = strings.Repeat("d", 64)
	otherRuntimeGuard.ContainerName = expectedAccountRuntimeContainerName(otherRuntimeGuard.Fence, otherRuntimeGuard.NonceSHA256)
	otherTaskGuard := testTaskProcessGuard(otherRuntimeGuard, now)
	otherTaskGuard.TaskFence.TaskID = "q3456789"
	otherTaskGuard.TaskFence.AgentInstanceID = "agent-3"
	otherTaskGuard.TaskFence.AgentSessionID = "session-3"
	for _, guard := range []TaskProcessGuard{taskGuard, secondTaskGuard, otherTaskGuard} {
		value := taskProcessLease{Guard: guard, Key: taskProcessKey(guard)}
		if err := store.createTask(value); err != nil {
			t.Fatalf("persist Task lease %s: %v", guard.TaskFence.TaskID, err)
		}
	}
	if err := store.createRuntime(accountRuntimeLease{Guard: otherRuntimeGuard}); err != nil {
		t.Fatalf("persist unrelated Runtime lease: %v", err)
	}
	canonical, err := json.Marshal(runtimeLease)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	proofSHA256 := hex.EncodeToString(digest[:])
	service := newService(store, nil, nil, func() time.Time { return now })
	runtimePath := filepath.Join(store.dir, "runtime-"+runtimeGuard.ContainerID+".json")
	taskPath := filepath.Join(store.dir, "task-"+taskProcessKey(taskGuard)+".json")
	secondTaskPath := filepath.Join(store.dir, "task-"+taskProcessKey(secondTaskGuard)+".json")
	otherRuntimePath := filepath.Join(store.dir, "runtime-"+otherRuntimeGuard.ContainerID+".json")
	otherTaskPath := filepath.Join(store.dir, "task-"+taskProcessKey(otherTaskGuard)+".json")
	wrongNonce := testNonceHash("wrong-runtime-nonce")
	if err := service.AcknowledgeAccountRuntimeStopped(context.Background(), runtimeGuard.Fence, wrongNonce, proofSHA256); err == nil {
		t.Fatal("Guardian acknowledged a Runtime proof under a different nonce")
	}
	wrongFence := runtimeGuard.Fence
	wrongFence.RuntimeGeneration++
	if err := service.AcknowledgeAccountRuntimeStopped(context.Background(), wrongFence, runtimeGuard.NonceSHA256, proofSHA256); err == nil {
		t.Fatal("Guardian acknowledged a Runtime proof under a different fence")
	}
	wrongDigest := strings.Repeat("f", 64)
	if wrongDigest == proofSHA256 {
		wrongDigest = strings.Repeat("e", 64)
	}
	if err := service.AcknowledgeAccountRuntimeStopped(context.Background(), runtimeGuard.Fence, runtimeGuard.NonceSHA256, wrongDigest); err == nil {
		t.Fatal("Guardian acknowledged a Runtime proof with a different digest")
	}
	for _, path := range []string{runtimePath, taskPath, secondTaskPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("rejected acknowledgement removed %s: %v", filepath.Base(path), err)
		}
	}

	// A malformed member must block cleanup of the entire Task-record group.
	if err := os.WriteFile(secondTaskPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.AcknowledgeAccountRuntimeStopped(context.Background(), runtimeGuard.Fence, runtimeGuard.NonceSHA256, proofSHA256); err == nil {
		t.Fatal("Guardian acknowledged a Runtime proof with a mismatched Task record")
	}
	for _, path := range []string{runtimePath, taskPath, secondTaskPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("failed group validation removed %s: %v", filepath.Base(path), err)
		}
	}
	secondTask := store.taskProcess[taskProcessKey(secondTaskGuard)]
	if err := store.persistNamed("task-"+secondTask.Key+".json", secondTask, validPersistedTaskProcessLease(secondTask)); err != nil {
		t.Fatalf("restore Task record after validation test: %v", err)
	}

	if err := service.AcknowledgeAccountRuntimeStopped(context.Background(), runtimeGuard.Fence, runtimeGuard.NonceSHA256, proofSHA256); err != nil {
		t.Fatalf("acknowledge persisted Runtime proof: %v", err)
	}
	for _, path := range []string{runtimePath, taskPath, secondTaskPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("acknowledged Runtime record remains at %s: %v", filepath.Base(path), err)
		}
	}
	if _, ok := store.getRuntime(runtimeGuard.ContainerID); ok {
		t.Fatal("acknowledged Runtime lease remains in memory")
	}
	if _, ok := store.getTask(taskProcessKey(taskGuard)); ok {
		t.Fatal("acknowledged Task lease remains in memory")
	}
	if _, ok := store.getTask(taskProcessKey(secondTaskGuard)); ok {
		t.Fatal("acknowledged second Task lease remains in memory")
	}
	for _, path := range []string{otherRuntimePath, otherTaskPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("acknowledgement removed unrelated record %s: %v", filepath.Base(path), err)
		}
	}
	if _, ok := store.getRuntime(otherRuntimeGuard.ContainerID); !ok {
		t.Fatal("acknowledgement removed unrelated Runtime lease from memory")
	}
	if _, ok := store.getTask(taskProcessKey(otherTaskGuard)); !ok {
		t.Fatal("acknowledgement removed unrelated Task lease from memory")
	}

	reopened, err := openLeaseStore(store.dir)
	if err != nil {
		t.Fatalf("reopen state after Runtime acknowledgement: %v", err)
	}
	restarted := newService(reopened, nil, nil, func() time.Time { return now })
	if err := restarted.AcknowledgeAccountRuntimeStopped(context.Background(), runtimeGuard.Fence, runtimeGuard.NonceSHA256, proofSHA256); err != nil {
		t.Fatalf("repeated Runtime proof acknowledgement after Guardian restart: %v", err)
	}
}

func TestGuardianClientAcknowledgesAccountRuntimeStopOverUnixSocket(t *testing.T) {
	now := time.Now().UTC()
	runtimeGuard := testAccountRuntimeGuard(now)
	runtimeLease := accountRuntimeLease{Guard: runtimeGuard, Destroyed: true, DestroyedAt: now}
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.createRuntime(runtimeLease); err != nil {
		t.Fatal(err)
	}
	taskGuard := testTaskProcessGuard(runtimeGuard, now)
	if err := store.createTask(taskProcessLease{Guard: taskGuard, Key: taskProcessKey(taskGuard)}); err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(runtimeLease)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	proofSHA256 := hex.EncodeToString(digest[:])
	service := newService(store, nil, nil, func() time.Time { return now })
	client, err := NewClient(startUnixHTTPServer(t, service.Handler()))
	if err != nil {
		t.Fatal(err)
	}
	wrongDigest := strings.Repeat("a", 64)
	if wrongDigest == proofSHA256 {
		wrongDigest = strings.Repeat("b", 64)
	}
	if err := client.AcknowledgeAccountRuntimeStopped(context.Background(), runtimeGuard.Fence, runtimeGuard.NonceSHA256, wrongDigest); err == nil {
		t.Fatal("Guardian HTTP client accepted a different Runtime proof digest")
	}
	if err := client.AcknowledgeAccountRuntimeStopped(context.Background(), runtimeGuard.Fence, runtimeGuard.NonceSHA256, proofSHA256); err != nil {
		t.Fatalf("Guardian HTTP client acknowledged Runtime proof: %v", err)
	}
	if err := client.AcknowledgeAccountRuntimeStopped(context.Background(), runtimeGuard.Fence, runtimeGuard.NonceSHA256, proofSHA256); err != nil {
		t.Fatalf("Guardian HTTP client repeated Runtime proof acknowledgement: %v", err)
	}
	for _, name := range []string{"runtime-" + runtimeGuard.ContainerID + ".json", "task-" + taskProcessKey(taskGuard) + ".json"} {
		if _, err := os.Stat(filepath.Join(store.dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("HTTP acknowledgement left %s: %v", name, err)
		}
	}
}

func TestAllRuntimesReturnsEachLeaseOnce(t *testing.T) {
	now := time.Now().UTC()
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	guard := testAccountRuntimeGuard(now)
	if err := store.createRuntime(accountRuntimeLease{Guard: guard}); err != nil {
		t.Fatal(err)
	}
	values := store.allRuntimes()
	if len(values) != 1 || values[0].Guard != guard {
		t.Fatalf("allRuntimes returned duplicate or missing leases: %+v", values)
	}
}

func TestAccountRuntimeArmRejectsForeignContainerAndDuplicateFence(t *testing.T) {
	now := time.Now().UTC()
	guard := testAccountRuntimeGuard(now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	processes := &fakeProcessInspector{}
	docker := newFakeAccountRuntimeDocker(guard, false, 0)
	service := newService(store, docker, processes, func() time.Time { return now })

	docker.value.Config.Labels["morde.account-agent-runtime.nonce-sha256"] = testNonceHash("foreign")
	if err := service.ArmAccountRuntime(context.Background(), guard); err == nil {
		t.Fatal("Guardian armed a container with a foreign Runtime nonce label")
	}
	if _, found := store.getRuntime(guard.ContainerID); found {
		t.Fatal("rejected container left a durable Runtime lease")
	}
	docker.value.Config.Labels["morde.account-agent-runtime.nonce-sha256"] = guard.NonceSHA256
	docker.setRunning(321)
	if err := service.ArmAccountRuntime(context.Background(), guard); err == nil {
		t.Fatal("Guardian armed an already-running account Runtime")
	}
	*docker.value.State.Running = false
	*docker.value.State.PID = 0
	if err := service.ArmAccountRuntime(context.Background(), guard); err != nil {
		t.Fatalf("arm exact stopped Runtime: %v", err)
	}
	if err := service.ArmAccountRuntime(context.Background(), guard); err != nil {
		t.Fatalf("retry exact Runtime arm: %v", err)
	}
	other := guard
	other.ContainerID = strings.Repeat("d", 64)
	otherDocker := newFakeAccountRuntimeDocker(other, false, 0)
	otherService := newService(store, otherDocker, processes, func() time.Time { return now })
	if err := otherService.ArmAccountRuntime(context.Background(), other); err == nil {
		t.Fatal("Guardian armed a second container under the same Runtime fence")
	}
	if _, found := store.getRuntime(other.ContainerID); found {
		t.Fatal("duplicate Runtime fence left a second durable lease")
	}
}

func TestAccountRuntimeMissingContainerRequiresRecoveredProcessProof(t *testing.T) {
	now := time.Now().UTC()
	guard := testAccountRuntimeGuard(now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	proof := processProof{PID: 321, StartTimeTick: 88, CgroupPath: "/system.slice/docker-" + guard.ContainerID + ".scope"}
	if err := store.createRuntime(accountRuntimeLease{Guard: guard, Process: proof, Started: true}); err != nil {
		t.Fatal(err)
	}
	processes := &fakeProcessInspector{proof: proof, killTreeErr: errors.New("cgroup kill unavailable")}
	docker := newFakeAccountRuntimeDocker(guard, true, proof.PID)
	docker.exists = false
	service := newService(store, docker, processes, func() time.Time { return now })
	if _, err := service.StopAccountRuntime(context.Background(), guard.Fence, guard.NonceSHA256); err == nil {
		t.Fatal("Guardian signed a stop proof while the vanished container's process tree was unproven")
	}
	value, found := store.getRuntime(guard.ContainerID)
	if !found || !value.UnprovenRunning || value.Destroyed {
		t.Fatalf("uncertain process state was not retained: %+v found=%t", value, found)
	}

	reopened, err := openLeaseStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	processes.killTreeErr = nil
	processes.killTreeClears = true
	restarted := newService(reopened, docker, processes, func() time.Time { return now })
	attestation, err := restarted.RecoverAccountRuntimeStopped(context.Background(), guard.Fence, guard.NonceSHA256)
	if err != nil || !attestation.Destroyed || processes.killTreeCalls != 2 {
		t.Fatalf("Guardian did not recheck the vanished container's process tree: proof=%+v err=%v kills=%d", attestation, err, processes.killTreeCalls)
	}
	verifyRuntimeAttestation(t, attestation)
	value, found = reopened.getRuntime(guard.ContainerID)
	if !found || value.UnprovenRunning || !value.Destroyed {
		t.Fatalf("recovered destruction proof was not durable: %+v found=%t", value, found)
	}
}

func TestAccountRuntimeStopRecoversProcessStartedBeforePortalRecordedIt(t *testing.T) {
	for _, captureWorks := range []bool{true, false} {
		name := "capture-unavailable"
		if captureWorks {
			name = "capture-and-stop"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Now().UTC()
			guard := testAccountRuntimeGuard(now)
			store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.createRuntime(accountRuntimeLease{Guard: guard}); err != nil {
				t.Fatal(err)
			}
			proof := processProof{PID: 321, StartTimeTick: 88, CgroupPath: "/system.slice/docker-" + guard.ContainerID + ".scope"}
			processes := &fakeProcessInspector{proof: proof, gone: true, killTreeClears: true}
			if !captureWorks {
				processes.proof = processProof{}
			}
			docker := newFakeAccountRuntimeDocker(guard, true, proof.PID)
			service := newService(store, docker, processes, func() time.Time { return now })
			attestation, err := service.StopAccountRuntime(context.Background(), guard.Fence, guard.NonceSHA256)
			value, found := store.getRuntime(guard.ContainerID)
			if captureWorks {
				if err != nil || !attestation.Destroyed || !found || !value.Destroyed || value.Process != proof ||
					processes.killTreeCalls != 1 || docker.exists {
					t.Fatalf("unrecorded process was not independently stopped: proof=%+v err=%v lease=%+v", attestation, err, value)
				}
				verifyRuntimeAttestation(t, attestation)
				return
			}
			if err == nil || !found || !value.UnprovenRunning || value.Destroyed || processes.killTreeCalls != 0 {
				t.Fatalf("unverified process was reported destroyed: proof=%+v err=%v lease=%+v", attestation, err, value)
			}
		})
	}
}

func TestTaskProcessStopFailureRetainsLeaseUntilRecovery(t *testing.T) {
	now := time.Now().UTC()
	runtimeGuard := testAccountRuntimeGuard(now)
	taskGuard := testTaskProcessGuard(runtimeGuard, now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	runtimeProof := processProof{PID: 321, StartTimeTick: 88, CgroupPath: "/system.slice/docker-" + runtimeGuard.ContainerID + ".scope"}
	processes := &fakeProcessInspector{proof: runtimeProof, gone: true}
	taskProof := taskProcessProof{ContainerPID: taskGuard.ContainerPID, HostPID: 654, StartTimeTick: 1001,
		CgroupPath: taskCgroupPath(runtimeProof.CgroupPath, taskProcessKey(taskGuard)), NamespaceInode: 77}
	taskProcesses := &fakeTaskProcessInspector{proof: taskProof}
	docker := newFakeAccountRuntimeDocker(runtimeGuard, false, 0)
	service := newService(store, docker, combinedFakeProcessInspector{processes, taskProcesses}, func() time.Time { return now })
	if err := service.ArmAccountRuntime(context.Background(), runtimeGuard); err != nil {
		t.Fatal(err)
	}
	docker.setRunning(runtimeProof.PID)
	if err := service.StartedAccountRuntime(context.Background(), runtimeGuard); err != nil {
		t.Fatal(err)
	}
	if err := service.ArmTaskProcess(context.Background(), taskGuard); err != nil {
		t.Fatal(err)
	}
	if err := service.StartedTaskProcess(context.Background(), taskGuard); err != nil {
		t.Fatal(err)
	}
	taskProcesses.proof.HostPID++
	if _, err := service.StopTaskProcess(context.Background(), taskGuard); err == nil {
		t.Fatal("Guardian signed a Task stop proof after cgroup identity changed")
	}
	value, found := store.getTask(taskProcessKey(taskGuard))
	if !found || !value.UnprovenRunning || value.Destroyed || !docker.exists {
		t.Fatalf("uncertain Task stop affected its Runtime or lost the lease: %+v found=%t", value, found)
	}

	reopened, err := openLeaseStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	taskProcesses.proof = taskProof
	restarted := newService(reopened, docker, combinedFakeProcessInspector{processes, taskProcesses}, func() time.Time { return now })
	attestation, err := restarted.RecoverTaskProcessStopped(context.Background(), runtimeGuard.Fence, taskGuard.TaskFence, taskGuard.NonceSHA256)
	if err != nil || !attestation.Destroyed || taskProcesses.killCalls != 1 || !docker.exists {
		t.Fatalf("Task recovery did not independently prove its cgroup stopped: proof=%+v err=%v task=%+v docker=%+v", attestation, err, taskProcesses, docker)
	}
	verifyTaskAttestation(t, attestation)
}

func TestAccountAndTaskLeaseRenewalRequireUnchangedHostIdentity(t *testing.T) {
	now := time.Now().UTC()
	runtimeGuard := testAccountRuntimeGuard(now)
	taskGuard := testTaskProcessGuard(runtimeGuard, now)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	runtimeProof := processProof{PID: 321, StartTimeTick: 88, CgroupPath: "/system.slice/docker-" + runtimeGuard.ContainerID + ".scope"}
	processes := &fakeProcessInspector{proof: runtimeProof}
	taskProof := taskProcessProof{ContainerPID: taskGuard.ContainerPID, HostPID: 654, StartTimeTick: 1001,
		CgroupPath: taskCgroupPath(runtimeProof.CgroupPath, taskProcessKey(taskGuard)), NamespaceInode: 77}
	taskProcesses := &fakeTaskProcessInspector{proof: taskProof}
	docker := newFakeAccountRuntimeDocker(runtimeGuard, false, 0)
	service := newService(store, docker, combinedFakeProcessInspector{processes, taskProcesses}, func() time.Time { return now })
	if err := service.ArmAccountRuntime(context.Background(), runtimeGuard); err != nil {
		t.Fatal(err)
	}
	docker.setRunning(runtimeProof.PID)
	if err := service.StartedAccountRuntime(context.Background(), runtimeGuard); err != nil {
		t.Fatal(err)
	}
	renewedRuntime := runtimeGuard
	renewedRuntime.ExpiresAt = now.Add(25 * time.Second)
	processes.proof.StartTimeTick++
	if err := service.RenewAccountRuntime(context.Background(), renewedRuntime); err == nil {
		t.Fatal("Guardian extended a Runtime lease after host PID identity changed")
	}
	processes.proof = runtimeProof
	docker.setRunning(runtimeProof.PID + 1)
	if err := service.RenewAccountRuntime(context.Background(), renewedRuntime); err == nil {
		t.Fatal("Guardian extended a Runtime lease after Docker PID changed")
	}
	docker.setRunning(runtimeProof.PID)
	if value, ok := store.getRuntime(runtimeGuard.ContainerID); !ok || value.Guard.ExpiresAt != runtimeGuard.ExpiresAt {
		t.Fatalf("rejected Runtime renewal changed its deadline: %+v", value)
	}
	if err := service.RenewAccountRuntime(context.Background(), renewedRuntime); err != nil {
		t.Fatalf("renew unchanged Runtime: %v", err)
	}
	if err := service.RenewAccountRuntime(context.Background(), runtimeGuard); err == nil {
		t.Fatal("Guardian shortened a Runtime lease")
	}
	if _, err := service.LookupAccountRuntime(context.Background(), runtimeGuard.Fence, testNonceHash("foreign")); err == nil {
		t.Fatal("Guardian resolved a Runtime under a foreign nonce")
	}
	if lookedUp, err := service.LookupAccountRuntime(context.Background(), runtimeGuard.Fence, runtimeGuard.NonceSHA256); err != nil || lookedUp != renewedRuntime {
		t.Fatalf("exact Runtime lookup failed: %+v, %v", lookedUp, err)
	}

	if err := service.ArmTaskProcess(context.Background(), taskGuard); err != nil {
		t.Fatal(err)
	}
	taskProcesses.proof.HostPID++
	if err := service.StartedTaskProcess(context.Background(), taskGuard); err == nil {
		t.Fatal("Guardian started a Task whose host PID changed after arming")
	}
	taskProcesses.proof = taskProof
	if err := service.StartedTaskProcess(context.Background(), taskGuard); err != nil {
		t.Fatal(err)
	}
	renewedTask := taskGuard
	renewedTask.ExpiresAt = now.Add(18 * time.Second)
	taskProcesses.proof.HostPID++
	if err := service.RenewTaskProcess(context.Background(), renewedTask); err == nil {
		t.Fatal("Guardian extended a Task lease after host PID identity changed")
	}
	taskProcesses.proof = taskProof
	if value, ok := store.getTask(taskProcessKey(taskGuard)); !ok || value.Guard.ExpiresAt != taskGuard.ExpiresAt {
		t.Fatalf("rejected Task renewal changed its deadline: %+v", value)
	}
	if err := service.RenewTaskProcess(context.Background(), renewedTask); err != nil {
		t.Fatalf("renew unchanged Task: %v", err)
	}
	if err := service.RenewTaskProcess(context.Background(), taskGuard); err == nil {
		t.Fatal("Guardian shortened a Task lease")
	}
}

func TestAccountRuntimeExpiredLeaseStopsTaskAndContainer(t *testing.T) {
	now := time.Now().UTC()
	runtimeGuard := testAccountRuntimeGuard(now)
	runtimeGuard.ExpiresAt = now.Add(3 * time.Second)
	taskGuard := testTaskProcessGuard(runtimeGuard, now)
	taskGuard.ExpiresAt = now.Add(5 * time.Second)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	runtimeProof := processProof{PID: 321, StartTimeTick: 88, CgroupPath: "/system.slice/docker-" + runtimeGuard.ContainerID + ".scope"}
	processes := &fakeProcessInspector{proof: runtimeProof, gone: true, killTreeClears: true}
	taskProcesses := &fakeTaskProcessInspector{proof: taskProcessProof{
		ContainerPID: taskGuard.ContainerPID, HostPID: 654, StartTimeTick: 1001,
		CgroupPath: taskCgroupPath(runtimeProof.CgroupPath, taskProcessKey(taskGuard)), NamespaceInode: 77,
	}}
	docker := newFakeAccountRuntimeDocker(runtimeGuard, false, 0)
	service := newService(store, docker, combinedFakeProcessInspector{processes, taskProcesses}, func() time.Time { return now })
	if err := service.ArmAccountRuntime(context.Background(), runtimeGuard); err != nil {
		t.Fatal(err)
	}
	docker.setRunning(runtimeProof.PID)
	if err := service.StartedAccountRuntime(context.Background(), runtimeGuard); err != nil {
		t.Fatal(err)
	}
	if err := service.ArmTaskProcess(context.Background(), taskGuard); err != nil {
		t.Fatal(err)
	}
	if err := service.StartedTaskProcess(context.Background(), taskGuard); err != nil {
		t.Fatal(err)
	}
	now = runtimeGuard.ExpiresAt.Add(time.Millisecond)
	var failures []error
	service.sweep(context.Background(), func(err error) { failures = append(failures, err) })
	if len(failures) != 0 || !taskProcesses.gone || taskProcesses.killCalls != 1 || docker.killCalls != 1 || docker.exists {
		t.Fatalf("Runtime expiry did not independently stop Task then Runtime: errors=%v task=%+v docker=%+v", failures, taskProcesses, docker)
	}
	if runtimeValue, ok := store.getRuntime(runtimeGuard.ContainerID); !ok || !runtimeValue.Destroyed {
		t.Fatal("Runtime expiry was not durably recorded")
	}
	if taskValue, ok := store.getTask(taskProcessKey(taskGuard)); !ok || !taskValue.Destroyed {
		t.Fatal("Task cgroup expiry was not durably recorded before account Runtime shutdown")
	}
}

func TestTaskProcessExpiredLeaseStopsOnlyItsCgroup(t *testing.T) {
	now := time.Now().UTC()
	runtimeGuard := testAccountRuntimeGuard(now)
	runtimeGuard.ExpiresAt = now.Add(20 * time.Second)
	taskGuard := testTaskProcessGuard(runtimeGuard, now)
	taskGuard.ExpiresAt = now.Add(5 * time.Second)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	runtimeProof := processProof{PID: 321, StartTimeTick: 88, CgroupPath: "/system.slice/docker-" + runtimeGuard.ContainerID + ".scope"}
	processes := &fakeProcessInspector{proof: runtimeProof, gone: true}
	taskProcesses := &fakeTaskProcessInspector{proof: taskProcessProof{
		ContainerPID: taskGuard.ContainerPID, HostPID: 654, StartTimeTick: 1001,
		CgroupPath: taskCgroupPath(runtimeProof.CgroupPath, taskProcessKey(taskGuard)), NamespaceInode: 77,
	}}
	docker := newFakeAccountRuntimeDocker(runtimeGuard, false, 0)
	service := newService(store, docker, combinedFakeProcessInspector{processes, taskProcesses}, func() time.Time { return now })
	if err := service.ArmAccountRuntime(context.Background(), runtimeGuard); err != nil {
		t.Fatal(err)
	}
	docker.setRunning(runtimeProof.PID)
	if err := service.StartedAccountRuntime(context.Background(), runtimeGuard); err != nil {
		t.Fatal(err)
	}
	if err := service.ArmTaskProcess(context.Background(), taskGuard); err != nil {
		t.Fatal(err)
	}
	if err := service.StartedTaskProcess(context.Background(), taskGuard); err != nil {
		t.Fatal(err)
	}
	now = taskGuard.ExpiresAt.Add(time.Millisecond)
	var failures []error
	service.sweep(context.Background(), func(err error) { failures = append(failures, err) })
	if len(failures) != 0 || !taskProcesses.gone || taskProcesses.killCalls != 1 || docker.killCalls != 0 || !docker.exists || processes.killTreeCalls != 0 {
		t.Fatalf("Task expiry affected account Runtime or failed to kill the Task cgroup: errors=%v task=%+v docker=%+v runtimeProcesses=%+v", failures, taskProcesses, docker, processes)
	}
}

func TestAccountRuntimeAndTaskGuardsRejectFenceMismatch(t *testing.T) {
	now := time.Now().UTC()
	runtimeGuard := testAccountRuntimeGuard(now)
	wrong := runtimeGuard
	wrong.Fence.RuntimeGeneration++
	wrong.ContainerName = expectedAccountRuntimeContainerName(wrong.Fence, wrong.NonceSHA256)
	if err := wrong.validateNew(now); err != nil {
		t.Fatal(err)
	}
	if expectedAccountRuntimeContainerName(runtimeGuard.Fence, runtimeGuard.NonceSHA256) == wrong.ContainerName {
		t.Fatal("runtime name did not bind the runtime generation")
	}
	taskGuard := testTaskProcessGuard(runtimeGuard, now)
	taskGuard.TaskFence.OwnerID = "u8765432"
	if err := taskGuard.validateIdentity(); err == nil {
		t.Fatal("Task process lease accepted a different account owner")
	}
}

func testAccountRuntimeGuard(now time.Time) AccountRuntimeGuard {
	fence := app.AccountAgentRuntimeFence{
		ServiceInstanceID: "123e4567-e89b-12d3-a456-426614174000", OwnerID: "u1234567", RuntimeID: "runtime-1",
		RuntimeGeneration: 1, LeaseOwner: "worker-1", LeaseVersion: 1, ExecutionEpoch: 1,
	}
	nonce := testNonceHash("runtime-nonce")
	return AccountRuntimeGuard{
		ContainerID: strings.Repeat("c", 64), ContainerName: expectedAccountRuntimeContainerName(fence, nonce),
		Fence: fence, NonceSHA256: nonce, ExpiresAt: now.Add(20 * time.Second),
	}
}

func testTaskProcessGuard(runtime AccountRuntimeGuard, now time.Time) TaskProcessGuard {
	return TaskProcessGuard{
		RuntimeFence: runtime.Fence, TaskFence: testGuard(now).Fence, RuntimeContainerID: runtime.ContainerID,
		ContainerPID: 5, NonceSHA256: testNonceHash("task-nonce"), ExpiresAt: now.Add(15 * time.Second),
	}
}

func testNonceHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func newFakeAccountRuntimeDocker(guard AccountRuntimeGuard, running bool, pid int) *fakeDocker {
	base := newFakeDocker(testGuard(guard.ExpiresAt), running, pid)
	base.value.ID = guard.ContainerID
	base.value.Name = "/" + guard.ContainerName
	base.value.Config.Labels = map[string]string{
		"morde.account-agent-runtime.fence":        mustRuntimeFenceJSON(guard.Fence),
		"morde.account-agent-runtime.nonce-sha256": guard.NonceSHA256,
	}
	return base
}

func mustRuntimeFenceJSON(fence app.AccountAgentRuntimeFence) string {
	data, _ := json.Marshal(fence)
	return string(data)
}

func verifyRuntimeAttestation(t *testing.T, attestation app.AccountAgentRuntimeStopAttestation) {
	t.Helper()
	authenticator, err := app.NewAccountAgentRuntimeGuardianStopAuthenticator(testAttestationPrivateKey().Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authenticator.VerifyAccountAgentRuntimeStop(context.Background(), attestation); err != nil {
		t.Fatalf("verify Guardian Runtime signature: %v", err)
	}
}

func verifyTaskAttestation(t *testing.T, attestation app.AccountAgentTaskStopAttestation) {
	t.Helper()
	authenticator, err := app.NewAccountAgentTaskGuardianStopAuthenticator(testAttestationPrivateKey().Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authenticator.VerifyAccountAgentTaskStop(context.Background(), attestation); err != nil {
		t.Fatalf("verify Guardian Task signature: %v", err)
	}
}

type fakeTaskProcessInspector struct {
	proof       taskProcessProof
	gone        bool
	killCalls   int
	removeCalls int
	err         error
}

func (p *fakeTaskProcessInspector) AttachTaskProcess(_ string, runtime processProof, containerPID int, _ string) (taskProcessProof, error) {
	if p.err != nil {
		return taskProcessProof{}, p.err
	}
	if p.proof.ContainerPID != containerPID || !cgroupPathWithin(runtime.CgroupPath, p.proof.CgroupPath) {
		return taskProcessProof{}, errors.New("fake task identity mismatch")
	}
	return p.proof, nil
}

func (p *fakeTaskProcessInspector) KillTaskProcess(_ string, _ processProof, proof taskProcessProof, _ string) error {
	if proof != p.proof {
		return errors.New("fake task cgroup mismatch")
	}
	p.killCalls++
	p.gone = true
	return nil
}

func (p *fakeTaskProcessInspector) TaskProcessGone(_ string, _ processProof, proof taskProcessProof, _ string) (bool, error) {
	if proof != p.proof {
		return false, errors.New("fake task proof mismatch")
	}
	return p.gone, nil
}

func (p *fakeTaskProcessInspector) RemoveTaskCgroup(_ string, _ processProof, proof taskProcessProof, _ string) error {
	if proof != p.proof || !p.gone {
		return errors.New("fake task cgroup is not empty")
	}
	p.removeCalls++
	return nil
}

var _ taskProcessInspector = (*fakeTaskProcessInspector)(nil)

type combinedFakeProcessInspector struct {
	*fakeProcessInspector
	*fakeTaskProcessInspector
}

var _ processInspector = combinedFakeProcessInspector{}

func TestAccountRuntimeLeaseStoreRejectsCorruptTaskOrRuntimeFiles(t *testing.T) {
	for _, name := range []string{"runtime-bad.json", "task-bad.json"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := openLeaseStore(dir); err == nil {
				t.Fatal("accepted an invalid runtime or Task lease filename")
			}
		})
	}
}
