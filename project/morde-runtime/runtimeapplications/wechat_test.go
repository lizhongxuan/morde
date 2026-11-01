package runtimeapplications

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"morde-runtime/contract"
)

func TestInstallIsIdempotentAndUsesOnlyFixedCommands(t *testing.T) {
	installer, requests, commands := newTestInstaller(t, []byte("fake package"))
	first, err := installer.installRequest("req-123")
	if err != nil {
		t.Fatal(err)
	}
	completed := waitInstall(t, installer, first.ID)
	if completed.Status != contract.OperationSucceeded || completed.ErrorCode != "" || completed.ProgressPercent != 100 {
		t.Fatalf("install operation = %#v", completed)
	}
	duplicate, err := installer.installRequest("req-123")
	if err != nil || duplicate.ID != first.ID {
		t.Fatalf("same request returned %#v, err=%v; original id=%q", duplicate, err, first.ID)
	}
	secondRequest, err := installer.installRequest("req-456")
	if err != nil || secondRequest.ID != first.ID {
		t.Fatalf("already installed request returned %#v, err=%v; original id=%q", secondRequest, err, first.ID)
	}
	if got := requests.count(); got != 1 {
		t.Fatalf("package downloads = %d, want 1", got)
	}
	if got := commands.count("/usr/bin/dpkg", "-i"); got != 1 {
		t.Fatalf("fixed dpkg install calls = %d, want 1", got)
	}
	for _, command := range commands.list() {
		if command.path != "/usr/bin/dpkg-deb" && command.path != "/usr/bin/dpkg" && command.path != "/usr/bin/dpkg-query" {
			t.Fatalf("unexpected command path: %#v", command)
		}
		if command.path == "/usr/bin/dpkg" && (len(command.args) != 2 || command.args[0] != "-i") {
			t.Fatalf("unexpected dpkg args: %#v", command.args)
		}
	}
}

func TestInstallRejectsInvalidRequestAndSanitizesFailures(t *testing.T) {
	if _, err := (&Installer{}).Install(wechatID, "bad request id"); !errors.Is(err, ErrInvalidInstallRequest) {
		t.Fatalf("invalid request error = %v", err)
	}

	installer, requests, commands := newTestInstaller(t, []byte("fake package"))
	installer.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.increment()
		return nil, errors.New("secret-token-and-hostname")
	})}
	operation, err := installer.installRequest("req-safe-1")
	if err != nil {
		t.Fatal(err)
	}
	completed := waitInstall(t, installer, operation.ID)
	if completed.Status != contract.OperationFailed || completed.ErrorCode != "download_failed" {
		t.Fatalf("sanitized install failure = %#v", completed)
	}
	encoded := string(mustJSON(t, completed))
	if strings.Contains(encoded, "secret-token") || strings.Contains(encoded, "hostname") {
		t.Fatalf("operation leaked transport error: %s", encoded)
	}
	if requests.count() != 1 || commands.count("/usr/bin/dpkg", "-i") != 0 {
		t.Fatalf("failed download side effects: requests=%d dpkg=%d", requests.count(), commands.count("/usr/bin/dpkg", "-i"))
	}
}

func TestInstallerRejectsApplicationOutsideItsManifest(t *testing.T) {
	installer, requests, commands := newTestInstaller(t, []byte("fake package"))
	if _, err := installer.Install("qq", "req-qq-01"); !errors.Is(err, ErrUnsupportedApplication) {
		t.Fatalf("unverified app install error = %v", err)
	}
	if requests.count() != 0 || commands.count("/usr/bin/dpkg", "-i") != 0 {
		t.Fatalf("unverified app caused side effects: downloads=%d installs=%d", requests.count(), commands.count("/usr/bin/dpkg", "-i"))
	}
}

func TestOfficialPackagePinAndProcessOutputSanitization(t *testing.T) {
	if wechatManifest.pkg.name != "wechat" || wechatManifest.pkg.version != "4.1.13.26" || wechatManifest.pkg.architecture != "amd64" ||
		wechatManifest.pkg.url != "https://dldir1v6.qq.com/weixin/Universal/Linux/WeChatLinux_x86_64.deb" ||
		wechatManifest.pkg.host != "dldir1v6.qq.com" || wechatManifest.pkg.sha256 != "a0db8caeee8d1e6cfe0aa5ce58710270f8d74a90ff4920f53a3fc22aa0956fe9" {
		t.Fatalf("unexpected WeChat package pin: %#v", wechatManifest.pkg)
	}
	installer, _, commands := newTestInstaller(t, []byte("fake package"))
	installer.run = func(_ context.Context, command applicationCommand) applicationCommandOutcome {
		commands.record(command)
		switch command.path {
		case "/usr/bin/dpkg-deb":
			return applicationCommandOutcome{stdout: []byte("wechat\n4.1.13.26\namd64\n")}
		case "/usr/bin/dpkg":
			return applicationCommandOutcome{stdout: []byte("process-secret"), err: errors.New("process-secret")}
		case "/usr/bin/dpkg-query":
			return applicationCommandOutcome{stdout: []byte("install ok installed\n" + wechatVersion)}
		default:
			return applicationCommandOutcome{err: errors.New("unexpected command")}
		}
	}
	operation, err := installer.installRequest("req-process-error")
	if err != nil {
		t.Fatal(err)
	}
	completed := waitInstall(t, installer, operation.ID)
	if completed.Status != contract.OperationFailed || completed.ErrorCode != "install_failed" {
		t.Fatalf("process error operation = %#v", completed)
	}
	if encoded := string(mustJSON(t, completed)); strings.Contains(encoded, "process-secret") {
		t.Fatalf("operation leaked package manager output: %s", encoded)
	}
}

func TestStatusDoesNotProbePackageManagerOnUnsupportedArchitecture(t *testing.T) {
	installer, _, commands := newTestInstaller(t, []byte("unused"))
	status := installer.Status()
	if status.Installable != (runtime.GOARCH == "amd64") {
		t.Fatalf("installable = %v on %s", status.Installable, runtime.GOARCH)
	}
	if runtime.GOARCH != "amd64" && commands.count("/usr/bin/dpkg-query", "-W") != 0 {
		t.Fatalf("unsupported architecture called dpkg-query: %#v", commands.list())
	}
	if runtime.GOARCH != "amd64" {
		if _, err := installer.Install(wechatID, "req-arch"); !errors.Is(err, ErrArchitectureUnsupported) {
			t.Fatalf("unsupported architecture install error = %v", err)
		}
	}
}

func TestStatusReportsInstalledWechatWithUnknownLoginState(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("the pinned WeChat package is amd64-only")
	}
	installer, _, commands := newTestInstaller(t, []byte("unused"))
	status := installer.Status()
	if !status.Installed || status.Version != wechatVersion || status.LoginStatus != contract.ApplicationLoginUnknown {
		t.Fatalf("installed WeChat status = %#v", status)
	}
	if !contract.ValidApplicationStatusItem(status) {
		t.Fatalf("installed WeChat status is outside the wire contract: %#v", status)
	}
	got := commands.list()
	if len(got) != 1 || got[0].path != "/usr/bin/dpkg-query" || len(got[0].args) != 3 ||
		got[0].args[1] != `-f=${Status}\n${Version}` {
		t.Fatalf("dpkg-query did not request package status and version: %#v", got)
	}
}

func TestStatusRequiresDpkgInstalledStateAndTrustedLauncher(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("the pinned WeChat package is amd64-only")
	}
	for _, test := range []struct {
		name            string
		query           string
		trustedLauncher bool
	}{
		{name: "unpacked package", query: "install ok unpacked\n" + wechatVersion, trustedLauncher: true},
		{name: "not installed package", query: "deinstall ok config-files\n" + wechatVersion, trustedLauncher: true},
		{name: "untrusted launcher", query: "install ok installed\n" + wechatVersion},
	} {
		t.Run(test.name, func(t *testing.T) {
			installer, _, _ := newTestInstaller(t, []byte("unused"))
			installer.launcherTrusted = func() bool { return test.trustedLauncher }
			installer.run = func(_ context.Context, command applicationCommand) applicationCommandOutcome {
				if command.path == "/usr/bin/dpkg-query" {
					return applicationCommandOutcome{stdout: []byte(test.query)}
				}
				return applicationCommandOutcome{}
			}
			status := installer.Status()
			if status.Installed || status.Version != "" || status.LoginStatus != contract.ApplicationLoginNotApplicable {
				t.Fatalf("untrusted/uninstalled WeChat status = %#v", status)
			}
		})
	}
}

func TestStartClosesPendingInstallOnUnsupportedArchitecture(t *testing.T) {
	if runtime.GOARCH == "amd64" {
		t.Skip("the test host supports the pinned package")
	}
	installer, requests, commands := newTestInstaller(t, []byte("unused"))
	operation := contract.ApplicationInstallOperation{
		ID: "app_0123456789abcdef01234567", ApplicationID: wechatID, Status: contract.OperationRunning,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	installer.journal.Operations = []storedInstallOperation{{RequestID: "req-unsupported", Operation: operation}}
	installer.journal.CurrentOperation = operation.ID
	if err := installer.persistLocked(); err != nil {
		t.Fatal(err)
	}
	if err := installer.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := installer.Status().InstallOperation
	if got == nil || got.Status != contract.OperationFailed || got.ErrorCode != "architecture_unsupported" {
		t.Fatalf("unsupported pending operation = %#v", got)
	}
	if requestID := installer.Status().InstallRequestID; requestID != "req-unsupported" {
		t.Fatalf("status install request ID = %q", requestID)
	}
	if requests.count() != 0 || len(commands.list()) != 0 {
		t.Fatalf("unsupported host ran package tools: requests=%d commands=%#v", requests.count(), commands.list())
	}
}

func TestInstallRejectsBadPackageDigestAndMetadata(t *testing.T) {
	t.Run("digest", func(t *testing.T) {
		installer, _, commands := newTestInstaller(t, []byte("not the official package"))
		installer.manifest.pkg.sha256 = strings.Repeat("0", sha256.Size*2)
		operation, err := installer.installRequest("req-digest")
		if err != nil {
			t.Fatal(err)
		}
		completed := waitInstall(t, installer, operation.ID)
		if completed.Status != contract.OperationFailed || completed.ErrorCode != "digest_mismatch" || commands.count("/usr/bin/dpkg", "-i") != 0 {
			t.Fatalf("digest rejection = %#v, dpkg calls=%d", completed, commands.count("/usr/bin/dpkg", "-i"))
		}
	})

	t.Run("metadata", func(t *testing.T) {
		installer, _, commands := newTestInstaller(t, []byte("fake package"))
		installer.run = func(_ context.Context, command applicationCommand) applicationCommandOutcome {
			if command.path == "/usr/bin/dpkg-deb" {
				return applicationCommandOutcome{stdout: []byte("wechat\n4.1.13.26\narm64\n")}
			}
			commands.record(command)
			return applicationCommandOutcome{}
		}
		operation, err := installer.installRequest("req-metadata")
		if err != nil {
			t.Fatal(err)
		}
		completed := waitInstall(t, installer, operation.ID)
		if completed.Status != contract.OperationFailed || completed.ErrorCode != "invalid_package" || commands.count("/usr/bin/dpkg", "-i") != 0 {
			t.Fatalf("metadata rejection = %#v, dpkg calls=%d", completed, commands.count("/usr/bin/dpkg", "-i"))
		}
	})
}

func TestRestoreRevalidatesAndUsesPersistentVerifiedCache(t *testing.T) {
	packageBytes := []byte("fake package for restart")
	installer, requests, commands := newTestInstaller(t, packageBytes)
	operation, err := installer.installRequest("req-restore")
	if err != nil {
		t.Fatal(err)
	}
	if got := waitInstall(t, installer, operation.ID); got.Status != contract.OperationSucceeded {
		t.Fatalf("initial install = %#v", got)
	}
	packagePath := installer.packagePath
	storeDir := filepath.Dir(filepath.Dir(packagePath))
	if err := installer.Close(); err != nil {
		t.Fatal(err)
	}

	restored, err := openInstaller(installerOptions{applicationID: wechatID, storeDir: storeDir, expectedOwnerUID: uint32(os.Geteuid())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	restored.manifest.pkg = testPackage(packageBytes)
	restored.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.increment()
		return nil, errors.New("cache miss should not download")
	})}
	restored.run = func(_ context.Context, command applicationCommand) applicationCommandOutcome {
		commands.record(command)
		if command.path == "/usr/bin/dpkg-deb" {
			return applicationCommandOutcome{stdout: []byte("wechat\n4.1.13.26\namd64\n")}
		}
		return applicationCommandOutcome{}
	}
	if err := restored.restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := restored.Status()
	if status.InstallOperation == nil || status.InstallOperation.ID != operation.ID || status.InstallOperation.Status != contract.OperationSucceeded {
		t.Fatalf("restored operation = %#v", status.InstallOperation)
	}
	if status.InstallRequestID != "req-restore" || !contract.ValidApplicationStatusItem(status) {
		t.Fatalf("restored operation lost request identity: %#v", status)
	}
	if requests.count() != 1 {
		t.Fatalf("restore downloaded package again: total requests=%d, want only initial download", requests.count())
	}
	if got := commands.count("/usr/bin/dpkg", "-i"); got != 2 {
		t.Fatalf("dpkg calls after restore = %d, want initial install + cached restore", got)
	}
	for _, path := range []string{filepath.Dir(packagePath), packagePath, restored.journalPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0700)
		if path != filepath.Dir(packagePath) {
			want = 0600
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s mode = %04o, want %04o", path, info.Mode().Perm(), want)
		}
	}
}

func TestDownloadRejectsRedirectAndOversizedBody(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		installer, requests, commands := newTestInstaller(t, nil)
		installer.client = applicationHTTPClient(wechatManifest.pkg.host, wechatManifest.installTimeout)
		installer.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests.increment()
			return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://attacker.example/package.deb"}}, Body: io.NopCloser(strings.NewReader("redirect")), Request: request}, nil
		})
		operation, err := installer.installRequest("req-redirect")
		if err != nil {
			t.Fatal(err)
		}
		completed := waitInstall(t, installer, operation.ID)
		if completed.ErrorCode != "download_failed" || requests.count() != 1 || commands.count("/usr/bin/dpkg", "-i") != 0 {
			t.Fatalf("redirect result=%#v requests=%d dpkg=%d", completed, requests.count(), commands.count("/usr/bin/dpkg", "-i"))
		}
	})

	t.Run("oversized", func(t *testing.T) {
		installer, _, commands := newTestInstaller(t, []byte("too large"))
		installer.manifest.pkg.maxBytes = 4
		operation, err := installer.installRequest("req-large")
		if err != nil {
			t.Fatal(err)
		}
		completed := waitInstall(t, installer, operation.ID)
		if completed.ErrorCode != "download_failed" || commands.count("/usr/bin/dpkg", "-i") != 0 {
			t.Fatalf("oversized result=%#v dpkg=%d", completed, commands.count("/usr/bin/dpkg", "-i"))
		}
	})
}

func TestRestoreResumesInterruptedPendingOperation(t *testing.T) {
	packageBytes := []byte("fake package interrupted")
	first, _, _ := newTestInstaller(t, packageBytes)
	first.journal.Operations = []storedInstallOperation{{RequestID: "req-pending", Operation: contract.ApplicationInstallOperation{
		ID: "app_0123456789abcdef01234567", ApplicationID: wechatID, Status: contract.OperationRunning,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}}}
	first.journal.CurrentOperation = first.journal.Operations[0].Operation.ID
	if err := first.persistLocked(); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Dir(first.dir)
	packagePath := first.packagePath
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	resumed, err := openInstaller(installerOptions{applicationID: wechatID, storeDir: storeDir, expectedOwnerUID: uint32(os.Geteuid())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Close() })
	resumed.manifest.pkg = testPackage(packageBytes)
	if err := os.WriteFile(packagePath, packageBytes, 0600); err != nil {
		t.Fatal(err)
	}
	resumed.run = func(_ context.Context, command applicationCommand) applicationCommandOutcome {
		if command.path == "/usr/bin/dpkg-deb" {
			return applicationCommandOutcome{stdout: []byte("wechat\n4.1.13.26\namd64\n")}
		}
		return applicationCommandOutcome{}
	}
	if err := resumed.restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := resumed.Status().InstallOperation; got == nil || got.Status != contract.OperationSucceeded {
		t.Fatalf("resumed operation = %#v", got)
	}
}

func TestRestoreHonorsEarlierHostStartupDeadline(t *testing.T) {
	installer, _, _ := newTestInstaller(t, []byte("cached package"))
	operation := contract.ApplicationInstallOperation{
		ID: "app_0123456789abcdef01234567", ApplicationID: wechatID, Status: contract.OperationSucceeded,
		ProgressPercent: 100, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	installer.journal.Operations = []storedInstallOperation{{RequestID: "req-startup-timeout", Operation: operation}}
	installer.journal.CurrentOperation = operation.ID
	if err := installer.persistLocked(); err != nil {
		t.Fatal(err)
	}
	installer.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := installer.restore(ctx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("restore exceeded caller's Host startup deadline: %s", elapsed)
	}
	got := installer.Status().InstallOperation
	if got == nil || got.Status != contract.OperationFailed || got.ErrorCode != "operation_interrupted" {
		t.Fatalf("timed-out restore = %#v; want resumable operation_interrupted", got)
	}
}

func TestRestoreSerializesWithStartAndInstall(t *testing.T) {
	packageBytes := []byte("cached package")
	installer, _, _ := newTestInstaller(t, packageBytes)
	if err := os.WriteFile(installer.packagePath, packageBytes, 0600); err != nil {
		t.Fatal(err)
	}
	operation := contract.ApplicationInstallOperation{
		ID: "app_0123456789abcdef01234567", ApplicationID: wechatID, Status: contract.OperationSucceeded,
		ProgressPercent: 100, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	installer.journal.Operations = []storedInstallOperation{{RequestID: "req-concurrent", Operation: operation}}
	installer.journal.CurrentOperation = operation.ID
	if err := installer.persistLocked(); err != nil {
		t.Fatal(err)
	}

	started, release := make(chan struct{}), make(chan struct{})
	var active, maxActive int
	var activityMu sync.Mutex
	installer.run = func(_ context.Context, command applicationCommand) applicationCommandOutcome {
		if command.path == "/usr/bin/dpkg-deb" {
			return applicationCommandOutcome{stdout: []byte("wechat\n4.1.13.26\namd64\n")}
		}
		if command.path == "/usr/bin/dpkg" {
			activityMu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			activityMu.Unlock()
			close(started)
			<-release
			activityMu.Lock()
			active--
			activityMu.Unlock()
		}
		return applicationCommandOutcome{}
	}

	restored := make(chan error, 1)
	go func() { restored <- installer.restore(context.Background()) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("restore did not reach fixed installer")
	}
	if err := installer.Start(context.Background()); !errors.Is(err, ErrInstallInProgress) {
		t.Fatalf("Start during Restore error = %v", err)
	}
	if _, err := installer.installRequest("req-overlap"); !errors.Is(err, ErrInstallInProgress) {
		t.Fatalf("Install during Restore error = %v", err)
	}
	if err := installer.restore(context.Background()); !errors.Is(err, ErrInstallInProgress) {
		t.Fatalf("second Restore error = %v", err)
	}
	close(release)
	if err := <-restored; err != nil {
		t.Fatal(err)
	}
	activityMu.Lock()
	defer activityMu.Unlock()
	if maxActive != 1 {
		t.Fatalf("simultaneous dpkg processes = %d, want 1", maxActive)
	}
}

func TestJournalRequiresCurrentOperationToBeLast(t *testing.T) {
	installer, _, _ := newTestInstaller(t, []byte("package"))
	first := contract.ApplicationInstallOperation{ID: "app_aaaaaaaaaaaaaaaaaaaaaaaa", ApplicationID: wechatID, Status: contract.OperationFailed,
		ErrorCode: "download_failed", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	second := contract.ApplicationInstallOperation{ID: "app_bbbbbbbbbbbbbbbbbbbbbbbb", ApplicationID: wechatID, Status: contract.OperationSucceeded,
		ProgressPercent: 100, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	installer.journal.Operations = []storedInstallOperation{{RequestID: "req-first", Operation: first}, {RequestID: "req-second", Operation: second}}
	installer.journal.CurrentOperation = first.ID
	if err := installer.persistLocked(); err != nil {
		t.Fatal(err)
	}
	if _, err := openInstaller(installerOptions{applicationID: wechatID, storeDir: filepath.Dir(installer.dir), expectedOwnerUID: uint32(os.Geteuid())}); err == nil {
		t.Fatal("journal with stale current operation was accepted")
	}
}

func newTestInstaller(t *testing.T, packageBytes []byte) (*Installer, *requestCounter, *commandCounter) {
	t.Helper()
	storeDir := filepath.Join(t.TempDir(), "apps")
	if err := os.Mkdir(storeDir, 0755); err != nil {
		t.Fatal(err)
	}
	installer, err := openInstaller(installerOptions{applicationID: wechatID, storeDir: storeDir, expectedOwnerUID: uint32(os.Geteuid())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = installer.Close() })
	installer.manifest.pkg = testPackage(packageBytes)
	installer.launcherTrusted = func() bool { return true }
	requests, commands := &requestCounter{}, &commandCounter{}
	installer.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.increment()
		if request.Method != http.MethodGet || request.URL.String() != wechatURL {
			return nil, errors.New("unexpected package request")
		}
		body := io.NopCloser(bytes.NewReader(packageBytes))
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, ContentLength: int64(len(packageBytes)), Request: request}, nil
	})}
	installer.run = func(_ context.Context, command applicationCommand) applicationCommandOutcome {
		commands.record(command)
		switch command.path {
		case "/usr/bin/dpkg-deb":
			return applicationCommandOutcome{stdout: []byte("wechat\n4.1.13.26\namd64\n")}
		case "/usr/bin/dpkg-query":
			return applicationCommandOutcome{stdout: []byte("install ok installed\n" + wechatVersion)}
		default:
			return applicationCommandOutcome{}
		}
	}
	return installer, requests, commands
}

func TestOpenInstallerRemovesOnlyInterruptedDownloadTempFiles(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "apps")
	appDir := filepath.Join(storeDir, wechatID)
	if err := os.Mkdir(storeDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(appDir, 0700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(appDir, wechatDownloadTempPrefix+"abcdefghijkl")
	if err := os.WriteFile(stale, []byte("partial package"), 0600); err != nil {
		t.Fatal(err)
	}
	malformed := filepath.Join(appDir, wechatDownloadTempPrefix+"short")
	if err := os.WriteFile(malformed, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	otherData := filepath.Join(appDir, "other-data")
	if err := os.WriteFile(otherData, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	unsafeLink := filepath.Join(appDir, wechatDownloadTempPrefix+"123456789abc")
	if err := os.Symlink(otherData, unsafeLink); err != nil {
		t.Fatal(err)
	}
	unsafeDirectory := filepath.Join(appDir, wechatDownloadTempPrefix+"mnopqrstuvwx")
	if err := os.Mkdir(unsafeDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	wrongOwner := filepath.Join(appDir, wechatDownloadTempPrefix+"123456789xyz")
	if err := os.WriteFile(wrongOwner, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(wrongOwner, 1, -1); err != nil {
			t.Fatal(err)
		}
	}

	installer, err := openInstaller(installerOptions{applicationID: wechatID, storeDir: storeDir, expectedOwnerUID: uint32(os.Geteuid())})
	if err != nil {
		t.Fatal(err)
	}
	defer installer.Close()
	if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted download still exists: %v", err)
	}
	for _, path := range []string{malformed, otherData, unsafeLink, unsafeDirectory} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("unrecognized or non-regular application data %q was removed: %v", path, err)
		}
	}
	if os.Geteuid() == 0 {
		if _, err := os.Lstat(wrongOwner); err != nil {
			t.Fatalf("download-shaped file with another owner was removed: %v", err)
		}
	}
}

func testPackage(value []byte) debPackage {
	digest := sha256.Sum256(value)
	return debPackage{name: wechatPackageName, version: wechatVersion, architecture: "amd64", url: wechatURL, host: wechatManifest.pkg.host, sha256: hex.EncodeToString(digest[:]), maxBytes: 1 << 20}
}

func waitInstall(t *testing.T, installer *Installer, id string) contract.ApplicationInstallOperation {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		installer.mu.Lock()
		entry := installer.findLocked(id)
		if entry != nil {
			operation := entry.Operation
			installer.mu.Unlock()
			if operation.Status == contract.OperationSucceeded || operation.Status == contract.OperationFailed {
				return operation
			}
		} else {
			installer.mu.Unlock()
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("install operation %q did not finish", id)
	return contract.ApplicationInstallOperation{}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type requestCounter struct {
	mu    sync.Mutex
	value int
}

func (c *requestCounter) increment() {
	c.mu.Lock()
	c.value++
	c.mu.Unlock()
}

func (c *requestCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

type commandCounter struct {
	mu       sync.Mutex
	commands []applicationCommand
}

func (c *commandCounter) record(command applicationCommand) {
	c.mu.Lock()
	command.args = append([]string(nil), command.args...)
	c.commands = append(c.commands, command)
	c.mu.Unlock()
}

func (c *commandCounter) list() []applicationCommand {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]applicationCommand(nil), c.commands...)
}

func (c *commandCounter) count(path, firstArg string) int {
	count := 0
	for _, command := range c.list() {
		if command.path == path && len(command.args) > 0 && command.args[0] == firstArg {
			count++
		}
	}
	return count
}
