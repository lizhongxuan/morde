//go:build linux

package dshguardian

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (p hostProcessInspector) AttachTaskProcess(containerID string, runtimeProof processProof, containerPID int, taskKey string) (taskProcessProof, error) {
	if err := p.validateTaskBoundary(containerID, runtimeProof, taskKey); err != nil {
		return taskProcessProof{}, err
	}
	if containerPID < 2 {
		return taskProcessProof{}, errors.New("container PID hint is invalid")
	}
	identity, err := p.resolveContainerPID(containerID, runtimeProof, containerPID)
	if err != nil {
		return taskProcessProof{}, err
	}
	taskPath := taskCgroupPath(runtimeProof.CgroupPath, taskKey)
	taskDir := cgroupPathForRoot(p.cgroupRoot, taskPath)
	err = os.Mkdir(taskDir, 0o700)
	created := err == nil
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return taskProcessProof{}, fmt.Errorf("create task cgroup: %w", err)
	}
	if err := p.requireTaskCgroup(taskDir); err != nil {
		return taskProcessProof{}, err
	}
	populated, err := readCgroupPopulated(filepath.Join(taskDir, "cgroup.events"))
	if err != nil || populated {
		if err != nil || created || identity.CgroupPath != taskPath {
			return taskProcessProof{}, errors.New("task cgroup is not empty before attachment")
		}
		proof, verifyErr := p.verifyTaskProcess(containerID, runtimeProof, identity.HostPID, containerPID, identity.StartTimeTick, identity.NamespaceInode, taskPath)
		if verifyErr != nil {
			return taskProcessProof{}, verifyErr
		}
		members, readErr := readCgroupPIDs(filepath.Join(taskDir, "cgroup.procs"))
		if readErr != nil || !containsPID(members, proof.HostPID) {
			return taskProcessProof{}, errors.New("existing task cgroup does not contain the exact process")
		}
		if err := p.validateTaskBoundary(containerID, runtimeProof, taskKey); err != nil {
			return taskProcessProof{}, err
		}
		return proof, nil
	}
	if identity.CgroupPath == taskPath {
		return taskProcessProof{}, errors.New("task process escaped an empty task cgroup")
	}
	if err := writeCgroupPID(filepath.Join(taskDir, "cgroup.procs"), identity.HostPID); err != nil {
		return taskProcessProof{}, fmt.Errorf("move exact task process into its cgroup: %w", err)
	}
	proof, err := p.verifyTaskProcess(containerID, runtimeProof, identity.HostPID, containerPID, identity.StartTimeTick, identity.NamespaceInode, taskPath)
	if err != nil {
		return taskProcessProof{}, err
	}
	processes, err := readCgroupPIDs(filepath.Join(taskDir, "cgroup.procs"))
	if err != nil || len(processes) != 1 || processes[0] != proof.HostPID {
		return taskProcessProof{}, errors.New("task cgroup contains an unexpected process during attachment")
	}
	if err := p.validateTaskBoundary(containerID, runtimeProof, taskKey); err != nil {
		return taskProcessProof{}, err
	}
	return proof, nil
}

func (p hostProcessInspector) KillTaskProcess(containerID string, runtimeProof processProof, proof taskProcessProof, taskKey string) error {
	taskDir, err := p.validateTaskProof(containerID, runtimeProof, proof, taskKey)
	if err != nil {
		return err
	}
	info, err := os.Lstat(taskDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect task cgroup before kill: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("task cgroup path is not a safe directory")
	}
	if err := p.requireTaskCgroup(taskDir); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(taskDir, "cgroup.kill"), os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open task cgroup.kill: %w", err)
	}
	written, writeErr := file.Write([]byte("1"))
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("kill task cgroup: %w", writeErr)
	}
	if written != 1 || closeErr != nil {
		return errors.New("task cgroup.kill did not confirm the process-tree signal")
	}
	return nil
}

func (p hostProcessInspector) TaskProcessGone(containerID string, runtimeProof processProof, proof taskProcessProof, taskKey string) (bool, error) {
	taskDir, err := p.validateTaskProof(containerID, runtimeProof, proof, taskKey)
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(taskDir)
	if err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false, errors.New("task cgroup path is not a safe directory")
		}
		eventsInfo, statErr := os.Lstat(filepath.Join(taskDir, "cgroup.events"))
		if statErr != nil || eventsInfo.Mode()&os.ModeSymlink != 0 || !eventsInfo.Mode().IsRegular() {
			return false, errors.New("task cgroup.events is unavailable or unsafe")
		}
		populated, err := readCgroupPopulated(filepath.Join(taskDir, "cgroup.events"))
		if err != nil {
			return false, err
		}
		if populated {
			return false, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("inspect task cgroup after kill: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(p.procRoot, strconv.Itoa(proof.HostPID), "stat"))
	if err == nil {
		startTime, parseErr := parseProcStartTime(data)
		if parseErr != nil {
			return false, parseErr
		}
		if startTime == proof.StartTimeTick {
			return false, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("verify task host PID exit: %w", err)
	}
	return true, nil
}

func (p hostProcessInspector) RemoveTaskCgroup(containerID string, runtimeProof processProof, proof taskProcessProof, taskKey string) error {
	taskDir, err := p.validateTaskProof(containerID, runtimeProof, proof, taskKey)
	if err != nil {
		return err
	}
	if gone, err := p.TaskProcessGone(containerID, runtimeProof, proof, taskKey); err != nil || !gone {
		if err != nil {
			return err
		}
		return errors.New("task cgroup cannot be removed while its process identity remains")
	}
	if err := os.Remove(taskDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove empty task cgroup: %w", err)
	}
	if _, err := os.Lstat(taskDir); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return errors.New("task cgroup identity remains after removal")
	}
	return nil
}

func (p hostProcessInspector) resolveContainerPID(containerID string, runtimeProof processProof, containerPID int) (taskProcessProof, error) {
	baseDir, err := secureCgroupDirectory(p.cgroupRoot, runtimeProof.CgroupPath)
	if err != nil {
		return taskProcessProof{}, errors.New("account Runtime cgroup is unavailable")
	}
	initNamespace, err := p.pidNamespaceInode(runtimeProof.PID)
	if err != nil {
		return taskProcessProof{}, fmt.Errorf("read Docker init PID namespace: %w", err)
	}
	var matches []taskProcessProof
	err = filepath.WalkDir(baseDir, func(dir string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("Runtime cgroup tree contains a symlink")
		}
		if !entry.IsDir() {
			return nil
		}
		members, readErr := readCgroupPIDs(filepath.Join(dir, "cgroup.procs"))
		if errors.Is(readErr, fs.ErrNotExist) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		for _, hostPID := range members {
			identity, identityErr := p.readCandidate(containerID, runtimeProof.CgroupPath, hostPID, containerPID, initNamespace)
			if errors.Is(identityErr, fs.ErrNotExist) {
				continue
			}
			if identityErr != nil {
				return identityErr
			}
			if identity.HostPID != 0 {
				matches = append(matches, identity)
			}
		}
		return nil
	})
	if err != nil {
		return taskProcessProof{}, fmt.Errorf("scan exact Runtime cgroup for namespace PID: %w", err)
	}
	if len(matches) != 1 {
		return taskProcessProof{}, errors.New("container PID did not resolve to exactly one host process")
	}
	return matches[0], nil
}

func (p hostProcessInspector) readCandidate(containerID, runtimeCgroup string, hostPID, containerPID int, namespaceInode uint64) (taskProcessProof, error) {
	if hostPID < 2 {
		return taskProcessProof{}, nil
	}
	procDir := filepath.Join(p.procRoot, strconv.Itoa(hostPID))
	first, err := os.ReadFile(filepath.Join(procDir, "stat"))
	if err != nil {
		return taskProcessProof{}, err
	}
	startTime, err := parseProcStartTime(first)
	if err != nil {
		return taskProcessProof{}, err
	}
	currentNamespace, err := p.pidNamespaceInode(hostPID)
	if err != nil {
		return taskProcessProof{}, err
	}
	if currentNamespace != namespaceInode {
		return taskProcessProof{}, nil
	}
	status, err := os.ReadFile(filepath.Join(procDir, "status"))
	if err != nil {
		return taskProcessProof{}, err
	}
	nspids, err := parseNSpid(status)
	if err != nil || len(nspids) == 0 || nspids[0] != hostPID {
		return taskProcessProof{}, errors.New("host process NSpid identity is malformed")
	}
	if nspids[len(nspids)-1] != containerPID {
		return taskProcessProof{}, nil
	}
	cgroupData, err := os.ReadFile(filepath.Join(procDir, "cgroup"))
	if err != nil {
		return taskProcessProof{}, err
	}
	cgroupPath, err := parseUnifiedCgroup(cgroupData)
	if err != nil {
		return taskProcessProof{}, err
	}
	if !cgroupPathWithin(runtimeCgroup, cgroupPath) || !containerCgroupMatches(cgroupPath, containerID) {
		return taskProcessProof{}, errors.New("host process cgroup changed while namespace PID was resolved")
	}
	second, err := os.ReadFile(filepath.Join(procDir, "stat"))
	if err != nil {
		return taskProcessProof{}, err
	}
	secondStart, err := parseProcStartTime(second)
	if err != nil || secondStart != startTime {
		return taskProcessProof{}, errors.New("host PID changed while namespace identity was captured")
	}
	return taskProcessProof{ContainerPID: containerPID, HostPID: hostPID, StartTimeTick: startTime, CgroupPath: cgroupPath, NamespaceInode: namespaceInode}, nil
}

func (p hostProcessInspector) verifyTaskProcess(containerID string, runtimeProof processProof, hostPID, containerPID int, startTime, namespaceInode uint64, expectedCgroup string) (taskProcessProof, error) {
	if hostPID < 2 || startTime == 0 || namespaceInode == 0 || !cgroupPathWithin(runtimeProof.CgroupPath, expectedCgroup) || !containerCgroupMatches(expectedCgroup, containerID) {
		return taskProcessProof{}, errors.New("task process proof does not identify the Runtime cgroup")
	}
	procDir := filepath.Join(p.procRoot, strconv.Itoa(hostPID))
	first, err := os.ReadFile(filepath.Join(procDir, "stat"))
	if err != nil {
		return taskProcessProof{}, err
	}
	actualStart, err := parseProcStartTime(first)
	if err != nil || actualStart != startTime {
		return taskProcessProof{}, errors.New("task host PID start time changed")
	}
	actualNamespace, err := p.pidNamespaceInode(hostPID)
	if err != nil || actualNamespace != namespaceInode {
		return taskProcessProof{}, errors.New("task host PID namespace changed")
	}
	status, err := os.ReadFile(filepath.Join(procDir, "status"))
	if err != nil {
		return taskProcessProof{}, err
	}
	nspids, err := parseNSpid(status)
	if err != nil || len(nspids) == 0 || nspids[0] != hostPID || nspids[len(nspids)-1] != containerPID {
		return taskProcessProof{}, errors.New("task container PID mapping changed")
	}
	cgroupData, err := os.ReadFile(filepath.Join(procDir, "cgroup"))
	if err != nil {
		return taskProcessProof{}, err
	}
	cgroupPath, err := parseUnifiedCgroup(cgroupData)
	if err != nil || cgroupPath != expectedCgroup {
		return taskProcessProof{}, errors.New("task process is no longer in its dedicated cgroup")
	}
	second, err := os.ReadFile(filepath.Join(procDir, "stat"))
	if err != nil {
		return taskProcessProof{}, err
	}
	secondStart, err := parseProcStartTime(second)
	if err != nil || secondStart != startTime {
		return taskProcessProof{}, errors.New("task host PID changed during cgroup verification")
	}
	return taskProcessProof{ContainerPID: containerPID, HostPID: hostPID, StartTimeTick: startTime, CgroupPath: cgroupPath, NamespaceInode: namespaceInode}, nil
}

func (p hostProcessInspector) validateTaskBoundary(containerID string, runtimeProof processProof, taskKey string) error {
	if err := p.requireCgroupV2(); err != nil {
		return err
	}
	if !containerIDRE.MatchString(containerID) || runtimeProof.PID < 2 || runtimeProof.StartTimeTick == 0 ||
		!containerCgroupMatches(runtimeProof.CgroupPath, containerID) || !validTaskKey(taskKey) {
		return errors.New("task cgroup boundary identity is invalid")
	}
	current, err := p.Capture(containerID, runtimeProof.PID)
	if err != nil || current != runtimeProof {
		return errors.New("account Runtime host PID or cgroup identity changed")
	}
	return nil
}

func (p hostProcessInspector) validateTaskProof(containerID string, runtimeProof processProof, proof taskProcessProof, taskKey string) (string, error) {
	if err := p.validateTaskBoundary(containerID, runtimeProof, taskKey); err != nil {
		return "", err
	}
	if proof.ContainerPID < 2 || proof.HostPID < 2 || proof.StartTimeTick == 0 || proof.NamespaceInode == 0 ||
		proof.CgroupPath != taskCgroupPath(runtimeProof.CgroupPath, taskKey) || !cgroupPathWithin(runtimeProof.CgroupPath, proof.CgroupPath) {
		return "", errors.New("saved task process evidence is invalid")
	}
	return cgroupPathForRoot(p.cgroupRoot, proof.CgroupPath), nil
}

func (p hostProcessInspector) requireTaskCgroup(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("task cgroup directory is unavailable or unsafe")
	}
	for _, name := range []string{"cgroup.procs", "cgroup.kill", "cgroup.events"} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("task cgroup does not support %s: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("task cgroup control %s is unsafe", name)
		}
	}
	return nil
}

func (p hostProcessInspector) pidNamespaceInode(pid int) (uint64, error) {
	target, err := os.Readlink(filepath.Join(p.procRoot, strconv.Itoa(pid), "ns", "pid"))
	if err != nil {
		return 0, err
	}
	if !strings.HasPrefix(target, "pid:[") || !strings.HasSuffix(target, "]") {
		return 0, errors.New("proc PID namespace identity is malformed")
	}
	inode, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(target, "pid:["), "]"), 10, 64)
	if err != nil || inode == 0 {
		return 0, errors.New("proc PID namespace inode is invalid")
	}
	return inode, nil
}

func readCgroupPIDs(path string) ([]int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, line := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(line)
		if err != nil || pid < 2 {
			return nil, errors.New("cgroup.procs contains an invalid PID")
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

func writeCgroupPID(path string, pid int) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	written, writeErr := file.Write([]byte(strconv.Itoa(pid)))
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if written != len(strconv.Itoa(pid)) || closeErr != nil {
		return errors.New("cgroup.procs did not confirm process attachment")
	}
	return nil
}

func readCgroupPopulated(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read cgroup.events: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "populated" {
			if fields[1] == "0" {
				return false, nil
			}
			if fields[1] == "1" {
				return true, nil
			}
		}
	}
	return false, errors.New("cgroup.events lacks a valid populated value")
}

func parseNSpid(status []byte) ([]int, error) {
	for _, line := range strings.Split(string(status), "\n") {
		if !strings.HasPrefix(line, "NSpid:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "NSpid:"))
		if len(fields) == 0 {
			return nil, errors.New("proc NSpid list is empty")
		}
		pids := make([]int, 0, len(fields))
		for _, field := range fields {
			pid, err := strconv.Atoi(field)
			if err != nil || pid < 1 {
				return nil, errors.New("proc NSpid list is invalid")
			}
			pids = append(pids, pid)
		}
		return pids, nil
	}
	return nil, errors.New("proc status does not expose NSpid")
}

func containsPID(pids []int, pid int) bool {
	for _, candidate := range pids {
		if candidate == pid {
			return true
		}
	}
	return false
}
