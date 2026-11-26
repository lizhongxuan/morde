//go:build linux

package dshguardian

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
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

func (p hostProcessInspector) SnapshotTaskProcess(containerID string, runtimeProof processProof, proof taskProcessProof,
	taskKey string) ([]TaskProcessSnapshotEntry, error) {
	taskDir, err := p.validateTaskProof(containerID, runtimeProof, proof, taskKey)
	if err != nil {
		return nil, err
	}
	if err := p.requireTaskCgroup(taskDir); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		first, err := p.readTaskProcessSnapshot(containerID, proof, taskDir)
		if err != nil {
			return nil, err
		}
		second, err := p.readTaskProcessSnapshot(containerID, proof, taskDir)
		if err != nil {
			return nil, err
		}
		if sameTaskProcessSnapshotIdentity(first, second) {
			return second, nil
		}
	}
	return nil, errors.New("Task process tree changed while its bounded snapshot was captured")
}

type taskProcessSnapshotRecord struct {
	hostPID       int
	hostParentPID int
	containerPID  int
	cgroupPath    string
	state         string
	startTimeTick uint64
}

func (p hostProcessInspector) readTaskProcessSnapshot(containerID string, proof taskProcessProof,
	taskDir string) ([]TaskProcessSnapshotEntry, error) {
	processesByHostPID := make(map[int]taskProcessSnapshotRecord)
	cgroupPaths := make(map[string]struct{})
	cgroupCount := 0
	err := filepath.WalkDir(taskDir, func(dir string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("Task cgroup tree contains a symlink")
		}
		if !entry.IsDir() {
			return nil
		}
		info, statErr := os.Lstat(dir)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("Task cgroup tree contains an unsafe directory")
		}
		relative, relErr := filepath.Rel(taskDir, dir)
		if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("Task cgroup traversal escaped its root")
		}
		cgroupPath := proof.CgroupPath
		if relative != "." {
			cgroupPath = path.Join(cgroupPath, filepath.ToSlash(relative))
		}
		if !cgroupPathWithin(proof.CgroupPath, cgroupPath) || !containerCgroupMatches(cgroupPath, containerID) {
			return errors.New("Task cgroup descendant escaped the guarded Runtime")
		}
		cgroupCount++
		if cgroupCount > maxTaskProcessSnapshotCgroups {
			return errors.New("Task cgroup subtree exceeds the snapshot size limit")
		}
		cgroupPaths[cgroupPath] = struct{}{}
		procsPath := filepath.Join(dir, "cgroup.procs")
		procsInfo, infoErr := os.Lstat(procsPath)
		if infoErr != nil || !procsInfo.Mode().IsRegular() || procsInfo.Mode()&os.ModeSymlink != 0 {
			return errors.New("Task cgroup process list is unavailable or unsafe")
		}
		pids, readErr := readTaskCgroupPIDsBounded(procsPath, maxTaskProcessSnapshotProcesses-len(processesByHostPID))
		if readErr != nil {
			return readErr
		}
		for _, hostPID := range pids {
			if _, duplicate := processesByHostPID[hostPID]; duplicate {
				return errors.New("Task process appears in multiple cgroup directories")
			}
			if len(processesByHostPID) >= maxTaskProcessSnapshotProcesses {
				return errors.New("Task process tree exceeds the snapshot size limit")
			}
			record, recordErr := p.readTaskProcessSnapshotRecord(hostPID, proof.NamespaceInode, proof.CgroupPath)
			if recordErr != nil {
				return recordErr
			}
			processesByHostPID[hostPID] = record
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("capture Task cgroup process tree: %w", err)
	}
	if len(processesByHostPID) == 0 {
		return nil, errors.New("Task cgroup process tree is empty")
	}
	for _, record := range processesByHostPID {
		if _, ok := cgroupPaths[record.cgroupPath]; !ok {
			return nil, errors.New("Task process membership changed during cgroup traversal")
		}
	}
	root, ok := processesByHostPID[proof.HostPID]
	if !ok || root.containerPID != proof.ContainerPID || root.startTimeTick != proof.StartTimeTick {
		return nil, errors.New("fenced Task root process is missing or has been reused")
	}

	containerPIDByHostPID := make(map[int]int, len(processesByHostPID))
	for hostPID, record := range processesByHostPID {
		if _, duplicate := containerPIDByHostPID[record.containerPID]; duplicate {
			return nil, errors.New("Task process PID namespace mapping is ambiguous")
		}
		containerPIDByHostPID[hostPID] = record.containerPID
	}
	entries := make([]TaskProcessSnapshotEntry, 0, len(processesByHostPID))
	seenContainerPIDs := make(map[int]struct{}, len(processesByHostPID))
	for _, record := range processesByHostPID {
		if _, duplicate := seenContainerPIDs[record.containerPID]; duplicate {
			return nil, errors.New("Task process PID namespace mapping is ambiguous")
		}
		seenContainerPIDs[record.containerPID] = struct{}{}
		entry := TaskProcessSnapshotEntry{PID: record.containerPID, State: record.state, StartTimeTick: record.startTimeTick,
			ParentOutsideScope: true}
		if parentPID, inside := containerPIDByHostPID[record.hostParentPID]; inside {
			if parentPID == record.containerPID {
				return nil, errors.New("Task process has an invalid self-parent relationship")
			}
			entry.ParentPID = parentPID
			entry.ParentOutsideScope = false
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].PID < entries[j].PID })
	return entries, nil
}

func (p hostProcessInspector) readTaskProcessSnapshotRecord(hostPID int, namespaceInode uint64,
	taskCgroupPath string) (taskProcessSnapshotRecord, error) {
	procDir := filepath.Join(p.procRoot, strconv.Itoa(hostPID))
	first, err := os.ReadFile(filepath.Join(procDir, "stat"))
	if err != nil {
		return taskProcessSnapshotRecord{}, fmt.Errorf("read Task process identity: %w", err)
	}
	state, hostParentPID, startTimeTick, err := parseProcSnapshotStat(first)
	if err != nil {
		return taskProcessSnapshotRecord{}, err
	}
	cgroupData, err := os.ReadFile(filepath.Join(procDir, "cgroup"))
	if err != nil {
		return taskProcessSnapshotRecord{}, fmt.Errorf("read Task process cgroup identity: %w", err)
	}
	cgroupPath, err := parseUnifiedCgroup(cgroupData)
	if err != nil || !cgroupPathWithin(taskCgroupPath, cgroupPath) {
		return taskProcessSnapshotRecord{}, errors.New("Task process is outside its Guardian cgroup subtree")
	}
	actualNamespace, err := p.pidNamespaceInode(hostPID)
	if err != nil || actualNamespace != namespaceInode {
		return taskProcessSnapshotRecord{}, errors.New("Task process PID namespace changed or is unavailable")
	}
	nspids, err := readProcNSpid(filepath.Join(procDir, "status"))
	if err != nil || len(nspids) == 0 || nspids[0] != hostPID {
		return taskProcessSnapshotRecord{}, errors.New("Task process namespace PID mapping is invalid")
	}
	second, err := os.ReadFile(filepath.Join(procDir, "stat"))
	if err != nil {
		return taskProcessSnapshotRecord{}, fmt.Errorf("recheck Task process identity: %w", err)
	}
	secondState, secondParentPID, secondStartTimeTick, err := parseProcSnapshotStat(second)
	if err != nil || secondStartTimeTick != startTimeTick || secondParentPID != hostParentPID {
		return taskProcessSnapshotRecord{}, errors.New("Task process PID was reused or reparented during capture")
	}
	secondCgroupData, err := os.ReadFile(filepath.Join(procDir, "cgroup"))
	if err != nil {
		return taskProcessSnapshotRecord{}, fmt.Errorf("recheck Task process cgroup identity: %w", err)
	}
	secondCgroupPath, err := parseUnifiedCgroup(secondCgroupData)
	if err != nil || secondCgroupPath != cgroupPath || len(nspids) == 0 || nspids[len(nspids)-1] < 1 {
		return taskProcessSnapshotRecord{}, errors.New("Task process scope or namespace PID changed during capture")
	}
	return taskProcessSnapshotRecord{hostPID: hostPID, hostParentPID: hostParentPID, containerPID: nspids[len(nspids)-1],
		cgroupPath: secondCgroupPath, state: secondState, startTimeTick: startTimeTick}, nil
}

func readTaskCgroupPIDsBounded(path string, maxPIDs int) ([]int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	pids := make([]int, 0)
	for scanner.Scan() {
		pid, parseErr := strconv.Atoi(strings.TrimSpace(scanner.Text()))
		if parseErr != nil || pid < 2 {
			return nil, errors.New("Task cgroup process list contains an invalid PID")
		}
		if len(pids) >= maxPIDs {
			return nil, errors.New("Task cgroup process list exceeds the snapshot size limit")
		}
		pids = append(pids, pid)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return pids, nil
}

func readProcNSpid(statusPath string) ([]int, error) {
	file, err := os.Open(statusPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Bytes()
		if strings.HasPrefix(string(line), "NSpid:") {
			return parseNSpid(append([]byte(nil), line...))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("proc status does not expose NSpid")
}

func parseProcSnapshotStat(data []byte) (string, int, uint64, error) {
	text := string(data)
	end := strings.LastIndexByte(text, ')')
	if end < 0 || end+1 >= len(text) {
		return "", 0, 0, errors.New("proc stat record is malformed")
	}
	fields := strings.Fields(text[end+1:])
	if len(fields) <= 19 || len(fields[0]) != 1 {
		return "", 0, 0, errors.New("proc stat record lacks process state or identity")
	}
	parentPID, err := strconv.Atoi(fields[1])
	if err != nil || parentPID < 0 {
		return "", 0, 0, errors.New("proc parent PID is invalid")
	}
	startTimeTick, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || startTimeTick == 0 {
		return "", 0, 0, errors.New("proc start time is invalid")
	}
	return fields[0], parentPID, startTimeTick, nil
}

func sameTaskProcessSnapshotIdentity(first, second []TaskProcessSnapshotEntry) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		left, right := first[index], second[index]
		if left.PID != right.PID || left.ParentPID != right.ParentPID || left.ParentOutsideScope != right.ParentOutsideScope ||
			left.StartTimeTick != right.StartTimeTick {
			return false
		}
	}
	return true
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
			return errors.New("runtime cgroup tree contains a symlink")
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
