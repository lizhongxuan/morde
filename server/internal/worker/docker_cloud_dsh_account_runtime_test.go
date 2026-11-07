package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/dshguardian"
)

type accountRuntimeArmProbe struct {
	dockerAccountRuntimeGuardian
	base     *dockerDSHFakeGuardian
	wantName string
	guard    dshguardian.AccountRuntimeGuard
	started  bool
}

func (p *accountRuntimeArmProbe) ArmAccountRuntime(_ context.Context, guard dshguardian.AccountRuntimeGuard) error {
	if guard.ContainerName != p.wantName {
		return errors.New("account Runtime name does not match Guardian contract")
	}
	p.guard = guard
	p.base.mu.Lock()
	p.base.armed[guard.ContainerName] = DockerCloudDSHGuard{}
	p.base.mu.Unlock()
	return nil
}

func (p *accountRuntimeArmProbe) StartedAccountRuntime(_ context.Context, guard dshguardian.AccountRuntimeGuard) error {
	if guard != p.guard {
		return errors.New("account Runtime changed after Guardian admission")
	}
	p.started = true
	return nil
}

func TestDockerAccountRuntimeNameMatchesGuardianContract(t *testing.T) {
	docker, engine, guardian := newDockerDSHFixture(t)
	fence := app.AccountAgentRuntimeFence{ServiceInstanceID: "123e4567-e89b-12d3-a456-426614174000",
		ConversationID: "c1234567", OwnerID: "u1234567", RuntimeID: "runtime-1", RuntimeGeneration: 1,
		LeaseOwner: "worker-runtime", LeaseVersion: 1, ExecutionEpoch: 1}
	nonce := bytes.Repeat([]byte{7}, 32)
	nonceHash := sha256.Sum256(nonce)
	encodedFence, err := json.Marshal(fence)
	if err != nil {
		t.Fatal(err)
	}
	identity := sha256.Sum256(append(append(encodedFence, 0), nonceHash[:]...))
	wantName := "morde-account-runtime-" + hex.EncodeToString(identity[:])
	probe := &accountRuntimeArmProbe{base: guardian, wantName: wantName}
	supervisor := &dockerCloudDSHAccountRuntimeSupervisor{docker: docker, guardian: probe}
	process, err := supervisor.StartAccountRuntime(context.Background(), fence, nonce, time.Now().Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = process.Input.Close(); _ = process.Output.Close() }()
	if _, err := process.Input.Write([]byte("dsh-input\n")); err != nil {
		t.Fatal(err)
	}
	if got := <-engine.input; got != "dsh-input\n" {
		t.Fatalf("runtime attach input = %q", got)
	}
	if got, err := bufio.NewReader(process.Output).ReadString('\n'); err != nil || got != "dsh-output\n" {
		t.Fatalf("runtime attach output = %q, %v", got, err)
	}
	if !probe.started || !engine.running || process.ContainerName != wantName || process.ContainerID != engine.id ||
		probe.guard.ContainerName != wantName || engine.name != wantName || probe.guard.Fence != fence ||
		probe.guard.NonceSHA256 != hex.EncodeToString(nonceHash[:]) || probe.guard.ContainerID != engine.id {
		t.Fatalf("Docker/Guardian account Runtime startup mismatch: process=%+v Guardian=%+v", process, probe.guard)
	}
	engine.mu.Lock()
	config := engine.created
	encoded, err := json.Marshal(config)
	engine.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	host := config["HostConfig"].(map[string]any)
	if host["NetworkMode"] != "none" || host["ReadonlyRootfs"] != true || host["Privileged"] != false ||
		host["Memory"].(float64) <= 0 || host["NanoCpus"].(float64) <= 0 || host["PidsLimit"].(float64) <= 0 ||
		config["User"] != dockerDSHUser || !strings.Contains(string(encoded), "no-new-privileges:true") ||
		!strings.Contains(string(encoded), `"ALL"`) || bytes.Contains(encoded, nonce) {
		t.Fatal("conversation Runtime container violated its isolation or secret boundary")
	}
	inspected, err := docker.inspect(context.Background(), process.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	inspected.HostConfig.Memory = 0
	if err := supervisor.validateAccountRuntime(inspected, probe.guard, true); err == nil {
		t.Fatal("account Runtime accepted a container without its memory limit")
	}
}

func TestDockerAccountRuntimeCleansContainerWhenGuardianArmFails(t *testing.T) {
	for _, test := range []struct {
		name            string
		keepAfterDelete bool
		wantError       string
	}{
		{name: "removed", wantError: "guardian could not arm"},
		{name: "removal unconfirmed", keepAfterDelete: true, wantError: "cleanup is unconfirmed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			docker, engine, guardian := newDockerDSHFixture(t)
			engine.keepAfterDelete = test.keepAfterDelete
			probe := &accountRuntimeArmProbe{base: guardian, wantName: "reject-arm"}
			supervisor := &dockerCloudDSHAccountRuntimeSupervisor{docker: docker, guardian: probe}
			fence := app.AccountAgentRuntimeFence{ServiceInstanceID: "123e4567-e89b-12d3-a456-426614174000",
				ConversationID: "c1234567", OwnerID: "u1234567", RuntimeID: "runtime-1", RuntimeGeneration: 1,
				LeaseOwner: "worker-runtime", LeaseVersion: 1, ExecutionEpoch: 1}
			_, err := supervisor.StartAccountRuntime(context.Background(), fence, bytes.Repeat([]byte{7}, 32), time.Now().Add(10*time.Second))
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Guardian Arm failure = %v, want %q", err, test.wantError)
			}
			engine.mu.Lock()
			removed, running := engine.removed, engine.running
			engine.mu.Unlock()
			if removed == test.keepAfterDelete || running {
				t.Fatalf("unarmed container removed=%t running=%t, keepAfterDelete=%t", removed, running, test.keepAfterDelete)
			}
		})
	}
}

func TestDockerAccountRuntimeCleansContainerWhenPreflightIdentityChanges(t *testing.T) {
	docker, engine, guardian := newDockerDSHFixture(t)
	engine.badAccountLabel = true
	probe := &accountRuntimeArmProbe{base: guardian}
	supervisor := &dockerCloudDSHAccountRuntimeSupervisor{docker: docker, guardian: probe}
	fence := app.AccountAgentRuntimeFence{ServiceInstanceID: "123e4567-e89b-12d3-a456-426614174000",
		ConversationID: "c1234567", OwnerID: "u1234567", RuntimeID: "runtime-1", RuntimeGeneration: 1,
		LeaseOwner: "worker-runtime", LeaseVersion: 1, ExecutionEpoch: 1}
	_, err := supervisor.StartAccountRuntime(context.Background(), fence, bytes.Repeat([]byte{7}, 32), time.Now().Add(10*time.Second))
	if err == nil || !strings.Contains(err.Error(), "does not match its identity") {
		t.Fatalf("tampered preflight = %v", err)
	}
	engine.mu.Lock()
	removed, running := engine.removed, engine.running
	engine.mu.Unlock()
	if !removed || running || probe.guard.ContainerID != "" {
		t.Fatalf("tampered container removed=%t running=%t Guardian=%+v", removed, running, probe.guard)
	}
}
