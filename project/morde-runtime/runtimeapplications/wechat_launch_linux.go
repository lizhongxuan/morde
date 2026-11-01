//go:build linux

package runtimeapplications

import (
	"context"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

const (
	wechatLauncherPath = "/opt/wechat/wechat"
	wechatDisplayProbe = "/usr/bin/xdpyinfo"
)

func wechatLaunchArchitectureSupported() bool { return runtime.GOARCH == "amd64" }

func init() { wechatLauncherTrustCheck = trustedWechatLauncher }

func prepareWechatLaunch(ctx context.Context) (*exec.Cmd, uint32, uint32, string) {
	if runtime.GOARCH != "amd64" {
		return nil, 0, 0, "architecture_unsupported"
	}
	if !trustedWechatLauncher() {
		return nil, 0, 0, "launch_failed"
	}
	display := os.Getenv("DISPLAY")
	if !validDisplayName(display) {
		return nil, 0, 0, "launch_failed"
	}
	account, err := user.Lookup("gem")
	if err != nil || account.HomeDir != "/home/gem" {
		return nil, 0, 0, "launch_failed"
	}
	uid, uidErr := strconv.ParseUint(account.Uid, 10, 32)
	gid, gidErr := strconv.ParseUint(account.Gid, 10, 32)
	if uidErr != nil || gidErr != nil || uid == 0 || gid == 0 {
		return nil, 0, 0, "launch_failed"
	}
	credential := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}
	authority := os.Getenv("XAUTHORITY")
	if authority == "" {
		authority = filepath.Join(account.HomeDir, ".Xauthority")
	}
	if !validXAuthority(authority, uint32(uid)) {
		return nil, 0, 0, "launch_failed"
	}
	if !validUserRuntimeDir(filepath.Join("/run/user", strconv.FormatUint(uid, 10)), uint32(uid)) {
		return nil, 0, 0, "launch_failed"
	}
	if err := probeWechatDisplay(ctx, display, authority, credential); err != nil {
		return nil, 0, 0, "launch_failed"
	}
	environment := []string{
		"DISPLAY=" + display, "XAUTHORITY=" + authority, "HOME=" + account.HomeDir,
		"USER=gem", "LOGNAME=gem", "PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8",
		"XDG_RUNTIME_DIR=" + filepath.Join("/run/user", strconv.FormatUint(uid, 10)),
	}
	busPath := filepath.Join("/run/user", strconv.FormatUint(uid, 10), "bus")
	if validUserBus(busPath, uint32(uid)) {
		environment = append(environment, "DBUS_SESSION_BUS_ADDRESS=unix:path="+busPath)
	}
	command := newWechatCommand()
	command.Dir = account.HomeDir
	command.Env = environment
	return command, uint32(uid), uint32(gid), ""
}

func newWechatCommand() *exec.Cmd { return exec.Command(wechatLauncherPath) }

func trustedWechatLauncher() bool {
	info, err := os.Lstat(wechatLauncherPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0
}

func validDisplayName(value string) bool {
	if len(value) == 0 || len(value) > 256 || !strings.Contains(value, ":") {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') && !strings.ContainsRune("._:-", character) {
			return false
		}
	}
	return true
}

func validXAuthority(path string, uid uint32) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.IndexByte(path, 0) >= 0 {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0002 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uid && info.Mode().Perm()&0400 != 0
}

func validUserRuntimeDir(path string, uid uint32) bool {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uid
}

func validUserBus(path string, uid uint32) bool {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uid
}

func probeWechatDisplay(ctx context.Context, display, authority string, credential *syscall.Credential) error {
	info, err := os.Lstat(wechatDisplayProbe)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 {
		return errInvalidPackage
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return errInvalidPackage
	}
	command := exec.CommandContext(ctx, wechatDisplayProbe, "-display", display)
	command.Dir = "/home/gem"
	command.Env = []string{"DISPLAY=" + display, "XAUTHORITY=" + authority, "HOME=/home/gem", "PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	command.Stdout, command.Stderr = io.Discard, io.Discard
	return command.Run()
}
