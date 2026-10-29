//go:build linux

package dshguardian

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	guardianSocketACLHelperEnv = "MORDE_GUARDIAN_SOCKET_ACL_HELPER"
	guardianSocketACLPathEnv   = "MORDE_GUARDIAN_SOCKET_ACL_PATH"
	guardianSocketACLDenyEnv   = "MORDE_GUARDIAN_SOCKET_ACL_DENY"
)

func TestGuardianGroupSocketACL(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to assign a test-only socket group and child credentials")
	}

	dir, err := os.MkdirTemp("/tmp", "dg-acl-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o711); err != nil {
		_ = os.RemoveAll(dir)
		t.Fatal(err)
	}
	socketPath := dir + "/guardian.sock"
	listener, cleanup, err := unixListener(socketPath, 0o660)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup()
		_ = os.RemoveAll(dir)
	})

	const portalGID, nonMemberGID = 42000, 42001
	if err := os.Chown(socketPath, os.Geteuid(), portalGID); err != nil {
		if isGuardianSocketACLPrivilegeError(err) {
			t.Skipf("runner cannot assign the test Portal group: %v", err)
		}
		t.Fatal(err)
	}
	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode().Perm() != 0o660 || stat.Gid != portalGID {
		t.Fatalf("Guardian socket mode/group = %o/%v; want 660/%d", info.Mode().Perm(), stat, portalGID)
	}

	runGuardianSocketACLClient(t, socketPath, 65534, portalGID, []uint32{portalGID}, false)
	runGuardianSocketACLClient(t, socketPath, 65533, nonMemberGID, []uint32{nonMemberGID}, true)
	_ = listener
}

func TestGuardianSocketACLClientHelper(t *testing.T) {
	if os.Getenv(guardianSocketACLHelperEnv) != "1" {
		return
	}

	conn, err := net.DialTimeout("unix", os.Getenv(guardianSocketACLPathEnv), time.Second)
	if os.Getenv(guardianSocketACLDenyEnv) == "1" {
		if err == nil {
			_ = conn.Close()
			t.Fatal("non-member UID/GID connected to the Guardian socket")
		}
		if !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EPERM) {
			t.Fatalf("non-member connection failed for a reason other than socket ACL: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("Portal group could not connect to Guardian socket: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
}

func runGuardianSocketACLClient(t *testing.T, socketPath string, uid, gid uint32, groups []uint32, wantDenied bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGuardianSocketACLClientHelper$")
	cmd.Env = guardianSocketACLEnv(socketPath, wantDenied)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: groups}}
	output, err := cmd.CombinedOutput()
	if isGuardianSocketACLPrivilegeError(err) {
		t.Skipf("runner cannot start an isolated UID/GID credential child: %v", err)
	}
	if err != nil {
		t.Fatalf("ACL child UID=%d GID=%d denied=%t: %v\n%s", uid, gid, wantDenied, err, strings.TrimSpace(string(output)))
	}
}

func guardianSocketACLEnv(socketPath string, wantDenied bool) []string {
	variables := map[string]string{
		guardianSocketACLHelperEnv: "1",
		guardianSocketACLPathEnv:   socketPath,
		guardianSocketACLDenyEnv:   "0",
	}
	if wantDenied {
		variables[guardianSocketACLDenyEnv] = "1"
	}
	env := make([]string, 0, len(os.Environ())+len(variables))
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if _, replaced := variables[name]; !replaced {
			env = append(env, value)
		}
	}
	for name, value := range variables {
		env = append(env, name+"="+value)
	}
	return env
}

func isGuardianSocketACLPrivilegeError(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EINVAL)
}
