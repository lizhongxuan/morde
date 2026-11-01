package runtimeapplications

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"morde-runtime/contract"
)

const (
	WeChatApplicationID = "wechat"
	wechatID            = WeChatApplicationID
	wechatVersion       = "4.1.13.26"
	wechatPackageName   = "wechat"
	wechatURL           = "https://dldir1v6.qq.com/weixin/Universal/Linux/WeChatLinux_x86_64.deb"
	wechatSHA256        = "a0db8caeee8d1e6cfe0aa5ce58710270f8d74a90ff4920f53a3fc22aa0956fe9"
	wechatMaxBytes      = 250 << 20
	wechatTimeout       = 6 * time.Minute
	// shortcut: pre-AIO restore is capped at 60s; raise only with a larger Host ready timeout.
	wechatRestoreTimeout = time.Minute
	wechatMaxHistory     = 128
	wechatMaxLaunches    = 4096
	wechatLaunchTimeout  = 5 * time.Second
	wechatStopTimeout    = 5 * time.Second
	// shortcut: startup cleanup accepts CreateTemp's current 12-character suffix; update matcher if Go changes that format.
	wechatDownloadTempPrefix = ".wechat-download-"
)

var wechatManifest = applicationManifest{
	id: wechatID, pkg: debPackage{
		name: wechatPackageName, version: wechatVersion, architecture: "amd64", url: wechatURL, host: "dldir1v6.qq.com",
		sha256: wechatSHA256, maxBytes: wechatMaxBytes,
	},
	downloadTempPrefix: wechatDownloadTempPrefix,
	installTimeout:     wechatTimeout, restoreTimeout: wechatRestoreTimeout, maxHistory: wechatMaxHistory,
	maxLaunches: wechatMaxLaunches, launchTimeout: wechatLaunchTimeout, stopTimeout: wechatStopTimeout,
	prepareLaunch: prepareWechatLaunch, launchArchitectureSupported: wechatLaunchArchitectureSupported,
}

// Non-Linux builds fail closed. The Linux build installs the existing
// trusted-launcher check during package initialization.
var wechatLauncherTrustCheck = func() bool { return false }

type debPackage struct {
	name, version, architecture, url, host, sha256 string
	maxBytes                                       int64
}

type applicationCommand struct {
	path          string
	args          []string
	captureLimit  int
	captureStdout bool
}

type applicationCommandOutcome struct {
	stdout   []byte
	exitCode int
	err      error
}

type applicationCommandRunner func(context.Context, applicationCommand) applicationCommandOutcome

type storedInstallOperation struct {
	RequestID string                               `json:"request_id"`
	Operation contract.ApplicationInstallOperation `json:"operation"`
}

type installJournal struct {
	Version          int                      `json:"version"`
	CurrentOperation string                   `json:"current_operation"`
	Operations       []storedInstallOperation `json:"operations"`
}

type storedApplicationLaunch struct {
	LaunchID  string                              `json:"launch_id"`
	HostFence contract.HostFence                  `json:"host_fence"`
	Operation contract.ApplicationLaunchOperation `json:"operation"`
}

type applicationLaunchJournal struct {
	Version    int                       `json:"version"`
	Operations []storedApplicationLaunch `json:"operations"`
}

type applicationProcessDomain interface {
	Prepare(string) error
	Start(string, *exec.Cmd, uint32, uint32) error
	Populated(string) (bool, error)
	Stop(context.Context, string) error
	RemoveEmpty(string) error
}

// Installer runs the package and launch rules from one compiled manifest.
type Installer struct {
	mu                                        sync.Mutex
	dir, packagePath, journalPath, launchPath string
	ownerUID                                  uint32
	client                                    *http.Client
	run                                       applicationCommandRunner
	manifest                                  applicationManifest
	now                                       func() time.Time
	parent                                    context.Context
	journal                                   installJournal
	launchJournal                             applicationLaunchJournal
	launchDomain                              applicationProcessDomain
	launchContext                             context.Context
	launchCancel                              context.CancelFunc
	launchWorkers                             sync.WaitGroup
	launchWatchers                            sync.WaitGroup
	prepareLaunch                             func(context.Context) (*exec.Cmd, uint32, uint32, string)
	installedCheck                            func() bool
	launcherTrusted                           func() bool
	launchArchitectureSupported               func() bool
	cancel                                    context.CancelFunc
	done                                      chan struct{}
	started, closed, restoring, storageFailed bool
}

func openInstaller(options installerOptions) (*Installer, error) {
	manifest, ok := manifestFor(options.applicationID)
	if !ok {
		return nil, ErrUnsupportedApplication
	}
	if os.Geteuid() != int(options.expectedOwnerUID) || !filepath.IsAbs(options.storeDir) || filepath.Clean(options.storeDir) != options.storeDir {
		return nil, errors.New("application store is unsafe")
	}
	storeInfo, err := os.Lstat(options.storeDir)
	if err != nil || storeInfo.Mode()&os.ModeSymlink != 0 || !storeInfo.IsDir() || storeInfo.Mode().Perm()&0022 != 0 || fileUID(storeInfo) != options.expectedOwnerUID {
		return nil, errors.New("application store is unsafe")
	}
	dir := filepath.Join(options.storeDir, manifest.id)
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, errors.New("application store is unavailable")
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil || dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() || dirInfo.Mode().Perm() != 0700 || fileUID(dirInfo) != options.expectedOwnerUID {
		return nil, errors.New("application store is unsafe")
	}
	if err := removeInterruptedApplicationDownloads(dir, options.expectedOwnerUID, manifest.downloadTempPrefix); err != nil {
		return nil, errors.New("application download recovery failed")
	}
	installer := &Installer{
		dir: dir, packagePath: filepath.Join(dir, manifest.pkg.name+"_"+manifest.pkg.version+"_"+manifest.pkg.architecture+".deb"),
		journalPath: filepath.Join(dir, "install.json"), launchPath: filepath.Join(dir, "launches.json"), ownerUID: options.expectedOwnerUID,
		client: applicationHTTPClient(manifest.pkg.host, manifest.installTimeout), run: runApplicationCommand, manifest: manifest,
		now: time.Now, journal: installJournal{Version: 1, Operations: []storedInstallOperation{}},
		launchJournal: applicationLaunchJournal{Version: 1, Operations: []storedApplicationLaunch{}},
		launchDomain:  options.launchDomain, prepareLaunch: manifest.prepareLaunch,
		launcherTrusted:             wechatLauncherTrustCheck,
		launchArchitectureSupported: manifest.launchArchitectureSupported,
	}
	if err := installer.load(); err != nil {
		return nil, err
	}
	if err := installer.loadLaunchJournal(); err != nil {
		return nil, err
	}
	return installer, nil
}

// Start resumes accepted or interrupted work and restores the cached package
// after Sandbox recreation. Package hash and Debian metadata are checked again.
func (i *Installer) Start(parent context.Context) error {
	if parent == nil {
		parent = context.Background()
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return errors.New("application installer is closed")
	}
	if i.restoring {
		return ErrInstallInProgress
	}
	if i.storageFailed {
		return errApplicationStoreUnavailable
	}
	if i.started {
		return nil
	}
	i.parent = parent
	if i.launchContext == nil {
		i.launchContext, i.launchCancel = context.WithCancel(parent)
	}
	current := i.currentLocked()
	if current == nil {
		return nil
	}
	if runtime.GOARCH != "amd64" {
		if current.Operation.Status == contract.OperationAccepted || current.Operation.Status == contract.OperationRunning ||
			current.Operation.Status == contract.OperationFailed && current.Operation.ErrorCode == "operation_interrupted" {
			return i.updateLocked(current.Operation.ID, contract.OperationFailed, 0, "architecture_unsupported")
		}
		return nil
	}
	switch current.Operation.Status {
	case contract.OperationAccepted, contract.OperationRunning:
		return i.startLocked(current.Operation.ID)
	case contract.OperationFailed:
		if current.Operation.ErrorCode == "operation_interrupted" {
			return i.startLocked(current.Operation.ID)
		}
	}
	return nil
}

// Restore synchronously reinstalls the last verified package before AIO starts.
func (i *Installer) Restore(ctx context.Context) error {
	if runtime.GOARCH != "amd64" {
		return nil
	}
	return i.restore(ctx)
}

func (i *Installer) restore(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	i.mu.Lock()
	current := i.currentLocked()
	if i.storageFailed {
		i.mu.Unlock()
		return errApplicationStoreUnavailable
	}
	if i.closed || i.started || i.restoring {
		i.mu.Unlock()
		return ErrInstallInProgress
	}
	if current == nil || current.Operation.Status != contract.OperationAccepted && current.Operation.Status != contract.OperationRunning &&
		current.Operation.Status != contract.OperationSucceeded &&
		(current.Operation.Status != contract.OperationFailed || current.Operation.ErrorCode != "operation_interrupted") {
		i.mu.Unlock()
		return nil
	}
	i.restoring = true
	id := current.Operation.ID
	if err := i.updateLocked(id, contract.OperationRunning, 0, ""); err != nil {
		i.restoring = false
		i.mu.Unlock()
		return errApplicationStoreUnavailable
	}
	i.mu.Unlock()
	defer func() {
		i.mu.Lock()
		i.restoring = false
		i.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(ctx, i.manifest.restoreTimeout)
	defer cancel()
	installErr := i.ensurePackage(ctx, id)
	if installErr == nil {
		result := i.run(ctx, applicationCommand{path: "/usr/bin/dpkg", args: []string{"-i", i.packagePath}})
		if result.err != nil || result.exitCode != 0 {
			installErr = errInstall
		}
	}
	if ctx.Err() != nil {
		installErr = errInterrupted
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if installErr != nil {
		progress := currentProgress(i.findLocked(id))
		if ctx.Err() != nil {
			progress = 0
		}
		if err := i.updateLocked(id, contract.OperationFailed, progress, installErrorCode(installErr)); err != nil {
			return errApplicationStoreUnavailable
		}
		return nil
	}
	if err := i.updateLocked(id, contract.OperationSucceeded, 100, ""); err != nil {
		return errApplicationStoreUnavailable
	}
	return nil
}

// Install is an idempotent ensure-installed request. Matching request IDs
// return their original operation; a different request cannot overlap active work.
func (i *Installer) Install(applicationID, requestID string) (contract.ApplicationInstallOperation, error) {
	if !validLocalRequestID(requestID) {
		return contract.ApplicationInstallOperation{}, ErrInvalidInstallRequest
	}
	if applicationID != i.manifest.id {
		return contract.ApplicationInstallOperation{}, ErrUnsupportedApplication
	}
	if runtime.GOARCH != "amd64" {
		return contract.ApplicationInstallOperation{}, ErrArchitectureUnsupported
	}
	return i.installRequest(requestID)
}

// installRequest contains the platform-independent journal flow. Install
// applies the production architecture gate before reaching it.
func (i *Installer) installRequest(requestID string) (contract.ApplicationInstallOperation, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed || i.storageFailed {
		return contract.ApplicationInstallOperation{}, errApplicationStoreUnavailable
	}
	if i.restoring {
		return contract.ApplicationInstallOperation{}, ErrInstallInProgress
	}
	for _, entry := range i.journal.Operations {
		if entry.RequestID == requestID {
			return entry.Operation, nil
		}
	}
	current := i.currentLocked()
	if current != nil {
		if current.Operation.Status == contract.OperationAccepted || current.Operation.Status == contract.OperationRunning {
			return contract.ApplicationInstallOperation{}, ErrInstallInProgress
		}
		if current.Operation.Status == contract.OperationSucceeded {
			return current.Operation, nil
		}
	}
	id, err := newInstallID()
	if err != nil {
		return contract.ApplicationInstallOperation{}, errApplicationStoreUnavailable
	}
	now := i.now().UTC()
	entry := storedInstallOperation{RequestID: requestID, Operation: contract.ApplicationInstallOperation{
		ID: id, ApplicationID: i.manifest.id, Status: contract.OperationAccepted,
		ProgressPercent: 0, CreatedAt: now, UpdatedAt: now,
	}}
	i.journal.Operations = append(i.journal.Operations, entry)
	if len(i.journal.Operations) > i.manifest.maxHistory {
		i.journal.Operations = append([]storedInstallOperation(nil), i.journal.Operations[len(i.journal.Operations)-i.manifest.maxHistory:]...)
	}
	i.journal.CurrentOperation = id
	if err := i.persistLocked(); err != nil {
		return contract.ApplicationInstallOperation{}, errApplicationStoreUnavailable
	}
	if err := i.startLocked(id); err != nil {
		_ = i.updateLocked(id, contract.OperationFailed, 0, "storage_error")
		return contract.ApplicationInstallOperation{}, errApplicationStoreUnavailable
	}
	return entry.Operation, nil
}

func (i *Installer) Status() contract.ApplicationStatusItem {
	item := contract.ApplicationStatusItem{ID: i.manifest.id, LoginStatus: contract.ApplicationLoginNotApplicable, Installable: runtime.GOARCH == "amd64"}
	i.mu.Lock()
	if i.storageFailed {
		i.mu.Unlock()
		return contract.ApplicationStatusItem{}
	}
	i.mu.Unlock()
	if version := i.installedVersion(); version == i.manifest.pkg.version {
		item.Installed, item.Version, item.LoginStatus = true, version, contract.ApplicationLoginUnknown
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.storageFailed {
		return contract.ApplicationStatusItem{}
	}
	if current := i.currentLocked(); current != nil {
		operation := current.Operation
		item.InstallRequestID = current.RequestID
		item.InstallOperation = &operation
	}
	return item
}

func (i *Installer) Close() error {
	i.mu.Lock()
	if i.closed {
		done := i.done
		i.mu.Unlock()
		if done != nil {
			<-done
		}
		i.launchWorkers.Wait()
		i.launchWatchers.Wait()
		return nil
	}
	i.closed = true
	cancel, done, launchCancel := i.cancel, i.done, i.launchCancel
	i.mu.Unlock()
	if launchCancel != nil {
		launchCancel()
	}
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	i.launchWorkers.Wait()
	i.launchWatchers.Wait()
	return nil
}

func (i *Installer) startLocked(operationID string) error {
	if i.started {
		return nil
	}
	parent := i.parent
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	i.cancel, i.done, i.started = cancel, done, true
	current := i.findLocked(operationID)
	if current == nil {
		i.started = false
		cancel()
		return errStore
	}
	if current.Operation.Status != contract.OperationRunning {
		if err := i.updateLocked(operationID, contract.OperationRunning, 0, ""); err != nil {
			i.started = false
			cancel()
			return err
		}
	}
	go i.execute(ctx, operationID, done)
	return nil
}

func (i *Installer) execute(ctx context.Context, operationID string, done chan struct{}) {
	defer func() {
		i.mu.Lock()
		i.started = false
		i.mu.Unlock()
		close(done)
	}()
	var installErr error
	if err := i.ensurePackage(ctx, operationID); err != nil {
		installErr = err
	} else {
		_ = i.setProgress(operationID, 85)
		commandCtx, cancel := context.WithTimeout(ctx, i.manifest.installTimeout)
		result := i.run(commandCtx, applicationCommand{path: "/usr/bin/dpkg", args: []string{"-i", i.packagePath}})
		cancel()
		if result.err != nil || result.exitCode != 0 {
			installErr = errInstall
		}
	}
	if ctx.Err() != nil {
		installErr = errInterrupted
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if installErr != nil {
		progress := currentProgress(i.findLocked(operationID))
		if ctx.Err() != nil {
			progress = 0
		}
		_ = i.updateLocked(operationID, contract.OperationFailed, progress, installErrorCode(installErr))
		return
	}
	_ = i.updateLocked(operationID, contract.OperationSucceeded, 100, "")
}

func (i *Installer) ensurePackage(ctx context.Context, operationID string) error {
	if err := i.validatePackage(ctx, i.packagePath); err == nil {
		return nil
	}
	if err := os.Remove(i.packagePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errStore
	}
	u, err := url.Parse(i.manifest.pkg.url)
	if err != nil || u.Scheme != "https" || u.Host != i.manifest.pkg.host || u.User != nil || u.Port() != "" {
		return errInvalidPackage
	}
	downloadCtx, cancel := context.WithTimeout(ctx, i.manifest.installTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, i.manifest.pkg.url, nil)
	if err != nil {
		return errDownload
	}
	response, err := i.client.Do(request)
	if err != nil {
		return errDownload
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > i.manifest.pkg.maxBytes {
		return errDownload
	}
	temp, err := os.CreateTemp(i.dir, i.manifest.downloadTempPrefix+"*")
	if err != nil {
		return errStore
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0600); err != nil {
		_ = temp.Close()
		return errStore
	}
	hasher := sha256.New()
	reader := io.TeeReader(response.Body, hasher)
	var total int64
	buf := make([]byte, 128<<10)
	for {
		if downloadCtx.Err() != nil {
			_ = temp.Close()
			return errDownload
		}
		count, readErr := reader.Read(buf)
		if count > 0 {
			total += int64(count)
			if total > i.manifest.pkg.maxBytes {
				_ = temp.Close()
				return errDownload
			}
			if _, err := temp.Write(buf[:count]); err != nil {
				_ = temp.Close()
				return errStore
			}
			basis := response.ContentLength
			if basis <= 0 {
				basis = i.manifest.pkg.maxBytes
			}
			progress := 10 + int(total*60/basis)
			if progress > 70 {
				progress = 70
			}
			if err := i.setProgress(operationID, progress); err != nil {
				_ = temp.Close()
				return errStore
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = temp.Close()
			return errDownload
		}
	}
	if total == 0 {
		_ = temp.Close()
		return errDownload
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return errStore
	}
	if err := temp.Close(); err != nil {
		return errStore
	}
	digest, err := hex.DecodeString(i.manifest.pkg.sha256)
	if err != nil || len(digest) != sha256.Size || subtle.ConstantTimeCompare(hasher.Sum(nil), digest) != 1 {
		return errDigestMismatch
	}
	if err := i.validatePackage(ctx, tempPath); err != nil {
		return err
	}
	if err := os.Rename(tempPath, i.packagePath); err != nil {
		return errStore
	}
	if err := syncDirectory(i.dir); err != nil {
		return errStore
	}
	return nil
}

func (i *Installer) validatePackage(ctx context.Context, path string) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 ||
		fileUID(info) != i.ownerUID || info.Size() <= 0 || info.Size() > i.manifest.pkg.maxBytes {
		return errInvalidPackage
	}
	hasher := sha256.New()
	input, err := os.Open(path)
	if err != nil {
		return errInvalidPackage
	}
	_, copyErr := io.Copy(hasher, input)
	closeErr := input.Close()
	if copyErr != nil || closeErr != nil {
		return errInvalidPackage
	}
	digest, err := hex.DecodeString(i.manifest.pkg.sha256)
	if err != nil || len(digest) != sha256.Size || subtle.ConstantTimeCompare(hasher.Sum(nil), digest) != 1 {
		return errDigestMismatch
	}
	commandCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result := i.run(commandCtx, applicationCommand{
		path: "/usr/bin/dpkg-deb", args: []string{"--field", path, "Package", "Version", "Architecture"},
		captureLimit: 256, captureStdout: true,
	})
	if result.err != nil || result.exitCode != 0 {
		return errInvalidPackage
	}
	fields := strings.Split(strings.TrimSuffix(string(result.stdout), "\n"), "\n")
	if len(fields) != 3 || fields[0] != i.manifest.pkg.name || fields[1] != i.manifest.pkg.version || fields[2] != i.manifest.pkg.architecture {
		return errInvalidPackage
	}
	return nil
}

func (i *Installer) installedVersion() string {
	if runtime.GOARCH != "amd64" || i.launcherTrusted == nil || !i.launcherTrusted() {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := i.run(ctx, applicationCommand{
		path: "/usr/bin/dpkg-query", args: []string{"-W", `-f=${Status}\n${Version}`, i.manifest.pkg.name},
		captureLimit: 64, captureStdout: true,
	})
	if result.err != nil || result.exitCode != 0 || string(result.stdout) != "install ok installed\n"+i.manifest.pkg.version {
		return ""
	}
	return i.manifest.pkg.version
}

func (i *Installer) setProgress(operationID string, progress int) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	current := i.findLocked(operationID)
	if current == nil || current.Operation.Status != contract.OperationRunning || progress <= current.Operation.ProgressPercent {
		return nil
	}
	return i.updateLocked(operationID, contract.OperationRunning, progress, "")
}

func (i *Installer) currentLocked() *storedInstallOperation {
	return i.findLocked(i.journal.CurrentOperation)
}

func (i *Installer) findLocked(id string) *storedInstallOperation {
	for index := range i.journal.Operations {
		if i.journal.Operations[index].Operation.ID == id {
			return &i.journal.Operations[index]
		}
	}
	return nil
}

func (i *Installer) updateLocked(id string, status contract.OperationStatus, progress int, errorCode string) error {
	if i.storageFailed {
		return errStore
	}
	entry := i.findLocked(id)
	if entry == nil {
		return errStore
	}
	previous := entry.Operation
	entry.Operation.Status = status
	entry.Operation.ProgressPercent = progress
	entry.Operation.UpdatedAt = i.now().UTC()
	entry.Operation.ErrorCode = errorCode
	if err := i.persistLocked(); err != nil {
		entry.Operation = previous
		return err
	}
	return nil
}

func (i *Installer) persistLocked() error {
	if i.storageFailed {
		return errStore
	}
	failed := func() error {
		i.storageFailed = true
		return errStore
	}
	data, err := json.Marshal(i.journal)
	if err != nil {
		return failed()
	}
	temp, err := os.CreateTemp(i.dir, ".install-state-*")
	if err != nil {
		return failed()
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0600); err != nil {
		_ = temp.Close()
		return failed()
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return failed()
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return failed()
	}
	if err := temp.Close(); err != nil {
		return failed()
	}
	if err := os.Rename(name, i.journalPath); err != nil {
		return failed()
	}
	if err := syncDirectory(i.dir); err != nil {
		return failed()
	}
	return nil
}

func (i *Installer) load() error {
	info, err := os.Lstat(i.journalPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 ||
		fileUID(info) != i.ownerUID || info.Size() > 1<<20 {
		return errors.New("application journal is unsafe")
	}
	data, err := os.ReadFile(i.journalPath)
	if err != nil {
		return errors.New("application journal is unavailable")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var journal installJournal
	if err := decoder.Decode(&journal); err != nil || journal.Version != 1 || len(journal.Operations) > i.manifest.maxHistory {
		return errors.New("application journal is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("application journal is invalid")
	}
	seenRequestIDs := make(map[string]struct{}, len(journal.Operations))
	seenOperationIDs := make(map[string]struct{}, len(journal.Operations))
	for _, entry := range journal.Operations {
		if !validLocalRequestID(entry.RequestID) || !contract.ValidApplicationInstallOperation(entry.Operation) ||
			entry.Operation.ApplicationID != i.manifest.id {
			return errors.New("application journal is invalid")
		}
		if _, exists := seenRequestIDs[entry.RequestID]; exists {
			return errors.New("application journal is invalid")
		}
		if _, exists := seenOperationIDs[entry.Operation.ID]; exists {
			return errors.New("application journal is invalid")
		}
		seenRequestIDs[entry.RequestID] = struct{}{}
		seenOperationIDs[entry.Operation.ID] = struct{}{}
	}
	if journal.CurrentOperation != "" && !hasInstallOperation(journal.Operations, journal.CurrentOperation) {
		return errors.New("application journal is invalid")
	}
	if len(journal.Operations) == 0 && journal.CurrentOperation != "" || len(journal.Operations) > 0 &&
		journal.CurrentOperation != journal.Operations[len(journal.Operations)-1].Operation.ID {
		return errors.New("application journal is invalid")
	}
	for index, entry := range journal.Operations {
		if index < len(journal.Operations)-1 && (entry.Operation.Status == contract.OperationAccepted || entry.Operation.Status == contract.OperationRunning) {
			return errors.New("application journal is invalid")
		}
	}
	i.journal = journal
	return nil
}

func validLocalRequestID(value string) bool {
	if len(value) < 1 || len(value) > 128 || !isAlphaNumeric(value[0]) {
		return false
	}
	for index := 1; index < len(value); index++ {
		character := value[index]
		if !isAlphaNumeric(character) && !strings.ContainsRune("._:-", rune(character)) {
			return false
		}
	}
	return true
}

func isAlphaNumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
}

func hasInstallOperation(operations []storedInstallOperation, id string) bool {
	for _, operation := range operations {
		if operation.Operation.ID == id {
			return true
		}
	}
	return false
}

func newInstallID() (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "app_" + hex.EncodeToString(value[:]), nil
}

func fileUID(info os.FileInfo) uint32 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ^uint32(0)
	}
	return stat.Uid
}

func removeInterruptedApplicationDownloads(dir string, ownerUID uint32, prefix string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !isApplicationDownloadTempName(entry.Name(), prefix) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || fileUID(info) != ownerUID {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func isApplicationDownloadTempName(name, prefix string) bool {
	suffix := strings.TrimPrefix(name, prefix)
	if suffix == name || len(suffix) != 12 {
		return false
	}
	for _, character := range suffix {
		if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9') {
			return false
		}
	}
	return true
}

func currentProgress(entry *storedInstallOperation) int {
	if entry == nil {
		return 0
	}
	return entry.Operation.ProgressPercent
}

func installErrorCode(err error) string {
	switch {
	case errors.Is(err, errDigestMismatch):
		return "digest_mismatch"
	case errors.Is(err, errInvalidPackage):
		return "invalid_package"
	case errors.Is(err, errStore):
		return "storage_error"
	case errors.Is(err, errInstall):
		return "install_failed"
	case errors.Is(err, errInterrupted):
		return "operation_interrupted"
	case errors.Is(err, ErrArchitectureUnsupported):
		return "architecture_unsupported"
	default:
		return "download_failed"
	}
}

func applicationHTTPClient(host string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 2 || request.URL.Scheme != "https" || request.URL.Host != host ||
				request.URL.User != nil || request.URL.Port() != "" {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

var (
	ErrInvalidInstallRequest       = errors.New("invalid application install request")
	ErrUnsupportedApplication      = errors.New("application is not supported by this installer")
	ErrInstallInProgress           = errors.New("application install is already in progress")
	ErrArchitectureUnsupported     = errors.New("application architecture is unsupported")
	errApplicationStoreUnavailable = errors.New("application store unavailable")
	errDownload                    = errors.New("application download failed")
	errDigestMismatch              = errors.New("application package digest mismatch")
	errInvalidPackage              = errors.New("application package metadata is invalid")
	errStore                       = errors.New("application store write failed")
	errInstall                     = errors.New("application installer failed")
	errInterrupted                 = errors.New("application install interrupted")
)
