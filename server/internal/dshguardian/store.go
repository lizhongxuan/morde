package dshguardian

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"morde/server/internal/app"
)

type leaseStore struct {
	mu          sync.Mutex
	dir         string
	data        map[string]lease
	runtimes    map[string]accountRuntimeLease
	taskProcess map[string]taskProcessLease
}

func openLeaseStore(dir string) (*leaseStore, error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || dir == "/" {
		return nil, errors.New("guardian state directory must be a clean absolute path")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create guardian state directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o700 != 0o700 || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("guardian state directory must be a private, non-symlink directory")
	}
	store := &leaseStore{dir: dir, data: make(map[string]lease), runtimes: make(map[string]accountRuntimeLease), taskProcess: make(map[string]taskProcessLease)}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *leaseStore) load() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("read guardian state directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".lease-") && strings.HasSuffix(name, ".tmp") {
			info, statErr := entry.Info()
			if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o600 != 0o600 || info.Mode().Perm()&0o077 != 0 {
				return errors.New("guardian state contains an unsafe temporary file")
			}
			if err := os.Remove(filepath.Join(s.dir, name)); err != nil {
				return fmt.Errorf("remove incomplete guardian state file: %w", err)
			}
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			return fmt.Errorf("guardian state contains an unexpected file %q", name)
		}
		id := strings.TrimSuffix(name, ".json")
		kind := "agent"
		if strings.HasPrefix(id, "runtime-") {
			kind, id = "runtime", strings.TrimPrefix(id, "runtime-")
		} else if strings.HasPrefix(id, "task-") {
			kind, id = "task", strings.TrimPrefix(id, "task-")
		}
		validID := containerIDRE.MatchString(id)
		if kind == "task" {
			validID = validTaskKey(id)
		}
		if !validID || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("guardian state contains an invalid lease filename")
		}
		info, statErr := entry.Info()
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o600 != 0o600 || info.Mode().Perm()&0o077 != 0 || info.Size() > maxRequestBytes {
			return errors.New("guardian lease file permissions or size are invalid")
		}
		file, openErr := os.Open(filepath.Join(s.dir, name))
		if openErr != nil {
			return fmt.Errorf("open guardian lease file: %w", openErr)
		}
		data, readErr := io.ReadAll(io.LimitReader(file, maxRequestBytes+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(data) > maxRequestBytes {
			return errors.New("guardian lease file cannot be read safely")
		}
		switch kind {
		case "agent":
			var value lease
			if err := decodeStrictJSON(data, &value); err != nil {
				return fmt.Errorf("decode guardian lease %s: %w", id, err)
			}
			if value.Guard.ContainerID != id || validPersistedLease(value) != nil || time.Until(value.Guard.ExpiresAt) > app.MaxAgentLease {
				return errors.New("guardian lease state is inconsistent")
			}
			s.data[id] = value
		case "runtime":
			var value accountRuntimeLease
			if err := decodeStrictJSON(data, &value); err != nil {
				return fmt.Errorf("decode account Runtime lease %s: %w", id, err)
			}
			if value.Guard.ContainerID != id || validPersistedAccountRuntimeLease(value) != nil || time.Until(value.Guard.ExpiresAt) > app.MaxAgentLease {
				return errors.New("account Runtime lease state is inconsistent")
			}
			s.runtimes[id] = value
		case "task":
			var value taskProcessLease
			if err := decodeStrictJSON(data, &value); err != nil {
				return fmt.Errorf("decode task process lease %s: %w", id, err)
			}
			if value.Key != id || validPersistedTaskProcessLease(value) != nil || time.Until(value.Guard.ExpiresAt) > app.MaxAgentLease {
				return errors.New("task process lease state is inconsistent")
			}
			s.taskProcess[id] = value
		}
	}
	return nil
}
func (s *leaseStore) get(id string) (lease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.data[id]
	return value, ok
}

func (s *leaseStore) find(fence app.AgentInstanceFence, nonceSHA256 string) (lease, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found lease
	count := 0
	for _, value := range s.data {
		if value.Guard.Fence == fence && value.Guard.NonceSHA256 == nonceSHA256 {
			found = value
			count++
		}
	}
	if count > 1 {
		return lease{}, false, errors.New("multiple guardian leases match one exact Agent fence")
	}
	return found, count == 1, nil
}

func (s *leaseStore) create(value lease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.data[value.Guard.ContainerID]; exists {
		return errors.New("guardian already has a lease for this container")
	}
	if err := s.persist(value); err != nil {
		return err
	}
	s.data[value.Guard.ContainerID] = value
	return nil
}

func (s *leaseStore) replace(value lease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.data[value.Guard.ContainerID]; !exists {
		return errors.New("guardian lease was not found")
	}
	if err := s.persist(value); err != nil {
		return err
	}
	s.data[value.Guard.ContainerID] = value
	return nil
}

func (s *leaseStore) all() []lease {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := make([]lease, 0, len(s.data))
	for _, value := range s.data {
		values = append(values, value)
	}
	return values
}

func (s *leaseStore) getRuntime(id string) (accountRuntimeLease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.runtimes[id]
	return value, ok
}

func (s *leaseStore) findRuntime(fence app.AccountAgentRuntimeFence, nonceSHA256 string) (accountRuntimeLease, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found accountRuntimeLease
	count := 0
	for _, value := range s.runtimes {
		if value.Guard.Fence == fence && value.Guard.NonceSHA256 == nonceSHA256 {
			found, count = value, count+1
		}
	}
	if count > 1 {
		return accountRuntimeLease{}, false, errors.New("multiple account Runtime leases match one exact fence")
	}
	return found, count == 1, nil
}

func (s *leaseStore) createRuntime(value accountRuntimeLease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.runtimes[value.Guard.ContainerID]; exists {
		return errors.New("guardian already has a lease for this Runtime container")
	}
	if err := s.persistNamed("runtime-"+value.Guard.ContainerID+".json", value, validPersistedAccountRuntimeLease(value)); err != nil {
		return err
	}
	s.runtimes[value.Guard.ContainerID] = value
	return nil
}

func (s *leaseStore) replaceRuntime(value accountRuntimeLease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.runtimes[value.Guard.ContainerID]; !exists {
		return errors.New("account Runtime lease was not found")
	}
	if err := s.persistNamed("runtime-"+value.Guard.ContainerID+".json", value, validPersistedAccountRuntimeLease(value)); err != nil {
		return err
	}
	s.runtimes[value.Guard.ContainerID] = value
	return nil
}

func (s *leaseStore) allRuntimes() []accountRuntimeLease {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := make([]accountRuntimeLease, 0, len(s.runtimes))
	for _, value := range s.runtimes {
		values = append(values, value)
	}
	return values
}

func (s *leaseStore) getTask(key string) (taskProcessLease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.taskProcess[key]
	return value, ok
}

func (s *leaseStore) findTask(runtimeFence app.AccountAgentRuntimeFence, taskFence app.AgentInstanceFence, nonceSHA256 string) (taskProcessLease, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found taskProcessLease
	count := 0
	for _, value := range s.taskProcess {
		if value.Guard.RuntimeFence == runtimeFence && value.Guard.TaskFence == taskFence && value.Guard.NonceSHA256 == nonceSHA256 {
			found, count = value, count+1
		}
	}
	if count > 1 {
		return taskProcessLease{}, false, errors.New("multiple task process leases match one exact Task fence")
	}
	return found, count == 1, nil
}

func (s *leaseStore) createTask(value taskProcessLease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.taskProcess[value.Key]; exists {
		return errors.New("guardian already has a lease for this Task process")
	}
	if err := s.persistNamed("task-"+value.Key+".json", value, validPersistedTaskProcessLease(value)); err != nil {
		return err
	}
	s.taskProcess[value.Key] = value
	return nil
}

func (s *leaseStore) replaceTask(value taskProcessLease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.taskProcess[value.Key]; !exists {
		return errors.New("task process lease was not found")
	}
	if err := s.persistNamed("task-"+value.Key+".json", value, validPersistedTaskProcessLease(value)); err != nil {
		return err
	}
	s.taskProcess[value.Key] = value
	return nil
}

func (s *leaseStore) allTasks() []taskProcessLease {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := make([]taskProcessLease, 0, len(s.taskProcess))
	for _, value := range s.taskProcess {
		values = append(values, value)
	}
	return values
}

func (s *leaseStore) acknowledgeTaskProcessProof(runtimeFence app.AccountAgentRuntimeFence, taskFence app.AgentInstanceFence,
	nonceSHA256, proofSHA256 string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var current taskProcessLease
	found := false
	for _, value := range s.taskProcess {
		if value.Guard.RuntimeFence != runtimeFence || value.Guard.TaskFence != taskFence {
			continue
		}
		if value.Guard.NonceSHA256 != nonceSHA256 {
			return errors.New("task process proof acknowledgement nonce does not match")
		}
		if found {
			return errors.New("multiple Task process leases match one proof acknowledgement")
		}
		current, found = value, true
	}
	if !found {
		return s.syncDirectory()
	}
	if current.Guard.RuntimeFence != runtimeFence || current.Guard.TaskFence != taskFence ||
		current.Guard.NonceSHA256 != nonceSHA256 || !current.Destroyed || current.DestroyedAt.IsZero() || current.UnprovenRunning {
		return errors.New("task process proof is not durably destroyed")
	}
	if err := validPersistedTaskProcessLease(current); err != nil {
		return err
	}
	canonical, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("encode durable Task process proof: %w", err)
	}
	path := filepath.Join(s.dir, "task-"+current.Key+".json")
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Mode().Perm()&0o400 == 0 ||
		before.Mode().Perm()&0o077 != 0 ||
		before.Size() > maxRequestBytes {
		return errors.New("durable Task process proof file is unavailable or unsafe")
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open durable Task process proof: %w", err)
	}
	opened, statErr := file.Stat()
	if statErr != nil || !opened.Mode().IsRegular() || opened.Mode().Perm()&0o400 == 0 || opened.Mode().Perm()&0o077 != 0 ||
		!os.SameFile(before, opened) {
		_ = file.Close()
		return errors.New("durable Task process proof changed while opening")
	}
	durable, readErr := io.ReadAll(io.LimitReader(file, maxRequestBytes+1))
	closeErr := file.Close()
	after, afterErr := os.Lstat(path)
	if readErr != nil || closeErr != nil || afterErr != nil || !after.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 ||
		after.Mode().Perm()&0o400 == 0 || after.Mode().Perm()&0o077 != 0 || !os.SameFile(opened, after) || len(durable) > maxRequestBytes {
		return errors.New("durable Task process proof changed while reading")
	}
	if !bytes.Equal(durable, canonical) {
		return errors.New("durable Task process proof differs from its in-memory lease")
	}
	digest := sha256.Sum256(durable)
	if hex.EncodeToString(digest[:]) != proofSHA256 {
		return errors.New("task process proof acknowledgement digest does not match")
	}
	latest, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, latest) {
		return errors.New("durable Task process proof changed before removal")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove acknowledged Task process proof: %w", err)
	}
	delete(s.taskProcess, current.Key)
	return s.syncDirectory()
}

func (s *leaseStore) acknowledgeAccountRuntimeProof(fence app.AccountAgentRuntimeFence, nonceSHA256, proofSHA256 string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var current accountRuntimeLease
	found := false
	for _, value := range s.runtimes {
		if value.Guard.Fence == fence {
			if value.Guard.NonceSHA256 != nonceSHA256 {
				return errors.New("account Runtime proof acknowledgement nonce does not match")
			}
			if found {
				return errors.New("multiple account Runtime leases match one proof acknowledgement")
			}
			current, found = value, true
			continue
		}
		if value.Guard.Fence.OwnerID == fence.OwnerID && value.Guard.Fence.RuntimeID == fence.RuntimeID {
			return errors.New("account Runtime proof acknowledgement fence does not match the stored generation")
		}
	}
	if !found {
		for _, value := range s.taskProcess {
			if value.Guard.RuntimeFence == fence {
				return errors.New("account Runtime proof is missing while Task process records remain")
			}
		}
		return s.syncDirectory()
	}
	if !current.Destroyed || current.DestroyedAt.IsZero() || current.UnprovenRunning {
		return errors.New("account Runtime proof is not durably destroyed")
	}
	if err := validPersistedAccountRuntimeLease(current); err != nil {
		return err
	}
	runtimeCanonical, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("encode durable account Runtime proof: %w", err)
	}
	runtimeName := "runtime-" + current.Guard.ContainerID + ".json"
	runtimeInfo, err := s.verifyCanonicalProofFile(runtimeName, runtimeCanonical)
	if err != nil {
		return fmt.Errorf("verify durable account Runtime proof: %w", err)
	}
	runtimeDigest := sha256.Sum256(runtimeCanonical)
	if hex.EncodeToString(runtimeDigest[:]) != proofSHA256 {
		return errors.New("account Runtime proof acknowledgement digest does not match")
	}
	type taskProofFile struct {
		key  string
		name string
		info os.FileInfo
	}
	taskFiles := make([]taskProofFile, 0)
	for key, value := range s.taskProcess {
		if value.Guard.RuntimeFence != fence {
			continue
		}
		if key != value.Key || value.Guard.RuntimeContainerID != current.Guard.ContainerID {
			return errors.New("task process record does not match its account Runtime fence")
		}
		if err := validPersistedTaskProcessLease(value); err != nil {
			return fmt.Errorf("invalid Task process record for account Runtime proof: %w", err)
		}
		canonical, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode durable Task process record: %w", err)
		}
		name := "task-" + key + ".json"
		info, err := s.verifyCanonicalProofFile(name, canonical)
		if err != nil {
			return fmt.Errorf("verify durable Task process record: %w", err)
		}
		taskFiles = append(taskFiles, taskProofFile{key: key, name: name, info: info})
	}
	for _, taskFile := range taskFiles {
		if err := s.removeVerifiedProofFile(taskFile.name, taskFile.info); err != nil {
			return fmt.Errorf("remove acknowledged Task process record: %w", err)
		}
		delete(s.taskProcess, taskFile.key)
	}
	if err := s.syncDirectory(); err != nil {
		return err
	}
	if err := s.removeVerifiedProofFile(runtimeName, runtimeInfo); err != nil {
		return fmt.Errorf("remove acknowledged account Runtime proof: %w", err)
	}
	delete(s.runtimes, current.Guard.ContainerID)
	return s.syncDirectory()
}

func (s *leaseStore) verifyCanonicalProofFile(name string, canonical []byte) (os.FileInfo, error) {
	if filepath.Base(name) != name {
		return nil, errors.New("proof file name is invalid")
	}
	path := filepath.Join(s.dir, name)
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 ||
		before.Mode().Perm()&0o600 != 0o600 || before.Mode().Perm()&0o077 != 0 || before.Size() > maxRequestBytes {
		return nil, errors.New("proof file is unavailable or unsafe")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open proof file: %w", err)
	}
	opened, statErr := file.Stat()
	if statErr != nil || !opened.Mode().IsRegular() || opened.Mode().Perm()&0o600 != 0o600 ||
		opened.Mode().Perm()&0o077 != 0 || !os.SameFile(before, opened) {
		_ = file.Close()
		return nil, errors.New("proof file changed while opening")
	}
	durable, readErr := io.ReadAll(io.LimitReader(file, maxRequestBytes+1))
	closeErr := file.Close()
	after, afterErr := os.Lstat(path)
	if readErr != nil || closeErr != nil || afterErr != nil || !after.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 ||
		after.Mode().Perm()&0o600 != 0o600 || after.Mode().Perm()&0o077 != 0 || !os.SameFile(opened, after) || len(durable) > maxRequestBytes {
		return nil, errors.New("proof file changed while reading")
	}
	if !bytes.Equal(durable, canonical) {
		return nil, errors.New("durable bytes differ from the in-memory lease")
	}
	return opened, nil
}

func (s *leaseStore) removeVerifiedProofFile(name string, verified os.FileInfo) error {
	path := filepath.Join(s.dir, name)
	latest, err := os.Lstat(path)
	if err != nil || !latest.Mode().IsRegular() || latest.Mode()&os.ModeSymlink != 0 ||
		latest.Mode().Perm()&0o600 != 0o600 || latest.Mode().Perm()&0o077 != 0 || !os.SameFile(verified, latest) {
		return errors.New("durable proof file changed before removal")
	}
	return os.Remove(path)
}

func (s *leaseStore) syncDirectory() error {
	directory, err := os.Open(s.dir)
	if err != nil {
		return fmt.Errorf("open Guardian state directory for sync: %w", err)
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync Guardian state directory: %w", err)
	}
	return nil
}

func (s *leaseStore) persist(value lease) error {
	return s.persistNamed(value.Guard.ContainerID+".json", value, validPersistedLease(value))
}

func (s *leaseStore) persistNamed(name string, value any, validationErr error) error {
	if validationErr != nil {
		return validationErr
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode guardian lease: %w", err)
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Errorf("create guardian lease temp name: %w", err)
	}
	temp := filepath.Join(s.dir, ".lease-"+hex.EncodeToString(random[:])+".tmp")
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create guardian lease temp file: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(temp)
		return fmt.Errorf("restrict guardian lease temp file: %w", err)
	}
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = os.Remove(temp)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write guardian lease: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync guardian lease: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close guardian lease: %w", err)
	}
	final := filepath.Join(s.dir, name)
	if err := os.Rename(temp, final); err != nil {
		return fmt.Errorf("commit guardian lease: %w", err)
	}
	cleanup = false
	directory, err := os.Open(s.dir)
	if err != nil {
		return fmt.Errorf("open guardian state directory for sync: %w", err)
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync guardian state directory: %w", err)
	}
	return nil
}
