//go:build linux

package runtimeactions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	applicationCgroupName = "morde-runtime-applications"
	applicationMemoryMax  = "2147483648"
	applicationPIDsMax    = "256"
	applicationCPUQuota   = "100000 100000"
)

type linuxApplicationProcessDomain struct {
	basePath     string
	pollInterval time.Duration
	credential   *syscall.Credential
}

func NewApplicationProcessDomain(options ApplicationProcessDomainOptions) (ApplicationProcessDomain, error) {
	if runtime.GOARCH != "amd64" || os.Geteuid() != 0 {
		return nil, ErrProcessDomainUnavailable
	}
	if options.CgroupRoot == "" {
		options.CgroupRoot = defaultCgroupRoot
	}
	if options.PollInterval == 0 {
		options.PollInterval = 10 * time.Millisecond
	}
	if options.PollInterval < time.Millisecond || options.PollInterval > time.Second {
		return nil, ErrProcessDomainUnavailable
	}
	root, err := filepath.EvalSymlinks(options.CgroupRoot)
	if err != nil {
		return nil, ErrProcessDomainUnavailable
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(root, &fs); err != nil || uint64(fs.Type) != cgroup2SuperMagic {
		return nil, ErrProcessDomainUnavailable
	}
	base := filepath.Join(root, applicationCgroupName)
	if err := makeTrustedCgroupDir(base); err != nil || enableResourceControllers(base) != nil {
		return nil, ErrProcessDomainUnavailable
	}
	credential, _, err := gemCredential()
	if err != nil {
		return nil, ErrProcessDomainUnavailable
	}
	domain := &linuxApplicationProcessDomain{basePath: base, pollInterval: options.PollInterval, credential: credential}
	if err := domain.probe(); err != nil {
		return nil, ErrProcessDomainUnavailable
	}
	return domain, nil
}

func (d *linuxApplicationProcessDomain) Start(launchID string, command *exec.Cmd, uid, gid uint32) error {
	if !validApplicationLaunchID(launchID) || command == nil || uid != d.credential.Uid || gid != d.credential.Gid {
		return ErrProcessDomainUnavailable
	}
	if err := cgroupMountStillAvailable(d.basePath); err != nil {
		return ErrProcessDomainUnavailable
	}
	path := d.groupPath(launchID)
	info, err := os.Lstat(path)
	if err != nil || !trustedCgroupChild(info) {
		return ErrProcessDomainUnavailable
	}
	populated, err := cgroupPopulated(filepath.Join(path, "cgroup.events"))
	if err != nil || populated {
		return ErrProcessDomainUnavailable
	}
	group, err := os.Open(path)
	if err != nil {
		return ErrProcessDomainUnavailable
	}
	credential := *d.credential
	credential.Uid, credential.Gid = uid, gid
	command.SysProcAttr = &syscall.SysProcAttr{
		Credential:  &credential,
		UseCgroupFD: true,
		CgroupFD:    int(group.Fd()),
	}
	startErr := command.Start()
	_ = group.Close()
	if startErr != nil {
		return ErrProcessDomainUnavailable
	}
	return nil
}

// Prepare registers an empty per-launch cgroup before any launcher preflight
// or process start. Recovery can then distinguish a known-empty group from a
// missing or untrusted cgroup without ever redispatching the application.
func (d *linuxApplicationProcessDomain) Prepare(launchID string) error {
	if !validApplicationLaunchID(launchID) || cgroupMountStillAvailable(d.basePath) != nil {
		return ErrProcessDomainUnavailable
	}
	if err := createApplicationGroup(d.groupPath(launchID)); err != nil {
		return ErrProcessDomainUnavailable
	}
	return nil
}

func (d *linuxApplicationProcessDomain) Populated(launchID string) (bool, error) {
	if !validApplicationLaunchID(launchID) || cgroupMountStillAvailable(d.basePath) != nil {
		return false, ErrProcessDomainUnavailable
	}
	path := d.groupPath(launchID)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, ErrProcessDomainUnavailable
	}
	if err != nil || !trustedCgroupChild(info) {
		return false, ErrProcessDomainUnavailable
	}
	populated, err := cgroupPopulated(filepath.Join(path, "cgroup.events"))
	if err != nil {
		return false, ErrProcessDomainUnavailable
	}
	return populated, nil
}

func (d *linuxApplicationProcessDomain) Stop(ctx context.Context, launchID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !validApplicationLaunchID(launchID) {
		return ErrProcessDomainUnavailable
	}
	path := d.groupPath(launchID)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if cgroupMountStillAvailable(d.basePath) != nil {
			return ErrProcessDomainUnavailable
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return ErrProcessDomainUnavailable
		}
		if err != nil || !trustedCgroupChild(info) {
			return ErrProcessDomainUnavailable
		}
		populated, err := cgroupPopulated(filepath.Join(path, "cgroup.events"))
		if err != nil {
			return ErrProcessDomainUnavailable
		}
		if !populated {
			return nil
		}
		if err := terminateCgroupMembers(path); err != nil {
			return ErrProcessDomainUnavailable
		}
		if time.Now().After(deadline) {
			return ErrProcessDomainUnavailable
		}
		select {
		case <-ctx.Done():
			return ErrProcessDomainUnavailable
		case <-time.After(min(d.pollInterval, time.Until(deadline))):
		}
	}
}

func terminateCgroupMembers(groupPath string) error {
	data, err := os.ReadFile(filepath.Join(groupPath, "cgroup.procs"))
	if err != nil {
		return err
	}
	lines := strings.Fields(string(data))
	if len(lines) > 256 {
		return ErrProcessDomainUnavailable
	}
	for _, line := range lines {
		pid, err := strconv.Atoi(line)
		if err != nil || pid <= 1 {
			return ErrProcessDomainUnavailable
		}
		fd, _, errno := unix.Syscall(unix.SYS_PIDFD_OPEN, uintptr(pid), 0, 0)
		if errno == unix.ESRCH {
			continue
		}
		if errno != 0 {
			return errno
		}
		belongs, checkErr := processBelongsToCgroup(pid, groupPath)
		if checkErr == nil && belongs {
			_, _, errno = unix.Syscall6(unix.SYS_PIDFD_SEND_SIGNAL, fd, uintptr(unix.SIGTERM), 0, 0, 0, 0)
		}
		_ = unix.Close(int(fd))
		if checkErr != nil || errno != 0 && errno != unix.ESRCH {
			return ErrProcessDomainUnavailable
		}
	}
	return nil
}

func processBelongsToCgroup(pid int, groupPath string) (bool, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	var mountRoot, mountPoint string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		separator := -1
		for index, field := range fields {
			if field == "-" {
				separator = index
				break
			}
		}
		if separator < 0 || separator+1 >= len(fields) || fields[separator+1] != "cgroup2" || len(fields) < 6 {
			continue
		}
		mountRoot, mountPoint = unescapeMountInfoPath(fields[3]), unescapeMountInfoPath(fields[4])
		break
	}
	if mountRoot == "" || mountPoint == "" {
		return false, ErrProcessDomainUnavailable
	}
	relative, err := filepath.Rel(mountPoint, groupPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false, ErrProcessDomainUnavailable
	}
	expected := filepath.Clean(filepath.Join(mountRoot, relative))
	data, err = os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) == 3 && fields[0] == "0" && fields[1] == "" {
			return filepath.Clean(fields[2]) == expected, nil
		}
	}
	return false, ErrProcessDomainUnavailable
}

func unescapeMountInfoPath(value string) string {
	return strings.NewReplacer("\\040", " ", "\\011", "\t", "\\012", "\n", "\\134", "\\").Replace(value)
}

func (d *linuxApplicationProcessDomain) RemoveEmpty(launchID string) error {
	if !validApplicationLaunchID(launchID) || cgroupMountStillAvailable(d.basePath) != nil {
		return ErrProcessDomainUnavailable
	}
	path := d.groupPath(launchID)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !trustedCgroupChild(info) {
		return ErrProcessDomainUnavailable
	}
	populated, err := cgroupPopulated(filepath.Join(path, "cgroup.events"))
	if err != nil || populated {
		return ErrProcessDomainUnavailable
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrProcessDomainUnavailable
	}
	return nil
}

func (d *linuxApplicationProcessDomain) groupPath(launchID string) string {
	digest := sha256.Sum256([]byte(launchID))
	return filepath.Join(d.basePath, "app-"+hex.EncodeToString(digest[:]))
}

func (d *linuxApplicationProcessDomain) probe() error {
	launchID := "launch_probe_" + strconv.FormatInt(time.Now().UnixNano(), 16)
	sleepPath, err := findTrustedExecutable([]string{"/bin/sleep", "/usr/bin/sleep"})
	if err != nil {
		return err
	}
	command := exec.Command(sleepPath, "30")
	command.Env = []string{"HOME=/home/gem", "PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	if err := d.Prepare(launchID); err != nil {
		return err
	}
	if err := d.Start(launchID, command, d.credential.Uid, d.credential.Gid); err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = d.Stop(ctx, launchID)
		cancel()
		_ = d.RemoveEmpty(launchID)
	}()
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		populated, err := d.Populated(launchID)
		if err != nil {
			_ = d.Stop(context.Background(), launchID)
			return err
		}
		if populated {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err = d.Stop(ctx, launchID)
			cancel()
			if err != nil {
				return err
			}
			select {
			case err := <-wait:
				if err == nil {
					return ErrProcessDomainUnavailable
				}
				return nil
			case <-time.After(time.Second):
				return ErrProcessDomainUnavailable
			}
		}
		select {
		case err := <-wait:
			return err
		case <-time.After(d.pollInterval):
		}
	}
	_ = d.Stop(context.Background(), launchID)
	return ErrProcessDomainUnavailable
}

func createApplicationGroup(path string) error {
	if err := os.Mkdir(path, 0700); err != nil {
		return err
	}
	for _, name := range []string{"cpu.max", "memory.max", "pids.max", "cgroup.events", "cgroup.procs"} {
		info, err := os.Stat(filepath.Join(path, name))
		if err != nil {
			_ = os.Remove(path)
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
			_ = os.Remove(path)
			return ErrProcessDomainUnavailable
		}
	}
	populated, err := cgroupPopulated(filepath.Join(path, "cgroup.events"))
	if err != nil || populated {
		_ = os.Remove(path)
		return ErrProcessDomainUnavailable
	}
	for name, value := range map[string]string{"cpu.max": applicationCPUQuota, "memory.max": applicationMemoryMax, "pids.max": applicationPIDsMax} {
		if err := writeCgroupFile(filepath.Join(path, name), value); err != nil {
			_ = os.Remove(path)
			return err
		}
	}
	return nil
}

func trustedCgroupChild(info os.FileInfo) bool {
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0
}

func validApplicationLaunchID(value string) bool {
	if len(value) < len("launch_")+1 || len(value) > 128 || !strings.HasPrefix(value, "launch_") || !applicationASCIILetterDigit(value[len("launch_")]) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value[len("launch_"):] {
		if !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') && !strings.ContainsRune("._:-", character) {
			return false
		}
	}
	return true
}

func applicationASCIILetterDigit(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
}
