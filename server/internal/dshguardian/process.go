package dshguardian

import (
	"errors"
	"strconv"
	"strings"
)

type taskProcessInspector interface {
	AttachTaskProcess(containerID string, runtimeProof processProof, containerPID int, taskKey string) (taskProcessProof, error)
	KillTaskProcess(containerID string, runtimeProof processProof, proof taskProcessProof, taskKey string) error
	TaskProcessGone(containerID string, runtimeProof processProof, proof taskProcessProof, taskKey string) (bool, error)
	RemoveTaskCgroup(containerID string, runtimeProof processProof, proof taskProcessProof, taskKey string) error
}

func containerCgroupMatches(path, containerID string) bool {
	if !validCgroupPath(path) || !containerIDRE.MatchString(containerID) {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == containerID || segment == "docker-"+containerID+".scope" {
			return true
		}
	}
	return false
}

func parseUnifiedCgroup(data []byte) (string, error) {
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || parts[0] != "0" || parts[1] != "" {
			continue
		}
		if !validCgroupPath(parts[2]) {
			return "", errors.New("unified cgroup path is invalid")
		}
		return parts[2], nil
	}
	return "", errors.New("unified cgroup membership is unavailable")
}

func parseProcStartTime(data []byte) (uint64, error) {
	text := string(data)
	end := strings.LastIndexByte(text, ')')
	if end < 0 || end+1 >= len(text) {
		return 0, errors.New("proc stat record is malformed")
	}
	fields := strings.Fields(text[end+1:])
	// The first field after the command is field 3 (state); starttime is field 22.
	if len(fields) <= 19 {
		return 0, errors.New("proc stat record lacks start time")
	}
	value, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || value == 0 {
		return 0, errors.New("proc start time is invalid")
	}
	return value, nil
}
