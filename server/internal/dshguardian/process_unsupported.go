//go:build !linux

package dshguardian

import "errors"

type unsupportedProcessInspector struct{}

func newProcessInspector() processInspector { return unsupportedProcessInspector{} }

func (unsupportedProcessInspector) Ready() error {
	return errors.New("independent process-tree proof is supported only on Linux cgroup v2 hosts")
}

func (unsupportedProcessInspector) Capture(string, int) (processProof, error) {
	return processProof{}, errors.New("independent process-tree proof is supported only on Linux cgroup v2 hosts")
}

func (unsupportedProcessInspector) KillTree(string, processProof) error {
	return errors.New("independent process-tree kill is supported only on Linux cgroup v2 hosts")
}

func (unsupportedProcessInspector) Gone(string, processProof) (bool, error) {
	return false, errors.New("independent process-tree proof is supported only on Linux cgroup v2 hosts")
}

func (unsupportedProcessInspector) AttachTaskProcess(string, processProof, int, string) (taskProcessProof, error) {
	return taskProcessProof{}, errors.New("task process isolation is supported only on Linux cgroup v2 hosts")
}

func (unsupportedProcessInspector) KillTaskProcess(string, processProof, taskProcessProof, string) error {
	return errors.New("task process isolation is supported only on Linux cgroup v2 hosts")
}

func (unsupportedProcessInspector) TaskProcessGone(string, processProof, taskProcessProof, string) (bool, error) {
	return false, errors.New("task process proof is supported only on Linux cgroup v2 hosts")
}

func (unsupportedProcessInspector) RemoveTaskCgroup(string, processProof, taskProcessProof, string) error {
	return errors.New("task cgroup removal is supported only on Linux cgroup v2 hosts")
}
