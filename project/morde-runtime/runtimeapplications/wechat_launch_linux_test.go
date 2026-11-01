//go:build linux

package runtimeapplications

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidXAuthorityAcceptsGemOwned0664FileAndRejectsUnsafePath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "Xauthority")
	if err := os.Mkdir(filepath.Join(root, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("cookie"), 0o664); err != nil {
		t.Fatal(err)
	}
	uid := uint32(os.Geteuid())
	if !validXAuthority(path, uid) {
		t.Fatal("expected owner-readable 0664 Xauthority file to be accepted")
	}
	if validXAuthority(filepath.Join(root, "child", "..", "Xauthority"), uid) {
		t.Fatal("accepted a non-canonical Xauthority path")
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if validXAuthority(path, uid) {
		t.Fatal("accepted world-writable Xauthority file")
	}
}

func TestWechatLauncherIsFixedToVerifiedPackagePath(t *testing.T) {
	command := newWechatCommand()
	if command.Path != "/opt/wechat/wechat" || len(command.Args) != 1 {
		t.Fatalf("launcher command is not fixed/no-args: path=%q args=%q", command.Path, command.Args)
	}
}
