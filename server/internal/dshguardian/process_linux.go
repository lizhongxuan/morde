//go:build linux

package dshguardian

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type hostProcessInspector struct {
	procRoot   string
	cgroupRoot string
}

func newProcessInspector() processInspector {
	return hostProcessInspector{procRoot: "/proc", cgroupRoot: "/sys/fs/cgroup"}
}

func (p hostProcessInspector) Ready() error { return p.requireCgroupV2() }

func (p hostProcessInspector) Capture(containerID string, pid int) (processProof, error) {
	if pid < 2 || !containerIDRE.MatchString(containerID) {
		return processProof{}, errors.New("invalid host PID or container ID")
	}
	if err := p.requireCgroupV2(); err != nil {
		return processProof{}, err
	}
	procDir := filepath.Join(p.procRoot, fmt.Sprintf("%d", pid))
	first, err := os.ReadFile(filepath.Join(procDir, "stat"))
	if err != nil {
		return processProof{}, fmt.Errorf("read host PID identity: %w", err)
	}
	startTime, err := parseProcStartTime(first)
	if err != nil {
		return processProof{}, err
	}
	cgroups, err := os.ReadFile(filepath.Join(procDir, "cgroup"))
	if err != nil {
		return processProof{}, fmt.Errorf("read host PID cgroup: %w", err)
	}
	cgroupPath, err := parseUnifiedCgroup(cgroups)
	if err != nil {
		return processProof{}, err
	}
	if !containerCgroupMatches(cgroupPath, containerID) {
		return processProof{}, errors.New("host PID is not in the guarded Docker cgroup")
	}
	if _, err := secureCgroupDirectory(p.cgroupRoot, cgroupPath); err != nil {
		return processProof{}, err
	}
	second, err := os.ReadFile(filepath.Join(procDir, "stat"))
	if err != nil {
		return processProof{}, fmt.Errorf("recheck host PID identity: %w", err)
	}
	secondStart, err := parseProcStartTime(second)
	if err != nil || secondStart != startTime {
		return processProof{}, errors.New("host PID changed while its cgroup was captured")
	}
	return processProof{PID: pid, StartTimeTick: startTime, CgroupPath: cgroupPath}, nil
}

func (p hostProcessInspector) KillTree(containerID string, proof processProof) error {
	if err := p.requireCgroupV2(); err != nil {
		return err
	}
	if proof.PID < 2 || proof.StartTimeTick == 0 || !containerCgroupMatches(proof.CgroupPath, containerID) {
		return errors.New("saved process evidence does not identify the guarded cgroup")
	}
	cgroupDir, err := secureCgroupDirectory(p.cgroupRoot, proof.CgroupPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect guarded cgroup before kill: %w", err)
	}
	killFile, err := os.OpenFile(filepath.Join(cgroupDir, "cgroup.kill"), os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open guarded cgroup.kill: %w", err)
	}
	written, writeErr := killFile.Write([]byte("1"))
	closeErr := killFile.Close()
	if writeErr != nil {
		return fmt.Errorf("kill guarded cgroup process tree: %w", writeErr)
	}
	if written != 1 || closeErr != nil {
		return errors.New("cgroup.kill did not confirm the process-tree signal")
	}
	return nil
}

func (p hostProcessInspector) Gone(containerID string, proof processProof) (bool, error) {
	if !containerIDRE.MatchString(containerID) {
		return false, errors.New("invalid container ID for process verification")
	}
	if err := p.requireCgroupV2(); err != nil {
		return false, err
	}
	if proof.PID != 0 {
		if proof.PID < 2 || proof.StartTimeTick == 0 || !containerCgroupMatches(proof.CgroupPath, containerID) {
			return false, errors.New("saved host process proof does not match the guarded container")
		}
		data, err := os.ReadFile(filepath.Join(p.procRoot, fmt.Sprintf("%d", proof.PID), "stat"))
		if err == nil {
			startTime, parseErr := parseProcStartTime(data)
			if parseErr != nil {
				return false, parseErr
			}
			if startTime == proof.StartTimeTick {
				return false, nil
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("verify host PID exit: %w", err)
		}
	}
	return p.noContainerCgroup(containerID)
}

func (p hostProcessInspector) requireCgroupV2() error {
	root, err := os.Lstat(p.cgroupRoot)
	if err != nil || !root.IsDir() || root.Mode()&os.ModeSymlink != 0 {
		return errors.New("guardian requires a safe cgroup v2 mount directory")
	}
	info, err := os.Lstat(filepath.Join(p.cgroupRoot, "cgroup.controllers"))
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("guardian requires readable unified cgroup v2 at /sys/fs/cgroup")
	}
	return nil
}

func (p hostProcessInspector) noContainerCgroup(containerID string) (bool, error) {
	found := false
	err := filepath.WalkDir(p.cgroupRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if name == containerID || name == "docker-"+containerID+".scope" {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("scan host cgroup tree: %w", err)
	}
	return !found, nil
}

func cgroupPathForRoot(root, path string) string {
	return filepath.Join(root, strings.TrimPrefix(path, "/"))
}

func secureCgroupDirectory(root, cgroupPath string) (string, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || !validCgroupPath(cgroupPath) {
		return "", errors.New("cgroup path is invalid")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("cgroup v2 mount is unavailable or unsafe")
	}
	current := root
	for _, segment := range strings.Split(strings.TrimPrefix(cgroupPath, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", errors.New("cgroup path contains an invalid segment")
		}
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("cgroup path traverses a non-directory or symlink")
		}
	}
	return current, nil
}
