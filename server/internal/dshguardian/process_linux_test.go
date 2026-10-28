//go:build linux

package dshguardian

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostProcessInspectorUsesFakeProcAndCgroupTrees(t *testing.T) {
	procRoot := filepath.Join(t.TempDir(), "proc")
	cgroupRoot := filepath.Join(t.TempDir(), "cgroup")
	containerID := strings.Repeat("a", 64)
	cgroupPath := "/system.slice/docker-" + containerID + ".scope"
	procDir := filepath.Join(procRoot, "321")
	cgroupDir := cgroupPathForRoot(cgroupRoot, cgroupPath)
	if err := os.MkdirAll(procDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cgroupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroupRoot, "cgroup.controllers"), []byte("cpu memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroupDir, "cgroup.kill"), []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeProc := func(startTime uint64) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(procDir, "stat"), fakeProcStat(321, startTime), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeProc(998877)
	if err := os.WriteFile(filepath.Join(procDir, "cgroup"), []byte("0::"+cgroupPath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	inspector := hostProcessInspector{procRoot: procRoot, cgroupRoot: cgroupRoot}
	if err := inspector.Ready(); err != nil {
		t.Fatalf("fake unified cgroup v2 was rejected: %v", err)
	}
	proof, err := inspector.Capture(containerID, 321)
	if err != nil || proof.PID != 321 || proof.StartTimeTick != 998877 || proof.CgroupPath != cgroupPath {
		t.Fatalf("Capture() = %+v, %v", proof, err)
	}
	if err := inspector.KillTree(containerID, proof); err != nil {
		t.Fatalf("KillTree() = %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(cgroupDir, "cgroup.kill")); err != nil || string(got) != "1" {
		t.Fatalf("cgroup.kill = %q, %v", got, err)
	}
	if err := os.Remove(filepath.Join(cgroupDir, "cgroup.kill")); err != nil {
		t.Fatal(err)
	}
	if err := inspector.KillTree(containerID, proof); err == nil {
		t.Fatal("KillTree accepted a cgroup without its kill control")
	}
	if gone, err := inspector.Gone(containerID, proof); err != nil || gone {
		t.Fatalf("Gone() accepted the original PID: gone=%t err=%v", gone, err)
	}
	writeTestFile(t, filepath.Join(procDir, "stat"), "invalid proc stat")
	if gone, err := inspector.Gone(containerID, proof); err == nil || gone {
		t.Fatalf("Gone() accepted a malformed live PID record: gone=%t err=%v", gone, err)
	}
	if _, err := inspector.Capture(containerID, 321); err == nil {
		t.Fatal("Capture() accepted a malformed host PID record")
	}

	writeProc(998878) // PID reuse must not be mistaken for the original process.
	if gone, err := inspector.Gone(containerID, proof); err != nil || gone {
		t.Fatalf("Gone() ignored the remaining container cgroup: gone=%t err=%v", gone, err)
	}
	if err := os.RemoveAll(procDir); err != nil {
		t.Fatal(err)
	}
	if gone, err := inspector.Gone(containerID, proof); err != nil || gone {
		t.Fatalf("Gone() ignored the remaining cgroup directory: gone=%t err=%v", gone, err)
	}
	if err := os.RemoveAll(cgroupDir); err != nil {
		t.Fatal(err)
	}
	if gone, err := inspector.Gone(containerID, proof); err != nil || !gone {
		t.Fatalf("Gone() after PID and cgroup removal = %t, %v", gone, err)
	}
	if err := inspector.KillTree(containerID, proof); err != nil {
		t.Fatalf("replayed cgroup kill after removal: %v", err)
	}
	if _, err := inspector.Capture(containerID, 321); err == nil {
		t.Fatal("Capture() accepted a vanished host PID")
	}
}

func TestHostProcessInspectorRejectsForeignOrEscapingCgroups(t *testing.T) {
	procRoot := filepath.Join(t.TempDir(), "proc")
	cgroupRoot := filepath.Join(t.TempDir(), "cgroup")
	containerID := strings.Repeat("a", 64)
	otherID := strings.Repeat("b", 64)
	procDir := filepath.Join(procRoot, "321")
	if err := os.MkdirAll(procDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cgroupRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroupRoot, "cgroup.controllers"), []byte("cpu memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procDir, "stat"), fakeProcStat(321, 998877), 0o600); err != nil {
		t.Fatal(err)
	}
	inspector := hostProcessInspector{procRoot: procRoot, cgroupRoot: cgroupRoot}
	for _, path := range []string{
		"/system.slice/docker-" + otherID + ".scope",
		"/../docker-" + containerID + ".scope",
	} {
		if err := os.WriteFile(filepath.Join(procDir, "cgroup"), []byte("0::"+path+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if proof, err := inspector.Capture(containerID, 321); err == nil {
			t.Fatalf("Capture() accepted cgroup path %q: %+v", path, proof)
		}
	}

	foreignProof := processProof{PID: 321, StartTimeTick: 998877, CgroupPath: "/system.slice/docker-" + otherID + ".scope"}
	if err := inspector.KillTree(containerID, foreignProof); err == nil {
		t.Fatal("KillTree() accepted process evidence for another container")
	}
	if gone, err := inspector.Gone(containerID, foreignProof); err == nil || gone {
		t.Fatalf("Gone() accepted process evidence for another container: gone=%t err=%v", gone, err)
	}
}

func TestHostProcessInspectorRejectsSymlinkedCgroupMount(t *testing.T) {
	dir := t.TempDir()
	realRoot := filepath.Join(dir, "real-cgroup")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(realRoot, "cgroup.controllers"), "cpu memory")
	linkRoot := filepath.Join(dir, "cgroup")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	inspector := hostProcessInspector{procRoot: filepath.Join(dir, "proc"), cgroupRoot: linkRoot}
	if err := inspector.Ready(); err == nil {
		t.Fatal("Guardian accepted a symlinked cgroup root")
	}
	if gone, err := inspector.Gone(strings.Repeat("a", 64), processProof{}); err == nil || gone {
		t.Fatalf("Guardian claimed process destruction through a symlinked cgroup root: gone=%t err=%v", gone, err)
	}
}

func TestHostProcessInspectorRejectsUnsafeCgroupControlsAndPath(t *testing.T) {
	dir := t.TempDir()
	procRoot := filepath.Join(dir, "proc")
	cgroupRoot := filepath.Join(dir, "cgroup")
	if err := os.Mkdir(cgroupRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	inspector := hostProcessInspector{procRoot: procRoot, cgroupRoot: cgroupRoot}
	if err := inspector.Ready(); err == nil {
		t.Fatal("Guardian accepted a cgroup mount without v2 controllers")
	}
	controllers := filepath.Join(cgroupRoot, "cgroup.controllers")
	writeTestFile(t, filepath.Join(dir, "outside-controllers"), "cpu memory")
	if err := os.Symlink(filepath.Join(dir, "outside-controllers"), controllers); err != nil {
		t.Fatal(err)
	}
	if err := inspector.Ready(); err == nil {
		t.Fatal("Guardian followed a symlinked cgroup.controllers control")
	}
	if err := os.Remove(controllers); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(controllers, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := inspector.Ready(); err == nil {
		t.Fatal("Guardian accepted a directory as cgroup.controllers")
	}
	if err := os.Remove(controllers); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, controllers, "cpu memory")
	containerID := strings.Repeat("a", 64)
	if _, err := inspector.Capture("foreign", 321); err == nil {
		t.Fatal("Guardian captured a process under an invalid container ID")
	}
	if _, err := inspector.Capture(containerID, 1); err == nil {
		t.Fatal("Guardian captured PID 1 as a container process")
	}
	if gone, err := inspector.Gone("bad-container-id", processProof{}); err == nil || gone {
		t.Fatal("Guardian claimed destruction under an invalid container ID")
	}
	cgroupPath := "/system.slice/docker-" + containerID + ".scope"
	if err := os.MkdirAll(filepath.Join(procRoot, "321"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(procRoot, "321", "stat"), string(fakeProcStat(321, 998877)))
	writeTestFile(t, filepath.Join(procRoot, "321", "cgroup"), "0::"+cgroupPath+"\n")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(filepath.Join(outside, "docker-"+containerID+".scope"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(outside, "docker-"+containerID+".scope", "cgroup.kill"), "0")
	if err := os.Symlink(outside, filepath.Join(cgroupRoot, "system.slice")); err != nil {
		t.Fatal(err)
	}
	if _, err := inspector.Capture(containerID, 321); err == nil {
		t.Fatal("Guardian captured process evidence through a symlinked cgroup path")
	}
	proof := processProof{PID: 321, StartTimeTick: 998877, CgroupPath: cgroupPath}
	if err := inspector.KillTree(containerID, proof); err == nil {
		t.Fatal("Guardian signaled a process tree through a symlinked cgroup path")
	}
	if data, err := os.ReadFile(filepath.Join(outside, "docker-"+containerID+".scope", "cgroup.kill")); err != nil || string(data) != "0" {
		t.Fatalf("Guardian changed an outside cgroup: %q, %v", data, err)
	}
}

func TestTaskCgroupUsesExactPIDNamespaceMappingAndStopsOnlyOneTask(t *testing.T) {
	procRoot := filepath.Join(t.TempDir(), "proc")
	cgroupRoot := filepath.Join(t.TempDir(), "cgroup")
	containerID := strings.Repeat("a", 64)
	runtimePath := "/system.slice/docker-" + containerID + ".scope"
	taskKey := strings.Repeat("1", 64)
	taskPath := taskCgroupPath(runtimePath, taskKey)
	runtimeDir := cgroupPathForRoot(cgroupRoot, runtimePath)
	taskDir := cgroupPathForRoot(cgroupRoot, taskPath)
	siblingDir := cgroupPathForRoot(cgroupRoot, taskCgroupPath(runtimePath, strings.Repeat("2", 64)))
	for _, dir := range []string{runtimeDir, taskDir, siblingDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(cgroupRoot, "cgroup.controllers"), "cpu memory")
	writeTestFile(t, filepath.Join(runtimeDir, "cgroup.procs"), "100\n")
	writeTaskCgroupFiles(t, taskDir, "101\n", "populated 1\n", "0")
	writeTaskCgroupFiles(t, siblingDir, "", "populated 0\n", "0")
	writeFakeNamespaceProc(t, procRoot, 100, 1000, 77, []int{100, 1}, runtimePath)
	writeFakeNamespaceProc(t, procRoot, 101, 1001, 77, []int{101, 2}, taskPath)

	inspector := hostProcessInspector{procRoot: procRoot, cgroupRoot: cgroupRoot}
	runtimeProof, err := inspector.Capture(containerID, 100)
	if err != nil {
		t.Fatalf("capture fake Docker init: %v", err)
	}
	proof, err := inspector.AttachTaskProcess(containerID, runtimeProof, 2, taskKey)
	if err != nil {
		t.Fatalf("resolve existing child in exact task cgroup: %v", err)
	}
	if proof.ContainerPID != 2 || proof.HostPID != 101 || proof.StartTimeTick != 1001 || proof.CgroupPath != taskPath || proof.NamespaceInode != 77 {
		t.Fatalf("task process mapping proof = %+v", proof)
	}
	if err := inspector.RemoveTaskCgroup(containerID, runtimeProof, proof, taskKey); err == nil {
		t.Fatal("Task cgroup removal accepted a live process")
	}
	if err := inspector.KillTaskProcess(containerID, runtimeProof, proof, taskKey); err != nil {
		t.Fatalf("kill task cgroup: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(taskDir, "cgroup.kill")); err != nil || string(got) != "1" {
		t.Fatalf("task cgroup.kill = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(siblingDir, "cgroup.kill")); err != nil || string(got) != "0" {
		t.Fatalf("sibling cgroup was affected: %q, %v", got, err)
	}
	if err := os.RemoveAll(taskDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(siblingDir, taskDir); err != nil {
		t.Fatal(err)
	}
	if err := inspector.KillTaskProcess(containerID, runtimeProof, proof, taskKey); err == nil {
		t.Fatal("task kill followed a symlink in the cgroup path")
	}
	if err := inspector.RemoveTaskCgroup(containerID, runtimeProof, proof, taskKey); err == nil {
		t.Fatal("Task cgroup removal followed a symlink into its sibling")
	}
	if got, err := os.ReadFile(filepath.Join(siblingDir, "cgroup.kill")); err != nil || string(got) != "0" {
		t.Fatalf("symlink task cgroup escaped into its sibling: %q, %v", got, err)
	}
	if err := os.Remove(taskDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(taskDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTaskCgroupFiles(t, taskDir, "101\n", "populated 1\n", "0")
	if gone, err := inspector.TaskProcessGone(containerID, runtimeProof, proof, taskKey); err != nil || gone {
		t.Fatalf("Task stop accepted a populated cgroup: gone=%t err=%v", gone, err)
	}
	writeTestFile(t, filepath.Join(taskDir, "cgroup.events"), "populated 0\n")
	if gone, err := inspector.TaskProcessGone(containerID, runtimeProof, proof, taskKey); err != nil || gone {
		t.Fatalf("Task stop accepted a live original PID: gone=%t err=%v", gone, err)
	}
	writeTestFile(t, filepath.Join(procRoot, "101", "stat"), string(fakeProcStat(101, 1002)))
	writeTestFile(t, filepath.Join(procRoot, "101", "cgroup"), "0::"+runtimePath+"\n")
	if gone, err := inspector.TaskProcessGone(containerID, runtimeProof, proof, taskKey); err != nil || !gone {
		t.Fatalf("Task stop mistook a reused host PID for the original process: gone=%t err=%v", gone, err)
	}
	if err := os.RemoveAll(filepath.Join(procRoot, "101")); err != nil {
		t.Fatal(err)
	}
	if gone, err := inspector.TaskProcessGone(containerID, runtimeProof, proof, taskKey); err != nil || !gone {
		t.Fatalf("Task stop did not require both empty cgroup and absent PID identity: gone=%t err=%v", gone, err)
	}
	if err := os.RemoveAll(taskDir); err != nil {
		t.Fatal(err)
	}
	if gone, err := inspector.TaskProcessGone(containerID, runtimeProof, proof, taskKey); err != nil || !gone {
		t.Fatalf("removed task cgroup identity was not accepted: gone=%t err=%v", gone, err)
	}
	if err := inspector.KillTaskProcess(containerID, runtimeProof, proof, taskKey); err != nil {
		t.Fatalf("replayed Task kill after cgroup removal: %v", err)
	}
	if err := inspector.RemoveTaskCgroup(containerID, runtimeProof, proof, taskKey); err != nil {
		t.Fatalf("replayed Task cgroup removal after destruction: %v", err)
	}
}

func TestTaskCgroupPIDWriteUsesExactHostPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cgroup.procs")
	writeTestFile(t, path, "000")
	if err := writeCgroupPID(path, 321); err != nil {
		t.Fatalf("write host PID to cgroup.procs: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "321" {
		t.Fatalf("cgroup.procs got %q, %v; want exact host PID", data, err)
	}
	if err := writeCgroupPID(filepath.Join(t.TempDir(), "missing", "cgroup.procs"), 321); err == nil {
		t.Fatal("Task attachment ignored an unavailable cgroup.procs control")
	}
}

func TestTaskProcessProofFailsClosedOnHostIdentityDrift(t *testing.T) {
	procRoot := filepath.Join(t.TempDir(), "proc")
	cgroupRoot := filepath.Join(t.TempDir(), "cgroup")
	containerID := strings.Repeat("a", 64)
	runtimePath := "/system.slice/docker-" + containerID + ".scope"
	taskKey := strings.Repeat("1", 64)
	taskPath := taskCgroupPath(runtimePath, taskKey)
	runtimeDir := cgroupPathForRoot(cgroupRoot, runtimePath)
	taskDir := cgroupPathForRoot(cgroupRoot, taskPath)
	if err := os.MkdirAll(taskDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(cgroupRoot, "cgroup.controllers"), "cpu memory")
	writeTestFile(t, filepath.Join(runtimeDir, "cgroup.procs"), "100\n")
	writeTaskCgroupFiles(t, taskDir, "101\n", "populated 1\n", "0")
	writeFakeNamespaceProc(t, procRoot, 100, 1000, 77, []int{100, 1}, runtimePath)
	writeFakeNamespaceProc(t, procRoot, 101, 1001, 77, []int{101, 2}, taskPath)
	inspector := hostProcessInspector{procRoot: procRoot, cgroupRoot: cgroupRoot}
	runtimeProof, err := inspector.Capture(containerID, 100)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := inspector.verifyTaskProcess(containerID, runtimeProof, 101, 2, 1001, 77, taskPath)
	if err != nil {
		t.Fatalf("verify exact Task identity: %v", err)
	}
	if _, err := inspector.validateTaskProof(containerID, runtimeProof, proof, taskKey); err != nil {
		t.Fatalf("validate persisted Task proof: %v", err)
	}
	wrongProof := proof
	wrongProof.CgroupPath = taskCgroupPath(runtimePath, strings.Repeat("2", 64))
	if _, err := inspector.validateTaskProof(containerID, runtimeProof, wrongProof, taskKey); err == nil {
		t.Fatal("Guardian accepted a Task proof for a sibling cgroup")
	}
	if err := inspector.KillTaskProcess(containerID, runtimeProof, wrongProof, taskKey); err == nil {
		t.Fatal("Guardian killed a Task under a sibling cgroup proof")
	}
	if err := inspector.RemoveTaskCgroup(containerID, runtimeProof, wrongProof, taskKey); err == nil {
		t.Fatal("Guardian removed a Task under a sibling cgroup proof")
	}
	eventsPath := filepath.Join(taskDir, "cgroup.events")
	if err := os.Remove(eventsPath); err != nil {
		t.Fatal(err)
	}
	if gone, err := inspector.TaskProcessGone(containerID, runtimeProof, proof, taskKey); err == nil || gone {
		t.Fatal("Guardian claimed Task destruction without cgroup.events")
	}
	if err := os.Symlink(filepath.Join(runtimeDir, "cgroup.procs"), eventsPath); err != nil {
		t.Fatal(err)
	}
	if gone, err := inspector.TaskProcessGone(containerID, runtimeProof, proof, taskKey); err == nil || gone {
		t.Fatal("Guardian followed a symlinked cgroup.events during Task proof")
	}
	if err := os.Remove(eventsPath); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, eventsPath, "populated unknown\n")
	if gone, err := inspector.TaskProcessGone(containerID, runtimeProof, proof, taskKey); err == nil || gone {
		t.Fatal("Guardian claimed Task destruction from malformed cgroup.events")
	}
	writeTestFile(t, eventsPath, "populated 1\n")

	childDir := filepath.Join(procRoot, "101")
	writeTestFile(t, filepath.Join(childDir, "status"), "Name:\tfake\nNSpid:\t101 3\n")
	if _, err := inspector.verifyTaskProcess(containerID, runtimeProof, 101, 2, 1001, 77, taskPath); err == nil {
		t.Fatal("Guardian accepted a changed container PID mapping")
	}
	writeTestFile(t, filepath.Join(childDir, "status"), "Name:\tfake\nNSpid:\tbad\n")
	if _, err := inspector.readCandidate(containerID, runtimePath, 101, 2, 77); err == nil {
		t.Fatal("Guardian accepted a malformed NSpid record")
	}
	writeTestFile(t, filepath.Join(childDir, "status"), "Name:\tfake\nNSpid:\t101 2\n")

	namespacePath := filepath.Join(childDir, "ns", "pid")
	if err := os.Remove(namespacePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pid:[78]", namespacePath); err != nil {
		t.Fatal(err)
	}
	if _, err := inspector.verifyTaskProcess(containerID, runtimeProof, 101, 2, 1001, 77, taskPath); err == nil {
		t.Fatal("Guardian accepted a changed PID namespace")
	}
	if err := os.Remove(namespacePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pid:[77]", namespacePath); err != nil {
		t.Fatal(err)
	}

	writeTestFile(t, filepath.Join(childDir, "cgroup"), "0::/user.slice/foreign.scope\n")
	if _, err := inspector.readCandidate(containerID, runtimePath, 101, 2, 77); err == nil {
		t.Fatal("Guardian accepted a foreign cgroup during PID resolution")
	}
	if _, err := inspector.verifyTaskProcess(containerID, runtimeProof, 101, 2, 1001, 77, taskPath); err == nil {
		t.Fatal("Guardian accepted a Task that left its dedicated cgroup")
	}
	writeTestFile(t, filepath.Join(childDir, "cgroup"), "0::"+taskPath+"\n")
	writeTestFile(t, filepath.Join(childDir, "stat"), string(fakeProcStat(101, 1002)))
	if _, err := inspector.verifyTaskProcess(containerID, runtimeProof, 101, 2, 1001, 77, taskPath); err == nil {
		t.Fatal("Guardian accepted a reused Task host PID")
	}
	writeTestFile(t, filepath.Join(childDir, "stat"), string(fakeProcStat(101, 1001)))

	writeTestFile(t, filepath.Join(procRoot, "100", "stat"), string(fakeProcStat(100, 1002)))
	if err := inspector.KillTaskProcess(containerID, runtimeProof, proof, taskKey); err == nil {
		t.Fatal("Guardian killed a Task under a reused account Runtime PID")
	}
	if data, err := os.ReadFile(filepath.Join(taskDir, "cgroup.kill")); err != nil || string(data) != "0" {
		t.Fatalf("rejected Task kill changed cgroup.kill: %q, %v", data, err)
	}
}

func TestTaskAttachmentRejectsOccupiedOrUnsafeCgroup(t *testing.T) {
	procRoot := filepath.Join(t.TempDir(), "proc")
	cgroupRoot := filepath.Join(t.TempDir(), "cgroup")
	containerID := strings.Repeat("a", 64)
	runtimePath := "/system.slice/docker-" + containerID + ".scope"
	taskKey := strings.Repeat("1", 64)
	taskDir := cgroupPathForRoot(cgroupRoot, taskCgroupPath(runtimePath, taskKey))
	runtimeDir := cgroupPathForRoot(cgroupRoot, runtimePath)
	if err := os.MkdirAll(taskDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(cgroupRoot, "cgroup.controllers"), "cpu memory")
	writeTestFile(t, filepath.Join(runtimeDir, "cgroup.procs"), "100\n101\n")
	writeTaskCgroupFiles(t, taskDir, "999\n", "populated 1\n", "0")
	writeFakeNamespaceProc(t, procRoot, 100, 1000, 77, []int{100, 1}, runtimePath)
	writeFakeNamespaceProc(t, procRoot, 101, 1001, 77, []int{101, 2}, runtimePath)
	inspector := hostProcessInspector{procRoot: procRoot, cgroupRoot: cgroupRoot}
	runtimeProof, err := inspector.Capture(containerID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inspector.AttachTaskProcess(containerID, runtimeProof, 1, taskKey); err == nil {
		t.Fatal("Guardian accepted an invalid container PID")
	}
	if _, err := inspector.AttachTaskProcess(containerID, runtimeProof, 2, "../foreign"); err == nil {
		t.Fatal("Guardian accepted a Task key outside the Runtime cgroup")
	}
	if _, err := inspector.AttachTaskProcess(containerID, runtimeProof, 2, taskKey); err == nil {
		t.Fatal("Guardian attached a Task to an occupied cgroup")
	}
	writeTestFile(t, filepath.Join(taskDir, "cgroup.events"), "populated unknown\n")
	if _, err := inspector.AttachTaskProcess(containerID, runtimeProof, 2, taskKey); err == nil {
		t.Fatal("Guardian attached a Task when cgroup occupancy was unparseable")
	}
	if err := os.RemoveAll(taskDir); err != nil {
		t.Fatal(err)
	}
	siblingDir := cgroupPathForRoot(cgroupRoot, taskCgroupPath(runtimePath, strings.Repeat("2", 64)))
	if err := os.Mkdir(siblingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTaskCgroupFiles(t, siblingDir, "999\n", "populated 1\n", "0")
	if err := os.Symlink(siblingDir, taskDir); err != nil {
		t.Fatal(err)
	}
	if _, err := inspector.AttachTaskProcess(containerID, runtimeProof, 2, taskKey); err == nil {
		t.Fatal("Guardian followed a symlink into another Task cgroup")
	}
	if data, err := os.ReadFile(filepath.Join(siblingDir, "cgroup.procs")); err != nil || string(data) != "999\n" {
		t.Fatalf("rejected attachment changed sibling cgroup: %q, %v", data, err)
	}
	if err := os.Remove(taskDir); err != nil {
		t.Fatal(err)
	}
	if _, err := inspector.AttachTaskProcess(containerID, runtimeProof, 2, taskKey); err == nil {
		t.Fatal("Guardian attached a Task when kernel cgroup controls were unavailable")
	}
}

func TestLinuxTaskControlParsersRejectMalformedState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control")
	for _, content := range []string{"1\n", "not-a-pid\n"} {
		writeTestFile(t, path, content)
		if _, err := readCgroupPIDs(path); err == nil {
			t.Fatalf("accepted malformed cgroup.procs %q", content)
		}
	}
	for _, content := range []string{"populated unknown\n", "frozen 0\n"} {
		writeTestFile(t, path, content)
		if _, err := readCgroupPopulated(path); err == nil {
			t.Fatalf("accepted malformed cgroup.events %q", content)
		}
	}
	for _, content := range []string{"Name:\tfake\n", "NSpid:\t\n", "NSpid:\t101 bad\n", "NSpid:\t101 0\n"} {
		if _, err := parseNSpid([]byte(content)); err == nil {
			t.Fatalf("accepted malformed /proc status %q", content)
		}
	}
	procRoot := filepath.Join(dir, "proc")
	namespaceDir := filepath.Join(procRoot, "101", "ns")
	if err := os.MkdirAll(namespaceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	inspector := hostProcessInspector{procRoot: procRoot}
	for _, target := range []string{"pid:[0]", "pid:[bad]", "wrong:[77]"} {
		link := filepath.Join(namespaceDir, "pid")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := inspector.pidNamespaceInode(101); err == nil {
			t.Fatalf("accepted malformed PID namespace link %q", target)
		}
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
	}
	taskDir := filepath.Join(dir, "task")
	if err := os.Mkdir(taskDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := inspector.requireTaskCgroup(taskDir); err == nil {
		t.Fatal("accepted Task cgroup without kernel controls")
	}
	writeTaskCgroupFiles(t, taskDir, "", "populated 0\n", "0")
	if err := inspector.requireTaskCgroup(taskDir); err != nil {
		t.Fatalf("rejected intact Task cgroup controls: %v", err)
	}
	if err := os.Remove(filepath.Join(taskDir, "cgroup.kill")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(taskDir, "cgroup.kill")); err != nil {
		t.Fatal(err)
	}
	if err := inspector.requireTaskCgroup(taskDir); err == nil {
		t.Fatal("accepted a symlinked Task cgroup control file")
	}
}

func TestTaskPIDResolutionFailsClosedForAmbiguousOrForeignProcesses(t *testing.T) {
	tests := []struct {
		name       string
		candidates []struct {
			pid       int
			namespace uint64
			cgroup    string
		}
	}{
		{name: "ambiguous namespace PID", candidates: []struct {
			pid       int
			namespace uint64
			cgroup    string
		}{{101, 77, ""}, {102, 77, ""}}},
		{name: "foreign PID namespace", candidates: []struct {
			pid       int
			namespace uint64
			cgroup    string
		}{{101, 78, ""}}},
		{name: "foreign cgroup", candidates: []struct {
			pid       int
			namespace uint64
			cgroup    string
		}{{101, 77, "/user.slice/foreign.scope"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			procRoot := filepath.Join(t.TempDir(), "proc")
			cgroupRoot := filepath.Join(t.TempDir(), "cgroup")
			containerID := strings.Repeat("a", 64)
			runtimePath := "/system.slice/docker-" + containerID + ".scope"
			runtimeDir := cgroupPathForRoot(cgroupRoot, runtimePath)
			if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(cgroupRoot, "cgroup.controllers"), "cpu memory")
			writeTestFile(t, filepath.Join(runtimeDir, "cgroup.procs"), "100\n")
			writeFakeNamespaceProc(t, procRoot, 100, 1000, 77, []int{100, 1}, runtimePath)
			members := []string{"100"}
			for _, candidate := range test.candidates {
				path := runtimePath
				if candidate.cgroup != "" {
					path = candidate.cgroup
					foreignDir := cgroupPathForRoot(cgroupRoot, path)
					if err := os.MkdirAll(foreignDir, 0o700); err != nil {
						t.Fatal(err)
					}
					writeTestFile(t, filepath.Join(foreignDir, "cgroup.procs"), fmt.Sprintf("%d\n", candidate.pid))
				} else {
					members = append(members, fmt.Sprint(candidate.pid))
				}
				writeFakeNamespaceProc(t, procRoot, candidate.pid, uint64(candidate.pid+900), candidate.namespace, []int{candidate.pid, 2}, path)
			}
			writeTestFile(t, filepath.Join(runtimeDir, "cgroup.procs"), strings.Join(members, "\n")+"\n")
			inspector := hostProcessInspector{procRoot: procRoot, cgroupRoot: cgroupRoot}
			runtimeProof := processProof{PID: 100, StartTimeTick: 1000, CgroupPath: runtimePath}
			if _, err := inspector.resolveContainerPID(containerID, runtimeProof, 2); err == nil {
				t.Fatal("namespace PID mapping was accepted without one exact host process")
			}
		})
	}
}

func writeTaskCgroupFiles(t *testing.T, dir, pids, events, kill string) {
	t.Helper()
	writeTestFile(t, filepath.Join(dir, "cgroup.procs"), pids)
	writeTestFile(t, filepath.Join(dir, "cgroup.events"), events)
	writeTestFile(t, filepath.Join(dir, "cgroup.kill"), kill)
}

func writeFakeNamespaceProc(t *testing.T, root string, pid int, startTime, namespace uint64, nspids []int, cgroup string) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(filepath.Join(dir, "ns"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "stat"), string(fakeProcStat(pid, startTime)))
	writeTestFile(t, filepath.Join(dir, "status"), fmt.Sprintf("Name:\tfake\nNSpid:\t%s\n", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(nspids)), " "), "[]")))
	writeTestFile(t, filepath.Join(dir, "cgroup"), "0::"+cgroup+"\n")
	if err := os.Symlink(fmt.Sprintf("pid:[%d]", namespace), filepath.Join(dir, "ns", "pid")); err != nil {
		t.Fatal(err)
	}
}

func writeTestFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fakeProcStat(pid int, startTime uint64) []byte {
	fields := []string{"S"}
	for len(fields) < 19 {
		fields = append(fields, "0")
	}
	fields = append(fields, fmt.Sprint(startTime))
	return []byte(fmt.Sprintf("%d (fake dsh process) %s", pid, strings.Join(fields, " ")))
}
