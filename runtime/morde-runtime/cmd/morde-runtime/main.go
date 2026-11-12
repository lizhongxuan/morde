package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"morde-runtime/contract"
	"morde-runtime/internal/operationstore"
	"morde-runtime/runtime_desktop"
	"morde-runtime/runtimeactions"
	"morde-runtime/runtimeapplications"
	"morde-runtime/runtimebrowser"
	"morde-runtime/runtimecore"
	"morde-runtime/runtimefiles"
	"morde-runtime/runtimehttp"
	"morde-runtime/runtimeidentity"
	"morde-runtime/workspacereplica"
)

const (
	listenAddress         = ":8090"
	credentialPath        = "/run/morde-runtime/credential"
	replicaCredentialPath = "/run/morde-runtime/replica-credential"
	storePath             = "/var/lib/morde-runtime"
	workspacePath         = "/home/gem/workspace"
	applicationStorePath  = "/run/morde-runtime/apps"
)

func main() {
	if len(os.Args) > 1 && (len(os.Args) != 2 || os.Args[1] != "healthcheck" && os.Args[1] != "restore-apps") {
		_, _ = fmt.Fprintln(os.Stderr, "morde-runtime: unsupported command")
		os.Exit(2)
	}
	if len(os.Args) == 2 {
		var err error
		switch os.Args[1] {
		case "healthcheck":
			err = healthcheck()
		case "restore-apps":
			err = restoreApps()
		}
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "morde-runtime: %s\n", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		// run and healthcheck only return fixed, sanitized stage messages. Printing
		// those lets operators diagnose startup without exposing credentials or
		// environment values.
		_, _ = fmt.Fprintf(os.Stderr, "morde-runtime: %s\n", err)
		os.Exit(1)
	}
}

func restoreApps() error {
	applications, err := runtimeapplications.OpenCatalog(runtimeapplications.CatalogOptions{
		StoreDir: applicationStorePath, ExpectedOwnerUID: 0,
	})
	if err != nil {
		return errors.New("application store unavailable")
	}
	defer applications.Close()
	if err := applications.Restore(context.Background()); err != nil {
		return errors.New("application restore state unavailable")
	}
	return nil
}

func healthcheck() error {
	hostID := os.Getenv("MORDE_HOST_ID")
	if !validHostID(hostID) {
		return errors.New("invalid Host ID")
	}
	credential, err := runtimeidentity.ReadCredentialFile(credentialPath)
	if err != nil {
		return errors.New("runtime credential unavailable")
	}
	tlsConfig, err := runtimeidentity.ClientTLSConfig(credential, hostID)
	if err != nil {
		return errors.New("runtime TLS identity unavailable")
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	response, err := client.Get("https://127.0.0.1:8090/readyz")
	if err != nil {
		return errors.New("runtime readiness endpoint unavailable")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16))
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "ok\n" {
		return errors.New("runtime operation store is not ready")
	}
	return nil
}

func run() error {
	hostID := os.Getenv("MORDE_HOST_ID")
	if !validHostID(hostID) {
		return errors.New("invalid Host ID")
	}
	generation, err := strconv.ParseInt(os.Getenv("MORDE_RUNTIME_GENERATION"), 10, 64)
	if err != nil || generation < 1 {
		return errors.New("invalid runtime generation")
	}
	credential, err := runtimeidentity.ReadCredentialFile(credentialPath)
	if err != nil {
		return errors.New("runtime credential unavailable")
	}
	portalURL := os.Getenv("MORDE_PORTAL_URL")
	if portalURL == "" {
		return errors.New("Host Replica Portal origin unavailable")
	}
	replicaCredential, err := runtimeidentity.ReadReplicaCredentialFile(replicaCredentialPath)
	if err != nil {
		return errors.New("Host Replica credential unavailable")
	}
	coreLedger, err := operationstore.OpenRemote(operationstore.RemoteOptions{
		HostID: hostID, Generation: generation, Namespace: operationstore.RemoteNamespaceCore,
		PortalURL: portalURL, Credential: replicaCredential,
	})
	if err != nil {
		return errors.New("runtime Portal operation ledger unavailable")
	}
	defer coreLedger.Close()
	applicationDomain, domainErr := runtimeactions.NewApplicationProcessDomain(runtimeactions.ApplicationProcessDomainOptions{})
	if domainErr != nil {
		_, _ = fmt.Fprintln(os.Stderr, "morde-runtime: native application launch unavailable")
	}
	applications, err := runtimeapplications.OpenCatalog(runtimeapplications.CatalogOptions{
		StoreDir: applicationStorePath, ExpectedOwnerUID: 0, LaunchDomain: applicationDomain,
	})
	if err != nil {
		return errors.New("runtime application store unavailable")
	}
	defer applications.Close()
	var handlers []runtimecore.ActionHandler
	viewUID, viewGID := uint32(os.Geteuid()), uint32(os.Getegid())
	if shellUID, shellGID, ownerErr := runtimeactions.GemCredentialIDs(); ownerErr == nil {
		viewUID, viewGID = shellUID, shellGID
	}
	workspace, err := runtimefiles.OpenTaskWorkspace(workspacePath, filepath.Join(workspacePath, ".morde-task-views"), viewUID, viewGID)
	if err != nil {
		workspace = nil
	}
	if err == nil {
		handlers = append(handlers, workspace.Handlers()...)
	} else {
		_, _ = fmt.Fprintln(os.Stderr, "morde-runtime: file capabilities unavailable")
	}
	if workspace != nil {
		if shell, shellErr := runtimeactions.NewShellHandler(runtimeactions.ShellOptions{DefaultWorkDir: workspacePath}); shellErr == nil {
			handlers = append(handlers, shell)
		} else {
			_, _ = fmt.Fprintln(os.Stderr, "morde-runtime: shell capability unavailable")
		}
	} else {
		_, _ = fmt.Fprintln(os.Stderr, "morde-runtime: shell capability unavailable")
	}
	desktop, desktopErr := runtime_desktop.NewHandlers(runtime_desktop.Options{})
	if desktopErr == nil {
		handlers = append(handlers, desktop.Handlers()...)
	} else {
		// Native desktop actions are advertised only after the current X display
		// and the fixed image/input tools have passed their runtime probes.
		_, _ = fmt.Fprintln(os.Stderr, "morde-runtime: desktop capabilities unavailable")
	}
	var browser *runtimebrowser.Controller
	var browserLedger *operationstore.Remote
	if os.Getenv("DISABLE_BROWSER") != "true" {
		browserLedger, err = operationstore.OpenRemote(operationstore.RemoteOptions{
			HostID: hostID, Generation: generation, Namespace: operationstore.RemoteNamespaceBrowser,
			PortalURL: portalURL, Credential: replicaCredential,
		})
		if err != nil {
			return errors.New("runtime browser operation ledger unavailable")
		}
		defer func() {
			if browserLedger != nil {
				_ = browserLedger.Close()
			}
		}()
	}
	if browserLedger != nil {
		var browserErr error
		browser, browserErr = runtimebrowser.NewHandlers(runtimebrowser.Options{
			StoreDir: storePath, ExpectedOwnerUID: 0, Store: browserLedger, Endpoint: os.Getenv("MORDE_BROWSER_CDP_ENDPOINT"),
		})
		if browserErr != nil {
			if errors.Is(browserErr, runtimebrowser.ErrOperationLedgerUnavailable) ||
				errors.Is(browserErr, operationstore.ErrStoreUnhealthy) || errors.Is(browserErr, operationstore.ErrStoreClosed) {
				return errors.New("runtime browser operation ledger reconciliation failed")
			}
			_ = browserLedger.Close()
			browserLedger = nil
			// Browser tools are advertised only after an isolated-context CDP probe
			// proves they cannot attach to or close a user's default-profile tabs.
			_, _ = fmt.Fprintln(os.Stderr, "morde-runtime: browser capabilities unavailable")
		} else {
			handlers = append(handlers, browser.Handlers()...)
		}
	}
	var manager *runtimecore.Manager
	managerClosed := false
	shutdownAttempted := false
	defer func() {
		if manager != nil && !managerClosed && !shutdownAttempted {
			managerClosed = manager.Close() == nil
		}
		// Keep the rooted file handle alive if the manager could not prove that
		// all actions had stopped. Process exit will release it after fail-closed
		// shutdown rather than racing a still-running file action.
		if workspace != nil && (manager == nil || managerClosed) {
			_ = workspace.Close()
		}
		if browser != nil && (manager == nil || managerClosed) {
			_ = browser.Close()
		}
		if desktop != nil && (manager == nil || managerClosed) {
			_ = desktop.Close()
		}
	}()
	manager, err = runtimecore.Open(runtimecore.Options{
		StoreDir: storePath, ExpectedOwnerUID: 0, Store: coreLedger, HostID: hostID, Generation: generation,
		ProtocolVersion: contract.ProtocolVersion, Handlers: handlers, TaskWorkspaceViews: workspace,
	})
	if err != nil {
		return errors.New("runtime operation store unavailable")
	}
	currentCapabilities, err := manager.Capabilities(contract.CapabilitiesRequest{
		ProtocolVersion: contract.ProtocolVersion, HostID: hostID, RuntimeGeneration: generation,
	})
	if err != nil || applications.ReconcileLaunches(contract.HostFence{
		HostID: currentCapabilities.HostID, RuntimeGeneration: currentCapabilities.RuntimeGeneration,
		RuntimeInstanceID: currentCapabilities.RuntimeInstanceID, ControlVersion: currentCapabilities.ControlVersion,
	}) != nil {
		return errors.New("runtime application state unavailable")
	}
	serverTLS, err := runtimeidentity.ServerTLSConfig(credential, hostID)
	if err != nil {
		return errors.New("runtime TLS identity unavailable")
	}
	handler, err := runtimehttp.NewHandlerWithApplications(manager, credential, applications)
	if err != nil {
		return errors.New("runtime HTTP handler unavailable")
	}
	shutdownContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := applications.Start(shutdownContext); err != nil {
		return errors.New("runtime application state unavailable")
	}
	server := &http.Server{
		Addr:              listenAddress,
		Handler:           handler,
		TLSConfig:         serverTLS,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       65 * time.Second,
		WriteTimeout:      65 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	if portalURL != "" {
		replica, err := workspacereplica.New(workspacereplica.Options{
			HostID: hostID, Generation: generation, PortalURL: portalURL,
			BootstrapCredential: replicaCredential, WorkspaceRoot: workspacePath,
			WorkspaceUID: 1000, WorkspaceGID: 1000, Manager: manager,
			OnError: func(error) { _, _ = fmt.Fprintln(os.Stderr, "morde-runtime: workspace synchronization paused") },
		})
		if err != nil {
			return errors.New("Host workspace replica unavailable")
		}
		go func() { _ = replica.Run(shutdownContext) }()
	}
	serveError := make(chan error, 1)
	go func() {
		serveError <- server.ListenAndServeTLS("", "")
	}()
	select {
	case err := <-serveError:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.New("runtime HTTPS listener failed")
	case <-shutdownContext.Done():
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			return errors.New("runtime HTTP shutdown failed")
		}
		if err := <-serveError; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return errors.New("runtime HTTPS listener failed")
		}
		if err := applications.Close(); err != nil {
			return errors.New("runtime application shutdown failed")
		}
		shutdownAttempted = true
		if err := manager.Close(); err != nil {
			return errors.New("runtime has unresolved operations")
		}
		managerClosed = true
		return nil
	}
}

func validHostID(value string) bool {
	if len(value) != 8 || value[0] != 'h' {
		return false
	}
	for index := 1; index < len(value); index++ {
		if !(value[index] >= 'a' && value[index] <= 'z') && !(value[index] >= '0' && value[index] <= '9') {
			return false
		}
	}
	return true
}
