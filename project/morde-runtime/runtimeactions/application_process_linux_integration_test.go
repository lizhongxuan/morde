//go:build linux

package runtimeactions

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// This opt-in integration check exercises the same CLONE_INTO_CGROUP and
// pidfd/SIGTERM path used by native application launches. It requires the
// runtime's privileged cgroup v2 environment and never touches another Host.
func TestApplicationProcessDomainCgroupAndPidfdStop(t *testing.T) {
	if os.Getenv("MORDE_RUNTIME_APP_INTEGRATION") != "1" {
		t.Skip("set MORDE_RUNTIME_APP_INTEGRATION=1 in a disposable Linux cgroup v2 runtime")
	}
	domain, err := NewApplicationProcessDomain(ApplicationProcessDomainOptions{})
	if err != nil {
		t.Fatalf("cgroup v2/pidfd self-check unavailable: %v", err)
	}
	launchID := "launch_test_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := domain.Prepare(launchID); err != nil {
		t.Fatalf("prepare cgroup: %v", err)
	}
	command := exec.Command("/bin/sleep", "30")
	command.Env = []string{"HOME=/home/gem", "PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	if err := domain.Start(launchID, command, domain.(*linuxApplicationProcessDomain).credential.Uid, domain.(*linuxApplicationProcessDomain).credential.Gid); err != nil {
		t.Fatalf("start in cgroup: %v", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		_ = domain.Stop(ctx, launchID)
		cancel()
		_ = domain.RemoveEmpty(launchID)
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		populated, err := domain.Populated(launchID)
		if err != nil {
			t.Fatalf("read cgroup.events: %v", err)
		}
		if populated {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	populated, err := domain.Populated(launchID)
	if err != nil || !populated {
		t.Fatalf("cgroup did not report populated before stop: populated=%v err=%v", populated, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	err = domain.Stop(ctx, launchID)
	cancel()
	if err != nil {
		t.Fatalf("pidfd SIGTERM stop: %v", err)
	}
	populated, err = domain.Populated(launchID)
	if err != nil || populated {
		t.Fatalf("stop lacked populated=0 receipt: populated=%v err=%v", populated, err)
	}
	select {
	case err := <-wait:
		if err == nil {
			t.Fatal("sleep exited successfully; expected SIGTERM")
		}
	case <-time.After(time.Second):
		t.Fatal("pidfd SIGTERM did not make the child exit")
	}
}
